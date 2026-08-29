package filer

import (
	"strings"
	"testing"
)

func TestIsVolumeFile(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"Dragon Ball Z - Vol. 001 (English - Colour).cbz", true},
		{"Volume 3.cbz", true},
		{"Berserk vol.12.cbz", true},
		{"Vol.1 Ch.3.cbz", false}, // chapter marker wins
		{"Ch. 001.cbz", false},
		{"Official_Z 1.cbz", false},
		{"Evolution 5.cbz", false}, // "vol" inside a word is not a marker
		{"v01.cbz", false},         // rule is deliberately narrow
		{"Chainsaw Man 5.cbz", false},
	}
	for _, c := range cases {
		if got := IsVolumeFile(c.name); got != c.want {
			t.Errorf("IsVolumeFile(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestChapterNumberPrefersMarker(t *testing.T) {
	cases := map[string]string{
		"Ch. 001.cbz":             "001",
		"Ch. 7.5.cbz":             "7.5",
		"Vol.1 Ch.3.cbz":          "3", // marker beats first number
		"Chapter 42.cbz":          "42",
		"Official_Z 1.cbz":        "1", // no marker → first number
		"1.0.cbz":                 "1.0",
		"Witch Hat Atelier 5.cbz": "5", // "ch" inside a word is not a marker
		"Chainsaw Man 5.cbz":      "5",
		"noname.cbz":              "noname",
	}
	for in, want := range cases {
		if got := ChapterNumber(in); got != want {
			t.Errorf("ChapterNumber(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVolumeNumber(t *testing.T) {
	cases := map[string]string{
		"Dragon Ball Z - Vol. 001 (English - Colour).cbz": "001",
		"Volume 3.cbz": "3",
		"vol.12.cbz":   "12",
	}
	for in, want := range cases {
		if got := VolumeNumber(in); got != want {
			t.Errorf("VolumeNumber(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderVolumeName(t *testing.T) {
	got := RenderVolumeName("{series}/{series} - Vol.{volume}.cbz", "Dragon Ball Z (Color Edition)", "Dragon Ball Z - Vol. 001 (English - Colour).cbz")
	want := "Dragon Ball Z (Color Edition)/Dragon Ball Z (Color Edition) - Vol.001.cbz"
	if got != want {
		t.Fatalf("want %q got %q", want, got)
	}
}

func TestValidateVolumeScheme(t *testing.T) {
	ok := []string{"{series}/{series} - Vol.{volume}.cbz", "{series}/v{volume}.cbz"}
	for _, s := range ok {
		if err := ValidateVolumeScheme(s); err != nil {
			t.Errorf("ValidateVolumeScheme(%q) unexpected error: %v", s, err)
		}
	}
	bad := map[string]string{
		"":                                  "empty",
		"{series}/{series}.cbz":             "must contain {volume}",
		"{volume}.cbz":                      "must contain {series}",
		"{series}/{series} - {chapter}.cbz": "must contain {volume}",
		"{series}/{volume} {chapter}.cbz":   "must not contain {chapter}",
		"{series}/{volume} {foo}.cbz":       "unknown token {foo}",
		"../{series}/{volume}.cbz":          "..",
		"/{series}/{volume}.cbz":            "/",
	}
	for s, want := range bad {
		err := ValidateVolumeScheme(s)
		if err == nil {
			t.Errorf("ValidateVolumeScheme(%q) expected error containing %q, got nil", s, want)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ValidateVolumeScheme(%q) error %q does not mention %q", s, err.Error(), want)
		}
	}
}

func TestValidateSchemePairRequiresSameDir(t *testing.T) {
	if err := ValidateSchemePair("{series}/{series} - Ch.{chapter}.cbz", "{series}/{series} - Vol.{volume}.cbz"); err != nil {
		t.Fatalf("same-dir pair should validate: %v", err)
	}
	err := ValidateSchemePair("{series}/{series} - Ch.{chapter}.cbz", "{series}/Volumes/{series} - Vol.{volume}.cbz")
	if err == nil || !strings.Contains(err.Error(), "same directory") {
		t.Fatalf("diverging pair should fail with a same-directory message, got %v", err)
	}
}

// TestRenderNameUsesMarkerChapter pins the extractor change on the existing
// chapter path: "Vol.1 Ch.3" used to render as Ch.1.
func TestRenderNameUsesMarkerChapter(t *testing.T) {
	got := RenderName("{series}/{series} - Ch.{chapter}.cbz", "X", "Vol.1 Ch.3.cbz")
	if want := "X/X - Ch.3.cbz"; got != want {
		t.Fatalf("want %q got %q", want, got)
	}
}
