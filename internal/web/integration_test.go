package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/orchestrator"
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

// TestStalledJobDetectorEndToEnd proves the whole stall-detection pipeline
// composes end-to-end: reconcile flips a downloaded chapter to done, the stall
// detector errors an empty chapter, the terminal-state check marks the job
// completed, and the /downloads page renders the errored-count badge.
//
// Fixture layout:
//
//	manga_id=99, source_id="src1", source_name="MangaDex EN"
//	chapter 1: fed, stalled (updated_at far in past), IsDownloaded=true in Suwayomi
//	chapter 2: fed, stalled, PageCount=0 + QueueState=ERROR → empty-chapter error
//
// After one Tick():
//
//	chapter 1 → done  (reconcile phase)
//	chapter 2 → errored, reason="empty chapter …" (stall detector branch 2)
//	job → Status=completed, CompletedChapters=1, ErroredChapters=1
//
// HTTP assertions:
//
//	GET /api/downloads/list?filter=all → renders pill-error "1 missing"
//	fakeStore.activity → one entry with ActionBulkChapterErrored, Via="bulk:MangaDex EN"
func TestStalledJobDetectorEndToEnd(t *testing.T) {
	// ── Arrange ──────────────────────────────────────────────────────────────
	// A time far enough in the past to exceed the 30-minute default stall timeout.
	stalledAt := time.Now().Add(-60 * time.Minute)

	h, st, sw := newTestHandler()

	// Seed the fakeStore with one running job, two fed+stalled chapters.
	const (
		jobID    = int64(1)
		mangaID  = int64(99)
		sourceID = "src1"
		ch1      = int64(1)
		ch2      = int64(2)
	)
	st.bulkJobs = []model.BulkJob{{
		ID:            jobID,
		MangaID:       mangaID,
		SourceID:      sourceID,
		Title:         "Attack on Titan",
		SourceName:    "MangaDex EN",
		Status:        model.BulkJobRunning,
		TotalChapters: 2,
	}}
	st.chaptersByJob = map[int64][]model.BulkJobChapter{
		jobID: {
			{JobID: jobID, ChapterID: ch1, State: model.BulkChapterFed, Tries: 1, UpdatedAt: stalledAt},
			{JobID: jobID, ChapterID: ch2, State: model.BulkChapterFed, Tries: 1, UpdatedAt: stalledAt},
		},
	}

	// Suwayomi says chapter 1 is downloaded (reconcile path).
	sw.chaptersForManga = map[int64][]int64{mangaID: {ch1, ch2}}
	sw.chaptersDownloaded = map[int64]bool{ch1: true, ch2: false}

	// Suwayomi chapter-meta for stall detection:
	// ch1: already downloaded — stall detector skips (branch 1).
	// ch2: PageCount=0 + QueueState=ERROR → branch 2 (empty chapter auto-error).
	sw.chapterMetas = map[int64]suwayomi.ChapterMeta{
		ch1: {IsDownloaded: true, PageCount: 5, QueueState: ""},
		ch2: {IsDownloaded: false, PageCount: 0, QueueState: "ERROR", Tries: 1},
	}
	// inFlight=0 so the orchestrator doesn't skip the job (passes refill check).
	sw.inFlight = map[string]int{}

	// ── Act ───────────────────────────────────────────────────────────────────
	// Wire an orchestrator against the SAME fakeStore + fakeSuwayomi the
	// web Handler uses, so HTTP responses see the mutations made by Tick().
	o := orchestrator.New(st, sw)
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// ── Assert: per-chapter states ────────────────────────────────────────────
	chapters := st.chaptersByJob[jobID]
	if len(chapters) != 2 {
		t.Fatalf("expected 2 chapters, got %d", len(chapters))
	}
	var gotCh1, gotCh2 model.BulkJobChapter
	for _, c := range chapters {
		switch c.ChapterID {
		case ch1:
			gotCh1 = c
		case ch2:
			gotCh2 = c
		}
	}
	if gotCh1.State != model.BulkChapterDone {
		t.Errorf("chapter 1: want state=done, got %q", gotCh1.State)
	}
	if gotCh2.State != model.BulkChapterErrored {
		t.Errorf("chapter 2: want state=errored, got %q", gotCh2.State)
	}
	if !strings.Contains(gotCh2.ErroredReason, "empty chapter") {
		t.Errorf("chapter 2: want ErroredReason containing 'empty chapter', got %q", gotCh2.ErroredReason)
	}

	// ── Assert: job counters + status ─────────────────────────────────────────
	var job model.BulkJob
	for _, j := range st.bulkJobs {
		if j.ID == jobID {
			job = j
		}
	}
	if job.CompletedChapters != 1 {
		t.Errorf("job CompletedChapters: want 1, got %d", job.CompletedChapters)
	}
	if job.ErroredChapters != 1 {
		t.Errorf("job ErroredChapters: want 1, got %d", job.ErroredChapters)
	}
	if job.Status != model.BulkJobCompleted {
		t.Errorf("job Status: want completed, got %q", job.Status)
	}

	// ── Assert: /downloads renders pill-error with "1 missing" ────────────────
	dlReq := httptest.NewRequest(http.MethodGet, "/api/downloads/list?filter=all", nil)
	dlRec := httptest.NewRecorder()
	h.ServeHTTP(dlRec, dlReq)
	if dlRec.Code != http.StatusOK {
		t.Fatalf("GET /api/downloads/list: want 200, got %d; body: %s", dlRec.Code, dlRec.Body.String())
	}
	dlBody := dlRec.Body.String()
	if !strings.Contains(dlBody, "pill-error") {
		t.Errorf("/downloads list: want pill-error in HTML, body:\n%s", dlBody)
	}
	if !strings.Contains(dlBody, "1 missing") {
		t.Errorf("/downloads list: want '1 missing' badge, body:\n%s", dlBody)
	}

	// ── Assert: activity log has one bulk-chapter-errored entry ───────────────
	// fakeStore.activity is pre-pended by AddActivity, so index 0 is the newest.
	// The initial fakeStore has one pre-seeded activity entry; the orchestrator
	// adds another, so we want at least 1 entry with ActionBulkChapterErrored.
	var found bool
	for _, e := range st.activity {
		if e.Action != model.ActionBulkChapterErrored {
			continue
		}
		found = true
		wantVia := "bulk:MangaDex EN"
		if e.Via != wantVia {
			t.Errorf("activity Via: want %q, got %q", wantVia, e.Via)
		}
		if !strings.Contains(e.Detail, "2") { // chapter id=2
			t.Errorf("activity Detail should mention chapter id 2, got %q", e.Detail)
		}
	}
	if !found {
		t.Errorf("expected at least one ActionBulkChapterErrored activity entry; got %+v", st.activity)
	}
}
