package dbengines

import (
	"context"
	"strings"
	"testing"
)

func TestAttachDatabaseRejectsInvalidEnvVarNameBeforeProvisioning(t *testing.T) {
	t.Parallel()

	svc := New(nil, nil, nil)
	_, err := svc.AttachDatabase(context.Background(), "project-id", "books", enginePostgres, "DATABASE-URL")
	if err == nil {
		t.Fatal("expected invalid env var error")
	}
	if !strings.Contains(err.Error(), "env var") {
		t.Fatalf("error = %v, want env var validation", err)
	}
}
