package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gavinmcfall/mangarr/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUpsertAndGetSeries(t *testing.T) {
	s := newTestStore(t)
	in := model.Series{Title: "Solo Leveling", SourcePath: "/dl/suwayomi/Solo Leveling", Source: "suwayomi", Status: model.StatusPending}
	id, err := s.UpsertSeries(in)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.GetSeriesByPath(in.SourcePath)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != id || got.Title != "Solo Leveling" {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	want := model.Settings{
		FileMode:      model.ModeHardlink,
		RenameScheme:  "{series}/{series} - Ch.{chapter}.cbz",
		PollMinutes:   15,
		LibraryRoots:  map[model.ContentType]string{model.TypeManhwa: "/media/Library/Books/Manhwa"},
		KavitaBaseURL: "http://kavita:5000",
		KavitaLibIDs:  []int64{2},
	}
	if err := s.SaveSettings(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.GetSettings()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.FileMode != want.FileMode || got.LibraryRoots[model.TypeManhwa] != "/media/Library/Books/Manhwa" {
		t.Fatalf("settings mismatch: %+v", got)
	}
}

func TestAddActivity(t *testing.T) {
	s := newTestStore(t)
	e := model.ActivityEntry{
		SeriesTitle: "Berserk",
		Action:      model.ActionFiled,
		Detail:      "placed into Manga library",
	}
	if err := s.AddActivity(e); err != nil {
		t.Fatalf("add activity: %v", err)
	}
}

func TestListSeries(t *testing.T) {
	s := newTestStore(t)
	for _, title := range []string{"Zzz", "Aaa", "Mmm"} {
		_, err := s.UpsertSeries(model.Series{
			Title:      title,
			SourcePath: "/dl/" + title,
			Source:     "test",
			Status:     model.StatusPending,
		})
		if err != nil {
			t.Fatalf("upsert %s: %v", title, err)
		}
	}
	list, err := s.ListSeries()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("want 3, got %d", len(list))
	}
	// must come back in alphabetical order
	if list[0].Title != "Aaa" || list[1].Title != "Mmm" || list[2].Title != "Zzz" {
		t.Fatalf("wrong order: %v %v %v", list[0].Title, list[1].Title, list[2].Title)
	}
}

// TestSettingsRoundTripSuwayomi verifies the Plan B Settings additions
// (Suwayomi connection params + category overrides) survive a JSON
// round-trip through the store. Covers the spec truth statement:
// "Settings shall round-trip SuwayomiBaseURL, SuwayomiAuthType,
// SuwayomiUsername, SuwayomiPassword, and SuwayomiCategoryOverrides
// without lossy conversion."
func TestSettingsRoundTripSuwayomi(t *testing.T) {
	s := newTestStore(t)
	want := model.Settings{
		FileMode:                  model.ModeHardlink,
		RenameScheme:              "{series}/{series} - Ch.{chapter}.cbz",
		PollMinutes:               15,
		SuwayomiBaseURL:           "http://suwayomi.entertainment.svc:4567",
		SuwayomiAuthType:          model.SuwayomiAuthSimple,
		SuwayomiUsername:          "alice",
		SuwayomiPassword:          "test-placeholder-pw",
		SuwayomiCategoryOverrides: map[int64]int64{42: 2, 43: 3, 9: 1},
	}
	if err := s.SaveSettings(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.GetSettings()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SuwayomiBaseURL != want.SuwayomiBaseURL {
		t.Errorf("SuwayomiBaseURL: want %q, got %q", want.SuwayomiBaseURL, got.SuwayomiBaseURL)
	}
	if got.SuwayomiAuthType != want.SuwayomiAuthType {
		t.Errorf("SuwayomiAuthType: want %q, got %q", want.SuwayomiAuthType, got.SuwayomiAuthType)
	}
	if got.SuwayomiUsername != want.SuwayomiUsername {
		t.Errorf("SuwayomiUsername: want %q, got %q", want.SuwayomiUsername, got.SuwayomiUsername)
	}
	if got.SuwayomiPassword != want.SuwayomiPassword {
		t.Errorf("SuwayomiPassword: want %q, got %q", want.SuwayomiPassword, got.SuwayomiPassword)
	}
	if len(got.SuwayomiCategoryOverrides) != len(want.SuwayomiCategoryOverrides) {
		t.Fatalf("override map size: want %d, got %d (%v)", len(want.SuwayomiCategoryOverrides), len(got.SuwayomiCategoryOverrides), got.SuwayomiCategoryOverrides)
	}
	for k, v := range want.SuwayomiCategoryOverrides {
		if got.SuwayomiCategoryOverrides[k] != v {
			t.Errorf("override[%d]: want %d, got %d", k, v, got.SuwayomiCategoryOverrides[k])
		}
	}
}

// TestSettingsRoundTripSuwayomiEmptyAndNilMap pins down the two edge
// shapes for SuwayomiCategoryOverrides: an explicitly empty map and a
// nil map. Both must round-trip without surfacing as garbage on read.
// The contract is "no entries", regardless of which shape went in.
func TestSettingsRoundTripSuwayomiEmptyAndNilMap(t *testing.T) {
	t.Run("nil map", func(t *testing.T) {
		s := newTestStore(t)
		want := model.Settings{
			FileMode:                  model.ModeHardlink,
			SuwayomiCategoryOverrides: nil,
		}
		if err := s.SaveSettings(want); err != nil {
			t.Fatalf("save: %v", err)
		}
		got, err := s.GetSettings()
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if len(got.SuwayomiCategoryOverrides) != 0 {
			t.Fatalf("want zero-entry override map, got %v", got.SuwayomiCategoryOverrides)
		}
	})
	t.Run("empty map", func(t *testing.T) {
		s := newTestStore(t)
		want := model.Settings{
			FileMode:                  model.ModeHardlink,
			SuwayomiCategoryOverrides: map[int64]int64{},
		}
		if err := s.SaveSettings(want); err != nil {
			t.Fatalf("save: %v", err)
		}
		got, err := s.GetSettings()
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if len(got.SuwayomiCategoryOverrides) != 0 {
			t.Fatalf("want zero-entry override map, got %v", got.SuwayomiCategoryOverrides)
		}
	})
}

func TestCacheClassification(t *testing.T) {
	s := newTestStore(t)
	if err := s.CacheClassification("solo leveling", model.TypeManhwa); err != nil {
		t.Fatalf("cache: %v", err)
	}
	got, ok, err := s.GetCachedClassification("solo leveling")
	if err != nil {
		t.Fatalf("get cache: %v", err)
	}
	if !ok || got != model.TypeManhwa {
		t.Fatalf("want Manhwa/true, got %q/%v", got, ok)
	}

	// miss
	_, ok2, err := s.GetCachedClassification("not-cached")
	if err != nil {
		t.Fatalf("miss err: %v", err)
	}
	if ok2 {
		t.Fatal("expected cache miss")
	}
}

func TestGetSeriesByIDReturnsSeries(t *testing.T) {
	s := newTestStore(t)
	in := model.Series{Title: "Dragon Ball Super (Color)", SourcePath: "/dl/dbs", Source: "tranga", Status: model.StatusUnmatched}
	id, err := s.UpsertSeries(in)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := s.GetSeriesByID(id)
	if err != nil {
		t.Fatalf("GetSeriesByID: %v", err)
	}
	if got.ID != id {
		t.Fatalf("want id=%d, got %d", id, got.ID)
	}
	if got.Title != in.Title {
		t.Fatalf("want title=%q, got %q", in.Title, got.Title)
	}
	if got.Status != model.StatusUnmatched {
		t.Fatalf("want status=unmatched, got %q", got.Status)
	}
}

func TestGetSeriesByIDNotFoundReturnsErr(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetSeriesByID(9999)
	if err == nil {
		t.Fatal("expected error for missing series, got nil")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("want sql.ErrNoRows, got %v", err)
	}
}

func TestUpsertSeriesUpdates(t *testing.T) {
	s := newTestStore(t)
	in := model.Series{Title: "Naruto", SourcePath: "/dl/naruto", Source: "suwayomi", Status: model.StatusPending}
	id1, err := s.UpsertSeries(in)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// update status + chapter count
	in.Status = model.StatusFiled
	in.ChapterCount = 42
	id2, err := s.UpsertSeries(in)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("upsert changed ID: %d -> %d", id1, id2)
	}

	got, err := s.GetSeriesByPath(in.SourcePath)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != model.StatusFiled || got.ChapterCount != 42 {
		t.Fatalf("update didn't take: %+v", got)
	}
}

// TestUpsertSeriesReturnsCorrectIDOnUpdateAfterOtherInserts is the
// regression guard for the LastInsertId-on-UPSERT bug: SQLite does not
// update last_insert_rowid on the ON CONFLICT DO UPDATE path, so it
// returns a stale rowid from a prior INSERT. With ≥2 series, re-upserting
// the FIRST one must still return ITS id — not the most-recently-inserted
// series' id. (The single-series TestUpsertSeriesUpdates passes either way
// because the stale rowid coincidentally equals the only row's id.)
func TestUpsertSeriesReturnsCorrectIDOnUpdateAfterOtherInserts(t *testing.T) {
	s := newTestStore(t)
	idA, err := s.UpsertSeries(model.Series{Title: "Berserk", SourcePath: "/dl/berserk", Source: "suwayomi", Status: model.StatusPending})
	if err != nil {
		t.Fatalf("insert A: %v", err)
	}
	idB, err := s.UpsertSeries(model.Series{Title: "Solo Leveling", SourcePath: "/dl/solo", Source: "suwayomi", Status: model.StatusPending})
	if err != nil {
		t.Fatalf("insert B: %v", err)
	}
	if idA == idB {
		t.Fatalf("two distinct series got the same id: %d", idA)
	}
	// Re-upsert A (the UPDATE path). The returned id must be A's, not B's
	// (B was the most recent real INSERT, so a LastInsertId-trusting impl
	// would wrongly return idB here).
	gotA, err := s.UpsertSeries(model.Series{Title: "Berserk", SourcePath: "/dl/berserk", Source: "suwayomi", Status: model.StatusFiled})
	if err != nil {
		t.Fatalf("re-upsert A: %v", err)
	}
	if gotA != idA {
		t.Fatalf("re-upsert of A returned wrong id: got %d, want %d (B's id is %d)", gotA, idA, idB)
	}
}

// TestUpsertSeriesPreservesTypeAndManualBinding pins the ON CONFLICT
// contract: an upsert with empty Type (the Scanner-built shape) must
// NOT erase a previously-written Type. Same for ManualBindingID.
// Without this, the poller's per-tick upfront upsert would clobber
// the FileOne manual-classify Type and the UI-set ManualBindingID
// on every successful tick.
func TestUpsertSeriesPreservesTypeAndManualBinding(t *testing.T) {
	s := newTestStore(t)

	// Seed: FileOne-style row with a written Type and a UI-set override.
	in := model.Series{
		Title:      "Solo Leveling",
		SourcePath: "/dl/solo",
		Source:     "suwayomi",
		Type:       model.TypeManhwa,
		Status:     model.StatusFiled,
	}
	if _, err := s.UpsertSeries(in); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}
	pin := int64(7)
	if err := s.SetSeriesManualBinding(/* by source_path lookup */ 1, &pin); err != nil {
		t.Fatalf("SetSeriesManualBinding: %v", err)
	}

	// Scanner-style upsert: empty Type, status=Pending, no ManualBindingID.
	if _, err := s.UpsertSeries(model.Series{
		Title:      "Solo Leveling",
		SourcePath: "/dl/solo",
		Source:     "suwayomi",
		Status:     model.StatusPending,
	}); err != nil {
		t.Fatalf("scanner upsert: %v", err)
	}

	got, err := s.GetSeriesByPath("/dl/solo")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Type != model.TypeManhwa {
		t.Errorf("Type clobbered by scanner upsert: want %q, got %q", model.TypeManhwa, got.Type)
	}
	if got.ManualBindingID == nil || *got.ManualBindingID != 7 {
		t.Errorf("ManualBindingID clobbered: want *7, got %v", got.ManualBindingID)
	}
	// Status SHOULD update — the scanner upsert is the per-tick refresh.
	if got.Status != model.StatusPending {
		t.Errorf("Status: want pending, got %q", got.Status)
	}
}

