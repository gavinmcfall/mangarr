package store

import (
	"database/sql"
	"fmt"
)

// migration5BulkChapterErroredReason adds an errored_reason column to
// bulk_job_chapters so the orchestrator can record WHY it gave up on a
// chapter (stall timeout, empty-chapter signal, max retries exceeded, etc.)
// without requiring the operator to dive into pod logs.
func migration5BulkChapterErroredReason(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE bulk_job_chapters ADD COLUMN errored_reason TEXT NOT NULL DEFAULT ''`)
	if err != nil {
		return fmt.Errorf("migration 5 add errored_reason: %w", err)
	}
	return nil
}

// migrateBulkDownloadsTables creates the three tables Plan A of the
// bulk-downloader spec depends on:
//
//   - bulk_jobs            — one row per series the operator kicked off
//   - bulk_job_chapters    — one row per chapter in flight (FK → bulk_jobs)
//   - library_cache        — lazy-loaded chapter counts per manga, keyed
//     on Suwayomi's mangaId
//
// Idempotent under the schema_versions gate; the CREATE TABLE IF NOT
// EXISTS statements are belt-and-braces against an operator who manually
// cleared schema_versions to replay history.
func migrateBulkDownloadsTables(tx *sql.Tx) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS bulk_jobs (
			id                    INTEGER PRIMARY KEY AUTOINCREMENT,
			manga_id              INTEGER NOT NULL,
			source_id             TEXT    NOT NULL,
			title                 TEXT    NOT NULL,
			source_name           TEXT    NOT NULL,
			status                TEXT    NOT NULL,
			total_chapters        INTEGER NOT NULL DEFAULT 0,
			completed_chapters    INTEGER NOT NULL DEFAULT 0,
			errored_chapters      INTEGER NOT NULL DEFAULT 0,
			last_error            TEXT,
			backoff_until         INTEGER,
			consecutive_failures  INTEGER NOT NULL DEFAULT 0,
			created_at            INTEGER NOT NULL DEFAULT (strftime('%s','now')),
			updated_at            INTEGER NOT NULL DEFAULT (strftime('%s','now'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_bulk_jobs_status_source
			ON bulk_jobs(status, source_id, created_at)`,
		`CREATE TABLE IF NOT EXISTS bulk_job_chapters (
			job_id      INTEGER NOT NULL REFERENCES bulk_jobs(id) ON DELETE CASCADE,
			chapter_id  INTEGER NOT NULL,
			state       TEXT    NOT NULL,
			updated_at  INTEGER NOT NULL DEFAULT (strftime('%s','now')),
			PRIMARY KEY (job_id, chapter_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_bulk_job_chapters_state
			ON bulk_job_chapters(state, job_id)`,
		`CREATE TABLE IF NOT EXISTS library_cache (
			manga_id          INTEGER PRIMARY KEY,
			title             TEXT    NOT NULL,
			source_id         TEXT    NOT NULL,
			source_name       TEXT    NOT NULL,
			total_chapters    INTEGER NOT NULL,
			downloaded        INTEGER NOT NULL,
			refreshed_at      INTEGER NOT NULL DEFAULT (strftime('%s','now'))
		)`,
	}
	for _, s := range statements {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("migration 4: %w", err)
		}
	}
	return nil
}
