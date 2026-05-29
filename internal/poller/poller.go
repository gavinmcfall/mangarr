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
	"context"
	"fmt"
	"log"
	"time"

	"github.com/gavinmcfall/mangarr/internal/filer"
	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/recyclebin"
	"github.com/gavinmcfall/mangarr/internal/suwayomi"
)

// Scanner returns all candidate series from every configured download root.
// The concrete implementation in main.go wraps scanner.Scan for each root.
type Scanner interface {
	ScanAll() ([]model.Series, error)
}

// Classifier maps a series to a ContentType plus a "via" reason string
// (e.g. "anilist:KR", "suwayomi-override:category=42", "unmatched") used
// in activity log entries. classifier.Classifier satisfies this interface
// directly via its ClassifySeries method.
//
// The Via channel exists so the Library Map override path (Plan B)
// records which classifier produced the routing decision. Pre-Plan-B
// callers (Preview, the one-off FileOne path) still use the plain
// title-only Classify method on the concrete classifier — they keep
// receiving just (ContentType, error) and write empty Via.
type Classifier interface {
	ClassifySeries(s model.Series) (model.ContentType, string, error)
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

// Cache backs the per-title classification override used by FileOne.
// store.Store satisfies this interface directly.
type Cache interface {
	CacheClassification(title string, ct model.ContentType) error
}

// SeriesStore is the subset of store.Store that FileOne needs for series lookup
// and type update.
type SeriesStore interface {
	GetSeriesByID(id int64) (model.Series, error)
	SetSeriesType(id int64, ct model.ContentType) error
}

// Planner returns a dry-run plan for a series without touching the filesystem.
// filer.Filer satisfies this interface via its Plan method.
type Planner interface {
	Plan(series, srcDir, dstRoot string) ([]filer.PlanEntry, error)
}

// PreviewEntry is one series' full pipeline preview.
type PreviewEntry struct {
	Title        string            `json:"title"`
	SourcePath   string            `json:"source_path"`
	Source       string            `json:"source"`
	Classified   model.ContentType `json:"classified"`    // empty if classifier returned Unknown
	Reason       string            `json:"reason"`        // why Classified is empty / cached / etc.
	DstRoot      string            `json:"dst_root"`      // empty if can't be filed (Unknown or no library root)
	ChapterPlans []filer.PlanEntry `json:"chapter_plans"` // per-chapter from Plan; empty when DstRoot is empty
	Status       string            `json:"status"`        // "matched" | "unmatched" | "misconfigured"
	Note         string            `json:"note"`          // human note for the row
}

// SettingsProvider returns the current Settings on demand. The poller
// reads through it at the top of each tick so user edits (download
// roots, Suwayomi connection params) take effect on the next tick
// without a process restart. *store.Store satisfies this directly.
type SettingsProvider interface {
	GetSettings() (model.Settings, error)
}

// SuwayomiClientFactory builds a fresh suwayomi.Client from the supplied
// Settings on every call. Lives on the Poller so tests can substitute a
// stub that never opens a real socket. Returns (nil, nil) when
// Settings.SuwayomiBaseURL is empty — the poller treats that as
// "feature disabled" and skips the refresh entirely.
type SuwayomiClientFactory func(set model.Settings) (*suwayomi.Client, error)

// Poller holds the wired-up dependencies and configuration for one orchestration tick.
type Poller struct {
	Scanner      Scanner
	Classifier   Classifier
	Filer        Filer
	Planner      Planner // optional; used by Preview only
	Kavita       Kavita
	Unmatched    UnmatchedSink
	Activity     ActivityWriter
	Metrics      MetricsSink                  // optional; nil disables all metric calls
	Cache        Cache                        // optional; used by FileOne to persist manual type overrides
	Store        SeriesStore                  // optional; used by FileOne to load and update series
	LibraryRoots map[model.ContentType]string // content type → absolute library path
	LibraryIDs   map[model.ContentType]int64  // content type → Kavita library ID
	RecycleBin   *recyclebin.Bin              // optional; GC is called at end of each RunOnce tick

	// Library Map (Plan B). All three optional; nil values disable the
	// Suwayomi override path without affecting AniList classification.
	//
	// SuwayomiCache is the long-lived shared cache the classifier reads on
	// the file-time hot path. The poller refreshes it at the top of each
	// RunOnce tick.
	//
	// SuwayomiClient is the constructor that builds a fresh client from
	// current Settings every tick (PR #28 fresh-per-call pattern). A nil
	// factory or an empty SuwayomiBaseURL in Settings skips the refresh.
	//
	// Settings is read on every tick to learn the current SuwayomiBaseURL,
	// auth params, and DownloadRoots (passed to PathCache.Refresh).
	SuwayomiCache  *suwayomi.PathCache
	SuwayomiClient SuwayomiClientFactory
	Settings       SettingsProvider
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
func (p *Poller) RunOnce(ctx context.Context) error {
	// Library Map (Plan B): refresh the Suwayomi path cache at the top
	// of every tick, BEFORE we scan, so the classifier's override path
	// has fresh data when classifying any files this tick discovers.
	//
	// Failure here is non-fatal — the classifier falls through to
	// AniList for cache misses, and previously-cached entries stay live
	// (PathCache.Refresh swaps atomically on success only). One warning
	// log per failed refresh, no activity entry — these are operator
	// concerns, not per-series events.
	//
	// ctx scope is intentionally narrow: only the Suwayomi refresh
	// observes cancellation today. Plumbing ctx into Scanner.ScanAll,
	// Classifier, Filer.File, Kavita.ScanLibrary, etc. is a separate
	// refactor and not in Plan B's scope — those calls retain their
	// pre-Plan-B signatures.
	p.refreshSuwayomiCache(ctx)

	series, err := p.Scanner.ScanAll()
	if err != nil {
		return err
	}

	// Track which Kavita library IDs have been scanned SUCCESSFULLY this tick.
	scanned := map[int64]bool{}

	for _, s := range series {
		ct, via, classifyErr := p.Classifier.ClassifySeries(s)
		if classifyErr != nil || ct == model.TypeUnknown {
			// Route to unmatched — classification failed or type unknown.
			if err := p.Unmatched.MarkUnmatched(s); err != nil {
				p.recordActivityVia(s.Title, model.ActionError, via,
					fmt.Sprintf("mark unmatched: %v", err))
				continue
			}
			p.recordActivityVia(s.Title, model.ActionUnmatched, via, "")
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
			p.recordActivityVia(s.Title, model.ActionError, via,
				fmt.Sprintf("type %s has no configured library root — check Settings", ct))
			continue
		}

		s.Type = ct
		if err := p.Filer.File(s, root); err != nil {
			// Filer error: record, do NOT trigger scan, move on.
			p.recordActivityVia(s.Title, model.ActionError, via,
				fmt.Sprintf("file: %v", err))
			if p.Metrics != nil {
				p.Metrics.IncFileError()
			}
			continue
		}
		p.recordActivityVia(s.Title, model.ActionFiled, via,
			fmt.Sprintf("filed into %s", root))
		if p.Metrics != nil {
			p.Metrics.IncFilesFiled(string(ct))
		}

		// Trigger a Kavita scan for this library (once per library per tick,
		// gated on SUCCESS so a transient failure can be retried by the next
		// same-type series in this tick).
		if id, ok := p.LibraryIDs[ct]; ok && !scanned[id] {
			if err := p.Kavita.ScanLibrary(id); err != nil {
				p.recordActivityVia(s.Title, model.ActionError, via,
					fmt.Sprintf("kavita scan library %d: %v", id, err))
				if p.Metrics != nil {
					p.Metrics.IncKavitaScan("error")
				}
				// Do NOT mark scanned[id] — let the next same-type series retry.
				continue
			}
			p.recordActivityVia(s.Title, model.ActionScanTriggered, via,
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

// refreshSuwayomiCache rebuilds the Suwayomi path cache from a fresh
// client built off current Settings. Called at the top of every tick.
//
// No-op when any of the Library Map collaborators is missing:
//   - SuwayomiCache nil (feature not wired)
//   - SuwayomiClient factory nil
//   - Settings provider nil
//   - Settings.SuwayomiBaseURL empty (user has not configured Suwayomi)
//
// A failed refresh is logged once and swallowed — the tick continues
// using whatever the cache already had. The classifier falls through to
// AniList for any cache miss, so an unreachable Suwayomi degrades the
// system to pre-Library-Map behaviour, not below it.
func (p *Poller) refreshSuwayomiCache(ctx context.Context) {
	if p.SuwayomiCache == nil || p.SuwayomiClient == nil || p.Settings == nil {
		return
	}
	set, err := p.Settings.GetSettings()
	if err != nil {
		log.Printf("poller: suwayomi refresh: read settings: %v", err)
		return
	}
	if set.SuwayomiBaseURL == "" {
		return
	}
	client, err := p.SuwayomiClient(set)
	if err != nil {
		log.Printf("poller: suwayomi refresh: build client: %v", err)
		return
	}
	if client == nil {
		return
	}
	if err := p.SuwayomiCache.Refresh(ctx, client, set.DownloadRoots); err != nil {
		log.Printf("poller: suwayomi refresh failed (continuing with last good cache): %v", err)
	}
}

// recordActivity writes an activity entry best-effort. A failure to write
// activity must never abort the tick. If Activity is nil (e.g. minimal test
// setups), the call is a no-op.
func (p *Poller) recordActivity(title string, action model.ActivityAction, detail string) {
	p.recordActivityVia(title, action, "", detail)
}

// recordActivityVia is recordActivity plus the Via reason from the
// classifier (e.g. "anilist:KR", "suwayomi-override:category=42",
// "unmatched"). Empty Via is fine — paths that don't run through the
// classifier (FileOne, recycle bin GC) just leave it blank.
func (p *Poller) recordActivityVia(title string, action model.ActivityAction, via, detail string) {
	if p.Activity == nil {
		return
	}
	_ = p.Activity.AddActivity(model.ActivityEntry{
		SeriesTitle: title,
		Action:      action,
		Detail:      detail,
		Via:         via,
	})
}

// FileOne applies the classify-and-file pipeline to a single series identified
// by its primary key (seriesID) and an explicitly supplied ContentType (ct).
//
// Unlike RunOnce, FileOne does NOT call the classifier — the type is given
// directly by the caller (user intent from the Unmatched page). It DOES write
// the classification cache via p.Cache so that future chapters of the same
// title auto-classify without going to AniList.
//
// Steps:
//  1. Load the series from p.Store by seriesID.
//  2. Write the override to p.Cache (if configured).
//  3. Update the series row's Type + reset Status to pending via p.Store.SetSeriesType.
//  4. Look up the library root for ct in p.LibraryRoots.
//  5. Call p.Filer.File; on error write ActionError activity and return.
//  6. On success, write ActionFiled activity, trigger a Kavita scan for the
//     matching library (if p.LibraryIDs[ct] is set), and write ActionScanTriggered.
//
// All activity writes are best-effort. p.Cache and p.Kavita are optional:
// a nil Cache skips the cache write; a missing LibraryIDs entry skips the scan.
func (p *Poller) FileOne(ctx context.Context, seriesID int64, ct model.ContentType) error {
	if p.Store == nil {
		return fmt.Errorf("FileOne: Store not configured")
	}

	series, err := p.Store.GetSeriesByID(seriesID)
	if err != nil {
		return fmt.Errorf("FileOne: load series %d: %w", seriesID, err)
	}

	// Write the manual override to the cache so future auto-classify picks it up.
	if p.Cache != nil {
		if err := p.Cache.CacheClassification(series.Title, ct); err != nil {
			log.Printf("poller: FileOne: cache write for %q failed (continuing): %v", series.Title, err)
		}
	}

	// Update the series type in the store.
	if err := p.Store.SetSeriesType(seriesID, ct); err != nil {
		p.recordActivity(series.Title, model.ActionError,
			fmt.Sprintf("FileOne: set series type: %v", err))
		return fmt.Errorf("FileOne: set series type: %w", err)
	}
	series.Type = ct

	// Resolve library root — misconfiguration should surface as an error, not silently pass.
	root, ok := p.LibraryRoots[ct]
	if !ok || root == "" {
		msg := fmt.Sprintf("type %s has no configured library root — check Settings", ct)
		p.recordActivity(series.Title, model.ActionError, msg)
		return fmt.Errorf("FileOne: %s", msg)
	}

	// File the series.
	if err := p.Filer.File(series, root); err != nil {
		p.recordActivity(series.Title, model.ActionError,
			fmt.Sprintf("file: %v", err))
		return fmt.Errorf("FileOne: filer: %w", err)
	}
	p.recordActivity(series.Title, model.ActionFiled,
		fmt.Sprintf("filed into %s", root))

	// Trigger Kavita scan if library ID is configured for this type.
	if id, ok := p.LibraryIDs[ct]; ok && id != 0 {
		if p.Kavita != nil {
			if err := p.Kavita.ScanLibrary(id); err != nil {
				p.recordActivity(series.Title, model.ActionError,
					fmt.Sprintf("kavita scan library %d: %v", id, err))
			} else {
				p.recordActivity(series.Title, model.ActionScanTriggered,
					fmt.Sprintf("library %d", id))
			}
		}
	}

	return nil
}

// Preview runs scanner → classifier → filer.Plan for every series, WITHOUT
// triggering Kavita scans, writing to disk, or modifying any state (incl. cache).
//
// The classifier's cache is READ for speed but NOT WRITTEN. Live network calls
// may still occur for cache-miss titles; if AniList is unavailable, the entry's
// Classified field stays empty and Reason records the error — the preview
// continues over other series.
//
// Returns a non-nil error only if the scanner itself fails. Individual series
// errors are surfaced as PreviewEntry.Note with Status="misconfigured".
func (p *Poller) Preview(ctx context.Context) ([]PreviewEntry, error) {
	series, err := p.Scanner.ScanAll()
	if err != nil {
		return nil, err
	}

	results := make([]PreviewEntry, 0, len(series))
	for _, s := range series {
		// Check context cancellation — long previews on large libraries should
		// respect the caller's deadline.
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}

		entry := PreviewEntry{
			Title:      s.Title,
			SourcePath: s.SourcePath,
			Source:     s.Source,
		}

		ct, _, classifyErr := p.Classifier.ClassifySeries(s)
		if classifyErr != nil {
			entry.Status = "unmatched"
			entry.Reason = fmt.Sprintf("classify error: %v", classifyErr)
			results = append(results, entry)
			continue
		}
		if ct == model.TypeUnknown {
			entry.Status = "unmatched"
			entry.Reason = "AniList returned no match"
			results = append(results, entry)
			continue
		}

		entry.Classified = ct

		root, ok := p.LibraryRoots[ct]
		if !ok || root == "" {
			entry.Status = "misconfigured"
			entry.Note = fmt.Sprintf("type %s has no configured library root — check Settings", ct)
			results = append(results, entry)
			continue
		}

		entry.DstRoot = root
		entry.Status = "matched"

		// Run the plan (read-only filesystem walk).
		if p.Planner != nil {
			plans, planErr := p.Planner.Plan(s.Title, s.SourcePath, root)
			if planErr != nil {
				entry.Status = "misconfigured"
				entry.Note = fmt.Sprintf("plan error: %v", planErr)
				entry.DstRoot = ""
				results = append(results, entry)
				continue
			}
			entry.ChapterPlans = plans
		}

		results = append(results, entry)
	}
	return results, nil
}
