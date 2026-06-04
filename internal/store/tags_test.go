package store

import (
	"reflect"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/model"
)

func TestSetSeriesTagsReplacesAndDedups(t *testing.T) {
	s := newTestStore(t)
	id, err := s.UpsertSeries(model.Series{Title: "Solo Leveling", SourcePath: "/dl/solo", Status: model.StatusPending})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSeriesTags(id, []string{"webtoon", "r18", "webtoon", "  ", "archived"}); err != nil {
		t.Fatalf("SetSeriesTags: %v", err)
	}
	got, err := s.tagsForSeries(id)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"archived", "r18", "webtoon"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tags: want %v, got %v", want, got)
	}
	if err := s.SetSeriesTags(id, []string{"manhwa"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.tagsForSeries(id)
	if !reflect.DeepEqual(got, []string{"manhwa"}) {
		t.Fatalf("replace-all failed: got %v", got)
	}
	if err := s.SetSeriesTags(id, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = s.tagsForSeries(id)
	if len(got) != 0 {
		t.Fatalf("expected no tags after clear, got %v", got)
	}
}

func TestListAllTagsDistinctSorted(t *testing.T) {
	s := newTestStore(t)
	id1, _ := s.UpsertSeries(model.Series{Title: "A", SourcePath: "/a", Status: model.StatusPending})
	id2, _ := s.UpsertSeries(model.Series{Title: "B", SourcePath: "/b", Status: model.StatusPending})
	_ = s.SetSeriesTags(id1, []string{"webtoon", "r18"})
	_ = s.SetSeriesTags(id2, []string{"webtoon", "archived"})
	got, err := s.ListAllTags()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"archived", "r18", "webtoon"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListAllTags: want %v, got %v", want, got)
	}
}

func TestListSeriesPopulatesTags(t *testing.T) {
	s := newTestStore(t)
	id, _ := s.UpsertSeries(model.Series{Title: "Solo Leveling", SourcePath: "/dl/solo", Status: model.StatusPending})
	if err := s.SetSeriesTags(id, []string{"manhwa", "webtoon"}); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSeries()
	if err != nil {
		t.Fatal(err)
	}
	var found *model.Series
	for i := range list {
		if list[i].ID == id {
			found = &list[i]
		}
	}
	if found == nil {
		t.Fatal("series not in ListSeries output")
	}
	if len(found.Tags) != 2 || found.Tags[0] != "manhwa" || found.Tags[1] != "webtoon" {
		t.Fatalf("Tags not populated/sorted: %v", found.Tags)
	}
}
