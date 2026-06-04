package classifier

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/gavinmcfall/mangarr/internal/anilist"
)

// CachingAniListClient wraps an AniListClient with an in-memory TTL cache.
//
// Why: AniList's public GraphQL endpoint enforces a hard 30-requests/minute
// rate limit (down from the documented 90 since their 2025 throttling change).
// The classifier runs once per series per poll tick — with 20+ series and a
// 15-minute poll interval, two consecutive polls can blow the budget. Once
// the limit hits, the AniListClient.Lookup returns an "anilist rate limited"
// error and the classifier falls through to step 6 (Unmatched) — even for
// series it correctly classified moments earlier. Operators see classifier
// non-determinism that's actually just rate-limit flapping.
//
// Strategy: cache successful results for 24h (manga country-of-origin /
// adult-flag / format don't change), and cache NotFound results for 6h so
// transient AniList outages don't pin titles as permanently unknown.
//
// All other errors (network, 5xx, rate-limit) are NOT cached so the next
// call retries fresh. Negative caching only applies to AniList's definitive
// "no match" signal.
type CachingAniListClient struct {
	inner AniListClient

	successTTL  time.Duration // Result hits — 24h is a safe default
	notFoundTTL time.Duration // ErrNotFound — 6h, so a temporary blip clears within a poll cycle

	mu sync.RWMutex
	m  map[string]cacheEntry
}

type cacheEntry struct {
	result    anilist.Result
	notFound  bool      // true when the AniList probe came back Media: null
	expiresAt time.Time // wall clock; entry is stale when time.Now().After(expiresAt)
}

// NewCachingAniListClient wraps inner with default TTLs (24h success, 6h
// negative). Zero-valued TTLs fall back to those defaults — passing
// time.Duration(0) does NOT disable the cache.
func NewCachingAniListClient(inner AniListClient, successTTL, notFoundTTL time.Duration) *CachingAniListClient {
	if successTTL <= 0 {
		successTTL = 24 * time.Hour
	}
	if notFoundTTL <= 0 {
		notFoundTTL = 6 * time.Hour
	}
	return &CachingAniListClient{
		inner:       inner,
		successTTL:  successTTL,
		notFoundTTL: notFoundTTL,
		m:           make(map[string]cacheEntry),
	}
}

// Lookup returns the AniList result for title, served from cache when fresh.
// A cache miss delegates to the inner client and stores the result. Transport
// errors (rate-limited, 5xx, network) are returned verbatim and NOT cached —
// the next call retries from scratch.
func (c *CachingAniListClient) Lookup(ctx context.Context, title string) (anilist.Result, error) {
	c.mu.RLock()
	e, ok := c.m[title]
	c.mu.RUnlock()
	if ok && time.Now().Before(e.expiresAt) {
		if e.notFound {
			return anilist.Result{}, anilist.ErrNotFound
		}
		return e.result, nil
	}

	res, err := c.inner.Lookup(ctx, title)
	switch {
	case err == nil:
		c.store(title, cacheEntry{result: res, expiresAt: time.Now().Add(c.successTTL)})
	case errors.Is(err, anilist.ErrNotFound):
		c.store(title, cacheEntry{notFound: true, expiresAt: time.Now().Add(c.notFoundTTL)})
	default:
		// transport / rate-limit / non-2xx — do NOT cache; the caller (the
		// classifier) will fall through to step 5/6 this tick, but the
		// next tick gets to try again.
	}
	return res, err
}

func (c *CachingAniListClient) store(title string, e cacheEntry) {
	c.mu.Lock()
	c.m[title] = e
	c.mu.Unlock()
}
