# mangarr — Design

> The missing *arr/organizer tier for a self-hosted manga stack.

## Purpose

A self-hosted manga pipeline typically has three tiers, mirroring the *arr/media-server world:

| Media world | Manga world | Role |
|---|---|---|
| qBittorrent / SABnzbd | Suwayomi / Tranga | download client — fetch the bytes |
| **Sonarr / Radarr** | **mangarr** | **organizer — watch downloads, classify, rename, file into the library, trigger a scan** |
| Plex / Jellyfin | Kavita | serve + read |
| Kometa (Plex Meta Manager) | Komf | enrich metadata in the server (covers, summaries, tags) |

The organizer tier does not exist for scanlation-sourced manga. Downloaders (Suwayomi, Tranga) write tidy-but-flat output organized **by source/tool**, while readers (Kavita) want content organized **by content type** (manga / manhwa / manhua) with type-appropriate reading direction. Nothing bridges the two.

**mangarr fills exactly that gap and nothing else.** It watches the download tier, classifies each series by country of origin, files it into the correct type-library (optionally renaming), and tells Kavita to scan.

### Non-goals (YAGNI)

mangarr does **not** download, monitor sources for new chapters, manage source extensions, fetch metadata, provide a dashboard/calendar/bulk operations, or support auth/multi-user. Suwayomi (browse + download), Tranga (monitor ongoing series), and Komf (metadata enrichment) keep their jobs.

## Key design insight

Files are routed into the deployer's **existing type-libraries** (`/media/Library/Books/{Manga,Manhwa,Manhua}`), each of which already carries a type-appropriate Kavita reading profile (manhwa → webtoon scroll, manga → right-to-left). Because library membership is folder-based, **landing a series in the right folder makes its reading direction correct automatically** — mangarr needs no reading-profile API call. Classification + filing is the whole job.

## Architecture

- **Language/runtime:** Go, compiled to a single static binary with an embedded web UI. One small container image.
- **Deployment:** standalone — its own deployment in the `entertainment` namespace, not a sidecar of the reader. Mounts the shared media NFS export read-write, keeps its own small config volume for SQLite, exposed on an internal-only route (LAN/VPN; no auth).
- **State:** SQLite (series, classifications, unmatched queue, activity log, settings).
- **Config home:** this repository. CI builds the container image to the registry; the GitOps repo only references the image in a HelmRelease.

```
poll /media/Downloads/{suwayomi,tranga}/ every N minutes
  → find series dirs with unprocessed (or newly-arrived) chapters
  → read Series name from ComicInfo.xml  (fallback: folder name)
  → AniList GraphQL countryOfOrigin → type
        JP → Manga · KR → Manhwa · CN/TW → Manhua
        ├─ confident → file
        └─ no/ambiguous match → Unmatched queue (leave files in Downloads)
  → rename per configurable scheme
  → place into /media/Library/Books/<type>/   (hardlink | move | copy — user setting)
  → trigger Kavita library scan (internal API + key)
  → record in Activity/History
```

## Components

Each is an independently testable Go package with one purpose:

| Package | Responsibility | Depends on |
|---|---|---|
| `poller` | scheduled scan of the download roots; emit "candidate series" events | `store`, `scanner` |
| `scanner` | parse a series dir: extract Series name from ComicInfo.xml, fall back to folder name; enumerate chapters | filesystem |
| `classifier` | look up a title on AniList, map `countryOfOrigin` → type; cache results | AniList GraphQL |
| `filer` | render rename scheme; hardlink/move/copy into the target library; idempotent | filesystem, `store` |
| `kavita` | trigger a library scan via Kavita's API | Kavita REST |
| `store` | SQLite persistence: series, unmatched queue, activity, settings | SQLite |
| `web` | embedded UI + JSON API | `store`, all of the above |
| `config` | load env + settings, validate | — |

## Classification

- **Signal:** AniList `Media.countryOfOrigin` — `JP` → Manga, `KR` → Manhwa, `CN`/`TW` → Manhua.
- **Title source:** ComicInfo.xml `<Series>` (both downloaders embed it); folder name fallback.
- **Caching:** classifications cached by normalized title to respect AniList's ~90 req/min limit; backoff on 429.
- **Ambiguity:** if AniList returns no match, multiple equally-likely matches, or a country mangarr doesn't map, the series goes to the **Unmatched queue** — files stay in Downloads, nothing is mis-filed. The user resolves it in the UI; the decision is remembered (keyed by title) so it auto-applies next time.

