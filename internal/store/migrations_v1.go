package store

import (
	"database/sql"
	"fmt"
)

// migrateInitBindingsAndRules creates the two new tables that hold
// user-defined bindings and classification rules. CREATE TABLE IF NOT
// EXISTS is belt-and-braces idempotency — the schema_versions gate in
// runMigrations is the primary protection.
func migrateInitBindingsAndRules(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS bindings (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			name             TEXT    NOT NULL,
			library_root     TEXT    NOT NULL,
			kavita_lib_id    INTEGER NOT NULL,
			default_is_adult INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS classification_rules (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			priority       INTEGER NOT NULL,
			name           TEXT    NOT NULL,
			condition_json TEXT    NOT NULL,
			binding_id     INTEGER NOT NULL REFERENCES bindings(id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_classification_rules_priority ON classification_rules(priority)`,
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("migrateInitBindingsAndRules: %w", err)
		}
	}
	return nil
}
