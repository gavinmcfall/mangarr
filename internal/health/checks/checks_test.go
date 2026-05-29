package checks

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/health"
	"github.com/gavinmcfall/mangarr/internal/model"
	_ "modernc.org/sqlite"
)

// ---- DownloadRootsCheck ----

func TestDownloadRootsCheckOK(t *testing.T) {
	dir := t.TempDir()
	c := DownloadRootsCheck([]string{dir})
	res := c.Run(context.Background())
	if res.Status != health.StatusOK {
		t.Errorf("want StatusOK, got %q: %s", res.Status, res.Message)
	}
}

func TestDownloadRootsCheckMissingPath(t *testing.T) {
	c := DownloadRootsCheck([]string{"/this/does/not/exist/at/all"})
	res := c.Run(context.Background())
	if res.Status != health.StatusError {
		t.Errorf("want StatusError for missing path, got %q: %s", res.Status, res.Message)
	}
}

func TestDownloadRootsCheckNoRoots(t *testing.T) {
	c := DownloadRootsCheck([]string{})
	res := c.Run(context.Background())
	if res.Status != health.StatusWarn {
		t.Errorf("want StatusWarn for empty roots, got %q: %s", res.Status, res.Message)
	}
}

// ---- LibraryRootsCheck ----

func TestLibraryRootsCheckNoSettings(t *testing.T) {
	loader := func() (model.Settings, error) {
		return model.Settings{}, nil
	}
	c := LibraryRootsCheck(loader)
	res := c.Run(context.Background())
	if res.Status != health.StatusError {
		t.Errorf("want StatusError when no library roots, got %q: %s", res.Status, res.Message)
	}
}

func TestLibraryRootsCheckOK(t *testing.T) {
	dir := t.TempDir()
	loader := func() (model.Settings, error) {
		return model.Settings{
			LibraryRoots: map[model.ContentType]string{
				model.TypeManga: dir,
			},
		}, nil
	}
	c := LibraryRootsCheck(loader)
	res := c.Run(context.Background())
	if res.Status != health.StatusOK {
		t.Errorf("want StatusOK for writable dir, got %q: %s", res.Status, res.Message)
	}
}

func TestLibraryRootsCheckMissingDir(t *testing.T) {
	loader := func() (model.Settings, error) {
		return model.Settings{
			LibraryRoots: map[model.ContentType]string{
				model.TypeManga: "/does/not/exist",
			},
		}, nil
	}
	c := LibraryRootsCheck(loader)
	res := c.Run(context.Background())
	if res.Status != health.StatusWarn {
		t.Errorf("want StatusWarn for missing dir, got %q: %s", res.Status, res.Message)
	}
}

func TestLibraryRootsCheckLoaderError(t *testing.T) {
	loader := func() (model.Settings, error) {
		return model.Settings{}, errors.New("db error")
	}
	c := LibraryRootsCheck(loader)
	res := c.Run(context.Background())
	if res.Status != health.StatusError {
		t.Errorf("want StatusError on loader failure, got %q: %s", res.Status, res.Message)
	}
}

// ---- KavitaCheck ----

type stubPinger struct{ err error }

func (s *stubPinger) Ping(_ context.Context) error { return s.err }

func TestKavitaCheckOK(t *testing.T) {
	c := KavitaCheck(&stubPinger{err: nil})
	res := c.Run(context.Background())
	if res.Status != health.StatusOK {
		t.Errorf("want StatusOK, got %q: %s", res.Status, res.Message)
	}
}

func TestKavitaCheckError(t *testing.T) {
	c := KavitaCheck(&stubPinger{err: errors.New("connection refused")})
	res := c.Run(context.Background())
	if res.Status != health.StatusError {
		t.Errorf("want StatusError, got %q: %s", res.Status, res.Message)
	}
}

func TestKavitaCheckNilClient(t *testing.T) {
	c := KavitaCheck(nil)
	res := c.Run(context.Background())
	if res.Status != health.StatusWarn {
		t.Errorf("want StatusWarn for nil client, got %q: %s", res.Status, res.Message)
	}
}

// ---- AniListCheck ----

func TestAniListCheckOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := AniListCheck(srv.URL)
	res := c.Run(context.Background())
	if res.Status != health.StatusOK {
		t.Errorf("want StatusOK, got %q: %s", res.Status, res.Message)
	}
}

func TestAniListCheckUnroutable(t *testing.T) {
	// Port :0 is not bindable as a server; connecting to it should fail fast.
	c := AniListCheck("http://127.0.0.1:0/graphql")
	res := c.Run(context.Background())
	if res.Status != health.StatusError {
		t.Errorf("want StatusError for un-routable address, got %q: %s", res.Status, res.Message)
	}
}

// ---- SQLiteCheck ----

func TestSQLiteCheckOK(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	c := SQLiteCheck(db)
	res := c.Run(context.Background())
	if res.Status != health.StatusOK {
		t.Errorf("want StatusOK, got %q: %s", res.Status, res.Message)
	}
}

