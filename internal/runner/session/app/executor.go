// Package app holds the runner's session use cases (KIN-23): it applies execution.assign and execution.cancel
// commands by running each attempt as a child process in its own process group, streams the attempt's events to
// the Control Plane and keeps an on-disk journal of running attempts, so after a restart it adopts a process that
// is still alive (or reports its result) instead of launching a second one.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/tr1gl1de/kindevai/internal/platform/redact"
	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// LaunchSpec is one attempt to start.
type LaunchSpec struct {
	Dir      string // the attempt's state directory (the mock agent's result file)
	Workdir  string // the process's working directory (the git worktree, KIN-29); Dir when empty
	Scenario string
	Attempt  int64 // the fencing token: distinct for every attempt of a step
	Stdout   io.Writer
	Stderr   io.Writer
	// Sandbox is set when the attempt's network profile is strict (KIN-31): the process runs only through it.
	Sandbox Sandbox
	// Agent is the AI CLI to run instead of the mock agent (KIN-79).
	Agent *AgentCommand
}

// Process is a running attempt process (the leader of its own process group).
type Process interface {
	PID() int
	PGID() int
	// StartTime identifies the process together with PID (the kernel's start time; "" when unknown), so a reused
	// pid is never taken for it after a restart (KIN-64).
	StartTime() string
	// Wait blocks until the process ended.
	Wait() domain.Exit
	// Signal sends SIGTERM (kill=false) or SIGKILL (kill=true) to the whole process group.
	Signal(kill bool) error
}

// Launcher starts attempt processes and finds them again after a restart.
type Launcher interface {
	Launch(ctx context.Context, spec LaunchSpec) (Process, error)
	// Adopt returns the process of a journal record; its Wait returns at once when the process is gone (with the
	// result the process left in dir, or Lost).
	Adopt(r domain.Record, dir string) Process
}

// Journal is the on-disk record of running attempts.
type Journal interface {
	// Prepare creates the attempt's working directory (if missing) and returns it.
	Prepare(step string, fence int64) (string, error)
	Save(r domain.Record) error
	// Delete removes the record and the working directory (an attempt that may be assigned again).
	Delete(step string, fence int64) error
	// Finish removes the record and the working directory and leaves a tombstone, so a re-delivered assignment of
	// a reported attempt is never run again (tombstones expire after the command log's retention).
	Finish(step string, fence int64) error
	Finished(step string, fence int64) (bool, error)
	List() ([]domain.Record, error)
}

// Stream is one attempt's event stream (connection.Attempt).
type Stream interface {
	Emit(kind string, body any) error
	Output(stream string) io.Writer
}

// Events opens attempt event streams on the runner's spool. reserve is nil for a stream that need not survive a
// restart.
type Events interface {
	Attempt(step string, fence, after int64, reserve func(upTo int64) error) Stream
}

// WorkspaceRequest asks for an attempt's git worktree (workspace context, KIN-29).
type WorkspaceRequest struct {
	Step       string
	Fence      int64
	Task       string
	Repository string
	Base       string
	Token      domain.Secret
	// ReadOnly: a reviewer's workspace, whose end commits nothing (KIN-35).
	ReadOnly bool
	// Progress gets the preparation's phases (protocol.Preparing*) as it reaches them (KIN-71).
	Progress func(phase string)
}

// Workspaces prepares and finishes attempt worktrees (the workspace context's Manager).
type Workspaces interface {
	// Prepare returns the worktree directory and its report (workspace.prepared).
	Prepare(ctx context.Context, r WorkspaceRequest) (string, protocol.WorkspacePrepared, error)
	// Finish commits leftover work to the attempt's branch, removes the worktree and returns what changed
	// (workspace.changes); nil for an attempt without a workspace.
	Finish(ctx context.Context, step string, fence int64) (*protocol.WorkspaceChanges, error)
	// Changes lists what the running attempt changed so far without committing (an Interim workspace.changes,
	// KIN-51); nil for an attempt without a workspace held by this process.
	Changes(ctx context.Context, step string, fence int64) (*protocol.WorkspaceChanges, error)
	// Diff is the attempt's worktree diff from..to (commits), at most limit bytes (cut reports a longer one; KIN-35).
	Diff(ctx context.Context, step string, fence int64, from, to string, limit int) (diff string, cut bool, err error)
}

// DefaultPrepareTimeout bounds an attempt's whole workspace preparation (KIN-71; each git command has its own
// bound too); DefaultPrepareKeepalive is how often a preparation in progress is reported (the Control Plane extends
// the claim's lease with it, so it stays well below the lease, 60 s).
const (
	DefaultPrepareTimeout   = 30 * time.Minute
	DefaultPrepareKeepalive = 20 * time.Second
)

// DefaultGrace is how long a cancelled process group has between SIGTERM and SIGKILL.
const DefaultGrace = 5 * time.Second

// Executor runs assignments (session context).
type Executor struct {
	Launcher Launcher
	Journal  Journal
	Events   Events
	Policy   domain.Policy
	// Workspaces prepares the git worktree of an assignment with a Base (KIN-29); without it such an assignment
	// fails.
	Workspaces Workspaces
	// Egress confines attempts whose network profile is strict (KIN-31); nil: no attempt is confined.
	Egress Confiner
	// Agents runs the AI CLIs (KIN-79); without it a CLI assignment fails.
	Agents Agents
	Grace  time.Duration // DefaultGrace when zero
	// ChangesEvery is how often a running attempt's worktree changes are reported (KIN-51; only when they differ
	// from the last report); zero reports them only at the attempt's end.
	ChangesEvery time.Duration
	// TestsTimeout bounds the review loop's test run after an implementer's CLI (KIN-35; 0: agent-exec's default).
	TestsTimeout time.Duration
	Logger       *slog.Logger
	// KnownSecrets returns secret values the runner holds besides the assignment's git token (proxy passwords,
	// KIN-77); they are masked in the attempt's output and failure reasons by exact match.
	KnownSecrets func() []string
	// PrepareTimeout bounds a workspace preparation (DefaultPrepareTimeout when zero); PrepareKeepalive is how
	// often one in progress is reported (DefaultPrepareKeepalive when zero).
	PrepareTimeout   time.Duration
	PrepareKeepalive time.Duration

	mu       sync.Mutex
	runs     map[key]*run
	closed   atomic.Bool
	wg       sync.WaitGroup
	lifeOnce sync.Once
	life     context.Context // ends with Close: background work of this runner process (workspace preparations)
	stopLife context.CancelFunc
}

