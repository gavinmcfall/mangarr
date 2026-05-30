package classifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gavinmcfall/mangarr/internal/anilist"
	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/suwayomi"
)

const defaultEndpoint = "https://graphql.anilist.co"

// Cache is the subset of store.Store used to short-circuit AniList lookups.
// Pass nil to New/NewWithCache to skip caching (tests do this).
type Cache interface {
	GetCachedClassification(titleNorm string) (model.ContentType, bool, error)
	CacheClassification(titleNorm string, t model.ContentType) error
}

// MetricsSink is the subset of the metrics.Registry interface the classifier uses.
// A nil value is safe: all calls are guarded with a nil check.
type MetricsSink interface {
	IncAniListLookup(result string)
}

// PathLookup is the subset of *suwayomi.PathCache the classifier consults
// for Suwayomi category overrides. *suwayomi.PathCache satisfies this
// directly; tests can pass a hand-built stub instead of standing up a
// full cache.
//
// In v1 ClassifySeries: a nil PathLookup disables override resolution —
// classifier behaviour is then identical to the pre-Library-Map AniList
// path.
//
// In v2 Classify: a nil PathLookup skips steps 2-3 of the six-step flow
// and proceeds straight to step 4 (AniList rules), then step 5/6
// (default-binding / unmatched). Path-only rules in step 1 do NOT depend
// on the PathLookup and still fire.
type PathLookup interface {
	Lookup(parentDir string) (suwayomi.CacheEntry, bool)
}

// SettingsReader is the subset of store.Store the classifier reads.
// In the v1 path (ClassifySeries) only GetSettings is consulted — the
// classifier reads SuwayomiCategoryOverrides + KavitaLibIDsByType off
// the Settings row.
//
// In the v2 path (Classify) we also need ListRules to drive the rule
// walk and (transitively, via Decision.BindingID) ListBindings so the
// poller can resolve the destination. ListBindings stays on the
// interface so callers can pass a single fake to either flow without
// adapter shims; the v2 Classify method itself does not call it (the
// poller does).
//
// A nil SettingsReader disables BOTH flows — there's no useful default.
type SettingsReader interface {
	GetSettings() (model.Settings, error)
	ListBindings() ([]model.Binding, error)
	ListRules() ([]model.ClassificationRule, error)
}

// AniListClient is the subset of *anilist.Client the v2 Classify method
// consumes. Tests pass a fake; production wires the real client.
type AniListClient interface {
	Lookup(ctx context.Context, title string) (anilist.Result, error)
}

// ScanItem is the input to v2 Classify. It carries the metadata the
// six-step flow needs to make a routing decision: the title to look up
// on AniList and the parent directory the file was downloaded into
// (so path-only rules and Suwayomi PathCache lookups can short-circuit
// before any network call).
type ScanItem struct {
	Title     string
	ParentDir string
}

// Classifier operates in one of two transitional modes during the
// v1 → v2 migration (Library Map → Library Bindings v2):
//
//	v1 mode: constructed by New / NewWithCache, optionally extended with
//	  WithSuwayomi. Uses endpoint / http / cache / pathCache / settings
//	  fields. Methods: ClassifyTitle(title) and ClassifySeries(series).
//	  Returns (ContentType, via, error).
//
//	v2 mode: constructed by NewV2. Uses anilist / pathCache / store
//	  fields. Method: Classify(ctx, ScanItem). Returns (Decision, error).
//
// The two modes are mutually exclusive at instance level — fields not
// populated by the chosen constructor are nil, and calling the wrong-
// mode method on an instance will nil-pointer-dereference. This is
// acceptable transitional state: Task 11 of the Library Bindings v2
// plan removes the v1 mode entirely and collapses the struct to a
// single shape (anilist / pathCache / store only).
//
// Field naming preserves git-blame continuity: pathCache is shared
// between v1 and v2 (suwayomi.PathCache implements PathLookup for both
// flows). settings is v1-only — v2 reads bindings/rules/settings via
// the wider `store` interface.
type Classifier struct {
	endpoint string
	http     *http.Client
	cache    Cache       // may be nil
	Metrics  MetricsSink // optional; nil disables all metric calls

	// Library Map (Plan B / v1). pathCache is also used by v2 Classify
	// (steps 2-3); settings is v1-only (v2 reads its settings off the
	// wider `store` interface).
	pathCache PathLookup
	settings  SettingsReader

	// v2 dependencies. anilist is the widened lookup client (returns
	// CountryOfOrigin + IsAdult + Format in one call); store is the
	// bindings/rules/settings reader. Both are populated by NewV2; the
	// v1 constructors leave them nil. Classify (v2) requires both to
	// be non-nil; ClassifySeries (v1) ignores them.
	anilist AniListClient
	store   SettingsReader
}

