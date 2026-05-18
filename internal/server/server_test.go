package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"testing"
	"time"

	"caddytower/internal/auth"
	"caddytower/internal/caddyadmin"
	"caddytower/internal/config"
	"caddytower/internal/dockerx"
	githubapp "caddytower/internal/github"
	"caddytower/internal/projects"
	"caddytower/internal/secrets"
	"caddytower/internal/store"
	"caddytower/internal/ui"
	"caddytower/internal/version"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

func TestRouterServesHome(t *testing.T) {
	t.Parallel()

	webUI, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New() error = %v", err)
	}

	srv := New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       "/tmp/caddytower",
		CaddyAdminURL: "http://shared-caddy:2019",
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, nil, nil, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	if !strings.Contains(rec.Body.String(), "CaddyTower dashboard") {
		t.Fatalf("body missing scaffold heading: %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Add project") {
		t.Fatalf("body missing add project entry point: %q", rec.Body.String())
	}
	for _, snippet := range []string{"Guided start", "Manual project", "Create manual project", "Adopt existing services", "adoption from Settings", "adopt running services"} {
		if strings.Contains(rec.Body.String(), snippet) {
			t.Fatalf("body still has removed dashboard action %q: %q", snippet, rec.Body.String())
		}
	}
	if !strings.Contains(rec.Body.String(), "Finish the first-run flow") {
		t.Fatalf("body missing onboarding-oriented copy: %q", rec.Body.String())
	}
	for _, snippet := range []string{"Resume setup guide", "Make the public admin hostname reachable", "Blocked until the final public HTTPS admin URL is live."} {
		if !strings.Contains(rec.Body.String(), snippet) {
			t.Fatalf("body missing hardened onboarding snippet %q: %q", snippet, rec.Body.String())
		}
	}
	if !strings.Contains(rec.Body.String(), "VPS status") || !strings.Contains(rec.Body.String(), "Status unavailable.") {
		t.Fatalf("body missing dashboard vps status content: %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Vital requirements") || !strings.Contains(rec.Body.String(), "Requirement checks unavailable.") {
		t.Fatalf("body missing dashboard requirement status content: %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "GitHub import works today") || strings.Contains(rec.Body.String(), "What’s coming") || strings.Contains(rec.Body.String(), "import flow next") || strings.Contains(rec.Body.String(), "planned onboarding") {
		t.Fatalf("body has stale github onboarding copy: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "transition:true") {
		t.Fatalf("body still uses CSP-conflicting htmx view transition swap: %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `id="project-delete-dialog"`) || strings.Contains(rec.Body.String(), "hx-confirm=") {
		t.Fatalf("body missing delete confirmation dialog or still uses hx-confirm: %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/assets/vendor/htmx.min.js") || strings.Contains(rec.Body.String(), "/assets/vendor/pico.classless.min.css") || strings.Contains(rec.Body.String(), "/assets/vendor/alpine.min.js") {
		t.Fatalf("body has unexpected ui assets: %q", rec.Body.String())
	}
}

func TestSecurityHeadersForPublicAdminPages(t *testing.T) {
	t.Parallel()

	webUI, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New() error = %v", err)
	}

	srv := New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "https://admin.example.com",
		DataDir:       "/tmp/caddytower",
		CaddyAdminURL: "http://shared-caddy:2019",
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, nil, nil, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("Strict-Transport-Security"); got == "" {
		t.Fatal("missing Strict-Transport-Security")
	}
	if got := rec.Header().Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'self'") {
		t.Fatalf("Content-Security-Policy = %q", got)
	}
}

func TestSetupPageIncludesQRCodeAndAuthenticatorGuidance(t *testing.T) {
	t.Parallel()

	webUI, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New() error = %v", err)
	}

	stateStore := openServerTestStore(t)
	authService := auth.New(stateStore, nil, "http://localhost:8080")
	srv := New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, stateStore, authService, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/setup?email=admin@example.com", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "data:image/png;base64,") {
		t.Fatalf("setup page missing QR code: %q", body)
	}
	for _, snippet := range []string{"data-setup-preview-form", "data-setup-email", "data-setup-secret", "data-setup-qr", "data-setup-otpauth"} {
		if !strings.Contains(body, snippet) {
			t.Fatalf("setup page missing preview hook %q: %q", snippet, body)
		}
	}
	if !strings.Contains(body, "Google Authenticator") || !strings.Contains(body, "Bitwarden") {
		t.Fatalf("setup page missing authenticator guidance: %q", body)
	}
}

func TestSetupTOTPPreviewUsesRequestedEmail(t *testing.T) {
	t.Parallel()

	webUI, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New() error = %v", err)
	}

	stateStore := openServerTestStore(t)
	authService := auth.New(stateStore, nil, "http://localhost:8080")
	srv := New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, stateStore, authService, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/setup/totp-preview?email=owner@example.com&secret=JBSWY3DPEHPK3PXP", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var payload struct {
		OTPAuthURL    string `json:"otp_auth_url"`
		QRCodeDataURL string `json:"qr_code_data_url"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatalf("decode preview response: %v", err)
	}
	if !strings.Contains(payload.OTPAuthURL, "owner@example.com") {
		t.Fatalf("preview otp auth url = %q, want requested email", payload.OTPAuthURL)
	}
	if !strings.HasPrefix(payload.QRCodeDataURL, "data:image/png;base64,") {
		t.Fatalf("preview qr code missing data url: %q", payload.QRCodeDataURL)
	}
}

func TestRootRedirectsToSetupWhenBootstrapRequired(t *testing.T) {
	t.Parallel()

	webUI, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New() error = %v", err)
	}

	stateStore := openServerTestStore(t)
	authService := auth.New(stateStore, nil, "http://localhost:8080")
	srv := New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, stateStore, authService, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if got := rec.Header().Get("Location"); got != "/setup" {
		t.Fatalf("location = %q", got)
	}
}

func TestRootRedirectsToLoginAfterBootstrap(t *testing.T) {
	t.Parallel()

	webUI, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New() error = %v", err)
	}

	stateStore := openServerTestStore(t)
	authService := auth.New(stateStore, nil, "http://localhost:8080")
	fixedNow := time.Date(2026, 5, 12, 7, 0, 0, 0, time.UTC)
	authService.SetNow(func() time.Time { return fixedNow })

	enrollment, err := authService.GenerateEnrollment("admin@example.com")
	if err != nil {
		t.Fatalf("GenerateEnrollment() error = %v", err)
	}
	code, err := totp.GenerateCodeCustom(enrollment.Secret, fixedNow, totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom() error = %v", err)
	}
	if _, _, err := authService.CreateInitialUser(context.Background(), "admin@example.com", "super-secure-password", "super-secure-password", enrollment.Secret, code, "127.0.0.1", "test-agent"); err != nil {
		t.Fatalf("CreateInitialUser() error = %v", err)
	}

	srv := New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, stateStore, authService, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusFound)
	}
	if got := rec.Header().Get("Location"); got != "/login" {
		t.Fatalf("location = %q", got)
	}
}

func TestDeployWebhookRedeploysProject(t *testing.T) {
	t.Parallel()

	webUI, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New() error = %v", err)
	}

	stateStore := openServerTestStore(t)
	projectService := projects.New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
		RootDomain:    "example.com",
	}, stateStore, nil, &serverTestDocker{}, &serverTestCaddy{}, newNoopLogger())

	project, err := projectService.CreateWebProject(context.Background(), projects.WebProjectInput{
		Name:         "Demo",
		Slug:         "demo",
		ImageRef:     "ghcr.io/example/demo:latest",
		Subdomain:    "demo",
		InternalPort: 3000,
	}, "")
	if err != nil {
		t.Fatalf("CreateWebProject() error = %v", err)
	}

	srv := New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
		RootDomain:    "example.com",
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, stateStore, nil, projectService, nil, nil)

	body := `{"ref":"refs/heads/main"}`
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/deploy/demo", strings.NewReader(body))
	req.Header.Set("X-Signature", testWebhookSignature(project.WebhookSecret, []byte(body)))
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
}

func TestProjectLogsStreamRequiresAuthAndStreams(t *testing.T) {
	t.Parallel()

	webUI, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New() error = %v", err)
	}

	stateStore := openServerTestStore(t)
	authService := auth.New(stateStore, nil, "http://localhost:8080")
	fixedNow := time.Date(2026, 5, 12, 7, 0, 0, 0, time.UTC)
	authService.SetNow(func() time.Time { return fixedNow })
	enrollment, err := authService.GenerateEnrollment("admin@example.com")
	if err != nil {
		t.Fatalf("GenerateEnrollment() error = %v", err)
	}
	code, err := totp.GenerateCodeCustom(enrollment.Secret, fixedNow, totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom() error = %v", err)
	}
	token, _, err := authService.CreateInitialUser(context.Background(), "admin@example.com", "super-secure-password", "super-secure-password", enrollment.Secret, code, "127.0.0.1", "test-agent")
	if err != nil {
		t.Fatalf("CreateInitialUser() error = %v", err)
	}

	projectService := projects.New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
		RootDomain:    "example.com",
	}, stateStore, nil, &serverTestDocker{logContent: "alpha\nbeta\n"}, &serverTestCaddy{}, newNoopLogger())

	project, err := projectService.CreateWebProject(context.Background(), projects.WebProjectInput{
		Name:         "Demo",
		Slug:         "demo",
		ImageRef:     "ghcr.io/example/demo:latest",
		Subdomain:    "demo",
		InternalPort: 3000,
	}, "")
	if err != nil {
		t.Fatalf("CreateWebProject() error = %v", err)
	}

	srv := New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
		RootDomain:    "example.com",
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, stateStore, authService, projectService, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/projects/"+project.ID+"/logs/stream", nil)
	req.AddCookie(&http.Cookie{Name: authService.SessionCookieName(), Value: token})
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "data: alpha") || !strings.Contains(rec.Body.String(), "data: beta") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestProjectPageRendersDeployHistoryAndDomains(t *testing.T) {
	t.Parallel()

	webUI, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New() error = %v", err)
	}

	stateStore := openServerTestStore(t)
	authService := auth.New(stateStore, nil, "http://localhost:8080")
	fixedNow := time.Date(2026, 5, 12, 7, 0, 0, 0, time.UTC)
	authService.SetNow(func() time.Time { return fixedNow })
	enrollment, err := authService.GenerateEnrollment("admin@example.com")
	if err != nil {
		t.Fatalf("GenerateEnrollment() error = %v", err)
	}
	code, err := totp.GenerateCodeCustom(enrollment.Secret, fixedNow, totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom() error = %v", err)
	}
	token, _, err := authService.CreateInitialUser(context.Background(), "admin@example.com", "super-secure-password", "super-secure-password", enrollment.Secret, code, "127.0.0.1", "test-agent")
	if err != nil {
		t.Fatalf("CreateInitialUser() error = %v", err)
	}

	projectService := projects.New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
		RootDomain:    "example.com",
	}, stateStore, nil, &serverTestDocker{}, &serverTestCaddy{}, newNoopLogger())
	if err := projectService.SaveSettings(context.Background(), projects.SettingsInput{
		RootDomain:     "example.com",
		OriginHostname: "origin.example.com",
	}, ""); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}
	project, err := projectService.CreateWebProject(context.Background(), projects.WebProjectInput{
		Name:         "Demo",
		Slug:         "demo",
		ImageRef:     "ghcr.io/example/demo:latest",
		Subdomain:    "demo",
		InternalPort: 3000,
		EnvText:      "API_KEY=secret",
	}, "")
	if err != nil {
		t.Fatalf("CreateWebProject() error = %v", err)
	}
	if _, err := projectService.AddProjectDomain(context.Background(), project.ID, "app.example.org", true, ""); err != nil {
		t.Fatalf("AddProjectDomain() error = %v", err)
	}

	srv := New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
		RootDomain:    "example.com",
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, stateStore, authService, projectService, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/projects/"+project.ID, nil)
	req.AddCookie(&http.Cookie{Name: authService.SessionCookieName(), Value: token})
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, fragment := range []string{"Deploy history", "Custom domain list", "Live deploy events", "Runtime snapshot", "Memory headroom", "Open app", "Delete project?", "app.example.org", "Add variable", "Paste or edit raw", "Health check path", "Health timeout"} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("project page missing %q: %q", fragment, body)
		}
	}
	if strings.Contains(body, "hx-confirm=") {
		t.Fatalf("project page still uses hx-confirm for destructive actions: %q", body)
	}
}

func TestDescribeUIErrorAddsHelpfulHints(t *testing.T) {
	t.Parallel()

	title, hints := describeUIError("health check failed after 3 attempts: request health check: connection refused")
	if title != "Deployment did not finish cleanly" {
		t.Fatalf("title = %q", title)
	}
	if len(hints) == 0 || !strings.Contains(hints[0], "live logs") {
		t.Fatalf("hints = %#v", hints)
	}

	title, hints = describeUIError("slug must match ^[a-z0-9][a-z0-9-]{1,62}$")
	if title != "Some project settings need fixing" {
		t.Fatalf("validation title = %q", title)
	}
	if len(hints) == 0 || !strings.Contains(hints[0], "project form values") {
		t.Fatalf("validation hints = %#v", hints)
	}
}

func TestSettingsPageRendersDeploymentSetup(t *testing.T) {
	t.Parallel()

	webUI, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New() error = %v", err)
	}

	stateStore := openServerTestStore(t)
	authService := auth.New(stateStore, nil, "http://localhost:8080")
	fixedNow := time.Date(2026, 5, 12, 7, 0, 0, 0, time.UTC)
	authService.SetNow(func() time.Time { return fixedNow })
	enrollment, err := authService.GenerateEnrollment("admin@example.com")
	if err != nil {
		t.Fatalf("GenerateEnrollment() error = %v", err)
	}
	code, err := totp.GenerateCodeCustom(enrollment.Secret, fixedNow, totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom() error = %v", err)
	}
	token, _, err := authService.CreateInitialUser(context.Background(), "admin@example.com", "super-secure-password", "super-secure-password", enrollment.Secret, code, "127.0.0.1", "test-agent")
	if err != nil {
		t.Fatalf("CreateInitialUser() error = %v", err)
	}

	projectService := projects.New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	}, stateStore, nil, nil, nil, newNoopLogger())

	srv := New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, stateStore, authService, projectService, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.AddCookie(&http.Cookie{Name: authService.SessionCookieName(), Value: token})
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "Deployment settings") || !strings.Contains(body, `placeholder="example.com"`) || !strings.Contains(body, `placeholder="server.example.com or 203.0.113.10"`) {
		t.Fatalf("settings page missing deployment setup content: %q", body)
	}
	if !strings.Contains(body, "Shared Caddy diagnostics") || !strings.Contains(body, "Shared Caddy diagnostics are unavailable because the admin client is not configured.") {
		t.Fatalf("settings page missing caddy diagnostics content: %q", body)
	}
	if strings.Contains(body, "Adopt existing") || strings.Contains(body, "adoption") {
		t.Fatalf("settings page should no longer include adoption ui: %q", body)
	}
	if !strings.Contains(body, "Audit log") || !strings.Contains(body, "user.bootstrap") || !strings.Contains(body, "admin@example.com") {
		t.Fatalf("settings page missing audit log content: %q", body)
	}
}

func TestSettingsPageShowsGitHubInstallationStatus(t *testing.T) {
	t.Parallel()

	webUI, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New() error = %v", err)
	}

	stateStore := openServerTestStore(t)
	authService := auth.New(stateStore, nil, "http://localhost:8080")
	fixedNow := time.Date(2026, 5, 12, 7, 0, 0, 0, time.UTC)
	authService.SetNow(func() time.Time { return fixedNow })
	enrollment, err := authService.GenerateEnrollment("admin@example.com")
	if err != nil {
		t.Fatalf("GenerateEnrollment() error = %v", err)
	}
	code, err := totp.GenerateCodeCustom(enrollment.Secret, fixedNow, totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom() error = %v", err)
	}
	token, _, err := authService.CreateInitialUser(context.Background(), "admin@example.com", "super-secure-password", "super-secure-password", enrollment.Secret, code, "127.0.0.1", "test-agent")
	if err != nil {
		t.Fatalf("CreateInitialUser() error = %v", err)
	}

	githubService := githubapp.New(githubapp.Config{
		AppID:          12345,
		AppSlug:        "caddytower",
		PrivateKeyPath: "/unused/in/test",
		WebhookSecret:  "github-secret",
		APIBaseURL:     "https://api.github.test",
		WebBaseURL:     "https://github.test",
	}, stateStore, nil)
	payload := []byte(`{"action":"created","installation":{"id":42,"account":{"login":"example-org","type":"Organization"}}}`)
	if _, err := githubService.HandleWebhook(context.Background(), "installation", testWebhookSignature("github-secret", payload), payload); err != nil {
		t.Fatalf("HandleWebhook() error = %v", err)
	}

	projectService := projects.New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	}, stateStore, nil, nil, nil, newNoopLogger())

	srv := New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, stateStore, authService, projectService, githubService, nil)

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.AddCookie(&http.Cookie{Name: authService.SessionCookieName(), Value: token})
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Connect on GitHub") || !strings.Contains(body, "Import from GitHub") || !strings.Contains(body, "ready") || !strings.Contains(body, "example-org") || !strings.Contains(body, "Manage on GitHub") {
		t.Fatalf("settings page missing github installation content: %q", body)
	}
}

func TestSettingsPageShowsGitHubSetupGuideWhenUnconfigured(t *testing.T) {
	t.Parallel()

	webUI, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New() error = %v", err)
	}

	stateStore := openServerTestStore(t)
	authService := auth.New(stateStore, nil, "http://localhost:8080")
	fixedNow := time.Date(2026, 5, 12, 7, 0, 0, 0, time.UTC)
	authService.SetNow(func() time.Time { return fixedNow })
	enrollment, err := authService.GenerateEnrollment("admin@example.com")
	if err != nil {
		t.Fatalf("GenerateEnrollment() error = %v", err)
	}
	code, err := totp.GenerateCodeCustom(enrollment.Secret, fixedNow, totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom() error = %v", err)
	}
	token, _, err := authService.CreateInitialUser(context.Background(), "admin@example.com", "super-secure-password", "super-secure-password", enrollment.Secret, code, "127.0.0.1", "test-agent")
	if err != nil {
		t.Fatalf("CreateInitialUser() error = %v", err)
	}

	projectService := projects.New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://127.0.0.1:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
		RootDomain:    "example.com",
	}, stateStore, nil, nil, nil, newNoopLogger())
	if err := projectService.SaveSettings(context.Background(), projects.SettingsInput{RootDomain: "example.com"}, ""); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}

	srv := New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://127.0.0.1:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
		RootDomain:    "example.com",
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, stateStore, authService, projectService, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.AddCookie(&http.Cookie{Name: authService.SessionCookieName(), Value: token})
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, snippet := range []string{
		"Save GitHub App settings",
		"https://caddytower.example.com",
		"/api/webhooks/github",
		"GitHub App private key PEM",
		"Paste the GitHub App details here",
		"CaddyTower still works without Cloudflare",
		"Cloudflare zone ID (optional)",
	} {
		if !strings.Contains(body, snippet) {
			t.Fatalf("settings page missing github setup guide snippet %q: %q", snippet, body)
		}
	}
	for _, snippet := range []string{
		"CADDYTOWER_GITHUB_APP_ID",
		"/run/secrets/github-app.pem",
		"/opt/caddytower/caddytower.env",
	} {
		if strings.Contains(body, snippet) {
			t.Fatalf("settings page still contains stale env-based github setup snippet %q: %q", snippet, body)
		}
	}
	if strings.Contains(body, "VPS status") {
		t.Fatalf("settings page should no longer render vps status in the settings view: %q", body)
	}
}

func TestSettingsPageShowsControllerUpdateAction(t *testing.T) {
	t.Parallel()

	webUI, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New() error = %v", err)
	}

	stateStore := openServerTestStore(t)
	authService := auth.New(stateStore, nil, "http://localhost:8080")
	fixedNow := time.Date(2026, 5, 12, 7, 0, 0, 0, time.UTC)
	authService.SetNow(func() time.Time { return fixedNow })
	enrollment, err := authService.GenerateEnrollment("admin@example.com")
	if err != nil {
		t.Fatalf("GenerateEnrollment() error = %v", err)
	}
	code, err := totp.GenerateCodeCustom(enrollment.Secret, fixedNow, totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom() error = %v", err)
	}
	token, _, err := authService.CreateInitialUser(context.Background(), "admin@example.com", "super-secure-password", "super-secure-password", enrollment.Secret, code, "127.0.0.1", "test-agent")
	if err != nil {
		t.Fatalf("CreateInitialUser() error = %v", err)
	}

	projectService := projects.New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	}, stateStore, nil, nil, nil, newNoopLogger())
	srv := New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	}, webUI, newNoopLogger(), version.Info{Version: "v1.0.0"}, stateStore, authService, projectService, nil, nil)
	srv.controllerUpdateStatusFunc = func(context.Context) ui.ControllerUpdateData {
		return ui.ControllerUpdateData{
			Checked:          true,
			CurrentVersion:   "v1.0.0",
			CurrentImage:     "ghcr.io/knopkem/caddytower:v1.0.0",
			LatestRelease:    "v1.1.0",
			LatestReleaseURL: "https://github.com/knopkem/caddytower/releases/tag/v1.1.0",
			StatusMessage:    "A newer release is available.",
			UpdateAvailable:  true,
			CanTrigger:       true,
			ButtonLabel:      "Update and restart",
			TargetImage:      "ghcr.io/knopkem/caddytower:v1.1.0",
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.AddCookie(&http.Cookie{Name: authService.SessionCookieName(), Value: token})
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, snippet := range []string{"Running version", "Latest release", "v1.1.0", "Update and restart", "A newer release is available.", "ghcr.io/knopkem/caddytower:v1.0.0"} {
		if !strings.Contains(body, snippet) {
			t.Fatalf("settings page missing update snippet %q: %q", snippet, body)
		}
	}
}

func TestSettingsPageShowsRestartPromptWhenRequested(t *testing.T) {
	t.Parallel()

	webUI, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New() error = %v", err)
	}

	stateStore := openServerTestStore(t)
	authService := auth.New(stateStore, nil, "http://localhost:8080")
	fixedNow := time.Date(2026, 5, 12, 7, 0, 0, 0, time.UTC)
	authService.SetNow(func() time.Time { return fixedNow })
	enrollment, err := authService.GenerateEnrollment("admin@example.com")
	if err != nil {
		t.Fatalf("GenerateEnrollment() error = %v", err)
	}
	code, err := totp.GenerateCodeCustom(enrollment.Secret, fixedNow, totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom() error = %v", err)
	}
	token, _, err := authService.CreateInitialUser(context.Background(), "admin@example.com", "super-secure-password", "super-secure-password", enrollment.Secret, code, "127.0.0.1", "test-agent")
	if err != nil {
		t.Fatalf("CreateInitialUser() error = %v", err)
	}

	projectService := projects.New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	}, stateStore, nil, nil, nil, newNoopLogger())
	srv := New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	}, webUI, newNoopLogger(), version.Info{Version: "v1.0.0"}, stateStore, authService, projectService, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/settings?restart_required=true&restart_message=Restart+to+apply+these+changes.", nil)
	req.AddCookie(&http.Cookie{Name: authService.SessionCookieName(), Value: token})
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, snippet := range []string{"Restart required", "Restart to apply these changes.", "Restart now"} {
		if !strings.Contains(body, snippet) {
			t.Fatalf("settings page missing restart prompt snippet %q: %q", snippet, body)
		}
	}
	if strings.Contains(body, "Use this card to check the current release") {
		t.Fatalf("settings page still contains stale static restart guidance: %q", body)
	}
}

func TestSettingsUpdateSchedulesControllerUpdate(t *testing.T) {
	t.Parallel()

	webUI, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New() error = %v", err)
	}

	stateStore := openServerTestStore(t)
	authService := auth.New(stateStore, nil, "http://localhost:8080")
	fixedNow := time.Date(2026, 5, 12, 7, 0, 0, 0, time.UTC)
	authService.SetNow(func() time.Time { return fixedNow })
	enrollment, err := authService.GenerateEnrollment("admin@example.com")
	if err != nil {
		t.Fatalf("GenerateEnrollment() error = %v", err)
	}
	code, err := totp.GenerateCodeCustom(enrollment.Secret, fixedNow, totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom() error = %v", err)
	}
	token, user, err := authService.CreateInitialUser(context.Background(), "admin@example.com", "super-secure-password", "super-secure-password", enrollment.Secret, code, "127.0.0.1", "test-agent")
	if err != nil {
		t.Fatalf("CreateInitialUser() error = %v", err)
	}

	projectService := projects.New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	}, stateStore, nil, nil, nil, newNoopLogger())
	srv := New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	}, webUI, newNoopLogger(), version.Info{Version: "v1.0.0"}, stateStore, authService, projectService, nil, nil)
	srv.controllerUpdateStatusFunc = func(context.Context) ui.ControllerUpdateData {
		return ui.ControllerUpdateData{
			Checked:         true,
			UpdateAvailable: true,
			CanTrigger:      true,
			TargetImage:     "ghcr.io/knopkem/caddytower:v1.1.0",
			LatestRelease:   "v1.1.0",
		}
	}

	var scheduledImage string
	var scheduledUser string
	srv.scheduleControllerUpdateFunc = func(image, userID string) {
		scheduledImage = image
		scheduledUser = userID
	}

	getReq := httptest.NewRequest(http.MethodGet, "/settings", nil)
	getReq.AddCookie(&http.Cookie{Name: authService.SessionCookieName(), Value: token})
	getRec := httptest.NewRecorder()
	srv.Router().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("settings page status = %d, want %d", getRec.Code, http.StatusOK)
	}

	var csrfCookie *http.Cookie
	for _, cookie := range getRec.Result().Cookies() {
		if cookie.Name != authService.SessionCookieName() {
			csrfCookie = cookie
			break
		}
	}
	if csrfCookie == nil {
		t.Fatal("missing csrf cookie")
	}

	postReq := httptest.NewRequest(http.MethodPost, "/settings/update", strings.NewReader("csrf_token="+csrfCookie.Value))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.AddCookie(&http.Cookie{Name: authService.SessionCookieName(), Value: token})
	postReq.AddCookie(csrfCookie)
	postRec := httptest.NewRecorder()

	srv.Router().ServeHTTP(postRec, postReq)

	if postRec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", postRec.Code, http.StatusFound)
	}
	if scheduledImage != "ghcr.io/knopkem/caddytower:v1.1.0" || scheduledUser != user.ID {
		t.Fatalf("scheduled update = %q %q", scheduledImage, scheduledUser)
	}
}

func TestSettingsGitHubSubmitEnablesRuntimeInstallFlow(t *testing.T) {
	t.Parallel()

	webUI, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New() error = %v", err)
	}

	stateStore := openServerTestStore(t)
	secretSvc, err := secrets.NewOptionalFromBase64("AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
	if err != nil {
		t.Fatalf("NewOptionalFromBase64() error = %v", err)
	}
	authService := auth.New(stateStore, nil, "https://caddytower.example.com")
	fixedNow := time.Date(2026, 5, 12, 7, 0, 0, 0, time.UTC)
	authService.SetNow(func() time.Time { return fixedNow })
	enrollment, err := authService.GenerateEnrollment("admin@example.com")
	if err != nil {
		t.Fatalf("GenerateEnrollment() error = %v", err)
	}
	code, err := totp.GenerateCodeCustom(enrollment.Secret, fixedNow, totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom() error = %v", err)
	}
	token, _, err := authService.CreateInitialUser(context.Background(), "admin@example.com", "super-secure-password", "super-secure-password", enrollment.Secret, code, "127.0.0.1", "test-agent")
	if err != nil {
		t.Fatalf("CreateInitialUser() error = %v", err)
	}

	cfg := config.Config{
		HTTPAddr:         ":8080",
		PublicBaseURL:    "https://caddytower.example.com",
		DataDir:          t.TempDir(),
		CaddyAdminURL:    "http://shared-caddy:2019",
		RootDomain:       "example.com",
		GitHubAPIBaseURL: "https://api.github.test",
		GitHubWebBaseURL: "https://github.test",
	}
	projectService := projects.New(cfg, stateStore, secretSvc, nil, nil, newNoopLogger())
	srv := New(cfg, webUI, newNoopLogger(), version.Info{Version: "test"}, stateStore, authService, projectService, nil, nil)

	getReq := httptest.NewRequest(http.MethodGet, "/settings", nil)
	getReq.AddCookie(&http.Cookie{Name: authService.SessionCookieName(), Value: token})
	getRec := httptest.NewRecorder()
	srv.Router().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("settings page status = %d, want %d", getRec.Code, http.StatusOK)
	}

	var csrfCookie *http.Cookie
	for _, cookie := range getRec.Result().Cookies() {
		if cookie.Name != authService.SessionCookieName() {
			csrfCookie = cookie
			break
		}
	}
	if csrfCookie == nil {
		t.Fatal("missing csrf cookie")
	}

	privateKeyPEM := generatedTestPrivateKeyPEM(t)
	formValues := neturl.Values{
		"csrf_token":             {csrfCookie.Value},
		"github_app_id":          {"12345"},
		"github_app_slug":        {"caddytower"},
		"github_webhook_secret":  {"github-secret"},
		"github_private_key_pem": {privateKeyPEM},
	}
	form := strings.NewReader(formValues.Encode())
	postReq := httptest.NewRequest(http.MethodPost, "/settings/github", form)
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.AddCookie(&http.Cookie{Name: authService.SessionCookieName(), Value: token})
	postReq.AddCookie(csrfCookie)
	postRec := httptest.NewRecorder()

	srv.Router().ServeHTTP(postRec, postReq)

	if postRec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", postRec.Code, http.StatusFound)
	}
	settings, err := projectService.GitHubSettings(context.Background())
	if err != nil {
		t.Fatalf("GitHubSettings() error = %v", err)
	}
	if !settings.StoredInApp || !settings.Configured || settings.AppSlug != "caddytower" {
		t.Fatalf("unexpected runtime github settings %#v", settings)
	}

	installReq := httptest.NewRequest(http.MethodGet, "/github/install", nil)
	installReq.AddCookie(&http.Cookie{Name: authService.SessionCookieName(), Value: token})
	installRec := httptest.NewRecorder()
	srv.Router().ServeHTTP(installRec, installReq)

	if installRec.Code != http.StatusFound {
		t.Fatalf("install status = %d, want %d", installRec.Code, http.StatusFound)
	}
	if location := installRec.Header().Get("Location"); location != "https://github.test/apps/caddytower/installations/new" {
		t.Fatalf("install redirect = %q", location)
	}
}

func TestSettingsPageFiltersAuditLog(t *testing.T) {
	t.Parallel()

	webUI, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New() error = %v", err)
	}

	stateStore := openServerTestStore(t)
	authService := auth.New(stateStore, nil, "http://localhost:8080")
	fixedNow := time.Date(2026, 5, 12, 7, 0, 0, 0, time.UTC)
	authService.SetNow(func() time.Time { return fixedNow })
	enrollment, err := authService.GenerateEnrollment("admin@example.com")
	if err != nil {
		t.Fatalf("GenerateEnrollment() error = %v", err)
	}
	code, err := totp.GenerateCodeCustom(enrollment.Secret, fixedNow, totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom() error = %v", err)
	}
	token, user, err := authService.CreateInitialUser(context.Background(), "admin@example.com", "super-secure-password", "super-secure-password", enrollment.Secret, code, "127.0.0.1", "test-agent")
	if err != nil {
		t.Fatalf("CreateInitialUser() error = %v", err)
	}
	if err := stateStore.InsertAuditLog(context.Background(), "audit-extra", user.ID, "project.redeploy", "project:test", map[string]any{"slug": "test"}); err != nil {
		t.Fatalf("InsertAuditLog() error = %v", err)
	}

	projectService := projects.New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	}, stateStore, nil, nil, nil, newNoopLogger())

	srv := New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, stateStore, authService, projectService, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/settings?audit=project.redeploy", nil)
	req.AddCookie(&http.Cookie{Name: authService.SessionCookieName(), Value: token})
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "project.redeploy") || !strings.Contains(body, "project:test") {
		t.Fatalf("filtered audit entry missing: %q", body)
	}
	if strings.Contains(body, "user.bootstrap") {
		t.Fatalf("unexpected bootstrap audit entry in filtered results: %q", body)
	}
}

func TestSettingsPageUsesInstallerRootDomainForGuidance(t *testing.T) {
	t.Parallel()

	webUI, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New() error = %v", err)
	}

	stateStore := openServerTestStore(t)
	authService := auth.New(stateStore, nil, "http://localhost:8080")
	fixedNow := time.Date(2026, 5, 12, 7, 0, 0, 0, time.UTC)
	authService.SetNow(func() time.Time { return fixedNow })
	enrollment, err := authService.GenerateEnrollment("admin@example.com")
	if err != nil {
		t.Fatalf("GenerateEnrollment() error = %v", err)
	}
	code, err := totp.GenerateCodeCustom(enrollment.Secret, fixedNow, totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom() error = %v", err)
	}
	token, _, err := authService.CreateInitialUser(context.Background(), "admin@example.com", "super-secure-password", "super-secure-password", enrollment.Secret, code, "127.0.0.1", "test-agent")
	if err != nil {
		t.Fatalf("CreateInitialUser() error = %v", err)
	}

	projectService := projects.New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
		RootDomain:    "example.com",
	}, stateStore, nil, serverTestDocker{}, nil, newNoopLogger())

	srv := New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
		RootDomain:    "example.com",
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, stateStore, authService, projectService, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.AddCookie(&http.Cookie{Name: authService.SessionCookieName(), Value: token})
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "https://caddytower.example.com") {
		t.Fatalf("settings page missing suggested installer-based URL: %q", body)
	}
	if !strings.Contains(body, `name="root_domain" value="example.com"`) {
		t.Fatalf("settings page missing installer root domain fallback: %q", body)
	}
}

func TestSettingsRestartSchedulesControllerRestart(t *testing.T) {
	t.Parallel()

	webUI, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New() error = %v", err)
	}

	stateStore := openServerTestStore(t)
	authService := auth.New(stateStore, nil, "http://localhost:8080")
	fixedNow := time.Date(2026, 5, 12, 7, 0, 0, 0, time.UTC)
	authService.SetNow(func() time.Time { return fixedNow })
	enrollment, err := authService.GenerateEnrollment("admin@example.com")
	if err != nil {
		t.Fatalf("GenerateEnrollment() error = %v", err)
	}
	code, err := totp.GenerateCodeCustom(enrollment.Secret, fixedNow, totp.ValidateOpts{
		Period:    30,
		Skew:      1,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom() error = %v", err)
	}
	token, _, err := authService.CreateInitialUser(context.Background(), "admin@example.com", "super-secure-password", "super-secure-password", enrollment.Secret, code, "127.0.0.1", "test-agent")
	if err != nil {
		t.Fatalf("CreateInitialUser() error = %v", err)
	}

	restartCount := 0
	projectService := projects.New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	}, stateStore, nil, serverTestDocker{restartCount: &restartCount}, nil, newNoopLogger())

	srv := New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, stateStore, authService, projectService, nil, nil)

	getReq := httptest.NewRequest(http.MethodGet, "/settings", nil)
	getReq.AddCookie(&http.Cookie{Name: authService.SessionCookieName(), Value: token})
	getRec := httptest.NewRecorder()
	srv.Router().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("settings page status = %d, want %d", getRec.Code, http.StatusOK)
	}

	var csrfCookie *http.Cookie
	for _, cookie := range getRec.Result().Cookies() {
		if cookie.Name != authService.SessionCookieName() {
			csrfCookie = cookie
			break
		}
	}
	if csrfCookie == nil {
		t.Fatal("missing csrf cookie")
	}

	postReq := httptest.NewRequest(http.MethodPost, "/settings/restart", strings.NewReader("csrf_token="+csrfCookie.Value))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.AddCookie(&http.Cookie{Name: authService.SessionCookieName(), Value: token})
	postReq.AddCookie(csrfCookie)
	postRec := httptest.NewRecorder()

	srv.Router().ServeHTTP(postRec, postReq)

	if postRec.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", postRec.Code, http.StatusFound)
	}
	if restartCount != 0 {
		t.Fatalf("restart count changed too early: %d", restartCount)
	}

	time.Sleep(1500 * time.Millisecond)

	if restartCount != 1 {
		t.Fatalf("restart count = %d, want 1", restartCount)
	}
}

func TestGitHubWebhookPersistsInstallation(t *testing.T) {
	t.Parallel()

	webUI, err := ui.New()
	if err != nil {
		t.Fatalf("ui.New() error = %v", err)
	}

	stateStore := openServerTestStore(t)
	githubService := githubapp.New(githubapp.Config{
		AppID:          12345,
		AppSlug:        "caddytower",
		PrivateKeyPath: "/unused/in/test",
		WebhookSecret:  "github-secret",
		APIBaseURL:     "https://api.github.test",
		WebBaseURL:     "https://github.test",
	}, stateStore, nil)
	srv := New(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, stateStore, nil, nil, githubService, nil)

	payload := []byte(`{"action":"created","installation":{"id":42,"account":{"login":"example-org","type":"Organization"}}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/github", strings.NewReader(string(payload)))
	req.Header.Set("X-GitHub-Event", "installation")
	req.Header.Set("X-Hub-Signature-256", testWebhookSignature("github-secret", payload))
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	status, err := githubService.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if len(status.Installations) != 1 || status.Installations[0].AccountLogin != "example-org" {
		t.Fatalf("unexpected installations %#v", status.Installations)
	}
}

func openServerTestStore(t *testing.T) *store.Store {
	t.Helper()

	stateStore, err := store.Open(config.Config{
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		DataDir:       t.TempDir(),
		CaddyAdminURL: "http://shared-caddy:2019",
	})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}

	t.Cleanup(func() {
		_ = stateStore.Close()
	})

	return stateStore
}

