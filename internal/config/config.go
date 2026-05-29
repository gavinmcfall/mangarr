package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	DownloadRoots           []string
	DBPath                  string
	HTTPAddr                string
	RecycleBinPath          string
	RecycleBinRetentionDays int
	BackupDir               string
	BackupRetentionDays     int
	BackupIntervalHours     int
}

func Load() (Config, error) {
	rootsRaw := strings.TrimSpace(os.Getenv("MANGARR_DOWNLOAD_ROOTS"))
	var roots []string
	for _, r := range strings.Split(rootsRaw, ",") {
		if r = strings.TrimSpace(r); r != "" {
			roots = append(roots, r)
		}
	}
	db := os.Getenv("MANGARR_DB_PATH")
	if db == "" {
		db = "/config/mangarr.db"
	}
	addr := os.Getenv("MANGARR_HTTP_ADDR")
	if addr == "" {
		addr = ":8590"
	}
	binPath := os.Getenv("MANGARR_RECYCLE_BIN_PATH")
	if binPath == "" {
		binPath = "/config/recycle-bin"
	}
	binRetention := 7
	if raw := os.Getenv("MANGARR_RECYCLE_BIN_RETENTION_DAYS"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			binRetention = n
		}
	}
	backupDir := os.Getenv("MANGARR_BACKUP_DIR")
	if backupDir == "" {
		backupDir = "/config/backups"
	}
	retentionDays := 14
	if s := strings.TrimSpace(os.Getenv("MANGARR_BACKUP_RETENTION_DAYS")); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			retentionDays = n
		}
	}
	intervalHours := 24
	if s := strings.TrimSpace(os.Getenv("MANGARR_BACKUP_INTERVAL_HOURS")); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			intervalHours = n
		}
	}
	return Config{
		DownloadRoots:           roots,
		DBPath:                  db,
		HTTPAddr:                addr,
		RecycleBinPath:          binPath,
		RecycleBinRetentionDays: binRetention,
		BackupDir:               backupDir,
		BackupRetentionDays:     retentionDays,
		BackupIntervalHours:     intervalHours,
	}, nil
}