// TestMarkUnmatchedStatusOnlyUpdate pins that MarkUnmatched is a
// status-flip on an EXISTING row, not a full upsert. Poller calls
// UpsertSeries upfront on every tick, so the row is guaranteed to
// exist by the time MarkUnmatched fires; doing a full upsert here
// would be a wasted second write.
func TestMarkUnmatchedStatusOnlyUpdate(t *testing.T) {
	s := newTestStore(t)
	in := model.Series{
		Title:      "Phantom",
		SourcePath: "/dl/phantom",
		Source:     "tranga",
		Status:     model.StatusPending,
	}
	if _, err := s.UpsertSeries(in); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.MarkUnmatched(in); err != nil {
		t.Fatalf("MarkUnmatched: %v", err)
	}
	got, err := s.GetSeriesByPath(in.SourcePath)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != model.StatusUnmatched {
		t.Errorf("Status: want unmatched, got %q", got.Status)
	}
}

// --- Library Bindings v2: Task 4 — Store CRUD for bindings ---

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

// TestSaveBindingsUpsertExistingByID exercises the ID>0 branch — an input
// row that carries an existing ID should update that row in place rather
// than insert a duplicate.
func TestSaveBindingsUpsertExistingByID(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveBindings([]model.Binding{{Name: "Manga", LibraryRoot: "/m/a", KavitaLibID: 1}}); err != nil {
		t.Fatalf("seed SaveBindings: %v", err)
	}
	seeded, _ := s.ListBindings()
	if len(seeded) != 1 {
		t.Fatalf("expected 1 binding after seed, got %d", len(seeded))
	}
	existingID := seeded[0].ID

	// Upsert: change the name + library root, keep the ID.
	updated := []model.Binding{{ID: existingID, Name: "Manga (renamed)", LibraryRoot: "/m/a-new", KavitaLibID: 11}}
	if err := s.SaveBindings(updated); err != nil {
		t.Fatalf("upsert SaveBindings: %v", err)
	}
	got, _ := s.ListBindings()
	if len(got) != 1 || got[0].ID != existingID || got[0].Name != "Manga (renamed)" || got[0].LibraryRoot != "/m/a-new" || got[0].KavitaLibID != 11 {
		t.Errorf("expected upserted binding, got %+v", got)
	}
}

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

