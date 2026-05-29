package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

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
