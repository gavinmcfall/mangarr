# Library Bindings v2 — Plan A Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace mangarr's closed 3-type ContentType enum with user-defined library bindings + priority-ordered classification rules + a small migrations framework. Implements Plan A from `docs/specs/2026-05-30-library-bindings-v2.md`. After this lands, mangarr can route to N user-defined library destinations via JSON Settings API; UI follows in Plan B.

**Architecture:** Three new types in `internal/model` (`Binding`, `ClassificationRule`, `RuleCondition`). New tables in SQLite (`bindings`, `classification_rules`, `schema_versions`). A small migrations framework (~50 LOC) that runs ordered numbered migrations on boot. The classifier becomes a six-step flow (path-only rules → Suwayomi overrides → AniList → AniList rules → DefaultBinding → Unmatched) returning a `Decision{BindingID, Via}`. AniList client widens its GraphQL query to include `isAdult` + `format`. Poller routes by `Decision.BindingID`, looking up `LibraryRoot` + `KavitaLibID` from the matching `Binding`.

**Tech Stack:** Go 1.25, `modernc.org/sqlite` driver, existing mangarr layout (`internal/{model,store,classifier,anilist,poller,suwayomi}`), existing test conventions (`httptest` for AniList stubs, table-driven tests for classifier branches). No new dependencies.

---

## Pre-flight

Working directory: `/home/gavin/my_other_repos/mangarr`. Branch: `feat/library-bindings-v2-plan-a` (cut from `origin/main` at `f1332ba` — the Plan-C commit of Library Map). Do NOT modify the spec doc at `docs/specs/2026-05-30-library-bindings-v2.md`; reference it freely.

**Test rule from prior reviews:** never use canonical placeholder credentials in tests (`hunter2`, `password123`, `admin/admin`). GitGuardian flags them. Use `"test-placeholder-pw"`-style strings. (See `[[feedback-avoid-meme-placeholder-secrets]]`.)

**Commit messages must NOT contain "claude" or "anthropic"** — Gavin's commit-msg hook blocks them. Drop the auto-inserted `Co-Authored-By` trailer. Re-write the message clean rather than `--no-verify`-ing.

**verify-gate**: after staging, `touch $CLAUDE_PROJECT_DIR/.claude/.verified` as its own Bash call BEFORE the commit. Edit/Write wipes the stamp.

```bash
git fetch origin main
git checkout -b feat/library-bindings-v2-plan-a origin/main
```

---

### Task 1: Migrations framework — schema_versions table + ordered runner

Lay the foundation. The framework only schedules migrations; no migrations exist yet.

**Files:**
- Create: `internal/store/migrations.go`
- Create: `internal/store/migrations_test.go`

- [ ] **Step 1: Write the failing test for empty migration list**

`internal/store/migrations_test.go`:
```go
package store

import (
	"database/sql"
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
```

- [ ] **Step 2: Run test, verify it fails**

```bash
go test ./internal/store/ -run TestRunMigrationsEmptyListCreatesVersionsTable -v
```

Expected: FAIL with `undefined: runMigrations` or similar.

- [ ] **Step 3: Implement the framework**

