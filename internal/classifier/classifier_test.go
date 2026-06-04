package classifier

import (
	"context"
	"fmt"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/anilist"
	"github.com/gavinmcfall/mangarr/internal/mangadex"
	"github.com/gavinmcfall/mangarr/internal/model"
	"github.com/gavinmcfall/mangarr/internal/suwayomi"
)

// stubPathLookup is a minimal PathLookup for tests that want to assert
// behaviour with a deliberately tiny surface (e.g. "no entries" or "a
// single planted entry"). Production wires *suwayomi.PathCache.
type stubPathLookup struct {
	entries map[string]suwayomi.CacheEntry
}

func (s *stubPathLookup) Lookup(parentDir string) (suwayomi.CacheEntry, bool) {
	if s == nil {
		return suwayomi.CacheEntry{}, false
	}
	e, ok := s.entries[parentDir]
	return e, ok
}

// ---------- six-step Classify(ctx, ScanItem) → Decision ----------

// fakeBindingsRulesStore implements SettingsReader for Classify tests.
// The settings field is the source of truth for DefaultBindingID and
// SuwayomiCategoryBindings; tests populate it directly.
type fakeBindingsRulesStore struct {
	bindings []model.Binding
	rules    []model.ClassificationRule
	settings model.Settings
}

func (s *fakeBindingsRulesStore) ListBindings() ([]model.Binding, error) {
	return s.bindings, nil
}

func (s *fakeBindingsRulesStore) ListRules() ([]model.ClassificationRule, error) {
	return s.rules, nil
}

func (s *fakeBindingsRulesStore) GetSettings() (model.Settings, error) {
	return s.settings, nil
}

// fakeAniListV2 satisfies AniListClient. onLookup fires each call so
// tests can assert AniList was (or was not) consulted.
type fakeAniListV2 struct {
	result    anilist.Result
	err       error
	callCount int
	onLookup  func(title string)
}

func (f *fakeAniListV2) Lookup(ctx context.Context, title string) (anilist.Result, error) {
	f.callCount++
	if f.onLookup != nil {
		f.onLookup(title)
	}
	return f.result, f.err
}

// fakeMangaDex satisfies MangaDexClient for step-4b fallback tests.
type fakeMangaDex struct {
	result    mangadex.Result
	err       error
	callCount int
}

func (f *fakeMangaDex) Lookup(ctx context.Context, title string) (mangadex.Result, error) {
	f.callCount++
	return f.result, f.err
}

func TestClassifyPathOnlyRuleShortCircuits(t *testing.T) {
	prefix := "/media/Downloads/comics/"
	st := &fakeBindingsRulesStore{
		bindings: []model.Binding{{ID: 7, Name: "Comics", LibraryRoot: "/m/c", KavitaLibID: 9}},
		rules: []model.ClassificationRule{
			{ID: 1, Priority: 10, Name: "comics-by-path",
				Condition: model.RuleCondition{SourcePathPrefix: &prefix}, BindingID: 7},
		},
	}
	al := &fakeAniListV2{}
	c := New(al, nil, st)

	d, err := c.Classify(context.Background(), ScanItem{Title: "Anything", ParentDir: "/media/Downloads/comics/Foo"})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if d.BindingID != 7 {
		t.Errorf("expected BindingID 7, got %d", d.BindingID)
	}
	if d.Via != "path-rule:1" {
		t.Errorf("expected Via path-rule:1, got %q", d.Via)
	}
	if al.callCount != 0 {
		t.Errorf("expected AniList NOT called when path-rule short-circuits, got %d calls", al.callCount)
	}
}

func TestClassifySuwayomiOverrideRoutes(t *testing.T) {
	st := &fakeBindingsRulesStore{
		bindings: []model.Binding{{ID: 5, Name: "Manhwa"}},
		settings: model.Settings{SuwayomiCategoryBindings: map[int64]int64{42: 5}},
	}
	pl := &stubPathLookup{entries: map[string]suwayomi.CacheEntry{
		"/dl/suwayomi/x": {MangaID: 1, CategoryIDs: []int64{42}},
	}}
	al := &fakeAniListV2{}
	c := New(al, pl, st)

	d, err := c.Classify(context.Background(), ScanItem{Title: "X", ParentDir: "/dl/suwayomi/x"})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if d.BindingID != 5 || d.Via != "suwayomi-override:category=42" {
		t.Errorf("expected BindingID 5 + Via suwayomi-override:category=42, got %+v", d)
	}
	if al.callCount != 0 {
		t.Errorf("expected AniList NOT called when Suwayomi override hits, got %d calls", al.callCount)
	}
}

