// Package sqlite stores the runners context in SQLite. Every statement runs on db.Conn, so use cases make their
// writes and the audit record atomic.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

const tsFormat = "2006-01-02T15:04:05.000000000Z07:00"

// Repository implements app.Repository.
type Repository struct{ db *sql.DB }

// New returns a repository on d.
func New(d *sql.DB) *Repository { return &Repository{db: d} }

func ts(t time.Time) string { return t.UTC().Format(tsFormat) }

func nullTS(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func parseTS(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

func parseNullTS(s sql.NullString) (*time.Time, error) {
	if !s.Valid {
		return nil, nil
	}
	t, err := parseTS(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

type scanner interface{ Scan(dest ...any) error }

const tokenCols = "id, token_hash, created_by, created_at, expires_at, consumed_at, runner_id, revoked_at" //nolint:gosec // G101: column names, not a credential

func scanToken(row scanner) (domain.EnrollmentToken, error) {
	var t domain.EnrollmentToken
	var created, expires string
	var consumed, runnerID, revoked sql.NullString
	err := row.Scan(&t.ID, &t.TokenHash, &t.CreatedBy, &created, &expires, &consumed, &runnerID, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return t, domain.ErrNotFound
	}
	if err != nil {
		return t, fmt.Errorf("reading enrollment token: %w", err)
	}
	t.RunnerID = runnerID.String
	if t.CreatedAt, err = parseTS(created); err == nil {
		if t.ExpiresAt, err = parseTS(expires); err == nil {
			if t.ConsumedAt, err = parseNullTS(consumed); err == nil {
				t.RevokedAt, err = parseNullTS(revoked)
			}
		}
	}
	if err != nil {
		return t, fmt.Errorf("reading enrollment token: %w", err)
	}
	return t, nil
}

// CreateToken inserts an enrollment token.
func (r *Repository) CreateToken(ctx context.Context, t domain.EnrollmentToken) error {
	_, err := db.Conn(ctx, r.db).ExecContext(ctx, `INSERT INTO runner_enrollment_tokens(id, token_hash, created_by, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`, t.ID, t.TokenHash, t.CreatedBy, ts(t.CreatedAt), ts(t.ExpiresAt))
	if err != nil {
		return fmt.Errorf("creating enrollment token: %w", err)
	}
	return nil
}

const tokenPendingAt = "consumed_at IS NULL AND revoked_at IS NULL AND expires_at > ?" //nolint:gosec // G101: SQL condition, not a credential

// ListTokens returns every token pending at now plus the newest limit others, newest first.
func (r *Repository) ListTokens(ctx context.Context, limit int, now time.Time) ([]domain.EnrollmentToken, error) {
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, "SELECT "+tokenCols+" FROM runner_enrollment_tokens WHERE ("+tokenPendingAt+
		") OR id IN (SELECT id FROM runner_enrollment_tokens ORDER BY created_at DESC, id LIMIT ?) ORDER BY created_at DESC, id", ts(now), limit)
	if err != nil {
		return nil, fmt.Errorf("listing enrollment tokens: %w", err)
	}
	defer rows.Close()
	var out []domain.EnrollmentToken
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TokenByID loads a token.
func (r *Repository) TokenByID(ctx context.Context, id string) (domain.EnrollmentToken, error) {
	return scanToken(db.Conn(ctx, r.db).QueryRowContext(ctx, "SELECT "+tokenCols+" FROM runner_enrollment_tokens WHERE id = ?", id))
}

// TokenByHash loads the token for a presented plaintext's hash.
func (r *Repository) TokenByHash(ctx context.Context, hash []byte) (domain.EnrollmentToken, error) {
	return scanToken(db.Conn(ctx, r.db).QueryRowContext(ctx, "SELECT "+tokenCols+" FROM runner_enrollment_tokens WHERE token_hash = ?", hash))
}

func (r *Repository) update(ctx context.Context, q string, args ...any) (bool, error) {
	res, err := db.Conn(ctx, r.db).ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ConsumeToken marks the token used by runnerID if it is still pending at now (the atomic guard against reuse).
func (r *Repository) ConsumeToken(ctx context.Context, id, runnerID string, now time.Time) (bool, error) {
	return r.update(ctx, "UPDATE runner_enrollment_tokens SET consumed_at = ?, runner_id = ? WHERE id = ? AND "+tokenPendingAt,
		ts(now), runnerID, id, ts(now))
}

// RevokeToken withdraws the token if it is still pending at now.
func (r *Repository) RevokeToken(ctx context.Context, id string, now time.Time) (bool, error) {
	return r.update(ctx, "UPDATE runner_enrollment_tokens SET revoked_at = ? WHERE id = ? AND "+tokenPendingAt, ts(now), id, ts(now))
}

const runnerCols = `id, public_key, fingerprint, hostname, os, arch, version, status, created_by, enrollment_token_id,
	enrolled_at, approved_by, approved_at, disabled_at, revoked_by, revoked_at, last_seen_at, max_parallel_steps`

func scanRunner(row scanner) (domain.Runner, error) {
	var x domain.Runner
	var key []byte
	var status, enrolled string
	var createdBy, approvedBy, approvedAt, disabledAt, revokedBy, revokedAt, lastSeen sql.NullString
	err := row.Scan(&x.ID, &key, &x.Fingerprint, &x.Metadata.Hostname, &x.Metadata.OS, &x.Metadata.Arch, &x.Metadata.Version,
		&status, &createdBy, &x.EnrollmentTokenID, &enrolled, &approvedBy, &approvedAt, &disabledAt, &revokedBy, &revokedAt, &lastSeen,
		&x.MaxParallelSteps)
	if errors.Is(err, sql.ErrNoRows) {
		return x, domain.ErrNotFound
	}
	if err != nil {
		return x, fmt.Errorf("reading runner: %w", err)
	}
	x.PublicKey, x.Status = domain.PublicKey(key), domain.Status(status)
	x.CreatedBy, x.ApprovedBy, x.RevokedBy = createdBy.String, approvedBy.String, revokedBy.String
	if x.EnrolledAt, err = parseTS(enrolled); err != nil {
		return x, fmt.Errorf("reading runner: %w", err)
	}
	for _, f := range []struct {
		src sql.NullString
		dst **time.Time
	}{{approvedAt, &x.ApprovedAt}, {disabledAt, &x.DisabledAt}, {revokedAt, &x.RevokedAt}, {lastSeen, &x.LastSeenAt}} {
		if *f.dst, err = parseNullTS(f.src); err != nil {
			return x, fmt.Errorf("reading runner: %w", err)
		}
	}
	return x, nil
}

// CreateRunner inserts a newly enrolled runner (without projects).
func (r *Repository) CreateRunner(ctx context.Context, x domain.Runner) error {
	_, err := db.Conn(ctx, r.db).ExecContext(ctx, `INSERT INTO runners(id, public_key, fingerprint, hostname, os, arch, version,
		status, created_by, enrollment_token_id, enrolled_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		x.ID, []byte(x.PublicKey), x.Fingerprint, x.Metadata.Hostname, x.Metadata.OS, x.Metadata.Arch, x.Metadata.Version,
		string(x.Status), nullStr(x.CreatedBy), x.EnrollmentTokenID, ts(x.EnrolledAt))
	if db.IsUniqueViolation(err) {
		return domain.ErrKeyEnrolled
	}
	if err != nil {
		return fmt.Errorf("creating runner: %w", err)
	}
	return nil
}

func (r *Repository) projects(ctx context.Context, x *domain.Runner) error {
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx,
		"SELECT project_id FROM runner_project_permissions WHERE runner_id = ? ORDER BY project_id", x.ID)
	if err != nil {
		return fmt.Errorf("loading runner projects: %w", err)
	}
	defer rows.Close()
	x.ProjectIDs = nil
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("loading runner projects: %w", err)
		}
		x.ProjectIDs = append(x.ProjectIDs, id)
	}
	return rows.Err()
}

// Runner loads one runner with its projects.
func (r *Repository) Runner(ctx context.Context, id string) (domain.Runner, error) {
	x, err := scanRunner(db.Conn(ctx, r.db).QueryRowContext(ctx, "SELECT "+runnerCols+" FROM runners WHERE id = ?", id))
	if err != nil {
		return x, err
	}
	return x, r.projects(ctx, &x)
}

// Runners lists every runner with its projects: pending first (they need the Owner), then newest first.
func (r *Repository) Runners(ctx context.Context) ([]domain.Runner, error) {
	out, err := r.runnerRows(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out { // after the rows are closed: the pool has a single connection
		if err := r.projects(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (r *Repository) runnerRows(ctx context.Context) ([]domain.Runner, error) {
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, "SELECT "+runnerCols+
		" FROM runners ORDER BY status <> 'pending', enrolled_at DESC, id")
	if err != nil {
		return nil, fmt.Errorf("listing runners: %w", err)
	}
	defer rows.Close()
	var out []domain.Runner
	for rows.Next() {
		x, err := scanRunner(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing runners: %w", err)
	}
	return out, nil
}

// SaveRunner writes the status fields and the Owner's parallel-step limit and replaces the project permissions.
func (r *Repository) SaveRunner(ctx context.Context, x domain.Runner) error {
	c := db.Conn(ctx, r.db)
	res, err := c.ExecContext(ctx, `UPDATE runners SET status = ?, approved_by = ?, approved_at = ?, disabled_at = ?,
		revoked_by = ?, revoked_at = ?, max_parallel_steps = ? WHERE id = ?`, string(x.Status), nullStr(x.ApprovedBy),
		nullTS(x.ApprovedAt), nullTS(x.DisabledAt), nullStr(x.RevokedBy), nullTS(x.RevokedAt), x.MaxParallelSteps, x.ID)
	if err != nil {
		return fmt.Errorf("saving runner: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("saving runner: %w", err)
	} else if n != 1 {
		return domain.ErrNotFound
	}
	if _, err := c.ExecContext(ctx, "DELETE FROM runner_project_permissions WHERE runner_id = ?", x.ID); err != nil {
		return fmt.Errorf("saving runner projects: %w", err)
	}
	for _, p := range x.ProjectIDs {
		_, err := c.ExecContext(ctx, "INSERT INTO runner_project_permissions(runner_id, project_id) VALUES (?, ?)", x.ID, p)
		if db.IsForeignKeyViolation(err) {
			return domain.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("saving runner projects: %w", err)
		}
	}
	return nil
}

// MarkSeen records when the gateway last authorized the runner (domain.ErrNotFound for an unknown runner).
func (r *Repository) MarkSeen(ctx context.Context, id string, at time.Time) error {
	ok, err := r.update(ctx, "UPDATE runners SET last_seen_at = ? WHERE id = ?", ts(at), id)
	if err != nil {
		return fmt.Errorf("updating runner last seen: %w", err)
	}
	if !ok {
		return domain.ErrNotFound
	}
	return nil
}
