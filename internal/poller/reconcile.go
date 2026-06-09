package poller

import (
	"time"

	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/store"
)

// reconcileStore is the seam the reconcile pass writes through.
type reconcileStore interface {
	ListSeriesLite() ([]store.SeriesLite, error)
	SetSeriesMissingSince(id int64, t *time.Time) error
	SetSeriesStatus(id int64, st model.Status) error
}

type reconcileConfig struct {
	Grace              time.Duration
	MassVanishPercent  int
	MassVanishMinCount int
}

type reconcileResult struct {
	Aborted         bool
	SkippedZeroScan bool
	Flagged         int
}

// reconcile compares the DB series set (via st) against onDisk (set of source
// paths seen this scan) and applies the floor+grace rules. Writes NOTHING when
// the floor trips. now is injected for deterministic tests.
//
// Safety guards:
//   - Zero-scan guard: if onDisk is empty (scanner returned nothing) the pass is
//     skipped entirely — an empty result is more likely a mount/process failure
//     than every series disappearing simultaneously.
//   - Mass-vanish floor: if the fraction of newly-absent active series exceeds
//     MassVanishPercent AND the raw count meets MassVanishMinCount, the pass is
//     aborted without writing anything.
func reconcile(st reconcileStore, onDisk map[string]bool, cfg reconcileConfig, now time.Time) reconcileResult {
	lite, err := st.ListSeriesLite()
	if err != nil || len(lite) == 0 {
		return reconcileResult{}
	}
	// Zero-scan guard: treat a completely empty on-disk result as a scanner
	// failure rather than genuine disappearance of all series.
	if len(onDisk) == 0 {
		return reconcileResult{SkippedZeroScan: true}
	}

	// Count active (non-orphaned) series that are newly absent this tick.
	active, newlyAbsent := 0, 0
	for _, l := range lite {
		if l.Status != model.StatusOrphaned {
			active++
			if !onDisk[l.SourcePath] {
				newlyAbsent++
			}
		}
	}

	// Mass-vanish floor: if too high a fraction disappears at once, something
	// systemic is wrong (mount dropped, Suwayomi restarted). Abort silently so
	// the operator can investigate rather than having hundreds of series flip to
	// orphaned in one tick.
	overPct := active > 0 && newlyAbsent*100 > cfg.MassVanishPercent*active
	if overPct && newlyAbsent >= cfg.MassVanishMinCount {
		return reconcileResult{Aborted: true}
	}

	res := reconcileResult{}
	// Writes are best-effort: a failed Set* leaves the series in its prior
	// state; the next reconcile tick re-evaluates it.
	for _, l := range lite {
		present := onDisk[l.SourcePath]
		switch {
		case present:
			// Series has reappeared — clear the absence timer and restore status
			// to pending so the next tick files it normally.
			if l.MissingSince != nil {
				_ = st.SetSeriesMissingSince(l.ID, nil)
			}
			if l.Status == model.StatusOrphaned {
				_ = st.SetSeriesStatus(l.ID, model.StatusPending)
			}
		case l.Status == model.StatusOrphaned:
			// Already orphaned and still gone — nothing further to do.
		case l.MissingSince == nil:
			// First tick this series is absent — start the grace clock.
			t := now
			_ = st.SetSeriesMissingSince(l.ID, &t)
		case now.Sub(*l.MissingSince) >= cfg.Grace:
			// Grace period exhausted — soft-delete by flipping to orphaned.
			_ = st.SetSeriesStatus(l.ID, model.StatusOrphaned)
			res.Flagged++
		default:
			// Absent, non-orphaned, still within the grace window — no action
			// this tick; the clock is already running.
		}
	}
	return res
}
