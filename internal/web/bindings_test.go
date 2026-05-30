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

// excerpt returns the substring of s around the first occurrence of needle.
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
