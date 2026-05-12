package caddyadmin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestBuildConfigSortsRoutesAndAddsHandlers(t *testing.T) {
	t.Parallel()

	cfg := BuildConfig([]HTTPRoute{
		{Host: "b.example.com", Upstreams: []string{"beta:8080"}},
		{Host: "a.example.com", Upstreams: []string{"alpha:8080"}},
	})

	routes := cfg.Apps.HTTP.Servers[defaultServerName].Routes
	if len(routes) != 2 {
		t.Fatalf("route count = %d", len(routes))
	}

	if got := routes[0].Match[0].Host[0]; got != "a.example.com" {
		t.Fatalf("first route host = %q", got)
	}

	if got := routes[0].Handle[0].Handler; got != "encode" {
		t.Fatalf("first handler = %q", got)
	}

	if got := routes[0].Handle[1].Upstreams[0].Dial; got != "alpha:8080" {
		t.Fatalf("first upstream = %q", got)
	}
}

func TestReconcileSkipsLoadWhenConfigMatches(t *testing.T) {
	t.Parallel()

	desired := BuildConfig([]HTTPRoute{
		{Host: "app.example.com", Upstreams: []string{"app:3000"}},
	})

	var loads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/config/":
			_ = json.NewEncoder(w).Encode(desired)
		case r.Method == http.MethodPost && r.URL.Path == "/load":
			loads.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	changed, err := client.Reconcile(context.Background(), desired)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if changed {
		t.Fatal("expected no change")
	}

	if got := loads.Load(); got != 0 {
		t.Fatalf("load calls = %d", got)
	}
}

func TestReconcileLoadsWhenConfigDiffers(t *testing.T) {
	t.Parallel()

	current := BuildConfig([]HTTPRoute{
		{Host: "old.example.com", Upstreams: []string{"old:8080"}},
	})
	desired := BuildConfig([]HTTPRoute{
		{Host: "new.example.com", Upstreams: []string{"new:8080"}},
	})

	var loads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/config/":
			_ = json.NewEncoder(w).Encode(current)
		case r.Method == http.MethodPost && r.URL.Path == "/load":
			loads.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := New(server.URL, server.Client())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	changed, err := client.Reconcile(context.Background(), desired)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if !changed {
		t.Fatal("expected config to change")
	}

	if got := loads.Load(); got != 1 {
		t.Fatalf("load calls = %d", got)
	}
}

func TestMergeManagedRoutesPreservesUnmanagedHosts(t *testing.T) {
	t.Parallel()

	current := []byte(`{
		"apps": {
			"http": {
				"servers": {
					"srv0": {
						"listen": [":80", ":443"],
						"routes": [
							{"match":[{"host":["legacy.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"legacy:80"}]}],"terminal":true},
							{"match":[{"host":["books.example.com"]}],"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"old-books:3000"}]}],"terminal":true}
						]
					}
				}
			}
		},
		"admin": {"disabled": false}
	}`)

	merged, err := MergeManagedRoutes(current, []HTTPRoute{
		{Host: "books.example.com", Upstreams: []string{"books:3000"}},
		{Host: "cameos.example.com", Upstreams: []string{"cameos:8080"}},
	}, []string{"books.example.com", "cameos.example.com"})
	if err != nil {
		t.Fatalf("MergeManagedRoutes() error = %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(merged, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if _, ok := decoded["admin"]; !ok {
		t.Fatal("expected admin block to be preserved")
	}

	servers := decoded["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)
	routes := servers["srv0"].(map[string]any)["routes"].([]any)

	if len(routes) != 3 {
		t.Fatalf("route count = %d", len(routes))
	}
}