## Filing

- **Modes (user-configurable, like Sonarr):** hardlink (default intent), move, copy.
  - **hardlink** — series exists in both Downloads (downloaders keep managing it) and the type-library (Kavita reads it); no data duplication. Requires source and destination on the same filesystem — `/media/Downloads` and `/media/Library` are the same NFS export, satisfied. If a hardlink fails (cross-device), fall back to copy and warn.
  - **move** — relocate out of Downloads. Cleaner single location, but downloaders lose track of their output.
  - **copy** — duplicate; fully decoupled, doubles disk.
- **Rename:** two configurable schemes. The chapter scheme (e.g. `{series}/{series} - Ch.{chapter}.cbz`) applies to chapter files; the volume scheme (default `{series}/{series} - Vol.{volume}.cbz`) applies to files whose name carries a `Vol.`/`Volume` marker and no chapter marker, so retail/volume releases (e.g. `Dragon Ball Z - Vol. 001.cbz`) land as volumes Kavita can parse instead of as chapter 1. Both must render into the same series directory. `{chapter}` is the number after a chapter marker when present, else the first number in the name. Preserves the embedded ComicInfo.xml so Kavita parses correctly regardless.
- **Idempotency:** a destination that already exists **and is the same file** (same inode, hardlink mode) is skipped; re-runs never re-file or double-link. New chapters of an already-filed series are picked up incrementally as they arrive.
- **Conflicts:** two source files that render to the same destination, or an existing library file that is *not* a hardlink of the source, are **conflicts** — nothing is written or overwritten. The rest of the series still files; the poller records a `conflict` activity naming the files, sets the series status to `conflict`, and still triggers the Kavita scan. (Field case: Weeb Central numbers Dragon Ball `1…194` and `Z 1…325`; before this, `Z 1` was silently skipped as "already exists".) The fix for a source that restarts numbering is a different source — mangarr refuses to corrupt the library and says why.
- **Intake beyond the downloaders:** any UI-managed download root works, e.g. `/media/Downloads/manual` for hand-collected volume folders; a folder of `.cbz` without ComicInfo.xml takes its title from the folder name, and the classifier retries AniList with a trailing `(…)` tag stripped.

## Web UI (MVP — "standard" set)

Stripped-down Sonarr in spirit. Internal-only.

- **Series** — everything organized: title, classified type, source, chapter count, status; per-series re-classify/override.
- **Unmatched** — series AniList couldn't place; pick type/library, choice remembered.
- **Activity / History** — every filing action with timestamp and outcome; a manual "Rescan now" action.
- **Settings** — type→library folder mappings + Kavita library IDs, move mode, rename scheme, poll interval, Kavita connection, AniList options.

## Secrets / configuration

- **Kavita API key** — injected from the secret manager (external secret), never committed.
- **AniList** — public GraphQL, no key.
- **Mappings + Kavita library IDs** — set in the UI, stored in SQLite.

## Error handling

- AniList rate limit / 429 → cache + exponential backoff.
- Kavita scan trigger failure → retry with backoff, surface in Activity.
- Hardlink cross-device → fall back to copy with a warning.
- Series mid-download (chapters trickling in) → safe to re-process; only new chapters are filed.
- Malformed/missing ComicInfo.xml → folder-name fallback, else Unmatched.

## Testing

- Unit tests per package: classifier country→type mapping; ComicInfo.xml parsing; filer hardlink/move/copy + idempotency against temp dirs; rename-scheme rendering.
- Integration test against a fake AniList GraphQL endpoint and a fake Kavita API.

## Deployment (separate, GitOps)

The container image is built from this repo by CI and published to the registry. The GitOps repository deploys it as a standalone app: shared media NFS mount, a small config volume for SQLite (backed up like other stateful apps), an external secret for the Kavita API key, and an internal route. mangarr is independent of the reader's lifecycle.
