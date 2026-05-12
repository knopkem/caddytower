package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type ProjectDomainRecord struct {
	ID            int64
	ProjectID     string
	Hostname      string
	IsPrimary     bool
	DNSVerifiedAt time.Time
	CreatedAt     time.Time
}

type ProjectDeployRecord struct {
	ID          int64
	ProjectID   string
	ImageDigest string
	ImageRef    string
	Status      string
	Trigger     string
	Actor       string
	StartedAt   time.Time
	FinishedAt  time.Time
	Error       string
}

func (s *Store) ListProjectDomains(ctx context.Context, projectID string) ([]ProjectDomainRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, hostname, is_primary, dns_verified_at, created_at
		FROM project_domains
		WHERE project_id = ?
		ORDER BY is_primary DESC, hostname ASC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list project domains for %s: %w", projectID, err)
	}
	defer rows.Close()

	domains := []ProjectDomainRecord{}
	for rows.Next() {
		var record ProjectDomainRecord
		var isPrimary int
		var verifiedAt sql.NullTime
		if err := rows.Scan(&record.ID, &record.ProjectID, &record.Hostname, &isPrimary, &verifiedAt, &record.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan project domain for %s: %w", projectID, err)
		}
		record.IsPrimary = isPrimary == 1
		if verifiedAt.Valid {
			record.DNSVerifiedAt = verifiedAt.Time
		}
		domains = append(domains, record)
	}
	return domains, rows.Err()
}

