package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

// This file implements app.WriteStore (KIN-38: YouTrack write-back).

// StateMap returns a binding's task state → YouTrack state map; domain.ErrNotFound when unbound.
func (r *Repository) StateMap(ctx context.Context, projectID string) (domain.StateMap, error) {
	var raw string
	err := r.conn(ctx).QueryRowContext(ctx, "SELECT state_map FROM project_youtrack WHERE project_id = ?", projectID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading state map: %w", err)
	}
	m := domain.StateMap{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("reading state map: %w", err)
	}
	return m, nil
}

// SetStateMap saves a binding's state map; domain.ErrNotFound when unbound.
func (r *Repository) SetStateMap(ctx context.Context, projectID string, m domain.StateMap, at time.Time) error {
	if m == nil {
		m = domain.StateMap{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	res, err := r.conn(ctx).ExecContext(ctx, "UPDATE project_youtrack SET state_map = ?, updated_at = ? WHERE project_id = ?",
		string(b), ts(at), projectID)
	return affected(res, err, "saving state map")
}

// SaveIssueWrite records a pending write; a key that exists is kept (the outbox ignores it too).
func (r *Repository) SaveIssueWrite(ctx context.Context, w domain.IssueWrite) error {
	_, err := r.conn(ctx).ExecContext(ctx, `INSERT INTO youtrack_writes(key, task_id, issue_id, kind, target, status,
		created_at, updated_at) VALUES (?, ?, ?, ?, ?, 'pending', ?, ?) ON CONFLICT(key) DO NOTHING`,
		w.Key, w.TaskID, w.IssueID, string(w.Kind), nullStr(w.Target), ts(w.CreatedAt), ts(w.CreatedAt))
	if err != nil {
		return fmt.Errorf("saving youtrack write: %w", err)
	}
	return nil
}

// MarkIssueWrite records a delivery's outcome; domain.ErrNotFound for an unknown key.
func (r *Repository) MarkIssueWrite(ctx context.Context, key string, s domain.WriteStatus, code string, attempts int, at time.Time) error {
	res, err := r.conn(ctx).ExecContext(ctx, `UPDATE youtrack_writes SET status = ?, error = ?, attempts = max(attempts, ?),
		updated_at = ? WHERE key = ?`, string(s), nullStr(code), attempts, ts(at), key)
	return affected(res, err, "saving youtrack write")
}

// IssueWrite returns one write; domain.ErrNotFound when none.
func (r *Repository) IssueWrite(ctx context.Context, key string) (domain.IssueWrite, error) {
	var w domain.IssueWrite
	var kind, status, created, updated string
	var target, code sql.NullString
	err := r.conn(ctx).QueryRowContext(ctx, `SELECT key, task_id, issue_id, kind, target, status, attempts, error,
		created_at, updated_at FROM youtrack_writes WHERE key = ?`, key).Scan(&w.Key, &w.TaskID, &w.IssueID, &kind,
		&target, &status, &w.Attempts, &code, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return w, domain.ErrNotFound
	}
	if err != nil {
		return w, fmt.Errorf("reading youtrack write: %w", err)
	}
	w.Kind, w.Status, w.Target, w.Error = domain.WriteKind(kind), domain.WriteStatus(status), target.String, code.String
	if w.CreatedAt, err = parseTS(created); err != nil {
		return w, fmt.Errorf("reading youtrack write: %w", err)
	}
	if w.UpdatedAt, err = parseTS(updated); err != nil {
		return w, fmt.Errorf("reading youtrack write: %w", err)
	}
	return w, nil
}

// TaskPublication sums a task's writes; LastError is the newest error of a pending or failed write.
func (r *Repository) TaskPublication(ctx context.Context, taskID string) (domain.Publication, error) {
	var p domain.Publication
	var last sql.NullString
	err := r.conn(ctx).QueryRowContext(ctx, `SELECT
		  coalesce(sum(status = 'pending'), 0), coalesce(sum(status = 'failed'), 0), coalesce(sum(status = 'published'), 0),
		  (SELECT error FROM youtrack_writes WHERE task_id = ?1 AND status <> 'published' AND error IS NOT NULL
		   ORDER BY updated_at DESC, key LIMIT 1)
		FROM youtrack_writes WHERE task_id = ?1`, taskID).Scan(&p.Pending, &p.Failed, &p.Published, &last)
	if err != nil {
		return p, fmt.Errorf("reading publication: %w", err)
	}
	p.LastError = last.String
	return p, nil
}
