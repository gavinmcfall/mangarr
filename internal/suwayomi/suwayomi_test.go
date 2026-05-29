package suwayomi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// ----- helpers -----

// catJSON is the response payload shape for GET /api/v1/category.
func catJSON(cats []Category) string {
	b, _ := json.Marshal(cats)
	return string(b)
}

// libraryJSON is the GraphQL-shape payload returned by /api/graphql for
// the ListLibraryWithCategories query.
type gqlMangaNode struct {
	ID         int64           `json:"id"`
	Title      string          `json:"title"`
	SourceID   string          `json:"sourceId"`
	Source     *gqlSource      `json:"source,omitempty"`
	Categories gqlCategoryConn `json:"categories"`
}
type gqlSource struct {
	DisplayName string `json:"displayName"`
}
type gqlCategoryConn struct {
	Nodes []gqlCatNode `json:"nodes"`
}
type gqlCatNode struct {
	ID    int64 `json:"id"`
	Order int   `json:"order"`
}

func libraryJSON(mangas []gqlMangaNode) string {
	resp := map[string]any{
		"data": map[string]any{
			"mangas": map[string]any{"nodes": mangas},
		},
	}
	b, _ := json.Marshal(resp)
	return string(b)
}

// handlerErr is the safe replacement for `t.Fatalf` inside an httptest
// handler goroutine. Fatal-from-handler only kills the handler goroutine
// (Goexit), leaves the HTTP client hanging, and gives misleading errors.
// Instead: log via t.Errorf (which records the failure) and return a
// 500 so the test goroutine sees a clean non-2xx and fails cleanly.
func handlerErr(t *testing.T, w http.ResponseWriter, format string, args ...any) {
	t.Helper()
	t.Errorf(format, args...)
	http.Error(w, "test handler error", http.StatusInternalServerError)
}

// ----- none auth -----

func TestNoneAuthSendsNoAuthorizationHeader(t *testing.T) {
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, catJSON([]Category{{ID: 1, Name: "Manga", Order: 0}}))
	}))
	defer srv.Close()

	c := New(srv.URL, NoAuth{})
	if _, err := c.ListCategories(context.Background()); err != nil {
		t.Fatalf("ListCategories: %v", err)
	}
	if sawAuth != "" {
		t.Fatalf("none auth must not send Authorization header, got %q", sawAuth)
	}
}

// ----- basic auth -----

func TestBasicAuthSendsBasicHeader(t *testing.T) {
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:pass"))
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, catJSON(nil))
	}))
	defer srv.Close()

	c := New(srv.URL, BasicAuth{Username: "user", Password: "pass"})
	if _, err := c.ListCategories(context.Background()); err != nil {
		t.Fatalf("ListCategories: %v", err)
	}
	if sawAuth != want {
		t.Fatalf("basic auth header mismatch: want %q, got %q", want, sawAuth)
	}
}

// ----- simple_login -----

