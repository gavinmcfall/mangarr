---
description: Library Bindings v2 — replace the closed 3-type ContentType enum (Manga/Manhwa/Manhua) with user-defined library bindings and priority-ordered classification rules. Lifts mangarr from "3 fixed libraries" to "N user-defined libraries" so users can model 18+ variants, Western Comics, Light Novels, or any custom taxonomy.
tags: [mangarr, bindings, classification, library-map, rules, migration]
audience: { human: 60, agent: 40 }
purpose: { design: 55, north-star: 15, flow: 10, plan: 20, findings: 0, gestalt: 0, reference: 0, research: 0, concepts: 0, high-agency-process: 0, low-agency-process: 0 }
---

# Library Bindings v2

> Successor to Library Map (docs/specs/2026-05-30-library-map-suwayomi.md). Replaces the closed 3-type `ContentType` enum with user-defined library bindings and priority-ordered classification rules. Lifts mangarr from "3 fixed libraries that match AniList countryOfOrigin" to "N user-defined libraries with arbitrary classification rules", so a public-release user can model whatever taxonomy they need — 18+ variants, Western Comics, Light Novels, publisher splits, etc.

## North star — what should be true

When this is done, the following statements are verifiable against the running system:

1. **A user can define an arbitrary number of library bindings**, each with its own name, library root path, Kavita library ID, and adult-default hint. Adding a new binding does not require a code change.
2. **A user can define an arbitrary number of classification rules** that route incoming series to bindings based on `countryOfOrigin`, `isAdult`, `format`, and source path prefix. Rules are priority-ordered; first match wins.
3. **A series whose AniList result is `format: NOVEL` routes to a Light Novels binding** if the user has configured a rule for it, regardless of `countryOfOrigin`.
4. **A series whose AniList result is `isAdult: true` routes to an 18+ binding** if the user has configured one, instead of the corresponding non-adult binding.
5. **A series whose chapter file lives under a configured source path prefix routes to the matching binding** before mangarr makes any AniList call.
6. **A Suwayomi category override routes to its mapped binding directly** without the v1 reverse-lookup-via-ContentType limitation. Any binding is a valid override target — not only the three primary content types.
7. **A series matching no rule and no override goes to the Unmatched queue by default**, OR to `Settings.DefaultBindingID` if the user has chosen one.
8. **The activity log shows which rule (or override) routed each series**, with a stable display name resolved at render time from the rule's user-given Name.
9. **A user upgrading from Library Map preserves their existing classification behaviour**: their `KavitaLibIDsByType` + `LibraryRoots` auto-convert to bindings on first boot, default rules are generated for JP/KR/CN/TW, and Suwayomi category overrides translate to the new binding IDs. No manual reconfiguration is required to maintain the v1 routing.
10. **The migration from v1 to v2 is idempotent**: rerunning mangarr does not duplicate bindings or rules; a `schema_versions` table tracks completed migrations.

## Findings — what's true now

