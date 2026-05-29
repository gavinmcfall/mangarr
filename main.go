// Command mangarr is the mangarr service binary.
//
// It wires together:
//   - config     (env-based)
//   - store      (SQLite)
//   - scanner    (walk download roots → series list)
//   - classifier (AniList lookup, store-backed cache)
//   - filer      (rename + hardlink/move/copy)
//   - kavita     (library scan trigger)
//   - poller     (orchestrate one pass; scheduler ticker)
//   - web        (embedded HTMX UI + JSON API)
//   - dbbackup   (scheduled VACUUM INTO + download API)
//
// Optional env vars (download roots are now managed via the Settings UI):
//
//	MANGARR_DOWNLOAD_ROOTS         (seed roots on first boot if Settings.DownloadRoots is empty)
//	MANGARR_DB_PATH                (default /config/mangarr.db)
//	MANGARR_HTTP_ADDR              (default :8590)
//	MANGARR_ANILIST_ENDPOINT       (override AniList GraphQL URL; default https://graphql.anilist.co)
//	MANGARR_BACKUP_DIR             (default /config/backups)
//	MANGARR_BACKUP_RETENTION_DAYS  (default 14)
//	MANGARR_BACKUP_INTERVAL_HOURS  (default 24)
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gavinmcfall/mangarr/internal/classifier"
	"github.com/gavinmcfall/mangarr/internal/config"
	"github.com/gavinmcfall/mangarr/internal/dbbackup"
	"github.com/gavinmcfall/mangarr/internal/diskspace"
	"github.com/gavinmcfall/mangarr/internal/filer"
	"github.com/gavinmcfall/mangarr/internal/health"
	healthchecks "github.com/gavinmcfall/mangarr/internal/health/checks"
	"github.com/gavinmcfall/mangarr/internal/kavita"
	"github.com/gavinmcfall/mangarr/internal/metrics"
	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/poller"
	"github.com/gavinmcfall/mangarr/internal/recyclebin"
	"github.com/gavinmcfall/mangarr/internal/scanner"
	"github.com/gavinmcfall/mangarr/internal/store"
	"github.com/gavinmcfall/mangarr/internal/suwayomi"
	"github.com/gavinmcfall/mangarr/internal/tasks"
	"github.com/gavinmcfall/mangarr/internal/web"
)

