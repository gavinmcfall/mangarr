package poller

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gavinmcfall/mangarr/internal/classifier"
	"github.com/gavinmcfall/mangarr/internal/filer"
	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/recyclebin"
	"github.com/gavinmcfall/mangarr/internal/suwayomi"
)

// fakeBindingStore satisfies poller.BindingLister for v2 tests.
type fakeBindingStore struct {
	bindings []model.Binding
	err      error
}

func (f *fakeBindingStore) ListBindings() ([]model.Binding, error) {
	return f.bindings, f.err
}

// fakeMetrics records MetricsSink calls so tests can assert call counts.
type fakeMetrics struct {
	filesFiled  map[string]int
	kavitaScans map[string]int
	unmatched   int
	fileErrors  int
	lastRunSet  bool
}

func newFakeMetrics() *fakeMetrics {
	return &fakeMetrics{
		filesFiled:  make(map[string]int),
		kavitaScans: make(map[string]int),
	}
}

func (f *fakeMetrics) IncFilesFiled(category string) { f.filesFiled[category]++ }
func (f *fakeMetrics) IncKavitaScan(result string)   { f.kavitaScans[result]++ }
func (f *fakeMetrics) IncUnmatched()                 { f.unmatched++ }
func (f *fakeMetrics) IncFileError()                 { f.fileErrors++ }
func (f *fakeMetrics) SetPollerLastRun(_ time.Time)  { f.lastRunSet = true }

// ----- fakes -----

// fakeClassifier returns a fixed Decision (or error) on every Classify call.
// Tests that exercise per-series decisions use fakeClassifierMap below.
type fakeClassifier struct {
	decision model.Decision
	err      error
	calls    int
}

func (f *fakeClassifier) Classify(_ context.Context, _ classifier.ScanItem) (model.Decision, error) {
	f.calls++
	return f.decision, f.err
}

// fakeClassifierMap maps a series title to a Decision (or err). Used by
// Preview tests where each input series should classify differently.
type fakeClassifierMap struct {
	decisions map[string]model.Decision
	errors    map[string]error
}

func (f *fakeClassifierMap) Classify(_ context.Context, item classifier.ScanItem) (model.Decision, error) {
	if err, ok := f.errors[item.Title]; ok {
		return model.Decision{Via: classifier.ViaUnmatched}, err
	}
	if d, ok := f.decisions[item.Title]; ok {
		return d, nil
	}
	return model.Decision{Via: classifier.ViaUnmatched}, nil
}

type fakeScanner struct{ out []model.Series }

func (f fakeScanner) ScanAll() ([]model.Series, error) { return f.out, nil }

// recorder satisfies Filer, Kavita, UnmatchedSink, and ActivityWriter.
// errFile / errScan let individual tests inject failures into those code paths.
type recorder struct {
	filed     []model.Series
	filedDst  []string
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
	r.filedDst = append(r.filedDst, dstRoot)
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

// ----- helpers -----

// manhwaBinding builds a single-binding store fixture so tests that used to
// configure LibraryRoots/LibraryIDs for TypeManhwa can keep the same call
// shape against the v2 BindingID routing.
func manhwaBinding(libRoot string, kavitaLibID int64) (*fakeBindingStore, model.Decision) {
	b := model.Binding{ID: 2, Name: "Manhwa", LibraryRoot: libRoot, KavitaLibID: kavitaLibID}
	return &fakeBindingStore{bindings: []model.Binding{b}}, model.Decision{BindingID: 2, Via: "anilist:KR"}
}

// mangaBinding mirrors manhwaBinding for Manga.
func mangaBinding(libRoot string, kavitaLibID int64) (*fakeBindingStore, model.Decision) {
	b := model.Binding{ID: 1, Name: "Manga", LibraryRoot: libRoot, KavitaLibID: kavitaLibID}
	return &fakeBindingStore{bindings: []model.Binding{b}}, model.Decision{BindingID: 1, Via: "anilist:JP"}
}

// ----- v2 behaviour tests -----

// TestRunOnceRoutesByBindingFromV2Classifier is the Task 10 spec test: the
// classifier returns a Decision with BindingID + Via; the poller resolves
// the binding via its BindingLister, passes Binding.LibraryRoot to the
// filer, and triggers a Kavita scan against Binding.KavitaLibID.
func TestRunOnceRoutesByBindingFromV2Classifier(t *testing.T) {
	st := &fakeBindingStore{bindings: []model.Binding{
		{ID: 7, Name: "Manga", LibraryRoot: "/dst/a", KavitaLibID: 99},
	}}
	cls := &fakeClassifier{decision: model.Decision{BindingID: 7, Via: "rule:1"}}
	rec := &recorder{}
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{{Title: "Foo", SourcePath: "/src/foo"}}},
		Classifier: cls,
		Filer:      rec,
		Kavita:     rec,
		Unmatched:  rec,
		Activity:   rec,
		Bindings:   st,
	}
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(rec.filedDst) != 1 || rec.filedDst[0] != "/dst/a" {
		t.Errorf("expected file routed to /dst/a, got %v", rec.filedDst)
	}
	if len(rec.scanned) != 1 || rec.scanned[0] != 99 {
		t.Errorf("expected Kavita scan triggered for lib 99, got %v", rec.scanned)
	}
	// Activity entry must carry the Via from the classifier.
	var sawFiled bool
	for _, e := range rec.activity {
		if e.Action == model.ActionFiled {
			sawFiled = true
			if e.Via != "rule:1" {
				t.Errorf("ActionFiled Via: want rule:1, got %q", e.Via)
			}
		}
	}
	if !sawFiled {
		t.Errorf("expected at least one ActionFiled activity entry")
	}
}

