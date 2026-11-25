package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// Publishing (KIN-36, domain/publish.go): a publish attempt's push report is checked here, a succeeded publish is
// recorded in the execution's change set with an outbox message, and the outbox handler opens (or reuses) the pull
// request into the repository's integration branch. main/master are refused in each of these steps and audited.

// Outbox topic and adapter of opening a pull request.
const (
	AdapterGitHub           = "github"
	TopicOpenPullRequest    = "workflow.pull_request_open"
	maxPullRequestTitleRune = 200
)

// Audit kinds of publishing.
const (
	KindPushRefused         = "workflow.push_refused"
	KindPullRequestRefused  = "workflow.pull_request_refused"
	KindPullRequestOpened   = "workflow.pull_request_opened"
	KindPullRequestReused   = "workflow.pull_request_reused"
	KindPullRequestFailed   = "workflow.pull_request_failed"
	auditSubjectTaskPrefix  = "task:"
	pullRequestBodyMaxRunes = 4000
)

// ChangeSets stores the executions' git artifacts (in the caller's transaction).
type ChangeSets interface {
	// SaveArtifact inserts or replaces the artifact of (execution, repository, branch).
	SaveArtifact(ctx context.Context, a domain.GitArtifact) error
	// Artifact returns one artifact (domain.ErrNotFound).
	Artifact(ctx context.Context, executionID, repository, branch string) (domain.GitArtifact, error)
	// TaskArtifacts lists the artifacts of all the task's executions, oldest first.
	TaskArtifacts(ctx context.Context, taskID string) ([]domain.GitArtifact, error)
}

// PullRequestRequest asks to open a pull request of Branch into the integration branch of the project's
// Repository (or to return the open one of the branch).
type PullRequestRequest struct {
	ProjectID  string
	Repository string
	Branch     string
	Title      string
	Body       string
}

// PullRequest is an open pull request.
type PullRequest struct {
	Number int
	URL    string
	Base   string
	Reused bool // it was open already (created by an earlier delivery or by someone else)
}

// PullRequests is the integrations context's GitHub App as publishing needs it. An error marked
// messaging.Permanent cannot be fixed by retrying (repository not registered, App not installed, protected base);
// domain.ErrProtectedBranch is a refused main/master base.
type PullRequests interface {
	OpenPullRequest(ctx context.Context, r PullRequestRequest) (PullRequest, error)
}

type pullRequestPayload struct {
	ExecutionID string `json:"executionId"`
	Repository  string `json:"repository"`
	Branch      string `json:"branch"`
}

// RecordPush stores a publish attempt's push report (workspace.pushed). A report naming main/master is refused: it
// is audited and the attempt fails (the step is retried within its budget); nothing is stored.
func (s *Service) RecordPush(ctx context.Context, stepID, runnerID string, fence int64, p domain.Push) error {
	return s.updateExecution(ctx, stepID, func(ctx context.Context, e *domain.Execution, now time.Time) error {
		err := e.RecordPush(stepID, runnerID, fence, p)
		if !errors.Is(err, domain.ErrProtectedBranch) {
			return err
		}
		t, terr := s.Repo.Task(ctx, e.TaskID)
		if terr != nil {
			return terr
		}
		if err := s.record(ctx, auditapp.Entry{Actor: auditapp.Runner(runnerID, ""), Kind: KindPushRefused,
			ProjectID: t.ProjectID, Subject: auditSubjectTaskPrefix + t.ID, Details: map[string]string{"step": stepID,
				"repository": p.Repository, "branch": p.Branch, "head": p.HeadSHA}}); err != nil {
			return err
		}
		return e.Fail(stepID, runnerID, fence, err.Error(), now, s.maxAttempts())
	})
}

// published records the steps whose publish succeeded in this change (before → e) in the change set and queues
// their pull requests.
func (s *Service) published(ctx context.Context, before, e *domain.Execution, now time.Time) error {
	for i := range e.Steps {
		st := &e.Steps[i]
		if st.State != domain.StepSucceeded || before.Step(st.ID) == nil || before.Step(st.ID).State == domain.StepSucceeded {
			continue
		}
		p, ok := st.Published()
		if !ok {
			continue
		}
		if s.ChangeSets == nil || s.Outbox == nil {
			return errors.New("publishing is not configured: no change set store or outbox")
		}
		if err := s.ChangeSets.SaveArtifact(ctx, domain.NewGitArtifact(e, st, p, now)); err != nil {
			return fmt.Errorf("recording the change set: %w", err)
		}
		b, err := json.Marshal(pullRequestPayload{ExecutionID: e.ID, Repository: p.Repository, Branch: p.Branch})
		if err != nil {
			return err
		}
		key := "pull-request/" + e.ID + "/" + p.Repository + "/" + p.Branch + "/" + p.HeadSHA
		if err := s.Outbox.Enqueue(ctx, messaging.Message{Adapter: AdapterGitHub, Topic: TopicOpenPullRequest, Key: key,
			Payload: b}); err != nil {
			return fmt.Errorf("enqueueing %s: %w", TopicOpenPullRequest, err)
		}
	}
	return nil
}

