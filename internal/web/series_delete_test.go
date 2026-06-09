package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/recyclebin"
)

func TestBinSeriesFilesMovesFilesAndRemovesDir(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "series")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"Ch.1.cbz", "Ch.2.cbz"} {
		if err := os.WriteFile(filepath.Join(src, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bin := &recyclebin.Bin{Root: filepath.Join(tmp, "bin"), Retention: time.Hour}
	if err := binSeriesFiles(bin, []string{src}, time.Unix(1700000000, 0)); err != nil {
		t.Fatalf("binSeriesFiles: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source dir not removed: %v", err)
	}
	entries, _ := filepath.Glob(filepath.Join(tmp, "bin", "*", "*.cbz"))
	if len(entries) != 2 {
		t.Errorf("want 2 files in bin, got %d", len(entries))
	}
}

func TestBinSeriesFilesMissingDirIsNoOp(t *testing.T) {
	tmp := t.TempDir()
	bin := &recyclebin.Bin{Root: filepath.Join(tmp, "bin"), Retention: time.Hour}
	if err := binSeriesFiles(bin, []string{filepath.Join(tmp, "ghost")}, time.Unix(1700000000, 0)); err != nil {
		t.Errorf("missing dir should be a no-op, got %v", err)
	}
}

func TestBinSeriesFilesRefusesShallowPath(t *testing.T) {
	bin := &recyclebin.Bin{Root: t.TempDir(), Retention: time.Hour}
	for _, p := range []string{"/", "/home", "."} {
		if err := binSeriesFiles(bin, []string{p}, time.Unix(1700000000, 0)); err == nil {
			t.Errorf("expected error for shallow path %q, got nil", p)
		}
	}
}

// --- HTTP-level handler tests ---

// deleteHandler builds a Handler wired with the supplied store, previewer, and
// recycle bin for the delete/restore handler tests.
func deleteHandler(st *fakeStore, prev Previewer, bin *recyclebin.Bin) *Handler {
	return NewHandler(HandlerOpts{
		Store:      st,
		Runner:     &fakeRunner{},
		Previewer:  prev,
		RecycleBin: bin,
	})
}

// TestAPISeriesDeleteUnknownSeriesRedirects checks that DELETE on a series that
// no longer exists produces a 303 redirect to /series (not a 500 or 404 error).
func TestAPISeriesDeleteUnknownSeriesRedirects(t *testing.T) {
	st := &fakeStore{
		settings: model.Settings{
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
		// series is empty so any GetSeriesByID returns sql.ErrNoRows
	}
	h := deleteHandler(st, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/series/999/delete", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/series" {
		t.Errorf("want redirect to /series, got %q", loc)
	}
}

// TestAPISeriesDeleteWithFilesRecyclesBinsSourceAndCallsDeleteSeries checks
// that delete_files=true bins the source dir's file and calls DeleteSeries.
func TestAPISeriesDeleteWithFilesRecyclesBinsSourceAndCallsDeleteSeries(t *testing.T) {
	tmp := t.TempDir()
	// Build a real source dir with a chapter file deep enough to pass the
	// shallow-path guard (needs ≥3 path segments under root).
	srcDir := filepath.Join(tmp, "downloads", "manga", "TestSeries")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "Ch.1.cbz"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	binRoot := filepath.Join(tmp, "bin")

	series := model.Series{
		ID:         7,
		Title:      "TestSeries",
		SourcePath: srcDir,
		Status:     model.StatusOrphaned,
	}
	st := &fakeStore{
		series: []model.Series{series},
		settings: model.Settings{
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	bin := &recyclebin.Bin{Root: binRoot, Retention: time.Hour}
	// fakePreviewer.ResolveLibraryDir returns "" so only the source is binned.
	prev := &fakePreviewer{}
	h := deleteHandler(st, prev, bin)

	form := url.Values{"delete_files": {"true"}}
	req := httptest.NewRequest(http.MethodPost, "/api/series/7/delete",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d; body: %s", rec.Code, rec.Body.String())
	}
	// Source dir must be gone.
	if _, err := os.Stat(srcDir); !os.IsNotExist(err) {
		t.Errorf("source dir should have been removed; stat err: %v", err)
	}
	// DeleteSeries must have been called — fakeStore removes the row.
	for _, s := range st.series {
		if s.ID == 7 {
			t.Error("series row still present; DeleteSeries was not called")
		}
	}
}

// TestAPISeriesRestoreCallsStoreAndRedirects checks that restore clears
// missing_since, sets status to pending, and redirects to /series/{id}.
func TestAPISeriesRestoreCallsStoreAndRedirects(t *testing.T) {
	now := time.Now()
	series := model.Series{
		ID:           7,
		Title:        "TestSeries",
		SourcePath:   "/some/path/to/series",
		Status:       model.StatusOrphaned,
		MissingSince: &now,
	}
	st := &fakeStore{
		series: []model.Series{series},
		settings: model.Settings{
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	h := deleteHandler(st, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/series/7/restore", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d; body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/series/7" {
		t.Errorf("want redirect to /series/7, got %q", loc)
	}
	// Verify store mutations: MissingSince cleared and Status set to pending.
	for _, s := range st.series {
		if s.ID != 7 {
			continue
		}
		if s.MissingSince != nil {
			t.Errorf("MissingSince should be nil after restore, got %v", s.MissingSince)
		}
		if s.Status != model.StatusPending {
			t.Errorf("Status should be pending after restore, got %q", s.Status)
		}
	}
}
