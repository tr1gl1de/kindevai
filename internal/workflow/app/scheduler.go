package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// Candidate is a runner the scheduler may give a project's steps to (see domain.RunnerCandidate).
type Candidate = domain.RunnerCandidate

// RunnerPool is the runners context's API as the scheduler needs it.
type RunnerPool interface {
	Candidates(ctx context.Context, projectID string) ([]Candidate, error)
}

// AgentRouter is the routing context (KIN-33) as the scheduler needs it.
type AgentRouter interface {
	// Route returns the permitted CLI chain for a step of role in the project (System → Project → Role binding),
	// author being the CLI that did the work under review ("" when unknown), with the runners' CLI cool-downs.
	Route(ctx context.Context, projectID, role, author string) (AgentRoute, error)
}

// AgentRoute is routing's answer for one step.
type AgentRoute struct {
	Chain []domain.AgentChoice // permitted choices in order; empty: nothing permitted
	// Blocked explains an empty Chain (shown as the task's BLOCKED(agent) detail).
	Blocked string
	// Cooling reports a runner's CLI held after cli_limit (until a reset, or until the Owner acts); nil: none.
	Cooling func(runnerID, agent string) (domain.Cooling, bool)
}

// AgentMock is the agent of mock plans (the mock adapter, no AI): they are never routed.
const AgentMock = "mock"

// Assignment is one attempt the scheduler hands to a runner (the runner checks it again itself).
type Assignment struct {
	StepID      string
	StepKey     string
	Fence       int64
	ExecutionID string
	TaskID      string
	ProjectID   string
	Repository  string
	Scope       []string
	// Agent is the AI CLI routing chose (AgentMock for mock plans, "" without routing), Model its model ("" = the
	// CLI's default) and Role the step's platform role (KIN-33).
	Agent string
	Model string
	Role  string
	// Prompt is the step's prompt/context package for an AI CLI (KIN-79), Resume the native session it continues
	// ("" for a new one; the planner's repeat and replan on the same runner, KIN-34).
	Prompt string
	Resume string
	// The review loop (KIN-35): TestCommand is the repository's test command the runner runs in the worktree after an
	// implementer's CLI ("" none); DiffBase..DiffHead the work a reviewer reviews (the runner adds the bounded diff to
	// its prompt).
	TestCommand string
	DiffBase    string
	DiffHead    string
}

// TestCommands gives the test command configured for a project's repository (KIN-35; "" when none is).
type TestCommands interface {
	TestCommand(ctx context.Context, projectID, repository string) (string, error)
}

// Dispatcher queues runner commands in the use case's transaction (the runner's ordered command log, KIN-22), so a
// command commits or rolls back with the state change that caused it.
type Dispatcher interface {
	Assign(ctx context.Context, runnerID string, a Assignment) error
	// Cancel asks the runner to stop the attempt's process group.
	Cancel(ctx context.Context, runnerID, stepID string, fence int64, reason string) error
}

// Cancel reasons sent to runners.
const (
	CancelReasonCancelled = "cancelled"
	CancelReasonStale     = "stale_fence"
	// CancelReasonRunnerDisabled: the Owner disabled the runner (KIN-63).
	CancelReasonRunnerDisabled = "runner_" + ReleaseDisabled
	// CancelReasonScopeExceeded: the attempt changed files beyond the approved scope; the step waits for a new
	// approval (KIN-51).
	CancelReasonScopeExceeded = "scope_exceeded"
	// CancelReasonAgentStopped: the Owner's emergency stop of the attempt's AI CLI (KIN-33).
	CancelReasonAgentStopped = "agent_stopped"
)

// Wait details shown with a task BLOCKED for a runner.
const (
	DetailNoRunner   = "waiting for a runner: no approved, online runner of this project has the step's capabilities and disk space"
	DetailRunnerLost = "waiting for the runner to reconnect: the outcome of a running attempt is unknown until it reconciles"
	DetailNoAgent    = "waiting for an AI CLI: none is enabled for the step's role in this project"
	DetailCLILimit   = "waiting for a CLI usage limit to reset"
	// DetailPinnedRunner: a step of the review loop waits for the runner whose repository cache holds its work
	// branch (KIN-35; %s is the runner).
	DetailPinnedRunner = "waiting for runner %s: it holds the step's work branch (implementation, review and fixes run there)"
)