`internal/store/migrations.go`:
```go
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
var migrations = []migration{}

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
			tx.Rollback()
			return fmt.Errorf("apply migration %d %q: %w", m.version, m.name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_versions (version, name) VALUES (?, ?)`, m.version, m.name); err != nil {
			tx.Rollback()
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
```

- [ ] **Step 4: Run test, verify it passes**

```bash
go test ./internal/store/ -run TestRunMigrationsEmptyListCreatesVersionsTable -v
```

Expected: PASS.

- [ ] **Step 5: Add idempotency + ordering tests**

Append to `migrations_test.go`:
```go
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
```

Add the import `"fmt"` near the top of the test file if not already present.

- [ ] **Step 6: Run all migrations-framework tests**

```bash
go test ./internal/store/ -run TestRunMigrations -v -race
```

Expected: 4 tests PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/store/migrations.go internal/store/migrations_test.go
touch .claude/.verified
git -c commit.gpgsign=false commit -m "feat(store): migrations framework (schema_versions + ordered runner)

First piece of the Library Bindings v2 spec. Adds the minimal migrations
framework predicted by the Plan B reviewer of Library Map (PR #31):
schema_versions table tracks applied migrations, runMigrations walks
the ordered registry, each migration runs in its own transaction with
the version row inserted atomically so partial application is
impossible.

No migrations registered yet — that's Task 2."
```

---

### Task 2: Migration 1 — create `bindings` and `classification_rules` tables

Register the first migration and verify the schema lands.

**Files:**
- Create: `internal/store/migrations_v1.go`
- Modify: `internal/store/migrations.go` (register migration 1 in the slice)
- Modify: `internal/store/migrations_test.go` (add real migration 1 tests)

- [ ] **Step 1: Write the failing test for migration 1**

Append to `internal/store/migrations_test.go`:
```go
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
		"default_is_adult": "INTEGER", // SQLite stores bool as integer 0/1
	}
	for col, ctype := range want {
		if got[col] != ctype {
			t.Errorf("bindings column %s: want type %q, got %q", col, ctype, got[col])
		}
	}
}
```

- [ ] **Step 2: Run tests, verify they fail**

```bash
go test ./internal/store/ -run 'TestMigration1' -v
```

Expected: FAIL with "table bindings not created" or similar.

- [ ] **Step 3: Implement migration 1**

Create `internal/store/migrations_v1.go`:
```go
package store

import (
	"database/sql"
	"fmt"
)

// migrateInitBindingsAndRules (v1 of the schema, NOT to be confused with
// "mangarr v1" the Library Map era) creates the two new tables that hold
// user-defined bindings and classification rules. Idempotent because
// CREATE TABLE IF NOT EXISTS swallows duplicates — the schema_versions
// gate above is the primary idempotency mechanism, this is belt-and-braces.
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
```

Modify `internal/store/migrations.go` to register it:
```go
var migrations = []migration{
	{1, "init-bindings-and-rules", migrateInitBindingsAndRules},
}
```

- [ ] **Step 4: Run tests, verify they pass**

```bash
go test ./internal/store/ -run 'TestMigration1' -v -race
go test ./internal/store/ -run TestRunMigrations -v -race
```

Expected: all PASS. The previous framework tests that stub out `migrations` should also still pass because they overwrite the slice.

- [ ] **Step 5: Commit**

```bash
git add internal/store/migrations.go internal/store/migrations_v1.go internal/store/migrations_test.go
touch .claude/.verified
git -c commit.gpgsign=false commit -m "feat(store): migration 1 creates bindings + classification_rules tables

Tables match the design spec at docs/specs/2026-05-30-library-bindings-v2.md:
- bindings(id, name, library_root, kavita_lib_id, default_is_adult)
- classification_rules(id, priority, name, condition_json, binding_id FK)
- index on classification_rules.priority for ascending walks

Migration 2 (v1 -> v2 settings conversion) lands in a later task."
```

---

### Task 3: Model types — `Binding`, `ClassificationRule`, `RuleCondition`, `Decision`

Add the Go types that match the SQL schema, with `*pointer` fields on `RuleCondition` so unset is distinguishable from explicit empty.

**Files:**
- Modify: `internal/model/model.go` (add types alongside existing `ContentType`, `Settings`, etc.)
- Create: `internal/model/binding_test.go`

- [ ] **Step 1: Write failing tests for the new types**

Create `internal/model/binding_test.go`:
```go
package model

import (
	"encoding/json"
	"testing"
)

func TestBindingZeroValueIsUsable(t *testing.T) {
	b := Binding{}
	if b.ID != 0 || b.Name != "" || b.LibraryRoot != "" || b.KavitaLibID != 0 || b.DefaultIsAdult != false {
		t.Errorf("expected zero-value Binding to have all-zero fields, got %+v", b)
	}
}

func TestRuleConditionAllNilMeansWildcard(t *testing.T) {
	c := RuleCondition{}
	if c.CountryOfOrigin != nil || c.IsAdult != nil || c.Format != nil || c.SourcePathPrefix != nil {
		t.Errorf("expected zero-value RuleCondition to have all-nil pointer fields, got %+v", c)
	}
}

func TestRuleConditionJSONRoundTripPreservesNilVsExplicitZero(t *testing.T) {
	jpStr := "JP"
	falseVal := false
	original := RuleCondition{
		CountryOfOrigin: &jpStr,
		IsAdult:         &falseVal, // explicit false (not nil) must round-trip
		Format:          nil,       // nil must stay nil
	}

	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded RuleCondition
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.CountryOfOrigin == nil || *decoded.CountryOfOrigin != "JP" {
		t.Errorf("CountryOfOrigin round-trip lost: want JP, got %v", decoded.CountryOfOrigin)
	}
	if decoded.IsAdult == nil {
		t.Errorf("IsAdult round-trip lost: explicit false became nil")
	} else if *decoded.IsAdult != false {
		t.Errorf("IsAdult round-trip wrong value: want false, got %v", *decoded.IsAdult)
	}
	if decoded.Format != nil {
		t.Errorf("Format round-trip wrong: nil became %v", *decoded.Format)
	}
}

func TestClassificationRulePriorityIsExportedInt(t *testing.T) {
	r := ClassificationRule{
		ID:        1,
		Priority:  100,
		Name:      "Japanese",
		BindingID: 1,
	}
	if r.Priority != 100 {
		t.Errorf("Priority round-trip: want 100, got %d", r.Priority)
	}
}

func TestDecisionShape(t *testing.T) {
	d := Decision{BindingID: 42, Via: "rule:7"}
	if d.BindingID != 42 || d.Via != "rule:7" {
		t.Errorf("Decision round-trip wrong: %+v", d)
	}
}
```

- [ ] **Step 2: Run tests, verify they fail**

```bash
go test ./internal/model/ -v
```

Expected: FAIL with `undefined: Binding`, `undefined: RuleCondition`, etc.

- [ ] **Step 3: Implement the types**

Append to `internal/model/model.go`:
```go
// Binding is one library destination the user has defined. Replaces the
// closed-enum routing of v1 (Library Map). Each binding owns a filesystem
// root and a Kavita library ID for the scan trigger.
type Binding struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	LibraryRoot    string `json:"library_root"`
	KavitaLibID    int64  `json:"kavita_lib_id"`
	DefaultIsAdult bool   `json:"default_is_adult"`
}

// ClassificationRule maps a metadata condition to a binding. Rules are
// stored as an ordered list; the classifier walks them ascending by
// Priority and routes the series to the first matching rule's binding.
type ClassificationRule struct {
	ID        int64         `json:"id"`
	Priority  int           `json:"priority"`
	Name      string        `json:"name"`
	Condition RuleCondition `json:"condition"`
	BindingID int64         `json:"binding_id"`
}

// RuleCondition is AND-semantics across set fields. Pointer types let nil
// mean "wildcard" (don't constrain) while explicit zero values (e.g. an
// explicit IsAdult=false) constrain to that value.
type RuleCondition struct {
	CountryOfOrigin  *string `json:"country_of_origin,omitempty"`
	IsAdult          *bool   `json:"is_adult,omitempty"`
	Format           *string `json:"format,omitempty"`
	SourcePathPrefix *string `json:"source_path_prefix,omitempty"`
}

// IsPathOnly reports whether this condition only constrains the source
// path. Path-only rules are evaluated in the classifier's step 1 short-
// circuit, before any AniList call.
func (c RuleCondition) IsPathOnly() bool {
	return c.SourcePathPrefix != nil && c.CountryOfOrigin == nil && c.IsAdult == nil && c.Format == nil
}

// Decision is the classifier's output: which binding to route to, plus
// the Via tag that gets recorded on the activity log entry so users can
// audit how each series was classified.
type Decision struct {
	BindingID int64
	Via       string
}
```

- [ ] **Step 4: Run tests, verify they pass**

```bash
go test ./internal/model/ -v -race
```

Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/model/model.go internal/model/binding_test.go
touch .claude/.verified
git -c commit.gpgsign=false commit -m "feat(model): Binding, ClassificationRule, RuleCondition, Decision types

Pointer fields on RuleCondition so nil distinguishes wildcard from
explicit zero (e.g. IsAdult: &false constrains to non-adult; IsAdult:
nil leaves the adult axis unconstrained). IsPathOnly helper used by
the classifier's step 1 short-circuit later in this plan."
```

---

### Task 4: Store CRUD for bindings

Atomic SaveBindings (replace-all) + ListBindings. Keep it simple — the UI in Plan B will atomically POST the whole list each save, no per-row endpoints.

**Files:**
- Modify: `internal/store/store.go` (add methods)
- Modify: `internal/store/store_test.go` (add tests)

- [ ] **Step 1: Write failing tests**

Append to `internal/store/store_test.go`:
```go
func TestListBindingsEmpty(t *testing.T) {
	s := newTestStore(t)
	got, err := s.ListBindings()
	if err != nil {
		t.Fatalf("ListBindings: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 bindings on fresh store, got %d", len(got))
	}
}

func TestSaveBindingsAtomicReplaceAll(t *testing.T) {
	s := newTestStore(t)
	first := []model.Binding{
		{Name: "Manga", LibraryRoot: "/m/a", KavitaLibID: 1, DefaultIsAdult: false},
		{Name: "Manhwa", LibraryRoot: "/m/b", KavitaLibID: 2, DefaultIsAdult: false},
	}
	if err := s.SaveBindings(first); err != nil {
		t.Fatalf("SaveBindings first: %v", err)
	}
	got, _ := s.ListBindings()
	if len(got) != 2 {
		t.Fatalf("expected 2 bindings after first save, got %d", len(got))
	}

	// Replace with a different set; first set must be gone.
	second := []model.Binding{
		{Name: "Comics", LibraryRoot: "/m/c", KavitaLibID: 3, DefaultIsAdult: false},
	}
	if err := s.SaveBindings(second); err != nil {
		t.Fatalf("SaveBindings second: %v", err)
	}
	got, _ = s.ListBindings()
	if len(got) != 1 || got[0].Name != "Comics" {
		t.Errorf("expected only Comics after replace, got %+v", got)
	}
}

func TestSaveBindingsAssignsIDsToNewRows(t *testing.T) {
	s := newTestStore(t)
	in := []model.Binding{{Name: "Manga", LibraryRoot: "/m/a", KavitaLibID: 1}}
	if err := s.SaveBindings(in); err != nil {
		t.Fatalf("SaveBindings: %v", err)
	}
	out, _ := s.ListBindings()
	if len(out) != 1 || out[0].ID == 0 {
		t.Errorf("expected SaveBindings to assign a non-zero ID, got %+v", out)
	}
}
```

If `newTestStore` doesn't already exist in this test file, add it near the top:
```go
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
```
(Use whatever the existing test helper is — check `internal/store/store_test.go` for the established pattern. The Library Map era added Suwayomi round-trip tests, so a helper almost certainly exists.)

- [ ] **Step 2: Run tests, verify they fail**

```bash
go test ./internal/store/ -run 'TestListBindings|TestSaveBindings' -v
```

Expected: FAIL with `undefined: ListBindings` / `undefined: SaveBindings`.

- [ ] **Step 3: Implement the methods**

Append to `internal/store/store.go`:
```go
// ListBindings returns all bindings ordered by name. Stable order matters
// for UI rendering; ID would also be stable but Name reads better in lists.
func (s *Store) ListBindings() ([]model.Binding, error) {
	rows, err := s.db.Query(`SELECT id, name, library_root, kavita_lib_id, default_is_adult FROM bindings ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list bindings: %w", err)
	}
	defer rows.Close()

	var out []model.Binding
	for rows.Next() {
		var b model.Binding
		var isAdult int64
		if err := rows.Scan(&b.ID, &b.Name, &b.LibraryRoot, &b.KavitaLibID, &isAdult); err != nil {
			return nil, fmt.Errorf("scan binding: %w", err)
		}
		b.DefaultIsAdult = isAdult != 0
		out = append(out, b)
	}
	return out, rows.Err()
}

// SaveBindings atomically replaces the entire bindings table contents.
// Existing rows that aren't in the input are deleted. Any input row with
// ID == 0 is treated as new and assigned an autoincremented ID; rows with
// ID > 0 are upserted by ID.
func (s *Store) SaveBindings(in []model.Binding) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin save bindings: %w", err)
	}
	defer tx.Rollback()

	// Collect input IDs (non-zero) to know which existing rows to keep.
	keep := make(map[int64]bool)
	for _, b := range in {
		if b.ID > 0 {
			keep[b.ID] = true
		}
	}

	// Delete existing rows whose IDs are not in the input. Use a NOT IN
	// query if there's anything to keep, or wipe the table if not.
	if len(keep) == 0 {
		if _, err := tx.Exec(`DELETE FROM bindings`); err != nil {
			return fmt.Errorf("delete all bindings: %w", err)
		}
	} else {
		ids := make([]any, 0, len(keep))
		placeholders := make([]string, 0, len(keep))
		for id := range keep {
			ids = append(ids, id)
			placeholders = append(placeholders, "?")
		}
		q := fmt.Sprintf(`DELETE FROM bindings WHERE id NOT IN (%s)`, strings.Join(placeholders, ","))
		if _, err := tx.Exec(q, ids...); err != nil {
			return fmt.Errorf("prune bindings: %w", err)
		}
	}

	// Upsert each input row. SQLite supports ON CONFLICT(id) DO UPDATE.
	for i := range in {
		b := &in[i]
		isAdult := int64(0)
		if b.DefaultIsAdult {
			isAdult = 1
		}
		if b.ID == 0 {
			res, err := tx.Exec(
				`INSERT INTO bindings (name, library_root, kavita_lib_id, default_is_adult) VALUES (?, ?, ?, ?)`,
				b.Name, b.LibraryRoot, b.KavitaLibID, isAdult,
			)
			if err != nil {
				return fmt.Errorf("insert binding %q: %w", b.Name, err)
			}
			id, _ := res.LastInsertId()
			b.ID = id
		} else {
			_, err := tx.Exec(
				`INSERT INTO bindings (id, name, library_root, kavita_lib_id, default_is_adult)
				 VALUES (?, ?, ?, ?, ?)
				 ON CONFLICT(id) DO UPDATE SET name=excluded.name, library_root=excluded.library_root,
				   kavita_lib_id=excluded.kavita_lib_id, default_is_adult=excluded.default_is_adult`,
				b.ID, b.Name, b.LibraryRoot, b.KavitaLibID, isAdult,
			)
			if err != nil {
				return fmt.Errorf("upsert binding id=%d: %w", b.ID, err)
			}
		}
	}

	return tx.Commit()
}
```

Add `"strings"` to the import block if not present.

- [ ] **Step 4: Run tests, verify they pass**

```bash
go test ./internal/store/ -run 'TestListBindings|TestSaveBindings' -v -race
```

Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/store_test.go
touch .claude/.verified
git -c commit.gpgsign=false commit -m "feat(store): ListBindings + atomic SaveBindings

Atomic replace-all semantics: input list becomes the authoritative
state. Rows with ID > 0 are upserted; rows with ID == 0 are inserted
and assigned a new ID. Existing rows not in the input are deleted.
Single transaction so partial failures don't leave the bindings table
in a torn state."
```

---

### Task 5: Store CRUD for classification rules

Same shape as Task 4 but with the JSON-encoded `Condition` field.

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/store_test.go`

- [ ] **Step 1: Write failing tests**

Append to `internal/store/store_test.go`:
```go
func TestListRulesEmpty(t *testing.T) {
	s := newTestStore(t)
	got, err := s.ListRules()
	if err != nil {
		t.Fatalf("ListRules: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 rules on fresh store, got %d", len(got))
	}
}

func TestSaveRulesAndListInPriorityOrder(t *testing.T) {
	s := newTestStore(t)

	// Need a binding first since rules FK to it.
	if err := s.SaveBindings([]model.Binding{{Name: "Manga", LibraryRoot: "/m/a", KavitaLibID: 1}}); err != nil {
		t.Fatalf("SaveBindings: %v", err)
	}
	bindings, _ := s.ListBindings()
	bid := bindings[0].ID

	jp := "JP"
	krFalse := false
	in := []model.ClassificationRule{
		{Priority: 200, Name: "second", Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: bid},
		{Priority: 100, Name: "first", Condition: model.RuleCondition{CountryOfOrigin: &jp, IsAdult: &krFalse}, BindingID: bid},
	}
	if err := s.SaveRules(in); err != nil {
		t.Fatalf("SaveRules: %v", err)
	}
	got, _ := s.ListRules()
	if len(got) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(got))
	}
	if got[0].Priority != 100 || got[1].Priority != 200 {
		t.Errorf("expected ascending priority order, got priorities %d, %d", got[0].Priority, got[1].Priority)
	}
	if got[0].Condition.CountryOfOrigin == nil || *got[0].Condition.CountryOfOrigin != "JP" {
		t.Errorf("first rule Condition.CountryOfOrigin lost in round-trip: %+v", got[0].Condition)
	}
	if got[0].Condition.IsAdult == nil || *got[0].Condition.IsAdult != false {
		t.Errorf("first rule Condition.IsAdult lost in round-trip: %+v", got[0].Condition)
	}
}

func TestSaveRulesReplacesAll(t *testing.T) {
	s := newTestStore(t)
	_ = s.SaveBindings([]model.Binding{{Name: "Manga", LibraryRoot: "/m/a", KavitaLibID: 1}})
	bindings, _ := s.ListBindings()
	bid := bindings[0].ID

	jp := "JP"
	if err := s.SaveRules([]model.ClassificationRule{
		{Priority: 100, Name: "old", Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: bid},
	}); err != nil {
		t.Fatalf("first SaveRules: %v", err)
	}
	if err := s.SaveRules([]model.ClassificationRule{
		{Priority: 200, Name: "new", Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: bid},
	}); err != nil {
		t.Fatalf("second SaveRules: %v", err)
	}

	got, _ := s.ListRules()
	if len(got) != 1 || got[0].Name != "new" {
		t.Errorf("expected only the new rule after replace, got %+v", got)
	}
}
```

- [ ] **Step 2: Run tests, verify they fail**

```bash
go test ./internal/store/ -run 'TestListRules|TestSaveRules' -v
```

Expected: FAIL with `undefined: ListRules` / `undefined: SaveRules`.

- [ ] **Step 3: Implement the methods**

Append to `internal/store/store.go`:
```go
// ListRules returns all classification rules sorted ascending by Priority
// so the classifier can walk them first-match-wins.
func (s *Store) ListRules() ([]model.ClassificationRule, error) {
	rows, err := s.db.Query(`SELECT id, priority, name, condition_json, binding_id FROM classification_rules ORDER BY priority`)
	if err != nil {
		return nil, fmt.Errorf("list rules: %w", err)
	}
	defer rows.Close()

	var out []model.ClassificationRule
	for rows.Next() {
		var r model.ClassificationRule
		var condJSON string
		if err := rows.Scan(&r.ID, &r.Priority, &r.Name, &condJSON, &r.BindingID); err != nil {
			return nil, fmt.Errorf("scan rule: %w", err)
		}
		if err := json.Unmarshal([]byte(condJSON), &r.Condition); err != nil {
			return nil, fmt.Errorf("unmarshal rule %d condition: %w", r.ID, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveRules atomically replaces the classification_rules table contents.
// Same upsert + prune shape as SaveBindings.
func (s *Store) SaveRules(in []model.ClassificationRule) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin save rules: %w", err)
	}
	defer tx.Rollback()

	keep := make(map[int64]bool)
	for _, r := range in {
		if r.ID > 0 {
			keep[r.ID] = true
		}
	}

	if len(keep) == 0 {
		if _, err := tx.Exec(`DELETE FROM classification_rules`); err != nil {
			return fmt.Errorf("delete all rules: %w", err)
		}
	} else {
		ids := make([]any, 0, len(keep))
		placeholders := make([]string, 0, len(keep))
		for id := range keep {
			ids = append(ids, id)
			placeholders = append(placeholders, "?")
		}
		q := fmt.Sprintf(`DELETE FROM classification_rules WHERE id NOT IN (%s)`, strings.Join(placeholders, ","))
		if _, err := tx.Exec(q, ids...); err != nil {
			return fmt.Errorf("prune rules: %w", err)
		}
	}

	for i := range in {
		r := &in[i]
		condJSON, err := json.Marshal(r.Condition)
		if err != nil {
			return fmt.Errorf("marshal rule %q condition: %w", r.Name, err)
		}
		if r.ID == 0 {
			res, err := tx.Exec(
				`INSERT INTO classification_rules (priority, name, condition_json, binding_id) VALUES (?, ?, ?, ?)`,
				r.Priority, r.Name, string(condJSON), r.BindingID,
			)
			if err != nil {
				return fmt.Errorf("insert rule %q: %w", r.Name, err)
			}
			id, _ := res.LastInsertId()
			r.ID = id
		} else {
			_, err := tx.Exec(
				`INSERT INTO classification_rules (id, priority, name, condition_json, binding_id)
				 VALUES (?, ?, ?, ?, ?)
				 ON CONFLICT(id) DO UPDATE SET priority=excluded.priority, name=excluded.name,
				   condition_json=excluded.condition_json, binding_id=excluded.binding_id`,
				r.ID, r.Priority, r.Name, string(condJSON), r.BindingID,
			)
			if err != nil {
				return fmt.Errorf("upsert rule id=%d: %w", r.ID, err)
			}
		}
	}

	return tx.Commit()
}
```

Add `"encoding/json"` to the imports if not present.

- [ ] **Step 4: Run tests**

```bash
go test ./internal/store/ -run 'TestListRules|TestSaveRules' -v -race
```

Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/store_test.go
touch .claude/.verified
git -c commit.gpgsign=false commit -m "feat(store): ListRules + atomic SaveRules

ListRules returns ascending by priority so the classifier's first-match
walk is sorted at the DB layer (also covered by the migration 1 index).
SaveRules mirrors SaveBindings' atomic replace-all shape. Condition is
JSON-encoded as text so the SQLite layer doesn't need to know about
RuleCondition's pointer fields."
```

---

### Task 6: Settings model addition — `DefaultBindingID`

Tiny but load-bearing for the fallback path.

**Files:**
- Modify: `internal/model/model.go`
- Modify: `internal/store/store_test.go`

- [ ] **Step 1: Write failing test**

Append to `internal/store/store_test.go`:
```go
func TestSettingsDefaultBindingIDRoundTripNilAndSet(t *testing.T) {
	s := newTestStore(t)

	// Nil case.
	settings := model.Settings{
		FileMode:     model.FileModeHardlink,
		RenameScheme: "{series}/{series} - Ch.{chapter}.cbz",
		PollMinutes:  15,
	}
	if err := s.PutSettings(settings); err != nil {
		t.Fatalf("PutSettings nil-default: %v", err)
	}
	got, err := s.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if got.DefaultBindingID != nil {
		t.Errorf("expected DefaultBindingID nil after round-trip, got %v", *got.DefaultBindingID)
	}

	// Set case.
	id := int64(42)
	settings.DefaultBindingID = &id
	if err := s.PutSettings(settings); err != nil {
		t.Fatalf("PutSettings set-default: %v", err)
	}
	got, _ = s.GetSettings()
	if got.DefaultBindingID == nil || *got.DefaultBindingID != 42 {
		t.Errorf("expected DefaultBindingID 42 after round-trip, got %v", got.DefaultBindingID)
	}
}
```

(Adapt the helper / method names — `PutSettings`/`GetSettings` may be `SaveSettings`/`Settings` in the actual code. Check `internal/store/store.go` first.)

- [ ] **Step 2: Run test, verify it fails**

```bash
go test ./internal/store/ -run TestSettingsDefaultBindingID -v
```

Expected: FAIL with `unknown field DefaultBindingID`.

- [ ] **Step 3: Add the field to Settings**

In `internal/model/model.go`, find the existing `Settings` struct (it already has `SuwayomiBaseURL`, etc. from Library Map) and add the field. Place it near the other Library-Bindings-v2-related future fields will go:

```go
type Settings struct {
	// ... existing fields preserved ...

	// DefaultBindingID is the optional catch-all routing target when no
	// classification rule matches and no Suwayomi override applies. nil
	// means "send unmatched series to the Unmatched queue" (the safe
	// default and the pre-v2 behaviour). Set to a Binding.ID to auto-
	// route everything else.
	DefaultBindingID *int64 `json:"default_binding_id,omitempty"`
}
```

The Settings JSON round-trip already works via the existing `json.Marshal`/`json.Unmarshal` in `store.go`; the `*int64` field uses the same machinery. No store changes needed for the round-trip — `omitempty` is enough.

- [ ] **Step 4: Run test, verify it passes**

```bash
go test ./internal/store/ -run TestSettingsDefaultBindingID -v -race
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/model/model.go internal/store/store_test.go
touch .claude/.verified
git -c commit.gpgsign=false commit -m "feat(model): Settings.DefaultBindingID for fallback routing

Optional catch-all target. nil means 'use Unmatched queue' (the safe
default and the pre-v2 behaviour); a set Binding.ID auto-routes any
series that no classification rule or Suwayomi override claimed."
```

---

### Task 7: Migration 2 — v1 Settings → v2 Bindings + Rules + translated overrides

The biggest task. Idempotency is gated by `schema_versions` (the framework), but the migration itself needs to handle several real edge cases.

**Files:**
- Modify: `internal/store/migrations_v1.go` (add migration 2 function)
- Modify: `internal/store/migrations.go` (register migration 2)
- Modify: `internal/store/migrations_test.go` (add integration tests)

- [ ] **Step 1: Write failing tests covering each migration scenario**

Append to `internal/store/migrations_test.go`. These are integration tests that seed a v1 Settings row, run migrations, and assert the v2 shape:

```go
import (
	"encoding/json"
	"strings"

	"github.com/gavinmcfall/mangarr/internal/model"
)

// seedV1Settings inserts a Settings row directly (skipping the schema_versions
// guard) so we can simulate a pre-v2 boot. Returns the inserted row's ID = 1
// (Settings is a singleton in this codebase).
func seedV1Settings(t *testing.T, db *sql.DB, s model.Settings) {
	t.Helper()
	// Ensure the settings table exists. In a fresh DB we have to create it
	// here because runMigrations hasn't been called yet.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS settings (id INTEGER PRIMARY KEY, json TEXT NOT NULL)`); err != nil {
		t.Fatalf("create settings table: %v", err)
	}
	b, _ := json.Marshal(s)
	if _, err := db.Exec(`INSERT OR REPLACE INTO settings (id, json) VALUES (1, ?)`, string(b)); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
}

func readV2State(t *testing.T, db *sql.DB) (bindings []model.Binding, rules []model.ClassificationRule, settings model.Settings) {
	t.Helper()
	rows, _ := db.Query(`SELECT id, name, library_root, kavita_lib_id, default_is_adult FROM bindings ORDER BY id`)
	for rows.Next() {
		var b model.Binding
		var ia int64
		rows.Scan(&b.ID, &b.Name, &b.LibraryRoot, &b.KavitaLibID, &ia)
		b.DefaultIsAdult = ia != 0
		bindings = append(bindings, b)
	}
	rows.Close()
	rrows, _ := db.Query(`SELECT id, priority, name, condition_json, binding_id FROM classification_rules ORDER BY priority`)
	for rrows.Next() {
		var r model.ClassificationRule
		var cj string
		rrows.Scan(&r.ID, &r.Priority, &r.Name, &cj, &r.BindingID)
		json.Unmarshal([]byte(cj), &r.Condition)
		rules = append(rules, r)
	}
	rrows.Close()
	var sjson string
	db.QueryRow(`SELECT json FROM settings WHERE id=1`).Scan(&sjson)
	json.Unmarshal([]byte(sjson), &settings)
	return
}

func TestMigration2FullV1ProducesThreeBindingsAndFourRules(t *testing.T) {
	db := freshDB(t)
	seedV1Settings(t, db, model.Settings{
		LibraryRoots: map[model.ContentType]string{
			model.TypeManga:  "/media/Library/Manga",
			model.TypeManhwa: "/media/Library/Manhwa",
			model.TypeManhua: "/media/Library/Manhua",
		},
		KavitaLibIDsByType: map[model.ContentType]int64{
			model.TypeManga:  1,
			model.TypeManhwa: 2,
			model.TypeManhua: 3,
		},
	})

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}

	bindings, rules, _ := readV2State(t, db)
	if len(bindings) != 3 {
		t.Fatalf("expected 3 bindings, got %d: %+v", len(bindings), bindings)
	}
	if len(rules) != 4 {
		t.Fatalf("expected 4 rules (JP/KR/CN/TW), got %d: %+v", len(rules), rules)
	}

	// Verify the rules' country codes match the expected priorities.
	wantPriorityCountry := []struct {
		priority int
		country  string
	}{
		{100, "JP"}, {200, "KR"}, {300, "CN"}, {310, "TW"},
	}
	for i, want := range wantPriorityCountry {
		if rules[i].Priority != want.priority {
			t.Errorf("rule %d priority: want %d, got %d", i, want.priority, rules[i].Priority)
		}
		if rules[i].Condition.CountryOfOrigin == nil || *rules[i].Condition.CountryOfOrigin != want.country {
			t.Errorf("rule %d country: want %q, got %v", i, want.country, rules[i].Condition.CountryOfOrigin)
		}
	}
}

func TestMigration2OnlyMangaProducesOneBindingOneRule(t *testing.T) {
	db := freshDB(t)
	seedV1Settings(t, db, model.Settings{
		LibraryRoots:       map[model.ContentType]string{model.TypeManga: "/media/Library/Manga"},
		KavitaLibIDsByType: map[model.ContentType]int64{model.TypeManga: 1},
	})

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}

	bindings, rules, _ := readV2State(t, db)
	if len(bindings) != 1 || bindings[0].Name != "Manga" {
		t.Errorf("expected one Manga binding, got %+v", bindings)
	}
	if len(rules) != 1 || rules[0].Condition.CountryOfOrigin == nil || *rules[0].Condition.CountryOfOrigin != "JP" {
		t.Errorf("expected one JP rule, got %+v", rules)
	}
}

