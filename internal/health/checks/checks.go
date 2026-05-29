// Package checks provides mangarr-specific health-check factories.
//
// Each exported function returns a health.Check pre-configured with
// its ID and dependency. Register the returned checks with a
// health.Registry in main.go.
package checks

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gavinmcfall/mangarr/internal/diskspace"
	"github.com/gavinmcfall/mangarr/internal/filer"
	"github.com/gavinmcfall/mangarr/internal/health"
	"github.com/gavinmcfall/mangarr/internal/model"
)

// KavitaPinger is the subset of kavita.Client needed for the health check.
type KavitaPinger interface {
	Ping(ctx context.Context) error
}

// DownloadRootsCheck returns a Check that verifies all configured download
// roots exist and are readable directories.
func DownloadRootsCheck(roots []string) health.Check {
	return health.Check{
		ID:   "download-roots",
		Name: "Download roots",
		Run: func(ctx context.Context) health.Result {
			if len(roots) == 0 {
				return health.Result{
					ID:          "download-roots",
					Name:        "Download roots",
					Status:      health.StatusWarn,
					Message:     "No download roots configured",
					Remediation: "Set MANGARR_DOWNLOAD_ROOTS to one or more comma-separated paths.",
				}
			}
			var failed []string
			ok := 0
			for _, root := range roots {
				info, err := os.Stat(root)
				if err != nil || !info.IsDir() {
					failed = append(failed, root)
				} else {
					ok++
				}
			}
			if len(failed) == 0 {
				return health.Result{
					ID:      "download-roots",
					Name:    "Download roots",
					Status:  health.StatusOK,
					Message: fmt.Sprintf("%d/%d roots accessible", ok, len(roots)),
				}
			}
			return health.Result{
				ID:          "download-roots",
				Name:        "Download roots",
				Status:      health.StatusError,
				Message:     fmt.Sprintf("%d/%d roots inaccessible: %s", len(failed), len(roots), strings.Join(failed, ", ")),
				Remediation: "Ensure all download root directories exist and are readable by the mangarr process.",
			}
		},
	}
}

// LibraryRootsCheck returns a Check that verifies all configured library
// roots exist and are writable.
func LibraryRootsCheck(settingsLoader func() (model.Settings, error)) health.Check {
	return health.Check{
		ID:   "library-roots",
		Name: "Library roots",
		Run: func(ctx context.Context) health.Result {
			settings, err := settingsLoader()
			if err != nil {
				return health.Result{
					ID:          "library-roots",
					Name:        "Library roots",
					Status:      health.StatusError,
					Message:     "Failed to load settings: " + err.Error(),
					Remediation: "Check database connectivity.",
				}
			}
			if len(settings.LibraryRoots) == 0 {
				return health.Result{
					ID:          "library-roots",
					Name:        "Library roots",
					Status:      health.StatusError,
					Message:     "No library roots configured",
					Remediation: "Configure at least one library root path in Settings.",
				}
			}

			var missing []string
			var unwritable []string
			ok := 0
			for ct, path := range settings.LibraryRoots {
				if path == "" {
					continue
				}
				info, err := os.Stat(path)
				if err != nil || !info.IsDir() {
					missing = append(missing, fmt.Sprintf("%s(%s)", path, ct))
					continue
				}
				// Write-test: create a temp dir and remove it.
				testDir := path + "/.mangarr-health-probe"
				if mkErr := os.Mkdir(testDir, 0o700); mkErr != nil {
					unwritable = append(unwritable, fmt.Sprintf("%s(%s)", path, ct))
					continue
				}
				os.Remove(testDir)
				ok++
			}

			switch {
			case len(missing) > 0:
				return health.Result{
					ID:          "library-roots",
					Name:        "Library roots",
					Status:      health.StatusWarn,
					Message:     fmt.Sprintf("%d root(s) missing: %s", len(missing), strings.Join(missing, ", ")),
					Remediation: "Ensure library root directories exist and are mounted.",
				}
			case len(unwritable) > 0:
				return health.Result{
					ID:          "library-roots",
					Name:        "Library roots",
					Status:      health.StatusError,
					Message:     fmt.Sprintf("%d root(s) not writable: %s", len(unwritable), strings.Join(unwritable, ", ")),
					Remediation: "Ensure the mangarr process has write permission on all library roots.",
				}
			default:
				return health.Result{
					ID:      "library-roots",
					Name:    "Library roots",
					Status:  health.StatusOK,
					Message: fmt.Sprintf("%d root(s) accessible and writable", ok),
				}
			}
		},
	}
}

// KavitaCheck returns a Check that verifies the Kavita client can authenticate.
func KavitaCheck(client KavitaPinger) health.Check {
	return health.Check{
		ID:   "kavita",
		Name: "Kavita",
		Run: func(ctx context.Context) health.Result {
			if client == nil {
				return health.Result{
					ID:          "kavita",
					Name:        "Kavita",
					Status:      health.StatusWarn,
					Message:     "Kavita client not configured",
					Remediation: "Set Kavita base URL and API key in Settings.",
				}
			}
			if err := client.Ping(ctx); err != nil {
				return health.Result{
					ID:          "kavita",
					Name:        "Kavita",
					Status:      health.StatusError,
					Message:     "Kavita unreachable: " + err.Error(),
					Remediation: "Check Kavita base URL, API key, and network connectivity.",
				}
			}
			return health.Result{
				ID:      "kavita",
				Name:    "Kavita",
				Status:  health.StatusOK,
				Message: "Authentication successful",
			}
		},
	}
}

