package scanner

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func writeCBZ(t *testing.T, path, comicInfo string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	if comicInfo != "" {
		w, _ := zw.Create("ComicInfo.xml")
		w.Write([]byte(comicInfo))
	}
	img, _ := zw.Create("001.jpg")
	img.Write([]byte("x"))
	zw.Close()
}

func TestScanReadsSeriesFromComicInfo(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Weeb Central", "Solo Leveling")
	writeCBZ(t, filepath.Join(dir, "Ch. 001.cbz"),
		`<?xml version="1.0"?><ComicInfo><Series>Solo Leveling</Series><Number>1</Number></ComicInfo>`)
	writeCBZ(t, filepath.Join(dir, "Ch. 002.cbz"),
		`<?xml version="1.0"?><ComicInfo><Series>Solo Leveling</Series><Number>2</Number></ComicInfo>`)

	got, err := Scan(root, "weeb")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 series, got %d", len(got))
	}
	if got[0].Title != "Solo Leveling" || got[0].ChapterCount != 2 {
		t.Fatalf("bad series: %+v", got[0])
	}
}

func TestScanFallsBackToFolderName(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "SomeSource", "Mystery Title")
	writeCBZ(t, filepath.Join(dir, "Ch. 001.cbz"), "") // no ComicInfo
	got, err := Scan(root, "x")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(got) != 1 || got[0].Title != "Mystery Title" {
		t.Fatalf("want folder-name fallback, got %+v", got)
	}
}
