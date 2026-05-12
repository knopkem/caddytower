package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type AuditLogRecord struct {
	ID        string
	Timestamp time.Time
	UserEmail string
	Action    string
	Target    string
	Payload   string
}

func (s *Store) ListAuditLogs(ctx context.Context, filter string, limit int) ([]AuditLogRecord, error) {
	query := `
		SELECT audit_log.id, audit_log.ts, COALESCE(users.email, ''), audit_log.action, audit_log.target, audit_log.payload_json
		FROM audit_log
		LEFT JOIN users ON users.id = audit_log.user_id
	`
	args := []any{}
	filter = strings.TrimSpace(filter)
	if filter != "" {
		query += ` WHERE audit_log.action LIKE ? OR audit_log.target LIKE ? OR COALESCE(users.email, '') LIKE ? `
		like := "%" + filter + "%"
		args = append(args, like, like, like)
	}
	query += ` ORDER BY audit_log.ts DESC `
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit logs: %w", err)
	}
	defer rows.Close()

	logs := []AuditLogRecord{}
	for rows.Next() {
		var record AuditLogRecord
		var userEmail sql.NullString
		if err := rows.Scan(&record.ID, &record.Timestamp, &userEmail, &record.Action, &record.Target, &record.Payload); err != nil {
			return nil, fmt.Errorf("scan audit log row: %w", err)
		}
		record.UserEmail = userEmail.String
		logs = append(logs, record)
	}
	return logs, rows.Err()
}
