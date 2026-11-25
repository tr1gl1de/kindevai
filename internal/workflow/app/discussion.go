package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// The plan discussion (KIN-43). The Owner or a project admin writes about the task's current plan revision; the
// message is stored PENDING with a planning run of kind discussion, which the scheduler hands to the planner role's
// CLI like a planner attempt (execution.assign through the runner's durable command log — the plan.discuss command
// of the spec), preferring the runner and CLI that hold the planner's native session (resumed) and otherwise
// starting a fresh session told the plan, the context package and the bounded history. The answer arrives with
// execution.completed (fenced: a repeated delivery is stale and stores nothing twice); a ```json plan in it goes
// through SubmitPlan — validation, the Policy Evaluator, a new immutable revision, the old approvals retired.

// DiscussionRepository stores the plan discussion on the caller's transaction.
type DiscussionRepository interface {
	// CreateMessage inserts a message at version 1; domain.ErrConflict when the turn already has an answer.
	CreateMessage(ctx context.Context, m *domain.DiscussionMessage) error
	Message(ctx context.Context, id string) (domain.DiscussionMessage, error)
	SaveMessage(ctx context.Context, m *domain.DiscussionMessage) error
	// Messages returns the task's newest limit messages, oldest first.
	Messages(ctx context.Context, taskID string, limit int) ([]domain.DiscussionMessage, error)
	CountUserMessages(ctx context.Context, taskID string) (int, error)
	// UserMessageTimes returns when the user wrote their messages since `since` (all tasks).
	UserMessageTimes(ctx context.Context, userID string, since time.Time) ([]time.Time, error)
	DiscussionLimits(ctx context.Context) (domain.DiscussionLimits, error) // the defaults until the Owner sets them
	SetDiscussionLimits(ctx context.Context, l domain.DiscussionLimits, by string, now time.Time) error
	// DiscussionWork sums, per task f selects, the CLI time of its discussion answers (agent time).
	DiscussionWork(ctx context.Context, f TaskFilter) (map[string]time.Duration, error)
}

// AuditLog records audit events in the caller's transaction (KIN-16).
type AuditLog interface {
	Record(ctx context.Context, e auditapp.Entry) error
}

// Audit event kinds of the plan discussion.
const (
	KindDiscussionMessageSent     = "plan_discussion.message_sent"
	KindDiscussionAnswered        = "plan_discussion.answered"
	KindDiscussionRevisionCreated = "plan_discussion.revision_created"
	KindDiscussionLimitHit        = "plan_discussion.limit_hit"
	KindDiscussionLimitsChanged   = "plan_discussion.limits_changed"
)

// MaxDiscussionList bounds the messages one discussion response returns.
const MaxDiscussionList = 200

// ErrDiscussionUnavailable: the plan discussion is not configured in this process.
var ErrDiscussionUnavailable = errors.New("workflow: plan discussion not configured")

func (s *Service) discussionOn() error {
	if s.Planning == nil || s.Discussion == nil {
		return ErrDiscussionUnavailable
	}
	return nil
}

// discussable: a task with a plan that may still change (READY or AWAITING_APPROVAL).
func discussable(t domain.Task) error {
	if t.PlanRevision == 0 || t.State != domain.TaskReady && t.State != domain.TaskAwaitingApproval {
		return fmt.Errorf("task %s: discuss the plan in %s: %w", t.ID, t.State, domain.ErrInvalidTransition)
	}
	return nil
}

func (s *Service) record(ctx context.Context, e auditapp.Entry) error {
	if s.Audit == nil {
		return nil
	}
	return s.Audit.Record(ctx, e)
}

