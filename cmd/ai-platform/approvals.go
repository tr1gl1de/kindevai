package main

import (
	"context"
	"crypto/rand"
	"log/slog"
	"time"

	approvalshttp "github.com/tr1gl1de/kindevai/internal/approvals/adapters/httpapi"
	approvalsdb "github.com/tr1gl1de/kindevai/internal/approvals/adapters/sqlite"
	approvalswf "github.com/tr1gl1de/kindevai/internal/approvals/adapters/workflow"
	approvalsapp "github.com/tr1gl1de/kindevai/internal/approvals/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	projectsdb "github.com/tr1gl1de/kindevai/internal/projects/adapters/sqlite"
	projectsapp "github.com/tr1gl1de/kindevai/internal/projects/app"
	projects "github.com/tr1gl1de/kindevai/internal/projects/domain"
	"github.com/tr1gl1de/kindevai/internal/server"
	workflowhttp "github.com/tr1gl1de/kindevai/internal/workflow/adapters/httpapi"
	workflownotify "github.com/tr1gl1de/kindevai/internal/workflow/adapters/notify"
	workflowdb "github.com/tr1gl1de/kindevai/internal/workflow/adapters/sqlite"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
)

// approvalsModule is the workflow context (minimal task API, KIN-23; full UI KIN-25) wired with the approvals context: the
// workflow takes every plan's effective risk from the approvals Policy Evaluator and reports accepted revisions
// and closed tasks to it in the same transaction; the Approval Center decides through the workflow's app API.
type approvalsModule struct {
	Workflow  *workflowapp.Service
	Approvals *approvalsapp.Service
	HTTP      server.Module
	Tasks     server.Module // minimal task API (KIN-23)
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
		Projects: workflowRepositories{projects: projectsSvc, repos: integ.Service}, // KIN-28 registry validates plans
		// KIN-35: the registry's test command of a repository for the review loop's implementer attempts
		TestCommands: workflowRepositories{projects: projectsSvc, repos: integ.Service},
		Access:       taskAccess{projectsSvc},
		Policy:       hooks,
		Events:       hooks,
		Outbox:       msg.Outbox,                                  // KIN-60 BLOCKED alerts
		Notifier:     workflownotify.Notifier{Outbox: msg.Outbox}, // finished tasks → Telegram (KIN-62)
		Tx:           tx,
		Now:          time.Now,
		NewID:        rand.Text,
	}
	apv.Workflow = approvalswf.Port{Svc: wf}
	return &approvalsModule{Workflow: wf, Approvals: apv,
		HTTP: &approvalshttp.Handler{Service: apv, Logger: logger}, Tasks: &workflowhttp.Handler{Service: wf, Logger: logger}}
}

// taskAccess gives the task API (KIN-23) the projects visible to a user, with their role.
type taskAccess struct{ svc *projectsapp.Service }

func (c taskAccess) Visible(ctx context.Context, p auth.Principal) (map[string]string, error) {
	ps, err := c.svc.List(ctx, projects.Actor{UserID: p.UserID, Login: p.Login, Owner: p.Owner})
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(ps))
	for _, v := range ps {
		out[v.Project.ID] = v.MyRole
	}
	return out, nil
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
