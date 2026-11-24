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
	Dir      string // the attempt's working directory
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
	// Delete removes the record and the working directory.
	Delete(step string, fence int64) error
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

// DefaultGrace is how long a cancelled process group has between SIGTERM and SIGKILL.
const DefaultGrace = 5 * time.Second

// Executor runs assignments (session context).
type Executor struct {
	Launcher Launcher
	Journal  Journal
	Events   Events
	Policy   domain.Policy
	Grace    time.Duration // DefaultGrace when zero
	Logger   *slog.Logger

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
	rec       domain.Record
	proc      Process
	stream    Stream
	cancelled string
	done      chan struct{}
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
		return x.Assign(ctx, domain.Assignment{Step: a.Step, Fence: a.Fence, Execution: a.Execution, Task: a.Task,
			Project: a.Project, Repository: a.Repository, Scope: a.Scope, Adapter: a.Adapter, Scenario: a.Scenario})
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
// repeated command never starts a second process). An assignment the runner's policy refuses fails at once.
func (x *Executor) Assign(ctx context.Context, a domain.Assignment) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.init()
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
	// A newer token for the step supersedes an older attempt still running here: never two processes per step.
	for ok, r := range x.runs {
		if ok.step == a.Step && ok.fence < a.Fence {
			x.terminate(r, protocol.CancelStaleFence)
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
	proc, err := x.Launcher.Launch(ctx, LaunchSpec{Dir: dir, Scenario: r.rec.Scenario, Attempt: a.Fence,
		Stdout: x.guard(r.stream.Output("stdout")), Stderr: x.guard(r.stream.Output("stderr"))})
	if err != nil {
		x.logger().Error("launching the attempt", "step", a.Step, "fence", a.Fence, "err", err)
		if err := r.stream.Emit(protocol.EventFailed, protocol.Failure{Reason: "launch failed: " + err.Error()}); err != nil {
			return err
		}
		return x.Journal.Delete(a.Step, a.Fence)
	}
	r.proc = proc
	r.rec.PID, r.rec.PGID = proc.PID(), proc.PGID()
	if err := x.Journal.Save(r.rec); err != nil {
		x.logger().Error("recording the attempt's process", "step", a.Step, "err", err)
	}
	x.runs[k] = r
	if err := r.stream.Emit(protocol.EventStarted, nil); err != nil {
		x.logger().Error("emitting started", "step", a.Step, "err", err)
	}
	x.watch(r)
	return nil
}

// abandon drops a run that never launched.
func (x *Executor) abandon(r *run, err error) error {
	_ = x.Journal.Delete(r.rec.Step, r.rec.Fence)
	return err
}

func idOK(s string) bool {
	return s != "" && len(s) <= protocol.MaxIDLen && !strings.ContainsAny(s, " \t\r\n")
}

// reserver persists the attempt's event number reservations in its journal record (called under the stream's lock).
func (x *Executor) reserver(r *run) func(int64) error {
	var mu sync.Mutex
	return func(upTo int64) error {
		mu.Lock()
		defer mu.Unlock()
		rec := r.rec
		rec.Reserved = upTo
		if err := x.Journal.Save(rec); err != nil {
			return err
		}
		r.rec.Reserved = upTo
		return nil
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

// watch waits for the process in the background and reports its end.
func (x *Executor) watch(r *run) {
	x.wg.Go(func() {
		exit := r.proc.Wait()
		close(r.done)
		x.mu.Lock()
		defer x.mu.Unlock()
		if x.closed.Load() {
			return // the next runner process adopts the attempt from the journal
		}
		delete(x.runs, key{r.rec.Step, r.rec.Fence})
		ok, reason := domain.Outcome(exit, r.cancelled)
		var err error
		if ok {
			err = r.stream.Emit(protocol.EventCompleted, nil)
		} else {
			err = r.stream.Emit(protocol.EventFailed, protocol.Failure{Reason: reason})
		}
		if err != nil {
			x.logger().Error("reporting the attempt's end; it is reported again after a restart", "step", r.rec.Step,
				"fence", r.rec.Fence, "err", err)
			return
		}
		if err := x.Journal.Delete(r.rec.Step, r.rec.Fence); err != nil {
			x.logger().Error("removing the attempt record", "step", r.rec.Step, "err", err)
		}
	})
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
	x.logger().Info("cancelling attempt", "step", r.rec.Step, "fence", r.rec.Fence, "reason", reason)
	if err := r.proc.Signal(false); err != nil {
		x.logger().Warn("SIGTERM to the attempt's process group", "step", r.rec.Step, "err", err)
	}
	grace := x.grace()
	x.wg.Go(func() {
		t := time.NewTimer(grace)
		defer t.Stop()
		select {
		case <-r.done:
		case <-t.C:
			if err := r.proc.Signal(true); err != nil {
				x.logger().Warn("SIGKILL to the attempt's process group", "step", r.rec.Step, "err", err)
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
			if err := r.stream.Emit(protocol.EventFailed, protocol.Failure{Reason: "launch interrupted by a runner restart"}); err != nil {
				return err
			}
			if err := x.Journal.Delete(rec.Step, rec.Fence); err != nil {
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
		out = append(out, protocol.AttemptStatus{Step: r.rec.Step, Fence: r.rec.Fence, PID: r.rec.PID, PGID: r.rec.PGID,
			Running: true})
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
