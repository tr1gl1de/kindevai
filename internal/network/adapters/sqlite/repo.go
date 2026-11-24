// Package sqlite stores the network context in SQLite. Every statement runs on db.Conn, so use cases make their
// writes, the runner commands and the audit record atomic.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tr1gl1de/kindevai/internal/network/app"
	"github.com/tr1gl1de/kindevai/internal/network/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
)

const tsFormat = "2006-01-02T15:04:05.000000000Z07:00"

// Repository implements app.Repo.
type Repository struct{ db *sql.DB }

var _ app.Repo = (*Repository)(nil)

// New returns a repository on d.
func New(d *sql.DB) *Repository { return &Repository{db: d} }

func ts(t time.Time) string { return t.UTC().Format(tsFormat) }

func parseTS(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

type scanner interface{ Scan(dest ...any) error }

const profileCols = "id, name, proxy_scheme, proxy_host, proxy_port, proxy_username, enforcement, dns_policy, fail_closed, " +
	"expected_ipv4, expected_ipv6, allow_json, diagnostic_url, created_at, updated_at"

func scanProfile(row scanner) (domain.Profile, error) {
	var p domain.Profile
	var scheme, host, user, allow, created, updated string
	var port int
	var failClosed int
	if err := row.Scan(&p.ID, &p.Name, &scheme, &host, &port, &user, &p.Enforcement, &p.DNS, &failClosed,
		&p.ExpectedIPv4, &p.ExpectedIPv6, &allow, &p.DiagnosticURL, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return p, domain.ErrNotFound
		}
		return p, err
	}
	p.FailClosed = failClosed == 1
	if scheme != "" {
		p.Proxy = &domain.Proxy{Scheme: domain.Scheme(scheme), Host: host, Port: port, Username: user}
	}
	if err := json.Unmarshal([]byte(allow), &p.Allow); err != nil {
		return p, fmt.Errorf("decoding allowlist of profile %s: %w", p.ID, err)
	}
	if len(p.Allow) == 0 {
		p.Allow = nil
	}
	var err error
	if p.CreatedAt, err = parseTS(created); err != nil {
		return p, err
	}
	p.UpdatedAt, err = parseTS(updated)
	return p, err
}

func profileArgs(p domain.Profile) ([]any, error) {
	allow := p.Allow
	if allow == nil {
		allow = []domain.Rule{}
	}
	aj, err := json.Marshal(allow)
	if err != nil {
		return nil, err
	}
	var scheme, host, user string
	var port int
	if p.Proxy != nil {
		scheme, host, port, user = string(p.Proxy.Scheme), p.Proxy.Host, p.Proxy.Port, p.Proxy.Username
	}
	fc := 0
	if p.FailClosed {
		fc = 1
	}
	return []any{p.ID, p.Name, scheme, host, port, user, string(p.Enforcement), string(p.DNS), fc, p.ExpectedIPv4,
		p.ExpectedIPv6, string(aj), p.DiagnosticURL, ts(p.CreatedAt), ts(p.UpdatedAt)}, nil
}

// Profiles lists all profiles by name.
func (r *Repository) Profiles(ctx context.Context) ([]domain.Profile, error) {
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, "SELECT "+profileCols+" FROM network_profiles ORDER BY name, id")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []domain.Profile
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Profile loads one profile.
func (r *Repository) Profile(ctx context.Context, id string) (domain.Profile, error) {
	return scanProfile(db.Conn(ctx, r.db).QueryRowContext(ctx, "SELECT "+profileCols+" FROM network_profiles WHERE id = ?", id))
}

func nameTaken(err error) error {
	if db.IsUniqueViolation(err) {
		return domain.ErrNameTaken
	}
	return err
}

// CreateProfile inserts a profile.
func (r *Repository) CreateProfile(ctx context.Context, p domain.Profile) error {
	args, err := profileArgs(p)
	if err != nil {
		return err
	}
	_, err = db.Conn(ctx, r.db).ExecContext(ctx, "INSERT INTO network_profiles ("+profileCols+
		") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", args...)
	return nameTaken(err)
}

