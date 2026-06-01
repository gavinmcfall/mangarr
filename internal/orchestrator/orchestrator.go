// Package orchestrator runs the bulk-download tick loop. On every tick
// (every 2 seconds when wired in main.go), it loads running jobs from
// the store, groups them by source_id, picks at most one job per source
// (FIFO by created_at), and feeds the next batch of chapters into
// Suwayomi's download queue when the in-flight count for that source
// is at or below the refill threshold from Settings.
package orchestrator

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/suwayomi"
)

// Store is the storage surface the orchestrator needs. *store.Store
// satisfies this directly via the methods added in Plan A Tasks 4 + 5.
type Store interface {
	ListBulkJobs(status model.BulkJobStatus) ([]model.BulkJob, error)
	ListBulkJobChapters(jobID int64, state model.BulkChapterState) ([]model.BulkJobChapter, error)
	UpdateBulkJobChapterState(jobID, chapterID int64, state model.BulkChapterState) error
	UpdateBulkJobStatus(id int64, status model.BulkJobStatus) error
	UpdateBulkJobBackoff(jobID int64, until time.Time, consecFailures int, lastError string) error
	ClearBulkJobBackoff(jobID int64) error
	GetSettings() (model.Settings, error)
}

// SuwayomiClient is the subset of *suwayomi.Client the orchestrator uses.
type SuwayomiClient interface {
	InFlightCountForSource(ctx context.Context, sourceID string) (int, error)
	EnqueueChapterDownloads(ctx context.Context, chapterIDs []int64) error
	ListChapters(ctx context.Context, mangaID int64) ([]suwayomi.Chapter, error)
}

// Orchestrator owns one Tick loop. It is goroutine-safe to share across
// the wiring boundary (main.go starts a single goroutine that calls Tick
// on a ticker), but Tick itself is NOT goroutine-safe — only one Tick
// may run at a time.
type Orchestrator struct {
	store    Store
	suwayomi SuwayomiClient
}

// New wires an Orchestrator.
func New(store Store, suwayomi SuwayomiClient) *Orchestrator {
	return &Orchestrator{store: store, suwayomi: suwayomi}
}

