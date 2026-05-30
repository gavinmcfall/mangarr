package store

import (
	"database/sql"
	"fmt"
	"testing"

	_ "modernc.org/sqlite"
)

func freshDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestRunMigrationsEmptyListCreatesVersionsTable(t *testing.T) {
	db := freshDB(t)
	prev := migrations
	migrations = nil
	t.Cleanup(func() { migrations = prev })

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}

	var name string
	err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='schema_versions'`).Scan(&name)
	if err != nil {
		t.Fatalf("schema_versions table not created: %v", err)
	}
}

func TestRunMigrationsAppliesRegisteredMigrationsInOrder(t *testing.T) {
	db := freshDB(t)
	var order []int
	prev := migrations
	migrations = []migration{
		{1, "first", func(tx *sql.Tx) error { order = append(order, 1); return nil }},
		{2, "second", func(tx *sql.Tx) error { order = append(order, 2); return nil }},
		{3, "third", func(tx *sql.Tx) error { order = append(order, 3); return nil }},
	}
	t.Cleanup(func() { migrations = prev })

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("expected migrations in order [1,2,3], got %v", order)
	}
}

func TestRunMigrationsIdempotent(t *testing.T) {
	db := freshDB(t)
	calls := 0
	prev := migrations
	migrations = []migration{
		{1, "once", func(tx *sql.Tx) error { calls++; return nil }},
	}
	t.Cleanup(func() { migrations = prev })

	if err := runMigrations(db); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := runMigrations(db); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected migration to run exactly once across two invocations, ran %d times", calls)
	}
}

func TestRunMigrationsRollsBackOnFailure(t *testing.T) {
	db := freshDB(t)
	prev := migrations
	migrations = []migration{
		{1, "creates-table", func(tx *sql.Tx) error {
			_, err := tx.Exec(`CREATE TABLE will_be_rolled_back (id INTEGER)`)
			return err
		}},
		{2, "fails", func(tx *sql.Tx) error { return fmt.Errorf("boom") }},
	}
	t.Cleanup(func() { migrations = prev })

	if err := runMigrations(db); err == nil {
		t.Fatalf("expected runMigrations to return error for failing migration")
	}
	// Migration 1 applied successfully → its table exists, version 1 recorded.
	var name string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='will_be_rolled_back'`).Scan(&name); err != nil {
		t.Fatalf("expected migration 1's table to exist after migration 2 failure: %v", err)
	}
	// Migration 2 rolled back → version 2 NOT recorded.
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_versions WHERE version=2`).Scan(&count); err != nil {
		t.Fatalf("query schema_versions: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected version 2 NOT to be recorded after rollback, found %d rows", count)
	}
}

func TestMigration1CreatesBindingsAndRulesTables(t *testing.T) {
	db := freshDB(t)
	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	for _, table := range []string{"bindings", "classification_rules"} {
		var name string
		err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		if err != nil {
			t.Fatalf("table %q not created: %v", table, err)
		}
	}
}

func TestMigration1BindingsTableShape(t *testing.T) {
	db := freshDB(t)
	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	rows, err := db.Query(`PRAGMA table_info(bindings)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info(bindings): %v", err)
	}
	defer rows.Close()
	got := make(map[string]string)
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan column info: %v", err)
		}
		got[name] = ctype
	}
	want := map[string]string{
		"id":               "INTEGER",
		"name":             "TEXT",
		"library_root":     "TEXT",
		"kavita_lib_id":    "INTEGER",
		"default_is_adult": "INTEGER",
	}
	for col, ctype := range want {
		if got[col] != ctype {
			t.Errorf("bindings column %s: want type %q, got %q", col, ctype, got[col])
		}
	}
}
