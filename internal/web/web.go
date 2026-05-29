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

	"github.com/gavinmcfall/mangarr/internal/dbbackup"
	"github.com/gavinmcfall/mangarr/internal/diskspace"
	"github.com/gavinmcfall/mangarr/internal/filer"
	"github.com/gavinmcfall/mangarr/internal/health"
	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/poller"
	"github.com/gavinmcfall/mangarr/internal/tasks"
)

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
}

// Runner can execute one poll pass on demand.
type Runner interface {
	RunOnce() error
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

// Handler is the HTTP handler for the web UI and JSON API.
type Handler struct {
	mux                     *http.ServeMux
	tmpls                   map[string]*template.Template // one template set per page
	store                   Store
	runner                  Runner
	previewer               Previewer                     // optional; /preview returns placeholder when nil
	seriesFiler             SeriesFiler                   // optional; /api/series/{id}/assign returns 503 when nil
	downloadRoots           []string // from config.DownloadRoots; used by disk-space endpoints
	browseRoots             []string // allowlist for /api/browse (injected; tests can override)
	recycleBinPath          string
	recycleBinRetentionDays int
	backupDir               string
	backupCfg               BackupConfig
	backupFn                func() (dbbackup.Entry, error) // injected for on-demand backup
	taskReg                 TaskRegistry                   // injected for Tasks page + /api/tasks
	healthReg               HealthRegistry                 // injected for Health page + /api/health
	metricsHandler          http.Handler                   // nil → /metrics returns 503
}

// NewHandler wires up all routes and parses embedded templates.
// runner may be nil (RunOnce calls will return 503).
// recycleBinPath and recycleBinRetentionDays are env-derived config values
// surfaced read-only on the Settings page.
// previewer may be nil (/preview returns a placeholder page).
// downloadRoots is the list of download root paths (may be empty in tests).
// taskReg may be nil (tasks routes return 503).
// healthReg may be nil (health routes show a placeholder warning).
// metrics may be nil (GET /metrics returns 503).
// Backup API is wired separately via NewHandlerWithBackup (returns 503 here).
func NewHandler(store Store, runner Runner, recycleBinPath string, recycleBinRetentionDays int, taskReg TaskRegistry, healthReg HealthRegistry, metrics MetricsSink, previewer Previewer, downloadRoots ...string) *Handler {
	return newHandlerFull(store, runner, nil, recycleBinPath, recycleBinRetentionDays, BackupConfig{}, nil, taskReg, healthReg, metrics, previewer, nil, downloadRoots)
}

// NewHandlerWithBackup is like NewHandler but also wires the backup API.
func NewHandlerWithBackup(store Store, runner Runner, recycleBinPath string, recycleBinRetentionDays int, cfg BackupConfig, backupFn func() (dbbackup.Entry, error), taskReg TaskRegistry, healthReg HealthRegistry, metrics MetricsSink, previewer Previewer, downloadRoots ...string) *Handler {
	return newHandlerFull(store, runner, nil, recycleBinPath, recycleBinRetentionDays, cfg, backupFn, taskReg, healthReg, metrics, previewer, nil, downloadRoots)
}

// NewHandlerWithFiler is like NewHandlerWithBackup but also wires the per-series filer
// used by POST /api/series/{id}/assign. In production main.go passes the *poller.Poller
// directly as it implements both Runner and SeriesFiler.
func NewHandlerWithFiler(store Store, runner Runner, seriesFiler SeriesFiler, recycleBinPath string, recycleBinRetentionDays int, cfg BackupConfig, backupFn func() (dbbackup.Entry, error), taskReg TaskRegistry, healthReg HealthRegistry, metrics MetricsSink, previewer Previewer, downloadRoots ...string) *Handler {
	return newHandlerFull(store, runner, seriesFiler, recycleBinPath, recycleBinRetentionDays, cfg, backupFn, taskReg, healthReg, metrics, previewer, nil, downloadRoots)
}

// NewHandlerWithBrowse is like NewHandlerWithFiler but also accepts an explicit
// browseRoots allowlist for the path-browser endpoints. If browseRoots is nil the
// production default (/media, /config) is used.
func NewHandlerWithBrowse(store Store, runner Runner, seriesFiler SeriesFiler, recycleBinPath string, recycleBinRetentionDays int, cfg BackupConfig, backupFn func() (dbbackup.Entry, error), taskReg TaskRegistry, healthReg HealthRegistry, metrics MetricsSink, previewer Previewer, browseRoots []string, downloadRoots ...string) *Handler {
	return newHandlerFull(store, runner, seriesFiler, recycleBinPath, recycleBinRetentionDays, cfg, backupFn, taskReg, healthReg, metrics, previewer, browseRoots, downloadRoots)
}

func newHandlerFull(store Store, runner Runner, seriesFiler SeriesFiler, recycleBinPath string, recycleBinRetentionDays int, cfg BackupConfig, backupFn func() (dbbackup.Entry, error), taskReg TaskRegistry, healthReg HealthRegistry, metrics MetricsSink, previewer Previewer, browseRoots []string, downloadRoots []string) *Handler {
	var mh http.Handler
	if metrics != nil {
		mh = metrics.Handler()
	}
	if browseRoots == nil {
		browseRoots = []string{"/media", "/config"}
	}
	h := &Handler{
		mux:                     http.NewServeMux(),
		tmpls:                   parsePageTemplates(),
		store:                   store,
		runner:                  runner,
		previewer:               previewer,
		seriesFiler:             seriesFiler,
		downloadRoots:           downloadRoots,
		browseRoots:             browseRoots,
		recycleBinPath:          recycleBinPath,
		recycleBinRetentionDays: recycleBinRetentionDays,
		backupDir:               cfg.Dir,
		backupCfg:               cfg,
		backupFn:                backupFn,
		taskReg:                 taskReg,
		healthReg:               healthReg,
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
	m := make(map[string]*template.Template, len(pages))
	for _, name := range pages {
		// Parse base + the specific page. This gives each page its own
		// independent template set so block overrides don't bleed across pages.
		t := template.Must(
			template.New("").Funcs(templateFuncs()).ParseFS(assets,
				"templates/base.html",
				"templates/"+name,
			),
		)
		m[name] = t
	}
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

// diskSpaceRow is a single row in the disk-space display: one path with its
// space info and presentation fields pre-computed server-side.
type diskSpaceRow struct {
	Label          string // e.g. "Download root" or "Manga library"
	Path           string
	Free           string // formatted free bytes, e.g. "42.0 GiB"
	Total          string // formatted total bytes
	PercentFmt     string // e.g. "73"  (integer %, no decimal, for bar width — represents %used)
	PercentUsedFmt string // e.g. "73%" — human-readable label shown inside/beside bar
	BarClass       string // "bar-ok" | "bar-warn" | "bar-err"
	Err            string // non-empty when path is unavailable
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
	RootManga               string
	RootManhwa              string
	RootManhua              string
	KavitaLibManga          int64
	KavitaLibManhwa         int64
	KavitaLibManhua         int64
	RenameExample           string
	DiskRows                []diskSpaceRow
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
	h.render(w, "activity.html", pageData{Page: "activity", Items: list})
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

	// Build disk-space rows: download roots first, then library roots.
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

	// Pre-extract values typed-keyed by model.ContentType into plain fields,
	// so the template can use {{.RootManga}} etc. with no reflection-time
	// type mismatch. See settingsPageData doc comment.
	h.render(w, "settings.html", settingsPageData{
		Page:                    "settings",
		Settings:                settings,
		KavitaAPIKey:            settings.KavitaAPIKey,
		Flash:                   flashMsg,
		RootManga:               settings.LibraryRoots[model.TypeManga],
		RootManhwa:              settings.LibraryRoots[model.TypeManhwa],
		RootManhua:              settings.LibraryRoots[model.TypeManhua],
		KavitaLibManga:          settings.KavitaLibIDsByType[model.TypeManga],
		KavitaLibManhwa:         settings.KavitaLibIDsByType[model.TypeManhwa],
		KavitaLibManhua:         settings.KavitaLibIDsByType[model.TypeManhua],
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
// Download roots come from the Handler (config.DownloadRoots).
// Library roots come from settings (may be partially configured).
func (h *Handler) buildDiskRows(settings model.Settings) []diskSpaceRow {
	type pathSpec struct {
		label string
		path  string
	}
	var specs []pathSpec
	for _, p := range h.downloadRoots {
		specs = append(specs, pathSpec{"Download root", p})
	}
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

	// Deduplicate: same physical path should only appear once
	// (a download root might equal a library root).
	seen := map[string]bool{}
	var rows []diskSpaceRow
	for _, spec := range specs {
		key := spec.label + "|" + spec.path
		if seen[key] {
			continue
		}
		seen[key] = true
		rows = append(rows, makeDiskRow(spec.label, spec.path))
	}
	return rows
}

// makeDiskRow builds a single diskSpaceRow from a path.
func makeDiskRow(label, path string) diskSpaceRow {
	info := diskspace.Stat(path)
	if info.Err != nil {
		return diskSpaceRow{
			Label:    label,
			Path:     path,
			Err:      "unavailable",
			BarClass: "bar-err",
		}
	}
	pctUsed := 100.0 - info.PercentFree()
	if pctUsed < 0 {
		pctUsed = 0
	}
	pctUsedInt := int(pctUsed)
	return diskSpaceRow{
		Label:          label,
		Path:           path,
		Free:           diskspace.FormatBytes(info.FreeBytes),
		Total:          diskspace.FormatBytes(info.TotalBytes),
		PercentFmt:     fmt.Sprintf("%d", pctUsedInt),
		PercentUsedFmt: fmt.Sprintf("%d%% used", pctUsedInt),
		BarClass:       diskSpaceClass(pctUsed),
	}
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
			RootManga:       settings.LibraryRoots[model.TypeManga],
			RootManhwa:      settings.LibraryRoots[model.TypeManhwa],
			RootManhua:      settings.LibraryRoots[model.TypeManhua],
			KavitaLibManga:  settings.KavitaLibIDsByType[model.TypeManga],
			KavitaLibManhwa: settings.KavitaLibIDsByType[model.TypeManhwa],
			KavitaLibManhua: settings.KavitaLibIDsByType[model.TypeManhua],
			RenameExample:   renameExample(settings.RenameScheme),
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
	if err := h.runner.RunOnce(); err != nil {
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

	// Collect all unique paths: download roots + configured library roots.
	type pathEntry struct {
		path string
	}
	seen := map[string]bool{}
	var paths []string
	for _, p := range h.downloadRoots {
		if !seen[p] {
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
func (h *Handler) apiBrowseFragment(w http.ResponseWriter, r *http.Request) {
	rawPath := r.URL.Query().Get("path")
	target := r.URL.Query().Get("target") // e.g. "root_manga"
	abs, errMsg, synthetic := h.resolveBrowsePath(rawPath)
	if errMsg == "forbidden" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `<div class="browse-error">Path is outside the allowed filesystem roots.</div>`)
		return
	}
	if errMsg != "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `<div class="browse-error">%s</div>`, html(errMsg))
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
