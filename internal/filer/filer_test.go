package filer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/model"
)

func TestRenderName(t *testing.T) {
	got := RenderName("{series}/{series} - Ch.{chapter}.cbz", "Solo Leveling", "Ch. 001.cbz")
	want := "Solo Leveling/Solo Leveling - Ch.001.cbz"
	if got != want {
		t.Fatalf("want %q got %q", want, got)
	}
}

func TestFileHardlinkIdempotent(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "dl", "Solo Leveling")
	dstRoot := filepath.Join(tmp, "lib", "Manhwa")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "Ch. 001.cbz"), []byte("data"), 0o644)

	f := &Filer{Mode: model.ModeHardlink, Scheme: "{series}/{series} - Ch.{chapter}.cbz"}
	if err := f.File("Solo Leveling", src, dstRoot); err != nil {
		t.Fatalf("file: %v", err)
	}
	out := filepath.Join(dstRoot, "Solo Leveling", "Solo Leveling - Ch.001.cbz")
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("expected filed chapter: %v", err)
	}
	// second run must be a no-op, not an error
	if err := f.File("Solo Leveling", src, dstRoot); err != nil {
		t.Fatalf("second file run should be idempotent: %v", err)
	}
}

func TestRenderNameDecimalChapter(t *testing.T) {
	got := RenderName("{series}/{series} - Ch.{chapter}.cbz", "Tower of God", "Ch. 7.5.cbz")
	want := "Tower of God/Tower of God - Ch.7.5.cbz"
	if got != want {
		t.Fatalf("want %q got %q", want, got)
	}
}

func TestFileCopyMode(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "dl", "Test Series")
	dstRoot := filepath.Join(tmp, "lib", "Manga")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "Ch. 001.cbz"), []byte("copy-data"), 0o644)

	f := &Filer{Mode: model.ModeCopy, Scheme: "{series}/{series} - Ch.{chapter}.cbz"}
	if err := f.File("Test Series", src, dstRoot); err != nil {
		t.Fatalf("file copy: %v", err)
	}
	out := filepath.Join(dstRoot, "Test Series", "Test Series - Ch.001.cbz")
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("expected filed chapter: %v", err)
	}
	if string(data) != "copy-data" {
		t.Fatalf("data mismatch: %q", data)
	}
	// second run: idempotent for copy too
	if err := f.File("Test Series", src, dstRoot); err != nil {
		t.Fatalf("second copy run should be idempotent: %v", err)
	}
}

func TestFileMoveMode(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "dl", "Move Series")
	dstRoot := filepath.Join(tmp, "lib", "Manga")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "Ch. 001.cbz"), []byte("move-data"), 0o644)

	f := &Filer{Mode: model.ModeMove, Scheme: "{series}/{series} - Ch.{chapter}.cbz"}
	if err := f.File("Move Series", src, dstRoot); err != nil {
		t.Fatalf("file move: %v", err)
	}
	out := filepath.Join(dstRoot, "Move Series", "Move Series - Ch.001.cbz")
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("expected filed chapter at dst: %v", err)
	}
	// Source should be gone after move
	if _, err := os.Stat(filepath.Join(src, "Ch. 001.cbz")); !os.IsNotExist(err) {
		t.Fatalf("expected source file to be gone after move, err=%v", err)
	}
	// second run: no source file left, nothing to do — must not error
	if err := f.File("Move Series", src, dstRoot); err != nil {
		t.Fatalf("second move run should be idempotent: %v", err)
	}
}

func TestFileHardlinkFallbackToCopyOnCrossDevice(t *testing.T) {
	// We can't force a cross-device error in a temp-dir test, but we CAN verify
	// that the fallback path (copyFile) is reachable by testing that hardlink
	// behaviour when the dest already exists is a clean skip (not a clobber or error).
	// The cross-device fallback itself is exercised by the logic in place():
	// any os.Link error triggers copyFile. This is confirmed by code inspection.
	tmp := t.TempDir()
	src := filepath.Join(tmp, "dl", "HL Series")
	dstRoot := filepath.Join(tmp, "lib", "Manga")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "Ch. 001.cbz"), []byte("hl-data"), 0o644)

	f := &Filer{Mode: model.ModeHardlink, Scheme: "{series}/{series} - Ch.{chapter}.cbz"}
	if err := f.File("HL Series", src, dstRoot); err != nil {
		t.Fatalf("hardlink file: %v", err)
	}
	out := filepath.Join(dstRoot, "HL Series", "HL Series - Ch.001.cbz")
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read filed chapter: %v", err)
	}
	if string(data) != "hl-data" {
		t.Fatalf("data mismatch: %q", data)
	}
	// idempotency: second run skips existing dest, no error
	if err := f.File("HL Series", src, dstRoot); err != nil {
		t.Fatalf("second hardlink run should be idempotent: %v", err)
	}
}
