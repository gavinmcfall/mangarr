package store

import (
	"database/sql"
	"fmt"
	"log"
)

// migration is one numbered, idempotent step against the SQLite database.
// Migrations are run in ascending version order; each runs at most once,
// tracked via the schema_versions table.
type migration struct {
	version int
	name    string
	apply   func(tx *sql.Tx) error
}

// migrations is the authoritative ordered list. Append new migrations to
// the end; never renumber. Tests may stub this list via the package-private
// variable.
var migrations = []migration{
	{1, "init-bindings-and-rules", migrateInitBindingsAndRules},
	{2, "v1-settings-into-bindings", migrateV1SettingsIntoBindings},
	{3, "series-manual-binding", migrateSeriesManualBinding},
	{4, "bulk-downloads-tables", migrateBulkDownloadsTables},
	{5, "bulk-chapter-errored-reason", migration5BulkChapterErroredReason},
	{6, "bulk-chapter-tries", migration6BulkChapterTries},
	{7, "series-current-binding", migrateSeriesCurrentBinding},
	{8, "series-tags", migrateSeriesTags},
	{9, "series-missing-since", migrateSeriesMissingSince},
	{10, "honest-counts-columns", migrateHonestCountsColumns},
}

// migrateSeriesManualBinding adds the manual_binding_id column to the
// series table so the Series-page reclassify control can persist a
// user-chosen override that the classifier reads at step 0 of its
// six-step flow.
//
// Idempotent under SQLite via the schema_versions gate; the inline
// duplicate-column-name check is belt-and-braces against an operator
// who manually cleared schema_versions to replay history.
//
// Tolerant of a missing series table: production's Store.Open() calls
// the legacy migrate() (CREATE TABLE series) before runMigrations(),
// so the table is always present in prod. Migration-only test fixtures
// open a fresh DB and call runMigrations directly without the legacy
// schema, so the table genuinely isn't there — treat that as a no-op
// rather than failing every unrelated migration test.
func migrateSeriesManualBinding(tx *sql.Tx) error {
	var seriesTable string
	err := tx.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='series'`).Scan(&seriesTable)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("probe series table: %w", err)
	}
	// Series table exists. Detect the column before ALTER — SQLite's
	// ADD COLUMN returns a hard error on duplicate, not a benign signal.
	var name string
	err = tx.QueryRow(`SELECT name FROM pragma_table_info('series') WHERE name = 'manual_binding_id'`).Scan(&name)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("probe series.manual_binding_id: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE series ADD COLUMN manual_binding_id INTEGER`); err != nil {
		return fmt.Errorf("add series.manual_binding_id: %w", err)
	}
	return nil
}

// migrateSeriesCurrentBinding adds the current_binding_id column to series
// so the poller can record which binding the classifier most recently
// resolved a series to. /series renders this column as the visible pill
// when no manual_binding_id is set, so operators can see the classifier's
// decision without bouncing to the Activity page.
//
// Same idempotency-and-fixture-tolerance shape as migrateSeriesManualBinding.
func migrateSeriesCurrentBinding(tx *sql.Tx) error {
	var seriesTable string
	err := tx.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='series'`).Scan(&seriesTable)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("probe series table: %w", err)
	}
	var name string
	err = tx.QueryRow(`SELECT name FROM pragma_table_info('series') WHERE name = 'current_binding_id'`).Scan(&name)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("probe series.current_binding_id: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE series ADD COLUMN current_binding_id INTEGER`); err != nil {
		return fmt.Errorf("add series.current_binding_id: %w", err)
	}
	return nil
}

// runMigrations creates the schema_versions table if missing, then applies
// every migration whose version is not yet recorded, in ascending order.
// Each migration runs in its own transaction; the version row is inserted
// in the same transaction so partial application is impossible.
func runMigrations(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_versions (
		version INTEGER PRIMARY KEY,
		name    TEXT    NOT NULL,
		applied_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now'))
	)`); err != nil {
		return fmt.Errorf("create schema_versions: %w", err)
	}

	applied, err := loadAppliedVersions(db)
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin tx for migration %d %q: %w", m.version, m.name, err)
		}
		if err := m.apply(tx); err != nil {
			if rbErr := tx.Rollback(); rbErr != nil {
				log.Printf("store: rollback migration %d %q: %v", m.version, m.name, rbErr)
			}
			return fmt.Errorf("apply migration %d %q: %w", m.version, m.name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_versions (version, name) VALUES (?, ?)`, m.version, m.name); err != nil {
			if rbErr := tx.Rollback(); rbErr != nil {
				log.Printf("store: rollback migration %d %q: %v", m.version, m.name, rbErr)
			}
			return fmt.Errorf("record migration %d %q: %w", m.version, m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d %q: %w", m.version, m.name, err)
		}
		log.Printf("store: applied migration %d %q", m.version, m.name)
	}
	return nil
}

// migrateSeriesMissingSince adds series.missing_since (unix seconds, NULL =
// present on disk). The reconcile pass sets it when a series' source folder
// first goes absent and uses it as the grace timer before flagging the
// series 'orphaned'. Idempotent under the schema_versions gate; tolerant of
// a missing series table (migration-only test fixtures).
func migrateSeriesMissingSince(tx *sql.Tx) error {
	var seriesTable string
	err := tx.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='series'`).Scan(&seriesTable)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("probe series table: %w", err)
	}
	var name string
	err = tx.QueryRow(`SELECT name FROM pragma_table_info('series') WHERE name = 'missing_since'`).Scan(&name)
	if err == nil {
		return nil // already present
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("probe missing_since column: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE series ADD COLUMN missing_since INTEGER`); err != nil {
		return fmt.Errorf("add series.missing_since: %w", err)
	}
	return nil
}

// migrateHonestCountsColumns adds the columns the Library "honest counts"
// feature needs: library_cache.dud_count and .filed_count (computed at Sync),
// and series.manga_id (the durable Suwayomi-manga join key, populated by the
// poller). Idempotent under the schema_versions gate; tolerant of a missing
// table (migration-only test fixtures) and a duplicate column (manual replay).
func migrateHonestCountsColumns(tx *sql.Tx) error {
	add := func(table, col, decl string) error {
		var t string
		err := tx.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&t)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return fmt.Errorf("probe %s: %w", table, err)
		}
		var c string
		err = tx.QueryRow(`SELECT name FROM pragma_table_info('`+table+`') WHERE name=?`, col).Scan(&c)
		if err == nil {
			return nil
		}
		if err != sql.ErrNoRows {
			return fmt.Errorf("probe %s.%s: %w", table, col, err)
		}
		if _, err := tx.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + col + ` ` + decl); err != nil {
			return fmt.Errorf("add %s.%s: %w", table, col, err)
		}
		return nil
	}
	if err := add("library_cache", "dud_count", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := add("library_cache", "filed_count", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	return add("series", "manga_id", "INTEGER")
}

func loadAppliedVersions(db *sql.DB) (map[int]bool, error) {
	rows, err := db.Query(`SELECT version FROM schema_versions`)
	if err != nil {
		return nil, fmt.Errorf("load applied versions: %w", err)
	}
	defer rows.Close()
	applied := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan applied version: %w", err)
		}
		applied[v] = true
	}
	return applied, rows.Err()
}
