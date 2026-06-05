// Package recyclebin provides a Sonarr-style recycle bin for filer operations.
//
// When a file would be deleted or overwritten, Send() moves it into
// Root/YYYY-MM-DD/ instead of destroying it. GC() removes entries older than
// the configured Retention duration and cleans up empty date subdirs.
package recyclebin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const dateFmt = "2006-01-02"

// Bin is the recycle bin root.
type Bin struct {
	Root      string        // absolute path; entries land under Root/YYYY-MM-DD/...
	Retention time.Duration // entries older than this are GC'd
}

// Send moves srcPath into the bin under today's date subdir.
// Returns the bin path on success. Creates Root and the date subdir as needed.
// Returns an error if srcPath does not exist, is a directory, or cannot be moved.
// If a file of the same name already exists under today's date, it is suffixed
// with " (1)", " (2)", etc. until a free slot is found.
func (b *Bin) Send(srcPath string, now time.Time) (string, error) {
	info, err := os.Stat(srcPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("recyclebin: source does not exist: %s", srcPath)
		}
		return "", fmt.Errorf("recyclebin: stat %s: %w", srcPath, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("recyclebin: source is a directory: %s", srcPath)
	}

	dateDir := filepath.Join(b.Root, now.Format(dateFmt))
	if err := os.MkdirAll(dateDir, 0o755); err != nil {
		return "", fmt.Errorf("recyclebin: create date dir %s: %w", dateDir, err)
	}

	base := filepath.Base(srcPath)
	dst := findFreePath(dateDir, base)

	if err := os.Rename(srcPath, dst); err != nil {
		return "", fmt.Errorf("recyclebin: move %s → %s: %w", srcPath, dst, err)
	}
	return dst, nil
}

// findFreePath returns a path under dir for a file named base that does not
// yet exist. If dir/base is free it is returned as-is. Otherwise the stem is
// suffixed " (1)", " (2)", ... until a free slot is found.
func findFreePath(dir, base string) string {
	candidate := filepath.Join(dir, base)
	if _, err := os.Stat(candidate); os.IsNotExist(err) {
		return candidate
	}
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for n := 1; ; n++ {
		candidate = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, n, ext))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

// GC removes any file under Root whose date-subdir is older than Retention.
// Directories whose names are not valid YYYY-MM-DD dates are left untouched.
// After removing files, empty date-subdirs are removed too.
// Returns the number of files removed and the number of empty date dirs removed.
func (b *Bin) GC(now time.Time) (filesRemoved int, dirsRemoved int, err error) {
	entries, err := os.ReadDir(b.Root)
	if err != nil {
		if os.IsNotExist(err) {
			// Nothing to GC if the bin root doesn't exist yet.
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("recyclebin: GC read root %s: %w", b.Root, err)
	}

	// Compute the cutoff as a calendar-date STRING in the same (local) frame
	// the date dirs are written in (dateDir uses now.Format(dateFmt) on move).
	// ISO "2006-01-02" strings sort lexicographically, so a string compare is
	// an exact date compare — and timezone-safe. The previous approach parsed
	// the dir name as UTC midnight and compared against cutoff.Truncate(24h)
	// (also UTC-aligned); on UTC+12/+13 hosts the local-date dir name's UTC
	// midnight collapsed onto the cutoff boundary, so an 8-day-old dir under a
	// 7-day retention was wrongly kept.
	cutoffName := now.Add(-b.Retention).Format(dateFmt)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Only process dirs whose name is a valid YYYY-MM-DD date.
		if _, parseErr := time.Parse(dateFmt, entry.Name()); parseErr != nil {
			continue
		}
		// Keep dirs dated on or after the cutoff date (within retention).
		if entry.Name() >= cutoffName {
			continue
		}

		dateDir := filepath.Join(b.Root, entry.Name())
		files, readErr := os.ReadDir(dateDir)
		if readErr != nil {
			err = fmt.Errorf("recyclebin: GC read date dir %s: %w", dateDir, readErr)
			continue
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			if rmErr := os.Remove(filepath.Join(dateDir, f.Name())); rmErr != nil {
				err = fmt.Errorf("recyclebin: GC remove file %s: %w", f.Name(), rmErr)
				continue
			}
			filesRemoved++
		}
		// Remove the date dir if now empty.
		remaining, _ := os.ReadDir(dateDir)
		if len(remaining) == 0 {
			if rmErr := os.Remove(dateDir); rmErr == nil {
				dirsRemoved++
			}
		}
	}
	return filesRemoved, dirsRemoved, err
}