// Bonus test addressing Task 4's reviewer-flagged gap: cover the mixed
// insert+upsert path in a single SaveRules call.
func TestSaveRulesMixedInsertAndUpsertInSameCall(t *testing.T) {
	s := newTestStore(t)
	_ = s.SaveBindings([]model.Binding{{Name: "Manga", LibraryRoot: "/m/a", KavitaLibID: 1}})
	bindings, _ := s.ListBindings()
	bid := bindings[0].ID

	jp := "JP"
	if err := s.SaveRules([]model.ClassificationRule{
		{Priority: 100, Name: "existing", Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: bid},
	}); err != nil {
		t.Fatalf("seed SaveRules: %v", err)
	}
	seeded, _ := s.ListRules()
	existingID := seeded[0].ID

	// Same call: upsert the existing one (rename), insert a brand new one (ID=0).
	if err := s.SaveRules([]model.ClassificationRule{
		{ID: existingID, Priority: 100, Name: "renamed", Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: bid},
		{Priority: 200, Name: "new-row", Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: bid},
	}); err != nil {
		t.Fatalf("mixed SaveRules: %v", err)
	}

	got, _ := s.ListRules()
	if len(got) != 2 {
		t.Fatalf("expected 2 rules after mixed save, got %d (%+v)", len(got), got)
	}
	if got[0].ID != existingID || got[0].Name != "renamed" {
		t.Errorf("upsert branch lost: existing rule should be 'renamed' with same ID, got %+v", got[0])
	}
	if got[1].Name != "new-row" || got[1].ID == 0 {
		t.Errorf("insert branch lost: new rule should have non-zero ID and name 'new-row', got %+v", got[1])
	}
}

