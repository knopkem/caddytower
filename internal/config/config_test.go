package config

import "testing"

func TestLoadFromLookupDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := LoadFromLookup(func(string) (string, bool) {
		return "", false
	})
	if err != nil {
		t.Fatalf("LoadFromLookup() error = %v", err)
	}

	if cfg.HTTPAddr != ":8080" {
		t.Fatalf("HTTPAddr = %q, want %q", cfg.HTTPAddr, ":8080")
	}

	if cfg.PublicBaseURL != "http://localhost:8080" {
		t.Fatalf("PublicBaseURL = %q", cfg.PublicBaseURL)
	}

	if cfg.DataDir != "./var" {
		t.Fatalf("DataDir = %q", cfg.DataDir)
	}

	if cfg.CaddyAdminURL != "http://shared-caddy:2019" {
		t.Fatalf("CaddyAdminURL = %q", cfg.CaddyAdminURL)
	}
}

func TestLoadFromLookupRejectsInvalidURL(t *testing.T) {
	t.Parallel()

	_, err := LoadFromLookup(func(key string) (string, bool) {
		if key == "CADDYTOWER_PUBLIC_BASE_URL" {
			return "not-a-url", true
		}
		return "", false
	})
	if err == nil {
		t.Fatal("expected error for invalid public base url")
	}
}
