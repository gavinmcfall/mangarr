package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gavinmcfall/mangarr/internal/dbbackup"
	"github.com/gavinmcfall/mangarr/internal/diskspace"
	"github.com/gavinmcfall/mangarr/internal/filer"
	"github.com/gavinmcfall/mangarr/internal/health"
	"github.com/gavinmcfall/mangarr/internal/kavita"
	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/poller"
	"github.com/gavinmcfall/mangarr/internal/suwayomi"
	"github.com/gavinmcfall/mangarr/internal/tasks"
	_ "modernc.org/sqlite"
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
	// track SetSeriesManualBinding calls (v2 reclassify)
	setManualBindingCalls []setManualBindingCall
	// Plan B: v2 data model
	bindings []model.Binding
	rules    []model.ClassificationRule
	// Plan A T13: bulk-download surfaces
	bulkJobs        []model.BulkJob
	savedBulkJobs   []model.BulkJob
	savedChapterIDs []int64
	libraryCache    map[int64]model.LibraryCacheEntry
	// Plan A T14: pause/resume/delete tracking
	bulkStatusUpdates []bulkStatusCall
	bulkClearBackoff  []int64
	bulkDeletes       []int64
	// Plan B T1: library_cache writes (POST /api/library/sync). Append-only
	// so tests can assert ordering matches the slice the Suwayomi client
	// returned.
	savedLibraryEntries []model.LibraryCacheEntry
	// callOrder records the sequence of Store mutations for tests that
	// pin ordering invariants (e.g. "ClearBulkJobBackoff must run BEFORE
	// UpdateBulkJobStatus on resume from errored"). Append-only.
	callOrder []string
}

type bulkStatusCall struct {
	id     int64
	status model.BulkJobStatus
}

type setTypeCall struct {
	id int64
	ct model.ContentType
}

type setManualBindingCall struct {
	id        int64
	bindingID *int64
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
func (f *fakeStore) SetSeriesManualBinding(id int64, bindingID *int64) error {
	f.setManualBindingCalls = append(f.setManualBindingCalls, setManualBindingCall{id, bindingID})
	for i := range f.series {
		if f.series[i].ID == id {
			f.series[i].ManualBindingID = bindingID
			f.series[i].Status = model.StatusPending
		}
	}
	return nil
}
func (f *fakeStore) ListBindings() ([]model.Binding, error)         { return f.bindings, nil }
func (f *fakeStore) ListRules() ([]model.ClassificationRule, error) { return f.rules, nil }
func (f *fakeStore) SaveBindings(in []model.Binding) error {
	// Assign a synthetic ID to any incoming row with ID==0 so subsequent
	// lookups (e.g. tests checking FK consistency) have something to bind to.
	// Real *store.Store does the same via INSERT-RETURNING.
	nextID := int64(1)
	for _, b := range f.bindings {
		if b.ID >= nextID {
			nextID = b.ID + 1
		}
	}
	out := make([]model.Binding, 0, len(in))
	for _, b := range in {
		if b.ID == 0 {
			b.ID = nextID
			nextID++
		}
		out = append(out, b)
	}
	f.bindings = out
	return nil
}
func (f *fakeStore) SaveRules(in []model.ClassificationRule) error {
	nextID := int64(1)
	for _, r := range f.rules {
		if r.ID >= nextID {
			nextID = r.ID + 1
		}
	}
	out := make([]model.ClassificationRule, 0, len(in))
	for _, r := range in {
		if r.ID == 0 {
			r.ID = nextID
			nextID++
		}
		out = append(out, r)
	}
	f.rules = out
	return nil
}

// --- Plan A T13: bulk-download Store surfaces ---

func (f *fakeStore) ListBulkJobs(s model.BulkJobStatus) ([]model.BulkJob, error) {
	if s == "" {
		return f.bulkJobs, nil
	}
	var out []model.BulkJob
	for _, j := range f.bulkJobs {
		if j.Status == s {
			out = append(out, j)
		}
	}
	return out, nil
}

func (f *fakeStore) SaveBulkJob(in model.BulkJob) (int64, error) {
	in.ID = int64(len(f.savedBulkJobs) + 1)
	f.savedBulkJobs = append(f.savedBulkJobs, in)
	return in.ID, nil
}

func (f *fakeStore) BatchInsertBulkJobChapters(jobID int64, ids []int64) error {
	f.savedChapterIDs = append(f.savedChapterIDs, ids...)
	return nil
}

func (f *fakeStore) GetLibraryCacheEntry(mangaID int64) (model.LibraryCacheEntry, error) {
	if e, ok := f.libraryCache[mangaID]; ok {
		return e, nil
	}
	return model.LibraryCacheEntry{}, sql.ErrNoRows
}

// ListLibraryCacheEntries returns every cached library entry sorted by
// title so the /library page test assertions are deterministic regardless
// of Go map iteration order.
func (f *fakeStore) ListLibraryCacheEntries() ([]model.LibraryCacheEntry, error) {
	out := make([]model.LibraryCacheEntry, 0, len(f.libraryCache))
	for _, e := range f.libraryCache {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out, nil
}

// SaveLibraryCacheEntry appends the entry to savedLibraryEntries (for
// ordering assertions) and upserts into libraryCache (so subsequent
// GetLibraryCacheEntry calls see the write).
func (f *fakeStore) SaveLibraryCacheEntry(in model.LibraryCacheEntry) error {
	f.savedLibraryEntries = append(f.savedLibraryEntries, in)
	if f.libraryCache == nil {
		f.libraryCache = map[int64]model.LibraryCacheEntry{}
	}
	f.libraryCache[in.MangaID] = in
	return nil
}

// --- Plan A T14: pause/resume/delete mutation surfaces ---

func (f *fakeStore) UpdateBulkJobStatus(id int64, s model.BulkJobStatus) error {
	f.bulkStatusUpdates = append(f.bulkStatusUpdates, bulkStatusCall{id, s})
	f.callOrder = append(f.callOrder, fmt.Sprintf("status:%d:%s", id, s))
	// Plan B T4: also mutate bulkJobs in-place so a subsequent
	// GetBulkJob re-read reflects the new status. The append-only
	// trackers above remain for Plan A T14's assertions; this update
	// is additive.
	for i := range f.bulkJobs {
		if f.bulkJobs[i].ID == id {
			f.bulkJobs[i].Status = s
		}
	}
	return nil
}

// GetBulkJob returns a single job by ID — Plan B T4's HX-Request branch
// in apiDownloadsAction re-reads the row after the status flip so the
// rendered <tr> reflects the mutation.
func (f *fakeStore) GetBulkJob(id int64) (model.BulkJob, error) {
	for _, j := range f.bulkJobs {
		if j.ID == id {
			return j, nil
		}
	}
	return model.BulkJob{}, sql.ErrNoRows
}

func (f *fakeStore) ClearBulkJobBackoff(id int64) error {
	f.bulkClearBackoff = append(f.bulkClearBackoff, id)
	f.callOrder = append(f.callOrder, fmt.Sprintf("clear:%d", id))
	return nil
}

func (f *fakeStore) DeleteBulkJob(id int64) error {
	f.bulkDeletes = append(f.bulkDeletes, id)
	f.callOrder = append(f.callOrder, fmt.Sprintf("delete:%d", id))
	return nil
}

// fakeSuwayomi implements web.SuwayomiClient for tests. Per-manga chapter
// IDs are configured via chaptersForManga; ListChapters returns each as
// a Chapter{IsDownloaded:false}. A nil/empty slice for a manga ID models
// the "no missing chapters" edge case POST /api/bulk uses to skip job
// creation.
type fakeSuwayomi struct {
	chaptersForManga map[int64][]int64
	// Plan B T2: per-chapter isDownloaded flag returned by ListChapters.
	// Looked up by chapter ID; missing/false entries leave IsDownloaded
	// at its zero value, which preserves the Plan A T13 behaviour where
	// every chapter looked undownloaded.
	chaptersDownloaded map[int64]bool
	// Plan B T1: library entries returned by ListLibraryWithCategories
	// (used by POST /api/library/sync). A nil/empty slice models the
	// "operator has no series in Suwayomi yet" edge case.
	libraryEntries []suwayomi.Manga
}

func (f *fakeSuwayomi) ListChapters(_ context.Context, mangaID int64) ([]suwayomi.Chapter, error) {
	if f == nil {
		return nil, nil
	}
	out := make([]suwayomi.Chapter, 0)
	for _, id := range f.chaptersForManga[mangaID] {
		out = append(out, suwayomi.Chapter{ID: id, IsDownloaded: f.chaptersDownloaded[id]})
	}
	return out, nil
}

// ListLibraryWithCategories returns the seeded libraryEntries slice
// unchanged so tests can assert against the same order the handler iterates.
func (f *fakeSuwayomi) ListLibraryWithCategories(_ context.Context) ([]suwayomi.Manga, error) {
	if f == nil {
		return nil, nil
	}
	return f.libraryEntries, nil
}

// fakeRunner records RunOnce calls.
type fakeRunner struct{ called int }

func (r *fakeRunner) RunOnce(_ context.Context) error { r.called++; return nil }

// fakeSeriesFiler records FileOne calls and optionally returns an error.
type fakeSeriesFiler struct {
	calls []fileOneCall
	err   error
}

type fileOneCall struct {
	seriesID int64
	ct       model.ContentType
}

func (f *fakeSeriesFiler) FileOne(ctx context.Context, seriesID int64, ct model.ContentType) error {
	f.calls = append(f.calls, fileOneCall{seriesID, ct})
	return f.err
}

// fakeTaskRegistry is a minimal in-process TaskRegistry for tests.
type fakeTaskRegistry struct {
	mu      sync.Mutex
	entries map[string]*fakeTaskEntry
}

type fakeTaskEntry struct {
	info  tasks.Info
	runFn func(ctx context.Context) error
}

func newFakeTaskRegistry() *fakeTaskRegistry {
	return &fakeTaskRegistry{entries: make(map[string]*fakeTaskEntry)}
}

func (f *fakeTaskRegistry) register(id, name string, fn func(ctx context.Context) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[id] = &fakeTaskEntry{
		info:  tasks.Info{ID: id, Name: name},
		runFn: fn,
	}
}

func (f *fakeTaskRegistry) List() []tasks.Info {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]tasks.Info, 0, len(f.entries))
	for _, e := range f.entries {
		out = append(out, e.info)
	}
	return out
}

