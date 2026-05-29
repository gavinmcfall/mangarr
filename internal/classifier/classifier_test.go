package classifier

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/suwayomi"
)

// stubPathLookup is a minimal PathLookup for ClassifySeries tests. We
// build a real *suwayomi.PathCache where possible to keep the test as
// close to production wiring as we can; this stub is for the cases that
// want to assert behaviour with a deliberately tiny surface (e.g. "no
// entries" or "a single planted entry").
type stubPathLookup struct {
	entries map[string]suwayomi.CacheEntry
}

func (s *stubPathLookup) Lookup(parentDir string) (suwayomi.CacheEntry, bool) {
	if s == nil {
		return suwayomi.CacheEntry{}, false
	}
	e, ok := s.entries[parentDir]
	return e, ok
}

// stubSettings returns a fixed Settings on every call.
type stubSettings struct{ s model.Settings }

func (s *stubSettings) GetSettings() (model.Settings, error) { return s.s, nil }

// fakeClassifierMetrics records IncAniListLookup calls.
type fakeClassifierMetrics struct {
	counts map[string]int
}

func newFakeClassifierMetrics() *fakeClassifierMetrics {
	return &fakeClassifierMetrics{counts: make(map[string]int)}
}

func (f *fakeClassifierMetrics) IncAniListLookup(result string) { f.counts[result]++ }

// stubCache is an in-memory Cache for testing the classifier's cache path.
type stubCache struct {
	entries map[string]model.ContentType
	writes  int
}

func newStubCache() *stubCache { return &stubCache{entries: map[string]model.ContentType{}} }

func (c *stubCache) GetCachedClassification(titleNorm string) (model.ContentType, bool, error) {
	ct, ok := c.entries[titleNorm]
	return ct, ok, nil
}

func (c *stubCache) CacheClassification(titleNorm string, t model.ContentType) error {
	c.entries[titleNorm] = t
	c.writes++
	return nil
}

func TestClassifyMapsCountry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"Media":{"countryOfOrigin":"KR"}}}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	got, err := c.Classify("Solo Leveling")
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got != model.TypeManhwa {
		t.Fatalf("want Manhwa, got %q", got)
	}
}

func TestClassifyNoMatchReturnsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"Media":null}}`))
	}))
	defer srv.Close()
	got, err := New(srv.URL).Classify("zzzznotreal")
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got != model.TypeUnknown {
		t.Fatalf("want Unknown, got %q", got)
	}
}

func TestClassifyCacheHitSkipsNetwork(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"data":{"Media":{"countryOfOrigin":"JP"}}}`))
	}))
	defer srv.Close()

	cache := newStubCache()
	cache.entries["Solo Leveling"] = model.TypeManhwa // pre-populated (e.g. a manual choice)

	c := NewWithCache(srv.URL, cache)
	got, err := c.Classify("Solo Leveling")
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got != model.TypeManhwa {
		t.Fatalf("want cached Manhwa, got %q", got)
	}
	if calls != 0 {
		t.Fatalf("cache hit should skip the network, but handler was called %d times", calls)
	}
}

func TestClassifyWritesThroughCache(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Write([]byte(`{"data":{"Media":{"countryOfOrigin":"KR"}}}`))
	}))
	defer srv.Close()

	cache := newStubCache()
	c := NewWithCache(srv.URL, cache)

	// First call: cache miss -> network hit -> writes through.
	got, err := c.Classify("Omniscient Reader")
	if err != nil {
		t.Fatalf("classify (first): %v", err)
	}
	if got != model.TypeManhwa {
		t.Fatalf("want Manhwa, got %q", got)
	}
	if calls != 1 {
		t.Fatalf("first call should hit network once, got %d", calls)
	}
	if cache.writes != 1 || cache.entries["Omniscient Reader"] != model.TypeManhwa {
		t.Fatalf("expected write-through to cache, got writes=%d entries=%v", cache.writes, cache.entries)
	}

	// Second call: cache hit -> no additional network.
	got, err = c.Classify("Omniscient Reader")
	if err != nil {
		t.Fatalf("classify (second): %v", err)
	}
	if got != model.TypeManhwa {
		t.Fatalf("want Manhwa on cache hit, got %q", got)
	}
	if calls != 1 {
		t.Fatalf("second call should not hit network, total calls=%d", calls)
	}
}