// TestClassifySuwayomiOverrideFirstMatchWins pins the contract that the
// PathCache returns CategoryIDs sorted ascending by Suwayomi category.order
// and the classifier walks them in that order — first ID present in
// SuwayomiCategoryBindings wins, even when later IDs also map.
func TestClassifySuwayomiOverrideFirstMatchWins(t *testing.T) {
	st := &fakeBindingsRulesStore{
		bindings: []model.Binding{{ID: 5, Name: "Manhwa"}, {ID: 6, Name: "Manhua"}},
		settings: model.Settings{SuwayomiCategoryBindings: map[int64]int64{42: 5, 99: 6}},
	}
	pl := &stubPathLookup{entries: map[string]suwayomi.CacheEntry{
		"/dl/multi": {MangaID: 1, CategoryIDs: []int64{42, 99}},
	}}
	c := New(&fakeAniListV2{}, pl, st)

	d, _ := c.Classify(context.Background(), ScanItem{Title: "X", ParentDir: "/dl/multi"})
	if d.BindingID != 5 {
		t.Errorf("first-match-wins: want BindingID 5 (cat 42), got %d", d.BindingID)
	}
	if d.Via != "suwayomi-override:category=42" {
		t.Errorf("want Via suwayomi-override:category=42, got %q", d.Via)
	}
}

func TestClassifyAniListRuleMatches(t *testing.T) {
	jp := "JP"
	st := &fakeBindingsRulesStore{
		bindings: []model.Binding{{ID: 1, Name: "Manga"}},
		rules: []model.ClassificationRule{
			{ID: 5, Priority: 100, Name: "Japanese",
				Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: 1},
		},
	}
	al := &fakeAniListV2{result: anilist.Result{CountryOfOrigin: "JP"}}
	c := New(al, nil, st)

	d, _ := c.Classify(context.Background(), ScanItem{Title: "Bleach", ParentDir: "/dl/suwayomi/bleach"})
	if d.BindingID != 1 || d.Via != "rule:5" {
		t.Errorf("expected {BindingID:1, Via:rule:5}, got %+v", d)
	}
}

func TestClassifyFirstMatchWinsByPriority(t *testing.T) {
	jp := "JP"
	yes := true
	st := &fakeBindingsRulesStore{
		bindings: []model.Binding{{ID: 1, Name: "Manga"}, {ID: 2, Name: "Manga 18+"}},
		rules: []model.ClassificationRule{
			// Lower priority number = walked first. "Japanese 18+" at 50 wins
			// over the looser "Japanese" at 100.
			{ID: 10, Priority: 50, Name: "Japanese 18+",
				Condition: model.RuleCondition{CountryOfOrigin: &jp, IsAdult: &yes}, BindingID: 2},
			{ID: 11, Priority: 100, Name: "Japanese",
				Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: 1},
		},
	}
	al := &fakeAniListV2{result: anilist.Result{CountryOfOrigin: "JP", IsAdult: true}}
	c := New(al, nil, st)

	d, _ := c.Classify(context.Background(), ScanItem{Title: "X"})
	if d.BindingID != 2 {
		t.Errorf("expected 18+ binding (ID 2) to win, got BindingID %d", d.BindingID)
	}
	if d.Via != "rule:10" {
		t.Errorf("expected Via rule:10, got %q", d.Via)
	}
}

