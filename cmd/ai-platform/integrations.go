package main

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"time"

	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/github"
	integrationshttp "github.com/tr1gl1de/kindevai/internal/integrations/adapters/httpapi"
	integrationsdb "github.com/tr1gl1de/kindevai/internal/integrations/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/youtrack"
	integrationsapp "github.com/tr1gl1de/kindevai/internal/integrations/app"
	integrations "github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	projectsdb "github.com/tr1gl1de/kindevai/internal/projects/adapters/sqlite"
	projectsapp "github.com/tr1gl1de/kindevai/internal/projects/app"
	projects "github.com/tr1gl1de/kindevai/internal/projects/domain"
	"github.com/tr1gl1de/kindevai/internal/server"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
	workflow "github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// integrationsModule is the integrations context: credentials vault, YouTrack connection, project bindings and
// repository registry. Service is the port later adapters (KIN-27/28/30/39) read settings and credentials from.
type integrationsModule struct {
	Service *integrationsapp.Service
	GitHub  *integrationsapp.GitHubService // GitHub App (KIN-28): RepositoryToken for later adapters
	HTTP    server.Module
}

// newIntegrations wires the integrations context and registers its outbox handlers; call it before msg.start.
func newIntegrations(inst *bootstrap.Installation, msg *messagingModule, audit *auditModule, githubAPI string, logger *slog.Logger) *integrationsModule {
	repo := integrationsdb.New(inst.DB)
	svc := &integrationsapp.Service{
		Vault:    &integrationsapp.Vault{Store: repo, Cipher: inst.Box, Now: time.Now},
		Store:    repo,
		Projects: integrationProjects{&projectsapp.Service{Repo: projectsdb.New(inst.DB)}},
		Tx:       db.Transactor{DB: inst.DB},
		Audit:    audit.Service,
		Outbox:   msg.Outbox,
		Probe:    &youtrack.Probe{},
		Now:      time.Now,
		NewID:    rand.Text,
		Logger:   logger,
	}
	msg.Outbox.Handle(integrationsapp.TopicYouTrackCheck, svc.HandleYouTrackCheck)
	gh := &integrationsapp.GitHubService{
		Vault:    svc.Vault,
		Store:    repo,
		Projects: svc.Projects,
		Tx:       svc.Tx,
		Audit:    audit.Service,
		Outbox:   msg.Outbox,
		Inbox:    msg.Inbox,
		API:      &github.Client{BaseURL: githubAPI},
		Now:      time.Now,
		NewID:    rand.Text,
		Logger:   logger,
	}
	svc.RepositoryAdded = gh.RepositoryAdded
	msg.Outbox.Handle(integrationsapp.TopicGitHubCheck, gh.HandleGitHubCheck)
	msg.Outbox.Handle(integrationsapp.TopicRepositorySync, gh.HandleRepositorySync)
	msg.Inbox.Handle(integrationsapp.TopicGitHubEvent, gh.HandleGitHubEvent)
	return &integrationsModule{Service: svc, GitHub: gh, HTTP: &integrationshttp.Handler{Service: svc, GitHub: gh, Logger: logger}}
}

// workflowRepositories implements the workflow context's port Projects.Repositories (KIN-21 plan validation) with
// the integrations repository registry: a project's repositories as "owner/name", workflow's ErrNotFound for an
// unknown project. Wire it into workflowapp.Service.Projects when the workflow context is served.
type workflowRepositories struct {
	projects *projectsapp.Service
	repos    *integrationsapp.Service
}

var _ workflowapp.Projects = workflowRepositories{}

func (w workflowRepositories) Repositories(ctx context.Context, projectID string) ([]string, error) {
	if _, err := w.projects.Lookup(ctx, projectID); errors.Is(err, projects.ErrNotFound) {
		return nil, workflow.ErrNotFound
	} else if err != nil {
		return nil, err
	}
	rs, err := w.repos.RepositoriesOf(ctx, projectID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.FullName())
	}
	return out, nil
}

// integrationProjects gives the integrations context the projects context's app API, translating its errors.
type integrationProjects struct{ svc *projectsapp.Service }

func (c integrationProjects) Lookup(ctx context.Context, id string) (integrationsapp.ProjectRef, error) {
	p, err := c.svc.Lookup(ctx, id)
	if errors.Is(err, projects.ErrNotFound) {
		return integrationsapp.ProjectRef{}, integrations.ErrNotFound
	}
	if err != nil {
		return integrationsapp.ProjectRef{}, err
	}
	return integrationsapp.ProjectRef{ID: p.ID, Name: string(p.Name)}, nil
}