// routedWait: the wait reasons the scheduler itself sets and lifts by routing the step again.
func routedWait(r domain.WaitReason) bool {
	return r == domain.WaitRunner || r == domain.WaitAgent || r == domain.WaitCLILimit
}

// Schedule is one scheduler pass (KIN-23): leases that ran out are handled, READY tasks start their execution, and
// every running execution's schedulable steps are claimed for a candidate runner with an execution.assign command
// in the same transaction. A step without a candidate waits for a runner, and a task that cannot progress is
// BLOCKED with reason runner until a candidate appears (resumed automatically). While Pause reports maintenance
// mode only the lease pass runs.
func (s *Service) Schedule(ctx context.Context) error {
	if _, err := s.ExpireLeases(ctx); err != nil {
		return err
	}
	if s.Pause != nil {
		paused, err := s.Pause.SchedulingPaused(ctx)
		if err != nil {
			return err
		}
		if paused {
			return nil // maintenance mode: no new executions or step claims (KIN-41)
		}
	}
	var errs []error
	if err := s.schedulePlanning(ctx); err != nil {
		errs = append(errs, err)
	}
	ready, err := s.Repo.TasksInState(ctx, domain.TaskReady)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, id := range ready {
		if _, err := s.StartExecution(ctx, id); err != nil && !errors.Is(err, domain.ErrInvalidTransition) &&
			!errors.Is(err, domain.ErrConflict) {
			errs = append(errs, fmt.Errorf("starting task %s: %w", id, err))
		}
	}
	running, err := s.Repo.RunningExecutions(ctx)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, id := range running {
		if err := s.scheduleExecution(ctx, id); err != nil && !errors.Is(err, domain.ErrConflict) {
			errs = append(errs, fmt.Errorf("scheduling execution %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// schedulable: READY, WAITING for a runner, an AI CLI or a CLI limit without an unknown attempt (KIN-33), or
// WAITING for the network once its retry is due (KIN-31). A step a CLI limit held (KIN-79) waits for the limit's
// reset unless routing may pick another CLI or runner (routed: the runner's CLI cools down in routing meanwhile).
func schedulable(st *domain.Step, now time.Time, routed bool) bool {
	return st.State == domain.StepReady ||
		st.State == domain.StepWaiting && routedWait(st.WaitReason) && !st.AttemptUnknown() &&
			(st.WaitReason != domain.WaitCLILimit || routed || st.CLILimitRetryDue(now)) ||
		st.NetworkRetryDue(now)
}

func (s *Service) scheduleExecution(ctx context.Context, id string) error {
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		e, err := s.Repo.Execution(ctx, id)
		if err != nil || e.State != domain.ExecutionRunning {
			return err
		}
		t, err := s.Repo.Task(ctx, e.TaskID)
		if err != nil {
			return err
		}
		blockedForRunner := t.State == domain.TaskBlocked && routedWait(t.BlockReason) &&
			t.BlockedFrom == domain.TaskRunning
		if t.State != domain.TaskRunning && !blockedForRunner {
			return nil // blocked for something else, or finishing: not the scheduler's business
		}
		rev, err := s.Repo.Revision(ctx, e.TaskID, e.PlanRevision)
		if err != nil {
			return err
		}
		caps := map[string]domain.PlanStep{}
		for _, ps := range rev.Plan().Steps {
			caps[ps.Key] = ps
		}
		routed := s.Agents != nil && rev.CreatedBy() != MockPlanner
		if !slices.ContainsFunc(e.Steps, func(st domain.Step) bool { return schedulable(&st, s.now(), routed) }) &&
			!blockedForRunner && t.State == domain.TaskRunning {
			return s.blockIfStuck(ctx, &e, &t, nil) // nothing to assign: only the task's block state may change
		}
		cands, err := s.Runners.Candidates(ctx, t.ProjectID)
		if err != nil {
			return fmt.Errorf("runner candidates: %w", err)
		}
		held, err := s.Repo.HeldSteps(ctx) // in this transaction: earlier assignments of the pass are committed
		if err != nil {
			return fmt.Errorf("runner load: %w", err)
		}
		if held == nil {
			held = map[string]int{}
		}
		now := s.now()
		before := e.Clone()
		why := map[string]domain.Reason{}
		routes := map[[2]string]AgentRoute{} // role, author → route, read once per pass
		for i := range e.Steps {
			st := &e.Steps[i]
			if !schedulable(st, now, routed) {
				continue
			}
			ps := caps[st.Key]
			c, choice, reason, ok, err := s.route(ctx, routed, routes, &e, t.ProjectID, ps, st, cands, held)
			if err != nil {
				return err
			}
			if !ok {
				why[st.ID] = reason
				if st.State == domain.StepReady || st.State == domain.StepWaiting && routedWait(st.WaitReason) &&
					st.WaitReason != reason.Code {
					if err := e.WaitStep(st.ID, reason.Code, now); err != nil {
						return err
					}
				}
				continue
			}
			if t.State == domain.TaskBlocked {
				if err := t.Resume(now); err != nil {
					return err
				}
			}
			if st.State == domain.StepWaiting {
				if err := e.ResumeStep(st.ID, now); err != nil {
					return err
				}
			}
			claim, err := s.stepClaim(ctx, routed, t.ProjectID, ps, st, choice)
			if err != nil {
				// The registry could not be read: this step waits for the next pass, the others go on (never without
				// its test evidence).
				continue
			}
			l, err := e.ClaimStep(st.ID, c.ID, claim, now, s.leaseTTL())
			if errors.Is(err, domain.ErrParallelismLimit) {
				break
			}
			if err != nil {
				return err
			}
			if err := s.Dispatcher.Assign(ctx, c.ID, stepAssignment(&e, t, rev.Plan(), ps, st, choice, l.Fence)); err != nil {
				return fmt.Errorf("queueing the assignment: %w", err)
			}
			held[c.ID]++
		}
		if !executionEqual(before, e) {
			if err := s.Repo.SaveExecution(ctx, &e); err != nil {
				return err
			}
		}
		return s.blockIfStuck(ctx, &e, &t, why)
	})
}

// route picks the runner and AI CLI for a step (KIN-33). Unrouted (mock plans, or no router configured) it is a
// plain runner pick; routed, the permitted chain of the step's role decides, and the reason says why nothing fits.
func (s *Service) route(ctx context.Context, routed bool, routes map[[2]string]AgentRoute, e *domain.Execution,
	projectID string, ps domain.PlanStep, st *domain.Step, cands []Candidate, held map[string]int) (Candidate, domain.AgentChoice, domain.Reason, bool, error) {
	if st.PinnedRunner != "" {
		// The step's work branch lives in this runner's repository cache: its review and fix rounds run there only.
		cands = slices.DeleteFunc(slices.Clone(cands), func(c Candidate) bool { return c.ID != st.PinnedRunner })
		if len(cands) == 0 {
			return Candidate{}, domain.AgentChoice{}, domain.Reason{Code: domain.WaitRunner,
				Detail: fmt.Sprintf(DetailPinnedRunner, st.PinnedRunner)}, false, nil
		}
	}
	if st.Phase == domain.PhasePublish {
		// KIN-36: the runner's own git push of the reviewed head, no AI CLI to route.
		c, ok := domain.PickRunner(cands, ps.Capabilities, held)
		return c, domain.AgentChoice{Agent: domain.AgentPublish}, domain.Reason{Code: domain.WaitRunner, Detail: DetailNoRunner}, ok, nil
	}
	if !routed {
		c, ok := domain.PickRunner(cands, ps.Capabilities, held)
		var choice domain.AgentChoice
		if s.Agents != nil {
			choice.Agent = AgentMock
		}
		return c, choice, domain.Reason{Code: domain.WaitRunner, Detail: DetailNoRunner}, ok, nil
	}
	key := [2]string{ps.StepRole(), author(e, ps)}
	if st.Phase == domain.PhaseReview {
		key = [2]string{domain.StepRoleReviewer, st.Author()} // the independent review of the step's own work
	}
	r, cached := routes[key]
	if !cached {
		var err error
		if r, err = s.Agents.Route(ctx, projectID, key[0], key[1]); err != nil {
			return Candidate{}, domain.AgentChoice{}, domain.Reason{}, false, fmt.Errorf("routing step %s: %w", ps.Key, err)
		}
		routes[key] = r
	}
	if len(r.Chain) == 0 {
		detail := r.Blocked
		if detail == "" {
			detail = DetailNoAgent
		}
		return Candidate{}, domain.AgentChoice{}, domain.Reason{Code: domain.WaitAgent, Detail: detail}, false, nil
	}
	c, choice, reason, ok := domain.PickRoute(cands, ps.Capabilities, held, r.Chain, r.Cooling)
	return c, choice, reason, ok, nil
}

// author is the AI CLI that did the work a step reviews: the CLI the Control Plane assigned to the latest claim of
// a succeeded dependency (never what a runner reports), the most recently finished one first; "" when none is
// known (mock, no dependency).
func author(e *domain.Execution, ps domain.PlanStep) string {
	if ps.StepRole() != domain.StepRoleReviewer {
		return ""
	}
	var best string
	var at time.Time
	for _, key := range ps.DependsOn {
		d := e.StepByKey(key)
		if d == nil || d.State != domain.StepSucceeded || d.Agent == "" || d.Agent == AgentMock || len(d.Attempts) == 0 {
			continue
		}
		last := d.Attempts[len(d.Attempts)-1]
		if f := last.FinishedAt; best == "" || f != nil && f.After(at) {
			best = d.Agent
			if a := d.Author(); a != "" {
				best = a // the step ran its own review loop (KIN-35): its implementer wrote the work, not its reviewer
			}
			if f != nil {
				at = *f
			}
		}
	}
	return best
}

// stepClaim decides the review loop for a claim (KIN-35): a routed implementer step on an AI CLI runs it, and its
// implementation attempts get the repository's test command.
func (s *Service) stepClaim(ctx context.Context, routed bool, projectID string, ps domain.PlanStep, st *domain.Step,
	choice domain.AgentChoice) (domain.StepClaim, error) {
	c := domain.StepClaim{Agent: choice,
		Review: routed && ps.StepRole() == domain.StepRoleImplementer && choice.Agent != "" && choice.Agent != AgentMock}
	if (c.Review || st.Review) && st.Phase == domain.PhaseImplement && s.TestCommands != nil {
		cmd, err := s.TestCommands.TestCommand(ctx, projectID, ps.Repository)
		if err != nil {
			return domain.StepClaim{}, fmt.Errorf("test command of %s: %w", ps.Repository, err)
		}
		c.TestCommand = cmd
	}
	return c, nil
}

// stepAssignment is the execution.assign of a claimed step: outside the review loop the plain step prompt; in it the
// implementer's prompt (plan step, test command, fix feedback) or the reviewer's (the work base..head, the runner's
// test evidence, the implementer's handoff).
func stepAssignment(e *domain.Execution, t domain.Task, plan domain.Plan, ps domain.PlanStep, st *domain.Step,
	choice domain.AgentChoice, fence int64) Assignment {
	a := Assignment{StepID: st.ID, StepKey: st.Key, Fence: fence, ExecutionID: e.ID, TaskID: t.ID, ProjectID: t.ProjectID,
		Repository: ps.Repository, Scope: ps.Scope, Agent: choice.Agent, Model: choice.Model, Role: ps.StepRole(),
		Prompt: domain.StepPrompt(t, plan, ps)}
	switch {
	case !st.Review:
	case st.Phase == domain.PhasePublish:
		// KIN-36: push exactly the reviewed head; the runner derives the branch from the task and step.
		impl, _ := st.LastImplementation()
		a.Role, a.Model, a.Prompt, a.DiffHead = domain.StepRolePublisher, "", "", impl.HeadSHA
	case st.Phase == domain.PhaseReview:
		impl, _ := st.LastImplementation()
		a.Role, a.DiffBase, a.DiffHead = domain.StepRoleReviewer, impl.BaseSHA, impl.HeadSHA
		a.Prompt = domain.ReviewPrompt(domain.ReviewInput{Task: t, Plan: plan, Step: ps,
			Handoff: domain.ParseHandoff(impl.Report.Result), Files: impl.Files, Tests: impl.Tests, BaseSHA: impl.BaseSHA,
			HeadSHA: impl.HeadSHA, Round: st.Round})
	default:
		a.TestCommand = st.TestCommand
		a.Prompt = domain.ImplementerPrompt(domain.ImplementerInput{Task: t, Plan: plan, Step: ps, TestCommand: st.TestCommand,
			Feedback: st.RetryFeedback(), Round: st.Round})
	}
	return a
}

func executionEqual(a, b domain.Execution) bool {
	if len(a.Steps) != len(b.Steps) {
		return false
	}
	for i := range a.Steps {
		x, y := a.Steps[i], b.Steps[i]
		if x.State != y.State || x.Fence != y.Fence || x.WaitReason != y.WaitReason || x.RunnerID != y.RunnerID ||
			x.Agent != y.Agent || x.Model != y.Model || x.Review != y.Review || x.Phase != y.Phase || x.Round != y.Round ||
			x.PinnedRunner != y.PinnedRunner || x.TestCommand != y.TestCommand || x.AttemptBase != y.AttemptBase {
			return false
		}
	}
	return true
}

// blockIfStuck saves the task BLOCKED while a step waits for a runner, an AI CLI or a CLI limit and none is claimed
// or running, and resumes it otherwise. The reason is the first waiting step's (why: this pass's routing reasons
// by step id; nil keeps the task's current reason); a blocked task whose reason changed is re-blocked with the new
// one. t is saved only when it changed (or was resumed above).
func (s *Service) blockIfStuck(ctx context.Context, e *domain.Execution, t *domain.Task, why map[string]domain.Reason) error {
	waiting, lost, active := false, false, false
	var reason *domain.Reason
	for i := range e.Steps {
		st := &e.Steps[i]
		switch {
		case st.State == domain.StepClaimed || st.State == domain.StepRunning:
			active = true
		case st.State == domain.StepWaiting && routedWait(st.WaitReason):
			waiting = true
			lost = lost || st.AttemptUnknown()
			if r, ok := why[st.ID]; ok && reason == nil {
				reason = &r
			}
		}
	}
	now := s.now()
	stored, err := s.Repo.Task(ctx, t.ID)
	if err != nil {
		return err
	}
	stuck := waiting && !active
	want := domain.Reason{Code: domain.WaitRunner, Detail: DetailNoRunner}
	switch {
	case lost:
		want.Detail = DetailRunnerLost
	case reason != nil:
		want = *reason
	}
	ours := t.State == domain.TaskBlocked && routedWait(t.BlockReason) && t.BlockedFrom == domain.TaskRunning
	switch {
	case stuck && t.State == domain.TaskRunning:
		if err := t.Transition(domain.TaskBlocked, want, now); err != nil {
			return err
		}
	case stuck && ours && (lost || reason != nil) && t.Reason != want:
		if err := t.Resume(now); err != nil {
			return err
		}
		if err := t.Transition(domain.TaskBlocked, want, now); err != nil {
			return err
		}
	case !stuck && ours:
		if err := t.Resume(now); err != nil {
			return err
		}
	}
	if t.State == stored.State && t.Reason == stored.Reason {
		return nil
	}
	return s.saveTask(ctx, t)
}

// RenewRunnerLeases extends the lease of every RUNNING step the runner holds (its heartbeat, KIN-20). A CLAIMED
// step is not renewed: its runner has not started it, so if the assignment was lost the lease returns it to READY
// (a late start is then stale and cancelled). A lease that already ran out is not renewed either: that attempt is
// reconciled instead.
func (s *Service) RenewRunnerLeases(ctx context.Context, runnerID string) error {
	ids, err := s.Repo.ExecutionsHeldBy(ctx, runnerID)
	if err != nil {
		return err
	}
	var errs []error
	if err := s.renewPlanningLeases(ctx, runnerID); err != nil {
		errs = append(errs, err)
	}
	for _, id := range ids {
		err := s.Tx.InTx(ctx, func(ctx context.Context) error {
			e, err := s.Repo.Execution(ctx, id)
			if err != nil {
				return err
			}
			now, renewed := s.now(), false
			for _, st := range e.Steps {
				if st.RunnerID != runnerID || st.State != domain.StepRunning {
					continue
				}
				if _, err := e.Renew(st.ID, runnerID, st.Fence, now, s.leaseTTL()); err == nil {
					renewed = true
				}
			}
			if !renewed {
				return nil
			}
			return s.Repo.SaveExecution(ctx, &e)
		})
		if err != nil && !errors.Is(err, domain.ErrConflict) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// AttemptReport is one attempt in a runner's reconciliation response.
type AttemptReport struct {
	StepID  string
	Fence   int64
	Running bool // the process is alive
	Result  bool // finished; its terminal event is still in the runner's spool and arrives as an event
}

// ReconcileRunner applies a runner's reconciliation (sent whenever its session opens, KIN-22/23). For every step
// the runner holds: a live process brings an unknown attempt back to RUNNING under the same token (no second
// process); a kept result is left to its spooled event; no process and no result fails the attempt (retried within
// the budget). A CLAIMED step the runner does not know yet is left alone: its assignment may still be in flight,
// and its lease returns it to READY otherwise. A running process the Control Plane no longer assigns to the runner
// (superseded token, finished execution) gets an execution.cancel.
func (s *Service) ReconcileRunner(ctx context.Context, runnerID string, reports []AttemptReport) error {
	if _, err := s.ExpireLeases(ctx); err != nil {
		return err
	}
	var planning []AttemptReport
	reports = slices.DeleteFunc(slices.Clone(reports), func(r AttemptReport) bool {
		_, ok := domain.PlanningRunID(r.StepID)
		if ok {
			planning = append(planning, r)
		}
		return ok
	})
	planErr := s.reconcilePlanning(ctx, runnerID, planning)
	ids, err := s.Repo.ExecutionsHeldBy(ctx, runnerID)
	if err != nil {
		return err
	}
	type ak struct {
		step  string
		fence int64
	}
	byKey := map[ak]AttemptReport{}
	for _, r := range reports {
		byKey[ak{r.StepID, r.Fence}] = r
	}
	matched := map[ak]bool{}
	var errs []error
	for _, id := range ids {
		err := s.updateExecutionByID(ctx, id, func(_ context.Context, e *domain.Execution, now time.Time) error {
			for i := range e.Steps {
				st := &e.Steps[i]
				if st.RunnerID != runnerID || !st.Holding() {
					continue
				}
				k := ak{st.ID, st.Fence}
				r, known := byKey[k]
				matched[k] = known
				switch {
				case known && r.Result:
				case known && r.Running && st.AttemptUnknown():
					if err := e.Reconcile(st.ID, domain.ProcessAlive, now, s.leaseTTL(), s.maxAttempts()); err != nil {
						return err
					}
				case known && r.Running && st.State == domain.StepRunning:
					if _, err := e.Renew(st.ID, runnerID, st.Fence, now, s.leaseTTL()); err != nil &&
						!errors.Is(err, domain.ErrLeaseExpired) {
						return err
					}
				case known || st.State == domain.StepClaimed:
				case st.AttemptUnknown():
					if err := e.Reconcile(st.ID, domain.ProcessGone, now, s.leaseTTL(), s.maxAttempts()); err != nil {
						return err
					}
				default: // RUNNING, but the runner has no such process and no result
					if err := e.Fail(st.ID, runnerID, st.Fence, "process lost: the runner has no process and no result for this attempt",
						now, s.maxAttempts()); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("reconciling execution %s: %w", id, err))
		}
	}
	if s.Dispatcher != nil {
		err := s.Tx.InTx(ctx, func(ctx context.Context) error {
			for _, r := range reports {
				if !r.Running || matched[ak{r.StepID, r.Fence}] {
					continue
				}
				if err := s.Dispatcher.Cancel(ctx, runnerID, r.StepID, r.Fence, CancelReasonStale); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(append(errs, planErr)...)
}

// Release reasons of ReleaseRunner (KIN-63).
const (
	ReleaseDisabled = "disabled"
	ReleaseRevoked  = "revoked"
)

// ReleaseRunner takes every attempt away from a runner the Owner disabled or revoked (KIN-63). It joins the
// caller's transaction, so the attempts end together with the runner's status change: a running or unknown attempt
// fails ("runner disabled/revoked by the Owner") and is retried on another runner within the budget, a claimed step
// returns to READY. A disabled runner gets an execution.cancel (runner_disabled) for every released attempt, delivered
// from its command log when it connects again. A revoked runner never connects again, so nothing is queued for it
// (it would stay in the log for good): it stops its processes itself (fail-closed, close code 4410).
func (s *Service) ReleaseRunner(ctx context.Context, runnerID, reason string) error {
	if reason != ReleaseDisabled && reason != ReleaseRevoked {
		return fmt.Errorf("unknown runner release reason %q", reason)
	}
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		ids, err := s.Repo.ExecutionsHeldBy(ctx, runnerID)
		if err != nil {
			return err
		}
		for _, id := range ids {
			var released []domain.Released
			err := s.updateExecutionByID(ctx, id, func(_ context.Context, e *domain.Execution, now time.Time) error {
				released = e.ReleaseRunner(runnerID, "runner "+reason+" by the Owner", now, s.maxAttempts())
				return nil
			})
			if err != nil {
				return fmt.Errorf("releasing execution %s: %w", id, err)
			}
			if s.Dispatcher == nil || reason == ReleaseRevoked {
				continue
			}
			for _, r := range released {
				if err := s.Dispatcher.Cancel(ctx, runnerID, r.StepID, r.Fence, CancelReasonRunnerDisabled); err != nil {
					return fmt.Errorf("queueing the cancel of step %s: %w", r.StepID, err)
				}
			}
		}
		return nil
	})
}

// cancelReleased asks runners to stop the processes of steps the change cancelled (an execution that failed or
// was cancelled cancels its other steps).
func (s *Service) cancelReleased(ctx context.Context, before, after *domain.Execution) error {
	if s.Dispatcher == nil {
		return nil
	}
	for _, b := range before.Steps {
		a := after.Step(b.ID)
		if !b.Holding() || a == nil || a.State != domain.StepCancelled {
			continue
		}
		if err := s.Dispatcher.Cancel(ctx, b.RunnerID, b.ID, b.Fence, CancelReasonCancelled); err != nil {
			return fmt.Errorf("queueing the cancel of step %s: %w", b.Key, err)
		}
	}
	return nil
}

// StopAgent is the Owner's emergency stop of an AI CLI (KIN-33): every attempt running (or claimed) with it is taken
// away at once — claimed steps return to READY, running or unknown attempts are cancelled without using the retry
// budget and their steps WAIT for an agent — and each runner gets an execution.cancel (agent_stopped) in the same
// transaction. The steps are routed again by the next pass. It joins the caller's transaction (the routing context
// disables the CLI globally in the same one) and returns the number of attempts stopped.
func (s *Service) StopAgent(ctx context.Context, agent string) (int, error) {
	n := 0
	err := s.Tx.InTx(ctx, func(ctx context.Context) error {
		ids, err := s.Repo.RunningExecutions(ctx)
		if err != nil {
			return err
		}
		for _, id := range ids {
			var released []domain.AgentReleased
			err := s.updateExecutionByID(ctx, id, func(_ context.Context, e *domain.Execution, now time.Time) error {
				released = e.ReleaseAgent(agent, "AI CLI "+agent+" stopped by the Owner (emergency stop)", now)
				return nil
			})
			if err != nil {
				return fmt.Errorf("stopping %s in execution %s: %w", agent, id, err)
			}
			n += len(released)
			if s.Dispatcher == nil {
				continue
			}
			for _, r := range released {
				if err := s.Dispatcher.Cancel(ctx, r.RunnerID, r.StepID, r.Fence, CancelReasonAgentStopped); err != nil {
					return fmt.Errorf("queueing the cancel of step %s: %w", r.StepID, err)
				}
			}
		}
		return nil
	})
	return n, err
}
