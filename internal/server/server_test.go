package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"caddytower/internal/config"
	"caddytower/internal/ui"
	"caddytower/internal/version"
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
	}, webUI, newNoopLogger(), version.Info{Version: "test"}, nil)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()

	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	if !strings.Contains(rec.Body.String(), "CaddyTower scaffold is ready") {
		t.Fatalf("body missing scaffold heading: %q", rec.Body.String())
	}
}
