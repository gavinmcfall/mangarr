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

// TestAPIBulkJobsReturnsJSONList covers the happy-path GET: every seeded
// job is returned regardless of status. Status filtering is exercised
// separately below.
func TestAPIBulkJobsReturnsJSONList(t *testing.T) {
	h, st, _ := newTestHandler()
	st.bulkJobs = []model.BulkJob{
		{ID: 1, MangaID: 7, SourceID: "42", Title: "One Piece", SourceName: "MangaDex EN",
			Status: model.BulkJobRunning, TotalChapters: 1076, CompletedChapters: 412},
		{ID: 2, MangaID: 8, SourceID: "99", Title: "Mashle", SourceName: "Mangapark",
			Status: model.BulkJobCompleted, TotalChapters: 162, CompletedChapters: 162},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/bulk/jobs", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var got []model.BulkJob
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("want 2 jobs, got %d", len(got))
	}
}

// TestAPIBulkJobsFiltersByStatus confirms ?status=running drops the
// Completed job from the result set.
func TestAPIBulkJobsFiltersByStatus(t *testing.T) {
	h, st, _ := newTestHandler()
	st.bulkJobs = []model.BulkJob{
		{ID: 1, Status: model.BulkJobRunning},
		{ID: 2, Status: model.BulkJobCompleted},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/bulk/jobs?status=running", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var got []model.BulkJob
	_ = json.NewDecoder(rec.Body).Decode(&got)
	if len(got) != 1 || got[0].ID != 1 {
		t.Errorf("filtered list: want [1], got %+v", got)
	}
}

// TestAPIBulkCreateMakesJobPerMangaID covers the confirm=1 happy path:
// one BulkJob row saved, three chapter rows inserted, 303 redirect to
// /downloads.
func TestAPIBulkCreateMakesJobPerMangaID(t *testing.T) {
	h, st, sw := newTestHandler()
	sw.chaptersForManga = map[int64][]int64{
		7: {100, 101, 102}, // 3 chapters, all missing
	}
	st.libraryCache = map[int64]model.LibraryCacheEntry{
		7: {MangaID: 7, Title: "One Piece", SourceID: "42", SourceName: "MangaDex EN", TotalChapters: 1076},
	}

	form := url.Values{}
	form.Add("manga_id", "7")
	form.Set("confirm", "1")
	req := httptest.NewRequest(http.MethodPost, "/api/bulk", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303 redirect, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(st.savedBulkJobs) != 1 {
		t.Fatalf("want 1 bulk job created, got %d", len(st.savedBulkJobs))
	}
	if st.savedBulkJobs[0].MangaID != 7 || st.savedBulkJobs[0].SourceID != "42" {
		t.Errorf("job created with wrong fields: %+v", st.savedBulkJobs[0])
	}
	if len(st.savedChapterIDs) != 3 {
		t.Errorf("want 3 chapter rows inserted, got %d", len(st.savedChapterIDs))
	}
}

// TestAPIBulkCreateRejectsUnknownMangaID: GetLibraryCacheEntry returns
// sql.ErrNoRows for manga IDs not in the library cache; the handler
// surfaces that as 400 so the operator knows to library-cache-refresh
// before retrying.
func TestAPIBulkCreateRejectsUnknownMangaID(t *testing.T) {
	h, _, _ := newTestHandler()
	// No library_cache entry for manga_id=999.
	form := url.Values{}
	form.Add("manga_id", "999")
	form.Set("confirm", "1")
	req := httptest.NewRequest(http.MethodPost, "/api/bulk", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400 on unknown manga_id, got %d", rec.Code)
	}
}

// TestAPIBulkCreateSkipsSeriesWithNoMissingChapters guards the "empty
// job" edge case: if ListChapters returns nothing missing, no BulkJob
// row is created (otherwise the UI would show a freshly-Completed
// 0-chapter row, which is confusing).
func TestAPIBulkCreateSkipsSeriesWithNoMissingChapters(t *testing.T) {
	h, st, sw := newTestHandler()
	st.libraryCache = map[int64]model.LibraryCacheEntry{
		7: {MangaID: 7, Title: "Naruto", SourceID: "42", SourceName: "MangaDex EN", TotalChapters: 700, Downloaded: 700},
	}
	sw.chaptersForManga = map[int64][]int64{7: nil} // all already downloaded
	form := url.Values{}
	form.Add("manga_id", "7")
	form.Set("confirm", "1")
	req := httptest.NewRequest(http.MethodPost, "/api/bulk", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if len(st.savedBulkJobs) != 0 {
		t.Errorf("expected NO job created when 0 missing chapters; got %d", len(st.savedBulkJobs))
	}
}

// TestAPIDownloadsPauseFlipsStatus: POST /api/downloads/{id}/pause calls
// UpdateBulkJobStatus(id, paused) and returns 200.
func TestAPIDownloadsPauseFlipsStatus(t *testing.T) {
	h, st, _ := newTestHandler()
	st.bulkJobs = []model.BulkJob{{ID: 1, Status: model.BulkJobRunning}}

	req := httptest.NewRequest(http.MethodPost, "/api/downloads/1/pause", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(st.bulkStatusUpdates) != 1 || st.bulkStatusUpdates[0].status != model.BulkJobPaused {
		t.Errorf("expected UpdateBulkJobStatus(1, paused); got %+v", st.bulkStatusUpdates)
	}
}

// TestAPIDownloadsResumeClearsBackoffOnErrored: an errored job's resume
// must clear backoff state AND flip status to running. Order matters —
// the resume handler clears first, then flips, so the next orchestrator
// tick sees consec_failures=0 + status=running together.
func TestAPIDownloadsResumeClearsBackoffOnErrored(t *testing.T) {
	h, st, _ := newTestHandler()
	st.bulkJobs = []model.BulkJob{{ID: 1, Status: model.BulkJobErrored, ConsecutiveFailures: 5}}

	req := httptest.NewRequest(http.MethodPost, "/api/downloads/1/resume", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if len(st.bulkClearBackoff) != 1 || st.bulkClearBackoff[0] != 1 {
		t.Errorf("expected ClearBulkJobBackoff(1); got %+v", st.bulkClearBackoff)
	}
	var sawRunning bool
	for _, u := range st.bulkStatusUpdates {
		if u.id == 1 && u.status == model.BulkJobRunning {
			sawRunning = true
		}
	}
	if !sawRunning {
		t.Errorf("expected status flip to running; got %+v", st.bulkStatusUpdates)
	}
	// Ordering invariant — the spec requires ClearBulkJobBackoff to run
	// BEFORE UpdateBulkJobStatus(Running) so the next orchestrator tick
	// reads consec_failures=0 + status=running together. A future
	// regression that swaps the two calls would still leave both lists
	// non-empty, so check ordering explicitly via callOrder.
	if len(st.callOrder) != 2 ||
		st.callOrder[0] != "clear:1" ||
		st.callOrder[1] != "status:1:running" {
		t.Errorf("resume must call ClearBulkJobBackoff BEFORE UpdateBulkJobStatus(Running); got callOrder=%v", st.callOrder)
	}
}

// TestAPIDownloadsUnknownActionReturns400 pins the default branch in
// apiDownloadsAction — an unknown action like "bogus" returns 400, not
// 405/500, and does NOT touch the store.
func TestAPIDownloadsUnknownActionReturns400(t *testing.T) {
	h, st, _ := newTestHandler()
	st.bulkJobs = []model.BulkJob{{ID: 1, Status: model.BulkJobRunning}}

	req := httptest.NewRequest(http.MethodPost, "/api/downloads/1/bogus", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 on unknown action, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if len(st.callOrder) != 0 {
		t.Errorf("unknown action must NOT mutate the store; got callOrder=%v", st.callOrder)
	}
}

// TestAPIDownloadsNonNumericIDReturns400 pins that a malformed id path
// segment returns 400 from the strconv.ParseInt guard, before any
// store call.
func TestAPIDownloadsNonNumericIDReturns400(t *testing.T) {
	h, st, _ := newTestHandler()

	req := httptest.NewRequest(http.MethodPost, "/api/downloads/abc/pause", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 on non-numeric id, got %d", rec.Code)
	}
	if len(st.callOrder) != 0 {
		t.Errorf("non-numeric id must NOT mutate the store; got callOrder=%v", st.callOrder)
	}
}

// TestAPIDownloadsDeleteRemovesJob: POST /api/downloads/{id}/delete calls
// DeleteBulkJob; chapter rows cascade via the FK at the SQLite layer
// (not exercised here — covered by the store-level cascade test).
func TestAPIDownloadsDeleteRemovesJob(t *testing.T) {
	h, st, _ := newTestHandler()
	st.bulkJobs = []model.BulkJob{{ID: 1, Status: model.BulkJobRunning}}

	req := httptest.NewRequest(http.MethodPost, "/api/downloads/1/delete", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if len(st.bulkDeletes) != 1 || st.bulkDeletes[0] != 1 {
		t.Errorf("expected DeleteBulkJob(1); got %+v", st.bulkDeletes)
	}
}

// TestAPIBulkCreatePreviewReturnsHTMLModalOnHXRequest: when the form
// arrives with HX-Request: true and confirm=0, the handler renders the
// confirmation modal HTML instead of JSON. Pin: per-provider grouping
// (both series share sourceId 42 → 1 provider, 2 series, 8 chapters),
// hidden inputs preserve the manga_ids for the confirm POST, and no
// job is created during the preview.
func TestAPIBulkCreatePreviewReturnsHTMLModalOnHXRequest(t *testing.T) {
	h, st, sw := newTestHandler()
	st.libraryCache = map[int64]model.LibraryCacheEntry{
		7: {MangaID: 7, Title: "One Piece", SourceID: "42", SourceName: "MangaDex EN"},
		8: {MangaID: 8, Title: "SOLO LEVELING", SourceID: "42", SourceName: "MangaDex EN"},
	}
	sw.chaptersForManga = map[int64][]int64{
		7: {100, 101, 102},
		8: {200, 201, 202, 203, 204},
	}

	form := url.Values{}
	form.Add("manga_id", "7")
	form.Add("manga_id", "8")
	form.Set("confirm", "0")
	req := httptest.NewRequest(http.MethodPost, "/api/bulk", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"You're about to queue <b>8 chapters</b>",
		"<b>2 series</b>",
		"<b>1 provider</b>",
		"MangaDex EN",
		`name="manga_id" value="7"`,
		`name="manga_id" value="8"`,
		`name="confirm" value="1"`,
		"Queue downloads",
		"Cancel",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("modal HTML missing %q. Body:\n%s", want, body)
		}
	}
	if len(st.savedBulkJobs) != 0 {
		t.Errorf("modal preview must NOT create jobs; got %d", len(st.savedBulkJobs))
	}
}

// TestAPIBulkCreatePreviewModalSkipsZeroMissingSeries: when every
// selected manga is already fully downloaded, the modal renders the
// empty-state copy from the spec's "Empty states" section instead of
// the queue-confirmation copy.
func TestAPIBulkCreatePreviewModalSkipsZeroMissingSeries(t *testing.T) {
	h, st, sw := newTestHandler()
	st.libraryCache = map[int64]model.LibraryCacheEntry{
		7: {MangaID: 7, Title: "Naruto", SourceID: "42", SourceName: "MangaDex EN"},
	}
	sw.chaptersForManga = map[int64][]int64{7: nil}

	form := url.Values{}
	form.Add("manga_id", "7")
	form.Set("confirm", "0")
	req := httptest.NewRequest(http.MethodPost, "/api/bulk", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "All selected series are fully downloaded") {
		t.Errorf("expected empty-state modal copy; got:\n%s", rec.Body.String())
	}
}

// TestAPIDownloadsPauseHXRequestReturnsUpdatedRow pins the Plan B T4
// HX-Request branch for pause: after the in-place status flip, the
// handler re-reads the job and renders a one-row HTML fragment whose
// status pill / button set reflects the new (paused) state.
func TestAPIDownloadsPauseHXRequestReturnsUpdatedRow(t *testing.T) {
	h, st, _ := newTestHandler()
	st.bulkJobs = []model.BulkJob{
		{ID: 1, MangaID: 7, SourceID: "42", Title: "One Piece", SourceName: "MangaDex EN",
			Status: model.BulkJobRunning, TotalChapters: 1076, CompletedChapters: 412},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/downloads/1/pause", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The returned <tr> reflects the new status (the fakeStore updates
	// bulkJobs in-place via UpdateBulkJobStatus so the GetBulkJob re-read
	// here sees "paused").
	for _, want := range []string{
		"<tr", "One Piece", "MangaDex EN",
		"412", "1076",
		"pill-paused", "paused",
		`hx-post="/api/downloads/1/resume"`,
		`hx-post="/api/downloads/1/delete"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("row fragment missing %q. Body:\n%s", want, body)
		}
	}
}

// TestAPIDownloadsDeleteHXRequestReturnsEmptyTR pins delete's special
// case: there's no row to render, so HTMX outerHTML-swaps an empty <tr>
// to remove the row visually.
func TestAPIDownloadsDeleteHXRequestReturnsEmptyTR(t *testing.T) {
	h, st, _ := newTestHandler()
	st.bulkJobs = []model.BulkJob{{ID: 1, Status: model.BulkJobRunning}}

	req := httptest.NewRequest(http.MethodPost, "/api/downloads/1/delete", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	body := strings.TrimSpace(rec.Body.String())
	// Empty <tr> so HTMX outerHTML swap removes the row visually.
	if body != "<tr></tr>" && body != "" {
		t.Errorf("delete should return empty <tr> for outerHTML swap; got %q", body)
	}
}

// TestAPIDownloadsResumeHXRequestReturnsRunningRow pins the resume path:
// ClearBulkJobBackoff fires BEFORE UpdateBulkJobStatus(Running), and the
// rendered row shows the Pause button (running state).
func TestAPIDownloadsResumeHXRequestReturnsRunningRow(t *testing.T) {
	h, st, _ := newTestHandler()
	st.bulkJobs = []model.BulkJob{
		{ID: 1, Title: "Berserk", SourceName: "MangaDex EN",
			Status: model.BulkJobErrored, TotalChapters: 100, CompletedChapters: 50},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/downloads/1/resume", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Berserk", "pill-running", "running",
		`hx-post="/api/downloads/1/pause"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("row fragment missing %q. Body:\n%s", want, body)
		}
	}
}