// New creates a Classifier that queries the given endpoint (use "" for the
// live AniList endpoint). Tests pass an httptest.Server URL here.
func New(endpoint string) *Classifier {
	return NewWithCache(endpoint, nil)
}

// NewWithCache creates a Classifier that checks the cache before hitting
// the network and writes through after a successful lookup.
func NewWithCache(endpoint string, cache Cache) *Classifier {
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	return &Classifier{endpoint: endpoint, http: &http.Client{Timeout: 15 * time.Second}, cache: cache}
}

// WithSuwayomi wires the Library Map dependencies onto a Classifier and
// returns it. Pass nil for either argument to leave the override path
// disabled. The Suwayomi path cache is the long-lived shared instance the
// poller refreshes at the top of each tick; the SettingsReader is
// consulted on every Classify call so UI edits to the override map take
// effect on the next file without requiring a restart.
func (c *Classifier) WithSuwayomi(cache PathLookup, settings SettingsReader) *Classifier {
	c.pathCache = cache
	c.settings = settings
	return c
}

const query = `query ($s: String) { Media(search: $s, type: MANGA) { countryOfOrigin } }`

type anilistResp struct {
	Data struct {
		Media *struct {
			CountryOfOrigin string `json:"countryOfOrigin"`
		} `json:"Media"`
	} `json:"data"`
}

// Via reason strings produced by ClassifySeries (v1) and Classify (v2).
// The spec example used the human-readable category name
// ("Korean Webtoons"), but the classifier here only has the category ID
// from the Suwayomi PathCache — resolving names would require an extra
// Suwayomi round-trip. Plan C's activity log renderer at
// internal/web/web.go uses these prefixes with strings.HasPrefix to
// detect entries and resolve human-friendly labels at display time.
//
// IMPORTANT: ViaSuwayomiOverridePrefix is shared between v1 and v2 so
// the renderer's HasPrefix check survives the Task 10 poller swap.
// Diverging the prefix between flows would silently break the activity
// log (raw "suwayomi-override:..." would render instead of the category
// name) — that regression is what motivated exporting the v2 prefixes
// as constants too.
const (
	ViaSuwayomiOverridePrefix = "suwayomi-override:category=" // + <int64 category ID>
	ViaAniListPrefix          = "anilist:"                    // + <ISO-3166-1 alpha-2 country code> (v1 only)
	ViaUnmatched              = "unmatched"

	// ViaPathRulePrefix is the v2 activity-log Via prefix for routes via
	// a path-only classification rule, formatted as "path-rule:<ruleID>".
	ViaPathRulePrefix = "path-rule:"

	// ViaRulePrefix is the v2 activity-log Via prefix for routes via an
	// AniList-matching classification rule, formatted as "rule:<ruleID>".
	ViaRulePrefix = "rule:"

	// ViaDefaultBinding is the v2 literal Via value when the no-match
	// fallback routed to Settings.DefaultBindingID.
	ViaDefaultBinding = "default-binding"
)

