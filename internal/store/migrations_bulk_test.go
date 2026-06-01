package store

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestMigration4CreatesBulkTables(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}

	for _, table := range []string{"bulk_jobs", "bulk_job_chapters", "library_cache"} {
		var name string
		err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`,
			table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %q missing after runMigrations: %v", table, err)
		}
	}
}

func TestMigration4BulkJobsColumns(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}

	wantCols := []string{
		"id", "manga_id", "source_id", "title", "source_name",
		"status", "total_chapters", "completed_chapters", "errored_chapters",
		"last_error", "backoff_until", "consecutive_failures",
		"created_at", "updated_at",
	}
	have := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM pragma_table_info('bulk_jobs')`)
	if err != nil {
		t.Fatalf("pragma: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		have[n] = true
	}
	for _, c := range wantCols {
		if !have[c] {
			t.Errorf("bulk_jobs missing column %q", c)
		}
	}
}

func TestMigration4BootRecoverySweep(t *testing.T) {
	// Seed bulk_job_chapters with rows in state='fed' before Open(),
	// then verify Open()'s boot sweep flips them back to 'pending'.
	t.Skip("Boot recovery sweep is verified in TestStoreOpenSweepsGhostFedChapters " +
		"because the SQL runs inside Open(), not inside runMigrations.")
}

func TestStoreOpenSweepsGhostFedChapters(t *testing.T) {
	// Use a temp file so we can close and re-open the store, simulating
	// a pod restart with rows in state='fed'.
	dir := t.TempDir()
	path := dir + "/m.db"

	s, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	// Seed a bulk_job + chapter rows directly via DB so we don't depend
	// on store CRUD methods that don't exist yet at this task's point.
	if _, err := s.DB().Exec(
		`INSERT INTO bulk_jobs (manga_id, source_id, title, source_name, status, total_chapters)
		 VALUES (1, '42', 'One Piece', 'MangaDex EN', 'running', 3)`,
	); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if _, err := s.DB().Exec(
		`INSERT INTO bulk_job_chapters (job_id, chapter_id, state) VALUES (1, 100, 'fed'), (1, 101, 'fed'), (1, 102, 'done')`,
	); err != nil {
		t.Fatalf("seed chapters: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Re-open — the sweep should flip the two 'fed' rows to 'pending'
	// and leave the 'done' row alone.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()

	var pending, fed, done int
	row := s2.DB().QueryRow(`SELECT
		(SELECT COUNT(*) FROM bulk_job_chapters WHERE state='pending'),
		(SELECT COUNT(*) FROM bulk_job_chapters WHERE state='fed'),
		(SELECT COUNT(*) FROM bulk_job_chapters WHERE state='done')`)
	if err := row.Scan(&pending, &fed, &done); err != nil {
		t.Fatalf("scan counts: %v", err)
	}
	if pending != 2 || fed != 0 || done != 1 {
		t.Errorf("boot sweep counts: want pending=2 fed=0 done=1, got pending=%d fed=%d done=%d",
			pending, fed, done)
	}
}