// ----- metrics tests -----

// TestClassifyMetricsSuccess verifies that a successful network lookup
// increments "success" in the MetricsSink.
func TestClassifyMetricsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"Media":{"countryOfOrigin":"JP"}}}`))
	}))
	defer srv.Close()

	fm := newFakeClassifierMetrics()
	c := New(srv.URL)
	c.Metrics = fm

	if _, err := c.Classify("Berserk"); err != nil {
		t.Fatalf("classify: %v", err)
	}
	if fm.counts["success"] != 1 {
		t.Errorf("want success=1, got %d (counts=%v)", fm.counts["success"], fm.counts)
	}
}

// TestClassifyMetricsMiss verifies that a Media=null response increments "miss".
func TestClassifyMetricsMiss(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"Media":null}}`))
	}))
	defer srv.Close()

	fm := newFakeClassifierMetrics()
	c := New(srv.URL)
	c.Metrics = fm

	if _, err := c.Classify("no such title"); err != nil {
		t.Fatalf("classify: %v", err)
	}
	if fm.counts["miss"] != 1 {
		t.Errorf("want miss=1, got %d (counts=%v)", fm.counts["miss"], fm.counts)
	}
}

// TestClassifyMetricsCached verifies that a cache hit increments "cached"
// and does NOT increment "success".
func TestClassifyMetricsCached(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"Media":{"countryOfOrigin":"JP"}}}`))
	}))
	defer srv.Close()

	cache := newStubCache()
	cache.entries["Solo Leveling"] = model.TypeManhwa

	fm := newFakeClassifierMetrics()
	c := NewWithCache(srv.URL, cache)
	c.Metrics = fm

	if _, err := c.Classify("Solo Leveling"); err != nil {
		t.Fatalf("classify: %v", err)
	}
	if fm.counts["cached"] != 1 {
		t.Errorf("want cached=1, got %d (counts=%v)", fm.counts["cached"], fm.counts)
	}
	if fm.counts["success"] != 0 {
		t.Errorf("want success=0 on cache hit, got %d", fm.counts["success"])
	}
}

// TestClassifyMetricsError verifies that HTTP error responses increment "error".
func TestClassifyMetricsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	fm := newFakeClassifierMetrics()
	c := New(srv.URL)
	c.Metrics = fm

	_, _ = c.Classify("something")
	if fm.counts["error"] != 1 {
		t.Errorf("want error=1 on HTTP 429, got %d (counts=%v)", fm.counts["error"], fm.counts)
	}
}

// ---------- ClassifySeries (Library Map / Plan B) ----------

// TestClassifySeriesOverrideHitSkipsAniList covers the truth statement:
// "While SuwayomiCategoryOverrides is non-empty AND PathCache.Lookup
// hits, the classifier routes via the first matching override and
// skips AniList." We assert the AniList stub is never called and the
// Via tag carries the matched Suwayomi category ID.
func TestClassifySeriesOverrideHitSkipsAniList(t *testing.T) {
	var anilistCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anilistCalls++
		w.Write([]byte(`{"data":{"Media":{"countryOfOrigin":"KR"}}}`))
	}))
	defer srv.Close()

	settings := &stubSettings{s: model.Settings{
		// Map Suwayomi category 42 → Kavita library 2; KavitaLibIDsByType
		// reverse-maps library 2 → TypeManhwa, so the classifier returns
		// TypeManhwa and the override Via.
		SuwayomiCategoryOverrides: map[int64]int64{42: 2},
		KavitaLibIDsByType:        map[model.ContentType]int64{model.TypeManhwa: 2},
	}}
	cache := &stubPathLookup{entries: map[string]suwayomi.CacheEntry{
		"/dl/suwayomi/Solo Leveling": {
			MangaID:     1,
			Title:       "Solo Leveling",
			CategoryIDs: []int64{42, 99},
		},
	}}

	c := New(srv.URL).WithSuwayomi(cache, settings)
	ct, via, err := c.ClassifySeries(model.Series{
		Title:      "Solo Leveling",
		SourcePath: "/dl/suwayomi/Solo Leveling",
	})
	if err != nil {
		t.Fatalf("ClassifySeries: %v", err)
	}
	if anilistCalls != 0 {
		t.Errorf("AniList must not be called on override hit, got %d calls", anilistCalls)
	}
	if ct != model.TypeManhwa {
		t.Errorf("ContentType: want Manhwa, got %q", ct)
	}
	if via != "suwayomi-override:category=42" {
		t.Errorf("Via: want %q, got %q", "suwayomi-override:category=42", via)
	}
}

// TestClassifySeriesOverrideFirstMatchWins asserts that when a manga
// has multiple categoryIDs and more than one is in the override map,
// the FIRST in CategoryIDs order wins (the PathCache hands us the
// list sorted by Suwayomi category.order — Plan A's contract).
func TestClassifySeriesOverrideFirstMatchWins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("AniList must not be hit when an override matches")
	}))
	defer srv.Close()

	settings := &stubSettings{s: model.Settings{
		SuwayomiCategoryOverrides: map[int64]int64{42: 2, 99: 3},
		KavitaLibIDsByType: map[model.ContentType]int64{
			model.TypeManhwa: 2,
			model.TypeManhua: 3,
		},
	}}
	cache := &stubPathLookup{entries: map[string]suwayomi.CacheEntry{
		"/dl/series": {
			MangaID:     1,
			Title:       "Multi-cat",
			CategoryIDs: []int64{42, 99}, // sorted ascending by Suwayomi order
		},
	}}

	c := New(srv.URL).WithSuwayomi(cache, settings)
	ct, via, err := c.ClassifySeries(model.Series{Title: "Multi-cat", SourcePath: "/dl/series"})
	if err != nil {
		t.Fatalf("ClassifySeries: %v", err)
	}
	if ct != model.TypeManhwa { // 42 wins, maps to library 2 = Manhwa
		t.Errorf("first-match-wins: want Manhwa, got %q", ct)
	}
	if via != "suwayomi-override:category=42" {
		t.Errorf("Via: want category=42, got %q", via)
	}
}

// TestClassifySeriesCacheMissFallsThroughToAniList covers:
// "If no override matches OR the PathCache has no entry for the
// parent directory, the classifier shall use the existing AniList
// countryOfOrigin path."
func TestClassifySeriesCacheMissFallsThroughToAniList(t *testing.T) {
	var anilistCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anilistCalls++
		w.Write([]byte(`{"data":{"Media":{"countryOfOrigin":"JP"}}}`))
	}))
	defer srv.Close()

	settings := &stubSettings{s: model.Settings{
		SuwayomiCategoryOverrides: map[int64]int64{42: 2},
	}}
	// Cache has no entry for the queried path → miss → AniList.
	cache := &stubPathLookup{entries: map[string]suwayomi.CacheEntry{}}

	c := New(srv.URL).WithSuwayomi(cache, settings)
	ct, via, err := c.ClassifySeries(model.Series{Title: "Berserk", SourcePath: "/dl/Berserk"})
	if err != nil {
		t.Fatalf("ClassifySeries: %v", err)
	}
	if anilistCalls != 1 {
		t.Errorf("want AniList called once on cache miss, got %d", anilistCalls)
	}
	if ct != model.TypeManga {
		t.Errorf("ContentType: want Manga, got %q", ct)
	}
	if via != "anilist:JP" {
		t.Errorf("Via: want anilist:JP, got %q", via)
	}
}

// TestClassifySeriesEmptyOverridesSkipsCacheLookup pins the
// performance/edge-case contract: when overrides is empty we must not
// even consult the path cache (no point in a Lookup we can't act on),
// and we must hit AniList for the country code as today.
func TestClassifySeriesEmptyOverridesSkipsCacheLookup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"Media":{"countryOfOrigin":"CN"}}}`))
	}))
	defer srv.Close()

	called := false
	cache := &countingLookup{inner: &stubPathLookup{entries: map[string]suwayomi.CacheEntry{
		"/dl/X": {MangaID: 1, CategoryIDs: []int64{42}},
	}}, hit: &called}

	settings := &stubSettings{s: model.Settings{
		SuwayomiCategoryOverrides: map[int64]int64{}, // empty
	}}

	c := New(srv.URL).WithSuwayomi(cache, settings)
	ct, via, err := c.ClassifySeries(model.Series{Title: "X", SourcePath: "/dl/X"})
	if err != nil {
		t.Fatalf("ClassifySeries: %v", err)
	}
	if called {
		t.Error("cache Lookup must not run when SuwayomiCategoryOverrides is empty")
	}
	if ct != model.TypeManhua {
		t.Errorf("want Manhua (CN), got %q", ct)
	}
	if via != "anilist:CN" {
		t.Errorf("Via: want anilist:CN, got %q", via)
	}
}

