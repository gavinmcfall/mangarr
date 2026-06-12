# Honest Counts & Dud-Chapter UX — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the Library page honest — break "N missing" into dud-at-source vs undownloaded, add a downloaded-but-not-filed line, and replace the stale bulk-job "Status" with a completeness-derived status — all computed during Sync and stored.

**Architecture:** A durable `series.manga_id` link (populated by the poller from the existing `SuwayomiCache`) joins each Suwayomi manga to its disk series. Sync computes `dud_count` (from a new `pageCount` field on `ListChapters`) and `filed_count` (a disk `.cbz` count in the series' Kavita dir) and stores them on `library_cache`. The Library renderer derives `undownloaded`/`filing_gap`/honest-status from the stored counts.

**Tech Stack:** Go 1.25, `modernc.org/sqlite`, `database/sql`, `html/template`, HTMX. Tests are `go test` with table-driven cases + in-memory fakes.

**Spec:** `docs/specs/2026-06-13-honest-counts-design.md`

---

## File Structure

| File | Responsibility | Change |
|------|----------------|--------|
| `internal/store/migrations.go` | schema migrations | add v10 (3 columns) |
| `internal/model/bulk.go` | `LibraryCacheEntry` | + `DudCount`, `FiledCount` |
| `internal/store/bulk.go` | library_cache persistence | save/read the 2 counts |
| `internal/store/store.go` | series persistence | `SetSeriesMangaID`, `GetSeriesByMangaID` |
| `internal/suwayomi/suwayomi.go` | `Chapter` + `ListChapters` | + `PageCount` |
| `internal/poller/poller.go` | `RunOnce` | persist `manga_id` from `SuwayomiCache` |
| `internal/web/bulk.go` | `apiLibrarySync` | compute `dud_count` + `filed_count` |
| `internal/web/web.go` | `pageLibrary`, `libraryRow` | derive counts + honest status |
| `internal/web/templates/library.html` | Library rows | breakdown sub-lines + status |

---

## Task 1: Migration v10 — three columns

**Files:**
- Modify: `internal/store/migrations.go`
- Test: `internal/store/migrations_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/store/migrations_test.go`:

```go
func TestMigration10AddsHonestCountsColumns(t *testing.T) {
	s := newTestStore(t)
	db := s.DB()
	cases := []struct{ table, col string }{
		{"library_cache", "dud_count"},
		{"library_cache", "filed_count"},
		{"series", "manga_id"},
	}
	for _, c := range cases {
		var name string
		err := db.QueryRow(
			`SELECT name FROM pragma_table_info(?) WHERE name=?`, c.table, c.col,
		).Scan(&name)
		if err != nil {
			t.Errorf("%s.%s missing after migrations: %v", c.table, c.col, err)
		}
	}
}
```

(Confirm the fresh-`*Store` helper name is `newTestStore` and the raw-handle accessor is `s.DB()` by grepping `internal/store/*_test.go` — they are used by the v9 test.)

- [ ] **Step 2: Run test to verify it fails**

Run: `cd internal/store && go test -run TestMigration10 -v`
Expected: FAIL — columns absent.

- [ ] **Step 3: Add the migration**

In `internal/store/migrations.go`, append to the `migrations` slice:

```go
	{10, "honest-counts-columns", migrateHonestCountsColumns},
```

Add the function (mirror the v9 `migrateSeriesMissingSince` probe-before-ALTER pattern, applied to three columns across two tables; each ALTER is independently guarded so a partial replay is safe):

```go
// migrateHonestCountsColumns adds the columns the Library "honest counts"
// feature needs: library_cache.dud_count and .filed_count (computed at Sync),
// and series.manga_id (the durable Suwayomi-manga join key, populated by the
// poller). Idempotent under the schema_versions gate; tolerant of a missing
// table (migration-only test fixtures) and a duplicate column (manual replay).
func migrateHonestCountsColumns(tx *sql.Tx) error {
	add := func(table, col, decl string) error {
		var t string
		err := tx.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&t)
		if err == sql.ErrNoRows {
			return nil // table absent in this fixture
		}
		if err != nil {
			return fmt.Errorf("probe %s: %w", table, err)
		}
		var c string
		err = tx.QueryRow(`SELECT name FROM pragma_table_info(`+"`"+table+"`"+`) WHERE name=?`, col).Scan(&c)
		if err == nil {
			return nil // already present
		}
		if err != sql.ErrNoRows {
			return fmt.Errorf("probe %s.%s: %w", table, col, err)
		}
		if _, err := tx.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + col + ` ` + decl); err != nil {
			return fmt.Errorf("add %s.%s: %w", table, col, err)
		}
		return nil
	}
	if err := add("library_cache", "dud_count", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := add("library_cache", "filed_count", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	return add("series", "manga_id", "INTEGER")
}
```

Note: `pragma_table_info` does not accept a bound parameter for the table name in all SQLite builds, so the table name is concatenated (it is a hard-coded literal here, never user input — no injection risk).

- [ ] **Step 4: Run test to verify it passes**

Run: `cd internal/store && go test -run TestMigration10 -v` → PASS. Then `cd internal/store && go test ./...` → all green.

- [ ] **Step 5: Commit**

```bash
git add internal/store/migrations.go internal/store/migrations_test.go
git commit -m "feat(store): migration v10 — honest-counts columns + series.manga_id"
```

---

## Task 2: Model + store — counts on the cache, manga_id on series

**Files:**
- Modify: `internal/model/bulk.go` (`LibraryCacheEntry`)
- Modify: `internal/store/bulk.go` (save + read library_cache)
- Modify: `internal/store/store.go` (`SetSeriesMangaID`, `GetSeriesByMangaID`)
- Test: `internal/store/bulk_test.go`, `internal/store/store_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/store/bulk_test.go`:

```go
func TestLibraryCacheEntryCarriesDudAndFiled(t *testing.T) {
	s := newTestStore(t)
	in := model.LibraryCacheEntry{
		MangaID: 5, Title: "X", SourceID: "1", SourceName: "src",
		TotalChapters: 100, Downloaded: 90, DudCount: 2, FiledCount: 88,
	}
	if err := s.SaveLibraryCacheEntry(in); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetLibraryCacheEntry(5)
	if err != nil {
		t.Fatal(err)
	}
	if got.DudCount != 2 || got.FiledCount != 88 {
		t.Fatalf("dud=%d filed=%d, want 2/88", got.DudCount, got.FiledCount)
	}
	list, _ := s.ListLibraryCacheEntries()
	var found bool
	for _, e := range list {
		if e.MangaID == 5 {
			found = true
			if e.DudCount != 2 || e.FiledCount != 88 {
				t.Errorf("list dud=%d filed=%d, want 2/88", e.DudCount, e.FiledCount)
			}
		}
	}
	if !found {
		t.Fatal("entry not in ListLibraryCacheEntries")
	}
}
```

Add to `internal/store/store_test.go`:

```go
func TestSetAndGetSeriesByMangaID(t *testing.T) {
	s := newTestStore(t)
	id, err := s.UpsertSeries(model.Series{
		Title: "Z", SourcePath: "/d/Z", Source: "suwayomi",
		Type: model.TypeUnknown, Status: model.StatusPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSeriesMangaID(id, 4242); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSeriesByMangaID(4242)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != id || got.Title != "Z" {
		t.Fatalf("got id=%d title=%q, want id=%d Z", got.ID, got.Title, id)
	}
	// Unknown manga id → ErrNoRows.
	if _, err := s.GetSeriesByMangaID(999999); err == nil {
		t.Error("want error for unknown manga id, got nil")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd internal/store && go test -run 'TestLibraryCacheEntryCarriesDudAndFiled|TestSetAndGetSeriesByMangaID' -v`
Expected: FAIL — fields/methods undefined.

- [ ] **Step 3: Implement**

In `internal/model/bulk.go`, add to `LibraryCacheEntry` after `Downloaded`:

```go
	// DudCount is the number of not-downloaded chapters Suwayomi reports with
	// pageCount==0 (permanent source duds). FiledCount is the number of .cbz
	// files present in the series' Kavita library dir. Both written at Sync.
	DudCount   int
	FiledCount int
```

In `internal/store/bulk.go`, update `SaveLibraryCacheEntry` to write the two columns:

```go
func (s *Store) SaveLibraryCacheEntry(in model.LibraryCacheEntry) error {
	_, err := s.db.Exec(`
INSERT INTO library_cache (manga_id, title, source_id, source_name, total_chapters, downloaded, dud_count, filed_count)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(manga_id) DO UPDATE SET
	title          = excluded.title,
	source_id      = excluded.source_id,
	source_name    = excluded.source_name,
	total_chapters = excluded.total_chapters,
	downloaded     = excluded.downloaded,
	dud_count      = excluded.dud_count,
	filed_count    = excluded.filed_count,
	refreshed_at   = strftime('%s','now')`,
		in.MangaID, in.Title, in.SourceID, in.SourceName, in.TotalChapters, in.Downloaded, in.DudCount, in.FiledCount,
	)
	if err != nil {
		return fmt.Errorf("SaveLibraryCacheEntry: %w", err)
	}
	return nil
}
```

Update the SELECT + Scan in BOTH `ListLibraryCacheEntries` and `GetLibraryCacheEntry` to include `dud_count, filed_count`. For `ListLibraryCacheEntries`:

```go
	rows, err := s.db.Query(`SELECT manga_id, title, source_id, source_name, total_chapters, downloaded, dud_count, filed_count, refreshed_at
		FROM library_cache ORDER BY title`)
	// ... in the row loop:
	//   var refreshedAt int64
	//   rows.Scan(&e.MangaID, &e.Title, &e.SourceID, &e.SourceName, &e.TotalChapters, &e.Downloaded, &e.DudCount, &e.FiledCount, &refreshedAt)
```

Apply the same `dud_count, filed_count` additions to `GetLibraryCacheEntry`'s query + `Scan` (read it first; mirror the column order exactly).

In `internal/store/store.go`, add:

```go
// SetSeriesMangaID records the Suwayomi manga id this disk series corresponds
// to (the durable join key for the Library page). 0 is a valid "unknown".
func (s *Store) SetSeriesMangaID(id, mangaID int64) error {
	_, err := s.db.Exec(`UPDATE series SET manga_id=? WHERE id=?`, mangaID, id)
	return err
}

// GetSeriesByMangaID returns the series linked to a Suwayomi manga id. Returns
// (wrapped) sql.ErrNoRows when no series carries that manga_id.
func (s *Store) GetSeriesByMangaID(mangaID int64) (model.Series, error) {
	var m model.Series
	var typ, status string
	var manual, current sql.NullInt64
	err := s.db.QueryRow(`SELECT id,title,source_path,source,type,status,chapter_count,manual_binding_id,current_binding_id FROM series WHERE manga_id=?`, mangaID).
		Scan(&m.ID, &m.Title, &m.SourcePath, &m.Source, &typ, &status, &m.ChapterCount, &manual, &current)
	if err != nil {
		return m, err
	}
	m.Type, m.Status = model.ContentType(typ), model.Status(status)
	if manual.Valid {
		v := manual.Int64
		m.ManualBindingID = &v
	}
	if current.Valid {
		v := current.Int64
		m.CurrentBindingID = &v
	}
	return m, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd internal/store && go test -run 'TestLibraryCacheEntryCarriesDudAndFiled|TestSetAndGetSeriesByMangaID' -v` → PASS. Then `cd internal/store && go test ./...`.

- [ ] **Step 5: Commit**

```bash
git add internal/model/bulk.go internal/store/bulk.go internal/store/store.go internal/store/bulk_test.go internal/store/store_test.go
git commit -m "feat(store): library_cache dud/filed counts + series manga_id link"
```

---

## Task 3: Suwayomi `ListChapters` returns `pageCount`

**Files:**
- Modify: `internal/suwayomi/suwayomi.go`
- Test: `internal/suwayomi/suwayomi_test.go`

- [ ] **Step 1: Write the failing test**

Grep `internal/suwayomi/suwayomi_test.go` for the existing `ListChapters` stub-server test (it builds an `httptest.Server` returning a chapters JSON payload). Add a test that the stub's `pageCount` flows into `Chapter.PageCount`. Pattern:

```go
func TestListChaptersParsesPageCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":{"chapters":{"nodes":[
			{"id":1,"name":"Ch 1","chapterNumber":1,"isDownloaded":true,"sourceOrder":0,"pageCount":18},
			{"id":2,"name":"Ch 2","chapterNumber":2,"isDownloaded":false,"sourceOrder":1,"pageCount":0}
		]}}}`)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "") // match the real NewClient signature in this file
	chs, err := c.ListChapters(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(chs) != 2 || chs[0].PageCount != 18 || chs[1].PageCount != 0 {
		t.Fatalf("page counts = %v, want [18 0]", []int{chs[0].PageCount, chs[1].PageCount})
	}
}
```

(Match `NewClient`'s real signature/auth arg from a neighbouring test in the same file.)

- [ ] **Step 2: Run test to verify it fails**

Run: `cd internal/suwayomi && go test -run TestListChaptersParsesPageCount -v`
Expected: FAIL — `Chapter` has no `PageCount`.

- [ ] **Step 3: Implement**

In `internal/suwayomi/suwayomi.go`:

1. Add `PageCount` to the `Chapter` struct (after `IsDownloaded`):
```go
	PageCount     int
