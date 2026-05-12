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
			records := []DNSRecord{}
			if r.URL.Query().Get("type") == "CNAME" {
				records = []DNSRecord{{
					ID:      "rec-1",
					Type:    "CNAME",
					Name:    "app.example.com",
					Content: "origin.example.com",
					Proxied: false,
				}}
			}
			_ = json.NewEncoder(w).Encode(apiResponse[[]DNSRecord]{Success: true, Result: records})
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

func TestDeleteCNAMERemovesMatchingRecords(t *testing.T) {
	t.Parallel()

	var deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/zones/zone-1/dns_records":
			records := []DNSRecord{}
			if r.URL.Query().Get("type") == "CNAME" {
				records = []DNSRecord{{
					ID:   "rec-1",
					Type: "CNAME",
					Name: "app.example.com",
				}}
			}
			_ = json.NewEncoder(w).Encode(apiResponse[[]DNSRecord]{Success: true, Result: records})
		case r.Method == http.MethodDelete && r.URL.Path == "/zones/zone-1/dns_records/rec-1":
			deletes.Add(1)
			_ = json.NewEncoder(w).Encode(apiResponse[DNSRecord]{Success: true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewWithBaseURL("secret-token", server.URL, server.Client())
	if err != nil {
		t.Fatalf("NewWithBaseURL() error = %v", err)
	}

	if err := client.DeleteCNAME(context.Background(), "zone-1", "app.example.com"); err != nil {
		t.Fatalf("DeleteCNAME() error = %v", err)
	}

	if got := deletes.Load(); got != 1 {
		t.Fatalf("delete count = %d", got)
	}
}

func TestUpsertRecordCreatesARecordForIPAddress(t *testing.T) {
	t.Parallel()

	var createdType atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/zones/zone-1/dns_records":
			_ = json.NewEncoder(w).Encode(apiResponse[[]DNSRecord]{Success: true, Result: []DNSRecord{}})
		case r.Method == http.MethodPost && r.URL.Path == "/zones/zone-1/dns_records":
			var payload recordRequest
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			createdType.Store(payload.Type)
			_ = json.NewEncoder(w).Encode(apiResponse[DNSRecord]{
				Success: true,
				Result: DNSRecord{
					ID:      "rec-1",
					Type:    payload.Type,
					Name:    payload.Name,
					Content: payload.Content,
					Proxied: payload.Proxied,
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

	record, changed, err := client.UpsertRecord(context.Background(), "zone-1", "app.example.com", "203.0.113.10", false)
	if err != nil {
		t.Fatalf("UpsertRecord() error = %v", err)
	}
	if !changed {
		t.Fatal("expected record to be created")
	}
	if got, _ := createdType.Load().(string); got != "A" {
		t.Fatalf("record type = %q, want A", got)
	}
	if record.Type != "A" {
		t.Fatalf("created record type = %q", record.Type)
	}
}
