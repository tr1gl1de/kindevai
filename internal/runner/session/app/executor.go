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
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
}

// Process is a running attempt process (the leader of its own process group).
type Process interface {
	PID() int
	PGID() int
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
}

// Workspaces prepares and finishes attempt worktrees (the workspace context's Manager).
type Workspaces interface {
	// Prepare returns the worktree directory and its report (workspace.prepared).
	Prepare(ctx context.Context, r WorkspaceRequest) (string, protocol.WorkspacePrepared, error)
	// Finish commits leftover work to the attempt's branch, removes the worktree and returns what changed
	// (workspace.changes); nil for an attempt without a workspace.
	Finish(ctx context.Context, step string, fence int64) (*protocol.WorkspaceChanges, error)
}

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
	Grace      time.Duration // DefaultGrace when zero
	Logger     *slog.Logger

	mu     sync.Mutex
	runs   map[key]*run
	closed atomic.Bool
	wg     sync.WaitGroup
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
	cancelled string
	done      chan struct{}
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
			Project: a.Project, Repository: a.Repository, Scope: a.Scope, Adapter: a.Adapter, Scenario: a.Scenario, Base: a.Base}
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
		Scenario: a.ScenarioOrDefault()}, done: make(chan struct{})}
	if err := x.Journal.Save(r.rec); err != nil {
		return fmt.Errorf("recording the attempt: %w", err)
	}
	r.stream = x.Events.Attempt(a.Step, a.Fence, 0, x.reserver(r))
	if err := r.stream.Emit(protocol.EventAccepted, nil); err != nil {
		return x.abandon(r, fmt.Errorf("emitting accepted: %w", err))
	}
	var workdir string
	if a.Base != "" {
		wd, failed, err := x.prepareWorkspace(ctx, a, r)
		switch {
		case err != nil:
			return x.abandon(r, err)
		case failed:
			return x.Journal.Finish(a.Step, a.Fence)
		}
		workdir = wd
	}
	proc, err := x.Launcher.Launch(ctx, LaunchSpec{Dir: dir, Workdir: workdir, Scenario: a.ScenarioOrDefault(),
		Attempt: a.Fence, Stdout: x.guard(r.stream.Output("stdout")), Stderr: x.guard(r.stream.Output("stderr"))})
	if err != nil {
		x.logger().Error("launching the attempt", "step", a.Step, "fence", a.Fence, "err", err)
		x.releaseWorkspace(r)
		if err := r.stream.Emit(protocol.EventFailed, protocol.Failure{Reason: "launch failed: " + err.Error()}); err != nil {
			return x.abandon(r, err) // the re-sent command tries again
		}
		return x.Journal.Finish(a.Step, a.Fence)
	}
	r.proc = proc
	if err := r.save(x.Journal, func(rec *domain.Record) { rec.PID, rec.PGID = proc.PID(), proc.PGID() }); err != nil {
		x.logger().Error("recording the attempt's process", "step", a.Step, "err", err)
	}
	x.runs[k] = r
	if err := r.stream.Emit(protocol.EventStarted, nil); err != nil {
		x.logger().Error("emitting started", "step", a.Step, "err", err)
	}
	x.watch(r)
	return nil
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

// prepareWorkspace creates the attempt's worktree and reports it. failed: the attempt failed (reported); err: the
// command should be re-sent.
func (x *Executor) prepareWorkspace(ctx context.Context, a domain.Assignment, r *run) (string, bool, error) {
	var (
		dir  string
		prep protocol.WorkspacePrepared
		err  = errors.New("this runner has no workspace manager")
	)
	if x.Workspaces != nil {
		dir, prep, err = x.Workspaces.Prepare(ctx, WorkspaceRequest{Step: a.Step, Fence: a.Fence, Task: a.Task,
			Repository: a.Repository, Base: a.Base, Token: a.Token})
	}
	if err != nil && ctx.Err() != nil {
		return "", false, fmt.Errorf("preparing the workspace: %w", ctx.Err()) // re-sent on the next connection
	}
	if err != nil {
		x.logger().Warn("preparing the workspace", "step", a.Step, "fence", a.Fence, "err", err)
		if err := r.stream.Emit(protocol.EventFailed, protocol.Failure{Reason: "workspace: " + err.Error()}); err != nil {
			return "", false, err
		}
		return "", true, nil
	}
	if err := r.save(x.Journal, func(rec *domain.Record) { rec.Workdir = dir }); err != nil {
		return "", false, fmt.Errorf("recording the workspace: %w", err)
	}
	if err := r.stream.Emit(protocol.EventWorkspacePrepared, prep); err != nil {
		return "", false, fmt.Errorf("emitting workspace.prepared: %w", err)
	}
	return dir, false, nil
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
		return
	}
	if ch != nil {
		if err := r.stream.Emit(protocol.EventWorkspaceChanges, ch); err != nil {
			x.logger().Error("emitting workspace.changes", "step", step, "fence", fence, "err", err)
		}
	}
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
	x.wg.Go(func() {
		exit := r.proc.Wait()
		if !x.closed.Load() {
			x.finishWorkspace(r) // before done: a newer attempt of the step waits for the worktree's release
		}
		close(r.done)
		x.mu.Lock()
		ok, reason := domain.Outcome(exit, r.cancelled)
		x.mu.Unlock()
		kind := protocol.EventCompleted
		if !ok {
			kind = protocol.EventFailed
		}
		for {
			if x.closed.Load() {
				return // the next runner process adopts the attempt from the journal
			}
			if x.report(r, kind, reason) {
				return
			}
			time.Sleep(reportRetry)
		}
	})
}

func (x *Executor) report(r *run, kind, reason string) bool {
	var err error
	if kind == protocol.EventCompleted {
		err = r.stream.Emit(kind, nil)
	} else {
		err = r.stream.Emit(kind, protocol.Failure{Reason: reason})
	}
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
			if err := r.stream.Emit(protocol.EventFailed, protocol.Failure{Reason: "launch interrupted by a runner restart"}); err != nil {
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

// Close stops reporting and leaves the processes running for the next runner process to adopt.
func (x *Executor) Close() {
	x.closed.Store(true)
}

// KillAll stops every running attempt's process group at once and waits for them (tests and shutdown with kill).
func (x *Executor) KillAll() {
	x.mu.Lock()
	for _, r := range x.runs {
		if r.cancelled == "" {
			r.cancelled = protocol.CancelCancelled
		}
		_ = r.proc.Signal(true)
	}
	x.mu.Unlock()
	x.wg.Wait()
}
