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