func (f *fakeTaskRegistry) Get(id string) (tasks.Info, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[id]
	if !ok {
		return tasks.Info{}, false
	}
	return e.info, true
}

func (f *fakeTaskRegistry) RunNow(ctx context.Context, id string) (tasks.Info, error) {
	f.mu.Lock()
	e, ok := f.entries[id]
	f.mu.Unlock()
	if !ok {
		return tasks.Info{}, errors.New("task not found: " + id)
	}
	runErr := e.runFn(ctx)
	f.mu.Lock()
	e.info.LastRun = time.Now()
	if runErr != nil {
		e.info.LastErr = runErr.Error()
	} else {
		e.info.LastErr = ""
	}
	snap := e.info
	f.mu.Unlock()
	return snap, runErr
}

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
	return NewHandler(HandlerOpts{
		Store:                   st,
		Runner:                  &fakeRunner{},
		RecycleBinPath:          "/config/recycle-bin",
		RecycleBinRetentionDays: 7,
	})
}

// newTestHandler builds a Handler with test fixtures.
//
// The 3rd return value is the Suwayomi fake, not the Runner — bulk_test.go
// uses `h, st, sw := newTestHandler()` to seed per-manga chapter lists for
// the POST /api/bulk path. Tests that need the runner use
// newTestHandlerWithRunner (one of the two remaining callsites).
func newTestHandler() (*Handler, *fakeStore, *fakeSuwayomi) {
	h, st, sw, _ := newTestHandlerFull()
	return h, st, sw
}

// newTestHandlerWithRunner is the legacy 3-tuple shape that exposes the
// Runner instead of Suwayomi. Used by TestAPIRescanCallsRunOnce which
// needs to assert RunOnce got called exactly once.
func newTestHandlerWithRunner() (*Handler, *fakeStore, *fakeRunner) {
	h, st, _, runner := newTestHandlerFull()
	return h, st, runner
}

// newTestHandlerFull is the canonical builder: every fixture is exposed so
// helpers above can pick the slice they need. Kept internal so test files
// stay terse — they always go through one of the two public helpers.
func newTestHandlerFull() (*Handler, *fakeStore, *fakeSuwayomi, *fakeRunner) {
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
	sw := &fakeSuwayomi{}
	h := NewHandler(HandlerOpts{
		Store:                   st,
		Runner:                  runner,
		Suwayomi:                sw,
		RecycleBinPath:          "/config/recycle-bin",
		RecycleBinRetentionDays: 7,
	})
	return h, st, sw, runner
}