// TestRunOnceBindingNotFound covers the deleted-between-save-and-tick case:
// classifier returns BindingID 99 but the bindings table has no row 99.
// Poller must record ActionError (NOT route to a phantom location) and
// NOT call the filer.
func TestRunOnceBindingNotFound(t *testing.T) {
	st := &fakeBindingStore{bindings: []model.Binding{
		{ID: 1, Name: "Manga", LibraryRoot: "/lib/manga", KavitaLibID: 1},
	}}
	cls := &fakeClassifier{decision: model.Decision{BindingID: 99, Via: "rule:5"}}
	rec := &recorder{}
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{{Title: "Orphan", SourcePath: "/src/orphan"}}},
		Classifier: cls,
		Filer:      rec,
		Kavita:     rec,
		Unmatched:  rec,
		Activity:   rec,
		Bindings:   st,
	}
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(rec.filed) != 0 {
		t.Errorf("filer must not be called when binding not found, got %v", rec.filed)
	}
	if got := rec.countActions(model.ActionError); got != 1 {
		t.Errorf("expected 1 ActionError for missing binding, got %d (activity=%+v)", got, rec.activity)
	}
}

func TestRunOnceFilesAndScans(t *testing.T) {
	s := model.Series{Title: "Solo Leveling", SourcePath: "/dl/Solo Leveling"}
	rec := &recorder{}
	st, dec := manhwaBinding(filepath.FromSlash("/lib/Manhwa"), 2)
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{s}},
		Classifier: &fakeClassifier{decision: dec},
		Filer:      rec,
		Kavita:     rec,
		Unmatched:  rec,
		Activity:   rec,
		Bindings:   st,
	}
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("runonce: %v", err)
	}
	if len(rec.filed) != 1 {
		t.Fatalf("expected one filed, got %+v", rec.filed)
	}
	if rec.filedDst[0] != filepath.FromSlash("/lib/Manhwa") {
		t.Fatalf("expected file routed to /lib/Manhwa, got %q", rec.filedDst[0])
	}
	if len(rec.scanned) != 1 || rec.scanned[0] != 2 {
		t.Fatalf("expected scan of lib 2, got %v", rec.scanned)
	}
}

func TestRunOnceUnmatchedWhenDecisionZero(t *testing.T) {
	rec := &recorder{}
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{{Title: "???"}}},
		Classifier: &fakeClassifier{decision: model.Decision{BindingID: 0, Via: classifier.ViaUnmatched}},
		Filer:      rec,
		Kavita:     rec,
		Unmatched:  rec,
		Activity:   rec,
		Bindings:   &fakeBindingStore{},
	}
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("runonce: %v", err)
	}
	if len(rec.unmatched) != 1 || len(rec.filed) != 0 {
		t.Fatalf("expected unmatched, got filed=%v unmatched=%v", rec.filed, rec.unmatched)
	}
}

