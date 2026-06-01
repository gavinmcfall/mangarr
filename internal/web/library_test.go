package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/model"
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

// TestAPILibraryRowMissingReturnsFragment pins the happy path: GET
// /api/library/{mangaId}/missing returns the 3-cell HTMX fragment with
// total/downloaded/missing counts and refreshes library_cache so the
// next read is warm.
func TestAPILibraryRowMissingReturnsFragment(t *testing.T) {
	h, st, sw := newTestHandler()
	st.libraryCache = map[int64]model.LibraryCacheEntry{
		7: {MangaID: 7, Title: "One Piece", SourceID: "42", SourceName: "MangaDex EN"},
	}
	// 3 chapters, 1 downloaded → Missing = 2
	sw.chaptersForManga = map[int64][]int64{7: {100, 101, 102}}
	sw.chaptersDownloaded = map[int64]bool{100: true}

	req := httptest.NewRequest(http.MethodGet, "/api/library/7/missing", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Asserts the fragment contains the 3 numeric values in cell shape.
	for _, want := range []string{">3<", ">1<", ">2<"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in fragment body; got:\n%s", want, body)
		}
	}
	// Confirm the cache write happened — counts must persist for the
	// next page load.
	got, err := st.GetLibraryCacheEntry(7)
	if err != nil {
		t.Fatalf("post-fragment GetLibraryCacheEntry(7) failed: %v", err)
	}
	if got.TotalChapters != 3 || got.Downloaded != 1 {
		t.Errorf("cache not refreshed: total=%d downloaded=%d", got.TotalChapters, got.Downloaded)
	}
}

// TestAPILibraryRowMissingReturns404WhenNotInCache pins the orphaned-row
// branch: a row whose manga isn't in library_cache returns 404 so HTMX
// swaps a visible "not in cache" indicator rather than blank cells.
func TestAPILibraryRowMissingReturns404WhenNotInCache(t *testing.T) {
	h, _, _ := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/api/library/999/missing", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}

// TestAPILibraryRowMissingReturns400OnBadID pins the invalid-input
// branch: a non-numeric manga ID returns 400 before any store/Suwayomi
// roundtrip.
func TestAPILibraryRowMissingReturns400OnBadID(t *testing.T) {
	h, _, _ := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/api/library/abc/missing", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}

// TestAPILibraryRowMissingReturns503WhenSuwayomiUnconfigured pins the
// "Suwayomi not wired" branch — even with a cached library entry, the
// handler can't fetch counts so it surfaces a clean 503.
func TestAPILibraryRowMissingReturns503WhenSuwayomiUnconfigured(t *testing.T) {
	st := &fakeStore{
		libraryCache: map[int64]model.LibraryCacheEntry{
			7: {MangaID: 7, Title: "One Piece"},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})
	req := httptest.NewRequest(http.MethodGet, "/api/library/7/missing", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", rec.Code, rec.Body.String())
	}
}
