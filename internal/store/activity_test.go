package store

import (
	"testing"
	"time"

	"github.com/gavinmcfall/mangarr/internal/model"
)

func seedActivity(t *testing.T, s *Store) {
	t.Helper()
	rows := []model.ActivityEntry{
		{SeriesTitle: "Berserk", Action: model.ActionFiled, Via: "rule:5"},
		{SeriesTitle: "Solo Leveling", Action: model.ActionFiled, Via: "rule:6"},
		{SeriesTitle: "Berserk", Action: model.ActionError, Detail: "boom"},
		{SeriesTitle: "Dandadan", Action: model.ActionUnmatched, Via: "unmatched"},
	}
	for _, r := range rows {
		if err := s.AddActivity(r); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListActivityFilteredByAction(t *testing.T) {
	s := newTestStore(t)
	seedActivity(t, s)
	page, err := s.ListActivityFiltered(model.ActivityFilter{Action: model.ActionFiled, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("action filter: want 2 filed, got total=%d len=%d", page.Total, len(page.Items))
	}
	for _, e := range page.Items {
		if e.Action != model.ActionFiled {
			t.Errorf("unexpected action %q", e.Action)
		}
	}
}

func TestListActivityFilteredBySeriesSubstring(t *testing.T) {
	s := newTestStore(t)
	seedActivity(t, s)
	page, err := s.ListActivityFiltered(model.ActivityFilter{SeriesLike: "ber", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 {
		t.Fatalf("series filter: want 2, got %d", page.Total)
	}
}

func TestListActivityFilteredPagination(t *testing.T) {
	s := newTestStore(t)
	seedActivity(t, s)
	page1, _ := s.ListActivityFiltered(model.ActivityFilter{Limit: 2, Offset: 0})
	page2, _ := s.ListActivityFiltered(model.ActivityFilter{Limit: 2, Offset: 2})
	if page1.Total != 4 || page2.Total != 4 {
		t.Fatalf("total should be 4 regardless of page; got %d/%d", page1.Total, page2.Total)
	}
	if len(page1.Items) != 2 || len(page2.Items) != 2 {
		t.Fatalf("page sizes: got %d/%d", len(page1.Items), len(page2.Items))
	}
	if page1.Items[0].ID == page2.Items[0].ID {
		t.Fatal("pages overlap — offset not applied")
	}
}

func TestListActivityFilteredByTag(t *testing.T) {
	s := newTestStore(t)
	seedActivity(t, s)
	id, _ := s.UpsertSeries(model.Series{Title: "Berserk", SourcePath: "/dl/berserk", Status: model.StatusPending})
	if err := s.SetSeriesTags(id, []string{"dark"}); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListActivityFiltered(model.ActivityFilter{Tag: "dark", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 { // both Berserk activity rows (matched by title)
		t.Fatalf("tag filter: want 2 Berserk rows, got %d", page.Total)
	}
}

func TestDeleteActivityOlderThan(t *testing.T) {
	s := newTestStore(t)
	seedActivity(t, s) // 4 rows at ~now
	// Back-date the two oldest rows by 100 days.
	if _, err := s.DB().Exec(
		`UPDATE activity SET ts = ? WHERE id IN (SELECT id FROM activity ORDER BY id ASC LIMIT 2)`,
		time.Now().AddDate(0, 0, -100).UTC().Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}
	n, err := s.DeleteActivityOlderThan(time.Now().AddDate(0, 0, -90))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("want 2 rows deleted, got %d", n)
	}
	page, _ := s.ListActivityFiltered(model.ActivityFilter{Limit: 50})
	if page.Total != 2 {
		t.Fatalf("want 2 rows remaining, got %d", page.Total)
	}
}

func TestGetSettingsDefaultsActivityRetention(t *testing.T) {
	s := newTestStore(t)
	set, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	if set.ActivityRetentionDays != 90 {
		t.Fatalf("want default 90, got %d", set.ActivityRetentionDays)
	}
}
