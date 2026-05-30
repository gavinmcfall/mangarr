package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})

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

func TestAPIBindingsEmptyReturnsJSONArrayNotNull(t *testing.T) {
	st := &fakeStore{}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})

	req := httptest.NewRequest(http.MethodGet, "/api/bindings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != "[]" {
		t.Errorf("empty bindings should marshal as []; got %q", body)
	}
}

func TestAPIRulesReturnsJSONListAscendingPriority(t *testing.T) {
	jp := "JP"
	st := &fakeStore{
		bindings: []model.Binding{{ID: 1, Name: "Manga"}},
		rules: []model.ClassificationRule{
			// fakeStore.ListRules returns these in declaration order; the
			// real store's ORDER BY priority is what the contract pins.
			// Declare them already-ascending so the handler can pass them
			// through unchanged — this test is a contract test, not a
			// store-sorting test.
			{ID: 1, Priority: 100, Name: "Japanese", Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: 1},
			{ID: 2, Priority: 200, Name: "Korean", Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: 1},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})

	req := httptest.NewRequest(http.MethodGet, "/api/rules", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type: want application/json, got %q", ct)
	}
	var got []model.ClassificationRule
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rules, got %d", len(got))
	}
	if got[0].Priority != 100 || got[1].Priority != 200 {
		t.Errorf("priority order: want 100 then 200, got %d then %d", got[0].Priority, got[1].Priority)
	}
}

func TestAPIRulesEmptyReturnsJSONArrayNotNull(t *testing.T) {
	st := &fakeStore{}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})

	req := httptest.NewRequest(http.MethodGet, "/api/rules", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	body := strings.TrimSpace(rec.Body.String())
	if body != "[]" {
		t.Errorf("empty rules should marshal as []; got %q", body)
	}
}

// TestSettingsPageRendersLibraryBindingsCard pins that the new card
// renders existing bindings as form rows with Name, LibraryRoot, the
// Kavita library dropdown, and a DefaultIsAdult checkbox.
func TestSettingsPageRendersLibraryBindingsCard(t *testing.T) {
	st := &fakeStore{
		bindings: []model.Binding{
			{ID: 1, Name: "Manga", LibraryRoot: "/media/Library/Manga", KavitaLibID: 1, DefaultIsAdult: false},
			{ID: 2, Name: "Manhwa 18+", LibraryRoot: "/media/Library/M18", KavitaLibID: 2, DefaultIsAdult: true},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if !strings.Contains(body, "Library Bindings") {
		t.Errorf("expected 'Library Bindings' heading in rendered HTML")
	}
	for _, want := range []string{"Manga", "/media/Library/Manga", "Manhwa 18+", "/media/Library/M18"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in rendered Library Bindings card", want)
		}
	}
	// DefaultIsAdult checkbox: the 18+ row (ID 2) should be checked.
	if !strings.Contains(body, `name="binding_default_is_adult_2" checked`) &&
		!strings.Contains(body, `name="binding_default_is_adult_2" value="on" checked`) {
		t.Errorf("expected DefaultIsAdult checkbox to be checked for binding ID 2; body excerpt: %s",
			excerpt(body, "binding_default_is_adult_2", 120))
	}
	// The non-adult row (ID 1) must NOT have a checked attribute.
	if strings.Contains(body, `name="binding_default_is_adult_1" checked`) {
		t.Errorf("expected DefaultIsAdult checkbox NOT checked for binding ID 1")
	}
}

// TestSettingsPageRendersAddBindingAffordance pins the + Add Binding
// button exists even when there are no bindings yet.
func TestSettingsPageRendersAddBindingAffordance(t *testing.T) {
	st := &fakeStore{}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "+ Add Binding") {
		t.Errorf("expected '+ Add Binding' affordance in rendered HTML")
	}
	// The <template> element is what powers the client-side add — pin it
	// with the placeholder token so Task 5's POST handler can rely on the
	// naming convention.
	if !strings.Contains(body, "__NEW_IDX__") {
		t.Errorf("expected '__NEW_IDX__' placeholder in the binding-row <template>")
	}
	if !strings.Contains(body, `id="binding-row-template"`) {
		t.Errorf("expected <template id=\"binding-row-template\"> in rendered HTML")
	}
}

