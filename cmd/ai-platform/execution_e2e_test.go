package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditdb "github.com/tr1gl1de/kindevai/internal/audit/adapters/sqlite"
	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/github"
	integrations "github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	projectsdb "github.com/tr1gl1de/kindevai/internal/projects/adapters/sqlite"
	projectsapp "github.com/tr1gl1de/kindevai/internal/projects/app"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/adapters/controlplane"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/adapters/probe"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/adapters/spool"
	connapp "github.com/tr1gl1de/kindevai/internal/runner/connection/app"
	conndomain "github.com/tr1gl1de/kindevai/internal/runner/connection/domain"
	"github.com/tr1gl1de/kindevai/internal/runner/session/adapters/journal"
	"github.com/tr1gl1de/kindevai/internal/runner/session/adapters/mockagent"
	"github.com/tr1gl1de/kindevai/internal/runner/session/adapters/process"
	"github.com/tr1gl1de/kindevai/internal/runner/session/adapters/spoolevents"
	sessionapp "github.com/tr1gl1de/kindevai/internal/runner/session/app"
	sessiondomain "github.com/tr1gl1de/kindevai/internal/runner/session/domain"
	runnershttp "github.com/tr1gl1de/kindevai/internal/runners/adapters/httpapi"
	runnersdb "github.com/tr1gl1de/kindevai/internal/runners/adapters/sqlite"
	runnersapp "github.com/tr1gl1de/kindevai/internal/runners/app"
	runners "github.com/tr1gl1de/kindevai/internal/runners/domain"
	"github.com/tr1gl1de/kindevai/internal/server"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
	workflow "github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// The test binary doubles as the mock agent child process (`ai-runner mock-agent`).