type key struct {
	step  string
	fence int64
}

type run struct {
	recMu     sync.Mutex // guards rec: the reserver saves it from the output writers' goroutines
	rec       domain.Record
	proc      Process
	stream    Stream
	output    []*redact.Stream // stdout, stderr: flushed before the terminal event
	known     []string         // secret values masked in the attempt's output and failure reasons (KIN-77)
	cancelled string
	// stopPrep stops the attempt's workspace preparation (KIN-71); set while it is prepared in the background.
	stopPrep context.CancelFunc
	sandbox  Sandbox       // released once the process ended
	follower AgentFollower // the AI CLI's output (KIN-79); nil for the mock agent
	agent    string
	done     chan struct{}
	// result is the terminal event kind once the process ended but reporting it failed; the run stays listed
	// (with the result) and the report is retried.
	result string
}

func (r *run) save(j Journal, fn func(rec *domain.Record)) error {
	r.recMu.Lock()
	defer r.recMu.Unlock()
	rec := r.rec
	fn(&rec)
	if err := j.Save(rec); err != nil {
		return err
	}
	r.rec = rec
	return nil
}

func (r *run) key() key {
	r.recMu.Lock()
	defer r.recMu.Unlock()
	return key{r.rec.Step, r.rec.Fence}
}

func (x *Executor) logger() *slog.Logger {
	if x.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return x.Logger
}

func (x *Executor) grace() time.Duration {
	if x.Grace > 0 {
		return x.Grace
	}
	return DefaultGrace
}

func (x *Executor) lifetime() context.Context {
	x.lifeOnce.Do(func() { x.life, x.stopLife = context.WithCancel(context.Background()) })
	return x.life
}

func (x *Executor) init() {
	if x.runs == nil {
		x.runs = map[key]*run{}
	}
}

// Command applies execution.assign and execution.cancel; other kinds are logged and acknowledged. It returns an
// error only when the command could not be applied and should be re-sent (e.g. the disk is full).
func (x *Executor) Command(ctx context.Context, cmd protocol.Command) error {
	switch cmd.Kind {
	case protocol.CommandAssign:
		var a protocol.Assign
		if err := json.Unmarshal(cmd.Body, &a); err != nil {
			x.logger().Warn("malformed assignment ignored", "id", cmd.ID, "err", err)
			return nil
		}
		as := domain.Assignment{Step: a.Step, Fence: a.Fence, Execution: a.Execution, Task: a.Task,
			Project: a.Project, Repository: a.Repository, Scope: a.Scope, Adapter: a.Adapter, Scenario: a.Scenario, Base: a.Base,
			Model: a.Model, Role: a.Role, Prompt: a.Prompt, Resume: a.Resume, BypassPermissions: a.BypassPermissions,
			TestCommand: a.TestCommand, DiffBase: a.DiffBase, DiffHead: a.DiffHead}
		if a.TimeoutSec != 0 {
			as.Timeout = time.Duration(min(max(a.TimeoutSec, -1), int64(domain.MaxAgentTimeout/time.Second)+1)) * time.Second
		}
		if a.Credential != nil {
			as.Token = domain.NewSecret(a.Credential.Token)
		}
		return x.Assign(ctx, as)
	case protocol.CommandCancel:
		var c protocol.Cancel
		if err := json.Unmarshal(cmd.Body, &c); err != nil {
			x.logger().Warn("malformed cancel ignored", "id", cmd.ID, "err", err)
			return nil
		}
		x.Cancel(c.Step, c.Fence, c.Reason)
		return nil
	}
	x.logger().Warn("command kind not implemented by this runner; acknowledged", "id", cmd.ID, "kind", cmd.Kind)
	return nil
}

func (x *Executor) known(k key) (bool, error) {
	if _, ok := x.runs[k]; ok {
		return true, nil
	}
	if done, err := x.Journal.Finished(k.step, k.fence); err != nil || done {
		return done, err
	}
	recs, err := x.Journal.List()
	if err != nil {
		return false, err
	}
	for _, r := range recs {
		if r.Step == k.step && r.Fence == k.fence {
			return true, nil
		}
	}
	return false, nil
}