// AniListCheck returns a Check that verifies the AniList endpoint is reachable.
// It sends a HEAD request (no GraphQL query, no rate-limit cost).
func AniListCheck(endpoint string) health.Check {
	return health.Check{
		ID:   "anilist",
		Name: "AniList",
		Run: func(ctx context.Context) health.Result {
			url := endpoint
			if url == "" {
				url = "https://graphql.anilist.co"
			}
			reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(reqCtx, http.MethodHead, url, nil)
			if err != nil {
				return health.Result{
					ID:          "anilist",
					Name:        "AniList",
					Status:      health.StatusError,
					Message:     "Failed to build request: " + err.Error(),
					Remediation: "Check MANGARR_ANILIST_ENDPOINT configuration.",
				}
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return health.Result{
					ID:          "anilist",
					Name:        "AniList",
					Status:      health.StatusError,
					Message:     "AniList unreachable: " + err.Error(),
					Remediation: "Check network connectivity to the AniList GraphQL endpoint.",
				}
			}
			resp.Body.Close()
			// Any HTTP response (even 4xx) means the endpoint is reachable.
			return health.Result{
				ID:      "anilist",
				Name:    "AniList",
				Status:  health.StatusOK,
				Message: fmt.Sprintf("Reachable (HTTP %d)", resp.StatusCode),
			}
		},
	}
}

// SQLiteCheck returns a Check that verifies the SQLite database is healthy.
func SQLiteCheck(db *sql.DB) health.Check {
	return health.Check{
		ID:   "sqlite",
		Name: "SQLite database",
		Run: func(ctx context.Context) health.Result {
			if err := db.PingContext(ctx); err != nil {
				return health.Result{
					ID:          "sqlite",
					Name:        "SQLite database",
					Status:      health.StatusError,
					Message:     "Ping failed: " + err.Error(),
					Remediation: "Check database file permissions and disk space.",
				}
			}
			var v int
			if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&v); err != nil || v != 1 {
				msg := "SELECT 1 failed"
				if err != nil {
					msg = err.Error()
				}
				return health.Result{
					ID:          "sqlite",
					Name:        "SQLite database",
					Status:      health.StatusError,
					Message:     msg,
					Remediation: "Database may be corrupt. Check disk health.",
				}
			}
			return health.Result{
				ID:      "sqlite",
				Name:    "SQLite database",
				Status:  health.StatusOK,
				Message: "Ping OK, SELECT 1 OK",
			}
		},
	}
}

// DiskSpaceCheck returns a Check that monitors free space across all roots.
// warnPct and errPct are thresholds for the percentage of free space
// (e.g. warnPct=15, errPct=5 means warn below 15% free, error below 5% free).
func DiskSpaceCheck(roots []string, warnPct, errPct float64) health.Check {
	return health.Check{
		ID:   "disk-space",
		Name: "Disk space",
		Run: func(ctx context.Context) health.Result {
			if len(roots) == 0 {
				return health.Result{
					ID:      "disk-space",
					Name:    "Disk space",
					Status:  health.StatusOK,
					Message: "No roots configured",
				}
			}
			var errorRoots []string
			var warnRoots []string
			var okCount int
			for _, root := range roots {
				info := diskspace.Stat(root)
				if info.Err != nil {
					errorRoots = append(errorRoots, fmt.Sprintf("%s (unavailable)", root))
					continue
				}
				pct := info.PercentFree()
				switch {
				case pct < errPct:
					errorRoots = append(errorRoots, fmt.Sprintf("%s (%.1f%% free)", root, pct))
				case pct < warnPct:
					warnRoots = append(warnRoots, fmt.Sprintf("%s (%.1f%% free)", root, pct))
				default:
					okCount++
				}
			}
			switch {
			case len(errorRoots) > 0:
				return health.Result{
					ID:          "disk-space",
					Name:        "Disk space",
					Status:      health.StatusError,
					Message:     fmt.Sprintf("Critical: %s", strings.Join(errorRoots, "; ")),
					Remediation: "Free up disk space or expand the filesystem.",
				}
			case len(warnRoots) > 0:
				return health.Result{
					ID:          "disk-space",
					Name:        "Disk space",
					Status:      health.StatusWarn,
					Message:     fmt.Sprintf("Low space: %s", strings.Join(warnRoots, "; ")),
					Remediation: "Consider freeing up disk space soon.",
				}
			default:
				return health.Result{
					ID:      "disk-space",
					Name:    "Disk space",
					Status:  health.StatusOK,
					Message: fmt.Sprintf("%d root(s) have adequate free space", okCount),
				}
			}
		},
	}
}

// RenameSchemeCheck returns a Check that validates the configured rename scheme.
func RenameSchemeCheck(settingsLoader func() (model.Settings, error)) health.Check {
	return health.Check{
		ID:   "rename-scheme",
		Name: "Rename scheme",
		Run: func(ctx context.Context) health.Result {
			settings, err := settingsLoader()
			if err != nil {
				return health.Result{
					ID:          "rename-scheme",
					Name:        "Rename scheme",
					Status:      health.StatusError,
					Message:     "Failed to load settings: " + err.Error(),
					Remediation: "Check database connectivity.",
				}
			}
			if err := filer.ValidateScheme(settings.RenameScheme); err != nil {
				return health.Result{
					ID:          "rename-scheme",
					Name:        "Rename scheme",
					Status:      health.StatusError,
					Message:     "Invalid scheme: " + err.Error(),
					Remediation: "Update the rename scheme in Settings to a valid pattern.",
				}
			}
			return health.Result{
				ID:      "rename-scheme",
				Name:    "Rename scheme",
				Status:  health.StatusOK,
				Message: fmt.Sprintf("Valid: %q", settings.RenameScheme),
			}
		},
	}
}
