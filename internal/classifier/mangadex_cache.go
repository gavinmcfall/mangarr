package classifier

import (
	"context"
	"errors"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/gavinmcfall/mangarr/internal/mangadex"
)

// CachingMangaDexClient wraps a MangaDexClient with an in-memory TTL cache,
// mirroring CachingAniListClient. MangaDex's rate limit is more generous
// than AniList's (~5 req/sec vs 30 req/min), but the classifier still runs
// once per series per poll tick — caching keeps repeat polls of the same
// title off the network entirely once resolved.
//
// 24h for successful results (a manga's originalLanguage never changes),
// 6h for NotFound so a transient outage clears within a poll cycle.
// Transport errors are NOT cached so the next tick retries fresh.
type CachingMangaDexClient struct {
	inner MangaDexClient

	successTTL  time.Duration
	notFoundTTL time.Duration

	mu sync.RWMutex
	m  map[string]mdCacheEntry

	// sf collapses concurrent cold-key lookups for the same title into a
	// single inner call (thundering-herd protection).
	sf singleflight.Group
}

type mdCacheEntry struct {
	result    mangadex.Result
	notFound  bool
	expiresAt time.Time
}

// NewCachingMangaDexClient wraps inner with default TTLs (24h success, 6h
// negative). Zero-valued TTLs fall back to those defaults.
func NewCachingMangaDexClient(inner MangaDexClient, successTTL, notFoundTTL time.Duration) *CachingMangaDexClient {
	if successTTL <= 0 {
		successTTL = 24 * time.Hour
	}
	if notFoundTTL <= 0 {
		notFoundTTL = 6 * time.Hour
	}
	return &CachingMangaDexClient{
		inner:       inner,
		successTTL:  successTTL,
		notFoundTTL: notFoundTTL,
		m:           make(map[string]mdCacheEntry),
	}
}

// Lookup serves from cache when fresh, else delegates and stores. Transport
// errors are returned verbatim and NOT cached.
func (c *CachingMangaDexClient) Lookup(ctx context.Context, title string) (mangadex.Result, error) {
	c.mu.RLock()
	e, ok := c.m[title]
	c.mu.RUnlock()
	if ok && time.Now().Before(e.expiresAt) {
		if e.notFound {
			return mangadex.Result{}, mangadex.ErrNotFound
		}
		return e.result, nil
	}

	v, err, _ := c.sf.Do(title, func() (interface{}, error) {
		res, err := c.inner.Lookup(ctx, title)
		switch {
		case err == nil:
			c.store(title, mdCacheEntry{result: res, expiresAt: time.Now().Add(c.successTTL)})
		case errors.Is(err, mangadex.ErrNotFound):
			c.store(title, mdCacheEntry{notFound: true, expiresAt: time.Now().Add(c.notFoundTTL)})
		default:
			// transport / rate-limit / non-2xx — not cached; retry next tick.
		}
		return res, err
	})
	if err != nil {
		return mangadex.Result{}, err
	}
	return v.(mangadex.Result), nil
}

func (c *CachingMangaDexClient) store(title string, e mdCacheEntry) {
	c.mu.Lock()
	c.m[title] = e
	c.mu.Unlock()
}
