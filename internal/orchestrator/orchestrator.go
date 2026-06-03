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
	"fmt"
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
	// IncrementBulkJobCompletedChapters is invoked once per chapter as the
	// reconcile phase flips its state from 'fed' to 'done'. Keeps the
	// BulkJob.CompletedChapters counter in lockstep with the per-chapter
	// state so GET /api/bulk/jobs returns live progress (the value written
	// by SaveBulkJob at creation is 0; without this bump it stays 0).
	IncrementBulkJobCompletedChapters(jobID int64) error
	GetSettings() (model.Settings, error)
	// ListStalledFedChapters returns fed chapters for a job whose updated_at
	// is older than olderThan, ordered by updated_at ASC (oldest first).
	// Used by detectStalledChapters to find chapters that Suwayomi may have
	// silently dropped from its queue.
	ListStalledFedChapters(jobID int64, olderThan time.Time) ([]model.BulkJobChapter, error)
	// MarkBulkJobChapterErrored atomically marks a chapter as errored and
	// bumps the parent job's errored_chapters counter. Idempotent: no-op
	// when the chapter is already 'done' or 'errored'. Returns (true, nil)
	// when the chapter was actually transitioned to errored; (false, nil)
	// when the chapter was already in a terminal state (idempotent no-op).
	MarkBulkJobChapterErrored(jobID, chapterID int64, reason string) (bool, error)
	// MarkBulkJobChapterFed marks a chapter as fed and bumps its mangarr-side
	// tries counter. Used by detectStalledChapters when re-feeding a chapter.
	MarkBulkJobChapterFed(jobID, chapterID int64) error
	// AddActivity appends one ActivityEntry to the activity log. Best-effort:
	// callers should swallow errors to avoid interrupting the surrounding
	// workflow on a log write failure.
	AddActivity(model.ActivityEntry) error
}

// SuwayomiClient is the subset of *suwayomi.Client the orchestrator uses.
type SuwayomiClient interface {
	InFlightCountForSource(ctx context.Context, sourceID string) (int, error)
	EnqueueChapterDownloads(ctx context.Context, chapterIDs []int64) error
	ListChapters(ctx context.Context, mangaID int64) ([]suwayomi.Chapter, error)
	// GetChapterMeta returns a stall-detection snapshot for one chapter:
	// its page count, download state, queue position, and Suwayomi's own
	// retry count. Used by detectStalledChapters for the four-way decision.
	GetChapterMeta(ctx context.Context, chapterID int64) (suwayomi.ChapterMeta, error)
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
						// Bump the job-level completed counter in lockstep so the
						// JSON API reports live progress rather than the value
						// SaveBulkJob wrote at creation (usually 0). Discard the
						// error per the surrounding skip-on-error pattern: a
						// transient SQLite failure here causes one tick's counter
						// drift, not a stuck job.
						_ = o.store.IncrementBulkJobCompletedChapters(job.ID)
					}
				}
			}
		}

		// Stall detection: probe Suwayomi for chapters that have been in
		// state='fed' longer than BulkStallTimeoutMinutes. Runs after the
		// reconcile phase so IsDownloaded=true chapters are already 'done'
		// and the stall detector won't re-feed them.
		_ = o.detectStalledChapters(ctx, job, settings)

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

// detectStalledChapters runs once per running job per tick, after the
// fed→done reconcile phase. It lists fed chapters that have been in that
// state longer than settings.BulkStallTimeoutMinutes and applies a four-way
// decision matrix for each:
//
//  1. IsDownloaded=true → reconcile already handled this tick; skip.
//  2. PageCount==0 AND QueueState=="ERROR" AND auto-error enabled → error.
//  3. QueueState=="ERROR" AND chapter.Tries >= BulkChapterMaxRetries → error.
//  4. QueueState in ("", "Queued", "Running") → re-feed via EnqueueChapterDownloads.
//
// Errors from individual chapter probes are skipped (Suwayomi may be
// transiently unreachable); the chapter will be re-evaluated on the next
// tick. Safe to call with an empty stalled list — returns nil immediately.
// Does NOT write activity log entries (T9 owns that via a wrapper).
func (o *Orchestrator) detectStalledChapters(ctx context.Context, job model.BulkJob, settings model.Settings) error {
	stallTimeout := settings.BulkStallTimeoutMinutes
	if stallTimeout <= 0 {
		stallTimeout = 30
	}
	maxRetries := settings.BulkChapterMaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}

	cutoff := time.Now().Add(-time.Duration(stallTimeout) * time.Minute)
	stalled, err := o.store.ListStalledFedChapters(job.ID, cutoff)
	if err != nil || len(stalled) == 0 {
		return err
	}

	for _, chapter := range stalled {
		meta, err := o.suwayomi.GetChapterMeta(ctx, chapter.ChapterID)
		if err != nil {
			// Suwayomi unreachable — skip this chapter, re-evaluate next tick.
			continue
		}

		// Branch 1: already downloaded — reconcile will have caught it next tick.
		if meta.IsDownloaded {
			continue
		}

		// Branch 2: empty chapter — auto-error if enabled (the default).
		if meta.PageCount == 0 && meta.QueueState == "ERROR" && !settings.BulkAutoErrorEmptyChaptersDisabled {
			reason := "empty chapter (source returned 0 pages)"
			if marked, _ := o.store.MarkBulkJobChapterErrored(job.ID, chapter.ChapterID, reason); marked {
				_ = o.store.AddActivity(model.ActivityEntry{
					Time:        time.Now().UTC(),
					SeriesTitle: job.Title,
					Action:      model.ActionBulkChapterErrored,
					Detail:      fmt.Sprintf("chapter %d — %s", chapter.ChapterID, reason),
					Via:         "bulk:" + job.SourceName,
				})
			}
			continue
		}

		// Branch 3: Suwayomi errored AND we have exhausted our retry budget.
		if meta.QueueState == "ERROR" && chapter.Tries >= maxRetries {
			reason := fmt.Sprintf("suwayomi gave up after %d retries", meta.Tries)
			if marked, _ := o.store.MarkBulkJobChapterErrored(job.ID, chapter.ChapterID, reason); marked {
				_ = o.store.AddActivity(model.ActivityEntry{
					Time:        time.Now().UTC(),
					SeriesTitle: job.Title,
					Action:      model.ActionBulkChapterErrored,
					Detail:      fmt.Sprintf("chapter %d — %s", chapter.ChapterID, reason),
					Via:         "bulk:" + job.SourceName,
				})
			}
			continue
		}

		// Branch 4: Suwayomi still thinks it's working on it (or chapter fell
		// out of the queue entirely — QueueState=="") — re-feed.
		if meta.QueueState == "" || meta.QueueState == "Queued" || meta.QueueState == "Running" {
			_ = o.suwayomi.EnqueueChapterDownloads(ctx, []int64{chapter.ChapterID})
			_ = o.store.MarkBulkJobChapterFed(job.ID, chapter.ChapterID)
			continue
		}

		// Otherwise: leave the chapter alone; it will be re-evaluated on the
		// next tick (e.g. QueueState is some other terminal state we don't yet
		// know how to route).
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
