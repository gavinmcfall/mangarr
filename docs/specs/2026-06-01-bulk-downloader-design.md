---
description: Safe-paced bulk-download orchestrator that lets the operator queue every missing chapter for one or many series from their Suwayomi library without triggering source bans
tags: [bulk-download, suwayomi, orchestrator, library, downloads]
audience: { human: 50, agent: 50 }
purpose: { design: 100, north-star: 0, gestalt: 0, reference: 0, research: 0, plan: 0, flow: 0, findings: 0, concepts: 0, high-agency-process: 0, low-agency-process: 0 }
---

# Bulk Downloader to Suwayomi — design

## Goal

Add a "Library" page that lists every series in the user's Suwayomi library and a "Downloads" page that shows a queue of in-flight bulk download jobs. The operator multi-selects N series on Library, clicks "Download Missing", and mangarr queues every undownloaded chapter into Suwayomi's download queue with safe pacing so the upstream source sites don't ban the user.

Trigger: Suwayomi itself warns operators with "Suwayomi is not a mass downloader and too many downloads can get you banned from sources and/or cause performance issues" before letting them dump 1,000+ chapters into its queue at once. Mangarr's role is to be the safety layer Suwayomi doesn't ship — a pacing orchestrator that feeds chapters into Suwayomi's queue 5 at a time, refilling when the in-flight count drops, with per-provider serialization across series so two MangaDex bulks don't run in parallel.

## Locked design decisions

All locked via `AskUserQuestion` during brainstorming, plus pre-brainstorm context from the operator:

| Decision | Choice |
|---|---|
| Pause behaviour | Stop feeding; let in-flight Suwayomi-queue items finish naturally. No dequeue mutation. |
| Restart recovery | Auto-resume running jobs on boot; leave paused jobs paused. |
| Provider rate-limit key | Suwayomi's `sourceId` (per extension). Refine to underlying-host later only if cross-language bans observed. |
| Library page count load | Lazy per-row via HTMX. `library_cache` table; refreshed on Sync click or after a bulk job completes. |
| Activity log integration | Per-job lifecycle entries only (Started/Paused/Resumed/Completed/Errored). NOT per chapter. Bulk Download page is the per-chapter source of truth. |
| Persistence | Bulk jobs + per-chapter state survive restart. New SQLite tables. |
| Concurrency | One bulk job per (series, provider) at a time. Multi-select of 12 series = 12 BulkJob rows; orchestrator serializes by `source_id`. |
| Pacing knobs | Configurable in Settings. |
| "Missing" semantics | `isDownloaded: false`. No re-download mode. |
| Backoff | HTTP 429 → exponential 5s / 15s / 60s / 5min, then mark errored. |

## Architecture

### Boundary

mangarr stays a single-process binary. No new deployment. The bulk-download orchestrator runs as a goroutine inside `cmd/mangarr` alongside the existing poller — same pattern, same lifecycle.

### Data model — three new tables (Migration 4)

