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
