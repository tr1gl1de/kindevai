package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/protocol"
	runnersgw "github.com/tr1gl1de/kindevai/internal/runners/adapters/gateway"
	runnershttp "github.com/tr1gl1de/kindevai/internal/runners/adapters/httpapi"
	runnersdb "github.com/tr1gl1de/kindevai/internal/runners/adapters/sqlite"
	runnersapp "github.com/tr1gl1de/kindevai/internal/runners/app"
	runnersdomain "github.com/tr1gl1de/kindevai/internal/runners/domain"
	"github.com/tr1gl1de/kindevai/internal/server"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
	workflowdomain "github.com/tr1gl1de/kindevai/internal/workflow/domain"
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
	// strict says, per project, which runners' network profile is strict (KIN-31); nil: none is.
	strict func(ctx context.Context, projectID string) (func(runnerID string) bool, error)
}

// Candidates lists the project's candidate runners. A runner whose effective network profile is strict is a
// candidate only once its strict egress sandbox verified (runner.capabilities strictEgress): macOS and hosts
// without namespaces never get such steps (KIN-31).
func (w workflowRunners) Candidates(ctx context.Context, projectID string) ([]workflowapp.Candidate, error) {
	cs, err := w.tel.Candidates(ctx, projectID, w.hub.Online)
	if err != nil {
		return nil, err
	}
	isStrict := func(string) bool { return false }
	if w.strict != nil && len(cs) > 0 {
		if isStrict, err = w.strict(ctx, projectID); err != nil {
			return nil, err
		}
	}
	return schedulable(cs, isStrict), nil
}

// schedulable converts candidates for the scheduler, dropping those that must enforce strict egress and cannot.
func schedulable(cs []runnersapp.Candidate, isStrict func(runnerID string) bool) []workflowapp.Candidate {
	out := make([]workflowapp.Candidate, 0, len(cs))
	for _, c := range cs {
		if !c.StrictEgress && isStrict(c.ID) {
			continue
		}
		out = append(out, workflowapp.Candidate{ID: c.ID, Tools: c.Tools, DiskHeavyAllowed: c.DiskHeavyAllowed,
			Disk: diskLevel(c.Disk), MaxParallel: c.MaxParallelSteps})
	}
	return out
}

// diskLevel ranks a runner's disk level for placement (KIN-76); critical is excluded by DiskHeavyAllowed already.
func diskLevel(l runnersdomain.Level) workflowdomain.DiskLevel {
	switch l {
	case runnersdomain.LevelWarning:
		return workflowdomain.DiskWarning
	case runnersdomain.LevelHigh, runnersdomain.LevelCritical:
		return workflowdomain.DiskHigh
	default:
		return workflowdomain.DiskOK
	}
}

// wireStrictEgress makes the scheduler check strict network profiles (KIN-31) once the network service exists.
func wireStrictEgress(wf *workflowapp.Service, strict func(ctx context.Context, projectID string) (func(string) bool, error)) {
	r, ok := wf.Runners.(workflowRunners)
	if !ok { // a wiring error: strict steps would reach runners that cannot enforce them
		panic("wireStrictEgress: the scheduler's runners are not workflowRunners")
	}
	r.strict = strict
	wf.Runners = r
}

// runnerDispatcher queues the workflow's commands in the runner's command log (joins the caller's transaction).
// An assignment for an AI CLI (claude, codex; chosen by routing, KIN-33) carries the CLI run (KIN-79); any other
// runs the mock agent, whose planner names its step after the scenario.
type runnerDispatcher struct{ ch *runnersapp.Channel }

func mockScenario(stepKey string) string {
	if slices.Contains(protocol.MockScenarios, stepKey) {
		return stepKey
	}
	return protocol.ScenarioSuccess
}

func (d runnerDispatcher) Assign(ctx context.Context, runnerID string, a workflowapp.Assignment) error {
	return d.ch.Send(ctx, runnerID, protocol.AssignCommandID(a.StepID, a.Fence), protocol.CommandAssign, assignBody(a))
}

