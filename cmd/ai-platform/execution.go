package main

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	runnersgw "github.com/tr1gl1de/kindevai/internal/runners/adapters/gateway"
	runnershttp "github.com/tr1gl1de/kindevai/internal/runners/adapters/httpapi"
	runnersapp "github.com/tr1gl1de/kindevai/internal/runners/app"
	"github.com/tr1gl1de/kindevai/internal/server"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
)

// DefaultSchedulerInterval is how often the scheduler looks for work when nothing wakes it.
const DefaultSchedulerInterval = time.Second

// wireExecution connects the workflow with the runners context for mock execution end to end (KIN-23): the
// scheduler takes candidates from runner telemetry and the live sessions, assignments and cancels go into the
// runner's ordered command log in the workflow's transaction, runner heartbeats renew the runner's leases and a
// runner's reconciliation resolves its attempts.
func wireExecution(wf *workflowapp.Service, ch *runnersapp.Channel, gw *runnersgw.Handler, tel *runnersapp.Telemetry,
	hub *runnersapp.Hub, logger *slog.Logger) {
	wf.Runners = workflowRunners{tel: tel, hub: hub}
	wf.Dispatcher = runnerDispatcher{ch: ch}
	ch.Reconciler = workflowReconciler{wf: wf}
	if hb := gw.Messages[protocol.TypeHeartbeat]; hb != nil {
		gw.Messages[protocol.TypeHeartbeat] = func(ctx context.Context, s runnersapp.Session, env protocol.Envelope) error {
			if err := hb(ctx, s, env); err != nil {
				return err
			}
			if err := wf.RenewRunnerLeases(ctx, s.RunnerID); err != nil {
				logger.Warn("renewing the runner's leases", "runner", s.RunnerID, "err", err)
			}
			return nil
		}
	}
}

// wireRunnerRelease lets the Owner's disable and revoke end the runner's attempts in the same transaction (KIN-63):
// the runners management service built by newProjects gets the workflow as its Attempts port.
func wireRunnerRelease(modules []server.Module, wf *workflowapp.Service) {
	for _, m := range modules {
		if h, ok := m.(*runnershttp.Handler); ok && h.Service != nil {
			h.Service.Attempts = wf
		}
	}
}

// startScheduler runs scheduler passes every interval until ctx ends; the returned stop waits for the loop.
func startScheduler(ctx context.Context, wf *workflowapp.Service, every time.Duration, logger *slog.Logger) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			if err := wf.Schedule(ctx); err != nil && ctx.Err() == nil {
				logger.Error("scheduler pass", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	})
	return func() { cancel(); wg.Wait() }
}

// workflowRunners gives the scheduler the runners context's candidates.
type workflowRunners struct {
	tel *runnersapp.Telemetry
	hub *runnersapp.Hub
}

func (w workflowRunners) Candidates(ctx context.Context, projectID string) ([]workflowapp.Candidate, error) {
	cs, err := w.tel.Candidates(ctx, projectID, w.hub.Online)
	if err != nil {
		return nil, err
	}
	out := make([]workflowapp.Candidate, 0, len(cs))
	for _, c := range cs {
		out = append(out, workflowapp.Candidate{ID: c.ID, Tools: c.Tools, DiskHeavyAllowed: c.DiskHeavyAllowed})
	}
	return out, nil
}

// runnerDispatcher queues the workflow's commands in the runner's command log (joins the caller's transaction).
// Until the CLI adapters (KIN-32) every assignment runs the mock agent; the mock planner names its step after the
// scenario.
type runnerDispatcher struct{ ch *runnersapp.Channel }

func mockScenario(stepKey string) string {
	if slices.Contains(protocol.MockScenarios, stepKey) {
		return stepKey
	}
	return protocol.ScenarioSuccess
}

func (d runnerDispatcher) Assign(ctx context.Context, runnerID string, a workflowapp.Assignment) error {
	scope := a.Scope
	if scope == nil {
		scope = []string{}
	}
	return d.ch.Send(ctx, runnerID, protocol.AssignCommandID(a.StepID, a.Fence), protocol.CommandAssign, protocol.Assign{
		Step: a.StepID, Fence: a.Fence, Execution: a.ExecutionID, Task: a.TaskID, Project: a.ProjectID,
		Repository: a.Repository, Scope: scope, Adapter: protocol.AdapterMock, Scenario: mockScenario(a.StepKey)})
}

func (d runnerDispatcher) Cancel(ctx context.Context, runnerID, stepID string, fence int64, reason string) error {
	return d.ch.Send(ctx, runnerID, protocol.CancelCommandID(stepID, fence), protocol.CommandCancel,
		protocol.Cancel{Step: stepID, Fence: fence, Reason: reason})
}

// workflowReconciler hands a runner's runner.reconcile.response to the workflow.
type workflowReconciler struct{ wf *workflowapp.Service }

func (r workflowReconciler) Reconciled(ctx context.Context, runnerID string, resp protocol.ReconcileResponse) error {
	reports := make([]workflowapp.AttemptReport, 0, len(resp.Attempts))
	for _, a := range resp.Attempts {
		reports = append(reports, workflowapp.AttemptReport{StepID: a.Step, Fence: a.Fence, Running: a.Running,
			Result: a.Result != ""})
	}
	return r.wf.ReconcileRunner(ctx, runnerID, reports)
}