func main() {
	// ---- config ----
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// ---- store ----
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	// ---- load persisted settings for initial wiring ----
	settings, err := st.GetSettings()
	if err != nil {
		log.Fatalf("settings: %v", err)
	}

	// ---- seed download roots from env on first boot ----
	// If Settings.DownloadRoots is empty AND the env var provided roots,
	// copy them in so the UI shows the initial configuration.
	if len(settings.DownloadRoots) == 0 && len(cfg.DownloadRoots) > 0 {
		settings.DownloadRoots = cfg.DownloadRoots
		if err := st.SaveSettings(settings); err != nil {
			log.Printf("settings: seed download roots: %v", err)
		} else {
			log.Printf("settings: seeded %d download root(s) from MANGARR_DOWNLOAD_ROOTS", len(cfg.DownloadRoots))
		}
	}
	if len(settings.DownloadRoots) == 0 {
		log.Printf("warning: no download roots configured — set them on the Settings page")
	}

	// ---- recycle bin ----
	bin := &recyclebin.Bin{
		Root:      cfg.RecycleBinPath,
		Retention: time.Duration(cfg.RecycleBinRetentionDays) * 24 * time.Hour,
	}
	if err := os.MkdirAll(bin.Root, 0o755); err != nil {
		log.Fatalf("recycle bin: create root %s: %v", bin.Root, err)
	}

	// ---- metrics registry ----
	metricsReg := metrics.NewRegistry()

	// ---- classifier (with store-backed cache) ----
	// AniList endpoint: use env override if set, else empty string = default (https://graphql.anilist.co).
	anilistEndpoint := os.Getenv("MANGARR_ANILIST_ENDPOINT")
	clf := classifier.NewWithCache(anilistEndpoint, st)
	clf.Metrics = metricsReg

	// ---- suwayomi path cache (Library Map — Plan B) ----
	// Long-lived, shared between the classifier (hot path) and the
	// poller (refreshes it at the top of every tick). The cache stays
	// empty + harmless until the user configures Suwayomi via Settings.
	suwayomiCache := suwayomi.NewPathCache()
	clf.WithSuwayomi(suwayomiCache, st)

	// ---- kavita client ----
	kavitaClient := kavita.New(settings.KavitaBaseURL, settings.KavitaAPIKey)

	// ---- filer ----
	filr := &filer.Filer{
		Mode:       settings.FileMode,
		Scheme:     settings.RenameScheme,
		RecycleBin: bin,
	}
	if filr.Mode == "" {
		filr.Mode = model.ModeHardlink
	}
	if filr.Scheme == "" {
		filr.Scheme = "{series}/{series} - Ch.{chapter}.cbz"
	}

	// ---- scanner adapter ----
	// The poller wants a Scanner interface (ScanAll() ([]Series, error)).
	// scanner.Scan takes (root, source) — we wrap all configured roots.
	// The closure re-reads Settings on each call so UI changes take effect
	// on the next poller tick without requiring a restart.
	scanAdapter := &multiScanner{settingsProvider: func() []string {
		s, err := st.GetSettings()
		if err != nil {
			log.Printf("scanner: get settings: %v (using empty roots)", err)
			return nil
		}
		return s.DownloadRoots
	}}

	// ---- filer adapter ----
	// poller.Filer wants: File(s model.Series, dstRoot string) error
	// filer.Filer has:    File(series string, srcDir string, dstRoot string) error
	filerAdpt := &filerAdapter{inner: filr}

	// ---- build LibraryIDs map from Settings ----
	libIDs := settings.KavitaLibIDsByType
	if libIDs == nil {
		libIDs = map[model.ContentType]int64{}
	}

	// ---- poller ----
	p := &poller.Poller{
		Scanner:    scanAdapter,
		Classifier: clf,
		Filer:      filerAdpt,
		Kavita:     kavitaClient,
		// store.Store.MarkUnmatched satisfies poller.UnmatchedSink directly.
		Unmatched: st,
		// store.Store.AddActivity satisfies poller.ActivityWriter directly.
		Activity:     st,
		Metrics:      metricsReg,
		// store.Store satisfies poller.Cache and poller.SeriesStore directly.
		Cache:        st,
		Store:        st,
		LibraryRoots: settings.LibraryRoots,
		LibraryIDs:   libIDs,
		RecycleBin:   bin,

		// Library Map (Plan B) — fresh-per-call Suwayomi client built off
		// the current Settings on every tick. nil-safe if the user
		// hasn't configured Suwayomi: the factory returns (nil, nil)
		// when BaseURL is empty and the poller skips the refresh.
		SuwayomiCache:  suwayomiCache,
		SuwayomiClient: newSuwayomiClient,
		Settings:       st,
	}

	// ---- backup function (closure over db and config) ----
	backupFn := func() (dbbackup.Entry, error) {
		path, err := dbbackup.Backup(st.DB(), cfg.BackupDir, time.Now())
		if err != nil {
			return dbbackup.Entry{}, err
		}
		// Build an Entry from the freshly-written file.
		entries, err := dbbackup.List(cfg.BackupDir)
		if err != nil {
			return dbbackup.Entry{}, err
		}
		for _, e := range entries {
			if e.Path == path {
				return e, nil
			}
		}
		// Fallback: return a minimal entry if List can't find it immediately.
		return dbbackup.Entry{Name: lastPathComponent(path), Path: path}, nil
	}

	// ---- task registry ----
	reg := tasks.NewRegistry()
	if err := reg.Register(tasks.Task{
		ID:   "poll-scan",
		Name: "Poll Scan",
		// Interval is metadata for the UI; actual scheduling is the ticker below.
		Interval: time.Duration(func() int {
			pm := settings.PollMinutes
			if pm <= 0 {
				pm = 15
			}
			return pm
		}()) * time.Minute,
		RunFn: func(ctx context.Context) error {
			return p.RunOnce()
		},
	}); err != nil {
		log.Fatalf("tasks: register poll-scan: %v", err)
	}

	// ---- health registry ----
	healthReg := health.NewRegistry()
	settingsLoader := func() (model.Settings, error) { return st.GetSettings() }
	// Download roots are now Settings-managed; derive them dynamically for checks.
	downloadRootsLoader := func() []string {
		s, err := st.GetSettings()
		if err != nil {
			return nil
		}
		return s.DownloadRoots
	}
	for _, check := range []health.Check{
		healthchecks.DownloadRootsCheck(downloadRootsLoader()),
		healthchecks.LibraryRootsCheck(settingsLoader),
		healthchecks.KavitaCheck(kavitaClient),
		healthchecks.AniListCheck(anilistEndpoint),
		healthchecks.SQLiteCheck(st.DB()),
		healthchecks.DiskSpaceCheck(downloadRootsLoader(), 15, 5),
		healthchecks.RenameSchemeCheck(settingsLoader),
	} {
		if err := healthReg.Register(check); err != nil {
			log.Fatalf("health: %v", err)
		}
	}

	// Wire the planner into the poller so Preview can call Plan.
	p.Planner = filr

	// ---- web handler ----
	h := web.NewHandler(web.HandlerOpts{
		Store:                   st,
		Runner:                  p,
		SeriesFiler:             p,
		TaskReg:                 reg,
		HealthReg:               healthReg,
		Metrics:                 metricsReg,
		Previewer:               p,
		BrowseRoots:             []string{"/media", "/config"},
		RecycleBinPath:          cfg.RecycleBinPath,
		RecycleBinRetentionDays: cfg.RecycleBinRetentionDays,
		Backup: web.BackupOpts{
			Config: web.BackupConfig{
				Dir:           cfg.BackupDir,
				RetentionDays: cfg.BackupRetentionDays,
				IntervalHours: cfg.BackupIntervalHours,
			},
			Fn: backupFn,
		},
	})
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// ---- graceful shutdown context ----
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// ---- metrics sweeper ----
	// Runs every 30 s; populates gauges from store + disk + health state.
	// Individual step errors are logged and skipped — never abort the sweep.
	metricsTicker := time.NewTicker(30 * time.Second)
	go func() {
		sweep := func() {
			// 1. Series counts by category.
			if seriesList, err := st.ListSeries(); err != nil {
				log.Printf("metrics sweeper: list series: %v", err)
			} else {
				counts := map[string]int{}
				for _, s := range seriesList {
					counts[string(s.Type)]++
				}
				for cat, n := range counts {
					metricsReg.SetSeriesCount(cat, n)
				}
			}

			// 2. Unmatched count.
			if unmatched, err := st.ListUnmatched(); err != nil {
				log.Printf("metrics sweeper: list unmatched: %v", err)
			} else {
				metricsReg.SetUnmatchedCount(len(unmatched))
			}

			// 3. Disk space for download roots + library roots.
			sweepSettings, err := st.GetSettings()
			if err != nil {
				log.Printf("metrics sweeper: get settings: %v", err)
			} else {
				paths := make(map[string]bool)
				for _, p := range sweepSettings.DownloadRoots {
					if p != "" {
						paths[p] = true
					}
				}
				for _, root := range sweepSettings.LibraryRoots {
					if root != "" {
						paths[root] = true
					}
				}
				for path := range paths {
					info := diskspace.Stat(path)
					if info.Err != nil {
						log.Printf("metrics sweeper: diskspace %q: %v", path, info.Err)
						continue
					}
					metricsReg.SetDiskFreeBytes(path, info.FreeBytes)
					metricsReg.SetDiskTotalBytes(path, info.TotalBytes)
				}
			}

			// 4. Health check statuses.
			healthResults := healthReg.RunAll(ctx)
			for _, res := range healthResults {
				var statusInt int
				switch res.Status {
				case health.StatusOK:
					statusInt = 0
				case health.StatusWarn:
					statusInt = 1
				default: // error
					statusInt = 2
				}
				metricsReg.SetHealthStatus(res.ID, statusInt)
			}

			// 5. Backup count + newest mod time.
			if entries, err := dbbackup.List(cfg.BackupDir); err != nil {
				log.Printf("metrics sweeper: list backups: %v", err)
			} else {
				metricsReg.SetBackupCount(len(entries))
				if len(entries) > 0 {
					metricsReg.SetBackupLastModTime(entries[0].ModTime)
				}
			}
		}

		// Run immediately on startup so metrics are populated before first scrape.
		sweep()
		for {
			select {
			case <-metricsTicker.C:
				sweep()
			case <-ctx.Done():
				metricsTicker.Stop()
				return
			}
		}
	}()

	// ---- poll scheduler ----
	pollMinutes := settings.PollMinutes
	if pollMinutes <= 0 {
		pollMinutes = 15
	}
	ticker := time.NewTicker(time.Duration(pollMinutes) * time.Minute)
	go func() {
		// Run once immediately on startup so the UI has data straight away.
		log.Printf("poller: initial scan starting")
		if _, err := reg.RunNow(ctx, "poll-scan"); err != nil {
			log.Printf("poller: initial run error: %v", err)
		}
		for {
			select {
			case <-ticker.C:
				log.Printf("poller: scheduled tick — running scan")
				if _, err := reg.RunNow(ctx, "poll-scan"); err != nil {
					log.Printf("poller: run error: %v", err)
				}
			case <-ctx.Done():
				log.Printf("poller: shutting down")
				ticker.Stop()
				return
			}
		}
	}()

	// ---- backup scheduler ----
	backupTicker := time.NewTicker(time.Duration(cfg.BackupIntervalHours) * time.Hour)
	retention := time.Duration(cfg.BackupRetentionDays) * 24 * time.Hour
	go func() {
		// Run once immediately on startup so there is always at least one backup.
		log.Printf("backup: initial backup starting")
		if path, err := dbbackup.Backup(st.DB(), cfg.BackupDir, time.Now()); err != nil {
			log.Printf("backup: initial backup error: %v", err)
		} else {
			log.Printf("backup: initial backup written to %s", path)
		}
		if n, err := dbbackup.GC(cfg.BackupDir, retention, time.Now()); err != nil {
			log.Printf("backup: gc error: %v", err)
		} else if n > 0 {
			log.Printf("backup: gc removed %d old backup(s)", n)
		}
		for {
			select {
			case <-backupTicker.C:
				log.Printf("backup: scheduled tick — running backup")
				if path, err := dbbackup.Backup(st.DB(), cfg.BackupDir, time.Now()); err != nil {
					log.Printf("backup: error: %v", err)
				} else {
					log.Printf("backup: written to %s", path)
				}
				if n, err := dbbackup.GC(cfg.BackupDir, retention, time.Now()); err != nil {
					log.Printf("backup: gc error: %v", err)
				} else if n > 0 {
					log.Printf("backup: gc removed %d old backup(s)", n)
				}
			case <-ctx.Done():
				log.Printf("backup: shutting down")
				backupTicker.Stop()
				return
			}
		}
	}()

	// ---- start web server ----
	go func() {
		log.Printf("mangarr listening on %s", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("http server error: %v", err)
		}
	}()

	// ---- wait for SIGINT/SIGTERM ----
	<-ctx.Done()
	log.Printf("mangarr shutting down")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Printf("http shutdown error: %v", err)
	}
}

