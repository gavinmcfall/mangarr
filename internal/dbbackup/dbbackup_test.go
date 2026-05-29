package dbbackup

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// openTestDB opens a fresh in-memory SQLite database and populates a test row.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE t (v TEXT); INSERT INTO t VALUES ('hello')`); err != nil {
		t.Fatalf("seed db: %v", err)
	}
	return db
}

func TestBackupWritesFile(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	path, err := Backup(db, dir, now)
	if err != nil {
		t.Fatalf("Backup error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("backup file not found: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("backup file is empty")
	}

	// Open the backup as a fresh SQLite and verify the row exists.
	bdb, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open backup db: %v", err)
	}
	defer bdb.Close()
	var v string
	if err := bdb.QueryRow(`SELECT v FROM t`).Scan(&v); err != nil {
		t.Fatalf("query backup db: %v", err)
	}
	if v != "hello" {
		t.Fatalf("want 'hello', got %q", v)
	}
}

func TestBackupFilenameFormat(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	path, err := Backup(db, dir, now)
	if err != nil {
		t.Fatalf("Backup error: %v", err)
	}
	name := filepath.Base(path)
	expected := "mangarr-20260102-030405.db"
	if name != expected {
		t.Fatalf("want filename %q, got %q", expected, name)
	}
}

func TestGCRemovesOldBackups(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	retention := 14 * 24 * time.Hour // 14 days

	// Create 2 old files (older than retention) and 1 recent file.
	oldTime := now.Add(-15 * 24 * time.Hour)
	recentTime := now.Add(-1 * time.Hour)

	for _, name := range []string{"mangarr-20260101-000000.db", "mangarr-20260102-000000.db"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
	}
	recentName := "mangarr-20260528-110000.db"
	recentPath := filepath.Join(dir, recentName)
	if err := os.WriteFile(recentPath, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(recentPath, recentTime, recentTime); err != nil {
		t.Fatal(err)
	}

	removed, err := GC(dir, retention, now)
	if err != nil {
		t.Fatalf("GC error: %v", err)
	}
	if removed != 2 {
		t.Fatalf("want 2 removed, got %d", removed)
	}

	// Recent file must still be present.
	if _, err := os.Stat(recentPath); os.IsNotExist(err) {
		t.Fatal("recent backup was incorrectly removed")
	}

	// Old files must be gone.
	for _, name := range []string{"mangarr-20260101-000000.db", "mangarr-20260102-000000.db"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("old backup %q should have been removed", name)
		}
	}
}

func TestListNewestFirst(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)

	// Three files with different mtimes.
	files := []struct {
		name string
		mtime time.Time
	}{
		{"mangarr-20260527-000000.db", now.Add(-48 * time.Hour)},
		{"mangarr-20260529-000000.db", now.Add(-1 * time.Hour)},
		{"mangarr-20260528-000000.db", now.Add(-24 * time.Hour)},
	}
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, f.mtime, f.mtime); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := List(dir)
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 entries, got %d", len(entries))
	}
	// Newest first: 20260529, 20260528, 20260527
	if entries[0].Name != "mangarr-20260529-000000.db" {
		t.Fatalf("want newest first, got %q", entries[0].Name)
	}
	if entries[1].Name != "mangarr-20260528-000000.db" {
		t.Fatalf("want second %q, got %q", "mangarr-20260528-000000.db", entries[1].Name)
	}
	if entries[2].Name != "mangarr-20260527-000000.db" {
		t.Fatalf("want oldest last, got %q", entries[2].Name)
	}
}

func TestListMissingDirReturnsEmpty(t *testing.T) {
	entries, err := List("/tmp/mangarr-dbbackup-nonexistent-dir-xyz")
	if err != nil {
		t.Fatalf("List on nonexistent dir returned error: %v", err)
	}
	if entries == nil {
		t.Fatal("want empty slice, got nil")
	}
	if len(entries) != 0 {
		t.Fatalf("want 0 entries, got %d", len(entries))
	}
}
