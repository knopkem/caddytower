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