// HandleOpenPullRequest is the outbox handler of TopicOpenPullRequest: it opens the pull request of a published
// branch into the repository's integration branch, or reuses the open one of the branch (idempotent: a redelivery
// finds the pull request it created), and records it. A refused base (main/master) or another permanent failure
// marks the artifact failed (audited); other errors are retried.
func (s *Service) HandleOpenPullRequest(ctx context.Context, d messaging.Delivery) error {
	var p pullRequestPayload
	if err := json.Unmarshal(d.Payload, &p); err != nil || p.ExecutionID == "" {
		return messaging.Permanent(errors.New("malformed pull request payload"))
	}
	if s.ChangeSets == nil || s.PullRequests == nil {
		return errors.New("publishing is not configured")
	}
	var req PullRequestRequest
	var task domain.Task
	err := s.Tx.InTx(ctx, func(ctx context.Context) error {
		a, err := s.ChangeSets.Artifact(ctx, p.ExecutionID, p.Repository, p.Branch)
		if err != nil {
			return err
		}
		if task, err = s.Repo.Task(ctx, a.TaskID); err != nil {
			return err
		}
		req = pullRequestFor(task, a)
		return nil
	})
	if errors.Is(err, domain.ErrNotFound) {
		return messaging.Permanent(err)
	}
	if err != nil {
		return err
	}
	pr, err := s.PullRequests.OpenPullRequest(ctx, req)
	if err == nil && domain.IsProtectedBranch(pr.Base) {
		err = fmt.Errorf("pull request #%d into %q: %w", pr.Number, pr.Base, domain.ErrProtectedBranch)
	}
	if err != nil && !messaging.IsPermanent(err) && !errors.Is(err, domain.ErrProtectedBranch) {
		return err
	}
	serr := s.Tx.InTx(ctx, func(ctx context.Context) error {
		a, aerr := s.ChangeSets.Artifact(ctx, p.ExecutionID, p.Repository, p.Branch)
		if aerr != nil {
			return aerr
		}
		now := s.now()
		entry := auditapp.Entry{Actor: auditapp.System(), ProjectID: task.ProjectID, Subject: auditSubjectTaskPrefix + task.ID,
			Details: map[string]string{"repository": a.Repository, "branch": a.Branch, "head": a.HeadSHA}}
		switch {
		case errors.Is(err, domain.ErrProtectedBranch):
			a.Failed(err.Error(), now)
			entry.Kind = KindPullRequestRefused
		case err != nil:
			a.Failed(err.Error(), now)
			entry.Kind, entry.Details["error"] = KindPullRequestFailed, a.Error
		default:
			if oerr := a.Opened(pr.Number, pr.URL, pr.Base, now); oerr != nil {
				return oerr
			}
			entry.Kind = KindPullRequestOpened
			if pr.Reused {
				entry.Kind = KindPullRequestReused
			}
			entry.Details["pullRequest"], entry.Details["base"] = strconv.Itoa(pr.Number), pr.Base
		}
		if err := s.ChangeSets.SaveArtifact(ctx, a); err != nil {
			return err
		}
		return s.record(ctx, entry)
	})
	if serr != nil {
		return serr
	}
	if err != nil {
		return messaging.Permanent(err)
	}
	return nil
}

// pullRequestFor is the pull request of a published branch: the task's title and where it came from.
func pullRequestFor(t domain.Task, a domain.GitArtifact) PullRequestRequest {
	title := strings.TrimSpace(t.Title)
	if t.YouTrackIssue != "" {
		title = t.YouTrackIssue + ": " + title
	}
	if r := []rune(title); len(r) > maxPullRequestTitleRune {
		title = string(r[:maxPullRequestTitleRune]) + "…"
	}
	body := fmt.Sprintf("Opened by kindevai for task %s (step %s).\n\nReviewed head: %s\nBase commit: %s\n",
		t.ID, a.StepID, a.HeadSHA, a.BaseSHA)
	if r := []rune(body); len(r) > pullRequestBodyMaxRunes {
		body = string(r[:pullRequestBodyMaxRunes])
	}
	return PullRequestRequest{ProjectID: t.ProjectID, Repository: a.Repository, Branch: a.Branch, Title: title, Body: body}
}

// TaskChangeSet lists the git artifacts (published branches and their pull requests) of a task (no access check:
// TaskDetail checks it).
func (s *Service) TaskChangeSet(ctx context.Context, taskID string) ([]domain.GitArtifact, error) {
	if s.ChangeSets == nil {
		return nil, nil
	}
	return s.ChangeSets.TaskArtifacts(ctx, taskID)
}
