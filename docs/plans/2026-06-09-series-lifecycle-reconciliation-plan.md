# Series Lifecycle & Reconciliation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the disk scan a true reconcile — soft-delete series whose source folder vanished (guarded), and add a durable user delete with optional file removal — so the series set stays trustworthy.

**Architecture:** A new reconcile pass in `poller.RunOnce` compares the on-disk series set against the DB set; a sanity floor rejects obviously-bad scans, a time grace debounces real removals into `status=orphaned`. A new `DeleteSeries` store method + web route gives a durable, two-step delete that optionally routes files through the existing recycle bin. Orphaned series surface in the UI with a confirm-removal banner.

**Tech Stack:** Go 1.25, `modernc.org/sqlite`, `database/sql`, `net/http` (std mux), `html/template`, HTMX. Tests are standard `go test` with table-driven cases and in-memory fakes.

**Spec:** `docs/specs/2026-06-09-series-lifecycle-reconciliation-design.md`

---

## File Structure

| File | Responsibility | Change |
|------|----------------|--------|
| `internal/store/migrations.go` | schema migration list | add v9 `series-missing-since` |
| `internal/model/model.go` | `Status`, `Series`, `Settings` | add `StatusOrphaned`, `Series.MissingSince`, 3 reconcile settings |
| `internal/store/store.go` | series persistence | add `MissingSince` to scans, `applySettingsDefaults`, new methods `ListSeriesLite`, `SetSeriesMissingSince`, `SetSeriesStatus`, `DeleteSeries` |
| `internal/poller/reconcile.go` | the reconcile pass (new file) | floor + grace logic |
| `internal/poller/poller.go` | wire reconcile into `RunOnce`; widen `SeriesStore` | call `p.reconcile(...)` after `ScanAll` |
| `internal/web/series_delete.go` | delete handler + file-removal helper (new file) | `apiSeriesDelete`, `apiSeriesRestore`, `binSeriesFiles` |
| `internal/web/web.go` | route registration | register the two new routes |
| `internal/web/templates/series.html` | UI | orphaned banner + delete modal trigger |

---

## Task 1: Migration v9 — `series.missing_since` column

**Files:**
- Modify: `internal/store/migrations.go` (append to `migrations` list + new func)
- Test: `internal/store/migrations_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/store/migrations_test.go`:

```go
func TestMigration9AddsMissingSinceColumn(t *testing.T) {
	db := newTestDB(t) // existing helper: opens a fresh DB + runs all migrations
	var name string
	err := db.QueryRow(
		`SELECT name FROM pragma_table_info('series') WHERE name='missing_since'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("missing_since column not present after migrations: %v", err)
	}
	if name != "missing_since" {
		t.Fatalf("got column %q, want missing_since", name)
	}
}
```

If `newTestDB` is not the helper name, grep `internal/store/migrations_test.go` for the existing fresh-DB helper and use it verbatim.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd internal/store && go test -run TestMigration9AddsMissingSinceColumn -v`
Expected: FAIL — column `missing_since` does not exist.

- [ ] **Step 3: Add the migration**

In `internal/store/migrations.go`, append to the `migrations` slice:

```go
	{9, "series-missing-since", migrateSeriesMissingSince},
```

