package ui

import (
	"bytes"
	"html/template"
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
		Requirements: RequirementsStatusData{
			Available:    true,
			HealthyCount: 2,
			WarningCount: 1,
			FailureCount: 0,
			Checks: []RequirementCheckData{
				{Name: "Docker daemon", Status: "ok", Summary: "Docker is reachable for deploys, logs, and container control."},
				{Name: "Shared Caddy admin API", Status: "ok", Summary: "Shared Caddy routing is reachable for route reconciliation."},
				{Name: "Watchtower auto-updater", Status: "warning", Summary: "Watchtower is not running."},
			},
		},
		EffectivePublicBaseURL: "https://caddytower.example.com",
		PublicURLReady:         true,
		PublicAdminHost:        "caddytower.example.com",
		SuggestedPublicBaseURL: "https://caddytower.example.com",
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	body := buf.String()
	if !strings.Contains(body, "Scaffold ready") {
		t.Fatalf("rendered body missing heading: %q", body)
	}
	if !strings.Contains(body, "Ship Docker projects behind shared Caddy") {
		t.Fatalf("rendered body missing project-first lede: %q", body)
	}
	if !strings.Contains(body, "Add project") {
		t.Fatalf("rendered home missing add project entry point: %q", body)
	}
	if !strings.Contains(body, "VPS status") {
		t.Fatalf("rendered home missing vps status card: %q", body)
	}
	if !strings.Contains(body, "Vital requirements") || !strings.Contains(body, "Docker daemon") || !strings.Contains(body, "Watchtower auto-updater") {
		t.Fatalf("rendered home missing requirement checks: %q", body)
	}
	for _, snippet := range []string{"Guided start", "Manual project", "Create manual project", "Adopt existing services", "adoption from Settings", "adopt running services"} {
		if strings.Contains(body, snippet) {
			t.Fatalf("rendered home still has removed dashboard action %q: %q", snippet, body)
		}
	}
	if !strings.Contains(body, "/assets/vendor/htmx.min.js") || strings.Contains(body, "/assets/vendor/pico.classless.min.css") {
		t.Fatalf("rendered body has unexpected ui assets: %q", body)
	}
}