// TestRunOnceDeduplicatesKavitaScan: two series routed to the same binding →
// exactly one Kavita scan.
func TestRunOnceDeduplicatesKavitaScan(t *testing.T) {
	series := []model.Series{
		{Title: "Solo Leveling", SourcePath: "/dl/Solo Leveling"},
		{Title: "Tower of God", SourcePath: "/dl/Tower of God"},
	}
	rec := &recorder{}
	st, dec := manhwaBinding(filepath.FromSlash("/lib/Manhwa"), 5)
	p := &Poller{
		Scanner:    fakeScanner{out: series},
		Classifier: &fakeClassifier{decision: dec},
		Filer:      rec,
		Kavita:     rec,
		Unmatched:  rec,
		Activity:   rec,
		Bindings:   st,
	}
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("runonce: %v", err)
	}
	if len(rec.filed) != 2 {
		t.Fatalf("expected 2 filed, got %d", len(rec.filed))
	}
	if len(rec.scanned) != 1 {
		t.Fatalf("expected exactly 1 kavita scan (deduped), got %v", rec.scanned)
	}
}

// TestRunOnceNoKavitaWhenBindingHasZeroLibID: a binding with KavitaLibID == 0
// is filed but no Kavita scan is triggered (legitimate case for a library
// that doesn't need an external scan trigger).
func TestRunOnceNoKavitaWhenBindingHasZeroLibID(t *testing.T) {
	s := model.Series{Title: "Berserk", SourcePath: "/dl/Berserk"}
	rec := &recorder{}
	st := &fakeBindingStore{bindings: []model.Binding{
		{ID: 1, Name: "Manga", LibraryRoot: filepath.FromSlash("/lib/Manga"), KavitaLibID: 0},
	}}
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{s}},
		Classifier: &fakeClassifier{decision: model.Decision{BindingID: 1, Via: "rule:1"}},
		Filer:      rec,
		Kavita:     rec,
		Unmatched:  rec,
		Activity:   rec,
		Bindings:   st,
	}
	if err := p.RunOnce(context.Background()); err != nil {
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
	st, dec := manhwaBinding(filepath.FromSlash("/lib/Manhwa"), 2)
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{{Title: "Solo Leveling"}}},
		Classifier: &fakeClassifier{decision: dec},
		Filer:      rec, Kavita: rec, Unmatched: rec, Activity: rec,
		Bindings: st,
	}
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("runonce: %v", err)
	}
	if got := rec.countActions(model.ActionFiled); got != 1 {
		t.Fatalf("expected 1 ActionFiled, got %d (activity=%+v)", got, rec.activity)
	}
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
		Scanner:    fakeScanner{out: []model.Series{{Title: "Unknown Series"}}},
		Classifier: &fakeClassifier{decision: model.Decision{Via: classifier.ViaUnmatched}},
		Filer:      rec, Kavita: rec, Unmatched: rec, Activity: rec,
		Bindings: &fakeBindingStore{},
	}
	if err := p.RunOnce(context.Background()); err != nil {
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
	st, dec := manhwaBinding(filepath.FromSlash("/lib/Manhwa"), 7)
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{{Title: "Solo Leveling"}}},
		Classifier: &fakeClassifier{decision: dec},
		Filer:      rec, Kavita: rec, Unmatched: rec, Activity: rec,
		Bindings: st,
	}
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("runonce: %v", err)
	}
	if got := rec.countActions(model.ActionScanTriggered); got != 1 {
		t.Fatalf("expected 1 ActionScanTriggered, got %d (activity=%+v)", got, rec.activity)
	}
}

func TestRunOnceRecordsErrorOnFilerFailure(t *testing.T) {
	rec := &recorder{errFile: errors.New("disk full")}
	st, dec := manhwaBinding(filepath.FromSlash("/lib/Manhwa"), 2)
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{{Title: "Solo Leveling"}}},
		Classifier: &fakeClassifier{decision: dec},
		Filer:      rec, Kavita: rec, Unmatched: rec, Activity: rec,
		Bindings: st,
	}
	if err := p.RunOnce(context.Background()); err != nil {
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

// TestRunOnceRecordsErrorOnKavitaFailure proves a Kavita failure must NOT
// poison the dedup map. The second same-binding series in this tick MUST
// retry the scan (so a transient blip is recoverable within one tick).
func TestRunOnceRecordsErrorOnKavitaFailure(t *testing.T) {
	rec := &recorder{errScan: errors.New("kavita 502")}
	series := []model.Series{
		{Title: "Solo Leveling", SourcePath: "/dl/Solo Leveling"},
		{Title: "Tower of God", SourcePath: "/dl/Tower of God"},
	}
	st, dec := manhwaBinding(filepath.FromSlash("/lib/Manhwa"), 9)
	p := &Poller{
		Scanner:    fakeScanner{out: series},
		Classifier: &fakeClassifier{decision: dec},
		Filer:      rec, Kavita: rec, Unmatched: rec, Activity: rec,
		Bindings: st,
	}
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("runonce: %v", err)
	}
	if len(rec.filed) != 2 {
		t.Fatalf("expected 2 filed, got %d", len(rec.filed))
	}
	if len(rec.scanned) != 0 {
		t.Fatalf("Kavita injected errScan but scanned slice has entries: %v", rec.scanned)
	}
	if got := rec.countActions(model.ActionError); got != 2 {
		t.Fatalf("expected 2 ActionError (one per failed scan attempt), got %d (activity=%+v)", got, rec.activity)
	}
	if got := rec.countActions(model.ActionScanTriggered); got != 0 {
		t.Fatalf("expected 0 ActionScanTriggered on failure, got %d", got)
	}
}

