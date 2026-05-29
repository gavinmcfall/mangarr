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

func TestLoadRecycleBinDefaults(t *testing.T) {
	t.Setenv("MANGARR_DOWNLOAD_ROOTS", "/media/Downloads/suwayomi")
	t.Setenv("MANGARR_RECYCLE_BIN_PATH", "")
	t.Setenv("MANGARR_RECYCLE_BIN_RETENTION_DAYS", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.RecycleBinPath != "/config/recycle-bin" {
		t.Fatalf("want RecycleBinPath=/config/recycle-bin, got %q", c.RecycleBinPath)
	}
	if c.RecycleBinRetentionDays != 7 {
		t.Fatalf("want RecycleBinRetentionDays=7, got %d", c.RecycleBinRetentionDays)
	}
}

func TestLoadRecycleBinCustom(t *testing.T) {
	t.Setenv("MANGARR_DOWNLOAD_ROOTS", "/media/Downloads/suwayomi")
	t.Setenv("MANGARR_RECYCLE_BIN_PATH", "/data/trash")
	t.Setenv("MANGARR_RECYCLE_BIN_RETENTION_DAYS", "14")
	c, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.RecycleBinPath != "/data/trash" {
		t.Fatalf("want RecycleBinPath=/data/trash, got %q", c.RecycleBinPath)
	}
	if c.RecycleBinRetentionDays != 14 {
		t.Fatalf("want RecycleBinRetentionDays=14, got %d", c.RecycleBinRetentionDays)
	}
}

func TestLoadBackupDefaults(t *testing.T) {
	t.Setenv("MANGARR_DOWNLOAD_ROOTS", "/media/Downloads/suwayomi")
	t.Setenv("MANGARR_BACKUP_DIR", "")
	t.Setenv("MANGARR_BACKUP_RETENTION_DAYS", "")
	t.Setenv("MANGARR_BACKUP_INTERVAL_HOURS", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.BackupDir != "/config/backups" {
		t.Fatalf("want BackupDir=/config/backups, got %q", c.BackupDir)
	}
	if c.BackupRetentionDays != 14 {
		t.Fatalf("want BackupRetentionDays=14, got %d", c.BackupRetentionDays)
	}
	if c.BackupIntervalHours != 24 {
		t.Fatalf("want BackupIntervalHours=24, got %d", c.BackupIntervalHours)
	}
}

func TestLoadBackupEnvOverrides(t *testing.T) {
	t.Setenv("MANGARR_DOWNLOAD_ROOTS", "/media/Downloads/suwayomi")
	t.Setenv("MANGARR_BACKUP_DIR", "/mnt/backups/mangarr")
	t.Setenv("MANGARR_BACKUP_RETENTION_DAYS", "7")
	t.Setenv("MANGARR_BACKUP_INTERVAL_HOURS", "6")
	c, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.BackupDir != "/mnt/backups/mangarr" {
		t.Fatalf("want BackupDir=/mnt/backups/mangarr, got %q", c.BackupDir)
	}
	if c.BackupRetentionDays != 7 {
		t.Fatalf("want BackupRetentionDays=7, got %d", c.BackupRetentionDays)
	}
	if c.BackupIntervalHours != 6 {
		t.Fatalf("want BackupIntervalHours=6, got %d", c.BackupIntervalHours)
	}
}
