package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/filer"
	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/poller"
	"github.com/gavinmcfall/mangarr/internal/recyclebin"
)

// --- Task 3: chapterFiles stat enrichment ---

func TestChapterFilesEnrichesSizeAndStatus(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "Berserk - Ch.1.cbz")
	if err := os.WriteFile(dst, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	plans := []filer.PlanEntry{
		{SrcPath: "/dl/berserk/1.cbz", DstPath: dst, Mode: model.ModeHardlink, Action: filer.PlanSkip},                         // exists on disk
		{SrcPath: "/dl/berserk/2.cbz", DstPath: filepath.Join(dir, "missing.cbz"), Mode: model.ModeHardlink, Action: filer.PlanFile}, // not yet filed
		{SrcPath: "/dl/berserk/3.cbz", DstPath: filepath.Join(dir, "bad.cbz"), Action: filer.PlanError, Error: "boom"},          // planning error
	}
	files := chapterFiles(plans)
	if len(files) != 3 {
		t.Fatalf("want 3 rows, got %d", len(files))
	}
	if files[0].SizeBytes != 5 || files[0].Status != "filed" {
		t.Fatalf("filed row wrong: %+v", files[0])
	}
	if files[0].FileName != "Berserk - Ch.1.cbz" {
		t.Fatalf("filename not derived: %+v", files[0])
	}
	if files[0].ModTime == "" {
		t.Fatalf("modtime not set for filed row: %+v", files[0])
	}
	if files[1].Status != "missing" {
		t.Fatalf("missing row wrong: %+v", files[1])
	}
	if files[2].Status != "error" || files[2].Reason != "boom" {
		t.Fatalf("error row wrong: %+v", files[2])
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "—"},
		{512, "512 B"},
		{1536, "1.5 KB"},
		{5 * 1024 * 1024, "5.0 MB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// detailHandler builds a Handler wired with the supplied previewer, refiler,
// and recycle bin for the detail-page tests.
func detailHandler(prev Previewer, refiler SeriesRefiler, bin *recyclebin.Bin) (*Handler, *fakeStore) {
	st := &fakeStore{
		bindings: []model.Binding{{ID: 1, Name: "Manga", LibraryRoot: "/lib/Manga"}},
		settings: model.Settings{
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	h := NewHandler(HandlerOpts{
		Store:      st,
		Runner:     &fakeRunner{},
		Previewer:  prev,
		Refiler:    refiler,
		RecycleBin: bin,
	})
	return h, st
}

// --- Task 4/5: GET /series/{id} ---

func TestSeriesDetailPageRendersChapterRows(t *testing.T) {
	prev := &fakePreviewer{one: poller.PreviewEntry{
		Title:       "Berserk",
		BindingName: "Manga",
		DstRoot:     "/lib/Manga",
		Status:      "matched",
		ChapterPlans: []filer.PlanEntry{
			{SrcPath: "/dl/Berserk/1.cbz", DstPath: "/lib/Manga/Berserk/Berserk - Ch.001.cbz", Mode: model.ModeHardlink, Action: filer.PlanFile},
		},
	}}
	h, _ := detailHandler(prev, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/series/7", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Berserk", "Manga", "/lib/Manga", "Berserk - Ch.001.cbz", "Re-run filer"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestSeriesDetailPageBadID(t *testing.T) {
	h, _ := detailHandler(&fakePreviewer{}, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/series/not-a-number", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestSeriesDetailPage503WhenNoPreviewer(t *testing.T) {
	h, _ := detailHandler(nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/series/7", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// --- Task 6: POST /api/series/{id}/refile ---

type recordingRefiler struct {
	ids []int64
	err error
}

func (r *recordingRefiler) RefileOne(ctx context.Context, seriesID int64) error {
	r.ids = append(r.ids, seriesID)
	return r.err
}

func TestSeriesRefileCallsRefiler(t *testing.T) {
	rf := &recordingRefiler{}
	h, _ := detailHandler(&fakePreviewer{}, rf, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/series/7/refile", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if len(rf.ids) != 1 || rf.ids[0] != 7 {
		t.Fatalf("RefileOne called with %v, want [7]", rf.ids)
	}
	if loc := rec.Header().Get("Location"); loc != "/series/7" {
		t.Fatalf("redirect = %q, want /series/7", loc)
	}
}

func TestSeriesRefile503WhenNoRefiler(t *testing.T) {
	h, _ := detailHandler(&fakePreviewer{}, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/series/7/refile", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// --- Task 7: POST /api/series/{id}/chapter/remove ---

func TestSeriesChapterRemoveMovesToBin(t *testing.T) {
	root := t.TempDir()
	binRoot := t.TempDir()
	chapter := filepath.Join(root, "Berserk", "Berserk - Ch.001.cbz")
	if err := os.MkdirAll(filepath.Dir(chapter), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chapter, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	prev := &fakePreviewer{one: poller.PreviewEntry{
		Title:   "Berserk",
		DstRoot: root,
		Status:  "matched",
		ChapterPlans: []filer.PlanEntry{
			{SrcPath: "/dl/Berserk/1.cbz", DstPath: chapter, Action: filer.PlanSkip},
		},
	}}
	bin := &recyclebin.Bin{Root: binRoot}
	h, _ := detailHandler(prev, nil, bin)

	form := url.Values{"name": {"Berserk - Ch.001.cbz"}}
	req := httptest.NewRequest(http.MethodPost, "/api/series/7/chapter/remove", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(chapter); !os.IsNotExist(err) {
		t.Fatalf("chapter still present at source; err=%v", err)
	}
	// It should now live somewhere under the bin root.
	var found bool
	_ = filepath.Walk(binRoot, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, "Berserk - Ch.001.cbz") {
			found = true
		}
		return nil
	})
	if !found {
		t.Fatalf("chapter not found in recycle bin %s", binRoot)
	}
}

func TestSeriesChapterRemoveRejectsUnknownName(t *testing.T) {
	root := t.TempDir()
	prev := &fakePreviewer{one: poller.PreviewEntry{
		Title:        "Berserk",
		DstRoot:      root,
		Status:       "matched",
		ChapterPlans: []filer.PlanEntry{{DstPath: filepath.Join(root, "real.cbz"), Action: filer.PlanSkip}},
	}}
	bin := &recyclebin.Bin{Root: t.TempDir()}
	h, _ := detailHandler(prev, nil, bin)

	// A traversal attempt resolves to no matching plan → 400, nothing removed.
	form := url.Values{"name": {"../../etc/passwd"}}
	req := httptest.NewRequest(http.MethodPost, "/api/series/7/chapter/remove", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestSeriesChapterRemove503WhenNoBin(t *testing.T) {
	prev := &fakePreviewer{one: poller.PreviewEntry{Status: "matched"}}
	h, _ := detailHandler(prev, nil, nil)
	form := url.Values{"name": {"x.cbz"}}
	req := httptest.NewRequest(http.MethodPost, "/api/series/7/chapter/remove", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
