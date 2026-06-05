// Package poller orchestrates a single scan→classify→file→kavita-trigger pass.
//
// On each RunOnce call the poller:
//  1. Calls Scanner.ScanAll() to get candidate series from all download roots.
//  2. Loads the bindings table once via Bindings.ListBindings.
//  3. For each series, classifies it via Classifier.Classify(ctx, ScanItem),
//     getting a Decision{BindingID, Via}.
//     - BindingID == 0 (or unresolvable): routed to UnmatchedSink + ActionUnmatched.
//     - BindingID present but no matching row: ActionError (binding deleted
//       between save and tick — surfaces the race to the operator).
//     - BindingID resolved: filed into Binding.LibraryRoot via Filer.File +
//       ActionFiled, then Kavita scan against Binding.KavitaLibID.
//  4. After filing, triggers a Kavita library scan (once per library per
//     RunOnce call). The dedup map is marked only on success — a transient
//     Kavita failure does not block a retry from a later same-binding series
//     in this tick.
//  5. Every outcome (filed/unmatched/scan-triggered/error) is recorded via
//     ActivityWriter with the Decision.Via tag for the UI's Activity/History
//     view. Activity writes are best-effort: a failed AddActivity does not
//     abort the tick.
//
// Interfaces are minimal so unit tests inject fakes without importing the
// concrete scanner/classifier/filer/kavita packages. The concrete wiring
// lives in main.go.
package poller

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/gavinmcfall/mangarr/internal/classifier"
	"github.com/gavinmcfall/mangarr/internal/filer"
	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/recyclebin"
	"github.com/gavinmcfall/mangarr/internal/suwayomi"
)

// Scanner returns all candidate series from every configured download root.
type Scanner interface {
	ScanAll() ([]model.Series, error)
}

// Classifier is the six-step routing entry point: returns a Decision
// the poller resolves to a Binding for filing + Kavita scan triggering.
// *classifier.Classifier (constructed via classifier.New) satisfies this.
type Classifier interface {
	Classify(ctx context.Context, item classifier.ScanItem) (model.Decision, error)
}

// BindingLister exposes the bindings table for the poller's per-tick
// resolution of Decision.BindingID. *store.Store satisfies this directly.
type BindingLister interface {
	ListBindings() ([]model.Binding, error)
}

// Filer moves/copies/hardlinks the series files into the destination root.
type Filer interface {
	File(s model.Series, dstRoot string) error
}

// Kavita triggers a library scan by library ID.
type Kavita interface {
	ScanLibrary(libraryID int64) error
}

// UnmatchedSink records series that produced Decision{BindingID:0}.
type UnmatchedSink interface {
	MarkUnmatched(s model.Series) error
	// UpsertSeries lands a discovered series in the persistent table
	// regardless of classification outcome. Without this, the matched-
	// on-first-try happy path silently bypasses the series table —
	// only MarkUnmatched writes — and those series never show up on
	// the Series page or expose a Reclassify dropdown.
	UpsertSeries(s model.Series) (int64, error)
}

// ActivityWriter records an audit entry for the UI's Activity/History view.
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
type Cache interface {
	CacheClassification(title string, ct model.ContentType) error
}

// SeriesStore is the subset of store.Store that FileOne needs for series
// lookup and type update. The Preview path also queries by source_path so
// it can enrich Scanner output (which builds fresh in-memory rows from
// disk and so never carries DB-side fields like ManualBindingID) with the
// persisted manual-override the operator set via the Series-page UI.
type SeriesStore interface {
	GetSeriesByID(id int64) (model.Series, error)
	GetSeriesByPath(path string) (model.Series, error)
	SetSeriesType(id int64, ct model.ContentType) error
	// SetSeriesCurrentBinding records the binding the auto-classifier
	// resolved this series to at the most recent successful file, or
	// clears it on Unmatched. /series reads this to render the visible
	// pill without bouncing to the activity log.
	SetSeriesCurrentBinding(id int64, bindingID *int64) error
}

