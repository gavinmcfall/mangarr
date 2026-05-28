package poller

import (
	"path/filepath"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/model"
)

// fakes

type fakeClassifier struct{ t model.ContentType }

func (f fakeClassifier) Classify(string) (model.ContentType, error) { return f.t, nil }

type fakeScanner struct{ out []model.Series }

func (f fakeScanner) ScanAll() ([]model.Series, error) { return f.out, nil }

// recorder satisfies Filer, Kavita, and UnmatchedSink interfaces.
// Uses int64 for library IDs (matches model.Settings.KavitaLibIDs []int64).
type recorder struct {
	filed     []model.Series
	scanned   []int64
	unmatched []model.Series
}

func (r *recorder) File(s model.Series, dstRoot string) error {
	r.filed = append(r.filed, s)
	return nil
}

func (r *recorder) ScanLibrary(libID int64) error {
	r.scanned = append(r.scanned, libID)
	return nil
}

func (r *recorder) MarkUnmatched(s model.Series) error {
	r.unmatched = append(r.unmatched, s)
	return nil
}

func TestRunOnceFilesAndScans(t *testing.T) {
	s := model.Series{Title: "Solo Leveling", SourcePath: "/dl/Solo Leveling"}
	rec := &recorder{}
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{s}},
		Classifier: fakeClassifier{t: model.TypeManhwa},
		Filer:      rec,
		Kavita:     rec,
		Unmatched:  rec,
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

// TestRunOnceDeduplicatesKavitaScan verifies that two series of the same type
// only trigger one Kavita scan per type per RunOnce call.
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

// TestRunOnceNoKavitaWhenNoLibraryID verifies that if a content type has no
// LibraryIDs entry, no Kavita scan is triggered (but filing still happens).
func TestRunOnceNoKavitaWhenNoLibraryID(t *testing.T) {
	s := model.Series{Title: "Berserk", SourcePath: "/dl/Berserk"}
	rec := &recorder{}
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{s}},
		Classifier: fakeClassifier{t: model.TypeManga},
		Filer:      rec,
		Kavita:     rec,
		Unmatched:  rec,
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
}

// TestRunOnceNoLibraryRootRoutesToUnmatched verifies that a known type with no
// configured library root is treated as unmatched rather than causing an error.
func TestRunOnceNoLibraryRootRoutesToUnmatched(t *testing.T) {
	s := model.Series{Title: "Vinland Saga", SourcePath: "/dl/Vinland Saga"}
	rec := &recorder{}
	p := &Poller{
		Scanner:      fakeScanner{out: []model.Series{s}},
		Classifier:   fakeClassifier{t: model.TypeManga},
		Filer:        rec,
		Kavita:       rec,
		Unmatched:    rec,
		LibraryRoots: map[model.ContentType]string{}, // no root for Manga
		LibraryIDs:   map[model.ContentType]int64{},
	}
	if err := p.RunOnce(); err != nil {
		t.Fatalf("runonce: %v", err)
	}
	if len(rec.unmatched) != 1 || len(rec.filed) != 0 {
		t.Fatalf("expected unmatched (no root), got filed=%v unmatched=%v", rec.filed, rec.unmatched)
	}
}
