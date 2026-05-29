package poller

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/recyclebin"
)

// fakes

type fakeClassifier struct {
	t   model.ContentType
	err error
}

func (f fakeClassifier) Classify(string) (model.ContentType, error) { return f.t, f.err }

type fakeScanner struct{ out []model.Series }

func (f fakeScanner) ScanAll() ([]model.Series, error) { return f.out, nil }

// recorder satisfies Filer, Kavita, UnmatchedSink, and ActivityWriter.
// errFile / errScan let individual tests inject failures into those code paths.
type recorder struct {
	filed     []model.Series
	scanned   []int64
	unmatched []model.Series
	activity  []model.ActivityEntry

	errFile error
	errScan error
}

func (r *recorder) File(s model.Series, dstRoot string) error {
	if r.errFile != nil {
		return r.errFile
	}
	r.filed = append(r.filed, s)
	return nil
}

func (r *recorder) ScanLibrary(libID int64) error {
	if r.errScan != nil {
		return r.errScan
	}
	r.scanned = append(r.scanned, libID)
	return nil
}

func (r *recorder) MarkUnmatched(s model.Series) error {
	r.unmatched = append(r.unmatched, s)
	return nil
}

func (r *recorder) AddActivity(e model.ActivityEntry) error {
	r.activity = append(r.activity, e)
	return nil
}

// countActions returns how many activity entries match the given action.
func (r *recorder) countActions(a model.ActivityAction) int {
	n := 0
	for _, e := range r.activity {
		if e.Action == a {
			n++
		}
	}
	return n
}

// ----- behavior tests -----

func TestRunOnceFilesAndScans(t *testing.T) {
	s := model.Series{Title: "Solo Leveling", SourcePath: "/dl/Solo Leveling"}
	rec := &recorder{}
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{s}},
		Classifier: fakeClassifier{t: model.TypeManhwa},
		Filer:      rec,
		Kavita:     rec,
		Unmatched:  rec,
		Activity:   rec,
		LibraryRoots: map[model.ContentType]string{
			model.TypeManhwa: filepath.FromSlash("/lib/Manhwa"),
		},
		LibraryIDs: map[model.ContentType]int64{model.TypeManhwa: 2},
	}
	if err := p.RunOnce(); err != nil {
		t.Fatalf("runonce: %v", err)
	}
	if len(rec.filed) != 1 || rec.filed[0].Type != model.TypeManhwa {
		t.Fatalf("expected one manhwa filed, got %+v", rec.filed)
	}
	if len(rec.scanned) != 1 || rec.scanned[0] != 2 {
		t.Fatalf("expected scan of lib 2, got %v", rec.scanned)
	}
}

func TestRunOnceUnmatchedWhenUnknown(t *testing.T) {
	rec := &recorder{}
	p := &Poller{
		Scanner:      fakeScanner{out: []model.Series{{Title: "???"}}},
		Classifier:   fakeClassifier{t: model.TypeUnknown},
		Filer:        rec,
		Kavita:       rec,
		Unmatched:    rec,
		Activity:     rec,
		LibraryRoots: map[model.ContentType]string{},
		LibraryIDs:   map[model.ContentType]int64{},
	}
	if err := p.RunOnce(); err != nil {
		t.Fatalf("runonce: %v", err)
	}
	if len(rec.unmatched) != 1 || len(rec.filed) != 0 {
		t.Fatalf("expected unmatched, got filed=%v unmatched=%v", rec.filed, rec.unmatched)
	}
}

// TestRunOnceDeduplicatesKavitaScan: two series of the same type → exactly one Kavita scan.
func TestRunOnceDeduplicatesKavitaScan(t *testing.T) {
	series := []model.Series{
		{Title: "Solo Leveling", SourcePath: "/dl/Solo Leveling"},
		{Title: "Tower of God", SourcePath: "/dl/Tower of God"},
	}
	rec := &recorder{}
	p := &Poller{
		Scanner:    fakeScanner{out: series},
		Classifier: fakeClassifier{t: model.TypeManhwa},
		Filer:      rec,
		Kavita:     rec,
		Unmatched:  rec,
		Activity:   rec,
		LibraryRoots: map[model.ContentType]string{
			model.TypeManhwa: filepath.FromSlash("/lib/Manhwa"),
		},
		LibraryIDs: map[model.ContentType]int64{model.TypeManhwa: 5},
	}
	if err := p.RunOnce(); err != nil {
		t.Fatalf("runonce: %v", err)
	}
	if len(rec.filed) != 2 {
		t.Fatalf("expected 2 filed, got %d", len(rec.filed))
	}
	if len(rec.scanned) != 1 {
		t.Fatalf("expected exactly 1 kavita scan (deduped), got %v", rec.scanned)
	}
}

