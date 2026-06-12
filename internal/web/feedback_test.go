package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/suwayomi"
)

// flashValue extracts the decoded mangarr_flash cookie value ("kind|msg") from
// a response, or "" if absent.
func flashValue(t *testing.T, res *http.Response) string {
	t.Helper()
	for _, c := range res.Cookies() {
		if c.Name == flashCookie {
			dec, err := url.QueryUnescape(c.Value)
			if err != nil {
				t.Fatalf("flash cookie value %q not URL-decodable: %v", c.Value, err)
			}
			return dec
		}
	}
	return ""
}

func TestSetFlashWritesCookie(t *testing.T) {
	rec := httptest.NewRecorder()
	setFlash(rec, "success", "Synced 3 series")
	got := flashValue(t, rec.Result())
	if got != "success|Synced 3 series" {
		t.Fatalf("flash = %q, want %q", got, "success|Synced 3 series")
	}
}

// Every page is wrapped in base.html, so the feedback markup must be present.
func TestBaseRendersFeedbackMarkup(t *testing.T) {
	h, _, _, _ := newTestHandlerFull()
	req := httptest.NewRequest(http.MethodGet, "/series", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /series = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`id="mangarr-progress"`, `id="toast-container"`, "mangarr_flash", "mangarrToast"} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page missing %q", want)
		}
	}
}

func TestAPILibrarySyncSetsFlash(t *testing.T) {
	h, _, sw, _ := newTestHandlerFull()
	sw.libraryEntries = []suwayomi.Manga{
		{ID: 10, Title: "A", Source: "Weeb Central (EN)", SourceID: "1"},
		{ID: 11, Title: "B", Source: "Weeb Central (EN)", SourceID: "1"},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/library/sync", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync = %d, want 200", rec.Code)
	}
	if got := flashValue(t, rec.Result()); got != "success|Synced 2 series" {
		t.Fatalf("flash = %q, want %q", got, "success|Synced 2 series")
	}
}

func TestAPISeriesRestoreSetsFlash(t *testing.T) {
	h, _, _, _ := newTestHandlerFull()
	req := httptest.NewRequest(http.MethodPost, "/api/series/1/restore", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("restore = %d, want 303", rec.Code)
	}
	if got := flashValue(t, rec.Result()); got != "success|Restored series" {
		t.Fatalf("flash = %q, want %q", got, "success|Restored series")
	}
}

func TestAPISeriesDeleteMangarrOnlySetsFlash(t *testing.T) {
	h, _, _, _ := newTestHandlerFull()
	form := strings.NewReader("delete_files=false")
	req := httptest.NewRequest(http.MethodPost, "/api/series/1/delete", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete = %d, want 303", rec.Code)
	}
	if got := flashValue(t, rec.Result()); got != "success|Removed series" {
		t.Fatalf("flash = %q, want %q", got, "success|Removed series")
	}
}
