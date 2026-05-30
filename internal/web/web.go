// Package web provides the embedded HTMX UI and JSON API for mangarr.
//
// Routes (Go 1.22 method+path syntax):
//
//	GET  /                           → redirect to /series
//	GET  /series                     → Series page (HTMX)
//	GET  /preview                    → Preview / dry-run page (HTMX)
//	GET  /unmatched                  → Unmatched page (HTMX)
//	GET  /activity                   → Activity/History page (HTMX)
//	GET  /health                     → Health checks page (HTMX)
//	GET  /tasks                      → Tasks page (HTMX)
//	GET  /settings                   → Settings page (HTMX)
//	POST /settings                   → Save settings (form POST, redirect back)
//	POST /api/series/{id}/reclassify → Override a series' type (HTMX form target, Series page)
//	POST /api/series/{id}/assign    → Classify, cache, file, and Kavita-scan one series (Unmatched page)
//	POST /api/rescan                 → Trigger poll-scan via task registry
//	GET  /api/preview                → JSON []PreviewEntry (dry-run pipeline)
//	GET  /api/series                 → JSON list of all series
//	GET  /api/unmatched              → JSON list of unmatched series
//	GET  /api/activity               → JSON activity log
//	GET  /api/health                 → JSON health check results (Gatus-compatible)
//	GET  /api/tasks                  → JSON list of registered tasks
//	POST /api/tasks/{id}/run         → Run a task by ID
//	GET  /api/settings               → JSON current settings
//	PUT  /api/settings               → JSON update settings
//	GET  /api/diskspace              → JSON disk space for all roots
//	GET  /api/browse                 → JSON directory listing (path browser; allowlist-restricted)
//	GET  /api/browse/fragment        → HTMX HTML fragment for the path-browser modal
//	GET  /api/backups                → JSON list of backup entries (newest first)
//	POST /api/backups/run            → Trigger immediate backup; returns new Entry as JSON
//	GET  /api/backups/{name}         → Download a backup file
//	GET  /api/kavita/libraries       → JSON list of Kavita libraries
//	GET  /api/kavita/libraries/fragment → HTMX HTML fragment: three library <select> elements
//	GET  /api/bindings                → JSON list of Library Bindings (Plan B v2)
//	GET  /api/rules                   → JSON list of Classification Rules ordered ascending by priority (Plan B v2)
//	GET  /metrics                    → Prometheus metrics (text/plain; version=0.0.4)
//	GET  /static/*                   → Embedded static assets (htmx.min.js)
package web

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gavinmcfall/mangarr/internal/classifier"
	"github.com/gavinmcfall/mangarr/internal/dbbackup"
	"github.com/gavinmcfall/mangarr/internal/diskspace"
	"github.com/gavinmcfall/mangarr/internal/filer"
	"github.com/gavinmcfall/mangarr/internal/health"
	"github.com/gavinmcfall/mangarr/internal/kavita"
	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/poller"
	"github.com/gavinmcfall/mangarr/internal/tasks"
)

// overrideRowView is the view-model for one Suwayomi category override row
// on the Settings page. It pre-resolves the display name (from a fresh
// Suwayomi snapshot) and the AniList content-type badge (from a reverse
// lookup against KavitaLibIDsByType — the Plan B carry-forward constraint).
type overrideRowView struct {
	Index       int
	CatID       int64
	CatName     string // resolved category name, or "Unknown (ID: N)" when stale
	CatKnown    bool   // false → render the "Unknown" hint inline
	LibID       int64
	LibLabel    string
	ContentType model.ContentType
}

//go:embed templates/*.html static/htmx.min.js static/mangarr.css
var assets embed.FS

// Store is the subset of store.Store the web package needs.
type Store interface {
	ListSeries() ([]model.Series, error)
	ListUnmatched() ([]model.Series, error)
	ListActivity(limit int) ([]model.ActivityEntry, error)
	GetSettings() (model.Settings, error)
	SaveSettings(model.Settings) error
	SetSeriesType(id int64, ct model.ContentType) error
	ListBindings() ([]model.Binding, error)
	ListRules() ([]model.ClassificationRule, error)
}

// Runner can execute one poll pass on demand.
//
// ctx is plumbed only to the Suwayomi PathCache.Refresh step inside
// RunOnce (Plan B). Older Scanner/Filer/Kavita calls inside RunOnce
// retain their pre-Plan-B signatures and ignore ctx — that wider
// refactor is intentionally out of scope here.
type Runner interface {
	RunOnce(ctx context.Context) error
}

// BackupConfig holds the backup-scheduler configuration passed into the Handler
// for display on the Settings page.
type BackupConfig struct {
	Dir           string
	RetentionDays int
	IntervalHours int
}

// TaskRegistry is the subset of tasks.Registry the web package needs.
type TaskRegistry interface {
	List() []tasks.Info
	Get(id string) (tasks.Info, bool)
	RunNow(ctx context.Context, id string) (tasks.Info, error)
}

// HealthRegistry is the subset of health.Registry the web package needs.
type HealthRegistry interface {
	RunAll(ctx context.Context) []health.Result
}

// MetricsSink is the subset of the metrics.Registry the web package needs.
type MetricsSink interface {
	Handler() http.Handler // promhttp.HandlerFor the registry
}

// Previewer can run the full pipeline dry-run without side effects.
// poller.Poller satisfies this interface via its Preview method.
type Previewer interface {
	Preview(ctx context.Context) ([]poller.PreviewEntry, error)
}

// SeriesFiler can file a single series on demand.
// poller.Poller satisfies this interface via its FileOne method.
type SeriesFiler interface {
	FileOne(ctx context.Context, seriesID int64, ct model.ContentType) error
}

// HandlerOpts is passed to NewHandler to wire all dependencies.
// Using a struct keeps the constructor stable as the surface grows:
// adding a new optional field is a backwards-compatible change, whereas
// adding a positional argument is not.
type HandlerOpts struct {
	Store       Store
	Runner      Runner
	SeriesFiler SeriesFiler    // optional; /api/series/{id}/assign returns 503 when nil
	TaskReg     TaskRegistry   // optional; tasks routes return 503 when nil
	HealthReg   HealthRegistry // optional; health routes show a placeholder
	Metrics     MetricsSink    // optional; /metrics returns 503 when nil
	Previewer   Previewer      // optional; /preview returns placeholder when nil

	BrowseRoots             []string // allowlist for /api/browse; defaults to ["/media", "/config"]
	RecycleBinPath          string
	RecycleBinRetentionDays int
	Backup                  BackupOpts
}

// BackupOpts groups backup-related configuration for HandlerOpts.
type BackupOpts struct {
	Config BackupConfig
	Fn     func() (dbbackup.Entry, error)
}

// Handler is the HTTP handler for the web UI and JSON API.
type Handler struct {
	mux                     *http.ServeMux
	tmpls                   map[string]*template.Template // one template set per page
	store                   Store
	runner                  Runner
	previewer               Previewer                     // optional; /preview returns placeholder when nil
	seriesFiler             SeriesFiler                   // optional; /api/series/{id}/assign returns 503 when nil
	browseRoots             []string                      // allowlist for /api/browse (injected; tests can override)
	recycleBinPath          string
	recycleBinRetentionDays int
	backupDir               string
	backupCfg               BackupConfig
	backupFn                func() (dbbackup.Entry, error) // injected for on-demand backup
	taskReg                 TaskRegistry                   // injected for Tasks page + /api/tasks
	healthReg               HealthRegistry                 // injected for Health page + /api/health
	metricsHandler          http.Handler                   // nil → /metrics returns 503
}

