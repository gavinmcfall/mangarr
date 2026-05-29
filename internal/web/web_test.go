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

// fakeStore implements web.Store with canned in-memory data.
type fakeStore struct {
	series    []model.Series
	unmatched []model.Series
	activity  []model.ActivityEntry
	settings  model.Settings
	saveErr   error
	// track SetSeriesType calls
	setTypeCalls []setTypeCall
}

type setTypeCall struct {
	id int64
	ct model.ContentType
}

func (f *fakeStore) ListSeries() ([]model.Series, error) { return f.series, nil }
func (f *fakeStore) ListUnmatched() ([]model.Series, error) {
	var out []model.Series
	for _, s := range f.series {
		if s.Status == model.StatusUnmatched {
			out = append(out, s)
		}
	}
	return out, nil
}
func (f *fakeStore) ListActivity(limit int) ([]model.ActivityEntry, error) {
	if limit > len(f.activity) {
		return f.activity, nil
	}
	return f.activity[:limit], nil
}
func (f *fakeStore) GetSettings() (model.Settings, error)     { return f.settings, nil }
func (f *fakeStore) SaveSettings(s model.Settings) error      { f.settings = s; return f.saveErr }
func (f *fakeStore) SetSeriesType(id int64, ct model.ContentType) error {
	f.setTypeCalls = append(f.setTypeCalls, setTypeCall{id, ct})
	// Update in-place so apiReclassify can re-fetch the updated row.
	for i := range f.series {
		if f.series[i].ID == id {
			f.series[i].Type = ct
			f.series[i].Status = model.StatusPending
		}
	}
	return nil
}

// fakeRunner records RunOnce calls.
type fakeRunner struct{ called int }

func (r *fakeRunner) RunOnce() error { r.called++; return nil }

// newEmptyHandler builds a Handler with a store that returns no series,
// no unmatched, and no activity. Used to exercise the empty-state templates.
func newEmptyHandler() *Handler {
	st := &fakeStore{
		series:   nil,
		activity: nil,
		settings: model.Settings{
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	return NewHandler(st, &fakeRunner{})
}

// newTestHandler builds a Handler with test fixtures.
func newTestHandler() (*Handler, *fakeStore, *fakeRunner) {
	st := &fakeStore{
		series: []model.Series{
			{ID: 1, Title: "Solo Leveling", Type: model.TypeManhwa, Status: model.StatusFiled, Source: "suwayomi", ChapterCount: 10},
			{ID: 2, Title: "Berserk", Type: model.TypeManga, Status: model.StatusPending, Source: "tranga", ChapterCount: 5},
			{ID: 3, Title: "Unknown Series", Type: model.TypeUnknown, Status: model.StatusUnmatched, Source: "suwayomi", ChapterCount: 2},
		},
		activity: []model.ActivityEntry{
			{ID: 1, SeriesTitle: "Solo Leveling", Action: model.ActionFiled, Detail: "filed into /lib/Manhwa"},
		},
		settings: model.Settings{
			FileMode:           model.ModeHardlink,
			RenameScheme:       "{series}/{series} - Ch.{chapter}.cbz",
			PollMinutes:        15,
			LibraryRoots:       map[model.ContentType]string{model.TypeManhwa: "/lib/Manhwa"},
			KavitaLibIDsByType: map[model.ContentType]int64{model.TypeManhwa: 2},
		},
	}
	runner := &fakeRunner{}
	return NewHandler(st, runner), st, runner
}

// ---- HTML page smoke tests ----

func TestSeriesPageReturns200(t *testing.T) {
	h, _, _ := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/series", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("want text/html, got %q", ct)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Solo Leveling") {
		t.Fatalf("series title not in response body")
	}
}

func TestUnmatchedPageReturns200(t *testing.T) {
	h, _, _ := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/unmatched", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Unknown Series") {
		t.Fatalf("unmatched series not in body")
	}
}

func TestActivityPageReturns200(t *testing.T) {
	h, _, _ := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/activity", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Solo Leveling") {
		t.Logf("full body:\n%s", body)
		t.Fatalf("activity entry not in body")
	}
}

func TestSettingsPageReturns200(t *testing.T) {
	h, _, _ := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	// Assert the form rendered to the END (not corrupted mid-render by a
	// template panic, which previously returned 200 with a half-baked body).
	if !strings.Contains(body, `name="root_manga"`) {
		t.Fatalf("settings form did not render root_manga input; body:\n%s", body)
	}
	if !strings.Contains(body, `name="kavita_lib_manhua"`) {
		t.Fatalf("settings form did not render kavita_lib_manhua input (renders near the end)")
	}
	if !strings.Contains(body, "Save settings") {
		t.Fatalf("settings form submit button missing — render incomplete")
	}
	// Template execution errors must NOT leak into the rendered body.
	if strings.Contains(body, "executing") || strings.Contains(body, "error calling") {
		t.Fatalf("settings page contains template-error text in body:\n%s", body)
	}
}

