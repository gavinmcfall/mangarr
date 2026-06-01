// Package suwayomi wraps the Suwayomi-Server REST + GraphQL surfaces that
// mangarr cares about: connectivity check, category listing, and a single
// shot at the user's library with category memberships eagerly loaded.
//
// The client supports all four upstream auth modes:
//
//	none        — no Authorization header on any request.
//	basic_auth  — RFC 7617 Basic credentials on every request.
//	simple_login — POSTs username/password to /login.html (form-encoded),
//	              captures the JSESSIONID-style cookie, and presents the
//	              cookie on every subsequent request.
//	              (Verified against
//	              github.com/Suwayomi/Suwayomi-Server JavalinSetup.kt —
//	              form fields are "user" + "pass".)
//	ui_login    — Runs the GraphQL `login(input: {username, password})`
//	              mutation, captures `accessToken`, and presents it as
//	              `Authorization: Bearer <accessToken>`. The matching
//	              `refreshToken` mutation is recognised here as a future
//	              upgrade path; for now mangarr just re-logs on 401 since
//	              one login per poll-tick is acceptable.
//	              (Verified against
//	              github.com/Suwayomi/Suwayomi-Server graphql/mutations/
//	              UserMutation.kt.)
//
// All four Auth implementations live in this file and satisfy the same
// Auth interface so the Client can treat them uniformly.
//
// Pattern mirrors internal/kavita: fresh-per-call construction, errors
// wrapped with %w, contexts plumbed through every HTTP call, response
// bodies drained before close, sorted slices from List* methods.
package suwayomi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrAuth is returned to callers when the server keeps rejecting our
// credentials after a re-authenticate + retry cycle.
var ErrAuth = errors.New("suwayomi: authentication failed")

// maxAuthAttempts caps the doJSON retry loop: one initial attempt plus
// one re-auth-and-retry on a 401. Two consecutive 401s surface as
// ErrAuth.
const maxAuthAttempts = 2

// Auth abstracts the four upstream auth modes. Apply mutates the outgoing
// request to carry the right header/cookie. EnsureSession is a no-op for
// stateless modes and performs the login round-trip for session-bearing
// modes when no cached session is held. Invalidate clears any cached
// session so the next request re-logs in.
type Auth interface {
	Apply(ctx context.Context, req *http.Request) error
	EnsureSession(ctx context.Context, base string, httpClient *http.Client) error
	Invalidate()
}

// NoAuth implements Auth for `server.authMode = "none"`.
type NoAuth struct{}

func (NoAuth) Apply(context.Context, *http.Request) error                { return nil }
func (NoAuth) EnsureSession(context.Context, string, *http.Client) error { return nil }
func (NoAuth) Invalidate()                                               {}

// BasicAuth implements Auth for `server.authMode = "basic_auth"`.
type BasicAuth struct {
	Username string
	Password string
}

func (b BasicAuth) Apply(_ context.Context, req *http.Request) error {
	raw := b.Username + ":" + b.Password
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(raw)))
	return nil
}

func (BasicAuth) EnsureSession(context.Context, string, *http.Client) error { return nil }
func (BasicAuth) Invalidate()                                               {}

// SimpleLoginAuth implements Auth for `server.authMode = "simple_login"`.
//
// Upstream wires this through Javalin as a form POST to /login.html with
// fields `user` and `pass`. On success the server sets a session cookie
// (the Javalin session id) which must be presented on every subsequent
// request. We capture every cookie set by the login response and replay
// them as one Cookie header — this keeps us forward-compatible with any
// CSRF/anti-fixation cookies upstream may add.
type SimpleLoginAuth struct {
	Username string
	Password string

	mu      sync.Mutex
	cookies []*http.Cookie
}

func (s *SimpleLoginAuth) Apply(_ context.Context, req *http.Request) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cookies) == 0 {
		return errors.New("suwayomi: simple_login session not established")
	}
	for _, c := range s.cookies {
		req.AddCookie(c)
	}
	return nil
}

