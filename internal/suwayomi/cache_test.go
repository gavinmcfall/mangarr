package suwayomi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
)

// libraryStubServer returns a server that always answers the GraphQL
// library query with the given mangas.
func libraryStubServer(t *testing.T, mangas []gqlMangaNode) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/graphql" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		resp := map[string]any{"data": map[string]any{
			"mangas": map[string]any{"nodes": mangas},
		}}
		b, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	}))
}

func TestRefreshBuildsCrossProductEntries(t *testing.T) {
	srv := libraryStubServer(t, []gqlMangaNode{
		{
			ID: 1, Title: "Solo Leveling", SourceID: "100",
			Source: &gqlSource{DisplayName: "MangaDex"},
			Categories: gqlCategoryConn{Nodes: []gqlCatNode{
				{ID: 10, Order: 1},
				{ID: 11, Order: 0},
			}},
		},
	})
	defer srv.Close()

	c := New(srv.URL, NoAuth{})
	cache := NewPathCache()
	roots := []string{"/media/Downloads/suwayomi", "/data/manga"}
	if err := cache.Refresh(context.Background(), c, roots); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	want := []string{
		filepath.Clean("/media/Downloads/suwayomi/MangaDex/Solo Leveling"),
		filepath.Clean("/data/manga/MangaDex/Solo Leveling"),
	}
	for _, k := range want {
		entry, ok := cache.Lookup(k)
		if !ok {
			t.Fatalf("Lookup(%q): not found; cache size=%d", k, cache.Size())
		}
		if entry.MangaID != 1 {
			t.Errorf("Lookup(%q): MangaID=%d want 1", k, entry.MangaID)
		}
		// Category IDs should be in ascending-order order: 11 (order=0) then 10 (order=1).
		if len(entry.CategoryIDs) != 2 || entry.CategoryIDs[0] != 11 || entry.CategoryIDs[1] != 10 {
			t.Errorf("Lookup(%q): CategoryIDs=%v want [11 10]", k, entry.CategoryIDs)
		}
		if entry.RefreshedAt.IsZero() {
			t.Errorf("Lookup(%q): RefreshedAt zero", k)
		}
	}
}

func TestRefreshFailurePreservesPreviousEntries(t *testing.T) {
	var serveOK bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !serveOK {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, "boom")
			return
		}
		resp := `{"data":{"mangas":{"nodes":[
			{"id":7,"title":"One Piece","sourceId":"1","source":{"displayName":"MangaDex"},"categories":{"nodes":[{"id":1,"order":0}]}}
		]}}}`
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, resp)
	}))
	defer srv.Close()

	c := New(srv.URL, NoAuth{})
	cache := NewPathCache()

	// First Refresh succeeds.
	serveOK = true
	roots := []string{"/media/dl"}
	if err := cache.Refresh(context.Background(), c, roots); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	key := filepath.Clean("/media/dl/MangaDex/One Piece")
	if _, ok := cache.Lookup(key); !ok {
		t.Fatalf("first Refresh: key %q missing", key)
	}

	// Second Refresh fails.
	serveOK = false
	if err := cache.Refresh(context.Background(), c, roots); err == nil {
		t.Fatal("second Refresh: want error, got nil")
	}

	// Entry from first Refresh must still be there.
	if _, ok := cache.Lookup(key); !ok {
		t.Fatalf("after failed Refresh: cached entry %q lost", key)
	}
}

func TestLookupConcurrentWithRefresh(t *testing.T) {
	srv := libraryStubServer(t, []gqlMangaNode{
		{
			ID: 1, Title: "Series", SourceID: "1",
			Source:     &gqlSource{DisplayName: "S"},
			Categories: gqlCategoryConn{Nodes: []gqlCatNode{{ID: 1, Order: 0}}},
		},
	})
	defer srv.Close()

	c := New(srv.URL, NoAuth{})
	cache := NewPathCache()
	roots := []string{"/dl"}
	if err := cache.Refresh(context.Background(), c, roots); err != nil {
		t.Fatalf("seed Refresh: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Refresh in a loop until stopped.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = cache.Refresh(context.Background(), c, roots)
			}
		}
	}()

	// Concurrent lookups. Must not race (run with -race).
	var lookups sync.WaitGroup
	for i := 0; i < 4; i++ {
		lookups.Add(1)
		go func() {
			defer lookups.Done()
			for j := 0; j < 500; j++ {
				cache.Lookup("/dl/S/Series")
				cache.Lookup("/no/match")
			}
		}()
	}

	lookups.Wait()
	close(stop)
	wg.Wait()
}

func TestLookupReturnsFalseOnMiss(t *testing.T) {
	cache := NewPathCache()
	if _, ok := cache.Lookup("/nope"); ok {
		t.Fatal("Lookup on empty cache should miss")
	}
}
