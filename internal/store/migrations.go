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
