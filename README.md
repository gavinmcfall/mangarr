# mangarr

The missing *arr/organizer tier for a self-hosted manga stack.

Manga downloaders (Suwayomi, Tranga) write output organized **by source**. Readers (Kavita) want it organized **by content type** — manga / manhwa / manhua — with type-appropriate reading direction. Nothing bridges the two. mangarr does. It watches the download folder, classifies each series by country of origin (via AniList), files it into the correct type library (hardlink / move / copy, with optional renaming), and triggers a Kavita scan. Like Sonarr, but for manga, and organizer-only — it doesn't download.

```
Suwayomi / Tranga  →  /media/Downloads/  →  [ mangarr ]  →  /media/Library/Books/<type>/  →  Kavita
   (download)                              classify + file                                    (read)
```

## Status

MVP implemented: scanner, AniList classifier (with SQLite-backed cache), filer (hardlink/move/copy + rename scheme), Kavita scan trigger, scheduled poller, embedded HTMX UI (Series / Unmatched / Activity / Settings), JSON API. Single static Go binary, distroless Docker image, CI to GHCR.

## Quick start

Required env var: `MANGARR_DOWNLOAD_ROOTS` (comma-separated paths to watch).

Optional:
- `MANGARR_DB_PATH` — SQLite path (default `/config/mangarr.db`)
- `MANGARR_HTTP_ADDR` — listen address (default `:8590`)
- `MANGARR_ANILIST_ENDPOINT` — override AniList GraphQL URL

```bash
docker run --rm -p 8590:8590 \
  -e MANGARR_DOWNLOAD_ROOTS=/media/Downloads \
  -v /path/to/config:/config \
  -v /path/to/media:/media \
  ghcr.io/gavinmcfall/mangarr:latest
```

Then open `http://localhost:8590/settings`, set the per-type library roots and Kavita base URL + API key + library IDs, and click **Save settings**. Two rename schemes are applied per file: the chapter scheme (`{series}`, `{chapter}`) and the volume scheme (`{series}`, `{volume}`, used for files named like `Vol. 001` with no chapter marker); both must file into the same series folder. Files whose destination is already owned by a different file are reported as **conflicts** (Activity page + series status) rather than skipped or overwritten. The first scan runs on startup; subsequent scans run on the configured poll interval (default 15 min). Hit **Rescan now** from the Series or Activity page to trigger a scan on demand.

## Why "organizer only"

Suwayomi handles browsing + downloading, Tranga handles monitoring ongoing series, Komf handles metadata enrichment. mangarr fills the one gap none of them cover: classifying downloaded series by type and filing them where the reader expects, so reading direction and library organization are correct automatically.

## Design

See [`docs/DESIGN.md`](docs/DESIGN.md) for the design rationale, and [`docs/plans/`](docs/plans/) for the implementation plan.
