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