// SaveProfile updates a profile.
func (r *Repository) SaveProfile(ctx context.Context, p domain.Profile) error {
	args, err := profileArgs(p)
	if err != nil {
		return err
	}
	res, err := db.Conn(ctx, r.db).ExecContext(ctx, `UPDATE network_profiles SET name = ?, proxy_scheme = ?, proxy_host = ?,
		proxy_port = ?, proxy_username = ?, enforcement = ?, dns_policy = ?, fail_closed = ?, expected_ipv4 = ?,
		expected_ipv6 = ?, allow_json = ?, diagnostic_url = ?, created_at = ?, updated_at = ? WHERE id = ?`,
		append(args[1:], args[0])...)
	if err != nil {
		return nameTaken(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// DeleteProfile deletes a profile (the schema refuses one that is still assigned).
func (r *Repository) DeleteProfile(ctx context.Context, id string) error {
	_, err := db.Conn(ctx, r.db).ExecContext(ctx, "DELETE FROM network_profiles WHERE id = ?", id)
	return err
}

// Assignments lists all assignments.
func (r *Repository) Assignments(ctx context.Context) ([]domain.Assignment, error) {
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx,
		"SELECT level, target, profile_id, updated_at FROM network_assignments ORDER BY level, target")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []domain.Assignment
	for rows.Next() {
		var a domain.Assignment
		var at string
		if err := rows.Scan(&a.Scope.Level, &a.Scope.Target, &a.ProfileID, &at); err != nil {
			return nil, err
		}
		if a.UpdatedAt, err = parseTS(at); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetAssignment inserts or replaces the assignment of a scope.
func (r *Repository) SetAssignment(ctx context.Context, a domain.Assignment) error {
	_, err := db.Conn(ctx, r.db).ExecContext(ctx, `INSERT INTO network_assignments (level, target, profile_id, updated_at)
		VALUES (?, ?, ?, ?) ON CONFLICT (level, target) DO UPDATE SET profile_id = excluded.profile_id, updated_at = excluded.updated_at`,
		string(a.Scope.Level), a.Scope.Target, a.ProfileID, ts(a.UpdatedAt))
	return err
}

// DeleteAssignment removes a scope's assignment (no error when there is none).
func (r *Repository) DeleteAssignment(ctx context.Context, s domain.Scope) error {
	_, err := db.Conn(ctx, r.db).ExecContext(ctx, "DELETE FROM network_assignments WHERE level = ? AND target = ?",
		string(s.Level), s.Target)
	return err
}

// NextRevision increments the configuration revision.
func (r *Repository) NextRevision(ctx context.Context) (int64, error) {
	var rev int64
	err := db.Conn(ctx, r.db).QueryRowContext(ctx,
		"UPDATE network_state SET revision = revision + 1 WHERE id = 1 RETURNING revision").Scan(&rev)
	return rev, err
}

// Revision reads the configuration revision.
func (r *Repository) Revision(ctx context.Context) (int64, error) {
	var rev int64
	err := db.Conn(ctx, r.db).QueryRowContext(ctx, "SELECT revision FROM network_state WHERE id = 1").Scan(&rev)
	return rev, err
}

const testCols = "id, runner_id, profile_id, requested_by, requested_at, expected_ipv4, expected_ipv6, status, code, " +
	"detail, observed_ipv4, observed_ipv6, checks_json, duration_ms, completed_at"

func scanTest(row scanner) (domain.Test, error) {
	var t domain.Test
	var requested, checks string
	var completed sql.NullString
	if err := row.Scan(&t.ID, &t.RunnerID, &t.ProfileID, &t.RequestedBy, &requested, &t.Expected.IPv4, &t.Expected.IPv6,
		&t.Status, &t.Code, &t.Detail, &t.ObservedIPv4, &t.ObservedIPv6, &checks, &t.DurationMs, &completed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return t, domain.ErrNotFound
		}
		return t, err
	}
	var err error
	if t.RequestedAt, err = parseTS(requested); err != nil {
		return t, err
	}
	if completed.Valid {
		c, err := parseTS(completed.String)
		if err != nil {
			return t, err
		}
		t.CompletedAt = &c
	}
	if err := json.Unmarshal([]byte(checks), &t.Checks); err != nil {
		return t, fmt.Errorf("decoding checks of test %s: %w", t.ID, err)
	}
	if len(t.Checks) == 0 {
		t.Checks = nil
	}
	return t, nil
}

func testArgs(t domain.Test) ([]any, error) {
	checks := t.Checks
	if checks == nil {
		checks = []domain.Check{}
	}
	cj, err := json.Marshal(checks)
	if err != nil {
		return nil, err
	}
	var completed any
	if t.CompletedAt != nil {
		completed = ts(*t.CompletedAt)
	}
	return []any{t.ID, t.RunnerID, t.ProfileID, t.RequestedBy, ts(t.RequestedAt), t.Expected.IPv4, t.Expected.IPv6,
		string(t.Status), t.Code, t.Detail, t.ObservedIPv4, t.ObservedIPv6, string(cj), t.DurationMs, completed}, nil
}

// testRetention bounds the stored tests: older ones beyond the newest testKeep are purged on insert.
const testKeep = 500

// CreateTest inserts a test and purges the oldest beyond testKeep.
func (r *Repository) CreateTest(ctx context.Context, t domain.Test) error {
	args, err := testArgs(t)
	if err != nil {
		return err
	}
	c := db.Conn(ctx, r.db)
	if _, err := c.ExecContext(ctx, "INSERT INTO network_tests ("+testCols+
		") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", args...); err != nil {
		return err
	}
	_, err = c.ExecContext(ctx, `DELETE FROM network_tests WHERE id IN (
		SELECT id FROM network_tests ORDER BY requested_at DESC, id DESC LIMIT -1 OFFSET ?)`, testKeep)
	return err
}

// Test loads one test.
func (r *Repository) Test(ctx context.Context, id string) (domain.Test, error) {
	return scanTest(db.Conn(ctx, r.db).QueryRowContext(ctx, "SELECT "+testCols+" FROM network_tests WHERE id = ?", id))
}

// SaveTest updates a test's result.
func (r *Repository) SaveTest(ctx context.Context, t domain.Test) error {
	args, err := testArgs(t)
	if err != nil {
		return err
	}
	_, err = db.Conn(ctx, r.db).ExecContext(ctx, `UPDATE network_tests SET status = ?, code = ?, detail = ?, observed_ipv4 = ?,
		observed_ipv6 = ?, checks_json = ?, duration_ms = ?, completed_at = ? WHERE id = ?`,
		args[7], args[8], args[9], args[10], args[11], args[12], args[13], args[14], t.ID)
	return err
}

// Tests lists the newest tests, of runnerID or all.
func (r *Repository) Tests(ctx context.Context, runnerID string, limit int) ([]domain.Test, error) {
	q, args := "SELECT "+testCols+" FROM network_tests ORDER BY requested_at DESC, id DESC LIMIT ?", []any{limit}
	if runnerID != "" {
		q, args = "SELECT "+testCols+" FROM network_tests WHERE runner_id = ? ORDER BY requested_at DESC, id DESC LIMIT ?",
			[]any{runnerID, limit}
	}
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []domain.Test
	for rows.Next() {
		t, err := scanTest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
