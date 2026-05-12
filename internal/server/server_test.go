package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"caddytower/internal/auth"
	"caddytower/internal/caddyadmin"
	"caddytower/internal/config"
	"caddytower/internal/dockerx"
	"caddytower/internal/projects"
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
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, nil, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	if !strings.Contains(rec.Body.String(), "CaddyTower dashboard") {
		t.Fatalf("body missing scaffold heading: %q", rec.Body.String())
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
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, nil, nil, nil, nil)

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
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, stateStore, authService, nil, nil)

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
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, stateStore, authService, nil, nil)

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
		Name:         "Books",
		Slug:         "books",
		ImageRef:     "ghcr.io/example/books:latest",
		Subdomain:    "books",
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
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, stateStore, nil, projectService, nil)

	body := `{"ref":"refs/heads/main"}`
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/deploy/books", strings.NewReader(body))
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
		Name:         "Books",
		Slug:         "books",
		ImageRef:     "ghcr.io/example/books:latest",
		Subdomain:    "books",
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
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, stateStore, authService, projectService, nil)

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

type serverTestDocker struct{ logContent string }

func (serverTestDocker) PullImage(context.Context, string) error { return nil }
func (serverTestDocker) RecreateContainer(_ context.Context, spec dockerx.ContainerSpec) (dockerx.ContainerInspect, error) {
	return dockerx.ContainerInspect{Name: spec.Name, Running: true}, nil
}
func (serverTestDocker) InspectContainer(context.Context, string) (dockerx.ContainerInspect, error) {
	return dockerx.ContainerInspect{Running: true}, nil
}
func (serverTestDocker) ListContainersByLabel(context.Context, string, string) ([]dockerx.ContainerSummary, error) {
	return nil, nil
}
func (serverTestDocker) ListContainers(context.Context) ([]dockerx.ContainerSummary, error) {
	return nil, nil
}
func (serverTestDocker) RemoveContainer(context.Context, string) error { return nil }
func (d serverTestDocker) StreamLogs(context.Context, string, int) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(d.logContent)), nil
}
func (serverTestDocker) Exec(context.Context, string, []string, []string, io.Writer, io.Writer) error {
	return nil
}

type serverTestCaddy struct{}

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
