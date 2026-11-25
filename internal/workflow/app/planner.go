package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// The planner step (KIN-34): a task is analysed by the AI CLI bound to the planner role (KIN-33) on a runner. The
// Control Plane builds the context package (the task, the issue text and linked articles read from YouTrack
// through the outbox), the runner runs the CLI read-only in a worktree of the project's first repository like any
// attempt (execution.assign, role planner, empty scope, never the permission bypass), and the CLI's final answer
// is parsed strictly as a plan. A valid plan becomes the next plan revision through SubmitPlan (W4: policy, then
// READY or AWAITING_APPROVAL); an invalid answer or a failed run gets one repeat, told why, then the task FAILS.

// PlanningRepository stores planning runs on the caller's transaction (optimistic concurrency like Repository).
type PlanningRepository interface {
	CreatePlanningRun(ctx context.Context, r *domain.PlanningRun) error // domain.ErrConflict: the task has an active run
	PlanningRun(ctx context.Context, id string) (domain.PlanningRun, error)
	LatestPlanningRun(ctx context.Context, taskID string) (domain.PlanningRun, error) // domain.ErrNotFound
	// LatestPlanRun is the task's newest run of kind plan (not a chat turn); domain.ErrNotFound.
	LatestPlanRun(ctx context.Context, taskID string) (domain.PlanningRun, error)
	SavePlanningRun(ctx context.Context, r *domain.PlanningRun) error
	// ActivePlanningRuns lists the runs that have not ended (the scheduler's work).
	ActivePlanningRuns(ctx context.Context) ([]string, error)
	// PlanningRunsHeldBy lists the RUNNING runs whose current attempt is the runner's.
	PlanningRunsHeldBy(ctx context.Context, runnerID string) ([]string, error)
}

// IssueContextSource reads the context package of a YouTrack issue: its text and the Knowledge Base articles it
// links, bounded. It calls YouTrack, so it only runs from the outbox handler. Errors wrapping
// ErrContextUnavailable are final (not configured, issue gone); others are retried.
type IssueContextSource interface {
	IssueContext(ctx context.Context, projectID, issueID string) (domain.PlanningContext, error)
}

// ErrPlannerUnavailable: the planner step is not configured in this process.
var ErrPlannerUnavailable = errors.New("workflow: planner not configured")

// ErrContextUnavailable: the issue's context cannot be read and retrying will not help.
var ErrContextUnavailable = errors.New("issue context unavailable")

// Planner constants.
const (
	// RolePlanner is the routing role of the planner (KIN-33).
	RolePlanner = "planner"
	// PlannerAuthorPrefix + the CLI is the author of a revision the planner produced.
	PlannerAuthorPrefix = "planner/"
	// TopicPlanningContext is the outbox topic that reads a planning run's context package from YouTrack.
	TopicPlanningContext = "workflow.planning_context"
	// AdapterYouTrack: the context is read from YouTrack (its health gates the delivery).
	AdapterYouTrack = "youtrack"
	// MaxContextFetchAttempts: after this many failed deliveries planning goes on without the issue text.
	MaxContextFetchAttempts = 5
	// MaxContextWait: a run still reading its context after this long (the delivery is held back or dead) goes on
	// without the issue text.
	MaxContextWait = 15 * time.Minute
	// plannerStepKey names the planner's attempts on the runner channel.
	plannerStepKey = "planner"
)

type contextPayload struct {
	RunID string `json:"runId"`
}