func TestRootRedirectsToSeries(t *testing.T) {
	h, _, _ := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/series" {
		t.Fatalf("want redirect to /series, got %q", loc)
	}
}

// ---- JSON API tests ----

func TestAPIListSeriesReturnsJSON(t *testing.T) {
	h, _, _ := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/api/series", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	var list []model.Series
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("parse JSON: %v; body=%s", err, rr.Body.String())
	}
	if len(list) != 3 {
		t.Fatalf("want 3 series, got %d", len(list))
	}
}

func TestAPIListUnmatchedReturnsJSON(t *testing.T) {
	h, _, _ := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/api/unmatched", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	var list []model.Series
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("parse JSON: %v", err)
	}
	if len(list) != 1 || list[0].Title != "Unknown Series" {
		t.Fatalf("want 1 unmatched (Unknown Series), got %+v", list)
	}
}

func TestAPIListActivityReturnsJSON(t *testing.T) {
	h, _, _ := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/api/activity", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	var list []model.ActivityEntry
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("parse JSON: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 activity entry, got %d", len(list))
	}
}

func TestAPIGetSettingsReturnsJSON(t *testing.T) {
	h, _, _ := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	var s model.Settings
	if err := json.Unmarshal(rr.Body.Bytes(), &s); err != nil {
		t.Fatalf("parse JSON: %v", err)
	}
	if s.PollMinutes != 15 {
		t.Fatalf("want PollMinutes=15, got %d", s.PollMinutes)
	}
}

func TestAPIPutSettingsUpdates(t *testing.T) {
	h, st, _ := newTestHandler()
	newSettings := model.Settings{
		FileMode:     model.ModeMove,
		RenameScheme: "{series}/{series} - Ch.{chapter}.cbz",
		PollMinutes:  30,
		LibraryRoots: map[model.ContentType]string{
			model.TypeManga: "/lib/Manga",
		},
	}
	body, _ := json.Marshal(newSettings)
	req := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d; body: %s", rr.Code, rr.Body.String())
	}
	if st.settings.PollMinutes != 30 {
		t.Fatalf("settings not persisted; PollMinutes=%d", st.settings.PollMinutes)
	}
}

func TestAPIRescanCallsRunOnce(t *testing.T) {
	h, _, runner := newTestHandler()
	req := httptest.NewRequest(http.MethodPost, "/api/rescan", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d", rr.Code)
	}
	if runner.called != 1 {
		t.Fatalf("RunOnce should be called once, called=%d", runner.called)
	}
}

func TestAPIRescanWithoutRunnerReturns503(t *testing.T) {
	h := NewHandler(&fakeStore{
		series:   []model.Series{},
		activity: []model.ActivityEntry{},
		settings: model.Settings{LibraryRoots: map[model.ContentType]string{}, KavitaLibIDsByType: map[model.ContentType]int64{}},
	}, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/rescan", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rr.Code)
	}
}

func TestAPIReclassifySetsType(t *testing.T) {
	h, st, _ := newTestHandler()
	form := url.Values{"type": {"Manga"}}
	req := httptest.NewRequest(http.MethodPost, "/api/series/1/reclassify",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	if len(st.setTypeCalls) != 1 || st.setTypeCalls[0].id != 1 || st.setTypeCalls[0].ct != model.TypeManga {
		t.Fatalf("expected SetSeriesType(1, Manga), got %+v", st.setTypeCalls)
	}
}

func TestSaveSettingsFormPost(t *testing.T) {
	h, st, _ := newTestHandler()
	form := url.Values{
		"file_mode":         {"copy"},
		"rename_scheme":     {"{series}/{series} - Ch.{chapter}.cbz"},
		"poll_minutes":      {"60"},
		"kavita_base_url":   {"http://kavita:5000"},
		"kavita_api_key":    {"test-key"},
		"root_manga":        {"/lib/Manga"},
		"root_manhwa":       {"/lib/Manhwa"},
		"root_manhua":       {""},
		"kavita_lib_manga":  {"7"},
		"kavita_lib_manhwa": {"9"},
	}
	req := httptest.NewRequest(http.MethodPost, "/settings",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d; body: %s", rr.Code, rr.Body.String())
	}
	if st.settings.PollMinutes != 60 {
		t.Fatalf("want PollMinutes=60, got %d", st.settings.PollMinutes)
	}
	if st.settings.FileMode != model.ModeCopy {
		t.Fatalf("want ModeCopy, got %q", st.settings.FileMode)
	}
	if st.settings.LibraryRoots[model.TypeManga] != "/lib/Manga" {
		t.Fatalf("want manga root /lib/Manga, got %q", st.settings.LibraryRoots[model.TypeManga])
	}
	if st.settings.KavitaAPIKey != "test-key" {
		t.Fatalf("want KavitaAPIKey=test-key, got %q", st.settings.KavitaAPIKey)
	}
	if st.settings.KavitaLibIDsByType[model.TypeManga] != 7 {
		t.Fatalf("want KavitaLibIDsByType[Manga]=7, got %d", st.settings.KavitaLibIDsByType[model.TypeManga])
	}

	// Round-trip: GET /settings and assert the rendered form pre-populates
	// the values we just saved. Guards against any future template type-mismatch
	// regression — if the template panics, the inputs will be empty/missing.
	req2 := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Fatalf("follow-up GET /settings: want 200, got %d", rr2.Code)
	}
	body := rr2.Body.String()
	if !strings.Contains(body, `value="/lib/Manga"`) {
		t.Fatalf("rendered settings form does not contain saved manga root; body excerpt:\n%s",
			snippet(body, "root_manga", 120))
	}
	if !strings.Contains(body, `value="/lib/Manhwa"`) {
		t.Fatalf("rendered settings form does not contain saved manhwa root")
	}
	if !strings.Contains(body, `value="60"`) {
		t.Fatalf("rendered settings form does not contain saved poll_minutes=60")
	}
	if !strings.Contains(body, `value="7"`) {
		t.Fatalf("rendered settings form does not contain saved kavita_lib_manga=7")
	}
	if !strings.Contains(body, `value="test-key"`) {
		t.Fatalf("rendered settings form does not contain saved kavita_api_key")
	}
}

