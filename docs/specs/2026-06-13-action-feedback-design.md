# Global Action Feedback — Design

**Date:** 2026-06-13
**Status:** Approved (design); ship then iterate

## Context

Almost every action in the UI uses the pattern
`hx-post … hx-swap="none" hx-on::after-request="window.location.reload()"`.
A click fires a background request and, only when it finishes, reloads the whole
page. In between there is **no feedback** — no spinner, no disabled button, no
"working…" — and the completion is a full-page reload that reads as a jarring
"quick flash." It is worst on **Sync**, which does a live `ListChapters` call to
Suwayomi for every library manga (8 in parallel) and so looks dead for seconds.

The app wires up **none** of HTMX's feedback hooks (`hx-indicator`,
`hx-disabled-elt`, `.htmx-request` styling) and has no toast component. The fix
is central and mostly CSS — `base.html` + `mangarr.css` — with no per-button
rewrites.

This is distinct from the "honest counts" work (Theme B): that makes the numbers
truthful; this makes the app feel responsive.

## Goals

1. Every HTMX action shows it is in flight (so nothing ever looks dead).
2. A busy control cannot be double-fired and visibly indicates it is working.
3. Completion gives a clear confirmation (toast) that survives the page reload.

## Non-goals

- Replacing the full-page-reload pattern with targeted fragment swaps (Layer 3).
  Once the in-flight state is visible, the reload reads as a deliberate refresh.
  Targeted swaps can be done incrementally later.
- Any change to what the actions actually do.

## Design

All three pieces live in `internal/web/templates/base.html` (markup + one
`<script>` block before `</body>`) and `internal/web/static/mangarr.css`.

### 1 · Top progress bar

A fixed 3px bar at the top of the viewport (`#global-progress`). Global HTMX
event listeners in `base.html`:
- `htmx:beforeRequest` → add `.is-loading` (bar animates from 0 → ~90%).
- `htmx:afterRequest` → complete to 100% then fade out.

A request counter guards overlapping requests (e.g. the 5s downloads-badge poll
must not hide the bar while a Sync is still running): increment on
`htmx:beforeRequest`, decrement on `htmx:afterRequest`, only hide at zero. The
periodic sidebar badge poll (`hx-trigger="every 5s"`) is **excluded** from the
bar so it does not flicker every 5 seconds — gate the listener on
`evt.detail.elt` not being the badge (or on the request not being a background
poll), see Testing.

### 2 · Busy triggering element (CSS only)

HTMX auto-adds `.htmx-request` to the element that triggered the request for its
duration. Style it in `mangarr.css`:
- buttons/`[type=submit]`: reduced opacity, `cursor: progress`,
  `pointer-events: none` (prevents double-fire), and an inline spinner
  (`::after` border-spinner).
Zero per-button markup changes — every existing `hx-` control inherits it.

### 3 · Toasts (reload-safe)

- Markup: `#toast-container` (fixed, bottom-right) in `base.html`.
- Helper: `window.mangarrToast(message, kind)` where `kind ∈ {success, error}`.
  Because most actions reload, the helper writes the toast to `sessionStorage`
  (key `mangarr.pendingToast`) rather than showing it inline; on `DOMContentLoaded`,
  `base.html` reads and shows any pending toast, then clears the key. A toast
  thus survives the post-action reload.
- Global `htmx:afterRequest` listener:
  - On success (`evt.detail.successful`): message from the `HX-Toast` response
    header if present, else a generic `"Done"`. kind `success`.
  - On failure: message from `HX-Toast` if present, else `"Something went wrong"`.
    kind `error`.
  - Skip toasts for background polls (the downloads badge) — same gate as the bar.
- Auto-dismiss after ~4s; dismiss-on-click.

### 4 · `HX-Toast` headers on high-value handlers

Add `w.Header().Set("HX-Toast", "...")` to a handful of handlers so their
completion message is specific rather than generic:
- `apiLibrarySync` → `Synced N series` (it already counts `len(entries)`).
- `apiBulkCreate` → `Queued N chapters` / `Started download` (use its computed count).
- `apiSeriesRefile` (RefileOne) → `Re-filed`.
- `apiSeriesDelete` → `Removed` (and `Removed + files` when `delete_files=true`).
- `apiSeriesRestore` → `Restored`.

Every other action still gets the generic toast — no need to touch them.

## Testing

This is mostly frontend; Go tests cover the server-observable behavior:
- `base.html` renders the `#global-progress`, `#toast-container`, and the
  `mangarrToast` script (assert the rendered shell contains these ids/markers).
- The `HX-Toast` header is set with the expected value by `apiLibrarySync`,
  `apiSeriesRefile`, `apiSeriesDelete` (both modes), `apiSeriesRestore`, and
  `apiBulkCreate` — assert via `httptest` on each handler.
- The bar/toast **exclusion** of the 5s downloads-badge poll: assert the badge
  endpoint/element is identifiable so the client gate can exclude it (the gate
  itself is JS, verified in the manual smoke).

Visual feel — bar animates, buttons spin and lock, toast appears after reload —
is verified in the manual smoke (click Sync / download / refile / delete).

## Decisions captured

- In-flight style = **top progress bar** + busy-button spinner (not a corner
  spinner).
- Toasts are **reload-safe via sessionStorage**, shown on next page load.
- Specific messages via an **`HX-Toast` response header**; generic fallback.
- Background polls (downloads badge, every 5s) are **excluded** from bar + toast.
- Layer 3 (kill full-page reloads) is **out of scope** for this pass.
