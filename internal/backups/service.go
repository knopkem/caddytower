package backups

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"caddytower/internal/config"
	"caddytower/internal/dbengines"
	"caddytower/internal/dockerx"
	"caddytower/internal/secrets"
	"caddytower/internal/store"
)

const (
	defaultRunTimeout = 20 * time.Minute
)

type Service struct {
	cfg       config.Config
	store     *store.Store
	dbengines *dbengines.Service
	logger    *slog.Logger
	now       func() time.Time

	mu sync.Mutex
}

type Snapshot struct {
	Name      string
	Path      string
	CreatedAt time.Time
	SizeBytes int64
}

type snapshotMetadata struct {
	CreatedAt string   `json:"created_at"`
	Trigger   string   `json:"trigger"`
	Files     []string `json:"files"`
}

func New(cfg config.Config, stateStore *store.Store, secretService *secrets.Service, dockerSvc dockerService, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	return &Service{
		cfg:       cfg,
		store:     stateStore,
		dbengines: dbengines.New(stateStore, secretService, dockerSvc),
		logger:    logger,
		now:       func() time.Time { return time.Now().UTC() },
	}
}

type dockerService interface {
	InspectContainer(context.Context, string) (dockerx.ContainerInspect, error)
	RecreateContainer(context.Context, dockerx.ContainerSpec) (dockerx.ContainerInspect, error)
	Exec(context.Context, string, []string, []string, io.Writer, io.Writer) error
}

func (s *Service) Start(ctx context.Context) {
	go s.loop(ctx)
}

func (s *Service) RunNow(ctx context.Context, trigger string) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.cfg.BackupDir(), 0o755); err != nil {
		return Snapshot{}, fmt.Errorf("create backup dir: %w", err)
	}

	startedAt := s.now().UTC()
	workDir, err := os.MkdirTemp(s.cfg.BackupDir(), "snapshot-*")
	if err != nil {
		return Snapshot{}, fmt.Errorf("create backup temp dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	files := make([]string, 0, 4)

	sqlitePath := filepath.Join(workDir, "state.db")
	if err := backupSQLite(ctx, s.store.DB(), sqlitePath); err != nil {
		return Snapshot{}, err
	}
	files = append(files, filepath.Base(sqlitePath))

	if s.cfg.BackupsIncludeEngineDumps {
		engineFiles, err := s.dbengines.DumpAll(ctx, workDir)
		if err != nil {
			return Snapshot{}, err
		}
		for _, file := range engineFiles {
			files = append(files, filepath.Base(file))
		}
	}
	sort.Strings(files)

	meta := snapshotMetadata{
		CreatedAt: startedAt.Format(time.RFC3339),
		Trigger:   trigger,
		Files:     files,
	}
	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return Snapshot{}, fmt.Errorf("marshal snapshot metadata: %w", err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "metadata.json"), append(metaBytes, '\n'), 0o644); err != nil {
		return Snapshot{}, fmt.Errorf("write snapshot metadata: %w", err)
	}

	name := fmt.Sprintf("backup-%s.tar.gz", startedAt.Format("20060102T150405Z"))
	finalPath := filepath.Join(s.cfg.BackupDir(), name)
	if err := archiveDirectory(workDir, finalPath); err != nil {
		return Snapshot{}, err
	}

	if err := s.pruneOld(); err != nil {
		return Snapshot{}, err
	}

	info, err := os.Stat(finalPath)
	if err != nil {
		return Snapshot{}, fmt.Errorf("stat backup %s: %w", finalPath, err)
	}

	snapshot := Snapshot{
		Name:      name,
		Path:      finalPath,
		CreatedAt: info.ModTime().UTC(),
		SizeBytes: info.Size(),
	}
	s.logger.Info("created backup", "name", snapshot.Name, "size_bytes", snapshot.SizeBytes, "trigger", trigger)
	return snapshot, nil
}