// PostMessage stores the user's message about the task's current plan revision and queues the AI's answer. Only
// the Owner and the project's admins write (ErrForbidden; domain.ErrNotFound for a project p does not see). A
// limit refuses the message with a *domain.LimitError, recorded in the audit log.
func (s *Service) PostMessage(ctx context.Context, p auth.Principal, taskID, text string) (domain.DiscussionMessage, error) {
	if err := s.discussionOn(); err != nil {
		return domain.DiscussionMessage{}, err
	}
	var out domain.DiscussionMessage
	var refused error
	err := s.Tx.InTx(ctx, func(ctx context.Context) error {
		t, lim, err := s.discussionTask(ctx, p, taskID)
		if err != nil {
			return err
		}
		now := s.now()
		m, err := domain.NewUserMessage(s.NewID(), t.ID, p.UserID, t.PlanRevision, text, lim, now)
		var le *domain.LimitError
		if errors.As(err, &le) {
			refused = le
			return s.limitHit(ctx, p, t, le)
		}
		if err != nil {
			return err
		}
		prev, inFlight, err := s.turnInFlight(ctx, t.ID)
		if err != nil {
			return err
		}
		n, err := s.Discussion.CountUserMessages(ctx, t.ID)
		if err != nil {
			return err
		}
		recent, err := s.Discussion.UserMessageTimes(ctx, p.UserID, now.Add(-time.Hour))
		if err != nil {
			return err
		}
		if err := lim.Check(inFlight, n, recent, now); errors.As(err, &le) {
			refused = le
			return s.limitHit(ctx, p, t, le)
		}
		run := domain.NewDiscussionRun(s.NewID(), t.ID, m.ID, p.UserID, prev, now)
		m.RunID = run.ID
		if err := s.Discussion.CreateMessage(ctx, &m); err != nil {
			return err
		}
		if err := s.Planning.CreatePlanningRun(ctx, &run); err != nil {
			if errors.Is(err, domain.ErrConflict) {
				return &domain.LimitError{Code: domain.LimitAnswerInFlight}
			}
			return err
		}
		out = m
		return s.record(ctx, auditapp.Entry{Actor: auditapp.UserActor(p), Kind: KindDiscussionMessageSent,
			ProjectID: t.ProjectID, Subject: "task:" + t.ID, Details: map[string]string{"message": m.ID,
				"revision": strconv.Itoa(m.PlanRevision), "chars": strconv.Itoa(len([]rune(m.Text)))}})
	})
	if err != nil {
		return domain.DiscussionMessage{}, err
	}
	if refused != nil {
		return domain.DiscussionMessage{}, refused
	}
	return out, nil
}

// RetryMessage sends a FAILED message of the task again (a new turn; it does not count as a new message).
func (s *Service) RetryMessage(ctx context.Context, p auth.Principal, taskID, messageID string) (domain.DiscussionMessage, error) {
	if err := s.discussionOn(); err != nil {
		return domain.DiscussionMessage{}, err
	}
	var out domain.DiscussionMessage
	err := s.Tx.InTx(ctx, func(ctx context.Context) error {
		t, _, err := s.discussionTask(ctx, p, taskID)
		if err != nil {
			return err
		}
		m, err := s.Discussion.Message(ctx, messageID)
		if err != nil {
			return err
		}
		if m.TaskID != t.ID || m.Role != domain.RoleUser {
			return domain.ErrNotFound
		}
		prev, inFlight, err := s.turnInFlight(ctx, t.ID)
		if err != nil {
			return err
		}
		if inFlight {
			return &domain.LimitError{Code: domain.LimitAnswerInFlight}
		}
		now := s.now()
		run := domain.NewDiscussionRun(s.NewID(), t.ID, m.ID, p.UserID, prev, now)
		if err := m.Resend(run.ID, now); err != nil {
			return err
		}
		if err := s.Discussion.SaveMessage(ctx, &m); err != nil {
			return err
		}
		if err := s.Planning.CreatePlanningRun(ctx, &run); err != nil {
			return err
		}
		out = m
		return s.record(ctx, auditapp.Entry{Actor: auditapp.UserActor(p), Kind: KindDiscussionMessageSent,
			ProjectID: t.ProjectID, Subject: "task:" + t.ID, Details: map[string]string{"message": m.ID,
				"revision": strconv.Itoa(m.PlanRevision), "resent": "true"}})
	})
	if err != nil {
		return domain.DiscussionMessage{}, err
	}
	return out, nil
}

// discussionTask loads a task p may write about and the limits.
func (s *Service) discussionTask(ctx context.Context, p auth.Principal, taskID string) (domain.Task, domain.DiscussionLimits, error) {
	t, err := s.Repo.Task(ctx, taskID)
	if err != nil {
		return t, domain.DiscussionLimits{}, err
	}
	if err := s.mayManage(ctx, p, t.ProjectID); err != nil {
		return t, domain.DiscussionLimits{}, err
	}
	if err := discussable(t); err != nil {
		return t, domain.DiscussionLimits{}, err
	}
	lim, err := s.Discussion.DiscussionLimits(ctx)
	return t, lim, err
}