// TestSettingsRoundTripDefaultBindingID verifies the Plan A v2 addition
// (Settings.DefaultBindingID, the no-match fallback) survives JSON
// round-trip through the store with both nil and explicitly-set values.
func TestSettingsRoundTripDefaultBindingID(t *testing.T) {
	t.Run("nil default", func(t *testing.T) {
		s := newTestStore(t)
		want := model.Settings{
			FileMode:     model.ModeHardlink,
			RenameScheme: "{series}/{series} - Ch.{chapter}.cbz",
			PollMinutes:  15,
		}
		if err := s.SaveSettings(want); err != nil {
			t.Fatalf("SaveSettings: %v", err)
		}
		got, err := s.GetSettings()
		if err != nil {
			t.Fatalf("GetSettings: %v", err)
		}
		if got.DefaultBindingID != nil {
			t.Errorf("expected DefaultBindingID nil after round-trip, got %v", *got.DefaultBindingID)
		}
	})

	t.Run("set default", func(t *testing.T) {
		s := newTestStore(t)
		id := int64(42)
		want := model.Settings{
			FileMode:         model.ModeHardlink,
			RenameScheme:     "{series}/{series} - Ch.{chapter}.cbz",
			PollMinutes:      15,
			DefaultBindingID: &id,
		}
		if err := s.SaveSettings(want); err != nil {
			t.Fatalf("SaveSettings: %v", err)
		}
		got, err := s.GetSettings()
		if err != nil {
			t.Fatalf("GetSettings: %v", err)
		}
		if got.DefaultBindingID == nil {
			t.Fatalf("expected DefaultBindingID set after round-trip, got nil")
		}
		if *got.DefaultBindingID != 42 {
			t.Errorf("expected DefaultBindingID 42, got %d", *got.DefaultBindingID)
		}
	})
}

