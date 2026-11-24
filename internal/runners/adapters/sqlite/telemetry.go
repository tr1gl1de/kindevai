package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

// Runner telemetry and storage metrics (KIN-20); implements app.TelemetryRepo.

// RecordHeartbeat stores the heartbeat (and last_seen_at), clears the unreachable mark and returns it.
func (r *Repository) RecordHeartbeat(ctx context.Context, runnerID string, at time.Time) (*time.Time, error) {
	c := db.Conn(ctx, r.db)
	var since sql.NullString
	err := c.QueryRowContext(ctx, "SELECT unreachable_since FROM runner_telemetry WHERE runner_id = ?", runnerID).Scan(&since)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("reading runner telemetry: %w", err)
	}
	prev, err := parseNullTS(since)
	if err != nil {
		return nil, fmt.Errorf("reading runner telemetry: %w", err)
	}
	if _, err := c.ExecContext(ctx, `INSERT INTO runner_telemetry(runner_id, last_heartbeat_at) VALUES (?, ?)
		ON CONFLICT(runner_id) DO UPDATE SET last_heartbeat_at = excluded.last_heartbeat_at, unreachable_since = NULL`,
		runnerID, ts(at)); err != nil {
		if db.IsForeignKeyViolation(err) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("recording heartbeat: %w", err)
	}
	if err := r.MarkSeen(ctx, runnerID, at); err != nil {
		return nil, err
	}
	return prev, nil
}

// MarkUnreachable marks approved runners whose last heartbeat is at or before cutoff.
func (r *Repository) MarkUnreachable(ctx context.Context, cutoff, now time.Time) ([]string, error) {
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, `UPDATE runner_telemetry SET unreachable_since = ?
		WHERE unreachable_since IS NULL AND last_heartbeat_at <= ?
		  AND runner_id IN (SELECT id FROM runners WHERE status = 'approved')
		RETURNING runner_id`, ts(now), ts(cutoff))
	if err != nil {
		return nil, fmt.Errorf("marking runners unreachable: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("marking runners unreachable: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

type capsJSON struct {
	OS           string     `json:"os"`
	Arch         string     `json:"arch"`
	WSL          bool       `json:"wsl"`
	StrictEgress bool       `json:"strictEgress"`
	Tools        []toolJSON `json:"tools"`
}

type toolJSON struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// SaveCapabilities stores the runner's last capability report.
func (r *Repository) SaveCapabilities(ctx context.Context, runnerID string, c domain.Capabilities) error {
	j := capsJSON{OS: c.OS, Arch: c.Arch, WSL: c.WSL, StrictEgress: c.StrictEgress, Tools: make([]toolJSON, 0, len(c.Tools))}
	for _, t := range c.Tools {
		j.Tools = append(j.Tools, toolJSON(t))
	}
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	_, err = db.Conn(ctx, r.db).ExecContext(ctx, `INSERT INTO runner_telemetry(runner_id, capabilities, capabilities_at)
		VALUES (?, ?, ?) ON CONFLICT(runner_id) DO UPDATE SET capabilities = excluded.capabilities,
		capabilities_at = excluded.capabilities_at`, runnerID, string(b), ts(c.ReportedAt))
	if db.IsForeignKeyViolation(err) {
		return domain.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("saving runner capabilities: %w", err)
	}
	return nil
}

// Telemetry loads what the Control Plane last heard from every runner.
func (r *Repository) Telemetry(ctx context.Context) (map[string]domain.RunnerTelemetry, error) {
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx,
		"SELECT runner_id, last_heartbeat_at, unreachable_since, capabilities, capabilities_at FROM runner_telemetry")
	if err != nil {
		return nil, fmt.Errorf("loading runner telemetry: %w", err)
	}
	defer rows.Close()
	out := map[string]domain.RunnerTelemetry{}
	for rows.Next() {
		var id string
		var hb, down, caps, capsAt sql.NullString
		if err := rows.Scan(&id, &hb, &down, &caps, &capsAt); err != nil {
			return nil, fmt.Errorf("loading runner telemetry: %w", err)
		}
		var x domain.RunnerTelemetry
		if x.LastHeartbeatAt, err = parseNullTS(hb); err == nil {
			x.UnreachableSince, err = parseNullTS(down)
		}
		if err == nil && caps.Valid {
			x.Capabilities, err = parseCaps(caps.String, capsAt)
		}
		if err != nil {
			return nil, fmt.Errorf("loading runner telemetry: %w", err)
		}
		out[id] = x
	}
	return out, rows.Err()
}

func parseCaps(s string, at sql.NullString) (*domain.Capabilities, error) {
	var j capsJSON
	if err := json.Unmarshal([]byte(s), &j); err != nil {
		return nil, err
	}
	c := domain.Capabilities{OS: j.OS, Arch: j.Arch, WSL: j.WSL, StrictEgress: j.StrictEgress, Tools: make([]domain.Tool, 0, len(j.Tools))}
	for _, t := range j.Tools {
		c.Tools = append(c.Tools, domain.Tool(t))
	}
	if at.Valid {
		t, err := parseTS(at.String)
		if err != nil {
			return nil, err
		}
		c.ReportedAt = t
	}
	return &c, nil
}

const volumeCols = "machine_id, role, volume_id, path, total_bytes, available_bytes, free_bytes, reported_at"

func (r *Repository) volumes(ctx context.Context, where string, args ...any) (map[string][]domain.Volume, error) {
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, "SELECT "+volumeCols+" FROM storage_volumes "+where+
		" ORDER BY machine_id, role", args...)
	if err != nil {
		return nil, fmt.Errorf("loading storage volumes: %w", err)
	}
	defer rows.Close()
	out := map[string][]domain.Volume{}
	for rows.Next() {
		var m, at string
		var v domain.Volume
		if err := rows.Scan(&m, &v.Role, &v.ID, &v.Path, &v.TotalBytes, &v.AvailableBytes, &v.FreeBytes, &at); err != nil {
			return nil, fmt.Errorf("loading storage volumes: %w", err)
		}
		if v.ReportedAt, err = parseTS(at); err != nil {
			return nil, fmt.Errorf("loading storage volumes: %w", err)
		}
		out[m] = append(out[m], v)
	}
	return out, rows.Err()
}