// turnInFlight returns the task's latest planning run and whether it (a planner run or a chat turn) is active.
func (s *Service) turnInFlight(ctx context.Context, taskID string) (domain.PlanningRun, bool, error) {
	prev, err := s.Planning.LatestPlanningRun(ctx, taskID)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.PlanningRun{}, false, nil
	}
	return prev, err == nil && prev.State.Active(), err
}

func (s *Service) limitHit(ctx context.Context, p auth.Principal, t domain.Task, le *domain.LimitError) error {
	return s.record(ctx, auditapp.Entry{Actor: auditapp.UserActor(p), Kind: KindDiscussionLimitHit, ProjectID: t.ProjectID,
		Subject: "task:" + t.ID, Details: map[string]string{"limit": le.Code, "value": strconv.Itoa(le.Limit)}})
}

// DiscussionThread is a task's plan discussion as its readers see it.
type DiscussionThread struct {
	Messages []domain.DiscussionMessage // the newest MaxDiscussionList, oldest first
	Limits   domain.DiscussionLimits
	CanWrite bool // p may write and the task's plan may change now
	InFlight bool // an answer (or a planner run) is in progress: no new message until it ends
	Used     int  // the task's user messages (against Limits.MaxMessagesPerPlan)
	Session  bool // the planner's native session exists (a turn resumes it where it lives)
}

// PlanDiscussion returns the task's plan discussion to anyone who sees the task (domain.ErrNotFound otherwise).
func (s *Service) PlanDiscussion(ctx context.Context, p auth.Principal, taskID string) (DiscussionThread, error) {
	if err := s.discussionOn(); err != nil {
		return DiscussionThread{}, err
	}
	t, err := s.Repo.Task(ctx, taskID)
	if err != nil {
		return DiscussionThread{}, err
	}
	v, err := s.visible(ctx, p)
	if err != nil {
		return DiscussionThread{}, err
	}
	role, ok := v[t.ProjectID]
	if !ok {
		return DiscussionThread{}, domain.ErrNotFound
	}
	var out DiscussionThread
	if out.Messages, err = s.Discussion.Messages(ctx, t.ID, MaxDiscussionList); err != nil {
		return out, err
	}
	if out.Limits, err = s.Discussion.DiscussionLimits(ctx); err != nil {
		return out, err
	}
	if out.Used, err = s.Discussion.CountUserMessages(ctx, t.ID); err != nil {
		return out, err
	}
	prev, inFlight, err := s.turnInFlight(ctx, t.ID)
	if err != nil {
		return out, err
	}
	out.InFlight, out.Session = inFlight, prev.Session != ""
	out.CanWrite = (p.Owner || role == RoleOwner || role == RoleProjectAdmin) && discussable(t) == nil
	return out, nil
}

// DiscussionLimits returns the discussion limits.
func (s *Service) DiscussionLimits(ctx context.Context) (domain.DiscussionLimits, error) {
	if err := s.discussionOn(); err != nil {
		return domain.DiscussionLimits{}, err
	}
	return s.Discussion.DiscussionLimits(ctx)
}

// SetDiscussionLimits changes the discussion limits (the Owner only; audited).
func (s *Service) SetDiscussionLimits(ctx context.Context, p auth.Principal, l domain.DiscussionLimits) (domain.DiscussionLimits, error) {
	if err := s.discussionOn(); err != nil {
		return l, err
	}
	if !p.Owner {
		return l, ErrForbidden
	}
	if err := l.Validate(); err != nil {
		return l, err
	}
	err := s.Tx.InTx(ctx, func(ctx context.Context) error {
		if err := s.Discussion.SetDiscussionLimits(ctx, l, p.UserID, s.now()); err != nil {
			return err
		}
		return s.record(ctx, auditapp.Entry{Actor: auditapp.UserActor(p), Kind: KindDiscussionLimitsChanged,
			Subject: "settings:plan_discussion", Details: map[string]string{
				"maxMessageChars": strconv.Itoa(l.MaxMessageChars), "maxMessagesPerPlan": strconv.Itoa(l.MaxMessagesPerPlan),
				"maxMessagesPerHour": strconv.Itoa(l.MaxMessagesPerHour), "answerTimeout": l.AnswerTimeout.String()}})
	})
	return l, err
}