func TestDefaultSettingsHasBulkPacingDefaults(t *testing.T) {
	s := newTestStore(t)
	set, err := s.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings on fresh store: %v", err)
	}
	if set.BulkMaxInFlight != 5 {
		t.Errorf("BulkMaxInFlight default: want 5, got %d", set.BulkMaxInFlight)
	}
	if set.BulkRefillThreshold != 2 {
		t.Errorf("BulkRefillThreshold default: want 2, got %d", set.BulkRefillThreshold)
	}
	if set.BulkInterBatchDelaySec != 1 {
		t.Errorf("BulkInterBatchDelaySec default: want 1, got %d", set.BulkInterBatchDelaySec)
	}
}

// TestGetSettingsAppliesNewBulkDefaults verifies that the stalled-job
// detector knobs have their correct defaults on a fresh store (no
// SaveSettings call). Covers:
//   - BulkStallTimeoutMinutes = 30
//   - BulkChapterMaxRetries   = 3
//   - BulkAutoErrorEmptyChaptersDisabled = false  (meaning auto-error IS enabled by default)
func TestGetSettingsAppliesNewBulkDefaults(t *testing.T) {
	s := newTestStore(t)
	set, err := s.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings on fresh store: %v", err)
	}
	if set.BulkStallTimeoutMinutes != 30 {
		t.Errorf("BulkStallTimeoutMinutes default: want 30, got %d", set.BulkStallTimeoutMinutes)
	}
	if set.BulkChapterMaxRetries != 3 {
		t.Errorf("BulkChapterMaxRetries default: want 3, got %d", set.BulkChapterMaxRetries)
	}
	// BulkAutoErrorEmptyChaptersDisabled defaults false → auto-error is ENABLED by default.
	if set.BulkAutoErrorEmptyChaptersDisabled {
		t.Errorf("BulkAutoErrorEmptyChaptersDisabled default: want false (auto-error enabled), got true")
	}
}

// TestSaveSettingsRoundTripsNewBulkFields verifies that explicit non-default
// values for the stalled-job detector knobs survive a SaveSettings /
// GetSettings round-trip without loss.
func TestApplySettingsDefaultsReconcile(t *testing.T) {
	var s model.Settings
	applySettingsDefaults(&s)
	if s.ReconcileGraceMinutes != 10 {
		t.Errorf("grace = %d, want 10", s.ReconcileGraceMinutes)
	}
	if s.ReconcileMassVanishPercent != 25 {
		t.Errorf("percent = %d, want 25", s.ReconcileMassVanishPercent)
	}
	if s.ReconcileMassVanishMinCount != 5 {
		t.Errorf("minCount = %d, want 5", s.ReconcileMassVanishMinCount)
	}
}

