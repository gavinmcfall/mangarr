// Package web provides the embedded HTMX UI and JSON API for mangarr.
//
// Routes (Go 1.22 method+path syntax):
//
//	GET  /                           → redirect to /series
//	GET  /series                     → Series page (HTMX)
//	GET  /unmatched                  → Unmatched page (HTMX)
//	GET  /activity                   → Activity/History page (HTMX)
//	GET  /settings                   → Settings page (HTMX)
//	POST /settings                   → Save settings (form POST, redirect back)
//	POST /api/series/{id}/reclassify → Override a series' type (HTMX form target)
//	POST /api/rescan                 → Trigger RunOnce on-demand
//	GET  /api/series                 → JSON list of all series
//	GET  /api/unmatched              → JSON list of unmatched series
//	GET  /api/activity               → JSON activity log
//	GET  /api/settings               → JSON current settings
//	PUT  /api/settings               → JSON update settings
//	GET  /static/*                   → Embedded static assets (htmx.min.js)
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/gavinmcfall/mangarr/internal/model"
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

// Handler is the HTTP handler for the web UI and JSON API.
type Handler struct {
	mux    *http.ServeMux
	tmpls  map[string]*template.Template // one template set per page
	store  Store
	runner Runner
}

// NewHandler wires up all routes and parses embedded templates.
// runner may be nil (RunOnce calls will return 503).
func NewHandler(store Store, runner Runner) *Handler {
	h := &Handler{
		mux:    http.NewServeMux(),
		tmpls:  parsePageTemplates(),
		store:  store,
		runner: runner,
	}

	// Static assets
	h.mux.Handle("GET /static/", http.FileServerFS(assets))

	// HTML pages
	h.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/series", http.StatusFound)
	})
	h.mux.HandleFunc("GET /series", h.pageSeries)
	h.mux.HandleFunc("GET /unmatched", h.pageUnmatched)
	h.mux.HandleFunc("GET /activity", h.pageActivity)
	h.mux.HandleFunc("GET /settings", h.pageSettings)
	h.mux.HandleFunc("POST /settings", h.saveSettings)

	// JSON API
	h.mux.HandleFunc("GET /api/series", h.apiListSeries)
	h.mux.HandleFunc("GET /api/unmatched", h.apiListUnmatched)
	h.mux.HandleFunc("GET /api/activity", h.apiListActivity)
	h.mux.HandleFunc("GET /api/settings", h.apiGetSettings)
	h.mux.HandleFunc("PUT /api/settings", h.apiPutSettings)
	h.mux.HandleFunc("POST /api/rescan", h.apiRescan)

	// HTMX action: per-series reclassify (POST /api/series/{id}/reclassify)
	h.mux.HandleFunc("POST /api/series/{id}/reclassify", h.apiReclassify)

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
	pages := []string{"series.html", "unmatched.html", "activity.html", "settings.html"}
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
	}
}

// ---- page data types ----

type pageData struct {
	Page  string
	Items interface{}
	Flash string
	Error string
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
	Page            string
	Settings        model.Settings
	KavitaAPIKey    string
	Flash           string
	Error           string
	RootManga       string
	RootManhwa      string
	RootManhua      string
	KavitaLibManga  int64
	KavitaLibManhwa int64
	KavitaLibManhua int64
}

// ---- HTML page handlers ----

func (h *Handler) pageSeries(w http.ResponseWriter, r *http.Request) {
	list, err := h.store.ListSeries()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.render(w, "series.html", pageData{Page: "series", Items: list})
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
	// Pre-extract values typed-keyed by model.ContentType into plain fields,
	// so the template can use {{.RootManga}} etc. with no reflection-time
	// type mismatch. See settingsPageData doc comment.
	h.render(w, "settings.html", settingsPageData{
		Page:            "settings",
		Settings:        settings,
		KavitaAPIKey:    settings.KavitaAPIKey,
		Flash:           flashMsg,
		RootManga:       settings.LibraryRoots[model.TypeManga],
		RootManhwa:      settings.LibraryRoots[model.TypeManhwa],
		RootManhua:      settings.LibraryRoots[model.TypeManhua],
		KavitaLibManga:  settings.KavitaLibIDsByType[model.TypeManga],
		KavitaLibManhwa: settings.KavitaLibIDsByType[model.TypeManhwa],
		KavitaLibManhua: settings.KavitaLibIDsByType[model.TypeManhua],
	})
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
	if err := h.store.SaveSettings(settings); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) apiRescan(w http.ResponseWriter, r *http.Request) {
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