// NewHandler wires up all routes and parses embedded templates from a HandlerOpts struct.
// All fields are optional except Store.
func NewHandler(opts HandlerOpts) *Handler {
	var mh http.Handler
	if opts.Metrics != nil {
		mh = opts.Metrics.Handler()
	}
	browseRoots := opts.BrowseRoots
	if browseRoots == nil {
		browseRoots = []string{"/media", "/config"}
	}
	h := &Handler{
		mux:                     http.NewServeMux(),
		tmpls:                   parsePageTemplates(),
		store:                   opts.Store,
		runner:                  opts.Runner,
		previewer:               opts.Previewer,
		seriesFiler:             opts.SeriesFiler,
		browseRoots:             browseRoots,
		recycleBinPath:          opts.RecycleBinPath,
		recycleBinRetentionDays: opts.RecycleBinRetentionDays,
		backupDir:               opts.Backup.Config.Dir,
		backupCfg:               opts.Backup.Config,
		backupFn:                opts.Backup.Fn,
		taskReg:                 opts.TaskReg,
		healthReg:               opts.HealthReg,
		metricsHandler:          mh,
	}

	// Static assets
	h.mux.Handle("GET /static/", http.FileServerFS(assets))

	// HTML pages
	h.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/series", http.StatusFound)
	})
	h.mux.HandleFunc("GET /series", h.pageSeries)
	h.mux.HandleFunc("GET /preview", h.pagePreview)
	h.mux.HandleFunc("GET /unmatched", h.pageUnmatched)
	h.mux.HandleFunc("GET /activity", h.pageActivity)
	h.mux.HandleFunc("GET /health", h.pageHealth)
	h.mux.HandleFunc("GET /tasks", h.pageTasks)
	h.mux.HandleFunc("GET /settings", h.pageSettings)
	h.mux.HandleFunc("POST /settings", h.saveSettings)

	// JSON API
	h.mux.HandleFunc("GET /api/preview", h.apiPreview)
	h.mux.HandleFunc("GET /api/series", h.apiListSeries)
	h.mux.HandleFunc("GET /api/unmatched", h.apiListUnmatched)
	h.mux.HandleFunc("GET /api/activity", h.apiListActivity)
	h.mux.HandleFunc("GET /api/health", h.apiHealth)
	h.mux.HandleFunc("GET /api/tasks", h.apiListTasks)
	h.mux.HandleFunc("POST /api/tasks/{id}/run", h.apiRunTask)
	h.mux.HandleFunc("GET /api/settings", h.apiGetSettings)
	h.mux.HandleFunc("PUT /api/settings", h.apiPutSettings)
	h.mux.HandleFunc("POST /api/rescan", h.apiRescan)
	h.mux.HandleFunc("GET /api/diskspace", h.apiDiskSpace)

	// Path-browser API (for Library Roots Browse button)
	h.mux.HandleFunc("GET /api/browse", h.apiBrowse)
	h.mux.HandleFunc("GET /api/browse/fragment", h.apiBrowseFragment)

	// Kavita library picker API
	h.mux.HandleFunc("GET /api/kavita/libraries", h.apiKavitaLibraries)
	h.mux.HandleFunc("GET /api/kavita/libraries/fragment", h.apiKavitaLibrariesFragment)

	// Suwayomi connection + category override API
	h.mux.HandleFunc("GET /api/suwayomi/test", h.apiSuwayomiTest)
	h.mux.HandleFunc("GET /api/suwayomi/categories", h.apiSuwayomiCategories)
	h.mux.HandleFunc("GET /api/suwayomi/categories/fragment", h.apiSuwayomiCategoriesFragment)

	// Library Bindings v2 read-only JSON endpoints (Plan B)
	h.mux.HandleFunc("GET /api/bindings", h.apiBindings)
	h.mux.HandleFunc("GET /api/rules", h.apiRules)

	// HTMX action: per-series reclassify (POST /api/series/{id}/reclassify)
	h.mux.HandleFunc("POST /api/series/{id}/reclassify", h.apiReclassify)

	// HTMX action: manual classify-and-file from Unmatched page (POST /api/series/{id}/assign)
	h.mux.HandleFunc("POST /api/series/{id}/assign", h.apiAssign)

	// Backup API
	h.mux.HandleFunc("GET /api/backups", h.apiListBackups)
	h.mux.HandleFunc("POST /api/backups/run", h.apiRunBackup)
	h.mux.HandleFunc("GET /api/backups/{name}", h.apiDownloadBackup)

	// Prometheus metrics endpoint
	h.mux.HandleFunc("GET /metrics", h.serveMetrics)

	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// parsePageTemplates builds a map of page-name → *template.Template.
// Each entry is a fresh clone of the base template with one page template
// overlaid, ensuring that {{block "content"}} resolves to THAT page's
// definition rather than whichever happened to be parsed last globally.
func parsePageTemplates() map[string]*template.Template {
	pages := []string{"series.html", "preview.html", "unmatched.html", "activity.html", "health.html", "tasks.html", "settings.html"}
	// Pages that need the override-rows partial. Listed explicitly so
	// adding the partial to a new page is a one-line change here, not a
	// fan-out across the codebase.
	withOverrideRows := map[string]bool{"settings.html": true}
	// Pages that need the binding-rows partial (Plan B Library Bindings card).
	withBindingRows := map[string]bool{"settings.html": true}
	// Pages that need the rule-rows partial (Plan B Classification Rules card).
	withRuleRows := map[string]bool{"settings.html": true}
	m := make(map[string]*template.Template, len(pages)+1)
	for _, name := range pages {
		// Parse base + the specific page. This gives each page its own
		// independent template set so block overrides don't bleed across pages.
		files := []string{"templates/base.html", "templates/" + name}
		if withOverrideRows[name] {
			files = append(files, "templates/override-rows.html")
		}
		if withBindingRows[name] {
			files = append(files, "templates/binding-rows.html")
		}
		if withRuleRows[name] {
			files = append(files, "templates/rule-rows.html")
		}
		t := template.Must(
			template.New("").Funcs(templateFuncs()).ParseFS(assets, files...),
		)
		m[name] = t
	}
	// Standalone override-fragment template for the HTMX swap target.
	// Same source file, so the override-rows partial is byte-for-byte
	// identical to what Settings renders. Single source of truth.
	m["override-fragment"] = template.Must(
		template.New("").Funcs(templateFuncs()).ParseFS(assets,
			"templates/override-rows.html",
		),
	)
	return m
}

// templateFuncs returns the custom template functions used in .html files.
//
// We deliberately do NOT define a "not" helper here — html/template already
// evaluates slice truthiness correctly (nil/empty → false), so templates use
// native `{{if .Items}}...{{else}}empty-state{{end}}` instead. A custom `not`
// helper that takes interface{} was previously broken because non-nil slices
// boxed in interface{} fall to a default-return-false branch, silently
// hiding the empty-state.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		// lower converts any string-like value to lowercase.
		// Uses fmt.Sprintf to handle named string types (model.ContentType, model.Status).
		"lower": func(v interface{}) string {
			return strings.ToLower(fmt.Sprintf("%s", v))
		},
		// formatAge renders a time.Time as a relative human-readable string.
		"formatAge": formatAge,
		// formatInterval renders a task IntervalMs as a short string, e.g. "15m".
		"formatInterval": formatInterval,
		// kavitaLibInList returns true if the given ID exists in the Library slice.
		// Used in settings.html to detect orphaned saved IDs.
		"kavitaLibInList": func(id int64, libs []kavita.Library) bool {
			for _, lib := range libs {
				if lib.ID == id {
					return true
				}
			}
			return false
		},
		// kavitaLibNotInList returns true if the given ID is NOT in the Library slice.
		// Avoids shadowing the built-in `not` template function which works on any truthy value.
		"kavitaLibNotInList": func(id int64, libs []kavita.Library) bool {
			for _, lib := range libs {
				if lib.ID == id {
					return false
				}
			}
			return true
		},
		// deref nil-safely dereferences a *bool, returning false for nil.
		// Used by rule-rows.html to compare a rule's IsAdult condition
		// pointer against a yes/no <option> value without exploding on nil.
		"deref": func(b *bool) bool {
			if b == nil {
				return false
			}
			return *b
		},
		// derefStr nil-safely dereferences a *string, returning "" for nil.
		// Used by rule-rows.html to compare a rule's CountryOfOrigin /
		// Format condition pointers against string literals in <option>
		// values. html/template's `eq` rejects *string vs string with
		// "incompatible types for comparison" — derefing in Go avoids
		// the reflect-time type mismatch.
		"derefStr": func(s *string) string {
			if s == nil {
				return ""
			}
			return *s
		},
		// bindingExists returns true if the given binding ID is present
		// in the supplied bindings slice. Used by rule-rows.html to detect
		// orphaned BindingID references and render the
		// "Unknown binding (ID: N)" placeholder option.
		"bindingExists": func(bindings []model.Binding, id int64) bool {
			for _, b := range bindings {
				if b.ID == id {
					return true
				}
			}
			return false
		},
		// int64Or nil-safely dereferences a *int64, returning the fallback
		// for nil. Used by the Default Binding picker so the template can
		// compare {{.Settings.DefaultBindingID}} against literal option
		// values without an html/template type-mismatch on the nil case.
		"int64Or": func(p *int64, fallback int64) int64 {
			if p == nil {
				return fallback
			}
			return *p
		},
	}
}

// ---- page data types ----

type pageData struct {
	Page  string
	Items interface{}
	Flash string
	Error string
}

// previewPageData is passed to templates/preview.html.
type previewPageData struct {
	Page         string
	Matched      []previewRow
	Unmatched    []previewRow
	Misconfigured []previewRow
	Placeholder  bool   // true when previewer is not wired
}

// previewRow is the view-model for one series on the Preview page.
type previewRow struct {
	Title        string
	Source       string
	SourcePath   string
	Type         string
	DstRoot      string
	Reason       string
	Note         string
	ChapterCount int
	FileSummary  string // e.g. "3 file, 5 skip"
}

// diskPathEntry is a labelled path within a filesystem group.
type diskPathEntry struct {
	Label string // e.g. "Download root" or "Manga library"
	Path  string
}

// fsDiskRow is a single row in the disk-space display: one unique filesystem
// with its space info and the list of all source paths that share it.
type fsDiskRow struct {
	MountLabel     string          // common path prefix or "Filesystem N"
	Paths          []diskPathEntry // all contributing paths
	Free           string          // formatted free bytes, e.g. "42.0 GiB"
	Total          string          // formatted total bytes
	PercentFmt     string          // e.g. "73"  (integer %, no decimal, for bar width — represents %used)
	PercentUsedFmt string          // e.g. "73%" — human-readable label shown inside/beside bar
	BarClass       string          // "bar-ok" | "bar-warn" | "bar-err"
	Err            string          // non-empty when path is unavailable
}

