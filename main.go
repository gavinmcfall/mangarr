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

	"github.com/gavinmcfall/mangarr/internal/anilist"
	"github.com/gavinmcfall/mangarr/internal/classifier"
	"github.com/gavinmcfall/mangarr/internal/config"
	"github.com/gavinmcfall/mangarr/internal/dbbackup"
	"github.com/gavinmcfall/mangarr/internal/diskspace"
	"github.com/gavinmcfall/mangarr/internal/filer"
	"github.com/gavinmcfall/mangarr/internal/health"
	healthchecks "github.com/gavinmcfall/mangarr/internal/health/checks"
	"github.com/gavinmcfall/mangarr/internal/kavita"
	"github.com/gavinmcfall/mangarr/internal/mangadex"
	"github.com/gavinmcfall/mangarr/internal/metrics"
	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/orchestrator"
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

	// ---- classifier (v2 six-step Classify returning Decision) ----
	// AniList endpoint: use env override if set, else empty string = default (https://graphql.anilist.co).
	anilistEndpoint := os.Getenv("MANGARR_ANILIST_ENDPOINT")

	// ---- suwayomi path cache (Library Map — Plan B) ----
	// Long-lived, shared between the classifier (hot path) and the
	// poller (refreshes it at the top of every tick). The cache stays
	// empty + harmless until the user configures Suwayomi via Settings.
	suwayomiCache := suwayomi.NewPathCache()

	// Classifier: widened AniList client (countryOfOrigin + isAdult +
	// format) + Suwayomi PathCache + store-backed bindings/rules/settings
	// reader. Note: the v1 in-process AniList cache (NewWithCache) is NOT
	// AniList's public endpoint enforces a hard 30-req/min rate limit (down
	// from the documented 90 since their 2025 throttle change). Two polls
	// over a 20-series library can blow the budget mid-tick; the inner
	// client returns "anilist rate limited" and the classifier falls to
	// Unmatched — even for series it classified moments earlier. Wrap the
	// raw client in a 24h success / 6h not-found in-memory TTL cache so
	// repeated polls of the same title don't touch AniList at all once
	// resolved. Transport errors (incl. rate-limit) are NOT cached so the
	// next tick gets a fresh shot.
	anilistRaw := anilist.New(anilistEndpoint)
	anilistClient := classifier.NewCachingAniListClient(anilistRaw, 24*time.Hour, 6*time.Hour)
	clf := classifier.New(anilistClient, suwayomiCache, st)
	clf.Metrics = metricsReg

	// MangaDex fallback (classifier step 4b): AniList's catalogue misses
	// popular manhwa/manhua (no MANGA entry, or only the anime adaptation
	// with a misleading countryOfOrigin). When AniList produces no rule
	// match, the classifier consults MangaDex and maps its originalLanguage
	// (ko/ja/zh → KR/JP/CN) onto the country-of-origin rules. Same TTL-cache
	// shape as AniList. MANGARR_MANGADEX_ENDPOINT overrides the API base for
	// tests; empty uses the public api.mangadex.org.
	mangadexRaw := mangadex.New(os.Getenv("MANGARR_MANGADEX_ENDPOINT"))
	mangadexClient := classifier.NewCachingMangaDexClient(mangadexRaw, 24*time.Hour, 6*time.Hour)
	clf.WithMangaDex(mangadexClient)

	// ---- kavita client ----
	kavitaClient := kavita.New(settings.KavitaBaseURL, settings.KavitaAPIKey)

	// ---- filer ----
	filr := &filer.Filer{
		Mode:         settings.FileMode,
		Scheme:       settings.RenameScheme,
		VolumeScheme: settings.VolumeRenameScheme,
		RecycleBin:   bin,
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
	filerAdpt := &filerAdapter{inner: filr, settings: st}

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
		Activity: st,
		Metrics:  metricsReg,
		// store.Store satisfies poller.Cache, poller.SeriesStore, and
		// poller.BindingLister directly.
		Cache:        st,
		Store:        st,
		Bindings:     st,
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
			return p.RunOnce(ctx)
		},
	}); err != nil {
		log.Fatalf("tasks: register poll-scan: %v", err)
	}

	// activity-gc: prune activity rows past the configured retention. Daily
	// interval is UI metadata; it also runs on the metrics-sweep ticker below
	// and is runnable on demand from the Tasks page.
	if err := reg.Register(tasks.Task{
		ID:       "activity-gc",
		Name:     "Activity GC",
		Interval: 24 * time.Hour,
		RunFn: func(ctx context.Context) error {
			set, err := st.GetSettings()
			if err != nil {
				return err
			}
			if set.ActivityRetentionDays <= 0 {
				return nil // retention disabled
			}
			cutoff := time.Now().AddDate(0, 0, -set.ActivityRetentionDays)
			n, err := st.DeleteActivityOlderThan(cutoff)
			if err != nil {
				return err
			}
			if n > 0 {
				log.Printf("activity-gc: pruned %d activity rows older than %d days", n, set.ActivityRetentionDays)
			}
			return nil
		},
	}); err != nil {
		log.Fatalf("tasks: register activity-gc: %v", err)
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

	// Suwayomi adapter — shared by the orchestrator (built later) and the
	// web handler. Fresh-per-call so URL/cred edits take effect without a
	// pod restart. nil-safe on unconfigured settings: each method returns
	// errSuwayomiUnconfigured which both consumers handle gracefully.
	suwayomiAdapter := &suwayomiOrchAdapter{settings: st}

	// ---- web handler ----
	h := web.NewHandler(web.HandlerOpts{
		Store:                   st,
		Runner:                  p,
		SeriesFiler:             p,
		Refiler:                 p,
		TaskReg:                 reg,
		HealthReg:               healthReg,
		Metrics:                 metricsReg,
		Previewer:               p,
		Suwayomi:                suwayomiAdapter,
		BrowseRoots:             []string{"/media", "/config"},
		RecycleBin:              bin,
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
		// activity-gc is daily-scale; the metrics sweep ticks every 30s, so
		// gate the prune to at most hourly rather than running the DELETE on
		// every sweep. zero value runs it on the first sweep after startup.
		var lastActivityGC time.Time
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

			// 6. Activity retention GC — at most hourly. Best-effort; errors
			// are surfaced via the task's LastErr on the Tasks page.
			if time.Since(lastActivityGC) >= time.Hour {
				lastActivityGC = time.Now()
				if _, err := reg.RunNow(ctx, "activity-gc"); err != nil {
					log.Printf("metrics sweeper: activity-gc: %v", err)
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

	// ---- bulk-download orchestrator ----
	// Ticks every 2 seconds, feeds chapters into Suwayomi's queue with
	// per-source serialisation. The adapter rebuilds the Suwayomi client
	// off current Settings on each call so UI edits take effect without
	// a restart (matches the poller's fresh-per-call pattern).
	bulkOrch := orchestrator.New(st, suwayomiAdapter)
	go func() {
		bulkTicker := time.NewTicker(2 * time.Second)
		defer bulkTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Printf("bulk orchestrator: shutting down")
				return
			case <-bulkTicker.C:
				if err := bulkOrch.Tick(ctx); err != nil {
					log.Printf("bulk orchestrator tick error: %v", err)
				}
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
//	poller.Filer:  File(s model.Series, dstRoot string) error
//	filer.Filer:   File(series string, srcDir string, dstRoot string) error
//
// settings is re-read on every call (fresh-per-call, like the scanner and
// Suwayomi adapters) so mode and rename-scheme edits in the UI take effect on
// the next filing pass without a restart. The Filer is shared with the
// preview Planner, which therefore sees the same refreshed schemes.
type filerAdapter struct {
	inner    *filer.Filer
	settings interface {
		GetSettings() (model.Settings, error)
	}
}

func (a *filerAdapter) File(s model.Series, dstRoot string) error {
	if a.settings != nil {
		if set, err := a.settings.GetSettings(); err != nil {
			log.Printf("filer: get settings: %v (keeping current schemes)", err)
		} else {
			if set.FileMode != "" {
				a.inner.Mode = set.FileMode
			}
			if set.RenameScheme != "" {
				a.inner.Scheme = set.RenameScheme
			}
			a.inner.VolumeScheme = set.VolumeRenameScheme
		}
	}
	return a.inner.File(s.Title, s.SourcePath, dstRoot)
}

// suwayomiOrchAdapter satisfies orchestrator.SuwayomiClient by building a
// fresh *suwayomi.Client off the current Settings on every call. This
// mirrors the poller's fresh-per-call pattern (newSuwayomiClient) so a
// user who edits Suwayomi base URL / credentials in the UI has the change
// take effect on the next orchestrator tick — no pod restart needed.
//
// Returns the orchestrator-friendly errors directly:
//   - settings unreachable    → returned as-is (caller skips the tick)
//   - Suwayomi not configured → returns a wrapped error per method;
//     the orchestrator's per-source error path absorbs it (skip source
//     for this tick, don't crash)
type suwayomiOrchAdapter struct {
	settings interface {
		GetSettings() (model.Settings, error)
	}
}

// errSuwayomiUnconfigured is returned from the adapter methods when the
// user hasn't configured Suwayomi yet. The orchestrator's per-source
// error path treats it as "skip this source for this tick" — identical
// to a transient network error.
var errSuwayomiUnconfigured = errSuwayomiUnconfiguredT{}

type errSuwayomiUnconfiguredT struct{}

func (errSuwayomiUnconfiguredT) Error() string { return "suwayomi not configured" }

func (a *suwayomiOrchAdapter) client() (*suwayomi.Client, error) {
	set, err := a.settings.GetSettings()
	if err != nil {
		return nil, err
	}
	c, err := newSuwayomiClient(set)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errSuwayomiUnconfigured
	}
	return c, nil
}

func (a *suwayomiOrchAdapter) InFlightCountForSource(ctx context.Context, sourceID string) (int, error) {
	c, err := a.client()
	if err != nil {
		return 0, err
	}
	return c.InFlightCountForSource(ctx, sourceID)
}

func (a *suwayomiOrchAdapter) EnqueueChapterDownloads(ctx context.Context, chapterIDs []int64) error {
	c, err := a.client()
	if err != nil {
		return err
	}
	return c.EnqueueChapterDownloads(ctx, chapterIDs)
}

func (a *suwayomiOrchAdapter) ListChapters(ctx context.Context, mangaID int64) ([]suwayomi.Chapter, error) {
	c, err := a.client()
	if err != nil {
		return nil, err
	}
	return c.ListChapters(ctx, mangaID)
}

func (a *suwayomiOrchAdapter) GetChapterMeta(ctx context.Context, chapterID int64) (suwayomi.ChapterMeta, error) {
	c, err := a.client()
	if err != nil {
		return suwayomi.ChapterMeta{}, err
	}
	return c.GetChapterMeta(ctx, chapterID)
}

// ListLibraryWithCategories satisfies web.SuwayomiClient so the same
// adapter wires into both the orchestrator and the /library + /api/bulk
// + /api/library/sync handlers. Fresh-per-call: edits to Suwayomi URL
// or credentials take effect on the next request without a pod restart.
func (a *suwayomiOrchAdapter) ListLibraryWithCategories(ctx context.Context) ([]suwayomi.Manga, error) {
	c, err := a.client()
	if err != nil {
		return nil, err
	}
	return c.ListLibraryWithCategories(ctx)
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
