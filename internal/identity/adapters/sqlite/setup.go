// Package sqlite implements the identity context's repositories on the Control Plane SQLite database.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/tr1gl1de/kindevai/internal/identity/app"
	"github.com/tr1gl1de/kindevai/internal/identity/domain"
)

const (
	metaSetupCompleted   = "setup_completed_at"
	metaSetupTokenHash   = "setup_token_hash"
	metaSetupTokenExpiry = "setup_token_expires_at" //nolint:gosec // G101: a meta key name, not a credential
	settingPlatformName  = "platform.name"
)

// SetupRepository stores setup state in meta and the owner in users.
type SetupRepository struct{ db *sql.DB }

// NewSetupRepository returns a repository on d.
func NewSetupRepository(d *sql.DB) *SetupRepository { return &SetupRepository{db: d} }

// InTx runs fn in an immediate (write-locked) transaction.
func (r *SetupRepository) InTx(ctx context.Context, fn func(app.SetupTx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if err := fn(&setupTx{tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// State reads the setup state without a write transaction.
func (r *SetupRepository) State(ctx context.Context) (domain.SetupState, error) {
	return readState(ctx, r.db)
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

type setupTx struct{ tx *sql.Tx }

func (t *setupTx) SetupState(ctx context.Context) (domain.SetupState, error) {
	return readState(ctx, t.tx)
}

// readState loads all setup keys with one statement, so a concurrent completion cannot be seen half-applied.
func readState(ctx context.Context, q querier) (domain.SetupState, error) {
	rows, err := q.QueryContext(ctx, "SELECT key, value FROM meta WHERE key IN (?, ?, ?)",
		metaSetupCompleted, metaSetupTokenHash, metaSetupTokenExpiry)
	if err != nil {
		return domain.SetupState{}, fmt.Errorf("reading setup state: %w", err)
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return domain.SetupState{}, fmt.Errorf("reading setup state: %w", err)
		}
		m[k] = v
	}
	if err := rows.Err(); err != nil {
		return domain.SetupState{}, fmt.Errorf("reading setup state: %w", err)
	}
	if _, done := m[metaSetupCompleted]; done {
		return domain.SetupState{Completed: true}, nil
	}
	h, ok := m[metaSetupTokenHash]
	if !ok {
		return domain.SetupState{}, nil
	}
	hash, err := hex.DecodeString(h)
	if err != nil {
		return domain.SetupState{}, fmt.Errorf("decoding setup token hash: %w", err)
	}
	at, err := time.Parse(time.RFC3339Nano, m[metaSetupTokenExpiry])
	if err != nil {
		return domain.SetupState{}, fmt.Errorf("decoding setup token expiry: %w", err)
	}
	return domain.SetupState{Token: &domain.SetupToken{Hash: hash, ExpiresAt: at}}, nil
}

func (t *setupTx) setMeta(ctx context.Context, key, value string) error {
	if _, err := t.tx.ExecContext(ctx,
		"INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, value); err != nil {
		return fmt.Errorf("writing %s: %w", key, err)
	}
	return nil
}

func (t *setupTx) SaveSetupToken(ctx context.Context, tok domain.SetupToken) error {
	if err := t.setMeta(ctx, metaSetupTokenHash, hex.EncodeToString(tok.Hash)); err != nil {
		return err
	}
	return t.setMeta(ctx, metaSetupTokenExpiry, tok.ExpiresAt.UTC().Format(time.RFC3339Nano))
}

func (t *setupTx) CompleteSetup(ctx context.Context, owner domain.User, name domain.PlatformName, at time.Time) error {
	ts := at.UTC().Format(time.RFC3339Nano)
	if _, err := t.tx.ExecContext(ctx,
		"INSERT INTO users(id, login, password_hash, platform_role, created_at) VALUES (?, ?, ?, ?, ?)",
		owner.ID, string(owner.Login), owner.PasswordHash, string(owner.Role), owner.CreatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("creating owner: %w", err)
	}
	if _, err := t.tx.ExecContext(ctx,
		"INSERT INTO settings(key, value, encrypted, updated_at) VALUES (?, ?, 0, ?) "+
			"ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at",
		settingPlatformName, []byte(name), ts); err != nil {
		return fmt.Errorf("saving platform name: %w", err)
	}
	if _, err := t.tx.ExecContext(ctx, "DELETE FROM meta WHERE key IN (?, ?)", metaSetupTokenHash, metaSetupTokenExpiry); err != nil {
		return fmt.Errorf("deleting setup token: %w", err)
	}
	return t.setMeta(ctx, metaSetupCompleted, ts)
}
