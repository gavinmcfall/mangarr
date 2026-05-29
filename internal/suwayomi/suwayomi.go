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
	s.mu.Lock()
	if len(s.cookies) > 0 {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

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
	cookies := resp.Cookies()
	if len(cookies) == 0 {
		return fmt.Errorf("%w: simple_login returned no session cookie (status %d)", ErrAuth, resp.StatusCode)
	}

	s.mu.Lock()
	s.cookies = cookies
	s.mu.Unlock()
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
	u.mu.Lock()
	if u.accessToken != "" {
		u.mu.Unlock()
		return nil
	}
	u.mu.Unlock()

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
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("%w: ui_login status %d", ErrAuth, resp.StatusCode)
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
	if len(out.Errors) > 0 {
		return fmt.Errorf("%w: %s", ErrAuth, out.Errors[0].Message)
	}
	if out.Data.Login.AccessToken == "" {
		return fmt.Errorf("%w: empty accessToken in login response", ErrAuth)
	}

	u.mu.Lock()
	u.accessToken = out.Data.Login.AccessToken
	u.refreshToken = out.Data.Login.RefreshToken
	u.mu.Unlock()
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

	var out gqlResp
	if err := c.doJSON(ctx, http.MethodPost, "/api/graphql", body, &out); err != nil {
		return nil, err
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

// deriveDownloadDir matches Suwayomi's default downloads layout:
// `<source>/<sanitisedTitle>`. Source falls back to the numeric source ID
// when the GraphQL surface did not include a displayName. The same
// sanitisation rules Suwayomi applies on disk (strip filesystem-hostile
// chars) are reproduced here so the path keys line up with what landed
// in /media/Downloads/...
func deriveDownloadDir(source, sourceID, title string) string {
	s := source
	if s == "" {
		s = sourceID
	}
	return sanitiseSegment(s) + "/" + sanitiseSegment(title)
}

// sanitiseSegment strips path separators and the small set of characters
// Suwayomi rejects in download folder names. Conservative — we'd rather
// miss a match (cache miss → AniList fallback) than route by a wrong
// path.
func sanitiseSegment(in string) string {
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
	return r.Replace(in)
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

	for attempt := 0; attempt < 2; attempt++ {
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
		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("suwayomi %s %s: %w", method, path, err)
		}
		if resp.StatusCode == http.StatusUnauthorized {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			c.auth.Invalidate()
			if attempt == 1 {
				return fmt.Errorf("%w: %s %s", ErrAuth, method, path)
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return fmt.Errorf("suwayomi %s %s status %d", method, path, resp.StatusCode)
		}
		defer resp.Body.Close()
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				return fmt.Errorf("suwayomi %s %s decode: %w", method, path, err)
			}
		} else {
			io.Copy(io.Discard, resp.Body)
		}
		return nil
	}
	return fmt.Errorf("%w: %s %s", ErrAuth, method, path)
}