// Assign runs the attempt unless it is already running or known (an assignment is applied at most once: a
// repeated command never starts a second process). An assignment the runner's policy refuses fails at once. An
// older attempt of the same step still running here is stopped (and waited for) before the new one starts.
func (x *Executor) Assign(ctx context.Context, a domain.Assignment) error {
	x.mu.Lock()
	x.init()
	var older []*run
	for k, r := range x.runs {
		if k.step == a.Step && k.fence < a.Fence {
			older = append(older, r)
		}
	}
	x.mu.Unlock()
	if len(older) > 0 && domain.CheckAssignment(a, x.Policy) == nil {
		for _, r := range older {
			x.mu.Lock()
			x.terminate(r, protocol.CancelStaleFence)
			x.mu.Unlock()
			select { // terminate sends SIGKILL after the grace period
			case <-r.done:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.closed.Load() {
		return errors.New("executor is shut down")
	}
	k := key{a.Step, a.Fence}
	if known, err := x.known(k); err != nil || known {
		return err
	}
	if err := domain.CheckAssignment(a, x.Policy); err != nil {
		x.logger().Warn("assignment refused", "step", a.Step, "fence", a.Fence, "project", a.Project, "err", err)
		if !idOK(a.Step) || a.Fence < 1 {
			return nil // nothing the Control Plane could match the failure to
		}
		return x.Events.Attempt(a.Step, a.Fence, 0, nil).Emit(protocol.EventFailed,
			protocol.Failure{Reason: "assignment refused by the runner: " + err.Error()})
	}
	for ok, r := range x.runs {
		if ok.step == a.Step && ok.fence < a.Fence && !ended(r) {
			return fmt.Errorf("an older attempt of step %s is still running", a.Step) // re-sent later
		}
	}
	dir, err := x.Journal.Prepare(a.Step, a.Fence)
	if err != nil {
		return fmt.Errorf("preparing the attempt directory: %w", err)
	}
	r := &run{rec: domain.Record{Step: a.Step, Fence: a.Fence, Execution: a.Execution, Project: a.Project,
		Scenario: a.ScenarioOrDefault(), Preparing: a.Base != ""}, done: make(chan struct{})}
	if err := x.Journal.Save(r.rec); err != nil {
		return fmt.Errorf("recording the attempt: %w", err)
	}
	r.known = []string{a.Token.Reveal()}
	if x.KnownSecrets != nil {
		r.known = append(r.known, x.KnownSecrets()...)
	}
	r.stream = x.Events.Attempt(a.Step, a.Fence, 0, x.reserver(r))
	if err := r.stream.Emit(protocol.EventAccepted, nil); err != nil {
		return x.abandon(r, fmt.Errorf("emitting accepted: %w", err))
	}
	if a.Base != "" {
		// KIN-71: the worktree (possibly a first clone) is prepared in the background, so the command is
		// acknowledged now and the channel keeps applying commands (a cancel stops the preparation).
		pctx, stop := context.WithTimeoutCause(x.lifetime(), x.prepareTimeout(), errPrepareTimeout)
		r.stopPrep = stop
		x.runs[k] = r
		x.wg.Go(func() { x.prepareAndLaunch(pctx, r, a, dir) })
		return nil
	}
	return x.launch(ctx, r, a, dir, "")
}

// launch starts the attempt's process in workdir (its worktree; "" for its journal directory). Called with x.mu
// held. An error means the attempt was dropped (its record removed): the command should be re-sent.
func (x *Executor) launch(ctx context.Context, r *run, a domain.Assignment, dir, workdir string) error {
	k := key{a.Step, a.Fence}
	var sandbox Sandbox
	if x.Egress != nil {
		sb, err := x.Egress.Confine(ctx, a, dir, workdir)
		if err != nil {
			return x.launchFailed(r, a, fmt.Errorf("%w: %w", ErrNetwork, err))
		}
		sandbox = sb
	}
	var agent *AgentCommand
	if a.CLI() {
		cmd, err := x.prepareAgent(a, dir, workdir, sandbox)
		if err != nil {
			if sandbox != nil {
				sandbox.Release()
			}
			return x.launchFailed(r, a, err)
		}
		agent, workdir = &cmd, cmd.Dir
		if err := r.save(x.Journal, func(rec *domain.Record) { rec.Agent = a.Adapter }); err != nil {
			return x.abandon(r, fmt.Errorf("recording the attempt's agent: %w", err))
		}
	}
	stdout, stderr := x.redacted(r, "stdout"), x.redacted(r, "stderr")
	proc, err := x.Launcher.Launch(ctx, LaunchSpec{Dir: dir, Workdir: workdir, Scenario: a.ScenarioOrDefault(),
		Attempt: a.Fence, Stdout: stdout, Stderr: stderr, Sandbox: sandbox, Agent: agent})
	if err != nil {
		if sandbox != nil {
			sandbox.Release()
		}
		return x.launchFailed(r, a, err)
	}
	r.proc, r.sandbox = proc, sandbox
	if proc.StartTime() == "" {
		x.logger().Warn("process start time unknown: after a restart a reused pid cannot be told apart", "step", a.Step,
			"pid", proc.PID())
	}
	if err := r.save(x.Journal, func(rec *domain.Record) { rec.PID, rec.PGID, rec.Start = proc.PID(), proc.PGID(), proc.StartTime() }); err != nil {
		x.logger().Error("recording the attempt's process", "step", a.Step, "err", err)
	}
	x.runs[k] = r
	if err := r.stream.Emit(protocol.EventStarted, protocol.Started{Agent: a.Adapter}); err != nil {
		x.logger().Error("emitting started", "step", a.Step, "err", err)
	}
	if agent != nil { // after execution.started: the CLI's events follow it
		r.agent = a.Adapter
		if r.follower = x.follow(r, dir, domain.OutputOffsets{}, stdout, stderr); r.follower == nil {
			// Without its output the run's result is unknown: stop it (reported as cancelled, never as a success).
			x.terminate(r, "the runner cannot read the CLI's output")
		}
	}
	x.watch(r)
	return nil
}

// prepareAgent decides the CLI's permission mode (the bypass only inside the strict egress sandbox, KIN-31: asked
// for elsewhere the attempt fails, the CLI never runs) and prepares its command.
func (x *Executor) prepareAgent(a domain.Assignment, dir, workdir string, sandbox Sandbox) (AgentCommand, error) {
	if x.Agents == nil {
		return AgentCommand{}, fmt.Errorf("%w: %s", ErrNoAgent, a.Adapter)
	}
	perms, err := domain.ChoosePermissions(a.BypassPermissions, (LaunchSpec{Sandbox: sandbox}).PermitBypass() == nil)
	if err != nil {
		return AgentCommand{}, err
	}
	if a.ReadOnly() {
		perms = domain.PermissionsReadOnly // a reviewer only reads, whatever else was asked (KIN-35)
	}
	req := domain.AgentRequest{Agent: a.Adapter, Workdir: workdir, Resume: a.Resume, Model: a.Model,
		BypassPermissions: a.BypassPermissions, Timeout: a.AgentTimeout()}
	if !a.ReadOnly() {
		// agent-exec runs it after a successful CLI and the follower reports its result (KIN-35).
		req.TestCommand, req.TestsTimeout = a.TestCommand, x.TestsTimeout
	}
	return x.Agents.Prepare(dir, req, x.reviewPrompt(a, workdir), perms)
}

// untrustedTagRe finds anything that could open or close an <untrusted> fence (any case, spaces), as the Control
// Plane's prompts neutralise it (workflow domain fence).
var untrustedTagRe = regexp.MustCompile(`(?i)<\s*/?\s*untrusted`)

// MaxReviewDiff bounds the diff a reviewer's prompt gets (bytes); the reviewer reads the files themselves beyond it.
const MaxReviewDiff = 48 << 10

// reviewPrompt is the assignment's prompt; a reviewer's gets the diff of the work it reviews (DiffBase..DiffHead),
// bounded and fenced as untrusted data, or a note when the runner cannot produce it.
func (x *Executor) reviewPrompt(a domain.Assignment, workdir string) string {
	if a.DiffBase == "" || workdir == "" || x.Workspaces == nil {
		return a.Prompt
	}
	diff, cut, err := x.Workspaces.Diff(context.Background(), a.Step, a.Fence, a.DiffBase, a.DiffHead, MaxReviewDiff)
	if err != nil {
		x.logger().Warn("reading the diff to review", "step", a.Step, "fence", a.Fence, "err", err)
		return a.Prompt + "\n## Diff\nThe runner could not produce the diff " + a.DiffBase + ".." + a.DiffHead +
			"; read the changed files in the working directory.\n"
	}
	note := ""
	if cut {
		note = fmt.Sprintf("(cut at %d KiB: read the remaining changed files in the working directory)\n", MaxReviewDiff>>10)
	}
	diff = untrustedTagRe.ReplaceAllStringFunc(diff, func(m string) string { return "‹" + strings.TrimLeft(m, "<") }) // the diff cannot close its fence
	return a.Prompt + "\n## Diff " + a.DiffBase + ".." + a.DiffHead + " (untrusted)\n" + note +
		"<untrusted source=\"diff\" id=\"" + a.DiffHead + "\">\n" + diff + "\n</untrusted>\n"
}

// follow starts following the CLI's output files: events and raw output beyond from go to the attempt's stream,
// the offsets reached are recorded for a restart. A follower that cannot start is logged and nil.
func (x *Executor) follow(r *run, dir string, from domain.OutputOffsets, stdout, stderr io.Writer) AgentFollower {
	k := r.key()
	f, err := x.Agents.Follow(r.agent, dir, AgentFollow{From: from, Stdout: stdout, Stderr: stderr,
		OnEvent: func(e domain.AgentEvent) { x.emitAgent(r, e) },
		Progress: func(o domain.OutputOffsets) {
			if x.closed.Load() {
				return // the output is no longer sent (guard): the next runner process reads on from the last offsets
			}
			if err := r.save(x.Journal, func(rec *domain.Record) { rec.Output = o }); err != nil {
				x.logger().Warn("recording the attempt's output offsets", "step", k.step, "fence", k.fence, "err", err)
			}
		}})
	if err != nil {
		x.logger().Error("following the CLI's output", "step", k.step, "fence", k.fence, "err", err)
		return nil
	}
	return f
}

// emitAgent sends one CLI event (masked and bounded like the output it summarises). It is best effort: an event
// the spool cannot take is logged, the raw output and the run's result still carry it.
func (x *Executor) emitAgent(r *run, e domain.AgentEvent) {
	if x.closed.Load() {
		return
	}
	pe := protocol.AgentEvent{Kind: string(e.Kind), Session: r.ident(e.Session, 128), Model: r.ident(e.Model, 128),
		Version: r.ident(e.Version, 64), Text: domain.Truncate(r.mask(e.Text), protocol.MaxAgentText), Tool: r.ident(e.Tool, 128),
		Input: domain.Truncate(r.mask(e.Input), protocol.MaxAgentInput), Usage: usage(e.Usage)}
	if f := e.Failure; f != nil {
		pe.Code, pe.Cause, pe.Message = string(f.Code), r.ident(f.Cause, 32), domain.Truncate(r.mask(f.Message), domain.MaxErrorMessage)
		if !f.ResetsAt.IsZero() {
			pe.ResetsAt = f.ResetsAt.UnixMilli()
		}
	}
	if err := r.stream.Emit(protocol.EventAgent, pe); err != nil {
		k := r.key()
		x.logger().Warn("emitting a CLI event", "step", k.step, "fence", k.fence, "kind", e.Kind, "err", err)
	}
}

func usage(u *domain.Usage) *protocol.AgentUsage {
	if u == nil {
		return nil
	}
	return &protocol.AgentUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens, CostUSD: u.CostUSD, Turns: u.Turns, DurationMS: u.DurationMS}
}

// agentEnd is the terminal event of a CLI run: its result with the agent report, the failure typed (cli_limit
// carries its cause and reset time).
func (r *run) agentEnd(res domain.AgentResult) (string, any) {
	rep := &protocol.AgentReport{Session: r.ident(res.Session, 128), Model: r.ident(res.Model, 128),
		Version: r.ident(res.Version, 64), Usage: usage(&res.Usage)}
	if res.Failure == nil {
		rep.Result = domain.Truncate(r.mask(res.Text), protocol.MaxAgentResult-len("…"))
		fitResult(rep)
		return protocol.EventCompleted, protocol.Completed{Agent: rep}
	}
	f := res.Failure
	reason := f.Reason()
	if f.Code == domain.FailureCancelled {
		reason = "cancelled: " + f.Message // as for any attempt (domain.Outcome)
	}
	body := protocol.Failure{Reason: domain.Truncate(r.mask(reason), protocol.MaxReasonLen), Code: string(f.Code),
		Cause: r.ident(f.Cause, 32), Agent: rep}
	if !f.ResetsAt.IsZero() {
		body.ResetsAt = f.ResetsAt.UnixMilli()
	}
	return protocol.EventFailed, body
}

// fitResult cuts the final answer until the execution.completed body fits the channel's limit (escaped control
// characters may grow it up to six times); a margin is left for the event envelope.
func fitResult(rep *protocol.AgentReport) {
	const room = protocol.MaxBodyBytes - 1<<10
	for rep.Result != "" {
		b, err := json.Marshal(protocol.Completed{Agent: rep})
		if err == nil && len(b) <= room {
			return
		}
		rep.Result = domain.Truncate(rep.Result, len(rep.Result)/2)
	}
}

// launchFailed reports an attempt that could not start. A strict egress failure (ErrNetwork) carries the code
// network: the step waits for the network instead of using its retry budget, and nothing ran unconfined.
func (x *Executor) launchFailed(r *run, a domain.Assignment, err error) error {
	x.logger().Error("launching the attempt", "step", a.Step, "fence", a.Fence, "err", err)
	x.releaseWorkspace(r)
	f := protocol.Failure{Reason: r.mask("launch failed: " + err.Error())}
	if errors.Is(err, ErrNetwork) {
		f = protocol.Failure{Reason: r.mask(err.Error()), Code: protocol.FailureNetwork}
	}
	if err := r.stream.Emit(protocol.EventFailed, f); err != nil {
		return x.abandon(r, err) // the re-sent command tries again
	}
	return x.Journal.Finish(a.Step, a.Fence)
}

func ended(r *run) bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// abandon drops a run that never launched.
func (x *Executor) abandon(r *run, err error) error {
	x.releaseWorkspace(r)
	k := r.key()
	_ = x.Journal.Delete(k.step, k.fence)
	return err
}

var errPrepareTimeout = errors.New("workspace preparation timed out")

func (x *Executor) prepareTimeout() time.Duration {
	if x.PrepareTimeout > 0 {
		return x.PrepareTimeout
	}
	return DefaultPrepareTimeout
}

func (x *Executor) prepareKeepalive() time.Duration {
	if x.PrepareKeepalive > 0 {
		return x.PrepareKeepalive
	}
	return DefaultPrepareKeepalive
}

// prepareAndLaunch prepares the attempt's worktree in the background and launches the attempt in it (KIN-71). A
// cancel, a newer token or StopAll stops the preparation (git is killed through ctx) and the attempt fails as
// cancelled; a preparation over PrepareTimeout fails it; when the executor is closed (the runner stops) nothing is
// reported: the next runner process fails the attempt from its journal record (Recover).
func (x *Executor) prepareAndLaunch(ctx context.Context, r *run, a domain.Assignment, dir string) {
	defer r.stopPrep()
	workdir, failure, err := x.prepareWorkspace(ctx, a, r)
	x.mu.Lock()
	r.stopPrep = nil
	switch {
	case x.closed.Load():
		x.mu.Unlock()
		close(r.done)
		return
	case r.cancelled != "":
		failure = "cancelled: " + r.cancelled
	case err != nil:
		x.mu.Unlock()
		x.logger().Error("preparing the attempt", "step", a.Step, "fence", a.Fence, "err", err)
		x.releaseWorkspace(r)
		f := protocol.Failure{Reason: domain.Truncate(r.mask("workspace: "+err.Error()), protocol.MaxReasonLen)}
		if x.report(r, protocol.EventFailed, f) {
			close(r.done)
			return
		}
		x.drop(r)
		return
	case failure == "":
		// Not the lifetime context itself: a Close racing with the launch must not turn it into a failure.
		err := x.launch(context.WithoutCancel(x.lifetime()), r, a, dir, workdir)
		launched := r.proc != nil
		if !launched {
			delete(x.runs, key{a.Step, a.Fence})
		}
		x.mu.Unlock()
		if err != nil {
			x.logger().Error("launching the attempt; dropped (the Control Plane assigns its step again)", "step", a.Step,
				"fence", a.Fence, "err", err)
		}
		if !launched {
			close(r.done)
		}
		return
	}
	x.mu.Unlock()
	x.releaseWorkspace(r)
	x.reportEnd(r, protocol.EventFailed, protocol.Failure{Reason: domain.Truncate(failure, protocol.MaxReasonLen)})
}

// drop forgets an attempt whose end cannot be reported either (the spool is unavailable): its record is removed
// and it is no longer listed, so the Control Plane's claim runs out (at most its lease, 60 s, after the last
// workspace.preparing) and the step is assigned again. The workspace was released already.
func (x *Executor) drop(r *run) {
	k := r.key()
	x.logger().Error("the attempt's failure cannot be reported; dropped", "step", k.step, "fence", k.fence)
	_ = x.Journal.Delete(k.step, k.fence)
	x.mu.Lock()
	delete(x.runs, r.key())
	x.mu.Unlock()
	close(r.done)
}

// reportEnd reports the end of an attempt that never launched; done is closed after the first try (a newer attempt
// of the step waits for it) and a failed report is retried like a process's end.
func (x *Executor) reportEnd(r *run, kind string, body any) {
	ok := x.report(r, kind, body)
	close(r.done)
	for !ok && !x.closed.Load() {
		select {
		case <-x.lifetime().Done():
			return // the next runner process reports it from the journal
		case <-time.After(reportRetry):
		}
		ok = x.report(r, kind, body)
	}
}

// preparing reports the preparation's phase now and every PrepareKeepalive until stop (which waits for a report in
// progress, so nothing follows it on the stream).
type preparing struct {
	x     *Executor
	r     *run
	start time.Time
	mu    sync.Mutex
	phase string
	stop  chan struct{}
	done  chan struct{}
}

func (x *Executor) startProgress(r *run) *preparing {
	p := &preparing{x: x, r: r, start: time.Now(), phase: protocol.PreparingQueued, stop: make(chan struct{}),
		done: make(chan struct{})}
	p.emit()
	go func() {
		defer close(p.done)
		t := time.NewTicker(x.prepareKeepalive())
		defer t.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-t.C:
				p.emit()
			}
		}
	}()
	return p
}