- After Library Map (PR #30/#31/#32, image `sha-f1332ba`), `Settings` carries `LibraryRoots map[ContentType]string`, `KavitaLibIDsByType map[ContentType]int64`, and `SuwayomiCategoryOverrides map[int64]int64` where the override map's values are Kavita library IDs.
- The classifier reverse-looks-up Suwayomi-override Kavita library IDs against `KavitaLibIDsByType` to derive a `ContentType`, then the poller routes by ContentType → LibraryRoot. Consequence (the load-bearing v2 motivation): overrides only function for the three primary content types. Mapping a Suwayomi category to a Kavita library that is not in `KavitaLibIDsByType` silently routes the series to Unmatched.
- The AniList client queries `Media(search, type: MANGA) { countryOfOrigin }` — only the country code. `isAdult` and `format` are available on the same node but not requested.
- `ContentType` is a closed enum in `internal/model`: `TypeManga`, `TypeManhwa`, `TypeManhua`, `TypeUnknown`. `CountryToType` is a fixed mapping (`JP → Manga`, `KR → Manhwa`, `CN/TW → Manhua`).
- Suwayomi's `PathCache` from PR #30 is a path → `(mangaID, categoryIDs)` map; nothing about it is content-type-aware. It still works under v2 unchanged.
- The settings store uses inline `ALTER TABLE ADD COLUMN` with an idempotent duplicate-column-error swallow (the Plan B reviewer noted: "the next migration triggers introducing a proper migrations framework"). v2 is that next migration.

## Flow — how a chapter file gets routed

```mermaid
flowchart TD
    A[Chapter file lands in /media/Downloads/...] --> B[Scanner extracts title and parent path]
    B --> C{Any path-only rule matches?<br/>(no AniList needed)}
    C -- yes --> R1[Route to mapped binding<br/>Via = path-rule:N]
    C -- no --> D{Path under Suwayomi tree<br/>AND in PathCache?}
    D -- yes --> E{Any categoryID maps<br/>in SuwayomiCategoryOverrides?}
    E -- yes --> R2[Route to mapped binding<br/>Via = suwayomi-override:cat=N]
    E -- no --> F[AniList Lookup:<br/>countryOfOrigin, isAdult, format]
    D -- no --> F
    F --> G{AniList match found?}
    G -- yes --> H{Any rule matches<br/>AniList result?}
    H -- yes --> R3[Route to mapped binding<br/>Via = rule:N]
    H -- no --> I{Settings.DefaultBindingID set?}
    G -- no --> I
    I -- yes --> R4[Route to default binding<br/>Via = default-binding]
    I -- no --> R5[Hold in Unmatched queue<br/>Via = unmatched]
    R1 --> Z[Filer hardlink/move/copy + Kavita scan]
    R2 --> Z
    R3 --> Z
    R4 --> Z
```

Failure modes:

- **AniList unreachable**: classifier returns the same "no match" path as a real no-result; series falls through to default binding or Unmatched. No regression vs v1.
- **Suwayomi cache empty or stale**: cache lookup misses; flow proceeds to AniList. Same as Library Map's behaviour.
- **Rule references a deleted binding**: the rule is skipped at evaluation time (log warning once per tick). Activity log Via still records the rule that matched, but the renderer surfaces `Unknown binding (ID: N)`.
- **Binding referenced by Suwayomi override is deleted**: same surfacing as above. Override is skipped at evaluation; flow falls through to AniList.
- **No bindings configured at all** (fresh install, no migration data): every series goes to Unmatched. Settings page surfaces a "Configure at least one binding" prompt.

## Design

### Data model

Three new types in `internal/model`:

```go
// Binding is one library destination the user has defined.
// Replaces the closed-enum routing of v1.
type Binding struct {
    ID             int64
    Name           string  // user label, e.g. "Manga", "Manhwa 18+", "Comics"
    LibraryRoot    string  // filesystem destination, e.g. /media/Library/Books/Manhwa
    KavitaLibID    int64   // Kavita library ID for scan trigger
    DefaultIsAdult bool    // hint surfaced in UI, does NOT gate routing
}

// ClassificationRule maps a metadata condition to a binding.
// Rules are stored as an ordered list; first match wins.
type ClassificationRule struct {
    ID        int64
    Priority  int             // lower number = higher priority; first match wins
    Name      string          // user label, e.g. "Korean 18+"
    Condition RuleCondition
    BindingID int64           // FK -> Binding
}

// RuleCondition is AND-semantics across set fields. Unset (*nil) = wildcard.
type RuleCondition struct {
    CountryOfOrigin  *string  // "JP" | "KR" | "CN" | "TW"
    IsAdult          *bool
    Format           *string  // "MANGA" | "NOVEL" | "ONE_SHOT" | ...
    SourcePathPrefix *string  // matched against the chapter file's canonical parent path
}
```

`Settings` gains:

```go
type Settings struct {
    // ... existing fields ...
    DefaultBindingID *int64  // nil = no fallback (use Unmatched); set = route no-match here

    // The following are DEPRECATED in v2 and removed one release after the
    // migration ships. Kept populated alongside Bindings for one release
    // so a rollback to v1.x doesn't nuke the user's setup.
    LibraryRoots              map[ContentType]string  // DEPRECATED
    KavitaLibIDsByType        map[ContentType]int64   // DEPRECATED
    SuwayomiCategoryOverrides map[int64]int64         // DEPRECATED — see below
}
```

`Settings.SuwayomiCategoryOverrides` is migrated in place: values change from Kavita library IDs to Binding IDs. A new field `SuwayomiCategoryOverridesV2 map[int64]int64` could shadow the old, but cleaner to migrate the existing map and document the value-semantics change in the migration's comment.

`ContentType` enum stays in `internal/model` as a vestigial-internal type used only by the v1 migration (to drive the auto-generated default rules). Nothing in the v2 classifier or routing flow references it.

### Classifier rewrite

Six-step flow, replacing the v1 classifier:

```go
type Decision struct {
    BindingID int64
    Via       string  // "rule:N" | "path-rule:N" | "suwayomi-override:cat=N" | "default-binding" | "unmatched"
}

func (c *Classifier) Classify(ctx context.Context, item ScanItem) (Decision, error) {
    s, _ := c.store.GetSettings()

    // 1. Path-only rules — short-circuit before any network call.
    for _, r := range pathOnlyRules(s.Rules) {  // rules whose Condition has ONLY SourcePathPrefix set
        if strings.HasPrefix(item.ParentDir, *r.Condition.SourcePathPrefix) {
            return Decision{r.BindingID, fmt.Sprintf("path-rule:%d", r.ID)}, nil
        }
    }

    // 2. Suwayomi override — fast path via PathCache.
    if entry, ok := c.suwayomi.Lookup(item.ParentDir); ok {
        for _, catID := range entry.CategoryIDs {  // pre-sorted ascending in Plan A
            if bindingID, mapped := s.SuwayomiCategoryOverrides[catID]; mapped {
                return Decision{bindingID, fmt.Sprintf("suwayomi-override:cat=%d", catID)}, nil
            }
        }
    }

    // 3. AniList lookup — now wider (countryOfOrigin + isAdult + format).
    result, anilistErr := c.anilist.Lookup(ctx, item.Title)

    // 4. AniList rules — walk all rules with at least one non-path condition.
    //    Mixed rules (path + AniList conditions) are evaluated here, NOT step 1, so
    //    the path constraint composes with the AniList constraint via AND-semantics.
    if anilistErr == nil {
        for _, r := range s.Rules {  // sorted ascending by Priority
            if isPathOnly(r.Condition) { continue }  // already evaluated in step 1
            if matches(r.Condition, result, item.ParentDir) {
                return Decision{r.BindingID, fmt.Sprintf("rule:%d", r.ID)}, nil
            }
        }
    }

    // 5. Default binding fallback.
    if s.DefaultBindingID != nil {
        return Decision{*s.DefaultBindingID, "default-binding"}, nil
    }

    // 6. Unmatched.
    return Decision{0, "unmatched"}, nil
}
```

`matches` is AND-semantics over set fields:

```go
func matches(cond RuleCondition, result anilist.Result, parentDir string) bool {
    if cond.CountryOfOrigin != nil && result.CountryOfOrigin != *cond.CountryOfOrigin { return false }
    if cond.IsAdult != nil && result.IsAdult != *cond.IsAdult { return false }
    if cond.Format != nil && result.Format != *cond.Format { return false }
    if cond.SourcePathPrefix != nil && !strings.HasPrefix(parentDir, *cond.SourcePathPrefix) { return false }
    return true
}
```

The poller routes by `BindingID` directly (no ContentType reverse-lookup). Look up the binding's `LibraryRoot` for the filesystem destination and `KavitaLibID` for the scan trigger.

### AniList client extension

Today's query in `internal/anilist`:

```graphql
Media(search: $title, type: MANGA) { countryOfOrigin }
```

Becomes:

```graphql
Media(search: $title, type: MANGA) {
    countryOfOrigin
    isAdult
    format
}
```

The client's exported result type widens:

```go
type Result struct {
    CountryOfOrigin string  // existing
    IsAdult         bool    // new
    Format          string  // new — "MANGA" | "NOVEL" | "ONE_SHOT" | ...
}
```

Callers (the classifier) consume the wider type. Tests update their fake AniList clients to return all three fields. No new round-trip, no rate-limit impact.

### Migrations framework

Plan B reviewer's note from PR #31 was specific: introduce a proper migrations framework when the next column lands. v2 is meaningfully more than one column — new tables for `bindings` and `classification_rules` plus the value-semantics change on `suwayomi_category_overrides` plus a few new Settings columns. Time for the framework.

Minimal shape (`internal/store/migrations.go`, ~50 LOC):

```go
type migration struct {
    version int
    name    string
    apply   func(tx *sql.Tx) error
}

var migrations = []migration{
    {1, "init-bindings-and-rules", migrateInitBindingsAndRules},
    {2, "migrate-v1-settings-into-bindings", migrateV1SettingsIntoBindings},
}

func runMigrations(db *sql.DB) error {
    if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_versions (version INTEGER PRIMARY KEY)`); err != nil {
        return err
    }
    applied := loadAppliedVersions(db)
    for _, m := range migrations {
        if applied[m.version] { continue }
        tx, _ := db.Begin()
        if err := m.apply(tx); err != nil { tx.Rollback(); return err }
        tx.Exec(`INSERT INTO schema_versions (version) VALUES (?)`, m.version)
        tx.Commit()
    }
    return nil
}
```

Migration 1 creates the new tables. Migration 2 reads existing Settings + Suwayomi overrides, generates bindings + default rules + translated overrides, and writes them. Migration 2 is idempotent because it's gated by `schema_versions`.

### Migration content (Migration 2)

Logic:

1. Read existing Settings.
2. For each `ContentType` in `{Manga, Manhwa, Manhua}` where the user has BOTH a `LibraryRoots[ct]` AND a `KavitaLibIDsByType[ct]` populated: create a `Binding{Name: string(ct), LibraryRoot: LibraryRoots[ct], KavitaLibID: KavitaLibIDsByType[ct], DefaultIsAdult: false}`. Skip ContentTypes without both fields populated.
3. Hold a `ContentType → BindingID` map for the next step.
4. Generate default `ClassificationRule`s, one per country code mapping to its content type IF the corresponding binding exists:
   - `{Priority: 100, Name: "Japanese", Condition: {CountryOfOrigin: ptr("JP")}, BindingID: bindings[Manga]}`
   - `{Priority: 200, Name: "Korean", Condition: {CountryOfOrigin: ptr("KR")}, BindingID: bindings[Manhwa]}`
   - `{Priority: 300, Name: "Chinese (CN)", Condition: {CountryOfOrigin: ptr("CN")}, BindingID: bindings[Manhua]}`
   - `{Priority: 310, Name: "Chinese (TW)", Condition: {CountryOfOrigin: ptr("TW")}, BindingID: bindings[Manhua]}`
   Priorities start at 100 so users have room to slot 18+ rules (10-90) above without renumbering.
5. Translate `SuwayomiCategoryOverrides`: for each `(catID, oldKavitaLibID)`, find the binding whose `KavitaLibID == oldKavitaLibID`. If found, write `(catID, newBindingID)` to the in-place map. If not found (the Plan B orphan case), log a warning and drop the entry.
6. Persist the new bindings + rules + translated overrides. Old `LibraryRoots`/`KavitaLibIDsByType` columns stay populated for the one-release deprecation grace period.

After migration, mangarr's classifier behaviour is byte-for-byte equivalent to v1 for any series whose AniList result the v1 classifier would have matched.

### UI shape

Settings page sections (post-Library-Map):

- **REMOVED**: "Library Map" two-card section, "Library Roots" filesystem-path inputs, "Kavita Libraries" picker. All three fold into Bindings.
- **ADDED**: Bindings card, Classification Rules card, Default Binding picker.
- **KEPT**: Suwayomi Category Overrides card with the right-hand dropdown widened to all bindings.
- **KEPT**: Suwayomi Connection + Kavita Connection panels unchanged.

**Library Bindings card** — CRUD list:

```
+-- Library Bindings -----------------------------------+ [+ Add Binding] +
| Name           | Library root             | Kavita lib   | Adult? |     |
|----------------|--------------------------|--------------|--------|-----|
| [Manga       ] | [/media/Library/.../Manga] | [Manga    v] | [ ]   | [x] |
| [Manhwa      ] | [/media/Library/.../Manhwa]| [Manhwa   v] | [ ]   | [x] |
| [Manhwa 18+  ] | [/media/Library/.../M18]   | [Manhwa18+ v]| [v]   | [x] |
| [Comics      ] | [/media/Library/.../Comics]| [Comics   v] | [ ]   | [x] |
| [Light Novels] | [/media/Library/.../LN]    | [LightNov v] | [ ]   | [x] |
+-------------------------------------------------------+
```

- Each row has Name (text), LibraryRoot (text + browse button mirroring the existing DownloadRoots browser), Kavita library (dropdown from Kavita's `/api/Library/Libraries`), Adult hint (checkbox), Delete.
- A new empty row appended via `+ Add Binding`. Form save persists rows with all required fields populated.
- A binding referenced by an active Rule cannot be deleted without confirmation; the UI shows "Delete (used by N rules)" and a confirmation modal.

**Classification Rules card** — priority-ordered list:

```
+-- Classification Rules ---------------------------------+ [+ Add Rule] +
| ≡ 100 [Japanese Manga    ] [x]
|       country=[JP v]  adult=[Any v]  format=[Any v]
|       path=[                                        ]
|       → binding=[Manga             v]
| ≡  50 [Japanese 18+ Manga] [x]
|       country=[JP v]  adult=[Yes v]  format=[Any v]
|       path=[                                        ]
|       → binding=[Manga 18+         v]
| ≡  80 [Light Novels      ] [x]
|       country=[Any v] adult=[Any v]  format=[NOVEL v]
|       path=[                                        ]
|       → binding=[Light Novels      v]
| ≡  30 [Comics by path    ] [x]
|       country=[Any v] adult=[Any v]  format=[Any v]
|       path=[/media/Downloads/comics                 ]
|       → binding=[Comics            v]
+---------------------------------------------------------+