// newTestHandlerWithRegistry builds a Handler with a seeded task registry.
func newTestHandlerWithRegistry() (*Handler, *fakeStore, *fakeRunner, *fakeTaskRegistry) {
	_, st, runner := newTestHandlerWithRunner()
	reg := newFakeTaskRegistry()
	reg.register("poll-scan", "Poll Scan", func(ctx context.Context) error {
		runner.called++
		return nil
	})
	h := NewHandler(HandlerOpts{
		Store:                   st,
		Runner:                  runner,
		TaskReg:                 reg,
		RecycleBinPath:          "/config/recycle-bin",
		RecycleBinRetentionDays: 7,
	})
	return h, st, runner, reg
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
	// Plan B Task 8: the v1 root_<type>/kavita_lib_<type> inputs are gone;
	// assert on Plan B surfaces (Library Bindings card + Save button) instead.
	if !strings.Contains(body, "Library Bindings") {
		t.Fatalf("settings form did not render Library Bindings card; body:\n%s", body)
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
	h, _, runner := newTestHandlerWithRunner()
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
	h := NewHandler(HandlerOpts{
		Store: &fakeStore{
			series:   []model.Series{},
			activity: []model.ActivityEntry{},
			settings: model.Settings{LibraryRoots: map[model.ContentType]string{}, KavitaLibIDsByType: map[model.ContentType]int64{}},
		},
		RecycleBinPath:          "/config/recycle-bin",
		RecycleBinRetentionDays: 7,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/rescan", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rr.Code)
	}
}

func TestAPIReclassifySetsManualBinding(t *testing.T) {
	h, st, _ := newTestHandler()
	// Seed a binding the handler will validate against.
	st.bindings = []model.Binding{{ID: 7, Name: "Manga", LibraryRoot: "/m", KavitaLibID: 1}}

	form := url.Values{"binding_id": {"7"}}
	req := httptest.NewRequest(http.MethodPost, "/api/series/1/reclassify",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	if len(st.setManualBindingCalls) != 1 {
		t.Fatalf("expected one SetSeriesManualBinding call, got %d: %+v",
			len(st.setManualBindingCalls), st.setManualBindingCalls)
	}
	got := st.setManualBindingCalls[0]
	if got.id != 1 || got.bindingID == nil || *got.bindingID != 7 {
		t.Errorf("expected SetSeriesManualBinding(1, *7), got id=%d bindingID=%v",
			got.id, got.bindingID)
	}
}

// TestAPIReclassifyZeroClearsOverride pins that binding_id=0 (the "No
// override" option) sends a nil pointer through, which the store
// contract treats as "clear the override; classify normally".
func TestAPIReclassifyZeroClearsOverride(t *testing.T) {
	h, st, _ := newTestHandler()
	form := url.Values{"binding_id": {"0"}}
	req := httptest.NewRequest(http.MethodPost, "/api/series/1/reclassify",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	if len(st.setManualBindingCalls) != 1 || st.setManualBindingCalls[0].bindingID != nil {
		t.Errorf("expected SetSeriesManualBinding(_, nil), got %+v", st.setManualBindingCalls)
	}
}

// TestAPIReclassifyRejectsUnknownBinding pins that an operator
// can't pin a series at a binding ID that doesn't exist (deleted
// since page render, or a manually-crafted POST). Returns 400 and
// does NOT call the store.
func TestAPIReclassifyRejectsUnknownBinding(t *testing.T) {
	h, st, _ := newTestHandler()
	st.bindings = []model.Binding{{ID: 7, Name: "Manga"}}
	form := url.Values{"binding_id": {"999"}}
	req := httptest.NewRequest(http.MethodPost, "/api/series/1/reclassify",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d; body: %s", rr.Code, rr.Body.String())
	}
	if len(st.setManualBindingCalls) != 0 {
		t.Errorf("expected NO SetSeriesManualBinding call on invalid binding; got %+v",
			st.setManualBindingCalls)
	}
}

func TestSaveSettingsFormPost(t *testing.T) {
	h, st, _ := newTestHandler()
	// Stub Kavita server so the round-trip GET /settings doesn't block on a real
	// DNS lookup for the placeholder hostname. Returns empty library list = OK.
	stub := kavitaStubServer(t, nil, 0, 0)
	defer stub.Close()
	// Plan B Task 8: the v1 root_<type> + kavita_lib_<type> form fields are
	// gone — the Library Bindings card supersedes them. This POST exercises
	// the remaining settings (Kavita connection, file mode, rename scheme,
	// poll minutes) which still round-trip through saveSettings.
	form := url.Values{
		"file_mode":       {"copy"},
		"rename_scheme":   {"{series}/{series} - Ch.{chapter}.cbz"},
		"poll_minutes":    {"60"},
		"kavita_base_url": {stub.URL},
		"kavita_api_key":  {"test-key"},
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
	if st.settings.KavitaAPIKey != "test-key" {
		t.Fatalf("want KavitaAPIKey=test-key, got %q", st.settings.KavitaAPIKey)
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
	if !strings.Contains(body, `value="60"`) {
		t.Fatalf("rendered settings form does not contain saved poll_minutes=60")
	}
	if !strings.Contains(body, `value="test-key"`) {
		t.Fatalf("rendered settings form does not contain saved kavita_api_key")
	}
}

func TestSaveSettingsInvalidSchemeReturns400(t *testing.T) {
	h, st, _ := newTestHandler()
	savedBefore := st.settings.RenameScheme

	form := url.Values{
		"file_mode":       {"copy"},
		"rename_scheme":   {"{series}/{series} - Ch.{chapter}.cbz - {volume}"},
		"poll_minutes":    {"60"},
		"kavita_base_url": {"http://kavita:5000"},
		"kavita_api_key":  {"test-key"},
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

// ---- backup API tests ----

// newBackupHandler creates a Handler wired with a real in-memory SQLite backup function
// and a temp backup directory.
func newBackupHandler(t *testing.T) (*Handler, string) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE t (v TEXT); INSERT INTO t VALUES ('test')`); err != nil {
		t.Fatalf("seed db: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	dir := t.TempDir()
	cfg := BackupConfig{Dir: dir, RetentionDays: 14, IntervalHours: 24}
	backupFn := func() (dbbackup.Entry, error) {
		path, err := dbbackup.Backup(db, dir, time.Now())
		if err != nil {
			return dbbackup.Entry{}, err
		}
		entries, err := dbbackup.List(dir)
		if err != nil {
			return dbbackup.Entry{}, err
		}
		for _, e := range entries {
			if e.Path == path {
				return e, nil
			}
		}
		return dbbackup.Entry{Name: path}, nil
	}
	st := &fakeStore{
		settings: model.Settings{
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	h := NewHandler(HandlerOpts{
		Store:  st,
		Runner: &fakeRunner{},
		Backup: BackupOpts{Config: cfg, Fn: backupFn},
	})
	return h, dir
}

func TestAPIListBackupsReturnsJSON(t *testing.T) {
	h, _ := newBackupHandler(t)

	// Run a backup first so there is at least one entry.
	runReq := httptest.NewRequest(http.MethodPost, "/api/backups/run", nil)
	runRR := httptest.NewRecorder()
	h.ServeHTTP(runRR, runReq)
	if runRR.Code != http.StatusOK {
		t.Fatalf("POST /api/backups/run: want 200, got %d; body: %s", runRR.Code, runRR.Body.String())
	}

	// Now list backups.
	req := httptest.NewRequest(http.MethodGet, "/api/backups", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/backups: want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	var entries []dbbackup.Entry
	if err := json.Unmarshal(rr.Body.Bytes(), &entries); err != nil {
		t.Fatalf("parse JSON: %v; body=%s", err, rr.Body.String())
	}
	if len(entries) < 1 {
		t.Fatalf("want at least 1 backup entry, got %d", len(entries))
	}
	if entries[0].Name == "" {
		t.Fatal("entry has empty Name")
	}
}

func TestAPIDownloadBackupServesFile(t *testing.T) {
	h, _ := newBackupHandler(t)

	// Run a backup to create the file.
	runReq := httptest.NewRequest(http.MethodPost, "/api/backups/run", nil)
	runRR := httptest.NewRecorder()
	h.ServeHTTP(runRR, runReq)
	if runRR.Code != http.StatusOK {
		t.Fatalf("POST /api/backups/run: want 200, got %d", runRR.Code)
	}
	var entry dbbackup.Entry
	if err := json.Unmarshal(runRR.Body.Bytes(), &entry); err != nil {
		t.Fatalf("parse entry JSON: %v", err)
	}

	// Download by name.
	req := httptest.NewRequest(http.MethodGet, "/api/backups/"+entry.Name, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/backups/<name>: want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() == 0 {
		t.Fatal("downloaded backup has zero bytes")
	}
	ct := rr.Header().Get("Content-Type")
	if ct != "application/octet-stream" {
		t.Fatalf("want Content-Type application/octet-stream, got %q", ct)
	}
}

func TestAPIDownloadBackupRejectsTraversal(t *testing.T) {
	h, _ := newBackupHandler(t)

	for _, badName := range []string{
		"../etc/passwd",
		"..%2fetc%2fpasswd",
		"mangarr-20260101-000000.db/../../etc/passwd",
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/backups/"+badName, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code == http.StatusOK {
			t.Errorf("traversal name %q: want non-200, got 200", badName)
		}
	}
}

func TestSettingsPageRendersBackups(t *testing.T) {
	h, _ := newBackupHandler(t)

	// Run a backup first.
	runReq := httptest.NewRequest(http.MethodPost, "/api/backups/run", nil)
	h.ServeHTTP(httptest.NewRecorder(), runReq)

	// GET /settings should contain the backup dir and at least one backup name.
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Backups") {
		t.Fatalf("settings page does not contain 'Backups' heading; body excerpt:\n%s",
			snippet(body, "Backups", 300))
	}
	if !strings.Contains(body, "mangarr-") {
		t.Fatalf("settings page does not contain a backup filename; body excerpt:\n%s",
			snippet(body, "mangarr-", 300))

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

// ---- disk space API + Settings page tests ----

// newHandlerWithRoots builds a Handler that includes a real /tmp download root
// and a library root so we can assert disk-space rendering.
// Download roots are now stored in Settings (not the Handler constructor).
func newHandlerWithRoots() *Handler {
	st := &fakeStore{
		series:   nil,
		activity: nil,
		settings: model.Settings{
			DownloadRoots: []string{"/tmp"},
			LibraryRoots: map[model.ContentType]string{
				model.TypeManga: "/tmp",
			},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	return NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})
}

func TestAPIDiskSpaceReturnsJSONArray(t *testing.T) {
	h := newHandlerWithRoots()
	req := httptest.NewRequest(http.MethodGet, "/api/diskspace", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("want application/json, got %q", ct)
	}

	var entries []struct {
		Path        string  `json:"path"`
		TotalBytes  uint64  `json:"total_bytes"`
		FreeBytes   uint64  `json:"free_bytes"`
		PercentFree float64 `json:"percent_free"`
		Error       string  `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &entries); err != nil {
		t.Fatalf("parse JSON: %v; body=%s", err, rr.Body.String())
	}
	// /tmp is a download root AND the manga library root — deduplication
	// means only one entry with path=/tmp.
	if len(entries) != 1 {
		t.Fatalf("want 1 entry (/tmp deduped), got %d: %+v", len(entries), entries)
	}
	e := entries[0]
	if e.Path != "/tmp" {
		t.Fatalf("want path=/tmp, got %q", e.Path)
	}
	if e.TotalBytes == 0 {
		t.Fatal("TotalBytes is 0 for /tmp")
	}
	if e.Error != "" {
		t.Fatalf("unexpected error for /tmp: %q", e.Error)
	}
	if e.PercentFree < 0 || e.PercentFree > 100 {
		t.Fatalf("PercentFree out of range: %f", e.PercentFree)
	}
}

func TestAPIDiskSpaceEmptyWhenNoRoots(t *testing.T) {
	// Handler with no download roots and no library roots → empty array.
	// Roots are now in Settings, not the constructor.
	st := &fakeStore{
		settings: model.Settings{
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})
	req := httptest.NewRequest(http.MethodGet, "/api/diskspace", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	var entries []interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &entries); err != nil {
		t.Fatalf("parse JSON: %v; body=%s", err, rr.Body.String())
	}
	if len(entries) != 0 {
		t.Fatalf("want empty array, got %d entries", len(entries))
	}
}

func TestSettingsPageRendersDiskSpaceSection(t *testing.T) {
	h := newHandlerWithRoots()
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	// Section heading must be present.
	if !strings.Contains(body, "Disk Space") {
		t.Fatalf("'Disk Space' section heading not found in settings body")
	}
	// The /tmp path must be rendered.
	if !strings.Contains(body, "/tmp") {
		t.Fatalf("path '/tmp' not rendered in disk-space section")
	}
	// The bar element must be present.
	if !strings.Contains(body, "space-bar") {
		t.Fatalf("disk-space bar element not rendered in settings page")
	}
}

func TestSettingsPageNoDiskSectionWhenNoRoots(t *testing.T) {
	// With no download roots and no library roots, the disk-space section
	// should be omitted entirely.
	h := newEmptyHandler()
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	if strings.Contains(body, "Disk Space") {
		t.Fatalf("'Disk Space' section should not render when no roots are configured")
	}
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

func TestSettingsPageRendersRecycleBin(t *testing.T) {
	st := &fakeStore{
		settings: model.Settings{
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	h := NewHandler(HandlerOpts{
		Store:                   st,
		Runner:                  &fakeRunner{},
		RecycleBinPath:          "/tmp/mg-bin",
		RecycleBinRetentionDays: 14,
	})
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "/tmp/mg-bin") {
		t.Fatalf("settings page does not contain recycle bin path; body excerpt:\n%s",
			snippet(body, "Recycle", 300))
	}
	if !strings.Contains(body, "14") {
		t.Fatalf("settings page does not contain retention days (14)")
	}
	if !strings.Contains(body, "Recycle Bin") {
		t.Fatalf("settings page missing 'Recycle Bin' heading")
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

// ---- Tasks page + API tests ----

func TestTasksPageReturns200(t *testing.T) {
	h, _, _, _ := newTestHandlerWithRegistry()
	req := httptest.NewRequest(http.MethodGet, "/tasks", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	// Page header and at least the poll-scan row must appear.
	if !strings.Contains(body, "Tasks") {
		t.Fatalf("page title 'Tasks' not in body:\n%s", body)
	}
	if !strings.Contains(body, "Poll Scan") {
		t.Fatalf("poll-scan task not in body:\n%s", body)
	}
}

func TestAPIListTasksReturnsJSON(t *testing.T) {
	h, _, _, _ := newTestHandlerWithRegistry()
	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	var list []tasks.Info
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("parse JSON: %v; body=%s", err, rr.Body.String())
	}
	if len(list) < 1 {
		t.Fatalf("want >=1 entry, got %d", len(list))
	}
}

func TestAPIRunTaskTriggersRunFn(t *testing.T) {
	st := &fakeStore{
		series:   []model.Series{},
		activity: []model.ActivityEntry{},
		settings: model.Settings{
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}

	var flagMu sync.Mutex
	flagSet := false
	reg := newFakeTaskRegistry()
	reg.register("test-task", "Test Task", func(ctx context.Context) error {
		flagMu.Lock()
		flagSet = true
		flagMu.Unlock()
		return nil
	})

	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}, TaskReg: reg})
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/test-task/run", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}

	// Flag must have been flipped.
	flagMu.Lock()
	set := flagSet
	flagMu.Unlock()
	if !set {
		t.Fatal("RunFn was not called")
	}

	// Response must be a valid Info with recent LastRun.
	var info tasks.Info
	if err := json.Unmarshal(rr.Body.Bytes(), &info); err != nil {
		t.Fatalf("parse response JSON: %v; body=%s", err, rr.Body.String())
	}
	if info.LastRun.IsZero() {
		t.Error("LastRun should be non-zero after successful run")
	}
	if time.Since(info.LastRun) > 5*time.Second {
		t.Errorf("LastRun %v is too old", info.LastRun)
	}
}

func TestAPIRunTaskUnknownReturns404(t *testing.T) {
	h, _, _, _ := newTestHandlerWithRegistry()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/no-such-task/run", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d; body: %s", rr.Code, rr.Body.String())
	}
}

func TestAPIRescanRoutesThroughRegistry(t *testing.T) {
	h, _, runner, reg := newTestHandlerWithRegistry()

	req := httptest.NewRequest(http.MethodPost, "/api/rescan", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d; body: %s", rr.Code, rr.Body.String())
	}

	// runner.called was incremented via the registry's RunNow.
	if runner.called < 1 {
		t.Fatalf("expected RunFn to be called at least once via registry, called=%d", runner.called)
	}

	// The registry should reflect an updated LastRun for poll-scan.
	info, ok := reg.Get("poll-scan")
	if !ok {
		t.Fatal("poll-scan not found in registry after rescan")
	}
	if info.LastRun.IsZero() {
		t.Error("poll-scan LastRun should be non-zero after rescan")
	}
	if time.Since(info.LastRun) > 5*time.Second {
		t.Errorf("poll-scan LastRun %v is too old", info.LastRun)
	}
}

// ---- Health page + API tests ----

// fakeHealthRegistry is a minimal in-process HealthRegistry for tests.
type fakeHealthRegistry struct {
	results []health.Result
}

func (f *fakeHealthRegistry) RunAll(ctx context.Context) []health.Result {
	return f.results
}

func newHealthHandler(results []health.Result) *Handler {
	st := &fakeStore{
		settings: model.Settings{
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	reg := &fakeHealthRegistry{results: results}
	return NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}, HealthReg: reg})
}

func TestHealthPageReturns200(t *testing.T) {
	results := []health.Result{
		{ID: "sqlite", Name: "SQLite database", Status: health.StatusOK, Message: "Ping OK"},
		{ID: "download-roots", Name: "Download roots", Status: health.StatusWarn, Message: "No roots configured"},
	}
	h := newHealthHandler(results)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	// Should contain at least one row.
	if !strings.Contains(body, "SQLite database") {
		t.Fatalf("health page missing 'SQLite database' row; body:\n%s", body)
	}
	if !strings.Contains(body, "Download roots") {
		t.Fatalf("health page missing 'Download roots' row; body:\n%s", body)
	}
	// Page title must appear.
	if !strings.Contains(body, "Health") {
		t.Fatalf("health page title missing; body:\n%s", body)
	}
}

func TestAPIHealthReturnsJSON(t *testing.T) {
	results := []health.Result{
		{ID: "sqlite", Name: "SQLite database", Status: health.StatusOK, Message: "Ping OK"},
		{ID: "kavita", Name: "Kavita", Status: health.StatusError, Message: "connection refused"},
	}
	h := newHealthHandler(results)
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("want application/json, got %q", ct)
	}
	var resp struct {
		Status  string `json:"status"`
		Results []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse JSON: %v; body=%s", err, rr.Body.String())
	}
	// Overall status should be the worst (error).
	if resp.Status != "error" {
		t.Errorf("want overall status 'error', got %q", resp.Status)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("want 2 results, got %d", len(resp.Results))
	}
}

func TestHealthPageWithoutRegistryShowsWarning(t *testing.T) {
	st := &fakeStore{
		settings: model.Settings{
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	// nil healthReg
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "not wired") {
		t.Fatalf("expected 'not wired' placeholder in health page; body:\n%s", body)
	}
}

func TestAPIHealthWithoutRegistryReturnsWarn(t *testing.T) {
	st := &fakeStore{
		settings: model.Settings{
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse JSON: %v; body=%s", err, rr.Body.String())
	}
	if resp.Status != "warn" {
		t.Errorf("want status 'warn' for nil registry, got %q", resp.Status)
	}
}

// ---- Prometheus /metrics endpoint tests ----

// fakeMetricsSink is a minimal MetricsSink implementation for web tests.
// It serves a static body so we can assert the handler delegates correctly.
type fakeMetricsSink struct{}

func (f *fakeMetricsSink) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "# HELP mangarr_files_filed_total Total.\n")
		fmt.Fprint(w, "# TYPE mangarr_files_filed_total counter\n")
		fmt.Fprint(w, `mangarr_files_filed_total{category="manga"} 3`, "\n")
	})
}

func newHandlerWithMetrics() *Handler {
	st := &fakeStore{
		settings: model.Settings{
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	return NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}, Metrics: &fakeMetricsSink{}})
}

func TestMetricsEndpointServesPrometheus(t *testing.T) {
	h := newHandlerWithMetrics()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "mangarr_") {
		t.Errorf("expected at least one mangarr_ metric line in body; got:\n%s", body)
	}
}