// Tick performs one orchestration pass:
//
//  1. Load all running jobs whose backoff_until is in the past (or nil).
//  2. Group by source_id; for each group, pick the FIFO-oldest by created_at.
//  3. For each picked job: ask Suwayomi for the in-flight count for that
//     source; skip if > refill_threshold.
//  4. Feed up to batch_size pending chapters into Suwayomi's queue.
//
// Errors from individual jobs do not abort the tick; the next job's turn
// continues. A non-nil error is returned only for failures that prevent
// any meaningful work (e.g. settings unreadable).
func (o *Orchestrator) Tick(ctx context.Context) error {
	settings, err := o.store.GetSettings()
	if err != nil {
		return err
	}
	maxInFlight := settings.BulkMaxInFlight
	refillThreshold := settings.BulkRefillThreshold
	if maxInFlight <= 0 {
		maxInFlight = 5
	}
	if refillThreshold < 0 {
		refillThreshold = 2
	}

	jobs, err := o.store.ListBulkJobs(model.BulkJobRunning)
	if err != nil {
		return err
	}
	now := time.Now()

	// Filter out jobs in active backoff.
	active := jobs[:0]
	for _, j := range jobs {
		if j.BackoffUntil != nil && j.BackoffUntil.After(now) {
			continue
		}
		active = append(active, j)
	}

	// Group by source_id; pick FIFO-oldest per group.
	bySource := map[string][]model.BulkJob{}
	for _, j := range active {
		bySource[j.SourceID] = append(bySource[j.SourceID], j)
	}
	for _, group := range bySource {
		sort.Slice(group, func(i, k int) bool {
			return group[i].CreatedAt.Before(group[k].CreatedAt)
		})
		job := group[0]

		inFlight, err := o.suwayomi.InFlightCountForSource(ctx, job.SourceID)
		if err != nil {
			// Suwayomi unreachable or error — skip this source for this tick.
			continue
		}
		if inFlight > refillThreshold {
			continue
		}

		// Reconcile phase: re-query Suwayomi's chapter list for this
		// job's manga and flip any fed chapters whose isDownloaded=true
		// to 'done'. We use ListChapters rather than downloadStatus
		// because Suwayomi removes FINISHED entries from its queue
		// after a short window — chapter.isDownloaded is the durable
		// source of truth (spec Section 2.4).
		fed, err := o.store.ListBulkJobChapters(job.ID, model.BulkChapterFed)
		if err == nil && len(fed) > 0 {
			chapters, err := o.suwayomi.ListChapters(ctx, job.MangaID)
			if err == nil {
				downloaded := map[int64]bool{}
				for _, c := range chapters {
					downloaded[c.ID] = c.IsDownloaded
				}
				for _, c := range fed {
					if downloaded[c.ChapterID] {
						_ = o.store.UpdateBulkJobChapterState(job.ID, c.ChapterID, model.BulkChapterDone)
					}
				}
			}
		}

		// Terminal state check: all chapters done → mark completed.
		allChapters, err := o.store.ListBulkJobChapters(job.ID, "")
		if err == nil && len(allChapters) > 0 {
			done := 0
			for _, c := range allChapters {
				if c.State == model.BulkChapterDone {
					done++
				}
			}
			if done == len(allChapters) {
				_ = o.store.UpdateBulkJobStatus(job.ID, model.BulkJobCompleted)
				continue
			}
		}

		// Feed the next batch.
		pending, err := o.store.ListBulkJobChapters(job.ID, model.BulkChapterPending)
		if err != nil {
			continue
		}
		if len(pending) == 0 {
			continue
		}
		// batch_size = min(MaxInFlight - inFlight, len(pending))
		room := maxInFlight - inFlight
		if room <= 0 {
			continue
		}
		batchSize := room
		if batchSize > len(pending) {
			batchSize = len(pending)
		}
		ids := make([]int64, batchSize)
		for i := 0; i < batchSize; i++ {
			ids[i] = pending[i].ChapterID
		}
		if err := o.suwayomi.EnqueueChapterDownloads(ctx, ids); err != nil {
			if errors.Is(err, suwayomi.ErrHTTP429) {
				next := job.ConsecutiveFailures + 1
				until, terminal := backoffFor(next, now)
				if terminal {
					_ = o.store.UpdateBulkJobBackoff(job.ID, until, next, "suwayomi 429 (5 consecutive failures)")
					_ = o.store.UpdateBulkJobStatus(job.ID, model.BulkJobErrored)
				} else {
					_ = o.store.UpdateBulkJobBackoff(job.ID, until, next, "suwayomi 429")
				}
			}
			continue
		}
		// Success path: clear any prior backoff state.
		if job.ConsecutiveFailures > 0 {
			_ = o.store.ClearBulkJobBackoff(job.ID)
		}
		for _, cid := range ids {
			_ = o.store.UpdateBulkJobChapterState(job.ID, cid, model.BulkChapterFed)
		}
	}
	return nil
}

// backoffFor returns (next backoff_until, terminal). The ladder is:
//
//	1st failure → 5s
//	2nd failure → 15s
//	3rd failure → 60s
//	4th failure → 5min
//	5th failure → terminal (caller marks job errored)
//
// Spec section "Backoff ladder" (Plan A).
func backoffFor(consecFailures int, now time.Time) (time.Time, bool) {
	switch consecFailures {
	case 1:
		return now.Add(5 * time.Second), false
	case 2:
		return now.Add(15 * time.Second), false
	case 3:
		return now.Add(60 * time.Second), false
	case 4:
		return now.Add(5 * time.Minute), false
	default:
		// 5th and beyond: terminal.
		return now, true
	}
}