// TestClassifySeriesUnmatchedViaTag asserts the spec truth statement:
// "When the classifier emits Unmatched, activity log entry shall carry
// Via = 'unmatched'."
func TestClassifySeriesUnmatchedViaTag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"Media":null}}`))
	}))
	defer srv.Close()

	c := New(srv.URL) // no Suwayomi wiring at all
	ct, via, err := c.ClassifySeries(model.Series{Title: "zzz", SourcePath: "/dl/zzz"})
	if err != nil {
		t.Fatalf("ClassifySeries: %v", err)
	}
	if ct != model.TypeUnknown {
		t.Errorf("ContentType: want Unknown, got %q", ct)
	}
	if via != "unmatched" {
		t.Errorf("Via: want unmatched, got %q", via)
	}
}

// TestClassifySeriesNilPathCacheSkipsOverride covers the "nil-safe"
// contract — if the classifier wasn't given a PathCache, the override
// branch is dead and we go straight to AniList.
func TestClassifySeriesNilPathCacheSkipsOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"Media":{"countryOfOrigin":"KR"}}}`))
	}))
	defer srv.Close()

	c := New(srv.URL) // no WithSuwayomi call
	ct, via, err := c.ClassifySeries(model.Series{Title: "Solo Leveling", SourcePath: "/dl/Solo"})
	if err != nil {
		t.Fatalf("ClassifySeries: %v", err)
	}
	if ct != model.TypeManhwa {
		t.Errorf("ContentType: want Manhwa, got %q", ct)
	}
	if via != "anilist:KR" {
		t.Errorf("Via: want anilist:KR, got %q", via)
	}
}

