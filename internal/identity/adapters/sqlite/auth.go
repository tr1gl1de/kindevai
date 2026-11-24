package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tr1gl1de/kindevai/internal/identity/domain"
)

// tsFormat is fixed-width UTC so stored timestamps sort correctly as text in SQL comparisons.
// time.Parse(time.RFC3339Nano, …) reads it back.
const tsFormat = "2006-01-02T15:04:05.000000000Z07:00"

// UserRepository reads users.
type UserRepository struct{ db *sql.DB }

// NewUserRepository returns a repository on d.
func NewUserRepository(d *sql.DB) *UserRepository { return &UserRepository{db: d} }

const userCols = "id, login, password_hash, platform_role, created_at, disabled_at IS NOT NULL"

func scanUser(row *sql.Row) (domain.User, error) {
	var u domain.User
	var login, role, created string
	err := row.Scan(&u.ID, &login, &u.PasswordHash, &role, &created, &u.Disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.User{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("reading user: %w", err)
	}
	u.Login, u.Role = domain.Login(login), domain.PlatformRole(role)
	if u.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return domain.User{}, fmt.Errorf("reading user: %w", err)
	}
	return u, nil
}

// ByLogin finds a user by normalised login.
func (r *UserRepository) ByLogin(ctx context.Context, l domain.Login) (domain.User, error) {
	return scanUser(r.db.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE login = ?", string(l)))
}

// ByID finds a user by id.
func (r *UserRepository) ByID(ctx context.Context, id string) (domain.User, error) {
	return scanUser(r.db.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE id = ?", id))
}

// SessionRepository stores login sessions.
type SessionRepository struct{ db *sql.DB }

// NewSessionRepository returns a repository on d.
func NewSessionRepository(d *sql.DB) *SessionRepository { return &SessionRepository{db: d} }

// Create stores s.
func (r *SessionRepository) Create(ctx context.Context, s domain.Session) error {
	_, err := r.db.ExecContext(ctx,
		"INSERT INTO user_sessions(token_hash, user_id, created_at, last_seen_at, expires_at) VALUES (?, ?, ?, ?, ?)",
		s.TokenHash, s.UserID, s.CreatedAt.UTC().Format(tsFormat), s.LastSeenAt.UTC().Format(tsFormat),
		s.ExpiresAt(s.LastSeenAt).UTC().Format(tsFormat))
	if err != nil {
		return fmt.Errorf("creating session: %w", err)
	}
	return nil
}

// ByTokenHash loads a session.
func (r *SessionRepository) ByTokenHash(ctx context.Context, hash []byte) (domain.Session, error) {
	var s domain.Session
	var created, seen string
	err := r.db.QueryRowContext(ctx,
		"SELECT token_hash, user_id, created_at, last_seen_at FROM user_sessions WHERE token_hash = ?", hash).
		Scan(&s.TokenHash, &s.UserID, &created, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Session{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Session{}, fmt.Errorf("reading session: %w", err)
	}
	if s.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return domain.Session{}, fmt.Errorf("reading session: %w", err)
	}
	if s.LastSeenAt, err = time.Parse(time.RFC3339Nano, seen); err != nil {
		return domain.Session{}, fmt.Errorf("reading session: %w", err)
	}
	return s, nil
}

// Touch records activity and moves the expiry.
func (r *SessionRepository) Touch(ctx context.Context, hash []byte, at time.Time) error {
	s, err := r.ByTokenHash(ctx, hash)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, "UPDATE user_sessions SET last_seen_at = ?, expires_at = ? WHERE token_hash = ?",
		at.UTC().Format(tsFormat), s.ExpiresAt(at).UTC().Format(tsFormat), hash)
	if err != nil {
		return fmt.Errorf("touching session: %w", err)
	}
	return nil
}

// Delete removes a session.
func (r *SessionRepository) Delete(ctx context.Context, hash []byte) error {
	if _, err := r.db.ExecContext(ctx, "DELETE FROM user_sessions WHERE token_hash = ?", hash); err != nil {
		return fmt.Errorf("deleting session: %w", err)
	}
	return nil
}

// DeleteExpired removes sessions past their expiry.
func (r *SessionRepository) DeleteExpired(ctx context.Context, now time.Time) error {
	if _, err := r.db.ExecContext(ctx, "DELETE FROM user_sessions WHERE expires_at <= ?", now.UTC().Format(tsFormat)); err != nil {
		return fmt.Errorf("purging sessions: %w", err)
	}
	return nil
}
