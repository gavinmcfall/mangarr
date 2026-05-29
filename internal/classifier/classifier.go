package classifier

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

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
// for Library Map overrides. *suwayomi.PathCache satisfies this directly;
// tests can pass a hand-built stub instead of standing up a full cache.
//
// A nil PathLookup disables override resolution — classifier behaviour is
// then identical to the pre-Library-Map AniList path.
type PathLookup interface {
	Lookup(parentDir string) (suwayomi.CacheEntry, bool)
}

// SettingsReader is the subset of store.Store the classifier needs to
// read the user's current SuwayomiCategoryOverrides map. Kept tiny so the
// classifier doesn't take a dependency on the whole store.
//
// A nil SettingsReader disables override resolution.
type SettingsReader interface {
	GetSettings() (model.Settings, error)
}

type Classifier struct {
	endpoint string
	http     *http.Client
	cache    Cache       // may be nil
	Metrics  MetricsSink // optional; nil disables all metric calls

	// Library Map (Plan B). Both must be non-nil for the override path
	// to engage; either being nil short-circuits to the AniList path.
	pathCache PathLookup
	settings  SettingsReader
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

// Via reason strings produced by ClassifySeries. The spec example used
// the human-readable category name ("Korean Webtoons"), but the
// classifier here only has the category ID from the Suwayomi PathCache —
// resolving names would require an extra Suwayomi round-trip. Plan C's
// activity log renderer can resolve names at display time from the
// Suwayomi categories endpoint.
const (
	ViaSuwayomiOverridePrefix = "suwayomi-override:category=" // + <int64 category ID>
	ViaAniListPrefix          = "anilist:"                    // + <ISO-3166-1 alpha-2 country code>
	ViaUnmatched              = "unmatched"
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

	ct, err := c.Classify(s.Title)
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

// Classify returns the content type for a title, or TypeUnknown if AniList
// has no match or the country is unmapped. Results are read from and written
// to the cache (when a Cache is configured) to respect AniList's rate limit.
func (c *Classifier) Classify(title string) (model.ContentType, error) {
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
