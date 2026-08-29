# Volume-Aware Filing & Collision Detection — Design

**Date:** 2026-08-30
**Status:** Approved (design, in chat)
**Origin:** Dragon Ball Z colour volumes sitting unfiled in `--SORT--`; the
Weeb Central "Dragon Ball" filed by mangarr found to be corrupt.

## Context

Two field findings on the same series drive this:

1. **mangarr cannot file volumes.** The only rename tokens are `{series}` and
   `{chapter}`, and `{chapter}` is "the first number in the filename". A
   volume file such as `Dragon Ball Z - Vol. 001 (English - Colour).cbz`
   would be filed as `Dragon Ball Z - Ch.001.cbz`, and Kavita would read a
   200-page volume as chapter 1. Retail/volume releases arrive from outside
   Suwayomi/Tranga (hand-collected folders in `--SORT--`, 107 GB of them)
   and today have no path into the library.
2. **The filer silently corrupts on filename collisions.** Weeb Central
   numbers Dragon Ball as `1…194` and the Z run as `Z 1…325`. Both
   `Official_1.cbz` and `Official_Z 1.cbz` render to `Dragon Ball - Ch.1.cbz`.
   The second is skipped as "destination already exists" (idempotency), so
   520 downloaded chapters became 326 filed: DB 1–194 followed by Z 195–325,
   with Z 1–194 missing. Nothing in the UI, activity log or metrics said so.
   Verified by inode on 2026-08-30.

Both are properties of the filer; both are fixed there.

### Non-goals (YAGNI)

- Splitting a source that restarts numbering ("Z 1") into a second series.
  The honest fix is a different source; mangarr's job is to refuse to
  corrupt the library and say why.
- Merging chapter files into volume archives. Volume files come from
  volume sources.
- Volume-aware Kavita metadata (ComicInfo.xml rewriting). Kavita parses
  `Vol.NNN` from the filename.

## Design

### 1. Volume detection (filer)

A file is a **volume file** when its base name matches
`(?i)\bvol(?:ume)?\.?\s*(\d+)` **and** does not contain a chapter marker
`(?i)\bch(?:apter)?\.?\s*\d`. `Vol.1 Ch.3.cbz` is therefore a chapter file.

While in the extractor, `{chapter}` becomes *the number following a chapter
marker, else the first number in the name*. Today it is blindly the first
number, which is the same class of defect as the collision (`Vol.1 Ch.3` →
chapter `1`).

Numbers are substituted as written in the source name (`001` stays `001`,
`7.5` stays `7.5`), matching existing `{chapter}` behaviour.

### 2. Volume rename scheme (settings)

New setting `VolumeRenameScheme` (`json:"volume_rename_scheme"`), default
`{series}/{series} - Vol.{volume}.cbz`. Validated by the same rules as
`RenameScheme` but requiring `{series}` and `{volume}` (and rejecting
`{chapter}`). One additional cross-check, `ValidateSchemePair`: both schemes
rendered with sample values must land in the **same directory**, because
`Poller.ResolveLibraryDir` derives the series' Kavita folder from the
chapter scheme alone.

Settings page gets one text field beside the existing rename scheme; the PUT
handler and JSON API carry it; `GetSettings` applies the default when empty
(existing rows).

### 3. Filing volumes

`Filer` gains `VolumeScheme string`. `File()`/`Plan()` choose the scheme per
file by the detection rule in §1. Everything else — mode, traversal guard,
idempotency, recycle bin — is unchanged.

Intake path for hand-collected volumes: a third download root
`/media/Downloads/manual` added through the existing UI-managed download
roots (no code). A folder of `.cbz` without `ComicInfo.xml` takes its title
from the folder name; the classifier already retries AniList with a trailing
parenthetical stripped, so `Dragon Ball Z (Color Edition)` resolves to
Dragon Ball Z → JP → Manga.

### 4. Collision detection (filer + poller)

Within one `File()`/`Plan()` walk the filer keeps `dst → src`. A file is a
**conflict** when either:

- another source file in the same walk already claimed its `dst`, or
- `dst` already exists on disk and, in hardlink mode, is **not the same
  inode** as `src` (`os.SameFile`). In move/copy mode an existing `dst` is
  still an idempotent skip — there is no inode to compare.

Conflicting files are never written. `File()` files every non-conflicting
file first, then returns `*ConflictError{Conflicts []Conflict}` (`Conflict`
= `Src`, `Dst`, `ClaimedBy`). `Plan()` returns them as `PlanConflict`
entries with `Error` populated, so the dry-run preview shows them.

The poller treats `ConflictError` as a partial success: it records a new
activity action `conflict` (detail lists up to 5 colliding names and the
total), sets the series status to a new `StatusConflict`, still records the
resolved binding and still triggers the Kavita scan for what did land.
Metrics gain a `mangarr_file_conflicts_total` counter. Any other error keeps
today's `error` path.

The `/series` page shows `conflict` as a status pill; the activity filter
list gains the new action. No new pages.

### 5. Repair of the field case

Out of code scope, sequenced after deploy:

1. Set `volume_rename_scheme`; add `/media/Downloads/manual` root.
2. `mv "--SORT--/Dragon Ball Z (Color Edition)" /media/Downloads/manual/`
   (same ZFS dataset — instant, hardlink-able).
3. Rescan; verify 26 volumes filed, 0 conflicts, Kavita shows 26 volumes,
   `nlink=2`.
4. Delete the Weeb Central "Dragon Ball" series (recycle bin), remove it from
   the Suwayomi library, rescan Kavita. The first post-deploy tick will have
   flagged it `conflict` — that is the detector working, not a regression.

## Error handling

- Invalid volume scheme or a scheme pair that diverges on directory is
  rejected at settings save with a message naming the rule.
- A volume file whose number cannot be extracted falls back to the chapter
  path (same as today's unknown-name behaviour); nothing is dropped.
- Conflicts never abort the series; the Kavita scan still fires for the
  files that landed.

## Testing

`go test ./...` + `go vet ./...` (CI gates). Table-driven:

- filer: volume detection incl. precedence (`Vol.1 Ch.3`), chapter-after-
  marker extraction, `Vol.` / `Volume` / `v01`-style is **not** matched
  (only `vol`/`volume` — keep the rule narrow), scheme-pair validation.
- filer: conflict on same-dst within a walk; conflict on existing dst with a
  different inode (hardlink mode); skip on same inode; move/copy modes keep
  skip semantics; `Plan()` mirrors `File()`.
- poller: `ConflictError` → `conflict` activity + `StatusConflict` +
  binding recorded + Kavita scan still triggered.
- web: settings round-trip for the new field; validation error surfaces.
