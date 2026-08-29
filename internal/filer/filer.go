package filer

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/recyclebin"
)

// Substitution tokens recognised by the two rename schemes. A chapter scheme
// may use {series} and {chapter}; a volume scheme may use {series} and
// {volume}. Neither may use the other's number token.
var (
	knownChapterTokens = []string{"{series}", "{chapter}"}
	knownVolumeTokens  = []string{"{series}", "{volume}"}
)

// tokenRe matches any {…} placeholder in a scheme string.
var tokenRe = regexp.MustCompile(`\{[^}]+\}`)

// ValidateScheme checks that a CHAPTER scheme is safe and well-formed before
// it is persisted. It returns a non-nil error for any of the following:
//
//   - scheme is empty
//   - scheme does not contain {series}
//   - scheme does not contain {chapter}
//   - scheme contains an unrecognised {token} (including {volume})
//   - scheme contains ".." (path traversal)
//   - scheme contains "\" (backslash — Windows-style paths are not supported)
//   - scheme starts with "/" (absolute paths are not allowed)
//   - scheme contains a NUL byte
//   - after rendering with fixed sample values, the resulting path escapes
//     the library root when joined to a fake root "/lib"
func ValidateScheme(scheme string) error {
	// {volume} in a chapter scheme surfaces through the unknown-token check
	// (message unchanged from before volume support existed).
	return validateScheme("rename scheme", scheme, "{chapter}", "", knownChapterTokens,
		func(s string) string { return RenderName(s, "Berserk", "Ch. 350.cbz") })
}

// ValidateVolumeScheme applies the same rules as ValidateScheme to a VOLUME
// scheme: {series} and {volume} are required, {chapter} is rejected.
func ValidateVolumeScheme(scheme string) error {
	return validateScheme("volume rename scheme", scheme, "{volume}", "{chapter}", knownVolumeTokens,
		func(s string) string { return RenderVolumeName(s, "Berserk", "Vol. 3.cbz") })
}

// ValidateSchemePair checks that the chapter and volume schemes render into
// the SAME series directory. Poller.ResolveLibraryDir derives a series'
// library folder from the chapter scheme alone, so a volume scheme that
// files into a different directory would make that folder wrong.
func ValidateSchemePair(chapterScheme, volumeScheme string) error {
	if err := ValidateScheme(chapterScheme); err != nil {
		return err
	}
	if err := ValidateVolumeScheme(volumeScheme); err != nil {
		return err
	}
	chDir := filepath.Dir(filepath.FromSlash(RenderName(chapterScheme, "Berserk", "Ch. 350.cbz")))
	volDir := filepath.Dir(filepath.FromSlash(RenderVolumeName(volumeScheme, "Berserk", "Vol. 3.cbz")))
	if chDir != volDir {
		return fmt.Errorf("rename scheme and volume rename scheme must render into the same directory (%q vs %q)", chDir, volDir)
	}
	return nil
}