```sql
CREATE TABLE bulk_jobs (
  id                    INTEGER PRIMARY KEY AUTOINCREMENT,
  manga_id              INTEGER NOT NULL,         -- Suwayomi numeric manga ID
  source_id             TEXT    NOT NULL,         -- Suwayomi numeric source ID (= per-provider lock key)
  title                 TEXT    NOT NULL,         -- snapshot at creation for display
  source_name           TEXT    NOT NULL,         -- snapshot at creation for display
  status                TEXT    NOT NULL,         -- 'pending' | 'running' | 'paused' | 'completed' | 'errored'
  total_chapters        INTEGER NOT NULL DEFAULT 0,
  completed_chapters    INTEGER NOT NULL DEFAULT 0,
  errored_chapters      INTEGER NOT NULL DEFAULT 0,
  last_error            TEXT,                     -- last 429/network/auth message (truncated)
  backoff_until         INTEGER,                  -- unix ts; orchestrator skips job until passed
  consecutive_failures  INTEGER NOT NULL DEFAULT 0,
  created_at            INTEGER NOT NULL DEFAULT (strftime('%s','now')),
  updated_at            INTEGER NOT NULL DEFAULT (strftime('%s','now'))
);
CREATE INDEX idx_bulk_jobs_status_source ON bulk_jobs(status, source_id, created_at);

CREATE TABLE bulk_job_chapters (
  job_id      INTEGER NOT NULL REFERENCES bulk_jobs(id) ON DELETE CASCADE,
  chapter_id  INTEGER NOT NULL,                   -- Suwayomi numeric chapter ID
  state       TEXT    NOT NULL,                   -- 'pending' | 'fed' | 'done' | 'errored'
  updated_at  INTEGER NOT NULL DEFAULT (strftime('%s','now')),
  PRIMARY KEY (job_id, chapter_id)
);
CREATE INDEX idx_bulk_job_chapters_state ON bulk_job_chapters(state, job_id);

CREATE TABLE library_cache (
  manga_id          INTEGER PRIMARY KEY,          -- Suwayomi numeric manga ID
  title             TEXT    NOT NULL,
  source_id         TEXT    NOT NULL,
  source_name       TEXT    NOT NULL,
  total_chapters    INTEGER NOT NULL,
  downloaded        INTEGER NOT NULL,
  refreshed_at      INTEGER NOT NULL DEFAULT (strftime('%s','now'))
);
```

### HTTP surface

```
GET  /library                          page (HTML, full Suwayomi library)
GET  /api/library/sync                 HTMX action: re-fetch from Suwayomi, swap table
GET  /api/library/{mangaId}/missing    HTMX fragment: count badge for one row
POST /api/bulk                         creates N BulkJob rows from multi-select; returns confirmation modal if confirm=0, redirects to /downloads if confirm=1
GET  /downloads                        page (HTML, queue dashboard)
GET  /api/downloads/list               HTMX fragment: queue rows (polled every 3s)
POST /api/downloads/{id}/pause         status: running → paused
POST /api/downloads/{id}/resume        status: paused | errored → running; resets consecutive_failures + backoff_until
POST /api/downloads/{id}/delete        removes row (in-flight Suwayomi chapters complete naturally)
```

### Live updates

HTMX `hx-trigger="every 3s"` on the queue dashboard `<tbody>`. No WebSockets, no SSE. Matches the existing Activity page polling pattern.

### Concurrency invariants

- **At most one bulk_job in `(status='running' AND in_flight > 0)` per `source_id` at a time.** The 2s tick's group-by enforces this naturally; no row-level lock needed.
- **The orchestrator tick is single-goroutine and sequential.** No fan-out within a tick.
- **UI mutations are SQL UPDATEs only.** Pause/Resume/Delete don't talk to Suwayomi directly. The orchestrator picks state up on the next tick.

## Suwayomi GraphQL surface

All four new client methods land in `internal/suwayomi/suwayomi.go` next to the existing `LibraryWithCategories` query. Auth reuses the 4-mode wiring already there.

### 1. ListChapters

```graphql
query ChaptersForManga($mangaId: Int!) {
  chapters(condition: {mangaId: $mangaId}) {
    nodes {
      id
      name
      chapterNumber
      isDownloaded
      sourceOrder
    }
  }
}
```

Returns full chapter list per series. Caller filters to `isDownloaded=false` for "missing chapters" semantics.

### 2. EnqueueChapterDownloads

```graphql
mutation EnqueueChapterDownloads($ids: [Int!]!) {
  enqueueChapterDownloads(input: {ids: $ids}) {
    clientMutationId
  }
}
```

Batched per-tick. Orchestrator picks `batch_size` (default 5) chapter IDs in `state='pending'`, fires this once, flips those rows to `state='fed'`.

### 3. GetDownloadStatus

```graphql
query DownloadStatus {
  downloadStatus {
    state
    queue {
      chapter { id mangaId }
      manga { source { id } }
      state         # QUEUED | DOWNLOADING | FINISHED | ERROR
      progress
      tries
    }
  }
}
```

