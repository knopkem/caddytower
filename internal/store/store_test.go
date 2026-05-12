package store

import (
	"context"
	"testing"

	"caddytower/internal/config"
)

func TestOpenAppliesMigrations(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		DataDir:       t.TempDir(),
		HTTPAddr:      ":8080",
		PublicBaseURL: "http://localhost:8080",
		CaddyAdminURL: "http://shared-caddy:2019",
	}

	stateStore, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() {
		_ = stateStore.Close()
	}()

	ctx := context.Background()
	for _, table := range []string{
		"schema_migrations",
		"users",
		"sessions",
		"settings",
		"projects",
		"project_env",
		"project_volumes",
		"project_ports",
		"project_db_attachments",
		"project_domains",
		"project_deploys",
		"github_installations",
		"audit_log",
	} {
		exists, err := TableExists(ctx, stateStore.DB(), table)
		if err != nil {
			t.Fatalf("TableExists(%q) error = %v", table, err)
		}
		if !exists {
			t.Fatalf("expected table %q to exist", table)
		}
	}
}
