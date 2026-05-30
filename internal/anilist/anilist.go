// Package anilist is a focused AniList GraphQL client that returns the
// raw fields the classifier needs to evaluate ClassificationRules.
//
// This package intentionally does NOT do caching, metrics, or any
// country-code-to-ContentType mapping. Those concerns live in the
// classifier (Plan A Task 9), which composes this client with a cache
// and a metrics sink to produce a model.Decision.
//
// The signature Lookup(ctx, title) (Result, error) is stable and is the
// contract the classifier's six-step Classify flow depends on.
package anilist

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// DefaultEndpoint is AniList's public GraphQL URL. Pass "" to New to use it.
const DefaultEndpoint = "https://graphql.anilist.co"

// Result is the subset of AniList Media fields the classifier reasons over.
//
//   - CountryOfOrigin drives the legacy JP/KR/CN routing (Manga / Manhwa / Manhua).
//   - IsAdult lets rules split adult variants (e.g. Hentai vs Manga) onto a
//     dedicated Kavita library without the user maintaining a separate title list.
//   - Format distinguishes prose (NOVEL) from comics so Light Novels can be
//     routed to a non-comic Kavita library.
//
// All three fields come from a single GraphQL round-trip — there is no
// extra rate-limit cost for IsAdult/Format beyond CountryOfOrigin alone.
type Result struct {
	CountryOfOrigin string
	IsAdult         bool
	Format          string
}

// Client issues GraphQL Lookup calls against an AniList-compatible endpoint.
// The zero value is NOT useful; construct with New.
type Client struct {
	endpoint string
	http     *http.Client
}

// New returns a Client that queries the given endpoint. Pass "" for the
// production AniList endpoint (DefaultEndpoint). Tests pass an
// httptest.Server URL here.
func New(endpoint string) *Client {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	return &Client{endpoint: endpoint, http: &http.Client{Timeout: 15 * time.Second}}
}

// query asks for the three fields the classifier needs. AniList counts
// this as a single request regardless of the field count, so widening
// the selection has zero rate-limit cost.
const query = `query ($s: String) { Media(search: $s, type: MANGA) { countryOfOrigin isAdult format } }`

type response struct {
	Data struct {
		Media *struct {
			CountryOfOrigin string `json:"countryOfOrigin"`
			IsAdult         bool   `json:"isAdult"`
			Format          string `json:"format"`
		} `json:"Media"`
	} `json:"data"`
}

// ErrNotFound is returned when AniList responds with `Media: null` —
// i.e. no series matched the search title. Callers (the classifier's
// six-step flow) translate this into a TypeUnknown / Unmatched Decision
// rather than propagating it as a hard error.
var ErrNotFound = fmt.Errorf("anilist: no media match")

// Lookup performs a single GraphQL query for the given title and
// returns the widened Result. Cache reads, metrics, and ContentType
// mapping are the caller's responsibility.
//
// The returned error is one of:
//   - ErrNotFound when AniList returns `Media: null` (no match)
//   - a wrapped error for transport / rate-limit / non-2xx responses
//   - a wrapped error for JSON decode failures
func (c *Client) Lookup(ctx context.Context, title string) (Result, error) {
	body, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": map[string]string{"s": title},
	})
	if err != nil {
		return Result{}, fmt.Errorf("anilist marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("anilist request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("anilist do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return Result{}, fmt.Errorf("anilist rate limited")
	}
	if resp.StatusCode >= 400 {
		return Result{}, fmt.Errorf("anilist status %d", resp.StatusCode)
	}
	var out response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Result{}, fmt.Errorf("anilist decode: %w", err)
	}
	if out.Data.Media == nil {
		return Result{}, ErrNotFound
	}
	return Result{
		CountryOfOrigin: out.Data.Media.CountryOfOrigin,
		IsAdult:         out.Data.Media.IsAdult,
		Format:          out.Data.Media.Format,
	}, nil
}
