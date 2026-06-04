package store

import (
	"database/sql"
	"fmt"
)

// migrateSeriesTags creates the series_tags many-to-many table backing the
// free-form per-series tags feature (Sonarr port #10). Tolerant of a missing
// series table for migration-only test fixtures, matching the other series
// migrations.
func migrateSeriesTags(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS series_tags (
		series_id INTEGER NOT NULL,
		tag       TEXT    NOT NULL,
		PRIMARY KEY (series_id, tag)
	)`); err != nil {
		return fmt.Errorf("create series_tags: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_series_tags_tag ON series_tags(tag)`); err != nil {
		return fmt.Errorf("create idx_series_tags_tag: %w", err)
	}
	return nil
}
