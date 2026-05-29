package suwayomi

import (
	"context"
	"path/filepath"
	"sync"
	"time"
)

// CacheEntry is what PathCache.Lookup hands back for a hit. CategoryIDs
// preserves the order from Manga — ascending by Suwayomi category.order —
// so the classifier's first-match-wins walk is deterministic.
type CacheEntry struct {
	MangaID     int64
	Title       string
	CategoryIDs []int64
	RefreshedAt time.Time
}

// PathCache maps canonical-cleaned parent-directory paths to the Suwayomi
// manga that owns them. The classifier consults this cache before falling
// through to AniList classification.
//
// Refresh is intended to be called once at startup and at the top of each
// poller tick. Lookup is the file-time hot path and is safe to call
// concurrently with Refresh — Refresh swaps the underlying map atomically.
//
// A failed Refresh leaves previously-cached entries in place. Cache miss
// → caller falls through to AniList (handled by the classifier, not here).
type PathCache struct {
	mu      sync.RWMutex
	entries map[string]CacheEntry
}

// NewPathCache returns an empty cache.
func NewPathCache() *PathCache {
	return &PathCache{entries: map[string]CacheEntry{}}
}

// Refresh rebuilds the cache from the Suwayomi library. For every manga
// in the user's library it joins the per-manga DownloadDir (a relative
// path under Suwayomi's downloads root) with every downloadRoots[] entry
// and stores both the joined form and the filepath.Clean'd form as keys.
//
// On error the previous map is left in place — callers can keep serving
// the last good snapshot until Suwayomi recovers. Returns the error from
// the underlying client call.
func (c *PathCache) Refresh(ctx context.Context, client *Client, downloadRoots []string) error {
	mangas, err := client.ListLibraryWithCategories(ctx)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	roots := len(downloadRoots)
	if roots < 1 {
		roots = 1
	}
	next := make(map[string]CacheEntry, len(mangas)*roots*2)

	for _, m := range mangas {
		entry := CacheEntry{
			MangaID:     m.ID,
			Title:       m.Title,
			CategoryIDs: append([]int64(nil), m.CategoryIDs...),
			RefreshedAt: now,
		}
		for _, root := range downloadRoots {
			if root == "" {
				continue
			}
			joined := filepath.Join(root, m.DownloadDir)
			cleaned := filepath.Clean(joined)
			next[joined] = entry
			next[cleaned] = entry
		}
	}

	c.mu.Lock()
	c.entries = next
	c.mu.Unlock()
	return nil
}

// Lookup returns the cache entry whose key matches the canonical-cleaned
// parent directory of a chapter file. Callers should pass
// filepath.Clean(filepath.Dir(chapterPath)) — Lookup also Cleans its
// argument defensively so a slightly off-shape key still hits.
func (c *PathCache) Lookup(parentDir string) (CacheEntry, bool) {
	key := filepath.Clean(parentDir)
	c.mu.RLock()
	defer c.mu.RUnlock()
	if entry, ok := c.entries[key]; ok {
		return entry, true
	}
	if entry, ok := c.entries[parentDir]; ok {
		return entry, true
	}
	return CacheEntry{}, false
}

// Size returns the current number of cached keys. Useful for the Test
// button + metrics — not part of the classifier path.
func (c *PathCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}