// Volumes loads every machine's current volumes.
func (r *Repository) Volumes(ctx context.Context) (map[string][]domain.Volume, error) {
	return r.volumes(ctx, "")
}

// ReplaceVolumes replaces a machine's current volumes, records the hour's history and returns the previous set.
func (r *Repository) ReplaceVolumes(ctx context.Context, machineID string, vs []domain.Volume) ([]domain.Volume, error) {
	prev, err := r.volumes(ctx, "WHERE machine_id = ?", machineID)
	if err != nil {
		return nil, err
	}
	c := db.Conn(ctx, r.db)
	if _, err := c.ExecContext(ctx, "DELETE FROM storage_volumes WHERE machine_id = ?", machineID); err != nil {
		return nil, fmt.Errorf("replacing storage volumes: %w", err)
	}
	for _, v := range vs {
		if _, err := c.ExecContext(ctx, "INSERT INTO storage_volumes("+volumeCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			machineID, v.Role, v.ID, v.Path, v.TotalBytes, v.AvailableBytes, v.FreeBytes, ts(v.ReportedAt)); err != nil {
			return nil, fmt.Errorf("replacing storage volumes: %w", err)
		}
		if _, err := c.ExecContext(ctx, `INSERT INTO storage_history(machine_id, role, hour, total_bytes, used_bytes)
			VALUES (?, ?, ?, ?, ?) ON CONFLICT(machine_id, role, hour) DO UPDATE SET total_bytes = excluded.total_bytes,
			used_bytes = excluded.used_bytes`, machineID, v.Role, hourOf(v.ReportedAt), v.TotalBytes, v.UsedBytes()); err != nil {
			return nil, fmt.Errorf("recording storage history: %w", err)
		}
	}
	return prev[machineID], nil
}

func hourOf(t time.Time) string { return t.UTC().Truncate(time.Hour).Format(time.RFC3339) }

// Samples returns the hourly history since the given time, oldest first.
func (r *Repository) Samples(ctx context.Context, since time.Time) (map[string]map[string][]domain.Sample, error) {
	return r.samples(ctx, "hour >= ?", hourOf(since))
}

// MachineSamples returns one machine's hourly history since the given time: volume role → samples, oldest first.
func (r *Repository) MachineSamples(ctx context.Context, machineID string, since time.Time) (map[string][]domain.Sample, error) {
	all, err := r.samples(ctx, "machine_id = ? AND hour >= ?", machineID, hourOf(since))
	if err != nil {
		return nil, err
	}
	if all[machineID] == nil {
		return map[string][]domain.Sample{}, nil
	}
	return all[machineID], nil
}

