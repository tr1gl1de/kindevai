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
	TaskByIssue(ctx context.Context, projectID, youtrackIssue string) (domain.Task, error) // domain.ErrNotFound
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
	RunningExecutions(ctx context.Context) ([]string, error)
	TasksInState(ctx context.Context, s domain.TaskState) ([]string, error)
	// ExecutionsHeldBy lists running executions with a step whose claim or unknown attempt is the runner's.
	ExecutionsHeldBy(ctx context.Context, runnerID string) ([]string, error)
	ListTasks(ctx context.Context, projectIDs []string, limit int) ([]domain.Task, error) // newest first
	LatestExecution(ctx context.Context, taskID string) (domain.Execution, error)         // domain.ErrNotFound
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

// RiskPolicy is the deterministic Policy Evaluator (approvals context, KIN-24). The planner's proposed risk is
// never trusted on its own: the effective risk of a revision always comes from here, and a result below the
// proposal is refused.
type RiskPolicy interface {
	EffectiveRisk(ctx context.Context, projectID string, plan domain.Plan) (domain.Risk, error)
}

// Events receives workflow domain events inside the use case's transaction; an error rolls the use case back.
// The approvals context requests approvals for a new revision and retires those of older revisions or of a
// closed task.
type Events interface {
	PlanAccepted(ctx context.Context, t domain.Task, rev domain.PlanRevision) error
	TaskClosed(ctx context.Context, t domain.Task) error // CANCELLED or FAILED
}

// TaskNotifier is told, inside the use case's transaction, that a task finished: it entered COMPLETED or FAILED
// (a cancellation is someone's own decision and is not announced). It runs once per state change, so the
// notification (an outbox message, KIN-62) commits or rolls back with the task.
type TaskNotifier interface {
	TaskFinished(ctx context.Context, t domain.Task) error
}

// Service implements the workflow use cases.
type Service struct {
	Repo        Repository
	Projects    Projects
	Policy      RiskPolicy   // required for SubmitPlan
	Events      Events       // optional
	Notifier    TaskNotifier // optional: "task finished" notifications (KIN-62)
	Runners     RunnerPool   // required for Schedule
	Dispatcher  Dispatcher   // runner commands; required for Schedule, optional otherwise
	Access      Access       // project visibility for the task API (KIN-23)
	Outbox      Outbox       // alert hooks (KIN-60); nil: none
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

// TaskForIssue returns the project's task for a YouTrack issue; domain.ErrNotFound if there is none.
func (s *Service) TaskForIssue(ctx context.Context, projectID, youtrackIssue string) (domain.Task, error) {
	return s.Repo.TaskByIssue(ctx, projectID, youtrackIssue)
}

// RetitleTask follows a changed YouTrack summary; it reports whether the title changed (finished tasks keep theirs).
func (s *Service) RetitleTask(ctx context.Context, taskID, title string) (bool, error) {
	var changed bool
	err := s.Tx.InTx(ctx, func(ctx context.Context) error {
		t, err := s.Repo.Task(ctx, taskID)
		if err != nil {
			return err
		}
		if changed, err = t.Retitle(title, s.now()); err != nil || !changed {
			return err
		}
		return s.saveTask(ctx, &t)
	})
	return changed, err
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
		from := t.State
		if err := fn(ctx, &t, s.now()); err != nil {
			return err
		}
		if err := s.saveTask(ctx, &t); err != nil {
			return err
		}
		return s.finished(ctx, from, t)
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

// SubmitPlan stores plan as the task's next immutable revision and routes the task by its effective risk, which
// the Policy Evaluator computes from the plan (it may only raise the planner's proposal): L0/L1 READY, L2/L3
// AWAITING_APPROVAL. The DAG is validated against the project's repositories. Approvals of older revisions are
// retired by the Events listener in the same transaction.
func (s *Service) SubmitPlan(ctx context.Context, taskID string, plan domain.Plan, author string) (domain.PlanRevision, error) {
	if s.Policy == nil {
		return domain.PlanRevision{}, errors.New("workflow: risk policy not configured")
	}
	var rev domain.PlanRevision
	_, err := s.updateTask(ctx, taskID, func(ctx context.Context, t *domain.Task, now time.Time) error {
		repos, err := s.Projects.Repositories(ctx, t.ProjectID)
		if err != nil {
			return fmt.Errorf("project %s: %w", t.ProjectID, err)
		}
		if err := t.CanAcceptPlan(); err != nil {
			return err
		}
		if err := domain.ValidatePlan(plan, repos); err != nil {
			return err
		}
		risk, err := s.Policy.EffectiveRisk(ctx, t.ProjectID, plan.Normalize())
		if err != nil {
			return fmt.Errorf("evaluating plan risk: %w", err)
		}
		if rev, err = domain.NewPlanRevision(t.ID, t.PlanRevision+1, plan, risk, repos, author, now); err != nil {
			return err
		}
		if err := t.AcceptPlan(rev.Number(), rev.Risk(), now); err != nil {
			return err
		}
		if err := s.Repo.CreateRevision(ctx, rev); err != nil {
			return err
		}
		if s.Events != nil {
			return s.Events.PlanAccepted(ctx, *t, rev)
		}
		return nil
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
		return s.applyExecution(ctx, e, fn)
	})
}

// updateExecutionByID is updateExecution for an execution id.
func (s *Service) updateExecutionByID(ctx context.Context, id string, fn func(ctx context.Context, e *domain.Execution, now time.Time) error) error {
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		e, err := s.Repo.Execution(ctx, id)
		if err != nil {
			return err
		}
		return s.applyExecution(ctx, e, fn)
	})
}

