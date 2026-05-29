package recyclebin

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fixedTime returns a deterministic time for tests: 2024-03-15 12:00:00 UTC.
func fixedTime() time.Time {
	return time.Date(2024, 3, 15, 12, 0, 0, 0, time.UTC)
}

func TestSendMovesFile(t *testing.T) {
	tmp := t.TempDir()
	binRoot := filepath.Join(tmp, "bin")
	srcPath := filepath.Join(tmp, "chapter.cbz")
	if err := os.WriteFile(srcPath, []byte("cbz-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := &Bin{Root: binRoot, Retention: 7 * 24 * time.Hour}
	dst, err := b.Send(srcPath, fixedTime())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Source must be gone.
	if _, err := os.Stat(srcPath); !os.IsNotExist(err) {
		t.Fatalf("expected source to be removed, stat err=%v", err)
	}

	// Destination must exist under Root/YYYY-MM-DD/.
	wantDir := filepath.Join(binRoot, "2024-03-15")
	wantDst := filepath.Join(wantDir, "chapter.cbz")
	if dst != wantDst {
		t.Fatalf("want dst=%q, got %q", wantDst, dst)
	}
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(data) != "cbz-data" {
		t.Fatalf("data mismatch: %q", data)
	}
}

func TestSendCollisionSuffixes(t *testing.T) {
	tmp := t.TempDir()
	binRoot := filepath.Join(tmp, "bin")

	// Write two source files with the same base name and Send both on the same day.
	src1 := filepath.Join(tmp, "chapter.cbz")
	src2 := filepath.Join(tmp, "sub", "chapter.cbz")
	if err := os.MkdirAll(filepath.Dir(src2), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src1, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src2, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := &Bin{Root: binRoot, Retention: 7 * 24 * time.Hour}
	now := fixedTime()

	dst1, err := b.Send(src1, now)
	if err != nil {
		t.Fatalf("Send 1: %v", err)
	}
	dst2, err := b.Send(src2, now)
	if err != nil {
		t.Fatalf("Send 2: %v", err)
	}

	// First send lands at chapter.cbz; second must be suffixed.
	wantDst1 := filepath.Join(binRoot, "2024-03-15", "chapter.cbz")
	wantDst2 := filepath.Join(binRoot, "2024-03-15", "chapter (1).cbz")
	if dst1 != wantDst1 {
		t.Fatalf("want dst1=%q, got %q", wantDst1, dst1)
	}
	if dst2 != wantDst2 {
		t.Fatalf("want dst2=%q, got %q", wantDst2, dst2)
	}
	// Both files must be readable with correct content.
	d1, _ := os.ReadFile(dst1)
	d2, _ := os.ReadFile(dst2)
	if string(d1) != "first" {
		t.Fatalf("dst1 data: %q", d1)
	}
	if string(d2) != "second" {
		t.Fatalf("dst2 data: %q", d2)
	}
}

func TestSendErrorsOnMissingSrc(t *testing.T) {
	tmp := t.TempDir()
	b := &Bin{Root: filepath.Join(tmp, "bin"), Retention: 7 * 24 * time.Hour}
	_, err := b.Send(filepath.Join(tmp, "nonexistent.cbz"), fixedTime())
	if err == nil {
		t.Fatal("expected error for non-existent source, got nil")
	}
}

func TestSendErrorsOnDir(t *testing.T) {
	tmp := t.TempDir()
	srcDir := filepath.Join(tmp, "adir")
	if err := os.Mkdir(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	b := &Bin{Root: filepath.Join(tmp, "bin"), Retention: 7 * 24 * time.Hour}
	_, err := b.Send(srcDir, fixedTime())
	if err == nil {
		t.Fatal("expected error when source is a directory, got nil")
	}
}

func TestGCRemovesOldEntries(t *testing.T) {
	tmp := t.TempDir()
	binRoot := filepath.Join(tmp, "bin")

	// Seed a date-dir that is older than 7-day retention.
	oldDate := "2024-03-07" // 8 days before fixedTime (2024-03-15)
	oldDir := filepath.Join(binRoot, oldDate)
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, "a.cbz"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, "b.cbz"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := &Bin{Root: binRoot, Retention: 7 * 24 * time.Hour}
	files, dirs, err := b.GC(fixedTime())
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if files != 2 {
		t.Fatalf("want 2 files removed, got %d", files)
	}
	if dirs != 1 {
		t.Fatalf("want 1 dir removed, got %d", dirs)
	}
	// The old dir must be gone.
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatalf("expected old date dir to be removed")
	}
}

func TestGCKeepsRecentEntries(t *testing.T) {
	tmp := t.TempDir()
	binRoot := filepath.Join(tmp, "bin")

	// Seed a date-dir that is within retention (yesterday relative to fixedTime).
	recentDate := "2024-03-14"
	recentDir := filepath.Join(binRoot, recentDate)
	if err := os.MkdirAll(recentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recentDir, "keep.cbz"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := &Bin{Root: binRoot, Retention: 7 * 24 * time.Hour}
	files, dirs, err := b.GC(fixedTime())
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if files != 0 || dirs != 0 {
		t.Fatalf("want 0 files and 0 dirs removed, got files=%d dirs=%d", files, dirs)
	}
	// The recent file must still be there.
	if _, err := os.Stat(filepath.Join(recentDir, "keep.cbz")); err != nil {
		t.Fatalf("recent file should not be removed: %v", err)
	}
}

func TestGCIgnoresMalformedDateDirs(t *testing.T) {
	tmp := t.TempDir()
	binRoot := filepath.Join(tmp, "bin")

	// Create a non-YYYY-MM-DD subdir with a file in it.
	badDir := filepath.Join(binRoot, "not-a-date")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "file.cbz"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := &Bin{Root: binRoot, Retention: 7 * 24 * time.Hour}
	files, dirs, err := b.GC(fixedTime())
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if files != 0 || dirs != 0 {
		t.Fatalf("malformed dir should be ignored; got files=%d dirs=%d", files, dirs)
	}
	// The bad dir and its file must still exist.
	if _, err := os.Stat(filepath.Join(badDir, "file.cbz")); err != nil {
		t.Fatalf("malformed dir file should not be touched: %v", err)
	}
}
