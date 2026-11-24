// Package app holds the workflow use cases: tasks move through their FSM, planners submit immutable plan
// revisions, and runners claim, start, renew, complete or fail execution steps under a lease and fencing token
// (KIN-A-14, KIN-A-15). Every use case is one transaction over whole aggregates.
package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// Defaults from the KIN-21 decisions (KIN-A-14 "Durable execution").
const (
	// DefaultLeaseTTL: a claim or run lease; the runner renews it with every heartbeat.
	DefaultLeaseTTL = 60 * time.Second
	// DefaultMaxAttempts: one retry after a confirmed failure (KIN-23 decision).
	DefaultMaxAttempts = 2
)

// ErrDuplicateIssue: the project already has a task for this YouTrack issue.
var ErrDuplicateIssue = errors.New("task for this YouTrack issue already exists")

// Repository stores the workflow aggregates. Every method runs on the caller's transaction (db.Conn). Save
// methods compare Version with the stored one (domain.ErrConflict when it changed) and increment it.
type Repository interface {
	CreateTask(ctx context.Context, t *domain.Task) error // ErrDuplicateIssue; domain.ErrNotFound for an unknown project
	Task(ctx context.Context, id string) (domain.Task, error)
	SaveTask(ctx context.Context, t *domain.Task) error

	CreateRevision(ctx context.Context, r domain.PlanRevision) error // revisions are never updated
	Revision(ctx context.Context, taskID string, number int) (domain.PlanRevision, error)

	CreateExecution(ctx context.Context, e *domain.Execution) error // domain.ErrConflict if the task has a running one
	Execution(ctx context.Context, id string) (domain.Execution, error)
	ExecutionByStep(ctx context.Context, stepID string) (domain.Execution, error)
	RunningExecution(ctx context.Context, taskID string) (domain.Execution, error)
	SaveExecution(ctx context.Context, e *domain.Execution) error
	// ExpiredLeaseExecutions lists running executions with a step lease that ran out at now.
	ExpiredLeaseExecutions(ctx context.Context, now time.Time) ([]string, error)
}

// TxRunner runs fn in one database transaction; repositories called with fn's ctx join it.
type TxRunner interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// Projects is the projects context's API as the workflow needs it.
type Projects interface {
	// Repositories lists the repositories of the project a plan may use; domain.ErrNotFound for an unknown project.
	Repositories(ctx context.Context, projectID string) ([]string, error)
}

// Service implements the workflow use cases.
type Service struct {
	Repo        Repository
	Projects    Projects
	Tx          TxRunner
	Now         func() time.Time
	NewID       func() string
	LeaseTTL    time.Duration // DefaultLeaseTTL when zero
	MaxAttempts int           // DefaultMaxAttempts when zero
}

func (s *Service) now() time.Time { return s.Now().UTC() }

func (s *Service) leaseTTL() time.Duration {
	if s.LeaseTTL > 0 {
		return s.LeaseTTL
	}
	return DefaultLeaseTTL
}

func (s *Service) maxAttempts() int {
	if s.MaxAttempts > 0 {
		return s.MaxAttempts
	}
	return DefaultMaxAttempts
}

// CreateTask creates a CREATED task, optionally linked to a YouTrack issue (one task per issue and project).
func (s *Service) CreateTask(ctx context.Context, projectID, title, youtrackIssue, createdBy string) (domain.Task, error) {
	t, err := domain.NewTask(s.NewID(), projectID, title, youtrackIssue, createdBy, s.now())
	if err != nil {
		return domain.Task{}, err
	}
	if _, err := s.Projects.Repositories(ctx, projectID); err != nil {
		return domain.Task{}, fmt.Errorf("project %s: %w", projectID, err)
	}
	if err := s.Repo.CreateTask(ctx, &t); err != nil {
		return domain.Task{}, err
	}
	return t, nil
}

// Task returns a task.
func (s *Service) Task(ctx context.Context, id string) (domain.Task, error) {
	return s.Repo.Task(ctx, id)
}

// Execution returns an execution with its steps and attempts.
func (s *Service) Execution(ctx context.Context, id string) (domain.Execution, error) {
	return s.Repo.Execution(ctx, id)
}

// updateTask loads the task, applies fn and saves it in one transaction.
func (s *Service) updateTask(ctx context.Context, id string, fn func(ctx context.Context, t *domain.Task, now time.Time) error) (domain.Task, error) {
	var t domain.Task
	err := s.Tx.InTx(ctx, func(ctx context.Context) error {
		var err error
		if t, err = s.Repo.Task(ctx, id); err != nil {
			return err
		}
		if err := fn(ctx, &t, s.now()); err != nil {
			return err
		}
		return s.Repo.SaveTask(ctx, &t)
	})
	if err != nil {
		return domain.Task{}, err
	}
	return t, nil
}

