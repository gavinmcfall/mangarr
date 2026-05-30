package web

import (
	"encoding/json"
	"net/http"

	"github.com/gavinmcfall/mangarr/internal/model"
)

// apiBindings is the JSON read-only endpoint at GET /api/bindings.
// Returns every binding in the order the store returns them (ascending
// by Name per the store's ORDER BY). Useful for power users scripting
// via curl and for the Plan B Settings UI's Library Bindings card.
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
	_ = json.NewEncoder(w).Encode(bindings)
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