func (s *SimpleLoginAuth) EnsureSession(ctx context.Context, base string, httpClient *http.Client) error {
	// Hold the mutex across the network call so two concurrent callers
	// can't both fire a login round-trip. Login is rare (once per cold
	// cache / once per poll tick) so contention is a non-issue and the
	// simple correctness wins. See review thread on Plan A.
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cookies) > 0 {
		return nil
	}

	form := url.Values{}
	form.Set("user", s.Username)
	form.Set("pass", s.Password)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/login.html", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("suwayomi simple_login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("suwayomi simple_login: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	// Upstream returns 303 SEE_OTHER on success (redirect to /). On bad
	// creds it re-renders the login page with 200 + error body, so we
	// must distinguish "got a session cookie" from "200 with login form".
	// Only 401/403 — or a 2xx without a session cookie — are auth
	// failures; everything else (5xx, network glitches surfaced as 5xx)
	// is an upstream availability problem and must not get wrapped as
	// ErrAuth, or callers using errors.Is(err, ErrAuth) get false
	// positives on outages.
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: simple_login status %d", ErrAuth, resp.StatusCode)
	case resp.StatusCode >= 500:
		return fmt.Errorf("suwayomi simple_login status %d", resp.StatusCode)
	}

	cookies := resp.Cookies()
	if len(cookies) == 0 {
		// 2xx with no cookie = login form re-rendered = bad creds.
		// Non-2xx with no cookie also lands here on, e.g., 400 — still
		// most plausibly an auth-shape problem, not an outage.
		return fmt.Errorf("%w: simple_login returned no session cookie (status %d)", ErrAuth, resp.StatusCode)
	}

	s.cookies = cookies
	return nil
}

func (s *SimpleLoginAuth) Invalidate() {
	s.mu.Lock()
	s.cookies = nil
	s.mu.Unlock()
}

// UILoginAuth implements Auth for `server.authMode = "ui_login"`.
//
// Upstream issues JWTs via a GraphQL `login` mutation at /api/graphql:
//
//	mutation { login(input:{username:"...",password:"..."})
//	           { accessToken refreshToken } }
//
// The access token is presented as `Authorization: Bearer <token>` on
// every subsequent request. We hold both tokens but for now only use
// accessToken — on 401 the client tier calls Invalidate + re-login,
// which is acceptable because Suwayomi clients live for one poll tick.
type UILoginAuth struct {
	Username string
	Password string

	mu           sync.Mutex
	accessToken  string
	refreshToken string
}

func (u *UILoginAuth) Apply(_ context.Context, req *http.Request) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.accessToken == "" {
		return errors.New("suwayomi: ui_login session not established")
	}
	req.Header.Set("Authorization", "Bearer "+u.accessToken)
	return nil
}