// diskSpaceClass returns the CSS class for the bar fill based on percent USED.
// Green < 75% used, orange 75–90%, red > 90%.
func diskSpaceClass(pctUsed float64) string {
	switch {
	case pctUsed < 75:
		return "bar-ok"
	case pctUsed < 90:
		return "bar-warn"
	default:
		return "bar-err"
	}
}

// settingsPageData holds pre-extracted plain fields for the Settings template.
//
// We deliberately AVOID using {{index .Settings.LibraryRoots "Manga"}} in
// the template: html/template's reflect-based call requires the key arg type
// to match exactly, and "Manga" is a string literal while LibraryRoots is
// keyed by model.ContentType. The reflection panic returns HTTP 200 with a
// half-rendered body — a silent UX break. Extract values in Go where types
// are checked at compile time, then pass plain strings/ints to the template.
type settingsPageData struct {
	Page                    string
	Settings                model.Settings
	KavitaAPIKey            string
	Flash                   string
	Error                   string
	DownloadRoots           []string // pre-extracted from Settings for template convenience
	RootManga               string
	RootManhwa              string
	RootManhua              string
	KavitaLibManga          int64
	KavitaLibManhwa         int64
	KavitaLibManhua         int64
	// KavitaLibraries holds the fetched Kavita library list for the select dropdowns.
	// Nil/empty means Kavita is not configured or unreachable → render placeholders.
	KavitaLibraries []kavita.Library
	// KavitaLibError is set when a library fetch attempt failed; displayed inline.
	KavitaLibError string

	// --- Library Bindings v2 (Plan B) ---
	// Bindings carries every persisted Library Binding from the v2 store.
	// Empty slice renders an empty-state hint; the "+ Add Binding"
	// affordance is always present. Reuses .KavitaLibraries (above) for
	// the per-row Kavita dropdown so we fetch once and render twice.
	Bindings []model.Binding

	// Rules carries every persisted Classification Rule from the v2 store
	// in ascending Priority order. Empty slice renders an empty-state hint;
	// the "+ Add Rule" affordance is always present. The per-row target
	// dropdown reuses .Bindings (above) for its options so a deleted
	// binding referenced by a rule renders as "Unknown binding (ID: N)"
	// instead of silently disappearing.
	Rules []model.ClassificationRule

	// --- Library Map: Suwayomi connection ---
	SuwayomiBaseURL  string
	SuwayomiAuthType model.SuwayomiAuthType
	SuwayomiUsername string
	SuwayomiPassword string
	// LibraryMap carries the override-card view-model shared with the
	// HTMX fragment endpoint. Single source of truth for the override
	// rows, library-name resolution, and configure-first prompts.
	LibraryMap libraryMapData
	RenameExample           string
	DiskRows                []fsDiskRow
	RecycleBinPath          string
	RecycleBinRetentionDays int
	BackupDir               string
	BackupRetentionDays     int
	BackupIntervalHours     int
	Backups                 []backupEntryView
	BackupNow               time.Time
}

// backupEntryView wraps dbbackup.Entry with pre-formatted display strings.
type backupEntryView struct {
	dbbackup.Entry
	SizeHuman string
	AgeHuman  string
}

// taskRow is the view-model for a single row on the Tasks page.
// We pre-compute display strings in Go to keep template logic minimal.
type taskRow struct {
	tasks.Info
	IntervalLabel string // e.g. "15m", "On demand"
	LastRunLabel  string // e.g. "3 minutes ago", "never"
	// ResultClass is the CSS class for the status pill.
	ResultClass string // "success", "error", "pending"
}

// tasksPageData is passed to templates/tasks.html.
type tasksPageData struct {
	Page  string
	Items []taskRow
}

// healthRow is the view-model for one row on the Health page.
type healthRow struct {
	health.Result
	// StatusClass is the CSS class for the status pill.
	StatusClass string // "success" | "warn" | "error"
}

// healthPageData is passed to templates/health.html.
type healthPageData struct {
	Page        string
	Items       []healthRow
	OverallStatus health.Status
	Unregistered  bool // true when healthReg is nil
}

// ---- HTML page handlers ----

// renameExample computes a live preview string for the Settings page.
// It renders the scheme with fixed sample values ("Berserk", "Ch. 350.cbz").
// If the scheme is empty or fails validation the returned string is a
// human-readable placeholder rather than an error, so the template can render
// it unconditionally.
func renameExample(scheme string) string {
	if scheme == "" || filer.ValidateScheme(scheme) != nil {
		return "(invalid scheme — fix to see preview)"
	}
	return filer.RenderName(scheme, "Berserk", "Ch. 350.cbz")
}

func (h *Handler) pageSeries(w http.ResponseWriter, r *http.Request) {
	list, err := h.store.ListSeries()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.render(w, "series.html", pageData{Page: "series", Items: list})
}

func (h *Handler) pagePreview(w http.ResponseWriter, r *http.Request) {
	if h.previewer == nil {
		h.render(w, "preview.html", previewPageData{Page: "preview", Placeholder: true})
		return
	}
	entries, err := h.previewer.Preview(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data := buildPreviewPageData(entries)
	data.Page = "preview"
	h.render(w, "preview.html", data)
}

func (h *Handler) apiPreview(w http.ResponseWriter, r *http.Request) {
	if h.previewer == nil {
		jsonOK(w, []poller.PreviewEntry{})
		return
	}
	entries, err := h.previewer.Preview(r.Context())
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []poller.PreviewEntry{}
	}
	jsonOK(w, entries)
}

// buildPreviewPageData converts raw PreviewEntry slice into the grouped view model.
func buildPreviewPageData(entries []poller.PreviewEntry) previewPageData {
	data := previewPageData{}
	for _, e := range entries {
		row := previewRow{
			Title:      e.Title,
			Source:     e.Source,
			SourcePath: e.SourcePath,
			Type:       string(e.Classified),
			DstRoot:    e.DstRoot,
			Reason:     e.Reason,
			Note:       e.Note,
		}
		// Count chapters from chapter plans.
		var nFile, nSkip int
		for _, p := range e.ChapterPlans {
			switch p.Action {
			case "file":
				nFile++
			case "skip":
				nSkip++
			}
		}
		row.ChapterCount = len(e.ChapterPlans)
		if len(e.ChapterPlans) > 0 {
			row.FileSummary = buildFileSummary(nFile, nSkip, len(e.ChapterPlans)-nFile-nSkip)
		}
		switch e.Status {
		case "matched":
			data.Matched = append(data.Matched, row)
		case "unmatched":
			data.Unmatched = append(data.Unmatched, row)
		default: // "misconfigured"
			data.Misconfigured = append(data.Misconfigured, row)
		}
	}
	return data
}

