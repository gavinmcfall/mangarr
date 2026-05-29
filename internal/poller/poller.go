// Package poller orchestrates a single scan→classify→file→kavita-trigger pass.
//
// On each RunOnce call the poller:
//  1. Calls Scanner.ScanAll() to get candidate series from all download roots.
//  2. For each series, classifies its type via Classifier.Classify.
//     - TypeUnknown (or classify error): routed to UnmatchedSink + ActionUnmatched.
//     - Known type with no configured library root: ActionError (misconfiguration).
//     - Known type with a root: filed via Filer.File + ActionFiled, then Kavita scan.
//  3. After filing, triggers a Kavita library scan (once per library per RunOnce
//     call). The dedup map is marked only on success — a transient Kavita
//     failure does not block a retry from a later same-type series in this tick.
//  4. Every outcome (filed/unmatched/scan-triggered/error) is recorded via
//     ActivityWriter for the UI's Activity/History view. Activity writes are
//     best-effort: a failed AddActivity does not abort the tick.
//
// Interfaces are minimal so unit tests inject fakes without importing the
// concrete scanner/classifier/filer/kavita packages. The concrete wiring lives
// in main.go (Task 9).
package poller

import (
	"fmt"
	"log"
	"time"

	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/recyclebin"
)

// Scanner returns all candidate series from every configured download root.
// The concrete implementation in main.go wraps scanner.Scan for each root.
type Scanner interface {
	ScanAll() ([]model.Series, error)
}

// Classifier maps a series title to a ContentType.
// classifier.Classifier satisfies this interface directly.
type Classifier interface {
	Classify(title string) (model.ContentType, error)
}

// Filer moves/copies/hardlinks the series files into the destination root.
// The concrete implementation in main.go adapts filer.Filer.File, which takes
// (series string, srcDir string, dstRoot string).
type Filer interface {
	File(s model.Series, dstRoot string) error
}

// Kavita triggers a library scan by library ID (int64 matches model.Settings.KavitaLibIDs).
// kavita.Client.ScanLibrary satisfies this interface directly.
type Kavita interface {
	ScanLibrary(libraryID int64) error
}

// UnmatchedSink records series whose type could not be determined.
// The concrete implementation in main.go upserts into the store with StatusUnmatched.
type UnmatchedSink interface {
	MarkUnmatched(s model.Series) error
}

// ActivityWriter records an audit entry for the UI's Activity/History view.
// store.Store.AddActivity satisfies this interface directly.
type ActivityWriter interface {
	AddActivity(e model.ActivityEntry) error
}

// MetricsSink is the subset of the metrics.Registry interface the poller uses.
// A nil value is safe: all methods are guarded with a nil check.
type MetricsSink interface {
	IncFilesFiled(category string)
	IncKavitaScan(result string)
	IncUnmatched()
	IncFileError()
	SetPollerLastRun(t time.Time)
}

// Poller holds the wired-up dependencies and configuration for one orchestration tick.
type Poller struct {
	Scanner      Scanner
	Classifier   Classifier
	Filer        Filer
	Kavita       Kavita
	Unmatched    UnmatchedSink
	Activity     ActivityWriter
	Metrics      MetricsSink                  // optional; nil disables all metric calls
	LibraryRoots map[model.ContentType]string // content type → absolute library path
	LibraryIDs   map[model.ContentType]int64  // content type → Kavita library ID
	RecycleBin   *recyclebin.Bin              // optional; GC is called at end of each RunOnce tick
}