// ClassifySeries is the orchestrated classification entry point used by
// the poller. It returns (type, via, error) where via captures which
// path produced the result so the activity log can show the user how a
// series was routed.
//
// Resolution order:
//  1. If the Library Map dependencies are wired (PathCache + Settings)
//     AND Settings.SuwayomiCategoryOverrides is non-empty AND
//     PathCache.Lookup(series.SourcePath) hits, walk entry.CategoryIDs
//     (already sorted ascending by Suwayomi category.order) and return
//     the first ID that maps in SuwayomiCategoryOverrides.
//     The mapped Kavita library ID is reverse-looked-up in
//     Settings.KavitaLibIDsByType to derive a ContentType, which the
//     poller uses to pick a LibraryRoot for filing. The Via field
//     records the matching Suwayomi category ID.
//  2. Otherwise fall through to the existing AniList lookup. The Via
//     field encodes the resolved country code; TypeUnknown carries
//     Via="unmatched".
//
// Any failure in the override resolution (settings read error, missing
// settings, empty overrides, cache miss, no matching category) is a
// non-error fall-through. AniList errors propagate to the caller.
//
// Note on the override → ContentType mapping: Plan B keeps the poller's
// per-ContentType LibraryRoots/LibraryIDs pipeline intact. The override
// gives us a Kavita library ID; KavitaLibIDsByType is the inverse
// mapping the user already maintains for the AniList default path, so
// we reuse it. If a user maps a Suwayomi category to a Kavita library
// that has no entry in KavitaLibIDsByType (i.e. they configured an
// override for a library they never assigned a ContentType to), we
// surface Via=suwayomi-override:category=N + TypeUnknown so the poller
// routes it to Unmatched and the user can see the misconfig — better
// than silently dropping it.
func (c *Classifier) ClassifySeries(s model.Series) (model.ContentType, string, error) {
	if c.pathCache != nil && c.settings != nil {
		if set, err := c.settings.GetSettings(); err == nil && len(set.SuwayomiCategoryOverrides) > 0 {
			if entry, ok := c.pathCache.Lookup(s.SourcePath); ok {
				for _, catID := range entry.CategoryIDs {
					if libID, mapped := set.SuwayomiCategoryOverrides[catID]; mapped {
						via := fmt.Sprintf("%s%d", ViaSuwayomiOverridePrefix, catID)
						ct := reverseKavitaLibLookup(set.KavitaLibIDsByType, libID)
						return ct, via, nil
					}
				}
			}
		}
	}

	ct, err := c.ClassifyTitle(s.Title)
	if err != nil {
		return model.TypeUnknown, ViaUnmatched, err
	}
	if ct == model.TypeUnknown {
		return model.TypeUnknown, ViaUnmatched, nil
	}
	return ct, ViaAniListPrefix + countryCodeForType(ct), nil
}

// reverseKavitaLibLookup finds the ContentType whose KavitaLibIDsByType
// entry equals libID. Returns TypeUnknown if no entry matches — the
// poller will then route the series to Unmatched, which surfaces the
// misconfiguration to the user.
//
// We iterate a fixed slice rather than `range m` because Go's map
// iteration order is randomised. If two ContentTypes happen to point at
// the same Kavita library ID (a Settings misconfig Plan C's UI will
// eventually prevent), a `range m` walk would return a different winner
// on each call → the same series routed to different libraries across
// poll ticks. Deterministic-by-priority is the better failure mode:
// Manga → Manhwa → Manhua wins in that order, which mirrors the
// CountryToType priority and gives operators a single fact to debug.
func reverseKavitaLibLookup(m map[model.ContentType]int64, libID int64) model.ContentType {
	for _, ct := range []model.ContentType{model.TypeManga, model.TypeManhwa, model.TypeManhua} {
		if m[ct] == libID {
			return ct
		}
	}
	return model.TypeUnknown
}

// countryCodeForType reverses CountryToType for the canonical code each
// content type carries in its Via reason. CN wins over TW since AniList
// is overwhelmingly CN-coded for Chinese-origin manhua and the activity
// log just wants something deterministic to render.
func countryCodeForType(ct model.ContentType) string {
	switch ct {
	case model.TypeManga:
		return "JP"
	case model.TypeManhwa:
		return "KR"
	case model.TypeManhua:
		return "CN"
	default:
		return ""
	}
}

