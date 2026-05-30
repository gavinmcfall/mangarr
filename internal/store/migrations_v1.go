package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/gavinmcfall/mangarr/internal/model"
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
	// to migrate; just record the version and return. A truly fresh install
	// may have no settings table at all (Store.Open creates it before
	// calling runMigrations in production, but tests sometimes seed
	// differently); treat "no such table" as no-op too.
	var settingsJSON string
	err := tx.QueryRow(`SELECT json FROM settings WHERE id = 1`).Scan(&settingsJSON)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
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
	// is the primary protection; this is a second line so an operator who
	// manually clears schema_versions can't silently duplicate bindings.
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

	// Translate Suwayomi category overrides into the v2 bindings map.
	// Leave the original SuwayomiCategoryOverrides untouched for rollback
	// safety (v1 reads it expecting Kavita library IDs; if we overwrote it
	// with Binding IDs, a pre-v2 rollback would silently misroute).
	//
	// Iteration is fully deterministic:
	//   - input map keys sorted ascending (sortedInt64Keys);
	//   - content-type lookup walks a fixed [Manga, Manhwa, Manhua] slice
	//     so a user-typo'd KavitaLibIDsByType{Manga:1, Manhwa:1} collision
	//     always resolves to the same binding across boots.
	//
	// Orphans (Kavita lib not in KavitaLibIDsByType, the Plan B
	// reverse-lookup case) are logged and dropped from the v2 map —
	// keeping them would silently route to nothing under v2 too. The
	// orphan entry is NOT removed from v1 SuwayomiCategoryOverrides
	// (rollback safety, see above).
	if len(settings.SuwayomiCategoryOverrides) > 0 {
		newBindings := make(map[int64]int64, len(settings.SuwayomiCategoryOverrides))
		for _, catID := range sortedInt64Keys(settings.SuwayomiCategoryOverrides) {
			oldKavitaLibID := settings.SuwayomiCategoryOverrides[catID]
			var translated int64
			for _, ct := range []model.ContentType{model.TypeManga, model.TypeManhwa, model.TypeManhua} {
				if bid, ok := typeToBindingID[ct]; ok && settings.KavitaLibIDsByType[ct] == oldKavitaLibID {
					translated = bid
					break
				}
			}
			if translated == 0 {
				log.Printf("store: migration 2: dropping orphan Suwayomi override (cat=%d → Kavita lib %d not in KavitaLibIDsByType)", catID, oldKavitaLibID)
				continue
			}
			newBindings[catID] = translated
		}
		settings.SuwayomiCategoryBindings = newBindings
	}

	// Write the updated settings row. v1 fields (LibraryRoots,
	// KavitaLibIDsByType, SuwayomiCategoryOverrides) stay populated for
	// rollback safety.
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
// the store package already uses the same substring-match pattern for
// the duplicate-column-error swallow (see sqliteDupColumnMarker).
func isNoSuchTable(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "no such table")
}

// sortedInt64Keys returns the keys of m in ascending order so callers
// that walk a map can do so deterministically (Go map iteration is
// randomised). Used by Migration 2 to translate
// SuwayomiCategoryOverrides → SuwayomiCategoryBindings.
func sortedInt64Keys(m map[int64]int64) []int64 {
	keys := make([]int64, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}