// TestSettingsPageRendersClassificationRulesCard pins the new card renders
// the heading, each rule's name, and the "+ Add Rule" affordance.
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
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if !strings.Contains(body, "Classification Rules") {
		t.Errorf("expected 'Classification Rules' heading")
	}
	// html/template HTML-escapes "+" as "&#43;" inside attribute values
	// (e.g. <input value="Japanese 18+">), so we accept either form.
	// The "+ Add Rule" button text is in DOM content where "+" is not
	// escaped — match it literally.
	for _, want := range []string{"Japanese", "+ Add Rule"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in rendered Classification Rules card", want)
		}
	}
	if !strings.Contains(body, "Japanese 18+") && !strings.Contains(body, "Japanese 18&#43;") {
		t.Errorf("expected rule name 'Japanese 18+' (or HTML-escaped 'Japanese 18&#43;') in rendered Classification Rules card")
	}
	// The <template> for client-side row creation must exist.
	if !strings.Contains(body, `id="rule-row-template"`) {
		t.Errorf("expected <template id=\"rule-row-template\"> in rendered HTML")
	}
	// Form-field naming follows rule_<column>_<id>. Pin a few to lock
	// Task 5's POST parser contract.
	for _, want := range []string{
		`name="rule_id_10"`,
		`name="rule_priority_10"`,
		`name="rule_name_10"`,
		`name="rule_country_10"`,
		`name="rule_adult_10"`,
		`name="rule_format_10"`,
		`name="rule_path_10"`,
		`name="rule_binding_10"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected form field %q in rendered Classification Rules card", want)
		}
	}
}

// TestSettingsPageRulesSortedByPriorityAscending pins that rules render in
// the order ListRules() returns them. The store's ORDER BY priority ASC is
// the source of truth; the handler trusts that.
func TestSettingsPageRulesSortedByPriorityAscending(t *testing.T) {
	jp := "JP"
	st := &fakeStore{
		bindings: []model.Binding{{ID: 1, Name: "Manga"}},
		rules: []model.ClassificationRule{
			// Declare already-ascending so the handler can pass them
			// through unchanged. Contract test, not a store-sorting test.
			{ID: 3, Priority: 50, Name: "C", Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: 1},
			{ID: 1, Priority: 100, Name: "A", Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: 1},
			{ID: 2, Priority: 200, Name: "B", Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: 1},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	// Match the value attribute of the rule_name_* input — uniquely
	// scoped per rule, won't collide with other "A"/"B"/"C" strings on
	// the page.
	cPos := strings.Index(body, `name="rule_name_3"`)
	aPos := strings.Index(body, `name="rule_name_1"`)
	bPos := strings.Index(body, `name="rule_name_2"`)
	if cPos == -1 || aPos == -1 || bPos == -1 {
		t.Fatalf("expected all three rule name inputs in HTML; got positions C(id=3)=%d A(id=1)=%d B(id=2)=%d", cPos, aPos, bPos)
	}
	if !(cPos < aPos && aPos < bPos) {
		t.Errorf("expected C(p=50) before A(p=100) before B(p=200) by priority; positions C=%d A=%d B=%d", cPos, aPos, bPos)
	}
}

// TestSettingsPageRulesUnknownBindingRendersAsUnknown pins the
// "Unknown binding (ID: N)" rendering for rules whose BindingID points at
// a deleted binding. The user must still be able to edit and re-target
// such rules instead of being silently broken.
func TestSettingsPageRulesUnknownBindingRendersAsUnknown(t *testing.T) {
	jp := "JP"
	st := &fakeStore{
		bindings: []model.Binding{{ID: 1, Name: "Manga"}},
		rules: []model.ClassificationRule{
			{ID: 7, Priority: 100, Name: "Orphaned", Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: 999},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	if !strings.Contains(body, "Unknown binding (ID: 999)") {
		t.Errorf("expected 'Unknown binding (ID: 999)' for orphaned rule; body excerpt:\n%s",
			excerpt(body, "rule_binding_7", 200))
	}
}

// TestSettingsPageRendersDefaultBindingPicker pins the Default Binding
// dropdown: lists every binding plus a "— Send to Unmatched —" nil-sentinel,
// and pre-selects the saved DefaultBindingID.
func TestSettingsPageRendersDefaultBindingPicker(t *testing.T) {
	id := int64(2)
	st := &fakeStore{
		bindings: []model.Binding{
			{ID: 1, Name: "Manga"},
			{ID: 2, Name: "Catch-all"},
		},
		settings: model.Settings{
			DefaultBindingID:   &id,
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if !strings.Contains(body, "Default Binding") {
		t.Errorf("expected 'Default Binding' label in rendered HTML")
	}
	// The Catch-all binding (ID 2) must be the selected option for the
	// default_binding_id select.
	if !strings.Contains(body, `value="2" selected`) {
		t.Errorf("expected DefaultBindingID 2 to be selected; body excerpt:\n%s",
			excerpt(body, "default_binding_id", 240))
	}
	// The nil-sentinel "Send to Unmatched" option must be present.
	if !strings.Contains(body, "Send to Unmatched") {
		t.Errorf("expected 'Send to Unmatched' option in Default Binding dropdown")
	}
	// Field name pinned for Task 5's POST parser.
	if !strings.Contains(body, `name="default_binding_id"`) {
		t.Errorf("expected default_binding_id select field in rendered HTML")
	}
}

// TestSettingsPageDefaultBindingDefaultsToUnmatched pins that when no
// DefaultBindingID is saved the "Send to Unmatched" sentinel is selected.
func TestSettingsPageDefaultBindingDefaultsToUnmatched(t *testing.T) {
	st := &fakeStore{
		bindings: []model.Binding{{ID: 1, Name: "Manga"}},
		settings: model.Settings{
			LibraryRoots:       map[model.ContentType]string{},
			KavitaLibIDsByType: map[model.ContentType]int64{},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	// The 0 (Unmatched) sentinel must be selected. Match the option attribute
	// shape that html/template emits.
	if !strings.Contains(body, `value="0" selected`) {
		t.Errorf("expected Unmatched sentinel selected when DefaultBindingID is nil; body excerpt:\n%s",
			excerpt(body, "default_binding_id", 240))
	}
}

// TestSuwayomiOverridesDropdownIncludesAllBindings pins that the right-hand
// dropdown in the Suwayomi Category Overrides card lists EVERY binding, not
// just the three primary content-type Kavita libraries (Library Map Plan C
// reverse-lookup constraint, dissolved by Plan A).
func TestSuwayomiOverridesDropdownIncludesAllBindings(t *testing.T) {
	cats := []map[string]any{
		{"id": 10, "name": "Korean Webtoons", "order": 1},
	}
	srv := suwayomiStubServer(t, cats, 0)
	defer srv.Close()

	st := &fakeStore{
		bindings: []model.Binding{
			{ID: 1, Name: "Manga"},
			{ID: 2, Name: "Manga 18+"},
			// Light Novels is explicitly NON-primary — v1 dropdown would
			// have filtered this out via overrideLibraryChoices().
			{ID: 3, Name: "Light Novels"},
		},
		settings: model.Settings{
			SuwayomiBaseURL:  srv.URL,
			SuwayomiAuthType: model.SuwayomiAuthNone,
			// Use v1 override map so a row renders (Task 5 migrates the
			// read-source to SuwayomiCategoryBindings). The widening Task 4
			// owns is the dropdown's option list, regardless of row source.
			SuwayomiCategoryOverrides: map[int64]int64{10: 3},
			// KavitaLibIDsByType non-empty so the override card is NOT
			// blocked by the "configure AniList first" prompt.
			KavitaLibIDsByType: map[model.ContentType]int64{model.TypeManga: 1},
			LibraryRoots:       map[model.ContentType]string{},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})

	req := httptest.NewRequest(http.MethodGet, "/api/suwayomi/categories/fragment", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// Locate the override row's binding-select block — match between the
	// override_binding_0 select open and the </select> close.
	start := strings.Index(body, `name="override_binding_0"`)
	if start < 0 {
		t.Fatalf("override_binding_0 select missing from fragment; body:\n%s", body)
	}
	end := strings.Index(body[start:], "</select>")
	if end < 0 {
		t.Fatalf("</select> not found after override_binding_0; body:\n%s", body)
	}
	selectBlock := body[start : start+end]

	// All three bindings must appear as <option> labels INSIDE the binding
	// select — not just elsewhere on the fragment.
	for _, want := range []string{
		`>Manga<`,
		`>Manga 18&#43;<`, // html/template HTML-escapes '+'
		`>Light Novels<`,  // CRUCIAL: was filtered out in v1
	} {
		if !strings.Contains(selectBlock, want) {
			t.Errorf("expected %q in widened override-row binding dropdown; selectBlock:\n%s",
				want, selectBlock)
		}
	}
	// And the saved binding (ID 3 = Light Novels) is selected.
	if !strings.Contains(selectBlock, `value="3" selected`) {
		t.Errorf("expected binding ID 3 selected in override row; selectBlock:\n%s", selectBlock)
	}
}