Default binding (no-match fallback): [— Send to Unmatched — v]
```

- Drag-handle (`≡`) reorders rows in the DOM; explicit Priority number is editable inline so the user can keep precise control over insertion order without dragging. (Decision per Section 3 of brainstorm: explicit numbers AND drag-to-reorder both supported. Drag updates the number; manual edit also works.)
- Condition fields are always-rendered (no advanced/hide toggle). Four widgets per rule (country dropdown, adult radio, format dropdown, path text). Setting `Any` on a dropdown means "wildcard / nil pointer".
- Path-only rules render as a visual hint that they short-circuit before AniList — a small badge like `path-first` next to the priority number when only the path field is set.
- A rule whose Binding has been deleted renders as `Unknown binding (ID: N)` and stays editable, mirroring the Library Map degraded-state pattern.

**Default Binding picker** — single dropdown below the Rules card. Options: `— Send to Unmatched —` (the default) + every existing binding.

**Suwayomi Category Overrides** — same card as today, but the right-hand binding dropdown widens from "3 primary content-type Kavita libraries" to "all bindings". The Plan B reverse-lookup pill stays as a hint but its constraint is gone — every binding is now a valid override target. The `Unknown (ID: N)` rendering for deleted Suwayomi categories stays.

**New endpoints**:

| Method | Path | Purpose |
|---|---|---|
| GET | `/api/bindings` | JSON list of bindings (for both UI and external API users) |
| GET | `/api/rules` | JSON list of rules, sorted by priority |
| POST | `/settings` (extended) | Persist bindings + rules + default binding from the form |
| GET | `/api/kavita/libraries[/fragment]` | Unchanged from Library Map (used by Bindings card's Kavita dropdown) |
| GET | `/api/suwayomi/categories[/fragment]` | Unchanged from Library Map (used by Suwayomi Overrides) |

JSON-only GET routes let advanced users script changes via `curl`; the POST goes through the existing settings form path (no per-row CRUD endpoints — atomic form submit keeps Plan B simpler).

**Activity log Via column**:

Renderer resolves the new Via shapes at page-render time:

| Via value | Renders as |
|---|---|
| `rule:<id>` | The rule's user-given `Name` (e.g. "Korean 18+"); falls back to `Unknown rule (ID: N)` if the rule was deleted |
| `path-rule:<id>` | Same as `rule:` |
| `suwayomi-override:cat=<catID>` | The Suwayomi category name (live lookup, mirrors Library Map's behaviour); falls back to `Unknown (ID: N)` |
| `default-binding` | The literal text "Default binding (<binding name>)" |
| `unmatched` | "Unmatched" |
| `anilist:JP` (legacy) | Still rendered as "AniList (JP)" for activity rows persisted before the upgrade |

### Settings model changes summary

```go
type Settings struct {
    // ... existing fields preserved ...

    // NEW
    DefaultBindingID *int64

    // DEPRECATED (kept populated alongside Bindings for one release; dropped in the release after)
    LibraryRoots              map[ContentType]string  // populated for v1 rollback safety
    KavitaLibIDsByType        map[ContentType]int64   // populated for v1 rollback safety
    SuwayomiCategoryOverrides map[int64]int64         // VALUE SEMANTICS CHANGE: now BindingID not KavitaLibID
}
```

`Bindings []Binding` and `Rules []ClassificationRule` are NOT fields on Settings — they're separate tables. Loading them with Settings is the store's job (sloppy join is fine for this scale; ~10s of rows max).

### What does not change

- `internal/suwayomi` package (client + PathCache). Plan A from Library Map is untouched.
- The poller's tick orchestration: scanner → classifier → filer → Kavita scan trigger. Only the classifier's internals change.
- The filer's hardlink/move/copy logic.
- The Kavita client (still queries `/api/Library/Libraries`, still triggers scans by libraryID).
- Existing Library Map UI elements that aren't part of the "Library Map" section (Suwayomi Connection, Kavita Connection, File Mode, Rename Scheme, Poll Minutes, Download Roots).

### Out of scope (backlog)

- Migration UX banner shown on the Settings page on first boot after upgrade ("We converted your 3-binding setup; review here"). Plan C will add a minimal version of this. A fancier multi-step wizard is backlog.
- Tag/genre conditions on rules. Listed in brainstorm Q2 as "Full" option; YAGNI for v2.
- Per-binding override list (e.g. "always classify this specific series here regardless of rules"). Use the Suwayomi-override + path-rule mechanisms instead.
- Importing rules from a preset library (e.g. "common 18+ split"). Hand-authoring is fine for the launch.
- Multi-source bindings (one binding writes to multiple library roots / multiple Kavita libraries). Out of scope; one binding = one destination.
- Bulk reclassification of historical Activity entries when rules change. The Activity log shows what was true at routing time, not what would be true today.

## Plan — truth statements

Three plan batches, each independently shippable. EARS syntax; agents verify each statement against the running system.

### Plan A — Data model + migrations framework + classifier rewrite + AniList extension

- **The `internal/store` package shall expose a `runMigrations(db) error` function** that creates a `schema_versions` table on first call and applies each pending numbered migration in order, recording the version on success.
- **`runMigrations` shall be idempotent**: a second call with no new migrations defined shall be a no-op.
- **Migration 1 shall create the `bindings` and `classification_rules` tables** with the schemas described in the Design section.
- **Migration 2 shall convert any pre-v2 `Settings` rows into the v2 shape**: one `Binding` per populated `(LibraryRoots[ct], KavitaLibIDsByType[ct])` pair, default `ClassificationRule`s (JP/KR/CN/TW priorities 100/200/300/310) for the existing content types, and `SuwayomiCategoryOverrides` values translated from Kavita library IDs to Binding IDs.
- **If a `SuwayomiCategoryOverrides` value cannot be translated** (the orphan case), Migration 2 shall log a warning, drop that entry, and continue.
- **The migration shall preserve `LibraryRoots`, `KavitaLibIDsByType`, and the original `SuwayomiCategoryOverrides` field content** on the settings row so a v1 rollback can read them. New Bindings live alongside, not replacing.
- **The `internal/model` package shall expose `Binding`, `ClassificationRule`, and `RuleCondition` types** matching the Design section's shapes, with `RuleCondition` fields as pointers so unset (`nil`) is distinguishable from explicit empty values.
- **The classifier's `Classify(ctx, item)` shall return a `Decision{BindingID, Via}`**, executing the six-step flow described in the Flow section: path-only rules → Suwayomi overrides → AniList → AniList rules → default binding → Unmatched.
- **Rule evaluation shall walk rules in ascending `Priority` order**, with first-match-wins.
- **A rule whose `Condition.SourcePathPrefix` is the only set field shall be evaluated in the path-rules pass** (step 1), short-circuiting before any AniList call.
- **The AniList client's `Lookup(ctx, title)` shall return `Result{CountryOfOrigin, IsAdult, Format}`**, all three populated from a single GraphQL request.
- **The poller shall route via `Decision.BindingID`**, looking up `LibraryRoot` and `KavitaLibID` from the matching `Binding` directly.
- **The activity log shall carry `Via` values** in one of the documented forms: `path-rule:<id>`, `rule:<id>`, `suwayomi-override:cat=<id>`, `default-binding`, or `unmatched`.

Verification: unit tests against the store + classifier; integration test exercising the full migration on a SQLite DB pre-populated with v1 shape Settings; classifier table-driven tests covering each routing branch.

### Plan B — Settings UI for bindings, rules, and default binding

- **The Settings page shall render a Library Bindings card** listing every `Binding`, with per-row inputs for Name, LibraryRoot, Kavita library (dropdown), and `DefaultIsAdult` (checkbox), plus a Delete affordance.
- **An "Add Binding" affordance shall append a new empty row**; saving the form persists rows with all required fields populated.
- **A binding cannot be deleted while any rule, Suwayomi override, or the default-binding picker references it**; the delete affordance shall be disabled with a tooltip naming the references.
- **The Settings page shall render a Classification Rules card** listing every `ClassificationRule` sorted ascending by `Priority`, with per-row inputs for Name, Priority (number), the four condition widgets (country / adult / format / path), and the target binding dropdown.
- **An "Add Rule" affordance shall append a new empty row**; saving persists rows with at least one populated condition AND a selected binding.
- **The Settings page shall render a Default Binding picker** with options `— Send to Unmatched —` plus every existing binding.
- **The Suwayomi Category Overrides card shall offer every binding as a valid target** in its right-hand dropdown, not only bindings flagged as primary content types.
- **A row referencing a deleted Binding (in Rules or Suwayomi Overrides) shall render as `Unknown binding (ID: N)`** and remain editable.
- **`GET /api/bindings` and `GET /api/rules` shall return JSON lists**, useful for scripted configuration.
- **The form POST shall atomically persist all bindings, rules, and the default-binding selection**, with validation rejecting invalid input (negative priorities, empty binding names, missing FKs) and re-rendering the form with errors highlighted.
- **The activity log renderer shall resolve `rule:<id>` and `path-rule:<id>` to the rule's `Name`**, with `Unknown rule (ID: N)` fallback for deleted rules.
- **The activity log renderer shall resolve `default-binding` to "Default binding (<binding name>)"**, with `Default binding (deleted)` fallback if the configured default has been deleted.

Verification: web handler tests with `httptest`-stubbed Kavita; rendered-HTML assertions over the new sections; Playwright screenshot of a fully-populated Settings page committed alongside.

### Plan C — Migration UX + cleanup + integration tests

- **On first page load after Migration 2 has run**, the Settings page shall surface a one-time banner: "We converted your previous library setup into N bindings and M rules. Review them below." The banner is dismissable; dismissal is persisted in a Settings flag so it doesn't recur.
- **An integration test shall exercise Migration 2 against a Settings row populated only with `Manga`** (no Manhwa or Manhua), confirming one binding + one rule generated.
- **An integration test shall exercise Migration 2 against a Settings row** with one Suwayomi override pointing at a Kavita library that is NOT in `KavitaLibIDsByType`, confirming the orphan is dropped with a log line.
- **The deprecated `LibraryRoots` and `KavitaLibIDsByType` Settings fields shall be removed** (column drop or struct removal — driver choice). This shall NOT ship in the same release as Plan A; it shall be a follow-up after one release has lived in users' hands.
- **An empty-bindings state on the Settings page shall surface a "Configure at least one binding" prompt** in place of the Rules and Suwayomi Overrides cards.
- **A "Suggest rules" affordance** shall appear when the user has at least one binding whose `Name` ends in "18+", offering to pre-fill a corresponding adult-axis rule. (Optional; gate behind a feature flag if it adds review time.)

Verification: integration tests covering each migration edge case; manual smoke test plan refresh.

## Open questions

None blocking. A few deferred-to-implementation choices:

- **Drag-to-reorder semantics**: dragging a rule recomputes Priorities so they're contiguous (e.g. 10, 20, 30…), or preserves user-set numbers and just changes display order. Pick contiguous-on-drop with the constraint that manually-typed priorities are also accepted. (Simpler than alternatives; users who care about exact numbers can edit them directly.)
- **Condition validation**: when a rule's condition has all four fields unset (universal wildcard), should saving be allowed or rejected? Rejected — a universal wildcard is what `DefaultBindingID` is for. UI shows an inline error.
- **Path prefix matching**: case-sensitive string `HasPrefix` against canonical-cleaned parent dir. Documented in code; no platform-aware normalisation. Operators on case-insensitive filesystems just need to match the on-disk case.

## References

- `docs/specs/2026-05-30-library-map-suwayomi.md` — the Library Map workstream this builds on. The Plan B reverse-lookup constraint that motivated v2 is documented in its design section.
- `internal/suwayomi/{client,cache}.go` — Plan A from Library Map (PR #30). Unchanged in v2; consumers' API stays the same.
- `internal/classifier/classifier.go` — Plan B from Library Map (PR #31). Rewritten in v2 Plan A.
- `internal/web/{suwayomi.go,templates/}` — Plan C from Library Map (PR #32). The "Library Map" section's UI is replaced by v2 Bindings + Rules cards; the Suwayomi Overrides card stays but the dropdown widens.
- `docs/DESIGN.md` — overall mangarr design and the AniList classification path this rewrites.
