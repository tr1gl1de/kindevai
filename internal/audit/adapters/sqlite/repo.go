// Package sqlite stores audit events in the Control Plane SQLite database (table audit_events, append-only).
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/tr1gl1de/kindevai/internal/audit/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
)

// Repository appends and reads audit events. It has no update or delete on purpose.
type Repository struct{ db *sql.DB }

// New returns a repository on d.
func New(d *sql.DB) *Repository { return &Repository{db: d} }

// Append inserts e on the caller's transaction, if any, and returns its id.
func (r *Repository) Append(ctx context.Context, e domain.Event) (int64, error) {
	details := []byte("{}")
	if len(e.Details) > 0 {
		b, err := json.Marshal(e.Details)
		if err != nil {
			return 0, fmt.Errorf("encoding details: %w", err)
		}
		details = b
	}
	var project any
	if e.ProjectID != "" {
		project = e.ProjectID
	}
	res, err := db.Conn(ctx, r.db).ExecContext(ctx, `INSERT INTO audit_events
		(occurred_at, actor_type, actor_id, actor_label, kind, project_id, subject, details)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		e.At.UnixMilli(), string(e.Actor.Type), e.Actor.ID, e.Actor.Label, string(e.Kind), project, e.Subject, string(details))
	if err != nil {
		return 0, fmt.Errorf("inserting audit event: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("reading audit event id: %w", err)
	}
	return id, nil
}

// Query returns at most f.Limit events matching f, newest first.
func (r *Repository) Query(ctx context.Context, f domain.Filter) ([]domain.Event, error) {
	var where []string
	var args []any
	add := func(cond string, a ...any) { where = append(where, cond); args = append(args, a...) }
	if f.Actor != "" {
		add("(actor_id = ? OR actor_label = ?)", f.Actor, f.Actor)
	}
	if f.ProjectID != "" {
		add("project_id = ?", f.ProjectID)
	}
	if f.Kind != "" {
		if strings.Contains(f.Kind, ".") {
			add("kind = ?", f.Kind)
		} else { // a namespace: kind is validated to [a-z0-9_.], so '_' is the only LIKE wildcard to escape
			add(`kind LIKE ? ESCAPE '\'`, strings.ReplaceAll(f.Kind, "_", `\_`)+".%")
		}
	}
	if !f.From.IsZero() {
		add("occurred_at >= ?", f.From.UnixMilli())
	}
	if !f.To.IsZero() {
		add("occurred_at < ?", f.To.UnixMilli())
	}
	if f.Before > 0 {
		add("id < ?", f.Before)
	}
	q := "SELECT id, occurred_at, actor_type, actor_id, actor_label, kind, COALESCE(project_id, ''), subject, details FROM audit_events"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, f.Limit)
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("querying audit events: %w", err)
	}
	defer rows.Close()
	var out []domain.Event
	for rows.Next() {
		var e domain.Event
		var at int64
		var actorType, kind, details string
		if err := rows.Scan(&e.ID, &at, &actorType, &e.Actor.ID, &e.Actor.Label, &kind, &e.ProjectID, &e.Subject, &details); err != nil {
			return nil, fmt.Errorf("reading audit event: %w", err)
		}
		e.At, e.Actor.Type, e.Kind = time.UnixMilli(at).UTC(), domain.ActorType(actorType), domain.Kind(kind)
		if details != "{}" {
			if err := json.Unmarshal([]byte(details), &e.Details); err != nil {
				return nil, fmt.Errorf("decoding audit event %d details: %w", e.ID, err)
			}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading audit events: %w", err)
	}
	return out, nil
}

// Kinds returns the distinct recorded kinds, sorted.
func (r *Repository) Kinds(ctx context.Context) ([]string, error) {
	rows, err := db.Conn(ctx, r.db).QueryContext(ctx, "SELECT DISTINCT kind FROM audit_events ORDER BY kind")
	if err != nil {
		return nil, fmt.Errorf("listing audit kinds: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("reading audit kind: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
