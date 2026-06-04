// Package mangadex is a focused MangaDex REST client used as a classifier
// fallback when AniList has no MANGA entry for a title.
//
// AniList's catalogue has real gaps — popular manhwa/manhua whose only
// AniList record is the anime adaptation (with a misleading
// countryOfOrigin), or that simply aren't catalogued as MANGA at all.
// MangaDex's manga catalogue is far more complete for non-Japanese comics
// and exposes originalLanguage directly.
//
// This package intentionally does NOT do caching, metrics, or any
// language-to-country mapping. Those concerns live in the classifier,
// which composes this client with a cache (mirroring the AniList cache)
// and maps originalLanguage onto the country-of-origin axis the rules
// already reason over.
//
// Mapping note (decided with the operator): originalLanguage is a
// PUBLICATION-language field, not a country-of-origin field. ko/ja/zh map
// cleanly to KR/JP/CN. en does NOT map — an officially-English-produced
// manhwa, an OEL comic, and an English light novel are indistinguishable
// by this field, so en-origin titles fall through to the default/unmatched
// path and are handled by a manual override. The classifier owns that
// policy; this client just returns the raw originalLanguage.
package mangadex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// DefaultEndpoint is MangaDex's public API base. Pass "" to New to use it.
const DefaultEndpoint = "https://api.mangadex.org"

// Result is the subset of MangaDex manga attributes the classifier needs.
//
// OriginalLanguage is an ISO 639-1 code (e.g. "ko", "ja", "zh", "en").
// The classifier maps the CJK subset onto its country-of-origin rules.
type Result struct {
	OriginalLanguage string
	// ContentRating is MangaDex's rating ("safe" | "suggestive" |
	// "erotica" | "pornographic"). Surfaced so a future rule could split
	// adult variants without an AniList isAdult round-trip; the current
	// classifier doesn't consume it yet.
	ContentRating string
}

// Client issues title-search lookups against a MangaDex-compatible API.
// The zero value is NOT useful; construct with New.
type Client struct {
	endpoint string
	http     *http.Client
}

// New returns a Client that queries the given endpoint. Pass "" for the
// production MangaDex API (DefaultEndpoint). Tests pass an httptest.Server
// URL here.
func New(endpoint string) *Client {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	return &Client{endpoint: endpoint, http: &http.Client{Timeout: 15 * time.Second}}
}

// ErrNotFound is returned when MangaDex returns an empty result set for
// the search title. The classifier translates this into a fall-through to
// the default/unmatched path, mirroring anilist.ErrNotFound.
var ErrNotFound = fmt.Errorf("mangadex: no manga match")

// searchResponse is the subset of the /manga response we decode.
type searchResponse struct {
	Result string `json:"result"`
	Data   []struct {
		Attributes struct {
			OriginalLanguage string `json:"originalLanguage"`
			ContentRating    string `json:"contentRating"`
			Title            map[string]string `json:"title"`
		} `json:"attributes"`
	} `json:"data"`
}

// Lookup searches MangaDex for the given title and returns the first
// result's attributes. MangaDex orders results by relevance, so the first
// entry is the best title match.
//
// The returned error is one of:
//   - ErrNotFound when the result set is empty
//   - a wrapped error for transport / non-2xx responses
//   - a wrapped error for JSON decode failures
func (c *Client) Lookup(ctx context.Context, title string) (Result, error) {
	u := c.endpoint + "/manga?" + url.Values{
		"title":            {title},
		"limit":            {"1"},
		"order[relevance]": {"desc"},
	}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Result{}, fmt.Errorf("mangadex request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("mangadex do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return Result{}, fmt.Errorf("mangadex rate limited")
	}
	if resp.StatusCode >= 400 {
		return Result{}, fmt.Errorf("mangadex status %d", resp.StatusCode)
	}
	var out searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Result{}, fmt.Errorf("mangadex decode: %w", err)
	}
	if len(out.Data) == 0 {
		return Result{}, ErrNotFound
	}
	a := out.Data[0].Attributes
	return Result{
		OriginalLanguage: a.OriginalLanguage,
		ContentRating:    a.ContentRating,
	}, nil
}