// buildFileSummary produces a compact summary like "3 file, 5 skip" or "all skip".
func buildFileSummary(nFile, nSkip, nErr int) string {
	var parts []string
	if nFile > 0 {
		parts = append(parts, fmt.Sprintf("%d file", nFile))
	}
	if nSkip > 0 {
		parts = append(parts, fmt.Sprintf("%d skip", nSkip))
	}
	if nErr > 0 {
		parts = append(parts, fmt.Sprintf("%d error", nErr))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

func (h *Handler) pageUnmatched(w http.ResponseWriter, r *http.Request) {
	list, err := h.store.ListUnmatched()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.render(w, "unmatched.html", pageData{Page: "unmatched", Items: list})
}

func (h *Handler) pageActivity(w http.ResponseWriter, r *http.Request) {
	list, err := h.store.ListActivity(200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Resolve Via labels (suwayomi-override:category=N → category name).
	// We fetch the Suwayomi category list once for the entire page render
	// so per-row resolution is O(1). Failure to reach Suwayomi falls back
	// to "Unknown (ID: N)" — the override still rendered through Plan B,
	// so the user just sees a degraded label, not an error page.
	rows := buildActivityRows(r.Context(), h.store, list)
	h.render(w, "activity.html", activityPageData{Page: "activity", Items: rows})
}

// activityRow is the view-model for one ActivityEntry, with Via pre-resolved
// to a human-readable label.
type activityRow struct {
	model.ActivityEntry
	ViaLabel string
}

// activityPageData is passed to activity.html.
type activityPageData struct {
	Page  string
	Items []activityRow
}

// buildActivityRows resolves the Via field of every entry to a display label.
// suwayomi-override:category=N is joined against the current Suwayomi
// category list; missing IDs render as "Unknown (ID: N)". anilist:JP/KR/CN/TW
// render as "AniList (XX)". unmatched/empty are handled in a switch.
//
// The Suwayomi category list is fetched at most once per page render — when
// at least one row carries a suwayomi-override Via.
func buildActivityRows(ctx context.Context, st Store, list []model.ActivityEntry) []activityRow {
	rows := make([]activityRow, 0, len(list))
	var catNamesByID map[int64]string
	needSuwayomi := false
	for _, e := range list {
		if strings.HasPrefix(e.Via, classifier.ViaSuwayomiOverridePrefix) {
			needSuwayomi = true
			break
		}
	}
	if needSuwayomi {
		settings, err := st.GetSettings()
		if err == nil {
			if client, ok := newSuwayomiClient(settings); ok {
				fetchCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				cats, err := client.ListCategories(fetchCtx)
				cancel()
				if err == nil {
					catNamesByID = make(map[int64]string, len(cats))
					for _, c := range cats {
						catNamesByID[c.ID] = c.Name
					}
				}
			}
		}
	}
	for _, e := range list {
		rows = append(rows, activityRow{
			ActivityEntry: e,
			ViaLabel:      formatVia(e.Via, catNamesByID),
		})
	}
	return rows
}

// formatVia renders a raw Via value into a human-readable label.
//
//	"anilist:JP"                  → "AniList (JP)"
//	"suwayomi-override:category=5" → "<categoryName>" or "Unknown (ID: 5)"
//	"unmatched"                   → "Unmatched"
//	""                            → "—" (pre-Plan-B legacy rows)
func formatVia(via string, catNamesByID map[int64]string) string {
	switch {
	case via == "":
		return "—"
	case via == classifier.ViaUnmatched:
		return "Unmatched"
	case strings.HasPrefix(via, classifier.ViaAniListPrefix):
		code := strings.TrimPrefix(via, classifier.ViaAniListPrefix)
		if code == "" {
			return "AniList"
		}
		return "AniList (" + code + ")"
	case strings.HasPrefix(via, classifier.ViaSuwayomiOverridePrefix):
		rawID := strings.TrimPrefix(via, classifier.ViaSuwayomiOverridePrefix)
		id, err := strconv.ParseInt(rawID, 10, 64)
		if err != nil {
			return via // surface raw on unparseable
		}
		if name, ok := catNamesByID[id]; ok && name != "" {
			return name
		}
		return fmt.Sprintf("Unknown (ID: %d)", id)
	default:
		return via
	}
}

func (h *Handler) pageTasks(w http.ResponseWriter, r *http.Request) {
	var rows []taskRow
	if h.taskReg != nil {
		for _, info := range h.taskReg.List() {
			rows = append(rows, buildTaskRow(info))
		}
	}
	h.render(w, "tasks.html", tasksPageData{Page: "tasks", Items: rows})
}

func buildTaskRow(info tasks.Info) taskRow {
	rc := "pending"
	if info.LastErr != "" {
		rc = "error"
	} else if !info.LastRun.IsZero() {
		rc = "success"
	}
	return taskRow{
		Info:          info,
		IntervalLabel: formatInterval(info.IntervalMs),
		LastRunLabel:  formatAge(time.Now(), info.LastRun),
		ResultClass:   rc,
	}
}

func (h *Handler) pageHealth(w http.ResponseWriter, r *http.Request) {
	if h.healthReg == nil {
		h.render(w, "health.html", healthPageData{
			Page:         "health",
			Unregistered: true,
		})
		return
	}
	results := h.healthReg.RunAll(r.Context())
	rows := make([]healthRow, 0, len(results))
	for _, res := range results {
		rows = append(rows, healthRow{
			Result:      res,
			StatusClass: healthStatusClass(res.Status),
		})
	}
	h.render(w, "health.html", healthPageData{
		Page:          "health",
		Items:         rows,
		OverallStatus: health.WorstStatus(results),
	})
}

// healthStatusClass maps a health.Status to the CSS pill class.
func healthStatusClass(s health.Status) string {
	switch s {
	case health.StatusOK:
		return "success"
	case health.StatusWarn:
		return "warn"
	default:
		return "error"
	}
}

func (h *Handler) pageSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := h.store.GetSettings()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if settings.LibraryRoots == nil {
		settings.LibraryRoots = map[model.ContentType]string{}
	}
	if settings.KavitaLibIDsByType == nil {
		settings.KavitaLibIDsByType = map[model.ContentType]int64{}
	}
	flash := r.URL.Query().Get("saved")
	flashMsg := ""
	if flash == "1" {
		flashMsg = "Settings saved."
	}

	// Build disk-space rows: download roots + library roots, grouped by filesystem.
	diskRows := h.buildDiskRows(settings)

	// Load backup list — treat a missing dir as empty, not an error.
	now := time.Now()
	entries, _ := dbbackup.List(h.backupDir) // error → nil slice → empty view
	views := make([]backupEntryView, 0, len(entries))
	for _, e := range entries {
		views = append(views, backupEntryView{
			Entry:     e,
			SizeHuman: formatBytes(e.SizeBytes),
			AgeHuman:  formatAge(now, e.ModTime),
		})
	}

	// Try fetching Kavita libraries for the select dropdowns if Kavita is configured.
	// We build a fresh client from CURRENT settings each call so URL/key changes
	// take effect immediately (no restart needed).
	var kavitaLibs []kavita.Library
	var kavitaLibErr string
	if settings.KavitaBaseURL != "" && settings.KavitaAPIKey != "" {
		fetchCtx, fetchCancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer fetchCancel()
		client := kavita.New(settings.KavitaBaseURL, settings.KavitaAPIKey)
		if libs, err := client.ListLibraries(fetchCtx); err != nil {
			kavitaLibErr = err.Error()
		} else {
			kavitaLibs = libs
		}
	}

	// --- Library Bindings v2 (Plan B) ---
	// Best-effort: a store error renders the card with an empty list and
	// the same "+ Add Binding" affordance so the user can still create
	// one. We deliberately don't fail the whole page on a binding load
	// error — the v1 sections below it must still be reachable until
	// Task 8 removes them.
	bindings, err := h.store.ListBindings()
	if err != nil {
		bindings = nil
	}
	// Classification rules — best-effort, same rationale as bindings above.
	rules, err := h.store.ListRules()
	if err != nil {
		rules = nil
	}

	// --- Library Map: Suwayomi + Override rows ---
	// Both /settings and the override-fragment HTMX endpoint route
	// through buildLibraryMapData → identical rendering on initial GET
	// and after the user clicks Refresh. The v2 override-row dropdown
	// lists every binding, so we pass them through here.
	lmCtx, lmCancel := context.WithTimeout(r.Context(), 3*time.Second)
	libraryMap := buildLibraryMapData(lmCtx, settings, bindings)
	lmCancel()

	// Pre-extract values typed-keyed by model.ContentType into plain fields,
	// so the template can use {{.RootManga}} etc. with no reflection-time
	// type mismatch. See settingsPageData doc comment.
	h.render(w, "settings.html", settingsPageData{
		Page:                    "settings",
		Settings:                settings,
		KavitaAPIKey:            settings.KavitaAPIKey,
		Flash:                   flashMsg,
		DownloadRoots:           settings.DownloadRoots,
		RootManga:               settings.LibraryRoots[model.TypeManga],
		RootManhwa:              settings.LibraryRoots[model.TypeManhwa],
		RootManhua:              settings.LibraryRoots[model.TypeManhua],
		KavitaLibManga:          settings.KavitaLibIDsByType[model.TypeManga],
		KavitaLibManhwa:         settings.KavitaLibIDsByType[model.TypeManhwa],
		KavitaLibManhua:         settings.KavitaLibIDsByType[model.TypeManhua],
		KavitaLibraries:         kavitaLibs,
		KavitaLibError:          kavitaLibErr,
		Bindings:                bindings,
		Rules:                   rules,
		SuwayomiBaseURL:         settings.SuwayomiBaseURL,
		SuwayomiAuthType:        settings.SuwayomiAuthType,
		SuwayomiUsername:        settings.SuwayomiUsername,
		SuwayomiPassword:        settings.SuwayomiPassword,
		LibraryMap:              libraryMap,
		RenameExample:           renameExample(settings.RenameScheme),
		DiskRows:                diskRows,
		RecycleBinPath:          h.recycleBinPath,
		RecycleBinRetentionDays: h.recycleBinRetentionDays,
		BackupDir:               h.backupCfg.Dir,
		BackupRetentionDays:     h.backupCfg.RetentionDays,
		BackupIntervalHours:     h.backupCfg.IntervalHours,
		Backups:                 views,
		BackupNow:               now,
	})
}

// buildDiskRows gathers disk-space rows for the Settings page.
// Download roots come from settings.DownloadRoots (UI-managed).
// Library roots come from settings.LibraryRoots.
// Paths that share the same filesystem (same FSID) are grouped into one row.
func (h *Handler) buildDiskRows(settings model.Settings) []fsDiskRow {
	type pathSpec struct {
		label string
		path  string
	}
	var specs []pathSpec

	// Download roots first.
	for _, p := range settings.DownloadRoots {
		if p != "" {
			specs = append(specs, pathSpec{"Download root", p})
		}
	}

	// Library roots.
	libLabels := []struct {
		ct    model.ContentType
		label string
	}{
		{model.TypeManga, "Manga library"},
		{model.TypeManhwa, "Manhwa library"},
		{model.TypeManhua, "Manhua library"},
	}
	for _, ll := range libLabels {
		if p := settings.LibraryRoots[ll.ct]; p != "" {
			specs = append(specs, pathSpec{ll.label, p})
		}
	}

	// Deduplicate exact label+path combos before stat'ing.
	seenKey := map[string]bool{}
	var unique []pathSpec
	for _, spec := range specs {
		k := spec.label + "|" + spec.path
		if !seenKey[k] {
			seenKey[k] = true
			unique = append(unique, spec)
		}
	}

	// Stat each unique path and group by FSID.
	// Errored paths get their own row (FSID is zero — isolate by path as key).
	type fsKey struct {
		fsid [2]int32
		errPath string // non-empty for error rows; prevents collapsing errors
	}
	type fsGroup struct {
		info  diskspace.Info
		paths []diskPathEntry
	}
	var order []fsKey
	groups := map[fsKey]*fsGroup{}

	for _, spec := range unique {
		info := diskspace.Stat(spec.path)
		var key fsKey
		if info.Err != nil {
			key = fsKey{errPath: spec.path}
		} else {
			key = fsKey{fsid: info.FSID}
		}
		if _, exists := groups[key]; !exists {
			order = append(order, key)
			groups[key] = &fsGroup{info: info}
		}
		groups[key].paths = append(groups[key].paths, diskPathEntry{
			Label: spec.label,
			Path:  spec.path,
		})
	}

	// Convert groups to fsDiskRow slice.
	rows := make([]fsDiskRow, 0, len(order))
	for _, key := range order {
		g := groups[key]
		// Derive a friendly display label by inspecting the filesystem source.
		// For NFS mounts that gives us the server's short hostname (e.g.
		// "Citadel" from "citadel.internal:/mnt/storage0/media"); for local
		// mounts it returns the mount point itself.
		anyPath := g.paths[0].Path
		mountLabel := diskspace.SourceLabel(anyPath)
		if mountLabel == "" || mountLabel == anyPath {
			mountLabel = commonPathPrefix(g.paths)
		}

		if g.info.Err != nil {
			rows = append(rows, fsDiskRow{
				MountLabel: mountLabel,
				Paths:      g.paths,
				Err:        "unavailable",
				BarClass:   "bar-err",
			})
			continue
		}
		pctUsed := 100.0 - g.info.PercentFree()
		if pctUsed < 0 {
			pctUsed = 0
		}
		pctUsedInt := int(pctUsed)
		rows = append(rows, fsDiskRow{
			MountLabel:     mountLabel,
			Paths:          g.paths,
			Free:           diskspace.FormatBytes(g.info.FreeBytes),
			Total:          diskspace.FormatBytes(g.info.TotalBytes),
			PercentFmt:     fmt.Sprintf("%d", pctUsedInt),
			PercentUsedFmt: fmt.Sprintf("%d%% used", pctUsedInt),
			BarClass:       diskSpaceClass(pctUsed),
		})
	}
	return rows
}

// commonPathPrefix returns the longest directory prefix shared by all paths
// in the group. If there is only one path, or no common ancestor exists beyond
// the filesystem root, it returns the first path's directory or the path itself.
func commonPathPrefix(paths []diskPathEntry) string {
	if len(paths) == 0 {
		return ""
	}
	if len(paths) == 1 {
		return filepath.Dir(paths[0].Path)
	}
	// Walk path segments of the first path and keep those that match all others.
	parts := strings.Split(filepath.Clean(paths[0].Path), string(filepath.Separator))
	for _, entry := range paths[1:] {
		other := strings.Split(filepath.Clean(entry.Path), string(filepath.Separator))
		n := len(parts)
		if len(other) < n {
			n = len(other)
		}
		for i := 0; i < n; i++ {
			if parts[i] != other[i] {
				parts = parts[:i]
				break
			}
		}
		if len(other) < len(parts) {
			parts = parts[:len(other)]
		}
	}
	if len(parts) == 0 {
		return "/"
	}
	result := strings.Join(parts, string(filepath.Separator))
	if result == "" {
		result = "/"
	}
	return result
}

func (h *Handler) saveSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}

	settings, err := h.store.GetSettings()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Read download_root[] — repeated field name; empty values dropped.
	var downloadRoots []string
	for _, v := range r.Form["download_root"] {
		if v = strings.TrimSpace(v); v != "" {
			downloadRoots = append(downloadRoots, v)
		}
	}
	settings.DownloadRoots = downloadRoots

	settings.FileMode = model.FileMode(r.FormValue("file_mode"))
	settings.RenameScheme = r.FormValue("rename_scheme")
	if pm, err := strconv.Atoi(r.FormValue("poll_minutes")); err == nil && pm > 0 {
		settings.PollMinutes = pm
	}
	settings.KavitaBaseURL = strings.TrimRight(r.FormValue("kavita_base_url"), "/")
	settings.KavitaAPIKey = r.FormValue("kavita_api_key")

	if settings.LibraryRoots == nil {
		settings.LibraryRoots = map[model.ContentType]string{}
	}
	setIfNonEmpty(settings.LibraryRoots, model.TypeManga, r.FormValue("root_manga"))
	setIfNonEmpty(settings.LibraryRoots, model.TypeManhwa, r.FormValue("root_manhwa"))
	setIfNonEmpty(settings.LibraryRoots, model.TypeManhua, r.FormValue("root_manhua"))

	if settings.KavitaLibIDsByType == nil {
		settings.KavitaLibIDsByType = map[model.ContentType]int64{}
	}
	setLibID(settings.KavitaLibIDsByType, model.TypeManga, r.FormValue("kavita_lib_manga"))
	setLibID(settings.KavitaLibIDsByType, model.TypeManhwa, r.FormValue("kavita_lib_manhwa"))
	setLibID(settings.KavitaLibIDsByType, model.TypeManhua, r.FormValue("kavita_lib_manhua"))

	// --- Suwayomi connection + category overrides ---
	settings.SuwayomiBaseURL = strings.TrimRight(strings.TrimSpace(r.FormValue("suwayomi_base_url")), "/")
	switch model.SuwayomiAuthType(r.FormValue("suwayomi_auth_type")) {
	case model.SuwayomiAuthBasic:
		settings.SuwayomiAuthType = model.SuwayomiAuthBasic
	case model.SuwayomiAuthSimple:
		settings.SuwayomiAuthType = model.SuwayomiAuthSimple
	case model.SuwayomiAuthUI:
		settings.SuwayomiAuthType = model.SuwayomiAuthUI
	default:
		settings.SuwayomiAuthType = model.SuwayomiAuthNone
	}
	settings.SuwayomiUsername = strings.TrimSpace(r.FormValue("suwayomi_username"))
	// Password kept as-is; trimming would silently break credentials with a
	// leading/trailing space.
	settings.SuwayomiPassword = r.FormValue("suwayomi_password")

	// Parse override rows. Form fields come as override_category_<idx> +
	// override_binding_<idx> pairs (idx is the JS counter, not stable).
	// Walk r.Form and pick out matching pairs. Plan B v2 renamed the
	// right-hand field; Task 5 will migrate the persisted target from
	// SuwayomiCategoryOverrides to SuwayomiCategoryBindings.
	settings.SuwayomiCategoryOverrides = parseSuwayomiOverrides(r.Form)

	// Validate the rename scheme before persisting. On failure, re-render the
	// Settings page with the error shown inline and all form values preserved
	// so the user does not lose in-progress edits.
	if err := filer.ValidateScheme(settings.RenameScheme); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		h.render(w, "settings.html", settingsPageData{
			Page:            "settings",
			Settings:        settings,
			KavitaAPIKey:    settings.KavitaAPIKey,
			Error:           "Invalid rename scheme: " + err.Error(),
			DownloadRoots:   settings.DownloadRoots,
			RootManga:       settings.LibraryRoots[model.TypeManga],
			RootManhwa:      settings.LibraryRoots[model.TypeManhwa],
			RootManhua:      settings.LibraryRoots[model.TypeManhua],
			KavitaLibManga:  settings.KavitaLibIDsByType[model.TypeManga],
			KavitaLibManhwa: settings.KavitaLibIDsByType[model.TypeManhwa],
			KavitaLibManhua: settings.KavitaLibIDsByType[model.TypeManhua],
			RenameExample:   renameExample(settings.RenameScheme),
			// Kavita libraries not re-fetched on validation error — user will see
			// placeholder selects, which is acceptable since this is an error path.
		})
		return
	}

	if err := h.store.SaveSettings(settings); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}