func TestRenderSettings(t *testing.T) {
	t.Parallel()

	webUI, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	var buf bytes.Buffer
	err = webUI.Render(&buf, "settings.gohtml", SettingsPageData{
		PageTitle: "CaddyTower | Settings",
		Headline:  "Settings and operations",
		Config: ConfigSummary{
			HTTPAddr:      ":8080",
			PublicBaseURL: "http://localhost:8080",
			DataDir:       "/var/lib/caddytower",
			StateDBPath:   "/var/lib/caddytower/state.db",
			CaddyAdminURL: "http://shared-caddy:2019",
			MasterKeySet:  true,
		},
		EffectiveRootDomain:    "pacsnode.com",
		EffectivePublicBaseURL: "https://caddytower.pacsnode.com",
		PublicAdminHost:        "caddytower.pacsnode.com",
		SuggestedPublicBaseURL: "https://caddytower.pacsnode.com",
		ControllerUpdate: ControllerUpdateData{
			Checked:          true,
			CurrentVersion:   "v1.0.0",
			CurrentImage:     "ghcr.io/knopkem/caddytower:v1.0.0",
			LatestRelease:    "v1.1.0",
			StatusMessage:    "A newer release is available.",
			UpdateAvailable:  true,
			CanTrigger:       true,
			ButtonLabel:      "Update and restart",
			LatestReleaseURL: "https://github.com/knopkem/caddytower/releases/tag/v1.1.0",
		},
		RestartPrompt: RestartPromptData{
			Visible:     true,
			Title:       "Restart required",
			Message:     "These changes will take effect after CaddyTower restarts.",
			ActionLabel: "Restart now",
		},
		CaddyDiagnostics: CaddyDiagnosticsData{
			Available:         true,
			Status:            "warning",
			Summary:           "1 of 2 managed routes need attention.",
			Detail:            "Expected routes are listed below so you can compare them with the live shared Caddy config.",
			ManagedRouteCount: 2,
			HealthyRouteCount: 1,
			DriftCount:        1,
			LiveRouteCount:    3,
			Routes: []CaddyRouteDiagnosticData{
				{
					Host:                     "caddytower.pacsnode.com",
					Status:                   "ok",
					ExpectedUpstreamsSummary: "caddytower:8080",
					LiveUpstreamsSummary:     "caddytower:8080",
					Detail:                   "Live route matches the expected upstream.",
				},
				{
					Host:                     "demo.pacsnode.com",
					Status:                   "warning",
					ExpectedUpstreamsSummary: "demo:3000",
					LiveUpstreamsSummary:     "old-demo:3000",
					Detail:                   "The live upstream target differs from what CaddyTower expects.",
				},
			},
			RawConfigAvailable: true,
			RawConfig: `{
  "apps": {
    "http": {
      "servers": {
        "srv0": {
          "routes": []
        }
      }
    }
  }
}`,
		},
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	body := buf.String()
	if !strings.Contains(body, "Deployment settings") {
		t.Fatalf("rendered settings missing deployment heading: %q", body)
	}
	if !strings.Contains(body, "Controller runtime") || !strings.Contains(body, "/var/lib/caddytower/state.db") {
		t.Fatalf("rendered settings missing runtime summary content: %q", body)
	}
	if !strings.Contains(body, "Restart CaddyTower") || !strings.Contains(body, "https://caddytower.pacsnode.com") || !strings.Contains(body, "caddytower.pacsnode.com") {
		t.Fatalf("rendered settings missing restart or composed-url guidance: %q", body)
	}
	if !strings.Contains(body, "Optional: automatic DNS updates with Cloudflare") || !strings.Contains(body, "CaddyTower still works without Cloudflare") {
		t.Fatalf("rendered settings missing optional cloudflare guidance: %q", body)
	}
	if !strings.Contains(body, "Save GitHub App settings") || !strings.Contains(body, "GitHub App private key PEM") {
		t.Fatalf("rendered settings missing app-managed github form: %q", body)
	}
	if !strings.Contains(body, "Latest release") || !strings.Contains(body, "Update and restart") || !strings.Contains(body, "A newer release is available.") {
		t.Fatalf("rendered settings missing controller update UI: %q", body)
	}
	if !strings.Contains(body, "Shared Caddy diagnostics") || !strings.Contains(body, "demo.pacsnode.com") || !strings.Contains(body, "Raw live Caddy config") {
		t.Fatalf("rendered settings missing shared caddy diagnostics UI: %q", body)
	}
	if !strings.Contains(body, "Restart required") || !strings.Contains(body, "Restart now") {
		t.Fatalf("rendered settings missing restart prompt UI: %q", body)
	}
	if strings.Contains(body, "Use this card to check the current release") {
		t.Fatalf("rendered settings should not include static restart guidance anymore: %q", body)
	}
	if strings.Contains(body, "CADDYTOWER_GITHUB_APP_ID") || strings.Contains(body, "/run/secrets/github-app.pem") {
		t.Fatalf("rendered settings should not include legacy env-based github guidance: %q", body)
	}
	if strings.Contains(body, "VPS status") {
		t.Fatalf("rendered settings should not include vps status card anymore: %q", body)
	}
}

func TestRenderSetup(t *testing.T) {
	t.Parallel()

	webUI, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	var buf bytes.Buffer
	err = webUI.Render(&buf, "setup.gohtml", SetupPageData{
		PageTitle:     "CaddyTower | Setup",
		Headline:      "Create the first admin user",
		Email:         "owner@example.com",
		ManualKey:     "JBSWY3DPEHPK3PXP",
		OTPAuthURL:    "otpauth://totp/CaddyTower:owner%40example.com?secret=JBSWY3DPEHPK3PXP&issuer=CaddyTower&period=30",
		QRCodeDataURL: template.URL("data:image/png;base64,abc"),
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	body := buf.String()
	for _, snippet := range []string{"data-setup-preview-form", "data-setup-email", "data-setup-secret", "data-setup-qr", "data-setup-otpauth"} {
		if !strings.Contains(body, snippet) {
			t.Fatalf("rendered setup missing preview hook %q: %q", snippet, body)
		}
	}
}

func TestRenderProjectPageIncludesMountsAndRoutes(t *testing.T) {
	t.Parallel()

	webUI, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	var buf bytes.Buffer
	err = webUI.Render(&buf, "project.gohtml", ProjectPageData{
		PageTitle: "CaddyTower | Demo",
		Headline:  "Edit Demo",
		Project: ProjectFormData{
			ID:             "project-1",
			Action:         "/projects/project-1",
			SubmitLabel:    "Save and deploy",
			Type:           "web",
			Name:           "Demo",
			Slug:           "demo",
			ImageRef:       "ghcr.io/example/demo:latest",
			Subdomain:      "demo",
			InternalPort:   3000,
			MountsText:     "/srv/demo/data | /app/data | rw",
			HTTPRoutesText: "@domains | path_prefix | /api | strip",
		},
		ProjectMeta: ProjectListItem{
			ID:            "project-1",
			Name:          "Demo",
			Type:          "web",
			ContainerName: "caddytower-demo",
			ImageRef:      "ghcr.io/example/demo:latest",
			Status:        "running",
		},
		Mounts: []ProjectMountItem{{
			Source: "/srv/demo/data",
			Target: "/app/data",
		}},
		HTTPRoutes: []ProjectHTTPRouteItem{{
			HostScope:        "Generated + custom domains",
			MatcherSummary:   "Path prefix /api",
			TransformSummary: "Strip matched prefix before proxying",
		}},
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}

	body := buf.String()
	for _, snippet := range []string{"Bind mounts", "HTTP route rules", "Mount summary", "HTTP route summary", "Path prefix /api"} {
		if !strings.Contains(body, snippet) {
			t.Fatalf("rendered project page missing %q: %q", snippet, body)
		}
	}
}