// assignBody is the execution.assign of an assignment: the AI CLI, model and role routing chose (KIN-33), or the
// mock agent with the scenario named by the step for mock plans (and without routing).
func assignBody(a workflowapp.Assignment) protocol.Assign {
	scope := a.Scope
	if scope == nil {
		scope = []string{}
	}
	body := protocol.Assign{Step: a.StepID, Fence: a.Fence, Execution: a.ExecutionID, Task: a.TaskID,
		Project: a.ProjectID, Repository: a.Repository, Scope: scope, Adapter: a.Agent, Model: a.Model, Role: a.Role,
		Prompt: a.Prompt, Resume: a.Resume, // the CLI run's prompt/context package (KIN-79) and session (KIN-34)
		TestCommand: a.TestCommand, DiffBase: a.DiffBase, DiffHead: a.DiffHead, // the review loop (KIN-35)
		BranchHead: a.BranchHead} // a CI fix round continues the published branch (KIN-106)
	if a.Agent == "" || a.Agent == workflowapp.AgentMock {
		body.Adapter, body.Model, body.Prompt, body.Resume, body.Scenario = protocol.AdapterMock, "", "", "", mockScenario(a.StepKey)
		body.TestCommand, body.DiffBase, body.DiffHead, body.BranchHead = "", "", "", ""
	}
	return body
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

// wireAttemptLog gives the task API (KIN-25) the attempts' recorded runner events.
func wireAttemptLog(wf *workflowapp.Service, inst *bootstrap.Installation) {
	wf.Log = workflowAttemptLog{log: runnersapp.AttemptLog{Repo: runnersdb.New(inst.DB)}}
}

var preparingPhases = []string{protocol.PreparingQueued, protocol.PreparingFetching, protocol.PreparingCheckout}

// workflowAttemptLog translates the runners context's recorded events into what the task UI shows. Bodies are
// the runner's (untrusted) data: they are decoded into typed fields and served as text, never interpreted.
type workflowAttemptLog struct{ log runnersapp.AttemptLog }

func (l workflowAttemptLog) AttemptEvents(ctx context.Context, stepID string, fence, after int64, limit int) ([]workflowapp.AttemptEvent, error) {
	evs, err := l.log.Events(ctx, stepID, fence, after, limit)
	if err != nil {
		return nil, err
	}
	out := make([]workflowapp.AttemptEvent, 0, len(evs))
	for _, e := range evs {
		v := workflowapp.AttemptEvent{Seq: e.AttemptSeq, Kind: e.Kind, At: e.RunnerAt}
		switch e.Kind {
		case protocol.EventOutput:
			var o protocol.Output
			if e.Body == nil || json.Unmarshal(e.Body, &o) != nil {
				v.Omitted, v.Reason = true, e.Reason
				break
			}
			v.Stream, v.Offset, v.Data = o.Stream, o.Offset, o.Data
		case protocol.EventOutputDropped:
			var d protocol.OutputDropped
			if e.Body == nil || json.Unmarshal(e.Body, &d) != nil {
				v.Omitted, v.Reason = true, e.Reason
				break
			}
			v.Bytes = d.Bytes
		case protocol.EventFailed:
			var f protocol.Failure
			_ = json.Unmarshal(e.Body, &f)
			v.Reason = f.Reason
		case protocol.EventWorkspacePreparing: // KIN-71: only a known phase is shown
			var p protocol.WorkspacePreparing
			if json.Unmarshal(e.Body, &p) == nil && slices.Contains(preparingPhases, p.Phase) {
				v.Reason = p.Phase
			}
		case protocol.EventAgent:
			var ag protocol.AgentEvent
			if e.Body == nil || json.Unmarshal(e.Body, &ag) != nil {
				v.Omitted, v.Reason = true, e.Reason
				break
			}
			v.Reason = agentSummary(ag)
		}
		out = append(out, v)
	}
	return out, nil
}

// AttemptWorkspace decodes the attempt's latest workspace reports (KIN-70) — the same recorded events the scope check
// reads. A missing or unreadable body is no report. Paths and hashes stay the runner's untrusted text.
func (l workflowAttemptLog) AttemptWorkspace(ctx context.Context, stepID string, fence int64) (workflowapp.AttemptWorkspace, error) {
	var out workflowapp.AttemptWorkspace
	var p protocol.WorkspacePrepared
	at, ok, err := l.latest(ctx, stepID, fence, protocol.EventWorkspacePrepared, &p)
	if err != nil {
		return out, err
	}
	if ok {
		v := &workflowapp.WorkspacePrepared{At: at, Repository: p.Repository, Branch: p.Branch, Base: p.Base, BaseSHA: p.BaseSHA,
			Instructions: make([]workflowapp.InstructionFile, 0, len(p.Instructions)), InstructionsTruncated: p.InstructionsTruncated}
		for _, f := range p.Instructions {
			v.Instructions = append(v.Instructions, workflowapp.InstructionFile(f))
		}
		out.Prepared = v
	}
	var c protocol.WorkspaceChanges
	at, ok, err = l.latest(ctx, stepID, fence, protocol.EventWorkspaceChanges, &c)
	if err != nil {
		return out, err
	}
	if ok {
		v := &workflowapp.WorkspaceChanges{At: at, Branch: c.Branch, BaseSHA: c.BaseSHA, HeadSHA: c.HeadSHA,
			Files: make([]workflowapp.ChangedFile, 0, len(c.Files)), FilesTotal: c.FilesTotal, Truncated: c.Truncated,
			InstructionsChanged: make([]workflowapp.InstructionChange, 0, len(c.InstructionsChanged))}
		for _, f := range c.Files {
			v.Files = append(v.Files, workflowapp.ChangedFile(f))
		}
		for _, f := range c.InstructionsChanged {
			v.InstructionsChanged = append(v.InstructionsChanged, workflowapp.InstructionChange(f))
		}
		out.Changes = v
	}
	return out, nil
}

// latest decodes the attempt's newest event of the kind into body; ok is false when there is none or it is unreadable.
func (l workflowAttemptLog) latest(ctx context.Context, stepID string, fence int64, kind string, body any) (time.Time, bool, error) {
	e, ok, err := l.log.Latest(ctx, stepID, fence, kind)
	if err != nil || !ok || e.Body == nil || json.Unmarshal(e.Body, body) != nil {
		return time.Time{}, false, err
	}
	return e.RunnerAt, true, nil
}

// maxAgentSummary bounds one CLI event's line in the attempt log (the raw output holds everything).
const maxAgentSummary = 500

// agentSummary is a CLI event (KIN-79) as one line of the attempt log: the runner's text, never interpreted.
func agentSummary(a protocol.AgentEvent) string {
	parts := []string{a.Kind}
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	switch a.Kind {
	case "session":
		add(a.Session)
		add(a.Model)
		if a.Version != "" {
			add("v" + a.Version)
		}
	case "tool_use":
		add(a.Tool)
		add(a.Input)
	case "failure":
		add(a.Code)
		add(a.Cause)
		add(a.Message)
	default:
		add(a.Text)
	}
	if u := a.Usage; u != nil {
		add(fmt.Sprintf("%d in / %d out tokens, %d turns, $%.4f", u.InputTokens, u.OutputTokens, u.Turns, u.CostUSD))
	}
	line := strings.Join(parts, " · ")
	line = strings.Join(strings.Fields(line), " ")
	if len(line) > maxAgentSummary {
		line = line[:maxAgentSummary]
		for len(line) > 0 && !utf8.ValidString(line) {
			line = line[:len(line)-1]
		}
		line += "…"
	}
	return line
}