// ---- JSON API handlers ----

func (h *Handler) apiListSeries(w http.ResponseWriter, r *http.Request) {
	list, err := h.store.ListSeries()
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, list)
}

func (h *Handler) apiListUnmatched(w http.ResponseWriter, r *http.Request) {
	list, err := h.store.ListUnmatched()
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, list)
}

func (h *Handler) apiListActivity(w http.ResponseWriter, r *http.Request) {
	limitStr := r.URL.Query().Get("limit")
	limit := 100
	if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
		limit = n
	}
	list, err := h.store.ListActivity(limit)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, list)
}

// apiHealthResponse is the JSON shape for GET /api/health.
type apiHealthResponse struct {
	Status  health.Status  `json:"status"`
	Results []health.Result `json:"results"`
}

func (h *Handler) apiHealth(w http.ResponseWriter, r *http.Request) {
	if h.healthReg == nil {
		// Return a single warning result when the registry isn't wired.
		resp := apiHealthResponse{
			Status: health.StatusWarn,
			Results: []health.Result{
				{
					ID:          "registry",
					Name:        "Health registry",
					Status:      health.StatusWarn,
					Message:     "Health checks not wired.",
					Remediation: "Ensure a HealthRegistry is passed to the web handler.",
				},
			},
		}
		jsonOK(w, resp)
		return
	}
	results := h.healthReg.RunAll(r.Context())
	jsonOK(w, apiHealthResponse{
		Status:  health.WorstStatus(results),
		Results: results,
	})
}

