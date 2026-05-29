package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/model"
)

// suwayomiStubServer returns an httptest server that mimics the subset of the
// Suwayomi REST API mangarr's web handlers care about: /api/v1/category.
// The categories slice is what /api/v1/category returns. status overrides the
// HTTP status (0 = default 200 OK).
func suwayomiStubServer(t *testing.T, categories []map[string]any, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/v1/category"):
			if status != 0 {
				w.WriteHeader(status)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			body, _ := json.Marshal(categories)
			w.Write(body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// newSuwayomiHandler builds a Handler whose store carries pre-set Suwayomi
// settings. baseURL "" means leave the URL empty (feature-disabled path).
// libIDs are the saved KavitaLibIDsByType so the override-card filter has
// non-empty input.
func newSuwayomiHandler(suwayomiURL string, overrides map[int64]int64, libIDs map[model.ContentType]int64) *Handler {
	st := &fakeStore{
		settings: model.Settings{
			SuwayomiBaseURL:           suwayomiURL,
			SuwayomiAuthType:          model.SuwayomiAuthNone,
			SuwayomiCategoryOverrides: overrides,
			LibraryRoots:              map[model.ContentType]string{},
			KavitaLibIDsByType:        libIDs,
		},
	}
	return NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})
}

// ---- Truth statement: Settings page renders Suwayomi Connection panel below Kavita Connection.

func TestSettingsPageRendersSuwayomiConnectionPanel(t *testing.T) {
	h := newSuwayomiHandler("", nil, map[model.ContentType]int64{})
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Suwayomi Connection") {
		t.Fatalf("'Suwayomi Connection' heading missing from Settings page")
	}
	idxKavita := strings.Index(body, "Kavita Connection")
	idxSuwayomi := strings.Index(body, "Suwayomi Connection")
	if idxKavita < 0 || idxSuwayomi < 0 {
		t.Fatalf("expected both 'Kavita Connection' and 'Suwayomi Connection' panels; kavita=%d suwayomi=%d", idxKavita, idxSuwayomi)
	}
	if idxSuwayomi < idxKavita {
		t.Fatalf("Suwayomi Connection panel appears BEFORE Kavita Connection — want Suwayomi below Kavita")
	}
}

// ---- Truth statement: Suwayomi Connection panel includes URL, auth-type, username, password, Test button.

