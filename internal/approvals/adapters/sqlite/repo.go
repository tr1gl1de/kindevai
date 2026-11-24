// Package sqlite stores the approvals context: project risk policies and approvals. Every statement runs on
// db.Conn, so an approval commits together with the workflow change, audit event and outbox message it belongs to.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tr1gl1de/kindevai/internal/approvals/app"
	"github.com/tr1gl1de/kindevai/internal/approvals/domain"
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

type ruleJSON struct {
	Category string   `json:"category"`
	Risk     string   `json:"risk"`
	Patterns []string `json:"patterns"`
}

type findingJSON struct {
	Category   string `json:"category"`
	Risk       string `json:"risk"`
	Pattern    string `json:"pattern"`
	Repository string `json:"repository"`
	Path       string `json:"path"`
}

// Policy loads the project's policy; ok is false when the project uses the default.
func (r *Repository) Policy(ctx context.Context, projectID string) (domain.Policy, bool, error) {
	var raw string
	err := r.conn(ctx).QueryRowContext(ctx, `SELECT rules_json FROM risk_policies WHERE project_id = ?`, projectID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Policy{}, false, nil
	}
	if err != nil {
		return domain.Policy{}, false, fmt.Errorf("reading risk policy: %w", err)
	}
	var rules []ruleJSON
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		return domain.Policy{}, false, fmt.Errorf("decoding risk policy: %w", err)
	}
	in := make([]domain.Rule, 0, len(rules))
	for _, x := range rules {
		in = append(in, domain.Rule{Category: domain.Category(x.Category), Risk: domain.Risk(x.Risk), Patterns: x.Patterns})
	}
	p, err := domain.NewPolicy(in)
	if err != nil {
		return domain.Policy{}, false, fmt.Errorf("stored risk policy: %w", err)
	}
	return p, true, nil
}

// SavePolicy inserts or replaces the project's policy.
func (r *Repository) SavePolicy(ctx context.Context, projectID string, p domain.Policy, by string, at time.Time) error {
	rules := make([]ruleJSON, 0, len(p.Rules))
	for _, x := range p.Rules {
		rules = append(rules, ruleJSON{Category: string(x.Category), Risk: string(x.Risk), Patterns: x.Patterns})
	}
	raw, err := json.Marshal(rules)
	if err != nil {
		return err
	}
	_, err = r.conn(ctx).ExecContext(ctx, `INSERT INTO risk_policies(project_id, rules_json, updated_by, updated_at)
		VALUES (?, ?, ?, ?) ON CONFLICT(project_id) DO UPDATE SET rules_json = excluded.rules_json,
		updated_by = excluded.updated_by, updated_at = excluded.updated_at`, projectID, string(raw), nullStr(by), ts(at))
	if db.IsForeignKeyViolation(err) {
		return domain.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("saving risk policy: %w", err)
	}
	return nil
}

const approvalCols = `id, project_id, task_id, task_title, plan_revision, plan_hash, scope, reason, step_key, step_id,
	paths_json, risk, proposed_risk, findings_json, state, approver_id, approver, comment, requested_at, decided_at`

func encode(a domain.Approval) (paths, findings string, err error) {
	p, err := json.Marshal(a.Paths)
	if err != nil {
		return "", "", err
	}
	fs := make([]findingJSON, 0, len(a.Findings))
	for _, f := range a.Findings {
		fs = append(fs, findingJSON{Category: string(f.Category), Risk: string(f.Risk), Pattern: f.Pattern,
			Repository: f.Repository, Path: f.Path})
	}
	f, err := json.Marshal(fs)
	if err != nil {
		return "", "", err
	}
	return string(p), string(f), nil
}

func nullTS(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ts(*t)
}

// Create inserts an approval; app.ErrDuplicatePending when the task has a pending one of the same scope and step.
func (r *Repository) Create(ctx context.Context, a domain.Approval) error {
	paths, findings, err := encode(a)
	if err != nil {
		return err
	}
	_, err = r.conn(ctx).ExecContext(ctx, `INSERT INTO approvals(`+approvalCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.ProjectID, a.TaskID, a.TaskTitle, a.Revision, a.PlanHash, a.Scope, a.Reason, a.StepKey, a.StepID,
		paths, a.Risk, a.ProposedRisk, findings, a.State, nullStr(a.ApproverID), a.Approver, a.Comment,
		ts(a.RequestedAt), nullTS(a.DecidedAt))
	switch {
	case db.IsUniqueViolation(err):
		return app.ErrDuplicatePending
	case db.IsForeignKeyViolation(err):
		return fmt.Errorf("task %s revision %d: %w", a.TaskID, a.Revision, domain.ErrNotFound)
	case err != nil:
		return fmt.Errorf("creating approval: %w", err)
	}
	return nil
}

