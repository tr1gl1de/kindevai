package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// YouTrack write-back (KIN-38, KB KIN-A-19): a task of a YouTrack issue tells the issue, in the transaction of the
// change, what happened — the accepted plan with the link to its approval, every state change (mapped by the
// project; comments only by default) and the outcome. The integrations context delivers through the outbox; the
// keys below are the writes' correlation ids, unique per change, so a repeated use case never writes twice.

// IssueSync is the integrations context's write-back as the workflow needs it; the request methods join the
// caller's transaction.
type IssueSync interface {
	Comment(ctx context.Context, key, taskID, issueID, text string) error
	State(ctx context.Context, key, taskID, projectID, issueID, taskState string) error
	Article(ctx context.Context, key, taskID, issueID, articleID, section, content string) error
	Publication(ctx context.Context, taskID string) (Publication, error)
}

// Publication sums a task's write-back: Pending writes are "awaiting publication" (being retried while YouTrack is
// unavailable), Failed ones were given up (LastError says why).
type Publication struct {
	Pending, Failed, Published int
	LastError                  string
}

// DocsStore keeps the documentation decisions of completed runs (KIN-38).
type DocsStore interface {
	SaveDocs(ctx context.Context, d domain.DocsDecision) error
	LatestDocs(ctx context.Context, taskID string) (domain.DocsDecision, error) // domain.ErrNotFound
}

// taskLink is the task page on the public URL ("" when it is not configured).
func (s *Service) taskLink(taskID string) string {
	if s.TaskLinkBase == "" {
		return ""
	}
	return strings.TrimRight(s.TaskLinkBase, "/") + "/tasks/" + taskID
}

// syncIssue writes back the change of a saved task (from the stored one): its state, a new plan revision, the end.
func (s *Service) syncIssue(ctx context.Context, stored, t domain.Task) error {
	if s.IssueSync == nil || t.YouTrackIssue == "" {
		return nil
	}
	prefix := "task/" + t.ID + "/"
	if stored.State != t.State {
		key := prefix + "v" + strconv.FormatInt(t.Version, 10) + "/state"
		if err := s.IssueSync.State(ctx, key, t.ID, t.ProjectID, t.YouTrackIssue, string(t.State)); err != nil {
			return fmt.Errorf("youtrack state: %w", err)
		}
	}
	if t.PlanRevision != stored.PlanRevision && (t.State == domain.TaskReady || t.State == domain.TaskAwaitingApproval) {
		if err := s.planComment(ctx, prefix+"plan/"+strconv.Itoa(t.PlanRevision), t); err != nil {
			return err
		}
	}
	if stored.State != t.State && t.State.Terminal() {
		return s.finalComment(ctx, prefix+"v"+strconv.FormatInt(t.Version, 10)+"/final", t)
	}
	return nil
}

func (s *Service) planComment(ctx context.Context, key string, t domain.Task) error {
	rev, err := s.Repo.Revision(ctx, t.ID, t.PlanRevision)
	if err != nil {
		return err
	}
	p := rev.Plan()
	text := domain.PlanComment(domain.PlanNote{Revision: rev.Number(), Summary: p.Summary, Risk: rev.Risk(),
		Steps: len(p.Steps), NeedsApproval: t.State == domain.TaskAwaitingApproval, Link: s.taskLink(t.ID)})
	if err := s.IssueSync.Comment(ctx, key, t.ID, t.YouTrackIssue, text); err != nil {
		return fmt.Errorf("youtrack plan comment: %w", err)
	}
	return nil
}

// finalComment reports the end: the pull requests, the agent time (CLI process time, KIN-25) and, for a completed
// task, the documentation decision, whose in-scope article changes are written as well.
func (s *Service) finalComment(ctx context.Context, key string, t domain.Task) error {
	n := domain.FinalNote{State: t.State, Reason: t.Reason.Detail, Link: s.taskLink(t.ID)}
	if s.ChangeSets != nil {
		arts, err := s.ChangeSets.TaskArtifacts(ctx, t.ID)
		if err != nil {
			return err
		}
		for _, a := range arts {
			if a.PRURL != "" && !slices.Contains(n.PullRequests, a.PRURL) {
				n.PullRequests = append(n.PullRequests, a.PRURL)
			}
		}
	}
	times, _, err := s.taskTimes(ctx, TaskFilter{TaskIDs: []string{t.ID}}, []string{t.ID})
	if err != nil {
		return err
	}
	n.AgentTime = times[t.ID].Agent
	if t.State == domain.TaskCompleted {
		d, err := s.decideDocs(ctx, t)
		if err != nil {
			return err
		}
		n.Docs = d
	}
	if err := s.IssueSync.Comment(ctx, key, t.ID, t.YouTrackIssue, domain.FinalComment(n)); err != nil {
		return fmt.Errorf("youtrack final comment: %w", err)
	}
	return nil
}

// decideDocs takes the documentation step of the completed run: the implementers' handoffs of the latest execution
// against the approved plan's docs scope. The decision is stored; its article changes are written.
func (s *Service) decideDocs(ctx context.Context, t domain.Task) (*domain.DocsDecision, error) {
	e, err := s.Repo.LatestExecution(ctx, t.ID)
	if errors.Is(err, domain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rev, err := s.Repo.Revision(ctx, t.ID, e.PlanRevision)
	if err != nil {
		return nil, err
	}
	var h domain.Handoff
	for i := range e.Steps {
		impl, ok := e.Steps[i].LastImplementation()
		if !ok {
			continue
		}
		sh := domain.ParseHandoff(impl.Report.Result)
		h.Articles = append(h.Articles, sh.Articles...)
		if h.DocsNotApplicable == "" {
			h.DocsNotApplicable = sh.DocsNotApplicable
		}
	}
	d := domain.DecideDocs(t.ID, e.ID, h, rev.Plan().Docs)
	if s.Docs != nil {
		if err := s.Docs.SaveDocs(ctx, d); err != nil {
			return nil, err
		}
	}
	for i, a := range d.Articles {
		key := "task/" + t.ID + "/" + e.ID + "/article/" + strconv.Itoa(i)
		if err := s.IssueSync.Article(ctx, key, t.ID, t.YouTrackIssue, a.Article, a.Section, a.Content); err != nil {
			return nil, fmt.Errorf("youtrack article: %w", err)
		}
	}
	return &d, nil
}
