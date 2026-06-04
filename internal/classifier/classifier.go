package classifier

import (
	"context"
	"fmt"
	"strings"

	"github.com/gavinmcfall/mangarr/internal/anilist"
	"github.com/gavinmcfall/mangarr/internal/mangadex"
	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/suwayomi"
)

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
// A nil PathLookup skips steps 2-3 of the six-step flow and proceeds
// straight to step 4 (AniList rules), then step 5/6 (default-binding /
// unmatched). Path-only rules in step 1 do NOT depend on the PathLookup
// and still fire.
type PathLookup interface {
	Lookup(parentDir string) (suwayomi.CacheEntry, bool)
}

// SettingsReader is the subset of store.Store the classifier reads.
// Classify needs ListRules to drive the rule walk and GetSettings for
// DefaultBindingID + SuwayomiCategoryBindings. ListBindings stays on the
// interface so the poller can resolve Decision.BindingID → destination
// via the same fake in tests.
//
// A nil SettingsReader disables Classify — there's no useful default.
type SettingsReader interface {
	GetSettings() (model.Settings, error)
	ListBindings() ([]model.Binding, error)
	ListRules() ([]model.ClassificationRule, error)
}

// AniListClient is the subset of *anilist.Client the Classify method
// consumes. Tests pass a fake; production wires the real client.
type AniListClient interface {
	Lookup(ctx context.Context, title string) (anilist.Result, error)
}

// MangaDexClient is the subset of *mangadex.Client the Classify method
// consumes as a fallback when AniList has no MANGA entry. Optional — nil
// disables step 4b. Tests pass a fake; production wires the real client.
type MangaDexClient interface {
	Lookup(ctx context.Context, title string) (mangadex.Result, error)
}

// ScanItem is the input to Classify. It carries the metadata the
// six-step flow needs to make a routing decision: the title to look up
// on AniList and the parent directory the file was downloaded into
// (so path-only rules and Suwayomi PathCache lookups can short-circuit
// before any network call).
type ScanItem struct {
	Title     string
	ParentDir string
	// ManualBindingID, when non-nil, short-circuits the six-step flow at
	// step 0 and routes straight to that binding with Via = "manual".
	// Set by the poller from Series.ManualBindingID, which is in turn
	// written by the Series-page reclassify control. nil means "no
	// override; classify normally".
	ManualBindingID *int64
}

// Classifier resolves a ScanItem to a routing Decision via the six-step
// flow documented on Classify. It is constructed by New, which wires:
//
//	anilist:  widened AniList client (countryOfOrigin + isAdult + format)
//	suwayomi: optional PathCache (nil disables steps 2-3)
//	store:    bindings/rules/settings reader
//
// Plan A Task 11 collapsed the previous dual-shape struct (v1
// ClassifySeries/ClassifyTitle + v2 Classify) down to this single
// shape. The v1 surface is gone.
type Classifier struct {
	Metrics MetricsSink // optional; nil disables all metric calls

	anilist  AniListClient
	mangadex MangaDexClient // optional; nil disables the step 4b fallback
	suwayomi PathLookup
	store    SettingsReader
}

// Via reason strings produced by Classify. The activity-log renderer at
// internal/web/web.go uses these prefixes with strings.HasPrefix to
// detect entries and resolve human-friendly labels at display time.
const (
	ViaSuwayomiOverridePrefix = "suwayomi-override:category=" // + <int64 category ID>
	ViaUnmatched              = "unmatched"

	// ViaAniListPrefix is the LEGACY v1 activity-log Via prefix for
	// AniList-routed entries, formatted as "anilist:<ISO country code>".
	// The current classifier never emits this — v2 emits ViaRulePrefix
	// instead — but historical activity log rows from before the v1 → v2
	// switch may still carry it, and the web renderer at
	// internal/web/web.go formatVia keeps a HasPrefix branch alive so
	// those rows render with a friendly label instead of the raw string.
	ViaAniListPrefix = "anilist:"

	// ViaPathRulePrefix is the activity-log Via prefix for routes via
	// a path-only classification rule, formatted as "path-rule:<ruleID>".
	ViaPathRulePrefix = "path-rule:"

	// ViaRulePrefix is the activity-log Via prefix for routes via an
	// AniList-matching classification rule, formatted as "rule:<ruleID>".
	ViaRulePrefix = "rule:"

	// ViaMangaDexRulePrefix is the activity-log Via prefix for routes via
	// the MangaDex fallback (step 4b) matching a classification rule,
	// formatted as "mangadex-rule:<ruleID>". Distinct from ViaRulePrefix so
	// the activity log shows which catalogue produced the match.
	ViaMangaDexRulePrefix = "mangadex-rule:"

	// ViaDefaultBinding is the literal Via value when the no-match
	// fallback routed to Settings.DefaultBindingID.
	ViaDefaultBinding = "default-binding"

	// ViaManual is the literal Via value when the poller's FileOne
	// surface (manual classify-from-Unmatched) records an activity entry.
	// Keeps the activity log Via column populated for user-driven routes.
	ViaManual = "manual"
)

// New constructs a Classifier wired for the six-step Classify flow:
// the widened AniList client (returns countryOfOrigin + isAdult + format
// in one call), an optional Suwayomi PathLookup (nil disables steps 2-3),
// and a SettingsReader that exposes bindings, rules, and settings.
func New(a AniListClient, p PathLookup, s SettingsReader) *Classifier {
	return &Classifier{anilist: a, suwayomi: p, store: s}
}

// WithMangaDex sets the optional MangaDex fallback client (classifier
// step 4b) and returns the receiver for chaining. nil leaves the fallback
// disabled — Classify then behaves exactly as before.
func (c *Classifier) WithMangaDex(m MangaDexClient) *Classifier {
	c.mangadex = m
	return c
}