func TestSaveSettingsRoundTripsNewBulkFields(t *testing.T) {
	s := newTestStore(t)
	want := model.Settings{
		FileMode:                          model.ModeHardlink,
		RenameScheme:                      "{series}/{series} - Ch.{chapter}.cbz",
		PollMinutes:                       15,
		BulkStallTimeoutMinutes:           45,
		BulkChapterMaxRetries:             5,
		BulkAutoErrorEmptyChaptersDisabled: true,
	}
	if err := s.SaveSettings(want); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	got, err := s.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if got.BulkStallTimeoutMinutes != 45 {
		t.Errorf("BulkStallTimeoutMinutes round-trip: want 45, got %d", got.BulkStallTimeoutMinutes)
	}
	if got.BulkChapterMaxRetries != 5 {
		t.Errorf("BulkChapterMaxRetries round-trip: want 5, got %d", got.BulkChapterMaxRetries)
	}
	if !got.BulkAutoErrorEmptyChaptersDisabled {
		t.Errorf("BulkAutoErrorEmptyChaptersDisabled round-trip: want true, got false")
	}
}

func TestSetSeriesMissingSinceAndStatusRoundTrip(t *testing.T) {
	s := newTestStore(t)
	id, err := s.UpsertSeries(model.Series{
		Title: "X", SourcePath: "/d/X", Source: "suwayomi",
		Type: model.TypeUnknown, Status: model.StatusPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0).UTC()
	if err := s.SetSeriesMissingSince(id, &now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSeriesStatus(id, model.StatusOrphaned); err != nil {
		t.Fatal(err)
	}
	lite, err := s.ListSeriesLite()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, l := range lite {
		if l.ID == id {
			found = true
			if l.Status != model.StatusOrphaned {
				t.Errorf("status = %q, want orphaned", l.Status)
			}
			if l.MissingSince == nil || !l.MissingSince.Equal(now) {
				t.Errorf("missing_since = %v, want %v", l.MissingSince, now)
			}
			if l.SourcePath != "/d/X" {
				t.Errorf("source_path = %q", l.SourcePath)
			}
		}
	}
	if !found {
		t.Fatal("series not returned by ListSeriesLite")
	}
	if err := s.SetSeriesMissingSince(id, nil); err != nil {
		t.Fatal(err)
	}
	lite, err = s.ListSeriesLite()
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lite {
		if l.ID == id && l.MissingSince != nil {
			t.Errorf("missing_since not cleared: %v", l.MissingSince)
		}
	}
}

func TestDeleteSeriesRemovesRowAndTags(t *testing.T) {
	s := newTestStore(t)
	id, err := s.UpsertSeries(model.Series{
		Title: "Y", SourcePath: "/d/Y", Source: "suwayomi",
		Type: model.TypeUnknown, Status: model.StatusPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSeriesTags(id, []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSeries(id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSeriesByID(id); err == nil {
		t.Fatal("series still present after delete")
	}
	tags, _ := s.tagsForSeries(id)
	if len(tags) != 0 {
		t.Errorf("tags not cascaded: %v", tags)
	}
	if err := s.DeleteSeries(id); err != nil {
		t.Errorf("second delete should be no-op, got %v", err)
	}
}

func TestSetAndGetSeriesByMangaID(t *testing.T) {
	s := newTestStore(t)
	id, err := s.UpsertSeries(model.Series{
		Title: "Z", SourcePath: "/d/Z", Source: "suwayomi",
		Type: model.TypeUnknown, Status: model.StatusPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSeriesMangaID(id, 4242); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSeriesByMangaID(4242)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != id || got.Title != "Z" {
		t.Fatalf("got id=%d title=%q, want id=%d Z", got.ID, got.Title, id)
	}
	if _, err := s.GetSeriesByMangaID(999999); err == nil {
		t.Error("want error for unknown manga id, got nil")
	}
}
