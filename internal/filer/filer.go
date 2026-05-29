package filer

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gavinmcfall/mangarr/internal/model"
)

// knownTokens is the set of substitution tokens recognised by RenderName.
var knownTokens = []string{"{series}", "{chapter}"}

// tokenRe matches any {…} placeholder in a scheme string.
var tokenRe = regexp.MustCompile(`\{[^}]+\}`)

// ValidateScheme checks that scheme is safe and well-formed before it is
// persisted. It returns a non-nil error for any of the following conditions:
//
//   - scheme is empty
//   - scheme does not contain {series}
//   - scheme does not contain {chapter}
//   - scheme contains an unrecognised {token}
//   - scheme contains ".." (path traversal)
//   - scheme contains "\" (backslash — Windows-style paths are not supported)
//   - scheme starts with "/" (absolute paths are not allowed)
//   - scheme contains a NUL byte
//   - after rendering with fixed sample values, the resulting path escapes
//     the library root when joined to a fake root "/lib"
func ValidateScheme(scheme string) error {
	if scheme == "" {
		return fmt.Errorf("rename scheme must not be empty")
	}

	// Structural safety checks on the raw scheme text.
	if strings.Contains(scheme, "..") {
		return fmt.Errorf("rename scheme must not contain \"..\"")
	}
	if strings.Contains(scheme, `\`) {
		return fmt.Errorf("rename scheme must not contain backslashes")
	}
	if strings.HasPrefix(scheme, "/") {
		return fmt.Errorf("rename scheme must not start with \"/\"")
	}
	if strings.ContainsRune(scheme, 0) {
		return fmt.Errorf("rename scheme must not contain NUL bytes")
	}

	// Required token checks.
	if !strings.Contains(scheme, "{series}") {
		return fmt.Errorf("rename scheme must contain {series}")
	}
	if !strings.Contains(scheme, "{chapter}") {
		return fmt.Errorf("rename scheme must contain {chapter}")
	}

	// Unknown token check — first unknown token wins.
	for _, tok := range tokenRe.FindAllString(scheme, -1) {
		known := false
		for _, k := range knownTokens {
			if tok == k {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("unknown token %s", tok)
		}
	}

	// Belt-and-braces: render with fixed sample values and verify the
	// resulting path stays inside a fake library root.
	rendered := RenderName(scheme, "Berserk", "Ch. 350.cbz")
	fakeRoot := "/lib"
	joined := filepath.Join(fakeRoot, rendered)
	cleanRoot := filepath.Clean(fakeRoot) + string(os.PathSeparator)
	if !strings.HasPrefix(filepath.Clean(joined)+string(os.PathSeparator), cleanRoot) {
		return fmt.Errorf("rendered path escapes library root")
	}

	return nil
}

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

		// Security: the series title comes from untrusted ComicInfo.xml.
		// filepath.Join cleans embedded "../" segments, so a crafted title
		// like "../../../../etc/cron.d" would escape the library root.
		// Reject any rendered path that does not stay under dstRoot.
		cleanRoot := filepath.Clean(dstRoot) + string(os.PathSeparator)
		if !strings.HasPrefix(filepath.Clean(dst)+string(os.PathSeparator), cleanRoot) {
			return fmt.Errorf("filer: rendered path %q escapes library root %q", dst, dstRoot)
		}

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

// copyFile copies src to dst. It uses a named return (err) so the cleanup
// closure observes failures from BOTH io.Copy and out.Close(): if the copy
// or the final flush/close fails, the incomplete destination is removed so
// that idempotency still holds on the next run (a half-written .cbz must not
// be mistaken for "already filed" via the os.Stat skip in File).
//
// It writes to dst directly (no temp+rename) since the idempotency check in
// File ensures dst does not exist yet.
func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() // source is closed regardless of outcome

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(dst)
		}
	}()

	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	// Assign to the named return so a Close failure triggers cleanup above.
	err = out.Close()
	return err
}

