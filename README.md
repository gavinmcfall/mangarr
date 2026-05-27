# mangarr

The missing *arr/organizer tier for a self-hosted manga stack.

Manga downloaders (Suwayomi, Tranga) write output organized **by source**. Readers (Kavita) want it organized **by content type** — manga / manhwa / manhua — with type-appropriate reading direction. Nothing bridges the two. mangarr does.

It watches the download folder, classifies each series by country of origin (via AniList), files it into the correct type library (hardlink / move / copy, with optional renaming), and triggers a Kavita scan. Like Sonarr, but for manga, and organizer-only — it doesn't download.

```
Suwayomi / Tranga  →  /media/Downloads/  →  [ mangarr ]  →  /media/Library/Books/<type>/  →  Kavita
   (download)                              classify + file                                    (read)
```

## Status

Design phase. See [`docs/DESIGN.md`](docs/DESIGN.md).

## Why "organizer only"

Suwayomi handles browsing + downloading, Tranga handles monitoring ongoing series, Komf handles metadata enrichment. mangarr fills the one gap none of them cover: classifying downloaded series by type and filing them where the reader expects, so reading direction and library organization are correct automatically.
