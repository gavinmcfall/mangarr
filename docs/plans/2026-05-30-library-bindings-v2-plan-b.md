# Library Bindings v2 — Plan B Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Wire the Plan A data model + classifier into the Settings UI. Users gain a Library Bindings CRUD card, a Classification Rules editor, a Default Binding picker, a widened Suwayomi Category Overrides dropdown (now spans all bindings), and an activity-log Via renderer that resolves rule IDs to names.

**Architecture:** Append new sections to the existing `internal/web/templates/settings.html` (do not rewrite). Atomic form POST persists bindings + rules + default-binding + Suwayomi overrides in a single transaction. The current `internal/web/suwayomi.go` (Library Map Plan C) is the closest pattern — mirror its handler + helper shape. Two new JSON-only endpoints (`/api/bindings`, `/api/rules`) for power users scripting via `curl`.

**Tech Stack:** Go 1.25, `html/template`, HTMX (already present), the existing `internal/web` package, the Plan A `internal/classifier` `Via*` constants, the Plan A store methods (`ListBindings`, `SaveBindings`, `ListRules`, `SaveRules`). No new external dependencies.

---

## Pre-flight

Working directory: `/home/gavin/my_other_repos/mangarr`. Branch: `feat/library-bindings-v2-plan-b` (cut from `origin/main` AFTER Plan A merges as `6a2f496`).

**Test rule from prior reviews:** never use canonical placeholder credentials in tests (`hunter2`, `password123`, `admin/admin`). Use `"test-placeholder-pw"`-style strings.

**Commit messages must NOT contain "claude" or "anthropic"** — Gavin's commit-msg hook blocks them. Drop any auto-inserted `Co-Authored-By` trailer.

**verify-gate**: after staging, `touch /home/gavin/my_other_repos/mangarr/.claude/.verified` as its own Bash call before commit. `$CLAUDE_PROJECT_DIR` is unset in subagent subshells — use the absolute path.

```bash
git fetch origin main
git checkout -b feat/library-bindings-v2-plan-b origin/main
```

## Context the implementer must read

Before any code: read the spec at `docs/specs/2026-05-30-library-bindings-v2.md`, specifically the **Plan B truth statements** (the EARS list under "Plan B — Settings UI for bindings, rules, and default binding") and the **UI shape** section under Design.

Also skim `internal/web/suwayomi.go` (Library Map Plan C from PR #32). It's the established pattern for: fresh-per-call clients, atomic form POST extending `pageSettings`/`saveSettings`, HTML fragment endpoints, "Unknown (ID: N)" rendering for deleted entities. Plan B mirrors that shape for bindings + rules.

The Plan A→Plan B boundary note matters: **today the Settings UI writes to `SuwayomiCategoryOverrides` (v1, Kavita-lib-IDs) which the v2 classifier ignores**. Plan B fixes this by writing to `SuwayomiCategoryBindings` (v2, Binding-IDs). Until Plan B lands the Suwayomi-override UI is silently broken — this PR fixes it.

---

### Task 1: JSON read-only endpoints — GET /api/bindings + /api/rules

Smallest piece. Self-contained, no template touches. Useful immediately for power users + tests later in this plan.

**Files:**
- Modify: `internal/web/web.go` (register routes + add handlers, OR put handlers in `internal/web/bindings.go` if you'd rather split — match the suwayomi.go precedent which has its own file)
- Create: `internal/web/bindings.go` (new file for Plan B handlers)
- Create: `internal/web/bindings_test.go`

- [ ] **Step 1: Write failing tests**

Create `internal/web/bindings_test.go`:
```go
package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/model"
)

func TestAPIBindingsReturnsJSONList(t *testing.T) {
	st := &fakeStore{
		bindings: []model.Binding{
			{ID: 1, Name: "Manga", LibraryRoot: "/m/a", KavitaLibID: 1, DefaultIsAdult: false},
			{ID: 2, Name: "Manhwa 18+", LibraryRoot: "/m/b", KavitaLibID: 2, DefaultIsAdult: true},
		},
	}
	h := newTestHandler(t, st)

	req := httptest.NewRequest(http.MethodGet, "/api/bindings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type: want application/json, got %q", ct)
	}
	var got []model.Binding
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 bindings, got %d", len(got))
	}
	if got[0].Name != "Manga" || got[1].Name != "Manhwa 18+" {
		t.Errorf("ordering or names wrong: %+v", got)
	}
}

func TestAPIRulesReturnsJSONListAscendingPriority(t *testing.T) {
	jp := "JP"
	st := &fakeStore{
		bindings: []model.Binding{{ID: 1, Name: "Manga"}},
		rules: []model.ClassificationRule{
			{ID: 2, Priority: 200, Name: "Korean", Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: 1},
			{ID: 1, Priority: 100, Name: "Japanese", Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: 1},
		},
	}
	h := newTestHandler(t, st)

	req := httptest.NewRequest(http.MethodGet, "/api/rules", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	var got []model.ClassificationRule
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rules, got %d", len(got))
	}
	// Even though the store-fake returns whatever order, ListRules is the
	// source of truth and the store's ORDER BY priority means callers
	// expect ascending. The handler trusts the store's order; this test
	// pins that contract end-to-end.
	if got[0].Priority != 100 || got[1].Priority != 200 {
		t.Errorf("priority order: want 100 then 200, got %d then %d", got[0].Priority, got[1].Priority)
	}
}
```

You'll need a `newTestHandler(t, *fakeStore) http.Handler` helper if one doesn't already exist matching the Plan A `fakeStore` shape. Existing `internal/web/web_test.go` likely already has helpers from Library Map era — extend, don't duplicate.

If `fakeStore` doesn't have `ListBindings`/`ListRules` methods yet, add them. The fakeStore probably backs the existing tests via simpler-shaped methods; widen its surface to satisfy the Plan B's `SettingsReader` interface.

- [ ] **Step 2: Run, verify failure**

```bash
go test ./internal/web/ -run 'TestAPIBindings|TestAPIRules' -v
```

Expected: FAIL — endpoints not registered or `fakeStore` missing methods.

- [ ] **Step 3: Implement the handlers**

Create `internal/web/bindings.go`:
```go
package web

import (
	"encoding/json"
	"net/http"
)

// apiBindings is the JSON read-only endpoint at GET /api/bindings.
// Returns every binding in the order the store returns them (currently
// ascending by Name). Useful for power users scripting via curl and for
// the Plan B Settings UI's Library Bindings card.
func (h *Handler) apiBindings(w http.ResponseWriter, r *http.Request) {
	bindings, err := h.store.ListBindings()
	if err != nil {
		http.Error(w, "list bindings: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if bindings == nil {
		bindings = []model.Binding{} // marshal as `[]`, not `null`
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(bindings); err != nil {
		// Encoder already wrote partial output; nothing we can do but log.
		// The existing codebase doesn't have a structured logger; use log.
		// (Inspect existing handlers — match whatever logging shape they use.)
	}
}

// apiRules is the JSON read-only endpoint at GET /api/rules.
// Returns every classification rule sorted ascending by Priority (the
// store's ORDER BY priority guarantees this).
func (h *Handler) apiRules(w http.ResponseWriter, r *http.Request) {
	rules, err := h.store.ListRules()
	if err != nil {
		http.Error(w, "list rules: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if rules == nil {
		rules = []model.ClassificationRule{}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(rules)
}
```

Add the necessary imports (`"github.com/gavinmcfall/mangarr/internal/model"`).

Register the routes in `internal/web/web.go` near the existing `/api/kavita/libraries` and `/api/suwayomi/categories` route registrations:

```go
mux.HandleFunc("GET /api/bindings", h.apiBindings)
mux.HandleFunc("GET /api/rules", h.apiRules)
```

Whatever the actual router shape is — Go 1.22+ `http.ServeMux` pattern syntax, or `chi`, or whatever this codebase uses — match it. Grep `internal/web/web.go` for `/api/kavita/libraries` and use the same shape.

The `h.store` field needs `ListBindings()` and `ListRules()` methods on its interface. If the Handler's store interface doesn't have them yet, extend it. The real `*store.Store` from Plan A already implements both.

- [ ] **Step 4: Run, verify pass**

```bash
go test ./internal/web/ -run 'TestAPIBindings|TestAPIRules' -v -race
go test ./internal/web/... -count=1 -race
go build ./...
go vet ./...
```

Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/web/bindings.go internal/web/bindings_test.go internal/web/web.go internal/web/web_test.go
touch /home/gavin/my_other_repos/mangarr/.claude/.verified
git -c commit.gpgsign=false commit -m "feat(web): GET /api/bindings + /api/rules JSON endpoints

Read-only JSON endpoints surface the Plan A v2 data model. Useful for
power users scripting Settings via curl and for the Plan B UI tasks
that follow to consume bindings + rules via fetch."
```

---

### Task 2: Library Bindings card — CRUD UI

The first user-facing piece. A table with one row per binding; Name, Library Root, Kavita library dropdown, DefaultIsAdult checkbox, Delete. An "Add Binding" affordance appends a new empty row.

**Files:**
- Modify: `internal/web/web.go` (extend `pageSettings` to load bindings + Kavita libs, populate page data)
- Modify: `internal/web/templates/settings.html` (add Library Bindings card)
- Create: `internal/web/templates/binding-rows.html` (partial used by initial render + future HTMX swap)
- Modify: `internal/web/bindings_test.go` (add render tests)

- [ ] **Step 1: Inspect the existing settings.html structure**

Read `internal/web/templates/settings.html` once to understand where to slot the new card. Look for the section that currently renders "Library Roots" (filesystem path inputs) and "Kavita Libraries" picker. The new Library Bindings card goes ABOVE these — those v1 sections stay for now (Task 9 removes them).

Also look for the existing CSS classes (`.card`, `.settings-row`, `.pill`, etc.) — match them.

- [ ] **Step 2: Write failing render test**

Append to `internal/web/bindings_test.go`:
```go
func TestSettingsPageRendersLibraryBindingsCard(t *testing.T) {
	st := &fakeStore{
		bindings: []model.Binding{
			{ID: 1, Name: "Manga", LibraryRoot: "/media/Library/Manga", KavitaLibID: 1, DefaultIsAdult: false},
			{ID: 2, Name: "Manhwa 18+", LibraryRoot: "/media/Library/M18", KavitaLibID: 2, DefaultIsAdult: true},
		},
	}
	h := newTestHandler(t, st)

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	body := rec.Body.String()

	// Card heading present
	if !strings.Contains(body, "Library Bindings") {
		t.Errorf("expected 'Library Bindings' heading in rendered HTML")
	}
	// Both bindings present with their names + roots
	for _, want := range []string{"Manga", "/media/Library/Manga", "Manhwa 18+", "/media/Library/M18"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in rendered Library Bindings card", want)
		}
	}
	// DefaultIsAdult checkbox: the 18+ row should have a checked checkbox
	// for binding ID 2.
	if !strings.Contains(body, `name="binding_default_is_adult_2" checked`) &&
		!strings.Contains(body, `name="binding_default_is_adult_2" value="on" checked`) {
		t.Errorf("expected DefaultIsAdult checkbox to be checked for binding ID 2")
	}
}

func TestSettingsPageRendersAddBindingAffordance(t *testing.T) {
	st := &fakeStore{}
	h := newTestHandler(t, st)
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "+ Add Binding") {
		t.Errorf("expected '+ Add Binding' affordance in rendered HTML")
	}
}
```

- [ ] **Step 3: Run, verify failure**

```bash
go test ./internal/web/ -run TestSettingsPageRendersLibraryBindings -v
```

Expected: FAIL — section not present.

- [ ] **Step 4: Extend pageSettings to load bindings + Kavita library options**

In `internal/web/web.go`, find `pageSettings` (the GET /settings handler). Add a step that loads bindings + Kavita libraries similar to how the Library Map Plan C handler already loads Kavita libs for the override-row dropdown:

```go
bindings, err := h.store.ListBindings()
if err != nil {
    bindings = nil // best-effort; the card renders empty state
}
// Reuse the existing Kavita library fetch from pageSettings — there's
// already a code path that fetches Kavita libraries for the v1 Kavita
// Libraries picker / Suwayomi override dropdown. Reuse those results.
// If the existing code stashes them in a local variable (likely
// kavitaLibs []kavita.Library or similar), pass the same slice to the
// new binding card.
```

Add these fields to whatever `settingsPageData` struct is used (or whatever it's called in this codebase):

```go
Bindings    []model.Binding
KavitaLibs  []kavita.Library // already exists; just reference it from the bindings template
```

- [ ] **Step 5: Create the binding-rows template partial**

Create `internal/web/templates/binding-rows.html`:
```html
{{define "binding-rows"}}
{{range .Bindings}}
<div class="settings-row binding-row" data-binding-id="{{.ID}}">
  <input type="hidden" name="binding_id_{{.ID}}" value="{{.ID}}">
  <input type="text" name="binding_name_{{.ID}}" value="{{.Name}}" placeholder="Name (e.g. Manga, Manhwa 18+)" class="binding-name">
  <input type="text" name="binding_library_root_{{.ID}}" value="{{.LibraryRoot}}" placeholder="/media/Library/..." class="binding-root">
  <select name="binding_kavita_lib_{{.ID}}" class="binding-kavita">
    <option value="0">(none)</option>
    {{$current := .KavitaLibID}}
    {{range $.KavitaLibs}}<option value="{{.ID}}"{{if eq .ID $current}} selected{{end}}>{{.Name}}</option>{{end}}
  </select>
  <label class="binding-adult">
    <input type="checkbox" name="binding_default_is_adult_{{.ID}}"{{if .DefaultIsAdult}} checked{{end}}>
    18+
  </label>
  <button type="button" class="btn-delete" onclick="this.closest('.binding-row').remove()" title="Delete binding">×</button>
