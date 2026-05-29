package classifier

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/model"
)

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
