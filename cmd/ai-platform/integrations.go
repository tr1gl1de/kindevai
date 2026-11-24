package main

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"time"

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
)

// integrationsModule is the integrations context: credentials vault, YouTrack connection, project bindings and
// repository registry. Service is the port later adapters (KIN-27/28/30/39) read settings and credentials from.
type integrationsModule struct {
	Service *integrationsapp.Service
	HTTP    server.Module
}

// newIntegrations wires the integrations context and registers its outbox handlers; call it before msg.start.
func newIntegrations(inst *bootstrap.Installation, msg *messagingModule, audit *auditModule, logger *slog.Logger) *integrationsModule {
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
	return &integrationsModule{Service: svc, HTTP: &integrationshttp.Handler{Service: svc, Logger: logger}}
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