</div>
{{end}}
{{end}}
```

The form-field naming convention `binding_<column>_<bindingID>` lets the POST handler parse the form by iterating over a set of IDs gathered from `binding_id_*` fields. New rows (added client-side via JS) use a JS-generated unique suffix instead of an ID; the POST handler treats unknown suffixes as ID=0 (new row).

- [ ] **Step 6: Add the card to settings.html**

In `internal/web/templates/settings.html`, find the existing "Kavita Libraries" or "Library Map" section. ADD the new Library Bindings card BEFORE it (Task 9 will delete the old sections later). Use this skeleton:

```html
<div class="card">
  <div class="card-header">
    <h3>Library Bindings</h3>
    <button type="button" class="btn-add" onclick="addBindingRow()">+ Add Binding</button>
  </div>
  <div class="card-body">
    <div class="binding-rows" id="binding-rows">
      {{template "binding-rows" .}}
    </div>
    {{if not .Bindings}}
    <p class="empty-state">No bindings configured. Click <b>+ Add Binding</b> above to create your first library destination.</p>
    {{end}}
  </div>
</div>

<template id="binding-row-template">
  <div class="settings-row binding-row" data-binding-id="0">
    <input type="hidden" name="binding_id___NEW_IDX__" value="0">
    <input type="text" name="binding_name___NEW_IDX__" value="" placeholder="Name (e.g. Manga, Manhwa 18+)" class="binding-name">
    <input type="text" name="binding_library_root___NEW_IDX__" value="" placeholder="/media/Library/..." class="binding-root">
    <select name="binding_kavita_lib___NEW_IDX__" class="binding-kavita">
      <option value="0">(none)</option>
      {{range .KavitaLibs}}<option value="{{.ID}}">{{.Name}}</option>{{end}}
    </select>
    <label class="binding-adult">
      <input type="checkbox" name="binding_default_is_adult___NEW_IDX__">
      18+
    </label>
    <button type="button" class="btn-delete" onclick="this.closest('.binding-row').remove()" title="Delete binding">×</button>
  </div>
</template>

