package orchestrator

import (
	"context"
	"strings"
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
	erroredChapters     []erroredChapterCall
	fedChapters         []fedChapterCall
	stalledCutoff       time.Time // last olderThan passed to ListStalledFedChapters
	activityEntries     []model.ActivityEntry
}

type erroredChapterCall struct {
	jobID, chapterID int64
	reason           string
}

type fedChapterCall struct {
	jobID, chapterID int64
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

func (f *fakeStore) ListStalledFedChapters(jobID int64, olderThan time.Time) ([]model.BulkJobChapter, error) {
	f.stalledCutoff = olderThan
	rows := f.chaptersByJob[jobID]
	var out []model.BulkJobChapter
	for _, c := range rows {
		if c.State == model.BulkChapterFed && c.UpdatedAt.Before(olderThan) {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeStore) MarkBulkJobChapterErrored(jobID, chapterID int64, reason string) (bool, error) {
	f.erroredChapters = append(f.erroredChapters, erroredChapterCall{jobID, chapterID, reason})
	return true, nil
}

func (f *fakeStore) MarkBulkJobChapterFed(jobID, chapterID int64) error {
	f.fedChapters = append(f.fedChapters, fedChapterCall{jobID, chapterID})
	return nil
}

func (f *fakeStore) AddActivity(e model.ActivityEntry) error {
	f.activityEntries = append(f.activityEntries, e)
	return nil
}

// fakeSuwayomi tracks calls so tests can assert which source got fed.
type fakeSuwayomi struct {
	inFlight       map[string]int
	enqueueHistory []enqueueCall
	chapterMetas   map[int64]suwayomi.ChapterMeta // keyed by chapterID
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

func (f *fakeSuwayomi) GetChapterMeta(ctx context.Context, chapterID int64) (suwayomi.ChapterMeta, error) {
	if f.chapterMetas != nil {
		if m, ok := f.chapterMetas[chapterID]; ok {
			return m, nil
		}
	}
	return suwayomi.ChapterMeta{}, nil
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

// TestDetectStalledChapters_EmptyChapter_MarksErrored verifies that a fed
// chapter with PageCount==0 and QueueState=="ERROR" gets marked errored
// with a reason containing "empty chapter" when
// BulkAutoErrorEmptyChaptersDisabled==false.
func TestDetectStalledChapters_EmptyChapter_MarksErrored(t *testing.T) {
	stalledAt := time.Now().Add(-35 * time.Minute) // older than default 30-min stall timeout
	job := model.BulkJob{
		ID: 1, MangaID: 7, SourceID: "42", Status: model.BulkJobRunning,
		CreatedAt: stalledAt,
	}
	chapter := model.BulkJobChapter{
		JobID: 1, ChapterID: 200, State: model.BulkChapterFed,
		Tries: 1, UpdatedAt: stalledAt,
	}
	st := &fakeStore{
		settings: model.Settings{
			BulkMaxInFlight:                    5,
			BulkRefillThreshold:                2,
			BulkStallTimeoutMinutes:            30,
			BulkChapterMaxRetries:              3,
			BulkAutoErrorEmptyChaptersDisabled: false,
		},
		jobs:          []model.BulkJob{job},
		chaptersByJob: map[int64][]model.BulkJobChapter{1: {chapter}},
	}
	sw := &fakeSuwayomi{
		inFlight: map[string]int{},
		chapterMetas: map[int64]suwayomi.ChapterMeta{
			200: {PageCount: 0, IsDownloaded: false, QueueState: "ERROR", Tries: 1},
		},
	}
	o := New(st, sw)
	settings := st.settings
	if err := o.detectStalledChapters(context.Background(), job, settings); err != nil {
		t.Fatalf("detectStalledChapters: %v", err)
	}

	if len(st.erroredChapters) != 1 {
		t.Fatalf("want 1 MarkBulkJobChapterErrored call, got %d: %+v", len(st.erroredChapters), st.erroredChapters)
	}
	got := st.erroredChapters[0]
	if got.chapterID != 200 {
		t.Errorf("want chapterID=200, got %d", got.chapterID)
	}
	if !strings.Contains(got.reason, "empty chapter") {
		t.Errorf("want reason containing 'empty chapter', got %q", got.reason)
	}
	if len(sw.enqueueHistory) != 0 {
		t.Errorf("want no EnqueueChapterDownloads calls, got %d", len(sw.enqueueHistory))
	}
	if len(st.fedChapters) != 0 {
		t.Errorf("want no MarkBulkJobChapterFed calls, got %d", len(st.fedChapters))
	}
}

// TestDetectStalledChapters_MaxRetries_MarksErrored verifies that a chapter
// that has exceeded BulkChapterMaxRetries gets marked errored with a reason
// containing "gave up after".
func TestDetectStalledChapters_MaxRetries_MarksErrored(t *testing.T) {
	stalledAt := time.Now().Add(-35 * time.Minute)
	job := model.BulkJob{
		ID: 1, MangaID: 7, SourceID: "42", Status: model.BulkJobRunning,
		CreatedAt: stalledAt,
	}
	chapter := model.BulkJobChapter{
		JobID: 1, ChapterID: 300, State: model.BulkChapterFed,
		Tries: 3, UpdatedAt: stalledAt, // at the max-retries limit
	}
	st := &fakeStore{
		settings: model.Settings{
			BulkMaxInFlight:                    5,
			BulkRefillThreshold:                2,
			BulkStallTimeoutMinutes:            30,
			BulkChapterMaxRetries:              3,
			BulkAutoErrorEmptyChaptersDisabled: false,
		},
		jobs:          []model.BulkJob{job},
		chaptersByJob: map[int64][]model.BulkJobChapter{1: {chapter}},
	}
	sw := &fakeSuwayomi{
		inFlight: map[string]int{},
		chapterMetas: map[int64]suwayomi.ChapterMeta{
			// PageCount=12 so the empty-chapter branch doesn't fire first
			300: {PageCount: 12, IsDownloaded: false, QueueState: "ERROR", Tries: 3},
		},
	}
	o := New(st, sw)
	settings := st.settings
	if err := o.detectStalledChapters(context.Background(), job, settings); err != nil {
		t.Fatalf("detectStalledChapters: %v", err)
	}

	if len(st.erroredChapters) != 1 {
		t.Fatalf("want 1 MarkBulkJobChapterErrored call, got %d: %+v", len(st.erroredChapters), st.erroredChapters)
	}
	got := st.erroredChapters[0]
	if got.chapterID != 300 {
		t.Errorf("want chapterID=300, got %d", got.chapterID)
	}
	if !strings.Contains(got.reason, "gave up after") {
		t.Errorf("want reason containing 'gave up after', got %q", got.reason)
	}
	if len(sw.enqueueHistory) != 0 {
		t.Errorf("want no EnqueueChapterDownloads calls, got %d", len(sw.enqueueHistory))
	}
	if len(st.fedChapters) != 0 {
		t.Errorf("want no MarkBulkJobChapterFed calls, got %d", len(st.fedChapters))
	}
}

// TestDetectStalledChapters_StillQueued_Refeeds verifies that a stalled
// chapter still in Suwayomi's queue (QueueState=="Queued") is re-fed rather
// than errored when tries < BulkChapterMaxRetries.
func TestDetectStalledChapters_StillQueued_Refeeds(t *testing.T) {
	stalledAt := time.Now().Add(-35 * time.Minute)
	job := model.BulkJob{
		ID: 1, MangaID: 7, SourceID: "42", Status: model.BulkJobRunning,
		CreatedAt: stalledAt,
	}
	chapter := model.BulkJobChapter{
		JobID: 1, ChapterID: 400, State: model.BulkChapterFed,
		Tries: 1, UpdatedAt: stalledAt,
	}
	st := &fakeStore{
		settings: model.Settings{
			BulkMaxInFlight:                    5,
			BulkRefillThreshold:                2,
			BulkStallTimeoutMinutes:            30,
			BulkChapterMaxRetries:              3,
			BulkAutoErrorEmptyChaptersDisabled: false,
		},
		jobs:          []model.BulkJob{job},
		chaptersByJob: map[int64][]model.BulkJobChapter{1: {chapter}},
	}
	sw := &fakeSuwayomi{
		inFlight: map[string]int{},
		chapterMetas: map[int64]suwayomi.ChapterMeta{
			400: {PageCount: 12, IsDownloaded: false, QueueState: "Queued", Tries: 1},
		},
	}
	o := New(st, sw)
	settings := st.settings
	if err := o.detectStalledChapters(context.Background(), job, settings); err != nil {
		t.Fatalf("detectStalledChapters: %v", err)
	}

	if len(sw.enqueueHistory) != 1 {
		t.Fatalf("want 1 EnqueueChapterDownloads call, got %d: %+v", len(sw.enqueueHistory), sw.enqueueHistory)
	}
	enqueued := sw.enqueueHistory[0].chapterIDs
	if len(enqueued) != 1 || enqueued[0] != 400 {
		t.Errorf("want chapterID 400 enqueued, got %v", enqueued)
	}
	if len(st.fedChapters) != 1 {
		t.Fatalf("want 1 MarkBulkJobChapterFed call, got %d: %+v", len(st.fedChapters), st.fedChapters)
	}
	if st.fedChapters[0].chapterID != 400 {
		t.Errorf("want chapterID=400 in MarkBulkJobChapterFed, got %d", st.fedChapters[0].chapterID)
	}
	if len(st.erroredChapters) != 0 {
		t.Errorf("want 0 MarkBulkJobChapterErrored calls, got %d: %+v", len(st.erroredChapters), st.erroredChapters)
	}
}

// TestDetectStalledChapters_WritesActivityOnErrored verifies that when a
// chapter is successfully marked errored, detectStalledChapters writes
// exactly one ActivityEntry with ActionBulkChapterErrored, the correct
// SeriesTitle, Via="bulk:<sourceName>", and a Detail containing the
// chapter ID and erroring reason.
func TestDetectStalledChapters_WritesActivityOnErrored(t *testing.T) {
	stalledAt := time.Now().Add(-35 * time.Minute)
	job := model.BulkJob{
		ID: 1, MangaID: 7, SourceID: "42", Title: "Attack on Titan", SourceName: "MangaDex JP",
		Status: model.BulkJobRunning, CreatedAt: stalledAt,
	}
	chapter := model.BulkJobChapter{
		JobID: 1, ChapterID: 500, State: model.BulkChapterFed,
		Tries: 1, UpdatedAt: stalledAt,
	}
	st := &fakeStore{
		settings: model.Settings{
			BulkMaxInFlight:                    5,
			BulkRefillThreshold:                2,
			BulkStallTimeoutMinutes:            30,
			BulkChapterMaxRetries:              3,
			BulkAutoErrorEmptyChaptersDisabled: false,
		},
		jobs:          []model.BulkJob{job},
		chaptersByJob: map[int64][]model.BulkJobChapter{1: {chapter}},
	}
	sw := &fakeSuwayomi{
		inFlight: map[string]int{},
		chapterMetas: map[int64]suwayomi.ChapterMeta{
			500: {PageCount: 0, IsDownloaded: false, QueueState: "ERROR", Tries: 1},
		},
	}
	o := New(st, sw)
	settings := st.settings
	if err := o.detectStalledChapters(context.Background(), job, settings); err != nil {
		t.Fatalf("detectStalledChapters: %v", err)
	}

	// Chapter must be marked errored.
	if len(st.erroredChapters) != 1 {
		t.Fatalf("want 1 MarkBulkJobChapterErrored call, got %d", len(st.erroredChapters))
	}

	// Activity entry must be written.
	if len(st.activityEntries) != 1 {
		t.Fatalf("want 1 activity entry, got %d: %+v", len(st.activityEntries), st.activityEntries)
	}
	entry := st.activityEntries[0]
	if entry.Action != model.ActionBulkChapterErrored {
		t.Errorf("activity Action: want %q, got %q", model.ActionBulkChapterErrored, entry.Action)
	}
	if entry.SeriesTitle != job.Title {
		t.Errorf("activity SeriesTitle: want %q, got %q", job.Title, entry.SeriesTitle)
	}
	wantVia := "bulk:" + job.SourceName
	if entry.Via != wantVia {
		t.Errorf("activity Via: want %q, got %q", wantVia, entry.Via)
	}
	if !strings.Contains(entry.Detail, "500") {
		t.Errorf("activity Detail should contain chapter id 500, got %q", entry.Detail)
	}
	if !strings.Contains(entry.Detail, "empty chapter") {
		t.Errorf("activity Detail should contain erroring reason, got %q", entry.Detail)
	}
}
