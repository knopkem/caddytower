package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchLatestReleaseUsesAPIWhenAvailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/knopkem/caddytower/releases/latest" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v1.2.3","html_url":"https://github.com/knopkem/caddytower/releases/tag/v1.2.3"}`))
	}))
	defer server.Close()

	previousClient := releaseLookupHTTPClient
	previousAPI := releaseLookupAPIBaseURL
	previousWeb := releaseLookupWebBaseURL
	releaseLookupHTTPClient = server.Client()
	releaseLookupAPIBaseURL = server.URL
	releaseLookupWebBaseURL = server.URL
	t.Cleanup(func() {
		releaseLookupHTTPClient = previousClient
		releaseLookupAPIBaseURL = previousAPI
		releaseLookupWebBaseURL = previousWeb
	})

	release, err := fetchLatestRelease(context.Background(), "knopkem", "caddytower")
	if err != nil {
		t.Fatalf("fetchLatestRelease() error = %v", err)
	}
	if release.TagName != "v1.2.3" || release.HTMLURL != "https://github.com/knopkem/caddytower/releases/tag/v1.2.3" {
		t.Fatalf("release = %#v", release)
	}
}

func TestFetchLatestReleaseFallsBackToWebRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/knopkem/caddytower/releases/latest":
			http.Error(w, "rate limited", http.StatusForbidden)
		case "/knopkem/caddytower/releases/latest":
			http.Redirect(w, r, "/knopkem/caddytower/releases/tag/v1.2.4", http.StatusFound)
		case "/knopkem/caddytower/releases/tag/v1.2.4":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	previousClient := releaseLookupHTTPClient
	previousAPI := releaseLookupAPIBaseURL
	previousWeb := releaseLookupWebBaseURL
	releaseLookupHTTPClient = server.Client()
	releaseLookupAPIBaseURL = server.URL
	releaseLookupWebBaseURL = server.URL
	t.Cleanup(func() {
		releaseLookupHTTPClient = previousClient
		releaseLookupAPIBaseURL = previousAPI
		releaseLookupWebBaseURL = previousWeb
	})

	release, err := fetchLatestRelease(context.Background(), "knopkem", "caddytower")
	if err != nil {
		t.Fatalf("fetchLatestRelease() error = %v", err)
	}
	if release.TagName != "v1.2.4" || release.HTMLURL != server.URL+"/knopkem/caddytower/releases/tag/v1.2.4" {
		t.Fatalf("release = %#v", release)
	}
}