// RunOnce performs one complete scan→classify→file→scan pass.
//
// Errors from individual series do not abort the tick; every series is
// processed. A non-nil error is only returned for failures that prevent any
// meaningful work (e.g. the scanner itself fails to start).
//
// Kavita scans are deduplicated: if multiple series share the same ContentType
// in a single RunOnce call, only one scan is triggered for that library —
// BUT only on success. A failed scan does not poison the dedup map, so a
// later same-type series in this tick will retry.
//
// Every outcome is recorded via ActivityWriter (filed / unmatched /
// scan-triggered / error) AFTER the action completes — never before — so a
// mid-tick crash cannot produce a phantom success entry.
func (p *Poller) RunOnce() error {
	series, err := p.Scanner.ScanAll()
	if err != nil {
		return err
	}

	// Track which Kavita library IDs have been scanned SUCCESSFULLY this tick.
	scanned := map[int64]bool{}

	for _, s := range series {
		ct, classifyErr := p.Classifier.Classify(s.Title)
		if classifyErr != nil || ct == model.TypeUnknown {
			// Route to unmatched — classification failed or type unknown.
			if err := p.Unmatched.MarkUnmatched(s); err != nil {
				p.recordActivity(s.Title, model.ActionError,
					fmt.Sprintf("mark unmatched: %v", err))
				continue
			}
			p.recordActivity(s.Title, model.ActionUnmatched, "")
			if p.Metrics != nil {
				p.Metrics.IncUnmatched()
			}
			continue
		}

		root, ok := p.LibraryRoots[ct]
		if !ok {
			// Known type but no configured root — this is a misconfiguration,
			// NOT something a user can fix from the Unmatched UI (reclassifying
			// "Manhwa" → "Manhwa" would just loop forever). Record as error so
			// the operator sees it in Activity and updates Settings.
			p.recordActivity(s.Title, model.ActionError,
				fmt.Sprintf("type %s has no configured library root — check Settings", ct))
			continue
		}

		s.Type = ct
		if err := p.Filer.File(s, root); err != nil {
			// Filer error: record, do NOT trigger scan, move on.
			p.recordActivity(s.Title, model.ActionError,
				fmt.Sprintf("file: %v", err))
			if p.Metrics != nil {
				p.Metrics.IncFileError()
			}
			continue
		}
		p.recordActivity(s.Title, model.ActionFiled,
			fmt.Sprintf("filed into %s", root))
		if p.Metrics != nil {
			p.Metrics.IncFilesFiled(string(ct))
		}

		// Trigger a Kavita scan for this library (once per library per tick,
		// gated on SUCCESS so a transient failure can be retried by the next
		// same-type series in this tick).
		if id, ok := p.LibraryIDs[ct]; ok && !scanned[id] {
			if err := p.Kavita.ScanLibrary(id); err != nil {
				p.recordActivity(s.Title, model.ActionError,
					fmt.Sprintf("kavita scan library %d: %v", id, err))
				if p.Metrics != nil {
					p.Metrics.IncKavitaScan("error")
				}
				// Do NOT mark scanned[id] — let the next same-type series retry.
				continue
			}
			p.recordActivity(s.Title, model.ActionScanTriggered,
				fmt.Sprintf("library %d", id))
			if p.Metrics != nil {
				p.Metrics.IncKavitaScan("success")
			}
			scanned[id] = true
		}
	}
	// GC the recycle bin at the end of each tick (best-effort — a GC failure
	// must never abort the tick or surface as a RunOnce error).
	if p.RecycleBin != nil {
		files, dirs, gcErr := p.RecycleBin.GC(time.Now())
		if gcErr != nil {
			log.Printf("poller: recycle bin GC error: %v", gcErr)
		} else if files > 0 || dirs > 0 {
			log.Printf("poller: recycle bin GC removed %d file(s) and %d empty dir(s)", files, dirs)
		}
	}

	// Update the poller-last-run gauge at the end of every successful tick.
	if p.Metrics != nil {
		p.Metrics.SetPollerLastRun(time.Now())
	}

	return nil
}

// recordActivity writes an activity entry best-effort. A failure to write
// activity must never abort the tick. If Activity is nil (e.g. minimal test
// setups), the call is a no-op.
func (p *Poller) recordActivity(title string, action model.ActivityAction, detail string) {
	if p.Activity == nil {
		return
	}
	_ = p.Activity.AddActivity(model.ActivityEntry{
		SeriesTitle: title,
		Action:      action,
		Detail:      detail,
	})
}