Per tick, mangarr counts queue entries where `manga.source.id == job.source_id AND state IN (QUEUED, DOWNLOADING)`. That's the in-flight count gating the refill decision.

### 4. Per-chapter completion check

Two ways to detect "chapter N finished downloading":

| Method | Issue |
|---|---|
| Watch `downloadStatus.queue` for FINISHED/ERROR entries | Suwayomi removes finished chapters from the queue after a short window — a chapter that finished between our 2s ticks would just disappear. **Race-prone.** |
| Re-query `chapters(condition:{mangaId}).isDownloaded` | Robust. The chapter list is the source of truth. |

**Use the second method.** The orchestrator does both per tick — `downloadStatus` for in-flight gating, `chapters.isDownloaded` for completion.

### Error classification

| Suwayomi state | Mangarr action |
|---|---|
| FINISHED (or `isDownloaded=true` next tick) | `bulk_job_chapters.state='done'` |
| ERROR with `tries < 3` | leave `fed`; Suwayomi retries internally |
| ERROR with `tries ≥ 3` | `state='errored'`; bump `bulk_jobs.consecutive_failures` |
| HTTP 429 from Suwayomi | `backoff_until = now + ladder[consecutive_failures]` on affected job |
| Auth expired (401) | existing JWT refresh runs; retry next tick |
| Suwayomi unreachable | skip tick, no state change |

### Caveat: schema drift

Suwayomi's GraphQL schema has drifted across versions. The implementer must verify exact field shapes (`condition` vs `filter` syntax, queue's `state` enum values) against the running instance's `/api/graphql` introspection before writing the queries. Plan B Task 8 of Library Bindings v2 surfaced a similar mismatch where field naming had changed.

## Orchestrator state machine

### Job lifecycle

```
                    POST /api/bulk
                          │
                          ▼
                      ┌───────┐
              ┌──────►│pending│
              │       └───┬───┘
              │           │ first tick (no backoff)
              │           ▼
              │      ┌─────────┐  pause clicked   ┌──────┐
              │      │ running │ ───────────────► │paused│
              │      └────┬────┘ ◄─────────────── └──────┘
              │           │       resume clicked
              │           │
   delete ────┤           ├─► all chapters done ──► ┌─────────┐
   removes    │           │                         │completed│
   row        │           │                         └─────────┘
              │           │
              │           └─► consecutive_failures > 5  ──► ┌───────┐
              │                OR all chapters errored      │errored│
              │                                             └───────┘
              ▼
            (gone)
```

### Per-chapter lifecycle

Rows in `bulk_job_chapters`:

```
pending  → fed       (orchestrator enqueueChapterDownloads call)
fed      → done      (chapter.isDownloaded=true on next tick)
fed      → errored   (queue.state=ERROR and tries ≥ 3)
fed      → pending   (job paused mid-flight; chapter wasn't completed yet —
                      drops back to pending so resume re-feeds it)
```

The pending→fed→pending demotion on pause is the cleanup that prevents ghost-fed chapters — if pause happens between feed and Suwayomi-completion, we'd otherwise have rows stuck in `fed` forever.

### Tick loop (every 2s, sequential, no fan-out)

```
def tick():
    jobs = load_jobs(status='running' AND backoff_until <= now)
    by_source = group(jobs, key='source_id')

    for source_id, candidates in by_source.items():
        job = candidates.sorted_by('created_at').first()    # FIFO per source

        in_flight = count_in_flight_for_source(source_id)
        if in_flight > refill_threshold:
            continue

        reconcile_fed_chapters(job)                          # done/errored transitions

        if all_chapters_done(job):     mark_completed(job); continue
        if too_many_errors(job):       mark_errored(job);   continue

        next_ids = pick_pending_chapter_ids(job, limit=batch_size)
        try:
            client.EnqueueChapterDownloads(next_ids)
            flip_chapters_to_fed(next_ids)
            job.consecutive_failures = 0
        except HTTP_429:               bump_backoff(job)
        except connection_refused:     continue              # try next tick
```

### Boot recovery

```sql
UPDATE bulk_job_chapters SET state='pending' WHERE state='fed';
```