func TestMigration2TranslatesSuwayomiCategoryOverrides(t *testing.T) {
	db := freshDB(t)
	seedV1Settings(t, db, model.Settings{
		LibraryRoots: map[model.ContentType]string{
			model.TypeManga:  "/media/Library/Manga",
			model.TypeManhwa: "/media/Library/Manhwa",
		},
		KavitaLibIDsByType: map[model.ContentType]int64{
			model.TypeManga:  1,
			model.TypeManhwa: 2,
		},
		SuwayomiCategoryOverrides: map[int64]int64{
			10: 1, // category 10 → Kavita lib 1 (Manga) → should become Binding.ID of the Manga binding
			11: 2, // category 11 → Kavita lib 2 (Manhwa)
		},
	})

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}

	bindings, _, settings := readV2State(t, db)
	bidByName := map[string]int64{}
	for _, b := range bindings {
		bidByName[b.Name] = b.ID
	}
	if settings.SuwayomiCategoryOverrides[10] != bidByName["Manga"] {
		t.Errorf("category 10 should map to Manga binding ID %d, got %d", bidByName["Manga"], settings.SuwayomiCategoryOverrides[10])
	}
	if settings.SuwayomiCategoryOverrides[11] != bidByName["Manhwa"] {
		t.Errorf("category 11 should map to Manhwa binding ID %d, got %d", bidByName["Manhwa"], settings.SuwayomiCategoryOverrides[11])
	}
}

