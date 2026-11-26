package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	projectsdb "github.com/tr1gl1de/kindevai/internal/projects/adapters/sqlite"
	projectsapp "github.com/tr1gl1de/kindevai/internal/projects/app"
	"github.com/tr1gl1de/kindevai/internal/protocol"
	routinghttp "github.com/tr1gl1de/kindevai/internal/routing/adapters/httpapi"
	routingdb "github.com/tr1gl1de/kindevai/internal/routing/adapters/sqlite"
	routingapp "github.com/tr1gl1de/kindevai/internal/routing/app"
	"github.com/tr1gl1de/kindevai/internal/server"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// newRouting wires the agent router (KIN-33): CLI switches, role bindings and cool-downs, the scheduler's routing
// of AI steps, and the emergency stop through the workflow. The returned service's CoolDown is the hook for
// cli_limit failures (KIN-79).
func newRouting(inst *bootstrap.Installation, audit *auditModule, wf *workflowapp.Service,
	logger *slog.Logger) (*routingapp.Service, server.Module) {
	svc := &routingapp.Service{
		Repo:     routingdb.New(inst.DB),
		Tx:       db.Transactor{DB: inst.DB},
		Audit:    audit.Service,
		Access:   taskAccess{&projectsapp.Service{Repo: projectsdb.New(inst.DB)}},
		Workflow: wf,
		Outbox:   wf.Outbox, // KIN-83: held CLIs raise alerts
		Now:      time.Now,
	}
	wf.Agents = workflowRouter{svc: svc}
	wf.CLILimits = cliCooldowns{svc: svc} // KIN-79: a cli_limit failure cools the runner's CLI down in routing
	return svc, &routinghttp.Handler{Service: svc, Logger: logger}
}

// workflowRouter gives the scheduler the routing context's chain for a step.
type workflowRouter struct{ svc *routingapp.Service }

func (r workflowRouter) Route(ctx context.Context, projectID, role, author string) (workflowapp.AgentRoute, error) {
	rt, err := r.svc.Route(ctx, projectID, role, author)
	if err != nil {
		return workflowapp.AgentRoute{}, err
	}
	out := workflowapp.AgentRoute{Blocked: rt.Blocked, Chain: make([]domain.AgentChoice, 0, len(rt.Chain)),
		Cooling: func(runnerID, agent string) (domain.Cooling, bool) {
			h, ok := rt.Cooling(runnerID, agent)
			return domain.Cooling{Until: h.Until, NeedsOwner: h.NeedsOwner()}, ok
		}}
	for _, c := range rt.Chain {
		out.Chain = append(out.Chain, domain.AgentChoice{Agent: c.Agent, Model: c.Model})
	}
	return out, nil
}

// cliCooldowns records a runner's CLI that hit its limit as a routing hold (KIN-79, KIN-83: backoff and the
// Owner's "ready again" for logged-out or billing-refused CLIs), in the report's transaction, and ends the pair's
// streak after a success. An attempt without a known CLI (the mock agent) has nothing to hold.
type cliCooldowns struct{ svc *routingapp.Service }

func (c cliCooldowns) CLILimitHit(ctx context.Context, runnerID, agent, cause string, resetsAt time.Time,
	reason string) (workflowapp.CLIHold, error) {
	if !protocol.CLIAdapter(agent) {
		return workflowapp.CLIHold{}, nil
	}
	h, err := c.svc.CoolDown(ctx, runnerID, agent, cause, resetsAt, reason)
	if err != nil {
		return workflowapp.CLIHold{}, err
	}
	return workflowapp.CLIHold{Known: true, Until: h.Until, NeedsOwner: h.NeedsOwner()}, nil
}

func (c cliCooldowns) CLIWorked(ctx context.Context, runnerID, agent string) error {
	if !protocol.CLIAdapter(agent) {
		return nil
	}
	return c.svc.Recovered(ctx, runnerID, agent)
}
