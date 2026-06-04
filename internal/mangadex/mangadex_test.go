package mangadex

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLookupParsesOriginalLanguage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("title"); got != "Solo Leveling" {
			t.Errorf("title query: want %q, got %q", "Solo Leveling", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"ok","data":[{"attributes":{"originalLanguage":"ko","contentRating":"safe","title":{"en":"Solo Leveling"}}}]}`))
	}))
	defer srv.Close()

	got, err := New(srv.URL).Lookup(context.Background(), "Solo Leveling")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.OriginalLanguage != "ko" {
		t.Errorf("OriginalLanguage: want ko, got %q", got.OriginalLanguage)
	}
	if got.ContentRating != "safe" {
		t.Errorf("ContentRating: want safe, got %q", got.ContentRating)
	}
}

func TestLookupReturnsNotFoundOnEmptyData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":"ok","data":[]}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL).Lookup(context.Background(), "Nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestLookupTakesFirstResult(t *testing.T) {
	// MangaDex orders by relevance; the client requests limit=1 but a
	// server returning multiple must still yield the first.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":"ok","data":[
			{"attributes":{"originalLanguage":"zh","contentRating":"safe","title":{"en":"First"}}},
			{"attributes":{"originalLanguage":"ja","contentRating":"safe","title":{"en":"Second"}}}
		]}`))
	}))
	defer srv.Close()

	got, err := New(srv.URL).Lookup(context.Background(), "whatever")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.OriginalLanguage != "zh" {
		t.Errorf("OriginalLanguage: want zh (first result), got %q", got.OriginalLanguage)
	}
}

func TestLookupRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := New(srv.URL).Lookup(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("want rate-limited error, got %v", err)
	}
}

func TestLookupNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := New(srv.URL).Lookup(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("want status 500 error, got %v", err)
	}
}