// StartAnalysis moves a CREATED task to ANALYZING (the planner takes it).
func (s *Service) StartAnalysis(ctx context.Context, taskID string) (domain.Task, error) {
	return s.updateTask(ctx, taskID, func(_ context.Context, t *domain.Task, now time.Time) error {
		return t.Transition(domain.TaskAnalyzing, domain.Reason{}, now)
	})
}

// SubmitPlan stores plan as the task's next immutable revision and routes the task by risk, the effective level
// after policy (L0/L1 READY, L2/L3 AWAITING_APPROVAL). The DAG is validated against the project's repositories.
func (s *Service) SubmitPlan(ctx context.Context, taskID string, plan domain.Plan, risk domain.Risk, author string) (domain.PlanRevision, error) {
	var rev domain.PlanRevision
	_, err := s.updateTask(ctx, taskID, func(ctx context.Context, t *domain.Task, now time.Time) error {
		repos, err := s.Projects.Repositories(ctx, t.ProjectID)
		if err != nil {
			return fmt.Errorf("project %s: %w", t.ProjectID, err)
		}
		if rev, err = domain.NewPlanRevision(t.ID, t.PlanRevision+1, plan, risk, repos, author, now); err != nil {
			return err
		}
		if err := t.AcceptPlan(rev.Number(), rev.Risk(), now); err != nil {
			return err
		}
		return s.Repo.CreateRevision(ctx, rev)
	})
	if err != nil {
		return domain.PlanRevision{}, err
	}
	return rev, nil
}

// StartExecution runs the current plan revision of a READY task: the task goes RUNNING and the plan's root steps
// become READY for claims.
func (s *Service) StartExecution(ctx context.Context, taskID string) (domain.Execution, error) {
	var e domain.Execution
	_, err := s.updateTask(ctx, taskID, func(ctx context.Context, t *domain.Task, now time.Time) error {
		if err := t.Transition(domain.TaskRunning, domain.Reason{}, now); err != nil {
			return err
		}
		rev, err := s.Repo.Revision(ctx, t.ID, t.PlanRevision)
		if err != nil {
			return fmt.Errorf("plan revision %d: %w", t.PlanRevision, err)
		}
		e = domain.NewExecution(s.NewID(), rev, s.NewID, now)
		return s.Repo.CreateExecution(ctx, &e)
	})
	if err != nil {
		return domain.Execution{}, err
	}
	return e, nil
}

// updateExecution loads the execution owning stepID, applies fn, saves it and carries a finished execution over
// to its task, all in one transaction. Nothing is saved when fn fails.
func (s *Service) updateExecution(ctx context.Context, stepID string, fn func(ctx context.Context, e *domain.Execution, now time.Time) error) error {
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		e, err := s.Repo.ExecutionByStep(ctx, stepID)
		if err != nil {
			return err
		}
		now := s.now()
		if err := fn(ctx, &e, now); err != nil {
			return err
		}
		if err := s.Repo.SaveExecution(ctx, &e); err != nil {
			return err
		}
		return s.syncTask(ctx, &e, now)
	})
}

// syncTask moves the task when its execution finished: SUCCEEDED -> VERIFYING, FAILED -> FAILED. A task blocked
// while running is resumed first.
func (s *Service) syncTask(ctx context.Context, e *domain.Execution, now time.Time) error {
	var to domain.TaskState
	var r domain.Reason
	switch e.State {
	case domain.ExecutionSucceeded:
		to = domain.TaskVerifying
	case domain.ExecutionFailed:
		to, r = domain.TaskFailed, domain.Reason{Detail: e.Reason}
	default:
		return nil
	}
	t, err := s.Repo.Task(ctx, e.TaskID)
	if err != nil {
		return err
	}
	if t.State == domain.TaskBlocked && t.BlockedFrom == domain.TaskRunning {
		if err := t.Resume(now); err != nil {
			return err
		}
	}
	if err := t.Transition(to, r, now); err != nil {
		return err
	}
	return s.Repo.SaveTask(ctx, &t)
}

