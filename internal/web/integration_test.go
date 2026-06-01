package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/suwayomi"
)

// TestBulkDownloadEndToEndViaHTTP exercises the full Plan B flow
// against the in-process Handler:
//  1. POST /api/library/sync writes library_cache.
//  2. POST /api/bulk?confirm=0 with HX-Request returns the modal HTML.
//  3. POST /api/bulk?confirm=1 (via the modal form) creates the BulkJob.
//  4. GET /downloads renders the job in the queue.
//  5. POST /api/downloads/{id}/pause swaps it to paused.
//  6. POST /api/downloads/{id}/delete removes the row.
func TestBulkDownloadEndToEndViaHTTP(t *testing.T) {
	h, st, sw := newTestHandler()
	sw.libraryEntries = []suwayomi.Manga{
		{ID: 7, Title: "One Piece", SourceID: "42", Source: "MangaDex EN"},
	}
	sw.chaptersForManga = map[int64][]int64{7: {100, 101, 102}}
	sw.chaptersDownloaded = map[int64]bool{}

	// 1. Sync library.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/library/sync", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("sync: want 200, got %d", rec.Code)
	}

	// 2. Preview (HX-Request).
	previewForm := url.Values{}
	previewForm.Set("manga_id", "7")
	previewForm.Set("confirm", "0")
	prevReq := httptest.NewRequest(http.MethodPost, "/api/bulk", strings.NewReader(previewForm.Encode()))
	prevReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	prevReq.Header.Set("HX-Request", "true")
	prevRec := httptest.NewRecorder()
	h.ServeHTTP(prevRec, prevReq)
	if prevRec.Code != http.StatusOK {
		t.Fatalf("preview: want 200, got %d", prevRec.Code)
	}
	if !strings.Contains(prevRec.Body.String(), "Queue downloads") {
		t.Fatalf("preview: modal HTML missing Queue downloads button")
	}

	// 3. Confirm.
	confirmForm := url.Values{}
	confirmForm.Set("manga_id", "7")
	confirmForm.Set("confirm", "1")
	confReq := httptest.NewRequest(http.MethodPost, "/api/bulk", strings.NewReader(confirmForm.Encode()))
	confReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	confRec := httptest.NewRecorder()
	h.ServeHTTP(confRec, confReq)
	if confRec.Code != http.StatusSeeOther {
		t.Fatalf("confirm: want 303, got %d", confRec.Code)
	}
	if len(st.savedBulkJobs) != 1 {
		t.Fatalf("confirm: expected 1 job, got %d", len(st.savedBulkJobs))
	}

	// 4. Render /downloads.
	// Seed bulkJobs since fakeStore's SaveBulkJob doesn't auto-append to bulkJobs.
	st.bulkJobs = append(st.bulkJobs, model.BulkJob{
		ID: 1, MangaID: 7, SourceID: "42",
		Title: "One Piece", SourceName: "MangaDex EN",
		Status: model.BulkJobRunning, TotalChapters: 3,
	})
	dlRec := httptest.NewRecorder()
	h.ServeHTTP(dlRec, httptest.NewRequest(http.MethodGet, "/downloads", nil))
	if !strings.Contains(dlRec.Body.String(), "One Piece") {
		t.Fatalf("/downloads missing the job we just created")
	}

	// 5. Pause (HX-Request).
	pauseReq := httptest.NewRequest(http.MethodPost, "/api/downloads/1/pause", nil)
	pauseReq.Header.Set("HX-Request", "true")
	pauseRec := httptest.NewRecorder()
	h.ServeHTTP(pauseRec, pauseReq)
	if pauseRec.Code != http.StatusOK {
		t.Fatalf("pause: want 200, got %d: %s", pauseRec.Code, pauseRec.Body.String())
	}
	if !strings.Contains(pauseRec.Body.String(), "pill-paused") {
		t.Errorf("pause: row fragment missing paused pill")
	}

	// 6. Delete (HX-Request).
	delReq := httptest.NewRequest(http.MethodPost, "/api/downloads/1/delete", nil)
	delReq.Header.Set("HX-Request", "true")
	delRec := httptest.NewRecorder()
	h.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Errorf("delete: want 200, got %d", delRec.Code)
	}
}
