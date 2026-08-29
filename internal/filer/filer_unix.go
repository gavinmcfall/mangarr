//go:build !windows

package filer

import (
	"os"
	"syscall"
)

// sameDevice reports whether two files live on the same filesystem device.
// Used to tell "an existing destination that is a different file" (a real
// conflict) from "an existing destination that is a byte-copy made by the
// cross-device hardlink fallback" (not a conflict). When the device cannot be
// determined the answer is true, so the conservative outcome is a reported
// conflict rather than a silent skip.
func sameDevice(a, b os.FileInfo) bool {
	sa, ok1 := a.Sys().(*syscall.Stat_t)
	sb, ok2 := b.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 {
		return true
	}
	return sa.Dev == sb.Dev
}
