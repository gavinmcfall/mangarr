package store

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/gavinmcfall/mangarr/internal/model"
)

// SaveBulkJob inserts a new BulkJob row and returns the assigned ID.
// Counters (CompletedChapters/ErroredChapters), ConsecutiveFailures,
// LastError, and BackoffUntil are taken from the input — callers that
// want defaults should pass the zero value of model.BulkJob and only
// populate the fields they care about.
func (s *Store) SaveBulkJob(in model.BulkJob) (int64, error) {
	var backoff sql.NullInt64
	if in.BackoffUntil != nil {
		backoff = sql.NullInt64{Int64: in.BackoffUntil.Unix(), Valid: true}
	}
	res, err := s.db.Exec(`
INSERT INTO bulk_jobs (
	manga_id, source_id, title, source_name, status,
	total_chapters, completed_chapters, errored_chapters,
	last_error, backoff_until, consecutive_failures
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.MangaID, in.SourceID, in.Title, in.SourceName, string(in.Status),
		in.TotalChapters, in.CompletedChapters, in.ErroredChapters,
		in.LastError, backoff, in.ConsecutiveFailures,
	)
	if err != nil {
		return 0, fmt.Errorf("SaveBulkJob: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("SaveBulkJob LastInsertId: %w", err)
	}
	return id, nil
}

// GetBulkJob returns the BulkJob with the given ID, or sql.ErrNoRows
// (wrapped) when no such job exists.
func (s *Store) GetBulkJob(id int64) (model.BulkJob, error) {
	row := s.db.QueryRow(`SELECT
		id, manga_id, source_id, title, source_name, status,
		total_chapters, completed_chapters, errored_chapters,
		last_error, backoff_until, consecutive_failures,
		created_at, updated_at
	FROM bulk_jobs WHERE id = ?`, id)
	return scanBulkJob(row)
}

// ListBulkJobs returns all bulk_jobs rows matching the given status, or
// all rows when status is the empty string. Ordered ascending by
// created_at so the orchestrator's FIFO-per-source pick is deterministic.
func (s *Store) ListBulkJobs(status model.BulkJobStatus) ([]model.BulkJob, error) {
	var rows *sql.Rows
	var err error
	q := `SELECT
		id, manga_id, source_id, title, source_name, status,
		total_chapters, completed_chapters, errored_chapters,
		last_error, backoff_until, consecutive_failures,
		created_at, updated_at
	FROM bulk_jobs`
	if status == "" {
		// created_at is whole-second resolution in SQLite (strftime('%s','now')),
		// so two jobs created in the same second tie. Adding `id ASC` makes
		// FIFO ordering deterministic at the data layer (the orchestrator's
		// sort.Slice is not stable).
		rows, err = s.db.Query(q + ` ORDER BY created_at ASC, id ASC`)
	} else {
		rows, err = s.db.Query(q+` WHERE status = ? ORDER BY created_at ASC, id ASC`, string(status))
	}
	if err != nil {
		return nil, fmt.Errorf("ListBulkJobs: %w", err)
	}
	defer rows.Close()
	var out []model.BulkJob
	for rows.Next() {
		j, err := scanBulkJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// UpdateBulkJobStatus flips the job's status and bumps updated_at.
// Used by pause/resume/delete UI actions and the orchestrator's
// terminal-state transitions.
func (s *Store) UpdateBulkJobStatus(id int64, status model.BulkJobStatus) error {
	_, err := s.db.Exec(
		`UPDATE bulk_jobs SET status = ?, updated_at = strftime('%s','now') WHERE id = ?`,
		string(status), id,
	)
	if err != nil {
		return fmt.Errorf("UpdateBulkJobStatus: %w", err)
	}
	return nil
}

// scanBulkJob unifies the row-scan logic for the QueryRow and Query
// callers. Accepts the sqlScanner interface so both *sql.Row and
// *sql.Rows can drive it.
type sqlScanner interface {
	Scan(dest ...interface{}) error
}

// BatchInsertBulkJobChapters inserts every chapter ID under the given
// job at state='pending'. Idempotent on the PK collision case (re-insert
// is a no-op via INSERT OR IGNORE) so a retry after a partial failure
// doesn't error out. Caller should pre-deduplicate to avoid the silent
// drop semantics if that's load-bearing.
func (s *Store) BatchInsertBulkJobChapters(jobID int64, chapterIDs []int64) error {
	if len(chapterIDs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("BatchInsertBulkJobChapters begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if rbErr := tx.Rollback(); rbErr != nil {
				log.Printf("store: rollback BatchInsertBulkJobChapters: %v", rbErr)
			}
		}
	}()
	stmt, err := tx.Prepare(
		`INSERT OR IGNORE INTO bulk_job_chapters (job_id, chapter_id, state) VALUES (?, ?, ?)`,
	)
	if err != nil {
		return fmt.Errorf("BatchInsertBulkJobChapters prepare: %w", err)
	}
	defer stmt.Close()
	for _, cid := range chapterIDs {
		if _, err := stmt.Exec(jobID, cid, string(model.BulkChapterPending)); err != nil {
			return fmt.Errorf("BatchInsertBulkJobChapters insert chapter %d: %w", cid, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("BatchInsertBulkJobChapters commit: %w", err)
	}
	committed = true
	return nil
}

// ListBulkJobChapters returns chapters for a job filtered by state.
// State="" returns all chapters for the job regardless of state. Ordered
// ascending by chapter_id so the orchestrator's feed picks chapters in a
// deterministic order across ticks.
func (s *Store) ListBulkJobChapters(jobID int64, state model.BulkChapterState) ([]model.BulkJobChapter, error) {
	var rows *sql.Rows
	var err error
	q := `SELECT job_id, chapter_id, state, errored_reason, tries, updated_at FROM bulk_job_chapters WHERE job_id = ?`
	if state == "" {
		rows, err = s.db.Query(q+` ORDER BY chapter_id ASC`, jobID)
	} else {
		rows, err = s.db.Query(q+` AND state = ? ORDER BY chapter_id ASC`, jobID, string(state))
	}
	if err != nil {
		return nil, fmt.Errorf("ListBulkJobChapters: %w", err)
	}
	defer rows.Close()
	var out []model.BulkJobChapter
	for rows.Next() {
		var c model.BulkJobChapter
		var stateStr string
		var updatedAt int64
		if err := rows.Scan(&c.JobID, &c.ChapterID, &stateStr, &c.ErroredReason, &c.Tries, &updatedAt); err != nil {
			return nil, fmt.Errorf("ListBulkJobChapters scan: %w", err)
		}
		c.State = model.BulkChapterState(stateStr)
		c.UpdatedAt = time.Unix(updatedAt, 0).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetBulkJobChapter returns the single chapter row for (jobID, chapterID),
// or sql.ErrNoRows (wrapped) when no such row exists.
func (s *Store) GetBulkJobChapter(jobID, chapterID int64) (model.BulkJobChapter, error) {
	var c model.BulkJobChapter
	var stateStr string
	var updatedAt int64
	err := s.db.QueryRow(
		`SELECT job_id, chapter_id, state, errored_reason, tries, updated_at
		   FROM bulk_job_chapters WHERE job_id = ? AND chapter_id = ?`,
		jobID, chapterID,
	).Scan(&c.JobID, &c.ChapterID, &stateStr, &c.ErroredReason, &c.Tries, &updatedAt)
	if err != nil {
		return c, fmt.Errorf("GetBulkJobChapter: %w", err)
	}
	c.State = model.BulkChapterState(stateStr)
	c.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return c, nil
}

// ListStalledFedChapters returns all bulk_job_chapters for the given job that
// are in state='fed' and whose updated_at is older than olderThan (passed as
// Unix seconds for comparison against the integer column). Ordered by
// updated_at ASC (oldest first) so the stall detector surfaces the
// most-likely-stuck chapters first.
func (s *Store) ListStalledFedChapters(jobID int64, olderThan time.Time) ([]model.BulkJobChapter, error) {
	rows, err := s.db.Query(
		`SELECT job_id, chapter_id, state, errored_reason, tries, updated_at
		   FROM bulk_job_chapters
		  WHERE job_id = ? AND state = 'fed' AND updated_at < ?
		  ORDER BY updated_at ASC`,
		jobID, olderThan.Unix(),
	)
	if err != nil {
		return nil, fmt.Errorf("ListStalledFedChapters: %w", err)
	}
	defer rows.Close()
	var out []model.BulkJobChapter
	for rows.Next() {
		var c model.BulkJobChapter
		var stateStr string
		var updatedAt int64
		if err := rows.Scan(&c.JobID, &c.ChapterID, &stateStr, &c.ErroredReason, &c.Tries, &updatedAt); err != nil {
			return nil, fmt.Errorf("ListStalledFedChapters scan: %w", err)
		}
		c.State = model.BulkChapterState(stateStr)
		c.UpdatedAt = time.Unix(updatedAt, 0).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkBulkJobChapterFed marks a chapter as fed to Suwayomi and bumps the
// chapter's mangarr-side tries counter. The tries counter is independent
// of Suwayomi's own tries field, which resets on Suwayomi restart.
// detectStalledChapters reads tries to decide whether to re-feed or
// escalate to errored.
func (s *Store) MarkBulkJobChapterFed(jobID, chapterID int64) error {
	_, err := s.db.Exec(
		`UPDATE bulk_job_chapters
		    SET state = 'fed', tries = tries + 1, updated_at = strftime('%s','now')
		  WHERE job_id = ? AND chapter_id = ?`,
		jobID, chapterID,
	)
	if err != nil {
		return fmt.Errorf("MarkBulkJobChapterFed: %w", err)
	}
	return nil
}

// MarkBulkJobChapterErrored atomically marks a chapter as errored and
// increments the parent job's errored_chapters counter. The update is
// conditional on the chapter currently being in 'fed' or 'pending' state —
// if the chapter is already 'done' or 'errored', this is a no-op (idempotent:
// a redundant detect-tick must not double-bump errored_chapters).
func (s *Store) MarkBulkJobChapterErrored(jobID, chapterID int64, reason string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("MarkBulkJobChapterErrored begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if rbErr := tx.Rollback(); rbErr != nil {
				log.Printf("store: rollback MarkBulkJobChapterErrored: %v", rbErr)
			}
		}
	}()

	res, err := tx.Exec(
		`UPDATE bulk_job_chapters
		    SET state = 'errored', errored_reason = ?, updated_at = strftime('%s','now')
		  WHERE job_id = ? AND chapter_id = ? AND state IN ('fed', 'pending')`,
		reason, jobID, chapterID,
	)
	if err != nil {
		return fmt.Errorf("MarkBulkJobChapterErrored update chapter: %w", err)
	}

	// Only bump the job counters if the chapter row was actually updated.
	// This is the idempotency gate: a second call when the chapter is already
	// 'errored' (or 'done') leaves RowsAffected==0 and skips the bump.
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("MarkBulkJobChapterErrored rows affected: %w", err)
	}
	if affected > 0 {
		if _, err := tx.Exec(
			`UPDATE bulk_jobs
			    SET errored_chapters = errored_chapters + 1, last_error = ?,
			        updated_at = strftime('%s','now')
			  WHERE id = ?`,
			reason, jobID,
		); err != nil {
			return fmt.Errorf("MarkBulkJobChapterErrored update job: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("MarkBulkJobChapterErrored commit: %w", err)
	}
	committed = true
	return nil
}

// UpdateBulkJobChapterState flips one chapter's state and bumps
// updated_at. Used by the orchestrator on every reconcile + feed.
func (s *Store) UpdateBulkJobChapterState(jobID, chapterID int64, state model.BulkChapterState) error {
	_, err := s.db.Exec(
		`UPDATE bulk_job_chapters SET state = ?, updated_at = strftime('%s','now') WHERE job_id = ? AND chapter_id = ?`,
		string(state), jobID, chapterID,
	)
	if err != nil {
		return fmt.Errorf("UpdateBulkJobChapterState: %w", err)
	}
	return nil
}

// UpdateBulkJobBackoff sets backoff_until + consecutive_failures + last_error
// in one statement. Used by the orchestrator on each ladder rung.
func (s *Store) UpdateBulkJobBackoff(jobID int64, until time.Time, consecFailures int, lastError string) error {
	_, err := s.db.Exec(`UPDATE bulk_jobs SET
		backoff_until = ?, consecutive_failures = ?, last_error = ?,
		updated_at = strftime('%s','now')
	WHERE id = ?`, until.Unix(), consecFailures, lastError, jobID)
	if err != nil {
		return fmt.Errorf("UpdateBulkJobBackoff: %w", err)
	}
	return nil
}

// IncrementBulkJobCompletedChapters atomically bumps the job's
// completed_chapters counter. Called by the orchestrator each time it
// flips a chapter from 'fed' to 'done' during reconcile, so the
// JSON API's BulkJob.CompletedChapters reflects real progress rather
// than the stale value SaveBulkJob wrote at job creation (usually 0).
func (s *Store) IncrementBulkJobCompletedChapters(jobID int64) error {
	_, err := s.db.Exec(`UPDATE bulk_jobs SET
		completed_chapters = completed_chapters + 1,
		updated_at = strftime('%s','now')
	WHERE id = ?`, jobID)
	if err != nil {
		return fmt.Errorf("IncrementBulkJobCompletedChapters: %w", err)
	}
	return nil
}

// ClearBulkJobBackoff resets backoff_until + consecutive_failures + last_error
// on a successful feed.
func (s *Store) ClearBulkJobBackoff(jobID int64) error {
	_, err := s.db.Exec(`UPDATE bulk_jobs SET
		backoff_until = NULL, consecutive_failures = 0, last_error = '',
		updated_at = strftime('%s','now')
	WHERE id = ?`, jobID)
	if err != nil {
		return fmt.Errorf("ClearBulkJobBackoff: %w", err)
	}
	return nil
}

// DeleteBulkJob removes a job. Chapter rows cascade via the FK; the
// store's Open() enables `PRAGMA foreign_keys = ON` so the ON DELETE
// CASCADE clause from Migration 4 actually fires.
func (s *Store) DeleteBulkJob(id int64) error {
	_, err := s.db.Exec(`DELETE FROM bulk_jobs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("DeleteBulkJob: %w", err)
	}
	return nil
}

