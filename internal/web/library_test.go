package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/suwayomi"
)

// TestAPILibrarySyncWritesLibraryCache pins the Plan A→B unblocker: POST
// /api/library/sync fetches every manga from Suwayomi via
// ListLibraryWithCategories and upserts one library_cache row per manga.
// Chapter counts (total / downloaded) stay 0 — Plan B T2's per-row
// fragment endpoint fills them in lazily.
func TestAPILibrarySyncWritesLibraryCache(t *testing.T) {
	h, st, sw := newTestHandler()
	sw.libraryEntries = []suwayomi.Manga{
		{ID: 7, Title: "One Piece", SourceID: "42", Source: "MangaDex EN"},
		{ID: 8, Title: "SOLO LEVELING", SourceID: "42", Source: "MangaDex EN"},
		{ID: 9, Title: "The Beginning After the End", SourceID: "99", Source: "Mangapark"},
	}

	req := httptest.NewRequest(http.MethodPost, "/api/library/sync", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Body.String(), `{"synced":3}`; got != want {
		t.Errorf("body: want %q, got %q", want, got)
	}
	if len(st.savedLibraryEntries) != 3 {
		t.Fatalf("expected 3 library_cache writes, got %d", len(st.savedLibraryEntries))
	}
	first := st.savedLibraryEntries[0]
	if first.MangaID != 7 || first.Title != "One Piece" || first.SourceID != "42" || first.SourceName != "MangaDex EN" {
		t.Errorf("first entry: %+v", first)
	}
	// Counts must stay at 0 — Plan B T2 owns those.
	if first.TotalChapters != 0 || first.Downloaded != 0 {
		t.Errorf("counts must stay 0 on sync (T2 owns them); got total=%d downloaded=%d",
			first.TotalChapters, first.Downloaded)
	}
	// Confirm a subsequent GetLibraryCacheEntry sees the write — this is
	// the contract POST /api/bulk depends on.
	if _, err := st.GetLibraryCacheEntry(9); err != nil {
		t.Errorf("post-sync GetLibraryCacheEntry(9) should succeed; got %v", err)
	}
}

// TestAPILibrarySyncReturns503WhenSuwayomiUnconfigured pins the
// "Suwayomi not wired" branch — operators who haven't configured the
// connection in Settings get a clean 503, not a panic.
func TestAPILibrarySyncReturns503WhenSuwayomiUnconfigured(t *testing.T) {
	// Build a handler WITHOUT a Suwayomi fake.
	h := NewHandler(HandlerOpts{Store: &fakeStore{}, Runner: &fakeRunner{}})

	req := httptest.NewRequest(http.MethodPost, "/api/library/sync", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 when suwayomi nil, got %d: %s", rec.Code, rec.Body.String())
	}
}