func (s *Store) GetProjectDomain(ctx context.Context, projectID string, domainID int64) (ProjectDomainRecord, error) {
	var record ProjectDomainRecord
	var isPrimary int
	var verifiedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT id, project_id, hostname, is_primary, dns_verified_at, created_at
		FROM project_domains
		WHERE project_id = ? AND id = ?
	`, projectID, domainID).Scan(&record.ID, &record.ProjectID, &record.Hostname, &isPrimary, &verifiedAt, &record.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProjectDomainRecord{}, ErrNotFound
		}
		return ProjectDomainRecord{}, fmt.Errorf("get project domain %d for %s: %w", domainID, projectID, err)
	}
	record.IsPrimary = isPrimary == 1
	if verifiedAt.Valid {
		record.DNSVerifiedAt = verifiedAt.Time
	}
	return record, nil
}

func (s *Store) CreateProjectDomain(ctx context.Context, record ProjectDomainRecord) (ProjectDomainRecord, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProjectDomainRecord{}, fmt.Errorf("begin create project domain tx: %w", err)
	}
	if record.IsPrimary {
		if _, err := tx.ExecContext(ctx, `UPDATE project_domains SET is_primary = 0 WHERE project_id = ?`, record.ProjectID); err != nil {
			_ = tx.Rollback()
			return ProjectDomainRecord{}, fmt.Errorf("clear primary project domains for %s: %w", record.ProjectID, err)
		}
	}
	var verifiedAt *time.Time
	if !record.DNSVerifiedAt.IsZero() {
		value := record.DNSVerifiedAt.UTC()
		verifiedAt = &value
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO project_domains (project_id, hostname, is_primary, dns_verified_at)
		VALUES (?, ?, ?, ?)
	`, record.ProjectID, record.Hostname, boolToInt(record.IsPrimary), nullableTime(verifiedAt))
	if err != nil {
		_ = tx.Rollback()
		return ProjectDomainRecord{}, fmt.Errorf("create project domain %s: %w", record.Hostname, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		_ = tx.Rollback()
		return ProjectDomainRecord{}, fmt.Errorf("read project domain insert id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ProjectDomainRecord{}, fmt.Errorf("commit create project domain tx: %w", err)
	}
	return s.GetProjectDomain(ctx, record.ProjectID, id)
}

func (s *Store) DeleteProjectDomain(ctx context.Context, projectID string, domainID int64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM project_domains WHERE project_id = ? AND id = ?`, projectID, domainID)
	if err != nil {
		return fmt.Errorf("delete project domain %d for %s: %w", domainID, projectID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for delete project domain %d: %w", domainID, err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) MarkProjectDomainVerified(ctx context.Context, projectID string, domainID int64, verifiedAt time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE project_domains
		SET dns_verified_at = ?
		WHERE project_id = ? AND id = ?
	`, verifiedAt.UTC(), projectID, domainID)
	if err != nil {
		return fmt.Errorf("mark project domain %d verified: %w", domainID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for mark project domain %d verified: %w", domainID, err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) StartProjectDeploy(ctx context.Context, record ProjectDeployRecord) (ProjectDeployRecord, error) {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO project_deploys (project_id, image_digest, image_ref, status, trigger, actor)
		VALUES (?, ?, ?, ?, ?, ?)
	`, record.ProjectID, nullableString(record.ImageDigest), record.ImageRef, record.Status, record.Trigger, nullableString(record.Actor))
	if err != nil {
		return ProjectDeployRecord{}, fmt.Errorf("start project deploy for %s: %w", record.ProjectID, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ProjectDeployRecord{}, fmt.Errorf("read project deploy insert id: %w", err)
	}
	return s.GetProjectDeploy(ctx, record.ProjectID, id)
}

func (s *Store) FinishProjectDeploy(ctx context.Context, projectID string, deployID int64, status, imageDigest, errorMessage string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE project_deploys
		SET status = ?, image_digest = ?, error = ?, finished_at = CURRENT_TIMESTAMP
		WHERE project_id = ? AND id = ?
	`, status, nullableString(imageDigest), nullableString(errorMessage), projectID, deployID)
	if err != nil {
		return fmt.Errorf("finish project deploy %d for %s: %w", deployID, projectID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for finish project deploy %d: %w", deployID, err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetProjectDeploy(ctx context.Context, projectID string, deployID int64) (ProjectDeployRecord, error) {
	var record ProjectDeployRecord
	var imageDigest sql.NullString
	var actor sql.NullString
	var finishedAt sql.NullTime
	var errorMessage sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT id, project_id, image_digest, image_ref, status, trigger, actor, started_at, finished_at, error
		FROM project_deploys
		WHERE project_id = ? AND id = ?
	`, projectID, deployID).Scan(&record.ID, &record.ProjectID, &imageDigest, &record.ImageRef, &record.Status, &record.Trigger, &actor, &record.StartedAt, &finishedAt, &errorMessage)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProjectDeployRecord{}, ErrNotFound
		}
		return ProjectDeployRecord{}, fmt.Errorf("get project deploy %d for %s: %w", deployID, projectID, err)
	}
	record.ImageDigest = imageDigest.String
	record.Actor = actor.String
	if finishedAt.Valid {
		record.FinishedAt = finishedAt.Time
	}
	record.Error = errorMessage.String
	return record, nil
}

func (s *Store) ListProjectDeploys(ctx context.Context, projectID string, limit int) ([]ProjectDeployRecord, error) {
	query := `
		SELECT id, project_id, image_digest, image_ref, status, trigger, actor, started_at, finished_at, error
		FROM project_deploys
		WHERE project_id = ?
		ORDER BY started_at DESC, id DESC
	`
	args := []any{projectID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list project deploys for %s: %w", projectID, err)
	}
	defer rows.Close()

	deploys := []ProjectDeployRecord{}
	for rows.Next() {
		var record ProjectDeployRecord
		var imageDigest sql.NullString
		var actor sql.NullString
		var finishedAt sql.NullTime
		var errorMessage sql.NullString
		if err := rows.Scan(&record.ID, &record.ProjectID, &imageDigest, &record.ImageRef, &record.Status, &record.Trigger, &actor, &record.StartedAt, &finishedAt, &errorMessage); err != nil {
			return nil, fmt.Errorf("scan project deploy for %s: %w", projectID, err)
		}
		record.ImageDigest = imageDigest.String
		record.Actor = actor.String
		if finishedAt.Valid {
			record.FinishedAt = finishedAt.Time
		}
		record.Error = errorMessage.String
		deploys = append(deploys, record)
	}
	return deploys, rows.Err()
}