// TestClassifySeriesDuplicateLibIDDeterministic asserts the
// fixed-priority reverse lookup: if two ContentTypes happen to point at
// the same Kavita library ID (a Settings misconfig), the same winner
// must come back on every call. Manga → Manhwa → Manhua priority.
// Without the fix, `range m` would randomise the result and the same
// override would route to different libraries across ticks.
func TestClassifySeriesDuplicateLibIDDeterministic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("AniList must not be hit on override path")
	}))
	defer srv.Close()

	settings := &stubSettings{s: model.Settings{
		SuwayomiCategoryOverrides: map[int64]int64{42: 7},
		// Three ContentTypes all pointing at library 7. Deterministic
		// priority must pick Manga (highest priority) every time.
		KavitaLibIDsByType: map[model.ContentType]int64{
			model.TypeManga:  7,
			model.TypeManhwa: 7,
			model.TypeManhua: 7,
		},
	}}
	cache := &stubPathLookup{entries: map[string]suwayomi.CacheEntry{
		"/dl/dup": {MangaID: 1, CategoryIDs: []int64{42}},
	}}
	c := New(srv.URL).WithSuwayomi(cache, settings)

	// Run many times — map iteration would have flipped the winner by
	// now if we were still walking `range m`.
	for i := 0; i < 50; i++ {
		ct, via, err := c.ClassifySeries(model.Series{Title: "Dup", SourcePath: "/dl/dup"})
		if err != nil {
			t.Fatalf("iter %d: ClassifySeries: %v", i, err)
		}
		if ct != model.TypeManga {
			t.Fatalf("iter %d: want Manga (priority winner), got %q", i, ct)
		}
		if via != "suwayomi-override:category=42" {
			t.Fatalf("iter %d: Via: want suwayomi-override:category=42, got %q", i, via)
		}
	}
}

// countingLookup wraps a PathLookup and flips a flag when Lookup is called.
type countingLookup struct {
	inner *stubPathLookup
	hit   *bool
}

func (c *countingLookup) Lookup(p string) (suwayomi.CacheEntry, bool) {
	*c.hit = true
	return c.inner.Lookup(p)
}

// TestClassifyMetricsNilSafe verifies that a nil Metrics field does not panic.
func TestClassifyMetricsNilSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"Media":{"countryOfOrigin":"JP"}}}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	c.Metrics = nil // explicitly nil — must not panic

	if _, err := c.Classify("Naruto"); err != nil {
		t.Fatalf("classify with nil metrics: %v", err)
	}
}