// Planner returns a dry-run plan for a series without touching the
// filesystem. filer.Filer satisfies this via its Plan method.
type Planner interface {
	Plan(series, srcDir, dstRoot string) ([]filer.PlanEntry, error)
}

// PreviewEntry is one series' full pipeline preview.
type PreviewEntry struct {
	Title        string            `json:"title"`
	SourcePath   string            `json:"source_path"`
	Source       string            `json:"source"`
	ChapterCount int               `json:"chapter_count"` // from the scanner — count of .cbz files on disk; surfaces even for unmatched rows
	Classified   model.ContentType `json:"classified"`    // empty under v2 — preview now routes by Binding, not ContentType
	BindingName  string            `json:"binding_name"`  // v2: human-readable binding label, empty when unmatched
	Reason       string            `json:"reason"`        // why the row is unmatched / errored
	DstRoot      string            `json:"dst_root"`      // empty if can't be filed
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
// Settings on every call.
type SuwayomiClientFactory func(set model.Settings) (*suwayomi.Client, error)

// Poller holds the wired-up dependencies and configuration for one
// orchestration tick.
type Poller struct {
	Scanner    Scanner
	Classifier Classifier
	Filer      Filer
	Planner    Planner // optional; used by Preview only
	Kavita     Kavita
	Unmatched  UnmatchedSink
	Activity   ActivityWriter
	Metrics    MetricsSink // optional; nil disables all metric calls

	// Bindings is the v2 routing table: the poller loads it once per tick
	// and resolves Decision.BindingID against it.
	Bindings BindingLister

	Cache Cache // optional; used by FileOne to persist manual type overrides
	Store SeriesStore // optional; used by FileOne to load and update series

	// v1 ContentType→destination maps. Used by FileOne (manual classify
	// from Unmatched) only; RunOnce uses Bindings instead. These remain
	// until the deprecated v1 Settings fields are dropped one release
	// after Plan A; the manual-classify UI still routes by ContentType.
	LibraryRoots map[model.ContentType]string
	LibraryIDs   map[model.ContentType]int64

	RecycleBin *recyclebin.Bin // optional; GC is called at end of each RunOnce tick

	// Library Map (Plan B). All three optional; nil values disable the
	// Suwayomi override path without affecting AniList classification.
	SuwayomiCache  *suwayomi.PathCache
	SuwayomiClient SuwayomiClientFactory
	Settings       SettingsProvider
}

// RunOnce performs one complete scan→classify→file→scan pass.
//
// Errors from individual series do not abort the tick; every series is
// processed. A non-nil error is only returned for failures that prevent
// any meaningful work (e.g. the scanner itself fails to start, or the
// bindings table is unreadable).
//
// Kavita scans are deduplicated per binding library ID: if multiple
// series share the same Binding in a single RunOnce call, only one scan
// is triggered for that library — BUT only on success. A failed scan
// does not poison the dedup map, so a later same-binding series in this
// tick will retry.
//
// Every outcome is recorded via ActivityWriter (filed / unmatched /
// scan-triggered / error) AFTER the action completes — never before —
// so a mid-tick crash cannot produce a phantom success entry.
func (p *Poller) RunOnce(ctx context.Context) error {
	// Library Map (Plan B): refresh the Suwayomi path cache at the top
	// of every tick before classification consumes it.
	p.refreshSuwayomiCache(ctx)

	series, err := p.Scanner.ScanAll()
	if err != nil {
		return err
	}

	// Load bindings once per tick and build a lookup map. Cheap (handful
	// of rows) and avoids N round-trips for series-heavy ticks.
	bindingByID, err := p.loadBindings()
	if err != nil {
		// Bindings table unreadable — this is a hard fail; without
		// bindings the poller has nowhere to route anything.
		return fmt.Errorf("load bindings: %w", err)
	}

	// Track which Kavita library IDs have been scanned SUCCESSFULLY this tick.
	scanned := map[int64]bool{}

	for _, s := range series {
		// Persist the discovered series first so the Series page reflects
		// EVERY title the scanner finds — not just the unmatched ones.
		// The upfront upsert lands the row at StatusPending; MarkUnmatched
		// later flips status on the failure branch via a status-only
		// UPDATE so there's no double-write.
		s.Status = model.StatusPending
		sid, err := p.Unmatched.UpsertSeries(s)
		if err != nil {
			// No classification has run yet, so Via is empty rather than
			// ViaUnmatched (which would falsely imply the classifier
			// produced an unmatched decision).
			p.recordActivityVia(s.Title, model.ActionError, "",
				fmt.Sprintf("upsert series: %v", err))
			continue
		}
		// Scanner.ScanAll builds Series fresh from disk — s.ID is the zero
		// value here. Backfill from the upsert result so downstream writes
		// (SetSeriesCurrentBinding) land on the right row.
		s.ID = sid

		// Scanner.ScanAll builds Series fresh from disk and never carries
		// DB-side fields like ManualBindingID. Pull the persisted override
		// from the row we just upserted so the classifier's step 0 fires
		// on poll-driven routing — without this, an operator-set override
		// would only take effect after a UI-triggered FileOne.
		s.ManualBindingID = p.resolveManualOverride(s)

		d, classifyErr := p.Classifier.Classify(ctx, classifier.ScanItem{
			Title:           s.Title,
			ParentDir:       s.SourcePath,
			ManualBindingID: s.ManualBindingID,
		})
		if classifyErr != nil || d.BindingID == 0 {
			// Classifier failed or routed to Unmatched.
			via := d.Via
			if via == "" {
				via = classifier.ViaUnmatched
			}
			if err := p.Unmatched.MarkUnmatched(s); err != nil {
				p.recordActivityVia(s.Title, model.ActionError, via,
					fmt.Sprintf("mark unmatched: %v", err))
				continue
			}
			// Clear the auto-classifier verdict on Unmatched so /series
			// stops showing a stale "auto" pill for a series whose AniList
			// lookup just dropped out. Errors are swallowed — Unmatched
			// activity row + UnmatchedSink are the durable signals.
			if p.Store != nil {
				_ = p.Store.SetSeriesCurrentBinding(s.ID, nil)
			}
			p.recordActivityVia(s.Title, model.ActionUnmatched, via, "")
			if p.Metrics != nil {
				p.Metrics.IncUnmatched()
			}
			continue
		}

		binding, ok := bindingByID[d.BindingID]
		if !ok {
			// Decision references a binding that no longer exists.
			// Race window: user deleted the binding between save and
			// tick. Surface as ActionError so the operator sees it.
			p.recordActivityVia(s.Title, model.ActionError, d.Via,
				fmt.Sprintf("binding %d not found — was it deleted?", d.BindingID))
			continue
		}
		if binding.LibraryRoot == "" {
			// Binding exists but has no destination — misconfiguration.
			p.recordActivityVia(s.Title, model.ActionError, d.Via,
				fmt.Sprintf("binding %q (id=%d) has empty library_root — check Settings", binding.Name, binding.ID))
			continue
		}

		if err := p.Filer.File(s, binding.LibraryRoot); err != nil {
			p.recordActivityVia(s.Title, model.ActionError, d.Via,
				fmt.Sprintf("file: %v", err))
			if p.Metrics != nil {
				p.Metrics.IncFileError()
			}
			continue
		}
		// Record the auto-classifier's verdict so /series can render the
		// resolved binding without having to query the activity log.
		// Errors swallowed — the file already landed, the activity row is
		// authoritative, and a stale CurrentBindingID self-corrects on the
		// next successful tick.
		if p.Store != nil {
			bid := binding.ID
			_ = p.Store.SetSeriesCurrentBinding(s.ID, &bid)
		}
		p.recordActivityVia(s.Title, model.ActionFiled, d.Via,
			fmt.Sprintf("filed into %s", binding.LibraryRoot))
		if p.Metrics != nil {
			p.Metrics.IncFilesFiled(binding.Name)
		}

		// Trigger a Kavita scan against the binding's library (once per
		// library per tick, gated on SUCCESS so a transient failure can
		// be retried by the next same-binding series in this tick).
		// KavitaLibID == 0 means "no Kavita scan needed" — skip silently.
		if binding.KavitaLibID != 0 && !scanned[binding.KavitaLibID] {
			if err := p.Kavita.ScanLibrary(binding.KavitaLibID); err != nil {
				p.recordActivityVia(s.Title, model.ActionError, d.Via,
					fmt.Sprintf("kavita scan library %d: %v", binding.KavitaLibID, err))
				if p.Metrics != nil {
					p.Metrics.IncKavitaScan("error")
				}
				// Do NOT mark scanned[id] — let next same-binding series retry.
				continue
			}
			p.recordActivityVia(s.Title, model.ActionScanTriggered, d.Via,
				fmt.Sprintf("library %d", binding.KavitaLibID))
			if p.Metrics != nil {
				p.Metrics.IncKavitaScan("success")
			}
			scanned[binding.KavitaLibID] = true
		}
	}

	// GC the recycle bin at the end of each tick (best-effort).
	if p.RecycleBin != nil {
		files, dirs, gcErr := p.RecycleBin.GC(time.Now())
		if gcErr != nil {
			log.Printf("poller: recycle bin GC error: %v", gcErr)
		} else if files > 0 || dirs > 0 {
			log.Printf("poller: recycle bin GC removed %d file(s) and %d empty dir(s)", files, dirs)
		}
	}

	if p.Metrics != nil {
		p.Metrics.SetPollerLastRun(time.Now())
	}

	return nil
}

// resolveManualOverride returns the manual binding pinned to the given
// series. Scanner.ScanAll builds Series fresh from disk and so never
// carries DB-side fields like ManualBindingID — both RunOnce and
// Preview need to hydrate from the persisted row so the classifier's
// step 0 honours operator-set overrides.
//
// Falls back to s.ManualBindingID if Store isn't wired (test fixtures)
// or the row doesn't exist yet.
func (p *Poller) resolveManualOverride(s model.Series) *int64 {
	if s.ManualBindingID != nil {
		return s.ManualBindingID
	}
	if p.Store == nil {
		return nil
	}
	persisted, err := p.Store.GetSeriesByPath(s.SourcePath)
	if err != nil {
		return nil
	}
	return persisted.ManualBindingID
}

// loadBindings fetches the bindings list and indexes it by ID. Nil
// Bindings is tolerated (returns empty map) so test fixtures and the
// pre-wiring main.go boot path don't crash; in production main.go wires
// the real store unconditionally.
func (p *Poller) loadBindings() (map[int64]model.Binding, error) {
	if p.Bindings == nil {
		return map[int64]model.Binding{}, nil
	}
	bindings, err := p.Bindings.ListBindings()
	if err != nil {
		return nil, err
	}
	out := make(map[int64]model.Binding, len(bindings))
	for _, b := range bindings {
		out[b.ID] = b
	}
	return out, nil
}

// refreshSuwayomiCache rebuilds the Suwayomi path cache from a fresh
// client built off current Settings. Called at the top of every tick.
//
// No-op when any of the Library Map collaborators is missing or when
// Settings.SuwayomiBaseURL is empty.
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

// recordActivityVia writes an activity entry best-effort with the Via
// reason from the classifier's Decision. A failure to write activity
// must never abort the tick.
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

// FileOne applies the classify-and-file pipeline to a single series
// identified by its primary key (seriesID) and an explicitly supplied
// ContentType (ct).
//
// Unlike RunOnce, FileOne does NOT call the classifier — the type is
// given directly by the caller (user intent from the Unmatched page).
// It still uses the v1 LibraryRoots/LibraryIDs maps (manually-set
// ContentType → destination); migrating this surface to v2 bindings is
// deferred until the deprecated v1 Settings fields are dropped one
// release after Plan A.
//
// Activity entries are written with Via = classifier.ViaManual so the
// activity log column distinguishes user-driven routes from automatic
// classifier decisions.
func (p *Poller) FileOne(ctx context.Context, seriesID int64, ct model.ContentType) error {
	if p.Store == nil {
		return fmt.Errorf("FileOne: Store not configured")
	}

	series, err := p.Store.GetSeriesByID(seriesID)
	if err != nil {
		return fmt.Errorf("FileOne: load series %d: %w", seriesID, err)
	}

	if p.Cache != nil {
		if err := p.Cache.CacheClassification(series.Title, ct); err != nil {
			log.Printf("poller: FileOne: cache write for %q failed (continuing): %v", series.Title, err)
		}
	}

	if err := p.Store.SetSeriesType(seriesID, ct); err != nil {
		p.recordActivityVia(series.Title, model.ActionError, classifier.ViaManual,
			fmt.Sprintf("FileOne: set series type: %v", err))
		return fmt.Errorf("FileOne: set series type: %w", err)
	}
	series.Type = ct

	root, ok := p.LibraryRoots[ct]
	if !ok || root == "" {
		msg := fmt.Sprintf("type %s has no configured library root — check Settings", ct)
		p.recordActivityVia(series.Title, model.ActionError, classifier.ViaManual, msg)
		return fmt.Errorf("FileOne: %s", msg)
	}

	if err := p.Filer.File(series, root); err != nil {
		p.recordActivityVia(series.Title, model.ActionError, classifier.ViaManual,
			fmt.Sprintf("file: %v", err))
		return fmt.Errorf("FileOne: filer: %w", err)
	}
	p.recordActivityVia(series.Title, model.ActionFiled, classifier.ViaManual,
		fmt.Sprintf("filed into %s", root))

	if id, ok := p.LibraryIDs[ct]; ok && id != 0 {
		if p.Kavita != nil {
			if err := p.Kavita.ScanLibrary(id); err != nil {
				p.recordActivityVia(series.Title, model.ActionError, classifier.ViaManual,
					fmt.Sprintf("kavita scan library %d: %v", id, err))
			} else {
				p.recordActivityVia(series.Title, model.ActionScanTriggered, classifier.ViaManual,
					fmt.Sprintf("library %d", id))
			}
		}
	}

	return nil
}

// Preview runs scanner → classifier → filer.Plan for every series, WITHOUT
// triggering Kavita scans, writing to disk, or modifying any state.
//
// Returns a non-nil error only if the scanner itself fails. Individual
// series errors are surfaced as PreviewEntry.Note with Status="misconfigured".
func (p *Poller) Preview(ctx context.Context) ([]PreviewEntry, error) {
	// Refresh the Suwayomi path cache at the top of Preview so it matches
	// what RunOnce would do — otherwise a cold-cache Preview would mis-route
	// items that RunOnce would route via a Suwayomi override.
	p.refreshSuwayomiCache(ctx)

	series, err := p.Scanner.ScanAll()
	if err != nil {
		return nil, err
	}

	bindingByID, err := p.loadBindings()
	if err != nil {
		return nil, fmt.Errorf("load bindings: %w", err)
	}

	results := make([]PreviewEntry, 0, len(series))
	for _, s := range series {
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}
		results = append(results, p.previewSeries(ctx, s, bindingByID))
	}
	return results, nil
}