func validateScheme(label, scheme, required, forbidden string, known []string, sample func(string) string) error {
	if scheme == "" {
		return fmt.Errorf("%s must not be empty", label)
	}

	// Structural safety checks on the raw scheme text.
	if strings.Contains(scheme, "..") {
		return fmt.Errorf("%s must not contain \"..\"", label)
	}
	if strings.Contains(scheme, `\`) {
		return fmt.Errorf("%s must not contain backslashes", label)
	}
	if strings.HasPrefix(scheme, "/") {
		return fmt.Errorf("%s must not start with \"/\"", label)
	}
	if strings.ContainsRune(scheme, 0) {
		return fmt.Errorf("%s must not contain NUL bytes", label)
	}

	// Required / forbidden token checks.
	if !strings.Contains(scheme, "{series}") {
		return fmt.Errorf("%s must contain {series}", label)
	}
	if !strings.Contains(scheme, required) {
		return fmt.Errorf("%s must contain %s", label, required)
	}
	if forbidden != "" && strings.Contains(scheme, forbidden) {
		return fmt.Errorf("%s must not contain %s", label, forbidden)
	}

	// Unknown token check — first unknown token wins.
	for _, tok := range tokenRe.FindAllString(scheme, -1) {
		isKnown := false
		for _, k := range known {
			if tok == k {
				isKnown = true
				break
			}
		}
		if !isKnown {
			return fmt.Errorf("unknown token %s", tok)
		}
	}

	// Belt-and-braces: render with fixed sample values and verify the
	// resulting path stays inside a fake library root.
	rendered := sample(scheme)
	fakeRoot := "/lib"
	joined := filepath.Join(fakeRoot, rendered)
	cleanRoot := filepath.Clean(fakeRoot) + string(os.PathSeparator)
	if !strings.HasPrefix(filepath.Clean(joined)+string(os.PathSeparator), cleanRoot) {
		return fmt.Errorf("rendered path escapes library root")
	}

	return nil
}

// Number extraction.
//
// chapterMarker: a number introduced by "ch"/"chapter" at a word boundary
// ("Ch. 001", "Chapter 42", "Vol.1 Ch.3" → 3). "Chainsaw" and "Witch" do
// not qualify: the marker must be followed by an optional dot, optional
// whitespace and then digits.
//
// volumeMarker: the same shape for "vol"/"volume" ("Vol. 001", "Volume 3").
// Deliberately narrow — "v01" is not treated as a volume, and "vol" inside a
// word ("Evolution") is not a marker.
//
// firstNumber: the pre-existing fallback — the first integer or decimal in
// the name.
var (
	chapterMarker = regexp.MustCompile(`(?i)\bch(?:apter)?\.?\s*(\d+(?:\.\d+)?)`)
	volumeMarker  = regexp.MustCompile(`(?i)\bvol(?:ume)?\.?\s*(\d+(?:\.\d+)?)`)
	firstNumber   = regexp.MustCompile(`\d+(?:\.\d+)?`)
)

// IsVolumeFile reports whether a file name denotes a whole volume rather
// than a chapter: it carries a volume marker and NO chapter marker. A name
// with both ("Vol.1 Ch.3") is a chapter file.
func IsVolumeFile(name string) bool {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	return volumeMarker.MatchString(base) && !chapterMarker.MatchString(base)
}

// ChapterNumber extracts the {chapter} value from a file name: the number
// after a chapter marker when there is one, else the first number in the
// name, else the name without its extension. Numbers are returned exactly as
// written ("001" stays "001", "7.5" stays "7.5").
func ChapterNumber(name string) string {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	if m := chapterMarker.FindStringSubmatch(base); m != nil {
		return m[1]
	}
	if m := firstNumber.FindString(base); m != "" {
		return m
	}
	return base
}

// VolumeNumber extracts the {volume} value from a file name: the number
// after a volume marker, else the first number, else the bare name.
func VolumeNumber(name string) string {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	if m := volumeMarker.FindStringSubmatch(base); m != nil {
		return m[1]
	}
	if m := firstNumber.FindString(base); m != "" {
		return m
	}
	return base
}

// RenderName substitutes tokens in a CHAPTER scheme:
//
//	{series}  → the series title
//	{chapter} → ChapterNumber(origFile)
//
// The scheme uses "/" as the directory separator (schemes are written
// cross-platform); callers join the result onto a root with filepath.Join.
func RenderName(scheme, series, origFile string) string {
	out := strings.ReplaceAll(scheme, "{series}", series)
	out = strings.ReplaceAll(out, "{chapter}", ChapterNumber(origFile))
	return out
}

// RenderVolumeName substitutes tokens in a VOLUME scheme:
//
//	{series} → the series title
//	{volume} → VolumeNumber(origFile)
func RenderVolumeName(scheme, series, origFile string) string {
	out := strings.ReplaceAll(scheme, "{series}", series)
	out = strings.ReplaceAll(out, "{volume}", VolumeNumber(origFile))
	return out
}

// PlannedAction describes what File() WOULD do for a single file.
type PlannedAction string

const (
	PlanFile     PlannedAction = "file"     // would create dst (hardlink/move/copy)
	PlanSkip     PlannedAction = "skip"     // dst already exists, idempotent skip
	PlanConflict PlannedAction = "conflict" // dst is claimed by a DIFFERENT source file; nothing written
	PlanError    PlannedAction = "error"    // would error (path traversal, etc.)
)

// PlanEntry is one file's planned outcome.
type PlanEntry struct {
	SrcPath string
	DstPath string // empty when PlanError
	Mode    model.FileMode
	Action  PlannedAction
	Error   string // populated when Action == PlanError or PlanConflict

	// claimedBy is the file that owns DstPath when Action == PlanConflict:
	// an earlier source file in the same walk, or the existing library file.
	claimedBy string
}

// Conflict records one source file that could not be filed because its
// destination belongs to a different file.
type Conflict struct {
	Src       string // the source file that lost
	Dst       string // the destination both files render to
	ClaimedBy string // the source file (same walk) or existing library file that owns Dst
}

// ConflictError is returned by File when at least one file in the walk was a
// conflict. Every NON-conflicting file has already been filed by the time it
// is returned, so callers should treat it as a partial success: record the
// conflicts, keep the binding, still scan the library.
type ConflictError struct {
	Conflicts []Conflict
}

func (e *ConflictError) Error() string {
	if len(e.Conflicts) == 1 {
		c := e.Conflicts[0]
		return fmt.Sprintf("filer: %s conflicts with %s (both render to %s)",
			filepath.Base(c.Src), filepath.Base(c.ClaimedBy), filepath.Base(c.Dst))
	}
	return fmt.Sprintf("filer: %d files conflict with other files rendering to the same destination", len(e.Conflicts))
}

// Filer places .cbz files from a source directory into a destination library
// root using the configured mode (hardlink/move/copy) and rename schemes.
//
// Scheme is the chapter scheme (required). VolumeScheme is applied to files
// IsVolumeFile recognises; when it is empty those files fall back to Scheme
// (the pre-volume behaviour).
//
// RecycleBin is optional (nil-safe). When non-nil and move-mode encounters a
// destination that already exists, the existing file is sent to the bin
// before the rename is retried. If RecycleBin is nil, the original rename
// error is returned unchanged.
type Filer struct {
	Mode         model.FileMode
	Scheme       string
	VolumeScheme string
	RecycleBin   *recyclebin.Bin
}

// render picks the scheme for one file and returns the relative destination.
func (f *Filer) render(series, name string) string {
	if f.VolumeScheme != "" && IsVolumeFile(name) {
		return RenderVolumeName(f.VolumeScheme, series, name)
	}
	return RenderName(f.Scheme, series, name)
}

// File places every .cbz from srcDir into dstRoot per the schemes + mode.
// It is idempotent: a destination that already exists AND is the same file
// (same inode, hardlink mode) is skipped without error, so repeated runs
// (e.g. the poller on a schedule) are safe. Directories are created as
// needed.
//
// A destination claimed by a DIFFERENT file — another source file in this
// walk that renders to the same name, or an existing library file that is
// not a hardlink of the source — is a conflict: it is never written or
// overwritten. All non-conflicting files are filed first; then, if any
// conflicts were seen, File returns a *ConflictError describing them.
func (f *Filer) File(series, srcDir, dstRoot string) error {
	plans, err := f.Plan(series, srcDir, dstRoot)
	if err != nil {
		return err
	}
	var conflicts []Conflict
	for _, p := range plans {
		switch p.Action {
		case PlanError:
			return fmt.Errorf("filer: %s", p.Error)
		case PlanConflict:
			conflicts = append(conflicts, Conflict{Src: p.SrcPath, Dst: p.DstPath, ClaimedBy: p.claimedBy})
		case PlanFile:
			if err := os.MkdirAll(filepath.Dir(p.DstPath), 0o755); err != nil {
				return err
			}
			if err := f.place(p.SrcPath, p.DstPath); err != nil {
				return err
			}
		}
	}
	if len(conflicts) > 0 {
		return &ConflictError{Conflicts: conflicts}
	}
	return nil
}

// Plan walks the same logic as File() but returns the planned actions without
// touching the filesystem. Used by the dry-run preview UI and by File itself,
// so the two can never disagree.
//
// Returns one PlanEntry per .cbz under srcDir. Per-entry path-traversal
// violations and conflicts are returned as PlanError / PlanConflict entries
// rather than aborting the whole walk.
func (f *Filer) Plan(series, srcDir, dstRoot string) ([]PlanEntry, error) {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return nil, err
	}

	mode := f.Mode
	if mode == "" {
		mode = model.ModeHardlink
	}

	cleanRoot := filepath.Clean(dstRoot) + string(os.PathSeparator)
	claimed := map[string]string{} // dst → src that claimed it first in this walk

	var plans []PlanEntry
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".cbz") {
			continue
		}
		src := filepath.Join(srcDir, e.Name())
		dst := filepath.Join(dstRoot, f.render(series, e.Name()))

		// Security: the series title comes from untrusted ComicInfo.xml.
		// filepath.Join cleans embedded "../" segments, so a crafted title
		// like "../../../../etc/cron.d" would escape the library root.
		// Reject any rendered path that does not stay under dstRoot.
		if !strings.HasPrefix(filepath.Clean(dst)+string(os.PathSeparator), cleanRoot) {
			plans = append(plans, PlanEntry{
				SrcPath: src,
				Mode:    mode,
				Action:  PlanError,
				Error:   fmt.Sprintf("rendered path %q escapes library root %q", dst, dstRoot),
			})
			continue
		}

		// Conflict: another file in this walk already owns dst.
		if prev, ok := claimed[dst]; ok {
			plans = append(plans, PlanEntry{
				SrcPath: src, DstPath: dst, Mode: mode, Action: PlanConflict,
				Error:     fmt.Sprintf("%s renders to the same destination as %s", e.Name(), filepath.Base(prev)),
				claimedBy: prev,
			})
			continue
		}
		claimed[dst] = src

		// Destination already exists: idempotent skip, unless (hardlink
		// mode) it is provably a different file on the same device.
		if dstInfo, err := os.Stat(dst); err == nil {
			action := PlanSkip
			errText := ""
			if mode == model.ModeHardlink {
				if srcInfo, serr := os.Stat(src); serr == nil && !os.SameFile(srcInfo, dstInfo) && sameDevice(srcInfo, dstInfo) {
					action = PlanConflict
					errText = fmt.Sprintf("%s already exists and is not a hardlink of %s", filepath.Base(dst), e.Name())
				}
			}
			plans = append(plans, PlanEntry{
				SrcPath: src, DstPath: dst, Mode: mode, Action: action,
				Error:     errText,
				claimedBy: dst,
			})
			continue
		}

		plans = append(plans, PlanEntry{
			SrcPath: src,
			DstPath: dst,
			Mode:    mode,
			Action:  PlanFile,
		})
	}
	return plans, nil
}

// place performs the actual file operation for a single file.
//
// Hardlink mode: attempts os.Link; if it fails for any reason (including
// cross-device / EXDEV on Linux), falls back to a byte-copy and logs a
// warning so the operator knows a hardlink was not possible.
//
// Move mode: os.Rename. If the rename fails because the destination already
// exists and RecycleBin is non-nil, the existing destination is sent to the
// bin and the rename is retried once. If RecycleBin is nil, the original
// rename error is returned. Cross-device moves are handled by the OS on most
// platforms, but will fail on Linux across mount points — the caller is
// responsible for ensuring move-mode is only configured within a single FS.
//
// Copy mode: always does a byte-copy.
func (f *Filer) place(src, dst string) error {
	switch f.Mode {
	case model.ModeMove:
		err := os.Rename(src, dst)
		if err == nil {
			return nil
		}
		// If dst already exists and we have a recycle bin, send the old dst to
		// the bin and retry. This path is currently unreachable in normal
		// operation (Plan's idempotent skip prevents it) but makes the failure
		// mode safe if any future change removes the skip.
		if _, statErr := os.Stat(dst); statErr == nil && f.RecycleBin != nil {
			if _, binErr := f.RecycleBin.Send(dst, time.Now()); binErr != nil {
				log.Printf("filer: recyclebin send %s failed (%v); returning original rename error", dst, binErr)
				return err
			}
			return os.Rename(src, dst)
		}
		return err
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
// be mistaken for "already filed" via the os.Stat skip in Plan).
//
// It writes to dst directly (no temp+rename) since the idempotency check in
// Plan ensures dst does not exist yet.
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
