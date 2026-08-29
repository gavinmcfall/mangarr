package poller

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/filer"
	"github.com/gavinmcfall/mangarr/internal/model"
)

// A filer.ConflictError is a PARTIAL success: the non-conflicting files were
// filed, so the poller must record a "conflict" activity, flip the series to
// StatusConflict, bump the conflict counter — and still record the binding,
// the "filed" activity and trigger the Kavita scan. It must NOT be counted
// or reported as a file error.
func TestRunOnceRecordsConflictAsPartialSuccess(t *testing.T) {
	ce := &filer.ConflictError{Conflicts: []filer.Conflict{{
		Src:       filepath.FromSlash("/dl/Dragon Ball/Official_Z 1.cbz"),
		Dst:       filepath.FromSlash("/lib/Manga/Dragon Ball/Dragon Ball - Ch.1.cbz"),
		ClaimedBy: filepath.FromSlash("/dl/Dragon Ball/Official_1.cbz"),
	}}}
	rec := &recorder{errFile: ce}
	st, dec := mangaBinding(filepath.FromSlash("/lib/Manga"), 1)
	metrics := newFakeMetrics()
	seriesStore := &fakeSeriesStore{series: map[int64]model.Series{}}
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{{Title: "Dragon Ball"}}},
		Classifier: &fakeClassifier{decision: dec},
		Filer:      rec, Kavita: rec, Unmatched: rec, Activity: rec,
		Bindings: st,
		Metrics:  metrics,
		Store:    seriesStore,
	}
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("runonce: %v", err)
	}

	if got := rec.countActions(model.ActionConflict); got != 1 {
		t.Fatalf("expected 1 ActionConflict, got %d (activity=%+v)", got, rec.activity)
	}
	if got := rec.countActions(model.ActionError); got != 0 {
		t.Fatalf("a conflict must not be recorded as an error, got %d ActionError", got)
	}
	if got := rec.countActions(model.ActionFiled); got != 1 {
		t.Fatalf("non-conflicting files were filed: expected 1 ActionFiled, got %d", got)
	}
	if len(rec.scanned) != 1 || rec.scanned[0] != 1 {
		t.Fatalf("Kavita scan must still fire for what landed, got %v", rec.scanned)
	}
	if metrics.conflicts != 1 || metrics.fileErrors != 0 {
		t.Fatalf("metrics: want conflicts=1 fileErrors=0, got conflicts=%d fileErrors=%d", metrics.conflicts, metrics.fileErrors)
	}

	var sawConflictStatus bool
	for _, c := range seriesStore.statusCalls {
		if c.st == model.StatusConflict {
			sawConflictStatus = true
		}
	}
	if !sawConflictStatus {
		t.Fatalf("series status should be set to conflict, calls=%+v", seriesStore.statusCalls)
	}

	for _, e := range rec.activity {
		if e.Action == model.ActionConflict {
			if !strings.Contains(e.Detail, "Official_Z 1.cbz") || !strings.Contains(e.Detail, "Official_1.cbz") {
				t.Fatalf("conflict detail should name both files, got %q", e.Detail)
			}
		}
	}
}

// Real filer errors keep their existing semantics (see
// TestRunOnceRecordsErrorOnFilerFailure); this pins that the new branch does
// not swallow them.
func TestRunOnceNonConflictErrorStillErrors(t *testing.T) {
	rec := &recorder{errFile: errNotAConflict{}}
	st, dec := mangaBinding(filepath.FromSlash("/lib/Manga"), 1)
	metrics := newFakeMetrics()
	p := &Poller{
		Scanner:    fakeScanner{out: []model.Series{{Title: "X"}}},
		Classifier: &fakeClassifier{decision: dec},
		Filer:      rec, Kavita: rec, Unmatched: rec, Activity: rec,
		Bindings: st, Metrics: metrics,
	}
	if err := p.RunOnce(context.Background()); err != nil {
		t.Fatalf("runonce: %v", err)
	}
	if rec.countActions(model.ActionError) != 1 || rec.countActions(model.ActionConflict) != 0 || metrics.fileErrors != 1 {
		t.Fatalf("plain error mis-routed: activity=%+v metrics=%+v", rec.activity, metrics)
	}
}

type errNotAConflict struct{}

func (errNotAConflict) Error() string { return "disk full" }