// previewSeries classifies a single series and builds its PreviewEntry without
// touching the filesystem or triggering any side-effects. It is the shared
// inner body used by both Preview (over scanner output) and PreviewOne (over a
// single persisted series looked up by ID).
//
// The ctx.Done cancellation check belongs in Preview's loop — not here —
// because previewSeries handles one already-selected series.
func (p *Poller) previewSeries(ctx context.Context, s model.Series, bindingByID map[int64]model.Binding) PreviewEntry {
	entry := PreviewEntry{
		Title:        s.Title,
		SourcePath:   s.SourcePath,
		Source:       s.Source,
		ChapterCount: s.ChapterCount,
	}

	manualOverride := p.resolveManualOverride(s)

	d, classifyErr := p.Classifier.Classify(ctx, classifier.ScanItem{
		Title:           s.Title,
		ParentDir:       s.SourcePath,
		ManualBindingID: manualOverride,
	})
	if classifyErr != nil {
		entry.Status = "unmatched"
		entry.Reason = fmt.Sprintf("classify error: %v", classifyErr)
		return entry
	}
	if d.BindingID == 0 {
		entry.Status = "unmatched"
		entry.Reason = "no binding matched"
		return entry
	}

	binding, ok := bindingByID[d.BindingID]
	if !ok {
		entry.Status = "misconfigured"
		entry.Note = fmt.Sprintf("binding %d not found — was it deleted?", d.BindingID)
		return entry
	}
	if binding.LibraryRoot == "" {
		entry.Status = "misconfigured"
		entry.Note = fmt.Sprintf("binding %q has empty library_root — check Settings", binding.Name)
		return entry
	}

	entry.BindingName = binding.Name
	entry.DstRoot = binding.LibraryRoot
	entry.Status = "matched"

	if p.Planner != nil {
		plans, planErr := p.Planner.Plan(s.Title, s.SourcePath, binding.LibraryRoot)
		if planErr != nil {
			entry.Status = "misconfigured"
			entry.Note = fmt.Sprintf("plan error: %v", planErr)
			entry.DstRoot = ""
			return entry
		}
		entry.ChapterPlans = plans
	}

	return entry
}