func TestMetricsEndpointWithoutHandlerReturns503(t *testing.T) {
	st := &fakeStore{
		settings: model.Settings{
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	// nil metrics — /metrics should return 503
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 when metrics not wired, got %d; body: %s", rr.Code, rr.Body.String())
	}
}

// ---- Preview page + API tests ----

// fakePreviewer implements Previewer with canned results.
type fakePreviewer struct {
	entries []poller.PreviewEntry
	err     error
}

func (f *fakePreviewer) Preview(ctx context.Context) ([]poller.PreviewEntry, error) {
	return f.entries, f.err
}

// newPreviewHandler builds a Handler wired with a fakePreviewer seeded with
// one matched, one unmatched, and one misconfigured entry.
func newPreviewHandler() *Handler {
	st := &fakeStore{
		settings: model.Settings{
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	prev := &fakePreviewer{
		entries: []poller.PreviewEntry{
			{
				Title:      "Berserk",
				SourcePath: "/dl/Berserk",
				Source:     "tranga",
				Classified: model.TypeManga,
				DstRoot:    "/lib/Manga",
				Status:     "matched",
				ChapterPlans: []filer.PlanEntry{
					{SrcPath: "/dl/Berserk/Ch.001.cbz", DstPath: "/lib/Manga/Berserk/Berserk - Ch.001.cbz", Action: filer.PlanFile},
					{SrcPath: "/dl/Berserk/Ch.002.cbz", DstPath: "/lib/Manga/Berserk/Berserk - Ch.002.cbz", Action: filer.PlanSkip},
				},
			},
			{
				Title:      "Unknown Series",
				SourcePath: "/dl/Unknown",
				Source:     "suwayomi",
				Status:     "unmatched",
				Reason:     "AniList returned no match",
			},
			{
				Title:      "Misconfigured",
				SourcePath: "/dl/Misc",
				Source:     "tranga",
				Classified: model.TypeManhua,
				Status:     "misconfigured",
				Note:       "type Manhua has no configured library root",
			},
		},
	}
	return NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}, Previewer: prev})
}