```
2. Add `pageCount` to the `ListChapters` GraphQL query's `nodes { ... }` selection (after `isDownloaded`):
```go
				pageCount
```
3. Add `PageCount` to the anonymous decode node struct (after `IsDownloaded bool ...`):
```go
					PageCount     int     `json:"pageCount"`
```
4. Map it in the node→Chapter loop (in the `Chapter{...}` literal):
```go
			PageCount:     n.PageCount,
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd internal/suwayomi && go test -run TestListChaptersParsesPageCount -v` → PASS. Then `cd internal/suwayomi && go test ./...` (existing ListChapters tests must still pass — the new field is additive).

- [ ] **Step 5: Commit**

```bash
git add internal/suwayomi/suwayomi.go internal/suwayomi/suwayomi_test.go
git commit -m "feat(suwayomi): ListChapters returns pageCount (dud detection)"
```

---

## Task 4: Poller persists `series.manga_id`

**Files:**
- Modify: `internal/poller/poller.go` (`SeriesStore` interface + `RunOnce` loop)
- Test: `internal/poller/poller_test.go`

The `SuwayomiCache` (`*suwayomi.PathCache`) already maps a series' `SourcePath` →
`CacheEntry{MangaID}` and is refreshed at the top of `RunOnce`. We persist that id.

- [ ] **Step 1: Write the failing test**

Grep `internal/poller/poller_test.go` for the existing `RunOnce` test harness (the `recorder`/`fakeSeriesStore`, a `Scanner` stub, and how `SuwayomiCache` is populated). Add a test that after `RunOnce`, a scanned series whose `SourcePath` resolves in the `SuwayomiCache` has `SetSeriesMangaID` called with the cache's `MangaID`. Concretely:

```go
func TestRunOncePersistsMangaID(t *testing.T) {
	// Build a PathCache that maps "/dl/Src/Title" -> MangaID 77.
	// (Use the same PathCache construction the existing Library-Map poller
	//  tests use; grep for SuwayomiCache / PathCache in this test file.)
	// Scanner returns one series with SourcePath "/dl/Src/Title".
	// fakeSeriesStore records SetSeriesMangaID(id, mangaID) calls.
	// After p.RunOnce(ctx): assert the recorded mangaID for that series == 77.
}
```

Implement the test against the real harness names you find (the body above is a
spec, not a placeholder — wire it to the existing `recorder`/cache stubs and the
real `SetSeriesMangaID` capture; assert the captured mangaID is 77).

- [ ] **Step 2: Run test to verify it fails**

Run: `cd internal/poller && go test -run TestRunOncePersistsMangaID -v`
Expected: FAIL — `SetSeriesMangaID` not on the interface / not called.

- [ ] **Step 3: Implement**

In `internal/poller/poller.go`, add to the `SeriesStore` interface:

```go
	SetSeriesMangaID(id, mangaID int64) error