// TestRunOnceNoKavitaWhenNoLibraryID: known type with no LibraryIDs entry →
// file but no Kavita scan, no error.
func TestRunOnceNoKavitaWhenNoLibraryID(t *testing.T) {
	s := model.Series{Title: "Berserk", SourcePath: "/dl/Berserk"}
	rec := &recorder{}
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{s}},
		Classifier: fakeClassifier{t: model.TypeManga},
		Filer:      rec,
		Kavita:     rec,
		Unmatched:  rec,
		Activity:   rec,
		LibraryRoots: map[model.ContentType]string{
			model.TypeManga: filepath.FromSlash("/lib/Manga"),
		},
		LibraryIDs: map[model.ContentType]int64{}, // no entry for Manga
	}
	if err := p.RunOnce(); err != nil {
		t.Fatalf("runonce: %v", err)
	}
	if len(rec.filed) != 1 {
		t.Fatalf("expected 1 filed, got %d", len(rec.filed))
	}
	if len(rec.scanned) != 0 {
		t.Fatalf("expected 0 kavita scans, got %v", rec.scanned)
	}
	if rec.countActions(model.ActionScanTriggered) != 0 {
		t.Fatalf("expected 0 scan-triggered activities, got %d", rec.countActions(model.ActionScanTriggered))
	}
}

// ----- activity-recording tests -----

func TestRunOnceRecordsFiledActivity(t *testing.T) {
	rec := &recorder{}
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{{Title: "Solo Leveling"}}},
		Classifier: fakeClassifier{t: model.TypeManhwa},
		Filer:      rec, Kavita: rec, Unmatched: rec, Activity: rec,
		LibraryRoots: map[model.ContentType]string{
			model.TypeManhwa: filepath.FromSlash("/lib/Manhwa"),
		},
		LibraryIDs: map[model.ContentType]int64{model.TypeManhwa: 2},
	}
	if err := p.RunOnce(); err != nil {
		t.Fatalf("runonce: %v", err)
	}
	if got := rec.countActions(model.ActionFiled); got != 1 {
		t.Fatalf("expected 1 ActionFiled, got %d (activity=%+v)", got, rec.activity)
	}
	// Find the ActionFiled entry and assert SeriesTitle is correct.
	var filed *model.ActivityEntry
	for i := range rec.activity {
		if rec.activity[i].Action == model.ActionFiled {
			filed = &rec.activity[i]
		}
	}
	if filed == nil || filed.SeriesTitle != "Solo Leveling" {
		t.Fatalf("ActionFiled entry missing or wrong title: %+v", filed)
	}
}

func TestRunOnceRecordsUnmatchedActivity(t *testing.T) {
	rec := &recorder{}
	p := &Poller{
		Scanner:      fakeScanner{out: []model.Series{{Title: "Unknown Series"}}},
		Classifier:   fakeClassifier{t: model.TypeUnknown},
		Filer:        rec, Kavita: rec, Unmatched: rec, Activity: rec,
		LibraryRoots: map[model.ContentType]string{},
		LibraryIDs:   map[model.ContentType]int64{},
	}
	if err := p.RunOnce(); err != nil {
		t.Fatalf("runonce: %v", err)
	}
	if got := rec.countActions(model.ActionUnmatched); got != 1 {
		t.Fatalf("expected 1 ActionUnmatched, got %d (activity=%+v)", got, rec.activity)
	}
	if got := rec.countActions(model.ActionFiled); got != 0 {
		t.Fatalf("expected 0 ActionFiled, got %d", got)
	}
}

func TestRunOnceRecordsScanTriggeredActivity(t *testing.T) {
	rec := &recorder{}
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{{Title: "Solo Leveling"}}},
		Classifier: fakeClassifier{t: model.TypeManhwa},
		Filer:      rec, Kavita: rec, Unmatched: rec, Activity: rec,
		LibraryRoots: map[model.ContentType]string{
			model.TypeManhwa: filepath.FromSlash("/lib/Manhwa"),
		},
		LibraryIDs: map[model.ContentType]int64{model.TypeManhwa: 7},
	}
	if err := p.RunOnce(); err != nil {
		t.Fatalf("runonce: %v", err)
	}
	if got := rec.countActions(model.ActionScanTriggered); got != 1 {
		t.Fatalf("expected 1 ActionScanTriggered, got %d (activity=%+v)", got, rec.activity)
	}
}

func TestRunOnceRecordsErrorOnFilerFailure(t *testing.T) {
	rec := &recorder{errFile: errors.New("disk full")}
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{{Title: "Solo Leveling"}}},
		Classifier: fakeClassifier{t: model.TypeManhwa},
		Filer:      rec, Kavita: rec, Unmatched: rec, Activity: rec,
		LibraryRoots: map[model.ContentType]string{
			model.TypeManhwa: filepath.FromSlash("/lib/Manhwa"),
		},
		LibraryIDs: map[model.ContentType]int64{model.TypeManhwa: 2},
	}
	if err := p.RunOnce(); err != nil {
		t.Fatalf("runonce: %v", err)
	}
	if got := rec.countActions(model.ActionError); got != 1 {
		t.Fatalf("expected 1 ActionError, got %d (activity=%+v)", got, rec.activity)
	}
	if got := rec.countActions(model.ActionFiled); got != 0 {
		t.Fatalf("expected 0 ActionFiled on filer failure, got %d", got)
	}
	if len(rec.scanned) != 0 {
		t.Fatalf("expected no Kavita scan on filer failure, got %v", rec.scanned)
	}
}