// ClassifyTitle returns the content type for a title, or TypeUnknown if
// AniList has no match or the country is unmapped. Results are read
// from and written to the cache (when a Cache is configured) to respect
// AniList's rate limit.
//
// This is the v1 (pre-Library-Bindings-v2) entry point used by
// ClassifySeries. The v2 Classify method (different signature, takes
// ctx + ScanItem, returns Decision) is the replacement; this method
// is preserved for one commit boundary so Task 10's poller swap can
// be reviewed independently and Task 11 can remove it cleanly.
func (c *Classifier) ClassifyTitle(title string) (model.ContentType, error) {
	// Cache read — skip network if we already know the answer.
	if c.cache != nil {
		if ct, ok, err := c.cache.GetCachedClassification(title); err == nil && ok {
			if c.Metrics != nil {
				c.Metrics.IncAniListLookup("cached")
			}
			return ct, nil
		}
	}

	body, _ := json.Marshal(map[string]any{
		"query":     query,
		"variables": map[string]string{"s": title},
	})
	req, err := http.NewRequest(http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		if c.Metrics != nil {
			c.Metrics.IncAniListLookup("error")
		}
		return model.TypeUnknown, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if c.Metrics != nil {
			c.Metrics.IncAniListLookup("error")
		}
		return model.TypeUnknown, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		if c.Metrics != nil {
			c.Metrics.IncAniListLookup("error")
		}
		return model.TypeUnknown, fmt.Errorf("anilist rate limited")
	}
	if resp.StatusCode >= 400 {
		if c.Metrics != nil {
			c.Metrics.IncAniListLookup("error")
		}
		return model.TypeUnknown, fmt.Errorf("anilist status %d", resp.StatusCode)
	}
	var out anilistResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		if c.Metrics != nil {
			c.Metrics.IncAniListLookup("error")
		}
		return model.TypeUnknown, err
	}
	if out.Data.Media == nil {
		if c.Metrics != nil {
			c.Metrics.IncAniListLookup("miss")
		}
		return model.TypeUnknown, nil
	}
	ct := model.CountryToType(out.Data.Media.CountryOfOrigin)

	// Cache write-through (best-effort; ignore error so caller still gets result).
	if c.cache != nil {
		_ = c.cache.CacheClassification(title, ct)
	}

	if c.Metrics != nil {
		c.Metrics.IncAniListLookup("success")
	}
	return ct, nil
}

// NewV2 constructs a Classifier wired for the v2 six-step Classify flow:
// the widened AniList client (returns countryOfOrigin + isAdult + format
// in one call), an optional Suwayomi PathLookup (nil disables steps 2-3),
// and a SettingsReader that exposes bindings, rules, and settings.
//
// The returned Classifier does NOT have the v1 endpoint/http/cache
// fields set — calling ClassifyTitle on it will panic on the nil
// http client. Tests and the poller must use the matching method for
// the constructor they chose. Task 11 deletes ClassifyTitle and the
// v1 constructors entirely; this split is a one-commit boundary.
func NewV2(a AniListClient, p PathLookup, s SettingsReader) *Classifier {
	return &Classifier{anilist: a, pathCache: p, store: s}
}