func TestMigration2DropsOrphanSuwayomiOverrides(t *testing.T) {
	db := freshDB(t)
	seedV1Settings(t, db, model.Settings{
		LibraryRoots:       map[model.ContentType]string{model.TypeManga: "/media/Library/Manga"},
		KavitaLibIDsByType: map[model.ContentType]int64{model.TypeManga: 1},
		SuwayomiCategoryOverrides: map[int64]int64{
			10: 1,  // valid → Manga binding
			99: 42, // ORPHAN: Kavita lib 42 not in KavitaLibIDsByType
		},
	})

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}

	_, _, settings := readV2State(t, db)
	if _, present := settings.SuwayomiCategoryOverrides[99]; present {
		t.Errorf("expected orphan override (cat=99) to be dropped, but it survived")
	}
	if _, present := settings.SuwayomiCategoryOverrides[10]; !present {
		t.Errorf("expected valid override (cat=10) to be preserved")
	}
}

func TestMigration2PreservesV1FieldsForRollbackSafety(t *testing.T) {
	db := freshDB(t)
	original := model.Settings{
		LibraryRoots:       map[model.ContentType]string{model.TypeManga: "/media/Library/Manga"},
		KavitaLibIDsByType: map[model.ContentType]int64{model.TypeManga: 1},
	}
	seedV1Settings(t, db, original)

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}

	_, _, settings := readV2State(t, db)
	if settings.LibraryRoots[model.TypeManga] != "/media/Library/Manga" {
		t.Errorf("v1 LibraryRoots[Manga] not preserved: %v", settings.LibraryRoots)
	}
	if settings.KavitaLibIDsByType[model.TypeManga] != 1 {
		t.Errorf("v1 KavitaLibIDsByType[Manga] not preserved: %v", settings.KavitaLibIDsByType)
	}
}