func (p *preparing) set(phase string) {
	p.mu.Lock()
	changed := p.phase != phase
	p.phase = phase
	p.mu.Unlock()
	if changed {
		p.emit()
	}
}

func (p *preparing) emit() {
	if p.x.closed.Load() {
		return
	}
	p.mu.Lock()
	body := protocol.WorkspacePreparing{Phase: p.phase, ElapsedSec: int64(time.Since(p.start) / time.Second)}
	p.mu.Unlock()
	if err := p.r.stream.Emit(protocol.EventWorkspacePreparing, body); err != nil {
		k := p.r.key()
		p.x.logger().Warn("reporting the workspace preparation", "step", k.step, "fence", k.fence, "err", err)
	}
}

func (p *preparing) end() {
	close(p.stop)
	<-p.done
}

// prepareWorkspace creates the attempt's worktree and reports it. failure: why the attempt fails (to be reported);
// err: the attempt could not be recorded or reported and is dropped.
func (x *Executor) prepareWorkspace(ctx context.Context, a domain.Assignment, r *run) (dir, failure string, err error) {
	progress := x.startProgress(r)
	var prep protocol.WorkspacePrepared
	err = errors.New("this runner has no workspace manager")
	if x.Workspaces != nil {
		dir, prep, err = x.Workspaces.Prepare(ctx, WorkspaceRequest{Step: a.Step, Fence: a.Fence, Task: a.Task,
			Repository: a.Repository, Base: a.Base, Token: a.Token, ReadOnly: a.ReadOnly(), Progress: progress.set})
	}
	progress.end()
	if err != nil {
		if errors.Is(context.Cause(ctx), errPrepareTimeout) {
			return "", fmt.Sprintf("%v after %s", errPrepareTimeout, x.prepareTimeout()), nil
		}
		if ctx.Err() == nil {
			x.logger().Warn("preparing the workspace", "step", a.Step, "fence", a.Fence, "err", err)
		}
		return "", r.mask("workspace: " + err.Error()), nil // a cancel's reason replaces it (prepareAndLaunch)
	}
	if err := r.save(x.Journal, func(rec *domain.Record) { rec.Workdir = dir }); err != nil {
		return "", "", fmt.Errorf("recording the workspace: %w", err)
	}
	if err := r.stream.Emit(protocol.EventWorkspacePrepared, prep); err != nil {
		return "", "", fmt.Errorf("emitting workspace.prepared: %w", err)
	}
	return dir, "", nil
}

