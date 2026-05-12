package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"caddytower/internal/version"
)

func TestRenderHome(t *testing.T) {
	t.Parallel()

	webUI, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	var buf bytes.Buffer
	err = webUI.Render(&buf, "home.gohtml", HomePageData{
		GeneratedAt: time.Unix(0, 0).UTC(),
		PageTitle:   "CaddyTower | Scaffold ready",
		Headline:    "CaddyTower scaffold is ready",
		Version: version.Info{
			Version: "test",
			Commit:  "abc123",
			Date:    "2026-05-12T00:00:00Z",
		},
		Config: ConfigSummary{
			HTTPAddr:      ":8080",
			PublicBaseURL: "http://localhost:8080",
			DataDir:       "/var/lib/caddytower",
			StateDBPath:   "/var/lib/caddytower/state.db",
			CaddyAdminURL: "http://shared-caddy:2019",
			MasterKeySet:  true,
		},
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	body := buf.String()
	if !strings.Contains(body, "Scaffold ready") {
		t.Fatalf("rendered body missing heading: %q", body)
	}
	if !strings.Contains(body, "http://shared-caddy:2019") {
		t.Fatalf("rendered body missing caddy admin url: %q", body)
	}
}
