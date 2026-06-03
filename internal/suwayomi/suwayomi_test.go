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

// ----- ListChapters -----

func TestListChaptersForManga(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/graphql" {
			handlerErr(t, w, "want /api/graphql, got %s", r.URL.Path)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"mangaId":42`) {
			t.Errorf("query didn't carry mangaId=42; body: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":{"chapters":{"nodes":[
			{"id":100,"name":"Chapter 1","chapterNumber":1,"isDownloaded":true,"sourceOrder":1},
			{"id":101,"name":"Chapter 2","chapterNumber":2,"isDownloaded":false,"sourceOrder":2},
			{"id":102,"name":"Chapter 3","chapterNumber":3,"isDownloaded":false,"sourceOrder":3}
		]}}}`)
	}))
	defer srv.Close()

	c := New(srv.URL, NoAuth{})
	chapters, err := c.ListChapters(context.Background(), 42)
	if err != nil {
		t.Fatalf("ListChapters: %v", err)
	}
	if len(chapters) != 3 {
		t.Fatalf("want 3 chapters, got %d", len(chapters))
	}
	if chapters[0].ID != 100 || !chapters[0].IsDownloaded {
		t.Errorf("chapter 0 mismatch: %+v", chapters[0])
	}
	if chapters[0].Name != "Chapter 1" || chapters[0].ChapterNumber != 1 || chapters[0].SourceOrder != 1 {
		t.Errorf("chapter 0 fields not populated: %+v", chapters[0])
	}
	if chapters[1].IsDownloaded {
		t.Errorf("chapter 1 should not be downloaded: %+v", chapters[1])
	}
	if chapters[2].IsDownloaded {
		t.Errorf("chapter 2 should not be downloaded: %+v", chapters[2])
	}
}

func TestListChaptersSurfacesGraphQLErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":{"chapters":{"nodes":[]}},"errors":[{"message":"manga not found"}]}`)
	}))
	defer srv.Close()

	c := New(srv.URL, NoAuth{})
	_, err := c.ListChapters(context.Background(), 99)
	if err == nil || !strings.Contains(err.Error(), "manga not found") {
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

// ----- EnqueueChapterDownloads -----

func TestEnqueueChapterDownloads(t *testing.T) {
	var capturedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/graphql" {
			handlerErr(t, w, "want /api/graphql, got %s", r.URL.Path)
			return
		}
		b, _ := io.ReadAll(r.Body)
		capturedBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":{"enqueueChapterDownloads":{"clientMutationId":null}}}`)
	}))
	defer srv.Close()

	c := New(srv.URL, NoAuth{})
	err := c.EnqueueChapterDownloads(context.Background(), []int64{100, 101, 102})
	if err != nil {
		t.Fatalf("EnqueueChapterDownloads: %v", err)
	}
	if !strings.Contains(capturedBody, `"ids":[100,101,102]`) {
		t.Errorf("mutation body didn't carry ids array; got: %s", capturedBody)
	}
}

func TestEnqueueChapterDownloadsEmptyIsNoOp(t *testing.T) {
	// Empty batch must not even hit the network — pointless roundtrip.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server should not be called for empty batch; got %s", r.URL.Path)
	}))
	defer srv.Close()
	c := New(srv.URL, NoAuth{})
	if err := c.EnqueueChapterDownloads(context.Background(), nil); err != nil {
		t.Errorf("empty batch should not error: %v", err)
	}
}

// ----- downloadStatus / InFlightCountForSource / 429 -----