func (r *Repository) samples(ctx context.Context, where string, args ...any) (map[string]map[string][]domain.Sample, error) {
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, `SELECT machine_id, role, hour, total_bytes, used_bytes
		FROM storage_history WHERE `+where+` ORDER BY machine_id, role, hour`, args...)
	if err != nil {
		return nil, fmt.Errorf("loading storage history: %w", err)
	}
	defer rows.Close()
	out := map[string]map[string][]domain.Sample{}
	for rows.Next() {
		var m, role, hour string
		var s domain.Sample
		if err := rows.Scan(&m, &role, &hour, &s.TotalBytes, &s.UsedBytes); err != nil {
			return nil, fmt.Errorf("loading storage history: %w", err)
		}
		if s.Hour, err = time.Parse(time.RFC3339, hour); err != nil {
			return nil, fmt.Errorf("loading storage history: %w", err)
		}
		if out[m] == nil {
			out[m] = map[string][]domain.Sample{}
		}
		out[m][role] = append(out[m][role], s)
	}
	return out, rows.Err()
}

// PurgeSamples deletes history older than before, then the oldest rows beyond maxRows.
func (r *Repository) PurgeSamples(ctx context.Context, before time.Time, maxRows int) (int64, error) {
	c := db.Conn(ctx, r.db)
	res, err := c.ExecContext(ctx, "DELETE FROM storage_history WHERE hour < ?", hourOf(before))
	if err != nil {
		return 0, fmt.Errorf("purging storage history: %w", err)
	}
	n, _ := res.RowsAffected()
	res, err = c.ExecContext(ctx, `DELETE FROM storage_history WHERE rowid IN (SELECT rowid FROM storage_history
		ORDER BY hour DESC, machine_id, role LIMIT -1 OFFSET ?)`, maxRows)
	if err != nil {
		return n, fmt.Errorf("purging storage history: %w", err)
	}
	m, _ := res.RowsAffected()
	return n + m, nil
}

// ReplaceSpaceConsumers stores a machine's latest space-consumer measurement, replacing the previous one.
func (r *Repository) ReplaceSpaceConsumers(ctx context.Context, machineID string, m domain.SpaceConsumers) error {
	c := db.Conn(ctx, r.db)
	if _, err := c.ExecContext(ctx, "DELETE FROM storage_consumers WHERE machine_id = ?", machineID); err != nil {
		return fmt.Errorf("replacing space consumers: %w", err)
	}
	for _, x := range m.Items {
		if _, err := c.ExecContext(ctx, `INSERT INTO storage_consumers(machine_id, category, bytes, entries, partial,
			measured_at) VALUES (?, ?, ?, ?, ?, ?)`, machineID, x.Category, x.Bytes, x.Entries, x.Partial, ts(m.MeasuredAt)); err != nil {
			return fmt.Errorf("replacing space consumers: %w", err)
		}
	}
	return nil
}

// SpaceConsumers loads a machine's latest measurement; nil when it was never measured (or measured nothing).
func (r *Repository) SpaceConsumers(ctx context.Context, machineID string) (*domain.SpaceConsumers, error) {
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, `SELECT category, bytes, entries, partial, measured_at
		FROM storage_consumers WHERE machine_id = ?`, machineID)
	if err != nil {
		return nil, fmt.Errorf("loading space consumers: %w", err)
	}
	defer rows.Close()
	var items []domain.SpaceConsumer
	var at time.Time
	for rows.Next() {
		var x domain.SpaceConsumer
		var measured string
		if err := rows.Scan(&x.Category, &x.Bytes, &x.Entries, &x.Partial, &measured); err != nil {
			return nil, fmt.Errorf("loading space consumers: %w", err)
		}
		if at, err = parseTS(measured); err != nil {
			return nil, fmt.Errorf("loading space consumers: %w", err)
		}
		items = append(items, x)
	}
	if err := rows.Err(); err != nil || items == nil {
		return nil, err
	}
	m, err := domain.NewSpaceConsumers(items, at)
	if err != nil {
		return nil, fmt.Errorf("loading space consumers: %w", err)
	}
	return &m, nil
}
