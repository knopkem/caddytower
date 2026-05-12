package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type DBAttachmentRecord struct {
	ID         int64
	ProjectID  string
	Engine     string
	DBName     string
	DBUser     string
	DBPassword string
	EnvVarName string
	CreatedAt  time.Time
}

func (s *Store) CreateDBAttachment(ctx context.Context, attachment DBAttachmentRecord) (DBAttachmentRecord, error) {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO project_db_attachments (
			project_id, engine, db_name, db_user, db_password, env_var_name
		) VALUES (?, ?, ?, ?, ?, ?)
	`, attachment.ProjectID, attachment.Engine, attachment.DBName, attachment.DBUser, attachment.DBPassword, attachment.EnvVarName)
	if err != nil {
		return DBAttachmentRecord{}, fmt.Errorf("create db attachment: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return DBAttachmentRecord{}, fmt.Errorf("last insert id for db attachment: %w", err)
	}

	return s.GetDBAttachment(ctx, id)
}

func (s *Store) GetDBAttachment(ctx context.Context, attachmentID int64) (DBAttachmentRecord, error) {
	var attachment DBAttachmentRecord
	err := s.db.QueryRowContext(ctx, `
		SELECT id, project_id, engine, db_name, db_user, db_password, env_var_name, created_at
		FROM project_db_attachments
		WHERE id = ?
	`, attachmentID).Scan(
		&attachment.ID,
		&attachment.ProjectID,
		&attachment.Engine,
		&attachment.DBName,
		&attachment.DBUser,
		&attachment.DBPassword,
		&attachment.EnvVarName,
		&attachment.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DBAttachmentRecord{}, ErrNotFound
		}
		return DBAttachmentRecord{}, fmt.Errorf("get db attachment %d: %w", attachmentID, err)
	}
	return attachment, nil
}

func (s *Store) ListDBAttachmentsByProject(ctx context.Context, projectID string) ([]DBAttachmentRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, engine, db_name, db_user, db_password, env_var_name, created_at
		FROM project_db_attachments
		WHERE project_id = ?
		ORDER BY created_at ASC, id ASC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list db attachments for project %s: %w", projectID, err)
	}
	defer rows.Close()

	var attachments []DBAttachmentRecord
	for rows.Next() {
		var attachment DBAttachmentRecord
		if err := rows.Scan(
			&attachment.ID,
			&attachment.ProjectID,
			&attachment.Engine,
			&attachment.DBName,
			&attachment.DBUser,
			&attachment.DBPassword,
			&attachment.EnvVarName,
			&attachment.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan db attachment: %w", err)
		}
		attachments = append(attachments, attachment)
	}

	return attachments, rows.Err()
}

func (s *Store) UpdateDBAttachmentPassword(ctx context.Context, attachmentID int64, encodedPassword string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE project_db_attachments
		SET db_password = ?
		WHERE id = ?
	`, encodedPassword, attachmentID)
	if err != nil {
		return fmt.Errorf("update db attachment password %d: %w", attachmentID, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for update db attachment password %d: %w", attachmentID, err)
	}
	if affected == 0 {
		return ErrNotFound
	}

	return nil
}

func (s *Store) DeleteDBAttachment(ctx context.Context, attachmentID int64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM project_db_attachments WHERE id = ?`, attachmentID)
	if err != nil {
		return fmt.Errorf("delete db attachment %d: %w", attachmentID, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected for delete db attachment %d: %w", attachmentID, err)
	}
	if affected == 0 {
		return ErrNotFound
	}

	return nil
}