Runs once on `Store.Open()` before the orchestrator's first tick. The first tick will reconcile via `isDownloaded` anyway, but this is cheap insurance against a crash mid-feed. Paused jobs stay paused. Pending jobs stay pending (next tick picks them up). Running jobs resume.

### Backoff ladder

Per-job, on HTTP 429 from Suwayomi:

| `consecutive_failures` after this failure | `backoff_until` set to |
|---|---|
| 1 | now + 5s |
| 2 | now + 15s |
| 3 | now + 60s |
| 4 | now + 300s |
| 5 | mark job `errored`, set `last_error` |

Any successful chapter feed resets `consecutive_failures = 0`. So a job that hits one 429 then recovers stays alive.

## UI shape

Two new pages, plus a confirmation modal and a Settings card.

### `/library` — Library page

Sidebar entry "Library" between "Series" and "Preview".

```
Library                                              [Sync]  [Download Missing (3)]
What's in your Suwayomi library. Click rows to select, then bulk-download missing chapters safely.

┌──┬─────────────────────────────────┬──────────────┬──────┬─────┬────────────┬────────┐
│☐ │ Title                           │ Source       │ Total│ Got │ Missing    │ Status │
├──┼─────────────────────────────────┼──────────────┼──────┼─────┼────────────┼────────┤
│☒ │ One Piece                       │ MangaDex EN  │ 1076 │   0 │ 1076 [pill]│        │
│☒ │ Solo Leveling                   │ MangaDex EN  │  200 │  47 │  153 [pill]│ running│
│☐ │ The Beginning After the End    │ Mangapark    │  280 │   1 │  279 [pill]│        │
│☒ │ Dragon Ball Super (Color)       │ MangaDex EN  │  104 │ 104 │    0       │   done │
│☐ │ The Infinite Mage               │ Mangapark    │  167 │ 167 │    0       │   done │
└──┴─────────────────────────────────┴──────────────┴──────┴─────┴────────────┴────────┘

         12 series total · 3 selected · 1,229 missing across selection
```

- Each row HTMX-loads its own Total/Got/Missing on render. Placeholder badge `…` until the fragment returns.
- Status column reflects the most-recent bulk-job state for that series (running/paused/done/errored) if one exists; else blank. If a series has multiple historical jobs, the row shows the latest by `created_at`.
- Sync button: `POST /api/library/sync` → re-fetch library from Suwayomi, repopulate `library_cache` table, swap table body.
- Multi-select via `<input type="checkbox" name="manga_id" value="…">` in a form. Bulk action button submits to `POST /api/bulk` with `confirm=0`.
- Header counter updates client-side as you toggle.

### `/downloads` — Bulk Download queue dashboard

Sidebar entry "Downloads" after "Library".

```
Downloads                                                            [Active ⨂] [All]
Bulk download queue. Mangarr paces these per-provider to avoid bans.

┌─────────────────────────────────┬──────────────┬─────────┬─────────────────┬──────────┬────────────────────────┐
│ Series                          │ Source       │ Status  │ Progress        │ Last     │ Actions                │
├─────────────────────────────────┼──────────────┼─────────┼─────────────────┼──────────┼────────────────────────┤
│ One Piece                       │ MangaDex EN  │ running │ ████░░ 412/1076 │   1m ago │ [pause] [delete]       │
│ Solo Leveling                   │ MangaDex EN  │ pending │   waiting       │   1m ago │           [delete]     │
│ The Beginning After the End    │ Mangapark    │ running │ ██░░░░  82/279  │   3s ago │ [pause] [delete]       │
│ Mashle                          │ MangaDex EN  │ paused  │ █░░░░░  15/162  │   2h ago │ [resume] [delete]      │
│ Naruto                          │ MangaDex EN  │  done   │ ██████ 700/700  │   1d ago │           [delete]     │
└─────────────────────────────────┴──────────────┴─────────┴─────────────────┴──────────┴────────────────────────┘
```

