package cloudflare

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestValidateToken(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret-token" {
			t.Fatalf("authorization header = %q", got)
		}
		_ = json.NewEncoder(w).Encode(apiResponse[map[string]any]{
			Success: true,
			Result:  map[string]any{"status": "active"},
		})
	}))
	defer server.Close()

	client, err := NewWithBaseURL("secret-token", server.URL, server.Client())
	if err != nil {
		t.Fatalf("NewWithBaseURL() error = %v", err)
	}

	if err := client.ValidateToken(context.Background()); err != nil {
		t.Fatalf("ValidateToken() error = %v", err)
	}
}

func TestUpsertCNAMECreatesRecord(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/zones/zone-1/dns_records":
			_ = json.NewEncoder(w).Encode(apiResponse[[]DNSRecord]{Success: true, Result: []DNSRecord{}})
		case r.Method == http.MethodPost && r.URL.Path == "/zones/zone-1/dns_records":
			_ = json.NewEncoder(w).Encode(apiResponse[DNSRecord]{
				Success: true,
				Result: DNSRecord{
					ID:      "rec-1",
					Type:    "CNAME",
					Name:    "app.example.com",
					Content: "origin.example.com",
					Proxied: false,
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewWithBaseURL("secret-token", server.URL, server.Client())
	if err != nil {
		t.Fatalf("NewWithBaseURL() error = %v", err)
	}

	record, changed, err := client.UpsertCNAME(context.Background(), "zone-1", "app.example.com", "origin.example.com", false)
	if err != nil {
		t.Fatalf("UpsertCNAME() error = %v", err)
	}

	if !changed {
		t.Fatal("expected record to be created")
	}

	if record.ID != "rec-1" {
		t.Fatalf("record id = %q", record.ID)
	}
}

func TestUpsertCNAMESkipsUpdateWhenUnchanged(t *testing.T) {
	t.Parallel()

	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/zones/zone-1/dns_records":
			_ = json.NewEncoder(w).Encode(apiResponse[[]DNSRecord]{
				Success: true,
				Result: []DNSRecord{{
					ID:      "rec-1",
					Type:    "CNAME",
					Name:    "app.example.com",
					Content: "origin.example.com",
					Proxied: false,
				}},
			})
		case r.Method == http.MethodPut || r.Method == http.MethodPost:
			writes.Add(1)
			http.Error(w, "should not write", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewWithBaseURL("secret-token", server.URL, server.Client())
	if err != nil {
		t.Fatalf("NewWithBaseURL() error = %v", err)
	}

	record, changed, err := client.UpsertCNAME(context.Background(), "zone-1", "app.example.com", "origin.example.com", false)
	if err != nil {
		t.Fatalf("UpsertCNAME() error = %v", err)
	}

	if changed {
		t.Fatal("expected no change")
	}

	if record.ID != "rec-1" {
		t.Fatalf("record id = %q", record.ID)
	}

	if got := writes.Load(); got != 0 {
		t.Fatalf("write count = %d", got)
	}
}