func TestSaveSettingsInvalidSchemeReturns400(t *testing.T) {
	h, st, _ := newTestHandler()
	savedBefore := st.settings.RenameScheme

	form := url.Values{
		"file_mode":         {"copy"},
		"rename_scheme":     {"{series}/{series} - Ch.{chapter}.cbz - {volume}"},
		"poll_minutes":      {"60"},
		"kavita_base_url":   {"http://kavita:5000"},
		"kavita_api_key":    {"test-key"},
		"root_manga":        {"/lib/Manga"},
		"kavita_lib_manga":  {"7"},
		"kavita_lib_manhwa": {"9"},
	}
	req := httptest.NewRequest(http.MethodPost, "/settings",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "unknown token {volume}") {
		t.Fatalf("expected validation error message in body; got:\n%s", body)
	}
	// Settings must NOT have been persisted.
	if st.settings.RenameScheme != savedBefore {
		t.Fatalf("settings were persisted despite validation failure")
	}
}

func TestAPIPutSettingsInvalidSchemeReturns400(t *testing.T) {
	h, st, _ := newTestHandler()
	savedBefore := st.settings.RenameScheme

	bad := model.Settings{
		FileMode:     model.ModeCopy,
		RenameScheme: "{series}/Ch.{chapter} - {episode}.cbz",
		PollMinutes:  30,
		LibraryRoots: map[model.ContentType]string{model.TypeManga: "/lib/Manga"},
	}
	body, _ := json.Marshal(bad)
	req := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d; body: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v; body=%s", err, rr.Body.String())
	}
	if !strings.Contains(resp["error"], "unknown token {episode}") {
		t.Fatalf("expected error about unknown token, got %q", resp["error"])
	}
	// Settings must NOT have been persisted.
	if st.settings.RenameScheme != savedBefore {
		t.Fatalf("settings were persisted despite validation failure")
	}
}

// snippet returns up to `n` chars surrounding the first occurrence of `marker` in s,
// used to make form-render failure messages legible.
func snippet(s, marker string, n int) string {
	i := strings.Index(s, marker)
	if i < 0 {
		if len(s) < n {
			return s
		}
		return s[:n]
	}
	start := i - n/2
	if start < 0 {
		start = 0
	}
	end := start + n
	if end > len(s) {
		end = len(s)
	}
	return s[start:end]
}

// ---- empty-state tests: prove the `<p class="empty">` element renders when
// the store returns no items, and the `<table>` does NOT render. Guards
// against regression of the broken `not` helper that previously hid the
// empty-state.

func TestSeriesPageEmptyStateRenders(t *testing.T) {
	h := newEmptyHandler()
	req := httptest.NewRequest(http.MethodGet, "/series", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "No series discovered yet") {
		t.Fatalf("series empty-state text not in body:\n%s", body)
	}
	if strings.Contains(body, "<table") {
		t.Fatalf("series table should NOT render with no items; body:\n%s", body)
	}
}

func TestUnmatchedPageEmptyStateRenders(t *testing.T) {
	h := newEmptyHandler()
	req := httptest.NewRequest(http.MethodGet, "/unmatched", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "No unmatched series") {
		t.Fatalf("unmatched empty-state text not in body:\n%s", body)
	}
	if strings.Contains(body, "<table") {
		t.Fatalf("unmatched table should NOT render with no items")
	}
}

func TestActivityPageEmptyStateRenders(t *testing.T) {
	h := newEmptyHandler()
	req := httptest.NewRequest(http.MethodGet, "/activity", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "No activity recorded yet") {
		t.Fatalf("activity empty-state text not in body:\n%s", body)
	}
	if strings.Contains(body, "<table") {
		t.Fatalf("activity table should NOT render with no items")
	}
}
