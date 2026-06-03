# Stalled-Job Detector — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task.

**Goal:** A bulk job that stops making progress for any reason — a chapter Suwayomi gave up on, an unrecoverable source-side fault, or a clear `pageCount=0` "empty chapter" condition — must terminate on its own within a bounded window, with the chapter marked errored and the job auto-completing. No more "stuck at 239/240 for 9 hours".

**Architecture:** Extend the orchestrator's reconcile path (Plan A T10) with two new signals — a per-chapter age check on `fed` rows, and a Suwayomi-side queue-state probe — that escalate to `errored` after bounded retries. The job's terminal-state check (Plan A T12) already handles completion once `CompletedChapters + ErroredChapters >= TotalChapters`, so this plan only needs the path that bumps `ErroredChapters`.

**Tech Stack:** Go 1.25, `modernc.org/sqlite`, existing orchestrator goroutine, Suwayomi GraphQL.

---

## Real-World Trigger That Motivated This Plan

2026-06-02: Job #6 (Second Life Ranker, Bbato source) stalled at 239/240 for ~9 hours. Diagnosis chain:

1. `kubectl exec deploy/mangarr -- wget ... /api/bulk/jobs` → one running job not progressing
2. Local `sqlite3` on copied DB → chapter 3185 in state `fed` since 9h ago
3. Suwayomi GraphQL probe for chapter 3185 → `pageCount: 0, isDownloaded: false`
4. Suwayomi `downloadStatus` queue → `state: ERROR, tries: 3, progress: 0.0`
5. Suwayomi pod logs → `EmptyChapterException: Chapter does not have any pages to download` at `Downloader.kt:142`

Bbato lists Chapter 191 of Second Life Ranker in its index, but Suwayomi's `Bbato 1.4.1` extension returns zero image pages when asked for its page list. Manual verification: the chapter renders fine on bbato.com itself — **the failure is in the Tachiyomi extension's HTML selector, not in Bbato's data**. Neither retry nor backoff helps; the parser is stale until the extension maintainer ships a fix.

This generalises: the same observable state (`pageCount==0 + queueState=ERROR`) covers genuinely-empty chapters, parser-stale extensions, region-locked/premium chapters returning no pages anonymously, and any future failure mode where Suwayomi reaches the source successfully but extracts no pages. The detection signal is on observable Suwayomi state, not on a guess about the underlying cause — so the orchestrator handles all of these without needing to distinguish.

**The detection signal is unambiguous: `chapter.pageCount == 0` AND Suwayomi queue state is `ERROR` ⇒ chapter is unrecoverable from this source.**

---

## File Structure

- **`internal/suwayomi/suwayomi.go`** — extend with `GetChapterMeta(ctx, chapterID)` returning `{PageCount int; QueueState string; Tries int}` so the orchestrator can probe both signals in one round-trip. Existing `ListChapters` stays the read-it-all path; this new method is the targeted "is this one chapter stuck?" probe.
- **`internal/orchestrator/orchestrator.go`** — extend the reconcile pass with stall detection. New helpers: `detectStalledChapters(job)`, `markChapterErrored(job, chapterID, reason)`. The Tick loop calls `detectStalledChapters` once per job per tick, after the existing fed→done reconcile.
- **`internal/store/bulk.go`** — add `MarkBulkJobChapterErrored(jobID, chapterID int64, reason string) error` mirroring the existing `MarkBulkJobChapterDone`. Atomic: bumps `bulk_job_chapters.state='errored'`, increments `bulk_jobs.errored_chapters`, writes `last_error` for operator visibility.
- **`internal/store/migrations_bulk.go`** — Migration 5 adds `bulk_job_chapters.errored_reason TEXT` so the operator can see WHY a chapter was given up on without going to logs.
- **`internal/model/bulk.go`** — extend `BulkJobChapterState` enum with `ChapterStateErrored`. Existing `BulkJobChapter` struct gains `ErroredReason string`.
- **`internal/web/templates/bulk-row.html`** — display "N missing" next to the existing progress bar when a job completes with `ErroredChapters > 0`, so the operator sees what couldn't be fetched.
- **`internal/web/bulk.go`** — `apiBulkJobs` JSON shape gains `ErroredChapterDetails []{ChapterID,Reason}` when queried with `?include_errored=1`, for future Activity log enrichment.

---

## Configuration Knobs (Settings)

Add to `model.Settings`, with sensible defaults:

- `BulkStallTimeoutMinutes int` (default `30`) — a `fed` chapter that hasn't transitioned to `done` for this long is considered stalled.
- `BulkChapterMaxRetries int` (default `3`) — orchestrator's own retry counter; after this many re-feeds, the chapter is marked errored. Independent of Suwayomi's internal `tries` counter.
- `BulkAutoErrorEmptyChapters bool` (default `true`) — when `true`, a chapter with `pageCount==0` AND Suwayomi queue state `ERROR` is immediately marked errored without waiting for the stall timeout. When `false`, the orchestrator falls back to the stall timeout. Default-on because empty-chapter is unambiguous; toggle exists in case a source temporarily returns 0 pages and recovers (low confidence; we'll learn in production).

All three appear on the Settings page Bulk Download card next to the existing pacing knobs.

---

## Task Decomposition (Bite-Sized)

### Task 1: Migration 5 — `bulk_job_chapters.errored_reason` column

**Files:**
- Modify: `internal/store/migrations_bulk.go` — append `migration5BulkChapterErroredReason`
- Test: `internal/store/migrations_bulk_test.go` (existing pattern)

- [ ] **Step 1: Write the failing test** asserting that after `store.New()`, the `bulk_job_chapters` table has an `errored_reason` column.

```go
func TestMigration5AddsErroredReasonColumn(t *testing.T) {
    s, _ := newTestStore(t)
    var name, typ string
    found := false
    rows, _ := s.DB().Query(`PRAGMA table_info(bulk_job_chapters)`)
    defer rows.Close()
    for rows.Next() {
        var cid, notnull, pk int
        var dflt sql.NullString
        rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk)
        if name == "errored_reason" { found = true; break }
    }
    if !found {
        t.Fatal("migration 5 did not add bulk_job_chapters.errored_reason")
    }
}
```

- [ ] **Step 2: Run the test** — expect FAIL: column not found.
- [ ] **Step 3: Add the migration** to the migrations slice.

```go
{
    Version: 5,
    Name:    "bulk_chapter_errored_reason",
    Up: `ALTER TABLE bulk_job_chapters ADD COLUMN errored_reason TEXT NOT NULL DEFAULT '';`,
},
```

- [ ] **Step 4: Run test** — PASS.
- [ ] **Step 5: Commit.**

### Task 2: `model.ChapterStateErrored` + `BulkJobChapter.ErroredReason`

**Files:**
- Modify: `internal/model/bulk.go`
- Test: `internal/model/bulk_test.go`

- [ ] **Step 1: Failing test** asserts `model.ChapterStateErrored = "errored"` and the struct serialises `errored_reason`.
- [ ] **Step 2: Add the const + struct field.**
- [ ] **Step 3: Test passes.**
- [ ] **Step 4: Commit.**

### Task 3: Store — `MarkBulkJobChapterErrored`

**Files:**
- Modify: `internal/store/bulk.go`
- Test: `internal/store/bulk_test.go`

Method signature:

```go
func (s *Store) MarkBulkJobChapterErrored(jobID, chapterID int64, reason string) error
```

Atomic transaction:

```sql
BEGIN;
UPDATE bulk_job_chapters
   SET state='errored', errored_reason=?, updated_at=strftime('%s','now')
 WHERE job_id=? AND chapter_id=? AND state IN ('fed','pending');
UPDATE bulk_jobs
   SET errored_chapters=errored_chapters+1, last_error=?, updated_at=strftime('%s','now')
 WHERE id=?;
COMMIT;
```

Skip the update if the chapter is already `done` or `errored` (idempotent — a redundant detect-tick must not double-bump `errored_chapters`).

- [ ] **Step 1: Failing test** — seed a `fed` chapter, call MarkErrored, assert chapter state='errored', job.ErroredChapters=1, job.LastError matches.
- [ ] **Step 2: Second test** — call MarkErrored twice on the same chapter, assert ErroredChapters stays 1 (idempotent).
- [ ] **Step 3: Third test** — call MarkErrored on a `done` chapter, assert state stays `done` and ErroredChapters stays unchanged.
- [ ] **Step 4: Implement.**
- [ ] **Step 5: All three tests pass.**
- [ ] **Step 6: Commit.**

### Task 4: Settings — three new fields

**Files:**
- Modify: `internal/model/model.go` — add `BulkStallTimeoutMinutes`, `BulkChapterMaxRetries`, `BulkAutoErrorEmptyChapters`
- Modify: `internal/store/store.go` — `GetSettings`/`SaveSettings` round-trip the new fields
- Test: `internal/store/store_test.go`

Defaults applied at GetSettings read time if the JSON blob is missing the field (don't migrate; existing `KavitaLibIDsByType` follows the same pattern):

```go
if set.BulkStallTimeoutMinutes == 0 { set.BulkStallTimeoutMinutes = 30 }
if set.BulkChapterMaxRetries == 0 { set.BulkChapterMaxRetries = 3 }
// BulkAutoErrorEmptyChapters defaults to true at Settings page render time,
// not here — false-is-the-zero-value would be wrong; surface explicitly.
```

- [ ] **Step 1: Failing test** that GetSettings on a blank store returns the three defaults.
- [ ] **Step 2: Implement.**
- [ ] **Step 3: Test passes.**
- [ ] **Step 4: Commit.**

### Task 5: Suwayomi client — `GetChapterMeta`

**Files:**
- Modify: `internal/suwayomi/suwayomi.go`
- Test: `internal/suwayomi/suwayomi_test.go` with `httptest.Server`

GraphQL query:

```graphql
{
  chapter(id: $id) { pageCount isDownloaded }
  downloadStatus { queue { chapter { id } state tries progress } }
}
```

Return shape:

```go
type ChapterMeta struct {
    PageCount    int
    IsDownloaded bool
    QueueState   string   // "Queued" | "Running" | "ERROR" | "" (not in queue)
    Tries        int
}
```

- [ ] **Step 1: Test** spins up `httptest.Server` returning a fixed GraphQL response, asserts `GetChapterMeta` parses pageCount + queue match for the right chapterID.
- [ ] **Step 2: Test** the "not in queue" case — `QueueState=""`, `Tries=0`.
- [ ] **Step 3: Test** the "queue has multiple chapters" case — only the matching one returns its state.
- [ ] **Step 4: Implement.**
- [ ] **Step 5: All three tests pass with `-race`.**
- [ ] **Step 6: Commit.**

### Task 6: Orchestrator — `detectStalledChapters`

**Files:**
- Modify: `internal/orchestrator/orchestrator.go`
- Test: `internal/orchestrator/orchestrator_test.go`

Per running job, per tick (cheap — single SQL + one Suwayomi probe per stalled candidate):

```
1. SELECT chapter_id, updated_at FROM bulk_job_chapters
       WHERE job_id=? AND state='fed' AND updated_at < now - stall_timeout
2. For each candidate: call suwayomi.GetChapterMeta(chapterID)
3. Decide:
   a. PageCount==0 AND QueueState=='ERROR' AND BulkAutoErrorEmptyChapters
      → MarkBulkJobChapterErrored(reason="empty chapter (source returned 0 pages)")
   b. QueueState=='ERROR' AND Tries >= settings.BulkChapterMaxRetries
      → MarkBulkJobChapterErrored(reason=f"suwayomi gave up after {Tries} retries")
   c. QueueState in ('','Queued','Running') AND stalled >= stall_timeout
      → re-feed via EnqueueChapterDownloads; bump our own retry counter (tracked
        in bulk_job_chapters.tries column added in Task 7)
   d. Otherwise: leave alone, will detect again next tick.
```

- [ ] **Step 1: Failing test** — fakeStore with a job that has one chapter stuck in `fed` for >30 min; fakeSuwayomi returns PageCount=0, QueueState=ERROR. Orchestrator's next Tick should mark the chapter errored and the job completed.
- [ ] **Step 2: Failing test** — Suwayomi reports Tries=5; expect orchestrator to error the chapter with the "gave up after 5 retries" reason.
- [ ] **Step 3: Failing test** — chapter stalled but Suwayomi reports QueueState="Queued" with low tries → orchestrator re-feeds and bumps its own counter, does NOT mark errored.
- [ ] **Step 4: Implement `detectStalledChapters` + thread it into the existing Tick loop after fed→done reconcile.**
- [ ] **Step 5: All three tests pass; `go test ./... -race` passes.**
- [ ] **Step 6: Commit.**

### Task 7: Migration 6 — `bulk_job_chapters.tries`

**Files:**
- Modify: `internal/store/migrations_bulk.go`
- Test: same pattern as Task 1

Mangarr tracks its own re-feed count, independent of Suwayomi's `tries` (because Suwayomi resets on restart):

```sql
ALTER TABLE bulk_job_chapters ADD COLUMN tries INTEGER NOT NULL DEFAULT 0;
```

`MarkBulkJobChapterFed` (existing) bumps `tries`. `detectStalledChapters` reads it when deciding to mark errored vs re-feed.

- [ ] **Step 1: Failing test** for migration.
- [ ] **Step 2: Add migration.**
- [ ] **Step 3: Failing test** asserting `MarkBulkJobChapterFed` increments tries.
- [ ] **Step 4: Update MarkBulkJobChapterFed.**
- [ ] **Step 5: Tests pass.**
- [ ] **Step 6: Commit.**

### Task 8: Settings page — Bulk Download card extension

**Files:**
- Modify: `internal/web/templates/settings.html` — three new numeric/checkbox inputs in the existing Bulk Download card
- Modify: `internal/web/web.go` `apiSaveSettings` — parse and persist
- Test: `internal/web/settings_test.go` (existing pattern)

- [ ] **Step 1: Failing test** — POST /api/settings with the new fields, GET back, assert round-tripped.
- [ ] **Step 2: Add input rows.**
- [ ] **Step 3: Wire `strconv.Atoi`/`r.FormValue` parsing.**
- [ ] **Step 4: Tests pass.**
- [ ] **Step 5: Commit.**

### Task 9: Activity log + Downloads UI surfaces "N errored"

**Files:**
- Modify: `internal/web/bulk.go` — `bulkRowViewT` + `renderBulkRow` show errored count next to progress when > 0
- Modify: `internal/web/templates/bulk-row.html` — `{{if gt .ErroredChapters 0}}<span class="pill pill-error">{{.ErroredChapters}} missing</span>{{end}}`
- Activity log entry: `model.ActionBulkChapterErrored` with detail `chapter N — <reason>`, via `bulk:<provider>`
- Test: `internal/web/bulk_test.go`

- [ ] **Step 1: Add ActionBulkChapterErrored const.**
- [ ] **Step 2: Failing test** — orchestrator marks a chapter errored; assert one ActivityEntry was written with the right Via/Detail.
- [ ] **Step 3: Failing UI test** — a job with ErroredChapters=1 renders the pill.
- [ ] **Step 4: Wire the activity write into MarkBulkJobChapterErrored (or a wrapper) so detectStalledChapters writes it once per chapter (idempotent — check `tries` column).**
- [ ] **Step 5: Tests pass.**
- [ ] **Step 6: Commit.**

### Task 10: Integration test — full stall-to-completion path

**Files:**
- Modify: `internal/web/integration_test.go`

Scenario:
1. Seed a bulk_job with 2 chapters fed
2. Tick orchestrator with fakeSuwayomi reporting chapter 1 IsDownloaded=true, chapter 2 PageCount=0 + QueueState=ERROR
3. After one tick: chapter 1 → done, chapter 2 → errored, job → completed
4. Assert: GET /downloads shows the row with `1 missing` pill
5. Assert: GET /activity has `bulk-chapter-errored` entry

- [ ] **Step 1: Write the test.**
- [ ] **Step 2: Test passes end-to-end via real HTTP.**
- [ ] **Step 3: Commit.**

---

## Backwards Compatibility

- Existing jobs in `running` state when the new image deploys: the new Tick loop sees them on its first pass. Any chapters in `fed` longer than the (new) stall timeout get probed against Suwayomi immediately. Empty-chapter ones get errored on first pass; others get a fresh re-feed. **No manual intervention required on deploy.**
- Existing `bulk_job_chapters` rows have `tries=0` after migration 6 — orchestrator treats this as "fresh", which means a brief grace period before the first re-feed counts.

## What This Plan Does NOT Cover

Captured as future work, not in this scope:

- **Source-extension self-healing** (auto-disable a source after N empty-chapter errors in a row). Suwayomi has the data; mangarr could nudge by suspending bulk jobs against that source. Separate plan.
- **Per-source pacing overrides** (carried from PR #40). Still deferred.
- **Activity-log retention** (carried from PR #40).
- **Stalled-job dashboard widget** showing "Bbato dropped 47 chapters this month" — analytics, not detection.

---

## Self-Review

- [x] Spec coverage: real-world trigger has direct task mappings (Task 5 = probe, Task 6 = decide, Task 3 = persist)
- [x] No placeholders — all SQL and Go signatures are concrete
- [x] Type consistency — `ChapterMeta`, `MarkBulkJobChapterErrored`, `BulkAutoErrorEmptyChapters` names match across tasks
- [x] Idempotency called out explicitly in Task 3 (double-tick safety)
- [x] Backwards-compat documented for in-flight jobs at deploy

---

## Execution Handoff

**Recommended:** superpowers:subagent-driven-development (10 tasks, mostly independent, well-spec'd — ideal for fresh-subagent-per-task)

Alternative: superpowers:executing-plans for inline execution if you want to eyeball each task before moving on.