- Tabs: "Active" (running / paused / pending / errored) | "All" (above + completed). Filter is a query param `?filter=active|all`.
- The `<tbody>` element has `hx-get="/api/downloads/list?filter=active" hx-trigger="every 3s" hx-swap="outerHTML"`. HTMX gates polling on tab visibility.
- Progress bar = `completed_chapters / total_chapters`. Green for running, yellow for paused, blue for done, red for errored.
- "Last" column shows `updated_at` as relative time.
- Action clicks are HTMX-driven; response swaps just that row.
- Solo Leveling sits at "pending" because there's already a running job for MangaDex EN; the per-source serialization shows as "waiting" implicitly. No special UI for the lock — operator just sees it pick up after One Piece's slot drains.

### Confirmation modal (triggered from `/library`'s "Download Missing")

Server-rendered HTML returned by `POST /api/bulk` with `confirm=0`:

```
┌─────────────────────────────────────────────────────────────────┐
│ Bulk download                                                   │
│                                                                 │
│ You're about to queue 1,229 chapters across 3 series and 2     │
│ providers.                                                      │
│                                                                 │
│   • MangaDex EN — 2 series · 1,229 chapters                    │
│   • Mangapark — 1 series · 279 chapters                        │
│                                                                 │
│ Mangarr will pace 5 chapters in flight per provider, refilling │
│ when down to 2. Suwayomi's per-chapter delay still applies on  │
│ top.                                                            │
│                                                                 │
│ Different providers download in parallel. Same provider runs   │
│ one series at a time.                                           │
│                                                                 │
│                                  [Cancel]  [Queue downloads]   │
└─────────────────────────────────────────────────────────────────┘
```

The "Queue downloads" button POSTs the same form with `confirm=1`, which creates the BulkJob rows and 303-redirects to `/downloads`.

### Settings — new "Bulk Download" card

Placed below the existing Suwayomi Connection card.

```
Bulk Download
Per-provider pacing for the safe-mass-download feature on the Library page.

  Max in-flight per provider          [  5 ]
  Refill threshold                    [  2 ]
  Inter-batch delay (seconds)         [  1 ]

  Backoff ladder on Suwayomi HTTP 429: 5s → 15s → 60s → 5min, then mark errored.
```

Three numeric inputs. Backoff ladder stays hardcoded — uncommon to need to tune; if cases arise, surface in v3.1.

### Empty states

- **No Suwayomi configured** on `/library`: "Configure Suwayomi in Settings to use Library."
- **No series in Suwayomi library** on `/library`: "Your Suwayomi library is empty. Add series via Suwayomi first."
- **No jobs** on `/downloads`: "No bulk downloads. Start one from the Library page."
- **All selected series fully downloaded** on confirmation: "All selected series are fully downloaded — nothing to do."

## Error handling, recovery, edge cases

### Suwayomi unreachable / network blip

Orchestrator does nothing. No state change. The tick simply skips. Operator sees a stale "Last update" timestamp; once Suwayomi comes back, the orchestrator picks up.

No surfacing in the UI for transient errors — would just spam. Only persistent failures (`consecutive_failures ≥ 3`) bubble up via `bulk_jobs.last_error`.

### Suwayomi auth expired mid-job

Existing client handles JWT refresh transparently. If refresh itself fails (e.g. password change), treat as 429:

```
- Bump consecutive_failures
- Set backoff_until per ladder
- Set last_error = "auth refresh failed: <reason>"
- Job continues at next tick; client retries auth
```

After 5 consecutive auth failures, job goes to `errored`. Operator fixes Suwayomi credentials in Settings, clicks Resume, which clears `consecutive_failures` and `backoff_until` and sets status back to `running`.

### User pauses mid-feed

Chapters in `state='fed'` that haven't completed get demoted to `pending` on pause. Suwayomi will continue downloading the already-queued ones in its own queue (those become "ghost" — finished downloading but not tracked by mangarr's job state since they were demoted). On resume, the orchestrator re-feeds them; `enqueueChapterDownloads` is idempotent for already-queued chapters in Suwayomi.

