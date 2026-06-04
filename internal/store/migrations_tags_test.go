package store

import (
	"database/sql"
	"testing"
)

func TestMigration8CreatesSeriesTagsTable(t *testing.T) {
	s := newTestStore(t)
	var name string
	err := s.DB().QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='series_tags'`).Scan(&name)
	if err == sql.ErrNoRows {
		t.Fatal("migration 8 did not create series_tags table")
	}
	if err != nil {
		t.Fatalf("probe series_tags: %v", err)
	}
	cols := map[string]bool{}
	rows, _ := s.DB().Query(`SELECT name FROM pragma_table_info('series_tags')`)
	defer rows.Close()
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		cols[c] = true
	}
	if !cols["series_id"] || !cols["tag"] {
		t.Fatalf("series_tags missing expected columns; got %v", cols)
	}
}
