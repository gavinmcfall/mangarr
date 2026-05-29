package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	DownloadRoots          []string
	DBPath                 string
	HTTPAddr               string
	RecycleBinPath         string
	RecycleBinRetentionDays int
}

func Load() (Config, error) {
	rootsRaw := strings.TrimSpace(os.Getenv("MANGARR_DOWNLOAD_ROOTS"))
	if rootsRaw == "" {
		return Config{}, fmt.Errorf("MANGARR_DOWNLOAD_ROOTS is required")
	}
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
	return Config{
		DownloadRoots:           roots,
		DBPath:                  db,
		HTTPAddr:                addr,
		RecycleBinPath:          binPath,
		RecycleBinRetentionDays: binRetention,
	}, nil
}
