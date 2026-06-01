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
	jobs                []model.BulkJob
	chaptersByJob       map[int64][]model.BulkJobChapter
	settings            model.Settings
	feedHistory         []feedCall
	statusHistory       []statusCall
	chapterUpdates      []chapterUpdateCall
	backoffHistory      []backoffCall
	clearBackoffHistory []int64
	completedBumps      []int64
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
type backoffCall struct {
	jobID          int64
	until          time.Time
	consecFailures int
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

func (f *fakeStore) UpdateBulkJobBackoff(jobID int64, until time.Time, consecFailures int, lastError string) error {
	f.backoffHistory = append(f.backoffHistory, backoffCall{jobID, until, consecFailures})
	return nil
}

func (f *fakeStore) ClearBulkJobBackoff(jobID int64) error {
	f.clearBackoffHistory = append(f.clearBackoffHistory, jobID)
	return nil
}

func (f *fakeStore) IncrementBulkJobCompletedChapters(jobID int64) error {
	f.completedBumps = append(f.completedBumps, jobID)
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

// fakeSuwayomiWithChapters extends fakeSuwayomi with a per-manga chapter
// list so reconcile-phase tests can drive isDownloaded transitions.
type fakeSuwayomiWithChapters struct {
	fakeSuwayomi
	chaptersByManga map[int64][]suwayomi.Chapter
}

func (f *fakeSuwayomiWithChapters) ListChapters(ctx context.Context, mangaID int64) ([]suwayomi.Chapter, error) {
	return f.chaptersByManga[mangaID], nil
}

func TestTickReconcilesFedToDoneOnIsDownloaded(t *testing.T) {
	now := time.Now()
	st := &fakeStore{
		settings: model.Settings{BulkMaxInFlight: 5, BulkRefillThreshold: 2},
		jobs: []model.BulkJob{
			{ID: 1, MangaID: 7, SourceID: "42", Status: model.BulkJobRunning, CreatedAt: now},
		},
		chaptersByJob: map[int64][]model.BulkJobChapter{
			1: {
				{JobID: 1, ChapterID: 100, State: model.BulkChapterFed},
				{JobID: 1, ChapterID: 101, State: model.BulkChapterFed},
				{JobID: 1, ChapterID: 102, State: model.BulkChapterPending},
			},
		},
	}
	sw := &fakeSuwayomiWithChapters{
		fakeSuwayomi: fakeSuwayomi{inFlight: map[string]int{"42": 1}},
		chaptersByManga: map[int64][]suwayomi.Chapter{
			7: {
				{ID: 100, IsDownloaded: true},  // should flip fed→done
				{ID: 101, IsDownloaded: false}, // still fed
				{ID: 102, IsDownloaded: false}, // still pending
			},
		},
	}
	o := New(st, sw)
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// Look for the chapter 100 → 'done' update.
	var sawDone bool
	for _, u := range st.chapterUpdates {
		if u.chapterID == 100 && u.state == model.BulkChapterDone {
			sawDone = true
		}
	}
	if !sawDone {
		t.Errorf("expected chapter 100 to flip to 'done' on isDownloaded=true; got updates: %+v", st.chapterUpdates)
	}
	// T12 carry-forward: exactly one completed_chapters bump for the single
	// chapter that flipped to 'done'. Guards against the regression where
	// reconcile only updated chapter state and left the job-level counter
	// stale (so GET /api/bulk/jobs reported 0 forever).
	if len(st.completedBumps) != 1 || st.completedBumps[0] != 1 {
		t.Errorf("expected exactly one IncrementBulkJobCompletedChapters(1) call; got %+v", st.completedBumps)
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

type fakeSuwayomi429 struct {
	fakeSuwayomi
	failNTimes int
	called     int
}

func (f *fakeSuwayomi429) EnqueueChapterDownloads(ctx context.Context, ids []int64) error {
	f.called++
	if f.called <= f.failNTimes {
		return suwayomi.ErrHTTP429
	}
	f.enqueueHistory = append(f.enqueueHistory, enqueueCall{chapterIDs: ids})
	return nil
}
func (f *fakeSuwayomi429) ListChapters(ctx context.Context, mangaID int64) ([]suwayomi.Chapter, error) {
	return nil, nil
}

func TestTickBackoffLadderProgresses(t *testing.T) {
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
	sw := &fakeSuwayomi429{
		fakeSuwayomi: fakeSuwayomi{inFlight: map[string]int{}},
		failNTimes:   1,
	}
	o := New(st, sw)
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	// The 429 should have set a backoff_until ~5s in the future.
	if len(st.backoffHistory) != 1 {
		t.Fatalf("expected 1 backoff update; got %d", len(st.backoffHistory))
	}
	bo := st.backoffHistory[0]
	if bo.consecFailures != 1 {
		t.Errorf("consecutive_failures: want 1, got %d", bo.consecFailures)
	}
	delta := time.Until(bo.until)
	if delta < 4*time.Second || delta > 6*time.Second {
		t.Errorf("backoff ladder 1st rung: want ~5s, got %v", delta)
	}
}

func TestTickResetsConsecFailuresOnSuccess(t *testing.T) {
	now := time.Now()
	st := &fakeStore{
		settings: model.Settings{BulkMaxInFlight: 5, BulkRefillThreshold: 2},
		jobs: []model.BulkJob{
			{ID: 1, SourceID: "42", Status: model.BulkJobRunning, CreatedAt: now, ConsecutiveFailures: 3},
		},
		chaptersByJob: map[int64][]model.BulkJobChapter{
			1: {{JobID: 1, ChapterID: 100, State: model.BulkChapterPending}},
		},
	}
	sw := &fakeSuwayomi429{
		fakeSuwayomi: fakeSuwayomi{inFlight: map[string]int{}},
		failNTimes:   0, // never fail
	}
	o := New(st, sw)
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(st.clearBackoffHistory) != 1 || st.clearBackoffHistory[0] != 1 {
		t.Errorf("expected ClearBulkJobBackoff(1) call; got %+v", st.clearBackoffHistory)
	}
}

func TestTickMarksJobCompletedWhenAllChaptersDone(t *testing.T) {
	now := time.Now()
	st := &fakeStore{
		settings: model.Settings{BulkMaxInFlight: 5, BulkRefillThreshold: 2},
		jobs: []model.BulkJob{
			{ID: 1, MangaID: 7, SourceID: "42", Status: model.BulkJobRunning, CreatedAt: now, TotalChapters: 2, CompletedChapters: 2},
		},
		chaptersByJob: map[int64][]model.BulkJobChapter{
			1: {
				{JobID: 1, ChapterID: 100, State: model.BulkChapterDone},
				{JobID: 1, ChapterID: 101, State: model.BulkChapterDone},
			},
		},
	}
	sw := &fakeSuwayomiWithChapters{
		fakeSuwayomi: fakeSuwayomi{inFlight: map[string]int{}},
	}
	o := New(st, sw)
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	var sawCompleted bool
	for _, s := range st.statusHistory {
		if s.jobID == 1 && s.status == model.BulkJobCompleted {
			sawCompleted = true
		}
	}
	if !sawCompleted {
		t.Errorf("expected job 1 to transition to 'completed'; got %+v", st.statusHistory)
	}
}

func TestTickMarksJobErroredAfter5ConsecutiveFailures(t *testing.T) {
	now := time.Now()
	st := &fakeStore{
		settings: model.Settings{BulkMaxInFlight: 5, BulkRefillThreshold: 2},
		jobs: []model.BulkJob{
			{ID: 1, SourceID: "42", Status: model.BulkJobRunning, CreatedAt: now, ConsecutiveFailures: 4},
		},
		chaptersByJob: map[int64][]model.BulkJobChapter{
			1: {{JobID: 1, ChapterID: 100, State: model.BulkChapterPending}},
		},
	}
	sw := &fakeSuwayomi429{
		fakeSuwayomi: fakeSuwayomi{inFlight: map[string]int{}},
		failNTimes:   1, // the 5th failure overall
	}
	o := New(st, sw)
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	var sawErrored bool
	for _, s := range st.statusHistory {
		if s.jobID == 1 && s.status == model.BulkJobErrored {
			sawErrored = true
		}
	}
	if !sawErrored {
		t.Errorf("expected job 1 to transition to 'errored' after 5th failure; got status history %+v", st.statusHistory)
	}
}
