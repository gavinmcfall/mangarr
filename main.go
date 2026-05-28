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
//
// Required env var: MANGARR_DOWNLOAD_ROOTS (comma-separated paths).
// Optional:
//   MANGARR_DB_PATH         (default /config/mangarr.db)
//   MANGARR_HTTP_ADDR       (default :8590)
//   MANGARR_ANILIST_ENDPOINT (override AniList GraphQL URL; default https://graphql.anilist.co)
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
	"github.com/gavinmcfall/mangarr/internal/filer"
	"github.com/gavinmcfall/mangarr/internal/kavita"
	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/poller"
	"github.com/gavinmcfall/mangarr/internal/scanner"
	"github.com/gavinmcfall/mangarr/internal/store"
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

	// ---- classifier (with store-backed cache) ----
	// AniList endpoint: use env override if set, else empty string = default (https://graphql.anilist.co).
	anilistEndpoint := os.Getenv("MANGARR_ANILIST_ENDPOINT")
	clf := classifier.NewWithCache(anilistEndpoint, st)

	// ---- kavita client ----
	kavitaClient := kavita.New(settings.KavitaBaseURL, settings.KavitaAPIKey)

	// ---- filer ----
	filr := &filer.Filer{
		Mode:   settings.FileMode,
		Scheme: settings.RenameScheme,
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
	scanAdapter := &multiScanner{roots: cfg.DownloadRoots}

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
		LibraryRoots: settings.LibraryRoots,
		LibraryIDs:   libIDs,
	}

	// ---- web handler ----
	h := web.NewHandler(st, p)
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// ---- graceful shutdown context ----
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// ---- poll scheduler ----
	pollMinutes := settings.PollMinutes
	if pollMinutes <= 0 {
		pollMinutes = 15
	}
	ticker := time.NewTicker(time.Duration(pollMinutes) * time.Minute)
	go func() {
		// Run once immediately on startup so the UI has data straight away.
		log.Printf("poller: initial scan starting")
		if err := p.RunOnce(); err != nil {
			log.Printf("poller: initial run error: %v", err)
		}
		for {
			select {
			case <-ticker.C:
				log.Printf("poller: scheduled tick — running scan")
				if err := p.RunOnce(); err != nil {
					log.Printf("poller: run error: %v", err)
				}
			case <-ctx.Done():
				log.Printf("poller: shutting down")
				ticker.Stop()
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
type multiScanner struct {
	roots []string
}

func (m *multiScanner) ScanAll() ([]model.Series, error) {
	var all []model.Series
	for _, root := range m.roots {
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
