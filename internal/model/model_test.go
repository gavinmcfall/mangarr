package model

import "testing"

func TestSeriesHasTagsField(t *testing.T) {
	s := Series{Tags: []string{"webtoon", "r18"}}
	if len(s.Tags) != 2 || s.Tags[0] != "webtoon" {
		t.Fatalf("Tags field not wired: %+v", s.Tags)
	}
}