func (s *Service) applyExecution(ctx context.Context, e domain.Execution, fn func(ctx context.Context, e *domain.Execution, now time.Time) error) error {
	now := s.now()
	before := e.Clone()
	if err := fn(ctx, &e, now); err != nil {
		return err
	}
	if executionEqual(before, e) && before.State == e.State && attemptsEqual(before, e) {
		return nil
	}
	if err := s.Repo.SaveExecution(ctx, &e); err != nil {
		return err
	}
	if err := s.cancelReleased(ctx, &before, &e); err != nil {
		return err
	}
	return s.syncTask(ctx, &e, now)
}

func attemptsEqual(a, b domain.Execution) bool {
	for i := range a.Steps {
		x, y := a.Steps[i], b.Steps[i]
		if len(x.Attempts) != len(y.Attempts) || !ptrTimeEqual(x.LeaseExpiresAt, y.LeaseExpiresAt) {
			return false
		}
		for j := range x.Attempts {
			if x.Attempts[j].State != y.Attempts[j].State {
				return false
			}
		}
	}
	return true
}

func ptrTimeEqual(a, b *time.Time) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Equal(*b)
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
	from := t.State
	if t.State == domain.TaskBlocked && t.BlockedFrom == domain.TaskRunning {
		if err := t.Resume(now); err != nil {
			return err
		}
	}
	if err := t.Transition(to, r, now); err != nil {
		return err
	}
	if err := s.saveTask(ctx, &t); err != nil {
		return err
	}
	if to == domain.TaskFailed && s.Events != nil {
		if err := s.Events.TaskClosed(ctx, t); err != nil {
			return err
		}
	}
	return s.finished(ctx, from, t)
}

// finished tells the notifier when the saved task has just entered COMPLETED or FAILED.
func (s *Service) finished(ctx context.Context, from domain.TaskState, t domain.Task) error {
	if s.Notifier == nil || from == t.State || (t.State != domain.TaskCompleted && t.State != domain.TaskFailed) {
		return nil
	}
	return s.Notifier.TaskFinished(ctx, t)
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
		if s.Events != nil {
			if err := s.Events.TaskClosed(ctx, *t); err != nil {
				return err
			}
		}
		e, err := s.Repo.RunningExecution(ctx, t.ID)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		before := e.Clone()
		if err := e.Cancel(reason, now); err != nil {
			return err
		}
		if err := s.Repo.SaveExecution(ctx, &e); err != nil {
			return err
		}
		return s.cancelReleased(ctx, &before, &e)
	})
}

// Revision returns a stored plan revision of a task.
func (s *Service) Revision(ctx context.Context, taskID string, number int) (domain.PlanRevision, error) {
	return s.Repo.Revision(ctx, taskID, number)
}

// currentRevision checks that (revision, hash) is the task's current plan revision.
func (s *Service) currentRevision(ctx context.Context, t *domain.Task, revision int, hash string) error {
	if revision != t.PlanRevision {
		return domain.ErrStaleRevision
	}
	rev, err := s.Repo.Revision(ctx, t.ID, revision)
	if err != nil {
		return err
	}
	if rev.Hash() != hash {
		return domain.ErrStaleRevision
	}
	return nil
}