// multiScanner wraps scanner.Scan for multiple download roots.
// It satisfies poller.Scanner.
// settingsProvider is called on every ScanAll() so UI changes to download roots
// take effect on the next poller tick without requiring a restart.
type multiScanner struct {
	settingsProvider func() []string
}

func (m *multiScanner) ScanAll() ([]model.Series, error) {
	roots := m.settingsProvider()
	if len(roots) == 0 {
		// No roots configured — return empty, don't error.
		return nil, nil
	}
	var all []model.Series
	for _, root := range roots {
		source := lastPathComponent(root)
		series, err := scanner.Scan(root, source)
		if err != nil {
			// Log and skip — one bad root must not abort the whole tick.
			log.Printf("scanner: root %q error: %v (skipping)", root, err)
			continue
		}
		all = append(all, series...)
	}
	return all, nil
}

// lastPathComponent returns the last slash-separated component of a path.
func lastPathComponent(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[i+1:]
		}
	}
	return p
}

// filerAdapter adapts *filer.Filer to the poller.Filer interface.
//
//   poller.Filer:  File(s model.Series, dstRoot string) error
//   filer.Filer:   File(series string, srcDir string, dstRoot string) error
type filerAdapter struct {
	inner *filer.Filer
}

func (a *filerAdapter) File(s model.Series, dstRoot string) error {
	return a.inner.File(s.Title, s.SourcePath, dstRoot)
}