// finishWorkspace saves the attempt's work to its branch, reports the changes and removes the worktree. A failure
// is logged and the worktree kept (its work is not lost).
func (x *Executor) finishWorkspace(r *run) {
	r.recMu.Lock()
	step, fence, wd := r.rec.Step, r.rec.Fence, r.rec.Workdir
	r.recMu.Unlock()
	if wd == "" || x.Workspaces == nil {
		return
	}
	// The leader ended; anything it left in its process group must not write into the worktree while it is saved
	// and removed.
	if err := r.proc.Signal(true); err != nil {
		x.logger().Warn("killing the attempt's leftover processes", "step", step, "fence", fence, "err", err)
	}
	ch, err := x.Workspaces.Finish(context.Background(), step, fence)
	if err != nil {
		x.logger().Error("finishing the workspace; the worktree is kept", "step", step, "fence", fence, "err", err)
		// The attempt's changes are unknown: a truncated report makes the Control Plane hold the step (KIN-51)
		// instead of completing it unchecked.
		ch = &protocol.WorkspaceChanges{Truncated: true}
	}
	if ch != nil {
		if err := r.stream.Emit(protocol.EventWorkspaceChanges, ch); err != nil {
			x.logger().Error("emitting workspace.changes", "step", step, "fence", fence, "err", err)
		}
	}
}

