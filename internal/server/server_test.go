package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"caddytower/internal/auth"
	"caddytower/internal/config"
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
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, nil, nil, nil)

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
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, stateStore, authService, nil)

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
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, stateStore, authService, nil)

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
