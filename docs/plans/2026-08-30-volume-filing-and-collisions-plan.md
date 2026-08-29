# Volume-Aware Filing & Collision Detection — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans or superpowers:test-driven-development task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** File volume archives (`… - Vol. 001.cbz`) under a volume rename scheme instead of mangling them into chapters, and stop the filer from silently skipping/overwriting when two source files render to the same destination.

**Architecture:** All new behaviour lives in `internal/filer` (detection, extraction, per-walk `dst→src` claims, `ConflictError`). The poller maps `ConflictError` to a `conflict` activity + `StatusConflict` and keeps filing/scanning what landed. Settings carry `VolumeRenameScheme`; the `filerAdapter` in `main.go` re-reads mode + schemes from settings per call.

**Tech Stack:** Go 1.26, `html/template`, HTMX, `go test ./...`.

**Spec:** `docs/specs/2026-08-30-volume-filing-and-collisions-design.md`

---

## File Structure

| File | Responsibility | Change |
|------|----------------|--------|
| `internal/filer/filer.go` | rename + place | volume detection, `ChapterNumber`/`VolumeNumber`, `RenderVolumeName`, `ValidateVolumeScheme`, `ValidateSchemePair`, `VolumeScheme` field, conflict tracking, `ConflictError`, `PlanConflict` |
| `internal/filer/filer_unix.go` / `filer_windows.go` | `sameDevice` | new (cross-device copy fallback must not read as a conflict) |
| `internal/filer/volume_test.go`, `conflict_test.go` | tests | new |
| `internal/model/model.go` | `Settings.VolumeRenameScheme`, `StatusConflict`, `ActionConflict` | add |
| `internal/store/store.go` | defaults | default volume scheme |
| `internal/metrics/metrics.go` | `mangarr_file_conflicts_total` | add + `IncFileConflict` |
| `internal/poller/poller.go` | `RunOnce`, `FileOne`, `RefileOne` | `ConflictError` branch; `MetricsSink.IncFileConflict` |
| `internal/poller/poller_test.go` | fakes + new test | `fakeMetrics.IncFileConflict`; conflict test |
| `internal/web/web.go` | settings form/API, activity filter | read/validate `volume_rename_scheme`; `ActionConflict` in `activityActions` |
| `internal/web/templates/settings.html` | UI | volume scheme field + example |
| `internal/web/static/mangarr.css` | pill | `.pill-conflict` |
| `main.go` | adapter | fresh-per-call mode/schemes |
| `docs/DESIGN.md` | Filing section | volumes + conflicts |

## Tasks

- [x] T1 filer: volume/chapter extraction + `IsVolumeFile` (tests first)
- [x] T2 filer: `ValidateVolumeScheme`, `ValidateSchemePair`, `RenderVolumeName`
- [x] T3 filer: `File()`/`Plan()` choose scheme per file; `VolumeScheme` field
- [x] T4 filer: conflict tracking + `ConflictError` + `PlanConflict` (+ `sameDevice`)
- [x] T5 model/store/metrics: `VolumeRenameScheme` default, `StatusConflict`, `ActionConflict`, counter
- [x] T6 poller: `ConflictError` branch in `RunOnce`/`FileOne`/`RefileOne`; test
- [x] T7 web: settings field + validation (form + JSON), activity filter, CSS pill
- [x] T8 main.go: adapter re-reads settings per call
- [x] T9 docs: DESIGN.md Filing section; README settings note
- [ ] T10 `go vet ./... && go test ./...`; PR
