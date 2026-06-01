package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/model"
)

func TestPageDownloadsRendersTableShellWith3sPoll(t *testing.T) {
	h, _, _ := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/downloads", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<h1", "Downloads",
		// Tabs
		"Active", "All",
		// Polling on tbody
		`hx-get="/api/downloads/list?filter=active"`,
		`hx-trigger="every 3s"`,
		`hx-swap="outerHTML"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("downloads page missing %q", want)
		}
	}
}

func TestPageDownloadsEmptyState(t *testing.T) {
	h, _, _ := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/downloads", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "No bulk downloads") {
		t.Errorf("expected empty-state copy")
	}
}

func TestAPIDownloadsListFiltersActiveVsAll(t *testing.T) {
	h, st, _ := newTestHandler()
	st.bulkJobs = []model.BulkJob{
		{ID: 1, Title: "Running", SourceName: "S", Status: model.BulkJobRunning, TotalChapters: 10, CompletedChapters: 3},
		{ID: 2, Title: "Paused", SourceName: "S", Status: model.BulkJobPaused, TotalChapters: 10, CompletedChapters: 5},
		{ID: 3, Title: "Done", SourceName: "S", Status: model.BulkJobCompleted, TotalChapters: 10, CompletedChapters: 10},
		{ID: 4, Title: "Errored", SourceName: "S", Status: model.BulkJobErrored, TotalChapters: 10, CompletedChapters: 2},
	}

	// Active filter excludes completed.
	req := httptest.NewRequest(http.MethodGet, "/api/downloads/list?filter=active", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "Running") || !strings.Contains(body, "Paused") || !strings.Contains(body, "Errored") {
		t.Errorf("active filter dropped expected jobs")
	}
	if strings.Contains(body, "Done") {
		t.Errorf("active filter included completed job")
	}

	// All filter includes completed.
	req2 := httptest.NewRequest(http.MethodGet, "/api/downloads/list?filter=all", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	body2 := rec2.Body.String()
	for _, want := range []string{"Running", "Paused", "Done", "Errored"} {
		if !strings.Contains(body2, want) {
			t.Errorf("all filter missing %q", want)
		}
	}
}

func TestStaticCSSContainsBulkProgressStyles(t *testing.T) {
	h, _, _ := newTestHandler()
	req := httptest.NewRequest(http.MethodGet, "/static/mangarr.css", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for css, got %d", rec.Code)
	}
	css := rec.Body.String()
	for _, want := range []string{
		".bulk-progress",
		".bulk-progress-bar",
		".pill-paused",
		".pill-errored",
		".modal-shell",
		".modal-card",
		".library-action-bar",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("mangarr.css missing rule for %q", want)
		}
	}
}
