// Package workflow connects the approvals context with the workflow context through the workflow's app API, in
// both directions: Port is the approvals app's Workflow port; Hooks implements the workflow's RiskPolicy and
// Events ports with the approvals use cases. Types and errors are translated here, so neither context imports
// the other's app or domain packages.
package workflow

import (
	"context"
	"errors"

	"github.com/tr1gl1de/kindevai/internal/approvals/app"
	"github.com/tr1gl1de/kindevai/internal/approvals/domain"
	wfapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
	wf "github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// translate maps workflow errors to approvals errors.
func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, wf.ErrNotFound):
		return errors.Join(domain.ErrNotFound, err)
	case errors.Is(err, wf.ErrStaleRevision), errors.Is(err, wf.ErrStaleFence):
		return errors.Join(domain.ErrStaleRevision, err)
	case errors.Is(err, wf.ErrInvalidTransition), errors.Is(err, wf.ErrAttemptUnknown):
		return errors.Join(domain.ErrNotReady, err)
	}
	return err
}

// Snapshot converts a workflow plan to the approvals view of it.
func Snapshot(p wf.Plan) domain.PlanSnapshot {
	out := domain.PlanSnapshot{Summary: p.Summary, Risk: domain.Risk(p.Risk), MaxParallel: p.MaxParallel,
		Steps: make([]domain.StepSnapshot, 0, len(p.Steps)), Tests: p.Tests, Contracts: p.Contracts,
		Migrations: p.Migrations, Docs: p.Docs}
	for _, s := range p.Steps {
		out.Steps = append(out.Steps, domain.StepSnapshot{Key: s.Key, Title: s.Title, Repository: s.Repository,
			Role: s.Role, Scope: s.Scope, DependsOn: s.DependsOn, Capabilities: s.Capabilities})
	}
	return out
}

func taskRef(t wf.Task) app.TaskRef {
	return app.TaskRef{ID: t.ID, ProjectID: t.ProjectID, Title: t.Title, State: string(t.State), PlanRevision: t.PlanRevision}
}

func revisionRef(r wf.PlanRevision) app.RevisionRef {
	return app.RevisionRef{Number: r.Number(), Hash: r.Hash(), Risk: domain.Risk(r.Risk()), Plan: Snapshot(r.Plan()),
		CreatedBy: r.CreatedBy(), CreatedAt: r.CreatedAt()}
}

// Port implements app.Workflow on the workflow service.
type Port struct{ Svc *wfapp.Service }

func (p Port) Task(ctx context.Context, id string) (app.TaskRef, error) {
	t, err := p.Svc.Task(ctx, id)
	return taskRef(t), translate(err)
}

func (p Port) Revision(ctx context.Context, taskID string, n int) (app.RevisionRef, error) {
	r, err := p.Svc.Revision(ctx, taskID, n)
	if err != nil {
		return app.RevisionRef{}, translate(err)
	}
	return revisionRef(r), nil
}

func (p Port) ApprovePlan(ctx context.Context, taskID string, n int, hash string) error {
	_, err := p.Svc.ApprovePlan(ctx, taskID, n, hash)
	return translate(err)
}

func (p Port) RejectPlan(ctx context.Context, taskID string, n int, hash string) error {
	_, err := p.Svc.RejectPlan(ctx, taskID, n, hash)
	return translate(err)
}

func (p Port) Step(ctx context.Context, stepID string) (app.StepRef, error) {
	info, err := p.Svc.StepInfo(ctx, stepID)
	if err != nil {
		return app.StepRef{}, translate(err)
	}
	ps, ok := info.PlanStep()
	if !ok {
		return app.StepRef{}, domain.ErrNotFound
	}
	return app.StepRef{StepID: stepID, TaskID: info.Task.ID, Key: ps.Key, Repository: ps.Repository,
		Revision: info.Revision.Number(), PlanHash: info.Revision.Hash(), Risk: domain.Risk(info.Revision.Risk()),
		Scope: ps.Scope}, nil
}

func (p Port) HoldStep(ctx context.Context, stepID, runnerID string, fence int64, msg string) error {
	return translate(p.Svc.HoldStep(ctx, stepID, runnerID, fence, msg))
}

func (p Port) ResumeStep(ctx context.Context, stepID string) error {
	return translate(p.Svc.ResumeHeldStep(ctx, stepID))
}

func (p Port) CancelTask(ctx context.Context, taskID, reason string) error {
	_, err := p.Svc.CancelTask(ctx, taskID, reason)
	return translate(err)
}

// Hooks implements the workflow's RiskPolicy and Events ports with the approvals service.
type Hooks struct{ Svc *app.Service }

// EffectiveRisk runs the Policy Evaluator on the plan.
func (h Hooks) EffectiveRisk(ctx context.Context, projectID string, plan wf.Plan) (wf.Risk, error) {
	r, err := h.Svc.EffectiveRisk(ctx, projectID, Snapshot(plan))
	return wf.Risk(r), err
}

// PlanAccepted requests the approvals of the new revision and supersedes older ones.
func (h Hooks) PlanAccepted(ctx context.Context, t wf.Task, rev wf.PlanRevision) error {
	return h.Svc.PlanAccepted(ctx, app.PlanEvent{TaskID: t.ID, ProjectID: t.ProjectID, TaskTitle: t.Title,
		Revision: rev.Number(), PlanHash: rev.Hash(), Risk: domain.Risk(rev.Risk()), Plan: Snapshot(rev.Plan())})
}

// TaskClosed supersedes the task's pending approvals.
func (h Hooks) TaskClosed(ctx context.Context, t wf.Task) error {
	return h.Svc.TaskClosed(ctx, t.ID)
}

var (
	_ app.Workflow     = Port{}
	_ wfapp.RiskPolicy = Hooks{}
	_ wfapp.Events     = Hooks{}
)
