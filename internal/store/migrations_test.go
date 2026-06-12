package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/model"
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

// seedV1Settings inserts a Settings row directly (skipping the schema_versions
// guard) so we can simulate a pre-v2 boot. Settings is a singleton in this
// codebase (id=1). Creates the settings table on demand so the test doesn't
// depend on Store.Open() having run.
func seedV1Settings(t *testing.T, db *sql.DB, s model.Settings) {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS settings (id INTEGER PRIMARY KEY, json TEXT NOT NULL)`); err != nil {
		t.Fatalf("create settings table: %v", err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	if _, err := db.Exec(`INSERT OR REPLACE INTO settings (id, json) VALUES (1, ?)`, string(b)); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
}

func readV2State(t *testing.T, db *sql.DB) (bindings []model.Binding, rules []model.ClassificationRule, settings model.Settings) {
	t.Helper()
	rows, err := db.Query(`SELECT id, name, library_root, kavita_lib_id, default_is_adult FROM bindings ORDER BY id`)
	if err != nil {
		t.Fatalf("query bindings: %v", err)
	}
	for rows.Next() {
		var b model.Binding
		var ia int64
		if err := rows.Scan(&b.ID, &b.Name, &b.LibraryRoot, &b.KavitaLibID, &ia); err != nil {
			t.Fatalf("scan binding: %v", err)
		}
		b.DefaultIsAdult = ia != 0
		bindings = append(bindings, b)
	}
	rows.Close()

	rrows, err := db.Query(`SELECT id, priority, name, condition_json, binding_id FROM classification_rules ORDER BY priority`)
	if err != nil {
		t.Fatalf("query rules: %v", err)
	}
	for rrows.Next() {
		var r model.ClassificationRule
		var cj string
		if err := rrows.Scan(&r.ID, &r.Priority, &r.Name, &cj, &r.BindingID); err != nil {
			t.Fatalf("scan rule: %v", err)
		}
		if err := json.Unmarshal([]byte(cj), &r.Condition); err != nil {
			t.Fatalf("unmarshal rule condition: %v", err)
		}
		rules = append(rules, r)
	}
	rrows.Close()

	var sjson string
	if err := db.QueryRow(`SELECT json FROM settings WHERE id=1`).Scan(&sjson); err != nil && err != sql.ErrNoRows {
		t.Fatalf("read settings: %v", err)
	}
	if sjson != "" {
		if err := json.Unmarshal([]byte(sjson), &settings); err != nil {
			t.Fatalf("unmarshal settings: %v", err)
		}
	}
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
			10: 1, // category 10 -> Kavita lib 1 (Manga) -> Manga binding ID
			11: 2, // category 11 -> Kavita lib 2 (Manhwa) -> Manhwa binding ID
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
	// v2 reads SuwayomiCategoryBindings (translated to Binding IDs).
	if settings.SuwayomiCategoryBindings[10] != bidByName["Manga"] {
		t.Errorf("category 10 should map to Manga binding ID %d, got %d", bidByName["Manga"], settings.SuwayomiCategoryBindings[10])
	}
	if settings.SuwayomiCategoryBindings[11] != bidByName["Manhwa"] {
		t.Errorf("category 11 should map to Manhwa binding ID %d, got %d", bidByName["Manhwa"], settings.SuwayomiCategoryBindings[11])
	}
	// v1 SuwayomiCategoryOverrides MUST be untouched so a pre-v2
	// rollback still sees the original Kavita library IDs.
	if settings.SuwayomiCategoryOverrides[10] != 1 {
		t.Errorf("v1 SuwayomiCategoryOverrides[10] mutated: want Kavita lib 1, got %d", settings.SuwayomiCategoryOverrides[10])
	}
	if settings.SuwayomiCategoryOverrides[11] != 2 {
		t.Errorf("v1 SuwayomiCategoryOverrides[11] mutated: want Kavita lib 2, got %d", settings.SuwayomiCategoryOverrides[11])
	}
}

func TestMigration2DropsOrphanSuwayomiOverrides(t *testing.T) {
	db := freshDB(t)
	seedV1Settings(t, db, model.Settings{
		LibraryRoots:       map[model.ContentType]string{model.TypeManga: "/media/Library/Manga"},
		KavitaLibIDsByType: map[model.ContentType]int64{model.TypeManga: 1},
		SuwayomiCategoryOverrides: map[int64]int64{
			10: 1,  // valid -> Manga binding
			99: 42, // ORPHAN: Kavita lib 42 not in KavitaLibIDsByType
		},
	})

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}

	_, _, settings := readV2State(t, db)
	// Orphan MUST NOT appear in the v2 bindings map.
	if _, present := settings.SuwayomiCategoryBindings[99]; present {
		t.Errorf("expected orphan override (cat=99) to be absent from SuwayomiCategoryBindings, but it survived")
	}
	// Valid translation present in v2 map.
	if _, present := settings.SuwayomiCategoryBindings[10]; !present {
		t.Errorf("expected valid override (cat=10) to be present in SuwayomiCategoryBindings")
	}
	// Orphan IS still present in v1 SuwayomiCategoryOverrides because
	// migration 2 does not mutate v1 fields (rollback safety). The drop
	// only applies to the v2 translation.
	if got := settings.SuwayomiCategoryOverrides[99]; got != 42 {
		t.Errorf("v1 SuwayomiCategoryOverrides[99] should still hold the original Kavita lib 42, got %d", got)
	}
	if got := settings.SuwayomiCategoryOverrides[10]; got != 1 {
		t.Errorf("v1 SuwayomiCategoryOverrides[10] should still hold Kavita lib 1, got %d", got)
	}
}

func TestMigration2PreservesV1FieldsForRollbackSafety(t *testing.T) {
	db := freshDB(t)
	original := model.Settings{
		LibraryRoots:       map[model.ContentType]string{model.TypeManga: "/media/Library/Manga"},
		KavitaLibIDsByType: map[model.ContentType]int64{model.TypeManga: 1},
		SuwayomiCategoryOverrides: map[int64]int64{
			10: 1, // category 10 -> Kavita lib 1 (Manga)
		},
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
	// SuwayomiCategoryOverrides MUST retain its original Kavita-library-ID
	// values so a pre-v2 rollback can read them. The v2 translation lives
	// in SuwayomiCategoryBindings (separate field).
	if got := settings.SuwayomiCategoryOverrides[10]; got != 1 {
		t.Errorf("v1 SuwayomiCategoryOverrides[10] mutated: want Kavita lib 1, got %d", got)
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

func TestMigration2NoSettingsTableIsNoOp(t *testing.T) {
	// Fresh install: bindings + classification_rules tables exist (migration 1),
	// settings table does not. Migration 2 must record version 2 and create
	// zero bindings, zero rules.
	db := freshDB(t)
	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations on fresh DB: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_versions WHERE version=2`).Scan(&count); err != nil {
		t.Fatalf("query schema_versions: %v", err)
	}
	if count != 1 {
		t.Errorf("expected version 2 recorded on fresh install, got %d rows", count)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM bindings`).Scan(&count); err != nil {
		t.Fatalf("query bindings count: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 bindings on fresh install, got %d", count)
	}
}

func TestMigration2EmptySettingsRowIsNoOp(t *testing.T) {
	// Settings table exists but has no singleton row. Migration 2 hits
	// sql.ErrNoRows path → no-op → version 2 recorded cleanly.
	db := freshDB(t)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS settings (id INTEGER PRIMARY KEY CHECK (id = 1), json TEXT NOT NULL)`); err != nil {
		t.Fatalf("create settings table: %v", err)
	}
	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_versions WHERE version=2`).Scan(&count); err != nil {
		t.Fatalf("query schema_versions: %v", err)
	}
	if count != 1 {
		t.Errorf("expected version 2 recorded with empty settings, got %d", count)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM bindings`).Scan(&count); err != nil {
		t.Fatalf("query bindings count: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 bindings with empty settings, got %d", count)
	}
}