func TestMigration2IdempotentOnRerun(t *testing.T) {
	db := freshDB(t)
	seedV1Settings(t, db, model.Settings{
		LibraryRoots:       map[model.ContentType]string{model.TypeManga: "/media/Library/Manga"},
		KavitaLibIDsByType: map[model.ContentType]int64{model.TypeManga: 1},
	})

	if err := runMigrations(db); err != nil {
		t.Fatalf("first runMigrations: %v", err)
	}
	if err := runMigrations(db); err != nil {
		t.Fatalf("second runMigrations: %v", err)
	}

	bindings, _, _ := readV2State(t, db)
	if len(bindings) != 1 {
		t.Errorf("rerunning migrations duplicated bindings: got %d, want 1", len(bindings))
	}
}
```

Note: `strings` import added so the test file can use it later if needed; the test code above doesn't use it but it's harmless. Drop the import line if Go complains.

- [ ] **Step 2: Run tests, verify they fail**

```bash
go test ./internal/store/ -run 'TestMigration2' -v
```

Expected: FAIL — migration 2 isn't registered.

- [ ] **Step 3: Implement migration 2**

Append to `internal/store/migrations_v1.go`:
```go
import (
	"encoding/json"
	"log"

	"github.com/gavinmcfall/mangarr/internal/model"
)

// migrateV1SettingsIntoBindings reads the singleton settings row, walks
// the v1 maps (LibraryRoots, KavitaLibIDsByType, SuwayomiCategoryOverrides),
// and writes the equivalent v2 shape (bindings + classification_rules +
// translated overrides). The v1 fields stay populated on the settings row
// so a rollback to a pre-v2 release can still load them.
//
// Idempotency is enforced by the schema_versions gate in runMigrations.
// This function additionally checks for non-empty bindings before doing
// work, as belt-and-braces against accidental double application.
func migrateV1SettingsIntoBindings(tx *sql.Tx) error {
	// Read existing settings. Fresh installs have no settings row → nothing
	// to migrate; just record the version and return.
	var settingsJSON string
	err := tx.QueryRow(`SELECT json FROM settings WHERE id = 1`).Scan(&settingsJSON)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		// settings table doesn't exist on a truly fresh install — that's also OK.
		// The store's New() creates it before calling runMigrations in production,
		// but tests sometimes seed differently; treat "no such table" as no-op.
		if isNoSuchTable(err) {
			return nil
		}
		return fmt.Errorf("read v1 settings: %w", err)
	}

	var settings model.Settings
	if err := json.Unmarshal([]byte(settingsJSON), &settings); err != nil {
		return fmt.Errorf("unmarshal v1 settings: %w", err)
	}

	// Defensive: if bindings already exist, skip. The schema_versions gate
	// is the primary protection; this is a second line.
	var existing int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM bindings`).Scan(&existing); err != nil {
		return fmt.Errorf("count existing bindings: %w", err)
	}
	if existing > 0 {
		return nil
	}

	// Generate one Binding per populated content type.
	typeToBindingID := make(map[model.ContentType]int64)
	for _, ct := range []model.ContentType{model.TypeManga, model.TypeManhwa, model.TypeManhua} {
		root, hasRoot := settings.LibraryRoots[ct]
		libID, hasLib := settings.KavitaLibIDsByType[ct]
		if !hasRoot || !hasLib || root == "" || libID == 0 {
			continue
		}
		res, err := tx.Exec(
			`INSERT INTO bindings (name, library_root, kavita_lib_id, default_is_adult) VALUES (?, ?, ?, 0)`,
			string(ct), root, libID,
		)
		if err != nil {
			return fmt.Errorf("insert binding %q: %w", ct, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("last insert ID for binding %q: %w", ct, err)
		}
		typeToBindingID[ct] = id
	}

	// Generate default classification rules for the bindings we created.
	// Priorities start at 100 so users have room to slot 18+/Light Novels/
	// Comics rules above (10-90) without renumbering.
	type seed struct {
		priority int
		name     string
		country  string
		ct       model.ContentType
	}
	seeds := []seed{
		{100, "Japanese", "JP", model.TypeManga},
		{200, "Korean", "KR", model.TypeManhwa},
		{300, "Chinese (CN)", "CN", model.TypeManhua},
		{310, "Chinese (TW)", "TW", model.TypeManhua},
	}
	for _, s := range seeds {
		bid, ok := typeToBindingID[s.ct]
		if !ok {
			continue
		}
		country := s.country
		cond := model.RuleCondition{CountryOfOrigin: &country}
		condJSON, err := json.Marshal(cond)
		if err != nil {
			return fmt.Errorf("marshal condition for rule %q: %w", s.name, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO classification_rules (priority, name, condition_json, binding_id) VALUES (?, ?, ?, ?)`,
			s.priority, s.name, string(condJSON), bid,
		); err != nil {
			return fmt.Errorf("insert rule %q: %w", s.name, err)
		}
	}

	// Translate Suwayomi category overrides: Kavita library ID → Binding ID.
	// Orphans (Kavita lib not in KavitaLibIDsByType, the Plan B reverse-lookup
	// case) are logged and dropped — keeping them would silently route to
	// nothing under v2 too.
	if len(settings.SuwayomiCategoryOverrides) > 0 {
		newOverrides := make(map[int64]int64, len(settings.SuwayomiCategoryOverrides))
		for catID, oldKavitaLibID := range settings.SuwayomiCategoryOverrides {
			var translated int64
			for ct, bid := range typeToBindingID {
				if settings.KavitaLibIDsByType[ct] == oldKavitaLibID {
					translated = bid
					break
				}
			}
			if translated == 0 {
				log.Printf("store: migration 2: dropping orphan Suwayomi override (cat=%d → Kavita lib %d not in KavitaLibIDsByType)", catID, oldKavitaLibID)
				continue
			}
			newOverrides[catID] = translated
		}
		settings.SuwayomiCategoryOverrides = newOverrides
	}

	// Write the updated settings row. v1 fields (LibraryRoots,
	// KavitaLibIDsByType) stay populated for rollback safety.
	updatedJSON, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("marshal updated settings: %w", err)
	}
	if _, err := tx.Exec(`UPDATE settings SET json = ? WHERE id = 1`, string(updatedJSON)); err != nil {
		return fmt.Errorf("write updated settings: %w", err)
	}
	return nil
}

// isNoSuchTable reports whether err is the SQLite "no such table" error.
// modernc.org/sqlite returns it as a plain message containing this text;
// the framework uses a string check elsewhere in the codebase (see the
// Plan B duplicate-column-error swallow). Same pattern.
func isNoSuchTable(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "no such table")
}
```

Add `"strings"` to the imports in `migrations_v1.go`.

Register migration 2 in `internal/store/migrations.go`:
```go
var migrations = []migration{
	{1, "init-bindings-and-rules", migrateInitBindingsAndRules},
	{2, "v1-settings-into-bindings", migrateV1SettingsIntoBindings},
}
```

- [ ] **Step 4: Run tests, verify they pass**

```bash
go test ./internal/store/ -run TestMigration2 -v -race
go test ./internal/store/ -v -race
```

Expected: all PASS. The full-suite second invocation catches any regressions in the earlier framework tests caused by adding migration 2 to the registered list.

- [ ] **Step 5: Commit**

```bash
git add internal/store/migrations.go internal/store/migrations_v1.go internal/store/migrations_test.go
touch .claude/.verified
git -c commit.gpgsign=false commit -m "feat(store): migration 2 converts v1 settings to bindings + rules