// TestSuwayomiOverridesFragmentUsesBindingFormFieldName pins the rename from
// override_library_<idx> to override_binding_<idx>. Task 5 wires the POST
// parser; this task just renames the template field.
func TestSuwayomiOverridesFragmentUsesBindingFormFieldName(t *testing.T) {
	cats := []map[string]any{
		{"id": 5, "name": "Korean Webtoons", "order": 1},
	}
	srv := suwayomiStubServer(t, cats, 0)
	defer srv.Close()

	st := &fakeStore{
		bindings: []model.Binding{{ID: 2, Name: "Manhwa"}},
		settings: model.Settings{
			SuwayomiBaseURL:  srv.URL,
			SuwayomiAuthType: model.SuwayomiAuthNone,
			// v1 override map so a row renders. Task 5 migrates the read
			// source; Task 4 only renames the right-hand form field.
			SuwayomiCategoryOverrides: map[int64]int64{5: 2},
			KavitaLibIDsByType:        map[model.ContentType]int64{model.TypeManga: 1},
			LibraryRoots:              map[model.ContentType]string{},
		},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})

	req := httptest.NewRequest(http.MethodGet, "/api/suwayomi/categories/fragment", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// New v2 field name MUST be present.
	if !strings.Contains(body, `name="override_binding_0"`) {
		t.Errorf("expected override_binding_0 (v2 form-field rename) in fragment; body:\n%s", body)
	}
	// Old v1 field name MUST be gone.
	if strings.Contains(body, `name="override_library_0"`) {
		t.Errorf("v1 form-field name override_library_0 still present after rename; body:\n%s", body)
	}
}

