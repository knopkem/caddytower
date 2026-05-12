package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestImageRefCheckerReportsReachableImage(t *testing.T) {
	t.Parallel()

	var manifestHits int
	var tokenHits int

	var registry *httptest.Server
	registry = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/example/demo/manifests/latest":
			manifestHits++
			if r.Header.Get("Authorization") != "Bearer good-token" {
				w.Header().Set("Www-Authenticate", `Bearer realm="`+registry.URL+`/token",service="ghcr.io",scope="repository:example/demo:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusOK)
		case "/token":
			tokenHits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"good-token"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer registry.Close()

	checker := newImageRefChecker()
	checker.client = registry.Client()
	checker.baseURLForRegistry = func(string) string { return registry.URL }

	result := checker.Check(context.Background(), "ghcr.io/example/demo:latest")
	if !result.OK || result.Kind != "ok" {
		t.Fatalf("result = %#v", result)
	}
	if manifestHits < 2 {
		t.Fatalf("manifestHits = %d, want at least 2", manifestHits)
	}
	if tokenHits != 1 {
		t.Fatalf("tokenHits = %d, want 1", tokenHits)
	}
}

func TestImageRefCheckerReportsDeniedImage(t *testing.T) {
	t.Parallel()

	var registry *httptest.Server
	registry = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/example/demo/manifests/latest":
			if r.Header.Get("Authorization") != "Bearer good-token" {
				w.Header().Set("Www-Authenticate", `Bearer realm="`+registry.URL+`/token",service="ghcr.io",scope="repository:example/demo:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusForbidden)
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"good-token"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer registry.Close()

	checker := newImageRefChecker()
	checker.client = registry.Client()
	checker.baseURLForRegistry = func(string) string { return registry.URL }

	result := checker.Check(context.Background(), "ghcr.io/example/demo:latest")
	if result.OK || result.Kind != "denied" {
		t.Fatalf("result = %#v", result)
	}
}

func TestImageRefCheckerReportsNotFoundImage(t *testing.T) {
	t.Parallel()

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer registry.Close()

	checker := newImageRefChecker()
	checker.client = registry.Client()
	checker.baseURLForRegistry = func(string) string { return registry.URL }

	result := checker.Check(context.Background(), "ghcr.io/example/demo:latest")
	if result.OK || result.Kind != "not-found" {
		t.Fatalf("result = %#v", result)
	}
}

func TestImageRefCheckerRejectsInvalidReference(t *testing.T) {
	t.Parallel()

	result := newImageRefChecker().Check(context.Background(), "not a valid ref")
	if result.OK || result.Kind != "invalid" {
		t.Fatalf("result = %#v", result)
	}
}
