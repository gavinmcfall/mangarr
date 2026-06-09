package web

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gavinmcfall/mangarr/internal/recyclebin"
)

func TestBinSeriesFilesMovesFilesAndRemovesDir(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "series")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"Ch.1.cbz", "Ch.2.cbz"} {
		if err := os.WriteFile(filepath.Join(src, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bin := &recyclebin.Bin{Root: filepath.Join(tmp, "bin"), Retention: time.Hour}
	if err := binSeriesFiles(bin, []string{src}, time.Unix(1700000000, 0)); err != nil {
		t.Fatalf("binSeriesFiles: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source dir not removed: %v", err)
	}
	entries, _ := filepath.Glob(filepath.Join(tmp, "bin", "*", "*.cbz"))
	if len(entries) != 2 {
		t.Errorf("want 2 files in bin, got %d", len(entries))
	}
}

func TestBinSeriesFilesMissingDirIsNoOp(t *testing.T) {
	tmp := t.TempDir()
	bin := &recyclebin.Bin{Root: filepath.Join(tmp, "bin"), Retention: time.Hour}
	if err := binSeriesFiles(bin, []string{filepath.Join(tmp, "ghost")}, time.Unix(1700000000, 0)); err != nil {
		t.Errorf("missing dir should be a no-op, got %v", err)
	}
}

func TestBinSeriesFilesRefusesShallowPath(t *testing.T) {
	bin := &recyclebin.Bin{Root: t.TempDir(), Retention: time.Hour}
	for _, p := range []string{"/", "/home", "."} {
		if err := binSeriesFiles(bin, []string{p}, time.Unix(1700000000, 0)); err == nil {
			t.Errorf("expected error for shallow path %q, got nil", p)
		}
	}
}
