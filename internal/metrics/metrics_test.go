package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// scrape calls the registry's Handler and returns the response body as a string.
func scrape(t *testing.T, r *Registry) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("scrape: want 200, got %d; body=%s", rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

// TestCountersIncrement verifies that counter Inc calls appear in the scraped output.
func TestCountersIncrement(t *testing.T) {
	r := NewRegistry()

	r.IncFilesFiled("manga")
	r.IncFilesFiled("manga")
	r.IncFilesFiled("manhwa")
	r.IncKavitaScan("success")
	r.IncKavitaScan("error")
	r.IncAniListLookup("success")
	r.IncAniListLookup("miss")
	r.IncAniListLookup("cached")
	r.IncAniListLookup("error")
	r.IncUnmatched()
	r.IncUnmatched()
	r.IncUnmatched()
	r.IncFileError()

	body := scrape(t, r)

	cases := []struct {
		metric string
		want   string
	}{
		// files_filed: manga=2, manhwa=1
		{`mangarr_files_filed_total{category="manga"}`, `2`},
		{`mangarr_files_filed_total{category="manhwa"}`, `1`},
		// kavita_scan: success=1, error=1
		{`mangarr_kavita_scan_total{result="success"}`, `1`},
		{`mangarr_kavita_scan_total{result="error"}`, `1`},
		// anilist_lookups: success=1, miss=1, cached=1, error=1
		{`mangarr_anilist_lookups_total{result="success"}`, `1`},
		{`mangarr_anilist_lookups_total{result="miss"}`, `1`},
		{`mangarr_anilist_lookups_total{result="cached"}`, `1`},
		{`mangarr_anilist_lookups_total{result="error"}`, `1`},
		// unmatched: 3
		{`mangarr_unmatched_total`, `3`},
		// file_errors: 1
		{`mangarr_file_errors_total`, `1`},
	}

	for _, tc := range cases {
		// Find the line containing the metric + label combination.
		found := false
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, tc.metric) {
				// The value is the last space-separated field.
				parts := strings.Fields(line)
				if len(parts) < 2 {
					continue
				}
				val := parts[len(parts)-1]
				if val != tc.want {
					t.Errorf("metric %s: want value %s, got %s (line: %q)",
						tc.metric, tc.want, val, line)
				}
				found = true
				break
			}
		}
		if !found {
			t.Errorf("metric %s not found in scrape output", tc.metric)
		}
	}
}

// TestGaugesSet verifies that gauge Set calls appear with correct values in the output.
func TestGaugesSet(t *testing.T) {
	r := NewRegistry()

	now := time.Unix(1748476800, 0) // fixed timestamp for determinism
	r.SetPollerLastRun(now)
	r.SetUnmatchedCount(7)
	r.SetSeriesCount("manga", 42)
	r.SetSeriesCount("manhwa", 13)
	r.SetDiskFreeBytes("/mnt/media", 1073741824)  // 1 GiB
	r.SetDiskTotalBytes("/mnt/media", 4294967296) // 4 GiB
	r.SetHealthStatus("sqlite", 0)
	r.SetHealthStatus("kavita", 2)
	r.SetBackupCount(5)
	r.SetBackupLastModTime(now)

	body := scrape(t, r)

	cases := []struct {
		metric string
		want   string
	}{
		{`mangarr_poller_last_run_timestamp_seconds`, `1.7484768e+09`},
		{`mangarr_unmatched_series`, `7`},
		{`mangarr_series_count{category="manga"}`, `42`},
		{`mangarr_series_count{category="manhwa"}`, `13`},
		{`mangarr_disk_free_bytes{root="/mnt/media"}`, `1.073741824e+09`},
		{`mangarr_disk_total_bytes{root="/mnt/media"}`, `4.294967296e+09`},
		{`mangarr_health_status{id="sqlite"}`, `0`},
		{`mangarr_health_status{id="kavita"}`, `2`},
		{`mangarr_backups_count`, `5`},
		{`mangarr_backup_last_modtime_seconds`, `1.7484768e+09`},
	}

	for _, tc := range cases {
		found := false
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, tc.metric) && !strings.HasPrefix(line, "#") {
				parts := strings.Fields(line)
				if len(parts) < 2 {
					continue
				}
				val := parts[len(parts)-1]
				if val != tc.want {
					t.Errorf("metric %s: want value %s, got %s (line: %q)",
						tc.metric, tc.want, val, line)
				}
				found = true
				break
			}
		}
		if !found {
			t.Errorf("metric %s not found in scrape output; body snippet:\n%s",
				tc.metric, body[:min(len(body), 1000)])
		}
	}
}

// TestHandlerReturnsContentType verifies the Prometheus text format Content-Type header.
func TestHandlerReturnsContentType(t *testing.T) {
	r := NewRegistry()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d; body=%s", rr.Code, rr.Body.String())
	}
	ct := rr.Header().Get("Content-Type")
	// Prometheus text format 0.0.4
	if !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("want Content-Type starting with text/plain, got %q", ct)
	}
	if !strings.Contains(ct, "version=0.0.4") {
		t.Errorf("want Content-Type to contain version=0.0.4, got %q", ct)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