// newSuwayomiClient is the SuwayomiClientFactory the poller invokes at
// the top of every tick. It builds a fresh client off the supplied
// Settings — never captures state at boot — so users can edit the
// Suwayomi URL/credentials in the UI and have them take effect on the
// next tick without a pod restart (PR #28 fresh-per-call pattern).
//
// Returns (nil, nil) when SuwayomiBaseURL is empty so the poller can
// distinguish "feature not configured" from "construction failed".
func newSuwayomiClient(set model.Settings) (*suwayomi.Client, error) {
	if set.SuwayomiBaseURL == "" {
		return nil, nil
	}
	var auth suwayomi.Auth
	switch set.SuwayomiAuthType {
	case model.SuwayomiAuthBasic:
		auth = suwayomi.BasicAuth{Username: set.SuwayomiUsername, Password: set.SuwayomiPassword}
	case model.SuwayomiAuthSimple:
		auth = &suwayomi.SimpleLoginAuth{Username: set.SuwayomiUsername, Password: set.SuwayomiPassword}
	case model.SuwayomiAuthUI:
		auth = &suwayomi.UILoginAuth{Username: set.SuwayomiUsername, Password: set.SuwayomiPassword}
	default:
		auth = suwayomi.NoAuth{}
	}
	return suwayomi.New(set.SuwayomiBaseURL, auth), nil
}
