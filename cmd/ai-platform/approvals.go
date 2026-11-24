package main

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"time"

	approvalshttp "github.com/tr1gl1de/kindevai/internal/approvals/adapters/httpapi"
	approvalsdb "github.com/tr1gl1de/kindevai/internal/approvals/adapters/sqlite"
	approvalswf "github.com/tr1gl1de/kindevai/internal/approvals/adapters/workflow"
	approvalsapp "github.com/tr1gl1de/kindevai/internal/approvals/app"
	integrationsapp "github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	projectsdb "github.com/tr1gl1de/kindevai/internal/projects/adapters/sqlite"
	projectsapp "github.com/tr1gl1de/kindevai/internal/projects/app"
	projects "github.com/tr1gl1de/kindevai/internal/projects/domain"
	"github.com/tr1gl1de/kindevai/internal/server"
	workflowdb "github.com/tr1gl1de/kindevai/internal/workflow/adapters/sqlite"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
	workflow "github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// approvalsModule is the workflow context (no HTTP API yet, KIN-25) wired with the approvals context: the
// workflow takes every plan's effective risk from the approvals Policy Evaluator and reports accepted revisions
// and closed tasks to it in the same transaction; the Approval Center decides through the workflow's app API.
type approvalsModule struct {
	Workflow  *workflowapp.Service
	Approvals *approvalsapp.Service
	HTTP      server.Module
}

func newApprovals(inst *bootstrap.Installation, msg *messagingModule, audit *auditModule, integ *integrationsModule,
	logger *slog.Logger) *approvalsModule {
	tx := db.Transactor{DB: inst.DB}
	projectsSvc := &projectsapp.Service{Repo: projectsdb.New(inst.DB)}
	apv := &approvalsapp.Service{
		Repo:     approvalsdb.New(inst.DB),
		Projects: approvalProjects{projectsSvc},
		Tx:       tx,
		Audit:    audit.Service,
		Outbox:   msg.Outbox,
		Now:      time.Now,
		NewID:    rand.Text,
	}
	hooks := approvalswf.Hooks{Svc: apv}
	wf := &workflowapp.Service{
		Repo:     workflowdb.New(inst.DB),
		Projects: workflowRepos{projects: projectsSvc, integ: integ.Service},
		Policy:   hooks,
		Events:   hooks,
		Tx:       tx,
		Now:      time.Now,
		NewID:    rand.Text,
	}
	apv.Workflow = approvalswf.Port{Svc: wf}
	return &approvalsModule{Workflow: wf, Approvals: apv,
		HTTP: &approvalshttp.Handler{Service: apv, Logger: logger}}
}

// approvalProjects gives the approvals context the projects visible to a user, with their role.
type approvalProjects struct{ svc *projectsapp.Service }

func (c approvalProjects) Visible(ctx context.Context, p auth.Principal) ([]approvalsapp.ProjectRef, error) {
	ps, err := c.svc.List(ctx, projects.Actor{UserID: p.UserID, Login: p.Login, Owner: p.Owner})
	if err != nil {
		return nil, err
	}
	out := make([]approvalsapp.ProjectRef, 0, len(ps))
	for _, v := range ps {
		out = append(out, approvalsapp.ProjectRef{ID: v.Project.ID, Name: string(v.Project.Name), Role: v.MyRole})
	}
	return out, nil
}

// workflowRepos gives the workflow context a project's registered repositories ("owner/name") from integrations;
// workflow.ErrNotFound for an unknown project.
type workflowRepos struct {
	projects *projectsapp.Service
	integ    *integrationsapp.Service
}

func (c workflowRepos) Repositories(ctx context.Context, projectID string) ([]string, error) {
	if _, err := c.projects.Lookup(ctx, projectID); errors.Is(err, projects.ErrNotFound) {
		return nil, workflow.ErrNotFound
	} else if err != nil {
		return nil, err
	}
	rs, err := c.integ.RepositoriesOf(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Owner+"/"+r.Name)
	}
	return out, nil
}