```

In `RunOnce`, inside the per-series loop AFTER the upsert backfills `s.ID` (so
`sid` is known) and the `SuwayomiCache` has been refreshed, add:

```go
		// Persist the Suwayomi manga id for this series (durable join key for
		// the Library page). Best-effort: a cache miss leaves manga_id NULL and
		// self-heals on a later tick once the path cache covers it.
		if p.Store != nil && p.SuwayomiCache != nil {
			if ce, ok := p.SuwayomiCache.Lookup(s.SourcePath); ok && ce.MangaID != 0 {
				_ = p.Store.SetSeriesMangaID(sid, ce.MangaID)
			}
		}
```

(Place it next to the existing `p.Store`-guarded writes in the loop. If a test
fake implements `SeriesStore`, add a `SetSeriesMangaID` stub so the package
compiles — grep `internal/poller/*_test.go` for the fake.)

- [ ] **Step 4: Run test + build**

Run: `cd internal/poller && go test -run TestRunOncePersistsMangaID -v` → PASS, then `cd internal/poller && go test ./... && cd ../.. && go build ./...`.

- [ ] **Step 5: Commit**

```bash
git add internal/poller/poller.go internal/poller/poller_test.go
git commit -m "feat(poller): persist series.manga_id from the Suwayomi path cache"
```

---

## Task 5: Sync computes `dud_count` + `filed_count`

**Files:**
- Modify: `internal/web/bulk.go` (`apiLibrarySync`)
- Modify: `internal/web/web.go` (widen the web `Store` interface with `GetSeriesByMangaID`; confirm `Previewer` has `ResolveLibraryDir`)
- Test: `internal/web/bulk_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/web/bulk_test.go` (reuse `newTestHandlerFull` + `fakeSuwayomi`/`fakeStore`; seed chapters with mixed pageCount/isDownloaded):

```go
func TestLibrarySyncComputesDudCount(t *testing.T) {
	h, st, sw, _ := newTestHandlerFull()
	sw.libraryEntries = []suwayomi.Manga{{ID: 5, Title: "X", Source: "src", SourceID: "1"}}
	sw.chaptersForManga = map[int64][]int64{5: {10, 11, 12}}
	sw.chaptersDownloaded = map[int64]bool{10: true} // 10 downloaded; 11,12 not
	sw.chapterPages = map[int64]int{11: 0, 12: 5}     // 11 is a dud (0 pages), 12 undownloaded
	req := httptest.NewRequest(http.MethodPost, "/api/library/sync", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync = %d, want 200", rec.Code)
	}
	got, ok := st.savedLibraryCache[5]
	if !ok {
		t.Fatal("manga 5 not saved to cache")
	}
	if got.DudCount != 1 {
		t.Errorf("dud_count = %d, want 1 (chapter 11)", got.DudCount)
	}
}
```

This requires `fakeSuwayomi` to return per-chapter `pageCount` and `fakeStore` to
record saved cache entries. In `internal/web/web_test.go`:
- Add a `chapterPages map[int64]int` field to `fakeSuwayomi`, and in its
  `ListChapters` set `PageCount: f.chapterPages[id]` on each returned `Chapter`.
- Add a `savedLibraryCache map[int64]model.LibraryCacheEntry` field to `fakeStore`
  and have its `SaveLibraryCacheEntry` record into it (keyed by `MangaID`).

- [ ] **Step 2: Run test to verify it fails**

Run: `cd internal/web && go test -run TestLibrarySyncComputesDudCount -v`
Expected: FAIL — `dud_count` is 0 (not yet computed) / fake fields missing.

- [ ] **Step 3: Implement**

In `internal/web/bulk.go` `apiLibrarySync`, extend the per-worker `countResult`
and loop to also count duds, and compute `filed_count` after via the manga→series
link. Replace the worker body's count loop and the `SaveLibraryCacheEntry` call:

```go
	type countResult struct {
		total      int
		downloaded int
		dud        int
		filed      int
	}
	counts := make([]countResult, len(entries))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, e := range entries {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, mangaID int64) {
			defer wg.Done()
			defer func() { <-sem }()
			chs, err := h.suwayomi.ListChapters(r.Context(), mangaID)
			if err != nil {
				return
			}
			cr := countResult{total: len(chs)}
			for _, c := range chs {
				if c.IsDownloaded {
					cr.downloaded++
				} else if c.PageCount == 0 {
					cr.dud++ // not downloaded AND zero pages = permanent source dud
				}
			}
			// filed_count: count .cbz in this manga's Kavita dir, resolved via
			// its linked series. Miss → leave filed at downloaded (no false gap).
			cr.filed = cr.downloaded
			if series, err := h.store.GetSeriesByMangaID(mangaID); err == nil {
				if dir := h.resolveSeriesDestDir(r.Context(), series.ID); dir != "" {
					if n, ok := countCBZ(dir); ok {
						cr.filed = n
					}
				}
			}
			counts[i] = cr
		}(i, e.ID)
	}
	wg.Wait()
```

And the save loop carries the new counts:

```go
		if err := h.store.SaveLibraryCacheEntry(model.LibraryCacheEntry{
			MangaID:       e.ID,
			Title:         e.Title,
			SourceID:      e.SourceID,
			SourceName:    e.Source,
			TotalChapters: counts[i].total,
			Downloaded:    counts[i].downloaded,
			DudCount:      counts[i].dud,
			FiledCount:    counts[i].filed,
		}); err != nil {
```

Add a small helper `countCBZ` (new, near the handler) — returns the count and
`false` when the dir can't be read so the caller keeps the safe fallback:

```go
// countCBZ returns the number of .cbz files directly under dir. The bool is
// false when dir cannot be read (caller keeps its fallback rather than
// recording a spurious 0).
func countCBZ(dir string) (int, bool) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, false
	}
	n := 0
	for _, e := range ents {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".cbz") {
			n++
		}
	}
	return n, true
}
```

(`os`, `strings`, `path/filepath` imports — add any missing to `bulk.go`.)

In `internal/web/web.go`, add `GetSeriesByMangaID(mangaID int64) (model.Series, error)`
to the web `Store` interface (so `h.store` exposes it). `resolveSeriesDestDir`
and `h.previewer.ResolveLibraryDir` already exist (Theme A). Update any web test
fake (`fakeStore`) to implement `GetSeriesByMangaID` (return a seeded series or
`sql.ErrNoRows`).

- [ ] **Step 4: Run test + suite**

Run: `cd internal/web && go test -run TestLibrarySyncComputesDudCount -v` → PASS, then `cd ../.. && go build ./... && go test ./internal/web/...`.

- [ ] **Step 5: Commit**

```bash
git add internal/web/bulk.go internal/web/web.go internal/web/web_test.go internal/web/bulk_test.go
git commit -m "feat(web): Sync computes dud_count + filed_count per series"
```

---

## Task 6: Library renders honest counts + honest status

**Files:**
- Modify: `internal/web/web.go` (`libraryRow`, `pageLibrary`, status helper)
- Modify: `internal/web/templates/library.html`
- Test: `internal/web/web_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/web/web_test.go`:

```go
func TestLibraryStatusIsHonest(t *testing.T) {
	cases := []struct {
		name                            string
		total, downloaded, dud, filed   int
		runningJob                      bool
		wantStatus                      string
	}{
		{"complete", 100, 100, 0, 100, false, "Complete"},
		{"all-dud", 181, 180, 1, 180, false, "Complete · 1 unavailable"},
		{"undownloaded", 1184, 1, 0, 1, false, "Incomplete · 1183 to download"},
		{"running", 100, 40, 0, 40, true, "Downloading"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := honestLibraryStatus(c.total, c.downloaded, c.dud, c.runningJob, 40, 100)
			if !strings.HasPrefix(got, strings.SplitN(c.wantStatus, " ", 2)[0]) {
				t.Errorf("status = %q, want prefix of %q", got, c.wantStatus)
			}
			if c.name != "running" && got != c.wantStatus {
				t.Errorf("status = %q, want %q", got, c.wantStatus)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd internal/web && go test -run TestLibraryStatusIsHonest -v`
Expected: FAIL — `honestLibraryStatus` undefined.

- [ ] **Step 3: Implement the status helper + row derivation**

In `internal/web/web.go`, add the pure helper:

```go
// honestLibraryStatus renders the Library "Status" cell from real completeness,
// not a stale bulk-job state. A running job wins (with progress); otherwise the
// status is derived: undownloaded chapters → Incomplete; only-dud gap → Complete
// with an "unavailable" note; nothing missing → Complete.
func honestLibraryStatus(total, downloaded, dud int, jobRunning bool, jobDone, jobTotal int) string {
	if jobRunning {
		if jobTotal > 0 {
			return fmt.Sprintf("Downloading · %d/%d", jobDone, jobTotal)
		}
		return "Downloading"
	}
	missing := total - downloaded
	if missing < 0 {
		missing = 0
	}
	undownloaded := missing - dud
	if undownloaded < 0 {
		undownloaded = 0
	}
	switch {
	case undownloaded > 0:
		return fmt.Sprintf("Incomplete · %d to download", undownloaded)
	case missing > 0: // all of it is dud
		return fmt.Sprintf("Complete · %d unavailable", missing)
	default:
		return "Complete"
	}
}
```

Extend `libraryRow` (in `web.go`) with the fields the template needs:

```go
	DudCount     int
	Undownloaded int
	FilingGap    int
	Status       string // honest, replaces the raw JobStatus string
	SeriesID     int64  // for the Re-run filer action; 0 when unlinked
```

Add a helper that returns the most-recent bulk job (not just its status string),
so `pageLibrary` can read `Status`/progress — replace the use of
`mostRecentBulkJobStatus` in `pageLibrary` with:

```go
func (h *Handler) mostRecentBulkJob(mangaID int64) *model.BulkJob {
	jobs, err := h.store.ListBulkJobs("")
	if err != nil {
		return nil
	}
	var newest *model.BulkJob
	for i := range jobs {
		if jobs[i].MangaID != mangaID {
			continue
		}
		if newest == nil || jobs[i].CreatedAt.After(newest.CreatedAt) {
			newest = &jobs[i]
		}
	}
	return newest
}
```

In `pageLibrary`, where each `libraryRow` is built, compute the derived fields:

```go
		missing := e.TotalChapters - e.Downloaded
		if missing < 0 {
			missing = 0
		}
		undownloaded := missing - e.DudCount
		if undownloaded < 0 {
			undownloaded = 0
		}
		filingGap := e.Downloaded - e.FiledCount
		if filingGap < 0 {
			filingGap = 0
		}
		job := h.mostRecentBulkJob(e.MangaID)
		running := job != nil && job.Status == model.BulkJobRunning
		done, jt := 0, 0
		if job != nil {
			done, jt = job.CompletedChapters, job.TotalChapters
		}
		var seriesID int64
		if s, err := h.store.GetSeriesByMangaID(e.MangaID); err == nil {
			seriesID = s.ID
		}
		rows = append(rows, libraryRow{
			MangaID:       e.MangaID,
			Title:         e.Title,
			SourceID:      e.SourceID,
			SourceName:    e.SourceName,
			TotalChapters: e.TotalChapters,
			Downloaded:    e.Downloaded,
			Missing:       missing,
			DudCount:      e.DudCount,
			Undownloaded:  undownloaded,
			FilingGap:     filingGap,
			Cached:        e.TotalChapters > 0,
			Status:        honestLibraryStatus(e.TotalChapters, e.Downloaded, e.DudCount, running, done, jt),
			SeriesID:      seriesID,
		})
```

Remove the now-unused `JobStatus` field and `mostRecentBulkJobStatus` if nothing
else references them (grep first; if other call sites exist, leave them and just
stop using `JobStatus` in `libraryRow`).

- [ ] **Step 4: Update the template**

In `internal/web/templates/library.html`, change the status cell to `{{.Status}}`
(it was `{{.JobStatus}}`) and update the row's `data-status` to `{{.Status}}`.
Under the missing count cell, add the breakdown sub-lines (only when non-zero):

```html
{{if gt .DudCount 0}}<div class="lib-sub lib-sub-muted">{{.DudCount}} dud at source (permanent)</div>{{end}}
{{if gt .Undownloaded 0}}<div class="lib-sub">{{.Undownloaded}} not downloaded</div>{{end}}
{{if gt .FilingGap 0}}
<div class="lib-sub lib-sub-warn">
  ⚠ {{.FilingGap}} downloaded, not filed to Kavita
  {{if gt .SeriesID 0}}
  <button type="button" class="btn btn-sm btn-secondary"
          hx-post="/api/series/{{.SeriesID}}/refile" hx-swap="none"
          hx-on::after-request="if(event.detail.successful){window.location.reload();}">Re-run filer</button>
  {{end}}
</div>
{{end}}
```

Add minimal CSS to `internal/web/static/mangarr.css`:

```css
.lib-sub { font-size: 0.8rem; margin-top: 2px; }
.lib-sub-muted { color: #8a8d93; }
.lib-sub-warn { color: #d8a657; }
```

- [ ] **Step 5: Run test + suite + build**

Run: `cd internal/web && go test -run TestLibraryStatusIsHonest -v` → PASS, then `cd ../.. && go build ./... && go test -race ./...` → all green.

- [ ] **Step 6: Commit**

```bash
git add internal/web/web.go internal/web/templates/library.html internal/web/static/mangarr.css internal/web/web_test.go
git commit -m "feat(web): honest Library status + missing breakdown + filing-gap line"
```

---

## Task 7: Full suite + manual smoke

- [ ] **Step 1:** `go test -race ./...` → all green; `go vet ./...` clean.
- [ ] **Step 2:** `go build ./...` → clean.
- [ ] **Step 3: Manual smoke (record in PR):**
  1. Click **Sync** → rows show the breakdown; a series with a real dud (e.g. Disaster-Class Ch41) shows "1 dud at source (permanent)" and status **"Complete · 1 unavailable"**, NOT "completed".
  2. A partially-downloaded series shows "N not downloaded" + **"Incomplete · N to download"**.
  3. A filing-gap series (downloaded in Suwayomi, fewer in Kavita) shows the ⚠ line + **Re-run filer**; clicking it re-files and the gap clears after the next Sync.
  4. A running bulk job shows **"Downloading · done/total"**.
  5. A fully-complete series shows just **"Complete"** with no sub-lines.

---

## Self-Review (completed at authoring time)

- **Spec coverage:** B1 model/migration → T1+T2; B2 Sync compute (pageCount → dud; filed_count) → T3+T5; B3 manga_id population → T4; B4 render (breakdown + honest status) → T6; B5 testing → each task + T7. All covered.
- **Type consistency:** `LibraryCacheEntry.DudCount/FiledCount`, `Chapter.PageCount`, `SetSeriesMangaID(id, mangaID)`, `GetSeriesByMangaID(mangaID)`, `countCBZ(dir) (int, bool)`, `honestLibraryStatus(total, downloaded, dud, jobRunning, jobDone, jobTotal)`, `mostRecentBulkJob(mangaID) *model.BulkJob`, `libraryRow{DudCount,Undownloaded,FilingGap,Status,SeriesID}` — used consistently across tasks.
- **Grep-before-you-guess (verify, don't assume):** the `newTestStore`/`s.DB()` helper names; `GetLibraryCacheEntry`'s exact column order; `suwayomi.NewClient` signature; the poller `RunOnce` loop var names (`sid`, `s.SourcePath`) and any `SeriesStore` test fake; `fakeStore`/`fakeSuwayomi` fields; whether `JobStatus`/`mostRecentBulkJobStatus` have other callers before removal; that `h.previewer.ResolveLibraryDir` + `h.resolveSeriesDestDir` exist (they were added in Theme A).
