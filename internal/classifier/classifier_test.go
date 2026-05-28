package classifier

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gavinmcfall/mangarr/internal/model"
)

func TestClassifyMapsCountry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"Media":{"countryOfOrigin":"KR"}}}`))
	}))
	defer srv.Close()

	c := New(srv.URL)
	got, err := c.Classify("Solo Leveling")
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got != model.TypeManhwa {
		t.Fatalf("want Manhwa, got %q", got)
	}
}

func TestClassifyNoMatchReturnsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"Media":null}}`))
	}))
	defer srv.Close()
	got, err := New(srv.URL).Classify("zzzznotreal")
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if got != model.TypeUnknown {
		t.Fatalf("want Unknown, got %q", got)
	}
}
