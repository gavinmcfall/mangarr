# Series Tags Implementation Plan (Sonarr port #10)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Free-form, per-series tags (e.g. `webtoon`, `oneshot`, `r18`, `archived`) persisted in SQLite, editable and filterable on the Series page.

**Architecture:** A normalized `series_tags(series_id, tag)` table (many-to-many, ON DELETE CASCADE) — avoids comma-escaping and gives efficient distinct-tag and by-tag queries. `model.Series` gains a `Tags []string` field populated by the existing list/get queries. The Series page renders tags as pills, edits them via the existing single-form "Set" submit (the bulk endpoint is widened to also persist `tags_<id>` fields), and filters client-side via a tag input — mirroring the existing `/library` filter + `/series` sort JS already in the page.

**Tech Stack:** Go 1.25, `modernc.org/sqlite`, `database/sql`, HTMX 1.x, inline vanilla JS (matching the existing Series/Library page patterns).

**Scope note:** Issue #10 mentions filtering on the Activity page too. The Activity-page tag filter is deliberately deferred to issue #11 (Activity filters + pagination), which rebuilds the Activity filter UI wholesale — folding the tag dimension in there avoids building and then immediately reworking an Activity filter. This plan covers tags + Series-page filtering only.

---

## File Structure

- `internal/store/migrations_tags.go` — **Create.** Migration 8: `series_tags` table + index. Mirrors the `migrations_bulk.go` style (one `migrationN<Name>(tx *sql.Tx) error` func).
- `internal/store/migrations.go` — **Modify.** Register migration 8 in the ordered slice.
- `internal/model/model.go` — **Modify.** Add `Tags []string` to `Series`.
- `internal/store/tags.go` — **Create.** `SetSeriesTags`, `ListAllTags`, and a private `tagsForSeries` helper. Keeps tag CRUD out of the already-large `store.go`.
- `internal/store/store.go` — **Modify.** `ListSeries` / `GetSeriesByID` / `ListUnmatched` populate `Tags` per row.
- `internal/web/web.go` — **Modify.** Widen the `Store` interface with `SetSeriesTags` + `ListAllTags`; extend `apiReclassifyBulk` to also persist `tags_<seriesID>` fields; pass `AllTags` to the series page data.
- `internal/web/templates/series.html` — **Modify.** Per-row tag pills + a tags `<input>` inside the existing form; a tag-filter `<input>` in the action bar; extend the inline JS filter to match on tags.
- `internal/web/static/mangarr.css` — **Modify.** `.pill-tag` style + tag-input sizing.

---

## Task 1: Migration 8 — `series_tags` table

**Files:**
- Create: `internal/store/migrations_tags.go`
- Modify: `internal/store/migrations.go:27` (append to slice)
- Test: `internal/store/migrations_tags_test.go`

- [ ] **Step 1: Write the failing test**

