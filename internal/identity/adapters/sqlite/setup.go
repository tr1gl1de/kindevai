// Package sqlite implements the identity context's repositories on the Control Plane SQLite database.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/tr1gl1de/kindevai/internal/identity/app"
	"github.com/tr1gl1de/kindevai/internal/identity/domain"
)

const (
	metaSetupCompleted   = "setup_completed_at"
	metaSetupTokenHash   = "setup_token_hash"
	metaSetupTokenExpiry = "setup_token_expires_at"
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

type setupTx struct{ tx *sql.Tx }

func (t *setupTx) meta(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := t.tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading %s: %w", key, err)
	}
	return v, true, nil
}

func (t *setupTx) SetupState(ctx context.Context) (domain.SetupState, error) {
	_, completed, err := t.meta(ctx, metaSetupCompleted)
	if err != nil || completed {
		return domain.SetupState{Completed: completed}, err
	}
	h, hasHash, err := t.meta(ctx, metaSetupTokenHash)
	if err != nil || !hasHash {
		return domain.SetupState{}, err
	}
	exp, _, err := t.meta(ctx, metaSetupTokenExpiry)
	if err != nil {
		return domain.SetupState{}, err
	}
	hash, err := hex.DecodeString(h)
	if err != nil {
		return domain.SetupState{}, fmt.Errorf("decoding setup token hash: %w", err)
	}
	at, err := time.Parse(time.RFC3339Nano, exp)
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