// scheduleDiscussionRun is the scheduler's pass over a chat turn (in the caller's transaction): a turn without an
// answer after the limit's timeout FAILS (its CLI is stopped), an expired lease FAILS it, a due turn is routed to
// a runner and assigned. A turn that cannot be placed waits (PENDING) until the timeout; the task is not blocked.
func (s *Service) scheduleDiscussionRun(ctx context.Context, run *domain.PlanningRun, now time.Time) error {
	lim, err := s.Discussion.DiscussionLimits(ctx)
	if err != nil {
		return err
	}
	running, runner, fence := run.State == domain.PlanningRunning, run.RunnerID, run.Fence
	switch {
	case now.Sub(run.CreatedAt) >= lim.AnswerTimeout:
		why := "no answer within " + lim.AnswerTimeout.String()
		if run.WaitDetail != "" {
			why += " (" + run.WaitDetail + ")"
		}
		if err := s.failTurn(ctx, run, why+"; send it again", now); err != nil {
			return err
		}
		if running && s.Dispatcher != nil {
			return s.Dispatcher.Cancel(ctx, runner, run.StepID(), fence, CancelReasonCancelled)
		}
		return nil
	case run.LeaseExpired(now):
		if err := s.failTurn(ctx, run, "lease expired: the runner stopped reporting on the answer; send it again", now); err != nil {
			return err
		}
		if s.Dispatcher == nil {
			return nil
		}
		return s.Dispatcher.Cancel(ctx, runner, run.StepID(), fence, CancelReasonStale)
	case !run.Due(now):
		return nil
	}
	t, err := s.Repo.Task(ctx, run.TaskID)
	if err != nil {
		return err
	}
	if discussable(t) != nil {
		return s.failTurn(ctx, run, "the task's plan can no longer change ("+string(t.State)+")", now)
	}
	c, choice, reason, ok, err := s.routeDiscussion(ctx, t.ProjectID, *run)
	if err != nil {
		return err
	}
	if !ok {
		if run.Wait(reason.Code, reason.Detail, now) {
			return s.Planning.SavePlanningRun(ctx, run)
		}
		return nil
	}
	repos, err := s.Projects.Repositories(ctx, t.ProjectID)
	if err != nil {
		return err
	}
	if len(repos) == 0 {
		return s.failTurn(ctx, run, "the project has no repository", now)
	}
	in, err := s.discussionInput(ctx, t, *run, repos)
	if err != nil {
		return err
	}
	fence, resume, err := run.Assign(c.ID, choice, now, s.leaseTTL())
	if err != nil {
		return err
	}
	if err := s.Planning.SavePlanningRun(ctx, run); err != nil {
		return err
	}
	in.Resumed = resume != ""
	return s.Dispatcher.Assign(ctx, c.ID, Assignment{StepID: run.StepID(), StepKey: plannerStepKey, Fence: fence,
		ExecutionID: run.StepID(), TaskID: t.ID, ProjectID: t.ProjectID, Repository: repos[0], Agent: choice.Agent,
		Model: choice.Model, Role: RolePlanner, Prompt: domain.DiscussionPrompt(in), Resume: resume})
}

// discussionInput gathers a turn's prompt: the current revision, the planner's context package, the history before
// the message and the message.
func (s *Service) discussionInput(ctx context.Context, t domain.Task, run domain.PlanningRun, repos []string) (domain.DiscussionInput, error) {
	rev, err := s.Repo.Revision(ctx, t.ID, t.PlanRevision)
	if err != nil {
		return domain.DiscussionInput{}, err
	}
	m, err := s.Discussion.Message(ctx, run.MessageID)
	if err != nil {
		return domain.DiscussionInput{}, err
	}
	in := domain.DiscussionInput{Task: t, Repositories: repos, Revision: rev.Number(), Risk: rev.Risk(), Plan: rev.Plan(),
		Message: m.Text}
	if pr, err := s.Planning.LatestPlanRun(ctx, t.ID); err == nil {
		in.Context = pr.Context
	} else if !errors.Is(err, domain.ErrNotFound) {
		return in, err
	}
	hist, err := s.Discussion.Messages(ctx, t.ID, domain.MaxDiscussionHistory+1)
	if err != nil {
		return in, err
	}
	for _, h := range hist {
		if h.ID != m.ID && h.ReplyTo != m.ID {
			in.History = append(in.History, h)
		}
	}
	return in, nil
}

