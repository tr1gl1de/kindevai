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

// InvitationRepository stores one-time invitations.
type InvitationRepository struct{ db *sql.DB }

// NewInvitationRepository returns a repository on d.
func NewInvitationRepository(d *sql.DB) *InvitationRepository { return &InvitationRepository{db: d} }

const invitationCols = "id, token_hash, project_id, role, created_by, created_at, expires_at, consumed_at, consumed_by, revoked_at"

func ts(t time.Time) string { return t.UTC().Format(tsFormat) }

func parseTS(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

func scanInvitation(row rowScanner) (domain.Invitation, error) {
	var inv domain.Invitation
	var created, expires string
	var consumed, consumedBy, revoked sql.NullString
	err := row.Scan(&inv.ID, &inv.TokenHash, &inv.ProjectID, &inv.Role, &inv.CreatedBy, &created, &expires,
		&consumed, &consumedBy, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Invitation{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Invitation{}, fmt.Errorf("reading invitation: %w", err)
	}
	if inv.CreatedAt, err = parseTS(created); err != nil {
		return domain.Invitation{}, fmt.Errorf("reading invitation: %w", err)
	}
	if inv.ExpiresAt, err = parseTS(expires); err != nil {
		return domain.Invitation{}, fmt.Errorf("reading invitation: %w", err)
	}
	for _, f := range []struct {
		src sql.NullString
		dst **time.Time
	}{{consumed, &inv.ConsumedAt}, {revoked, &inv.RevokedAt}} {
		if f.src.Valid {
			t, err := parseTS(f.src.String)
			if err != nil {
				return domain.Invitation{}, fmt.Errorf("reading invitation: %w", err)
			}
			*f.dst = &t
		}
	}
	inv.ConsumedBy = consumedBy.String
	return inv, nil
}

// Create inserts an invitation.
func (r *InvitationRepository) Create(ctx context.Context, inv domain.Invitation) error {
	_, err := db.Conn(ctx, r.db).ExecContext(ctx, `INSERT INTO invitations(id, token_hash, project_id, role, created_by, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, inv.ID, inv.TokenHash, inv.ProjectID, inv.Role, inv.CreatedBy, ts(inv.CreatedAt), ts(inv.ExpiresAt))
	if db.IsForeignKeyViolation(err) {
		return domain.ErrNotFound // the project was deleted after the lookup
	}
	if err != nil {
		return fmt.Errorf("creating invitation: %w", err)
	}
	return nil
}

// List returns up to limit invitations, newest first.
func (r *InvitationRepository) List(ctx context.Context, limit int) ([]domain.Invitation, error) {
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, "SELECT "+invitationCols+" FROM invitations ORDER BY created_at DESC, id LIMIT ?", limit)
	if err != nil {
		return nil, fmt.Errorf("listing invitations: %w", err)
	}
	defer rows.Close()
	var out []domain.Invitation
	for rows.Next() {
		inv, err := scanInvitation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// ByID loads an invitation.
func (r *InvitationRepository) ByID(ctx context.Context, id string) (domain.Invitation, error) {
	return scanInvitation(db.Conn(ctx, r.db).QueryRowContext(ctx, "SELECT "+invitationCols+" FROM invitations WHERE id = ?", id))
}

// ByTokenHash loads the invitation for a presented token.
func (r *InvitationRepository) ByTokenHash(ctx context.Context, hash []byte) (domain.Invitation, error) {
	return scanInvitation(db.Conn(ctx, r.db).QueryRowContext(ctx, "SELECT "+invitationCols+" FROM invitations WHERE token_hash = ?", hash))
}

const pendingAt = "consumed_at IS NULL AND revoked_at IS NULL AND expires_at > ?"

func (r *InvitationRepository) update(ctx context.Context, q string, args ...any) (bool, error) {
	res, err := db.Conn(ctx, r.db).ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// MarkConsumed records the acceptance if the invitation is still pending at now.
func (r *InvitationRepository) MarkConsumed(ctx context.Context, id, userID string, now time.Time) (bool, error) {
	return r.update(ctx, "UPDATE invitations SET consumed_at = ?, consumed_by = ? WHERE id = ? AND "+pendingAt,
		ts(now), userID, id, ts(now))
}

// Revoke withdraws the invitation if it is still pending at now.
func (r *InvitationRepository) Revoke(ctx context.Context, id string, now time.Time) (bool, error) {
	return r.update(ctx, "UPDATE invitations SET revoked_at = ? WHERE id = ? AND "+pendingAt, ts(now), id, ts(now))
}
