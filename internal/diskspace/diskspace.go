// Package diskspace reports filesystem space for local and mounted paths.
//
// It is used by the web layer to show free/total disk space for download roots
// and library roots on the Settings page and via GET /api/diskspace.
package diskspace

import (
	"fmt"
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
