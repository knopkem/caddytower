package backups

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"caddytower/internal/config"
	"caddytower/internal/dockerx"
	"caddytower/internal/store"
)

func TestRunNowCreatesArchiveWithSQLiteAndEngineDumps(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		HTTPAddr:                  ":8080",
		PublicBaseURL:             "http://localhost:8080",
		DataDir:                   t.TempDir(),
		CaddyAdminURL:             "http://shared-caddy:2019",
		BackupsRetentionDays:      14,
		BackupsScheduleUTC:        "02:30",
		BackupsIncludeEngineDumps: true,
	}
	stateStore, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = stateStore.Close() })

	if err := stateStore.UpsertSettings(context.Background(), map[string]string{
		"postgres_root_password": "pgpass",
		"mariadb_root_password":  "mypass",
	}); err != nil {
		t.Fatalf("UpsertSettings() error = %v", err)
	}

	docker := &fakeDocker{
		inspectByName: map[string]dockerx.ContainerInspect{
			"caddytower-postgres": {Running: true},
			"caddytower-mariadb":  {Running: true},
		},
		execOutputByName: map[string]string{
			"caddytower-postgres": "-- postgres dump --\n",
			"caddytower-mariadb":  "-- mariadb dump --\n",
		},
	}

	svc := New(cfg, stateStore, nil, docker, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.now = func() time.Time { return time.Date(2026, 5, 12, 2, 30, 0, 0, time.UTC) }

	snapshot, err := svc.RunNow(context.Background(), "manual")
	if err != nil {
		t.Fatalf("RunNow() error = %v", err)
	}

	if snapshot.Name == "" {
		t.Fatal("snapshot name should not be empty")
	}

	entries := archiveEntries(t, snapshot.Path)
	for _, required := range []string{"metadata.json", "state.db", "postgres.sql", "mariadb.sql"} {
		if !slices.Contains(entries, required) {
			t.Fatalf("archive missing %s: %#v", required, entries)
		}
	}
}

func TestRunNowPrunesOldBackups(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		HTTPAddr:                  ":8080",
		PublicBaseURL:             "http://localhost:8080",
		DataDir:                   t.TempDir(),
		CaddyAdminURL:             "http://shared-caddy:2019",
		BackupsRetentionDays:      14,
		BackupsScheduleUTC:        "02:30",
		BackupsIncludeEngineDumps: true,
	}
	stateStore, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = stateStore.Close() })

	oldPath := filepath.Join(cfg.BackupDir(), "backup-old.tar.gz")
	if err := os.MkdirAll(cfg.BackupDir(), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(oldPath, []byte("old"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	oldTime := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}

	svc := New(cfg, stateStore, nil, &fakeDocker{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.now = func() time.Time { return time.Date(2026, 5, 12, 2, 30, 0, 0, time.UTC) }

	if _, err := svc.RunNow(context.Background(), "manual"); err != nil {
		t.Fatalf("RunNow() error = %v", err)
	}

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("expected old backup to be pruned, stat err = %v", err)
	}
}

func TestRunNowCanSkipEngineDumps(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		HTTPAddr:                  ":8080",
		PublicBaseURL:             "http://localhost:8080",
		DataDir:                   t.TempDir(),
		CaddyAdminURL:             "http://shared-caddy:2019",
		BackupsRetentionDays:      14,
		BackupsScheduleUTC:        "02:30",
		BackupsIncludeEngineDumps: false,
	}
	stateStore, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = stateStore.Close() })

	svc := New(cfg, stateStore, nil, &fakeDocker{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	snapshot, err := svc.RunNow(context.Background(), "manual")
	if err != nil {
		t.Fatalf("RunNow() error = %v", err)
	}

	entries := archiveEntries(t, snapshot.Path)
	if slices.Contains(entries, "postgres.sql") || slices.Contains(entries, "mariadb.sql") {
		t.Fatalf("unexpected engine dump entries: %#v", entries)
	}
}

type fakeDocker struct {
	inspectByName    map[string]dockerx.ContainerInspect
	execOutputByName map[string]string
}

func (f *fakeDocker) InspectContainer(_ context.Context, name string) (dockerx.ContainerInspect, error) {
	if inspect, ok := f.inspectByName[name]; ok {
		return inspect, nil
	}
	return dockerx.ContainerInspect{}, nil
}

func (f *fakeDocker) RecreateContainer(_ context.Context, spec dockerx.ContainerSpec) (dockerx.ContainerInspect, error) {
	return dockerx.ContainerInspect{Name: spec.Name, Running: true}, nil
}

func (f *fakeDocker) Exec(_ context.Context, containerName string, _ []string, _ []string, stdout, _ io.Writer) error {
	if stdout == nil {
		return nil
	}
	_, err := io.Copy(stdout, strings.NewReader(f.execOutputByName[containerName]))
	return err
}

func archiveEntries(t *testing.T, path string) []string {
	t.Helper()

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer file.Close()

	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatalf("gzip.NewReader() error = %v", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var names []string
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatalf("tar.Next() error = %v", err)
		}
		names = append(names, header.Name)
	}
}