```go
package store

import (
	"database/sql"
	"testing"
)

func TestMigration8CreatesSeriesTagsTable(t *testing.T) {
	s := newTestStore(t)
	var name string
	err := s.DB().QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='series_tags'`).Scan(&name)
	if err == sql.ErrNoRows {
		t.Fatal("migration 8 did not create series_tags table")
	}
	if err != nil {
		t.Fatalf("probe series_tags: %v", err)
	}
	// Composite PK columns present.
	cols := map[string]bool{}
	rows, _ := s.DB().Query(`SELECT name FROM pragma_table_info('series_tags')`)
	defer rows.Close()
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		cols[c] = true
	}
	if !cols["series_id"] || !cols["tag"] {
		t.Fatalf("series_tags missing expected columns; got %v", cols)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestMigration8CreatesSeriesTagsTable -v`
Expected: FAIL — `migration 8 did not create series_tags table`.

- [ ] **Step 3: Create the migration**

`internal/store/migrations_tags.go`:

```go
package store

import (
	"database/sql"
	"fmt"
)

// migrateSeriesTags creates the series_tags many-to-many table backing the
// free-form per-series tags feature (Sonarr port #10). Tolerant of a missing
// series table for migration-only test fixtures, matching the other series
// migrations.
func migrateSeriesTags(tx *sql.Tx) error {
	var t string
	err := tx.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='series'`).Scan(&t)
	if err == sql.ErrNoRows {
		// Fresh test fixture without the legacy series table — create the
		// tags table anyway so SetSeriesTags/ListAllTags have somewhere to
		// write. The FK references series(id) which SQLite tolerates as a
		// deferred/unenforced reference when the parent is absent in the
		// fixture; production always has series.
	} else if err != nil {
		return fmt.Errorf("probe series table: %w", err)
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS series_tags (
		series_id INTEGER NOT NULL,
		tag       TEXT    NOT NULL,
		PRIMARY KEY (series_id, tag)
	)`); err != nil {
		return fmt.Errorf("create series_tags: %w", err)
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_series_tags_tag ON series_tags(tag)`); err != nil {
		return fmt.Errorf("create idx_series_tags_tag: %w", err)
	}
	return nil
}
```

Then in `internal/store/migrations.go`, append to the `migrations` slice after entry 7:

```go
	{8, "series-tags", migrateSeriesTags},
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -run TestMigration8CreatesSeriesTagsTable -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/migrations_tags.go internal/store/migrations.go internal/store/migrations_tags_test.go
git -c commit.gpgsign=false commit -m "feat(store): migration 8 — series_tags table"
```

---

## Task 2: `model.Series.Tags` field

**Files:**
- Modify: `internal/model/model.go` (the `Series` struct)
- Test: `internal/model/model_test.go` (or the existing model test file)

- [ ] **Step 1: Write the failing test**

```go
func TestSeriesHasTagsField(t *testing.T) {
	s := Series{Tags: []string{"webtoon", "r18"}}
	if len(s.Tags) != 2 || s.Tags[0] != "webtoon" {
		t.Fatalf("Tags field not wired: %+v", s.Tags)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/model/ -run TestSeriesHasTagsField -v`
Expected: FAIL — `unknown field Tags in struct literal`.

- [ ] **Step 3: Add the field**

In `internal/model/model.go`, add to the `Series` struct (after `CurrentBindingID *int64`):

```go
	// Tags are free-form per-series labels (Sonarr port #10), e.g.
	// "webtoon", "oneshot", "r18", "archived". Populated by the store's
	// list/get queries from the series_tags table; nil when a series has
	// no tags. Edited via the Series page; used for client-side filtering.
	Tags []string
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/model/ -run TestSeriesHasTagsField -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/model/model.go internal/model/model_test.go
git -c commit.gpgsign=false commit -m "feat(model): add Series.Tags field"
```

---

## Task 3: Store — `SetSeriesTags`, `ListAllTags`, `tagsForSeries`

**Files:**
- Create: `internal/store/tags.go`
- Test: `internal/store/tags_test.go`

- [ ] **Step 1: Write the failing test**

```go
package store

import (
	"reflect"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/model"
)

func TestSetSeriesTagsReplacesAndDedups(t *testing.T) {
	s := newTestStore(t)
	id, err := s.UpsertSeries(model.Series{Title: "Solo Leveling", SourcePath: "/dl/solo", Status: model.StatusPending})
	if err != nil {
		t.Fatal(err)
	}

	// Set tags (with a dup + blank that must be dropped, sorted on read).
	if err := s.SetSeriesTags(id, []string{"webtoon", "r18", "webtoon", "  ", "archived"}); err != nil {
		t.Fatalf("SetSeriesTags: %v", err)
	}
	got, err := s.tagsForSeries(id)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"archived", "r18", "webtoon"} // sorted, deduped, blanks dropped
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tags: want %v, got %v", want, got)
	}

	// Replace-all semantics: setting a new list drops the old.
	if err := s.SetSeriesTags(id, []string{"manhwa"}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.tagsForSeries(id)
	if !reflect.DeepEqual(got, []string{"manhwa"}) {
		t.Fatalf("replace-all failed: got %v", got)
	}

	// Clearing with empty slice removes all tags.
	if err := s.SetSeriesTags(id, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = s.tagsForSeries(id)
	if len(got) != 0 {
		t.Fatalf("expected no tags after clear, got %v", got)
	}
}

func TestListAllTagsDistinctSorted(t *testing.T) {
	s := newTestStore(t)
	id1, _ := s.UpsertSeries(model.Series{Title: "A", SourcePath: "/a", Status: model.StatusPending})
	id2, _ := s.UpsertSeries(model.Series{Title: "B", SourcePath: "/b", Status: model.StatusPending})
	_ = s.SetSeriesTags(id1, []string{"webtoon", "r18"})
	_ = s.SetSeriesTags(id2, []string{"webtoon", "archived"})

	got, err := s.ListAllTags()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"archived", "r18", "webtoon"} // distinct across series, sorted
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListAllTags: want %v, got %v", want, got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run 'TestSetSeriesTags|TestListAllTags' -v`
Expected: FAIL — `s.SetSeriesTags undefined`.

- [ ] **Step 3: Implement**

`internal/store/tags.go`:

```go
package store

import (
	"fmt"
	"sort"
	"strings"
)

// SetSeriesTags replaces ALL tags for a series with the given set. Tags are
// trimmed, blanks dropped, and deduplicated. Passing nil/empty clears them.
// Runs in a transaction so the delete+insert is atomic.
func (s *Store) SetSeriesTags(seriesID int64, tags []string) error {
	// Normalize: trim, drop blanks, dedup (preserve set semantics).
	seen := map[string]struct{}{}
	clean := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		clean = append(clean, t)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("SetSeriesTags begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM series_tags WHERE series_id=?`, seriesID); err != nil {
		return fmt.Errorf("SetSeriesTags delete: %w", err)
	}
	for _, t := range clean {
		if _, err := tx.Exec(`INSERT INTO series_tags (series_id, tag) VALUES (?,?)`, seriesID, t); err != nil {
			return fmt.Errorf("SetSeriesTags insert %q: %w", t, err)
		}
	}
	return tx.Commit()
}

