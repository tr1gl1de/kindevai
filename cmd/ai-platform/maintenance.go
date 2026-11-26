package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	maintenancehttp "github.com/tr1gl1de/kindevai/internal/maintenance/adapters/httpapi"
	maintenancedb "github.com/tr1gl1de/kindevai/internal/maintenance/adapters/sqlite"
	maintenanceapp "github.com/tr1gl1de/kindevai/internal/maintenance/app"
	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	workflowdb "github.com/tr1gl1de/kindevai/internal/workflow/adapters/sqlite"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
)

// newMaintenance wires maintenance mode (KIN-41): the persisted mode is loaded, the scheduler pauses on it and the
// returned handler serves /api/maintenance and the read-only middleware.
func newMaintenance(ctx context.Context, inst *bootstrap.Installation, audit *auditModule, wf *workflowapp.Service,
	logger *slog.Logger) (*maintenancehttp.Handler, error) {
	svc := &maintenanceapp.Service{
		Store: maintenancedb.New(inst.DB),
		Audit: audit.Service,
		Tx:    db.Transactor{DB: inst.DB},
		Drain: workflowdb.New(inst.DB),
		Now:   time.Now,
	}
	if err := svc.Load(ctx); err != nil {
		return nil, err
	}
	if m := svc.Mode(); m.Enabled {
		logger.Warn("maintenance mode is on: no new steps are scheduled and user changes are refused",
			"since", m.Since, "by", m.By)
	}
	if wf == nil {
		return nil, fmt.Errorf("maintenance: no workflow service")
	}
	wf.Pause = svc
	return &maintenancehttp.Handler{Service: svc, Logger: logger}, nil
}
