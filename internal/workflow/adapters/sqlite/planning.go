package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/workflow/app"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

var _ app.PlanningRepository = (*Repository)(nil)

const planningCols = `id, task_id, state, fence, used, runner_id, agent, model, session, session_runner, session_agent,
	feedback, result, context_json, wait_reason, wait_detail, not_before, lease_expires, reason, revision, created_by,
	created_at, updated_at, version`

func scanPlanningRun(row scanner) (domain.PlanningRun, error) {
	var r domain.PlanningRun
	var ctxJSON, created, updated string
	var notBefore, lease sql.NullString
	err := row.Scan(&r.ID, &r.TaskID, &r.State, &r.Fence, &r.Used, &r.RunnerID, &r.Agent, &r.Model, &r.Session,
		&r.SessionRunner, &r.SessionAgent, &r.Feedback, &r.Result, &ctxJSON, &r.WaitReason, &r.WaitDetail, &notBefore,
		&lease, &r.Reason, &r.Revision, &r.CreatedBy, &created, &updated, &r.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return r, domain.ErrNotFound
	}
	if err != nil {
		return r, fmt.Errorf("reading planning run: %w", err)
	}
	if err := json.Unmarshal([]byte(ctxJSON), &r.Context); err != nil {
		return r, fmt.Errorf("planning run %s context: %w", r.ID, err)
	}
	if r.NotBefore, err = parseNullTS(notBefore); err == nil {
		if r.LeaseExpires, err = parseNullTS(lease); err == nil {
			if r.CreatedAt, err = parseTS(created); err == nil {
				r.UpdatedAt, err = parseTS(updated)
			}
		}
	}
	if err != nil {
		return r, fmt.Errorf("reading planning run %s: %w", r.ID, err)
	}
	return r, nil
}

func planningArgs(r *domain.PlanningRun) ([]any, error) {
	c, err := json.Marshal(r.Context)
	if err != nil {
		return nil, fmt.Errorf("encoding the planning context: %w", err)
	}
	return []any{r.State, r.Fence, r.Used, r.RunnerID, r.Agent, r.Model, r.Session, r.SessionRunner, r.SessionAgent,
		r.Feedback, r.Result, string(c), r.WaitReason, r.WaitDetail, nullTS(r.NotBefore), nullTS(r.LeaseExpires),
		r.Reason, r.Revision}, nil
}

// CreatePlanningRun inserts a run at version 1; domain.ErrConflict when the task already has an active run.
func (r *Repository) CreatePlanningRun(ctx context.Context, p *domain.PlanningRun) error {
	args, err := planningArgs(p)
	if err != nil {
		return err
	}
	args = append([]any{p.ID, p.TaskID}, args...)
	args = append(args, p.CreatedBy, ts(p.CreatedAt), ts(p.UpdatedAt))
	_, err = db.Conn(ctx, r.db).ExecContext(ctx, `INSERT INTO planning_runs(`+planningCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`, args...)
	switch {
	case db.IsUniqueViolation(err):
		return domain.ErrConflict
	case db.IsForeignKeyViolation(err):
		return fmt.Errorf("task %s: %w", p.TaskID, domain.ErrNotFound)
	case err != nil:
		return fmt.Errorf("creating planning run: %w", err)
	}
	p.Version = 1
	return nil
}

// PlanningRun loads a run.
func (r *Repository) PlanningRun(ctx context.Context, id string) (domain.PlanningRun, error) {
	return scanPlanningRun(db.Conn(ctx, r.db).QueryRowContext(ctx, "SELECT "+planningCols+" FROM planning_runs WHERE id = ?", id))
}

// LatestPlanningRun loads the task's newest run.
func (r *Repository) LatestPlanningRun(ctx context.Context, taskID string) (domain.PlanningRun, error) {
	return scanPlanningRun(db.Conn(ctx, r.db).QueryRowContext(ctx, "SELECT "+planningCols+
		" FROM planning_runs WHERE task_id = ? ORDER BY created_at DESC, rowid DESC LIMIT 1", taskID))
}

// SavePlanningRun writes the run's mutable fields if its version is unchanged and increments the version.
func (r *Repository) SavePlanningRun(ctx context.Context, p *domain.PlanningRun) error {
	args, err := planningArgs(p)
	if err != nil {
		return err
	}
	args = append(args, ts(p.UpdatedAt), p.ID, p.Version)
	res, err := db.Conn(ctx, r.db).ExecContext(ctx, `UPDATE planning_runs SET state = ?, fence = ?, used = ?,
		runner_id = ?, agent = ?, model = ?, session = ?, session_runner = ?, session_agent = ?, feedback = ?, result = ?,
		context_json = ?, wait_reason = ?, wait_detail = ?, not_before = ?, lease_expires = ?, reason = ?, revision = ?,
		updated_at = ?, version = version + 1 WHERE id = ? AND version = ?`, args...)
	if err := oneRow(res, err, "saving planning run"); err != nil {
		return err
	}
	p.Version++
	return nil
}

// ActivePlanningRuns lists the runs that have not ended, oldest first.
func (r *Repository) ActivePlanningRuns(ctx context.Context) ([]string, error) {
	return r.ids(ctx, "listing planning runs", `SELECT id FROM planning_runs WHERE state IN ('PREPARING', 'QUEUED', 'RUNNING')
		ORDER BY created_at, rowid`)
}

// PlanningRunsHeldBy lists the RUNNING runs whose attempt the runner holds.
func (r *Repository) PlanningRunsHeldBy(ctx context.Context, runnerID string) ([]string, error) {
	return r.ids(ctx, "listing held planning runs", `SELECT id FROM planning_runs WHERE state = 'RUNNING' AND runner_id = ?
		ORDER BY created_at, rowid`, runnerID)
}