func TestPreviewPageReturns200(t *testing.T) {
	h := newPreviewHandler()
	req := httptest.NewRequest(http.MethodGet, "/preview", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Preview") {
		t.Fatalf("page does not contain 'Preview' heading; body excerpt:\n%s", snippet(body, "Preview", 300))
	}
	// All three series must appear on the page.
	if !strings.Contains(body, "Berserk") {
		t.Fatalf("'Berserk' not in preview page body")
	}
	if !strings.Contains(body, "Unknown Series") {
		t.Fatalf("'Unknown Series' not in preview page body")
	}
	if !strings.Contains(body, "Misconfigured") {
		t.Fatalf("'Misconfigured' not in preview page body")
	}
}

func TestAPIPreviewReturnsJSON(t *testing.T) {
	h := newPreviewHandler()
	req := httptest.NewRequest(http.MethodGet, "/api/preview", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("want application/json, got %q", ct)
	}
	var entries []poller.PreviewEntry
	if err := json.Unmarshal(rr.Body.Bytes(), &entries); err != nil {
		t.Fatalf("parse JSON: %v; body=%s", err, rr.Body.String())
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 preview entries, got %d", len(entries))
	}
	// Find the matched one and verify chapter plans are present.
	var berserk *poller.PreviewEntry
	for i := range entries {
		if entries[i].Title == "Berserk" {
			berserk = &entries[i]
		}
	}
	if berserk == nil {
		t.Fatal("Berserk entry missing from /api/preview response")
	}
	if len(berserk.ChapterPlans) != 2 {
		t.Fatalf("Berserk: want 2 chapter plans, got %d", len(berserk.ChapterPlans))
	}
}

