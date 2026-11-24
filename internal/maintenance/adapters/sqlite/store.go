// Package sqlite stores the maintenance mode in the installation's meta table (one JSON row).
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tr1gl1de/kindevai/internal/maintenance/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
)

const metaKey = "maintenance"

// Store is the maintenance mode row.
type Store struct{ db *sql.DB }

// New returns a store on d.
func New(d *sql.DB) *Store { return &Store{db: d} }

type row struct {
	Enabled bool      `json:"enabled"`
	Reason  string    `json:"reason,omitempty"`
	Since   time.Time `json:"since,omitzero"`
	By      string    `json:"by,omitempty"`
}

// Load returns the stored mode (disabled when nothing is stored).
func (s *Store) Load(ctx context.Context) (domain.Mode, error) {
	var v string
	err := db.Conn(ctx, s.db).QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Disabled(), nil
	}
	if err != nil {
		return domain.Mode{}, fmt.Errorf("reading maintenance mode: %w", err)
	}
	var r row
	if err := json.Unmarshal([]byte(v), &r); err != nil {
		return domain.Mode{}, fmt.Errorf("maintenance mode row is corrupt: %w", err)
	}
	return domain.Mode{Enabled: r.Enabled, Reason: r.Reason, Since: r.Since, By: r.By}, nil
}

// Save stores m.
func (s *Store) Save(ctx context.Context, m domain.Mode) error {
	b, err := json.Marshal(row{Enabled: m.Enabled, Reason: m.Reason, Since: m.Since, By: m.By})
	if err != nil {
		return err
	}
	if _, err := db.Conn(ctx, s.db).ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		metaKey, string(b)); err != nil {
		return fmt.Errorf("saving maintenance mode: %w", err)
	}
	return nil
}