<script>
let bindingIdxCounter = 0;
function addBindingRow() {
  bindingIdxCounter++;
  const tmpl = document.getElementById('binding-row-template');
  const idx = 'new' + bindingIdxCounter;
  const html = tmpl.innerHTML.replaceAll('__NEW_IDX__', idx);
  document.getElementById('binding-rows').insertAdjacentHTML('beforeend', html);
}
</script>
```

The `__NEW_IDX__` token is replaced client-side when Add Binding is clicked. Each new row uses a unique suffix so its form fields don't collide.

- [ ] **Step 7: Wire the template partial in the page render**

The template loader probably uses `template.ParseFS` or `template.ParseGlob` over `internal/web/templates/*.html`. The new `binding-rows.html` file should be picked up automatically if the loader glob includes it. Verify by reading the template-loading code (likely in `internal/web/web.go` near a `template.New("settings")` or similar).

If the loader is explicit-list-of-files, add `binding-rows.html` to the list.

- [ ] **Step 8: Run, verify pass**

```bash
go test ./internal/web/ -run TestSettingsPageRendersLibraryBindings -v -race
go test ./internal/web/... -count=1 -race
go build ./...
go vet ./...
```

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/web/web.go internal/web/templates/settings.html internal/web/templates/binding-rows.html internal/web/bindings_test.go
touch /home/gavin/my_other_repos/mangarr/.claude/.verified
git -c commit.gpgsign=false commit -m "feat(web): Library Bindings card with CRUD-friendly form

Renders existing bindings as form rows (Name, Library Root, Kavita
library dropdown, DefaultIsAdult checkbox, Delete affordance).
'+ Add Binding' appends a new empty row client-side via a <template>
element with __NEW_IDX__ placeholder substitution. The actual POST
handler that consumes the form fields lands in Task 5."
```

---

### Task 3: Classification Rules card — priority-ordered editor

Per-rule row: priority number, name, four condition widgets (country / adult / format / path prefix), target binding dropdown, delete. Add Rule appends an empty row.

**Files:**
- Modify: `internal/web/web.go` (extend `pageSettings` to load rules + populate target-binding choices)
- Modify: `internal/web/templates/settings.html` (add Classification Rules card)
- Create: `internal/web/templates/rule-rows.html` (template partial)
- Modify: `internal/web/bindings_test.go` (add render tests)

- [ ] **Step 1: Write failing tests**

Append to `internal/web/bindings_test.go`:
```go
func TestSettingsPageRendersClassificationRulesCard(t *testing.T) {
	jp := "JP"
	yes := true
	st := &fakeStore{
		bindings: []model.Binding{{ID: 1, Name: "Manga"}, {ID: 2, Name: "Manga 18+"}},
		rules: []model.ClassificationRule{
			{ID: 10, Priority: 50, Name: "Japanese 18+", Condition: model.RuleCondition{CountryOfOrigin: &jp, IsAdult: &yes}, BindingID: 2},
			{ID: 11, Priority: 100, Name: "Japanese", Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: 1},
		},
	}
	h := newTestHandler(t, st)

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, "Classification Rules") {
		t.Errorf("expected 'Classification Rules' heading")
	}
	for _, want := range []string{"Japanese 18+", "Japanese", "+ Add Rule"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in rendered Classification Rules card", want)
		}
	}
}

func TestSettingsPageRulesSortedByPriorityAscending(t *testing.T) {
	jp := "JP"
	st := &fakeStore{
		bindings: []model.Binding{{ID: 1, Name: "Manga"}},
		rules: []model.ClassificationRule{
			// fakeStore should preserve ListRules ORDER BY priority semantics
			{ID: 1, Priority: 100, Name: "A", Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: 1},
			{ID: 2, Priority: 200, Name: "B", Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: 1},
			{ID: 3, Priority: 50,  Name: "C", Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: 1},
		},
	}
	h := newTestHandler(t, st)
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	// Find positions of each rule's name in the HTML; C (priority 50)
	// must appear before A (priority 100) which must appear before B
	// (priority 200).
	cPos := strings.Index(body, ">C<")
	aPos := strings.Index(body, ">A<")
	bPos := strings.Index(body, ">B<")
	if cPos == -1 || aPos == -1 || bPos == -1 {
		t.Fatalf("expected all three rule names in HTML; got positions C=%d A=%d B=%d", cPos, aPos, bPos)
	}
	if !(cPos < aPos && aPos < bPos) {
		t.Errorf("expected C before A before B by priority; got positions C=%d A=%d B=%d", cPos, aPos, bPos)
	}
}
```

The `fakeStore.rules` slice should be returned by `ListRules()` in the order it's stored. If your fakeStore sorts, the test still works because the input is already ascending. If your fakeStore doesn't sort, this test pins that the production code's `ListRules` IS what surfaces the order.

- [ ] **Step 2: Run, verify failure**

```bash
go test ./internal/web/ -run 'TestSettingsPageRendersClassificationRules|TestSettingsPageRulesSortedByPriority' -v
```

Expected: FAIL — section not present.

- [ ] **Step 3: Extend pageSettings**

In `internal/web/web.go`'s `pageSettings`, after loading bindings, also load rules:

```go
rules, err := h.store.ListRules()
if err != nil {
    rules = nil
}
// stash in settingsPageData:
//   Rules []model.ClassificationRule
```

- [ ] **Step 4: Create the rule-rows template partial**

Create `internal/web/templates/rule-rows.html`:
```html
{{define "rule-rows"}}
{{range .Rules}}
<div class="settings-row rule-row" data-rule-id="{{.ID}}">
  <input type="hidden" name="rule_id_{{.ID}}" value="{{.ID}}">
  <input type="number" name="rule_priority_{{.ID}}" value="{{.Priority}}" min="0" max="9999" class="rule-priority" title="Lower number = higher priority">
  <input type="text" name="rule_name_{{.ID}}" value="{{.Name}}" placeholder="Rule name" class="rule-name">

  <div class="rule-conditions">
    <label>country
      <select name="rule_country_{{.ID}}">
        <option value=""{{if not .Condition.CountryOfOrigin}} selected{{end}}>Any</option>
        {{$cur := ""}}{{if .Condition.CountryOfOrigin}}{{$cur = .Condition.CountryOfOrigin}}{{end}}
        <option value="JP"{{if eq $cur "JP"}} selected{{end}}>JP</option>
        <option value="KR"{{if eq $cur "KR"}} selected{{end}}>KR</option>
        <option value="CN"{{if eq $cur "CN"}} selected{{end}}>CN</option>
        <option value="TW"{{if eq $cur "TW"}} selected{{end}}>TW</option>
      </select>
    </label>

    <label>adult
      <select name="rule_adult_{{.ID}}">
        <option value=""{{if not .Condition.IsAdult}} selected{{end}}>Any</option>
        <option value="yes"{{if and .Condition.IsAdult (eq (deref .Condition.IsAdult) true)}} selected{{end}}>Yes</option>
        <option value="no"{{if and .Condition.IsAdult (eq (deref .Condition.IsAdult) false)}} selected{{end}}>No</option>
      </select>
    </label>

    <label>format
      <select name="rule_format_{{.ID}}">
        <option value=""{{if not .Condition.Format}} selected{{end}}>Any</option>
        {{$fcur := ""}}{{if .Condition.Format}}{{$fcur = .Condition.Format}}{{end}}
        <option value="MANGA"{{if eq $fcur "MANGA"}} selected{{end}}>Manga</option>
        <option value="NOVEL"{{if eq $fcur "NOVEL"}} selected{{end}}>Novel</option>
        <option value="ONE_SHOT"{{if eq $fcur "ONE_SHOT"}} selected{{end}}>One-shot</option>
      </select>
    </label>

    <label>path
      <input type="text" name="rule_path_{{.ID}}" value="{{if .Condition.SourcePathPrefix}}{{.Condition.SourcePathPrefix}}{{end}}" placeholder="/media/Downloads/..." class="rule-path">
    </label>
  </div>

  <label>→ binding
    <select name="rule_binding_{{.ID}}" class="rule-binding">
      <option value="0">— pick —</option>
      {{$current := .BindingID}}
      {{range $.Bindings}}<option value="{{.ID}}"{{if eq .ID $current}} selected{{end}}>{{.Name}}</option>{{end}}
      {{if not (bindingExists $.Bindings .BindingID)}}
        <option value="{{.BindingID}}" selected>Unknown binding (ID: {{.BindingID}})</option>
      {{end}}
    </select>
  </label>

  <button type="button" class="btn-delete" onclick="this.closest('.rule-row').remove()" title="Delete rule">×</button>
</div>
{{end}}
{{end}}
```

The template uses `deref` and `bindingExists` template funcs that don't exist yet. Either add them as template helpers (preferred) OR rework the comparisons to use field-by-field nil checks.

To add the funcs, find where the template is constructed in `internal/web/web.go` (probably `template.New("...").Funcs(...).ParseFS(...)` or similar) and add:

```go
funcMap := template.FuncMap{
    // ... existing funcs ...
    "deref": func(b *bool) bool {
        if b == nil { return false }
        return *b
    },
    "bindingExists": func(bindings []model.Binding, id int64) bool {
        for _, b := range bindings {
            if b.ID == id { return true }
        }
        return false
    },
}
```

If the existing template setup uses a different shape, match it.

- [ ] **Step 5: Add the card to settings.html**

In `internal/web/templates/settings.html`, ADD a new Classification Rules card immediately AFTER the Library Bindings card from Task 2:

```html
<div class="card">
  <div class="card-header">
    <h3>Classification Rules</h3>
    <button type="button" class="btn-add" onclick="addRuleRow()">+ Add Rule</button>
  </div>
  <div class="card-body">
    <p class="card-hint">Rules are evaluated top-to-bottom by Priority (lower number = higher priority). First matching rule wins.</p>
    <div class="rule-rows" id="rule-rows">
      {{template "rule-rows" .}}
    </div>
    {{if not .Rules}}
    <p class="empty-state">No rules configured. Click <b>+ Add Rule</b> above to define how series get classified.</p>
    {{end}}
  </div>
</div>

<template id="rule-row-template">
  <div class="settings-row rule-row" data-rule-id="0">
    <input type="hidden" name="rule_id___NEW_IDX__" value="0">
    <input type="number" name="rule_priority___NEW_IDX__" value="1000" min="0" max="9999" class="rule-priority" title="Lower number = higher priority">
    <input type="text" name="rule_name___NEW_IDX__" value="" placeholder="Rule name" class="rule-name">
    <div class="rule-conditions">
      <label>country
        <select name="rule_country___NEW_IDX__">
          <option value="" selected>Any</option>
          <option value="JP">JP</option>
          <option value="KR">KR</option>
          <option value="CN">CN</option>
          <option value="TW">TW</option>
        </select>
      </label>
      <label>adult
        <select name="rule_adult___NEW_IDX__">
          <option value="" selected>Any</option>
          <option value="yes">Yes</option>
          <option value="no">No</option>
        </select>
      </label>
      <label>format
        <select name="rule_format___NEW_IDX__">
          <option value="" selected>Any</option>
          <option value="MANGA">Manga</option>
          <option value="NOVEL">Novel</option>
          <option value="ONE_SHOT">One-shot</option>
        </select>
      </label>
      <label>path
        <input type="text" name="rule_path___NEW_IDX__" value="" placeholder="/media/Downloads/..." class="rule-path">
      </label>
    </div>
    <label>→ binding
      <select name="rule_binding___NEW_IDX__" class="rule-binding">
        <option value="0">— pick —</option>
        {{range .Bindings}}<option value="{{.ID}}">{{.Name}}</option>{{end}}
      </select>
    </label>
    <button type="button" class="btn-delete" onclick="this.closest('.rule-row').remove()" title="Delete rule">×</button>
  </div>
</template>

<script>
let ruleIdxCounter = 0;
function addRuleRow() {
  ruleIdxCounter++;
  const tmpl = document.getElementById('rule-row-template');
  const idx = 'new' + ruleIdxCounter;
  const html = tmpl.innerHTML.replaceAll('__NEW_IDX__', idx);
  document.getElementById('rule-rows').insertAdjacentHTML('beforeend', html);
}
</script>
```

- [ ] **Step 6: Run, verify pass**

```bash
go test ./internal/web/ -run 'TestSettingsPageRendersClassificationRules|TestSettingsPageRulesSortedByPriority' -v -race
go test ./internal/web/... -count=1 -race
go build ./...
go vet ./...
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/web/web.go internal/web/templates/settings.html internal/web/templates/rule-rows.html internal/web/bindings_test.go
touch /home/gavin/my_other_repos/mangarr/.claude/.verified
git -c commit.gpgsign=false commit -m "feat(web): Classification Rules editor card

Per-rule row: priority (lower = higher precedence), name, four
condition widgets (country / adult / format / path-prefix), target
binding dropdown, delete. '+ Add Rule' appends an empty row.
Rules render sorted ascending by Priority. Deleted target bindings
render as 'Unknown binding (ID: N)' and stay editable. POST handler
that consumes the form fields lands in Task 5."
```

---

### Task 4: Default Binding picker + Suwayomi Category Overrides widening

Two small additions: a Default Binding dropdown (the no-match fallback) and the existing Suwayomi Category Overrides card's right-hand dropdown widens from "3 primary Kavita libraries" to "all bindings".

**Files:**
- Modify: `internal/web/web.go` (extend `pageSettings` to populate DefaultBinding selection)
- Modify: `internal/web/templates/settings.html` (add Default Binding picker)
- Modify: `internal/web/templates/override-rows.html` (widen the binding dropdown)
- Modify: `internal/web/bindings_test.go` (add render tests)

- [ ] **Step 1: Write failing tests**

Append to `internal/web/bindings_test.go`:
```go
func TestSettingsPageRendersDefaultBindingPicker(t *testing.T) {
	id := int64(2)
	st := &fakeStore{
		bindings: []model.Binding{{ID: 1, Name: "Manga"}, {ID: 2, Name: "Catch-all"}},
		settings: model.Settings{DefaultBindingID: &id},
	}
	h := newTestHandler(t, st)
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, "Default Binding") {
		t.Errorf("expected 'Default Binding' label in rendered HTML")
	}
	// Catch-all binding (ID 2) should be the selected option.
	if !strings.Contains(body, `value="2" selected`) {
		t.Errorf("expected DefaultBindingID 2 to be the selected option")
	}
	// The 'Send to Unmatched' option should be present (nil-default path).
	if !strings.Contains(body, "Send to Unmatched") {
		t.Errorf("expected '— Send to Unmatched —' option in dropdown")
	}
}

func TestSuwayomiOverridesDropdownIncludesAllBindings(t *testing.T) {
	st := &fakeStore{
		bindings: []model.Binding{
			{ID: 1, Name: "Manga"},
			{ID: 2, Name: "Manga 18+"},
			{ID: 3, Name: "Light Novels"}, // explicitly NON-primary; v1 would have excluded it
		},
		settings: model.Settings{
			SuwayomiBaseURL:          "http://suwayomi.example:4567",
			SuwayomiCategoryBindings: map[int64]int64{10: 1},
		},
	}
	h := newTestHandler(t, st)
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	// All three bindings must appear in the override-row dropdown.
	for _, want := range []string{
		`>Manga<`,
		`>Manga 18+<`,
		`>Light Novels<`, // CRUCIAL: this was filtered out in Library Map Plan C
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in widened override-row dropdown", want)
		}
	}
}
```

- [ ] **Step 2: Run, verify failure**

```bash
go test ./internal/web/ -run 'TestSettingsPageRendersDefaultBindingPicker|TestSuwayomiOverridesDropdownIncludesAllBindings' -v
```

Expected: FAIL.

- [ ] **Step 3: Add the Default Binding picker to settings.html**

In `internal/web/templates/settings.html`, immediately AFTER the Classification Rules card from Task 3:

```html
<div class="card">
  <div class="card-header">
    <h3>Default Binding</h3>
  </div>
  <div class="card-body">
    <p class="card-hint">When no classification rule and no Suwayomi override matches a series, route it here. Leave as <b>Send to Unmatched</b> to hold series for manual classification (safer; matches pre-v2 behaviour).</p>
    <label>
      Fallback target:
      <select name="default_binding_id">
        {{$cur := int64Or .Settings.DefaultBindingID 0}}
        <option value="0"{{if eq $cur 0}} selected{{end}}>— Send to Unmatched —</option>
        {{range .Bindings}}<option value="{{.ID}}"{{if eq .ID $cur}} selected{{end}}>{{.Name}}</option>{{end}}
      </select>
    </label>
  </div>
</div>
```

Add the `int64Or` template func to the FuncMap (it nil-safely dereferences `*int64`):
```go
"int64Or": func(p *int64, fallback int64) int64 {
    if p == nil { return fallback }
    return *p
},
```

- [ ] **Step 4: Widen the override-rows dropdown**

The existing `internal/web/templates/override-rows.html` (from Library Map Plan C) currently filters the right-hand dropdown to "3 primary Kavita libraries" via a `overrideLibraryChoices` helper or similar. The widened version uses `.Bindings` (all bindings) directly.

Open `internal/web/templates/override-rows.html` and find the `<select name="override_library_...">`. Replace its option-rendering loop:

Current shape (probably):
```html
{{range .OverrideLibChoices}}<option value="{{.ID}}"...>{{.Label}}</option>{{end}}
```

New shape:
```html
{{$cur := .BindingID}}
{{range $.AllBindings}}<option value="{{.ID}}"{{if eq .ID $cur}} selected{{end}}>{{.Name}}</option>{{end}}
{{if not (bindingExists $.AllBindings .BindingID)}}
  <option value="{{.BindingID}}" selected>Unknown binding (ID: {{.BindingID}})</option>
{{end}}
```

The actual field names depend on whether the current code uses `OverrideRow.BindingID` or `OverrideRow.LibraryID` — match what's there. If the override row's BindingID field is currently a Kavita library ID (from Library Map Plan C), this is Task 5's job to migrate to the v2 `SuwayomiCategoryBindings` shape; the template doesn't care which it is as long as `Bindings[].ID` matches.

The page-data setup in `pageSettings` needs to pass `.AllBindings` (or whatever the field is named — pick consistently with the other Plan B tasks).

Also: the existing override-row form-field for the right-hand value needs renaming from `override_library_<idx>` to `override_binding_<idx>` to reflect v2 semantics. Task 5 hands its POST handler.

- [ ] **Step 5: Run, verify pass**

```bash
go test ./internal/web/ -run 'TestSettingsPageRendersDefaultBindingPicker|TestSuwayomiOverridesDropdownIncludesAllBindings' -v -race
go test ./internal/web/... -count=1 -race
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/web/web.go internal/web/templates/settings.html internal/web/templates/override-rows.html internal/web/bindings_test.go
touch /home/gavin/my_other_repos/mangarr/.claude/.verified
git -c commit.gpgsign=false commit -m "feat(web): Default Binding picker + widen Suwayomi overrides to all bindings

Default Binding dropdown defaults to '— Send to Unmatched —' (nil
DefaultBindingID, matches pre-v2 behaviour). Selecting any binding
auto-routes the no-match fallback there.

Suwayomi Category Overrides right-hand dropdown now lists every
binding (was: only 3 primary content-type Kavita libraries per the
Library Map Plan C reverse-lookup constraint, which Plan A dissolved).
Override rows pointing at deleted bindings render as 'Unknown binding
(ID: N)' and stay editable.

POST handler that consumes the new shape lands in Task 5."
```

---

### Task 5: Atomic form POST — parse + persist bindings + rules + default + overrides

The load-bearing wire-up. Replaces the form-fields path of the existing POST `/settings` handler. All four new shapes (Bindings, Rules, DefaultBindingID, SuwayomiCategoryBindings) are persisted in one logical operation; on validation error the form re-renders with errors.

**Files:**
- Modify: `internal/web/web.go` (extend `saveSettings`)
- Create: `internal/web/save.go` (new file for the parsing logic; keeps web.go from ballooning)
- Modify: `internal/web/bindings_test.go` (add E2E POST tests)

- [ ] **Step 1: Write failing E2E tests**

Append to `internal/web/bindings_test.go`:
```go
func TestPOSTSettingsPersistsNewBindings(t *testing.T) {
	st := &fakeStore{}
	h := newTestHandler(t, st)

	form := url.Values{}
	// Existing v1 fields that the current handler reads — keep them
	// populated so the existing flow doesn't error.
	form.Set("file_mode", "hardlink")
	form.Set("rename_scheme", "{series}/{series} - Ch.{chapter}.cbz")
	form.Set("poll_minutes", "15")
	// Two new bindings, both with ID=0 (new). The form-field naming
	// convention is binding_<column>_<suffix> where suffix is either
	// the existing ID or a JS-generated new id.
	form.Set("binding_id_new1", "0")
	form.Set("binding_name_new1", "Manga")
	form.Set("binding_library_root_new1", "/media/Library/Manga")
	form.Set("binding_kavita_lib_new1", "1")
	// no binding_default_is_adult_new1 → unchecked
	form.Set("binding_id_new2", "0")
	form.Set("binding_name_new2", "Manhwa 18+")
	form.Set("binding_library_root_new2", "/media/Library/M18")
	form.Set("binding_kavita_lib_new2", "2")
	form.Set("binding_default_is_adult_new2", "on")

	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusOK {
		t.Fatalf("status: want 303 or 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	bindings, _ := st.ListBindings()
	if len(bindings) != 2 {
		t.Fatalf("want 2 bindings persisted, got %d (%+v)", len(bindings), bindings)
	}
	// Order is by Name; "Manga" before "Manhwa 18+"
	if bindings[0].Name != "Manga" || bindings[1].Name != "Manhwa 18+" {
		t.Errorf("bindings persisted in wrong order or names: %+v", bindings)
	}
	if bindings[1].DefaultIsAdult != true {
		t.Errorf("expected Manhwa 18+ to have DefaultIsAdult=true, got %+v", bindings[1])
	}
}

func TestPOSTSettingsPersistsRules(t *testing.T) {
	st := &fakeStore{
		bindings: []model.Binding{{ID: 1, Name: "Manga"}}, // seeded for FK
	}
	h := newTestHandler(t, st)

	form := url.Values{}
	form.Set("file_mode", "hardlink")
	form.Set("rule_id_new1", "0")
	form.Set("rule_priority_new1", "100")
	form.Set("rule_name_new1", "Japanese")
	form.Set("rule_country_new1", "JP")
	form.Set("rule_binding_new1", "1")

	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusOK {
		t.Fatalf("status: want 303 or 200, got %d", rec.Code)
	}

	rules, _ := st.ListRules()
	if len(rules) != 1 || rules[0].Priority != 100 || rules[0].Name != "Japanese" {
		t.Errorf("rule not persisted as expected: %+v", rules)
	}
	if rules[0].Condition.CountryOfOrigin == nil || *rules[0].Condition.CountryOfOrigin != "JP" {
		t.Errorf("rule CountryOfOrigin lost: %+v", rules[0].Condition)
	}
}

func TestPOSTSettingsPersistsDefaultBindingID(t *testing.T) {
	st := &fakeStore{
		bindings: []model.Binding{{ID: 5, Name: "Catch-all"}},
	}
	h := newTestHandler(t, st)

	form := url.Values{}
	form.Set("file_mode", "hardlink")
	form.Set("default_binding_id", "5")

	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	got, _ := st.GetSettings()
	if got.DefaultBindingID == nil || *got.DefaultBindingID != 5 {
		t.Errorf("expected DefaultBindingID 5 after POST, got %v", got.DefaultBindingID)
	}
}

func TestPOSTSettingsPersistsSuwayomiOverridesToV2Field(t *testing.T) {
	st := &fakeStore{
		bindings: []model.Binding{{ID: 7, Name: "Light Novels"}},
	}
	h := newTestHandler(t, st)

	form := url.Values{}
	form.Set("file_mode", "hardlink")
	// Existing v1 override-row field naming was override_category_<idx>
	// + override_library_<idx>. v2 renames the right-hand to
	// override_binding_<idx>; verify Plan B writes to
	// SuwayomiCategoryBindings, NOT the v1 SuwayomiCategoryOverrides.
	form.Set("override_category_new1", "42")
	form.Set("override_binding_new1", "7")

	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	got, _ := st.GetSettings()
	if got.SuwayomiCategoryBindings[42] != 7 {
		t.Errorf("expected SuwayomiCategoryBindings[42] = 7, got %v", got.SuwayomiCategoryBindings)
	}
	// The v1 SuwayomiCategoryOverrides map is preserved by Migration 2;
	// Plan B's POST should NOT add new entries there. (It's left alone.)
}

func TestPOSTSettingsEmptyDefaultBindingClearsTheField(t *testing.T) {
	id := int64(5)
	st := &fakeStore{
		bindings: []model.Binding{{ID: 5, Name: "Catch-all"}},
		settings: model.Settings{DefaultBindingID: &id},
	}
	h := newTestHandler(t, st)

	form := url.Values{}
	form.Set("file_mode", "hardlink")
	form.Set("default_binding_id", "0") // 0 → nil

	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	got, _ := st.GetSettings()
	if got.DefaultBindingID != nil {
		t.Errorf("expected DefaultBindingID nil after POST with 0, got %v", *got.DefaultBindingID)
	}
}
```

If `fakeStore` doesn't have `SaveBindings`/`SaveRules`/`SaveSettings` methods that persist back to its own state, add them so these tests can assert the round-trip. The real `*store.Store` already has them.

- [ ] **Step 2: Run, verify failure**

```bash
go test ./internal/web/ -run 'TestPOSTSettingsPersists|TestPOSTSettingsEmptyDefault' -v
```

Expected: FAIL — form parsing not implemented.

- [ ] **Step 3: Implement the parsers**

Create `internal/web/save.go`:
```go
package web

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/gavinmcfall/mangarr/internal/model"
)

var bindingKeyRE = regexp.MustCompile(`^binding_id_(.+)$`)
var ruleKeyRE = regexp.MustCompile(`^rule_id_(.+)$`)
var overrideKeyRE = regexp.MustCompile(`^override_category_(.+)$`)

// parseBindingsFromForm walks the form and reconstructs a []model.Binding.
// Each binding's row uses suffix S: binding_id_S, binding_name_S,
// binding_library_root_S, binding_kavita_lib_S, binding_default_is_adult_S.
// Rows with both Name and LibraryRoot empty are dropped (treated as
// abandoned edits).
func parseBindingsFromForm(r *http.Request) ([]model.Binding, error) {
	if err := r.ParseForm(); err != nil {
		return nil, fmt.Errorf("parse form: %w", err)
	}
	suffixes := collectSuffixes(r.Form, bindingKeyRE)
	out := make([]model.Binding, 0, len(suffixes))
	for _, s := range suffixes {
		name := strings.TrimSpace(r.FormValue("binding_name_" + s))
		root := strings.TrimSpace(r.FormValue("binding_library_root_" + s))
		if name == "" && root == "" {
			continue // abandoned row
		}
		id, _ := strconv.ParseInt(r.FormValue("binding_id_" + s), 10, 64)
		kavitaLib, _ := strconv.ParseInt(r.FormValue("binding_kavita_lib_" + s), 10, 64)
		isAdult := r.FormValue("binding_default_is_adult_" + s) != ""
		out = append(out, model.Binding{
			ID:             id,
			Name:           name,
			LibraryRoot:    root,
			KavitaLibID:    kavitaLib,
			DefaultIsAdult: isAdult,
		})
	}
	return out, nil
}

// parseRulesFromForm walks the form and reconstructs a []model.ClassificationRule.
// Each rule's row uses suffix S: rule_id_S, rule_priority_S, rule_name_S,
// rule_country_S, rule_adult_S, rule_format_S, rule_path_S, rule_binding_S.
// Rows where BindingID == 0 OR all four conditions are empty are dropped.
func parseRulesFromForm(r *http.Request) ([]model.ClassificationRule, error) {
	suffixes := collectSuffixes(r.Form, ruleKeyRE)
	out := make([]model.ClassificationRule, 0, len(suffixes))
	for _, s := range suffixes {
		bindingID, _ := strconv.ParseInt(r.FormValue("rule_binding_" + s), 10, 64)
		if bindingID == 0 {
			continue
		}
		id, _ := strconv.ParseInt(r.FormValue("rule_id_" + s), 10, 64)
		priority, _ := strconv.Atoi(r.FormValue("rule_priority_" + s))
		name := strings.TrimSpace(r.FormValue("rule_name_" + s))

		cond := model.RuleCondition{}
		if c := strings.TrimSpace(r.FormValue("rule_country_" + s)); c != "" {
			cond.CountryOfOrigin = &c
		}
		switch r.FormValue("rule_adult_" + s) {
		case "yes":
			t := true
			cond.IsAdult = &t
		case "no":
			f := false
			cond.IsAdult = &f
		}
		if f := strings.TrimSpace(r.FormValue("rule_format_" + s)); f != "" {
			cond.Format = &f
		}
		if p := strings.TrimSpace(r.FormValue("rule_path_" + s)); p != "" {
			cond.SourcePathPrefix = &p
		}

		// Universal-wildcard rule (no condition AT ALL) → reject (per spec).
		// The user should use DefaultBindingID for "catch-all" semantics.
		if cond.CountryOfOrigin == nil && cond.IsAdult == nil &&
			cond.Format == nil && cond.SourcePathPrefix == nil {
			continue
		}

		out = append(out, model.ClassificationRule{
			ID:        id,
			Priority:  priority,
			Name:      name,
			Condition: cond,
			BindingID: bindingID,
		})
	}
	return out, nil
}

// parseSuwayomiOverridesFromForm rebuilds the v2 SuwayomiCategoryBindings
// map. Form-field naming: override_category_<suffix> (the category ID) +
// override_binding_<suffix> (the v2 Binding ID).
// Rows with either field empty/zero are dropped.
func parseSuwayomiOverridesFromForm(r *http.Request) map[int64]int64 {
	suffixes := collectSuffixes(r.Form, overrideKeyRE)
	if len(suffixes) == 0 {
		return nil
	}
	out := make(map[int64]int64, len(suffixes))
	for _, s := range suffixes {
		cat, _ := strconv.ParseInt(r.FormValue("override_category_" + s), 10, 64)
		binding, _ := strconv.ParseInt(r.FormValue("override_binding_" + s), 10, 64)
		if cat == 0 || binding == 0 {
			continue
		}
		out[cat] = binding
	}
	return out
}

// parseDefaultBindingIDFromForm returns nil for "0" (the "Send to
// Unmatched" sentinel) or a pointer to the parsed int64.
func parseDefaultBindingIDFromForm(r *http.Request) *int64 {
	v := strings.TrimSpace(r.FormValue("default_binding_id"))
	if v == "" || v == "0" {
		return nil
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id <= 0 {
		return nil
	}
	return &id
}

// collectSuffixes finds every form-key suffix matching the given regex.
// Sorted for deterministic iteration so duplicate-by-mistake form keys
// resolve the same way every time (last-wins by sorted-suffix order).
func collectSuffixes(form map[string][]string, re *regexp.Regexp) []string {
	seen := make(map[string]bool)
	for k := range form {
		if m := re.FindStringSubmatch(k); m != nil {
			seen[m[1]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	// Sort by numeric suffix when possible (for "new1", "new2", ... ordering),
	// fall back to lexicographic. Simple lex sort is fine for now.
	sort.Strings(out)
	return out
}
```

Add `"sort"` to the imports.

In `internal/web/web.go`, find the existing `saveSettings` handler. After the existing v1-field parsing but BEFORE the persist, add:

```go
bindings, err := parseBindingsFromForm(r)
if err != nil {
    http.Error(w, "parse bindings: "+err.Error(), http.StatusBadRequest)
    return
}
rules, err := parseRulesFromForm(r)
if err != nil {
    http.Error(w, "parse rules: "+err.Error(), http.StatusBadRequest)
    return
}
defaultBindingID := parseDefaultBindingIDFromForm(r)
overrides := parseSuwayomiOverridesFromForm(r)

// Persist the v2 surfaces. SaveBindings + SaveRules are each atomic
// (single-tx replace-all from Plan A). The Settings round-trip writes
// DefaultBindingID and SuwayomiCategoryBindings into the singleton
// settings row.
if err := h.store.SaveBindings(bindings); err != nil {
    http.Error(w, "save bindings: "+err.Error(), http.StatusInternalServerError)
    return
}
if err := h.store.SaveRules(rules); err != nil {
    http.Error(w, "save rules: "+err.Error(), http.StatusInternalServerError)
    return
}
settings, _ := h.store.GetSettings()
settings.DefaultBindingID = defaultBindingID
settings.SuwayomiCategoryBindings = overrides
if err := h.store.SaveSettings(settings); err != nil {
    http.Error(w, "save settings: "+err.Error(), http.StatusInternalServerError)
    return
}
```

Place this AFTER any existing v1-field parsing but before the existing redirect/render. The order matters: SaveBindings must happen before SaveRules (so the rule's BindingID FK is valid against the just-saved rows).

Note: SaveBindings + SaveRules + SaveSettings are three separate transactions, not one. Atomicity within each method holds; cross-method atomicity isn't strictly necessary for this scope (the worst case is a partial save mid-failure leaves the user with bindings but no rules — the next form save corrects). If true cross-method atomicity is wanted later, the store can grow a `SaveAll(bindings, rules, settings) error` wrapper. Out of scope for Plan B.

- [ ] **Step 4: Run, verify pass**

```bash
go test ./internal/web/ -run 'TestPOSTSettings' -v -race
go test ./internal/web/... -count=1 -race
go build ./...
go vet ./...
```

Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/web/save.go internal/web/web.go internal/web/bindings_test.go
touch /home/gavin/my_other_repos/mangarr/.claude/.verified
git -c commit.gpgsign=false commit -m "feat(web): atomic POST persists bindings + rules + default + Suwayomi overrides

POST /settings now parses the four new form-field shapes and writes:
- Library Bindings via store.SaveBindings (atomic replace-all)
- Classification Rules via store.SaveRules (atomic replace-all)
- DefaultBindingID via Settings round-trip
- SuwayomiCategoryBindings (v2 map) via Settings round-trip

Universal-wildcard rules (no condition set) and rows with empty
Name+LibraryRoot are dropped at parse time so abandoned edits
don't pollute the persisted state. Rows with BindingID=0 in rules
or override_binding=0 in Suwayomi overrides are dropped as
intent-incomplete.

Plan A→B boundary: this commit makes Suwayomi-override edits via
the UI actually work for v2; until now they wrote to the legacy
v1 SuwayomiCategoryOverrides field which the classifier ignored."
```

---

### Task 6: Validation guards — no-delete-while-referenced + Unknown bindings

Two related guards: (1) attempting to delete a binding that's referenced by any rule, Suwayomi override, or default-binding picker should be rejected with a clear error message; (2) any row referencing a deleted binding renders as `Unknown binding (ID: N)` and stays editable (covered partially in earlier tasks; this task finalises it).

**Files:**
- Modify: `internal/web/save.go` (add `validateBindingsNotReferenced` before SaveBindings)
- Modify: `internal/web/web.go` (handle validation error: re-render settings with error banner)
- Modify: `internal/web/bindings_test.go` (add validation tests)

- [ ] **Step 1: Write failing tests**

Append to `internal/web/bindings_test.go`:
```go
func TestPOSTSettingsRejectsDeletingReferencedBinding(t *testing.T) {
	jp := "JP"
	st := &fakeStore{
		bindings: []model.Binding{
			{ID: 1, Name: "Manga", LibraryRoot: "/m/a", KavitaLibID: 1},
			{ID: 2, Name: "Doomed", LibraryRoot: "/m/x", KavitaLibID: 9},
		},
		rules: []model.ClassificationRule{
			{ID: 1, Priority: 100, Name: "Japanese", Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: 2},
		},
	}
	h := newTestHandler(t, st)

	form := url.Values{}
	form.Set("file_mode", "hardlink")
	// Submit ONLY binding ID 1; binding 2 is omitted (would be deleted by
	// SaveBindings' replace-all). Rule still references binding 2 →
	// validation should reject.
	form.Set("binding_id_1", "1")
	form.Set("binding_name_1", "Manga")
	form.Set("binding_library_root_1", "/m/a")
	form.Set("binding_kavita_lib_1", "1")
	// Submit rule referencing the about-to-be-deleted binding 2.
	form.Set("rule_id_1", "1")
	form.Set("rule_priority_1", "100")
	form.Set("rule_name_1", "Japanese")
	form.Set("rule_country_1", "JP")
	form.Set("rule_binding_1", "2")

	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// Expect the POST to fail with a 400 or render the form back with an
	// error. Don't over-constrain the status; check that the binding was
	// NOT deleted.
	bindings, _ := st.ListBindings()
	if len(bindings) != 2 {
		t.Errorf("expected binding 2 to NOT be deleted (rule references it); got bindings: %+v", bindings)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "Doomed") && !strings.Contains(body, "referenced") {
		t.Errorf("expected error banner naming the referenced binding; body: %s", body)
	}
}
```

- [ ] **Step 2: Run, verify failure**

```bash
go test ./internal/web/ -run TestPOSTSettingsRejectsDeletingReferencedBinding -v
```

Expected: FAIL — validation not implemented.

- [ ] **Step 3: Implement the validator**

In `internal/web/save.go`, add:
```go
// validateBindingsNotReferenced returns an error if the submitted bindings
// drop any rows that the submitted rules / overrides / default-binding
// still reference. Called before SaveBindings so partial state never
// lands.
func validateBindingsNotReferenced(
	submitted []model.Binding,
	rules []model.ClassificationRule,
	overrides map[int64]int64,
	defaultBindingID *int64,
	existing []model.Binding,
) error {
	keepSet := make(map[int64]bool, len(submitted))
	for _, b := range submitted {
		if b.ID > 0 {
			keepSet[b.ID] = true
		}
	}

	// Build a name lookup against the existing bindings for nicer errors.
	existingByID := make(map[int64]model.Binding, len(existing))
	for _, b := range existing {
		existingByID[b.ID] = b
	}

	type ref struct {
		bindingID int64
		reason    string
	}
	var refs []ref
	for _, r := range rules {
		if r.BindingID > 0 && !keepSet[r.BindingID] {
			refs = append(refs, ref{r.BindingID, fmt.Sprintf("rule %q", r.Name)})
		}
	}
	for catID, bid := range overrides {
		if bid > 0 && !keepSet[bid] {
			refs = append(refs, ref{bid, fmt.Sprintf("Suwayomi override (category %d)", catID)})
		}
	}
	if defaultBindingID != nil && *defaultBindingID > 0 && !keepSet[*defaultBindingID] {
		refs = append(refs, ref{*defaultBindingID, "the Default Binding picker"})
	}
	if len(refs) == 0 {
		return nil
	}

	// Compose a friendly error message.
	parts := make([]string, 0, len(refs))
	for _, r := range refs {
		name := fmt.Sprintf("Unknown binding (ID: %d)", r.bindingID)
		if b, ok := existingByID[r.bindingID]; ok {
			name = fmt.Sprintf("%q (ID: %d)", b.Name, r.bindingID)
		}
		parts = append(parts, fmt.Sprintf("%s — referenced by %s", name, r.reason))
	}
	return fmt.Errorf("cannot delete bindings still in use:\n  %s", strings.Join(parts, "\n  "))
}
```

In `internal/web/web.go`'s `saveSettings`, call `validateBindingsNotReferenced` BEFORE `SaveBindings`:

```go
existing, _ := h.store.ListBindings()
if err := validateBindingsNotReferenced(bindings, rules, overrides, defaultBindingID, existing); err != nil {
    // Re-render the settings page with the error banner. The simplest
    // path: pass the error to the existing pageSettings render. If the
    // existing handler doesn't have a "render with error" path, add one
    // — match whatever the Kavita-test error rendering already does
    // (Library Map Plan C added a Kavita error banner that may be the
    // pattern to mirror).
    renderSettingsWithError(w, r, h, err)
    return
}
```

Add a small `renderSettingsWithError(w, r, h, err)` helper if one doesn't already exist. It should re-load the same data `pageSettings` loads, attach an `Error string` field to the page data, and render the template. The template needs an `{{if .Error}}<div class="form-error">{{.Error}}</div>{{end}}` block somewhere visible (top of the page).

- [ ] **Step 4: Run, verify pass**

```bash
go test ./internal/web/ -run TestPOSTSettingsRejectsDeletingReferencedBinding -v -race
go test ./internal/web/... -count=1 -race
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/web/save.go internal/web/web.go internal/web/templates/settings.html internal/web/bindings_test.go
touch /home/gavin/my_other_repos/mangarr/.claude/.verified
git -c commit.gpgsign=false commit -m "feat(web): reject deleting bindings still referenced by rules/overrides/default

Validation runs before SaveBindings. If the submitted form omits a
binding that any submitted rule, Suwayomi override, or the default-
binding picker still references, the form re-renders with an error
banner naming each reference. Partial state never lands.

Rows referencing a deleted binding (e.g. when bindings are removed
out-of-band) continue to render as 'Unknown binding (ID: N)' and
stay editable — this was wired in earlier tasks; finalised here."
```

---

### Task 7: Activity log Via renderer — resolve rule IDs to names + new shapes

The activity log Via column needs to render Plan A's new prefixes (`rule:N`, `path-rule:N`, `default-binding`) into user-friendly labels. Existing Library Map Plan C renderer handles `suwayomi-override:category=N` and `anilist:JP` already; we add the rest.

**Files:**
- Modify: `internal/web/web.go` (extend `formatVia` or whatever the activity-log renderer is)
- Modify: `internal/web/web_test.go` (add renderer tests; the existing test file probably already has formatVia tests)

- [ ] **Step 1: Inspect existing formatVia**

```bash
grep -nE 'formatVia|ViaSuwayomiOverridePrefix|ViaAniListPrefix' internal/web/web.go | head -10
```

Find the existing function (likely in `internal/web/web.go`, possibly called `formatVia(via string, ...)` or similar). Read its current implementation: it probably switches on `strings.HasPrefix(via, classifier.ViaSuwayomiOverridePrefix)` and a few other cases, with a default that returns the raw string.

- [ ] **Step 2: Write failing tests**

Append to `internal/web/web_test.go`:
```go
func TestFormatViaResolvesRuleIDToName(t *testing.T) {
	rules := []model.ClassificationRule{
		{ID: 5, Name: "Japanese 18+"},
	}
	bindings := []model.Binding{}
	cats := map[int64]string{}
	got := formatVia("rule:5", rules, bindings, cats)
	if got != "Japanese 18+" {
		t.Errorf("formatVia rule:5: want 'Japanese 18+', got %q", got)
	}
}

func TestFormatViaFallsBackToUnknownForMissingRule(t *testing.T) {
	got := formatVia("rule:999", nil, nil, nil)
	if got != "Unknown rule (ID: 999)" {
		t.Errorf("formatVia missing rule: want 'Unknown rule (ID: 999)', got %q", got)
	}
}

func TestFormatViaPathRuleSameAsRule(t *testing.T) {
	rules := []model.ClassificationRule{
		{ID: 7, Name: "Comics by path"},
	}
	got := formatVia("path-rule:7", rules, nil, nil)
	if got != "Comics by path" {
		t.Errorf("formatVia path-rule:7: want 'Comics by path', got %q", got)
	}
}

func TestFormatViaDefaultBindingShowsBindingName(t *testing.T) {
	id := int64(3)
	settings := model.Settings{DefaultBindingID: &id}
	bindings := []model.Binding{{ID: 3, Name: "Catch-all"}}
	got := formatViaWithSettings("default-binding", nil, bindings, nil, settings)
	if !strings.Contains(got, "Catch-all") {
		t.Errorf("formatVia default-binding: want label including 'Catch-all', got %q", got)
	}
}

func TestFormatViaDefaultBindingFallbackWhenBindingDeleted(t *testing.T) {
	id := int64(99)
	settings := model.Settings{DefaultBindingID: &id}
	got := formatViaWithSettings("default-binding", nil, nil, nil, settings)
	if !strings.Contains(got, "deleted") && !strings.Contains(got, "Unknown") {
		t.Errorf("formatVia default-binding with deleted binding: want fallback, got %q", got)
	}
}
```

Adapt the function signature — if `formatVia` already takes a Handler receiver and reads from `h.store`, use that pattern instead of free-standing args. The tests should match the signature.

- [ ] **Step 3: Run, verify failure**

```bash
go test ./internal/web/ -run TestFormatVia -v
```

Expected: FAIL.

- [ ] **Step 4: Extend formatVia**

In `internal/web/web.go`, find `formatVia`. Add cases for the new prefixes:
```go
import "github.com/gavinmcfall/mangarr/internal/classifier"

func formatVia(via string, rules []model.ClassificationRule, bindings []model.Binding, cats map[int64]string) string {
    switch {
    case via == "":
        return "—"
    case via == classifier.ViaUnmatched:
        return "Unmatched"
    case strings.HasPrefix(via, classifier.ViaRulePrefix):
        // "rule:5" → look up rule by ID
        idStr := strings.TrimPrefix(via, classifier.ViaRulePrefix)
        id, _ := strconv.ParseInt(idStr, 10, 64)
        for _, r := range rules {
            if r.ID == id { return r.Name }
        }
        return fmt.Sprintf("Unknown rule (ID: %s)", idStr)
    case strings.HasPrefix(via, classifier.ViaPathRulePrefix):
        idStr := strings.TrimPrefix(via, classifier.ViaPathRulePrefix)
        id, _ := strconv.ParseInt(idStr, 10, 64)
        for _, r := range rules {
            if r.ID == id { return r.Name }
        }
        return fmt.Sprintf("Unknown rule (ID: %s)", idStr)
    case via == classifier.ViaDefaultBinding:
        // Caller-side: this branch needs Settings + bindings.
        // If you keep formatVia stateless, return a sentinel and have
        // the caller (the page renderer) substitute the binding name.
        return "Default binding"
    case strings.HasPrefix(via, classifier.ViaSuwayomiOverridePrefix):
        idStr := strings.TrimPrefix(via, classifier.ViaSuwayomiOverridePrefix)
        id, _ := strconv.ParseInt(idStr, 10, 64)
        if name, ok := cats[id]; ok { return name }
        return fmt.Sprintf("Unknown (ID: %s)", idStr)
    case strings.HasPrefix(via, classifier.ViaAniListPrefix):
        code := strings.TrimPrefix(via, classifier.ViaAniListPrefix)
        if code == "" { return "AniList" }
        return fmt.Sprintf("AniList (%s)", code)
    }
    return via // fallback: raw
}
```

For the `default-binding` case that needs Settings, add a sister function `formatViaWithSettings` that takes the Settings shape and resolves the binding name:
```go
func formatViaWithSettings(via string, rules []model.ClassificationRule, bindings []model.Binding, cats map[int64]string, settings model.Settings) string {
    if via == classifier.ViaDefaultBinding {
        if settings.DefaultBindingID == nil {
            return "Default binding (none)"
        }
        for _, b := range bindings {
            if b.ID == *settings.DefaultBindingID {
                return fmt.Sprintf("Default binding (%s)", b.Name)
            }
        }
        return fmt.Sprintf("Default binding (deleted, ID: %d)", *settings.DefaultBindingID)
    }
    return formatVia(via, rules, bindings, cats)
}
```

In `pageActivity` (or whatever renders the activity page), call `formatViaWithSettings` with the loaded rules + bindings + cats + settings.

- [ ] **Step 5: Run, verify pass**

```bash
go test ./internal/web/ -run TestFormatVia -v -race
go test ./internal/web/... -count=1 -race
```

Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/web/web.go internal/web/web_test.go
touch /home/gavin/my_other_repos/mangarr/.claude/.verified
git -c commit.gpgsign=false commit -m "feat(web): activity-log Via renderer covers Plan A's new shapes

Extends formatVia to resolve:
- rule:<id>      → ClassificationRule.Name (fallback: Unknown rule (ID: N))
- path-rule:<id> → same as rule:
- default-binding → 'Default binding (<binding name>)' with 'deleted'
  fallback when the configured DefaultBindingID no longer exists

Existing v1 shapes (suwayomi-override:category=, anilist:, unmatched)
keep their renderings."
```

---

### Task 8: Remove obsolete v1 Settings sections

Plan A made the v1 "Library Roots" filesystem-path inputs + "Kavita Libraries" picker dropdowns vestigial — Bindings cover both surfaces. Delete those sections from `settings.html` and their associated form-parsing in `saveSettings`. The migration-preserved v1 Settings fields stay on the model for one release; the UI just doesn't expose them anymore.

**Files:**
- Modify: `internal/web/templates/settings.html` (delete v1 sections)
- Modify: `internal/web/web.go` (delete v1 form-parsing for `library_root_<type>` + `kavita_lib_<type>` form fields if they exist)
- Modify: `internal/web/bindings_test.go` (add a test that confirms the obsolete section is gone)

- [ ] **Step 1: Write failing test**

Append to `internal/web/bindings_test.go`:
```go
func TestSettingsPageNoLongerRendersV1LibraryRootsSection(t *testing.T) {
	st := &fakeStore{}
	h := newTestHandler(t, st)
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	// The v1 "Library Roots" filesystem-path inputs are replaced by the
	// Bindings card's LibraryRoot column. Confirm the v1 heading is gone.
	if strings.Contains(body, "<h3>Library Roots</h3>") {
		t.Errorf("v1 'Library Roots' section heading still present; should be replaced by Bindings card")
	}
	if strings.Contains(body, `name="library_root_manga"`) ||
		strings.Contains(body, `name="library_root_manhwa"`) ||
		strings.Contains(body, `name="library_root_manhua"`) {
		t.Errorf("v1 library_root_<type> form fields still rendered")
	}
}

func TestSettingsPageNoLongerRendersV1KavitaLibrariesSection(t *testing.T) {
	st := &fakeStore{}
	h := newTestHandler(t, st)
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	if strings.Contains(body, "<h3>Kavita Libraries</h3>") {
		t.Errorf("v1 'Kavita Libraries' section heading still present")
	}
	if strings.Contains(body, `name="kavita_lib_manga"`) ||
		strings.Contains(body, `name="kavita_lib_manhwa"`) ||
		strings.Contains(body, `name="kavita_lib_manhua"`) {
		t.Errorf("v1 kavita_lib_<type> form fields still rendered")
	}
}
```

- [ ] **Step 2: Run, verify failure**

```bash
go test ./internal/web/ -run 'TestSettingsPageNoLongerRendersV1' -v
```

Expected: FAIL — sections still present.

- [ ] **Step 3: Delete v1 sections from settings.html**

Open `internal/web/templates/settings.html`. Find and delete:
- The `<h3>Library Roots</h3>` section and its body (3 filesystem-path inputs).
- The `<h3>Kavita Libraries</h3>` section and its body (3 Kavita library dropdowns).

Leave the Suwayomi Category Overrides card intact (Task 4 widened its dropdown).

- [ ] **Step 4: Delete v1 form-parsing from saveSettings**

In `internal/web/web.go`'s `saveSettings`, find and delete the form-parsing that reads `library_root_<type>` and `kavita_lib_<type>` form fields. These fields no longer arrive on the form, so the parsing is dead code that would just produce empty values silently.

The Settings model fields `LibraryRoots` and `KavitaLibIDsByType` stay populated from Migration 2 (rollback safety per Plan A). On SaveSettings the existing values pass through untouched because the new POST handler doesn't overwrite them.

- [ ] **Step 5: Run, verify pass**

```bash
go test ./internal/web/ -count=1 -race
go build ./...
go vet ./...
```

Expected: PASS. Some EXISTING tests may have asserted on the v1 sections — they need updating to match the new shape. Read each failing test; if it asserts on the old surface, either delete it (it's about a behaviour that no longer exists) or rewrite it against the Bindings card.

- [ ] **Step 6: Commit**

```bash
git add internal/web/templates/settings.html internal/web/web.go internal/web/bindings_test.go internal/web/web_test.go
touch /home/gavin/my_other_repos/mangarr/.claude/.verified
git -c commit.gpgsign=false commit -m "feat(web): remove v1 Library Roots + Kavita Libraries Settings sections

Both surfaces are now covered by the Library Bindings card from
Task 2. The v1 Settings model fields (LibraryRoots,
KavitaLibIDsByType) stay populated from Migration 2 for rollback
safety; only the UI exposure is removed.

A separate Plan C task will drop the v1 model fields one release
after Plan A merges."
```

---

### Task 9: Empty states + Settings page polish

Final task. Cover the remaining truth statements: when Bindings is empty, surface a friendly "configure at least one binding" prompt and disable the Rules + Default Binding cards (they need a binding to reference). When Rules is empty, the card just shows the empty-state prompt that Task 3 already wired.

**Files:**
- Modify: `internal/web/templates/settings.html` (add conditional empty-state messaging)
- Modify: `internal/web/bindings_test.go` (add empty-state tests)

- [ ] **Step 1: Write failing tests**

Append to `internal/web/bindings_test.go`:
```go
func TestSettingsPageEmptyBindingsShowsConfigurePrompt(t *testing.T) {
	st := &fakeStore{} // no bindings
	h := newTestHandler(t, st)
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, "configure at least one binding") &&
		!strings.Contains(body, "first library destination") {
		t.Errorf("expected empty-bindings configure prompt; body:\n%s", body)
	}
}

func TestSettingsPageEmptyBindingsHidesRulesEditor(t *testing.T) {
	st := &fakeStore{}
	h := newTestHandler(t, st)
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	// The Rules + Default Binding cards still render but their bodies
	// should be disabled or replaced with "Configure bindings first".
	// Don't over-constrain — just check that '+ Add Rule' is NOT
	// clickable when no bindings exist (rules need a target binding).
	addRuleIdx := strings.Index(body, "+ Add Rule")
	if addRuleIdx >= 0 {
		// If the affordance is present, it should be disabled or behind
		// a "configure bindings first" message.
		surrounding := body[max(0, addRuleIdx-200):min(len(body), addRuleIdx+200)]
		if !strings.Contains(surrounding, "disabled") && !strings.Contains(surrounding, "configure bindings") {
			t.Errorf("expected '+ Add Rule' to be disabled or hidden when no bindings exist; got surrounding HTML:\n%s", surrounding)
		}
	}
}
```

`max` and `min` for int are Go 1.21+ built-ins; if your Go version is older add small helpers.

- [ ] **Step 2: Run, verify failure**

```bash
go test ./internal/web/ -run 'TestSettingsPageEmpty' -v
```

Expected: FAIL.

- [ ] **Step 3: Add conditional empty-states to settings.html**

In `internal/web/templates/settings.html`'s Library Bindings card body, the empty-state from Task 2 already exists. Enhance to be more directive when zero bindings:

```html
<div class="card-body">
  <div class="binding-rows" id="binding-rows">
    {{template "binding-rows" .}}
  </div>
  {{if not .Bindings}}
  <div class="empty-state">
    <p><b>No bindings configured.</b></p>
    <p>To start using mangarr, configure at least one binding to define your first library destination. Click <b>+ Add Binding</b> above.</p>
  </div>
  {{end}}
</div>
```

In the Classification Rules card body, gate the Add Rule affordance + the empty-state message:

```html
<div class="card-body">
  {{if .Bindings}}
    <p class="card-hint">Rules are evaluated top-to-bottom by Priority. First match wins.</p>
    <div class="rule-rows" id="rule-rows">{{template "rule-rows" .}}</div>
    {{if not .Rules}}<p class="empty-state">No rules configured yet. Click <b>+ Add Rule</b> above to define how series get classified.</p>{{end}}
  {{else}}
    <p class="empty-state">Configure at least one binding above before adding classification rules — rules need a target binding to route to.</p>
  {{end}}
</div>
```

Move the `+ Add Rule` button into a `{{if .Bindings}}…{{end}}` block too so it doesn't render when there's nothing to point at.

Same treatment for the Default Binding picker:
```html
<div class="card-body">
  {{if .Bindings}}
    <p class="card-hint">When no rule matches, route the series here.</p>
    <select name="default_binding_id">...</select>
  {{else}}
    <p class="empty-state">Configure at least one binding above to enable a default-binding fallback.</p>
  {{end}}
</div>
```

- [ ] **Step 4: Run, verify pass**

```bash
go test ./internal/web/ -run 'TestSettingsPageEmpty' -v -race
go test ./internal/web/... -count=1 -race
go build ./...
go vet ./...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/web/templates/settings.html internal/web/bindings_test.go
touch /home/gavin/my_other_repos/mangarr/.claude/.verified
git -c commit.gpgsign=false commit -m "feat(web): empty-state prompts for unconfigured Settings page

When no bindings are configured, Library Bindings card shows a
configure-first directive, and the Classification Rules + Default
Binding cards hide their interactive surfaces (no '+ Add Rule', no
Default Binding picker) behind a 'configure bindings first' prompt.
The Add Binding affordance is always available so the user has a
clear next step.

Plan B is complete with this commit."
```

---

## Self-Review

### Spec coverage

Plan B truth statements from `docs/specs/2026-05-30-library-bindings-v2.md` Plan B section, each mapped to the task that implements it:

| Truth statement | Implemented by |
|---|---|
| Settings page renders Library Bindings card | Task 2 |
| Add Binding affordance appends empty row; save persists complete rows | Task 2 + Task 5 |
| Binding cannot be deleted while referenced | Task 6 |
| Settings page renders Classification Rules card sorted by Priority | Task 3 |
| Add Rule affordance + save persists rows with condition + binding | Task 3 + Task 5 |
| Default Binding picker with `— Send to Unmatched —` option | Task 4 |
| Suwayomi overrides dropdown widens to all bindings | Task 4 |
| Deleted binding renders as `Unknown binding (ID: N)` and stays editable | Tasks 3 + 4 + 6 |
| GET /api/bindings + GET /api/rules return JSON | Task 1 |
| Form POST atomically persists bindings + rules + default-binding | Task 5 |
| Activity log renderer resolves rule:N → Name, default-binding → label | Task 7 |
| Empty bindings state hides Rules + Default Binding editing | Task 9 |

All 12 truth statements covered.

### Placeholder scan

No `TBD` / `TODO` / "add appropriate error handling" patterns. Each step has runnable code or a runnable command. A few "match what's already there" adapt-as-you-go directions (e.g. existing template loader shape, existing renderSettingsWithError pattern) are explicit instructions to read existing code, not placeholders.

### Type consistency

- `model.Binding`, `model.ClassificationRule`, `model.RuleCondition`, `model.Settings.DefaultBindingID`, `model.Settings.SuwayomiCategoryBindings` — all from Plan A, used consistently across all 9 tasks.
- Form-field naming convention `binding_<col>_<suffix>`, `rule_<col>_<suffix>`, `override_<col>_<suffix>` consistent across Tasks 2/3/4/5/6.
- `classifier.ViaRulePrefix`, `ViaPathRulePrefix`, `ViaSuwayomiOverridePrefix`, `ViaAniListPrefix`, `ViaDefaultBinding`, `ViaUnmatched` — all from Plan A, used consistently in Task 7.
- Template names `binding-rows`, `rule-rows`, `override-rows` (existing from Library Map Plan C) consistent across tasks.
- New web file `internal/web/save.go` introduced in Task 5 and extended in Task 6.

No drift detected.

## Execution Handoff

Plan complete and saved to `docs/plans/2026-05-30-library-bindings-v2-plan-b.md`. Two execution options:

**1. Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration. Same pattern that landed Plan A cleanly.

**2. Inline Execution** — Execute tasks in this session using executing-plans, batched with checkpoints.

Which approach?