// StartPlanning asks the planner for a plan (Owner or the project's admin): a CREATED or FAILED task, or one with a
// plan not yet running (READY, AWAITING_APPROVAL: replanning creates a new revision), goes to ANALYZING with a new
// planning run. A task linked to a YouTrack issue first has its context read through the outbox. A replan continues
// the previous planner session when it runs on the same runner and CLI.
func (s *Service) StartPlanning(ctx context.Context, p auth.Principal, taskID string) (domain.Task, error) {
	if s.Planning == nil {
		return domain.Task{}, ErrPlannerUnavailable
	}
	t, err := s.Repo.Task(ctx, taskID)
	if err != nil {
		return domain.Task{}, err
	}
	if err := s.mayManage(ctx, p, t.ProjectID); err != nil {
		return domain.Task{}, err
	}
	return s.updateTask(ctx, taskID, func(ctx context.Context, t *domain.Task, now time.Time) error {
		prev, err := s.Planning.LatestPlanningRun(ctx, t.ID)
		switch {
		case errors.Is(err, domain.ErrNotFound):
		case err != nil:
			return err
		case prev.State.Active():
			return fmt.Errorf("task %s is already being planned: %w", t.ID, domain.ErrInvalidTransition)
		}
		switch t.State {
		case domain.TaskCreated, domain.TaskFailed, domain.TaskReady, domain.TaskAwaitingApproval:
			if err := t.Transition(domain.TaskAnalyzing, domain.Reason{}, now); err != nil {
				return err
			}
		case domain.TaskAnalyzing:
		default:
			return fmt.Errorf("task %s: plan in %s: %w", t.ID, t.State, domain.ErrInvalidTransition)
		}
		prepare := t.YouTrackIssue != "" && s.IssueContext != nil && s.Outbox != nil
		run := domain.NewPlanningRun(s.NewID(), t.ID, p.UserID, prepare, now)
		run.Session, run.SessionRunner, run.SessionAgent = prev.Session, prev.SessionRunner, prev.SessionAgent
		if t.YouTrackIssue != "" && !prepare {
			run.Context.Note = "the issue text is unavailable (YouTrack is not connected); plan from the title and the repository"
		}
		if err := s.Planning.CreatePlanningRun(ctx, &run); err != nil {
			return err
		}
		if !prepare {
			return nil
		}
		body, err := json.Marshal(contextPayload{RunID: run.ID})
		if err != nil {
			return err
		}
		return s.Outbox.Enqueue(ctx, messaging.Message{Adapter: AdapterYouTrack, Topic: TopicPlanningContext,
			Key: "planning-context/" + run.ID, Payload: body})
	})
}

// HandlePlanningContext is the outbox handler of TopicPlanningContext: it reads the issue's context package and
// queues the run for the planner. A final failure, or the last attempt of a transient one, queues the run with a
// note instead: planning never waits for YouTrack forever.
func (s *Service) HandlePlanningContext(ctx context.Context, d messaging.Delivery) error {
	var p contextPayload
	if err := json.Unmarshal(d.Payload, &p); err != nil || p.RunID == "" {
		return messaging.Permanent(errors.New("malformed planning context payload"))
	}
	run, err := s.Planning.PlanningRun(ctx, p.RunID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if run.State != domain.PlanningPreparing {
		return nil
	}
	t, err := s.Repo.Task(ctx, run.TaskID)
	if err != nil {
		return err
	}
	c, err := s.IssueContext.IssueContext(ctx, t.ProjectID, t.YouTrackIssue)
	if err != nil {
		if !errors.Is(err, ErrContextUnavailable) && d.Attempt < MaxContextFetchAttempts {
			return err
		}
		why := "YouTrack unreachable"
		if errors.Is(err, ErrContextUnavailable) {
			why = err.Error() // our own fixed wording (not configured, refused, issue gone)
		}
		c = domain.PlanningContext{Note: "the issue text is unavailable (" + why + "); plan from the title and the repository"}
	}
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		run, err := s.Planning.PlanningRun(ctx, p.RunID)
		if err != nil {
			return err
		}
		if run.State != domain.PlanningPreparing {
			return nil
		}
		if err := run.ContextReady(c, s.now()); err != nil {
			return err
		}
		return s.Planning.SavePlanningRun(ctx, &run)
	})
}