func TestPreviewPageWithoutPreviewerShowsPlaceholder(t *testing.T) {
	// nil previewer → placeholder message.
	h := NewHandler(HandlerOpts{
		Store: &fakeStore{
			settings: model.Settings{
				LibraryRoots:       map[model.ContentType]string{},
				KavitaLibIDsByType: map[model.ContentType]int64{},
			},
		},
		Runner: &fakeRunner{},
	})
	req := httptest.NewRequest(http.MethodGet, "/preview", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "not available") && !strings.Contains(body, "not wired") {
		t.Fatalf("placeholder text not found in body;\nbody:\n%s", snippet(body, "Preview", 400))
	}
}

// ---- /api/series/{id}/assign tests ----

// newHandlerWithFilerForTest builds a Handler with a fakeSeriesFiler and a seeded store.
func newHandlerWithFilerForTest(sf *fakeSeriesFiler) (*Handler, *fakeStore) {
	st := &fakeStore{
		series: []model.Series{
			{ID: 1, Title: "Dragon Ball Super (Color)", Type: model.TypeUnknown, Status: model.StatusUnmatched, Source: "suwayomi"},
		},
		settings: model.Settings{
			LibraryRoots:       map[model.ContentType]string{model.TypeManga: "/lib/Manga"},
			KavitaLibIDsByType: map[model.ContentType]int64{model.TypeManga: 3},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}, SeriesFiler: sf})
	return h, st
}