// ApprovePlan applies the Owner's execute approval of (revision, planHash): AWAITING_APPROVAL -> READY. Anything
// but the current revision and its hash is domain.ErrStaleRevision. The approvals context records the decision.
func (s *Service) ApprovePlan(ctx context.Context, taskID string, revision int, hash string) (domain.Task, error) {
	return s.updateTask(ctx, taskID, func(ctx context.Context, t *domain.Task, now time.Time) error {
		if err := s.currentRevision(ctx, t, revision, hash); err != nil {
			return err
		}
		return t.Approve(revision, now)
	})
}

// RejectPlan applies the Owner's rejection of (revision, planHash): AWAITING_APPROVAL -> ANALYZING for a new plan.
func (s *Service) RejectPlan(ctx context.Context, taskID string, revision int, hash string) (domain.Task, error) {
	return s.updateTask(ctx, taskID, func(ctx context.Context, t *domain.Task, now time.Time) error {
		if err := s.currentRevision(ctx, t, revision, hash); err != nil {
			return err
		}
		return t.RejectPlan(revision, now)
	})
}

// StepInfo is a step with the task and plan revision it runs under (its approved scope is in the revision).
type StepInfo struct {
	Task      domain.Task
	Execution domain.Execution
	Step      domain.Step
	Revision  domain.PlanRevision
}

// PlanStep returns the plan step of the revision with the step's key.
func (i StepInfo) PlanStep() (domain.PlanStep, bool) {
	for _, p := range i.Revision.Plan().Steps {
		if p.Key == i.Step.Key {
			return p, true
		}
	}
	return domain.PlanStep{}, false
}

// StepInfo returns the step with its task and plan revision.
func (s *Service) StepInfo(ctx context.Context, stepID string) (StepInfo, error) {
	e, err := s.Repo.ExecutionByStep(ctx, stepID)
	if err != nil {
		return StepInfo{}, err
	}
	st := e.Step(stepID)
	if st == nil {
		return StepInfo{}, domain.ErrNotFound
	}
	t, err := s.Repo.Task(ctx, e.TaskID)
	if err != nil {
		return StepInfo{}, err
	}
	rev, err := s.Repo.Revision(ctx, e.TaskID, e.PlanRevision)
	if err != nil {
		return StepInfo{}, err
	}
	return StepInfo{Task: t, Execution: e, Step: *st, Revision: rev}, nil
}

// HoldStep stops a step whose runner changed files beyond the approved scope until a new approval: the attempt is
// cancelled and the step WAITs with reason approval. runnerID and fence come from the authenticated runner report.
func (s *Service) HoldStep(ctx context.Context, stepID, runnerID string, fence int64, msg string) error {
	return s.updateExecution(ctx, stepID, func(_ context.Context, e *domain.Execution, now time.Time) error {
		return e.HoldForApproval(stepID, runnerID, fence, msg, now)
	})
}

// ResumeHeldStep returns a step held for approval (WAITING approval) to READY after the Owner approved the
// extended scope; a step waiting for anything else is domain.ErrInvalidTransition.
func (s *Service) ResumeHeldStep(ctx context.Context, stepID string) error {
	return s.updateExecution(ctx, stepID, func(_ context.Context, e *domain.Execution, now time.Time) error {
		if st := e.Step(stepID); st != nil && (st.State != domain.StepWaiting || st.WaitReason != domain.WaitApproval) {
			return fmt.Errorf("step %s is not held for approval: %w", st.Key, domain.ErrInvalidTransition)
		}
		return e.ResumeStep(stepID, now)
	})
}

// FinishedSteps returns, of stepIDs, the steps whose execution has finished with the time it finished; a step
// that does not exist (its task was deleted) maps to the zero time. Steps of a running execution are absent: the
// runners context keeps their records for fencing and reconciliation (KIN-52 retention).
func (s *Service) FinishedSteps(ctx context.Context, stepIDs []string) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	wanted, known := map[string]bool{}, map[string]bool{}
	for _, id := range stepIDs {
		wanted[id] = true
	}
	for _, id := range stepIDs {
		if known[id] {
			continue
		}
		e, err := s.Repo.ExecutionByStep(ctx, id)
		if errors.Is(err, domain.ErrNotFound) {
			known[id], out[id] = true, time.Time{}
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, st := range e.Steps { // one load answers for every step of the execution
			known[st.ID] = true
			if e.FinishedAt != nil && wanted[st.ID] {
				out[st.ID] = *e.FinishedAt
			}
		}
	}
	return out, nil
}
