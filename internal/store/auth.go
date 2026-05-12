package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var ErrNotFound = errors.New("store: not found")

type UserRecord struct {
	ID               string
	Email            string
	PasswordHash     string
	TOTPSecret       string
	FailedLoginCount int
	LockedUntil      *time.Time
	CreatedAt        time.Time
}

type SessionRecord struct {
	Token     string
	UserID    string
	ExpiresAt time.Time
	IP        string
	UserAgent string
	CreatedAt time.Time
}

func (s *Store) UserCount(ctx context.Context) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return count, nil
}

func (s *Store) CreateUser(ctx context.Context, user UserRecord) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users (
			id, email, password_hash, totp_secret, failed_login_count, locked_until
		) VALUES (?, ?, ?, ?, ?, ?)
	`, user.ID, user.Email, user.PasswordHash, user.TOTPSecret, user.FailedLoginCount, nullableTime(user.LockedUntil))
	if err != nil {
		return fmt.Errorf("create user %s: %w", user.Email, err)
	}
	return nil
}

func (s *Store) GetUserByEmail(ctx context.Context, email string) (UserRecord, error) {
	return getUser(ctx, s.db, `SELECT id, email, password_hash, totp_secret, failed_login_count, locked_until, created_at FROM users WHERE email = ?`, email)
}

func (s *Store) GetUserByID(ctx context.Context, userID string) (UserRecord, error) {
	return getUser(ctx, s.db, `SELECT id, email, password_hash, totp_secret, failed_login_count, locked_until, created_at FROM users WHERE id = ?`, userID)
}

func getUser(ctx context.Context, db *sql.DB, query string, arg string) (UserRecord, error) {
	var user UserRecord
	var lockedUntil sql.NullTime

	err := db.QueryRowContext(ctx, query, arg).Scan(
		&user.ID,
		&user.Email,
		&user.PasswordHash,
		&user.TOTPSecret,
		&user.FailedLoginCount,
		&lockedUntil,
		&user.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UserRecord{}, ErrNotFound
		}
		return UserRecord{}, fmt.Errorf("get user: %w", err)
	}

	if lockedUntil.Valid {
		user.LockedUntil = &lockedUntil.Time
	}

	return user, nil
}

func (s *Store) UpdateUserLoginFailures(ctx context.Context, userID string, failedCount int, lockedUntil *time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE users
		SET failed_login_count = ?, locked_until = ?
		WHERE id = ?
	`, failedCount, nullableTime(lockedUntil), userID)
	if err != nil {
		return fmt.Errorf("update user login failures for %s: %w", userID, err)
	}
	return nil
}

func (s *Store) ClearUserLoginFailures(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE users
		SET failed_login_count = 0, locked_until = NULL
		WHERE id = ?
	`, userID)
	if err != nil {
		return fmt.Errorf("clear user login failures for %s: %w", userID, err)
	}
	return nil
}

func (s *Store) CreateSession(ctx context.Context, session SessionRecord) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (token, user_id, expires_at, ip, user_agent)
		VALUES (?, ?, ?, ?, ?)
	`, session.Token, session.UserID, session.ExpiresAt, session.IP, session.UserAgent)
	if err != nil {
		return fmt.Errorf("create session for user %s: %w", session.UserID, err)
	}
	return nil
}

func (s *Store) GetSession(ctx context.Context, token string) (SessionRecord, error) {
	var session SessionRecord
	err := s.db.QueryRowContext(ctx, `
		SELECT token, user_id, expires_at, ip, user_agent, created_at
		FROM sessions
		WHERE token = ?
	`, token).Scan(
		&session.Token,
		&session.UserID,
		&session.ExpiresAt,
		&session.IP,
		&session.UserAgent,
		&session.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SessionRecord{}, ErrNotFound
		}
		return SessionRecord{}, fmt.Errorf("get session: %w", err)
	}
	return session, nil
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token = ?`, token)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, now)
	if err != nil {
		return fmt.Errorf("delete expired sessions: %w", err)
	}
	return nil
}

func (s *Store) InsertAuditLog(ctx context.Context, id, userID, action, target string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal audit payload: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO audit_log (id, user_id, action, target, payload_json)
		VALUES (?, ?, ?, ?, ?)
	`, id, nullableString(userID), action, target, string(body))
	if err != nil {
		return fmt.Errorf("insert audit log %s: %w", action, err)
	}
	return nil
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
