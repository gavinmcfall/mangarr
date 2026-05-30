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
// from override_category_<idx> + override_binding_<idx> pairs. Indices need
// not be contiguous — JS may have removed rows mid-edit. Any pair with a
// zero/empty category OR binding ID is dropped (the "Add" → "Delete"
// lifecycle saves clean state without JS having to scrub hidden inputs).
//
// Plan B Task 5 changed the write target in saveSettings to
// SuwayomiCategoryBindings (v2 — values are Library Binding IDs); this
// parser is field-name-keyed (override_binding_<idx>) and routing-target-
// agnostic. Caller chooses which Settings map to assign the result into.
//
// Returns nil when no valid overrides are found, so the round-trip JSON
// stays compact when the feature is unused.
//
// Determinism on duplicate categories: indices are walked in ascending
// numeric order (non-numeric indices sort lexicographically and run last),
// and the LAST-INDEX-WINS rule applies. This matches the UI semantic of
// "the row I most recently added is the one that takes effect" — appending
// a second override for category 5 after seeing the first one rendered
// will save the appended row.
func parseSuwayomiOverrides(form map[string][]string) map[int64]int64 {
	// Index field names by suffix so we can pair them up regardless of
	// which JS-counter idx was used.
	cats := map[string]string{}
	bindings := map[string]string{}
	for k, vs := range form {
		if len(vs) == 0 {
			continue
		}
		switch {
		case strings.HasPrefix(k, "override_category_"):
			cats[strings.TrimPrefix(k, "override_category_")] = vs[0]
		case strings.HasPrefix(k, "override_binding_"):
			bindings[strings.TrimPrefix(k, "override_binding_")] = vs[0]
		}
	}
	// Collect the suffix keys and sort them with a numeric-aware comparator
	// so iteration order is deterministic regardless of Go's map-iteration
	// randomisation.
	keys := make([]string, 0, len(cats))
	for k := range cats {
		keys = append(keys, k)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		ai, errA := strconv.Atoi(keys[i])
		bi, errB := strconv.Atoi(keys[j])
		if errA == nil && errB == nil {
			return ai < bi
		}
		// Mixed / non-numeric: numerics first (sorted), then lex.
		if errA == nil {
			return true
		}
		if errB == nil {
			return false
		}
		return keys[i] < keys[j]
	})

	out := map[int64]int64{}
	for _, idx := range keys {
		bindingRaw, ok := bindings[idx]
		if !ok {
			continue
		}
		catID, err := strconv.ParseInt(strings.TrimSpace(cats[idx]), 10, 64)
		if err != nil || catID <= 0 {
			continue
		}
		bindingID, err := strconv.ParseInt(strings.TrimSpace(bindingRaw), 10, 64)
		if err != nil || bindingID <= 0 {
			continue
		}
		// LAST-INDEX-WINS: later-index rows overwrite earlier rows that
		// reference the same Suwayomi category. Plain map assignment
		// achieves this because we walk indices in ascending order.
		out[catID] = bindingID
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

// libraryMapData is the view-model shared by /settings and the
// /api/suwayomi/categories/fragment HTMX swap target. Both paths build it
// via buildLibraryMapData → the rendered HTML stays byte-for-byte identical
// across both routes (single source of truth, fixes the Refresh-button
// label-regression that surfaced in code review).
type libraryMapData struct {
	SuwayomiCategories []suwayomi.Category
	OverrideLibChoices []overrideLibraryChoice
	OverrideRows       []overrideRowView
	// Bindings carries every v2 Library Binding for the override row's
	// right-hand dropdown. Plan B widens the dropdown from "3 primary
	// content-type Kavita libraries" (the Library Map Plan C constraint
	// dissolved by Plan A) to every persisted binding. The fragment
	// endpoint and /settings both populate this from store.ListBindings.
	Bindings []model.Binding
	// OverrideUnconfigured is true when KavitaLibIDsByType is empty →
	// override card renders the "configure AniList first" prompt and no
	// rows.
	OverrideUnconfigured bool
	// OverrideSuwayomiUnconfigured is true when Suwayomi base URL is
	// empty → override card renders the "configure Suwayomi" prompt
	// AND the saved rows (so the user can see what is persisted).
	OverrideSuwayomiUnconfigured bool
	// OverrideError carries a Suwayomi fetch error (sanitised) when
	// Suwayomi is configured but unreachable.
	OverrideError string
}

// buildLibraryMapData assembles the override-card view-model. It is the
// single source of truth for what /settings AND the fragment endpoint
// render. Both call sites pass through here so the Kavita library-name
// upgrade (and any future enrichment) cannot drift between them.
//
// Network calls inside (Suwayomi categories + Kavita library names) are
// bounded by the supplied context. Errors are surfaced via OverrideError /
// OverrideSuwayomiUnconfigured rather than returned, so the caller can
// always render the card even when the upstreams are down.
func buildLibraryMapData(ctx context.Context, settings model.Settings, bindings []model.Binding) libraryMapData {
	out := libraryMapData{Bindings: bindings}
	libChoices := overrideLibraryChoices(settings)

	// Empty KavitaLibIDsByType = nothing to route to. Render the
	// configure-first prompt; don't fetch anything from Suwayomi.
	if len(libChoices) == 0 {
		out.OverrideUnconfigured = true
		return out
	}

	// Upgrade override-library labels with real Kavita names where
	// possible. Best-effort: failure leaves placeholder labels in place.
	libChoices = resolveOverrideLibraryNames(ctx, settings, libChoices)
	out.OverrideLibChoices = libChoices

	if strings.TrimSpace(settings.SuwayomiBaseURL) == "" {
		out.OverrideSuwayomiUnconfigured = true
		// Render saved rows even with empty SuwayomiCategories — they
		// fall back to "Unknown (ID: N)" so the user sees what's saved.
		out.OverrideRows = buildOverrideRows(settings.SuwayomiCategoryOverrides, nil, libChoices)
		return out
	}

	client, ok := newSuwayomiClient(settings)
	if !ok {
		out.OverrideSuwayomiUnconfigured = true
		out.OverrideRows = buildOverrideRows(settings.SuwayomiCategoryOverrides, nil, libChoices)
		return out
	}
	cats, err := client.ListCategories(ctx)
	if err != nil {
		out.OverrideError = sanitiseSuwayomiError(err, settings)
		out.OverrideRows = buildOverrideRows(settings.SuwayomiCategoryOverrides, nil, libChoices)
		return out
	}
	out.SuwayomiCategories = cats
	out.OverrideRows = buildOverrideRows(settings.SuwayomiCategoryOverrides, cats, libChoices)
	return out
}

// apiSuwayomiCategoriesFragment handles GET /api/suwayomi/categories/fragment.
//
// Returns the override-card body: one row per entry in
// SuwayomiCategoryOverrides plus an empty "Add" row template (rendered via
// JS on click). Renders via the shared override-fragment template so the
// output is byte-for-byte identical to what the Settings page renders on
// initial GET. Always returns 200 with HTML so HTMX can swap the result
// cleanly even when Suwayomi is unreachable.
func (h *Handler) apiSuwayomiCategoriesFragment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	settings, err := h.store.GetSettings()
	if err != nil {
		fmt.Fprintf(w, `<div class="form-error">Cannot read settings: %s</div>`, html(err.Error()))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	// Best-effort: a binding-load error here renders the override card with
	// an empty Bindings slice (dropdown gets only "(select binding)"). The
	// fragment must never fail-hard on a store glitch — the card still has
	// useful diagnostic output even with an empty dropdown.
	bindings, _ := h.store.ListBindings()
	data := buildLibraryMapData(ctx, settings, bindings)
	if err := h.renderTemplate(w, "override-fragment", "override-fragment", data); err != nil {
		fmt.Fprintf(w, `<div class="form-error">render error: %s</div>`, html(err.Error()))
	}
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

