package kavita

import (
	"context"
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

func TestListLibrariesReturnsSorted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/plugin/authenticate"):
			w.Write([]byte(`{"token":"jwt123"}`))
		case strings.Contains(r.URL.Path, "/api/Library"):
			if r.Header.Get("Authorization") != "Bearer jwt123" {
				t.Errorf("missing bearer token on /api/Library")
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[{"id":3,"name":"Zebra","type":0},{"id":1,"name":"Apple","type":0},{"id":2,"name":"Mango","type":0}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "APIKEY")
	libs, err := c.ListLibraries(context.Background())
	if err != nil {
		t.Fatalf("ListLibraries: %v", err)
	}
	if len(libs) != 3 {
		t.Fatalf("want 3 libraries, got %d", len(libs))
	}
	// Sorted by Name case-insensitively.
	if libs[0].Name != "Apple" || libs[1].Name != "Mango" || libs[2].Name != "Zebra" {
		t.Fatalf("want [Apple, Mango, Zebra], got [%s, %s, %s]", libs[0].Name, libs[1].Name, libs[2].Name)
	}
	if libs[0].ID != 1 || libs[1].ID != 2 || libs[2].ID != 3 {
		t.Fatalf("IDs not preserved after sort: want [1,2,3], got [%d,%d,%d]", libs[0].ID, libs[1].ID, libs[2].ID)
	}
}

func TestListLibrariesAuthFailureReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := New(srv.URL, "BADKEY")
	_, err := c.ListLibraries(context.Background())
	if err == nil {
		t.Fatal("expected error on auth 401")
	}
}

func TestListLibrariesNon2xxReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/plugin/authenticate"):
			w.Write([]byte(`{"token":"jwt123"}`))
		case strings.Contains(r.URL.Path, "/api/Library"):
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "APIKEY")
	_, err := c.ListLibraries(context.Background())
	if err == nil {
		t.Fatal("expected error on /api/Library 500 response")
	}
}

func TestAuthEmptyTokenReturnsError(t *testing.T) {
	var hitScan bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/plugin/authenticate"):
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"token":""}`))
		case strings.Contains(r.URL.Path, "/api/library/scan"):
			hitScan = true
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "APIKEY")
	err := c.ScanLibrary(2)
	if err == nil {
		t.Fatal("expected error on empty token in auth response")
	}
	if hitScan {
		t.Fatal("scan endpoint should not be hit when token is empty")
	}
}