func TestSQLiteCheckClosed(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.Close() // close immediately

	c := SQLiteCheck(db)
	res := c.Run(context.Background())
	if res.Status != health.StatusError {
		t.Errorf("want StatusError for closed db, got %q: %s", res.Status, res.Message)
	}
}

// ---- DiskSpaceCheck ----

func TestDiskSpaceCheckOK(t *testing.T) {
	// /tmp should have plenty of free space under normal conditions.
	c := DiskSpaceCheck([]string{"/tmp"}, 5, 1)
	res := c.Run(context.Background())
	// We don't know the exact free space, but /tmp should be above 1%.
	if res.Status == health.StatusError && res.Message != "Critical: /tmp (unavailable)" {
		// Only fail if it's an unexpected error (not just "low disk").
		t.Errorf("unexpected error for /tmp: %q: %s", res.Status, res.Message)
	}
}

func TestDiskSpaceCheckWarn(t *testing.T) {
	// Set impossibly high warn threshold (110%) so /tmp always warns.
	c := DiskSpaceCheck([]string{"/tmp"}, 110, 105)
	res := c.Run(context.Background())
	// /tmp statfs should succeed; result should be warn or error (both indicate
	// threshold breach), never ok.
	if res.Status == health.StatusOK {
		t.Errorf("want warn/error with 110%% threshold, got OK: %s", res.Message)
	}
}

func TestDiskSpaceCheckNoRoots(t *testing.T) {
	c := DiskSpaceCheck([]string{}, 15, 5)
	res := c.Run(context.Background())
	if res.Status != health.StatusOK {
		t.Errorf("want StatusOK for empty roots, got %q", res.Status)
	}
}

func TestDiskSpaceCheckBadPath(t *testing.T) {
	c := DiskSpaceCheck([]string{"/this/absolutely/does/not/exist"}, 15, 5)
	res := c.Run(context.Background())
	if res.Status != health.StatusError {
		t.Errorf("want StatusError for bad path, got %q: %s", res.Status, res.Message)
	}
}

// ---- RenameSchemeCheck ----

func TestRenameSchemeCheckOK(t *testing.T) {
	loader := func() (model.Settings, error) {
		return model.Settings{
			RenameScheme: "{series}/{series} - Ch.{chapter}.cbz",
		}, nil
	}
	c := RenameSchemeCheck(loader)
	res := c.Run(context.Background())
	if res.Status != health.StatusOK {
		t.Errorf("want StatusOK for valid scheme, got %q: %s", res.Status, res.Message)
	}
}

func TestRenameSchemeCheckInvalid(t *testing.T) {
	loader := func() (model.Settings, error) {
		return model.Settings{
			RenameScheme: "{volume}",
		}, nil
	}
	c := RenameSchemeCheck(loader)
	res := c.Run(context.Background())
	if res.Status != health.StatusError {
		t.Errorf("want StatusError for invalid scheme, got %q: %s", res.Status, res.Message)
	}
}

func TestRenameSchemeCheckLoaderError(t *testing.T) {
	loader := func() (model.Settings, error) {
		return model.Settings{}, errors.New("db down")
	}
	c := RenameSchemeCheck(loader)
	res := c.Run(context.Background())
	if res.Status != health.StatusError {
		t.Errorf("want StatusError on loader failure, got %q: %s", res.Status, res.Message)
	}
}

// ---- ensure check IDs are set correctly ----

func TestCheckIDsAreSet(t *testing.T) {
	checks := []struct {
		name string
		id   string
		c    health.Check
	}{
		{"DownloadRoots", "download-roots", DownloadRootsCheck([]string{"/tmp"})},
		{"LibraryRoots", "library-roots", LibraryRootsCheck(func() (model.Settings, error) { return model.Settings{}, nil })},
		{"Kavita", "kavita", KavitaCheck(nil)},
		{"AniList", "anilist", AniListCheck("")},
		{"SQLite", "sqlite", SQLiteCheck(nil)},
		{"DiskSpace", "disk-space", DiskSpaceCheck(nil, 15, 5)},
		{"RenameScheme", "rename-scheme", RenameSchemeCheck(func() (model.Settings, error) { return model.Settings{}, nil })},
	}
	for _, tc := range checks {
		if tc.c.ID != tc.id {
			t.Errorf("%s: want ID=%q, got %q", tc.name, tc.id, tc.c.ID)
		}
	}
}

// ---- helper for write-test coverage ----

func TestLibraryRootsCheckReadOnlyDir(t *testing.T) {
	dir := t.TempDir()
	// Make dir read-only so mkdir inside fails.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skip("cannot chmod: " + err.Error())
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	// Skip on root (root can write to 0o500 dirs).
	if os.Getuid() == 0 {
		t.Skip("running as root, chmod write-protection test not meaningful")
	}

	loader := func() (model.Settings, error) {
		return model.Settings{
			LibraryRoots: map[model.ContentType]string{
				model.TypeManga: dir,
			},
		}, nil
	}
	c := LibraryRootsCheck(loader)
	res := c.Run(context.Background())
	if res.Status != health.StatusError {
		t.Errorf("want StatusError for read-only dir, got %q: %s", res.Status, res.Message)
	}
}