func TestSuwayomiConnectionPanelHasAllFields(t *testing.T) {
	h := newSuwayomiHandler("http://suwayomi.example:4567", nil, map[model.ContentType]int64{})
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	body := rr.Body.String()

	for _, want := range []string{
		`name="suwayomi_base_url"`,
		`name="suwayomi_auth_type"`,
		`name="suwayomi_username"`,
		`name="suwayomi_password"`,
		`hx-get="/api/suwayomi/test"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Suwayomi Connection panel missing field %q", want)
		}
	}
}

// ---- Truth statement: Test button + reachable Suwayomi → `connected, N categories`.

func TestSuwayomiTestEndpointReportsCategoryCount(t *testing.T) {
	cats := []map[string]any{
		{"id": 1, "name": "Korean Webtoons", "order": 1},
		{"id": 2, "name": "Japanese Manga", "order": 2},
		{"id": 3, "name": "Chinese Manhua", "order": 3},
	}
	srv := suwayomiStubServer(t, cats, 0)
	defer srv.Close()

	h := newSuwayomiHandler(srv.URL, nil, map[model.ContentType]int64{})
	req := httptest.NewRequest(http.MethodGet, "/api/suwayomi/test", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	var resp suwayomiTestResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse JSON: %v; body=%s", err, rr.Body.String())
	}
	if !resp.Ok {
		t.Errorf("want ok=true, got false; error=%s", resp.Error)
	}
	if resp.CategoryCount != 3 {
		t.Errorf("want category_count=3, got %d", resp.CategoryCount)
	}
	if resp.Error != "" {
		t.Errorf("want no error on success, got %q", resp.Error)
	}
}

// ---- Truth statement: Test button + unreachable Suwayomi → error returned, no URL or password leaked.

func TestSuwayomiTestEndpointDoesNotLeakURLOrPassword(t *testing.T) {
	// 401 unauthorized → the suwayomi client surfaces an error including the
	// base URL. Our sanitiser must strip the URL and password.
	srv := suwayomiStubServer(t, nil, http.StatusUnauthorized)
	defer srv.Close()

	// Use Basic auth so password sanitisation is also exercised.
	st := &fakeStore{
		settings: model.Settings{
			SuwayomiBaseURL:    srv.URL,
			SuwayomiAuthType:   model.SuwayomiAuthBasic,
			SuwayomiUsername:   "admin",
			SuwayomiPassword:   "test-placeholder-pw",
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})
	req := httptest.NewRequest(http.MethodGet, "/api/suwayomi/test", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200 (envelope shape), got %d; body: %s", rr.Code, rr.Body.String())
	}
	var resp suwayomiTestResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse JSON: %v; body=%s", err, rr.Body.String())
	}
	if resp.Ok {
		t.Fatalf("want ok=false on unreachable, got true")
	}
	if resp.Error == "" {
		t.Fatalf("want non-empty error message")
	}
	if strings.Contains(resp.Error, srv.URL) {
		t.Errorf("error message LEAKED Suwayomi base URL %q: %s", srv.URL, resp.Error)
	}
	if strings.Contains(resp.Error, "test-placeholder-pw") {
		t.Errorf("error message LEAKED password: %s", resp.Error)
	}
}

// ---- Truth statement: Settings page renames "Kavita Libraries" → "Library Map".

func TestSettingsPageHasLibraryMapHeading(t *testing.T) {
	h := newSuwayomiHandler("", nil, map[model.ContentType]int64{})
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	body := rr.Body.String()
	if !strings.Contains(body, "Library Map") {
		t.Errorf("'Library Map' section heading missing")
	}
	// And the AniList sub-card MUST appear inside it.
	if !strings.Contains(body, "Default: AniList Classification") {
		t.Errorf("'Default: AniList Classification' sub-card heading missing")
	}
	if !strings.Contains(body, "Suwayomi Category Overrides") {
		t.Errorf("'Suwayomi Category Overrides' sub-card heading missing")
	}
}

// ---- Truth statement: Library Map contains two sub-cards in the right order.

func TestLibraryMapSubcardOrdering(t *testing.T) {
	h := newSuwayomiHandler("", nil, map[model.ContentType]int64{
		model.TypeManga: 1, model.TypeManhwa: 2, model.TypeManhua: 3,
	})
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	body := rr.Body.String()
	idxAniList := strings.Index(body, "Default: AniList Classification")
	idxOverrides := strings.Index(body, "Suwayomi Category Overrides")
	if idxAniList < 0 || idxOverrides < 0 {
		t.Fatalf("both sub-cards must be present (anilist=%d override=%d)", idxAniList, idxOverrides)
	}
	if idxOverrides < idxAniList {
		t.Fatalf("Suwayomi Category Overrides sub-card appears BEFORE Default AniList — want AniList first (Plan B carry-forward)")
	}
}

// ---- Truth statement: Each override row offers category dropdown + library dropdown + Delete.

func TestOverrideRowsHaveDropdownsAndDelete(t *testing.T) {
	cats := []map[string]any{
		{"id": 5, "name": "Korean Webtoons", "order": 1},
	}
	srv := suwayomiStubServer(t, cats, 0)
	defer srv.Close()

	h := newSuwayomiHandler(srv.URL, map[int64]int64{5: 2}, map[model.ContentType]int64{
		model.TypeManga: 1, model.TypeManhwa: 2,
	})
	req := httptest.NewRequest(http.MethodGet, "/api/suwayomi/categories/fragment", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `name="override_category_0"`) {
		t.Errorf("override_category select missing from fragment; body:\n%s", body)
	}
	if !strings.Contains(body, `name="override_library_0"`) {
		t.Errorf("override_library select missing from fragment; body:\n%s", body)
	}
	if !strings.Contains(body, "override-delete") {
		t.Errorf("delete affordance missing from override row; body:\n%s", body)
	}
	// + Add affordance must be present so user can append.
	if !strings.Contains(body, "Add override") {
		t.Errorf("'Add override' button missing from fragment")
	}
}

// ---- Truth statement: Add appends; saving persists rows with both fields populated.

func TestSaveSettingsPersistsOverrideRows(t *testing.T) {
	st := &fakeStore{
		settings: model.Settings{
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{model.TypeManga: 1, model.TypeManhwa: 2},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})

	form := url.Values{
		"file_mode":              {"hardlink"},
		"rename_scheme":          {"{series}/{series} - Ch.{chapter}.cbz"},
		"poll_minutes":           {"15"},
		"suwayomi_base_url":      {"http://suwayomi:4567"},
		"suwayomi_auth_type":     {"basic"},
		"suwayomi_username":      {"admin"},
		"suwayomi_password":      {"test-placeholder-pw"},
		"override_category_0":    {"5"},
		"override_library_0":     {"2"},
		"override_category_1":    {"7"},
		"override_library_1":     {"1"},
		"override_category_2":    {"0"}, // dropped: empty category
		"override_library_2":     {"3"},
		"override_category_3":    {"9"},
		"override_library_3":     {"0"}, // dropped: empty library
	}
	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d; body: %s", rr.Code, rr.Body.String())
	}
	if st.settings.SuwayomiBaseURL != "http://suwayomi:4567" {
		t.Errorf("Suwayomi base URL not persisted; got %q", st.settings.SuwayomiBaseURL)
	}
	if st.settings.SuwayomiAuthType != model.SuwayomiAuthBasic {
		t.Errorf("Suwayomi auth type not persisted; got %q", st.settings.SuwayomiAuthType)
	}
	if st.settings.SuwayomiUsername != "admin" {
		t.Errorf("Suwayomi username not persisted; got %q", st.settings.SuwayomiUsername)
	}
	if st.settings.SuwayomiPassword != "test-placeholder-pw" {
		t.Errorf("Suwayomi password not persisted; got %q", st.settings.SuwayomiPassword)
	}
	if got := st.settings.SuwayomiCategoryOverrides; len(got) != 2 {
		t.Fatalf("want exactly 2 overrides persisted (rows with both fields populated), got %d: %+v", len(got), got)
	}
	if st.settings.SuwayomiCategoryOverrides[5] != 2 {
		t.Errorf("want overrides[5]=2, got %d", st.settings.SuwayomiCategoryOverrides[5])
	}
	if st.settings.SuwayomiCategoryOverrides[7] != 1 {
		t.Errorf("want overrides[7]=1, got %d", st.settings.SuwayomiCategoryOverrides[7])
	}
}

// ---- Truth statement: Row referencing a now-deleted Suwayomi category renders as Unknown (ID: N).

func TestOverrideRowWithStaleCategoryRendersUnknown(t *testing.T) {
	// Suwayomi reports only category 5 but the saved override references 99.
	cats := []map[string]any{
		{"id": 5, "name": "Korean Webtoons", "order": 1},
	}
	srv := suwayomiStubServer(t, cats, 0)
	defer srv.Close()

	h := newSuwayomiHandler(srv.URL, map[int64]int64{99: 2}, map[model.ContentType]int64{
		model.TypeManga: 1, model.TypeManhwa: 2,
	})
	req := httptest.NewRequest(http.MethodGet, "/api/suwayomi/categories/fragment", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	body := rr.Body.String()
	if !strings.Contains(body, "Unknown (ID: 99)") {
		t.Fatalf("expected 'Unknown (ID: 99)' option for stale category; body:\n%s", body)
	}
	// Row must still be editable: select stays enabled (no `disabled`)
	// and the saved option must be `selected`.
	if !strings.Contains(body, `<option value="99" selected>Unknown (ID: 99)</option>`) {
		t.Errorf("stale row should still be editable + show as selected; body:\n%s", body)
	}
}

// ---- Truth statement: Endpoints build fresh client per call.

func TestSuwayomiEndpointsUseCurrentSettings(t *testing.T) {
	// Bootstrap a handler with NO Suwayomi configured; assert /test reports
	// "not configured", then mutate Settings via the store and assert the
	// next call sees the new URL.
	cats := []map[string]any{{"id": 1, "name": "Test", "order": 1}}
	srv := suwayomiStubServer(t, cats, 0)
	defer srv.Close()

	st := &fakeStore{
		settings: model.Settings{
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})

	// First call: not configured.
	rr1 := httptest.NewRecorder()
	h.ServeHTTP(rr1, httptest.NewRequest(http.MethodGet, "/api/suwayomi/test", nil))
	var resp1 suwayomiTestResponse
	json.Unmarshal(rr1.Body.Bytes(), &resp1)
	if resp1.Ok {
		t.Fatalf("first call should fail (not configured), got ok=true")
	}
	if !strings.Contains(resp1.Error, "not configured") {
		t.Errorf("want 'not configured' error, got %q", resp1.Error)
	}

	// Mutate settings via the store (simulates the user saving the form).
	st.settings.SuwayomiBaseURL = srv.URL
	st.settings.SuwayomiAuthType = model.SuwayomiAuthNone

	// Second call: should now succeed against the stub.
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, httptest.NewRequest(http.MethodGet, "/api/suwayomi/test", nil))
	var resp2 suwayomiTestResponse
	json.Unmarshal(rr2.Body.Bytes(), &resp2)
	if !resp2.Ok {
		t.Fatalf("second call should succeed after Settings updated; got error=%q", resp2.Error)
	}
	if resp2.CategoryCount != 1 {
		t.Errorf("want category_count=1, got %d", resp2.CategoryCount)
	}
}

// ---- Truth statement: Empty Suwayomi base URL → inline prompt, no outbound call.

func TestEmptySuwayomiURLDoesNotCallNetwork(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Note: settings has the kavita libs picked but NO Suwayomi URL.
	// The fragment endpoint must render the configure-first prompt and
	// MUST NOT hit the network.
	h := newSuwayomiHandler("", map[int64]int64{}, map[model.ContentType]int64{
		model.TypeManga: 1, model.TypeManhwa: 2,
	})
	req := httptest.NewRequest(http.MethodGet, "/api/suwayomi/categories/fragment", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Set the base URL") {
		t.Errorf("expected configure-first prompt; body:\n%s", body)
	}
	if called {
		t.Errorf("network call made even though Suwayomi base URL is empty")
	}
}

// ---- Truth statement: empty KavitaLibIDsByType → inline prompt to configure AniList first.

func TestNoAniListClassificationShowsConfigureFirstPrompt(t *testing.T) {
	cats := []map[string]any{{"id": 1, "name": "Test", "order": 1}}
	srv := suwayomiStubServer(t, cats, 0)
	defer srv.Close()

	h := newSuwayomiHandler(srv.URL, nil, map[model.ContentType]int64{})
	req := httptest.NewRequest(http.MethodGet, "/api/suwayomi/categories/fragment", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	body := rr.Body.String()
	if !strings.Contains(body, "Configure AniList") {
		t.Errorf("expected 'Configure AniList' prompt when KavitaLibIDsByType empty; body:\n%s", body)
	}
}

// ---- Truth statement: override-row Kavita library dropdown filtered to KavitaLibIDsByType.

func TestOverrideRowLibraryDropdownFilteredToContentTypes(t *testing.T) {
	cats := []map[string]any{{"id": 5, "name": "Korean Webtoons", "order": 1}}
	srv := suwayomiStubServer(t, cats, 0)
	defer srv.Close()

	h := newSuwayomiHandler(srv.URL, nil, map[model.ContentType]int64{
		model.TypeManga:  10,
		model.TypeManhwa: 20,
		model.TypeManhua: 30,
	})

	// Force the page to surface a row so we can inspect the lib dropdown.
	// Easiest: GET /settings (renders with an empty Add button + template).
	req := httptest.NewRequest(http.MethodGet, "/api/suwayomi/categories/fragment", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	body := rr.Body.String()
	// Library options 10, 20, 30 must all appear in the template.
	for _, want := range []string{`value="10"`, `value="20"`, `value="30"`} {
		if !strings.Contains(body, want) {
			t.Errorf("library option %q missing from override fragment", want)
		}
	}
	// A library NOT in KavitaLibIDsByType (e.g. 999) must NOT appear.
	if strings.Contains(body, `value="999"`) {
		t.Errorf("unexpected library option 999 in override fragment — must be filtered to KavitaLibIDsByType")
	}
}

// ---- formatVia coverage ----

func TestFormatVia(t *testing.T) {
	cats := map[int64]string{5: "Korean Webtoons"}
	tests := []struct {
		in   string
		want string
	}{
		{"", "—"},
		{"unmatched", "Unmatched"},
		{"anilist:JP", "AniList (JP)"},
		{"anilist:KR", "AniList (KR)"},
		{"anilist:", "AniList"},
		{"suwayomi-override:category=5", "Korean Webtoons"},
		{"suwayomi-override:category=99", "Unknown (ID: 99)"},
		{"suwayomi-override:category=bad", "suwayomi-override:category=bad"},
	}
	for _, tc := range tests {
		got := formatVia(tc.in, cats)
		if got != tc.want {
			t.Errorf("formatVia(%q): want %q, got %q", tc.in, tc.want, got)
		}
	}
}

// parseSuwayomiOverrides direct test — covers the row-pairing logic.
func TestParseSuwayomiOverrides(t *testing.T) {
	form := map[string][]string{
		"override_category_0":    {"5"},
		"override_library_0":     {"100"},
		"override_category_1":    {"7"},
		"override_library_1":     {"200"},
		"override_category_99":   {"0"}, // dropped
		"override_library_99":    {"300"},
		"override_category_100":  {"50"},
		"override_library_100":   {"0"}, // dropped
		"override_category_xyz":  {"60"},
		"override_library_xyz":   {"400"},
		"unrelated_field":        {"ignored"},
	}
	got := parseSuwayomiOverrides(form)
	want := map[int64]int64{5: 100, 7: 200, 60: 400}
	if len(got) != len(want) {
		t.Fatalf("want %d entries, got %d: %+v", len(want), len(got), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("want [%d]=%d, got %d", k, v, got[k])
		}
	}
}

// Activity page renders Via labels (joined against Suwayomi when reachable).
func TestActivityPageRendersViaLabels(t *testing.T) {
	cats := []map[string]any{
		{"id": 5, "name": "Korean Webtoons", "order": 1},
	}
	srv := suwayomiStubServer(t, cats, 0)
	defer srv.Close()

	st := &fakeStore{
		activity: []model.ActivityEntry{
			{ID: 1, SeriesTitle: "Solo Leveling", Action: model.ActionFiled, Via: "suwayomi-override:category=5"},
			{ID: 2, SeriesTitle: "Berserk", Action: model.ActionFiled, Via: "anilist:JP"},
			{ID: 3, SeriesTitle: "Mystery", Action: model.ActionUnmatched, Via: "unmatched"},
			{ID: 4, SeriesTitle: "Legacy Row", Action: model.ActionFiled, Via: ""},
		},
		settings: model.Settings{
			SuwayomiBaseURL:    srv.URL,
			SuwayomiAuthType:   model.SuwayomiAuthNone,
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})

	req := httptest.NewRequest(http.MethodGet, "/activity", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{"Korean Webtoons", "AniList (JP)", "Unmatched"} {
		if !strings.Contains(body, want) {
			t.Errorf("activity page missing Via label %q; body excerpt:\n%s", want, snippet(body, "Solo", 400))
		}
	}
	// The "Via" header must exist.
	if !strings.Contains(body, "<th>Via</th>") {
		t.Errorf("activity table missing 'Via' header column")
	}
}

// Ensure the suwayomi endpoint returns 503 if Kavita library JSON contains an
// item with an integer-like ID field (guard against an int/int64 mismatch on
// the wire). Sanity check, mostly for parser stability.
func TestSuwayomiCategoriesEndpoint(t *testing.T) {
	cats := []map[string]any{
		{"id": 1, "name": "A", "order": 0},
		{"id": 2, "name": "B", "order": 1},
	}
	srv := suwayomiStubServer(t, cats, 0)
	defer srv.Close()
	h := newSuwayomiHandler(srv.URL, nil, map[model.ContentType]int64{})
	req := httptest.NewRequest(http.MethodGet, "/api/suwayomi/categories", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	var resp suwayomiCategoriesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse JSON: %v; body=%s", err, rr.Body.String())
	}
	if len(resp.Categories) != 2 {
		t.Fatalf("want 2 categories, got %d", len(resp.Categories))
	}
	if resp.Categories[0].Name != "A" {
		t.Errorf("want first category 'A', got %q", resp.Categories[0].Name)
	}
}