### User deletes a job mid-feed

`DELETE FROM bulk_jobs WHERE id=?` (ON DELETE CASCADE clears `bulk_job_chapters`). Orchestrator won't pick the job up next tick. Any chapters already in Suwayomi's queue continue downloading — files land in `/media/Downloads/suwayomi/<title>/` and mangarr's existing poller picks them up normally.

Delete button on `/downloads` shows confirmation if `status='running'`: "X chapters are currently downloading via Suwayomi. They'll continue but won't be tracked here. Delete anyway?"

### Series removed from Suwayomi library mid-job

`enqueueChapterDownloads` on a chapter for a no-longer-in-library manga returns an error. Treated as a regular feed failure (`consecutive_failures` increments, ladder applies, eventually `errored` with `last_error="manga no longer in Suwayomi library"`).

Recovery: re-add to Suwayomi + Resume, OR delete the job.

### Chapter list changed mid-job

Long-running bulk job: One Piece queued at 1,076 chapters. Three days in, chapter 1,077 publishes. **Mangarr does NOT discover it** — the bulk job's chapter list is a snapshot taken at creation. The new chapter falls under mangarr's normal poller flow (or the next bulk job).

This is intentional. Re-scoping a running job is complex and rarely what users want; a fresh bulk job is the right answer.

### Suwayomi's queue stuck (in-flight never drops)

A job running for 10+ minutes with no `completed_chapters` movement. Rare. v3.0 has no stalled-job detector — that's v3.1 scope. Operator can Pause → Resume to kick things, or inspect Suwayomi's own queue page.

### Pod OOM'd / SIGKILL mid-tick

Same as boot recovery. Tick is non-transactional across the network call — `enqueueChapterDownloads` may have fired but the local `state='fed'` flip didn't land. On boot the chapters look pending again, get re-fed, and Suwayomi's queue is idempotent so this is harmless; the chapters complete and `isDownloaded=true` flips them to `done` next tick.

### Concurrent UI mutations

Last write wins. Two tabs both clicking pause → second is a no-op. No race condition.

### Settings change mid-job

Operator lowers max-in-flight from 5 to 3. Next tick reads the new value. If 4 chapters were already fed, they continue (orchestrator only checks `in_flight ≤ refill_threshold` before feeding NEW chapters). Refill stays gated at the new lower threshold from now on.

### Bulk job count

v3.0 has no per-operator cap on number of bulk jobs. A user could create 100 jobs via multi-select. Per-source serialization handles correctness — only one runs per source at a time; the rest sit at `pending`.

### Activity log space

Per-job lifecycle entries means at most ~5 entries per bulk job. A 50-job bulk session = 250 entries. Existing activity log has no retention policy; adding one is out of scope for this feature.

## Plan split

Two independently-reviewable batches ship together as v3.0. A third is genuinely future scope.

### Plan A — Foundation (server-side, no UI)

Everything needed to run a bulk download via curl + read state via JSON.

**Files / packages:**
- `internal/model/bulk.go` — new `BulkJob` + `BulkJobChapter` + `LibraryCacheEntry` types
- `internal/store/migrations.go` — Migration 4 (3 new tables) + boot-recovery sweep
- `internal/store/bulk.go` — CRUD for BulkJob + BulkJobChapter
- `internal/suwayomi/suwayomi.go` — 4 new client methods + Chapter/DownloadStatus types
- `internal/orchestrator/` — new package: tick loop, state machine, backoff ladder, per-source serialization
- `internal/web/web.go` — 5 JSON endpoints (`POST /api/bulk`, `GET /api/bulk/jobs`, `POST /api/bulk/jobs/{id}/{pause,resume,delete}`)
- `internal/model/settings.go` — 3 new fields (`BulkMaxInFlight`, `BulkRefillThreshold`, `BulkInterBatchDelaySec`) with defaults