// Classify is the v2 six-step routing flow:
//
//  1. Path-only rules — if any rule's Condition has only SourcePathPrefix
//     set and item.ParentDir starts with that prefix, route to the rule's
//     binding with Via = "path-rule:<ruleID>". Skip the AniList call.
//  2. Suwayomi PathCache lookup — if a *suwayomi.PathCache is wired and
//     it has an entry for item.ParentDir, walk the entry's CategoryIDs
//     (pre-sorted ascending by Suwayomi category.order); first ID present
//     in Settings.SuwayomiCategoryBindings (the v2 routing map) wins.
//     Route with Via = "suwayomi-override:cat=<categoryID>".
//  3. AniList lookup — call c.anilist.Lookup(ctx, item.Title). If it
//     errors (network failure, ErrNotFound, anything), skip step 4 and
//     fall through to step 5. The error is intentionally swallowed so
//     a transient AniList outage doesn't crash the poller tick.
//  4. AniList rules — walk all non-path-only rules in priority order;
//     first to match (AND-semantics across set Condition fields) wins.
//     Route with Via = "rule:<ruleID>".
//  5. Default binding fallback — if Settings.DefaultBindingID is set,
//     route there with Via = "default-binding".
//  6. Unmatched — return Decision{BindingID: 0, Via: "unmatched"}.
//
// The store is consulted at the top of every call so UI edits to the
// rules or default-binding take effect on the next file without
// requiring a restart. Rules come back from the store ORDER BY priority,
// so step 1 and step 4 inherit the priority walk without an extra sort.
//
// Reads Settings.SuwayomiCategoryBindings (the v2 routing map, populated
// by Migration 2) — NOT the v1 SuwayomiCategoryOverrides field. The v1
// field stays untouched on the settings row so a rollback to the v1
// classifier still finds its data.
func (c *Classifier) Classify(ctx context.Context, item ScanItem) (model.Decision, error) {
	settings, err := c.store.GetSettings()
	if err != nil {
		return model.Decision{}, fmt.Errorf("load settings: %w", err)
	}
	rules, err := c.store.ListRules()
	if err != nil {
		return model.Decision{}, fmt.Errorf("load rules: %w", err)
	}
	// rules already sorted by priority in the store.

	// Step 1: path-only rules short-circuit before any AniList call.
	for _, r := range rules {
		if !r.Condition.IsPathOnly() {
			continue
		}
		if strings.HasPrefix(item.ParentDir, *r.Condition.SourcePathPrefix) {
			return model.Decision{
				BindingID: r.BindingID,
				Via:       fmt.Sprintf("%s%d", ViaPathRulePrefix, r.ID),
			}, nil
		}
	}

	// Step 2-3: Suwayomi PathCache lookup, then category overrides. The
	// Via prefix is shared with v1 (ViaSuwayomiOverridePrefix) so the
	// activity-log renderer's HasPrefix check keeps working after the
	// Task 10 poller swap.
	if c.pathCache != nil {
		if entry, ok := c.pathCache.Lookup(item.ParentDir); ok {
			for _, catID := range entry.CategoryIDs {
				if bindingID, mapped := settings.SuwayomiCategoryBindings[catID]; mapped {
					return model.Decision{
						BindingID: bindingID,
						Via:       fmt.Sprintf("%s%d", ViaSuwayomiOverridePrefix, catID),
					}, nil
				}
			}
		}
	}

	// Step 4: AniList rules. Errors are swallowed so a transient outage
	// degrades gracefully to step 5/6 rather than failing the poll tick.
	result, anilistErr := c.anilist.Lookup(ctx, item.Title)
	if anilistErr == nil {
		for _, r := range rules {
			if r.Condition.IsPathOnly() {
				continue // already evaluated in step 1
			}
			if matchesRule(r.Condition, result, item.ParentDir) {
				return model.Decision{
					BindingID: r.BindingID,
					Via:       fmt.Sprintf("%s%d", ViaRulePrefix, r.ID),
				}, nil
			}
		}
	}

	// Step 5: Default binding fallback.
	if settings.DefaultBindingID != nil {
		return model.Decision{
			BindingID: *settings.DefaultBindingID,
			Via:       ViaDefaultBinding,
		}, nil
	}

	// Step 6: Unmatched.
	return model.Decision{BindingID: 0, Via: ViaUnmatched}, nil
}

// matchesRule applies AND-semantics across set Condition fields. Unset
// pointer = wildcard (don't constrain on that axis). All four axes are
// independent so a rule can mix any subset (e.g. country+isAdult, or
// path+format) and the rule matches only when every set axis matches.
func matchesRule(cond model.RuleCondition, result anilist.Result, parentDir string) bool {
	if cond.CountryOfOrigin != nil && result.CountryOfOrigin != *cond.CountryOfOrigin {
		return false
	}
	if cond.IsAdult != nil && result.IsAdult != *cond.IsAdult {
		return false
	}
	if cond.Format != nil && result.Format != *cond.Format {
		return false
	}
	if cond.SourcePathPrefix != nil && !strings.HasPrefix(parentDir, *cond.SourcePathPrefix) {
		return false
	}
	return true
}