Reads the singleton settings row, walks v1 maps (LibraryRoots,
KavitaLibIDsByType, SuwayomiCategoryOverrides), writes the equivalent
v2 shape:
  - one Binding per populated content type
  - default rules at priorities 100/200/300/310 for JP/KR/CN/TW
  - SuwayomiCategoryOverrides values translated from Kavita lib IDs to
    Binding IDs via reverse-lookup; orphans logged + dropped
v1 fields stay populated on the settings row so a rollback to a pre-v2
release can still read them. One release after this lands the
deprecated fields get removed (separate plan)."
```

---

### Task 8: AniList client widening — fetch `isAdult` + `format`

Tiny — extend one GraphQL query and widen the result type.

**Files:**
- Modify: `internal/anilist/anilist.go`
- Modify: `internal/anilist/anilist_test.go`

- [ ] **Step 1: Inspect current shape**

Run `grep -nE 'type Result|countryOfOrigin' internal/anilist/anilist.go` to find the existing result type and query. The v1 query is along the lines of:

```graphql
query ($title: String) {
  Media(search: $title, type: MANGA) { countryOfOrigin }
}
```

and the result type carries only `CountryOfOrigin string`.

- [ ] **Step 2: Write failing tests for the wider fields**

In `internal/anilist/anilist_test.go`, add a test that drives the new `IsAdult` and `Format` fields through the existing `httptest.Server` stub:
```go
func TestLookupReturnsIsAdultAndFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":{"Media":{"countryOfOrigin":"JP","isAdult":true,"format":"NOVEL"}}}`)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL)
	got, err := c.Lookup(context.Background(), "Some Title")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.CountryOfOrigin != "JP" {
		t.Errorf("CountryOfOrigin: want JP, got %q", got.CountryOfOrigin)
	}
	if got.IsAdult != true {
		t.Errorf("IsAdult: want true, got %v", got.IsAdult)
	}
	if got.Format != "NOVEL" {
		t.Errorf("Format: want NOVEL, got %q", got.Format)
	}
}
```

If `httptest`/`io`/`context` aren't already imported in the test file, add them.

- [ ] **Step 3: Run test, verify it fails**

```bash
go test ./internal/anilist/ -run TestLookupReturnsIsAdultAndFormat -v
```

Expected: FAIL with "unknown field IsAdult" or similar.

- [ ] **Step 4: Widen the Result type + GraphQL query**

In `internal/anilist/anilist.go`:
- Add `IsAdult bool` and `Format string` to the exported `Result` struct.
- Extend the GraphQL query string to request both new fields.
- Extend the response-decoding struct to receive them.

The exact lines depend on the existing layout, but the change is:
- query: `Media(search: $title, type: MANGA) { countryOfOrigin isAdult format }`
- response struct: `Media struct { CountryOfOrigin string `json:"countryOfOrigin"`; IsAdult bool `json:"isAdult"`; Format string `json:"format"` }`
- assignment: copy both new fields into the returned `Result`

If `Lookup` decodes into a local anonymous struct with only `CountryOfOrigin`, extend that struct with the two new fields and assign them. Keep the signature `Lookup(ctx, title) (Result, error)` unchanged.

- [ ] **Step 5: Run test, verify it passes**

```bash
go test ./internal/anilist/ -v -race
```

Expected: all PASS, including any existing CountryOfOrigin tests.

- [ ] **Step 6: Commit**

```bash
git add internal/anilist/anilist.go internal/anilist/anilist_test.go
touch .claude/.verified
git -c commit.gpgsign=false commit -m "feat(anilist): widen Lookup to return isAdult and format

Same GraphQL round-trip, two more fields selected. No rate-limit impact.
The classifier's six-step flow uses these in step 4 (AniList-rule
matching) to support 18+ variants and Light Novels."
```

---

### Task 9: Classifier rewrite — six-step flow returning `Decision`

The biggest behavioural change. Keep the existing `ClassifySeries` method around as a shim that delegates to the new method for one commit boundary, then swap callers in Task 10.

**Files:**
- Modify: `internal/classifier/classifier.go`
- Modify: `internal/classifier/classifier_test.go`

- [ ] **Step 1: Write failing tests covering each flow branch**

Replace or append in `internal/classifier/classifier_test.go`:
```go
func TestClassifyPathOnlyRuleShortCircuits(t *testing.T) {
	prefix := "/media/Downloads/comics/"
	st := &fakeStore{
		bindings: []model.Binding{{ID: 7, Name: "Comics", LibraryRoot: "/m/c", KavitaLibID: 9}},
		rules: []model.ClassificationRule{
			{ID: 1, Priority: 10, Name: "comics-by-path",
				Condition: model.RuleCondition{SourcePathPrefix: &prefix}, BindingID: 7},
		},
	}
	anilistCalls := 0
	c := New(&fakeAniList{onLookup: func(string) { anilistCalls++ }}, nil, st)

	d, err := c.Classify(context.Background(), ScanItem{Title: "Anything", ParentDir: "/media/Downloads/comics/Foo"})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if d.BindingID != 7 {
		t.Errorf("expected BindingID 7, got %d", d.BindingID)
	}
	if d.Via != "path-rule:1" {
		t.Errorf("expected Via path-rule:1, got %q", d.Via)
	}
	if anilistCalls != 0 {
		t.Errorf("expected AniList NOT to be called when path-rule short-circuits, but got %d calls", anilistCalls)
	}
}

func TestClassifyAniListRuleMatches(t *testing.T) {
	jp := "JP"
	st := &fakeStore{
		bindings: []model.Binding{{ID: 1, Name: "Manga", LibraryRoot: "/m/a", KavitaLibID: 1}},
		rules: []model.ClassificationRule{
			{ID: 5, Priority: 100, Name: "Japanese",
				Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: 1},
		},
	}
	c := New(&fakeAniList{result: anilist.Result{CountryOfOrigin: "JP"}}, nil, st)

	d, _ := c.Classify(context.Background(), ScanItem{Title: "Bleach", ParentDir: "/media/Downloads/suwayomi/bleach"})
	if d.BindingID != 1 || d.Via != "rule:5" {
		t.Errorf("expected {BindingID:1, Via:rule:5}, got %+v", d)
	}
}

func TestClassifyFirstMatchWinsByPriority(t *testing.T) {
	jp := "JP"
	yes := true
	st := &fakeStore{
		bindings: []model.Binding{
			{ID: 1, Name: "Manga"},
			{ID: 2, Name: "Manga 18+"},
		},
		rules: []model.ClassificationRule{
			// Higher number = lower priority; "Japanese 18+" at 50 wins over "Japanese" at 100.
			{ID: 10, Priority: 50, Name: "Japanese 18+",
				Condition: model.RuleCondition{CountryOfOrigin: &jp, IsAdult: &yes}, BindingID: 2},
			{ID: 11, Priority: 100, Name: "Japanese",
				Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: 1},
		},
	}
	c := New(&fakeAniList{result: anilist.Result{CountryOfOrigin: "JP", IsAdult: true}}, nil, st)

	d, _ := c.Classify(context.Background(), ScanItem{Title: "X"})
	if d.BindingID != 2 {
		t.Errorf("expected 18+ binding (ID 2) to win, got BindingID %d", d.BindingID)
	}
}

func TestClassifyMixedConditionEvaluatedInStepFourNotStepOne(t *testing.T) {
	prefix := "/media/Downloads/x/"
	jp := "JP"
	st := &fakeStore{
		bindings: []model.Binding{{ID: 1, Name: "Manga"}},
		rules: []model.ClassificationRule{
			// Path + country: NOT path-only, must wait for AniList result.
			{ID: 1, Priority: 50, Name: "mixed",
				Condition: model.RuleCondition{SourcePathPrefix: &prefix, CountryOfOrigin: &jp}, BindingID: 1},
		},
	}
	called := 0
	c := New(&fakeAniList{result: anilist.Result{CountryOfOrigin: "JP"}, onLookup: func(string) { called++ }}, nil, st)

	d, _ := c.Classify(context.Background(), ScanItem{Title: "X", ParentDir: "/media/Downloads/x/foo"})
	if d.BindingID != 1 || d.Via != "rule:1" {
		t.Errorf("expected mixed rule to match in step 4, got %+v", d)
	}
	if called == 0 {
		t.Errorf("expected AniList to be called for mixed condition (path + country), but it was not")
	}
}

func TestClassifyDefaultBindingFallback(t *testing.T) {
	defaultID := int64(42)
	st := &fakeStore{
		bindings:         []model.Binding{{ID: 42, Name: "Default"}},
		rules:            nil,
		defaultBindingID: &defaultID,
	}
	c := New(&fakeAniList{result: anilist.Result{CountryOfOrigin: "JP"}}, nil, st)

	d, _ := c.Classify(context.Background(), ScanItem{Title: "Unmatched"})
	if d.BindingID != 42 || d.Via != "default-binding" {
		t.Errorf("expected default-binding fallback, got %+v", d)
	}
}

