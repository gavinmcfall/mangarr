//go:build windows

package filer

import "os"

// sameDevice on Windows: there is no cross-device hardlink fallback to
// distinguish here (tests run on a single volume), so an existing destination
// that is not the same file is always treated as a conflict.
func sameDevice(_, _ os.FileInfo) bool { return true }