**Truth statements (EARS-style):**
- Migration 4 shall create `bulk_jobs`, `bulk_job_chapters`, and `library_cache` tables.
- `Store.Open` shall run `UPDATE bulk_job_chapters SET state='pending' WHERE state='fed'` before the orchestrator's first tick.
- `Orchestrator.Tick` shall run every 2 seconds and shall be idempotent on transient errors.
- For each `source_id` present in running jobs, exactly one job shall be eligible to feed chapters per tick.
- A job's `consecutive_failures` shall reset to 0 on any successful chapter feed.
- When `consecutive_failures` reaches 5, a job's `status` shall be set to `errored` (no further backoff is set).
- `POST /api/bulk` shall accept `{manga_ids: [int64]}` and create one `BulkJob` per manga_id, populating chapter rows from `ListChapters(manga_id)` filtered to `isDownloaded=false`.
- Resume from `errored` shall reset `consecutive_failures=0`, `backoff_until=NULL`, `status='running'`.

**Verification:** unit tests for the orchestrator tick (`fakeSuwayomi` + table-driven state transitions); store CRUD round-trip tests; an integration test that runs 3 ticks against a fake Suwayomi.

### Plan B — UI (Library page + Downloads dashboard)

Wraps Plan A's JSON endpoints in HTML.

**Files / packages:**
- `internal/web/templates/library.html` — new page
- `internal/web/templates/downloads.html` — new page
- `internal/web/templates/bulk-row.html` — partial for HTMX row swaps
- `internal/web/templates/bulk-confirm.html` — confirmation modal partial
- `internal/web/templates/library-row-counts.html` — partial for lazy count badges
- `internal/web/templates/base.html` — sidebar entries
- `internal/web/templates/settings.html` — new Bulk Download card
- `internal/web/web.go` — page handlers + HTMX fragment endpoints + a typed page-data struct
- `internal/web/static/mangarr.css` — `.bulk-progress`, `.pill-paused`, `.pill-errored` styling

**Truth statements:**
- `/library` shall render every manga in the Suwayomi library with placeholder badges replaced via HTMX per-row.
- `/api/library/sync` shall re-fetch from Suwayomi and update `library_cache`.
- A checkbox-driven form on `/library` shall POST to `/api/bulk` and trigger the confirmation modal.
- `/downloads` shall render the bulk-job queue with a 3-second HTMX poll on the `<tbody>`.
- Pause/Resume/Delete actions on `/downloads` shall HTMX-swap just the affected row.
- The Settings page shall include a "Bulk Download" card exposing the 3 pacing knobs from Plan A.
- All 4 empty states shall render with the documented copy.

**Verification:** rendered-HTML assertion tests; a Playwright screenshot of `/library` with multi-select active + the confirmation modal open.

### Plan C — Polish (v3.1, not blocking)

- **Stalled-job detector** — orchestrator detects a job with no `completed_chapters` movement for N minutes (default 10), flags `bulk_jobs.last_error="stalled — Suwayomi queue appears stuck"`, and pauses the job.
- **Per-operator job count cap** — Settings field "Max active bulk jobs" (default unlimited).
- **Activity log retention policy** — separate from bulk download but related.
- **Per-source backoff overrides** — Settings table for known-hostile sources with custom backoff ladders.

### Out of scope (won't ship in any v3.x plan)

- Tranga bulk download (Tranga has no GraphQL API for this).
- Re-download mode (overwrite existing files).
- Per-chapter pause (only job-level).
- Cross-job dependencies / chaining.
- Multi-source bindings on a single job.
- WebSocket/SSE live updates.
- Background workers / separate deployment.

## Testing approach

### Unit tests

**Orchestrator (`internal/orchestrator/orchestrator_test.go`):**
- `TestTickPicksOneJobPerSource` — 2 jobs same source_id → only first FIFO feeds
- `TestTickRunsDifferentSourcesInParallel` — 2 jobs different source_ids → both feed
- `TestTickSkipsWhenInFlightAboveThreshold` — in_flight=10, threshold=2 → no feed
- `TestTickReconcilesFedToDoneOnIsDownloaded` — chapter flips fed→done on next tick
- `TestTickReconcilesFedToErroredOnExhaustedTries` — tries≥3 → state='errored'
- `TestBackoffLadderProgresses` — 5× HTTP 429 → ladder hits each rung, 5th marks errored
- `TestConsecutiveFailuresResetOnSuccess` — 3× 429 then success → consecutive_failures=0
- `TestPauseDemotesFedToPending`
- `TestAllChaptersDoneMarksCompleted`
- `TestSuwayomiUnreachableNoStateChange` — connection refused → no DB writes