func TestClassifyUnmatchedWhenNoDefault(t *testing.T) {
	st := &fakeStore{}
	c := New(&fakeAniList{result: anilist.Result{CountryOfOrigin: "JP"}}, nil, st)

	d, _ := c.Classify(context.Background(), ScanItem{Title: "Nothing"})
	if d.BindingID != 0 || d.Via != "unmatched" {
		t.Errorf("expected unmatched, got %+v", d)
	}
}
```

You'll need a `fakeStore` test helper (likely already exists in some form; if not, add it):
```go
type fakeStore struct {
	bindings         []model.Binding
	rules            []model.ClassificationRule
	settings         model.Settings  // for Suwayomi overrides
	defaultBindingID *int64
}

func (s *fakeStore) ListBindings() ([]model.Binding, error)          { return s.bindings, nil }
func (s *fakeStore) ListRules() ([]model.ClassificationRule, error)  { return s.rules, nil }
func (s *fakeStore) GetSettings() (model.Settings, error) {
	out := s.settings
	out.DefaultBindingID = s.defaultBindingID
	return out, nil
}
```

And a `fakeAniList`:
```go
type fakeAniList struct {
	result   anilist.Result
	err      error
	onLookup func(title string)
}

func (f *fakeAniList) Lookup(ctx context.Context, title string) (anilist.Result, error) {
	if f.onLookup != nil {
		f.onLookup(title)
	}
	return f.result, f.err
}
```

If the existing Library Map era test file already has stubs called `fakeStore`/`fakeAniList`, reuse and extend them. Don't duplicate.

- [ ] **Step 2: Run tests, verify they fail**

```bash
go test ./internal/classifier/ -run TestClassify -v
```

Expected: FAIL with "undefined: Classify" or wrong return type.

- [ ] **Step 3: Implement the rewrite**

Rewrite `internal/classifier/classifier.go` so the `Classifier` exposes a new `Classify(ctx, ScanItem) (model.Decision, error)` method following the six-step flow from the spec. Keep imports clean; do NOT delete the existing `ClassifySeries` yet — it's the v1-era method poller may still call. We'll swap callers in Task 10.

```go
type Classifier struct {
	anilist  AniListClient
	suwayomi PathLookup // can be nil; nil-safe in flow
	store    SettingsReader
}

type AniListClient interface {
	Lookup(ctx context.Context, title string) (anilist.Result, error)
}

type PathLookup interface {
	Lookup(parentDir string) (suwayomi.CacheEntry, bool)
}

type SettingsReader interface {
	ListBindings() ([]model.Binding, error)
	ListRules() ([]model.ClassificationRule, error)
	GetSettings() (model.Settings, error)
}

type ScanItem struct {
	Title     string
	ParentDir string
}

func New(a AniListClient, p PathLookup, s SettingsReader) *Classifier {
	return &Classifier{anilist: a, suwayomi: p, store: s}
}

// Classify is the v2 six-step flow described in
// docs/specs/2026-05-30-library-bindings-v2.md.
func (c *Classifier) Classify(ctx context.Context, item ScanItem) (model.Decision, error) {
	settings, err := c.store.GetSettings()
	if err != nil {
		return model.Decision{}, fmt.Errorf("load settings: %w", err)
	}
	rules, err := c.store.ListRules()
	if err != nil {
		return model.Decision{}, fmt.Errorf("load rules: %w", err)
	}
	// Rules already sorted by Priority in the store; the index from migration 1
	// guarantees the SQL order matches Go ascending.

	// Step 1: path-only rules short-circuit before any AniList call.
	for _, r := range rules {
		if !r.Condition.IsPathOnly() {
			continue
		}
		if strings.HasPrefix(item.ParentDir, *r.Condition.SourcePathPrefix) {
			return model.Decision{BindingID: r.BindingID, Via: fmt.Sprintf("path-rule:%d", r.ID)}, nil
		}
	}

	// Step 2-3: Suwayomi PathCache lookup, then category overrides.
	// Use the v2 SuwayomiCategoryBindings map (populated by Migration 2
	// from the v1-era SuwayomiCategoryOverrides via reverse-lookup).
	// The v1 field is left untouched on the settings row for rollback
	// safety; v2 code paths must read the new field.
	if c.suwayomi != nil {
		if entry, ok := c.suwayomi.Lookup(item.ParentDir); ok {
			for _, catID := range entry.CategoryIDs {
				if bindingID, mapped := settings.SuwayomiCategoryBindings[catID]; mapped {
					return model.Decision{
						BindingID: bindingID,
						Via:       fmt.Sprintf("suwayomi-override:cat=%d", catID),
					}, nil
				}
			}
		}
	}

	// Step 4: AniList rules. Skip if AniList call errored (network /
	// no-match); fall through to default / unmatched.
	result, anilistErr := c.anilist.Lookup(ctx, item.Title)
	if anilistErr == nil {
		for _, r := range rules {
			if r.Condition.IsPathOnly() {
				continue // already evaluated in step 1
			}
			if matchesRule(r.Condition, result, item.ParentDir) {
				return model.Decision{BindingID: r.BindingID, Via: fmt.Sprintf("rule:%d", r.ID)}, nil
			}
		}
	}

	// Step 5: Default binding fallback.
	if settings.DefaultBindingID != nil {
		return model.Decision{BindingID: *settings.DefaultBindingID, Via: "default-binding"}, nil
	}

	// Step 6: Unmatched.
	return model.Decision{BindingID: 0, Via: "unmatched"}, nil
}

// matchesRule applies AND-semantics across set Condition fields. Unset
// pointer = wildcard (no constraint on that axis).
func matchesRule(cond model.RuleCondition, result anilist.Result, parentDir string) bool {
	if cond.CountryOfOrigin != nil && result.CountryOfOrigin != *cond.CountryOfOrigin {
		return false
	}
	if cond.IsAdult != nil && result.IsAdult != *cond.IsAdult {
		return false
	}
	if cond.Format != nil && result.Format != *cond.Format {
		return false
	}
	if cond.SourcePathPrefix != nil && !strings.HasPrefix(parentDir, *cond.SourcePathPrefix) {
		return false
	}
	return true
}
```

Imports to ensure are present: `context`, `fmt`, `strings`, `github.com/gavinmcfall/mangarr/internal/model`, `github.com/gavinmcfall/mangarr/internal/anilist`, `github.com/gavinmcfall/mangarr/internal/suwayomi`.

If the existing `Classify` (no-ctx, no-Decision return) exists, leave it alone for this commit. Task 10 swaps callers and Task 11 removes the old method.

- [ ] **Step 4: Run tests, verify they pass**

```bash
go test ./internal/classifier/ -run TestClassify -v -race
go test ./internal/classifier/ -v -race
```

Expected: new tests PASS; existing tests (which call the old `ClassifySeries`) also still PASS because we haven't removed it.

- [ ] **Step 5: Commit**

```bash
git add internal/classifier/classifier.go internal/classifier/classifier_test.go
touch .claude/.verified
git -c commit.gpgsign=false commit -m "feat(classifier): v2 six-step Classify returning Decision

Path-only rules short-circuit step 1 before any AniList call; Suwayomi
PathCache + category overrides remain the step 2-3 fast path; AniList
lookup widens to consume countryOfOrigin/isAdult/format; AniList rules
walk in priority order with AND-semantics across set Condition fields;
default-binding fallback in step 5; Unmatched in step 6.