func TestGetDownloadStatusGroupsBySource(t *testing.T) {
	// Stub returns 4 queue entries: 3 on source "42" (2 QUEUED + 1
	// DOWNLOADING, all in-flight) + 1 on source "99" (DOWNLOADING).
	// InFlightCountForSource must count by source AND in-flight state,
	// and must return 0 for a source not present in the queue.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/graphql" {
			handlerErr(t, w, "want /api/graphql, got %s", r.URL.Path)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"data": {
				"downloadStatus": {
					"state": "STARTED",
					"queue": [
						{"chapter":{"id":1,"mangaId":10},"manga":{"source":{"id":"42"}},"state":"QUEUED","progress":0.0,"tries":0},
						{"chapter":{"id":2,"mangaId":10},"manga":{"source":{"id":"42"}},"state":"QUEUED","progress":0.0,"tries":0},
						{"chapter":{"id":3,"mangaId":11},"manga":{"source":{"id":"42"}},"state":"DOWNLOADING","progress":0.5,"tries":1},
						{"chapter":{"id":4,"mangaId":20},"manga":{"source":{"id":"99"}},"state":"DOWNLOADING","progress":0.25,"tries":0}
					]
				}
			}
		}`)
	}))
	defer srv.Close()

	c := New(srv.URL, NoAuth{})
	ctx := context.Background()

	if got, err := c.InFlightCountForSource(ctx, "42"); err != nil || got != 3 {
		t.Errorf("InFlightCountForSource(42) = (%d, %v); want (3, nil)", got, err)
	}
	if got, err := c.InFlightCountForSource(ctx, "99"); err != nil || got != 1 {
		t.Errorf("InFlightCountForSource(99) = (%d, %v); want (1, nil)", got, err)
	}
	if got, err := c.InFlightCountForSource(ctx, "404"); err != nil || got != 0 {
		t.Errorf("InFlightCountForSource(404) = (%d, %v); want (0, nil)", got, err)
	}

	// Also confirm GetDownloadStatus surfaces the full queue with all
	// fields decoded — the orchestrator may grow finer-grained checks
	// later and we don't want the decoder to silently drop fields.
	status, err := c.GetDownloadStatus(ctx)
	if err != nil {
		t.Fatalf("GetDownloadStatus: %v", err)
	}
	if status.State != "STARTED" {
		t.Errorf("State = %q; want STARTED", status.State)
	}
	if len(status.Queue) != 4 {
		t.Fatalf("Queue len = %d; want 4", len(status.Queue))
	}
	// Spot-check the DOWNLOADING entry on source 42 for full decode.
	var dl DownloadQueueEntry
	for _, e := range status.Queue {
		if e.ChapterID == 3 {
			dl = e
		}
	}
	if dl.MangaID != 11 || dl.SourceID != "42" || dl.State != "DOWNLOADING" || dl.Progress != 0.5 || dl.Tries != 1 {
		t.Errorf("queue[chapterId=3] = %+v; want MangaID=11 SourceID=42 State=DOWNLOADING Progress=0.5 Tries=1", dl)
	}
}

func TestSuwayomiHTTP429ReturnsTypedError(t *testing.T) {
	// Stub returns HTTP 429 for every request. The doJSON helper must
	// translate this into ErrHTTP429 so callers using
	// errors.Is(err, ErrHTTP429) — notably the bulk-download backoff
	// ladder — can branch on it without parsing error strings.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := New(srv.URL, NoAuth{})
	_, err := c.GetDownloadStatus(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrHTTP429) {
		t.Errorf("errors.Is(err, ErrHTTP429) = false; err = %v", err)
	}
}

// ----- GetChapterMeta -----

// TestGetChapterMetaHappyPathInQueueError covers the primary stall-detection
// signal: pageCount==0, isDownloaded==false, queue entry matches the
// requested chapterID with state=ERROR and tries=3.
func TestGetChapterMetaHappyPathInQueueError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/graphql" {
			handlerErr(t, w, "want /api/graphql, got %s", r.URL.Path)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"id":3185`) {
			t.Errorf("query didn't carry id=3185; body: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"data": {
				"chapter": {"pageCount": 0, "isDownloaded": false},
				"downloadStatus": {
					"queue": [
						{"chapter": {"id": 3185}, "state": "ERROR", "progress": 0.0, "tries": 3}
					]
				}
			}
		}`)
	}))
	defer srv.Close()

	c := New(srv.URL, NoAuth{})
	meta, err := c.GetChapterMeta(context.Background(), 3185)
	if err != nil {
		t.Fatalf("GetChapterMeta: %v", err)
	}
	if meta.PageCount != 0 {
		t.Errorf("PageCount: want 0, got %d", meta.PageCount)
	}
	if meta.IsDownloaded {
		t.Errorf("IsDownloaded: want false, got true")
	}
	if meta.QueueState != "ERROR" {
		t.Errorf("QueueState: want ERROR, got %q", meta.QueueState)
	}
	if meta.Tries != 3 {
		t.Errorf("Tries: want 3, got %d", meta.Tries)
	}
}

// TestGetChapterMetaNotInQueue covers the case where the chapter exists (has
// pages) but is not in Suwayomi's download queue at all — no match means
// QueueState="" and Tries=0.
func TestGetChapterMetaNotInQueue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/graphql" {
			handlerErr(t, w, "want /api/graphql, got %s", r.URL.Path)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"data": {
				"chapter": {"pageCount": 12, "isDownloaded": false},
				"downloadStatus": {"queue": []}
			}
		}`)
	}))
	defer srv.Close()

	c := New(srv.URL, NoAuth{})
	meta, err := c.GetChapterMeta(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetChapterMeta: %v", err)
	}
	if meta.PageCount != 12 {
		t.Errorf("PageCount: want 12, got %d", meta.PageCount)
	}
	if meta.IsDownloaded {
		t.Errorf("IsDownloaded: want false, got true")
	}
	if meta.QueueState != "" {
		t.Errorf("QueueState: want empty string, got %q", meta.QueueState)
	}
	if meta.Tries != 0 {
		t.Errorf("Tries: want 0, got %d", meta.Tries)
	}
}

// TestGetChapterMetaQueueWithMultipleChapters verifies that when the queue
// contains several entries only the one matching the requested chapterID
// contributes to QueueState and Tries. The other entries must be ignored.
func TestGetChapterMetaQueueWithMultipleChapters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/graphql" {
			handlerErr(t, w, "want /api/graphql, got %s", r.URL.Path)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Queue has 3 entries; only chapter 200 matches the requested ID.
		io.WriteString(w, `{
			"data": {
				"chapter": {"pageCount": 5, "isDownloaded": false},
				"downloadStatus": {
					"queue": [
						{"chapter": {"id": 100}, "state": "QUEUED",      "progress": 0.0, "tries": 0},
						{"chapter": {"id": 200}, "state": "DOWNLOADING", "progress": 0.4, "tries": 1},
						{"chapter": {"id": 300}, "state": "ERROR",       "progress": 0.0, "tries": 5}
					]
				}
			}
		}`)
	}))
	defer srv.Close()

	c := New(srv.URL, NoAuth{})
	meta, err := c.GetChapterMeta(context.Background(), 200)
	if err != nil {
		t.Fatalf("GetChapterMeta: %v", err)
	}
	if meta.PageCount != 5 {
		t.Errorf("PageCount: want 5, got %d", meta.PageCount)
	}
	if meta.QueueState != "DOWNLOADING" {
		t.Errorf("QueueState: want DOWNLOADING, got %q", meta.QueueState)
	}
	if meta.Tries != 1 {
		t.Errorf("Tries: want 1 (from id=200 entry), got %d", meta.Tries)
	}
}
