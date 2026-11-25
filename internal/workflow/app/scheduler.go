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
)

// Wait details shown with a task BLOCKED for a runner.
const (
	DetailNoRunner   = "waiting for a runner: no approved, online runner of this project has the step's capabilities and disk space"
	DetailRunnerLost = "waiting for the runner to reconnect: the outcome of a running attempt is unknown until it reconciles"
)

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
	ready, err := s.Repo.TasksInState(ctx, domain.TaskReady)
	if err != nil {
		return err
	}
	var errs []error
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

// schedulable: READY, WAITING for a runner without an unknown attempt, or WAITING for the network once its retry
// is due (KIN-31).
func schedulable(st *domain.Step, now time.Time) bool {
	return st.State == domain.StepReady ||
		st.State == domain.StepWaiting && st.WaitReason == domain.WaitRunner && !st.AttemptUnknown() ||
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
		blockedForRunner := t.State == domain.TaskBlocked && t.BlockReason == domain.WaitRunner &&
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
		if !slices.ContainsFunc(e.Steps, func(st domain.Step) bool { return schedulable(&st, s.now()) }) &&
			!blockedForRunner && t.State == domain.TaskRunning {
			return s.blockIfStuck(ctx, &e, &t) // nothing to assign: only the task's block state may change
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
		for i := range e.Steps {
			st := &e.Steps[i]
			if !schedulable(st, now) {
				continue
			}
			ps := caps[st.Key]
			c, ok := domain.PickRunner(cands, ps.Capabilities, held)
			if !ok {
				if st.State == domain.StepReady {
					if err := e.WaitStep(st.ID, domain.WaitRunner, now); err != nil {
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
			l, err := e.Claim(st.ID, c.ID, now, s.leaseTTL())
			if errors.Is(err, domain.ErrParallelismLimit) {
				break
			}
			if err != nil {
				return err
			}
			if err := s.Dispatcher.Assign(ctx, c.ID, Assignment{StepID: st.ID, StepKey: st.Key, Fence: l.Fence,
				ExecutionID: e.ID, TaskID: t.ID, ProjectID: t.ProjectID, Repository: ps.Repository, Scope: ps.Scope}); err != nil {
				return fmt.Errorf("queueing the assignment: %w", err)
			}
			held[c.ID]++
		}
		if !executionEqual(before, e) {
			if err := s.Repo.SaveExecution(ctx, &e); err != nil {
				return err
			}
		}
		return s.blockIfStuck(ctx, &e, &t)
	})
}

func executionEqual(a, b domain.Execution) bool {
	if len(a.Steps) != len(b.Steps) {
		return false
	}
	for i := range a.Steps {
		x, y := a.Steps[i], b.Steps[i]
		if x.State != y.State || x.Fence != y.Fence || x.WaitReason != y.WaitReason || x.RunnerID != y.RunnerID {
			return false
		}
	}
	return true
}

// blockIfStuck saves the task BLOCKED(runner) while a step waits for a runner and none is claimed or running, and
// resumes it otherwise. t is saved only when it changed (or was resumed above).
func (s *Service) blockIfStuck(ctx context.Context, e *domain.Execution, t *domain.Task) error {
	waiting, lost, active := false, false, false
	for i := range e.Steps {
		st := &e.Steps[i]
		switch {
		case st.State == domain.StepClaimed || st.State == domain.StepRunning:
			active = true
		case st.State == domain.StepWaiting && st.WaitReason == domain.WaitRunner:
			waiting = true
			lost = lost || st.AttemptUnknown()
		}
	}
	now := s.now()
	stored, err := s.Repo.Task(ctx, t.ID)
	if err != nil {
		return err
	}
	stuck := waiting && !active
	switch {
	case stuck && t.State == domain.TaskRunning:
		detail := DetailNoRunner
		if lost {
			detail = DetailRunnerLost
		}
		if err := t.Transition(domain.TaskBlocked, domain.Reason{Code: domain.WaitRunner, Detail: detail}, now); err != nil {
			return err
		}
	case !stuck && t.State == domain.TaskBlocked && t.BlockReason == domain.WaitRunner:
		if err := t.Resume(now); err != nil {
			return err
		}
	}
	if t.State == stored.State && t.Reason == stored.Reason {
		return nil
	}
	return s.Repo.SaveTask(ctx, t)
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
	return errors.Join(errs...)
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
