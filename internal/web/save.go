package web

import (
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gavinmcfall/mangarr/internal/model"
)

// Form-field key prefix anchors. Each row submitted by the Settings page
// uses a suffix S in fields like binding_id_S, rule_id_S, etc. The regex
// anchors against the leading column name so unrelated form fields don't
// match.
var (
	bindingKeyRE = regexp.MustCompile(`^binding_id_(.+)$`)
	ruleKeyRE    = regexp.MustCompile(`^rule_id_(.+)$`)
)

// parseBindingsFromForm walks the form and reconstructs a []model.Binding.
// Each binding row uses a suffix S: binding_id_S, binding_name_S,
// binding_library_root_S, binding_kavita_lib_S, binding_default_is_adult_S.
// Rows where both Name and LibraryRoot are empty are dropped (treated as
// abandoned edits).
func parseBindingsFromForm(r *http.Request) ([]model.Binding, error) {
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	suffixes := collectSuffixes(r.Form, bindingKeyRE)
	out := make([]model.Binding, 0, len(suffixes))
	for _, s := range suffixes {
		name := strings.TrimSpace(r.FormValue("binding_name_" + s))
		root := strings.TrimSpace(r.FormValue("binding_library_root_" + s))
		if name == "" && root == "" {
			continue // abandoned row
		}
		id, _ := strconv.ParseInt(r.FormValue("binding_id_"+s), 10, 64)
		kavitaLib, _ := strconv.ParseInt(r.FormValue("binding_kavita_lib_"+s), 10, 64)
		isAdult := r.FormValue("binding_default_is_adult_"+s) != ""
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

// parseRulesFromForm walks the form and reconstructs []model.ClassificationRule.
// Each rule row uses a suffix S: rule_id_S, rule_priority_S, rule_name_S,
// rule_country_S, rule_adult_S, rule_format_S, rule_path_S, rule_binding_S.
// Rules with BindingID==0 (no target) or with no condition fields set at all
// (universal wildcard) are dropped — the DefaultBindingID picker is the
// supported way to express catch-all routing.
func parseRulesFromForm(r *http.Request) ([]model.ClassificationRule, error) {
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	suffixes := collectSuffixes(r.Form, ruleKeyRE)
	out := make([]model.ClassificationRule, 0, len(suffixes))
	for _, s := range suffixes {
		bindingID, _ := strconv.ParseInt(r.FormValue("rule_binding_"+s), 10, 64)
		if bindingID == 0 {
			continue
		}
		id, _ := strconv.ParseInt(r.FormValue("rule_id_"+s), 10, 64)
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

		// Universal wildcard (no constraint set anywhere) → reject. Users
		// should express catch-all routing via DefaultBindingID instead.
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

// parseDefaultBindingIDFromForm returns nil for "" / "0" (the "Send to
// Unmatched" sentinel) or a pointer to the parsed binding ID otherwise.
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

// collectSuffixes finds every form-key suffix matched by re. Result is
// sorted with a numeric-aware comparator: pure numeric suffixes come first
// in ascending order, then non-numeric in lexicographic order. This makes
// row iteration deterministic across map-iteration randomness so duplicate
// keys (rare, but possible if a row's id field is mis-edited) resolve the
// same way every time.
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
	sort.SliceStable(out, func(i, j int) bool {
		ai, errA := strconv.Atoi(out[i])
		bj, errB := strconv.Atoi(out[j])
		if errA == nil && errB == nil {
			return ai < bj
		}
		if errA == nil {
			return true
		}
		if errB == nil {
			return false
		}
		return out[i] < out[j]
	})
	return out
}
