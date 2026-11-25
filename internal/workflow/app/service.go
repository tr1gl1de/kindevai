// Package app holds the workflow use cases: tasks move through their FSM, planners submit immutable plan
// revisions, and runners claim, start, renew, complete or fail execution steps under a lease and fencing token
// (KIN-A-14, KIN-A-15). Every use case is one transaction over whole aggregates.
package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	// HeldSteps counts, per runner id, the steps of running executions the runner holds (Step.Holding: CLAIMED,
	// RUNNING or an unknown attempt) — the scheduler's load measure (KIN-65).
	HeldSteps(ctx context.Context) (map[string]int, error)
	ListTasks(ctx context.Context, f TaskFilter, limit int) ([]domain.Task, error) // newest first
	LatestExecution(ctx context.Context, taskID string) (domain.Execution, error)  // domain.ErrNotFound
	// EachAttempt calls fn for every step attempt of the tasks f selects (agent time, KIN-25).
	EachAttempt(ctx context.Context, f TaskFilter, fn func(taskID string, a domain.Attempt) error) error
	// ExecutionSpans returns, per task, when its first execution started and its last one ended.
	ExecutionSpans(ctx context.Context, taskIDs []string) (map[string]ExecutionSpan, error)
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
	Agents      AgentRouter  // AI CLI routing (KIN-33); nil: steps are not routed (every attempt runs the mock agent)
	Dispatcher  Dispatcher   // runner commands; required for Schedule, optional otherwise
	Access      Access       // project visibility for the task API (KIN-23)
	Outbox      Outbox       // alert hooks (KIN-60); nil: none
	CLILimits   CLILimits    // KIN-79: a CLI that hit its limit (KIN-33 routing cool-downs); nil: none
	Log         AttemptLog   // attempt events for the task API (KIN-25); optional
	Tx          TxRunner
	Now         func() time.Time
	NewID       func() string
	LeaseTTL    time.Duration // DefaultLeaseTTL when zero
	MaxAttempts int           // DefaultMaxAttempts when zero
	Pause       Pause         // optional: maintenance mode (KIN-41)
	// Planning stores planner runs (KIN-34); nil: the planner step is off. IssueContext reads a YouTrack issue's
	// context package from the outbox handler (with Outbox); nil: planners get the task title only.
	Planning     PlanningRepository
	IssueContext IssueContextSource
	// Discussion stores the plan discussion (KIN-43; with Planning); nil: the plan chat is off. Audit records its
	// messages, answers and limits (KIN-16); nil: not audited.
	Discussion DiscussionRepository
	Audit      AuditLog
}

