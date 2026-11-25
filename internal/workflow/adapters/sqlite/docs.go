package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// This file implements app.DocsStore (KIN-38).

// SaveDocs stores (or replaces) the documentation decision of a run.
func (r *Repository) SaveDocs(ctx context.Context, d domain.DocsDecision) error {
	if d.Status == domain.DocsNotApplicable && strings.TrimSpace(d.Reason) == "" {
		return domain.ErrDocsReasonRequired
	}
	arts, err := json.Marshal(orEmpty(d.Articles))
	if err != nil {
		return err
	}
	rej, err := json.Marshal(orEmpty(d.Rejected))
	if err != nil {
		return err
	}
	_, err = db.Conn(ctx, r.db).ExecContext(ctx, `INSERT INTO task_docs(task_id, execution_id, status, reason, articles, rejected,
		created_at) VALUES (?, ?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		ON CONFLICT(task_id, execution_id) DO UPDATE SET status = excluded.status, reason = excluded.reason,
		articles = excluded.articles, rejected = excluded.rejected`,
		d.TaskID, d.ExecutionID, string(d.Status), nullString(d.Reason), string(arts), string(rej))
	if err != nil {
		return fmt.Errorf("saving docs decision: %w", err)
	}
	return nil
}

func orEmpty[T any](xs []T) []T {
	if xs == nil {
		return []T{}
	}
	return xs
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// LatestDocs returns the task's newest documentation decision; domain.ErrNotFound when none.
func (r *Repository) LatestDocs(ctx context.Context, taskID string) (domain.DocsDecision, error) {
	d := domain.DocsDecision{TaskID: taskID}
	var status, arts, rej string
	var reason sql.NullString
	err := db.Conn(ctx, r.db).QueryRowContext(ctx, `SELECT execution_id, status, reason, articles, rejected FROM task_docs
		WHERE task_id = ? ORDER BY created_at DESC, rowid DESC LIMIT 1`, taskID).Scan(&d.ExecutionID, &status, &reason,
		&arts, &rej)
	if errors.Is(err, sql.ErrNoRows) {
		return d, domain.ErrNotFound
	}
	if err != nil {
		return d, fmt.Errorf("reading docs decision: %w", err)
	}
	d.Status, d.Reason = domain.DocsStatus(status), reason.String
	if err := json.Unmarshal([]byte(arts), &d.Articles); err != nil {
		return d, fmt.Errorf("reading docs decision: %w", err)
	}
	if err := json.Unmarshal([]byte(rej), &d.Rejected); err != nil {
		return d, fmt.Errorf("reading docs decision: %w", err)
	}
	return d, nil
}