// excerpt returns the substring of s around the first occurrence of needle.
// ----- Task 5: atomic POST persists bindings + rules + default + Suwayomi overrides -----

func TestPOSTSettingsPersistsNewBindings(t *testing.T) {
	st := &fakeStore{}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})

	form := url.Values{}
	// Existing v1 fields the saveSettings handler reads — keep them populated
	// so the existing flow doesn't error.
	form.Set("file_mode", "hardlink")
	form.Set("rename_scheme", "{series}/{series} - Ch.{chapter}.cbz")
	form.Set("poll_minutes", "15")
	// Two new bindings, both with ID=0 (new). Form-field naming convention
	// binding_<column>_<suffix>.
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
	// Form-suffix sort puts new1 before new2 lexicographically; fakeStore
	// preserves submission order.
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
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})

	form := url.Values{}
	form.Set("file_mode", "hardlink")
	form.Set("rename_scheme", "{series}/{series} - Ch.{chapter}.cbz")
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
		t.Fatalf("rule not persisted as expected: %+v", rules)
	}
	if rules[0].Condition.CountryOfOrigin == nil || *rules[0].Condition.CountryOfOrigin != "JP" {
		t.Errorf("rule CountryOfOrigin lost: %+v", rules[0].Condition)
	}
}

func TestPOSTSettingsPersistsDefaultBindingID(t *testing.T) {
	st := &fakeStore{
		bindings: []model.Binding{{ID: 5, Name: "Catch-all"}},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})

	form := url.Values{}
	form.Set("file_mode", "hardlink")
	form.Set("rename_scheme", "{series}/{series} - Ch.{chapter}.cbz")
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
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})

	form := url.Values{}
	form.Set("file_mode", "hardlink")
	form.Set("rename_scheme", "{series}/{series} - Ch.{chapter}.cbz")
	// v2 renamed the right-hand to override_binding_<idx>; verify Plan B
	// writes to SuwayomiCategoryBindings, NOT the v1 SuwayomiCategoryOverrides.
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
}

func TestPOSTSettingsEmptyDefaultBindingClearsTheField(t *testing.T) {
	id := int64(5)
	st := &fakeStore{
		bindings: []model.Binding{{ID: 5, Name: "Catch-all"}},
		settings: model.Settings{DefaultBindingID: &id},
	}
	h := NewHandler(HandlerOpts{Store: st, Runner: &fakeRunner{}})

	form := url.Values{}
	form.Set("file_mode", "hardlink")
	form.Set("rename_scheme", "{series}/{series} - Ch.{chapter}.cbz")
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

// Used to make test failures readable when the assertion is "body contains X".
func excerpt(s, needle string, width int) string {
	i := strings.Index(s, needle)
	if i < 0 {
		return "(needle not present)"
	}
	start := i - width/2
	if start < 0 {
		start = 0
	}
	end := i + len(needle) + width/2
	if end > len(s) {
		end = len(s)
	}
	return "..." + s[start:end] + "..."
}