**Store (`internal/store/bulk_test.go`):**
- `TestUpsertBulkJobRoundTrip`
- `TestListBulkJobsByStatus`
- `TestBulkJobChaptersCascadeDeleteOnJobDelete`
- `TestMigration4CreatesThreeTables`
- `TestMigration4BootRecoveryFlipsFedToPending`
- `TestUpdateBulkJobStatusPreservesUpdatedAt`
- `TestSaveLibraryCacheUpsertSourceIdKey`

**Suwayomi client (`internal/suwayomi/suwayomi_test.go`):**
- `TestListChaptersFiltersByMangaId`
- `TestEnqueueChapterDownloadsBatch` — stub captures mutation body
- `TestGetDownloadStatusGroupsBySource`
- `TestSuwayomiAuthExpiredRetriesAfterRefresh`
- `TestSuwayomiHTTP429ReturnsTypedError`

### Integration tests

In `internal/orchestrator/` against `fakeSuwayomi` + real `*store.Store`:

- `TestOrchestratorEndToEndOneJob` — seed manga+chapters → POST /api/bulk → 5 ticks → all chapters complete → status='completed'
- `TestOrchestratorEndToEndPerSourceSerialization` — 2 jobs same source → first runs to completion before second starts
- `TestOrchestratorBootRecoveryResumesRunning` — seed running job + 3 fed chapters → Open() + first tick → chapters re-fed, eventually complete

### Web tests (rendered-HTML)

- `TestLibraryPageRendersAllMangaWithPlaceholderBadges`
- `TestLibraryPageHasMultiSelectFormAndSubmitButton`
- `TestLibraryRowCountsFragmentReturnsCounts`
- `TestLibrarySyncRefetchesFromSuwayomi`
- `TestBulkPreviewReturnsConfirmationModal`
- `TestBulkConfirmedCreatesNJobsForNMangaIds`
- `TestBulkConfirmedRejectsDeletedMangaIds`
- `TestDownloadsPageRendersJobsWithProgressBars`
- `TestDownloadsListFragmentFiltersActiveVsAll`
- `TestDownloadActionsPauseResumeDeleteSwapsRow`
- `TestSettingsBulkDownloadCardRoundTripsPacingKnobs`
- `TestEmptyStatesAllFour`

### Production verification

Post-deploy smoke checklist:

1. Pod boots, log shows `store: applied migration 4 "bulk-downloads-tables"`
2. `/library` page loads, shows full Suwayomi library
3. Click Sync — placeholder badges flip to real counts within ~30s
4. Multi-select 2 small series (different sources, <20 chapters each), click Download Missing
5. Confirmation modal shows correct counts + per-provider breakdown
6. Click Queue downloads — `/downloads` shows 2 jobs running in parallel
7. Wait for completion (~2-3 min) — both flip to `done`, files appear in `/media/Downloads/suwayomi/`
8. Existing mangarr poller picks up the new files on its next tick, classifies via Bindings, files into Kavita library
9. No Kavita scan 400 errors (existing fix from PR #36 still works)
10. Try multi-select 2 series same source — verify they run sequentially, not in parallel (second sits at `pending`)
11. Pause a running job — Suwayomi continues already-fed chapters but no new ones get enqueued
12. Resume — feeding resumes

### Test fakes

- `fakeStore` in `internal/web/web_test.go` — extend with BulkJob/BulkJobChapter CRUD
- New `fakeSuwayomi` — modelled on the existing `httptest`-based stubs in `internal/suwayomi/suwayomi_test.go`
- `recorder` in `internal/poller/poller_test.go` — irrelevant; orchestrator gets its own fixture