func TestClassifyMixedConditionEvaluatedInStepFourNotStepOne(t *testing.T) {
	prefix := "/media/Downloads/x/"
	jp := "JP"
	st := &fakeBindingsRulesStore{
		bindings: []model.Binding{{ID: 1, Name: "Manga"}},
		rules: []model.ClassificationRule{
			// Path + country: NOT path-only (CountryOfOrigin is also set),
			// must wait for AniList result in step 4.
			{ID: 1, Priority: 50, Name: "mixed",
				Condition: model.RuleCondition{SourcePathPrefix: &prefix, CountryOfOrigin: &jp}, BindingID: 1},
		},
	}
	al := &fakeAniListV2{result: anilist.Result{CountryOfOrigin: "JP"}}
	c := New(al, nil, st)

	d, _ := c.Classify(context.Background(), ScanItem{Title: "X", ParentDir: "/media/Downloads/x/foo"})
	if d.BindingID != 1 || d.Via != "rule:1" {
		t.Errorf("expected mixed rule to match in step 4, got %+v", d)
	}
	if al.callCount == 0 {
		t.Errorf("expected AniList to be called for mixed condition, but it was not")
	}
}

func TestClassifyDefaultBindingFallback(t *testing.T) {
	defaultID := int64(42)
	st := &fakeBindingsRulesStore{
		bindings: []model.Binding{{ID: 42, Name: "Default"}},
		settings: model.Settings{DefaultBindingID: &defaultID},
	}
	al := &fakeAniListV2{result: anilist.Result{CountryOfOrigin: "JP"}}
	c := New(al, nil, st)

	d, _ := c.Classify(context.Background(), ScanItem{Title: "Unmatched"})
	if d.BindingID != 42 || d.Via != "default-binding" {
		t.Errorf("expected default-binding fallback, got %+v", d)
	}
}

func TestClassifyUnmatchedWhenNoDefault(t *testing.T) {
	st := &fakeBindingsRulesStore{}
	al := &fakeAniListV2{result: anilist.Result{CountryOfOrigin: "JP"}}
	c := New(al, nil, st)

	d, _ := c.Classify(context.Background(), ScanItem{Title: "Nothing"})
	if d.BindingID != 0 || d.Via != "unmatched" {
		t.Errorf("expected unmatched, got %+v", d)
	}
}

// TestClassifyAniListErrorFallsThroughToDefaultOrUnmatched pins that an
// AniList error (network, ErrNotFound, anything) is swallowed — the
// classifier falls through to step 5 (default-binding) and finally step 6
// (unmatched). The error is intentionally not propagated to the poller
// because a transient AniList outage should still let the poller route
// to the configured default or queue for Unmatched, not crash the tick.
func TestClassifyAniListErrorFallsThroughToDefaultOrUnmatched(t *testing.T) {
	st := &fakeBindingsRulesStore{}
	al := &fakeAniListV2{err: fmt.Errorf("simulated network failure")}
	c := New(al, nil, st)

	d, err := c.Classify(context.Background(), ScanItem{Title: "X"})
	if err != nil {
		t.Errorf("expected Classify to swallow AniList error and fall through, got %v", err)
	}
	if d.Via != "unmatched" {
		t.Errorf("expected fallback to unmatched on AniList error, got Via=%q", d.Via)
	}
}

// TestClassifyAniListErrorWithDefaultRoutesToDefault complements the above
// — when AniList errors AND a default binding is configured, the default
// should win (step 5 runs whether or not step 4 produced a hit).
func TestClassifyAniListErrorWithDefaultRoutesToDefault(t *testing.T) {
	defaultID := int64(99)
	st := &fakeBindingsRulesStore{
		settings: model.Settings{DefaultBindingID: &defaultID},
	}
	al := &fakeAniListV2{err: fmt.Errorf("network down")}
	c := New(al, nil, st)

	d, _ := c.Classify(context.Background(), ScanItem{Title: "Whatever"})
	if d.BindingID != 99 || d.Via != "default-binding" {
		t.Errorf("expected default fallback on AniList error, got %+v", d)
	}
}