// routeDiscussion prefers the runner and CLI holding the planner's session (it is resumed there); otherwise the
// planner role is routed like the planner (a fresh session).
func (s *Service) routeDiscussion(ctx context.Context, projectID string, run domain.PlanningRun) (Candidate, domain.AgentChoice, domain.Reason, bool, error) {
	if run.Session != "" && s.Agents != nil {
		r, err := s.Agents.Route(ctx, projectID, RolePlanner, "")
		if err != nil {
			return Candidate{}, domain.AgentChoice{}, domain.Reason{}, false, fmt.Errorf("routing the planner: %w", err)
		}
		i := slices.IndexFunc(r.Chain, func(c domain.AgentChoice) bool { return c.Agent == run.SessionAgent })
		cooling := false
		if r.Cooling != nil {
			_, cooling = r.Cooling(run.SessionRunner, run.SessionAgent)
		}
		if i >= 0 && !cooling {
			cands, err := s.Runners.Candidates(ctx, projectID)
			if err != nil {
				return Candidate{}, domain.AgentChoice{}, domain.Reason{}, false, fmt.Errorf("runner candidates: %w", err)
			}
			for _, c := range cands {
				if c.ID == run.SessionRunner && c.Eligible([]string{run.SessionAgent}) {
					return c, r.Chain[i], domain.Reason{}, true, nil
				}
			}
		}
	}
	return s.routePlanner(ctx, projectID)
}

// failTurn ends a chat turn without an answer: the run and its message FAIL with why (the message may be sent again).
func (s *Service) failTurn(ctx context.Context, run *domain.PlanningRun, why string, now time.Time) error {
	if err := run.Fail(why, now); err != nil {
		return err
	}
	if err := s.Planning.SavePlanningRun(ctx, run); err != nil {
		return err
	}
	return s.failMessage(ctx, run, run.Reason, now)
}

func (s *Service) failMessage(ctx context.Context, run *domain.PlanningRun, why string, now time.Time) error {
	m, err := s.Discussion.Message(ctx, run.MessageID)
	if err != nil {
		return err
	}
	if m.RunID != run.ID || m.Fail(why, now) != nil {
		return nil // the message moved on (answered, sent again)
	}
	return s.Discussion.SaveMessage(ctx, &m)
}

// deliverTurn records that the turn's CLI started on the message.
func (s *Service) deliverTurn(ctx context.Context, run *domain.PlanningRun, now time.Time) error {
	m, err := s.Discussion.Message(ctx, run.MessageID)
	if err != nil {
		return err
	}
	if m.RunID != run.ID || m.Status != domain.MessagePending {
		return nil
	}
	if err := m.Deliver(now); err != nil {
		return err
	}
	return s.Discussion.SaveMessage(ctx, &m)
}

