package store

import (
	"testing"
	"time"

	"github.com/gavinmcfall/mangarr/internal/model"
)

func TestSaveBulkJobAssignsID(t *testing.T) {
	s := newTestStore(t)
	in := model.BulkJob{
		MangaID: 1, SourceID: "42",
		Title: "One Piece", SourceName: "MangaDex EN",
		Status:        model.BulkJobPending,
		TotalChapters: 1076,
	}
	id, err := s.SaveBulkJob(in)
	if err != nil {
		t.Fatalf("SaveBulkJob: %v", err)
	}
	if id <= 0 {
		t.Errorf("want id > 0, got %d", id)
	}
}

func TestGetBulkJobRoundTrip(t *testing.T) {
	s := newTestStore(t)
	in := model.BulkJob{
		MangaID: 1, SourceID: "42",
		Title: "Solo Leveling", SourceName: "MangaDex EN",
		Status:        model.BulkJobRunning,
		TotalChapters: 200, CompletedChapters: 47,
	}
	id, err := s.SaveBulkJob(in)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.GetBulkJob(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != id || got.MangaID != 1 || got.SourceID != "42" || got.Title != "Solo Leveling" {
		t.Errorf("round trip mismatch: %+v", got)
	}
	if got.Status != model.BulkJobRunning || got.TotalChapters != 200 || got.CompletedChapters != 47 {
		t.Errorf("round trip mismatch: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("timestamps not populated: %+v", got)
	}
}

func TestListBulkJobsFiltersByStatus(t *testing.T) {
	s := newTestStore(t)
	for _, st := range []model.BulkJobStatus{
		model.BulkJobRunning,
		model.BulkJobRunning,
		model.BulkJobPaused,
		model.BulkJobCompleted,
	} {
		if _, err := s.SaveBulkJob(model.BulkJob{
			MangaID: 1, SourceID: "1", Title: "x", SourceName: "y", Status: st,
		}); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	running, err := s.ListBulkJobs(model.BulkJobRunning)
	if err != nil {
		t.Fatalf("list running: %v", err)
	}
	if len(running) != 2 {
		t.Errorf("want 2 running, got %d", len(running))
	}
	all, err := s.ListBulkJobs("")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 4 {
		t.Errorf("want 4 total, got %d", len(all))
	}
}

func TestUpdateBulkJobStatusFlipsAndBumpsTimestamp(t *testing.T) {
	s := newTestStore(t)
	id, err := s.SaveBulkJob(model.BulkJob{
		MangaID: 1, SourceID: "1", Title: "x", SourceName: "y",
		Status: model.BulkJobRunning,
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	before, _ := s.GetBulkJob(id)

	time.Sleep(1100 * time.Millisecond) // SQLite strftime('%s') is whole-second resolution
	if err := s.UpdateBulkJobStatus(id, model.BulkJobPaused); err != nil {
		t.Fatalf("update: %v", err)
	}
	after, err := s.GetBulkJob(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Status != model.BulkJobPaused {
		t.Errorf("status: want paused, got %q", after.Status)
	}
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Errorf("updated_at should bump on status flip")
	}
}

func TestBatchInsertBulkJobChapters(t *testing.T) {
	s := newTestStore(t)
	jobID, err := s.SaveBulkJob(model.BulkJob{
		MangaID: 1, SourceID: "1", Title: "x", SourceName: "y", Status: model.BulkJobPending,
	})
	if err != nil {
		t.Fatalf("SaveBulkJob: %v", err)
	}
	if err := s.BatchInsertBulkJobChapters(jobID, []int64{100, 101, 102}); err != nil {
		t.Fatalf("BatchInsert: %v", err)
	}
	got, err := s.ListBulkJobChapters(jobID, model.BulkChapterPending)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("want 3 pending chapters, got %d", len(got))
	}

	// Idempotency: re-inserting the same chapter IDs is a no-op via
	// INSERT OR IGNORE, so the row count doesn't change.
	if err := s.BatchInsertBulkJobChapters(jobID, []int64{100, 101, 102}); err != nil {
		t.Fatalf("BatchInsert re-run: %v", err)
	}
	got, _ = s.ListBulkJobChapters(jobID, "")
	if len(got) != 3 {
		t.Errorf("INSERT OR IGNORE should keep row count at 3, got %d", len(got))
	}

	// Empty slice is a no-op (no transaction overhead, no error).
	if err := s.BatchInsertBulkJobChapters(jobID, nil); err != nil {
		t.Fatalf("BatchInsert empty: %v", err)
	}
}

func TestUpdateBulkJobChapterState(t *testing.T) {
	s := newTestStore(t)
	jobID, err := s.SaveBulkJob(model.BulkJob{
		MangaID: 1, SourceID: "1", Title: "x", SourceName: "y", Status: model.BulkJobPending,
	})
	if err != nil {
		t.Fatalf("SaveBulkJob: %v", err)
	}
	if err := s.BatchInsertBulkJobChapters(jobID, []int64{100, 101}); err != nil {
		t.Fatalf("BatchInsert: %v", err)
	}

	if err := s.UpdateBulkJobChapterState(jobID, 100, model.BulkChapterFed); err != nil {
		t.Fatalf("update: %v", err)
	}
	pending, _ := s.ListBulkJobChapters(jobID, model.BulkChapterPending)
	fed, _ := s.ListBulkJobChapters(jobID, model.BulkChapterFed)
	if len(pending) != 1 || len(fed) != 1 {
		t.Errorf("want pending=1 fed=1, got pending=%d fed=%d", len(pending), len(fed))
	}
}

func TestBulkJobChaptersCascadeDeleteOnJobDelete(t *testing.T) {
	s := newTestStore(t)
	jobID, err := s.SaveBulkJob(model.BulkJob{
		MangaID: 1, SourceID: "1", Title: "x", SourceName: "y", Status: model.BulkJobPending,
	})
	if err != nil {
		t.Fatalf("SaveBulkJob: %v", err)
	}
	if err := s.BatchInsertBulkJobChapters(jobID, []int64{100, 101, 102}); err != nil {
		t.Fatalf("BatchInsert: %v", err)
	}
	if err := s.DeleteBulkJob(jobID); err != nil {
		t.Fatalf("DeleteBulkJob: %v", err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM bulk_job_chapters WHERE job_id = ?`, jobID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("expected cascade delete; %d chapter rows remain", n)
	}
}

func TestSaveLibraryCacheEntryUpsertByMangaID(t *testing.T) {
	s := newTestStore(t)
	in := model.LibraryCacheEntry{
		MangaID: 7, Title: "One Piece", SourceID: "42", SourceName: "MangaDex EN",
		TotalChapters: 1076, Downloaded: 0,
	}
	if err := s.SaveLibraryCacheEntry(in); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Upsert: same manga_id, updated counts.
	in.Downloaded = 47
	if err := s.SaveLibraryCacheEntry(in); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.GetLibraryCacheEntry(7)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Downloaded != 47 || got.TotalChapters != 1076 {
		t.Errorf("upsert didn't take: %+v", got)
	}
	if got.RefreshedAt.IsZero() {
		t.Errorf("refreshed_at should be populated on upsert")
	}
}

func TestListLibraryCacheEntries(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []int64{1, 3, 2} {
		if err := s.SaveLibraryCacheEntry(model.LibraryCacheEntry{
			MangaID: id, Title: "x", SourceID: "1", SourceName: "y",
			TotalChapters: int(id * 10), Downloaded: 0,
		}); err != nil {
			t.Fatalf("save %d: %v", id, err)
		}
	}
	got, err := s.ListLibraryCacheEntries()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("want 3 entries, got %d", len(got))
	}
}

// seedJobAndFedChapter is a shared helper that creates a bulk job with one
// chapter in 'fed' state and returns both IDs.
func seedJobAndFedChapter(t *testing.T, s *Store) (jobID, chapterID int64) {
	t.Helper()
	var err error
	jobID, err = s.SaveBulkJob(model.BulkJob{
		MangaID: 1, SourceID: "1", Title: "test", SourceName: "MangaDex EN",
		Status: model.BulkJobRunning, TotalChapters: 1,
	})
	if err != nil {
		t.Fatalf("SaveBulkJob: %v", err)
	}
	chapterID = int64(200)
	if err := s.BatchInsertBulkJobChapters(jobID, []int64{chapterID}); err != nil {
		t.Fatalf("BatchInsertBulkJobChapters: %v", err)
	}
	if err := s.UpdateBulkJobChapterState(jobID, chapterID, model.BulkChapterFed); err != nil {
		t.Fatalf("UpdateBulkJobChapterState to fed: %v", err)
	}
	return jobID, chapterID
}

func TestMarkBulkJobChapterErroredHappyPath(t *testing.T) {
	s := newTestStore(t)
	jobID, chapterID := seedJobAndFedChapter(t, s)

	marked, err := s.MarkBulkJobChapterErrored(jobID, chapterID, "test reason")
	if err != nil {
		t.Fatalf("MarkBulkJobChapterErrored: %v", err)
	}
	if !marked {
		t.Errorf("MarkBulkJobChapterErrored: want marked=true on first call, got false")
	}

	// Assert chapter state = errored, ErroredReason populated.
	ch, err := s.GetBulkJobChapter(jobID, chapterID)
	if err != nil {
		t.Fatalf("GetBulkJobChapter: %v", err)
	}
	if ch.State != model.BulkChapterErrored {
		t.Errorf("chapter state: want %q, got %q", model.BulkChapterErrored, ch.State)
	}
	if ch.ErroredReason != "test reason" {
		t.Errorf("ErroredReason: want %q, got %q", "test reason", ch.ErroredReason)
	}

	// Assert job counters.
	job, err := s.GetBulkJob(jobID)
	if err != nil {
		t.Fatalf("GetBulkJob: %v", err)
	}
	if job.ErroredChapters != 1 {
		t.Errorf("ErroredChapters: want 1, got %d", job.ErroredChapters)
	}
	if job.LastError != "test reason" {
		t.Errorf("LastError: want %q, got %q", "test reason", job.LastError)
	}
}

func TestMarkBulkJobChapterErroredIdempotent(t *testing.T) {
	s := newTestStore(t)
	jobID, chapterID := seedJobAndFedChapter(t, s)

	// First call: transitions chapter fed → errored, bumps ErroredChapters.
	marked1, err := s.MarkBulkJobChapterErrored(jobID, chapterID, "first reason")
	if err != nil {
		t.Fatalf("first MarkBulkJobChapterErrored: %v", err)
	}
	if !marked1 {
		t.Errorf("first MarkBulkJobChapterErrored: want marked=true, got false")
	}

	// Second call: chapter is already errored, must be a no-op and return false.
	marked2, err := s.MarkBulkJobChapterErrored(jobID, chapterID, "second reason")
	if err != nil {
		t.Fatalf("second MarkBulkJobChapterErrored: %v", err)
	}
	if marked2 {
		t.Errorf("second MarkBulkJobChapterErrored: want marked=false (idempotent no-op), got true")
	}

	job, err := s.GetBulkJob(jobID)
	if err != nil {
		t.Fatalf("GetBulkJob: %v", err)
	}
	if job.ErroredChapters != 1 {
		t.Errorf("ErroredChapters after double-call: want 1, got %d", job.ErroredChapters)
	}

	// Reason should still be from the first call (second call was a no-op).
	ch, err := s.GetBulkJobChapter(jobID, chapterID)
	if err != nil {
		t.Fatalf("GetBulkJobChapter: %v", err)
	}
	if ch.ErroredReason != "first reason" {
		t.Errorf("ErroredReason: want %q (first call), got %q", "first reason", ch.ErroredReason)
	}
}

func TestMarkBulkJobChapterFedBumpsTries(t *testing.T) {
	s := newTestStore(t)
	jobID, err := s.SaveBulkJob(model.BulkJob{
		MangaID: 1, SourceID: "1", Title: "test", SourceName: "MangaDex EN",
		Status: model.BulkJobRunning, TotalChapters: 1,
	})
	if err != nil {
		t.Fatalf("SaveBulkJob: %v", err)
	}
	chapterID := int64(300)
	if err := s.BatchInsertBulkJobChapters(jobID, []int64{chapterID}); err != nil {
		t.Fatalf("BatchInsertBulkJobChapters: %v", err)
	}

	// First feed: tries should become 1.
	if err := s.MarkBulkJobChapterFed(jobID, chapterID); err != nil {
		t.Fatalf("first MarkBulkJobChapterFed: %v", err)
	}
	ch, err := s.GetBulkJobChapter(jobID, chapterID)
	if err != nil {
		t.Fatalf("GetBulkJobChapter after first feed: %v", err)
	}
	if ch.State != model.BulkChapterFed {
		t.Errorf("state: want %q, got %q", model.BulkChapterFed, ch.State)
	}
	if ch.Tries != 1 {
		t.Errorf("tries after first feed: want 1, got %d", ch.Tries)
	}

	// Second feed (re-feed after stall): tries should become 2.
	if err := s.MarkBulkJobChapterFed(jobID, chapterID); err != nil {
		t.Fatalf("second MarkBulkJobChapterFed: %v", err)
	}
	ch, err = s.GetBulkJobChapter(jobID, chapterID)
	if err != nil {
		t.Fatalf("GetBulkJobChapter after second feed: %v", err)
	}
	if ch.Tries != 2 {
		t.Errorf("tries after second feed: want 2, got %d", ch.Tries)
	}
}

func TestMarkBulkJobChapterErroredSkipsAlreadyDone(t *testing.T) {
	s := newTestStore(t)
	jobID, chapterID := seedJobAndFedChapter(t, s)

	// Mark chapter done first (simulates normal completion before stall detector fires).
	if err := s.UpdateBulkJobChapterState(jobID, chapterID, model.BulkChapterDone); err != nil {
		t.Fatalf("UpdateBulkJobChapterState to done: %v", err)
	}

	// MarkBulkJobChapterErrored must be a no-op on an already-done chapter.
	markedDone, err := s.MarkBulkJobChapterErrored(jobID, chapterID, "should be ignored")
	if err != nil {
		t.Fatalf("MarkBulkJobChapterErrored on done chapter: %v", err)
	}
	if markedDone {
		t.Errorf("MarkBulkJobChapterErrored on done chapter: want marked=false, got true")
	}

	ch, err := s.GetBulkJobChapter(jobID, chapterID)
	if err != nil {
		t.Fatalf("GetBulkJobChapter: %v", err)
	}
	if ch.State != model.BulkChapterDone {
		t.Errorf("chapter state should stay done, got %q", ch.State)
	}

	job, err := s.GetBulkJob(jobID)
	if err != nil {
		t.Fatalf("GetBulkJob: %v", err)
	}
	if job.ErroredChapters != 0 {
		t.Errorf("ErroredChapters should stay 0, got %d", job.ErroredChapters)
	}
}

func TestLibraryCacheEntryCarriesDudAndFiled(t *testing.T) {
	s := newTestStore(t)
	in := model.LibraryCacheEntry{
		MangaID: 5, Title: "X", SourceID: "1", SourceName: "src",
		TotalChapters: 100, Downloaded: 90, DudCount: 2, FiledCount: 88,
	}
	if err := s.SaveLibraryCacheEntry(in); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetLibraryCacheEntry(5)
	if err != nil {
		t.Fatal(err)
	}
	if got.DudCount != 2 || got.FiledCount != 88 {
		t.Fatalf("dud=%d filed=%d, want 2/88", got.DudCount, got.FiledCount)
	}
	list, _ := s.ListLibraryCacheEntries()
	var found bool
	for _, e := range list {
		if e.MangaID == 5 {
			found = true
			if e.DudCount != 2 || e.FiledCount != 88 {
				t.Errorf("list dud=%d filed=%d, want 2/88", e.DudCount, e.FiledCount)
			}
		}
	}
	if !found {
		t.Fatal("entry not in ListLibraryCacheEntries")
	}
}

func TestPruneLibraryCacheExcept(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []int64{1, 2, 3} {
		if err := s.SaveLibraryCacheEntry(model.LibraryCacheEntry{MangaID: id, Title: "M", SourceID: "x", SourceName: "y", TotalChapters: 1, Downloaded: 1}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PruneLibraryCacheExcept([]int64{2})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("pruned %d, want 2", n)
	}
	list, _ := s.ListLibraryCacheEntries()
	if len(list) != 1 || list[0].MangaID != 2 {
		t.Errorf("kept = %v, want only manga 2", list)
	}
	// Empty keep must be a no-op — never wipe the whole cache.
	if n, _ := s.PruneLibraryCacheExcept(nil); n != 0 {
		t.Errorf("empty keep pruned %d, want 0", n)
	}
	if list, _ := s.ListLibraryCacheEntries(); len(list) != 1 {
		t.Errorf("empty keep changed cache: %v", list)
	}
}