// TestRunOnceRecordsErrorOnKavitaFailure proves Fix 3: a Kavita failure must
// NOT poison the dedup map. The second same-type series in this tick MUST
// retry the scan (so a transient blip is recoverable within one tick).
func TestRunOnceRecordsErrorOnKavitaFailure(t *testing.T) {
	rec := &recorder{errScan: errors.New("kavita 502")}
	series := []model.Series{
		{Title: "Solo Leveling", SourcePath: "/dl/Solo Leveling"},
		{Title: "Tower of God", SourcePath: "/dl/Tower of God"},
	}
	p := &Poller{
		Scanner:    fakeScanner{out: series},
		Classifier: fakeClassifier{t: model.TypeManhwa},
		Filer:      rec, Kavita: rec, Unmatched: rec, Activity: rec,
		LibraryRoots: map[model.ContentType]string{
			model.TypeManhwa: filepath.FromSlash("/lib/Manhwa"),
		},
		LibraryIDs: map[model.ContentType]int64{model.TypeManhwa: 9},
	}
	if err := p.RunOnce(); err != nil {
		t.Fatalf("runonce: %v", err)
	}
	// Both series filed.
	if len(rec.filed) != 2 {
		t.Fatalf("expected 2 filed, got %d", len(rec.filed))
	}
	// Both Kavita attempts failed → none in scanned slice.
	if len(rec.scanned) != 0 {
		t.Fatalf("Kavita injected errScan but scanned slice has entries: %v", rec.scanned)
	}
	// Fix 3 proof: BOTH series must have attempted the scan (= 2 ActionError
	// entries for kavita), which only happens if the first failure did NOT
	// poison scanned[id].
	if got := rec.countActions(model.ActionError); got != 2 {
		t.Fatalf("expected 2 ActionError (one per failed scan attempt), got %d (activity=%+v)", got, rec.activity)
	}
	if got := rec.countActions(model.ActionScanTriggered); got != 0 {
		t.Fatalf("expected 0 ActionScanTriggered on failure, got %d", got)
	}
}

// TestRunOnceCallsGCWhenBinPresent: when RecycleBin is set, RunOnce must call
// GC at the end of the tick. We verify this by seeding the bin with an
// expired file and asserting it has been removed after RunOnce returns.
func TestRunOnceCallsGCWhenBinPresent(t *testing.T) {
	tmp := t.TempDir()
	binRoot := filepath.Join(tmp, "bin")

	// Seed an old file that is 8 days in the past (beyond the 7-day retention).
	oldDate := time.Now().AddDate(0, 0, -8).Format("2006-01-02")
	oldDir := filepath.Join(binRoot, oldDate)
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldFile := filepath.Join(oldDir, "old.cbz")
	if err := os.WriteFile(oldFile, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	bin := &recyclebin.Bin{Root: binRoot, Retention: 7 * 24 * time.Hour}
	rec := &recorder{}
	p := &Poller{
		Scanner:      fakeScanner{out: []model.Series{}},
		Classifier:   fakeClassifier{t: model.TypeUnknown},
		Filer:        rec,
		Kavita:       rec,
		Unmatched:    rec,
		Activity:     rec,
		LibraryRoots: map[model.ContentType]string{},
		LibraryIDs:   map[model.ContentType]int64{},
		RecycleBin:   bin,
	}

	if err := p.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// GC must have run and removed the expired file.
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Fatalf("expected old bin file to be GC'd after RunOnce, stat err=%v", err)
	}
}

// TestRunOnceMissingLibraryRootIsActionError: a known type with no
// LibraryRoots entry is a Settings misconfiguration, NOT a classification
// ambiguity. It must produce ActionError (not Unmatched) so the operator
// fixes Settings rather than reclassifying in a loop.
func TestRunOnceMissingLibraryRootIsActionError(t *testing.T) {
	rec := &recorder{}
	p := &Poller{
		Scanner:      fakeScanner{out: []model.Series{{Title: "Vinland Saga"}}},
		Classifier:   fakeClassifier{t: model.TypeManga},
		Filer:        rec, Kavita: rec, Unmatched: rec, Activity: rec,
		LibraryRoots: map[model.ContentType]string{}, // no root configured for Manga
		LibraryIDs:   map[model.ContentType]int64{},
	}
	if err := p.RunOnce(); err != nil {
		t.Fatalf("runonce: %v", err)
	}
	if got := rec.countActions(model.ActionError); got != 1 {
		t.Fatalf("expected 1 ActionError for missing library root, got %d (activity=%+v)", got, rec.activity)
	}
	if len(rec.unmatched) != 0 {
		t.Fatalf("missing library root must NOT route to Unmatched (would loop); got %v", rec.unmatched)
	}
	if got := rec.countActions(model.ActionUnmatched); got != 0 {
		t.Fatalf("expected 0 ActionUnmatched for misconfig, got %d", got)
	}
	if len(rec.filed) != 0 {
		t.Fatalf("expected no filing for misconfig, got %v", rec.filed)
	}
}
