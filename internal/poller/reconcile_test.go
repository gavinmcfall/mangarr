package poller

import (
	"testing"
	"time"

	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/store"
)

type fakeReconcileStore struct {
	lite       []store.SeriesLite
	missingSet map[int64]*time.Time
	statusSet  map[int64]model.Status
}

func (f *fakeReconcileStore) ListSeriesLite() ([]store.SeriesLite, error) { return f.lite, nil }
func (f *fakeReconcileStore) SetSeriesMissingSince(id int64, t *time.Time) error {
	f.missingSet[id] = t
	return nil
}
func (f *fakeReconcileStore) SetSeriesStatus(id int64, st model.Status) error {
	f.statusSet[id] = st
	return nil
}

func newFake(lite []store.SeriesLite) *fakeReconcileStore {
	return &fakeReconcileStore{lite: lite, missingSet: map[int64]*time.Time{}, statusSet: map[int64]model.Status{}}
}

func cfg() reconcileConfig {
	return reconcileConfig{Grace: 10 * time.Minute, MassVanishPercent: 25, MassVanishMinCount: 5}
}

func TestReconcileReappearClearsMissing(t *testing.T) {
	old := time.Unix(1000, 0).UTC()
	f := newFake([]store.SeriesLite{{ID: 1, SourcePath: "/d/A", Status: model.StatusOrphaned, MissingSince: &old}})
	res := reconcile(f, map[string]bool{"/d/A": true}, cfg(), time.Unix(2000, 0).UTC())
	if res.Aborted {
		t.Fatal("should not abort")
	}
	if got, ok := f.missingSet[1]; !ok || got != nil {
		t.Errorf("missing_since not cleared: %v", got)
	}
	if f.statusSet[1] != model.StatusPending {
		t.Errorf("orphaned not restored to pending: %v", f.statusSet[1])
	}
}

func TestReconcileFirstAbsenceSetsTimer(t *testing.T) {
	f := newFake([]store.SeriesLite{{ID: 1, SourcePath: "/d/A", Status: model.StatusPending}})
	now := time.Unix(2000, 0).UTC()
	// onDisk is non-empty (scanner ran successfully) but /d/A is absent.
	res := reconcile(f, map[string]bool{"/d/SENTINEL": true}, cfg(), now)
	if res.Aborted {
		t.Fatal("should not abort (only 1 series, below min count)")
	}
	got := f.missingSet[1]
	if got == nil || !got.Equal(now) {
		t.Errorf("missing_since = %v, want %v", got, now)
	}
	if _, flipped := f.statusSet[1]; flipped {
		t.Error("status should not flip within grace")
	}
}

func TestReconcileGraceExceededFlipsOrphaned(t *testing.T) {
	old := time.Unix(1000, 0).UTC()
	f := newFake([]store.SeriesLite{{ID: 1, SourcePath: "/d/A", Status: model.StatusPending, MissingSince: &old}})
	// onDisk is non-empty (scanner ran successfully) but /d/A is absent.
	res := reconcile(f, map[string]bool{"/d/SENTINEL": true}, cfg(), old.Add(11*time.Minute))
	if f.statusSet[1] != model.StatusOrphaned {
		t.Errorf("status = %v, want orphaned", f.statusSet[1])
	}
	if res.Flagged != 1 {
		t.Errorf("Flagged = %d, want 1", res.Flagged)
	}
}

func TestReconcileWithinGraceDoesNothing(t *testing.T) {
	start := time.Unix(1000, 0).UTC()
	f := newFake([]store.SeriesLite{
		{ID: 1, SourcePath: "/d/A", Status: model.StatusPending, MissingSince: &start},
	})
	// 5 minutes in, grace is 10 — must be a no-op
	reconcile(f, map[string]bool{"/d/SENTINEL": true}, cfg(), start.Add(5*time.Minute))
	if len(f.missingSet) != 0 || len(f.statusSet) != 0 {
		t.Error("series within grace window must not be touched")
	}
}

func TestReconcileZeroScanSkips(t *testing.T) {
	f := newFake([]store.SeriesLite{{ID: 1, SourcePath: "/d/A", Status: model.StatusPending}})
	res := reconcile(f, map[string]bool{}, cfg(), time.Unix(2000, 0).UTC())
	if !res.SkippedZeroScan {
		t.Fatal("expected SkippedZeroScan when on-disk set empty and DB non-empty")
	}
	if len(f.missingSet) != 0 || len(f.statusSet) != 0 {
		t.Error("zero-scan reconcile must write nothing")
	}
}

func TestReconcileMassVanishAborts(t *testing.T) {
	var lite []store.SeriesLite
	for i := int64(1); i <= 10; i++ {
		lite = append(lite, store.SeriesLite{ID: i, SourcePath: "/d/" + string(rune('A'+i)), Status: model.StatusPending})
	}
	f := newFake(lite)
	onDisk := map[string]bool{"/d/" + string(rune('A'+1)): true, "/d/" + string(rune('A'+2)): true}
	res := reconcile(f, onDisk, cfg(), time.Unix(2000, 0).UTC())
	if !res.Aborted {
		t.Fatal("expected abort on mass vanish")
	}
	if len(f.missingSet) != 0 || len(f.statusSet) != 0 {
		t.Error("aborted reconcile must write nothing")
	}
}