// Save stores the decision of a pending approval; app.ErrConflict when it is no longer pending.
func (r *Repository) Save(ctx context.Context, a domain.Approval) error {
	res, err := r.conn(ctx).ExecContext(ctx, `UPDATE approvals SET state = ?, approver_id = ?, approver = ?,
		comment = ?, decided_at = ? WHERE id = ? AND state = 'pending'`,
		a.State, nullStr(a.ApproverID), a.Approver, a.Comment, nullTS(a.DecidedAt), a.ID)
	if err != nil {
		return fmt.Errorf("saving approval: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return app.ErrConflict
	}
	return nil
}

type scanner interface{ Scan(dest ...any) error }

func scan(row scanner) (domain.Approval, error) {
	var a domain.Approval
	var paths, findings, requested string
	var approverID, decided sql.NullString
	err := row.Scan(&a.ID, &a.ProjectID, &a.TaskID, &a.TaskTitle, &a.Revision, &a.PlanHash, &a.Scope, &a.Reason,
		&a.StepKey, &a.StepID, &paths, &a.Risk, &a.ProposedRisk, &findings, &a.State, &approverID, &a.Approver,
		&a.Comment, &requested, &decided)
	if err != nil {
		return a, err
	}
	a.ApproverID = approverID.String
	if err := json.Unmarshal([]byte(paths), &a.Paths); err != nil {
		return a, fmt.Errorf("decoding approval paths: %w", err)
	}
	var fs []findingJSON
	if err := json.Unmarshal([]byte(findings), &fs); err != nil {
		return a, fmt.Errorf("decoding approval findings: %w", err)
	}
	a.Findings = make([]domain.Finding, 0, len(fs))
	for _, f := range fs {
		a.Findings = append(a.Findings, domain.Finding{Category: domain.Category(f.Category), Risk: domain.Risk(f.Risk),
			Pattern: f.Pattern, Repository: f.Repository, Path: f.Path})
	}
	if a.RequestedAt, err = time.Parse(time.RFC3339Nano, requested); err != nil {
		return a, err
	}
	if decided.Valid {
		t, err := time.Parse(time.RFC3339Nano, decided.String)
		if err != nil {
			return a, err
		}
		a.DecidedAt = &t
	}
	return a, nil
}

func (r *Repository) query(ctx context.Context, q string, args ...any) ([]domain.Approval, error) {
	rows, err := r.conn(ctx).QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("listing approvals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []domain.Approval{}
	for rows.Next() {
		a, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("reading approval: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Get loads an approval.
func (r *Repository) Get(ctx context.Context, id string) (domain.Approval, error) {
	a, err := scan(r.conn(ctx).QueryRowContext(ctx, `SELECT `+approvalCols+` FROM approvals WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, domain.ErrNotFound
	}
	if err != nil {
		return a, fmt.Errorf("reading approval: %w", err)
	}
	return a, nil
}

// ForTask lists the task's approvals, oldest first.
func (r *Repository) ForTask(ctx context.Context, taskID string) ([]domain.Approval, error) {
	return r.query(ctx, `SELECT `+approvalCols+` FROM approvals WHERE task_id = ? ORDER BY requested_at, rowid`, taskID)
}

// List returns approvals matching f, newest first.
func (r *Repository) List(ctx context.Context, f app.Filter) ([]domain.Approval, error) {
	var where []string
	var args []any
	if !f.AllProjects {
		if len(f.ProjectIDs) == 0 {
			return []domain.Approval{}, nil
		}
		where = append(where, "project_id IN (?"+strings.Repeat(", ?", len(f.ProjectIDs)-1)+")")
		for _, id := range f.ProjectIDs {
			args = append(args, id)
		}
	}
	if len(f.States) > 0 {
		where = append(where, "state IN (?"+strings.Repeat(", ?", len(f.States)-1)+")")
		for _, s := range f.States {
			args = append(args, string(s))
		}
	}
	q := `SELECT ` + approvalCols + ` FROM approvals`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	limit := f.Limit
	if limit <= 0 || limit > app.MaxList {
		limit = app.MaxList
	}
	q += " ORDER BY requested_at DESC, rowid DESC LIMIT ?"
	args = append(args, limit)
	return r.query(ctx, q, args...)
}