// SaveLibraryCacheEntry inserts-or-updates by manga_id and bumps
// refreshed_at. Used by /api/library/sync to repopulate the cache after
// a Suwayomi roundtrip.
func (s *Store) SaveLibraryCacheEntry(in model.LibraryCacheEntry) error {
	_, err := s.db.Exec(`
INSERT INTO library_cache (manga_id, title, source_id, source_name, total_chapters, downloaded)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(manga_id) DO UPDATE SET
	title          = excluded.title,
	source_id      = excluded.source_id,
	source_name    = excluded.source_name,
	total_chapters = excluded.total_chapters,
	downloaded     = excluded.downloaded,
	refreshed_at   = strftime('%s','now')`,
		in.MangaID, in.Title, in.SourceID, in.SourceName, in.TotalChapters, in.Downloaded,
	)
	if err != nil {
		return fmt.Errorf("SaveLibraryCacheEntry: %w", err)
	}
	return nil
}

// GetLibraryCacheEntry returns one entry by manga_id, or sql.ErrNoRows
// (wrapped) when no row exists yet.
func (s *Store) GetLibraryCacheEntry(mangaID int64) (model.LibraryCacheEntry, error) {
	var e model.LibraryCacheEntry
	var refreshedAt int64
	err := s.db.QueryRow(`SELECT manga_id, title, source_id, source_name, total_chapters, downloaded, refreshed_at
		FROM library_cache WHERE manga_id = ?`, mangaID,
	).Scan(&e.MangaID, &e.Title, &e.SourceID, &e.SourceName, &e.TotalChapters, &e.Downloaded, &refreshedAt)
	if err != nil {
		return e, fmt.Errorf("GetLibraryCacheEntry: %w", err)
	}
	e.RefreshedAt = time.Unix(refreshedAt, 0).UTC()
	return e, nil
}