// completeDiscussion stores the turn's answer (in the attempt's transaction): the assistant message with its usage
// and CLI time; a proposed plan becomes the next revision through SubmitPlan when it was written for the current
// revision; a system notice says what became of it. Nothing in the answer changes scope or rights beyond what the
// plan validation and the Policy Evaluator accept.
func (s *Service) completeDiscussion(ctx context.Context, run *domain.PlanningRun, now time.Time) error {
	m, err := s.Discussion.Message(ctx, run.MessageID)
	if err != nil {
		return err
	}
	if m.RunID != run.ID || m.Status != domain.MessageDelivered {
		if err := run.Cancel("the message no longer waits for this answer", now); err != nil {
			return err
		}
		return s.Planning.SavePlanningRun(ctx, run)
	}
	text := strings.TrimSpace(run.Result)
	if text == "" {
		return s.failTurn(ctx, run, "the AI gave no answer; send it again", now)
	}
	t, err := s.Repo.Task(ctx, run.TaskID)
	if err != nil {
		return err
	}
	var work time.Duration
	if m.DeliveredAt != nil {
		work = now.Sub(*m.DeliveredAt)
	}
	ans := domain.NewAnswer(s.NewID(), m, t.PlanRevision, run.Agent, run.Model, text, run.Usage, work, run.ID, now)
	if err := s.Discussion.CreateMessage(ctx, &ans); err != nil {
		return err
	}
	created := 0
	if plan, proposed, perr := domain.ProposedPlan(text); proposed {
		notice, wording := s.applyProposedPlan(ctx, t, m, run, plan, perr)
		if notice == "" {
			return wording.err
		}
		if notice == domain.NoticeRevisionCreated {
			created = wording.revision
		}
		n := domain.NewNotice(s.NewID(), m, t.PlanRevision, notice, wording.text, created, now)
		if err := s.Discussion.CreateMessage(ctx, &n); err != nil {
			return err
		}
	}
	if err := m.Answer(now); err != nil {
		return err
	}
	if err := s.Discussion.SaveMessage(ctx, &m); err != nil {
		return err
	}
	if err := run.Succeed(created, now); err != nil {
		return err
	}
	if err := s.Planning.SavePlanningRun(ctx, run); err != nil {
		return err
	}
	d := map[string]string{"message": m.ID, "answer": ans.ID, "agent": run.Agent, "workMs": strconv.FormatInt(work.Milliseconds(), 10)}
	if u := run.Usage; u != (domain.AgentUsage{}) {
		d["inputTokens"], d["outputTokens"] = strconv.FormatInt(u.InputTokens, 10), strconv.FormatInt(u.OutputTokens, 10)
		d["costUsd"] = strconv.FormatFloat(u.CostUSD, 'f', 4, 64)
	}
	if created > 0 {
		d["revision"] = strconv.Itoa(created)
	}
	return s.record(ctx, auditapp.Entry{Actor: auditapp.System(), Kind: KindDiscussionAnswered, ProjectID: t.ProjectID,
		Subject: "task:" + t.ID, Details: d})
}

type planOutcome struct {
	text     string
	revision int
	err      error
}

// applyProposedPlan tries the answer's plan: the notice code and its wording, or "" with an error to return.
func (s *Service) applyProposedPlan(ctx context.Context, t domain.Task, m domain.DiscussionMessage, run *domain.PlanningRun,
	plan domain.Plan, perr error) (string, planOutcome) {
	if perr != nil {
		return domain.NoticePlanRejected, planOutcome{text: "the proposed plan was not accepted: " + perr.Error()}
	}
	if m.PlanRevision != t.PlanRevision {
		return domain.NoticePlanOutdated, planOutcome{text: fmt.Sprintf(
			"the proposed plan was written for revision %d, but revision %d is current; it was not applied", m.PlanRevision, t.PlanRevision)}
	}
	rev, err := s.SubmitPlan(ctx, t.ID, plan, PlannerAuthorPrefix+run.Agent)
	var ve *domain.ValidationError
	switch {
	case errors.As(err, &ve):
		return domain.NoticePlanRejected, planOutcome{text: "the proposed plan was not accepted: " + err.Error()}
	case errors.Is(err, domain.ErrInvalidTransition), errors.Is(err, domain.ErrStaleRevision):
		return domain.NoticePlanNotApplied, planOutcome{text: "the task's state no longer takes a new plan; it was not applied"}
	case err != nil:
		return "", planOutcome{err: err}
	}
	if err := s.record(ctx, auditapp.Entry{Actor: auditapp.System(), Kind: KindDiscussionRevisionCreated,
		ProjectID: t.ProjectID, Subject: "task:" + t.ID, Details: map[string]string{"message": m.ID,
			"revision": strconv.Itoa(rev.Number()), "risk": string(rev.Risk()), "planHash": rev.Hash(), "author": rev.CreatedBy()}}); err != nil {
		return "", planOutcome{err: err}
	}
	return domain.NoticeRevisionCreated, planOutcome{revision: rev.Number(), text: fmt.Sprintf(
		"revision %d was created from the answer (risk %s)", rev.Number(), rev.Risk())}
}
