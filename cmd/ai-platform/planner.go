package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/youtrack"
	integrationsapp "github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	workflowdb "github.com/tr1gl1de/kindevai/internal/workflow/adapters/sqlite"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
	workflow "github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// wirePlanner turns on the planner step (KIN-34): planning runs are stored with the workflow, and a task linked to a
// YouTrack issue gets its context package (issue text, linked Knowledge Base articles) read by an outbox handler.
// Call it before msg.start.
func wirePlanner(wf *workflowapp.Service, inst *bootstrap.Installation, msg *messagingModule, integ *integrationsModule) {
	wf.Planning = workflowdb.New(inst.DB)
	wf.IssueContext = plannerContext{reader: integrationsapp.IssueContextReader{Settings: integ.Service.YouTrackSettings,
		Source: &youtrack.Client{}}}
	msg.Outbox.Handle(workflowapp.TopicPlanningContext, wf.HandlePlanningContext)
}

// plannerContext translates the integrations context's issue context into the workflow's context package. The
// text stays untrusted data: the workflow only bounds and fences it.
type plannerContext struct {
	reader integrationsapp.IssueContextReader
}

func (p plannerContext) IssueContext(ctx context.Context, _, issueID string) (workflow.PlanningContext, error) {
	c, err := p.reader.Read(ctx, issueID)
	if errors.Is(err, integrationsapp.ErrContextUnavailable) {
		return workflow.PlanningContext{}, fmt.Errorf("%w: %w", workflowapp.ErrContextUnavailable, err)
	}
	if err != nil {
		return workflow.PlanningContext{}, err
	}
	out := workflow.PlanningContext{IssueText: c.Description}
	for _, a := range c.Articles {
		out.Articles = append(out.Articles, workflow.ContextArticle{ID: a.ID, Title: a.Title, Text: a.Content})
	}
	if len(c.Missing) > 0 {
		out.Note = "linked articles not readable: " + strings.Join(c.Missing, ", ")
	}
	return out, nil
}