The v1 ClassifySeries entry point is intentionally preserved for one
commit so the poller test suite stays green; the next task swaps
callers and the one after removes the legacy method."
```

---

### Task 10: Poller adaptation — route by `Decision.BindingID`

Swap callers from `ClassifySeries` to `Classify`; route by binding ID instead of ContentType reverse-lookup.

**Files:**
- Modify: `internal/poller/poller.go`
- Modify: `internal/poller/poller_test.go` (update fakes)

- [ ] **Step 1: Inspect current poller flow**

```bash
grep -nE 'ClassifySeries|reverseKavitaLibLookup|LibraryRoots\[|KavitaLibIDsByType\[' internal/poller/poller.go
```

The current flow (Library Map era): poller calls `ClassifySeries(s)`, gets `(ContentType, via, error)`, then uses `LibraryRoots[ContentType]` + `KavitaLibIDsByType[ContentType]` to derive the filesystem destination + Kavita scan ID. Replace with: call `Classify(ctx, ScanItem{Title, ParentDir})`, look up the matching `Binding` from the cached bindings list, use `Binding.LibraryRoot` + `Binding.KavitaLibID`.

- [ ] **Step 2: Write failing test**

Add to `internal/poller/poller_test.go`:
```go
func TestRunOnceRoutesByBindingFromV2Classifier(t *testing.T) {
	// Build a Poller wired to a fake classifier that returns Decision{BindingID: 7, Via: "rule:1"}
	// and a store with one Binding{ID: 7, LibraryRoot: "/dst/a", KavitaLibID: 99}.
	// Assert the file lands under /dst/a and a Kavita scan is triggered for libID 99.

	st := &fakeStore{
		bindings: []model.Binding{{ID: 7, Name: "Manga", LibraryRoot: "/dst/a", KavitaLibID: 99}},
	}
	cls := &fakeClassifier{decision: model.Decision{BindingID: 7, Via: "rule:1"}}
	kav := &fakeKavita{}
	scn := &fakeScanner{items: []ScanItem{{Title: "Foo", ParentDir: "/src/foo"}}}
	flr := &fakeFiler{}

	p := &Poller{
		Scanner:    scn,
		Classifier: cls,
		Filer:      flr,
		KavitaScan: kav,
		Store:      st,
	}
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if flr.lastDst != "/dst/a" {
		t.Errorf("expected file routed to /dst/a, got %q", flr.lastDst)
	}
	if kav.lastScanLibID != 99 {
		t.Errorf("expected Kavita scan triggered for lib 99, got %d", kav.lastScanLibID)
	}
}
```

The exact field names of `Poller`, `fakeScanner`, `fakeFiler`, `fakeKavita` depend on what's in the file. Read the file first and adapt the test to use the established shapes. The point of the test is: classifier returns BindingID; poller looks up the Binding; routes accordingly.

- [ ] **Step 3: Run test, verify it fails**

```bash
go test ./internal/poller/ -run TestRunOnceRoutesByBinding -v
```

Expected: FAIL because the poller still uses the old ContentType path.

- [ ] **Step 4: Implement the swap**

In `internal/poller/poller.go`:
- Change the `Classifier` field's type to be the new `*classifier.Classifier` (or the new interface if you keep the existing `Classifier interface` pattern).
- In `RunOnce`, build a `ScanItem{Title, ParentDir}` and call `c.Classifier.Classify(ctx, item)` to get a `Decision`.
- Load bindings once per tick (cheap; ~10 rows). Build `bindingByID := map[int64]model.Binding{}`.
- Resolve `d.BindingID` to a `Binding`. If not found (deleted between save and tick), log + skip + record activity as `unmatched`.
- Pass `binding.LibraryRoot` to the filer and `binding.KavitaLibID` to the Kavita scan trigger.
- Write the activity entry with `d.Via`.

Tests fakes (`fakeClassifier`, etc.) update accordingly. If `fakeClassifier` previously returned `(ContentType, via, error)`, change it to return `(model.Decision, error)`.

- [ ] **Step 5: Run all poller tests**

```bash
go test ./internal/poller/ -v -race
```

Expected: all PASS. The pre-existing `TestRunOnceCallsGCWhenBinPresent` failure (broken on main) may still fail — that's OK, not yours to fix.

- [ ] **Step 6: Commit**

```bash
git add internal/poller/poller.go internal/poller/poller_test.go
touch .claude/.verified
git -c commit.gpgsign=false commit -m "feat(poller): route by Decision.BindingID from v2 classifier

Poller calls Classifier.Classify(ctx, ScanItem) to get a Decision,
resolves the BindingID against the bindings table loaded once per
tick, and passes Binding.LibraryRoot + Binding.KavitaLibID downstream.
The v1 ContentType reverse-lookup is gone — any binding is now a
valid routing target regardless of which primary content type it's
associated with."
```

---

### Task 11: Remove legacy `ClassifySeries` + `main.go` wiring

Wire `runMigrations` into the store's `New()` boot path so production servers actually run migrations. Then delete the v1 classifier entry point now that nothing calls it.

**Files:**
- Modify: `internal/store/store.go` (call `runMigrations` in `New()`)
- Modify: `internal/classifier/classifier.go` (delete `ClassifySeries` and any v1-only helpers)
- Modify: `internal/classifier/classifier_test.go` (delete tests for the removed method)
- Modify: `main.go` (verify wiring; usually no change needed if store's `New()` runs migrations transparently)

- [ ] **Step 1: Wire migrations into store.New()**

In `internal/store/store.go`, find `func New(path string) (*Store, error)`. After the DB is opened and the existing `settings` table is ensured (or wherever the existing `CREATE TABLE settings` happens), call `runMigrations`:
```go
if err := runMigrations(s.db); err != nil {
    s.db.Close()
    return nil, fmt.Errorf("run migrations: %w", err)
}
```

- [ ] **Step 2: Confirm boot flow with a test**

Add to `internal/store/store_test.go`:
```go
func TestNewRunsMigrationsOnBoot(t *testing.T) {
	s := newTestStore(t)
	// After New(), the schema_versions table must exist and both registered
	// migrations recorded.
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_versions`).Scan(&count)
	if err != nil {
		t.Fatalf("query schema_versions: %v", err)
	}
	if count < 2 {
		t.Errorf("expected at least 2 migrations recorded on boot, got %d", count)
	}
}
```

- [ ] **Step 3: Delete the v1 classifier entry point**

In `internal/classifier/classifier.go`, remove:
- The `ClassifySeries` method (whatever signature it had: `(s model.Series) (model.ContentType, string, error)` or similar).
- The `reverseKavitaLibLookup` helper (no longer used).
- The `WithSuwayomi` / `SettingsReader` plumbing IF it was specifically v1-style. (The new `Classify` already uses these via the `*Classifier` fields populated in `New(...)` — adapt accordingly so nothing v2 breaks.)

In `internal/classifier/classifier_test.go`, delete tests for the removed method.

- [ ] **Step 4: Run the full suite**

```bash
go build ./...
go vet ./...
go test ./... -count=1 -race
```

Expected: green except the pre-existing `TestRunOnceCallsGCWhenBinPresent` poller failure (broken on main, unrelated).

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/store_test.go internal/classifier/classifier.go internal/classifier/classifier_test.go
touch .claude/.verified
git -c commit.gpgsign=false commit -m "feat(store,classifier): run migrations on boot; remove v1 ClassifySeries

store.New() now calls runMigrations after opening the DB, so any
production server that boots picks up the new bindings + rules tables
and runs migration 2 against an existing v1 settings row.

The legacy ClassifySeries entry point and reverseKavitaLibLookup
helper are deleted; nothing calls them after the poller swap in the
previous task."
```

---

## Self-Review

### Spec coverage

Plan-A truth statements from `docs/specs/2026-05-30-library-bindings-v2.md` (Plan A section), each mapped to the task that implements it:

| Truth statement | Implemented by |
|---|---|
| `runMigrations` creates schema_versions, applies pending in order, records on success | Task 1 |
| `runMigrations` idempotent | Task 1 |
| Migration 1 creates `bindings` and `classification_rules` tables | Task 2 |
| Migration 2 converts pre-v2 Settings (bindings per content type, default rules, translated overrides) | Task 7 |
| Migration 2 logs + drops orphan overrides | Task 7 |
| Migration 2 preserves `LibraryRoots`/`KavitaLibIDsByType`/original overrides on rollback path | Task 7 |
| `model` exposes `Binding`, `ClassificationRule`, `RuleCondition` with pointer fields | Task 3 |
| Classifier returns `Decision{BindingID, Via}` via six-step flow | Task 9 |
| Rules walked in ascending Priority order, first-match-wins | Task 9 + Task 5 (index) |
| Path-only rules evaluated in step 1 | Task 9 |
| AniList `Lookup` returns `Result{CountryOfOrigin, IsAdult, Format}` single GraphQL | Task 8 |
| Poller routes via `Decision.BindingID` using `Binding` directly | Task 10 |
| Activity log carries `Via` in documented forms | Tasks 9 + 10 (poller writes Via from Decision) |

Coverage complete; no spec requirement without a task.

### Placeholder scan

No `TBD` / `TODO` / "add appropriate error handling" patterns. Every step has runnable code or a runnable command. Two adapt-as-you-go notes (Task 4's `newTestStore` helper, Task 10's poller-field-names) are explicit instructions to read existing code, not placeholders.

### Type consistency

- `Binding`, `ClassificationRule`, `RuleCondition`, `Decision` all defined in Task 3 and used identically in Tasks 4, 5, 7, 9, 10, 11. Field names (`ID`, `Name`, `LibraryRoot`, `KavitaLibID`, `DefaultIsAdult`, `Priority`, `Condition`, `BindingID`) consistent throughout.
- `Classifier.Classify(ctx, ScanItem) (model.Decision, error)` defined in Task 9 and called in Task 10 with matching signature. `ScanItem` carries `Title` and `ParentDir`; both fields used consistently.
- `RuleCondition.IsPathOnly()` defined in Task 3 and used in Task 9's `Classify`.
- `migrateInitBindingsAndRules` defined in Task 2 and registered in Task 2. `migrateV1SettingsIntoBindings` defined in Task 7 and registered in Task 7.
- Store methods `ListBindings` / `SaveBindings` / `ListRules` / `SaveRules` / `GetSettings` / `PutSettings` (the latter is the existing v1-era method name — Task 6's test uses `PutSettings`; if the actual method is `SaveSettings`, the implementer should adapt the test, noted in Task 6).
- `anilist.Result` widens to include `IsAdult` and `Format` in Task 8; used in Task 9's `matchesRule`.

No type or signature drift across tasks.