func (s *Service) ListSnapshots() ([]Snapshot, error) {
	if err := os.MkdirAll(s.cfg.BackupDir(), 0o755); err != nil {
		return nil, fmt.Errorf("create backup dir: %w", err)
	}

	entries, err := os.ReadDir(s.cfg.BackupDir())
	if err != nil {
		return nil, fmt.Errorf("read backup dir: %w", err)
	}

	snapshots := make([]Snapshot, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".tar.gz") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("stat backup entry %s: %w", entry.Name(), err)
		}
		snapshots = append(snapshots, Snapshot{
			Name:      entry.Name(),
			Path:      filepath.Join(s.cfg.BackupDir(), entry.Name()),
			CreatedAt: info.ModTime().UTC(),
			SizeBytes: info.Size(),
		})
	}

	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].CreatedAt.After(snapshots[j].CreatedAt)
	})
	return snapshots, nil
}

func (s *Service) OpenSnapshot(name string) (*os.File, Snapshot, error) {
	if !validSnapshotName(name) {
		return nil, Snapshot{}, fmt.Errorf("invalid backup name")
	}

	path := filepath.Join(s.cfg.BackupDir(), name)
	file, err := os.Open(path)
	if err != nil {
		return nil, Snapshot{}, fmt.Errorf("open backup %s: %w", name, err)
	}

	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, Snapshot{}, fmt.Errorf("stat backup %s: %w", name, err)
	}

	return file, Snapshot{
		Name:      name,
		Path:      path,
		CreatedAt: info.ModTime().UTC(),
		SizeBytes: info.Size(),
	}, nil
}

func (s *Service) loop(ctx context.Context) {
	for {
		wait := time.Until(s.nextScheduledRun())
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		runCtx, cancel := context.WithTimeout(ctx, defaultRunTimeout)
		if _, err := s.RunNow(runCtx, "scheduled"); err != nil && !errorsIsContext(err) {
			s.logger.Error("run scheduled backup", "error", err)
		}
		cancel()
	}
}

func (s *Service) pruneOld() error {
	snapshots, err := s.ListSnapshots()
	if err != nil {
		return err
	}

	cutoff := s.now().AddDate(0, 0, -s.cfg.BackupsRetentionDays)
	for _, snapshot := range snapshots {
		if snapshot.CreatedAt.After(cutoff) {
			continue
		}
		if err := os.Remove(snapshot.Path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove old backup %s: %w", snapshot.Name, err)
		}
	}
	return nil
}

func backupSQLite(ctx context.Context, db *sql.DB, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("create sqlite backup dir: %w", err)
	}
	if err := os.RemoveAll(destPath); err != nil {
		return fmt.Errorf("remove existing sqlite backup: %w", err)
	}

	quotedPath := strings.ReplaceAll(destPath, "'", "''")
	if _, err := db.ExecContext(ctx, fmt.Sprintf("VACUUM INTO '%s';", quotedPath)); err != nil {
		return fmt.Errorf("backup sqlite database: %w", err)
	}
	return nil
}

func archiveDirectory(srcDir, destPath string) error {
	file, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create backup archive: %w", err)
	}
	defer file.Close()

	gz := gzip.NewWriter(file)
	defer gz.Close()

	tw := tar.NewWriter(gz)
	defer tw.Close()

	return filepath.WalkDir(srcDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relPath)
		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()

		if _, err := io.Copy(tw, src); err != nil {
			return err
		}
		return nil
	})
}

func (s *Service) nextScheduledRun() time.Time {
	return nextScheduledRunAt(s.now(), s.cfg.BackupsScheduleUTC)
}

func nextScheduledRunAt(now time.Time, raw string) time.Time {
	current := now.UTC()
	hour, minute := scheduledClockParts(raw)
	next := time.Date(current.Year(), current.Month(), current.Day(), hour, minute, 0, 0, time.UTC)
	if !current.Before(next) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

func validSnapshotName(name string) bool {
	if strings.TrimSpace(name) == "" || strings.Contains(name, "/") || strings.Contains(name, `\`) {
		return false
	}
	return strings.HasSuffix(name, ".tar.gz")
}

func scheduledClockParts(raw string) (int, int) {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	if len(parts) != 2 {
		return 2, 30
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return 2, 30
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return 2, 30
	}
	return hour, minute
}

func errorsIsContext(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}
