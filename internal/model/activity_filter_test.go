package model

import "testing"

func TestActivityFilterZeroValueIsUnfiltered(t *testing.T) {
	var f ActivityFilter
	if f.Action != "" || f.SeriesLike != "" || f.Tag != "" || !f.After.IsZero() || f.Limit != 0 || f.Offset != 0 {
		t.Fatalf("zero ActivityFilter should be fully unfiltered: %+v", f)
	}
}

func TestActivityPageHoldsItemsAndTotal(t *testing.T) {
	p := ActivityPage{Items: []ActivityEntry{{ID: 1}}, Total: 42}
	if len(p.Items) != 1 || p.Total != 42 {
		t.Fatalf("ActivityPage shape wrong: %+v", p)
	}
}
