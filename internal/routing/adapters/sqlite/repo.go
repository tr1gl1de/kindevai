// Package sqlite stores the routing context (KIN-33) in SQLite. Every statement runs on db.Conn, so a use case's
// writes and its audit record commit together.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/routing/app"
	"github.com/tr1gl1de/kindevai/internal/routing/domain"
)

const tsFormat = "2006-01-02T15:04:05.000000000Z07:00"

// Repository implements app.Repo.
type Repository struct{ db *sql.DB }

var _ app.Repo = (*Repository)(nil)

// New returns a repository on d.
func New(d *sql.DB) *Repository { return &Repository{db: d} }

func ts(t time.Time) string { return t.UTC().Format(tsFormat) }

func parseTS(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

// Switches lists the stored switches of a level (projectID "" = global).
func (r *Repository) Switches(ctx context.Context, projectID string) ([]app.Switch, error) {
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, `SELECT agent, enabled, updated_by, updated_at FROM agent_switches
		WHERE project_id = ? ORDER BY agent`, projectID)
	if err != nil {
		return nil, fmt.Errorf("listing CLI switches: %w", err)
	}
	defer rows.Close()
	var out []app.Switch
	for rows.Next() {
		s := app.Switch{ProjectID: projectID}
		var on int
		var at string
		if err := rows.Scan(&s.Agent, &on, &s.UpdatedBy, &at); err != nil {
			return nil, fmt.Errorf("reading a CLI switch: %w", err)
		}
		s.Enabled = on == 1
		if s.UpdatedAt, err = parseTS(at); err != nil {
			return nil, fmt.Errorf("reading a CLI switch: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SetSwitch stores a switch (insert or replace).
func (r *Repository) SetSwitch(ctx context.Context, s app.Switch) error {
	if _, err := db.Conn(ctx, r.db).ExecContext(ctx, `INSERT INTO agent_switches(project_id, agent, enabled, updated_by,
		updated_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT(project_id, agent) DO UPDATE SET enabled = excluded.enabled,
		updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		s.ProjectID, s.Agent, s.Enabled, s.UpdatedBy, ts(s.UpdatedAt)); err != nil {
		return fmt.Errorf("saving the %s switch: %w", s.Agent, err)
	}
	return nil
}

type choiceJSON struct {
	Agent string `json:"agent"`
	Model string `json:"model"`
}

// Bindings lists the stored bindings of a level.
func (r *Repository) Bindings(ctx context.Context, projectID string) ([]app.StoredBinding, error) {
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, `SELECT role, primary_agent, primary_model, fallback_json, strategy,
		updated_by, updated_at FROM agent_role_bindings WHERE project_id = ? ORDER BY role`, projectID)
	if err != nil {
		return nil, fmt.Errorf("listing role bindings: %w", err)
	}
	defer rows.Close()
	var out []app.StoredBinding
	for rows.Next() {
		b := app.StoredBinding{ProjectID: projectID}
		var fb, at string
		if err := rows.Scan(&b.Binding.Role, &b.Binding.Primary.Agent, &b.Binding.Primary.Model, &fb,
			&b.Binding.Strategy, &b.UpdatedBy, &at); err != nil {
			return nil, fmt.Errorf("reading a role binding: %w", err)
		}
		var cs []choiceJSON
		if err := json.Unmarshal([]byte(fb), &cs); err != nil {
			return nil, fmt.Errorf("reading the fallback of role %s: %w", b.Binding.Role, err)
		}
		b.Binding.Fallback = make([]domain.Choice, 0, len(cs))
		for _, c := range cs {
			b.Binding.Fallback = append(b.Binding.Fallback, domain.Choice(c))
		}
		if b.UpdatedAt, err = parseTS(at); err != nil {
			return nil, fmt.Errorf("reading a role binding: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SetBinding stores a binding (insert or replace).
func (r *Repository) SetBinding(ctx context.Context, b app.StoredBinding) error {
	cs := make([]choiceJSON, 0, len(b.Binding.Fallback))
	for _, c := range b.Binding.Fallback {
		cs = append(cs, choiceJSON(c))
	}
	fb, err := json.Marshal(cs)
	if err != nil {
		return fmt.Errorf("encoding the fallback: %w", err)
	}
	if _, err := db.Conn(ctx, r.db).ExecContext(ctx, `INSERT INTO agent_role_bindings(project_id, role, primary_agent,
		primary_model, fallback_json, strategy, updated_by, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_id, role) DO UPDATE SET primary_agent = excluded.primary_agent,
		primary_model = excluded.primary_model, fallback_json = excluded.fallback_json, strategy = excluded.strategy,
		updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		b.ProjectID, b.Binding.Role, b.Binding.Primary.Agent, b.Binding.Primary.Model, string(fb), b.Binding.Strategy,
		b.UpdatedBy, ts(b.UpdatedAt)); err != nil {
		return fmt.Errorf("saving the %s binding: %w", b.Binding.Role, err)
	}
	return nil
}

// DeleteBinding removes a level's binding of role (no-op when none).
func (r *Repository) DeleteBinding(ctx context.Context, projectID string, role domain.Role) error {
	if _, err := db.Conn(ctx, r.db).ExecContext(ctx, `DELETE FROM agent_role_bindings WHERE project_id = ? AND role = ?`,
		projectID, role); err != nil {
		return fmt.Errorf("deleting the %s binding: %w", role, err)
	}
	return nil
}

// SetCooldown stores a runner's CLI cool-down (replacing an earlier one).
func (r *Repository) SetCooldown(ctx context.Context, c app.Cooldown) error {
	if _, err := db.Conn(ctx, r.db).ExecContext(ctx, `INSERT INTO agent_cooldowns(runner_id, agent, until_at, reason,
		cause, strikes, parked) VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(runner_id, agent) DO UPDATE SET
		until_at = excluded.until_at, reason = excluded.reason, cause = excluded.cause, strikes = excluded.strikes,
		parked = excluded.parked`, c.RunnerID, c.Agent, ts(c.Until), c.Reason, c.Cause, c.Strikes, c.Parked); err != nil {
		return fmt.Errorf("saving a CLI cool-down: %w", err)
	}
	return nil
}

const cooldownCols = `runner_id, agent, until_at, reason, cause, strikes, parked`

func scanCooldown(row interface{ Scan(...any) error }) (app.Cooldown, error) {
	var cd app.Cooldown
	var until string
	if err := row.Scan(&cd.RunnerID, &cd.Agent, &until, &cd.Reason, &cd.Cause, &cd.Strikes, &cd.Parked); err != nil {
		return app.Cooldown{}, err
	}
	var err error
	if cd.Until, err = parseTS(until); err != nil {
		return app.Cooldown{}, fmt.Errorf("reading a CLI cool-down: %w", err)
	}
	return cd, nil
}

// Cooldowns lists the cool-downs running at now (parked ones whatever their end), by runner and CLI.
func (r *Repository) Cooldowns(ctx context.Context, now time.Time) ([]app.Cooldown, error) {
	c := db.Conn(ctx, r.db)
	rows, err := c.QueryContext(ctx, `SELECT `+cooldownCols+` FROM agent_cooldowns
		WHERE until_at > ? OR parked = 1 ORDER BY runner_id, agent`, ts(now))
	if err != nil {
		return nil, fmt.Errorf("listing CLI cool-downs: %w", err)
	}
	defer rows.Close()
	var out []app.Cooldown
	for rows.Next() {
		cd, err := scanCooldown(rows)
		if err != nil {
			return nil, fmt.Errorf("reading a CLI cool-down: %w", err)
		}
		out = append(out, cd)
	}
	return out, rows.Err()
}

// Cooldown returns the pair's last cool-down, expired or not.
func (r *Repository) Cooldown(ctx context.Context, runnerID, agent string) (app.Cooldown, bool, error) {
	cd, err := scanCooldown(db.Conn(ctx, r.db).QueryRowContext(ctx, `SELECT `+cooldownCols+` FROM agent_cooldowns
		WHERE runner_id = ? AND agent = ?`, runnerID, agent))
	if errors.Is(err, sql.ErrNoRows) {
		return app.Cooldown{}, false, nil
	}
	if err != nil {
		return app.Cooldown{}, false, fmt.Errorf("loading a CLI cool-down: %w", err)
	}
	return cd, true, nil
}

// DeleteCooldown forgets the pair's cool-down; it reports whether there was one.
func (r *Repository) DeleteCooldown(ctx context.Context, runnerID, agent string) (bool, error) {
	res, err := db.Conn(ctx, r.db).ExecContext(ctx, `DELETE FROM agent_cooldowns WHERE runner_id = ? AND agent = ?`,
		runnerID, agent)
	if err != nil {
		return false, fmt.Errorf("deleting a CLI cool-down: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