// Pause reports whether the scheduler must not hand out new work (maintenance mode, KIN-41). Running attempts
// continue (drain); leases still expire and runners still reconcile.
type Pause interface {
	SchedulingPaused(ctx context.Context) (bool, error)
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
			if a, b := x.Attempts[j], y.Attempts[j]; a.State != b.State || a.Agent != b.Agent || a.Report != b.Report ||
				a.WorkIncomplete != b.WorkIncomplete || !ptrTimeEqual(a.WorkStart, b.WorkStart) || !ptrTimeEqual(a.WorkEnd, b.WorkEnd) {
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

// StartStep records that the runner launched the claimed step (at the Control Plane's time; see StartStepAt).
func (s *Service) StartStep(ctx context.Context, stepID, runnerID string, fence int64) error {
	return s.StartStepAt(ctx, stepID, runnerID, fence, s.now(), "")
}

// StartStepAt records that the runner launched the claimed step: at is the execution.started event's time on the
// runner's clock and agent the CLI adapter running it; both start the attempt's agent time (KIN-25).
func (s *Service) StartStepAt(ctx context.Context, stepID, runnerID string, fence int64, at time.Time, agent string) error {
	return s.updateExecution(ctx, stepID, func(_ context.Context, e *domain.Execution, now time.Time) error {
		if err := e.Start(stepID, runnerID, fence, now, s.leaseTTL()); err != nil {
			return err
		}
		e.MarkWorkStart(stepID, runnerID, fence, agent, at, now)
		return nil
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

// CompleteStep records the step's success and promotes its dependents (see CompleteStepAt).
func (s *Service) CompleteStep(ctx context.Context, stepID, runnerID string, fence int64) error {
	return s.CompleteStepAt(ctx, stepID, runnerID, fence, s.now())
}

// CompleteStepAt is CompleteStep with the event's time on the runner's clock, which ends the attempt's agent time.
func (s *Service) CompleteStepAt(ctx context.Context, stepID, runnerID string, fence int64, at time.Time) error {
	err := s.updateExecution(ctx, stepID, func(_ context.Context, e *domain.Execution, now time.Time) error {
		if err := e.Succeed(stepID, runnerID, fence, now); err != nil {
			return refusedReport{err}
		}
		e.MarkWorkEnd(stepID, runnerID, fence, at, now)
		return s.cliWorked(ctx, e.Step(stepID), runnerID)
	})
	return s.lateWorkEnd(ctx, err, stepID, runnerID, fence, at)
}

// cliWorked tells routing that the step's CLI worked on the runner (KIN-83: its hold streak is over).
func (s *Service) cliWorked(ctx context.Context, st *domain.Step, runnerID string) error {
	if s.CLILimits == nil || st == nil || len(st.Attempts) == 0 {
		return nil
	}
	if agent := st.Attempts[len(st.Attempts)-1].Agent; agent != "" {
		return s.CLILimits.CLIWorked(ctx, runnerID, agent)
	}
	return nil
}

// FailStep records a failed attempt; the step is retried within the attempt budget, else the task fails.
func (s *Service) FailStep(ctx context.Context, stepID, runnerID string, fence int64, msg string) error {
	return s.FailStepAt(ctx, stepID, runnerID, fence, msg, s.now())
}

// FailStepAt is FailStep with the event's time on the runner's clock, which ends the attempt's agent time.
func (s *Service) FailStepAt(ctx context.Context, stepID, runnerID string, fence int64, msg string, at time.Time) error {
	err := s.updateExecution(ctx, stepID, func(_ context.Context, e *domain.Execution, now time.Time) error {
		if err := e.Fail(stepID, runnerID, fence, msg, now, s.maxAttempts()); err != nil {
			return refusedReport{err}
		}
		e.MarkWorkEnd(stepID, runnerID, fence, at, now)
		return nil
	})
	return s.lateWorkEnd(ctx, err, stepID, runnerID, fence, at)
}

// HoldStepForNetwork parks a step whose runner could not build or verify the strict egress sandbox (KIN-31): the
// attempt never ran unconfined; the step WAITs with reason network (no retry budget used) and the scheduler
// assigns it again after domain.NetworkRetryDelayAfter its holds in a row; from domain.NetworkAlertAfter holds in a
// row on the runner the Owner gets an egress alert (KIN-81). at ends the attempt's agent time.
func (s *Service) HoldStepForNetwork(ctx context.Context, stepID, runnerID string, fence int64, msg string, at time.Time) error {
	err := s.updateExecution(ctx, stepID, func(ctx context.Context, e *domain.Execution, now time.Time) error {
		if err := e.HoldForNetwork(stepID, runnerID, fence, msg, now); err != nil {
			return refusedReport{err}
		}
		e.MarkWorkEnd(stepID, runnerID, fence, at, now)
		return s.alertNetworkHolds(ctx, e.Step(stepID), runnerID) // KIN-81
	})
	return s.lateWorkEnd(ctx, err, stepID, runnerID, fence, at)
}

// CLILimits is routing's per-runner CLI hold (KIN-33, KIN-79, KIN-83), in the report's transaction: CLILimitHit
// records a cli_limit report (cause rate_limit | auth | billing, the reset the CLI named or zero) and returns the
// pair's hold; CLIWorked ends the pair's streak after a successful attempt.
type CLILimits interface {
	CLILimitHit(ctx context.Context, runnerID, agent, cause string, resetsAt time.Time, reason string) (CLIHold, error)
	CLIWorked(ctx context.Context, runnerID, agent string) error
}

// CLIHold is a runner's CLI hold as routing decided it: until when, or (NeedsOwner) until the Owner marks the CLI
// ready again. Known is false when routing keeps no hold for the attempt's agent (the mock adapter).
type CLIHold struct {
	Known      bool
	Until      time.Time
	NeedsOwner bool
}

// HoldStepForCLILimit parks a step whose AI CLI hit its subscription/rate limit or is logged out (KIN-79): the
// attempt never counts against the retry budget, the step WAITs with reason cli_limit (until resetsAt, see
// domain.Execution.HoldForCLILimit, or the hold routing decided: a logged-out or billing-refused CLI backs off and
// finally waits for the Owner, KIN-83) and the task is BLOCKED(cli_limit) (an alert for the Owner) until the
// scheduler assigns the step again. cause is the runner's (rate_limit | auth | billing). at ends the attempt's
// agent time.
func (s *Service) HoldStepForCLILimit(ctx context.Context, stepID, runnerID string, fence int64, cause, msg string, resetsAt, at time.Time) error {
	err := s.updateExecution(ctx, stepID, func(ctx context.Context, e *domain.Execution, now time.Time) error {
		if err := e.HoldForCLILimit(stepID, runnerID, fence, msg, resetsAt, now); err != nil {
			return refusedReport{err}
		}
		e.MarkWorkEnd(stepID, runnerID, fence, at, now)
		st := e.Step(stepID)
		var hold CLIHold
		if s.CLILimits != nil && st != nil {
			agent := ""
			if a := st.Attempts; len(a) > 0 {
				agent = a[len(a)-1].Agent
			}
			h, err := s.CLILimits.CLILimitHit(ctx, runnerID, agent, cause, resetsAt, msg)
			if err != nil {
				return err
			}
			if hold = h; h.Known {
				if err := e.RetryCLILimitAt(stepID, h.Until); err != nil {
					return err
				}
			}
		}
		t, err := s.Repo.Task(ctx, e.TaskID)
		if err != nil {
			return err
		}
		if t.State != domain.TaskRunning {
			return nil
		}
		detail := msg
		switch {
		case hold.NeedsOwner:
			detail += " (waiting for the Owner: log the CLI in on runner " + runnerID +
				" or fix its billing, then mark it ready again on the Agents page)"
		case st != nil && st.WaitUntil != nil:
			detail += " (retry at " + st.WaitUntil.Format(time.RFC3339) + ")"
		}
		if err := t.Transition(domain.TaskBlocked, domain.Reason{Code: domain.WaitCLILimit, Detail: detail}, now); err != nil {
			return err
		}
		return s.saveTask(ctx, &t)
	})
	return s.lateWorkEnd(ctx, err, stepID, runnerID, fence, at)
}

// RecordAgentReport stores what the attempt's AI CLI reported (usage, native session, model, version; KIN-79,
// agent time accounting of KB KIN-A-16) with the attempt of runnerID under fence, whatever the step's state now.
// An attempt the runner does not hold is domain.ErrStaleFence.
func (s *Service) RecordAgentReport(ctx context.Context, stepID, runnerID string, fence int64, r domain.AgentReport) error {
	return s.updateExecution(ctx, stepID, func(_ context.Context, e *domain.Execution, _ time.Time) error {
		st := e.Step(stepID)
		if st == nil {
			return domain.ErrNotFound
		}
		if !slices.ContainsFunc(st.Attempts, func(a domain.Attempt) bool { return a.RunnerID == runnerID && a.Fence == fence }) {
			return fmt.Errorf("attempt %d of step %s on runner %s: %w", fence, st.Key, runnerID, domain.ErrStaleFence)
		}
		e.RecordAgentReport(stepID, runnerID, fence, r)
		return nil
	})
}

// refusedReport marks the execution's own refusal of a terminal report (stale token, step already moved on), as
// opposed to an error from saving or from the task's follow-up transition: only a refusal records a late end.
type refusedReport struct{ err error }

func (r refusedReport) Error() string { return r.err.Error() }
func (r refusedReport) Unwrap() error { return r.err }

// lateWorkEnd handles a terminal report the workflow refused (stale token or a step that already moved on, e.g.
// after a cancellation or a lost-process reconciliation): the runner still says when the attempt's process ended,
// so its agent time is closed (once) even though the step's state is not changed. The refusal err is returned.
func (s *Service) lateWorkEnd(ctx context.Context, err error, stepID, runnerID string, fence int64, at time.Time) error {
	var refused refusedReport
	if !errors.As(err, &refused) {
		return err
	}
	err = refused.err
	if !errors.Is(err, domain.ErrStaleFence) && !errors.Is(err, domain.ErrInvalidTransition) {
		return err
	}
	lerr := s.Tx.InTx(ctx, func(ctx context.Context) error {
		e, err := s.Repo.ExecutionByStep(ctx, stepID)
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !e.MarkWorkEnd(stepID, runnerID, fence, at, s.now()) {
			return nil
		}
		return s.Repo.SaveExecution(ctx, &e)
	})
	if lerr != nil {
		return fmt.Errorf("recording the attempt's end: %w", lerr)
	}
	return err
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
		if err := s.cancelPlanning(ctx, t.ID, reason, now); err != nil {
			return err
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
// cancelled, the step WAITs with reason approval and the runner is told to stop the attempt's process in the same
// transaction (KIN-51). runnerID and fence come from the authenticated runner report.
func (s *Service) HoldStep(ctx context.Context, stepID, runnerID string, fence int64, msg string) error {
	return s.updateExecution(ctx, stepID, func(ctx context.Context, e *domain.Execution, now time.Time) error {
		if err := e.HoldForApproval(stepID, runnerID, fence, msg, now); err != nil {
			return err
		}
		if s.Dispatcher == nil {
			return nil
		}
		return s.Dispatcher.Cancel(ctx, runnerID, stepID, fence, CancelReasonScopeExceeded)
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
		if _, ok := domain.PlanningRunID(id); ok {
			at, done, err := s.finishedPlanningStep(ctx, id)
			if err != nil {
				return nil, err
			}
			if known[id] = true; done {
				out[id] = at
			}
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
