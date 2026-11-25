package main

import (
	"context"
	"net/url"
	"strings"
	"time"

	integrationsdb "github.com/tr1gl1de/kindevai/internal/integrations/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/youtrack"
	integrationsapp "github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	workflowdb "github.com/tr1gl1de/kindevai/internal/workflow/adapters/sqlite"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
)

// wireWriteBack turns on the YouTrack write-back (KIN-38): task changes of YouTrack issues become outbox writes
// (comments, mapped states, Knowledge Base sections) delivered as the integration user, the documentation decisions
// are stored, and the comments link to the task page on the first --public-url origin. Call it before msg.start.
func wireWriteBack(wf *workflowapp.Service, inst *bootstrap.Installation, msg *messagingModule, integ *integrationsModule,
	publicURL string) {
	repo := integrationsdb.New(inst.DB)
	s := &integrationsapp.IssueSync{Connection: integ.Service, Client: &youtrack.Client{}, Outbox: msg.Outbox, Store: repo,
		Tx: db.Transactor{DB: inst.DB}, Now: time.Now}
	wf.IssueSync, wf.Docs, wf.TaskLinkBase = workflowIssueSync{s}, workflowdb.New(inst.DB), taskLinkBase(publicURL)
	msg.Outbox.Handle(integrationsapp.TopicYouTrackWrite, s.HandleWrite)
	// KIN-108: a write whose message dies outside HandleWrite fails (code "dead") with its alert; a retry reopens it.
	msg.Outbox.OnDead(integrationsapp.TopicYouTrackWrite, s.HandleDead)
	msg.Outbox.OnRetry(integrationsapp.TopicYouTrackWrite, s.HandleRetried)
}

// taskLinkBase is the first origin of --public-url ("" when none or malformed; main validates the flag).
func taskLinkBase(publicURL string) string {
	first, _, _ := strings.Cut(publicURL, ",")
	u, err := url.Parse(strings.TrimSpace(first))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// workflowIssueSync gives the workflow the integrations context's write-back.
type workflowIssueSync struct{ s *integrationsapp.IssueSync }

func (w workflowIssueSync) Comment(ctx context.Context, key, taskID, projectID, issueID, text string) error {
	return w.s.Comment(ctx, key, taskID, projectID, issueID, text)
}

func (w workflowIssueSync) State(ctx context.Context, key, taskID, projectID, issueID, taskState string) error {
	return w.s.State(ctx, key, taskID, projectID, issueID, taskState)
}

func (w workflowIssueSync) Article(ctx context.Context, key, taskID, projectID, issueID, articleID, section,
	content string) error {
	return w.s.Article(ctx, key, taskID, projectID, issueID, articleID, section, content)
}

func (w workflowIssueSync) Publication(ctx context.Context, taskID string) (workflowapp.Publication, error) {
	p, err := w.s.Publication(ctx, taskID)
	return workflowapp.Publication{Pending: p.Pending, Failed: p.Failed, Published: p.Published, LastError: p.LastError}, err
}