// reportChanges reports the running attempt's worktree changes every ChangesEvery when they differ from the last
// report, so the Control Plane can stop a step that leaves its approved scope while it runs (KIN-51). The returned
// stop ends the reports and waits for one in progress.
func (x *Executor) reportChanges(r *run) (stop func()) {
	r.recMu.Lock()
	step, fence, wd := r.rec.Step, r.rec.Fence, r.rec.Workdir
	r.recMu.Unlock()
	if x.ChangesEvery <= 0 || x.Workspaces == nil || wd == "" {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(x.ChangesEvery)
		defer t.Stop()
		last := ""
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if x.closed.Load() {
				return
			}
			ch, err := x.Workspaces.Changes(ctx, step, fence)
			if err != nil || ch == nil {
				if err != nil && ctx.Err() == nil {
					x.logger().Warn("listing the attempt's changes", "step", step, "fence", fence, "err", err)
				}
				continue
			}
			sig := changesSignature(ch)
			if sig == last {
				continue
			}
			if err := r.stream.Emit(protocol.EventWorkspaceChanges, ch); err != nil {
				x.logger().Warn("emitting workspace.changes", "step", step, "fence", fence, "err", err)
				continue
			}
			last = sig
		}
	}()
	return func() { cancel(); <-done }
}

// changesSignature identifies a change list; an empty list matches the start (nothing to report).
func changesSignature(ch *protocol.WorkspaceChanges) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d/%t", ch.FilesTotal, ch.Truncated)
	if ch.FilesTotal == 0 && !ch.Truncated {
		return ""
	}
	for _, f := range ch.Files {
		b.WriteString("\x00" + f.Status + f.Path)
	}
	return b.String()
}