func TestSimpleLoginLogsInOnceThenSendsCookie(t *testing.T) {
	var loginHits, listHits int32
	var sawCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login.html":
			atomic.AddInt32(&loginHits, 1)
			if err := r.ParseForm(); err != nil {
				handlerErr(t, w, "parse form: %v", err)
				return
			}
			if r.PostForm.Get("user") != "u" || r.PostForm.Get("pass") != "p" {
				t.Errorf("login form mismatch: %v", r.PostForm)
			}
			http.SetCookie(w, &http.Cookie{Name: "JSESSIONID", Value: "session-abc"})
			w.WriteHeader(http.StatusSeeOther)
		case "/api/v1/category":
			atomic.AddInt32(&listHits, 1)
			if c, _ := r.Cookie("JSESSIONID"); c != nil {
				sawCookie = c.Value
			}
			io.WriteString(w, catJSON(nil))
		default:
			handlerErr(t, w, "unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, &SimpleLoginAuth{Username: "u", Password: "p"})
	for i := 0; i < 3; i++ {
		if _, err := c.ListCategories(context.Background()); err != nil {
			t.Fatalf("ListCategories #%d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&loginHits); got != 1 {
		t.Fatalf("login should be called exactly once, got %d", got)
	}
	if got := atomic.LoadInt32(&listHits); got != 3 {
		t.Fatalf("list should be called 3 times, got %d", got)
	}
	if sawCookie != "session-abc" {
		t.Fatalf("cookie not presented to list endpoint, got %q", sawCookie)
	}
}

// ----- ui_login -----

func TestUILoginLogsInOnceThenSendsBearer(t *testing.T) {
	var loginHits, listHits int32
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/graphql":
			// Could be the login mutation OR a library query — disambiguate by body.
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "login(input") {
				atomic.AddInt32(&loginHits, 1)
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"data":{"login":{"accessToken":"jwt-xyz","refreshToken":"r-xyz"}}}`)
				return
			}
			// Library query path — assert bearer presented.
			sawAuth = r.Header.Get("Authorization")
			io.WriteString(w, libraryJSON(nil))
		case "/api/v1/category":
			atomic.AddInt32(&listHits, 1)
			sawAuth = r.Header.Get("Authorization")
			io.WriteString(w, catJSON(nil))
		default:
			handlerErr(t, w, "unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, &UILoginAuth{Username: "u", Password: "p"})
	for i := 0; i < 3; i++ {
		if _, err := c.ListCategories(context.Background()); err != nil {
			t.Fatalf("ListCategories #%d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&loginHits); got != 1 {
		t.Fatalf("ui login should be called once, got %d", got)
	}
	if got := atomic.LoadInt32(&listHits); got != 3 {
		t.Fatalf("list should be called 3 times, got %d", got)
	}
	if sawAuth != "Bearer jwt-xyz" {
		t.Fatalf("bearer not presented, got %q", sawAuth)
	}
}

// ----- 401 retry -----

func Test401TriggersReloginAndRetryThenSucceeds(t *testing.T) {
	var loginHits int32
	listAttempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/graphql":
			n := atomic.AddInt32(&loginHits, 1)
			io.WriteString(w, `{"data":{"login":{"accessToken":"jwt-`+strconv.Itoa(int(n))+`","refreshToken":"r"}}}`)
		case "/api/v1/category":
			listAttempts++
			if listAttempts == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			io.WriteString(w, catJSON([]Category{{ID: 1, Name: "Manga", Order: 0}}))
		default:
			handlerErr(t, w, "unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, &UILoginAuth{Username: "u", Password: "p"})
	got, err := c.ListCategories(context.Background())
	if err != nil {
		t.Fatalf("ListCategories: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 category, got %d", len(got))
	}
	if atomic.LoadInt32(&loginHits) != 2 {
		t.Fatalf("login should have been called twice (initial + post-401), got %d", loginHits)
	}
	if listAttempts != 2 {
		t.Fatalf("list should have been retried once after 401, got %d attempts", listAttempts)
	}
}

func TestPersistent401SurfacesAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/graphql":
			io.WriteString(w, `{"data":{"login":{"accessToken":"jwt","refreshToken":"r"}}}`)
		case "/api/v1/category":
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, &UILoginAuth{Username: "u", Password: "p"})
	_, err := c.ListCategories(context.Background())
	if err == nil {
		t.Fatal("expected auth error after two consecutive 401s")
	}
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("want ErrAuth, got %v", err)
	}
}

// TestLoginUpstreamOutageDoesNotWrapErrAuth — a 5xx from the login
// endpoint is an availability problem, not an auth failure. Callers
// using errors.Is(err, ErrAuth) to distinguish "wrong password" from
// "Suwayomi is down" must see false for outages.
func TestUILoginUpstreamOutageDoesNotWrapErrAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	c := New(srv.URL, &UILoginAuth{Username: "u", Password: "p"})
	_, err := c.ListCategories(context.Background())
	if err == nil {
		t.Fatal("expected error on 502 from login endpoint")
	}
	if errors.Is(err, ErrAuth) {
		t.Fatalf("5xx from login must NOT wrap as ErrAuth, got %v", err)
	}
}

func TestSimpleLoginUpstreamOutageDoesNotWrapErrAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login.html" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, &SimpleLoginAuth{Username: "u", Password: "p"})
	_, err := c.ListCategories(context.Background())
	if err == nil {
		t.Fatal("expected error on 503 from login endpoint")
	}
	if errors.Is(err, ErrAuth) {
		t.Fatalf("5xx from simple_login must NOT wrap as ErrAuth, got %v", err)
	}
}

// ----- ListCategories sorts -----

func TestListCategoriesSortsByOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, catJSON([]Category{
			{ID: 3, Name: "C", Order: 2},
			{ID: 1, Name: "A", Order: 0},
			{ID: 2, Name: "B", Order: 1},
		}))
	}))
	defer srv.Close()

	c := New(srv.URL, NoAuth{})
	got, err := c.ListCategories(context.Background())
	if err != nil {
		t.Fatalf("ListCategories: %v", err)
	}
	if len(got) != 3 || got[0].ID != 1 || got[1].ID != 2 || got[2].ID != 3 {
		t.Fatalf("not sorted by order: %#v", got)
	}
}

// ----- ListLibraryWithCategories -----

func TestListLibraryWithCategoriesPopulatesCategoryIDsOrdered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/graphql" {
			handlerErr(t, w, "want /api/graphql, got %s", r.URL.Path)
			return
		}
		io.WriteString(w, libraryJSON([]gqlMangaNode{
			{
				ID: 11, Title: "Solo Leveling", SourceID: "100",
				Source: &gqlSource{DisplayName: "MangaDex"},
				Categories: gqlCategoryConn{Nodes: []gqlCatNode{
					{ID: 30, Order: 2},
					{ID: 10, Order: 0},
					{ID: 20, Order: 1},
				}},
			},
			{
				ID: 12, Title: "AAA", SourceID: "200",
				Categories: gqlCategoryConn{Nodes: nil},
			},
		}))
	}))
	defer srv.Close()

	c := New(srv.URL, NoAuth{})
	mangas, err := c.ListLibraryWithCategories(context.Background())
	if err != nil {
		t.Fatalf("ListLibrary: %v", err)
	}
	if len(mangas) != 2 {
		t.Fatalf("want 2 mangas, got %d", len(mangas))
	}
	// Sorted by title — AAA first.
	if mangas[0].Title != "AAA" || mangas[1].Title != "Solo Leveling" {
		t.Fatalf("mangas not sorted by title: %#v", mangas)
	}
	sl := mangas[1]
	wantIDs := []int64{10, 20, 30}
	if len(sl.CategoryIDs) != len(wantIDs) {
		t.Fatalf("category count mismatch: %v", sl.CategoryIDs)
	}
	for i, id := range wantIDs {
		if sl.CategoryIDs[i] != id {
			t.Fatalf("category at %d: want %d got %d", i, id, sl.CategoryIDs[i])
		}
	}
	if sl.DownloadDir == "" {
		t.Fatal("DownloadDir should be derived, got empty")
	}
}

func TestListLibraryWithCategoriesSurfacesGraphQLErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":{"mangas":{"nodes":[]}},"errors":[{"message":"boom"}]}`)
	}))
	defer srv.Close()

	c := New(srv.URL, NoAuth{})
	_, err := c.ListLibraryWithCategories(context.Background())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want graphql error surfaced, got %v", err)
	}
}

// ----- sanitiseSegment edge cases -----

func TestSanitiseSegmentEmptyInputReturnsFallback(t *testing.T) {
	got := sanitiseSegment("", "fallback")
	if got != "fallback" {
		t.Fatalf("empty input: want fallback, got %q", got)
	}
}

func TestSanitiseSegmentAllSpecialCharsReturnsFallback(t *testing.T) {
	got := sanitiseSegment("...", "fallback")
	if got != "fallback" {
		t.Fatalf("dots-only input: want fallback, got %q", got)
	}
}

func TestSanitiseSegmentWhitespaceReturnsFallback(t *testing.T) {
	got := sanitiseSegment("   ", "fallback")
	if got != "fallback" {
		t.Fatalf("whitespace input: want fallback, got %q", got)
	}
}

func TestSanitiseSegmentPreservesValidNames(t *testing.T) {
	got := sanitiseSegment("Solo Leveling", "fallback")
	if got != "Solo Leveling" {
		t.Fatalf("valid input: want %q, got %q", "Solo Leveling", got)
	}
}
