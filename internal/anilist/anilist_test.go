package anilist

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLookupReturnsIsAdultAndFormat is the truth-statement test for
// Plan A Task 8: Lookup carries CountryOfOrigin AND IsAdult AND Format
// from a single GraphQL round-trip.
func TestLookupReturnsIsAdultAndFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":{"Media":{"countryOfOrigin":"JP","isAdult":true,"format":"NOVEL"}}}`)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL)
	got, err := c.Lookup(context.Background(), "Some Title")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.CountryOfOrigin != "JP" {
		t.Errorf("CountryOfOrigin: want JP, got %q", got.CountryOfOrigin)
	}
	if got.IsAdult != true {
		t.Errorf("IsAdult: want true, got %v", got.IsAdult)
	}
	if got.Format != "NOVEL" {
		t.Errorf("Format: want NOVEL, got %q", got.Format)
	}
}

// TestLookupRequestsAllThreeFieldsInOneCall guards the rate-limit promise:
// IsAdult/Format must not cost an extra round trip. We assert the
// outbound GraphQL body asks for all three fields in one query.
func TestLookupRequestsAllThreeFieldsInOneCall(t *testing.T) {
	var calls int
	var seenBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":{"Media":{"countryOfOrigin":"KR","isAdult":false,"format":"MANGA"}}}`)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL)
	if _, err := c.Lookup(context.Background(), "Solo Leveling"); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if calls != 1 {
		t.Errorf("want exactly 1 GraphQL call, got %d", calls)
	}
	for _, field := range []string{"countryOfOrigin", "isAdult", "format"} {
		if !strings.Contains(seenBody, field) {
			t.Errorf("outbound query missing %q field; body=%s", field, seenBody)
		}
	}
}

// TestLookupReturnsErrNotFoundOnNullMedia keeps the "no match" path
// distinguishable from transport errors so the classifier can map it
// to TypeUnknown without confusing it with an outage.
func TestLookupReturnsErrNotFoundOnNullMedia(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":{"Media":null}}`)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL)
	_, err := c.Lookup(context.Background(), "Nonexistent Series")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// TestLookupErrorsOnRateLimit guards the 429 path the classifier uses
// to back off.
func TestLookupErrorsOnRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL)
	_, err := c.Lookup(context.Background(), "anything")
	if err == nil {
		t.Fatal("want rate-limit error, got nil")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error should mention rate limit; got %v", err)
	}
}

// TestLookupErrorsOn5xx covers transient AniList outages.
func TestLookupErrorsOn5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL)
	_, err := c.Lookup(context.Background(), "anything")
	if err == nil {
		t.Fatal("want 5xx error, got nil")
	}
	if !strings.Contains(err.Error(), "status 502") {
		t.Errorf("error should mention status 502; got %v", err)
	}
}

// TestLookupRespectsContextCancellation ensures the classifier can
// abort an in-flight call when the poller tick times out.
func TestLookupRespectsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":{"Media":{"countryOfOrigin":"JP","isAdult":false,"format":"MANGA"}}}`)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Lookup(ctx, "anything")
	if err == nil {
		t.Fatal("want context-cancelled error, got nil")
	}
}

// TestNewDefaultsEmptyEndpoint guards the "" → DefaultEndpoint default
// so production callers don't accidentally hit a localhost stub.
func TestNewDefaultsEmptyEndpoint(t *testing.T) {
	c := New("")
	if c.endpoint != DefaultEndpoint {
		t.Errorf("empty endpoint should default to %q, got %q", DefaultEndpoint, c.endpoint)
	}
}
