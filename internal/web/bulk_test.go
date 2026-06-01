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