// TestRunOnceCallsGCWhenBinPresent: when RecycleBin is set, RunOnce must call
// GC at the end of the tick.
func TestRunOnceCallsGCWhenBinPresent(t *testing.T) {
	tmp := t.TempDir()
	binRoot := filepath.Join(tmp, "bin")

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
		Scanner:    fakeScanner{out: []model.Series{}},
		Classifier: &fakeClassifier{decision: model.Decision{Via: classifier.ViaUnmatched}},
		Filer:      rec,
		Kavita:     rec,
		Unmatched:  rec,
		Activity:   rec,
		Bindings:   &fakeBindingStore{},
		RecycleBin: bin,
	}

	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Fatalf("expected old bin file to be GC'd after RunOnce, stat err=%v", err)
	}
}

// ----- metrics tests -----

func TestMetricsFiledAndScan(t *testing.T) {
	rec := &recorder{}
	fm := newFakeMetrics()
	st, dec := mangaBinding(filepath.FromSlash("/lib/Manga"), 1)
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{{Title: "Berserk", SourcePath: "/dl/Berserk"}}},
		Classifier: &fakeClassifier{decision: dec},
		Filer:      rec, Kavita: rec, Unmatched: rec, Activity: rec,
		Metrics:  fm,
		Bindings: st,
	}
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("runonce: %v", err)
	}
	if fm.filesFiled["Manga"] != 1 {
		t.Errorf("want filesFiled[Manga]=1, got %d", fm.filesFiled["Manga"])
	}
	if fm.kavitaScans["success"] != 1 {
		t.Errorf("want kavitaScans[success]=1, got %d", fm.kavitaScans["success"])
	}
	if !fm.lastRunSet {
		t.Error("SetPollerLastRun was not called at end of RunOnce")
	}
}

func TestMetricsUnmatched(t *testing.T) {
	rec := &recorder{}
	fm := newFakeMetrics()
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{{Title: "???"}}},
		Classifier: &fakeClassifier{decision: model.Decision{Via: classifier.ViaUnmatched}},
		Filer:      rec, Kavita: rec, Unmatched: rec, Activity: rec,
		Metrics:  fm,
		Bindings: &fakeBindingStore{},
	}
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("runonce: %v", err)
	}
	if fm.unmatched != 1 {
		t.Errorf("want unmatched=1, got %d", fm.unmatched)
	}
	if !fm.lastRunSet {
		t.Error("SetPollerLastRun was not called")
	}
}

func TestMetricsFilerError(t *testing.T) {
	rec := &recorder{errFile: errors.New("no space")}
	fm := newFakeMetrics()
	st, dec := mangaBinding(filepath.FromSlash("/lib/Manga"), 3)
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{{Title: "Vagabond"}}},
		Classifier: &fakeClassifier{decision: dec},
		Filer:      rec, Kavita: rec, Unmatched: rec, Activity: rec,
		Metrics:  fm,
		Bindings: st,
	}
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("runonce: %v", err)
	}
	if fm.fileErrors != 1 {
		t.Errorf("want fileErrors=1, got %d", fm.fileErrors)
	}
	if fm.filesFiled["Manga"] != 0 {
		t.Errorf("want filesFiled[Manga]=0 on error, got %d", fm.filesFiled["Manga"])
	}
}

