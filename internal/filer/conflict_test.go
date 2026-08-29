package filer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/model"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const chScheme = "{series}/{series} - Ch.{chapter}.cbz"
const volScheme = "{series}/{series} - Vol.{volume}.cbz"

// Two source files that render to the same destination: the first is filed,
// the second is reported as a conflict and never written. Non-conflicting
// files in the same walk are still filed.
func TestFileReportsSameDstConflict(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "dl", "Dragon Ball")
	dstRoot := filepath.Join(tmp, "lib", "Manga")
	writeFile(t, filepath.Join(src, "Official_1.cbz"), "db-1")
	writeFile(t, filepath.Join(src, "Official_Z 1.cbz"), "z-1")
	writeFile(t, filepath.Join(src, "Official_2.cbz"), "db-2")

	f := &Filer{Mode: model.ModeHardlink, Scheme: chScheme, VolumeScheme: volScheme}
	err := f.File("Dragon Ball", src, dstRoot)
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *ConflictError, got %v", err)
	}
	if len(ce.Conflicts) != 1 {
		t.Fatalf("want 1 conflict, got %d: %+v", len(ce.Conflicts), ce.Conflicts)
	}
	c := ce.Conflicts[0]
	if filepath.Base(c.Src) != "Official_Z 1.cbz" || filepath.Base(c.ClaimedBy) != "Official_1.cbz" {
		t.Fatalf("unexpected conflict %+v", c)
	}
	// Filed: Ch.1 (from Official_1) and Ch.2. Nothing overwritten.
	got, _ := os.ReadFile(filepath.Join(dstRoot, "Dragon Ball", "Dragon Ball - Ch.1.cbz"))
	if string(got) != "db-1" {
		t.Fatalf("Ch.1 should be the first claimant's content, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(dstRoot, "Dragon Ball", "Dragon Ball - Ch.2.cbz")); err != nil {
		t.Fatalf("non-conflicting chapter should still be filed: %v", err)
	}
}

// An existing destination that is NOT the same inode as the source (hardlink
// mode) is a conflict — this is the field case where a previous run filed a
// different source file under the same name.
func TestFileHardlinkExistingDifferentInodeIsConflict(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "dl", "S")
	dstRoot := filepath.Join(tmp, "lib", "Manga")
	writeFile(t, filepath.Join(src, "Ch. 1.cbz"), "new")
	writeFile(t, filepath.Join(dstRoot, "S", "S - Ch.1.cbz"), "old-unrelated")

	f := &Filer{Mode: model.ModeHardlink, Scheme: chScheme, VolumeScheme: volScheme}
	err := f.File("S", src, dstRoot)
	var ce *ConflictError
	if !errors.As(err, &ce) || len(ce.Conflicts) != 1 {
		t.Fatalf("expected one conflict, got %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dstRoot, "S", "S - Ch.1.cbz"))
	if string(got) != "old-unrelated" {
		t.Fatalf("existing destination must not be touched, got %q", got)
	}
}

// Same inode (a previous hardlink of this very file) is the idempotent skip.
func TestFileHardlinkExistingSameInodeIsSkip(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "dl", "S")
	dstRoot := filepath.Join(tmp, "lib", "Manga")
	writeFile(t, filepath.Join(src, "Ch. 1.cbz"), "x")
	f := &Filer{Mode: model.ModeHardlink, Scheme: chScheme, VolumeScheme: volScheme}
	if err := f.File("S", src, dstRoot); err != nil {
		t.Fatal(err)
	}
	if err := f.File("S", src, dstRoot); err != nil {
		t.Fatalf("second run must be a clean skip, got %v", err)
	}
}

// Copy mode has no inode to compare: an existing destination stays a skip.
func TestFileCopyModeExistingDstIsSkip(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "dl", "S")
	dstRoot := filepath.Join(tmp, "lib", "Manga")
	writeFile(t, filepath.Join(src, "Ch. 1.cbz"), "new")
	writeFile(t, filepath.Join(dstRoot, "S", "S - Ch.1.cbz"), "old")
	f := &Filer{Mode: model.ModeCopy, Scheme: chScheme, VolumeScheme: volScheme}
	if err := f.File("S", src, dstRoot); err != nil {
		t.Fatalf("copy mode existing dst should skip, got %v", err)
	}
}

// Volume files are filed under the volume scheme, chapters under the chapter
// scheme, into the same series directory.
func TestFileVolumesUseVolumeScheme(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "dl", "Dragon Ball Z (Color Edition)")
	dstRoot := filepath.Join(tmp, "lib", "Manga")
	writeFile(t, filepath.Join(src, "Dragon Ball Z - Vol. 001 (English - Colour).cbz"), "v1")
	writeFile(t, filepath.Join(src, "Dragon Ball Z - Vol. 026 (English - Colour).cbz"), "v26")
	writeFile(t, filepath.Join(src, "Extra Ch. 3.cbz"), "c3")

	f := &Filer{Mode: model.ModeHardlink, Scheme: chScheme, VolumeScheme: volScheme}
	if err := f.File("Dragon Ball Z (Color Edition)", src, dstRoot); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(dstRoot, "Dragon Ball Z (Color Edition)")
	for _, want := range []string{
		"Dragon Ball Z (Color Edition) - Vol.001.cbz",
		"Dragon Ball Z (Color Edition) - Vol.026.cbz",
		"Dragon Ball Z (Color Edition) - Ch.3.cbz",
	} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("expected %s: %v", want, err)
		}
	}
}

// An empty VolumeScheme falls back to the chapter scheme (pre-feature behaviour).
func TestFileEmptyVolumeSchemeFallsBack(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "dl", "S")
	dstRoot := filepath.Join(tmp, "lib", "Manga")
	writeFile(t, filepath.Join(src, "S - Vol. 002.cbz"), "v2")
	f := &Filer{Mode: model.ModeHardlink, Scheme: chScheme}
	if err := f.File("S", src, dstRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dstRoot, "S", "S - Ch.002.cbz")); err != nil {
		t.Fatalf("fallback should render under chapter scheme: %v", err)
	}
}

// Plan mirrors File: conflicts show up as PlanConflict, nothing is written.
func TestPlanReportsConflicts(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "dl", "Dragon Ball")
	dstRoot := filepath.Join(tmp, "lib", "Manga")
	writeFile(t, filepath.Join(src, "Official_1.cbz"), "db-1")
	writeFile(t, filepath.Join(src, "Official_Z 1.cbz"), "z-1")
	writeFile(t, filepath.Join(src, "Vol. 3.cbz"), "v3")

	f := &Filer{Mode: model.ModeHardlink, Scheme: chScheme, VolumeScheme: volScheme}
	plans, err := f.Plan("Dragon Ball", src, dstRoot)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]PlannedAction{}
	for _, p := range plans {
		actions[filepath.Base(p.SrcPath)] = p.Action
		if p.Action == PlanConflict && p.Error == "" {
			t.Errorf("conflict entry must carry an explanation: %+v", p)
		}
		if p.Action == PlanFile && filepath.Base(p.SrcPath) == "Vol. 3.cbz" && filepath.Base(p.DstPath) != "Dragon Ball - Vol.3.cbz" {
			t.Errorf("volume plan should use the volume scheme, got %s", p.DstPath)
		}
	}
	if actions["Official_1.cbz"] != PlanFile || actions["Official_Z 1.cbz"] != PlanConflict || actions["Vol. 3.cbz"] != PlanFile {
		t.Fatalf("unexpected plan actions: %v", actions)
	}
	if entries, _ := os.ReadDir(dstRoot); len(entries) != 0 {
		t.Fatal("Plan must not write")
	}
}