func (h *Handler) apiListTasks(w http.ResponseWriter, r *http.Request) {
	if h.taskReg == nil {
		jsonErr(w, fmt.Errorf("task registry not configured"), http.StatusServiceUnavailable)
		return
	}
	jsonOK(w, h.taskReg.List())
}

func (h *Handler) apiRunTask(w http.ResponseWriter, r *http.Request) {
	if h.taskReg == nil {
		jsonErr(w, fmt.Errorf("task registry not configured"), http.StatusServiceUnavailable)
		return
	}
	id := r.PathValue("id")
	info, ok := h.taskReg.Get(id)
	if !ok {
		jsonErr(w, fmt.Errorf("task not found: %s", id), http.StatusNotFound)
		return
	}
	if info.Running {
		jsonErr(w, fmt.Errorf("task %s is already running", id), http.StatusConflict)
		return
	}
	updated, _ := h.taskReg.RunNow(r.Context(), id)
	jsonOK(w, updated)
}

func (h *Handler) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := h.store.GetSettings()
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, settings)
}

func (h *Handler) apiPutSettings(w http.ResponseWriter, r *http.Request) {
	var settings model.Settings
	if err := json.NewDecoder(r.Body).Decode(&settings); err != nil {
		jsonErr(w, fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest)
		return
	}
	if err := filer.ValidateScheme(settings.RenameScheme); err != nil {
		jsonErr(w, fmt.Errorf("invalid rename scheme: %w", err), http.StatusBadRequest)
		return
	}
	if err := h.store.SaveSettings(settings); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) apiRescan(w http.ResponseWriter, r *http.Request) {
	// Prefer the task registry path so LastRun stays accurate in the Tasks UI.
	if h.taskReg != nil {
		_, err := h.taskReg.RunNow(r.Context(), "poll-scan")
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Fallback: direct runner (tests or minimal setups without a registry).
	if h.runner == nil {
		jsonErr(w, fmt.Errorf("poller not configured"), http.StatusServiceUnavailable)
		return
	}
	if err := h.runner.RunOnce(r.Context()); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// diskSpaceAPIEntry is the JSON shape for GET /api/diskspace entries.
type diskSpaceAPIEntry struct {
	Path        string  `json:"path"`
	TotalBytes  uint64  `json:"total_bytes"`
	FreeBytes   uint64  `json:"free_bytes"`
	PercentFree float64 `json:"percent_free"`
	Error       string  `json:"error,omitempty"`
}

func (h *Handler) apiDiskSpace(w http.ResponseWriter, r *http.Request) {
	settings, err := h.store.GetSettings()
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if settings.LibraryRoots == nil {
		settings.LibraryRoots = map[model.ContentType]string{}
	}

	// Collect all unique paths: Settings.DownloadRoots + configured library roots.
	seen := map[string]bool{}
	var paths []string
	for _, p := range settings.DownloadRoots {
		if p != "" && !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	for _, ct := range []model.ContentType{model.TypeManga, model.TypeManhwa, model.TypeManhua} {
		if p := settings.LibraryRoots[ct]; p != "" && !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}

	result := make([]diskSpaceAPIEntry, 0, len(paths))
	for _, p := range paths {
		info := diskspace.Stat(p)
		entry := diskSpaceAPIEntry{
			Path:        p,
			TotalBytes:  info.TotalBytes,
			FreeBytes:   info.FreeBytes,
			PercentFree: info.PercentFree(),
		}
		if info.Err != nil {
			entry.Error = info.Err.Error()
		}
		result = append(result, entry)
	}
	jsonOK(w, result)
}

func (h *Handler) apiReclassify(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	typeStr := r.FormValue("type")
	ct := model.ContentType(typeStr)
	if ct != model.TypeManga && ct != model.TypeManhwa && ct != model.TypeManhua {
		http.Error(w, "invalid type", http.StatusBadRequest)
		return
	}

	if err := h.store.SetSeriesType(id, ct); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Return an updated row fragment for HTMX swap (series page uses outerHTML swap).
	// We fetch fresh data so the rendered row reflects the change.
	list, err := h.store.ListSeries()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var updated *model.Series
	for i := range list {
		if list[i].ID == id {
			updated = &list[i]
			break
		}
	}
	if updated == nil {
		// Series no longer exists; return empty row so HTMX removes it.
		w.WriteHeader(http.StatusOK)
		return
	}

	// Render the row fragment inline (not a full page).
	row := fmt.Sprintf(`<tr>
  <td>%s</td>
  <td><span class="pill pill-%s">%s</span></td>
  <td class="td-dim">%s</td>
  <td class="td-dim">%d</td>
  <td><span class="pill pill-%s">%s</span></td>
  <td class="td-right">
    <form class="reclassify-form"
      hx-post="/api/series/%d/reclassify"
      hx-target="closest tr"
      hx-swap="outerHTML">
      <select name="type" class="reclassify-select">
        <option value="Manga"%s>Manga</option>
        <option value="Manhwa"%s>Manhwa</option>
        <option value="Manhua"%s>Manhua</option>
      </select>
      <button type="submit" class="btn-sm">Set</button>
    </form>
  </td>
</tr>`,
		html(updated.Title),
		strings.ToLower(string(updated.Type)), html(string(updated.Type)),
		html(updated.Source),
		updated.ChapterCount,
		html(string(updated.Status)), html(string(updated.Status)),
		updated.ID,
		selectedIf(updated.Type == model.TypeManga),
		selectedIf(updated.Type == model.TypeManhwa),
		selectedIf(updated.Type == model.TypeManhua),
	)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, row)
}

// apiAssign handles POST /api/series/{id}/assign.
//
// It classifies the series to the given type, writes the override to the
// classification cache, files the series immediately, and triggers a Kavita
// scan — all in one click from the Unmatched page.
//
// On success it returns an empty 200 body: the HTMX target row is swapped out
// (deleted) by the caller's hx-swap="outerHTML" pointing at an empty response,
// which removes the row from the Unmatched table without a full page reload.
//
// Returns 503 when the SeriesFiler is not configured (minimal test setups).
// Returns 400 on invalid id or unknown type.
func (h *Handler) apiAssign(w http.ResponseWriter, r *http.Request) {
	if h.seriesFiler == nil {
		http.Error(w, "series filer not configured", http.StatusServiceUnavailable)
		return
	}

	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	typeStr := r.FormValue("type")
	ct := model.ContentType(typeStr)
	if ct != model.TypeManga && ct != model.TypeManhwa && ct != model.TypeManhua {
		http.Error(w, "invalid type", http.StatusBadRequest)
		return
	}

	if err := h.seriesFiler.FileOne(r.Context(), id, ct); err != nil {
		// Surface as 404 if the series doesn't exist, else 500.
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "series not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Return empty body — the HTMX hx-swap="outerHTML" replaces the row with
	// nothing, removing it from the Unmatched table.
	w.WriteHeader(http.StatusOK)
}

// ---- backup API handlers ----

func (h *Handler) apiListBackups(w http.ResponseWriter, r *http.Request) {
	entries, err := dbbackup.List(h.backupDir)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, entries)
}

func (h *Handler) apiRunBackup(w http.ResponseWriter, r *http.Request) {
	if h.backupFn == nil {
		jsonErr(w, fmt.Errorf("backup not configured"), http.StatusServiceUnavailable)
		return
	}
	entry, err := h.backupFn()
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, entry)
}

func (h *Handler) apiDownloadBackup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !dbbackup.ValidateName(name) {
		http.Error(w, "invalid backup name", http.StatusBadRequest)
		return
	}
	// Extra defence: reject path separators even after ValidateName.
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		http.Error(w, "invalid backup name", http.StatusBadRequest)
		return
	}
	if h.backupDir == "" {
		http.Error(w, "backup not configured", http.StatusServiceUnavailable)
		return
	}
	path := h.backupDir + "/" + name
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// ---- path-browser API ----

// browseEntry is a single entry in the /api/browse JSON response.
type browseEntry struct {
	Name string `json:"name"`
	Type string `json:"type"` // always "dir"
	Path string `json:"path"`
}

// browseResponse is the JSON shape returned by GET /api/browse.
type browseResponse struct {
	Path    string        `json:"path"`
	Parent  string        `json:"parent"`
	Entries []browseEntry `json:"entries"`
}

// resolveBrowsePath validates and resolves the requested path against the
// configured allowlist. It returns the cleaned absolute path, or an empty
// string and an error string to send to the caller (403/400 as appropriate).
// An empty or missing path is treated as the synthetic "root view" signal
// by returning ("", "") — callers should check for both being empty.
func (h *Handler) resolveBrowsePath(rawPath string) (resolved string, errMsg string, synthetic bool) {
	if rawPath == "" {
		return "", "", true // synthetic root view
	}
	cleaned := filepath.Clean(filepath.FromSlash(rawPath))
	abs, err := filepath.Abs(cleaned)
	if err != nil {
		return "", "invalid path", false
	}
	for _, root := range h.browseRoots {
		rootAbs, err := filepath.Abs(filepath.Clean(root))
		if err != nil {
			continue
		}
		// Accept the root itself or anything strictly inside it.
		if abs == rootAbs || strings.HasPrefix(abs, rootAbs+string(filepath.Separator)) {
			return abs, "", false
		}
	}
	return "", "forbidden", false
}

