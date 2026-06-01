package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/suwayomi"
)

// fakeStore is the minimum surface Tick needs in this task.
type fakeStore struct {
	jobs           []model.BulkJob
	chaptersByJob  map[int64][]model.BulkJobChapter
	settings       model.Settings
	feedHistory    []feedCall
	statusHistory  []statusCall
	chapterUpdates []chapterUpdateCall
}

type feedCall struct {
	jobID    int64
	chapters []int64
}
type statusCall struct {
	jobID  int64
	status model.BulkJobStatus
}
type chapterUpdateCall struct {
	jobID, chapterID int64
	state            model.BulkChapterState
}

func (f *fakeStore) ListBulkJobs(s model.BulkJobStatus) ([]model.BulkJob, error) {
	if s == "" {
		return f.jobs, nil
	}
	var out []model.BulkJob
	for _, j := range f.jobs {
		if j.Status == s {
			out = append(out, j)
		}
	}
	return out, nil
}

func (f *fakeStore) ListBulkJobChapters(jobID int64, state model.BulkChapterState) ([]model.BulkJobChapter, error) {
	rows := f.chaptersByJob[jobID]
	if state == "" {
		return rows, nil
	}
	var out []model.BulkJobChapter
	for _, c := range rows {
		if c.State == state {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeStore) UpdateBulkJobChapterState(jobID, chapterID int64, state model.BulkChapterState) error {
	f.chapterUpdates = append(f.chapterUpdates, chapterUpdateCall{jobID, chapterID, state})
	return nil
}

func (f *fakeStore) UpdateBulkJobStatus(id int64, s model.BulkJobStatus) error {
	f.statusHistory = append(f.statusHistory, statusCall{id, s})
	return nil
}

func (f *fakeStore) GetSettings() (model.Settings, error) {
	return f.settings, nil
}

// fakeSuwayomi tracks calls so tests can assert which source got fed.
type fakeSuwayomi struct {
	inFlight       map[string]int
	enqueueHistory []enqueueCall
}

type enqueueCall struct {
	chapterIDs []int64
}

func (f *fakeSuwayomi) InFlightCountForSource(ctx context.Context, sourceID string) (int, error) {
	return f.inFlight[sourceID], nil
}

func (f *fakeSuwayomi) EnqueueChapterDownloads(ctx context.Context, ids []int64) error {
	f.enqueueHistory = append(f.enqueueHistory, enqueueCall{chapterIDs: ids})
	return nil
}

func (f *fakeSuwayomi) ListChapters(ctx context.Context, mangaID int64) ([]suwayomi.Chapter, error) {
	return nil, nil
}

func TestTickPicksOneJobPerSource(t *testing.T) {
	now := time.Now()
	st := &fakeStore{
		settings: model.Settings{BulkMaxInFlight: 5, BulkRefillThreshold: 2},
		jobs: []model.BulkJob{
			// Same source_id, two jobs — only the older (created_at earlier) should feed.
			{ID: 1, SourceID: "42", Status: model.BulkJobRunning, CreatedAt: now.Add(-2 * time.Minute)},
			{ID: 2, SourceID: "42", Status: model.BulkJobRunning, CreatedAt: now.Add(-1 * time.Minute)},
		},
		chaptersByJob: map[int64][]model.BulkJobChapter{
			1: {{JobID: 1, ChapterID: 100, State: model.BulkChapterPending},
				{JobID: 1, ChapterID: 101, State: model.BulkChapterPending}},
			2: {{JobID: 2, ChapterID: 200, State: model.BulkChapterPending}},
		},
	}
	sw := &fakeSuwayomi{inFlight: map[string]int{}}
	o := New(st, sw)

	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if len(sw.enqueueHistory) != 1 {
		t.Fatalf("want 1 enqueue call (FIFO per source), got %d: %+v", len(sw.enqueueHistory), sw.enqueueHistory)
	}
	if sw.enqueueHistory[0].chapterIDs[0] != 100 {
		t.Errorf("want first job's chapters fed, got chapter %d", sw.enqueueHistory[0].chapterIDs[0])
	}
}

func TestTickRunsDifferentSourcesInParallel(t *testing.T) {
	now := time.Now()
	st := &fakeStore{
		settings: model.Settings{BulkMaxInFlight: 5, BulkRefillThreshold: 2},
		jobs: []model.BulkJob{
			{ID: 1, SourceID: "42", Status: model.BulkJobRunning, CreatedAt: now.Add(-2 * time.Minute)},
			{ID: 2, SourceID: "99", Status: model.BulkJobRunning, CreatedAt: now.Add(-1 * time.Minute)},
		},
		chaptersByJob: map[int64][]model.BulkJobChapter{
			1: {{JobID: 1, ChapterID: 100, State: model.BulkChapterPending}},
			2: {{JobID: 2, ChapterID: 200, State: model.BulkChapterPending}},
		},
	}
	sw := &fakeSuwayomi{inFlight: map[string]int{}}
	o := New(st, sw)

	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(sw.enqueueHistory) != 2 {
		t.Fatalf("want 2 enqueue calls (different sources in parallel), got %d", len(sw.enqueueHistory))
	}
}

func TestTickSkipsWhenInFlightAboveThreshold(t *testing.T) {
	now := time.Now()
	st := &fakeStore{
		settings: model.Settings{BulkMaxInFlight: 5, BulkRefillThreshold: 2},
		jobs: []model.BulkJob{
			{ID: 1, SourceID: "42", Status: model.BulkJobRunning, CreatedAt: now},
		},
		chaptersByJob: map[int64][]model.BulkJobChapter{
			1: {{JobID: 1, ChapterID: 100, State: model.BulkChapterPending}},
		},
	}
	sw := &fakeSuwayomi{inFlight: map[string]int{"42": 4}} // > threshold
	o := New(st, sw)

	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(sw.enqueueHistory) != 0 {
		t.Errorf("expected NO enqueue when in_flight > threshold, got %d", len(sw.enqueueHistory))
	}
}

func TestTickHonoursBackoffUntil(t *testing.T) {
	future := time.Now().Add(5 * time.Minute)
	st := &fakeStore{
		settings: model.Settings{BulkMaxInFlight: 5, BulkRefillThreshold: 2},
		jobs: []model.BulkJob{
			{ID: 1, SourceID: "42", Status: model.BulkJobRunning, BackoffUntil: &future, CreatedAt: time.Now()},
		},
		chaptersByJob: map[int64][]model.BulkJobChapter{
			1: {{JobID: 1, ChapterID: 100, State: model.BulkChapterPending}},
		},
	}
	sw := &fakeSuwayomi{inFlight: map[string]int{}}
	o := New(st, sw)

	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(sw.enqueueHistory) != 0 {
		t.Errorf("expected NO enqueue while backoff_until is in the future, got %d", len(sw.enqueueHistory))
	}
}
