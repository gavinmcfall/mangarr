# Series Lifecycle & Reconciliation — Design

**Date:** 2026-06-09
**Status:** Approved (design); implementation plan to follow
**Theme:** A of the Mangarr robustness roadmap (A → B → C)

## Context

Mangarr discovers series by scanning the download roots on disk
(`poller.RunOnce` → `Scanner.ScanAll()` → `Store.UpsertSeries`). The scan
**only ever adds**: there is no `DeleteSeries` store method and no
series-delete route. Two consequences, both observed in production:

- **Deletion doesn't stick / no pruning.** A series whose source folder is
  removed stays in the DB forever as an orphan row. Any "delete" action in
  the UI is non-durable because nothing removes the row, and the next scan
  would re-add it anyway while the folder exists.
- **Source-removal leaves orphans.** Removing a series from Suwayomi leaves
  stray download files *and* orphaned Kavita hardlinks on disk; Mangarr never
  reconciles disk reality.

Worked example (2026-06-09): switching "The Return of the Disaster-Class
Hero" from FlameScans to Asura left series id 1263 as a permanent orphan row
even after both disk folders were manually deleted, because the scan never
prunes.

This design makes the disk scan a true **reconcile** — disk is the source of
truth for *existence* — while keeping a human in the loop for every removal
and refusing to act on an obviously-broken scan.

### Roadmap position

- **Theme A (this doc):** Series lifecycle & reconciliation — foundation.
- Theme B: Honest counts & dud-chapter UX (depends on A).
- Theme C: Multi-source same-series identity (depends on A + B).

## Goals

1. A series whose source folder genuinely disappeared is surfaced for
   removal, never silently hard-deleted.
2. A user-initiated delete is durable and offers an explicit choice about
   whether on-disk files are also removed.
3. Reconciliation cannot mass-delete the library on a transient mount/Suwayomi
   failure.

## Non-goals

- Detecting that two sources are the same series (Theme C).
- Classifying *why* chapters are missing (Theme B).
- Auto-removing anything without explicit user confirmation.

## Design

### A1 · Data model

- New `model.Status` value **`orphaned`** ("removed from source"). An
  orphaned series is excluded from classification and filing, but retained in
  the DB and shown in the UI.
- New `series` column **`missing_since INTEGER NULL`** (unix seconds): the
  time the reconcile first observed the folder absent. `NULL` = present on
  disk this/last scan. Drives the grace timer.

### A2 · Reconcile pass (in `poller.RunOnce`, after `ScanAll`)

Today the scan only upserts found series. Add a reconcile pass that compares
the on-disk set against the DB set:

1. **Sanity floor (reject obviously-bad scans).**
   - If the scan root is unreachable or the scan returned **zero** items,
     **skip the reconcile entirely** for this tick (still upsert anything
     found). A healthy library never legitimately drops to zero.
   - If a single reconcile would newly mark series absent in numbers that are
     **both > 25% of currently-active (non-orphaned) series AND ≥ 5 series**,
     **abort the reconcile + emit an alert activity entry**. The absolute
     floor (≥ 5) stops small libraries and ordinary one- or two-series
     deletions from false-tripping the ratio; the ratio catches mass
     disappearance (mount or Suwayomi broke), not a real bulk delete.
2. **Grace (debounce real removals).**
   - For each DB series **not** present in the scan: if `missing_since` is
     `NULL`, set it to now. If it has been absent **≥ T** (default **10
     minutes**), flip `status = orphaned`.
   - For each DB series **present** in the scan: clear `missing_since` (back
     to `NULL`) and, if it was `orphaned`, restore it to `pending` so a
     reappearing source self-heals and re-runs classification next tick (the
     binding may have changed while it was gone).
3. Orphaned series are **never** auto-hard-deleted; removal is always a user
   action (A3).

Constants (`T = 10m`, floor `= 25%`, zero-scan guard) live in Settings with
these defaults so they are tunable without a rebuild, consistent with the
existing bulk settings.

### A3 · Durable user delete (new)

- New store method `DeleteSeries(id int64, deleteFiles bool) error` and route
  `POST /api/series/{id}/delete` (form field `delete_files=true|false`).
- **Two-step UI:**
  1. Click **Delete** → modal asks **which**:
     *(a) Remove from Mangarr only (keep files on disk)*, or
     *(b) Remove from Mangarr **and** delete files.*
  2. Choosing (b) requires a **second confirm** before anything is destroyed.
- **File deletion routes through the existing `recyclebin.Bin`** (recoverable,
  consistent with the #8 per-chapter remove): it sends the Suwayomi download
  directory and the Kavita library directory to the bin, then deletes the row.
  Option (a) deletes only the DB row (+ `series_tags` cascade) and leaves disk
  untouched.
- Delete is idempotent: deleting an already-absent series/row is a no-op, not
  an error.

### A4 · UI surfacing

- Orphaned series render in `/series` and `/library` with a **"Removed from
  source — confirm removal?"** banner offering:
  - **Remove** → opens the A3 delete flow (defaulting to "Mangarr only",
    since the files are already gone in the common case).
  - **Keep / Restore** → clears `orphaned` status (back to `pending`) and
    `missing_since` (for an intentional temporary source move; also self-clears
    if the source returns).

### A5 · Error handling & safety

- Reconcile failures are non-fatal: a bad scan skips the reconcile, it never
  aborts the whole poll tick or the upsert of found series.
- The sanity floor is evaluated **before** any state write, so an aborted
  reconcile leaves `missing_since` and statuses untouched.
- All destructive file operations go through the recycle bin, never a direct
  unlink, so a wrong removal is recoverable.

## Testing

- **Floor:** empty/zero-item scan → reconcile skipped, no rows touched.
- **Floor:** scan that would orphan > 25% → reconcile aborted + alert logged,
  no rows touched.
- **Grace:** series absent for < T → `missing_since` set, status unchanged;
  absent ≥ T → `orphaned`; reappears before T → `missing_since` cleared;
  reappears while orphaned → restored.
- **Delete:** `deleteFiles=false` removes row only; `deleteFiles=true` routes
  download dir + Kavita dir to recycle bin then removes row; both idempotent.
- **Web:** delete route requires the two-step form contract; missing series →
  no-op 303, not 500.

## Decisions captured

- Folder vanished → **soft-delete + UI confirm**, not hard-delete.
- User delete → **popup chooses Mangarr-only vs. also-files**, second confirm
  on the destructive path.
- Reconcile guard → **sanity floor (zero-scan + >25% abort) + 10-minute time
  grace**.
- Restore option retained for intentional temporary source moves.
- Deletes route through the **recycle bin**, not hard unlink.