func TestMain(m *testing.M) {
	if os.Getenv("KINDEV_TEST_MOCK_AGENT") == "1" {
		os.Exit(mockagent.Main(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// End-to-end mock execution (KIN-23): a real SQLite Control Plane wired like `serve` (workflow + approvals +
// runners gateway and channel + scheduler) and a runner built from the real ai-runner packages (connector, spool,
// executor, process launcher with the mock agent as a child process). Timings are shortened: leases of
// e2eLeaseTTL, heartbeats every e2eHeartbeat.
const (
	e2eLeaseTTL  = 1500 * time.Millisecond
	e2eHeartbeat = 100 * time.Millisecond
	e2eSchedule  = 30 * time.Millisecond
)

// e2eSlots bounds how many of these tests run at once: each mock agent re-executes the race-instrumented test
// binary, and running them all together starves the package's other start-up-time-bounded tests.
var e2eSlots = make(chan struct{}, 2)

func e2eSlot(t *testing.T) {
	t.Helper()
	select {
	case e2eSlots <- struct{}{}:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	t.Cleanup(func() { <-e2eSlots })
}

var ownerP = auth.Principal{UserID: "owner1", Login: "owner", Owner: true}

type controlPlane struct {
	dataDir string
	addr    string
	inst    *bootstrap.Installation
	wf      *workflowapp.Service
	hub     *runnersapp.Hub
	stop    func()
}

// startCP runs the Control Plane on addr ("127.0.0.1:0" for a new one) over dataDir.
func startCP(t *testing.T, dataDir, addr string) *controlPlane {
	t.Helper()
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	logger := slog.New(slog.DiscardHandler)
	inst, err := bootstrap.Run(ctx, bootstrap.Options{DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	audit := newAudit(inst, logger)
	msg := newMessaging(inst, logger)
	hub := runnersapp.NewHub()
	tel := newTelemetry(inst, msg, logger)
	gh, _ := integrations.NewBaseURL(github.DefaultBaseURL)
	integ := newIntegrations(inst, msg, audit, gh, logger)
	apv := newApprovals(inst, msg, audit, integ, logger)
	apv.Workflow.LeaseTTL = e2eLeaseTTL
	gateway := newRunnerGateway(inst, hub, tel, nil, nil, logger)
	channel := newRunnerChannel(inst, apv.Workflow, runners.DefaultRetention(), logger)
	channel.PollInterval = 20 * time.Millisecond
	attachRunnerChannel(gateway, channel)
	wireExecution(apv.Workflow, channel, gateway, tel, hub, logger)
	mux := http.NewServeMux()
	gateway.Register(mux)
	var ln net.Listener
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ln, err = (&net.ListenConfig{}).Listen(ctx, "tcp", addr); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond) // the previous Control Plane's port is being released
	}
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	srv.RegisterOnShutdown(hub.Shutdown)
	go func() { _ = srv.Serve(ln) }()
	stopScheduler := startScheduler(ctx, apv.Workflow, e2eSchedule, logger)
	cp := &controlPlane{dataDir: dataDir, addr: ln.Addr().String(), inst: inst, wf: apv.Workflow, hub: hub}
	var once sync.Once
	cp.stop = func() {
		once.Do(func() {
			stopScheduler()
			sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer scancel()
			_ = srv.Shutdown(sctx)
			_ = gateway.Wait(sctx)
			cancel()
			_ = inst.Close()
		})
	}
	t.Cleanup(cp.stop)
	return cp
}

// seed creates the Owner, projects p1 and p2 with one repository each and an enrolled runner approved for
// projects; it returns the runner id and key.
func (cp *controlPlane) seed(t *testing.T, projects ...string) (string, ed25519.PrivateKey) {
	t.Helper()
	ctx := t.Context()
	for _, q := range []string{
		"INSERT INTO users(id, login, password_hash, platform_role, created_at) VALUES ('owner1', 'owner', 'h', 'owner', '2026-11-25T00:00:00Z')",
		"INSERT INTO projects(id, slug, name, created_at) VALUES ('p1', 'alpha', 'Alpha', '2026-11-25T00:00:00Z')",
		"INSERT INTO projects(id, slug, name, created_at) VALUES ('p2', 'beta', 'Beta', '2026-11-25T00:00:00Z')",
		"INSERT INTO project_repositories(id, project_id, owner, name, integration_branch, created_at, updated_at) VALUES ('r1', 'p1', 'acme', 'app', 'dev', '2026-11-25T00:00:00Z', '2026-11-25T00:00:00Z')",
		"INSERT INTO project_repositories(id, project_id, owner, name, integration_branch, created_at, updated_at) VALUES ('r2', 'p2', 'acme', 'other', 'dev', '2026-11-25T00:00:00Z', '2026-11-25T00:00:00Z')",
	} {
		if _, err := cp.inst.DB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	svc := cp.runners()
	_, tok, err := svc.CreateToken(ctx, ownerP)
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	res, err := svc.Enroll(ctx, runnersapp.EnrollRequest{Token: tok, PublicKey: base64.StdEncoding.EncodeToString(pub),
		Hostname: "box", OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) > 0 {
		if _, err := svc.Approve(ctx, ownerP, res.Runner.ID, projects); err != nil {
			t.Fatal(err)
		}
	}
	return res.Runner.ID, priv
}

// runners is the runners management service wired like `serve` (disable/revoke close the session and end the
// runner's attempts, KIN-63).
func (cp *controlPlane) runners() *runnersapp.Service {
	svc := &runnersapp.Service{Repo: runnersdb.New(cp.inst.DB),
		Projects: runnerProjects{&projectsapp.Service{Repo: projectsdb.New(cp.inst.DB)}}, Tx: db.Transactor{DB: cp.inst.DB},
		Audit: &auditapp.Service{Repo: auditdb.New(cp.inst.DB), Now: time.Now}, Rand: rand.Reader, Now: time.Now, NewID: rand.Text,
		Sessions: cp.hub}
	h := &runnershttp.Handler{Service: svc}
	wireRunnerRelease([]server.Module{h}, cp.wf)
	return svc
}

// countingLauncher counts process launches (a second process for one attempt is the bug KIN-23 rules out).
type countingLauncher struct {
	sessionapp.Launcher
	n atomic.Int64
}

func (c *countingLauncher) Launch(ctx context.Context, s sessionapp.LaunchSpec) (sessionapp.Process, error) {
	c.n.Add(1)
	return c.Launcher.Launch(ctx, s)
}

type runner struct {
	dir      string
	id       string
	key      ed25519.PrivateKey
	launches *countingLauncher
	x        *sessionapp.Executor
	spool    *spool.Spool
	cancel   context.CancelFunc
	done     chan struct{}
	err      error // what the connector returned, once done is closed
}

// startRunner starts the runner daemon's parts over dir (a restart reuses dir): the executor recovers its journal,
// then the connector keeps the link to the Control Plane at addr.
func startRunner(t *testing.T, dir, addr, id string, key ed25519.PrivateKey, allow []string, launches *countingLauncher) *runner {
	t.Helper()
	sp, err := spool.Open(spool.Config{Dir: filepath.Join(dir, "events")})
	if err != nil {
		t.Fatal(err)
	}
	ch := &connapp.Channel{Spool: sp, Cursor: spool.CursorFile{Path: filepath.Join(dir, "events", "commands.json")},
		AckTimeout: time.Second}
	if launches == nil {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		launches = &countingLauncher{Launcher: &process.Launcher{Program: exe, Env: []string{"KINDEV_TEST_MOCK_AGENT=1"},
			Poll: 20 * time.Millisecond}}
	}
	x := &sessionapp.Executor{Launcher: launches, Journal: journal.Dir{Path: filepath.Join(dir, "attempts")},
		Events: spoolevents.Events{Channel: ch}, Policy: sessiondomain.Policy{Projects: allow}, Grace: 300 * time.Millisecond}
	ch.Commands, ch.Processes = x.Command, x
	if err := x.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	server, err := conndomain.ParseServerURL("http://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	ident, err := conndomain.NewIdentity(key)
	if err != nil {
		t.Fatal(err)
	}
	conn := &connapp.Connector{Dialer: controlplane.New(), MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond,
		Handlers: []connapp.LinkHandler{ch, &connapp.Reporter{Probe: probe.New(dir), HeartbeatEvery: e2eHeartbeat}},
		Halt:     x.StopAll}
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	r := &runner{dir: dir, id: id, key: key, launches: launches, x: x, spool: sp, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		r.err = conn.Run(ctx, conndomain.Enrollment{Server: server, RunnerID: id}, ident)
	}()
	t.Cleanup(func() { r.disconnect(); x.KillAll(); _ = sp.Close() })
	return r
}

// disconnect drops the link; the executor and its processes keep running.
func (r *runner) disconnect() { r.cancel(); <-r.done }

// restart stops the runner process (its attempts keep running) and starts a new one over the same data dir.
func (r *runner) restart(t *testing.T, addr string) *runner {
	t.Helper()
	r.disconnect()
	r.x.Close()
	_ = r.spool.Close()
	return startRunner(t, r.dir, addr, r.id, r.key, nil, r.launches)
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for {
		ok, state := cond()
		if ok {
			return
		}
		last = state
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; last state: %s", what, last)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (cp *controlPlane) detail(t *testing.T, taskID string) workflowapp.TaskDetail {
	t.Helper()
	d, err := cp.wf.TaskDetail(t.Context(), ownerP, taskID)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func describe(d workflowapp.TaskDetail) string {
	s := string(d.Task.State) + " " + d.Task.Reason.Detail
	if d.Execution != nil {
		s += " exec=" + string(d.Execution.State)
		for _, st := range d.Execution.Steps {
			s += " step=" + string(st.State) + "/" + string(st.WaitReason)
			for _, a := range st.Attempts {
				s += " [" + string(a.State) + " " + a.Error + "]"
			}
		}
	}
	return s
}

func (cp *controlPlane) waitTask(t *testing.T, taskID string, timeout time.Duration, ok func(workflowapp.TaskDetail) bool) workflowapp.TaskDetail {
	t.Helper()
	var d workflowapp.TaskDetail
	waitFor(t, "task "+taskID, timeout, func() (bool, string) {
		d = cp.detail(t, taskID)
		return ok(d), describe(d)
	})
	return d
}

func (cp *controlPlane) mockTask(t *testing.T, project, scenario string) string {
	t.Helper()
	task, err := cp.wf.CreateTaskAs(t.Context(), ownerP, project, "Mock "+scenario)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cp.wf.RunMock(t.Context(), ownerP, task.ID, scenario); err != nil {
		t.Fatal(err)
	}
	return task.ID
}

func stepRunning(d workflowapp.TaskDetail) bool {
	return d.Execution != nil && d.Execution.Steps[0].State == workflow.StepRunning
}

// attemptDir is the working directory of the step's current attempt on the runner.
func attemptDir(r *runner, d workflowapp.TaskDetail) string {
	st := d.Execution.Steps[0]
	return filepath.Join(r.dir, "attempts", st.ID+"-"+strconv.FormatInt(st.Fence, 10))
}

func release(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, mockagent.ReleaseFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func succeeded(d workflowapp.TaskDetail) bool {
	return d.Task.State == workflow.TaskVerifying && d.Execution.State == workflow.ExecutionSucceeded
}

// task → mock plan (L1, READY without an approval) → scheduled to the approved runner → the mock agent runs as a
// child process → events and output reach the Control Plane → the task is VERIFYING. A failure is retried once
// with a new token; a second failure fails the task.
func TestMockExecutionEndToEnd(t *testing.T) {
	t.Parallel()
	e2eSlot(t)
	cp := startCP(t, t.TempDir(), "127.0.0.1:0")
	id, key := cp.seed(t, "p1")
	r := startRunner(t, t.TempDir(), cp.addr, id, key, nil, nil)

	ok := cp.mockTask(t, "p1", "success")
	d := cp.waitTask(t, ok, 30*time.Second, succeeded)
	if a := d.Execution.Steps[0].Attempts; len(a) != 1 || a[0].RunnerID != id || a[0].State != workflow.AttemptSucceeded {
		t.Fatalf("attempts %+v", a)
	}
	var output string
	waitFor(t, "output stored", 5*time.Second, func() (bool, string) {
		_ = cp.inst.DB.QueryRowContext(t.Context(), `SELECT COALESCE(group_concat(body, ''), '') FROM runner_attempt_events
			WHERE step_id = ? AND kind = 'execution.output'`, d.Execution.Steps[0].ID).Scan(&output)
		return strings.Contains(output, "mock agent: done"), output
	})

	flaky := cp.mockTask(t, "p1", "flaky")
	d = cp.waitTask(t, flaky, 30*time.Second, succeeded)
	if a := d.Execution.Steps[0].Attempts; len(a) != 2 || a[0].State != workflow.AttemptFailed ||
		!strings.Contains(a[0].Error, "exit status 1") || a[1].Fence != a[0].Fence+1 {
		t.Fatalf("flaky attempts %+v", a)
	}

	crash := cp.mockTask(t, "p1", "crash")
	d = cp.waitTask(t, crash, 30*time.Second, func(d workflowapp.TaskDetail) bool { return d.Task.State == workflow.TaskFailed })
	if a := d.Execution.Steps[0].Attempts; len(a) != 2 || !strings.Contains(a[1].Error, "signal") {
		t.Fatalf("crash attempts %+v", a)
	}
	if n := r.launches.n.Load(); n != 1+2+2 {
		t.Fatalf("%d processes launched", n)
	}
}

// Scheduling scope: a runner approved for another project gets nothing and the task is BLOCKED waiting for a
// runner; a PENDING runner gets nothing either. A runner approved for the project but whose own allowlist excludes
// it refuses the assignment itself (no process is started).
func TestSchedulingScopeAndRunnerSideCheck(t *testing.T) {
	t.Parallel()
	e2eSlot(t)
	cp := startCP(t, t.TempDir(), "127.0.0.1:0")
	other, otherKey := cp.seed(t, "p2")
	startRunner(t, t.TempDir(), cp.addr, other, otherKey, nil, nil)
	task := cp.mockTask(t, "p1", "success")
	d := cp.waitTask(t, task, 30*time.Second, func(d workflowapp.TaskDetail) bool {
		return d.Task.State == workflow.TaskBlocked && d.Task.BlockReason == workflow.WaitRunner
	})
	if d.Task.Reason.Detail != workflowapp.DetailNoRunner {
		t.Fatalf("blocked %+v", d.Task)
	}
	if _, err := cp.wf.CancelTaskAs(t.Context(), ownerP, task, "test over"); err != nil {
		t.Fatal(err)
	}

	cp2 := startCP(t, t.TempDir(), "127.0.0.1:0")
	id, key := cp2.seed(t, "p1")
	r := startRunner(t, t.TempDir(), cp2.addr, id, key, []string{"p2"}, nil) // the runner itself allows only p2
	task = cp2.mockTask(t, "p1", "success")
	d = cp2.waitTask(t, task, 30*time.Second, func(d workflowapp.TaskDetail) bool { return d.Task.State == workflow.TaskFailed })
	if a := d.Execution.Steps[0].Attempts; len(a) != 2 || !strings.Contains(a[0].Error, "not allowed on this runner") {
		t.Fatalf("attempts %+v", a)
	}
	if n := r.launches.n.Load(); n != 0 {
		t.Fatalf("%d processes launched for a refused project", n)
	}

	cp3 := startCP(t, t.TempDir(), "127.0.0.1:0")
	pending, pendingKey := cp3.seed(t) // enrolled, never approved: it cannot even connect
	startRunner(t, t.TempDir(), cp3.addr, pending, pendingKey, nil, nil)
	task = cp3.mockTask(t, "p1", "success")
	cp3.waitTask(t, task, 30*time.Second, func(d workflowapp.TaskDetail) bool { return d.Task.State == workflow.TaskBlocked })
}

// Cancelling the task stops the attempt's whole process group on the runner.
func TestCancelKillsTheProcessGroup(t *testing.T) {
	t.Parallel()
	e2eSlot(t)
	cp := startCP(t, t.TempDir(), "127.0.0.1:0")
	id, key := cp.seed(t, "p1")
	r := startRunner(t, t.TempDir(), cp.addr, id, key, nil, nil)
	task := cp.mockTask(t, "p1", "hang")
	cp.waitTask(t, task, 30*time.Second, stepRunning)
	var pid int
	waitFor(t, "process reported", 5*time.Second, func() (bool, string) {
		if a := r.x.Attempts(); len(a) == 1 {
			pid = a[0].PGID
		}
		return pid > 0, ""
	})
	if !process.Alive(pid, pid) {
		t.Fatal("process not alive")
	}
	if _, err := cp.wf.CancelTaskAs(t.Context(), ownerP, task, "stop it"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "process group gone", 30*time.Second, func() (bool, string) { return !process.Alive(pid, pid), "" })
	if d := cp.detail(t, task); d.Task.State != workflow.TaskCancelled || d.Execution.State != workflow.ExecutionCancelled {
		t.Fatalf("after cancel: %s", describe(d))
	}
}

// Losing the runner: its lease runs out, the attempt is UNKNOWN and the task BLOCKED; nothing is started again
// until the runner reconciles. It comes back with the process alive: the attempt continues under the same token
// and completes. A runner restart adopts the live process from its journal (no second process).
func TestLostRunnerReconcilesWithoutASecondProcess(t *testing.T) {
	t.Parallel()
	e2eSlot(t)
	cp := startCP(t, t.TempDir(), "127.0.0.1:0")
	id, key := cp.seed(t, "p1")
	r := startRunner(t, t.TempDir(), cp.addr, id, key, nil, nil)
	task := cp.mockTask(t, "p1", "gate")
	d := cp.waitTask(t, task, 30*time.Second, stepRunning)
	fence := d.Execution.Steps[0].Fence

	r.disconnect()
	d = cp.waitTask(t, task, 30*time.Second, func(d workflowapp.TaskDetail) bool {
		return d.Task.State == workflow.TaskBlocked && d.Execution.Steps[0].AttemptUnknown()
	})
	if d.Task.Reason.Detail != workflowapp.DetailRunnerLost {
		t.Fatalf("blocked: %s", describe(d))
	}
	time.Sleep(3 * e2eSchedule) // several scheduler passes: still nothing assigned
	if n := r.launches.n.Load(); n != 1 {
		t.Fatalf("%d processes launched while the attempt was unknown", n)
	}

	r = r.restart(t, cp.addr) // the runner process restarts and adopts the running agent
	d = cp.waitTask(t, task, 30*time.Second, func(d workflowapp.TaskDetail) bool {
		return d.Task.State == workflow.TaskRunning && stepRunning(d)
	})
	if st := d.Execution.Steps[0]; st.Fence != fence || len(st.Attempts) != 1 {
		t.Fatalf("after reconcile: %+v", st)
	}
	release(t, attemptDir(r, d))
	cp.waitTask(t, task, 30*time.Second, succeeded)
	if n := r.launches.n.Load(); n != 1 {
		t.Fatalf("%d processes launched in total", n)
	}
}

// A process that died while the runner was away is reconciled as gone: the attempt fails and is retried once.
func TestDeadProcessIsRetriedAfterReconcile(t *testing.T) {
	t.Parallel()
	e2eSlot(t)
	cp := startCP(t, t.TempDir(), "127.0.0.1:0")
	id, key := cp.seed(t, "p1")
	r := startRunner(t, t.TempDir(), cp.addr, id, key, nil, nil)
	task := cp.mockTask(t, "p1", "hang")
	cp.waitTask(t, task, 30*time.Second, stepRunning)
	r.disconnect()
	r.x.Close()
	r.x.KillAll() // the machine lost the process
	_ = r.spool.Close()
	cp.waitTask(t, task, 30*time.Second, func(d workflowapp.TaskDetail) bool { return d.Execution.Steps[0].AttemptUnknown() })
	_ = os.RemoveAll(filepath.Join(r.dir, "attempts")) // and its journal (no result to report)
	startRunner(t, r.dir, cp.addr, id, key, nil, r.launches)
	d := cp.waitTask(t, task, 30*time.Second, func(d workflowapp.TaskDetail) bool {
		return len(d.Execution.Steps[0].Attempts) == 2 && stepRunning(d)
	})
	if a := d.Execution.Steps[0].Attempts[0]; a.State != workflow.AttemptFailed || !strings.Contains(a.Error, "process lost") {
		t.Fatalf("first attempt %+v", a)
	}
	if _, err := cp.wf.CancelTaskAs(t.Context(), ownerP, task, "done"); err != nil {
		t.Fatal(err)
	}
}

// A Control Plane restart while the step runs: everything is in SQLite, the runner reconnects, reconciles and the
// execution continues to the end.
func TestControlPlaneRestartContinuesTheExecution(t *testing.T) {
	t.Parallel()
	e2eSlot(t)
	dataDir := t.TempDir()
	cp := startCP(t, dataDir, "127.0.0.1:0")
	id, key := cp.seed(t, "p1")
	r := startRunner(t, t.TempDir(), cp.addr, id, key, nil, nil)
	task := cp.mockTask(t, "p1", "gate")
	d := cp.waitTask(t, task, 30*time.Second, stepRunning)
	dir := attemptDir(r, d)
	addr := cp.addr

	cp.stop()
	time.Sleep(2 * e2eLeaseTTL) // down longer than a lease
	cp = startCP(t, dataDir, addr)
	cp.waitTask(t, task, 30*time.Second, func(d workflowapp.TaskDetail) bool {
		return d.Task.State == workflow.TaskRunning && stepRunning(d)
	})
	release(t, dir)
	d = cp.waitTask(t, task, 30*time.Second, succeeded)
	if a := d.Execution.Steps[0].Attempts; len(a) != 1 {
		t.Fatalf("attempts %+v", a)
	}
	if n := r.launches.n.Load(); n != 1 {
		t.Fatalf("%d processes launched", n)
	}
}

// runningPGID waits until the runner reports the attempt's process and returns its process group.
func runningPGID(t *testing.T, r *runner) int {
	t.Helper()
	var pgid int
	waitFor(t, "process reported", 5*time.Second, func() (bool, string) {
		if a := r.x.Attempts(); len(a) == 1 {
			pgid = a[0].PGID
		}
		return pgid > 0, ""
	})
	return pgid
}

// KIN-63: disabling the runner fails its running attempt in the same transaction (retry budget permitting) and
// the runner, told 4403, stops the process group itself; once enabled again, the step runs again under a new token.
func TestDisabledRunnerStopsItsAttemptAndTheStepIsRetried(t *testing.T) {
	t.Parallel()
	e2eSlot(t)
	cp := startCP(t, t.TempDir(), "127.0.0.1:0")
	id, key := cp.seed(t, "p1")
	r := startRunner(t, t.TempDir(), cp.addr, id, key, nil, nil)
	task := cp.mockTask(t, "p1", "hang")
	first := cp.waitTask(t, task, 30*time.Second, stepRunning).Execution.Steps[0]
	pgid := runningPGID(t, r)
	svc := cp.runners()
	if _, err := svc.Disable(t.Context(), ownerP, id); err != nil {
		t.Fatal(err)
	}
	d := cp.detail(t, task) // decided in the disable's transaction, not after a lease runs out
	if st := d.Execution.Steps[0]; st.State == workflow.StepRunning || st.Attempts[0].State != workflow.AttemptFailed ||
		!strings.Contains(st.Attempts[0].Error, "runner disabled") {
		t.Fatalf("after disable: %s", describe(d))
	}
	waitFor(t, "process group gone", 30*time.Second, func() (bool, string) { return !process.Alive(pgid, pgid), "" })
	if _, err := svc.Enable(t.Context(), ownerP, id); err != nil {
		t.Fatal(err)
	}
	d = cp.waitTask(t, task, 30*time.Second, func(d workflowapp.TaskDetail) bool {
		return stepRunning(d) && d.Execution.Steps[0].Fence > first.Fence
	})
	if n := r.launches.n.Load(); n != 2 {
		t.Fatalf("launches = %d, want 2 (%s)", n, describe(d))
	}
}

// KIN-63: a revoked runner stops its attempt itself before the connector gives up for good (it never receives the
// execution.cancel queued for it); the Control Plane already failed the attempt.
func TestRevokedRunnerStopsItsAttemptBeforeExiting(t *testing.T) {
	t.Parallel()
	e2eSlot(t)
	cp := startCP(t, t.TempDir(), "127.0.0.1:0")
	id, key := cp.seed(t, "p1")
	r := startRunner(t, t.TempDir(), cp.addr, id, key, nil, nil)
	task := cp.mockTask(t, "p1", "hang")
	cp.waitTask(t, task, 30*time.Second, stepRunning)
	pgid := runningPGID(t, r)
	if _, err := cp.runners().Revoke(t.Context(), ownerP, id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.done:
	case <-time.After(30 * time.Second):
		t.Fatal("the revoked runner kept running")
	}
	if !errors.Is(r.err, conndomain.ErrRunnerRevoked) {
		t.Fatalf("runner stopped with %v", r.err)
	}
	if process.Alive(pgid, pgid) {
		t.Fatal("the revoked runner left its attempt running")
	}
	if st := cp.detail(t, task).Execution.Steps[0]; st.Attempts[0].State != workflow.AttemptFailed ||
		!strings.Contains(st.Attempts[0].Error, "runner revoked") {
		t.Fatalf("attempt after revoke: %+v", st.Attempts[0])
	}
}