// mangaDexLangToCountry maps MangaDex's originalLanguage (publication
// language, ISO 639-1) onto the country-of-origin axis the rules reason
// over. Only the unambiguous CJK languages map: a Korean manhwa is "ko",
// a Japanese manga "ja", a Chinese manhua "zh". en is deliberately ABSENT
// — publication language is not country of origin (an officially-English
// manhwa, an OEL comic, and an English light novel are indistinguishable
// here), so en-origin titles fall through to default/unmatched and are
// resolved by a manual override. Returns "" for anything unmapped.
func mangaDexLangToCountry(lang string) string {
	switch lang {
	case "ko":
		return "KR"
	case "ja":
		return "JP"
	case "zh", "zh-hk":
		return "CN"
	default:
		return ""
	}
}

// Classify is the six-step routing flow:
//
//  1. Path-only rules — if any rule's Condition has only SourcePathPrefix
//     set and item.ParentDir starts with that prefix, route to the rule's
//     binding with Via = "path-rule:<ruleID>". Skip the AniList call.
//  2. Suwayomi PathCache lookup — if a *suwayomi.PathCache is wired and
//     it has an entry for item.ParentDir, walk the entry's CategoryIDs
//     (pre-sorted ascending by Suwayomi category.order); first ID present
//     in Settings.SuwayomiCategoryBindings (the v2 routing map) wins.
//     Route with Via = "suwayomi-override:category=<categoryID>".
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
	// Step 0: manual override. The Series-page reclassify control sets
	// this on the series; the poller copies it into ScanItem. When
	// present it wins over every other routing path so the operator
	// can pin a series that AniList simply doesn't have catalogued.
	if item.ManualBindingID != nil && *item.ManualBindingID != 0 {
		return model.Decision{
			BindingID: *item.ManualBindingID,
			Via:       ViaManual,
		}, nil
	}

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

	// Step 2-3: Suwayomi PathCache lookup, then category overrides.
	if c.suwayomi != nil {
		if entry, ok := c.suwayomi.Lookup(item.ParentDir); ok {
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
	//
	// Titles with a trailing parenthetical / bracketed variant tag (e.g.
	// "Dragon Ball Super (Color)", "Made in Abyss [Official Edition]")
	// commonly fail AniList's search because the canonical entry is the
	// bare title. On a not-found, retry once with the trailing tag
	// stripped. Only one retry; the helper returns the original string
	// unchanged when there's nothing to strip, so we don't double-hit
	// AniList for already-bare titles.
	result, anilistErr := c.anilist.Lookup(ctx, item.Title)
	if anilistErr != nil {
		if bare := stripTrailingTag(item.Title); bare != "" && bare != item.Title {
			if r2, err2 := c.anilist.Lookup(ctx, bare); err2 == nil {
				result, anilistErr = r2, nil
			}
		}
	}
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

	// Step 4b: MangaDex fallback. AniList's catalogue misses popular
	// manhwa/manhua (no MANGA entry, or only an anime adaptation with a
	// misleading countryOfOrigin). When wired, consult MangaDex and map
	// its originalLanguage onto the country-of-origin axis, then re-run the
	// SAME rule walk. Only CJK languages map (see mangaDexLangToCountry);
	// en-origin titles produce an empty country and won't match a
	// country rule — by design. Errors swallowed like the AniList step.
	if c.mangadex != nil {
		md, mdErr := c.mangadex.Lookup(ctx, item.Title)
		if mdErr != nil {
			if bare := stripTrailingTag(item.Title); bare != "" && bare != item.Title {
				if r2, err2 := c.mangadex.Lookup(ctx, bare); err2 == nil {
					md, mdErr = r2, nil
				}
			}
		}
		if mdErr == nil {
			if country := mangaDexLangToCountry(md.OriginalLanguage); country != "" {
				synth := anilist.Result{CountryOfOrigin: country}
				for _, r := range rules {
					if r.Condition.IsPathOnly() {
						continue
					}
					// Only country-of-origin rules can match a MangaDex
					// synthetic result — isAdult/format aren't populated
					// from this source, so a rule constraining those axes
					// correctly won't fire.
					if matchesRule(r.Condition, synth, item.ParentDir) {
						return model.Decision{
							BindingID: r.BindingID,
							Via:       fmt.Sprintf("%s%d", ViaMangaDexRulePrefix, r.ID),
						}, nil
					}
				}
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

// stripTrailingTag drops a single trailing parenthetical or bracketed
// tag from the title plus any whitespace that immediately precedes it.
// Used by the AniList retry path so titles like
//
//	"Dragon Ball Super (Color)"
//	"Made in Abyss [Official Edition]"
//	"Series Name (2024)"
//
// also classify when only the bare title is present in AniList's
// catalogue. Returns the input unchanged when no trailing tag is
// present so callers can compare original vs result to decide whether
// to retry. Only strips ONE trailing group — chained tags are rare
// enough that the second pass would risk losing real title content.
func stripTrailingTag(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	last := s[len(s)-1]
	var open byte
	switch last {
	case ')':
		open = '('
	case ']':
		open = '['
	default:
		return s
	}
	// Walk back to the matching opener, respecting nested groups so
	// "Foo (Bar (1)) " produces "Foo".
	depth := 1
	for i := len(s) - 2; i >= 0; i-- {
		switch s[i] {
		case last:
			depth++
		case open:
			depth--
			if depth == 0 {
				return strings.TrimRight(s[:i], " \t")
			}
		}
	}
	// Unbalanced — leave the input alone.
	return s
}