func TestAPIAssignTriggersFileOne(t *testing.T) {
	sf := &fakeSeriesFiler{}
	h, _ := newHandlerWithFilerForTest(sf)

	form := url.Values{"type": {"Manga"}}
	req := httptest.NewRequest(http.MethodPost, "/api/series/1/assign",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	if len(sf.calls) != 1 {
		t.Fatalf("expected 1 FileOne call, got %d", len(sf.calls))
	}
	if sf.calls[0].seriesID != 1 || sf.calls[0].ct != model.TypeManga {
		t.Fatalf("expected FileOne(1, Manga), got %+v", sf.calls[0])
	}
}

func TestAPIAssignReturnsEmptyFragment(t *testing.T) {
	sf := &fakeSeriesFiler{}
	h, _ := newHandlerWithFilerForTest(sf)

	form := url.Values{"type": {"Manhwa"}}
	req := httptest.NewRequest(http.MethodPost, "/api/series/1/assign",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	// Body must be empty so the HTMX outerHTML swap removes the row.
	if body := strings.TrimSpace(rr.Body.String()); body != "" {
		t.Fatalf("expected empty body for successful assign, got %q", body)
	}
}

func TestAPIAssignRejectsBadType(t *testing.T) {
	sf := &fakeSeriesFiler{}
	h, _ := newHandlerWithFilerForTest(sf)

	form := url.Values{"type": {"Whatever"}}
	req := httptest.NewRequest(http.MethodPost, "/api/series/1/assign",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d; body: %s", rr.Code, rr.Body.String())
	}
	if len(sf.calls) != 0 {
		t.Fatalf("FileOne must not be called on invalid type, got %d calls", len(sf.calls))
	}
}

func TestAPIAssignWithoutFilerReturns503(t *testing.T) {
	// Handler with no SeriesFiler → 503.
	st := &fakeStore{
		settings: model.Settings{
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})

	form := url.Values{"type": {"Manga"}}
	req := httptest.NewRequest(http.MethodPost, "/api/series/1/assign",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d; body: %s", rr.Code, rr.Body.String())
	}
}

// ---- /api/browse tests ----

// newBrowseHandler builds a Handler with browseRoots injected for testing.
func newBrowseHandler(t *testing.T, browseRoots []string) *Handler {
	t.Helper()
	st := &fakeStore{
		settings: model.Settings{
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	return NewHandler(HandlerOpts{
		Store:       st,
		Runner:      &fakeRunner{},
		BrowseRoots: browseRoots,
	})
}

func TestAPIBrowseRootViewListsAllowlist(t *testing.T) {
	h := newBrowseHandler(t, []string{"/media", "/config"})
	req := httptest.NewRequest(http.MethodGet, "/api/browse", nil) // no path param → synthetic root
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Path    string `json:"path"`
		Entries []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse JSON: %v; body=%s", err, rr.Body.String())
	}
	if resp.Path != "" {
		t.Errorf("want empty path for root view, got %q", resp.Path)
	}
	if len(resp.Entries) < 2 {
		t.Fatalf("want >=2 entries (/media + /config), got %d: %+v", len(resp.Entries), resp.Entries)
	}
	var paths []string
	for _, e := range resp.Entries {
		paths = append(paths, e.Path)
	}
	foundMedia := false
	foundConfig := false
	for _, p := range paths {
		if p == "/media" {
			foundMedia = true
		}
		if p == "/config" {
			foundConfig = true
		}
	}
	if !foundMedia {
		t.Errorf("want /media in entries, got %v", paths)
	}
	if !foundConfig {
		t.Errorf("want /config in entries, got %v", paths)
	}
}

func TestAPIBrowseRejectsOutsideAllowlist(t *testing.T) {
	h := newBrowseHandler(t, []string{"/media", "/config"})
	req := httptest.NewRequest(http.MethodGet, "/api/browse?path=/etc", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("want 403 for /etc, got %d; body: %s", rr.Code, rr.Body.String())
	}
}

func TestAPIBrowseRejectsTraversal(t *testing.T) {
	h := newBrowseHandler(t, []string{"/media", "/config"})
	for _, bad := range []string{"/media/../etc", "/config/../etc/passwd"} {
		req := httptest.NewRequest(http.MethodGet, "/api/browse?path="+url.QueryEscape(bad), nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("traversal %q: want 403, got %d; body: %s", bad, rr.Code, rr.Body.String())
		}
	}
}

func TestAPIBrowseListsTempDir(t *testing.T) {
	// Inject /tmp as the sole browse root so the test can navigate it.
	h := newBrowseHandler(t, []string{"/tmp"})

	// Create a couple of test subdirs.
	dir := t.TempDir() // somewhere under /tmp
	sub1 := dir + "/alpha"
	sub2 := dir + "/beta"
	if err := os.MkdirAll(sub1, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(sub2, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/browse?path="+url.QueryEscape(dir), nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Path    string `json:"path"`
		Entries []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse JSON: %v; body=%s", err, rr.Body.String())
	}
	if len(resp.Entries) < 2 {
		t.Fatalf("want >=2 dir entries (alpha, beta), got %d: %+v", len(resp.Entries), resp.Entries)
	}
	// Entries must be sorted case-insensitively.
	if resp.Entries[0].Name != "alpha" || resp.Entries[1].Name != "beta" {
		t.Errorf("want sorted [alpha, beta], got %v", resp.Entries)
	}
	for _, e := range resp.Entries {
		if e.Type != "dir" {
			t.Errorf("entry %q: want type=dir, got %q", e.Name, e.Type)
		}
	}
}

func TestAPIBrowseFragmentRendersHTML(t *testing.T) {
	h := newBrowseHandler(t, []string{"/tmp"})
	dir := t.TempDir()
	_ = os.MkdirAll(dir+"/subdir", 0o755)

	req := httptest.NewRequest(http.MethodGet, "/api/browse/fragment?path="+url.QueryEscape(dir)+"&target=root_manga", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("want text/html, got %q", ct)
	}
	if !strings.Contains(body, "browse-breadcrumbs") {
		t.Errorf("breadcrumbs not in fragment; body:\n%s", body)
	}
	if !strings.Contains(body, "Select this folder") {
		t.Errorf("'Select this folder' button not in fragment; body:\n%s", body)
	}
	if !strings.Contains(body, "root_manga") {
		t.Errorf("target field 'root_manga' not in fragment; body:\n%s", body)
	}
}

func TestDiskBarShowsPercentUsed(t *testing.T) {
	h := newHandlerWithRoots() // uses /tmp as both download root and manga library
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "% used") {
		t.Fatalf("expected percent-used label in disk-space bar; body excerpt:\n%s",
			snippet(body, "space-bar", 400))
	}
}

func TestSettingsFooterIsBelowBackups(t *testing.T) {
	h, _ := newBackupHandler(t)
	// Run a backup so the Backups card has actual content.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/backups/run", nil))

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()

	// Both landmarks must be present.
	if !strings.Contains(body, "backups-section") {
		t.Fatalf("backups-section not in settings page body")
	}
	if !strings.Contains(body, "settings-footer") {
		t.Fatalf("settings-footer not in settings page body")
	}
	// Backups card must appear BEFORE the sticky footer (footer is last).
	idxBackups := strings.Index(body, "backups-section")
	idxFooter := strings.Index(body, "settings-footer")
	if idxBackups < 0 || idxFooter < 0 {
		t.Fatalf("both landmarks must be present (backups=%d, footer=%d)", idxBackups, idxFooter)
	}
	if idxBackups > idxFooter {
		t.Fatalf("backups-section appears AFTER settings-footer — want backups before footer (idxBackups=%d idxFooter=%d)", idxBackups, idxFooter)
	}
}

// ---- New tests for Settings + Disk Space changes ----

func TestSettingsPageRendersDownloadRootRows(t *testing.T) {
	st := &fakeStore{
		settings: model.Settings{
			DownloadRoots:      []string{"/media/Downloads/suwayomi", "/media/Downloads/tranga"},
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Download Roots") {
		t.Errorf("'Download Roots' heading not in settings page")
	}
	if !strings.Contains(body, "/media/Downloads/suwayomi") {
		t.Errorf("first download root not rendered")
	}
	if !strings.Contains(body, "/media/Downloads/tranga") {
		t.Errorf("second download root not rendered")
	}
	// Each row has a Browse button — check hx-vals pattern.
	if !strings.Contains(body, `hx-vals='js:{path:`) {
		t.Errorf("browse-from-current hx-vals not rendered on download root rows")
	}
	// Each row has a remove button (✕ character or its HTML entity).
	removeCount := strings.Count(body, "✕") + strings.Count(body, "&#x2715;")
	if removeCount < 2 {
		t.Errorf("expected at least 2 remove (✕) buttons for 2 roots, found %d", removeCount)
	}
}

func TestSaveSettingsFormPostUpdatesDownloadRoots(t *testing.T) {
	st := &fakeStore{
		settings: model.Settings{
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})

	form := url.Values{
		"file_mode":      {"hardlink"},
		"rename_scheme":  {"{series}/{series} - Ch.{chapter}.cbz"},
		"poll_minutes":   {"15"},
		"download_root":  {"/media/Downloads/suwayomi", "/media/Downloads/tranga", ""}, // empty dropped
	}
	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d; body: %s", rr.Code, rr.Body.String())
	}
	if len(st.settings.DownloadRoots) != 2 {
		t.Fatalf("want 2 download roots persisted, got %d: %v", len(st.settings.DownloadRoots), st.settings.DownloadRoots)
	}
	if st.settings.DownloadRoots[0] != "/media/Downloads/suwayomi" {
		t.Errorf("want first root=/media/Downloads/suwayomi, got %q", st.settings.DownloadRoots[0])
	}
	if st.settings.DownloadRoots[1] != "/media/Downloads/tranga" {
		t.Errorf("want second root=/media/Downloads/tranga, got %q", st.settings.DownloadRoots[1])
	}
}

func TestBrowseFromCurrentPathRendersHxVals(t *testing.T) {
	// After Plan B Task 8 the v1 Library Roots browse buttons are gone, so
	// this test now exercises the Download Roots browse buttons (the only
	// remaining Browse-button surface on the Settings page).
	st := &fakeStore{
		settings: model.Settings{
			DownloadRoots:      []string{"/media/Downloads/suwayomi"},
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	body := rr.Body.String()
	// All Browse buttons must use hx-vals js: pattern, not static hx-get with hardcoded path.
	if !strings.Contains(body, `hx-vals='js:{path:`) {
		t.Errorf("expected hx-vals='js:{path:...' pattern on Browse buttons; body excerpt:\n%s",
			snippet(body, "btn-browse", 400))
	}
	// Must NOT have the old static path pattern.
	if strings.Contains(body, `hx-get="/api/browse/fragment?path=/media`) {
		t.Errorf("found old static path pattern — should use hx-vals instead")
	}
}

func TestBrowseFragmentReturnsFriendlyErrorForBadPath(t *testing.T) {
	h := newBrowseHandler(t, []string{"/tmp"})

	// /etc is outside /tmp → previously 403, now 200 + friendly message.
	req := httptest.NewRequest(http.MethodGet, "/api/browse/fragment?path=/etc&target=root_manga", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	// Must return 200 so HTMX swaps the fragment.
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200 (friendly error), got %d; body: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "outside the allowed") {
		t.Errorf("expected 'outside the allowed' message in fragment; body:\n%s", body)
	}
	// Must have a 'Back to root' button.
	if !strings.Contains(body, "Back to root") {
		t.Errorf("expected 'Back to root' button in friendly error fragment; body:\n%s", body)
	}
}

func TestDiskRowsGroupedByFSID(t *testing.T) {
	// Two paths on the same filesystem (/tmp and a subdir of /tmp)
	// should produce exactly one fsDiskRow with both paths listed.
	dir := t.TempDir() // under /tmp — same FS
	st := &fakeStore{
		settings: model.Settings{
			DownloadRoots: []string{"/tmp"},
			LibraryRoots:  map[model.ContentType]string{model.TypeManga: dir},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})
	rows := h.buildDiskRows(st.settings)
	if len(rows) != 1 {
		t.Fatalf("want 1 row (same FS), got %d: %+v", len(rows), rows)
	}
	if len(rows[0].Paths) != 2 {
		t.Fatalf("want 2 paths in the single row, got %d: %+v", len(rows[0].Paths), rows[0].Paths)
	}
}

func TestDiskRowsSeparateForDifferentFSIDs(t *testing.T) {
	// /tmp is typically on tmpfs; / is on a different FS.
	// If they happen to be the same FS in this environment, skip.
	infoTmp := diskspace.Stat("/tmp")
	infoRoot := diskspace.Stat("/")
	if infoTmp.Err != nil || infoRoot.Err != nil {
		t.Skip("cannot stat /tmp or /: skip")
	}
	if infoTmp.FSID == infoRoot.FSID {
		t.Skip("/tmp and / share the same FSID in this environment — skip")
	}

	st := &fakeStore{
		settings: model.Settings{
			DownloadRoots: []string{"/tmp"},
			LibraryRoots:  map[model.ContentType]string{model.TypeManga: "/"},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})
	rows := h.buildDiskRows(st.settings)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (different FSIDs), got %d: %+v", len(rows), rows)
	}
}

// ---- Kavita library picker tests ----

// kavitaStubServer returns an httptest server that mimics Kavita's auth + library endpoints.
// libs is the canned library list returned from /api/Library. authStatus controls the
// HTTP status code of the auth endpoint (0 = default 200 OK + jwt123 token). libraryStatus
// controls the /api/Library status (0 = default 200 OK + libs JSON).
func kavitaStubServer(t *testing.T, libs []kavita.Library, authStatus, libraryStatus int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/plugin/authenticate"):
			if authStatus != 0 {
				w.WriteHeader(authStatus)
				return
			}
			w.Write([]byte(`{"token":"jwt123"}`))
		case strings.Contains(r.URL.Path, "/api/Library"):
			if libraryStatus != 0 {
				w.WriteHeader(libraryStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			body, _ := json.Marshal(libs)
			w.Write(body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// newKavitaLibHandler builds a Handler whose store returns settings pointing at the
// given Kavita base URL (use a kavitaStubServer for happy-path tests, an unreachable
// URL for failure tests). The API key is fixed to "stubkey" — the stub server doesn't
// check it.
func newKavitaLibHandler(kavitaURL string, savedManga, savedManhwa, savedManhua int64) *Handler {
	st := &fakeStore{
		settings: model.Settings{
			KavitaBaseURL: kavitaURL,
			KavitaAPIKey:  "stubkey",
			LibraryRoots:  map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{
				model.TypeManga:  savedManga,
				model.TypeManhwa: savedManhwa,
				model.TypeManhua: savedManhua,
			},
		},
	}
	return NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})
}

func TestAPIKavitaLibrariesReturnsJSON(t *testing.T) {
	libs := []kavita.Library{
		{ID: 1, Name: "Manga", Type: 0},
		{ID: 2, Name: "Comics", Type: 1},
	}
	srv := kavitaStubServer(t, libs, 0, 0)
	defer srv.Close()
	h := newKavitaLibHandler(srv.URL, 0, 0, 0)
	req := httptest.NewRequest(http.MethodGet, "/api/kavita/libraries", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("want application/json, got %q", ct)
	}
	var resp struct {
		Libraries []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"libraries"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse JSON: %v; body=%s", err, rr.Body.String())
	}
	if len(resp.Libraries) != 2 {
		t.Fatalf("want 2 libraries, got %d", len(resp.Libraries))
	}
	// kavita.ListLibraries sorts by Name (case-insensitive): Comics before Manga.
	if resp.Libraries[0].Name != "Comics" || resp.Libraries[1].Name != "Manga" {
		t.Fatalf("unexpected library names (want sorted [Comics, Manga]): %+v", resp.Libraries)
	}
}

func TestAPIKavitaLibrariesWhenUnconfiguredReturns503(t *testing.T) {
	// Settings has no Kavita URL/key → endpoint returns 503 with a JSON error.
	st := &fakeStore{
		settings: model.Settings{
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})
	req := httptest.NewRequest(http.MethodGet, "/api/kavita/libraries", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 when Kavita not configured, got %d; body: %s", rr.Code, rr.Body.String())
	}
}

// TestSeriesPageShowsManualBindingNameInsteadOfUnknown pins the UX fix
// for the post-reclassify confusion: when a series has ManualBindingID
// set, the Type column shows the binding's name with a "manual" badge
// instead of "unknown" (which it would otherwise show until a poll
// successfully classifies via the manual override). The dropdown
// alone wasn't enough signal — operators reported the page looked
// like the reclassify "didn't stick" because the Type column hadn't
// updated.
func TestSeriesPageShowsManualBindingNameInsteadOfUnknown(t *testing.T) {
	pinned := int64(5)
	st := &fakeStore{
		bindings: []model.Binding{
			{ID: 1, Name: "Manga"},
			{ID: 5, Name: "Manhwa"},
		},
		series: []model.Series{
			{ID: 1, Title: "Plain", Type: model.TypeUnknown, Status: model.StatusUnmatched},
			{ID: 2, Title: "TBATE", Type: model.TypeUnknown, Status: model.StatusPending,
				ManualBindingID: &pinned},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})
	req := httptest.NewRequest(http.MethodGet, "/series", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	// The row WITHOUT a manual override still shows "unknown".
	if !strings.Contains(body, "pill-unknown") {
		t.Errorf("expected 'unknown' pill for the unpinned row")
	}
	// The row WITH a manual override shows the binding name and a
	// manual-styled pill.
	if !strings.Contains(body, "pill-manual") {
		t.Errorf("expected .pill-manual class on the row with ManualBindingID; body:\n%s",
			excerpt(body, "TBATE", 400))
	}
	if !strings.Contains(body, ">Manhwa<") {
		t.Errorf("expected 'Manhwa' (the bound binding's name) in the pinned row's Type cell")
	}
}

// TestSeriesPageManualBindingAtDeletedIDRendersFallback pins the
// graceful-degradation path: if an operator pinned a binding then
// deleted it from Settings, the series row should still render —
// with a "deleted binding" pill — rather than crash the template or
// show the cryptic "unknown" (which would mask the misconfiguration).
func TestSeriesPageManualBindingAtDeletedIDRendersFallback(t *testing.T) {
	deleted := int64(999)
	st := &fakeStore{
		bindings: []model.Binding{{ID: 1, Name: "Manga"}},
		series: []model.Series{
			{ID: 1, Title: "Orphaned", Type: model.TypeUnknown, Status: model.StatusPending,
				ManualBindingID: &deleted},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})
	req := httptest.NewRequest(http.MethodGet, "/series", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, "deleted binding") {
		t.Errorf("expected 'deleted binding' fallback for manual override at unknown ID; body:\n%s",
			excerpt(body, "Orphaned", 400))
	}
}
