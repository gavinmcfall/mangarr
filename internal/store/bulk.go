package store

import (
	"database/sql"
	"fmt"
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
		rows, err = s.db.Query(q + ` ORDER BY created_at ASC`)
	} else {
		rows, err = s.db.Query(q+` WHERE status = ? ORDER BY created_at ASC`, string(status))
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
