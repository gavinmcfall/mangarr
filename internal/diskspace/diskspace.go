// Package diskspace reports filesystem space for local and mounted paths.
//
// It is used by the web layer to show free/total disk space for download roots
// and library roots on the Settings page and via GET /api/diskspace.
package diskspace

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Info reports filesystem space for a single path.
type Info struct {
	Path       string   // the path queried
	TotalBytes uint64
	FreeBytes  uint64   // available to non-root user (Bavail * Bsize)
	FSID       [2]int32 // filesystem identifier from statfs Fsid; identifies unique mount
	Err        error    // non-nil if the path can't be statfs'd (e.g. NFS unmounted)
}

// PercentFree returns the percentage of free space [0, 100].
// Returns 0 if TotalBytes is zero or Err is non-nil.
func (i Info) PercentFree() float64 {
	if i.Err != nil || i.TotalBytes == 0 {
		return 0
	}
	return float64(i.FreeBytes) / float64(i.TotalBytes) * 100
}

// Stat returns disk space info for a path.
// It never panics; errors are encoded in Info.Err.
func Stat(path string) Info {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return Info{Path: path, Err: fmt.Errorf("statfs %q: %w", path, err)}
	}
	// Bsize can be negative on some platforms — guard against that.
	if fs.Bsize <= 0 {
		return Info{Path: path, Err: fmt.Errorf("statfs %q: unexpected Bsize %d", path, fs.Bsize)}
	}
	bsize := uint64(fs.Bsize) //nolint:unconvert — int64 on Linux, needs explicit cast
	return Info{
		Path:       path,
		TotalBytes: fs.Blocks * bsize,
		FreeBytes:  fs.Bavail * bsize,
		FSID:       [2]int32{fs.Fsid.X__val[0], fs.Fsid.X__val[1]},
	}
}

// FormatBytes returns a human-readable representation using IEC binary prefixes
// (KiB, MiB, GiB, TiB). Values below 1 KiB are formatted as bytes.
func FormatBytes(b uint64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
		tib = 1024 * gib
	)
	switch {
	case b >= tib:
		return fmt.Sprintf("%.1f TiB", float64(b)/tib)
	case b >= gib:
		return fmt.Sprintf("%.1f GiB", float64(b)/gib)
	case b >= mib:
		return fmt.Sprintf("%.1f MiB", float64(b)/mib)
	case b >= kib:
		return fmt.Sprintf("%.1f KiB", float64(b)/kib)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// SourceLabel returns a friendly name for the filesystem backing the given path.
// It reads /proc/self/mountinfo and picks the mount whose mount-point is the
// longest ancestor of path. For NFS-like sources of form "host:export", returns
// the host's leftmost label, title-cased ("citadel.internal:/x" → "Citadel").
// For other mounts, returns the mount point ("/", "/var", etc.). On any parsing
// failure, returns the original path as a sensible fallback.
func SourceLabel(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil || abs == "" {
		abs = path
	}
	mountPoint, source := findMount(abs)
	if mountPoint == "" {
		return path
	}
	// NFS-style: "host[.domain.tld]:/exported/path"
	if i := strings.Index(source, ":"); i > 0 && !strings.HasPrefix(source, "/") {
		host := source[:i]
		if dot := strings.Index(host, "."); dot > 0 {
			host = host[:dot]
		}
		if host != "" {
			// Title-case the first byte (ASCII-safe for the hostnames we expect).
			return strings.ToUpper(host[:1]) + host[1:]
		}
	}
	return mountPoint
}

// findMount scans /proc/self/mountinfo and returns the mount point + source for
// the longest ancestor of absPath. Returns ("", "") if no mount can be found
// (e.g. /proc isn't readable, or the file format is unexpected).
func findMount(absPath string) (mountPoint, source string) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", ""
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var best, bestSrc string
	for scan.Scan() {
		// mountinfo format:
		//   36 35 98:0 / /mnt/foo rw shared:1 - ext4 /dev/sda1 rw
		// Fields are space-separated; a single " - " token separates the
		// "mount fields" from "fstype source super-options".
		parts := strings.Fields(scan.Text())
		sep := -1
		for i, p := range parts {
			if p == "-" {
				sep = i
				break
			}
		}
		if sep < 5 || sep+2 >= len(parts) {
			continue
		}
		mp := parts[4]
		src := parts[sep+2]
		if mp == absPath || mp == "/" || strings.HasPrefix(absPath, mp+"/") {
			if len(mp) > len(best) {
				best, bestSrc = mp, src
			}
		}
	}
	return best, bestSrc
}