// Claim gives runnerID the READY step with a new fencing token and a lease, only while the task is RUNNING (not
// BLOCKED). Concurrent claims of one step are serialized by the transaction and the aggregate version: exactly one
// wins. runnerID must come from the authenticated runner channel, and the caller (the scheduler, KIN-23) checks the
// runner is approved for the project and has the step's capabilities.
func (s *Service) Claim(ctx context.Context, stepID, runnerID string) (domain.Lease, error) {
	var l domain.Lease
	err := s.updateExecution(ctx, stepID, func(ctx context.Context, e *domain.Execution, now time.Time) error {
		t, err := s.Repo.Task(ctx, e.TaskID)
		if err != nil {
			return err
		}
		if t.State != domain.TaskRunning {
			return fmt.Errorf("task %s is %s: claim: %w", t.ID, t.State, domain.ErrInvalidTransition)
		}
		l, err = e.Claim(stepID, runnerID, now, s.leaseTTL())
		return err
	})
	return l, err
}

// ReconcileStep resolves a step whose attempt became UNKNOWN when its lease ran out (KIN-23 reconciliation).
func (s *Service) ReconcileStep(ctx context.Context, stepID string, p domain.ProcessState) error {
	return s.updateExecution(ctx, stepID, func(_ context.Context, e *domain.Execution, now time.Time) error {
		return e.Reconcile(stepID, p, now, s.leaseTTL(), s.maxAttempts())
	})
}

// StartStep records that the runner launched the claimed step.
func (s *Service) StartStep(ctx context.Context, stepID, runnerID string, fence int64) error {
	return s.updateExecution(ctx, stepID, func(_ context.Context, e *domain.Execution, now time.Time) error {
		return e.Start(stepID, runnerID, fence, now, s.leaseTTL())
	})
}

// RenewLease extends the step's lease (runner heartbeat) and returns the new expiry.
func (s *Service) RenewLease(ctx context.Context, stepID, runnerID string, fence int64) (time.Time, error) {
	var exp time.Time
	err := s.updateExecution(ctx, stepID, func(_ context.Context, e *domain.Execution, now time.Time) error {
		var err error
		exp, err = e.Renew(stepID, runnerID, fence, now, s.leaseTTL())
		return err
	})
	return exp, err
}

// CompleteStep records the step's success and promotes its dependents.
func (s *Service) CompleteStep(ctx context.Context, stepID, runnerID string, fence int64) error {
	return s.updateExecution(ctx, stepID, func(_ context.Context, e *domain.Execution, now time.Time) error {
		return e.Succeed(stepID, runnerID, fence, now)
	})
}

// FailStep records a failed attempt; the step is retried within the attempt budget, else the task fails.
func (s *Service) FailStep(ctx context.Context, stepID, runnerID string, fence int64, msg string) error {
	return s.updateExecution(ctx, stepID, func(_ context.Context, e *domain.Execution, now time.Time) error {
		return e.Fail(stepID, runnerID, fence, msg, now, s.maxAttempts())
	})
}

// ExpireLeases handles every lease that ran out (one transaction per execution) and returns how many steps changed.
func (s *Service) ExpireLeases(ctx context.Context) (int, error) {
	ids, err := s.Repo.ExpiredLeaseExecutions(ctx, s.now())
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		err := s.Tx.InTx(ctx, func(ctx context.Context) error {
			e, err := s.Repo.Execution(ctx, id)
			if err != nil {
				return err
			}
			changed := e.ExpireLeases(s.now())
			if len(changed) == 0 {
				return nil
			}
			n += len(changed)
			return s.Repo.SaveExecution(ctx, &e)
		})
		if err != nil {
			return n, fmt.Errorf("expiring leases of execution %s: %w", id, err)
		}
	}
	return n, nil
}

// BlockTask parks the task with a wait reason (runner, network, cli_limit, approval).
func (s *Service) BlockTask(ctx context.Context, taskID string, reason domain.WaitReason, detail string) (domain.Task, error) {
	return s.updateTask(ctx, taskID, func(_ context.Context, t *domain.Task, now time.Time) error {
		return t.Transition(domain.TaskBlocked, domain.Reason{Code: reason, Detail: detail}, now)
	})
}

// ResumeTask returns a blocked task to the state it was blocked in.
func (s *Service) ResumeTask(ctx context.Context, taskID string) (domain.Task, error) {
	return s.updateTask(ctx, taskID, func(_ context.Context, t *domain.Task, now time.Time) error {
		return t.Resume(now)
	})
}

// CancelTask cancels the task and its running execution, if any.
func (s *Service) CancelTask(ctx context.Context, taskID, reason string) (domain.Task, error) {
	return s.updateTask(ctx, taskID, func(ctx context.Context, t *domain.Task, now time.Time) error {
		if err := t.Transition(domain.TaskCancelled, domain.Reason{Detail: reason}, now); err != nil {
			return err
		}
		e, err := s.Repo.RunningExecution(ctx, t.ID)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := e.Cancel(reason, now); err != nil {
			return err
		}
		return s.Repo.SaveExecution(ctx, &e)
	})
}