func generatedTestPrivateKeyPEM(t *testing.T) string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

type serverTestDocker struct {
	logContent   string
	restartCount *int
}

func (serverTestDocker) Ping(context.Context) error              { return nil }
func (serverTestDocker) PullImage(context.Context, string) error { return nil }
func (serverTestDocker) RecreateContainer(_ context.Context, spec dockerx.ContainerSpec) (dockerx.ContainerInspect, error) {
	return dockerx.ContainerInspect{Name: spec.Name, Running: true, ImageID: "sha256:test-image"}, nil
}
func (serverTestDocker) InspectContainer(context.Context, string) (dockerx.ContainerInspect, error) {
	return dockerx.ContainerInspect{Running: true}, nil
}
func (serverTestDocker) ContainerStats(context.Context, string) (dockerx.ContainerStatsSnapshot, error) {
	return dockerx.ContainerStatsSnapshot{
		ReadAt:           time.Date(2026, 5, 12, 11, 0, 0, 0, time.UTC),
		CPUPercent:       17.5,
		MemoryUsageBytes: 128 * 1024 * 1024,
		MemoryLimitBytes: 512 * 1024 * 1024,
		MemoryPercent:    25,
		NetworkRxBytes:   2 * 1024 * 1024,
		NetworkTxBytes:   4 * 1024 * 1024,
		BlockReadBytes:   1024,
		BlockWriteBytes:  2048,
		PIDs:             9,
	}, nil
}
func (serverTestDocker) ListContainersByLabel(context.Context, string, string) ([]dockerx.ContainerSummary, error) {
	return nil, nil
}
func (serverTestDocker) ListContainers(context.Context) ([]dockerx.ContainerSummary, error) {
	return nil, nil
}
func (d serverTestDocker) RestartContainer(context.Context, string) error {
	if d.restartCount != nil {
		*d.restartCount = *d.restartCount + 1
	}
	return nil
}
func (serverTestDocker) RemoveContainer(context.Context, string) error { return nil }
func (d serverTestDocker) StreamLogs(context.Context, string, int) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(d.logContent)), nil
}
func (serverTestDocker) Exec(context.Context, string, []string, []string, io.Writer, io.Writer) error {
	return nil
}

type serverTestCaddy struct{}

func (serverTestCaddy) Ping(context.Context) error { return nil }
func (serverTestCaddy) GetConfig(context.Context) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
func (serverTestCaddy) ReconcileManagedRoutes(context.Context, []caddyadmin.HTTPRoute, []string) (bool, error) {
	return true, nil
}

func testWebhookSignature(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return fmt.Sprintf("sha256=%x", mac.Sum(nil))
}