// schedulePlanning is the scheduler's planner pass: expired leases end their attempt, due runs are routed to a
// runner with the planner role's CLI; a run that cannot be placed BLOCKs its task with the reason.
func (s *Service) schedulePlanning(ctx context.Context) error {
	if s.Planning == nil {
		return nil
	}
	ids, err := s.Planning.ActivePlanningRuns(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err := s.schedulePlanningRun(ctx, id); err != nil && !errors.Is(err, domain.ErrConflict) {
			errs = append(errs, fmt.Errorf("planning run %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) schedulePlanningRun(ctx context.Context, id string) error {
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		run, err := s.Planning.PlanningRun(ctx, id)
		if err != nil {
			return err
		}
		now := s.now()
		if run.Kind == domain.PlanningKindDiscussion {
			return s.scheduleDiscussionRun(ctx, &run, now)
		}
		if run.State == domain.PlanningPreparing {
			if now.Sub(run.CreatedAt) < MaxContextWait {
				return nil
			}
			if err := run.ContextReady(domain.PlanningContext{Note: "the issue text is unavailable (YouTrack did not answer in time); " +
				"plan from the title and the repository"}, now); err != nil {
				return err
			}
		}
		if run.LeaseExpired(now) {
			runner, fence := run.RunnerID, run.Fence
			if err := s.endPlanningAttempt(ctx, &run, "lease expired: the runner stopped reporting on the planner", now); err != nil {
				return err
			}
			if s.Dispatcher == nil {
				return nil
			}
			// The runner may still run the CLI: stop it before another runner may plan the task.
			return s.Dispatcher.Cancel(ctx, runner, run.StepID(), fence, CancelReasonStale)
		}
		if !run.Due(now) {
			return nil
		}
		t, err := s.Repo.Task(ctx, run.TaskID)
		if err != nil {
			return err
		}
		if t.State.Terminal() { // closed without the run (e.g. cancelled by another path)
			if err := run.Cancel("the task was closed", now); err != nil {
				return err
			}
			return s.Planning.SavePlanningRun(ctx, &run)
		}
		c, choice, reason, ok, err := s.routePlanner(ctx, t.ProjectID)
		if err != nil {
			return err
		}
		if !ok {
			changed := run.Wait(reason.Code, reason.Detail, now)
			if changed {
				if err := s.Planning.SavePlanningRun(ctx, &run); err != nil {
					return err
				}
			}
			return s.blockPlanning(ctx, &t, reason, now)
		}
		repos, err := s.Projects.Repositories(ctx, t.ProjectID)
		if err != nil {
			return err
		}
		if len(repos) == 0 {
			return s.failPlanning(ctx, &run, "the project has no repository to plan in", now)
		}
		if t.State == domain.TaskBlocked && t.BlockedFrom == domain.TaskAnalyzing {
			if err := t.Resume(now); err != nil {
				return err
			}
			if err := s.saveTask(ctx, &t); err != nil {
				return err
			}
		}
		feedback := run.Feedback
		fence, resume, err := run.Assign(c.ID, choice, now, s.leaseTTL())
		if err != nil {
			return err
		}
		if err := s.Planning.SavePlanningRun(ctx, &run); err != nil {
			return err
		}
		prompt := domain.PlannerPrompt(domain.PlannerInput{Task: t, Repositories: repos, Context: run.Context,
			Feedback: feedback, Resumed: resume != ""})
		return s.Dispatcher.Assign(ctx, c.ID, Assignment{StepID: run.StepID(), StepKey: plannerStepKey, Fence: fence,
			ExecutionID: run.StepID(), TaskID: t.ID, ProjectID: t.ProjectID, Repository: repos[0], Agent: choice.Agent,
			Model: choice.Model, Role: RolePlanner, Prompt: prompt, Resume: resume})
	})
}

// routePlanner picks the runner and CLI for the planner role (KIN-33 routing; no capabilities beyond the CLI).
func (s *Service) routePlanner(ctx context.Context, projectID string) (Candidate, domain.AgentChoice, domain.Reason, bool, error) {
	if s.Agents == nil {
		return Candidate{}, domain.AgentChoice{}, domain.Reason{Code: domain.WaitAgent, Detail: DetailNoAgent}, false, nil
	}
	r, err := s.Agents.Route(ctx, projectID, RolePlanner, "")
	if err != nil {
		return Candidate{}, domain.AgentChoice{}, domain.Reason{}, false, fmt.Errorf("routing the planner: %w", err)
	}
	if len(r.Chain) == 0 {
		detail := r.Blocked
		if detail == "" {
			detail = DetailNoAgent
		}
		return Candidate{}, domain.AgentChoice{}, domain.Reason{Code: domain.WaitAgent, Detail: detail}, false, nil
	}
	cands, err := s.Runners.Candidates(ctx, projectID)
	if err != nil {
		return Candidate{}, domain.AgentChoice{}, domain.Reason{}, false, fmt.Errorf("runner candidates: %w", err)
	}
	held, err := s.Repo.HeldSteps(ctx)
	if err != nil {
		return Candidate{}, domain.AgentChoice{}, domain.Reason{}, false, fmt.Errorf("runner load: %w", err)
	}
	c, choice, reason, ok := domain.PickRoute(cands, nil, held, r.Chain, r.Cooling)
	return c, choice, reason, ok, nil
}

// blockPlanning BLOCKs an ANALYZING task with the planner's wait reason (or re-blocks it when the reason changed).
func (s *Service) blockPlanning(ctx context.Context, t *domain.Task, reason domain.Reason, now time.Time) error {
	switch {
	case t.State == domain.TaskAnalyzing:
	case t.State == domain.TaskBlocked && t.BlockedFrom == domain.TaskAnalyzing && t.Reason != reason:
		if err := t.Resume(now); err != nil {
			return err
		}
	default:
		return nil
	}
	if err := t.Transition(domain.TaskBlocked, reason, now); err != nil {
		return err
	}
	return s.saveTask(ctx, t)
}

// endPlanningAttempt ends the current attempt without a plan: a repeat is queued, or the task FAILS. A chat turn
// FAILS at once with its message.
func (s *Service) endPlanningAttempt(ctx context.Context, run *domain.PlanningRun, why string, now time.Time) error {
	if run.Kind == domain.PlanningKindDiscussion {
		return s.failTurn(ctx, run, why+"; send it again", now)
	}
	if !run.Retry(why, now) {
		return s.Planning.SavePlanningRun(ctx, run)
	}
	if err := s.Planning.SavePlanningRun(ctx, run); err != nil {
		return err
	}
	return s.failPlanningTask(ctx, run.TaskID, run.Reason, now)
}

// failPlanning fails the run and its task at once (nothing a repeat could fix).
func (s *Service) failPlanning(ctx context.Context, run *domain.PlanningRun, why string, now time.Time) error {
	run.Used = domain.MaxPlanningAttempts - 1
	return s.endPlanningAttempt(ctx, run, why, now)
}

// failPlanningTask moves the planned task to FAILED with the reason (resumed first if it was blocked meanwhile).
func (s *Service) failPlanningTask(ctx context.Context, taskID, reason string, now time.Time) error {
	t, err := s.Repo.Task(ctx, taskID)
	if err != nil {
		return err
	}
	from := t.State
	if t.State == domain.TaskBlocked && t.BlockedFrom == domain.TaskAnalyzing {
		if err := t.Resume(now); err != nil {
			return err
		}
	}
	if t.State != domain.TaskAnalyzing {
		return nil // the task moved on (cancelled, planned by other means)
	}
	if t.PlanRevision > 0 {
		// A failed replan keeps the current plan: the task returns where its revision routes it (its approvals were
		// never superseded); the run's reason says why there is no new one.
		rev, err := s.Repo.Revision(ctx, t.ID, t.PlanRevision)
		if err != nil {
			return err
		}
		to := domain.TaskReady
		if rev.Risk().RequiresApproval() {
			to = domain.TaskAwaitingApproval
		}
		if err := t.Transition(to, domain.Reason{}, now); err != nil {
			return err
		}
		return s.saveTask(ctx, &t)
	}
	if err := t.Transition(domain.TaskFailed, domain.Reason{Detail: reason}, now); err != nil {
		return err
	}
	if err := s.saveTask(ctx, &t); err != nil {
		return err
	}
	if s.Events != nil {
		if err := s.Events.TaskClosed(ctx, t); err != nil {
			return err
		}
	}
	return s.finished(ctx, from, t)
}

// updatePlanningAttempt loads the run of a planning step, checks the runner holds the attempt and applies fn in one
// transaction.
func (s *Service) updatePlanningAttempt(ctx context.Context, stepID, runnerID string, fence int64,
	fn func(ctx context.Context, run *domain.PlanningRun, now time.Time) error) error {
	id, ok := domain.PlanningRunID(stepID)
	if !ok {
		return domain.ErrNotFound
	}
	return s.Tx.InTx(ctx, func(ctx context.Context) error {
		run, err := s.Planning.PlanningRun(ctx, id)
		if err != nil {
			return err
		}
		if err := run.Check(runnerID, fence); err != nil {
			return err
		}
		return fn(ctx, &run, s.now())
	})
}

// CheckPlanningAttempt verifies that the runner holds the planning attempt (any event of it).
func (s *Service) CheckPlanningAttempt(ctx context.Context, stepID, runnerID string, fence int64) error {
	return s.updatePlanningAttempt(ctx, stepID, runnerID, fence, func(context.Context, *domain.PlanningRun, time.Time) error {
		return nil
	})
}

// StartPlanningAttempt records that the planner's CLI was launched (its lease is renewed).
func (s *Service) StartPlanningAttempt(ctx context.Context, stepID, runnerID string, fence int64) error {
	return s.updatePlanningAttempt(ctx, stepID, runnerID, fence, func(ctx context.Context, run *domain.PlanningRun, now time.Time) error {
		run.Renew(now, s.leaseTTL())
		if err := s.Planning.SavePlanningRun(ctx, run); err != nil {
			return err
		}
		if run.Kind == domain.PlanningKindDiscussion {
			return s.deliverTurn(ctx, run, now)
		}
		return nil
	})
}

// RecordPlanningReport stores what the planner's CLI reported at its end: the native session and the final answer
// (untrusted text, parsed only as a plan).
func (s *Service) RecordPlanningReport(ctx context.Context, stepID, runnerID string, fence int64, r domain.AgentReport, result string) error {
	return s.updatePlanningAttempt(ctx, stepID, runnerID, fence, func(ctx context.Context, run *domain.PlanningRun, _ time.Time) error {
		if err := run.Report(runnerID, fence, r.Session, result); err != nil {
			return err
		}
		run.Usage = r.Usage
		return s.Planning.SavePlanningRun(ctx, run)
	})
}

// CompletePlanningAttempt takes the planner's answer: a valid plan becomes the task's next revision (SubmitPlan:
// DAG validation against the project's repositories, the Policy Evaluator, READY or AWAITING_APPROVAL); an answer
// that is no valid plan queues the repeat with the reason, or FAILS the task after the last attempt.
func (s *Service) CompletePlanningAttempt(ctx context.Context, stepID, runnerID string, fence int64) error {
	return s.updatePlanningAttempt(ctx, stepID, runnerID, fence, func(ctx context.Context, run *domain.PlanningRun, now time.Time) error {
		if run.Kind == domain.PlanningKindDiscussion {
			return s.completeDiscussion(ctx, run, now)
		}
		if err := s.resumeAnalysis(ctx, run.TaskID, now); err != nil {
			return err
		}
		if t, err := s.Repo.Task(ctx, run.TaskID); err != nil {
			return err
		} else if t.State != domain.TaskAnalyzing {
			// The task got a plan by other means meanwhile (the plan chat, by hand) or moved on: a late answer never
			// replaces it.
			if err := run.Cancel("the task is no longer being planned", now); err != nil {
				return err
			}
			return s.Planning.SavePlanningRun(ctx, run)
		}
		plan, err := domain.ExtractPlan(run.Result)
		if err == nil {
			var rev domain.PlanRevision
			rev, err = s.SubmitPlan(ctx, run.TaskID, plan, PlannerAuthorPrefix+run.Agent)
			if err == nil {
				if err := run.Succeed(rev.Number(), now); err != nil {
					return err
				}
				return s.Planning.SavePlanningRun(ctx, run)
			}
		}
		var ve *domain.ValidationError
		switch {
		case errors.As(err, &ve):
			return s.endPlanningAttempt(ctx, run, err.Error(), now)
		case errors.Is(err, domain.ErrInvalidTransition):
			// The task left ANALYZING meanwhile (cancelled, planned by hand): the answer is no longer needed.
			if err := run.Cancel("the task is no longer being planned", now); err != nil {
				return err
			}
			return s.Planning.SavePlanningRun(ctx, run)
		}
		return err
	})
}

// resumeAnalysis returns a task blocked while planning to ANALYZING so the plan can be accepted.
func (s *Service) resumeAnalysis(ctx context.Context, taskID string, now time.Time) error {
	t, err := s.Repo.Task(ctx, taskID)
	if err != nil || t.State != domain.TaskBlocked || t.BlockedFrom != domain.TaskAnalyzing {
		return err
	}
	if err := t.Resume(now); err != nil {
		return err
	}
	return s.saveTask(ctx, &t)
}

// FailPlanningAttempt ends a planner run that failed on the runner (CLI error, timeout): repeat or FAIL.
func (s *Service) FailPlanningAttempt(ctx context.Context, stepID, runnerID string, fence int64, reason string) error {
	return s.updatePlanningAttempt(ctx, stepID, runnerID, fence, func(ctx context.Context, run *domain.PlanningRun, now time.Time) error {
		return s.endPlanningAttempt(ctx, run, "the planner run failed: "+reason, now)
	})
}

// PlannerChangedFiles stops a planner that changed files in its worktree (it must only read): the process is
// cancelled and the attempt ends without a plan (repeat or FAIL). The worktree is never published either way.
func (s *Service) PlannerChangedFiles(ctx context.Context, stepID, runnerID string, fence int64, files []string) error {
	return s.updatePlanningAttempt(ctx, stepID, runnerID, fence, func(ctx context.Context, run *domain.PlanningRun, now time.Time) error {
		shown := files
		if len(shown) > 5 {
			shown = append(slices.Clone(shown[:5]), "…")
		}
		why := "the planner changed files although it may only read: " + strings.Join(shown, ", ")
		if err := s.endPlanningAttempt(ctx, run, why, now); err != nil {
			return err
		}
		if s.Dispatcher == nil {
			return nil
		}
		return s.Dispatcher.Cancel(ctx, runnerID, stepID, fence, CancelReasonScopeExceeded)
	})
}

// HoldPlanningForCLILimit returns the attempt to the queue until the CLI's limit resets (no attempt used), cools the
// runner's CLI down in routing and BLOCKs the task with reason cli_limit. cause is the runner's (rate_limit | auth |
// billing): a logged-out or billing-refused CLI backs off and finally waits for the Owner (KIN-83), as for a step.
func (s *Service) HoldPlanningForCLILimit(ctx context.Context, stepID, runnerID string, fence int64, cause, reason string, resetsAt time.Time) error {
	return s.updatePlanningAttempt(ctx, stepID, runnerID, fence, func(ctx context.Context, run *domain.PlanningRun, now time.Time) error {
		until := resetsAt.UTC()
		if resetsAt.IsZero() || !resetsAt.After(now) {
			until = now.Add(domain.CLILimitRetryDelay)
		}
		until = minTime(until, now.Add(domain.MaxCLILimitWait))
		var hold CLIHold
		if s.CLILimits != nil {
			h, err := s.CLILimits.CLILimitHit(ctx, runnerID, run.Agent, cause, resetsAt, reason)
			if err != nil {
				return err
			}
			if hold = h; h.Known {
				until = h.Until.UTC()
			}
		}
		detail := reason + " (retry at " + until.Format(time.RFC3339) + ")"
		if hold.NeedsOwner {
			detail = reason + " (waiting for the Owner: log the CLI in on runner " + runnerID +
				" or fix its billing, then mark it ready again on the Agents page)"
		}
		if err := run.Hold(runnerID, fence, domain.WaitCLILimit, detail, until, now); err != nil {
			return err
		}
		if err := s.Planning.SavePlanningRun(ctx, run); err != nil {
			return err
		}
		if run.Kind == domain.PlanningKindDiscussion {
			return nil // the turn waits for the CLI (or its timeout); the task is not blocked
		}
		t, err := s.Repo.Task(ctx, run.TaskID)
		if err != nil {
			return err
		}
		return s.blockPlanning(ctx, &t, domain.Reason{Code: domain.WaitCLILimit, Detail: detail}, now)
	})
}

// HoldPlanningForNetwork returns the attempt to the queue for domain.NetworkRetryDelay (no attempt used): the runner
// could not verify its strict egress sandbox, so the CLI never ran (KIN-31).
func (s *Service) HoldPlanningForNetwork(ctx context.Context, stepID, runnerID string, fence int64, reason string) error {
	return s.updatePlanningAttempt(ctx, stepID, runnerID, fence, func(ctx context.Context, run *domain.PlanningRun, now time.Time) error {
		if err := run.Hold(runnerID, fence, domain.WaitNetwork, reason, now.Add(domain.NetworkRetryDelay), now); err != nil {
			return err
		}
		return s.Planning.SavePlanningRun(ctx, run)
	})
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// renewPlanningLeases extends the leases of the runner's RUNNING planning attempts (its heartbeat).
func (s *Service) renewPlanningLeases(ctx context.Context, runnerID string) error {
	if s.Planning == nil {
		return nil
	}
	ids, err := s.Planning.PlanningRunsHeldBy(ctx, runnerID)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		err := s.Tx.InTx(ctx, func(ctx context.Context) error {
			run, err := s.Planning.PlanningRun(ctx, id)
			if err != nil || run.State != domain.PlanningRunning || run.RunnerID != runnerID || run.LeaseExpired(s.now()) {
				return err
			}
			run.Renew(s.now(), s.leaseTTL())
			return s.Planning.SavePlanningRun(ctx, &run)
		})
		if err != nil && !errors.Is(err, domain.ErrConflict) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// reconcilePlanning applies a runner's reconciliation to its planning attempts: a live process renews the lease, a
// kept result waits for its event, neither ends the attempt ("process lost"); a running planner process the runner
// holds without a current attempt is cancelled.
func (s *Service) reconcilePlanning(ctx context.Context, runnerID string, reports []AttemptReport) error {
	handled := map[string]bool{}
	byStep := map[string]AttemptReport{}
	for _, r := range reports {
		if _, ok := domain.PlanningRunID(r.StepID); ok {
			byStep[r.StepID+"/"+fmt.Sprint(r.Fence)] = r
		}
	}
	if s.Planning == nil {
		return nil
	}
	ids, err := s.Planning.PlanningRunsHeldBy(ctx, runnerID)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		err := s.Tx.InTx(ctx, func(ctx context.Context) error {
			run, err := s.Planning.PlanningRun(ctx, id)
			if err != nil || run.State != domain.PlanningRunning || run.RunnerID != runnerID {
				return err
			}
			k := run.StepID() + "/" + fmt.Sprint(run.Fence)
			r, known := byStep[k]
			handled[k] = known
			now := s.now()
			switch {
			case known && r.Result:
				return nil
			case known && r.Running:
				run.Renew(now, s.leaseTTL())
				return s.Planning.SavePlanningRun(ctx, &run)
			}
			return s.endPlanningAttempt(ctx, &run, "process lost: the runner has no process and no result for the planner", now)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("reconciling planning run %s: %w", id, err))
		}
	}
	if s.Dispatcher != nil {
		err := s.Tx.InTx(ctx, func(ctx context.Context) error {
			for k, r := range byStep {
				if !r.Running || handled[k] {
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

// cancelPlanning cancels the task's active planning run and stops a running planner (in the caller's transaction).
func (s *Service) cancelPlanning(ctx context.Context, taskID, reason string, now time.Time) error {
	if s.Planning == nil {
		return nil
	}
	run, err := s.Planning.LatestPlanningRun(ctx, taskID)
	if errors.Is(err, domain.ErrNotFound) || err == nil && !run.State.Active() {
		return nil
	}
	if err != nil {
		return err
	}
	running := run.State == domain.PlanningRunning
	if err := run.Cancel(reason, now); err != nil {
		return err
	}
	if err := s.Planning.SavePlanningRun(ctx, &run); err != nil {
		return err
	}
	if run.Kind == domain.PlanningKindDiscussion && s.Discussion != nil {
		if err := s.failMessage(ctx, &run, "the task was closed: "+reason, now); err != nil {
			return err
		}
	}
	if !running || s.Dispatcher == nil {
		return nil
	}
	return s.Dispatcher.Cancel(ctx, run.RunnerID, run.StepID(), run.Fence, CancelReasonCancelled)
}

// finishedPlanningStep answers FinishedSteps for a planning step: the time an ended run ended, the zero time for an
// unknown run, ok false while the run is active.
func (s *Service) finishedPlanningStep(ctx context.Context, stepID string) (time.Time, bool, error) {
	id, _ := domain.PlanningRunID(stepID)
	if s.Planning == nil {
		return time.Time{}, true, nil
	}
	run, err := s.Planning.PlanningRun(ctx, id)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return time.Time{}, true, nil
	case err != nil:
		return time.Time{}, false, err
	case run.State.Active():
		return time.Time{}, false, nil
	}
	return run.UpdatedAt, true, nil
}

// PlannerSession is the planner's native CLI session of a task: the plan chat (KIN-43) resumes it on its runner.
type PlannerSession struct {
	Session  string
	RunnerID string
	Agent    string
}

// PlannerSession returns the task's latest planner session; domain.ErrNotFound when no planner run reported one.
func (s *Service) PlannerSession(ctx context.Context, taskID string) (PlannerSession, error) {
	if s.Planning == nil {
		return PlannerSession{}, domain.ErrNotFound
	}
	run, err := s.Planning.LatestPlanningRun(ctx, taskID)
	if err != nil {
		return PlannerSession{}, err
	}
	if run.Session == "" {
		return PlannerSession{}, domain.ErrNotFound
	}
	return PlannerSession{Session: run.Session, RunnerID: run.SessionRunner, Agent: run.SessionAgent}, nil
}

// planningVisible checks that the planning attempt (step, fence) belongs to the task.
func (s *Service) planningVisible(ctx context.Context, taskID, stepID string, fence int64) error {
	id, _ := domain.PlanningRunID(stepID)
	if s.Planning == nil {
		return domain.ErrNotFound
	}
	run, err := s.Planning.PlanningRun(ctx, id)
	if err != nil {
		return err
	}
	if run.TaskID != taskID || fence < 1 || fence > run.Fence {
		return domain.ErrNotFound
	}
	return nil
}
