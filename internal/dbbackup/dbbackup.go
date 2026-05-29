// Package dbbackup provides SQLite database backup via VACUUM INTO.
//
// Backups are named mangarr-YYYYMMDD-HHMMSS.db and written to a configurable
// directory. A GC helper prunes old backups. A List helper returns existing
// backups sorted newest-first.
package dbbackup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Entry describes a single backup file.
type Entry struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	SizeBytes int64     `json:"size_bytes"`
	ModTime   time.Time `json:"mod_time"`
}

// Backup runs VACUUM INTO against the live db and writes a snapshot to
// backupDir/mangarr-YYYYMMDD-HHMMSS.db.
// backupDir is created if it does not exist.
// Returns the absolute path of the new file.
func Backup(db *sql.DB, backupDir string, now time.Time) (string, error) {
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return "", fmt.Errorf("dbbackup: mkdir %q: %w", backupDir, err)
	}
	name := "mangarr-" + now.UTC().Format("20060102-150405") + ".db"
	target := filepath.Join(backupDir, name)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", target); err != nil {
		return "", fmt.Errorf("dbbackup: VACUUM INTO: %w", err)
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return target, nil // best-effort
	}
	return abs, nil
}

// GC removes backup files whose modification time is older than retention.
// Returns the count of files removed.
func GC(backupDir string, retention time.Duration, now time.Time) (int, error) {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("dbbackup: gc readdir: %w", err)
	}
	cutoff := now.Add(-retention)
	removed := 0
	for _, de := range entries {
		if !isBackupFile(de.Name()) {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			p := filepath.Join(backupDir, de.Name())
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return removed, fmt.Errorf("dbbackup: gc remove %q: %w", p, err)
			}
			removed++
		}
	}
	return removed, nil
}

// List returns existing backups from backupDir sorted newest-first.
// Returns an empty slice (not nil) if the directory doesn't exist.
func List(backupDir string) ([]Entry, error) {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Entry{}, nil
		}
		return nil, fmt.Errorf("dbbackup: list readdir: %w", err)
	}
	var out []Entry
	for _, de := range entries {
		if de.IsDir() || !isBackupFile(de.Name()) {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		abs, _ := filepath.Abs(filepath.Join(backupDir, de.Name()))
		out = append(out, Entry{
			Name:      de.Name(),
			Path:      abs,
			SizeBytes: info.Size(),
			ModTime:   info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ModTime.After(out[j].ModTime)
	})
	if out == nil {
		return []Entry{}, nil
	}
	return out, nil
}

// isBackupFile returns true if the filename looks like a mangarr backup.
func isBackupFile(name string) bool {
	return strings.HasPrefix(name, "mangarr-") && strings.HasSuffix(name, ".db")
}

// ValidateName returns true if name is safe to serve as a download — it must
// match the backup filename pattern and contain no path separators.
func ValidateName(name string) bool {
	// Reject anything with a directory separator or parent-dir component.
	if strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, "..") {
		return false
	}
	if !strings.HasPrefix(name, "mangarr-") || !strings.HasSuffix(name, ".db") {
		return false
	}
	// Must be exactly: mangarr-YYYYMMDD-HHMMSS.db (26 chars)
	// e.g. mangarr-20260101-120000.db
	if len(name) != 26 {
		return false
	}
	return true
}