// ListLibraryCacheEntries returns all rows, ordered by title COLLATE
// NOCASE for stable, case-insensitive UI rendering on the Library page.
func (s *Store) ListLibraryCacheEntries() ([]model.LibraryCacheEntry, error) {
	rows, err := s.db.Query(`SELECT manga_id, title, source_id, source_name, total_chapters, downloaded, refreshed_at
		FROM library_cache ORDER BY title COLLATE NOCASE ASC`)
	if err != nil {
		return nil, fmt.Errorf("ListLibraryCacheEntries: %w", err)
	}
	defer rows.Close()
	var out []model.LibraryCacheEntry
	for rows.Next() {
		var e model.LibraryCacheEntry
		var refreshedAt int64
		if err := rows.Scan(&e.MangaID, &e.Title, &e.SourceID, &e.SourceName, &e.TotalChapters, &e.Downloaded, &refreshedAt); err != nil {
			return nil, fmt.Errorf("ListLibraryCacheEntries scan: %w", err)
		}
		e.RefreshedAt = time.Unix(refreshedAt, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanBulkJob(sc sqlScanner) (model.BulkJob, error) {
	var j model.BulkJob
	var statusStr string
	var lastErr sql.NullString
	var backoff sql.NullInt64
	var createdAt, updatedAt int64
	if err := sc.Scan(
		&j.ID, &j.MangaID, &j.SourceID, &j.Title, &j.SourceName, &statusStr,
		&j.TotalChapters, &j.CompletedChapters, &j.ErroredChapters,
		&lastErr, &backoff, &j.ConsecutiveFailures,
		&createdAt, &updatedAt,
	); err != nil {
		return j, err
	}
	j.Status = model.BulkJobStatus(statusStr)
	if lastErr.Valid {
		j.LastError = lastErr.String
	}
	if backoff.Valid {
		t := time.Unix(backoff.Int64, 0).UTC()
		j.BackoffUntil = &t
	}
	j.CreatedAt = time.Unix(createdAt, 0).UTC()
	j.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return j, nil
}