func (u *UILoginAuth) EnsureSession(ctx context.Context, base string, httpClient *http.Client) error {
	// Hold the mutex across the network call — see SimpleLoginAuth for
	// rationale. Concurrent callers must not both fire a login.
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.accessToken != "" {
		return nil
	}

	payload := map[string]any{
		"query": `mutation Login($u: String!, $p: String!) {
			login(input: { username: $u, password: $p }) {
				accessToken
				refreshToken
			}
		}`,
		"variables": map[string]string{"u": u.Username, "p": u.Password},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("suwayomi ui_login marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/graphql", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("suwayomi ui_login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("suwayomi ui_login: %w", err)
	}
	defer resp.Body.Close()
	// Only 401/403 are auth failures; 5xx and other non-2xx codes are
	// upstream availability problems and must not wrap as ErrAuth.
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("%w: ui_login status %d", ErrAuth, resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("suwayomi ui_login status %d", resp.StatusCode)
	}

	var out struct {
		Data struct {
			Login struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
			} `json:"login"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("suwayomi ui_login decode: %w", err)
	}
	// GraphQL surfaces auth failures as a 2xx + errors[] payload, so
	// these stay wrapped as ErrAuth.
	if len(out.Errors) > 0 {
		return fmt.Errorf("%w: %s", ErrAuth, out.Errors[0].Message)
	}
	if out.Data.Login.AccessToken == "" {
		return fmt.Errorf("%w: empty accessToken in login response", ErrAuth)
	}

	u.accessToken = out.Data.Login.AccessToken
	u.refreshToken = out.Data.Login.RefreshToken
	return nil
}

func (u *UILoginAuth) Invalidate() {
	u.mu.Lock()
	u.accessToken = ""
	u.refreshToken = ""
	u.mu.Unlock()
}

// Category is one row from Suwayomi's GET /api/v1/category.
type Category struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Order int    `json:"order"`
}

// Chapter is one chapter row from Suwayomi's GraphQL chapters() query.
// The fields mirror the upstream selection set used by ListChapters.
type Chapter struct {
	ID            int64
	Name          string
	ChapterNumber float64
	IsDownloaded  bool
	SourceOrder   int
}

// Manga is one entry in the user's Suwayomi library.
//
// SourceID is Suwayomi's numeric source ID encoded as a string (e.g. the
// MangaDex EN source). Source is the human-readable display name when we
// have it (e.g. "MangaDex (EN)"); empty when only the ID is known.
//
// DownloadDir is the on-disk path of this manga's chapters relative to
// Suwayomi's downloads root. mangarr derives it as `<source>/<title>`
// matching Suwayomi's default folder layout. Operators with a custom
// downloads layout can map their roots to match.
type Manga struct {
	ID          int64   `json:"id"`
	Title       string  `json:"title"`
	SourceID    string  `json:"sourceId"`
	Source      string  `json:"source"`
	DownloadDir string  `json:"downloadDir"`
	CategoryIDs []int64 `json:"categoryIds"`
}

// Client is the fresh-per-call Suwayomi REST + GraphQL handle. It mirrors
// the internal/kavita Client shape and is safe to construct on every
// poll tick or handler call. Long-lived session state lives only inside
// the Auth implementation.
type Client struct {
	base string
	auth Auth
	http *http.Client
}

// New returns a Client targeting the given Suwayomi base URL.
//
// base is the URL prefix in front of /api/v1 and /api/graphql (e.g.
// "http://suwayomi.entertainment.svc.cluster.local:4567"). Trailing
// slashes are stripped so callers do not need to be careful.
func New(base string, auth Auth) *Client {
	return &Client{
		base: strings.TrimRight(base, "/"),
		auth: auth,
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

// Ping confirms reachability + auth in one call. It uses /api/v1/category
// because it is light, authenticated, and present on every Suwayomi
// build, which makes it a better health probe than the GraphQL surface.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.ListCategories(ctx)
	return err
}

// ListCategories returns every category Suwayomi reports, sorted ascending
// by the upstream `order` field so the first-match-wins resolution in the
// classifier is deterministic.
//
// On a 401 it invalidates the cached session, re-logs in, and retries
// exactly once. Two consecutive 401s surface as ErrAuth.
func (c *Client) ListCategories(ctx context.Context) ([]Category, error) {
	var out []Category
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/category", nil, &out); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out, nil
}

// ListLibraryWithCategories returns one entry per manga in the user's
// library, with CategoryIDs eagerly populated. It executes a single
// GraphQL query against /api/graphql. CategoryIDs on each manga are
// ordered ascending by the parent Category.order, matching the
// classifier's first-match-wins evaluation.
//
// The query asks for { id title sourceId source { displayName } categories
// { nodes { id order } } } — every selection is on the documented
// Suwayomi GraphQL surface.
func (c *Client) ListLibraryWithCategories(ctx context.Context) ([]Manga, error) {
	const query = `query LibraryWithCategories {
		mangas(filter: { inLibrary: { equalTo: true } }) {
			nodes {
				id
				title
				sourceId
				source { displayName }
				categories { nodes { id order } }
			}
		}
	}`
	type gqlResp struct {
		Data struct {
			Mangas struct {
				Nodes []struct {
					ID       int64  `json:"id"`
					Title    string `json:"title"`
					SourceID string `json:"sourceId"`
					Source   *struct {
						DisplayName string `json:"displayName"`
					} `json:"source"`
					Categories struct {
						Nodes []struct {
							ID    int64 `json:"id"`
							Order int   `json:"order"`
						} `json:"nodes"`
					} `json:"categories"`
				} `json:"nodes"`
			} `json:"mangas"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	body, err := json.Marshal(map[string]any{"query": query})
	if err != nil {
		return nil, fmt.Errorf("suwayomi library marshal: %w", err)
	}

	raw, err := c.doGraphQL(ctx, body)
	if err != nil {
		return nil, err
	}
	var out gqlResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("suwayomi library decode: %w", err)
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("suwayomi library graphql: %s", out.Errors[0].Message)
	}

	mangas := make([]Manga, 0, len(out.Data.Mangas.Nodes))
	for _, n := range out.Data.Mangas.Nodes {
		// Sort category IDs by ascending order so the classifier's
		// first-match-wins walk is deterministic.
		cats := append([]struct {
			ID    int64 `json:"id"`
			Order int   `json:"order"`
		}(nil), n.Categories.Nodes...)
		sort.SliceStable(cats, func(i, j int) bool { return cats[i].Order < cats[j].Order })
		catIDs := make([]int64, 0, len(cats))
		for _, ct := range cats {
			catIDs = append(catIDs, ct.ID)
		}

		sourceDisplay := ""
		if n.Source != nil {
			sourceDisplay = n.Source.DisplayName
		}

		mangas = append(mangas, Manga{
			ID:          n.ID,
			Title:       n.Title,
			SourceID:    n.SourceID,
			Source:      sourceDisplay,
			DownloadDir: deriveDownloadDir(sourceDisplay, n.SourceID, n.Title),
			CategoryIDs: catIDs,
		})
	}

	// Stable order for callers / tests: by title.
	sort.SliceStable(mangas, func(i, j int) bool {
		return strings.ToLower(mangas[i].Title) < strings.ToLower(mangas[j].Title)
	})
	return mangas, nil
}

// ListChapters returns every chapter Suwayomi knows about for the given
// manga. The result mixes downloaded and not-yet-downloaded chapters;
// callers filter on Chapter.IsDownloaded as needed.
//
// Schema-drift caveat: the query uses `condition: {mangaId: $mangaId}`,
// matching Suwayomi 1.x. Older or newer instances may use `filter:` with
// a different shape; adapt the const below if introspection disagrees.
func (c *Client) ListChapters(ctx context.Context, mangaID int64) ([]Chapter, error) {
	const query = `query ChaptersForManga($mangaId: Int!) {
		chapters(condition: {mangaId: $mangaId}) {
			nodes {
				id
				name
				chapterNumber
				isDownloaded
				sourceOrder
			}
		}
	}`
	body, err := json.Marshal(map[string]any{
		"query":     query,
		"variables": map[string]any{"mangaId": mangaID},
	})
	if err != nil {
		return nil, fmt.Errorf("suwayomi chapters marshal: %w", err)
	}

	raw, err := c.doGraphQL(ctx, body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Data struct {
			Chapters struct {
				Nodes []struct {
					ID            int64   `json:"id"`
					Name          string  `json:"name"`
					ChapterNumber float64 `json:"chapterNumber"`
					IsDownloaded  bool    `json:"isDownloaded"`
					SourceOrder   int     `json:"sourceOrder"`
				} `json:"nodes"`
			} `json:"chapters"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("suwayomi chapters decode: %w", err)
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("suwayomi chapters graphql: %s", out.Errors[0].Message)
	}

	chapters := make([]Chapter, len(out.Data.Chapters.Nodes))
	for i, n := range out.Data.Chapters.Nodes {
		chapters[i] = Chapter{
			ID:            n.ID,
			Name:          n.Name,
			ChapterNumber: n.ChapterNumber,
			IsDownloaded:  n.IsDownloaded,
			SourceOrder:   n.SourceOrder,
		}
	}
	return chapters, nil
}

// EnqueueChapterDownloads adds the given chapter IDs to Suwayomi's
// download queue. Idempotent on the Suwayomi side — a re-enqueue of an
// already-queued chapter is a no-op, which is what makes the
// orchestrator's boot-recovery sweep (state='fed' → 'pending' → re-feed)
// safe across pod restarts.
//
// An empty ids slice is a no-op (no network call). The mutation is
// fire-and-forget from mangarr's perspective; we don't introspect the
// clientMutationId in the response.
func (c *Client) EnqueueChapterDownloads(ctx context.Context, chapterIDs []int64) error {
	if len(chapterIDs) == 0 {
		return nil
	}
	const mutation = `mutation EnqueueChapterDownloads($ids: [Int!]!) {
		enqueueChapterDownloads(input: {ids: $ids}) {
			clientMutationId
		}
	}`
	body, err := json.Marshal(map[string]any{
		"query":     mutation,
		"variables": map[string]any{"ids": chapterIDs},
	})
	if err != nil {
		return fmt.Errorf("suwayomi enqueue marshal: %w", err)
	}
	if _, err := c.doGraphQL(ctx, body); err != nil {
		return err
	}
	return nil
}

// doGraphQL POSTs a pre-marshalled GraphQL request body to /api/graphql
// and returns the raw response bytes. It runs through doJSON so auth,
// 401-retry, and error wrapping are handled the same as REST endpoints.
//
// Callers unmarshal the returned bytes themselves so they can declare
// per-query struct shapes without leaking concrete types into this
// helper. This is the single GraphQL entry point used by
// ListLibraryWithCategories, ListChapters, and the bulk-download
// EnqueueChapterDownloads / GetDownloadStatus methods.
func (c *Client) doGraphQL(ctx context.Context, body []byte) ([]byte, error) {
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodPost, "/api/graphql", body, &raw); err != nil {
		return nil, err
	}
	return []byte(raw), nil
}

// deriveDownloadDir matches Suwayomi's default downloads layout:
// `<source>/<sanitisedTitle>`. Source falls back to the numeric source ID
// when the GraphQL surface did not include a displayName.
//
// Best-effort: mangarr's sanitiser is NOT a byte-for-byte port of the
// upstream Kotlin sanitiser (it lives in Suwayomi-Server's downloader
// code path and may evolve). Any manga whose on-disk folder name diverges
// from what we derive here will cache-miss and fall through to the
// AniList classifier — which is the documented failure-mode in the spec
// ("cache miss → fall through to AniList"). A future Plan B/C
// improvement is to verify against the live Suwayomi tree at refresh
// time and surface mismatches in the activity log.
func deriveDownloadDir(source, sourceID, title string) string {
	s := source
	if s == "" {
		s = sourceID
	}
	return sanitiseSegment(s, sourceID) + "/" + sanitiseSegment(title, fmt.Sprintf("manga-%s", sourceID))
}

// sanitiseSegment strips path separators and a conservative set of
// filesystem-hostile characters. fallback is returned when sanitisation
// collapses the input to an empty string (e.g. title was only `?*<>` or
// whitespace) so the cache key remains unique rather than degenerating
// to "MangaDex/" for every such manga.
func sanitiseSegment(in, fallback string) string {
	in = strings.TrimSpace(in)
	r := strings.NewReplacer(
		"/", "_",
		"\\", "_",
		":", "_",
		"*", "_",
		"?", "_",
		"\"", "_",
		"<", "_",
		">", "_",
		"|", "_",
	)
	out := strings.TrimSpace(r.Replace(in))
	// Strip leading/trailing dots — Windows hates them and Suwayomi
	// avoids them on disk; cross-platform parity keeps the cache key
	// derivable from either side.
	out = strings.Trim(out, ".")
	if out == "" {
		return fallback
	}
	return out
}

// doJSON is the single request entry point used by every public method.
// It runs EnsureSession, signs the request via Auth.Apply, makes the
// call, and — on a 401 — invalidates the session, re-runs EnsureSession,
// and retries exactly once. Two consecutive 401s surface as ErrAuth.
//
// reqBody is nil for GETs and a JSON []byte for POSTs. out is decoded
// from the response body on 2xx. out may be nil if the caller does not
// care about the response payload.
func (c *Client) doJSON(ctx context.Context, method, path string, reqBody []byte, out any) error {
	build := func() (*http.Request, error) {
		var body io.Reader
		if reqBody != nil {
			body = bytes.NewReader(reqBody)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
		if err != nil {
			return nil, fmt.Errorf("suwayomi %s %s request: %w", method, path, err)
		}
		if reqBody != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		return req, nil
	}

	for attempt := 0; attempt < maxAuthAttempts; attempt++ {
		if err := c.auth.EnsureSession(ctx, c.base, c.http); err != nil {
			return err
		}
		req, err := build()
		if err != nil {
			return err
		}
		if err := c.auth.Apply(ctx, req); err != nil {
			// Session went away between EnsureSession and Apply.
			c.auth.Invalidate()
			continue
		}
		done, err := c.tryOnce(req, method, path, out)
		if err != nil || done {
			return err
		}
		// !done → got a 401, session invalidated, loop will re-auth + retry.
	}
	return fmt.Errorf("%w: %s %s", ErrAuth, method, path)
}

// tryOnce runs a single attempt. It returns (done=true, err=nil) on
// success, (done=true, err=<wrapped>) on a terminal non-401 error, or
// (done=false, err=nil) when the caller should re-auth + retry. The
// response body is always closed before return — no defer-in-loop
// footgun.
func (c *Client) tryOnce(req *http.Request, method, path string, out any) (bool, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return true, fmt.Errorf("suwayomi %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		io.Copy(io.Discard, resp.Body)
		c.auth.Invalidate()
		return false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, resp.Body)
		return true, fmt.Errorf("suwayomi %s %s status %d", method, path, resp.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return true, fmt.Errorf("suwayomi %s %s decode: %w", method, path, err)
		}
	} else {
		io.Copy(io.Discard, resp.Body)
	}
	return true, nil
}
