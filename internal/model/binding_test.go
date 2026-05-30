package model

import (
	"encoding/json"
	"testing"
)

func TestBindingZeroValueIsUsable(t *testing.T) {
	b := Binding{}
	if b.ID != 0 || b.Name != "" || b.LibraryRoot != "" || b.KavitaLibID != 0 || b.DefaultIsAdult != false {
		t.Errorf("expected zero-value Binding to have all-zero fields, got %+v", b)
	}
}

func TestRuleConditionAllNilMeansWildcard(t *testing.T) {
	c := RuleCondition{}
	if c.CountryOfOrigin != nil || c.IsAdult != nil || c.Format != nil || c.SourcePathPrefix != nil {
		t.Errorf("expected zero-value RuleCondition to have all-nil pointer fields, got %+v", c)
	}
}

func TestRuleConditionJSONRoundTripPreservesNilVsExplicitZero(t *testing.T) {
	jpStr := "JP"
	falseVal := false
	original := RuleCondition{
		CountryOfOrigin: &jpStr,
		IsAdult:         &falseVal, // explicit false (not nil) must round-trip
		Format:          nil,       // nil must stay nil
	}

	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded RuleCondition
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.CountryOfOrigin == nil || *decoded.CountryOfOrigin != "JP" {
		t.Errorf("CountryOfOrigin round-trip lost: want JP, got %v", decoded.CountryOfOrigin)
	}
	if decoded.IsAdult == nil {
		t.Errorf("IsAdult round-trip lost: explicit false became nil")
	} else if *decoded.IsAdult != false {
		t.Errorf("IsAdult round-trip wrong value: want false, got %v", *decoded.IsAdult)
	}
	if decoded.Format != nil {
		t.Errorf("Format round-trip wrong: nil became %v", *decoded.Format)
	}
}

func TestClassificationRulePriorityIsExportedInt(t *testing.T) {
	r := ClassificationRule{
		ID:        1,
		Priority:  100,
		Name:      "Japanese",
		BindingID: 1,
	}
	if r.Priority != 100 {
		t.Errorf("Priority round-trip: want 100, got %d", r.Priority)
	}
}

func TestDecisionShape(t *testing.T) {
	d := Decision{BindingID: 42, Via: "rule:7"}
	if d.BindingID != 42 || d.Via != "rule:7" {
		t.Errorf("Decision round-trip wrong: %+v", d)
	}
}

func TestRuleConditionIsPathOnlyTrueOnlyWhenOnlyPathIsSet(t *testing.T) {
	path := "/x"
	jp := "JP"

	pathOnly := RuleCondition{SourcePathPrefix: &path}
	if !pathOnly.IsPathOnly() {
		t.Errorf("expected IsPathOnly true when only SourcePathPrefix is set")
	}

	mixed := RuleCondition{SourcePathPrefix: &path, CountryOfOrigin: &jp}
	if mixed.IsPathOnly() {
		t.Errorf("expected IsPathOnly false when path AND country are both set")
	}

	noPath := RuleCondition{CountryOfOrigin: &jp}
	if noPath.IsPathOnly() {
		t.Errorf("expected IsPathOnly false when only country is set")
	}

	empty := RuleCondition{}
	if empty.IsPathOnly() {
		t.Errorf("expected IsPathOnly false on empty (wildcard) condition")
	}
}