func TestMetricsKavitaError(t *testing.T) {
	rec := &recorder{errScan: errors.New("kavita down")}
	fm := newFakeMetrics()
	st, dec := mangaBinding(filepath.FromSlash("/lib/Manga"), 4)
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{{Title: "Vinland Saga"}}},
		Classifier: &fakeClassifier{decision: dec},
		Filer:      rec, Kavita: rec, Unmatched: rec, Activity: rec,
		Metrics:  fm,
		Bindings: st,
	}
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("runonce: %v", err)
	}
	if fm.kavitaScans["error"] != 1 {
		t.Errorf("want kavitaScans[error]=1, got %d", fm.kavitaScans["error"])
	}
	if fm.kavitaScans["success"] != 0 {
		t.Errorf("want kavitaScans[success]=0, got %d", fm.kavitaScans["success"])
	}
}

func TestMetricsNilSafe(t *testing.T) {
	rec := &recorder{}
	st, dec := mangaBinding(filepath.FromSlash("/lib/Manga"), 5)
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{{Title: "One Piece"}}},
		Classifier: &fakeClassifier{decision: dec},
		Filer:      rec, Kavita: rec, Unmatched: rec, Activity: rec,
		Metrics:  nil, // explicitly nil — must not panic
		Bindings: st,
	}
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("runonce with nil metrics: %v", err)
	}
}

// ---- Preview tests ----

// fakePlanner records Plan calls and returns canned plan entries.
type fakePlanner struct {
	plans     []filer.PlanEntry
	planCalls int
	err       error
}

func (f *fakePlanner) Plan(series, srcDir, dstRoot string) ([]filer.PlanEntry, error) {
	f.planCalls++
	if f.err != nil {
		return nil, f.err
	}
	return f.plans, nil
}

// fakeKavitaRecorder counts ScanLibrary calls.
type fakeKavitaRecorder struct {
	calls int
}

func (f *fakeKavitaRecorder) ScanLibrary(libraryID int64) error {
	f.calls++
	return nil
}

// TestPreviewWalksAllSeries verifies that Preview processes every series from
// the scanner and assigns the correct Status values.
func TestPreviewWalksAllSeries(t *testing.T) {
	series := []model.Series{
		{Title: "Berserk", SourcePath: "/dl/Berserk", Source: "tranga"},
		{Title: "Unknown", SourcePath: "/dl/Unknown", Source: "suwayomi"},
		{Title: "Error Series", SourcePath: "/dl/Error", Source: "suwayomi"},
	}

	clf := &fakeClassifierMap{
		decisions: map[string]model.Decision{
			"Berserk": {BindingID: 1, Via: "rule:1"},
			"Unknown": {Via: classifier.ViaUnmatched},
		},
		errors: map[string]error{
			"Error Series": errors.New("anilist timeout"),
		},
	}

	planner := &fakePlanner{plans: []filer.PlanEntry{
		{SrcPath: "/dl/Berserk/Ch. 001.cbz", DstPath: "/lib/Manga/Berserk/Berserk - Ch.001.cbz", Action: filer.PlanFile},
	}}

	st := &fakeBindingStore{bindings: []model.Binding{
		{ID: 1, Name: "Manga", LibraryRoot: "/lib/Manga", KavitaLibID: 0},
	}}

	p := &Poller{
		Scanner:    fakeScanner{out: series},
		Classifier: clf,
		Planner:    planner,
		Filer:      &recorder{},
		Kavita:     &recorder{},
		Unmatched:  &recorder{},
		Activity:   &recorder{},
		Bindings:   st,
	}

	entries, err := p.Preview(context.Background())
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 preview entries, got %d: %+v", len(entries), entries)
	}

	byTitle := map[string]PreviewEntry{}
	for _, e := range entries {
		byTitle[e.Title] = e
	}

	if got := byTitle["Berserk"].Status; got != "matched" {
		t.Errorf("Berserk: want status=matched, got %q", got)
	}
	if got := byTitle["Unknown"].Status; got != "unmatched" {
		t.Errorf("Unknown: want status=unmatched, got %q", got)
	}
	if got := byTitle["Error Series"].Status; got != "unmatched" {
		t.Errorf("Error Series: want status=unmatched, got %q", got)
	}
}

