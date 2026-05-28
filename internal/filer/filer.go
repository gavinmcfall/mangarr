package filer

import (
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gavinmcfall/mangarr/internal/model"
)

// chapterNum matches the first integer or decimal number in a filename.
// Examples: "Ch. 001.cbz" → "001", "Ch. 7.5.cbz" → "7.5", "1.0.cbz" → "1.0"
var chapterNum = regexp.MustCompile(`(\d+(?:\.\d+)?)`)

// RenderName substitutes tokens in the scheme:
//
//	{series}  → the series title
//	{chapter} → the numeric portion extracted from origFile (e.g. "001", "7.5")
//
// The resulting path uses the OS path separator for directory components, but
// the scheme uses "/" as the separator (cross-platform schemes are written with /).
func RenderName(scheme, series, origFile string) string {
	ch := strings.TrimSuffix(origFile, filepath.Ext(origFile))
	if m := chapterNum.FindString(origFile); m != "" {
		ch = m
	}
	out := strings.ReplaceAll(scheme, "{series}", series)
	out = strings.ReplaceAll(out, "{chapter}", ch)
	return out
}

// Filer places .cbz files from a source directory into a destination library root
// using the configured mode (hardlink/move/copy) and rename scheme.
type Filer struct {
	Mode   model.FileMode
	Scheme string
}

// File places every .cbz from srcDir into dstRoot per the scheme + mode.
// It is idempotent: if the destination file already exists it is skipped
// without error, so repeated runs (e.g. the poller on a schedule) are safe.
// Directories are created as needed.
func (f *Filer) File(series, srcDir, dstRoot string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".cbz") {
			continue
		}
		rel := RenderName(f.Scheme, series, e.Name())
		dst := filepath.Join(dstRoot, rel)

		// Idempotency: destination already exists → skip.
		if _, err := os.Stat(dst); err == nil {
			continue
		}

		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}

		src := filepath.Join(srcDir, e.Name())
		if err := f.place(src, dst); err != nil {
			return err
		}
	}
	return nil
}

// place performs the actual file operation for a single chapter.
//
// Hardlink mode: attempts os.Link; if it fails for any reason (including
// cross-device / EXDEV on Linux), falls back to a byte-copy and logs a
// warning so the operator knows a hardlink was not possible.
//
// Move mode: os.Rename; cross-device moves are handled by the OS on most
// platforms, but will fail on Linux across mount points — the caller is
// responsible for ensuring move-mode is only configured within a single FS.
//
// Copy mode: always does a byte-copy.
func (f *Filer) place(src, dst string) error {
	switch f.Mode {
	case model.ModeMove:
		return os.Rename(src, dst)
	case model.ModeCopy:
		return copyFile(src, dst)
	default: // ModeHardlink (and any unrecognised value)
		if err := os.Link(src, dst); err != nil {
			// Cross-device link (syscall.EXDEV) or any other link failure:
			// fall back to copy and surface a warning so the operator knows.
			log.Printf("filer: hardlink %s → %s failed (%v); falling back to copy", src, dst, err)
			// If the copy also fails, return the copy error (original link
			// error is already logged above).
			return copyFile(src, dst)
		}
		return nil
	}
}

// copyFile copies src to dst atomically enough for our use-case:
// it writes to dst directly (no temp+rename) since the idempotency check
// above ensures dst does not exist yet.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		// If copy fails partway through, clean up the incomplete destination
		// so that idempotency still holds on the next run.
		if err != nil {
			_ = os.Remove(dst)
		}
	}()

	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// isNotExist is a helper that also handles the wrapped errors that os.Stat
// can return in some edge cases.
func isNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
