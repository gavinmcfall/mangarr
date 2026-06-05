# Series Detail Page Implementation Plan (Sonarr port #8)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A per-series detail page (`/series/{id}`) listing every chapter file with source path, target path, link mode, status, size, and mtime — plus per-series actions (re-classify, re-run filer) and a per-chapter remove-to-bin — so an operator can debug "did mangarr file ch.42 correctly?" without shelling into the pod.

**Architecture:** Reuse the existing Preview pipeline. The poller already classifies a series → resolves its binding's `LibraryRoot` → calls `filer.Plan` to produce per-chapter `PlanEntry{SrcPath, DstPath, Mode, Action}`. A new `PreviewOne(ctx, seriesID)` runs that for a single persisted series. The web layer enriches each PlanEntry with on-disk size + mtime (stat) into a detail view-model and renders the page. Actions reuse existing endpoints where possible (re-classify) and add focused, path-traversal-guarded handlers for re-run filer and remove-to-bin.

**Tech Stack:** Go 1.25, `modernc.org/sqlite`, HTMX 1.x, server-rendered templates.

**Scope decisions (baked; flag if different preference):**
- **Re-run filer** files the series via its current classification (same logic as a poll tick for one series), not a v1 ContentType. Added as `poller.RefileOne(ctx, seriesID)`.
- **Remove-to-bin** is per chapter file, addressed by FILENAME (not full path) and resolved server-side under the series' destination root with the same traversal guard `filer.File` uses, then `recyclebin.Bin.Send`. The poller already owns a `*recyclebin.Bin`.
- The page shows files from the **destination** (library) side — that's where "did it file correctly?" is answered. Source-side missing files surface as `Action: file` (planned-but-not-yet-filed) rows.

---

## File Structure

- `internal/poller/poller.go` — **Modify.** Add `PreviewOne(ctx, seriesID int64) (PreviewEntry, error)` (single-series Preview) and `RefileOne(ctx, seriesID int64) error` (classify + File one persisted series). Both reuse existing helpers (`loadBindings`, `resolveManualOverride`, `Planner.Plan`, `Filer.File`).
- `internal/web/series_detail.go` — **Create.** The detail page handler, its view-models, the refile + remove-chapter action handlers, and the stat-enrichment helper. Keeps this feature's surface out of the already-large `web.go`.
- `internal/web/web.go` — **Modify.** Register the 3 new routes; widen the web `Previewer`/`SeriesFiler`-style interfaces as needed with `PreviewOne`/`RefileOne`; nothing else.
- `internal/web/templates/series-detail.html` — **Create.** The detail page.
- `internal/web/templates/series.html` — **Modify.** Make each title link to `/series/{id}`.
- `internal/web/static/mangarr.css` — **Modify.** Detail-page table + action button styles.

---

## Task 1: poller `PreviewOne`

**Files:**
- Modify: `internal/poller/poller.go`
- Test: `internal/poller/poller_test.go`

`PreviewOne` mirrors `Preview` for one series, looked up from the store by ID (so it carries the persisted SourcePath + manual override), returning the same `PreviewEntry` (Title, SourcePath, BindingName, DstRoot, Status, ChapterPlans, Reason/Note).

- [ ] **Step 1: Write the failing test**

