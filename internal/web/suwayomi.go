// Suwayomi web endpoints + Settings page helpers.
//
// Mirrors the Kavita endpoints in web.go: fresh-per-call client construction,
// JSON test/list endpoints, and an HTMX fragment for the Settings page.
//
// All three handlers build a fresh suwayomi.Client from the CURRENT Settings
// every call. Long-lived session state lives only inside the Auth
// implementation — see [[reference-settings-driven-clients-fresh-per-call]].

package web

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gavinmcfall/mangarr/internal/kavita"
	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/suwayomi"
)

// suwayomiTestResponse is the JSON shape for GET /api/suwayomi/test.
//
// On success: Ok=true, CategoryCount=N. On failure: Ok=false, Error=<message>.
// Status code is always 200; the body's Ok field is the source of truth so
// the UI can render `● connected, N categories` or `● <error>` without
// branching on HTTP code.
type suwayomiTestResponse struct {
	Ok            bool   `json:"ok"`
	CategoryCount int    `json:"category_count"`
	Error         string `json:"error,omitempty"`
}

// suwayomiCategoryDTO is the JSON shape returned by GET /api/suwayomi/categories.
// We re-shape Category here so the JSON keys stay stable even if the upstream
// internal struct changes a tag.
type suwayomiCategoryDTO struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Order int    `json:"order"`
}

// suwayomiCategoriesResponse wraps the category list in an object so future
// metadata fields (cache age, total count, etc.) can be added without a
// breaking-shape change.
type suwayomiCategoriesResponse struct {
	Categories []suwayomiCategoryDTO `json:"categories"`
}

// buildSuwayomiAuth maps a settings row to the matching Auth implementation.
// Returns NoAuth on an unknown / empty SuwayomiAuthType so the caller never
// has to nil-check.
func buildSuwayomiAuth(s model.Settings) suwayomi.Auth {
	switch s.SuwayomiAuthType {
	case model.SuwayomiAuthBasic:
		return suwayomi.BasicAuth{Username: s.SuwayomiUsername, Password: s.SuwayomiPassword}
	case model.SuwayomiAuthSimple:
		return &suwayomi.SimpleLoginAuth{Username: s.SuwayomiUsername, Password: s.SuwayomiPassword}
	case model.SuwayomiAuthUI:
		return &suwayomi.UILoginAuth{Username: s.SuwayomiUsername, Password: s.SuwayomiPassword}
	default:
		return suwayomi.NoAuth{}
	}
}

// newSuwayomiClient builds a fresh client from the given Settings.
// Returns (nil, false) when the base URL is empty — caller surfaces a
// "not configured" message without making any outbound network call.
func newSuwayomiClient(s model.Settings) (*suwayomi.Client, bool) {
	base := strings.TrimSpace(s.SuwayomiBaseURL)
	if base == "" {
		return nil, false
	}
	return suwayomi.New(base, buildSuwayomiAuth(s)), true
}

// sanitiseSuwayomiError strips the configured base URL and password from an
// error message before surfacing it to the UI. This prevents the API base URL
// or password from leaking into a Test-button result that may be screenshotted
// or pasted into chat. The error MESSAGE itself stays — operators still need
// to see why the test failed.
func sanitiseSuwayomiError(err error, s model.Settings) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if u := strings.TrimSpace(s.SuwayomiBaseURL); u != "" {
		msg = strings.ReplaceAll(msg, u, "[suwayomi]")
		// Strip trailing-slash variants too.
		msg = strings.ReplaceAll(msg, strings.TrimRight(u, "/"), "[suwayomi]")
	}
	if p := s.SuwayomiPassword; p != "" {
		msg = strings.ReplaceAll(msg, p, "[redacted]")
	}
	return msg
}