// listDirs returns sorted directory entries under dir.
func listDirs(dir string) ([]browseEntry, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	infos, err := f.Readdir(-1)
	if err != nil {
		return nil, err
	}
	var entries []browseEntry
	for _, info := range infos {
		if info.IsDir() {
			name := info.Name()
			entries = append(entries, browseEntry{
				Name: name,
				Type: "dir",
				Path: filepath.Join(dir, name),
			})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return entries, nil
}

// browseSyntheticRoot returns the synthetic root listing (all allowlist roots).
func (h *Handler) browseSyntheticRoot() browseResponse {
	entries := make([]browseEntry, 0, len(h.browseRoots))
	for _, root := range h.browseRoots {
		abs, err := filepath.Abs(filepath.Clean(root))
		if err != nil {
			abs = root
		}
		entries = append(entries, browseEntry{
			Name: abs,
			Type: "dir",
			Path: abs,
		})
	}
	return browseResponse{Path: "", Parent: "", Entries: entries}
}

// browseParent returns the parent path, or "" if the path equals one of the
// allowlist roots (can't navigate above a root).
func (h *Handler) browseParent(abs string) string {
	for _, root := range h.browseRoots {
		rootAbs, err := filepath.Abs(filepath.Clean(root))
		if err != nil {
			continue
		}
		if abs == rootAbs {
			return "" // at a root — no parent to navigate to
		}
	}
	return filepath.Dir(abs)
}

func (h *Handler) apiBrowse(w http.ResponseWriter, r *http.Request) {
	rawPath := r.URL.Query().Get("path")
	abs, errMsg, synthetic := h.resolveBrowsePath(rawPath)
	if errMsg == "forbidden" {
		http.Error(w, "path outside allowed roots", http.StatusForbidden)
		return
	}
	if errMsg != "" {
		http.Error(w, errMsg, http.StatusBadRequest)
		return
	}
	if synthetic {
		jsonOK(w, h.browseSyntheticRoot())
		return
	}
	entries, err := listDirs(abs)
	if err != nil {
		http.Error(w, "cannot read directory", http.StatusInternalServerError)
		return
	}
	jsonOK(w, browseResponse{
		Path:    abs,
		Parent:  h.browseParent(abs),
		Entries: entries,
	})
}

// apiBrowseFragment renders the HTMX HTML fragment for the path-browser modal.
// Both "forbidden" and error cases return HTTP 200 with a friendly HTML message so
// HTMX always swaps the fragment and the user sees a readable error in the modal.
func (h *Handler) apiBrowseFragment(w http.ResponseWriter, r *http.Request) {
	rawPath := r.URL.Query().Get("path")
	target := r.URL.Query().Get("target") // e.g. "root_manga"
	abs, errMsg, synthetic := h.resolveBrowsePath(rawPath)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if errMsg == "forbidden" {
		fmt.Fprintf(w,
			`<div class="browse-fragment"><div class="browse-error">This path is outside the allowed filesystem roots.</div>`+
				`<div class="browse-actions"><button type="button" class="btn" `+
				`hx-get="/api/browse/fragment?target=%s" hx-target="#browse-modal-body" hx-swap="innerHTML">Back to root</button>`+
				`<button type="button" class="btn browse-cancel-btn" `+
				`onclick="var w=document.getElementById('browse-modal');w.classList.remove('browse-modal-open');document.getElementById('browse-modal-body').innerHTML=''">Cancel</button>`+
				`</div></div>`,
			html(target),
		)
		return
	}
	if errMsg != "" {
		fmt.Fprintf(w, `<div class="browse-fragment"><div class="browse-error">%s</div></div>`, html(errMsg))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if synthetic {
		// Render the allowlist roots as the top-level listing.
		fmt.Fprint(w, `<div class="browse-fragment">`)
		fmt.Fprint(w, `<div class="browse-breadcrumbs"><span class="browse-crumb-sep">/</span></div>`)
		fmt.Fprint(w, `<ul class="browse-dir-list">`)
		for _, root := range h.browseRoots {
			rootAbs, err := filepath.Abs(filepath.Clean(root))
			if err != nil {
				rootAbs = root
			}
			fmt.Fprintf(w,
				`<li class="browse-dir-item"><button type="button" class="browse-dir-btn" `+
					`hx-get="/api/browse/fragment?path=%s&amp;target=%s" `+
					`hx-target="#browse-modal-body" hx-swap="innerHTML">%s</button></li>`,
				html(rootAbs), html(target), html(rootAbs),
			)
		}
		fmt.Fprint(w, `</ul>`)
		fmt.Fprint(w, `<div class="browse-actions">`)
		fmt.Fprint(w,
			`<button type="button" class="btn browse-cancel-btn" `+
				`onclick="var w=document.getElementById('browse-modal');w.classList.remove('browse-modal-open');document.getElementById('browse-modal-body').innerHTML=''">Cancel</button>`,
		)
		fmt.Fprint(w, `</div></div>`)
		return
	}
	// Build breadcrumbs from path segments relative to the allowlist root.
	crumbs := buildBreadcrumbs(abs, h.browseRoots)
	entries, err := listDirs(abs)
	if err != nil {
		fmt.Fprint(w, `<div class="browse-error">Cannot read directory.</div>`)
		return
	}
	parent := h.browseParent(abs)
	fmt.Fprint(w, `<div class="browse-fragment">`)
	// Breadcrumbs
	fmt.Fprint(w, `<div class="browse-breadcrumbs">`)
	for i, crumb := range crumbs {
		if i < len(crumbs)-1 {
			// Clickable ancestor
			fmt.Fprintf(w,
				`<button type="button" class="browse-crumb-btn" `+
					`hx-get="/api/browse/fragment?path=%s&amp;target=%s" `+
					`hx-target="#browse-modal-body" hx-swap="innerHTML">%s</button>`+
					`<span class="browse-crumb-sep">/</span>`,
				html(crumb.Path), html(target), html(crumb.Label),
			)
		} else {
			fmt.Fprintf(w, `<span class="browse-crumb-current">%s</span>`, html(crumb.Label))
		}
	}
	fmt.Fprint(w, `</div>`)
	// Directory list
	fmt.Fprint(w, `<ul class="browse-dir-list">`)
	if parent != "" {
		fmt.Fprintf(w,
			`<li class="browse-dir-item browse-dir-up"><button type="button" class="browse-dir-btn" `+
				`hx-get="/api/browse/fragment?path=%s&amp;target=%s" `+
				`hx-target="#browse-modal-body" hx-swap="innerHTML">..</button></li>`,
			html(parent), html(target),
		)
	}
	for _, e := range entries {
		fmt.Fprintf(w,
			`<li class="browse-dir-item"><button type="button" class="browse-dir-btn" `+
				`hx-get="/api/browse/fragment?path=%s&amp;target=%s" `+
				`hx-target="#browse-modal-body" hx-swap="innerHTML">%s</button></li>`,
			html(e.Path), html(target), html(e.Name),
		)
	}
	fmt.Fprint(w, `</ul>`)
	// Footer actions
	fmt.Fprint(w, `<div class="browse-actions">`)
	fmt.Fprintf(w,
		`<button type="button" class="btn btn-primary browse-select-btn" `+
			`onclick="document.getElementById('%s').value='%s';var w=document.getElementById('browse-modal');w.classList.remove('browse-modal-open');document.getElementById('browse-modal-body').innerHTML=''">Select this folder</button>`,
		html(target), html(abs),
	)
	fmt.Fprint(w,
		`<button type="button" class="btn browse-cancel-btn" `+
			`onclick="var w=document.getElementById('browse-modal');w.classList.remove('browse-modal-open');document.getElementById('browse-modal-body').innerHTML=''">Cancel</button>`,
	)
	fmt.Fprint(w, `</div></div>`)
}

// ---- Kavita library picker API ----

// kavitaLibrariesResponse is the JSON shape for GET /api/kavita/libraries.
type kavitaLibrariesResponse struct {
	Libraries []kavita.Library `json:"libraries"`
}

// apiKavitaLibraries handles GET /api/kavita/libraries.
// Returns JSON {libraries:[{id,name,type},...]} or {error:"..."} + appropriate status.
// Builds a fresh kavita.Client from current Settings so URL/key changes apply
// immediately without requiring a restart.
func (h *Handler) apiKavitaLibraries(w http.ResponseWriter, r *http.Request) {
	settings, err := h.store.GetSettings()
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if settings.KavitaBaseURL == "" || settings.KavitaAPIKey == "" {
		jsonErr(w, fmt.Errorf("kavita base URL and API key not configured"), http.StatusServiceUnavailable)
		return
	}
	client := kavita.New(settings.KavitaBaseURL, settings.KavitaAPIKey)
	libs, err := client.ListLibraries(r.Context())
	if err != nil {
		jsonErr(w, err, http.StatusBadGateway)
		return
	}
	if libs == nil {
		libs = []kavita.Library{}
	}
	jsonOK(w, kavitaLibrariesResponse{Libraries: libs})
}

// apiKavitaLibrariesFragment handles GET /api/kavita/libraries/fragment.
// Returns an HTML fragment for HTMX: three labeled <select> elements populated
// with Kavita library options. On failure returns HTTP 200 with an inline error
// message (so HTMX always swaps the content and the user sees a readable message).
//
// Builds a fresh kavita.Client from current Settings each call so the user can
// change Kavita URL/key in the form, click Save, then click Sync — and it just
// works without any restart.
func (h *Handler) apiKavitaLibrariesFragment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	settings, err := h.store.GetSettings()
	if err != nil {
		// Can't read settings — render error + disabled placeholders.
		fmt.Fprintf(w, `<div class="form-error">Cannot read settings: %s</div>`, html(err.Error()))
		writeKavitaLibPlaceholders(w, 0, 0, 0)
		return
	}
	if settings.KavitaLibIDsByType == nil {
		settings.KavitaLibIDsByType = map[model.ContentType]int64{}
	}
	savedManga := settings.KavitaLibIDsByType[model.TypeManga]
	savedManhwa := settings.KavitaLibIDsByType[model.TypeManhwa]
	savedManhua := settings.KavitaLibIDsByType[model.TypeManhua]

	if settings.KavitaBaseURL == "" || settings.KavitaAPIKey == "" {
		fmt.Fprint(w, `<div class="form-error">Kavita not configured. Set the base URL and API key in Settings &#8594; Kavita Connection above, click Save, then Sync.</div>`)
		writeKavitaLibPlaceholders(w, savedManga, savedManhwa, savedManhua)
		return
	}

	fetchCtx, fetchCancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer fetchCancel()
	client := kavita.New(settings.KavitaBaseURL, settings.KavitaAPIKey)
	libs, err := client.ListLibraries(fetchCtx)
	if err != nil {
		fmt.Fprintf(w, `<div class="form-error">Kavita unreachable: %s. Check Settings &#8594; Kavita Connection.</div>`,
			html(err.Error()))
		writeKavitaLibPlaceholders(w, savedManga, savedManhwa, savedManhua)
		return
	}

	writeKavitaLibSelects(w, libs, savedManga, savedManhwa, savedManhua)
}

// writeKavitaLibSelects renders three labeled <select> elements populated with
// the given Kavita library list. The currently-saved IDs get the selected attribute.
// If a saved ID is non-zero but missing from the list, an "Unknown (ID: N)" option
// is prepended.
func writeKavitaLibSelects(w http.ResponseWriter, libs []kavita.Library, savedManga, savedManhwa, savedManhua int64) {
	type entry struct {
		label   string
		name    string
		savedID int64
	}
	rows := []entry{
		{"MANGA LIBRARY", "kavita_lib_manga", savedManga},
		{"MANHWA LIBRARY", "kavita_lib_manhwa", savedManhwa},
		{"MANHUA LIBRARY", "kavita_lib_manhua", savedManhua},
	}
	for _, row := range rows {
		fmt.Fprintf(w, `<div class="settings-row"><label class="settings-label">%s</label><div class="settings-input-wrap">`,
			html(row.label))
		fmt.Fprintf(w, `<select name="%s">`, html(row.name))
		fmt.Fprint(w, `<option value="0">(none)</option>`)
		// Prepend unknown option if saved ID is non-zero but not in list.
		if row.savedID > 0 {
			found := false
			for _, lib := range libs {
				if lib.ID == row.savedID {
					found = true
					break
				}
			}
			if !found {
				fmt.Fprintf(w, `<option value="%d" selected>Unknown (ID: %d)</option>`,
					row.savedID, row.savedID)
			}
		}
		for _, lib := range libs {
			sel := ""
			if lib.ID == row.savedID {
				sel = " selected"
			}
			fmt.Fprintf(w, `<option value="%d"%s>%s</option>`,
				lib.ID, sel, html(lib.Name))
		}
		fmt.Fprint(w, `</select></div></div>`)
	}
}

// writeKavitaLibPlaceholders renders three disabled <select> elements with a
// placeholder option. The select name= attributes are preserved so form POST
// parsing keeps working. A non-zero savedID renders a "(saved: N)" hint so the
// user knows a value is persisted even though the list is unavailable.
func writeKavitaLibPlaceholders(w http.ResponseWriter, savedManga, savedManhwa, savedManhua int64) {
	type entry struct {
		label   string
		name    string
		savedID int64
	}
	rows := []entry{
		{"MANGA LIBRARY", "kavita_lib_manga", savedManga},
		{"MANHWA LIBRARY", "kavita_lib_manhwa", savedManhwa},
		{"MANHUA LIBRARY", "kavita_lib_manhua", savedManhua},
	}
	for _, row := range rows {
		fmt.Fprintf(w, `<div class="settings-row"><label class="settings-label">%s</label><div class="settings-input-wrap">`,
			html(row.label))
		if row.savedID > 0 {
			fmt.Fprintf(w, `<select name="%s" disabled><option value="%d" selected>Click Sync after configuring Kavita above (saved: %d)</option></select>`,
				html(row.name), row.savedID, row.savedID)
		} else {
			fmt.Fprintf(w, `<select name="%s" disabled><option value="0">Click Sync after configuring Kavita above</option></select>`,
				html(row.name))
		}
		fmt.Fprint(w, `</div></div>`)
	}
}

// breadcrumb represents one segment in the path-browser breadcrumbs.
type breadcrumb struct {
	Label string
	Path  string
}

// buildBreadcrumbs constructs a slice of breadcrumbs for abs relative to the
// deepest matching allowlist root. If abs does not match any root, returns a
// single crumb for abs itself.
func buildBreadcrumbs(abs string, roots []string) []breadcrumb {
	// Find the longest matching root.
	bestRoot := ""
	for _, root := range roots {
		rootAbs, err := filepath.Abs(filepath.Clean(root))
		if err != nil {
			continue
		}
		if abs == rootAbs || strings.HasPrefix(abs, rootAbs+string(filepath.Separator)) {
			if len(rootAbs) > len(bestRoot) {
				bestRoot = rootAbs
			}
		}
	}
	if bestRoot == "" {
		return []breadcrumb{{Label: filepath.Base(abs), Path: abs}}
	}
	// Walk from root to abs.
	rel, err := filepath.Rel(bestRoot, abs)
	if err != nil {
		return []breadcrumb{{Label: filepath.Base(abs), Path: abs}}
	}
	var crumbs []breadcrumb
	// Add the root crumb itself.
	crumbs = append(crumbs, breadcrumb{Label: bestRoot, Path: bestRoot})
	if rel == "." {
		return crumbs
	}
	parts := strings.Split(rel, string(filepath.Separator))
	cur := bestRoot
	for _, p := range parts {
		cur = filepath.Join(cur, p)
		crumbs = append(crumbs, breadcrumb{Label: p, Path: cur})
	}
	return crumbs
}

// ---- metrics endpoint ----

// serveMetrics serves the Prometheus metrics text format.
// Returns 503 when no metricsHandler is wired (nil MetricsSink).
func (h *Handler) serveMetrics(w http.ResponseWriter, r *http.Request) {
	if h.metricsHandler == nil {
		http.Error(w, "metrics not configured", http.StatusServiceUnavailable)
		return
	}
	h.metricsHandler.ServeHTTP(w, r)
}

// ---- helpers ----

// renderTemplate executes a named template within a template set without
// the base.html wrapping. Used by the override-fragment HTMX endpoint
// where we want raw inner HTML, not a full page. Returns the underlying
// ExecuteTemplate error so the caller can surface it inline.
func (h *Handler) renderTemplate(w http.ResponseWriter, set, name string, data interface{}) error {
	t, ok := h.tmpls[set]
	if !ok {
		return fmt.Errorf("template set not found: %s", set)
	}
	return t.ExecuteTemplate(w, name, data)
}

func (h *Handler) render(w http.ResponseWriter, name string, data interface{}) {
	t, ok := h.tmpls[name]
	if !ok {
		http.Error(w, "template not found: "+name, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Each page template file starts with {{template "base" .}} at the top level.
	// ParseFS sets the file's basename as the template name, so execute by page name.
	// The page template then calls "base" which calls {{block "content" .}},
	// resolved from that page's own {{define "content"}}.
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func jsonOK(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, err error, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func setIfNonEmpty(m map[model.ContentType]string, k model.ContentType, v string) {
	if v = strings.TrimSpace(v); v != "" {
		m[k] = v
	}
}

func setLibID(m map[model.ContentType]int64, k model.ContentType, v string) {
	if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n > 0 {
		m[k] = n
	}
}

func selectedIf(b bool) string {
	if b {
		return " selected"
	}
	return ""
}

func html(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&#34;")
	// Escape single quotes too — defence-in-depth so the helper is safe if
	// future code places a value inside a single-quoted attribute.
	s = strings.ReplaceAll(s, "'", "&#39;")
	return s
}