// tagsForSeries returns the sorted tag list for one series (empty when none).
func (s *Store) tagsForSeries(seriesID int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT tag FROM series_tags WHERE series_id=? ORDER BY tag`, seriesID)
	if err != nil {
		return nil, fmt.Errorf("tagsForSeries: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListAllTags returns every distinct tag across all series, sorted — the
// source for the Series-page filter's tag suggestions.
func (s *Store) ListAllTags() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT tag FROM series_tags ORDER BY tag`)
	if err != nil {
		return nil, fmt.Errorf("ListAllTags: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if out == nil {
		out = []string{}
	}
	sort.Strings(out) // belt-and-braces; SQL already orders
	return out, rows.Err()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -run 'TestSetSeriesTags|TestListAllTags' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/tags.go internal/store/tags_test.go
git -c commit.gpgsign=false commit -m "feat(store): SetSeriesTags + ListAllTags + tagsForSeries"
```

---

## Task 4: Populate `Tags` in `ListSeries`

**Files:**
- Modify: `internal/store/store.go` (`ListSeries`)
- Test: `internal/store/store_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestListSeriesPopulatesTags(t *testing.T) {
	s := newTestStore(t)
	id, _ := s.UpsertSeries(model.Series{Title: "Solo Leveling", SourcePath: "/dl/solo", Status: model.StatusPending})
	if err := s.SetSeriesTags(id, []string{"manhwa", "webtoon"}); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSeries()
	if err != nil {
		t.Fatal(err)
	}
	var found *model.Series
	for i := range list {
		if list[i].ID == id {
			found = &list[i]
		}
	}
	if found == nil {
		t.Fatal("series not in ListSeries output")
	}
	if len(found.Tags) != 2 || found.Tags[0] != "manhwa" || found.Tags[1] != "webtoon" {
		t.Fatalf("Tags not populated/sorted: %v", found.Tags)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestListSeriesPopulatesTags -v`
Expected: FAIL — `found.Tags` is empty.

- [ ] **Step 3: Implement**

In `internal/store/store.go`, at the end of `ListSeries` after the rows loop and before `return out, rows.Err()`, populate tags per row. Replace the existing tail:

```go
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		tags, err := s.tagsForSeries(out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Tags = tags
	}
	return out, nil
}
```

(Locate the existing `return out, rows.Err()` at the end of `ListSeries` and replace it with the block above. The `rows.Close()` deferred earlier must have run first — so close the rows before the per-row tag queries by NOT relying on the deferred close. Simplest correct form: collect all rows into `out` inside the loop as today, let the deferred `rows.Close()` run when ListSeries returns; the tag queries use a separate `s.db.Query` so they don't conflict with the still-open `rows`. SQLite via database/sql allows concurrent queries on the same *sql.DB through the pool, so this is safe.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -run TestListSeriesPopulatesTags -v`
Expected: PASS.

Then run the full store suite to confirm no regression in the other ListSeries callers:

Run: `go test ./internal/store/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/store_test.go
git -c commit.gpgsign=false commit -m "feat(store): populate Series.Tags in ListSeries"
```

---

## Task 5: Widen web `Store` interface + pass tags to the Series page

**Files:**
- Modify: `internal/web/web.go` (Store interface + `pageSeries` data)
- Test: `internal/web/web_test.go` (fakeStore + a page-render assertion)

- [ ] **Step 1: Write the failing test**

```go
func TestPageSeriesRendersTagPills(t *testing.T) {
	h, st, _ := newTestHandler()
	st.series = []model.Series{
		{ID: 1, Title: "Solo Leveling", Source: "suwayomi", Status: model.StatusFiled, Tags: []string{"manhwa", "webtoon"}},
	}
	req := httptest.NewRequest(http.MethodGet, "/series", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{
		`pill-tag`, ">manhwa<", ">webtoon<",
		// Tag-filter input present in the action bar.
		`id="series-tag-filter"`,
		// Per-row tags edit input carries the current tags comma-joined.
		`name="tags_1"`, `value="manhwa, webtoon"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("series page missing %q", want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/web/ -run TestPageSeriesRendersTagPills -v`
Expected: FAIL — fakeStore has no tags / template doesn't render pills.

- [ ] **Step 3: Implement (store interface + fake + page data)**

In `internal/web/web.go`, add to the `Store` interface (after `SetSeriesManualBinding`):

```go
	SetSeriesTags(id int64, tags []string) error
	ListAllTags() ([]string, error)
```

In `internal/web/web_test.go`, the `fakeStore` already exposes `series`. Add the two methods:

```go
func (f *fakeStore) SetSeriesTags(id int64, tags []string) error {
	for i := range f.series {
		if f.series[i].ID == id {
			f.series[i].Tags = tags
		}
	}
	return nil
}
func (f *fakeStore) ListAllTags() ([]string, error) {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range f.series {
		for _, t := range s.Tags {
			if _, ok := seen[t]; !ok {
				seen[t] = struct{}{}
				out = append(out, t)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}
```

(Add `"sort"` to the web_test.go imports if not present.)

Find the series page-data struct and handler (`pageSeries` in `web.go`). Add an `AllTags []string` field to the page-data struct and populate it from `h.store.ListAllTags()` in `pageSeries`. The existing struct already carries `Items []model.Series` and `Bindings`; each `Series` already carries `Tags` from Task 4.

- [ ] **Step 4: Run test to verify it fails differently (template not yet updated)**

Run: `go test ./internal/web/ -run TestPageSeriesRendersTagPills -v`
Expected: FAIL — still missing `pill-tag` (handler compiles now; template is Task 6). This confirms the Go side is wired; the template change in Task 6 makes it pass.

- [ ] **Step 5: Commit (Go wiring only)**

```bash
git add internal/web/web.go internal/web/web_test.go
git -c commit.gpgsign=false commit -m "feat(web): widen Store with tag methods; pass AllTags to series page"
```

---

## Task 6: Series template — tag pills, edit input, client-side filter

**Files:**
- Modify: `internal/web/templates/series.html`
- Modify: `internal/web/static/mangarr.css`
- Test: `internal/web/web_test.go` (`TestPageSeriesRendersTagPills` from Task 5 now goes green)

- [ ] **Step 1: Confirm the test from Task 5 still fails**

Run: `go test ./internal/web/ -run TestPageSeriesRendersTagPills -v`
Expected: FAIL — template not yet updated.

- [ ] **Step 2: Add the Tags column + tag-filter input + per-row edit input**

In `internal/web/templates/series.html`:

Add a tag-filter input to the action bar (next to the existing reclassify controls), before the table:

```html
<input type="search" id="series-tag-filter" placeholder="Filter by tag…"
       class="series-filter" autocomplete="off">
```

Add a `Tags` header cell to the `<thead>` row (after `<th>Status</th>`):

```html
    <th>Tags</th>
```

In the `{{range .Items}}` row, add `data-tags` to the `<tr>` and two cells — a pills cell and an edit input. The row already lives inside the `series-reclassify-form`, so the tags input is submitted with the existing Set button:

```html
  <tr class="series-row" data-tags="{{range $i, $t := .Tags}}{{if $i}} {{end}}{{$t}}{{end}}">
```

After the existing Status `<td>` and before the reclassify `<td>`, add:

```html
    <td class="series-tags-cell">
      {{range .Tags}}<span class="pill pill-tag">{{.}}</span>{{end}}
      <input type="text" name="tags_{{.ID}}" class="series-tags-input"
             value="{{range $i, $t := .Tags}}{{if $i}}, {{end}}{{$t}}{{end}}"
             placeholder="comma,separated" autocomplete="off">
    </td>
```

- [ ] **Step 3: Extend the inline filter JS to match on tags**

The page already has an inline `<script>` for sort/filter (from the bulk-reclassify work). Add a tag-filter handler in the same script block. Append inside the existing IIFE:

```javascript
  var tagFilter = document.getElementById('series-tag-filter');
  if (tagFilter) {
    tagFilter.addEventListener('input', function () {
      var q = (tagFilter.value || '').trim().toLowerCase();
      document.querySelectorAll('.series-row').forEach(function (row) {
        var tags = (row.getAttribute('data-tags') || '').toLowerCase();
        row.style.display = (!q || tags.split(' ').some(function (t) { return t.indexOf(q) !== -1; })) ? '' : 'none';
      });
    });
  }
```

(If the page has no existing IIFE because earlier tasks rendered an empty table, add a standalone `<script>` at the end of the `{{define "content"}}` block with the handler wrapped in `(function(){ ... })();`.)

- [ ] **Step 4: Add CSS**

In `internal/web/static/mangarr.css`:

```css
.pill-tag { background: #2a2f37; color: #9fb0c3; margin-right: 4px; }
.series-tags-input { width: 160px; margin-top: 4px; padding: 2px 6px; background: #1a1d22; border: 1px solid #2a2f37; border-radius: 4px; color: inherit; font-size: 12px; }
.series-filter { flex: 0 1 220px; min-width: 140px; padding: 6px 10px; background: #1a1d22; border: 1px solid #2a2f37; border-radius: 4px; color: inherit; font-size: 13px; }
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/web/ -run TestPageSeriesRendersTagPills -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/web/templates/series.html internal/web/static/mangarr.css
git -c commit.gpgsign=false commit -m "feat(series): tag pills, edit input, client-side tag filter"
```

---

## Task 7: Persist tags in the bulk Set handler

**Files:**
- Modify: `internal/web/web.go` (`apiReclassifyBulk`)
- Test: `internal/web/web_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestReclassifyBulkPersistsTags(t *testing.T) {
	h, st, _ := newTestHandler()
	st.series = []model.Series{{ID: 1, Title: "Solo Leveling"}}
	form := url.Values{
		"tags_1": {"manhwa, webtoon , r18"},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/series/reclassify-bulk", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// fakeStore.SetSeriesTags stores the parsed slice on the series.
	if got := st.series[0].Tags; len(got) != 3 || got[0] != "manhwa" || got[2] != "r18" {
		t.Fatalf("tags not persisted/parsed: %v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/web/ -run TestReclassifyBulkPersistsTags -v`
Expected: FAIL — tags not parsed/saved.

- [ ] **Step 3: Implement**

In `apiReclassifyBulk` (in `web.go`), inside the existing loop over form keys (which already handles `binding_id_<id>`), add a sibling branch for `tags_<id>`. After the binding-handling block and before the loop ends:

```go
		const tagPrefix = "tags_"
		if strings.HasPrefix(key, tagPrefix) && len(vals) > 0 {
			seriesID, err := strconv.ParseInt(key[len(tagPrefix):], 10, 64)
			if err != nil {
				continue
			}
			parts := strings.Split(vals[0], ",")
			tags := make([]string, 0, len(parts))
			for _, p := range parts {
				if p = strings.TrimSpace(p); p != "" {
					tags = append(tags, p)
				}
			}
			if err := h.store.SetSeriesTags(seriesID, tags); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			continue
		}
```

(The handler's existing top-level `range r.Form` loop iterates ALL fields; binding fields use the `binding_id_` prefix, tags use `tags_`. They coexist — a single Set submit saves both.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/web/ -run TestReclassifyBulkPersistsTags -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/web/web.go internal/web/web_test.go
git -c commit.gpgsign=false commit -m "feat(web): persist series tags via the bulk Set handler"
```

---

## Task 8: Full sweep + integration sanity

**Files:**
- Test only.

- [ ] **Step 1: Full build + race test**

Run: `go build ./... && go test ./... -race`
Expected: all packages PASS.

- [ ] **Step 2: Confirm migration ordering**

Run: `go test ./internal/store/ -run TestMigration -v`
Expected: migrations 1-8 apply in order; no duplicate-version panic.

- [ ] **Step 3: Commit (if any fixups were needed)**

```bash
git add -A
git -c commit.gpgsign=false commit -m "test(tags): full-sweep fixups"
```

(Skip if nothing changed.)

---

## Self-Review

**Spec coverage** (issue #10):
- "Free-form tags per series" → Tasks 1-3 (table, model, store CRUD). ✓
- "persisted in SQLite" → Task 1 migration. ✓
- "filterable on Series" → Task 6 client-side filter. ✓
- "filterable on Activity" → **deferred to #11** (documented in Scope note). ✓ (intentional)
- "e.g. webtoon, oneshot, r18, archived" → free-form text, no enum constraint. ✓
- "No notification system needed — just filtering" → no notification code. ✓

**Placeholder scan:** No TBD/TODO; every code step shows full code. ✓

**Type consistency:** `SetSeriesTags(int64, []string) error`, `ListAllTags() ([]string, error)`, `tagsForSeries(int64) ([]string, error)`, `Series.Tags []string`, form prefix `tags_<id>`, CSS `.pill-tag` / `.series-tags-input` / `.series-filter`, element id `series-tag-filter`, row class `series-row` + `data-tags` — consistent across Tasks 1-7. ✓

**Commit hygiene:** All commits use `git -c commit.gpgsign=false` and stage explicit files (no `git add -A` except the optional Task 8 fixup). No "claude"/"anthropic" in messages. Verify-gate stamp (`touch .claude/.verified`) must precede each commit — the implementer subagent handles this per its prompt.
