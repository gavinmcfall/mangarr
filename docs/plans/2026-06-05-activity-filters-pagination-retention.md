# Activity Filters + Pagination + Retention Implementation Plan (Sonarr port #11)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the Activity page a usable audit log: server-side filtering (event type, series, tag, date range), pagination, and a configurable retention with a GC task — instead of one unbounded 200-row list.

**Architecture:** The current `ListActivity(limit)` is replaced by a filtered, paginated query `ListActivityFiltered(ActivityFilter) ([]ActivityEntry, total int, error)`. Filters compose as SQL `WHERE` clauses; pagination via `LIMIT/OFFSET`; total count via a sibling `COUNT(*)` over the same predicate so the UI can render page navigation. The tag filter resolves through a `series_tags` subquery (tags landed in #10). Retention adds `Settings.ActivityRetentionDays` + a `DeleteActivityOlderThan` store method + an `activity-gc` task registered with the existing tasks registry (#7) and invoked on the poller's periodic ticker.

**Tech Stack:** Go 1.25, `modernc.org/sqlite`, `database/sql`, HTMX 1.x, server-rendered templates.

**Scope decisions (defaults chosen; flag to operator if different preference):**
- **Retention default:** 90 days. `0` = keep forever (safe default for anyone who doesn't set it).
- **Page size:** 50 rows.
- **Date range:** preset buttons (24h / 7d / 30d / all) rather than free date pickers — lighter, matches the issue's "Light" effort, and covers the real audit use case. Free from/to pickers can come later if needed.
- **Tag filter:** folded in here from #10 (deferred deliberately so the Activity filter UI is built once).

---

## File Structure

- `internal/model/activity_filter.go` — **Create.** `ActivityFilter` struct (the query parameters) + a small `ActivityPage` result wrapper. Keeps the filter type out of the already-large model.go.
- `internal/model/model.go` — **Modify.** Add `Settings.ActivityRetentionDays int`.
- `internal/store/activity.go` — **Create.** `ListActivityFiltered`, `DeleteActivityOlderThan`. Moves activity queries out of store.go into a focused file.
- `internal/store/store.go` — **Modify.** Keep the existing `ListActivity(limit)` (other callers/tests use it) — `ListActivityFiltered` is additive.
- `internal/store/settings_defaults` (wherever GetSettings applies bulk defaults) — **Modify.** Default `ActivityRetentionDays` to 90 when zero-on-read is ambiguous — see Task 3 note.
- `internal/web/web.go` — **Modify.** Rework `pageActivity` + `apiListActivity` to parse filter+page query params; widen the `Store` interface with the two new methods; add the activity page-data fields (filter echo, page nav, distinct actions, all tags).
- `internal/web/templates/activity.html` — **Modify.** Filter bar (type dropdown, series text, tag dropdown, date presets), pagination nav, preserve existing row rendering + Via labels.
- `internal/tasks` wiring in `main.go` — **Modify.** Register `activity-gc` task; invoke periodically.

---

## Task 1: `ActivityFilter` model type

**Files:**
- Create: `internal/model/activity_filter.go`
- Test: `internal/model/activity_filter_test.go`

- [ ] **Step 1: Write the failing test**

```go
package model

import "testing"

func TestActivityFilterZeroValueIsUnfiltered(t *testing.T) {
	var f ActivityFilter
	if f.Action != "" || f.SeriesLike != "" || f.Tag != "" || !f.After.IsZero() || f.Limit != 0 || f.Offset != 0 {
		t.Fatalf("zero ActivityFilter should be fully unfiltered: %+v", f)
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/model/ -run TestActivityFilterZeroValueIsUnfiltered -v` — Expected FAIL (undefined ActivityFilter).

- [ ] **Step 3: Create** `internal/model/activity_filter.go`:

```go
package model

import "time"

// ActivityFilter is the query parameter set for the Activity page's
// server-side filtering + pagination. The zero value selects everything
// (no filters, no paging) so callers can set only what they need.
type ActivityFilter struct {
	Action     ActivityAction // "" = any action
	SeriesLike string         // "" = any series; case-insensitive substring on series_title
	Tag        string         // "" = any tag; matches series carrying this exact tag
	After      time.Time      // zero = no lower bound (inclusive)
	Limit      int            // 0 = no limit (caller should set a page size)
	Offset     int            // pagination offset
}

// ActivityPage is one page of filtered activity plus the total matching
// count (across all pages) so the UI can render navigation.
type ActivityPage struct {
	Items []ActivityEntry
	Total int
}
```

- [ ] **Step 4: Run** the test — Expected PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/model/activity_filter.go internal/model/activity_filter_test.go
git -c commit.gpgsign=false commit -m "feat(model): ActivityFilter + ActivityPage types"
```

---

## Task 2: Store — `ListActivityFiltered`

**Files:**
- Create: `internal/store/activity.go`
- Test: `internal/store/activity_test.go`

- [ ] **Step 1: Write the failing test** (seed rows across actions/series/times, assert each filter dimension + pagination + total)

```go
package store

import (
	"testing"
	"time"

	"github.com/gavinmcfall/mangarr/internal/model"
)

func seedActivity(t *testing.T, s *Store) {
	t.Helper()
	// Insert via AddActivity (sets ts=CURRENT_TIMESTAMP), then back-date some
	// rows directly so the date-range filter is testable.
	rows := []model.ActivityEntry{
		{SeriesTitle: "Berserk", Action: model.ActionFiled, Via: "rule:5"},
		{SeriesTitle: "Solo Leveling", Action: model.ActionFiled, Via: "rule:6"},
		{SeriesTitle: "Berserk", Action: model.ActionError, Detail: "boom"},
		{SeriesTitle: "Dandadan", Action: model.ActionUnmatched, Via: "unmatched"},
	}
	for _, r := range rows {
		if err := s.AddActivity(r); err != nil {
			t.Fatal(err)
		}
	}
}

func TestListActivityFilteredByAction(t *testing.T) {
	s := newTestStore(t)
	seedActivity(t, s)
	page, err := s.ListActivityFiltered(model.ActivityFilter{Action: model.ActionFiled, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("action filter: want 2 filed, got total=%d len=%d", page.Total, len(page.Items))
	}
	for _, e := range page.Items {
		if e.Action != model.ActionFiled {
			t.Errorf("unexpected action %q", e.Action)
		}
	}
}

func TestListActivityFilteredBySeriesSubstring(t *testing.T) {
	s := newTestStore(t)
	seedActivity(t, s)
	page, err := s.ListActivityFiltered(model.ActivityFilter{SeriesLike: "ber", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 { // both Berserk rows, case-insensitive
		t.Fatalf("series filter: want 2, got %d", page.Total)
	}
}

func TestListActivityFilteredPagination(t *testing.T) {
	s := newTestStore(t)
	seedActivity(t, s)
	page1, _ := s.ListActivityFiltered(model.ActivityFilter{Limit: 2, Offset: 0})
	page2, _ := s.ListActivityFiltered(model.ActivityFilter{Limit: 2, Offset: 2})
	if page1.Total != 4 || page2.Total != 4 {
		t.Fatalf("total should be 4 regardless of page; got %d/%d", page1.Total, page2.Total)
	}
	if len(page1.Items) != 2 || len(page2.Items) != 2 {
		t.Fatalf("page sizes: got %d/%d", len(page1.Items), len(page2.Items))
	}
	if page1.Items[0].ID == page2.Items[0].ID {
		t.Fatal("pages overlap — offset not applied")
	}
}

func TestListActivityFilteredByTag(t *testing.T) {
	s := newTestStore(t)
	seedActivity(t, s)
	// Tag "Berserk" series so the tag filter resolves through series_tags.
	id, _ := s.UpsertSeries(model.Series{Title: "Berserk", SourcePath: "/dl/berserk", Status: model.StatusPending})
	if err := s.SetSeriesTags(id, []string{"dark"}); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListActivityFiltered(model.ActivityFilter{Tag: "dark", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 { // both Berserk activity rows
		t.Fatalf("tag filter: want 2 Berserk rows, got %d", page.Total)
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/store/ -run TestListActivityFiltered -v` — Expected FAIL (undefined).

- [ ] **Step 3: Implement** `internal/store/activity.go`. Build the predicate once, use it for both the COUNT and the page query.

```go
package store

import (
	"fmt"
	"strings"

	"github.com/gavinmcfall/mangarr/internal/model"
)

// ListActivityFiltered returns one page of activity entries matching the
// filter, plus the total count across all pages (for navigation). Newest
// first (ORDER BY id DESC). A zero Limit means "no limit".
func (s *Store) ListActivityFiltered(f model.ActivityFilter) (model.ActivityPage, error) {
	var where []string
	var args []any

	if f.Action != "" {
		where = append(where, "action = ?")
		args = append(args, string(f.Action))
	}
	if f.SeriesLike != "" {
		where = append(where, "series_title LIKE ? COLLATE NOCASE")
		args = append(args, "%"+f.SeriesLike+"%")
	}
	if f.Tag != "" {
		where = append(where, `series_title IN (
			SELECT s.title FROM series s
			JOIN series_tags t ON t.series_id = s.id
			WHERE t.tag = ?)`)
		args = append(args, f.Tag)
	}
	if !f.After.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, f.After.UTC().Format("2006-01-02 15:04:05"))
	}

	clause := ""
	if len(where) > 0 {
		clause = "WHERE " + strings.Join(where, " AND ")
	}

	// Total (across all pages).
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM activity `+clause, args...).Scan(&total); err != nil {
		return model.ActivityPage{}, fmt.Errorf("ListActivityFiltered count: %w", err)
	}

	// Page query.
	q := `SELECT id, ts, series_title, action, detail, via FROM activity ` + clause + ` ORDER BY id DESC`
	pageArgs := append([]any{}, args...)
	if f.Limit > 0 {
		q += " LIMIT ?"
		pageArgs = append(pageArgs, f.Limit)
		if f.Offset > 0 {
			q += " OFFSET ?"
			pageArgs = append(pageArgs, f.Offset)
		}
	}
	rows, err := s.db.Query(q, pageArgs...)
	if err != nil {
		return model.ActivityPage{}, fmt.Errorf("ListActivityFiltered query: %w", err)
	}
	defer rows.Close()
	var out []model.ActivityEntry
	for rows.Next() {
		var e model.ActivityEntry
		var ts string
		if err := rows.Scan(&e.ID, &ts, &e.SeriesTitle, &e.Action, &e.Detail, &e.Via); err != nil {
			return model.ActivityPage{}, err
		}
		// Match the existing ListActivity ts parsing (check store.go ListActivity
		// for the exact layout; SQLite CURRENT_TIMESTAMP is "2006-01-02 15:04:05").
		e.Time = parseActivityTS(ts)
		out = append(out, e)
	}
	return model.ActivityPage{Items: out, Total: total}, rows.Err()
}
```

**Note:** Reuse the existing timestamp parse from `ListActivity` in `store.go`. If it parses inline, extract a `parseActivityTS(string) time.Time` helper and use it in both places (DRY). Confirm the exact format string the existing code uses and mirror it.

- [ ] **Step 4: Run** the tests — Expected PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/activity.go internal/store/activity_test.go internal/store/store.go
git -c commit.gpgsign=false commit -m "feat(store): ListActivityFiltered with action/series/tag/date filters + pagination"
```

---

## Task 3: Store — `DeleteActivityOlderThan` + retention setting

**Files:**
- Modify: `internal/store/activity.go`
- Modify: `internal/model/model.go` (Settings)
- Modify: wherever GetSettings applies defaults (search for `BulkStallTimeoutMinutes` default)
- Test: `internal/store/activity_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestDeleteActivityOlderThan(t *testing.T) {
	s := newTestStore(t)
	seedActivity(t, s) // 4 rows at ~now
	// Back-date two rows by 100 days via direct UPDATE.
	if _, err := s.DB().Exec(`UPDATE activity SET ts = ? WHERE id IN (SELECT id FROM activity ORDER BY id ASC LIMIT 2)`,
		time.Now().AddDate(0, 0, -100).UTC().Format("2006-01-02 15:04:05")); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().AddDate(0, 0, -90)
	n, err := s.DeleteActivityOlderThan(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("want 2 rows deleted, got %d", n)
	}
	page, _ := s.ListActivityFiltered(model.ActivityFilter{Limit: 50})
	if page.Total != 2 {
		t.Fatalf("want 2 rows remaining, got %d", page.Total)
	}
}

func TestGetSettingsDefaultsActivityRetention(t *testing.T) {
	s := newTestStore(t)
	set, err := s.GetSettings()
	if err != nil {
		t.Fatal(err)
	}
	if set.ActivityRetentionDays != 90 {
		t.Fatalf("want default 90, got %d", set.ActivityRetentionDays)
	}
}
```

- [ ] **Step 2: Run** — Expected FAIL.

- [ ] **Step 3: Implement**

In `internal/store/activity.go`:

```go
// DeleteActivityOlderThan removes activity rows with ts strictly before the
// cutoff. Returns the number of rows deleted. The activity-gc task calls
// this with now-minus-retention.
func (s *Store) DeleteActivityOlderThan(cutoff time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM activity WHERE ts < ?`,
		cutoff.UTC().Format("2006-01-02 15:04:05"))
	if err != nil {
		return 0, fmt.Errorf("DeleteActivityOlderThan: %w", err)
	}
	return res.RowsAffected()
}
```

(Add `"time"` import to activity.go.)

In `internal/model/model.go` Settings struct:

```go
	// ActivityRetentionDays controls the activity-gc task: rows older than
	// this many days are deleted. 0 = keep forever. Default 90 (applied at
	// GetSettings read time).
	ActivityRetentionDays int `json:"activity_retention_days"`
```

In the GetSettings defaults block (next to `BulkStallTimeoutMinutes == 0` → 30):

```go
	if set.ActivityRetentionDays == 0 {
		set.ActivityRetentionDays = 90
	}
```

**Note:** this makes `0` resolve to 90 on read, so "keep forever" can't be expressed as 0 once defaults apply. That's an acceptable tradeoff for this issue (90-day default is the safe behaviour; the issue says "configurable retention" not "infinite option"). If infinite-retention is wanted, that's a follow-up using a sentinel (-1). Flag this in the PR.

- [ ] **Step 4: Run** — Expected PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/activity.go internal/model/model.go internal/store/store.go
git -c commit.gpgsign=false commit -m "feat: activity retention — DeleteActivityOlderThan + ActivityRetentionDays default 90"
```

---

## Task 4: `activity-gc` task + periodic wiring

**Files:**
- Modify: `main.go`
- Test: manual (the tasks registry already has its own tests; this is wiring)

- [ ] **Step 1: Read** `main.go` where the tasks registry is built and where `reg.Register(...)` is called for existing tasks (e.g. backup, poll-scan). Read `internal/tasks/tasks.go` for the `Register` + task-func signature.

- [ ] **Step 2: Register the task.** Where other tasks are registered, add:

```go
// activity-gc: prune activity rows past the configured retention. Runs on
// the same periodic cadence as the metrics sweep; also runnable on demand
// from the Tasks page.
_ = reg.Register(tasks.New("activity-gc", "Activity GC", 0, func(ctx context.Context) error {
	set, err := st.GetSettings()
	if err != nil {
		return err
	}
	if set.ActivityRetentionDays <= 0 {
		return nil // retention disabled
	}
	cutoff := time.Now().AddDate(0, 0, -set.ActivityRetentionDays)
	n, err := st.DeleteActivityOlderThan(cutoff)
	if err != nil {
		return err
	}
	if n > 0 {
		log.Printf("activity-gc: pruned %d activity rows older than %d days", n, set.ActivityRetentionDays)
	}
	return nil
}))
```

(Match the actual `tasks.New` / registration signature found in Step 1 — adapt arg order/types as needed.)

- [ ] **Step 3: Invoke periodically.** Find the existing metrics-sweep ticker goroutine in main.go; add an `activity-gc` run on the same ticker (once per sweep is plenty for a daily-scale retention), via `reg.RunNow(ctx, "activity-gc")` or by calling the task func directly. Keep it best-effort (log errors, don't crash the sweep).

- [ ] **Step 4: Build + run** `go build ./... && go test ./...` — green (no new test; this is wiring verified by build + existing task-registry tests).

- [ ] **Step 5: Commit**

```bash
git add main.go
git -c commit.gpgsign=false commit -m "feat: register activity-gc task + run on periodic ticker"
```

---

## Task 5: Web — widen Store interface + filtered/paginated `pageActivity`

**Files:**
- Modify: `internal/web/web.go`
- Test: `internal/web/web_test.go`

- [ ] **Step 1: Write the failing test** (the page accepts query params and echoes filter state + paginates)

```go
func TestPageActivityFiltersAndPaginates(t *testing.T) {
	h, st, _ := newTestHandler()
	// fakeStore.activity seeded with a known set (extend fakeStore as needed).
	st.activity = []model.ActivityEntry{
		{ID: 4, SeriesTitle: "Dandadan", Action: model.ActionUnmatched},
		{ID: 3, SeriesTitle: "Berserk", Action: model.ActionError},
		{ID: 2, SeriesTitle: "Solo Leveling", Action: model.ActionFiled},
		{ID: 1, SeriesTitle: "Berserk", Action: model.ActionFiled},
	}
	req := httptest.NewRequest(http.MethodGet, "/activity?action=filed&page=1&page_size=50", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	// Filter bar renders with the selected action; only filed rows shown.
	for _, want := range []string{`name="action"`, `name="series"`, "Solo Leveling", "Berserk"} {
		if !strings.Contains(body, want) {
			t.Errorf("activity page missing %q", want)
		}
	}
	if strings.Contains(body, "Dandadan") {
		t.Error("action=filed should exclude the Unmatched Dandadan row")
	}
}
```

- [ ] **Step 2:** Add `ListActivityFiltered(model.ActivityFilter) (model.ActivityPage, error)` to the web `Store` interface. Extend `fakeStore` with an in-memory implementation that honors Action/SeriesLike/Limit/Offset (tag/date can be minimally supported). Run — Expected FAIL until the handler is reworked.

- [ ] **Step 3: Rework `pageActivity`** to parse query params (`action`, `series`, `tag`, `range` preset → After, `page`, `page_size`), build an `ActivityFilter`, call `ListActivityFiltered`, and pass filter echo + pagination metadata (current page, total pages, total count) + `AllTags` + the distinct action list to the template. Keep `buildActivityRows` for Via-label resolution on the returned page.

- [ ] **Step 4: Run** — Expected PASS (with the Task 6 template; if the template isn't updated yet, this fails on missing filter-bar markup — that's the Task 6 boundary, same pattern as the #10 plan).

- [ ] **Step 5: Commit**

```bash
git add internal/web/web.go internal/web/web_test.go
git -c commit.gpgsign=false commit -m "feat(web): server-side activity filter + pagination in pageActivity"
```

---

## Task 6: Activity template — filter bar + pagination nav

**Files:**
- Modify: `internal/web/templates/activity.html`
- Modify: `internal/web/static/mangarr.css`
- Test: `internal/web/web_test.go` (Task 5 test goes green)

- [ ] **Step 1:** Confirm Task 5 test still fails (template not updated).

- [ ] **Step 2: Add the filter bar** above the activity table: a `<form method="get" action="/activity">` with —
  - `<select name="action">` (All + each distinct action),
  - `<input type="search" name="series">` (echoes current value),
  - `<select name="tag">` (All + `.AllTags`),
  - date preset buttons/links (`?range=24h|7d|30d|all`),
  - a Filter submit button.
  Each control pre-selects/echoes the current filter from the page data.

- [ ] **Step 3: Add pagination nav** below the table: Prev/Next links that carry the current filter query params with `page` incremented/decremented, plus "Page X of Y (N total)". Disable Prev on page 1 and Next on the last page.

- [ ] **Step 4: CSS** for the filter bar layout + pagination controls (reuse existing `.series-filter` / button styles where possible).

- [ ] **Step 5: Run** the Task 5 test — Expected PASS. Full web suite green.

- [ ] **Step 6: Commit**

```bash
git add internal/web/templates/activity.html internal/web/static/mangarr.css
git -c commit.gpgsign=false commit -m "feat(activity): filter bar (type/series/tag/date) + pagination nav"
```

---

## Task 7: Settings page — retention knob

**Files:**
- Modify: `internal/web/templates/settings.html`
- Modify: `internal/web/web.go` (settings save handler)
- Test: `internal/web/web_test.go`

- [ ] **Step 1: Write the failing test** — POST /settings with `activity_retention_days=30`, GET back, assert round-trip.

```go
func TestSaveSettingsRoundTripsActivityRetention(t *testing.T) {
	h, st, _ := newTestHandler()
	stub := kavitaStubServer(t, nil, 0, 0)
	defer stub.Close()
	form := url.Values{
		"file_mode":              {"hardlink"},
		"rename_scheme":          {"{series}/{series} - Ch.{chapter}.cbz"},
		"poll_minutes":           {"15"},
		"kavita_base_url":        {stub.URL},
		"kavita_api_key":         {"k"},
		"activity_retention_days": {"30"},
	}
	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d: %s", rec.Code, rec.Body.String())
	}
	if st.settings.ActivityRetentionDays != 30 {
		t.Fatalf("retention not saved: %d", st.settings.ActivityRetentionDays)
	}
}
```

- [ ] **Step 2: Run** — Expected FAIL.

- [ ] **Step 3: Implement** — add a numeric input to settings.html (near the Bulk Download card or a new "Activity" line) bound to `.Settings.ActivityRetentionDays`; parse `activity_retention_days` in the settings save handler with the `strconv.Atoi`-or-ignore pattern used for the bulk fields.

- [ ] **Step 4: Run** — Expected PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/web/templates/settings.html internal/web/web.go internal/web/web_test.go
git -c commit.gpgsign=false commit -m "feat(settings): activity retention days input"
```

---

## Task 8: Full sweep + final review

- [ ] **Step 1:** `go build ./... && go test ./... -race` — all green (run with `TZ=UTC` to avoid the known unrelated poller GC timezone flake on local UTC+12).
- [ ] **Step 2:** Manual scan of the plan vs issue #11 requirements (self-review below).
- [ ] **Step 3:** Commit any fixups.

---

## Self-Review

**Spec coverage (issue #11):**
- "type filter (Filed / Unmatched / ScanTriggered / Error)" → Task 2 Action filter + Task 6 dropdown. ✓ (also covers bulk-* actions since the dropdown lists distinct actions present)
- "series filter" → Task 2 SeriesLike + Task 6 input. ✓
- "date range" → Task 2 After + Task 6 presets. ✓ (presets, not free pickers — flagged)
- "pagination" → Task 2 Limit/Offset + Total, Task 6 nav. ✓
- "configurable retention — keep activity for N days" → Task 3 setting + Task 7 UI. ✓
- "GC task" → Task 4 activity-gc task + periodic run. ✓ (depends on #7 tasks registry — shipped)
- Deferred tag filter from #10 → Task 2 Tag + Task 6 dropdown. ✓

**Placeholder scan:** Concrete SQL + Go throughout. The two "match existing parse / registration signature" notes (Task 2 ts parse, Task 4 tasks.New) are deliberate — the implementer reads the real signature first; full surrounding code is shown.

**Type consistency:** `ActivityFilter{Action, SeriesLike, Tag, After, Limit, Offset}`, `ActivityPage{Items, Total}`, `ListActivityFiltered(ActivityFilter) (ActivityPage, error)`, `DeleteActivityOlderThan(time.Time) (int64, error)`, `Settings.ActivityRetentionDays int`, query params `action/series/tag/range/page/page_size`, task id `activity-gc` — consistent across Tasks 1-7. ✓

**Commit hygiene:** `git -c commit.gpgsign=false`, explicit file staging, no "claude"/"anthropic", verify-gate stamp before each commit (implementer subagent handles per its prompt). Run tests with `TZ=UTC` to dodge the unrelated poller GC local flake.
