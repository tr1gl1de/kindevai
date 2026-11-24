// Package sqlite stores the alerts context: alerts and notification preferences. Every statement runs on db.Conn,
// so an alert commits together with the outbox messages that announce it.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tr1gl1de/kindevai/internal/alerts/app"
	"github.com/tr1gl1de/kindevai/internal/alerts/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
)

const tsFormat = "2006-01-02T15:04:05.000000000Z07:00"

// Repository implements app.Repository.
type Repository struct{ db *sql.DB }

// New returns a repository on d.
func New(d *sql.DB) *Repository { return &Repository{db: d} }

func (r *Repository) conn(ctx context.Context) db.Executor { return db.Conn(ctx, r.db) }

func ts(t time.Time) string { return t.UTC().Format(tsFormat) }

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullTS(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

const columns = `id, coalesce(project_id, ''), kind, severity, dedup_key, subject, details_json, state, occurrences,
	first_seen_at, last_seen_at, coalesce(acked_by, ''), acked_at, coalesce(closed_by, ''), closed_at, last_event`

type rowScanner interface{ Scan(dest ...any) error }

func parseTS(s string) (time.Time, error) { return time.Parse(tsFormat, s) }

func parseNullTS(s sql.NullString) (*time.Time, error) {
	if !s.Valid {
		return nil, nil
	}
	t, err := parseTS(s.String)
	return &t, err
}

func scan(row rowScanner) (domain.Alert, error) {
	var a domain.Alert
	var kind, sev, state, details, first, last string
	var acked, closed sql.NullString
	if err := row.Scan(&a.ID, &a.ProjectID, &kind, &sev, &a.DedupKey, &a.Subject, &details, &state, &a.Occurrences,
		&first, &last, &a.AckedBy, &acked, &a.ClosedBy, &closed, &a.LastEvent); err != nil {
		return domain.Alert{}, err
	}
	a.Kind, a.Severity, a.State = domain.Kind(kind), domain.Severity(sev), domain.State(state)
	if err := json.Unmarshal([]byte(details), &a.Details); err != nil {
		return domain.Alert{}, fmt.Errorf("alert %s details: %w", a.ID, err)
	}
	var err error
	if a.FirstSeenAt, err = parseTS(first); err != nil {
		return domain.Alert{}, err
	}
	if a.LastSeenAt, err = parseTS(last); err != nil {
		return domain.Alert{}, err
	}
	if a.AckedAt, err = parseNullTS(acked); err != nil {
		return domain.Alert{}, err
	}
	if a.ClosedAt, err = parseNullTS(closed); err != nil {
		return domain.Alert{}, err
	}
	return a, nil
}

func one(row *sql.Row) (domain.Alert, error) {
	a, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Alert{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Alert{}, fmt.Errorf("reading alert: %w", err)
	}
	return a, nil
}

// Active returns the active alert with the dedup key.
func (r *Repository) Active(ctx context.Context, key string) (domain.Alert, error) {
	return one(r.conn(ctx).QueryRowContext(ctx, `SELECT `+columns+` FROM alerts
		WHERE dedup_key = ? AND state IN ('OPEN', 'ACKNOWLEDGED')`, key))
}

// Get returns an alert by id.
func (r *Repository) Get(ctx context.Context, id string) (domain.Alert, error) {
	return one(r.conn(ctx).QueryRowContext(ctx, `SELECT `+columns+` FROM alerts WHERE id = ?`, id))
}

func detailsJSON(a domain.Alert) (string, error) {
	if a.Details == nil {
		return "{}", nil
	}
	b, err := json.Marshal(a.Details)
	return string(b), err
}

// Create inserts a new alert.
func (r *Repository) Create(ctx context.Context, a domain.Alert) error {
	d, err := detailsJSON(a)
	if err != nil {
		return err
	}
	_, err = r.conn(ctx).ExecContext(ctx, `INSERT INTO alerts (id, project_id, kind, severity, dedup_key, subject,
		details_json, state, occurrences, first_seen_at, last_seen_at, acked_by, acked_at, closed_by, closed_at, last_event)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, nullStr(a.ProjectID), string(a.Kind), string(a.Severity), a.DedupKey, a.Subject, d, string(a.State),
		a.Occurrences, ts(a.FirstSeenAt), ts(a.LastSeenAt), nullStr(a.AckedBy), nullTS(a.AckedAt),
		nullStr(a.ClosedBy), nullTS(a.ClosedAt), a.LastEvent)
	if err != nil {
		return fmt.Errorf("inserting alert: %w", err)
	}
	return nil
}

// Save stores an alert's changes.
func (r *Repository) Save(ctx context.Context, a domain.Alert) error {
	d, err := detailsJSON(a)
	if err != nil {
		return err
	}
	res, err := r.conn(ctx).ExecContext(ctx, `UPDATE alerts SET severity = ?, subject = ?, details_json = ?, state = ?,
		occurrences = ?, last_seen_at = ?, acked_by = ?, acked_at = ?, closed_by = ?, closed_at = ?, last_event = ?
		WHERE id = ?`,
		string(a.Severity), a.Subject, d, string(a.State), a.Occurrences, ts(a.LastSeenAt), nullStr(a.AckedBy),
		nullTS(a.AckedAt), nullStr(a.ClosedBy), nullTS(a.ClosedAt), a.LastEvent, a.ID)
	if err != nil {
		return fmt.Errorf("updating alert: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// List returns the alerts of the query, newest activity first.
func (r *Repository) List(ctx context.Context, q app.ListQuery) ([]domain.Alert, error) {
	var where []string
	var args []any
	scope := []string{}
	if q.Platform {
		scope = append(scope, "project_id IS NULL")
	}
	if len(q.ProjectIDs) > 0 {
		scope = append(scope, "project_id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(q.ProjectIDs)), ",")+")")
		for _, id := range q.ProjectIDs {
			args = append(args, id)
		}
	}
	if len(scope) == 0 {
		return []domain.Alert{}, nil
	}
	where = append(where, "("+strings.Join(scope, " OR ")+")")
	if q.ActiveOnly {
		where = append(where, "state IN ('OPEN', 'ACKNOWLEDGED')")
	}
	limit := q.Limit
	if limit <= 0 || limit > app.MaxList {
		limit = app.MaxList
	}
	args = append(args, limit)
	rows, err := r.conn(ctx).QueryContext(ctx, `SELECT `+columns+` FROM alerts WHERE `+strings.Join(where, " AND ")+`
		ORDER BY last_seen_at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("listing alerts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []domain.Alert{}
	for rows.Next() {
		a, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("reading alert: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Preferences returns a user's notification settings, or the defaults.
func (r *Repository) Preferences(ctx context.Context, userID string) (domain.Preferences, error) {
	var lang, disabled string
	err := r.conn(ctx).QueryRowContext(ctx, `SELECT language, disabled_json FROM notification_preferences
		WHERE user_id = ?`, userID).Scan(&lang, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.DefaultPreferences(userID), nil
	}
	if err != nil {
		return domain.Preferences{}, fmt.Errorf("reading preferences: %w", err)
	}
	var types []domain.NotificationType
	if err := json.Unmarshal([]byte(disabled), &types); err != nil {
		return domain.Preferences{}, fmt.Errorf("reading preferences: %w", err)
	}
	return domain.Preferences{UserID: userID, Language: domain.Language(lang), Disabled: types}, nil
}

// SavePreferences replaces a user's notification settings.
func (r *Repository) SavePreferences(ctx context.Context, p domain.Preferences, now time.Time) error {
	types := p.Disabled
	if types == nil {
		types = []domain.NotificationType{}
	}
	b, err := json.Marshal(types)
	if err != nil {
		return err
	}
	_, err = r.conn(ctx).ExecContext(ctx, `INSERT INTO notification_preferences (user_id, language, disabled_json, updated_at)
		VALUES (?, ?, ?, ?) ON CONFLICT (user_id) DO UPDATE SET language = excluded.language,
		disabled_json = excluded.disabled_json, updated_at = excluded.updated_at`,
		p.UserID, string(p.Language), string(b), ts(now))
	if err != nil {
		return fmt.Errorf("saving preferences: %w", err)
	}
	return nil
}
