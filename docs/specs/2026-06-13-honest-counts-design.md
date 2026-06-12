# Honest Counts & Dud-Chapter UX — Design

**Date:** 2026-06-13
**Status:** Approved (design)
**Theme:** B of the Mangarr robustness roadmap (A → B → C)

## Context

The Library page lies in two ways:

1. **The "N missing" count is a single opaque number** (`total − downloaded`). It
   can mean three completely different things, which a user has to reverse-engineer
   by hand (we did, repeatedly, in the field):
   - **dud at source** — the chapter exists in Suwayomi's list but has
     `pageCount == 0` (e.g. FlameScans "Chapter 41"). It can *never* download;
     "1 missing" is permanent and not actionable.
   - **genuinely undownloaded** — the chapter has pages, just isn't grabbed yet.
   - **filing gap** — the chapter *is* downloaded in Suwayomi but isn't hardlinked
     into Kavita (e.g. Berserk: 400 downloaded, 384 in Kavita).
2. **The "Status" column is a stale job status.** It renders
   `mostRecentBulkJobStatus(mangaID)` — the state of the last bulk *download job*,
   not actual completeness. A row shows **"completed"** while chapters are still
   grabbable, because the source added chapters after that job ran, or the job
   reached its terminal state after erroring a dud chapter.

This theme makes both honest, computed during the existing Library **Sync** and
stored so the page is truthful at a glance.

### Roadmap position

- Theme A (done): Series lifecycle & reconciliation.
- **Theme B (this doc):** Honest counts & dud-chapter UX.
- Theme C: Multi-source same-series identity — builds on the durable
  `series.manga_id` link introduced here.

### Scope

In: the missing breakdown (dud vs undownloaded), the filing-gap line, the honest
Status column, and the durable `series.manga_id` link that powers the join.

Out (separate future work, not this spec): immediate dud-erroring in the
orchestrator (the 30-min stall), a dedicated jobs page, and replacing the
Library page's full-page reloads.

## Design

### B1 · Data model

- **Migration v10** (idempotent, table-tolerant — same pattern as v9), adds:
  - `library_cache.dud_count INTEGER NOT NULL DEFAULT 0` — not-downloaded
    chapters with `pageCount == 0`.
  - `library_cache.filed_count INTEGER NOT NULL DEFAULT 0` — `.cbz` files present
    in the series' Kavita library dir.
  - `series.manga_id INTEGER` (nullable) — the Suwayomi manga id this disk series
    corresponds to. The durable join key.
- Everything else is **derived at render**, never stored, so it cannot drift:
  - `missing = total − downloaded`
  - `undownloaded = missing − dud_count`
  - `filing_gap = max(0, downloaded − filed_count)`

### B2 · Sync computes the counts

The existing `apiLibrarySync` already calls `ListChapters(mangaID)` per manga in
an 8-worker pool. Two cheap additions:

- **`dud_count`** — add `pageCount` to the `ListChapters` GraphQL query, its node
  struct, and `suwayomi.Chapter` (one field, **no new round-trips**). In the
  worker's count loop: `dud_count = count(!isDownloaded && pageCount == 0)`.
  `pageCount == -1` (Suwayomi hasn't fetched it) and `> 0` both count as
  *undownloaded*, not dud.
- **`filed_count`** — resolve this manga to its series via `GetSeriesByMangaID`,
  then `ResolveLibraryDir` (Theme A) → count `.cbz` files in that dir. This is a
  **local disk stat, no network**, done inside the same worker. A miss (series
  not yet linked, dir absent) falls back to `filed_count = downloaded` so a
  lookup miss never invents a false filing gap.

Both land in the `SaveLibraryCacheEntry` write alongside `total`/`downloaded`.

### B3 · `series.manga_id` populated by the poller

`poller.RunOnce` already refreshes the `SuwayomiCache` (`PathCache`) at the top of
every tick and then loops every scanned series. The `PathCache.CacheEntry`
carries `MangaID`, keyed by the series' parent dir. So in the per-series loop:
`if e, ok := p.SuwayomiCache.Lookup(s.SourcePath); ok { persist e.MangaID }`.
A new store method `SetSeriesMangaID(seriesID, mangaID)` writes it; a still-NULL
`manga_id` (series not yet through a tick) just makes B2's `filed_count` fall
back, and self-heals on the next tick. New store method
`GetSeriesByMangaID(mangaID) (Series, error)` powers the reverse lookup B2 uses.

### B4 · Library rendering — honest counts + honest status

`pageLibrary` builds each `libraryRow` from the cache entry; it gains the derived
fields and an honest status. The template shows only non-zero sub-lines so clean
rows stay clean:

```
Disaster-Class Hero   181 · 180 · 1 missing      Complete · 1 unavailable
   └ 1 dud at source (permanent)
One Piece             1184 · 1 · 1183 missing     Incomplete · 1183 to download
   └ 1183 not downloaded
Berserk               400 · 400 · 0 missing       Complete
   ⚠ 16 downloaded, not filed to Kavita   [Re-run filer]
```

**Missing sub-lines:**
- `dud_count > 0` → "N dud at source (permanent)" — muted; it explains, it is not
  an action.
- `undownloaded > 0` → "N not downloaded".
- `filing_gap > 0` → a ⚠ line "N downloaded, not filed to Kavita" with a
  **Re-run filer** action (POSTs to Theme A's `RefileOne` route for the joined
  series; uses the action-feedback toast).

**Honest Status column** (replaces the raw `mostRecentBulkJobStatus` string),
evaluated in order:
1. A bulk job for this manga is **running** → **"Downloading"** (with its
   `completed/total` progress when available).
2. Else the most recent job is **errored** or **paused** → show that (actionable).
3. Else derive from counts:
   - `undownloaded > 0` → **"Incomplete · N to download"**.
   - `missing > 0` but `undownloaded == 0` (all the gap is dud) →
     **"Complete · N unavailable"** (as complete as the source allows).
   - `missing == 0` → **"Complete"**.

So "Complete/completed" can never render next to grabbable chapters again.
`mostRecentBulkJobStatus` stays (for the running/errored/paused cases) but no
longer *is* the status — it is one input to it.

### B5 · Testing

- **Migration v10:** the three columns exist after migration; idempotent;
  table-tolerant for the bare-DB fixture.
- **Store:** `SetSeriesMangaID` / `GetSeriesByMangaID` round-trip; library_cache
  save/read carries `dud_count` + `filed_count`.
- **Poller:** `RunOnce` persists `manga_id` from a `SuwayomiCache` hit; a miss
  leaves it NULL (no write / no error).
- **Sync (`apiLibrarySync`):** with a `fakeSuwayomi` returning chapters with mixed
  `pageCount` (0 / -1 / >0) and `isDownloaded`, the saved cache entry has the
  right `dud_count`; `filed_count` reflects the resolved dir (and falls back to
  `downloaded` on a manga_id miss).
- **Render (`pageLibrary`):** the honest Status string for each case
  (running→Downloading, undownloaded→Incomplete, all-dud→Complete·N unavailable,
  zero→Complete); the dud / undownloaded / filing-gap sub-lines appear only when
  non-zero; a clean row shows none.
- **Derivations:** `undownloaded = missing − dud`, `filing_gap = max(0, downloaded
  − filed)` — pure, table-driven.

## Decisions captured

- Computed on **Sync**, stored in `library_cache`, **always visible** (not lazy).
- Scope = **missing breakdown (dud vs undownloaded) + filing-gap line + honest
  Status column**.
- The join is a **durable persisted `series.manga_id`** (migration v10, populated
  by the poller from the existing `SuwayomiCache`), not a render-time string
  match — and it is the foundation Theme C will build on.
- `dud` = `pageCount == 0`; `pageCount == -1` (unfetched) and `> 0` are
  *undownloaded*.
- `filed_count` resolution miss → fall back to `downloaded` (never a false gap).
- Status precedence: running job → errored/paused → derived-from-counts.
