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
