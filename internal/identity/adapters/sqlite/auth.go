package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tr1gl1de/kindevai/internal/identity/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
)

// tsFormat is fixed-width UTC so stored timestamps sort correctly as text in SQL comparisons.
// time.Parse(time.RFC3339Nano, …) reads it back.
const tsFormat = "2006-01-02T15:04:05.000000000Z07:00"

// UserRepository reads users.
type UserRepository struct{ db *sql.DB }

// NewUserRepository returns a repository on d.
func NewUserRepository(d *sql.DB) *UserRepository { return &UserRepository{db: d} }

const userCols = "id, login, password_hash, platform_role, created_at, disabled_at IS NOT NULL, must_change_password"

type rowScanner interface{ Scan(dest ...any) error }

func scanUser(row rowScanner) (domain.User, error) {
	var u domain.User
	var login, role, created string
	err := row.Scan(&u.ID, &login, &u.PasswordHash, &role, &created, &u.Disabled, &u.MustChangePassword)
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
	return scanUser(db.Conn(ctx, r.db).QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE login = ?", string(l)))
}

// ByID finds a user by id.
func (r *UserRepository) ByID(ctx context.Context, id string) (domain.User, error) {
	return scanUser(db.Conn(ctx, r.db).QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE id = ?", id))
}

// Create inserts an account; a duplicate login is domain.ErrLoginTaken.
func (r *UserRepository) Create(ctx context.Context, u domain.User) error {
	_, err := db.Conn(ctx, r.db).ExecContext(ctx, `INSERT INTO users(id, login, password_hash, platform_role, created_at, must_change_password)
		VALUES (?, ?, ?, ?, ?, ?)`, u.ID, string(u.Login), u.PasswordHash, string(u.Role), u.CreatedAt.UTC().Format(tsFormat), u.MustChangePassword)
	if db.IsUniqueViolation(err) {
		return domain.ErrLoginTaken
	}
	if err != nil {
		return fmt.Errorf("creating user: %w", err)
	}
	return nil
}

// List returns all accounts ordered by login.
func (r *UserRepository) List(ctx context.Context) ([]domain.User, error) {
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, "SELECT "+userCols+" FROM users ORDER BY login")
	if err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}
	defer rows.Close()
	var out []domain.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (r *UserRepository) update(ctx context.Context, q string, args ...any) error {
	res, err := db.Conn(ctx, r.db).ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("updating user: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// SetDisabled disables (now) or re-enables an account.
func (r *UserRepository) SetDisabled(ctx context.Context, id string, disabled bool) error {
	if disabled {
		return r.update(ctx, "UPDATE users SET disabled_at = coalesce(disabled_at, ?) WHERE id = ?",
			time.Now().UTC().Format(tsFormat), id)
	}
	return r.update(ctx, "UPDATE users SET disabled_at = NULL WHERE id = ?", id)
}

// SetPassword stores a new password hash and whether it is temporary.
func (r *UserRepository) SetPassword(ctx context.Context, id, hash string, mustChange bool) error {
	return r.update(ctx, "UPDATE users SET password_hash = ?, must_change_password = ? WHERE id = ?", hash, mustChange, id)
}

// SessionRepository stores login sessions.
type SessionRepository struct{ db *sql.DB }

// NewSessionRepository returns a repository on d.
func NewSessionRepository(d *sql.DB) *SessionRepository { return &SessionRepository{db: d} }

// Create stores s.
func (r *SessionRepository) Create(ctx context.Context, s domain.Session) error {
	_, err := db.Conn(ctx, r.db).ExecContext(ctx,
		"INSERT INTO user_sessions(token_hash, user_id, created_at, last_seen_at, expires_at, via) VALUES (?, ?, ?, ?, ?, ?)",
		s.TokenHash, s.UserID, s.CreatedAt.UTC().Format(tsFormat), s.LastSeenAt.UTC().Format(tsFormat),
		s.ExpiresAt(s.LastSeenAt).UTC().Format(tsFormat), string(via(s.Via)))
	if err != nil {
		return fmt.Errorf("creating session: %w", err)
	}
	return nil
}

// ByTokenHash loads a session.
func (r *SessionRepository) ByTokenHash(ctx context.Context, hash []byte) (domain.Session, error) {
	var s domain.Session
	var created, seen, v string
	err := db.Conn(ctx, r.db).QueryRowContext(ctx,
		"SELECT token_hash, user_id, created_at, last_seen_at, via FROM user_sessions WHERE token_hash = ?", hash).
		Scan(&s.TokenHash, &s.UserID, &created, &seen, &v)
	s.Via = domain.SessionVia(v)
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

// Touch records activity and moves the expiry in one statement.
func (r *SessionRepository) Touch(ctx context.Context, s domain.Session, at time.Time) error {
	res, err := db.Conn(ctx, r.db).ExecContext(ctx, "UPDATE user_sessions SET last_seen_at = ?, expires_at = ? WHERE token_hash = ?",
		at.UTC().Format(tsFormat), s.ExpiresAt(at).UTC().Format(tsFormat), s.TokenHash)
	if err != nil {
		return fmt.Errorf("touching session: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// Delete removes a session.
func (r *SessionRepository) Delete(ctx context.Context, hash []byte) error {
	if _, err := db.Conn(ctx, r.db).ExecContext(ctx, "DELETE FROM user_sessions WHERE token_hash = ?", hash); err != nil {
		return fmt.Errorf("deleting session: %w", err)
	}
	return nil
}

// DeleteByUser ends all sessions of a user except the one with token hash keep (nil keeps none).
func (r *SessionRepository) DeleteByUser(ctx context.Context, userID string, keep []byte) error {
	if keep == nil {
		keep = []byte{}
	}
	if _, err := db.Conn(ctx, r.db).ExecContext(ctx, "DELETE FROM user_sessions WHERE user_id = ? AND token_hash <> ?", userID, keep); err != nil {
		return fmt.Errorf("revoking sessions: %w", err)
	}
	return nil
}

// DeleteByUserVia ends all sessions of a user that were opened via v.
func (r *SessionRepository) DeleteByUserVia(ctx context.Context, userID string, v domain.SessionVia) error {
	if _, err := db.Conn(ctx, r.db).ExecContext(ctx, "DELETE FROM user_sessions WHERE user_id = ? AND via = ?", userID, string(v)); err != nil {
		return fmt.Errorf("revoking sessions: %w", err)
	}
	return nil
}

func via(v domain.SessionVia) domain.SessionVia {
	if v == "" {
		return domain.ViaPassword
	}
	return v
}

// DeleteExpired removes sessions past their expiry.
func (r *SessionRepository) DeleteExpired(ctx context.Context, now time.Time) error {
	if _, err := db.Conn(ctx, r.db).ExecContext(ctx, "DELETE FROM user_sessions WHERE expires_at <= ?", now.UTC().Format(tsFormat)); err != nil {
		return fmt.Errorf("purging sessions: %w", err)
	}
	return nil
}