// PreviewOne returns the PreviewEntry that Preview would produce for a single
// persisted series identified by seriesID. It loads the series from Store (so
// the persisted SourcePath and manual override are honoured), then delegates to
// previewSeries.
func (p *Poller) PreviewOne(ctx context.Context, seriesID int64) (PreviewEntry, error) {
	if p.Store == nil {
		return PreviewEntry{}, fmt.Errorf("PreviewOne: no store wired")
	}
	s, err := p.Store.GetSeriesByID(seriesID)
	if err != nil {
		return PreviewEntry{}, fmt.Errorf("PreviewOne: series %d: %w", seriesID, err)
	}
	bindingByID, err := p.loadBindings()
	if err != nil {
		return PreviewEntry{}, fmt.Errorf("PreviewOne: load bindings: %w", err)
	}
	return p.previewSeries(ctx, s, bindingByID), nil
}

// RefileOne classifies one persisted series via the current classifier and
// files it into the resolved binding's LibraryRoot. It records an ActionFiled
// activity entry and updates the series' CurrentBindingID on success.
//
// Returns an error if the series cannot be found, the classifier returns
// BindingID==0 (unmatched), the binding is missing or has no library_root, or
// the filer fails.
func (p *Poller) RefileOne(ctx context.Context, seriesID int64) error {
	if p.Store == nil {
		return fmt.Errorf("RefileOne: no store wired")
	}
	s, err := p.Store.GetSeriesByID(seriesID)
	if err != nil {
		return fmt.Errorf("RefileOne: series %d: %w", seriesID, err)
	}
	bindingByID, err := p.loadBindings()
	if err != nil {
		return fmt.Errorf("RefileOne: load bindings: %w", err)
	}
	d, err := p.Classifier.Classify(ctx, classifier.ScanItem{
		Title: s.Title, ParentDir: s.SourcePath, ManualBindingID: p.resolveManualOverride(s),
	})
	if err != nil {
		return fmt.Errorf("RefileOne: classify: %w", err)
	}
	if d.BindingID == 0 {
		return fmt.Errorf("RefileOne: series %d is unmatched — nothing to file", seriesID)
	}
	binding, ok := bindingByID[d.BindingID]
	if !ok || binding.LibraryRoot == "" {
		return fmt.Errorf("RefileOne: binding %d missing or has no library_root", d.BindingID)
	}
	if err := p.Filer.File(s, binding.LibraryRoot); err != nil {
		return fmt.Errorf("RefileOne: file: %w", err)
	}
	bid := binding.ID
	_ = p.Store.SetSeriesCurrentBinding(s.ID, &bid)
	p.recordActivityVia(s.Title, model.ActionFiled, d.Via, fmt.Sprintf("refiled into %s", binding.LibraryRoot))
	return nil
}