func TestPreviewDoesNotCallKavita(t *testing.T) {
	series := []model.Series{
		{Title: "Berserk", SourcePath: "/dl/Berserk", Source: "tranga"},
	}
	kav := &fakeKavitaRecorder{}
	st, dec := mangaBinding("/lib/Manga", 1)
	p := &Poller{
		Scanner:    fakeScanner{out: series},
		Classifier: &fakeClassifier{decision: dec},
		Planner:    &fakePlanner{},
		Filer:      &recorder{},
		Kavita:     kav,
		Unmatched:  &recorder{},
		Activity:   &recorder{},
		Bindings:   st,
	}

	if _, err := p.Preview(context.Background()); err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if kav.calls != 0 {
		t.Fatalf("Preview must not call ScanLibrary, got %d call(s)", kav.calls)
	}
}

// ---- FileOne tests ----
//
// FileOne is the manual-classify-from-Unmatched path. It does NOT go through
// the v2 Decision/BindingID flow — the user has already supplied a
// ContentType, so it uses the v1 LibraryRoots / LibraryIDs maps directly.
// Task 11 will migrate FileOne to v2 bindings; until then these tests cover
// the existing surface.

// fakeSeriesStore satisfies poller.SeriesStore for FileOne tests.
type fakeSeriesStore struct {
	series       map[int64]model.Series
	setTypeCalls []struct {
		id int64
		ct model.ContentType
	}
	getErr error
	setErr error
}

func (f *fakeSeriesStore) GetSeriesByID(id int64) (model.Series, error) {
	if f.getErr != nil {
		return model.Series{}, f.getErr
	}
	s, ok := f.series[id]
	if !ok {
		return model.Series{}, sql.ErrNoRows
	}
	return s, nil
}

func (f *fakeSeriesStore) SetSeriesType(id int64, ct model.ContentType) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.setTypeCalls = append(f.setTypeCalls, struct {
		id int64
		ct model.ContentType
	}{id, ct})
	if s, ok := f.series[id]; ok {
		s.Type = ct
		s.Status = model.StatusPending
		f.series[id] = s
	}
	return nil
}

// fakeCache satisfies poller.Cache for FileOne tests.
type fakeCache struct {
	writes []struct {
		title string
		ct    model.ContentType
	}
	err error
}

func (f *fakeCache) CacheClassification(title string, ct model.ContentType) error {
	if f.err != nil {
		return f.err
	}
	f.writes = append(f.writes, struct {
		title string
		ct    model.ContentType
	}{title, ct})
	return nil
}

// newFileOnePoller builds a minimal Poller ready for FileOne tests.
func newFileOnePoller(st *fakeSeriesStore, cache *fakeCache, rec *recorder, libRoots map[model.ContentType]string, libIDs map[model.ContentType]int64) *Poller {
	return &Poller{
		Scanner:      fakeScanner{},
		Classifier:   &fakeClassifier{decision: model.Decision{Via: classifier.ViaUnmatched}},
		Filer:        rec,
		Kavita:       rec,
		Unmatched:    rec,
		Activity:     rec,
		Store:        st,
		Cache:        cache,
		LibraryRoots: libRoots,
		LibraryIDs:   libIDs,
		Bindings:     &fakeBindingStore{},
	}
}

func TestFileOneFilesAndScans(t *testing.T) {
	st := &fakeSeriesStore{
		series: map[int64]model.Series{
			1: {ID: 1, Title: "Dragon Ball Super (Color)", SourcePath: "/dl/dbs", Source: "suwayomi", Status: model.StatusUnmatched},
		},
	}
	cache := &fakeCache{}
	rec := &recorder{}
	p := newFileOnePoller(st, cache, rec,
		map[model.ContentType]string{model.TypeManga: filepath.FromSlash("/lib/Manga")},
		map[model.ContentType]int64{model.TypeManga: 3},
	)

	if err := p.FileOne(context.Background(), 1, model.TypeManga); err != nil {
		t.Fatalf("FileOne: %v", err)
	}

	if len(cache.writes) != 1 || cache.writes[0].title != "Dragon Ball Super (Color)" || cache.writes[0].ct != model.TypeManga {
		t.Fatalf("expected cache write for Dragon Ball Super (Color)/Manga, got %+v", cache.writes)
	}
	if len(st.setTypeCalls) != 1 || st.setTypeCalls[0].id != 1 || st.setTypeCalls[0].ct != model.TypeManga {
		t.Fatalf("expected SetSeriesType(1, Manga), got %+v", st.setTypeCalls)
	}
	if len(rec.filed) != 1 {
		t.Fatalf("expected 1 filed, got %d", len(rec.filed))
	}
	if len(rec.scanned) != 1 || rec.scanned[0] != 3 {
		t.Fatalf("expected scan of lib 3, got %v", rec.scanned)
	}
	if got := rec.countActions(model.ActionFiled); got != 1 {
		t.Fatalf("expected 1 ActionFiled, got %d", got)
	}
	if got := rec.countActions(model.ActionScanTriggered); got != 1 {
		t.Fatalf("expected 1 ActionScanTriggered, got %d", got)
	}
}