// releaseWorkspace finishes the workspace of an attempt that never ran (no report). It does not rely on the
// record's Workdir: a workspace may have been prepared before the record could be saved (Finish is a no-op for an
// attempt without one).
func (x *Executor) releaseWorkspace(r *run) {
	r.recMu.Lock()
	step, fence := r.rec.Step, r.rec.Fence
	r.recMu.Unlock()
	if x.Workspaces == nil {
		return
	}
	if _, err := x.Workspaces.Finish(context.Background(), step, fence); err != nil {
		x.logger().Error("releasing the workspace; the worktree is kept", "step", step, "fence", fence, "err", err)
	}
}

func idOK(s string) bool {
	return s != "" && len(s) <= protocol.MaxIDLen && !strings.ContainsAny(s, " \t\r\n")
}

// reserver persists the attempt's event number reservations in its journal record (called under the stream's lock).
func (x *Executor) reserver(r *run) func(int64) error {
	return func(upTo int64) error {
		return r.save(x.Journal, func(rec *domain.Record) { rec.Reserved = upTo })
	}
}

// ident bounds and masks a short field the CLI reported (session id, model, version, tool name): CLI output is
// untrusted and an event must stay below protocol.MaxBodyBytes.
func (r *run) ident(s string, n int) string { return domain.Truncate(r.mask(s), n) }

// mask redacts a failure reason that may quote a git error (KIN-77).
func (r *run) mask(reason string) string { return redact.Lines(reason, r.known...) }

// outputIdle is how long a partial output line (a prompt, a progress line) is held back for redaction before it
// is sent anyway.
const outputIdle = time.Second

// redacted is the attempt's output stream with secrets masked before they are spooled (KIN-77): the git token and
// the runner's other known secrets by exact value, secret shapes by redact.Lines; a secret split across the
// process's writes is masked too (the partial last line is held back).
// The known values are read once per attempt: a proxy password changed while it runs is masked from the next one.
func (x *Executor) redacted(r *run, name string) io.Writer {
	s := redact.NewStream(x.guard(r.stream.Output(name)), outputIdle, r.known...)
	r.output = append(r.output, s)
	return s
}

// guard drops output once the executor is shut down (a restarting runner's old pipes keep draining, so the process
// never blocks on a full pipe).
func (x *Executor) guard(w io.Writer) io.Writer { return guardedWriter{x, w} }

type guardedWriter struct {
	x *Executor
	w io.Writer
}

func (g guardedWriter) Write(p []byte) (int, error) {
	if g.x.closed.Load() {
		return len(p), nil
	}
	return g.w.Write(p)
}

// reportRetry is how often a failed report of an attempt's end is retried.
const reportRetry = time.Second

// watch waits for the process in the background and reports its end; a report that fails (e.g. the disk is full)
// is retried, and until it succeeds the attempt is listed with its result for reconciliation.
func (x *Executor) watch(r *run) {
	stopReports := x.reportChanges(r)
	x.wg.Go(func() {
		exit := r.proc.Wait()
		var res *domain.AgentResult
		if r.follower != nil { // read the CLI's output to its end before the output streams are flushed
			x.mu.Lock()
			cancelled := r.cancelled
			x.mu.Unlock()
			v := r.follower.Finish(exit, cancelled)
			res = &v
		}
		if r.sandbox != nil {
			r.sandbox.Release()
		}
		for _, s := range r.output { // the pipes are drained: send the held-back partial lines before the end
			if err := s.Flush(); err != nil {
				x.logger().Warn("sending the attempt's last output", "step", r.rec.Step, "err", err)
			}
		}
		if res != nil && res.Tests != nil {
			x.reportTests(r, *res.Tests) // the evidence precedes the final changes and the end (KIN-35)
		}
		stopReports() // the worktree is about to be saved and removed
		if !x.closed.Load() {
			x.finishWorkspace(r) // before done: a newer attempt of the step waits for the worktree's release
		}
		close(r.done)
		x.mu.Lock()
		ok, reason := domain.Outcome(exit, r.cancelled)
		x.mu.Unlock()
		kind, body := protocol.EventCompleted, any(nil)
		if !ok {
			kind, body = protocol.EventFailed, protocol.Failure{Reason: reason}
		}
		if res != nil {
			kind, body = r.agentEnd(*res)
		}
		for {
			if x.closed.Load() {
				return // the next runner process adopts the attempt from the journal
			}
			if x.report(r, kind, body) {
				return
			}
			time.Sleep(reportRetry)
		}
	})
}

// reportTests sends the attempt's test result (masked, the output's tail bounded). A result the spool cannot take is
// logged: the Control Plane then finds no evidence and fails the attempt (never a pass).
func (x *Executor) reportTests(r *run, t domain.TestsRun) {
	body := protocol.TestResult{Command: domain.Truncate(r.mask(t.Command), protocol.MaxTestCommand), ExitCode: t.Code,
		TimedOut: t.TimedOut, DurationMS: t.DurationMS, Output: tail(r.mask(t.Output), protocol.MaxTestOutput)}
	if err := r.stream.Emit(protocol.EventTests, body); err != nil {
		k := r.key()
		x.logger().Error("emitting the test result", "step", k.step, "fence", k.fence, "err", err)
	}
}

// tail is the end of s in at most n bytes ("…" marks a cut), cut at a rune boundary.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n+len("…"):]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return "…" + s
}

func (x *Executor) report(r *run, kind string, body any) bool {
	err := r.stream.Emit(kind, body)
	k := r.key()
	x.mu.Lock()
	defer x.mu.Unlock()
	if err != nil {
		r.result = kind
		x.logger().Error("reporting the attempt's end; retrying", "step", k.step, "fence", k.fence, "err", err)
		return false
	}
	delete(x.runs, k)
	if err := x.Journal.Finish(k.step, k.fence); err != nil {
		x.logger().Error("removing the attempt record", "step", k.step, "err", err)
	}
	return true
}

