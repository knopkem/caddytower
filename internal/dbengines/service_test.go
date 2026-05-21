package dbengines

import (
	"context"
	"regexp"
	"strings"
	"testing"

	mysql "github.com/go-sql-driver/mysql"
)

func TestAttachDatabaseRejectsInvalidEnvVarNameBeforeProvisioning(t *testing.T) {
	t.Parallel()

	svc := New(nil, nil, nil)
	_, err := svc.AttachDatabase(context.Background(), "project-id", "demo", enginePostgres, "DATABASE-URL")
	if err == nil {
		t.Fatal("expected invalid env var error")
	}
	if !strings.Contains(err.Error(), "env var") {
		t.Fatalf("error = %v, want env var validation", err)
	}
}

func TestMariaDBDSNPreservesRawPassword(t *testing.T) {
	t.Parallel()

	password := "$*NR4o2aaezd_nPQadMp9#&##3wump"
	cfg, err := mysql.ParseDSN(mariaDBDSN("root", password, "caddytower-mariadb", 3306, "mysql"))
	if err != nil {
		t.Fatalf("ParseDSN() error = %v", err)
	}
	if cfg.Passwd != password {
		t.Fatalf("cfg.Passwd = %q, want %q", cfg.Passwd, password)
	}
}

func TestRandomPasswordUsesDSNSafeCharacters(t *testing.T) {
	t.Parallel()

	allowed := regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	for range 100 {
		password := randomPassword(24)
		if !allowed.MatchString(password) {
			t.Fatalf("password %q contains DSN-hostile characters", password)
		}
	}
}