```go
func TestPreviewOneResolvesBindingAndPlans(t *testing.T) {
	// fakeSeriesStore returns a series by ID; fakeClassifier maps it to a
	// binding; fakePlanner returns 2 chapter plans. Assert PreviewOne wires
	// them into a matched PreviewEntry.
	srcDir := t.TempDir()
	st := &fakeSeriesStore{series: map[int64]model.Series{
		7: {ID: 7, Title: "Berserk", SourcePath: srcDir, Source: "suwayomi"},
	}}
	p := &Poller{
		Store:      st,
		Classifier: &fakeClassifier{decision: model.Decision{BindingID: 1, Via: "rule:5"}},
		Bindings:   &fakeBindingStore{bindings: []model.Binding{{ID: 1, Name: "Manga", LibraryRoot: "/lib/Manga"}}},
		Planner:    fakePlanner{plans: []filer.PlanEntry{{SrcPath: srcDir + "/c1.cbz", DstPath: "/lib/Manga/Berserk/c1.cbz", Mode: model.ModeHardlink, Action: filer.PlanFile}}},
	}
	entry, err := p.PreviewOne(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Status != "matched" || entry.BindingName != "Manga" || entry.DstRoot != "/lib/Manga" {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	if len(entry.ChapterPlans) != 1 || entry.ChapterPlans[0].DstPath != "/lib/Manga/Berserk/c1.cbz" {
		t.Fatalf("chapter plans not wired: %+v", entry.ChapterPlans)
	}
}

func TestPreviewOneUnknownSeries(t *testing.T) {
	st := &fakeSeriesStore{series: map[int64]model.Series{}}
	p := &Poller{Store: st}
	_, err := p.PreviewOne(context.Background(), 999)
	if err == nil {
		t.Fatal("want error for unknown series id")
	}
}
```
(Inspect `internal/poller/poller_test.go` for the real `fakeSeriesStore`, `fakeClassifier`, `fakeBindingStore`, `fakePlanner` shapes and adapt the literals to match. Reuse `Preview`'s per-series branch logic.)

- [ ] **Step 2: Run** `go test ./internal/poller/ -run TestPreviewOne -v` — FAIL (undefined).

- [ ] **Step 3: Implement** `PreviewOne` by extracting the per-series body of `Preview` into a shared helper `previewSeries(ctx, s model.Series, bindingByID map[int64]model.Binding) PreviewEntry`, then:
```go
func (p *Poller) PreviewOne(ctx context.Context, seriesID int64) (PreviewEntry, error) {
	if p.Store == nil {
		return PreviewEntry{}, fmt.Errorf("PreviewOne: no store wired")
	}
	s, err := p.Store.GetSeriesByID(seriesID)
	if err != nil {
		return PreviewEntry{}, fmt.Errorf("PreviewOne: series %d: %w", seriesID, err)
	}
	bindingByID, err := p.loadBindings()
	if err != nil {
		return PreviewEntry{}, fmt.Errorf("PreviewOne: load bindings: %w", err)
	}
	return p.previewSeries(ctx, s, bindingByID), nil
}
```
Refactor `Preview`'s loop to call the same `previewSeries` so there's one code path (DRY). Keep `Preview`'s behaviour identical — run its existing tests to confirm no regression.

- [ ] **Step 4: Run** the new tests + the existing `Preview` tests — all PASS.

- [ ] **Step 5: Commit** — `feat(poller): PreviewOne for single-series detail`.

---

## Task 2: poller `RefileOne`

**Files:**
- Modify: `internal/poller/poller.go`
- Test: `internal/poller/poller_test.go`

Files one persisted series via its current classification (the per-series body of `RunOnce`'s file step).

- [ ] **Step 1: Write the failing test** — `fakeFiler` records `File(series, dstRoot)` calls; assert `RefileOne` resolves the binding and calls File once with the right root. Add an unknown-series error case.

```go
func TestRefileOneFilesViaResolvedBinding(t *testing.T) {
	st := &fakeSeriesStore{series: map[int64]model.Series{7: {ID: 7, Title: "Berserk", SourcePath: "/dl/berserk"}}}
	fr := &recordingFiler{}
	p := &Poller{
		Store:      st,
		Classifier: &fakeClassifier{decision: model.Decision{BindingID: 1, Via: "rule:5"}},
		Bindings:   &fakeBindingStore{bindings: []model.Binding{{ID: 1, Name: "Manga", LibraryRoot: "/lib/Manga"}}},
		Filer:      fr,
		Activity:   &recorder{},
	}
	if err := p.RefileOne(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if len(fr.fileCalls) != 1 || fr.fileCalls[0].dstRoot != "/lib/Manga" {
		t.Fatalf("expected one File(_, /lib/Manga) call, got %+v", fr.fileCalls)
	}
}
```
(Use the poller test's existing filer fake; if it doesn't record calls, extend it minimally. Match the real `Filer` interface — `File(s model.Series, dstRoot string) error`.)

- [ ] **Step 2: Run** — FAIL.

- [ ] **Step 3: Implement**:
```go
func (p *Poller) RefileOne(ctx context.Context, seriesID int64) error {
	if p.Store == nil {
		return fmt.Errorf("RefileOne: no store wired")
	}
	s, err := p.Store.GetSeriesByID(seriesID)
	if err != nil {
		return fmt.Errorf("RefileOne: series %d: %w", seriesID, err)
	}
	bindingByID, err := p.loadBindings()
	if err != nil {
		return fmt.Errorf("RefileOne: load bindings: %w", err)
	}
	d, err := p.Classifier.Classify(ctx, classifier.ScanItem{
		Title: s.Title, ParentDir: s.SourcePath, ManualBindingID: p.resolveManualOverride(s),
	})
	if err != nil {
		return fmt.Errorf("RefileOne: classify: %w", err)
	}
	if d.BindingID == 0 {
		return fmt.Errorf("RefileOne: series %d is unmatched — nothing to file", seriesID)
	}
	binding, ok := bindingByID[d.BindingID]
	if !ok || binding.LibraryRoot == "" {
		return fmt.Errorf("RefileOne: binding %d missing or has no library_root", d.BindingID)
	}
	if err := p.Filer.File(s, binding.LibraryRoot); err != nil {
		return fmt.Errorf("RefileOne: file: %w", err)
	}
	if p.Store != nil {
		bid := binding.ID
		_ = p.Store.SetSeriesCurrentBinding(s.ID, &bid)
	}
	p.recordActivityVia(s.Title, model.ActionFiled, d.Via, fmt.Sprintf("refiled into %s", binding.LibraryRoot))
	return nil
}
```

- [ ] **Step 4: Run** the tests — PASS.

- [ ] **Step 5: Commit** — `feat(poller): RefileOne to file a single series on demand`.

---

## Task 3: web detail view-model + stat enrichment

**Files:**
- Create: `internal/web/series_detail.go`
- Test: `internal/web/series_detail_test.go`

- [ ] **Step 1: Write the failing test** for the stat-enrichment helper using real temp files:

```go
func TestChapterFilesEnrichesSizeAndStatus(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "Berserk - Ch.1.cbz")
	if err := os.WriteFile(dst, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	plans := []filer.PlanEntry{
		{SrcPath: "/dl/berserk/1.cbz", DstPath: dst, Mode: model.ModeHardlink, Action: filer.PlanSkip},     // exists on disk
		{SrcPath: "/dl/berserk/2.cbz", DstPath: filepath.Join(dir, "missing.cbz"), Mode: model.ModeHardlink, Action: filer.PlanFile}, // not yet filed
	}
	files := chapterFiles(plans)
	if len(files) != 2 {
		t.Fatalf("want 2 rows, got %d", len(files))
	}
	if files[0].SizeBytes != 5 || files[0].Status != "filed" {
		t.Fatalf("filed row wrong: %+v", files[0])
	}
	if files[1].Status != "missing" {
		t.Fatalf("missing row wrong: %+v", files[1])
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/web/ -run TestChapterFilesEnriches -v` — FAIL.

- [ ] **Step 3: Implement** in `internal/web/series_detail.go`:
```go
package web

import (
	"os"

	"github.com/gavinmcfall/mangarr/internal/filer"
	"github.com/gavinmcfall/mangarr/internal/model"
)

// chapterFileView is one row on the series detail page.
type chapterFileView struct {
	SrcPath   string
	DstPath   string
	Mode      model.FileMode
	Status    string // "filed" (dst exists), "missing" (planned, not on disk), "error"
	Reason    string // populated when the plan entry was a PlanError
	SizeBytes int64
	ModTime   string // formatted; "" when the file doesn't exist
	FileName  string // base name of DstPath — used as the remove-to-bin handle
}

// chapterFiles enriches filer plan entries with on-disk size/mtime + a status
// the detail page renders. A dst that exists = "filed"; a plannable dst that
// isn't on disk = "missing"; a PlanError = "error".
func chapterFiles(plans []filer.PlanEntry) []chapterFileView {
	out := make([]chapterFileView, 0, len(plans))
	for _, p := range plans {
		v := chapterFileView{SrcPath: p.SrcPath, DstPath: p.DstPath, Mode: p.Mode, FileName: baseName(p.DstPath)}
		switch p.Action {
		case filer.PlanError:
			v.Status = "error"
			v.Reason = p.Error
		default:
			if fi, err := os.Stat(p.DstPath); err == nil {
				v.Status = "filed"
				v.SizeBytes = fi.Size()
				v.ModTime = fi.ModTime().Format("2006-01-02 15:04")
			} else {
				v.Status = "missing"
			}
		}
		out = append(out, v)
	}
	return out
}

func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}
```

- [ ] **Step 4: Run** the test — PASS.

- [ ] **Step 5: Commit** — `feat(web): chapter-file view-model + stat enrichment`.

---

## Task 4: GET /series/{id} detail page handler + route

**Files:**
- Modify: `internal/web/series_detail.go` (add the handler)
- Modify: `internal/web/web.go` (route + Previewer interface widen)
- Test: `internal/web/series_detail_test.go`

- [ ] **Step 1: Write the failing test** — a handler test using `newTestHandler` with a fake previewer returning a known PreviewEntry; assert the page renders the title, binding, and a chapter row.

(Inspect how `newTestHandler` wires the `Previewer`; extend the fake previewer with `PreviewOne`. The web `Previewer` interface must gain `PreviewOne(ctx, seriesID int64) (poller.PreviewEntry, error)`.)

- [ ] **Step 2: Run** — FAIL.

- [ ] **Step 3: Implement** the handler `pageSeriesDetail`:
  - Parse `{id}` → int64 (400 on bad).
  - `entry, err := h.previewer.PreviewOne(r.Context(), id)` (503 if previewer nil; 500 on err).
  - `files := chapterFiles(entry.ChapterPlans)`.
  - Render `series-detail.html` with a `seriesDetailData{Page, SeriesID, Title, BindingName, DstRoot, Status, Reason, Note, Files, Bindings}` (Bindings for the embedded reclassify control).
  - Register `h.mux.HandleFunc("GET /series/{id}", h.pageSeriesDetail)` in web.go. Widen the `Previewer` interface with `PreviewOne`.

- [ ] **Step 4: Run** — PASS (after Task 5 template; same red→template→green boundary as prior plans).

- [ ] **Step 5: Commit** — `feat(web): GET /series/{id} detail handler + route`.

---

## Task 5: series-detail.html template + row link

**Files:**
- Create: `internal/web/templates/series-detail.html`
- Modify: `internal/web/templates/series.html` (link the title)
- Modify: `internal/web/static/mangarr.css`
- Test: the Task 4 handler test goes green.

- [ ] **Step 1: Confirm** the Task 4 test still fails (template missing).

- [ ] **Step 2: Create** `series-detail.html` with `{{template "base" .}}` + `{{define "content"}}`:
  - Header: title, binding pill, dst root, status.
  - A per-series action row: a re-classify `<select>` posting to the existing `/api/series/{id}/reclassify`, and a "Re-run filer" button posting to `/api/series/{id}/refile`.
  - A table of `.Files`: FileName, Status (pill), Size (human-readable), ModTime, Mode, and a per-row "Remove" button posting `name=<FileName>` to `/api/series/{id}/chapter/remove`.
  - Empty-state when `.Files` is empty.

- [ ] **Step 3: Link** the series-page title: in `series.html`, wrap the title cell `<td><a href="/series/{{.ID}}">{{.Title}}</a></td>`.

- [ ] **Step 4: CSS** for the detail table + status pills (reuse existing `.pill-*`; add `.pill-filed`/`.pill-missing` if absent — green/grey).

- [ ] **Step 5: Run** the Task 4 handler test — PASS.

- [ ] **Step 6: Commit** — `feat(series): detail page template + row link`.

---

## Task 6: POST /api/series/{id}/refile

**Files:**
- Modify: `internal/web/series_detail.go` (handler)
- Modify: `internal/web/web.go` (route + SeriesFiler-style interface widen with RefileOne)
- Test: `internal/web/series_detail_test.go`

- [ ] **Step 1: Write the failing test** — POST to the route; assert the fake's RefileOne was called with the right id and the response is a redirect back to `/series/{id}` (or 200).

- [ ] **Step 2: Run** — FAIL.

- [ ] **Step 3: Implement** `apiSeriesRefile`: parse id, call `h.seriesFiler.RefileOne(ctx, id)` (the interface gains RefileOne; wire the poller in main.go — it already satisfies it), 303 back to `/series/{id}` on success, 500 on error, 503 if unwired. Register the route.

- [ ] **Step 4: Run** — PASS.

- [ ] **Step 5: Commit** — `feat(web): re-run filer action on series detail`.

---

## Task 7: POST /api/series/{id}/chapter/remove (remove-to-bin)

**Files:**
- Modify: `internal/web/series_detail.go`
- Modify: `internal/web/web.go` (route; the handler needs the recycle bin + previewer to resolve the dst root)
- Test: `internal/web/series_detail_test.go`

- [ ] **Step 1: Write the failing test** — real temp dst root with a file; POST `name=<file>`; assert the file is gone from the root and landed in the bin. Plus a traversal-guard test: `name=../../etc/passwd` is rejected (400) and nothing is removed.

- [ ] **Step 2: Run** — FAIL.

- [ ] **Step 3: Implement** `apiSeriesChapterRemove`:
  - Parse `{id}`; `r.FormValue("name")` is the chapter filename.
  - `entry, err := h.previewer.PreviewOne(ctx, id)` → `entry.DstRoot` is the series' library root.
  - Resolve `target := filepath.Join(entry.DstRoot, <series-subdir-if-any from the matching plan's DstPath>)`. SIMPLER + SAFE: find the chapter in `entry.ChapterPlans` whose `baseName(DstPath) == name`; use that plan's exact `DstPath` (already computed + traversal-guarded by the filer). If no plan matches `name`, 400. This avoids re-deriving paths from user input.
  - Guard: confirm the resolved `DstPath` is under `entry.DstRoot` (defence in depth) before acting.
  - `h.recycleBin.Send(dstPath, time.Now())`; 303 back to `/series/{id}` on success.
  - Register the route. The Handler needs a `*recyclebin.Bin` reference — add it to `HandlerOpts` + the Handler struct and wire it in main.go (the bin already exists there as `bin`).

- [ ] **Step 4: Run** — PASS (happy path + traversal-rejection).

- [ ] **Step 5: Commit** — `feat(web): per-chapter remove-to-bin on series detail`.

---

## Task 8: main.go wiring + full sweep

**Files:**
- Modify: `main.go`
- Test only otherwise.

- [ ] **Step 1:** Wire the new HandlerOpts fields in main.go: the poller already satisfies `PreviewOne`/`RefileOne` (added in T1/T2) — pass it as the previewer/seriesFiler if not already; pass `bin` as the recycle bin.
- [ ] **Step 2:** `go build ./... && go test ./... -race` — all green (local TZ; the GC fix is in).
- [ ] **Step 3:** Manual self-review vs issue #8 requirements.
- [ ] **Step 4:** Commit any fixups.

---

## Self-Review

**Spec coverage (issue #8):**
- "lists every filed chapter with source path, target path, link mode used, size, mtime" → Task 3 view-model + Task 5 table. ✓
- "actions (re-classify, remove-to-bin, re-run filer)" → re-classify (existing endpoint, embedded T5), remove-to-bin (T7), re-run filer (T2 poller + T6 endpoint). ✓
- "Clicking a series row opens a page" → Task 5 row link. ✓
- "without shelling into the pod" → the page shows on-disk size/mtime/status. ✓
- Dependency #3 (recycle bin) → used by T7. ✓

**Placeholder scan:** Concrete Go/SQL/HTML throughout. The "inspect the real fake shapes" notes in T1/T2/T4 are deliberate (the implementer reads the actual poller/web test fakes first); full target code is shown.

**Type consistency:** `PreviewOne(ctx, int64) (PreviewEntry, error)`, `RefileOne(ctx, int64) error`, `chapterFileView{SrcPath,DstPath,Mode,Status,Reason,SizeBytes,ModTime,FileName}`, `chapterFiles([]filer.PlanEntry) []chapterFileView`, routes `/series/{id}`, `/api/series/{id}/refile`, `/api/series/{id}/chapter/remove` — consistent across tasks.

**Commit hygiene:** `git -c commit.gpgsign=false`, explicit staging, no "claude"/"anthropic", verify-gate stamp before each commit. Run the suite in local TZ (GC fix landed).
