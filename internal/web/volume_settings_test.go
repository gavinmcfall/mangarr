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

// The volume rename scheme round-trips through the settings form and is
// rendered back into the page.
func TestSaveSettingsRoundTripsVolumeRenameScheme(t *testing.T) {
	h, st, _ := newTestHandler()
	stub := kavitaStubServer(t, nil, 0, 0)
	defer stub.Close()

	form := url.Values{
		"file_mode":            {"hardlink"},
		"rename_scheme":        {"{series}/{series} - Ch.{chapter}.cbz"},
		"volume_rename_scheme": {"{series}/{series} - v{volume}.cbz"},
		"poll_minutes":         {"15"},
		"kavita_base_url":      {stub.URL},
		"kavita_api_key":       {"k"},
	}
	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d; body: %s", rr.Code, rr.Body.String())
	}
	if st.settings.VolumeRenameScheme != "{series}/{series} - v{volume}.cbz" {
		t.Fatalf("volume scheme not persisted, got %q", st.settings.VolumeRenameScheme)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	body := rr2.Body.String()
	if !strings.Contains(body, `name="volume_rename_scheme"`) {
		t.Fatalf("settings page missing the volume scheme input")
	}
	if !strings.Contains(body, "Berserk - v3.cbz") {
		t.Fatalf("settings page should render the volume example, body:\n%s", body)
	}
}

// Omitting the field (older clients, pre-feature forms) applies the default
// instead of failing validation.
func TestSaveSettingsDefaultsVolumeRenameSchemeWhenOmitted(t *testing.T) {
	h, st, _ := newTestHandler()
	stub := kavitaStubServer(t, nil, 0, 0)
	defer stub.Close()

	form := url.Values{
		"file_mode":       {"hardlink"},
		"rename_scheme":   {"{series}/{series} - Ch.{chapter}.cbz"},
		"poll_minutes":    {"15"},
		"kavita_base_url": {stub.URL},
		"kavita_api_key":  {"k"},
	}
	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d; body: %s", rr.Code, rr.Body.String())
	}
	if st.settings.VolumeRenameScheme != model.DefaultVolumeRenameScheme {
		t.Fatalf("want default volume scheme, got %q", st.settings.VolumeRenameScheme)
	}
}

// A volume scheme that files into a different directory than the chapter
// scheme is rejected with the same-directory message; nothing is persisted.
func TestSaveSettingsRejectsDivergingVolumeScheme(t *testing.T) {
	h, st, _ := newTestHandler()
	savedBefore := st.settings.VolumeRenameScheme

	form := url.Values{
		"file_mode":            {"hardlink"},
		"rename_scheme":        {"{series}/{series} - Ch.{chapter}.cbz"},
		"volume_rename_scheme": {"{series}/Volumes/{series} - Vol.{volume}.cbz"},
		"poll_minutes":         {"15"},
		"kavita_base_url":      {"http://kavita:5000"},
		"kavita_api_key":       {"k"},
	}
	req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d; body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "same directory") {
		t.Fatalf("expected same-directory validation message, body:\n%s", rr.Body.String())
	}
	if st.settings.VolumeRenameScheme != savedBefore {
		t.Fatalf("settings were persisted despite validation failure")
	}
}

// JSON PUT: an omitted volume scheme gets the default; a bad one is a 400.
func TestAPIPutSettingsVolumeRenameScheme(t *testing.T) {
	h, st, _ := newTestHandler()

	ok := model.Settings{
		FileMode:     model.ModeHardlink,
		RenameScheme: "{series}/{series} - Ch.{chapter}.cbz",
		PollMinutes:  15,
		LibraryRoots: map[model.ContentType]string{model.TypeManga: "/lib/Manga"},
	}
	body, _ := json.Marshal(ok)
	req := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("want 204, got %d; body: %s", rr.Code, rr.Body.String())
	}
	if st.settings.VolumeRenameScheme != model.DefaultVolumeRenameScheme {
		t.Fatalf("omitted volume scheme should default, got %q", st.settings.VolumeRenameScheme)
	}

	bad := ok
	bad.VolumeRenameScheme = "{series}/{series} - Vol.{volume} {chapter}.cbz"
	body, _ = json.Marshal(bad)
	req = httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d; body: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v; body=%s", err, rr.Body.String())
	}
	if !strings.Contains(resp["error"], "must not contain {chapter}") {
		t.Fatalf("expected volume-scheme validation error, got %q", resp["error"])
	}
	if st.settings.VolumeRenameScheme != model.DefaultVolumeRenameScheme {
		t.Fatalf("bad scheme must not be persisted, got %q", st.settings.VolumeRenameScheme)
	}
}

// The activity filter offers the new conflict action.
func TestActivityActionsIncludeConflict(t *testing.T) {
	for _, a := range activityActions() {
		if a == string(model.ActionConflict) {
			return
		}
	}
	t.Fatalf("activityActions() does not include %q", model.ActionConflict)
}