Add the function (mirror `migrateSeriesManualBinding`'s table-probe + duplicate-column guard):

```go
// migrateSeriesMissingSince adds series.missing_since (unix seconds, NULL =
// present on disk). The reconcile pass sets it when a series' source folder
// first goes absent and uses it as the grace timer before flagging the
// series 'orphaned'. Idempotent under the schema_versions gate; tolerant of
// a missing series table (migration-only test fixtures).
func migrateSeriesMissingSince(tx *sql.Tx) error {
	var seriesTable string
	err := tx.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='series'`).Scan(&seriesTable)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("probe series table: %w", err)
	}
	var name string
	err = tx.QueryRow(`SELECT name FROM pragma_table_info('series') WHERE name = 'missing_since'`).Scan(&name)
	if err == nil {
		return nil // already present
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("probe missing_since column: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE series ADD COLUMN missing_since INTEGER`); err != nil {
		return fmt.Errorf("add series.missing_since: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd internal/store && go test -run TestMigration9AddsMissingSinceColumn -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store/migrations.go internal/store/migrations_test.go
git commit -m "feat(store): migration v9 adds series.missing_since column"
```

---

## Task 2: Model — `StatusOrphaned` + reconcile settings

**Files:**
- Modify: `internal/model/model.go`
- Modify: `internal/store/store.go` (`applySettingsDefaults`)
- Test: `internal/store/store_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/store/store_test.go`:

```go
func TestApplySettingsDefaultsReconcile(t *testing.T) {
	var s model.Settings
	applySettingsDefaults(&s)
	if s.ReconcileGraceMinutes != 10 {
		t.Errorf("grace = %d, want 10", s.ReconcileGraceMinutes)
	}
	if s.ReconcileMassVanishPercent != 25 {
		t.Errorf("percent = %d, want 25", s.ReconcileMassVanishPercent)
	}
	if s.ReconcileMassVanishMinCount != 5 {
		t.Errorf("minCount = %d, want 5", s.ReconcileMassVanishMinCount)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd internal/store && go test -run TestApplySettingsDefaultsReconcile -v`
Expected: FAIL — fields undefined (compile error).

- [ ] **Step 3: Add the model + status + defaults**

In `internal/model/model.go`, after `StatusError`:

```go
	// StatusOrphaned marks a series whose source folder vanished from disk
	// (confirmed by the reconcile grace timer). Excluded from classify/file;
	// retained in the DB and surfaced in the UI for confirm-removal.
	StatusOrphaned Status = "orphaned"
```

In `model.Series` struct, after `UpdatedAt`:

```go
	// MissingSince is set by the reconcile pass when the series' source
	// folder is first observed absent; nil when present on disk. When the
	// gap exceeds the reconcile grace, Status flips to StatusOrphaned.
	MissingSince *time.Time
```

In `model.Settings` struct, after `ActivityRetentionDays`:

```go
	// ReconcileGraceMinutes is how long a series' source folder must be
	// continuously absent before reconcile flags it orphaned. Default 10.
	ReconcileGraceMinutes int `json:"reconcile_grace_minutes,omitempty"`
	// ReconcileMassVanishPercent and ReconcileMassVanishMinCount form the
	// sanity floor: a single reconcile aborts (logs an alert, writes
	// nothing) when the newly-absent count is BOTH > Percent% of active
	// series AND >= MinCount. Defaults 25 and 5.
	ReconcileMassVanishPercent  int `json:"reconcile_mass_vanish_percent,omitempty"`
	ReconcileMassVanishMinCount int `json:"reconcile_mass_vanish_min_count,omitempty"`
```

In `internal/store/store.go`, in `applySettingsDefaults`, before the closing brace:

```go
	if s.ReconcileGraceMinutes == 0 {
		s.ReconcileGraceMinutes = 10
	}
	if s.ReconcileMassVanishPercent == 0 {
		s.ReconcileMassVanishPercent = 25
	}
	if s.ReconcileMassVanishMinCount == 0 {
		s.ReconcileMassVanishMinCount = 5
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd internal/store && go test -run TestApplySettingsDefaultsReconcile -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/model/model.go internal/store/store.go internal/store/store_test.go
git commit -m "feat(model): add StatusOrphaned + reconcile settings defaults"
```

---

## Task 3: Store — reconcile read/write + durable delete

**Files:**
- Modify: `internal/store/store.go`
- Test: `internal/store/store_test.go`

`ListSeriesLite` returns only the columns the reconcile needs (id, source_path, status, missing_since) to avoid touching the existing full-row scans.

- [ ] **Step 1: Write the failing tests**

Add to `internal/store/store_test.go`:

```go
func TestSetSeriesMissingSinceAndStatusRoundTrip(t *testing.T) {
	s := newStore(t) // existing helper: a *Store on a fresh migrated DB
	id, err := s.UpsertSeries(model.Series{
		Title: "X", SourcePath: "/d/X", Source: "suwayomi",
		Type: model.TypeUnknown, Status: model.StatusPending,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0).UTC()
	if err := s.SetSeriesMissingSince(id, &now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSeriesStatus(id, model.StatusOrphaned); err != nil {
		t.Fatal(err)
	}
	lite, err := s.ListSeriesLite()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, l := range lite {
		if l.ID == id {
			found = true
			if l.Status != model.StatusOrphaned {
				t.Errorf("status = %q, want orphaned", l.Status)
			}
			if l.MissingSince == nil || !l.MissingSince.Equal(now) {
				t.Errorf("missing_since = %v, want %v", l.MissingSince, now)
			}
			if l.SourcePath != "/d/X" {
				t.Errorf("source_path = %q", l.SourcePath)
			}
		}
	}
	if !found {
		t.Fatal("series not returned by ListSeriesLite")
	}

	// Clearing missing_since sets it back to NULL.
	if err := s.SetSeriesMissingSince(id, nil); err != nil {
		t.Fatal(err)
	}
	lite, _ = s.ListSeriesLite()
	for _, l := range lite {
		if l.ID == id && l.MissingSince != nil {
			t.Errorf("missing_since not cleared: %v", l.MissingSince)
		}
	}
}

func TestDeleteSeriesRemovesRowAndTags(t *testing.T) {
	s := newStore(t)
	id, _ := s.UpsertSeries(model.Series{
		Title: "Y", SourcePath: "/d/Y", Source: "suwayomi",
		Type: model.TypeUnknown, Status: model.StatusPending,
	})
	if err := s.SetSeriesTags(id, []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSeries(id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSeriesByID(id); err == nil {
		t.Fatal("series still present after delete")
	}
	tags, _ := s.tagsForSeries(id)
	if len(tags) != 0 {
		t.Errorf("tags not cascaded: %v", tags)
	}
	// Idempotent.
	if err := s.DeleteSeries(id); err != nil {
		t.Errorf("second delete should be no-op, got %v", err)
	}
}
```

Confirm the fresh-`*Store` helper name by grepping `internal/store/store_test.go` (it may be `newStore`/`openTestStore`); use the real name in both tests.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd internal/store && go test -run 'TestSetSeriesMissingSince|TestDeleteSeries' -v`
Expected: FAIL — methods/`SeriesLite` undefined.

- [ ] **Step 3: Implement**

In `internal/store/store.go`, add the lite type and methods:

```go
// SeriesLite is the minimal projection the reconcile pass needs.
type SeriesLite struct {
	ID         int64
	SourcePath string
	Status     model.Status
	MissingSince *time.Time
}

// ListSeriesLite returns id, source_path, status, missing_since for every
// series. Cheap projection used by the reconcile pass.
func (s *Store) ListSeriesLite() ([]SeriesLite, error) {
	rows, err := s.db.Query(`SELECT id, source_path, status, missing_since FROM series`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeriesLite
	for rows.Next() {
		var l SeriesLite
		var status string
		var ms sql.NullInt64
		if err := rows.Scan(&l.ID, &l.SourcePath, &status, &ms); err != nil {
			return nil, err
		}
		l.Status = model.Status(status)
		if ms.Valid {
			t := time.Unix(ms.Int64, 0).UTC()
			l.MissingSince = &t
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SetSeriesMissingSince sets (or clears, when t is nil) series.missing_since.
func (s *Store) SetSeriesMissingSince(id int64, t *time.Time) error {
	if t == nil {
		_, err := s.db.Exec(`UPDATE series SET missing_since=NULL WHERE id=?`, id)
		return err
	}
	_, err := s.db.Exec(`UPDATE series SET missing_since=? WHERE id=?`, t.Unix(), id)
	return err
}

// SetSeriesStatus updates series.status.
func (s *Store) SetSeriesStatus(id int64, st model.Status) error {
	_, err := s.db.Exec(`UPDATE series SET status=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, string(st), id)
	return err
}

// DeleteSeries removes the series row and its tags. Idempotent: deleting a
// non-existent id is a no-op, not an error. Does NOT touch files on disk;
// file removal is the caller's concern (web binSeriesFiles).
func (s *Store) DeleteSeries(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM series_tags WHERE series_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM series WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}
```

If `series_tags` uses a different FK column than `series_id`, match `internal/store/migrations_tags.go`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd internal/store && go test -run 'TestSetSeriesMissingSince|TestDeleteSeries' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/store_test.go
git commit -m "feat(store): ListSeriesLite, SetSeriesMissingSince/Status, DeleteSeries"
```

---

## Task 4: Poller — the reconcile pass

**Files:**
- Create: `internal/poller/reconcile.go`
- Modify: `internal/poller/poller.go` (widen `SeriesStore`; call reconcile in `RunOnce`)
- Test: `internal/poller/reconcile_test.go`

The reconcile is pure decision logic over an injected store interface, so it tests without a real DB.

- [ ] **Step 1: Define the reconcile store seam + write the failing test**

Create `internal/poller/reconcile_test.go`:

```go
package poller

import (
	"testing"
	"time"

	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/store"
)

type fakeReconcileStore struct {
	lite       []store.SeriesLite
	missingSet map[int64]*time.Time
	statusSet  map[int64]model.Status
}

func (f *fakeReconcileStore) ListSeriesLite() ([]store.SeriesLite, error) { return f.lite, nil }
func (f *fakeReconcileStore) SetSeriesMissingSince(id int64, t *time.Time) error {
	f.missingSet[id] = t
	return nil
}
func (f *fakeReconcileStore) SetSeriesStatus(id int64, st model.Status) error {
	f.statusSet[id] = st
	return nil
}

func newFake(lite []store.SeriesLite) *fakeReconcileStore {
	return &fakeReconcileStore{lite: lite, missingSet: map[int64]*time.Time{}, statusSet: map[int64]model.Status{}}
}

func cfg() reconcileConfig {
	return reconcileConfig{Grace: 10 * time.Minute, MassVanishPercent: 25, MassVanishMinCount: 5}
}

// Present folder clears a previously-set missing_since.
func TestReconcileReappearClearsMissing(t *testing.T) {
	old := time.Unix(1000, 0).UTC()
	f := newFake([]store.SeriesLite{{ID: 1, SourcePath: "/d/A", Status: model.StatusOrphaned, MissingSince: &old}})
	onDisk := map[string]bool{"/d/A": true}
	now := time.Unix(2000, 0).UTC()
	res := reconcile(f, onDisk, cfg(), now)
	if res.Aborted {
		t.Fatal("should not abort")
	}
	if got, ok := f.missingSet[1]; !ok || got != nil {
		t.Errorf("missing_since not cleared: %v", got)
	}
	if f.statusSet[1] != model.StatusPending {
		t.Errorf("orphaned not restored to pending: %v", f.statusSet[1])
	}
}

// First absence sets missing_since; status unchanged within grace.
func TestReconcileFirstAbsenceSetsTimer(t *testing.T) {
	f := newFake([]store.SeriesLite{{ID: 1, SourcePath: "/d/A", Status: model.StatusPending}})
	now := time.Unix(2000, 0).UTC()
	res := reconcile(f, map[string]bool{}, cfg(), now)
	if res.Aborted {
		t.Fatal("should not abort (only 1 series, below min count)")
	}
	got := f.missingSet[1]
	if got == nil || !got.Equal(now) {
		t.Errorf("missing_since = %v, want %v", got, now)
	}
	if _, flipped := f.statusSet[1]; flipped {
		t.Error("status should not flip within grace")
	}
}

// Absence beyond grace flips to orphaned.
func TestReconcileGraceExceededFlipsOrphaned(t *testing.T) {
	old := time.Unix(1000, 0).UTC()
	f := newFake([]store.SeriesLite{{ID: 1, SourcePath: "/d/A", Status: model.StatusPending, MissingSince: &old}})
	now := old.Add(11 * time.Minute)
	reconcile(f, map[string]bool{}, cfg(), now)
	if f.statusSet[1] != model.StatusOrphaned {
		t.Errorf("status = %v, want orphaned", f.statusSet[1])
	}
}

// Zero-item scan with non-empty DB is skipped entirely.
func TestReconcileZeroScanSkips(t *testing.T) {
	f := newFake([]store.SeriesLite{{ID: 1, SourcePath: "/d/A", Status: model.StatusPending}})
	res := reconcile(f, map[string]bool{}, cfg(), time.Unix(2000, 0).UTC())
	// zeroScan guard is applied by the caller via res.Aborted path:
	if !res.SkippedZeroScan {
		t.Fatal("expected SkippedZeroScan when on-disk set empty and DB non-empty")
	}
	if len(f.missingSet) != 0 || len(f.statusSet) != 0 {
		t.Error("zero-scan reconcile must write nothing")
	}
}

// Mass vanish (> 25% AND >= 5) aborts and writes nothing.
func TestReconcileMassVanishAborts(t *testing.T) {
	var lite []store.SeriesLite
	for i := int64(1); i <= 10; i++ {
		lite = append(lite, store.SeriesLite{ID: i, SourcePath: "/d/" + string(rune('A'+i)), Status: model.StatusPending})
	}
	f := newFake(lite)
	// Only 2 of 10 still on disk → 8 vanish (>25% and >=5).
	onDisk := map[string]bool{"/d/" + string(rune('A'+1)): true, "/d/" + string(rune('A'+2)): true}
	res := reconcile(f, onDisk, cfg(), time.Unix(2000, 0).UTC())
	if !res.Aborted {
		t.Fatal("expected abort on mass vanish")
	}
	if len(f.missingSet) != 0 || len(f.statusSet) != 0 {
		t.Error("aborted reconcile must write nothing")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd internal/poller && go test -run TestReconcile -v`
Expected: FAIL — `reconcile`, `reconcileConfig`, `reconcileResult` undefined.

- [ ] **Step 3: Implement `internal/poller/reconcile.go`**

```go
package poller

import (
	"time"

	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/store"
)

// reconcileStore is the seam the reconcile pass writes through.
type reconcileStore interface {
	ListSeriesLite() ([]store.SeriesLite, error)
	SetSeriesMissingSince(id int64, t *time.Time) error
	SetSeriesStatus(id int64, st model.Status) error
}

type reconcileConfig struct {
	Grace              time.Duration
	MassVanishPercent  int
	MassVanishMinCount int
}

type reconcileResult struct {
	Aborted         bool // sanity floor tripped (mass vanish)
	SkippedZeroScan bool // on-disk set empty while DB non-empty
	Flagged         int  // newly orphaned this pass
}

// reconcile compares the DB series set (via store) against onDisk (set of
// source paths seen this scan) and applies the floor+grace rules. It writes
// NOTHING when the floor trips. now is injected for deterministic tests.
func reconcile(st reconcileStore, onDisk map[string]bool, cfg reconcileConfig, now time.Time) reconcileResult {
	lite, err := st.ListSeriesLite()
	if err != nil || len(lite) == 0 {
		return reconcileResult{}
	}

	// Sanity floor 1: a non-empty DB but an empty scan = the root is
	// unreachable/empty. Trust nothing.
	if len(onDisk) == 0 {
		return reconcileResult{SkippedZeroScan: true}
	}

	// Count newly-absent active series for the mass-vanish floor.
	active, newlyAbsent := 0, 0
	for _, l := range lite {
		if l.Status != model.StatusOrphaned {
			active++
		}
		if !onDisk[l.SourcePath] && l.Status != model.StatusOrphaned {
			newlyAbsent++
		}
	}
	// Sanity floor 2: mass vanish = > Percent% AND >= MinCount.
	overPct := active > 0 && newlyAbsent*100 > cfg.MassVanishPercent*active
	if overPct && newlyAbsent >= cfg.MassVanishMinCount {
		return reconcileResult{Aborted: true}
	}

	res := reconcileResult{}
	for _, l := range lite {
		present := onDisk[l.SourcePath]
		switch {
		case present:
			// Reappeared / still here: clear timer; restore orphaned→pending.
			if l.MissingSince != nil {
				_ = st.SetSeriesMissingSince(l.ID, nil)
			}
			if l.Status == model.StatusOrphaned {
				_ = st.SetSeriesStatus(l.ID, model.StatusPending)
			}
		case l.Status == model.StatusOrphaned:
			// Already orphaned and still gone: nothing to do.
		case l.MissingSince == nil:
			// First time seen absent: start the grace timer.
			t := now
			_ = st.SetSeriesMissingSince(l.ID, &t)
		case now.Sub(*l.MissingSince) >= cfg.Grace:
			// Absent past the grace window: flag orphaned.
			_ = st.SetSeriesStatus(l.ID, model.StatusOrphaned)
			res.Flagged++
		}
	}
	return res
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd internal/poller && go test -run TestReconcile -v`
Expected: PASS

- [ ] **Step 5: Wire reconcile into `RunOnce` + widen `SeriesStore`**

In `internal/poller/poller.go`, widen the `SeriesStore` interface (so `*store.Store` keeps satisfying `Poller.Store`) by adding the three reconcile methods:

```go
	ListSeriesLite() ([]store.SeriesLite, error)
	SetSeriesMissingSince(id int64, t *time.Time) error
	SetSeriesStatus(id int64, st model.Status) error
```

(Add `"github.com/gavinmcfall/mangarr/internal/store"` and `"time"` imports if not already present.)

In `RunOnce`, immediately after `series, err := p.Scanner.ScanAll()` succeeds and before the per-series loop, add:

```go
	// Reconcile: soft-delete series whose source folder vanished. Guarded
	// by the sanity floor; skipped entirely when Store/Settings are nil.
	if p.Store != nil && p.Settings != nil {
		set, serr := p.Settings.GetSettings()
		if serr == nil {
			onDisk := make(map[string]bool, len(series))
			for _, s := range series {
				onDisk[s.SourcePath] = true
			}
			cfg := reconcileConfig{
				Grace:              time.Duration(set.ReconcileGraceMinutes) * time.Minute,
				MassVanishPercent:  set.ReconcileMassVanishPercent,
				MassVanishMinCount: set.ReconcileMassVanishMinCount,
			}
			res := reconcile(p.Store, onDisk, cfg, time.Now())
			if res.Aborted {
				p.recordActivityVia("", model.ActionError, "reconcile",
					"reconcile aborted: mass series disappearance — likely a mount or Suwayomi failure, not real deletions")
			}
		}
	}
```

Confirm `model.ActionError` exists (it backs the existing upsert-error activity). If a more fitting action constant exists for system alerts, use it; otherwise `ActionError` is correct.

- [ ] **Step 6: Run the package + full build**

Run: `cd internal/poller && go test ./... && cd ../.. && go build ./...`
Expected: PASS / clean build (`p.Store` now requires the wider interface — `*store.Store` satisfies it).

- [ ] **Step 7: Commit**

```bash
git add internal/poller/reconcile.go internal/poller/reconcile_test.go internal/poller/poller.go
git commit -m "feat(poller): reconcile pass — soft-delete vanished series with floor+grace"
```

---

## Task 5: Web — durable delete route + file binning

**Files:**
- Create: `internal/web/series_delete.go`
- Modify: `internal/web/web.go` (register routes; widen the series store interface used by the handler)
- Test: `internal/web/series_delete_test.go`

`binSeriesFiles` walks the series' source dir (and, when resolvable, the Kavita dest dir) sending each file to the recycle bin, then removes the emptied dirs. `recyclebin.Send` rejects directories, so we send file-by-file.

- [ ] **Step 1: Write the failing test**

Create `internal/web/series_delete_test.go`:

```go
package web

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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
	// Both files now live under the bin's date dir.
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd internal/web && go test -run TestBinSeriesFiles -v`
Expected: FAIL — `binSeriesFiles` undefined.

- [ ] **Step 3: Implement `internal/web/series_delete.go`**

```go
package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gavinmcfall/mangarr/internal/recyclebin"
)

// binSeriesFiles sends every regular file under each dir to the recycle bin,
// then removes the now-empty dirs. recyclebin.Send rejects directories, so we
// recurse and send files individually. A dir that does not exist is a no-op
// (the common case: the source was already deleted upstream).
func binSeriesFiles(bin *recyclebin.Bin, dirs []string, now time.Time) error {
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		info, err := os.Stat(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			continue
		}
		walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			_, sendErr := bin.Send(path, now)
			return sendErr
		})
		if walkErr != nil {
			return walkErr
		}
		// Remove the emptied tree (RemoveAll tolerates leftover empty subdirs).
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	return nil
}

// apiSeriesDelete handles POST /api/series/{id}/delete.
// Form: delete_files=true|false. With delete_files=true, the series' source
// dir (and Kavita dest dir when resolvable) are sent to the recycle bin
// before the row is removed. Always durable: the DB row is deleted so the
// next scan cannot resurrect it (the folder is gone or about to be).
func (h *Handler) apiSeriesDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	series, err := h.store.GetSeriesByID(id)
	if err != nil {
		// Already gone — treat as success so the UI row clears.
		http.Redirect(w, r, "/series", http.StatusSeeOther)
		return
	}
	deleteFiles := r.FormValue("delete_files") == "true"
	if deleteFiles {
		if h.recycleBin == nil {
			http.Error(w, "recycle bin not configured", http.StatusServiceUnavailable)
			return
		}
		dirs := []string{series.SourcePath}
		if dst := h.resolveSeriesDestDir(r.Context(), id); dst != "" {
			dirs = append(dirs, dst)
		}
		if err := binSeriesFiles(h.recycleBin, dirs, time.Now()); err != nil {
			http.Error(w, "bin files: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if err := h.store.DeleteSeries(id); err != nil {
		http.Error(w, "delete series: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/series", http.StatusSeeOther)
}

// apiSeriesRestore handles POST /api/series/{id}/restore — clears the
// orphaned flag (back to pending) and the missing_since timer, for an
// intentional temporary source move.
func (h *Handler) apiSeriesRestore(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	_ = h.store.SetSeriesMissingSince(id, nil)
	_ = h.store.SetSeriesStatus(id, modelStatusPending())
	http.Redirect(w, r, "/series", http.StatusSeeOther)
}
```

`resolveSeriesDestDir` reuses the existing preview pipeline that already computes per-chapter `DstPath` (the #8 series-detail path via `PreviewOne`). Add it next to the handler:

```go
// resolveSeriesDestDir returns the Kavita library directory for a series by
// taking the parent dir of the first chapter plan from the previewer, or ""
// if the series cannot be planned (unmatched/misconfigured).
func (h *Handler) resolveSeriesDestDir(ctx context.Context, id int64) string {
	if h.previewer == nil {
		return ""
	}
	pe, err := h.previewer.PreviewOne(ctx, id)
	if err != nil || len(pe.ChapterPlans) == 0 {
		return ""
	}
	return filepath.Dir(pe.ChapterPlans[0].DstPath)
}
```

Add a tiny helper `modelStatusPending()` returning `model.StatusPending` (or inline `model.StatusPending` with the model import — prefer the direct import). Confirm the `Handler` field names `h.store`, `h.recycleBin`, `h.previewer` against `internal/web/web.go` (they were added in #8) and the `PlanEntry.DstPath` field name against `internal/filer`.

- [ ] **Step 4: Register the routes**

In `internal/web/web.go`, beside the existing `POST /api/series/{id}/chapter/remove` registration:

```go
	h.mux.HandleFunc("POST /api/series/{id}/delete", h.apiSeriesDelete)
	h.mux.HandleFunc("POST /api/series/{id}/restore", h.apiSeriesRestore)
```

Ensure the store interface the handler holds (`h.store`) includes `GetSeriesByID`, `DeleteSeries`, `SetSeriesMissingSince`, `SetSeriesStatus` — widen the web-side store interface in `web.go` to add the three new methods if it is a narrow interface rather than `*store.Store`.

- [ ] **Step 5: Run tests + build**

Run: `cd internal/web && go test -run TestBinSeriesFiles -v && cd ../.. && go build ./...`
Expected: PASS / clean build.

- [ ] **Step 6: Commit**

```bash
git add internal/web/series_delete.go internal/web/series_delete_test.go internal/web/web.go
git commit -m "feat(web): durable series delete with optional recycle-bin file removal"
```

---

## Task 6: UI — orphaned banner + two-step delete modal

**Files:**
- Modify: `internal/web/templates/series.html`
- Test: `internal/web/web_test.go` (render assertion)

Mirror the existing bulk-confirm modal pattern (`templates/bulk-confirm.html`) for the two-step delete.

- [ ] **Step 1: Write the failing render test**

Add to `internal/web/web_test.go` (adapt to the existing series-page render-test harness — grep for an existing `Test*Series*` render test and copy its setup):

```go
func TestSeriesPageShowsOrphanedBanner(t *testing.T) {
	// Arrange a handler whose store returns one series with Status=orphaned.
	// (Use the existing fakeStore used by other web tests; set the row's
	// Status to model.StatusOrphaned.)
	h := newTestHandlerWithSeries(t, model.Series{
		ID: 1, Title: "Gone", SourcePath: "/d/Gone", Status: model.StatusOrphaned,
	})
	body := getBody(t, h, "/series")
	if !strings.Contains(body, "Removed from source") {
		t.Error("orphaned banner copy missing")
	}
	if !strings.Contains(body, `/api/series/1/restore`) {
		t.Error("restore action missing")
	}
	if !strings.Contains(body, `/api/series/1/delete`) {
		t.Error("delete action missing")
	}
}
```

If no reusable `newTestHandlerWithSeries`/`getBody` helpers exist, build the handler the same way the nearest existing `/series` render test does and assert on the rendered string.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd internal/web && go test -run TestSeriesPageShowsOrphanedBanner -v`
Expected: FAIL — banner/markup absent.

- [ ] **Step 3: Add the markup to `series.html`**

In each series row, when `.Status` equals `orphaned`, render a banner. Add (templating syntax must match the file's existing `{{if eq .Status "..."}}` idioms):

```html
{{if eq (printf "%s" .Status) "orphaned"}}
<div class="series-orphaned-banner" role="alert">
  Removed from source — confirm removal?
  <form method="post" action="/api/series/{{.ID}}/restore" style="display:inline">
    <button type="submit" class="btn btn-secondary">Keep / Restore</button>
  </form>
  <button type="button" class="btn btn-danger"
          onclick="openDeleteModal({{.ID}}, {{printf "%q" .Title}})">Remove…</button>
</div>
{{end}}
```

Add the two-step modal + script once at the bottom of the page (single shared modal parameterised by id/title):

```html
<dialog id="delete-series-modal">
  <form method="post" id="delete-series-form" action="">
    <h3 id="delete-series-title"></h3>
    <p>How should this series be removed?</p>
    <label><input type="radio" name="delete_files" value="false" checked> Remove from Mangarr only (keep files on disk)</label>
    <label><input type="radio" name="delete_files" value="true"> Remove from Mangarr <strong>and delete files</strong> (downloads + Kavita)</label>
    <menu>
      <button type="button" onclick="document.getElementById('delete-series-modal').close()">Cancel</button>
      <button type="submit" id="delete-series-submit">Continue</button>
    </menu>
  </form>
</dialog>
<script>
function openDeleteModal(id, title) {
  const f = document.getElementById('delete-series-form');
  f.action = '/api/series/' + id + '/delete';
  document.getElementById('delete-series-title').textContent = 'Delete: ' + title;
  document.getElementById('delete-series-modal').showModal();
}
document.getElementById('delete-series-form').addEventListener('submit', function (e) {
  const wantFiles = this.querySelector('input[name=delete_files]:checked').value === 'true';
  if (wantFiles && !this.dataset.confirmed) {
    e.preventDefault();
    if (confirm('This will move the downloaded files AND the Kavita library copy to the recycle bin. Continue?')) {
      this.dataset.confirmed = '1';
      this.submit();
    }
  }
});
</script>
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd internal/web && go test -run TestSeriesPageShowsOrphanedBanner -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/web/templates/series.html internal/web/web_test.go
git commit -m "feat(web): orphaned series banner + two-step delete modal"
```

---

## Task 7: Full suite + manual smoke

**Files:** none (verification only)

- [ ] **Step 1: Run the whole suite under the local timezone**

Run: `go test ./...`
Expected: all packages PASS. (No `TZ=UTC` needed since the recyclebin GC timezone bug was fixed; but if a flake appears, re-run with `TZ=UTC go test ./...` and note it.)

- [ ] **Step 2: Build the binary**

Run: `go build ./...`
Expected: clean.

- [ ] **Step 3: Manual smoke checklist (record results in the PR)**

1. A series whose folder you remove on disk stays visible, then after the grace window shows the **"Removed from source — confirm removal?"** banner.
2. **Keep / Restore** clears the banner and the row returns to normal.
3. **Remove… → Mangarr only** deletes the row; it does **not** come back on the next scan.
4. **Remove… → and delete files** asks a second confirm, then moves the download + Kavita dirs to the recycle bin and deletes the row.
5. Simulate a mount blip (point the download root at an empty/again-full dir): the library is **not** mass-orphaned; an `reconcile aborted` activity entry appears.

- [ ] **Step 4: Commit any test-only fixups, then open the PR**

```bash
git add -A
git commit -m "test: series lifecycle reconciliation suite green"
```

(Stage only files this plan created/modified.)

---

## Self-Review (completed at authoring time)

- **Spec coverage:** A1 model → Tasks 1–2; A2 reconcile floor+grace → Task 4; A3 durable delete + recycle bin → Tasks 3,5; A4 UI banner/modal/restore → Task 6; A5 safety/tests → Tasks 4–7. All covered.
- **Type consistency:** `SeriesLite`, `reconcileConfig{Grace,MassVanishPercent,MassVanishMinCount}`, `reconcileResult{Aborted,SkippedZeroScan,Flagged}`, `binSeriesFiles(bin, dirs, now)`, `DeleteSeries(id)`, `SetSeriesMissingSince(id,*time.Time)`, `SetSeriesStatus(id,Status)` used identically across tasks.
- **Open verification points for the implementer (grep before coding, do not guess):** the fresh-DB/`*Store` test-helper names in `internal/store/*_test.go`; the `series_tags` FK column name; the `Handler` field names `h.store`/`h.recycleBin`/`h.previewer` and the previewer interface method `PreviewOne`; `filer.PlanEntry.DstPath`; the `series.html` conditional idiom. Each is a one-line grep; the plan notes where.