// parseSuwayomiOverrides walks a parsed form and assembles the override map
// from override_category_<idx> + override_library_<idx> pairs. Indices need
// not be contiguous — JS may have removed rows mid-edit. Any pair with a
// zero/empty category OR library ID is dropped (the "Add" → "Delete"
// lifecycle saves clean state without JS having to scrub hidden inputs).
//
// Returns nil when no valid overrides are found, so the round-trip JSON
// stays compact when the feature is unused.
func parseSuwayomiOverrides(form map[string][]string) map[int64]int64 {
	out := map[int64]int64{}
	// Index field names by suffix so we can pair them up regardless of
	// which JS-counter idx was used.
	cats := map[string]string{}
	libs := map[string]string{}
	for k, vs := range form {
		if len(vs) == 0 {
			continue
		}
		switch {
		case strings.HasPrefix(k, "override_category_"):
			cats[strings.TrimPrefix(k, "override_category_")] = vs[0]
		case strings.HasPrefix(k, "override_library_"):
			libs[strings.TrimPrefix(k, "override_library_")] = vs[0]
		}
	}
	for idx, catRaw := range cats {
		libRaw, ok := libs[idx]
		if !ok {
			continue
		}
		catID, err := strconv.ParseInt(strings.TrimSpace(catRaw), 10, 64)
		if err != nil || catID <= 0 {
			continue
		}
		libID, err := strconv.ParseInt(strings.TrimSpace(libRaw), 10, 64)
		if err != nil || libID <= 0 {
			continue
		}
		out[catID] = libID
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// apiSuwayomiTest handles GET /api/suwayomi/test.
//
// Builds a fresh client from current Settings, calls Ping (which itself runs
// ListCategories), and returns {ok, category_count, error}. 5 second timeout
// caps the round-trip so the UI never hangs.
func (h *Handler) apiSuwayomiTest(w http.ResponseWriter, r *http.Request) {
	settings, err := h.store.GetSettings()
	if err != nil {
		jsonOK(w, suwayomiTestResponse{Error: err.Error()})
		return
	}
	client, ok := newSuwayomiClient(settings)
	if !ok {
		jsonOK(w, suwayomiTestResponse{Error: "Suwayomi base URL not configured"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	cats, err := client.ListCategories(ctx)
	if err != nil {
		jsonOK(w, suwayomiTestResponse{Error: sanitiseSuwayomiError(err, settings)})
		return
	}
	jsonOK(w, suwayomiTestResponse{Ok: true, CategoryCount: len(cats)})
}

// apiSuwayomiCategories handles GET /api/suwayomi/categories.
// Returns {categories:[{id,name,order},...]} or {error:"..."} + 503/502.
func (h *Handler) apiSuwayomiCategories(w http.ResponseWriter, r *http.Request) {
	settings, err := h.store.GetSettings()
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	client, ok := newSuwayomiClient(settings)
	if !ok {
		jsonErr(w, fmt.Errorf("suwayomi base URL not configured"), http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	cats, err := client.ListCategories(ctx)
	if err != nil {
		jsonErr(w, fmt.Errorf("%s", sanitiseSuwayomiError(err, settings)), http.StatusBadGateway)
		return
	}
	out := make([]suwayomiCategoryDTO, 0, len(cats))
	for _, c := range cats {
		out = append(out, suwayomiCategoryDTO{ID: c.ID, Name: c.Name, Order: c.Order})
	}
	jsonOK(w, suwayomiCategoriesResponse{Categories: out})
}

// apiSuwayomiCategoriesFragment handles GET /api/suwayomi/categories/fragment.
//
// Returns the override-card body: one row per entry in
// SuwayomiCategoryOverrides plus an empty "Add" row template (rendered via
// JS on click). Mirrors apiKavitaLibrariesFragment in shape — always returns
// 200 with HTML so HTMX can swap the result cleanly even on failure.
func (h *Handler) apiSuwayomiCategoriesFragment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	settings, err := h.store.GetSettings()
	if err != nil {
		fmt.Fprintf(w, `<div class="form-error">Cannot read settings: %s</div>`, html(err.Error()))
		return
	}

	libChoices := overrideLibraryChoices(settings)

	// Empty base URL = feature disabled. Render the configure-first prompt
	// without making any outbound call.
	if strings.TrimSpace(settings.SuwayomiBaseURL) == "" {
		fmt.Fprint(w, `<div class="form-error">Suwayomi not configured. Set the base URL in the Suwayomi Connection panel above, click Save, then Sync.</div>`)
		writeOverrideRows(w, settings.SuwayomiCategoryOverrides, nil, libChoices)
		return
	}

	// Empty content-type → library mapping = override card has nothing to
	// route to. Surface a configure-first prompt rather than render rows
	// with empty dropdowns.
	if len(libChoices) == 0 {
		fmt.Fprint(w, `<div class="form-error">Configure AniList Classification above before adding Suwayomi overrides.</div>`)
		return
	}

	client, ok := newSuwayomiClient(settings)
	if !ok {
		fmt.Fprint(w, `<div class="form-error">Suwayomi not configured.</div>`)
		writeOverrideRows(w, settings.SuwayomiCategoryOverrides, nil, libChoices)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	cats, err := client.ListCategories(ctx)
	if err != nil {
		fmt.Fprintf(w, `<div class="form-error">Suwayomi unreachable: %s. Check Settings &#8594; Suwayomi Connection.</div>`,
			html(sanitiseSuwayomiError(err, settings)))
		writeOverrideRows(w, settings.SuwayomiCategoryOverrides, nil, libChoices)
		return
	}
	writeOverrideRows(w, settings.SuwayomiCategoryOverrides, cats, libChoices)
}

// overrideLibraryChoice is one entry in the override-row Kavita library
// dropdown. The dropdown is filtered to libraries assigned to one of the
// three primary content types — overrides that map to other libraries would
// silently route to Unmatched (Plan B carry-forward constraint).
type overrideLibraryChoice struct {
	ID          int64
	Label       string            // human-readable: kavita library name
	ContentType model.ContentType // resolved content type from KavitaLibIDsByType
}

// overrideLibraryChoices returns the Kavita libraries the user has assigned
// to one of the three primary content types. We do NOT re-fetch from Kavita
// here — the assignment lives in Settings.KavitaLibIDsByType, and the names
// can be fetched server-side once when rendering. For the fragment endpoint
// we get the names lazily (id-only labels are acceptable when Kavita is
// unreachable, since the override card depends on user configuration above).
func overrideLibraryChoices(s model.Settings) []overrideLibraryChoice {
	if len(s.KavitaLibIDsByType) == 0 {
		return nil
	}
	order := []model.ContentType{model.TypeManga, model.TypeManhwa, model.TypeManhua}
	var out []overrideLibraryChoice
	seen := map[int64]bool{}
	for _, ct := range order {
		id := s.KavitaLibIDsByType[ct]
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, overrideLibraryChoice{
			ID:          id,
			Label:       fmt.Sprintf("Library #%d (%s)", id, ct),
			ContentType: ct,
		})
	}
	return out
}

// resolveOverrideLibraryNames upgrades the Library label from "Library #N (Manga)"
// to "<actualName> (Manga)" when Kavita is reachable. Best-effort: failure is
// invisible — the operator still sees a working override card with id labels.
func resolveOverrideLibraryNames(ctx context.Context, s model.Settings, choices []overrideLibraryChoice) []overrideLibraryChoice {
	if len(choices) == 0 || s.KavitaBaseURL == "" || s.KavitaAPIKey == "" {
		return choices
	}
	client := kavita.New(s.KavitaBaseURL, s.KavitaAPIKey)
	libs, err := client.ListLibraries(ctx)
	if err != nil {
		return choices
	}
	byID := make(map[int64]string, len(libs))
	for _, lib := range libs {
		byID[lib.ID] = lib.Name
	}
	out := make([]overrideLibraryChoice, len(choices))
	for i, c := range choices {
		if name, ok := byID[c.ID]; ok && name != "" {
			out[i] = overrideLibraryChoice{
				ID:          c.ID,
				Label:       fmt.Sprintf("%s (%s)", name, c.ContentType),
				ContentType: c.ContentType,
			}
		} else {
			out[i] = c
		}
	}
	return out
}

// findContentTypeForLibrary returns the ContentType assigned to libID in
// Settings.KavitaLibIDsByType, or empty when libID is not in the map.
// Used by the activity log + override-row badge.
func findContentTypeForLibrary(s model.Settings, libID int64) model.ContentType {
	for _, ct := range []model.ContentType{model.TypeManga, model.TypeManhwa, model.TypeManhua} {
		if s.KavitaLibIDsByType[ct] == libID {
			return ct
		}
	}
	return ""
}

// buildOverrideRows assembles the view-models the Settings template renders
// directly (initial GET /settings). cats may be nil — saved categories then
// render as "Unknown (ID: N)".
func buildOverrideRows(overrides map[int64]int64, cats []suwayomi.Category, libChoices []overrideLibraryChoice) []overrideRowView {
	type savedRow struct {
		catID int64
		libID int64
	}
	var rows []savedRow
	for catID, libID := range overrides {
		rows = append(rows, savedRow{catID, libID})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].catID < rows[j].catID })

	catByID := make(map[int64]suwayomi.Category, len(cats))
	for _, c := range cats {
		catByID[c.ID] = c
	}

	out := make([]overrideRowView, 0, len(rows))
	for i, r := range rows {
		view := overrideRowView{Index: i, CatID: r.catID, LibID: r.libID}
		if c, ok := catByID[r.catID]; ok {
			view.CatName = c.Name
			view.CatKnown = true
		} else {
			view.CatName = fmt.Sprintf("Unknown (ID: %d)", r.catID)
		}
		for _, lc := range libChoices {
			if lc.ID == r.libID {
				view.LibLabel = lc.Label
				view.ContentType = lc.ContentType
				break
			}
		}
		out = append(out, view)
	}
	return out
}

// writeOverrideRows emits one row per override entry, an empty row template
// (the JS "Add" button stamps out copies), and a hidden index counter. cats
// may be nil when Suwayomi is unreachable — rows still render with the
// saved category ID as "Unknown (ID: N)".
func writeOverrideRows(w http.ResponseWriter, overrides map[int64]int64, cats []suwayomi.Category, libChoices []overrideLibraryChoice) {
	// Stable display order: by saved category ID ascending. cats may
	// provide a richer order field, but the saved row always shows even
	// when Suwayomi is unreachable, so use the only key we always know:
	// the saved category ID.
	type savedRow struct {
		catID int64
		libID int64
	}
	var rows []savedRow
	for catID, libID := range overrides {
		rows = append(rows, savedRow{catID, libID})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].catID < rows[j].catID })

	catByID := make(map[int64]suwayomi.Category, len(cats))
	for _, c := range cats {
		catByID[c.ID] = c
	}

	fmt.Fprint(w, `<div class="override-rows" id="override-rows-body">`)
	for i, r := range rows {
		writeOverrideRow(w, i, r.catID, r.libID, cats, catByID, libChoices)
	}
	fmt.Fprint(w, `</div>`)

	// Hidden state for JS "Add" handler: next index to use + the available
	// category/library options encoded as data attributes. Categories are
	// JSON-encoded so the JS can re-render dropdowns identically.
	fmt.Fprintf(w, `<input type="hidden" id="override-next-idx" value="%d">`, len(rows))
	// "Add" button — JS appends a fresh row by cloning the template.
	fmt.Fprint(w, `<div class="override-actions" style="margin-top:12px;">`)
	fmt.Fprint(w, `<button type="button" class="btn-sm" onclick="mangarrAddOverrideRow()">+ Add override</button>`)
	fmt.Fprint(w, `</div>`)

	// Template kept off-screen so JS can clone it. Mirrors the rendered row.
	fmt.Fprint(w, `<template id="override-row-template">`)
	writeOverrideRow(w, -1, 0, 0, cats, catByID, libChoices)
	fmt.Fprint(w, `</template>`)
}

// writeOverrideRow renders one override row.
// idx < 0 = the template (no rendered ID, JS rewrites name="" suffix on clone).
func writeOverrideRow(w http.ResponseWriter, idx int, savedCatID, savedLibID int64,
	cats []suwayomi.Category, catByID map[int64]suwayomi.Category,
	libChoices []overrideLibraryChoice) {
	idxAttr := strconv.Itoa(idx)
	if idx < 0 {
		idxAttr = "__IDX__"
	}
	fmt.Fprintf(w, `<div class="settings-row override-row" data-idx="%s">`, html(idxAttr))

	// Category dropdown
	fmt.Fprint(w, `<div class="settings-input-wrap">`)
	fmt.Fprintf(w, `<select name="override_category_%s">`, html(idxAttr))
	fmt.Fprint(w, `<option value="0">(select category)</option>`)
	// Unknown saved ID gets prepended.
	if savedCatID > 0 {
		if _, found := catByID[savedCatID]; !found {
			fmt.Fprintf(w, `<option value="%d" selected>Unknown (ID: %d)</option>`, savedCatID, savedCatID)
		}
	}
	for _, c := range cats {
		sel := ""
		if c.ID == savedCatID {
			sel = " selected"
		}
		fmt.Fprintf(w, `<option value="%d"%s>%s</option>`, c.ID, sel, html(c.Name))
	}
	fmt.Fprint(w, `</select></div>`)

	// Arrow separator
	fmt.Fprint(w, `<span class="override-arrow">&rarr;</span>`)

	// Library dropdown (filtered to KavitaLibIDsByType libraries)
	fmt.Fprint(w, `<div class="settings-input-wrap">`)
	fmt.Fprintf(w, `<select name="override_library_%s">`, html(idxAttr))
	fmt.Fprint(w, `<option value="0">(select library)</option>`)
	for _, lc := range libChoices {
		sel := ""
		if lc.ID == savedLibID {
			sel = " selected"
		}
		fmt.Fprintf(w, `<option value="%d"%s>%s</option>`, lc.ID, sel, html(lc.Label))
	}
	// Saved library ID not in choices → render as Unknown so user sees + can re-pick.
	if savedLibID > 0 {
		found := false
		for _, lc := range libChoices {
			if lc.ID == savedLibID {
				found = true
				break
			}
		}
		if !found {
			fmt.Fprintf(w, `<option value="%d" selected>Unknown library (ID: %d)</option>`, savedLibID, savedLibID)
		}
	}
	fmt.Fprint(w, `</select></div>`)

	// Resolved content-type badge (Plan B carry-forward: shows which
	// AniList content type this override maps to via the reverse lookup).
	ct := ""
	for _, lc := range libChoices {
		if lc.ID == savedLibID {
			ct = string(lc.ContentType)
			break
		}
	}
	if ct != "" {
		fmt.Fprintf(w, `<span class="override-badge pill pill-%s">%s</span>`, html(strings.ToLower(ct)), html(ct))
	} else if savedLibID > 0 {
		fmt.Fprint(w, `<span class="override-badge pill pill-error">unmapped</span>`)
	}

	// Delete button
	fmt.Fprint(w, `<button type="button" class="btn-sm override-delete" onclick="this.closest('.override-row').remove()">&#x2715;</button>`)

	fmt.Fprint(w, `</div>`)
}
