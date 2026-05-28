package kavita

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestScanAuthenticatesThenScans(t *testing.T) {
	var hitAuth, hitScan bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/plugin/authenticate"):
			hitAuth = true
			w.Write([]byte(`{"token":"jwt123"}`))
		case strings.Contains(r.URL.Path, "/api/library/scan"):
			hitScan = true
			if r.Header.Get("Authorization") != "Bearer jwt123" {
				t.Errorf("missing bearer token")
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "APIKEY")
	if err := c.ScanLibrary(2); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !hitAuth || !hitScan {
		t.Fatalf("auth=%v scan=%v", hitAuth, hitScan)
	}
}

func TestScanNon2xxReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/plugin/authenticate"):
			w.Write([]byte(`{"token":"jwt123"}`))
		case strings.Contains(r.URL.Path, "/api/library/scan"):
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "APIKEY")
	err := c.ScanLibrary(2)
	if err == nil {
		t.Fatal("expected error on non-2xx scan response")
	}
}

func TestAuthFailureReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := New(srv.URL, "BADKEY")
	err := c.ScanLibrary(2)
	if err == nil {
		t.Fatal("expected error on auth failure")
	}
}
