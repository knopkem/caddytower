package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

type ProjectRecord struct {
	ID                string
	Slug              string
	Name              string
	Type              string
	ImageRef          string
	InternalPort      int
	Subdomain         string
	WatchtowerEnabled bool
	WebhookSecret     string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Env               map[string]string
	Ports             []ProjectPortRecord
}

type ProjectPortRecord struct {
	ID            int64
	ProjectID     string
	Proto         string
	HostPort      int
	ContainerPort int
	CreatedAt     time.Time
}

func (s *Store) GetSettings(ctx context.Context, keys ...string) (map[string]string, error) {
	settings := map[string]string{}

	if len(keys) == 0 {
		rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
		if err != nil {
			return nil, fmt.Errorf("query settings: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var key, value string
			if err := rows.Scan(&key, &value); err != nil {
				return nil, fmt.Errorf("scan settings row: %w", err)
			}
			settings[key] = value
		}
		return settings, rows.Err()
	}

	query := `SELECT key, value FROM settings WHERE key IN (` + placeholders(len(keys)) + `)`
	args := make([]any, 0, len(keys))
	for _, key := range keys {
		args = append(args, key)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query settings by key: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("scan settings row: %w", err)
		}
		settings[key] = value
	}

	return settings, rows.Err()
}

func (s *Store) UpsertSettings(ctx context.Context, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin settings tx: %w", err)
	}

	for key, value := range values {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO settings (key, value, updated_at)
			VALUES (?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(key) DO UPDATE SET
				value = excluded.value,
				updated_at = CURRENT_TIMESTAMP
		`, key, value); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("upsert setting %s: %w", key, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit settings tx: %w", err)
	}

	return nil
}

func (s *Store) CreateProject(ctx context.Context, project ProjectRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin create project tx: %w", err)
	}

	if err := upsertProject(ctx, tx, project, false); err != nil {
		_ = tx.Rollback()
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit create project tx: %w", err)
	}

	return nil
}

func (s *Store) UpdateProject(ctx context.Context, project ProjectRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin update project tx: %w", err)
	}

	if err := upsertProject(ctx, tx, project, true); err != nil {
		_ = tx.Rollback()
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit update project tx: %w", err)
	}

	return nil
}

func upsertProject(ctx context.Context, tx *sql.Tx, project ProjectRecord, update bool) error {
	if update {
		result, err := tx.ExecContext(ctx, `
			UPDATE projects
			SET name = ?, type = ?, image_ref = ?, internal_port = ?, subdomain = ?, watchtower_enabled = ?, updated_at = CURRENT_TIMESTAMP
			WHERE id = ?
		`, project.Name, project.Type, project.ImageRef, project.InternalPort, project.Subdomain, boolToInt(project.WatchtowerEnabled), project.ID)
		if err != nil {
			return fmt.Errorf("update project %s: %w", project.ID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("rows affected for update project %s: %w", project.ID, err)
		}
		if affected == 0 {
			return ErrNotFound
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO projects (
				id, slug, name, type, image_ref, internal_port, subdomain, watchtower_enabled, webhook_secret
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, project.ID, project.Slug, project.Name, project.Type, project.ImageRef, project.InternalPort, project.Subdomain, boolToInt(project.WatchtowerEnabled), project.WebhookSecret); err != nil {
			return fmt.Errorf("create project %s: %w", project.ID, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM project_env WHERE project_id = ?`, project.ID); err != nil {
		return fmt.Errorf("clear env for project %s: %w", project.ID, err)
	}

	keys := make([]string, 0, len(project.Env))
	for key := range project.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO project_env (project_id, key, value)
			VALUES (?, ?, ?)
		`, project.ID, key, project.Env[key]); err != nil {
			return fmt.Errorf("insert env %s for project %s: %w", key, project.ID, err)
		}
	}

	if err := replaceProjectPorts(ctx, tx, project.ID, project.Ports); err != nil {
		return err
	}

	return nil
}

func (s *Store) GetProject(ctx context.Context, projectID string) (ProjectRecord, error) {
	var project ProjectRecord
	var watchtowerEnabled int
	err := s.db.QueryRowContext(ctx, `
		SELECT id, slug, name, type, image_ref, internal_port, subdomain, watchtower_enabled, webhook_secret, created_at, updated_at
		FROM projects
		WHERE id = ?
	`, projectID).Scan(
		&project.ID,
		&project.Slug,
		&project.Name,
		&project.Type,
		&project.ImageRef,
		&project.InternalPort,
		&project.Subdomain,
		&watchtowerEnabled,
		&project.WebhookSecret,
		&project.CreatedAt,
		&project.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProjectRecord{}, ErrNotFound
		}
		return ProjectRecord{}, fmt.Errorf("get project %s: %w", projectID, err)
	}

	project.WatchtowerEnabled = watchtowerEnabled == 1
	env, err := s.getProjectEnv(ctx, project.ID)
	if err != nil {
		return ProjectRecord{}, err
	}
	project.Env = env
	ports, err := s.getProjectPorts(ctx, project.ID)
	if err != nil {
		return ProjectRecord{}, err
	}
	project.Ports = ports

	return project, nil
}

func (s *Store) GetProjectBySlug(ctx context.Context, slug string) (ProjectRecord, error) {
	var project ProjectRecord
	var watchtowerEnabled int
	err := s.db.QueryRowContext(ctx, `
		SELECT id, slug, name, type, image_ref, internal_port, subdomain, watchtower_enabled, webhook_secret, created_at, updated_at
		FROM projects
		WHERE slug = ?
	`, slug).Scan(
		&project.ID,
		&project.Slug,
		&project.Name,
		&project.Type,
		&project.ImageRef,
		&project.InternalPort,
		&project.Subdomain,
		&watchtowerEnabled,
		&project.WebhookSecret,
		&project.CreatedAt,
		&project.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProjectRecord{}, ErrNotFound
		}
		return ProjectRecord{}, fmt.Errorf("get project by slug %s: %w", slug, err)
	}

	project.WatchtowerEnabled = watchtowerEnabled == 1
	env, err := s.getProjectEnv(ctx, project.ID)
	if err != nil {
		return ProjectRecord{}, err
	}
	project.Env = env
	ports, err := s.getProjectPorts(ctx, project.ID)
	if err != nil {
		return ProjectRecord{}, err
	}
	project.Ports = ports

	return project, nil
}

func (s *Store) ListProjects(ctx context.Context) ([]ProjectRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, slug, name, type, image_ref, internal_port, subdomain, watchtower_enabled, webhook_secret, created_at, updated_at
		FROM projects
		ORDER BY created_at ASC, name ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var projects []ProjectRecord
	for rows.Next() {
		var project ProjectRecord
		var watchtowerEnabled int
		if err := rows.Scan(
			&project.ID,
			&project.Slug,
			&project.Name,
			&project.Type,
			&project.ImageRef,
			&project.InternalPort,
			&project.Subdomain,
			&watchtowerEnabled,
			&project.WebhookSecret,
			&project.CreatedAt,
			&project.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan project row: %w", err)
		}
		project.WatchtowerEnabled = watchtowerEnabled == 1
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range projects {
		env, err := s.getProjectEnv(ctx, projects[i].ID)
		if err != nil {
			return nil, err
		}
		projects[i].Env = env
		ports, err := s.getProjectPorts(ctx, projects[i].ID)
		if err != nil {
			return nil, err
		}
		projects[i].Ports = ports
	}

	return projects, nil
}

func (s *Store) DeleteProject(ctx context.Context, projectID string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, projectID)
	if err != nil {
		return fmt.Errorf("delete project %s: %w", projectID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for delete project %s: %w", projectID, err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) getProjectEnv(ctx context.Context, projectID string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT key, value
		FROM project_env
		WHERE project_id = ?
		ORDER BY key ASC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("query env for project %s: %w", projectID, err)
	}
	defer rows.Close()

	env := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("scan env for project %s: %w", projectID, err)
		}
		env[key] = value
	}
	return env, rows.Err()
}

func (s *Store) getProjectPorts(ctx context.Context, projectID string) ([]ProjectPortRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, proto, host_port, container_port, created_at
		FROM project_ports
		WHERE project_id = ?
		ORDER BY proto ASC, host_port ASC, container_port ASC, id ASC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("query ports for project %s: %w", projectID, err)
	}
	defer rows.Close()

	var ports []ProjectPortRecord
	for rows.Next() {
		var port ProjectPortRecord
		if err := rows.Scan(
			&port.ID,
			&port.ProjectID,
			&port.Proto,
			&port.HostPort,
			&port.ContainerPort,
			&port.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan port for project %s: %w", projectID, err)
		}
		ports = append(ports, port)
	}

	return ports, rows.Err()
}

func replaceProjectPorts(ctx context.Context, tx *sql.Tx, projectID string, ports []ProjectPortRecord) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM project_ports WHERE project_id = ?`, projectID); err != nil {
		return fmt.Errorf("clear ports for project %s: %w", projectID, err)
	}

	if len(ports) == 0 {
		return nil
	}

	sortedPorts := append([]ProjectPortRecord(nil), ports...)
	sort.Slice(sortedPorts, func(i, j int) bool {
		if sortedPorts[i].Proto != sortedPorts[j].Proto {
			return sortedPorts[i].Proto < sortedPorts[j].Proto
		}
		if sortedPorts[i].HostPort != sortedPorts[j].HostPort {
			return sortedPorts[i].HostPort < sortedPorts[j].HostPort
		}
		return sortedPorts[i].ContainerPort < sortedPorts[j].ContainerPort
	})

	for _, port := range sortedPorts {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO project_ports (project_id, proto, host_port, container_port)
			VALUES (?, ?, ?, ?)
		`, projectID, port.Proto, port.HostPort, port.ContainerPort); err != nil {
			return fmt.Errorf("insert port %s/%d->%d for project %s: %w", port.Proto, port.HostPort, port.ContainerPort, projectID, err)
		}
	}

	return nil
}

func placeholders(count int) string {
	if count <= 0 {
		return ""
	}
	result := "?"
	for i := 1; i < count; i++ {
		result += ", ?"
	}
	return result
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