// Cancel stops the attempt's process group: SIGTERM, then SIGKILL after the grace period. An unknown attempt is
// ignored (it already ended, or never ran here).
func (x *Executor) Cancel(step string, fence int64, reason string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if r, ok := x.runs[key{step, fence}]; ok {
		x.terminate(r, reason)
	}
}

func (x *Executor) terminate(r *run, reason string) {
	if reason == "" {
		reason = protocol.CancelCancelled
	}
	if r.cancelled != "" {
		return
	}
	r.cancelled = reason
	k := r.key()
	x.logger().Info("cancelling attempt", "step", k.step, "fence", k.fence, "reason", reason)
	if r.proc == nil { // still preparing its workspace (KIN-71): stop git; the attempt then fails as cancelled
		if r.stopPrep != nil {
			r.stopPrep()
		}
		return
	}
	if err := r.proc.Signal(false); err != nil {
		x.logger().Warn("SIGTERM to the attempt's process group", "step", k.step, "err", err)
	}
	grace := x.grace()
	x.wg.Go(func() {
		t := time.NewTimer(grace)
		defer t.Stop()
		select {
		case <-r.done:
		case <-t.C:
			if err := r.proc.Signal(true); err != nil {
				x.logger().Warn("SIGKILL to the attempt's process group", "step", k.step, "err", err)
			}
		}
	})
}

// Recover adopts the attempts of the journal after a (re)start: a process still alive is watched again, a finished
// one is reported with the result it left, and a record that never got a process fails.
func (x *Executor) Recover(_ context.Context) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.init()
	recs, err := x.Journal.List()
	if err != nil {
		return err
	}
	for _, rec := range recs {
		k := key{rec.Step, rec.Fence}
		if _, ok := x.runs[k]; ok {
			continue
		}
		r := &run{rec: rec, done: make(chan struct{})}
		r.stream = x.Events.Attempt(rec.Step, rec.Fence, rec.Reserved, x.reserver(r))
		if rec.PID == 0 {
			x.releaseWorkspace(r)
			reason := "launch interrupted by a runner restart"
			if rec.Preparing && rec.Workdir == "" {
				reason = "workspace preparation interrupted by a runner restart"
			}
			if err := r.stream.Emit(protocol.EventFailed, protocol.Failure{Reason: reason}); err != nil {
				return err
			}
			if err := x.Journal.Finish(rec.Step, rec.Fence); err != nil {
				return err
			}
			continue
		}
		d, err := x.Journal.Prepare(rec.Step, rec.Fence)
		if err != nil {
			return err
		}
		r.proc = x.Launcher.Adopt(rec, d)
		if rec.Agent != "" { // KIN-79: read the CLI's output files on from where the last runner process stopped
			r.agent = rec.Agent
			if x.KnownSecrets != nil {
				r.known = x.KnownSecrets()
			}
			if x.Agents == nil {
				x.logger().Error("adopted a CLI attempt without CLI agents: its result is lost", "step", rec.Step)
			} else {
				r.follower = x.follow(r, d, rec.Output, x.redacted(r, "stdout"), x.redacted(r, "stderr"))
			}
		}
		x.runs[k] = r
		x.logger().Info("adopted attempt after restart", "step", rec.Step, "fence", rec.Fence, "pid", rec.PID)
		x.watch(r)
	}
	return nil
}

// Attempts reports the running attempts for runner.reconcile (connection.Processes).
func (x *Executor) Attempts() []protocol.AttemptStatus {
	x.mu.Lock()
	defer x.mu.Unlock()
	out := make([]protocol.AttemptStatus, 0, len(x.runs))
	for _, r := range x.runs {
		r.recMu.Lock()
		st := protocol.AttemptStatus{Step: r.rec.Step, Fence: r.rec.Fence, PID: r.rec.PID, PGID: r.rec.PGID,
			Running: r.result == "", Result: r.result}
		r.recMu.Unlock()
		out = append(out, st)
	}
	return out
}

// stopWait bounds how long StopAll waits beyond the grace period for a killed process group to be reaped.
const stopWait = 5 * time.Second

// StopAll stops every running attempt's process group (SIGTERM, SIGKILL after the grace period) with reason and
// returns once they all ended (KIN-63: the Control Plane disabled or revoked the runner, which stops its attempts
// itself instead of waiting for execution.cancel). Their ends are reported as usual (failed, "cancelled: reason").
func (x *Executor) StopAll(reason string) {
	x.mu.Lock()
	runs := make([]*run, 0, len(x.runs))
	for _, r := range x.runs {
		if r.result == "" && !ended(r) { // a running process, or an attempt still preparing (KIN-71)
			x.terminate(r, reason)
			runs = append(runs, r)
		}
	}
	x.mu.Unlock()
	if len(runs) == 0 {
		return
	}
	t := time.NewTimer(x.grace() + stopWait)
	defer t.Stop()
	for _, r := range runs {
		select {
		case <-r.done:
		case <-t.C:
			x.logger().Error("attempt processes did not end after SIGKILL", "reason", reason)
			return
		}
	}
}

// Close stops reporting and leaves the processes running for the next runner process to adopt.
func (x *Executor) Close() {
	x.closed.Store(true)
	x.lifetime()
	x.stopLife() // workspace preparations stop; the next runner process fails their attempts (Recover)
}

// KillAll stops every running attempt's process group at once and waits for them (tests and shutdown with kill).
func (x *Executor) KillAll() {
	x.mu.Lock()
	for _, r := range x.runs {
		if r.cancelled == "" {
			r.cancelled = protocol.CancelCancelled
		}
		if r.proc == nil { // preparing its workspace (KIN-71)
			if r.stopPrep != nil {
				r.stopPrep()
			}
			continue
		}
		_ = r.proc.Signal(true)
	}
	x.mu.Unlock()
	x.wg.Wait()
}