func TestMigration9AddsMissingSinceColumn(t *testing.T) {
	// newTestStore calls Open() which runs migrate() (creates series table)
	// then runMigrations() (applies all numbered migrations). This mirrors
	// production boot and ensures migration 9 runs against a real series table.
	s := newTestStore(t)
	var name string
	err := s.DB().QueryRow(
		`SELECT name FROM pragma_table_info('series') WHERE name='missing_since'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("missing_since column not present after migrations: %v", err)
	}
	if name != "missing_since" {
		t.Fatalf("got column %q, want missing_since", name)
	}
}

func TestMigration10AddsHonestCountsColumns(t *testing.T) {
	s := newTestStore(t)
	db := s.DB()
	cases := []struct{ table, col string }{
		{"library_cache", "dud_count"},
		{"library_cache", "filed_count"},
		{"series", "manga_id"},
	}
	for _, c := range cases {
		var name string
		err := db.QueryRow(
			`SELECT name FROM pragma_table_info('`+c.table+`') WHERE name=?`, c.col,
		).Scan(&name)
		if err != nil {
			t.Errorf("%s.%s missing after migrations: %v", c.table, c.col, err)
		}
	}
}

func TestMigration2CorruptSettingsJSONReturnsErrorWithoutRecording(t *testing.T) {
	// Corrupt JSON in the settings row → migration fails → schema_versions
	// does NOT record version 2 → next boot retries (idempotent recovery).
	db := freshDB(t)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS settings (id INTEGER PRIMARY KEY CHECK (id = 1), json TEXT NOT NULL)`); err != nil {
		t.Fatalf("create settings table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO settings (id, json) VALUES (1, '{not valid json')`); err != nil {
		t.Fatalf("seed corrupt json: %v", err)
	}
	err := runMigrations(db)
	if err == nil {
		t.Fatal("expected runMigrations to fail on corrupt JSON, got nil")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_versions WHERE version=2`).Scan(&count); err != nil {
		t.Fatalf("query schema_versions: %v", err)
	}
	if count != 0 {
		t.Errorf("expected version 2 NOT recorded after JSON parse failure, got %d rows", count)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM bindings`).Scan(&count); err != nil {
		t.Fatalf("query bindings count: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 bindings after JSON parse failure, got %d", count)
	}
}