// TestClassifyNilSuwayomiSkipsOverrideStep ensures a nil PathLookup is
// safe — Suwayomi was never wired. The classifier must skip step 2/3
// without panicking and proceed to step 4 (AniList rules) then fallback.
func TestClassifyNilSuwayomiSkipsOverrideStep(t *testing.T) {
	jp := "JP"
	st := &fakeBindingsRulesStore{
		bindings: []model.Binding{{ID: 1, Name: "Manga"}},
		rules: []model.ClassificationRule{
			{ID: 5, Priority: 100, Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: 1},
		},
	}
	al := &fakeAniListV2{result: anilist.Result{CountryOfOrigin: "JP"}}
	c := New(al, nil, st)

	d, err := c.Classify(context.Background(), ScanItem{Title: "X", ParentDir: "/dl/x"})
	if err != nil {
		t.Fatalf("Classify with nil suwayomi: %v", err)
	}
	if d.BindingID != 1 || d.Via != "rule:5" {
		t.Errorf("expected rule:5 match with nil suwayomi, got %+v", d)
	}
}

// TestStripTrailingTag pins the behaviour of the helper used by the
// AniList retry path. Documents all the shapes we expect to handle and
// the cases left intentionally alone.
func TestStripTrailingTag(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Dragon Ball Super (Color)", "Dragon Ball Super"},
		{"Made in Abyss [Official Colored Edition]", "Made in Abyss"},
		{"Series Name (2024)", "Series Name"},
		{"Foo (Bar (1))", "Foo"},        // nested groups
		{"  Trim Spaces (Color)  ", "Trim Spaces"},
		{"No Tag Here", "No Tag Here"},  // unchanged
		{"K-On!", "K-On!"},              // punctuation but no paren
		{"Tokyo Ghoul:re", "Tokyo Ghoul:re"},
		{"", ""},
		{"Unbalanced (", "Unbalanced ("}, // leave as-is
		{"Mid-string (paren) tail", "Mid-string (paren) tail"}, // only trailing tags strip
	}
	for _, c := range cases {
		if got := stripTrailingTag(c.in); got != c.want {
			t.Errorf("stripTrailingTag(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestClassifyRetriesAniListWithBareTitleOnNotFound pins the retry
// path: when the first AniList lookup fails (ErrNotFound, network
// blip, etc.) AND the title has a trailing tag, the classifier
// retries once with the bare title. If the bare title matches, the
// resulting Result drives the rule walk just like a fresh lookup.
func TestClassifyRetriesAniListWithBareTitleOnNotFound(t *testing.T) {
	jp := "JP"
	st := &fakeBindingsRulesStore{
		bindings: []model.Binding{{ID: 1, Name: "Manga", LibraryRoot: "/m/jp", KavitaLibID: 11}},
		rules: []model.ClassificationRule{
			{ID: 7, Priority: 100, Name: "JP",
				Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: 1},
		},
	}
	calls := []string{}
	// scriptedAniList replays a queued sequence of (Result, error) pairs
	// so we can simulate first-fail / second-succeed without state mgmt
	// in the test body.
	al := &scriptedAniList{
		responses: []scriptedResponse{
			{err: anilist.ErrNotFound},
			{result: anilist.Result{CountryOfOrigin: "JP"}},
		},
		onLookup: func(title string) { calls = append(calls, title) },
	}
	c := New(al, nil, st)

	d, err := c.Classify(context.Background(),
		ScanItem{Title: "Dragon Ball Super (Color)", ParentDir: "/dl"})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if d.BindingID != 1 || d.Via != "rule:7" {
		t.Errorf("expected rule:7 match after retry, got %+v", d)
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 AniList calls (original + retry), got %d: %v", len(calls), calls)
	}
	if calls[0] != "Dragon Ball Super (Color)" {
		t.Errorf("first call should use original title; got %q", calls[0])
	}
	if calls[1] != "Dragon Ball Super" {
		t.Errorf("retry should use bare title; got %q", calls[1])
	}
}

// TestClassifyDoesNotRetryWhenNoTrailingTag pins that titles without a
// parenthetical/bracketed tail don't trigger a second AniList call —
// no point burning AniList budget when the helper would return the
// same string.
func TestClassifyDoesNotRetryWhenNoTrailingTag(t *testing.T) {
	st := &fakeBindingsRulesStore{}
	al := &scriptedAniList{
		responses: []scriptedResponse{{err: anilist.ErrNotFound}},
	}
	c := New(al, nil, st)

	_, err := c.Classify(context.Background(),
		ScanItem{Title: "Plain Title", ParentDir: "/dl"})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if al.callCount != 1 {
		t.Errorf("expected exactly 1 AniList call when no trailing tag; got %d", al.callCount)
	}
}

// scriptedAniList satisfies AniListClient by returning queued responses
// in order. Used by retry-path tests to simulate first-call failure +
// second-call success without test-body state management.
type scriptedAniList struct {
	responses []scriptedResponse
	callCount int
	onLookup  func(title string)
}

type scriptedResponse struct {
	result anilist.Result
	err    error
}

func (s *scriptedAniList) Lookup(ctx context.Context, title string) (anilist.Result, error) {
	if s.onLookup != nil {
		s.onLookup(title)
	}
	i := s.callCount
	s.callCount++
	if i >= len(s.responses) {
		return anilist.Result{}, anilist.ErrNotFound
	}
	return s.responses[i].result, s.responses[i].err
}

// TestClassifyManualBindingShortCircuits pins the v2 reclassify path:
// when ScanItem carries a ManualBindingID, the classifier returns
// immediately with Via="manual" — before AniList, before rules, before
// the default-binding fallback. Used by the Series-page reclassify
// control to pin a series at a binding when AniList has no match or
// the operator wants to override the rule chain.
func TestClassifyManualBindingShortCircuits(t *testing.T) {
	st := &fakeBindingsRulesStore{}
	// Even if everything else is wired, manual override wins.
	al := &fakeAniListV2{result: anilist.Result{CountryOfOrigin: "JP"}}
	c := New(al, nil, st)

	pin := int64(42)
	d, err := c.Classify(context.Background(), ScanItem{
		Title:           "Some Series",
		ParentDir:       "/dl",
		ManualBindingID: &pin,
	})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if d.BindingID != 42 || d.Via != "manual" {
		t.Errorf("expected BindingID 42 + Via manual, got %+v", d)
	}
	if al.callCount != 0 {
		t.Errorf("AniList must NOT be called when manual override is set; got %d calls", al.callCount)
	}
}

// TestClassifyManualBindingZeroIsIgnored pins that a *int64 pointing
// at 0 (not really "set" — defensive) doesn't short-circuit. nil OR
// *0 means "no override; classify normally".
func TestClassifyManualBindingZeroIsIgnored(t *testing.T) {
	jp := "JP"
	st := &fakeBindingsRulesStore{
		bindings: []model.Binding{{ID: 1, Name: "Manga"}},
		rules: []model.ClassificationRule{
			{ID: 5, Priority: 100, Name: "JP",
				Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: 1},
		},
	}
	al := &fakeAniListV2{result: anilist.Result{CountryOfOrigin: "JP"}}
	c := New(al, nil, st)

	zero := int64(0)
	d, err := c.Classify(context.Background(), ScanItem{
		Title:           "X",
		ParentDir:       "/dl",
		ManualBindingID: &zero,
	})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if d.BindingID != 1 || d.Via != "rule:5" {
		t.Errorf("expected rule:5 match when override is *0, got %+v", d)
	}
}

// --- Step 4b: MangaDex fallback ---

// kr/jp helpers are declared per-test as needed; this block exercises the
// fallback that fires only when AniList produced no rule match.

func TestClassifyMangaDexFallbackResolvesWhenAniListMisses(t *testing.T) {
	kr := "KR"
	st := &fakeBindingsRulesStore{
		bindings: []model.Binding{{ID: 5, Name: "Manhwa"}},
		rules: []model.ClassificationRule{
			{ID: 6, Priority: 200, Name: "KR",
				Condition: model.RuleCondition{CountryOfOrigin: &kr}, BindingID: 5},
		},
	}
	// AniList returns NotFound — the catalogue gap case (e.g. a manhwa
	// only present on AniList as its anime adaptation, or not at all).
	al := &fakeAniListV2{err: anilist.ErrNotFound}
	md := &fakeMangaDex{result: mangadex.Result{OriginalLanguage: "ko"}}
	c := New(al, nil, st).WithMangaDex(md)

	d, err := c.Classify(context.Background(), ScanItem{Title: "Legend of the Northern Blade", ParentDir: "/dl/lonb"})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if d.BindingID != 5 || d.Via != "mangadex-rule:6" {
		t.Errorf("expected {BindingID:5, Via:mangadex-rule:6}, got %+v", d)
	}
	if md.callCount != 1 {
		t.Errorf("expected MangaDex consulted once, got %d", md.callCount)
	}
}

func TestClassifyMangaDexFallbackNotConsultedWhenAniListMatches(t *testing.T) {
	jp := "JP"
	st := &fakeBindingsRulesStore{
		bindings: []model.Binding{{ID: 1, Name: "Manga"}},
		rules: []model.ClassificationRule{
			{ID: 5, Priority: 100, Name: "JP",
				Condition: model.RuleCondition{CountryOfOrigin: &jp}, BindingID: 1},
		},
	}
	al := &fakeAniListV2{result: anilist.Result{CountryOfOrigin: "JP"}}
	md := &fakeMangaDex{result: mangadex.Result{OriginalLanguage: "ko"}}
	c := New(al, nil, st).WithMangaDex(md)

	d, _ := c.Classify(context.Background(), ScanItem{Title: "Bleach", ParentDir: "/dl/bleach"})
	if d.Via != "rule:5" {
		t.Errorf("expected AniList rule:5, got %+v", d)
	}
	if md.callCount != 0 {
		t.Errorf("MangaDex must NOT be consulted when AniList already matched; got %d calls", md.callCount)
	}
}

func TestClassifyMangaDexFallbackEnLanguageFallsThrough(t *testing.T) {
	kr := "KR"
	defBinding := int64(99)
	st := &fakeBindingsRulesStore{
		bindings: []model.Binding{{ID: 5, Name: "Manhwa"}, {ID: 99, Name: "Default"}},
		rules: []model.ClassificationRule{
			{ID: 6, Priority: 200, Name: "KR",
				Condition: model.RuleCondition{CountryOfOrigin: &kr}, BindingID: 5},
		},
		settings: model.Settings{DefaultBindingID: &defBinding},
	}
	al := &fakeAniListV2{err: anilist.ErrNotFound}
	// en-origin (e.g. The Beginning After the End) — must NOT map to any
	// country rule; falls through to the default binding.
	md := &fakeMangaDex{result: mangadex.Result{OriginalLanguage: "en"}}
	c := New(al, nil, st).WithMangaDex(md)

	d, _ := c.Classify(context.Background(), ScanItem{Title: "The Beginning After the End", ParentDir: "/dl/tbate"})
	if d.Via != ViaDefaultBinding || d.BindingID != 99 {
		t.Errorf("en-origin must fall through to default binding, got %+v", d)
	}
}

func TestClassifyMangaDexFallbackErrorIsSwallowed(t *testing.T) {
	st := &fakeBindingsRulesStore{
		bindings: []model.Binding{{ID: 1, Name: "Manga"}},
		rules:    []model.ClassificationRule{},
	}
	al := &fakeAniListV2{err: anilist.ErrNotFound}
	md := &fakeMangaDex{err: fmt.Errorf("mangadex rate limited")}
	c := New(al, nil, st).WithMangaDex(md)

	d, err := c.Classify(context.Background(), ScanItem{Title: "X", ParentDir: "/dl/x"})
	if err != nil {
		t.Fatalf("MangaDex transport error must be swallowed, got %v", err)
	}
	if d.Via != ViaUnmatched {
		t.Errorf("expected unmatched fall-through on MangaDex error, got %+v", d)
	}
}

func TestClassifyNilMangaDexDisablesFallback(t *testing.T) {
	st := &fakeBindingsRulesStore{
		bindings: []model.Binding{{ID: 1, Name: "Manga"}},
		rules:    []model.ClassificationRule{},
	}
	al := &fakeAniListV2{err: anilist.ErrNotFound}
	c := New(al, nil, st) // no WithMangaDex

	d, err := c.Classify(context.Background(), ScanItem{Title: "X", ParentDir: "/dl/x"})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if d.Via != ViaUnmatched {
		t.Errorf("expected unmatched with no fallback wired, got %+v", d)
	}
}
