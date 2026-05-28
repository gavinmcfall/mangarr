package config

import (
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("MANGARR_DOWNLOAD_ROOTS", "/media/Downloads/suwayomi,/media/Downloads/tranga")
	t.Setenv("MANGARR_DB_PATH", "/config/mangarr.db")
	c, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.DownloadRoots) != 2 {
		t.Fatalf("want 2 roots, got %d", len(c.DownloadRoots))
	}
	if c.DBPath != "/config/mangarr.db" {
		t.Fatalf("want db path, got %q", c.DBPath)
	}
}

func TestLoadRequiresRoots(t *testing.T) {
	t.Setenv("MANGARR_DOWNLOAD_ROOTS", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected error when no download roots set")
	}
}