func TestFileOneRecordsErrorWhenNoLibraryRoot(t *testing.T) {
	st := &fakeSeriesStore{
		series: map[int64]model.Series{
			1: {ID: 1, Title: "Berserk", SourcePath: "/dl/Berserk", Status: model.StatusUnmatched},
		},
	}
	cache := &fakeCache{}
	rec := &recorder{}
	p := newFileOnePoller(st, cache, rec,
		map[model.ContentType]string{},
		map[model.ContentType]int64{},
	)

	err := p.FileOne(context.Background(), 1, model.TypeManga)
	if err == nil {
		t.Fatal("expected error for missing library root, got nil")
	}
	if got := rec.countActions(model.ActionError); got != 1 {
		t.Fatalf("expected 1 ActionError, got %d (activity=%+v)", got, rec.activity)
	}
	if len(rec.filed) != 0 {
		t.Fatalf("filer must not be called when library root missing, got %v", rec.filed)
	}
	if len(rec.scanned) != 0 {
		t.Fatalf("kavita must not be called when library root missing, got %v", rec.scanned)
	}
}

func TestFileOneRecordsErrorOnFilerFailure(t *testing.T) {
	st := &fakeSeriesStore{
		series: map[int64]model.Series{
			1: {ID: 1, Title: "Solo Leveling", SourcePath: "/dl/SL", Status: model.StatusUnmatched},
		},
	}
	cache := &fakeCache{}
	rec := &recorder{errFile: errors.New("disk full")}
	p := newFileOnePoller(st, cache, rec,
		map[model.ContentType]string{model.TypeManhwa: filepath.FromSlash("/lib/Manhwa")},
		map[model.ContentType]int64{model.TypeManhwa: 2},
	)

	err := p.FileOne(context.Background(), 1, model.TypeManhwa)
	if err == nil {
		t.Fatal("expected error from filer failure, got nil")
	}
	if got := rec.countActions(model.ActionError); got != 1 {
		t.Fatalf("expected 1 ActionError, got %d (activity=%+v)", got, rec.activity)
	}
	if len(rec.scanned) != 0 {
		t.Fatalf("Kavita must not be called on filer failure, got %v", rec.scanned)
	}
	if got := rec.countActions(model.ActionScanTriggered); got != 0 {
		t.Fatalf("expected 0 ActionScanTriggered, got %d", got)
	}
}

func TestFileOneStillWritesCacheOnSuccess(t *testing.T) {
	st := &fakeSeriesStore{
		series: map[int64]model.Series{
			42: {ID: 42, Title: "Tower of God", SourcePath: "/dl/tog", Status: model.StatusUnmatched},
		},
	}
	cache := &fakeCache{}
	rec := &recorder{}
	p := newFileOnePoller(st, cache, rec,
		map[model.ContentType]string{model.TypeManhwa: filepath.FromSlash("/lib/Manhwa")},
		map[model.ContentType]int64{},
	)

	if err := p.FileOne(context.Background(), 42, model.TypeManhwa); err != nil {
		t.Fatalf("FileOne: %v", err)
	}

	if len(cache.writes) != 1 {
		t.Fatalf("expected 1 cache write, got %d: %+v", len(cache.writes), cache.writes)
	}
	if cache.writes[0].title != "Tower of God" || cache.writes[0].ct != model.TypeManhwa {
		t.Fatalf("unexpected cache write: %+v", cache.writes[0])
	}
}

// ---------- Library Map (Plan B) — Suwayomi refresh ----------

func suwayomiStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"mangas":{"nodes":[]}}}`))
	}))
}

// stubSettingsProvider hands back a fixed Settings on every call.
type stubSettingsProvider struct{ s model.Settings }

func (p *stubSettingsProvider) GetSettings() (model.Settings, error) { return p.s, nil }

func TestRunOnceCallsSuwayomiRefreshWhenConfigured(t *testing.T) {
	srv := suwayomiStub(t)
	defer srv.Close()

	cache := suwayomi.NewPathCache()
	var factoryCalls int32

	p := &Poller{
		Scanner:    fakeScanner{out: nil},
		Classifier: &fakeClassifier{decision: model.Decision{Via: classifier.ViaUnmatched}},
		Filer:      &recorder{},
		Kavita:     &recorder{},
		Unmatched:  &recorder{},
		Activity:   &recorder{},
		Bindings:   &fakeBindingStore{},
		SuwayomiCache: cache,
		SuwayomiClient: func(set model.Settings) (*suwayomi.Client, error) {
			atomic.AddInt32(&factoryCalls, 1)
			return suwayomi.New(srv.URL, suwayomi.NoAuth{}), nil
		},
		Settings: &stubSettingsProvider{s: model.Settings{
			SuwayomiBaseURL: srv.URL,
		}},
	}

	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := atomic.LoadInt32(&factoryCalls); got != 1 {
		t.Errorf("SuwayomiClient factory: want 1 call, got %d", got)
	}
	if got := cache.Size(); got != 0 {
		t.Errorf("cache.Size after refresh of empty library: want 0, got %d", got)
	}
}

func TestRunOnceContinuesWhenSuwayomiRefreshFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	cache := suwayomi.NewPathCache()
	rec := &recorder{}

	series := model.Series{Title: "Solo Leveling", SourcePath: "/dl/Solo Leveling"}
	st, dec := manhwaBinding(filepath.FromSlash("/lib/Manhwa"), 2)
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{series}},
		Classifier: &fakeClassifier{decision: dec},
		Filer:      rec,
		Kavita:     rec,
		Unmatched:  rec,
		Activity:   rec,
		Bindings:   st,
		SuwayomiCache: cache,
		SuwayomiClient: func(set model.Settings) (*suwayomi.Client, error) {
			return suwayomi.New(srv.URL, suwayomi.NoAuth{}), nil
		},
		Settings: &stubSettingsProvider{s: model.Settings{
			SuwayomiBaseURL: srv.URL,
		}},
	}

	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce returned error after refresh failure: %v", err)
	}
	if len(rec.filed) != 1 {
		t.Errorf("tick must continue after refresh failure: want 1 filed, got %d", len(rec.filed))
	}
}

func TestRunOnceSkipsRefreshWhenSuwayomiURLEmpty(t *testing.T) {
	var factoryCalls int32
	cache := suwayomi.NewPathCache()
	p := &Poller{
		Scanner:    fakeScanner{out: nil},
		Classifier: &fakeClassifier{decision: model.Decision{Via: classifier.ViaUnmatched}},
		Filer:      &recorder{},
		Kavita:     &recorder{},
		Unmatched:  &recorder{},
		Activity:   &recorder{},
		Bindings:   &fakeBindingStore{},
		SuwayomiCache: cache,
		SuwayomiClient: func(set model.Settings) (*suwayomi.Client, error) {
			atomic.AddInt32(&factoryCalls, 1)
			return nil, nil
		},
		Settings: &stubSettingsProvider{s: model.Settings{
			// SuwayomiBaseURL deliberately empty.
		}},
	}
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := atomic.LoadInt32(&factoryCalls); got != 0 {
		t.Errorf("factory must not be called when SuwayomiBaseURL is empty, got %d", got)
	}
}

// TestRunOnceWritesViaIntoActivity asserts that the Via tag the classifier's
// Decision carries ends up on the ActivityEntry.
func TestRunOnceWritesViaIntoActivity(t *testing.T) {
	s := model.Series{Title: "Solo Leveling", SourcePath: "/dl/Solo Leveling"}
	rec := &recorder{}
	st, _ := manhwaBinding(filepath.FromSlash("/lib/Manhwa"), 2)
	dec := model.Decision{BindingID: 2, Via: "anilist:KR"}
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{s}},
		Classifier: &fakeClassifier{decision: dec},
		Filer:      rec,
		Kavita:     rec,
		Unmatched:  rec,
		Activity:   rec,
		Bindings:   st,
	}
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	var sawFiled bool
	for _, e := range rec.activity {
		if e.Action == model.ActionFiled {
			sawFiled = true
			if e.Via != "anilist:KR" {
				t.Errorf("ActionFiled Via: want anilist:KR, got %q", e.Via)
			}
		}
	}
	if !sawFiled {
		t.Errorf("expected at least one ActionFiled activity entry")
	}
}
