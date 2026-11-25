package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	"github.com/tr1gl1de/kindevai/internal/platform/redact"
	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

// ChannelRepo stores the command logs, channel cursors and recorded events.
type ChannelRepo interface {
	// EnsureChannel creates the runner's channel with epoch unless it exists.
	EnsureChannel(ctx context.Context, runnerID, epoch string) error
	Channel(ctx context.Context, runnerID string) (domain.Channel, bool, error)
	// AppendCommand adds the command with the next Seq unless its ID is already in the log; it returns the Seq.
	AppendCommand(ctx context.Context, runnerID string, c domain.QueuedCommand, now time.Time) (int64, error)
	CommandsAfter(ctx context.Context, runnerID string, after int64, limit int) ([]domain.QueuedCommand, error)
	// AckCommands records the cumulative ACK at now (never moving back) and purges commands acknowledged more than
	// domain.AckedCommandRetention ago.
	AckCommands(ctx context.Context, runnerID string, seq int64, now time.Time) error
	SaveEventCursor(ctx context.Context, runnerID, epoch string, last int64) error
	EventSeen(ctx context.Context, runnerID, step string, fence, attemptSeq int64) (bool, error)
	RecordEvent(ctx context.Context, e domain.AttemptEvent) error
	// AddOutput counts n output bytes for the attempt at now if they fit both limits; the verdict says which
	// limit they would exceed otherwise.
	AddOutput(ctx context.Context, step string, fence, n int64, limits domain.OutputLimits, now time.Time) (domain.OutputVerdict, error)
}

// Executions is the workflow as the channel needs it. runnerID comes from the authenticated session. Errors:
// domain.ErrStaleAttempt (token superseded, or the lease ran out before the start), domain.ErrEventRejected
// (the event does not fit the step's state); anything else is transient (the event is not acknowledged and the
// runner sends it again).
type Executions interface {
	CheckAttempt(ctx context.Context, runnerID, step string, fence int64) error
	// StartAttempt, CompleteAttempt and FailAttempt get the event's time on the runner's clock (agent time, KIN-25);
	// StartAttempt also the agent the runner reports.
	StartAttempt(ctx context.Context, runnerID, step string, fence int64, at time.Time, agent string) error
	CompleteAttempt(ctx context.Context, runnerID, step string, fence int64, at time.Time) error
	FailAttempt(ctx context.Context, runnerID, step string, fence int64, reason string, at time.Time) error
}

// ScopeChecker evaluates what an attempt reports it changed (workspace.changes) against the step's approved scope
// (KIN-51), in the event's transaction. runnerID comes from the authenticated session and the attempt's token was
// checked first. A violation holds the step for a new approval; errors are those of Executions.
type ScopeChecker interface {
	CheckChanges(ctx context.Context, runnerID, step string, fence int64, ch protocol.WorkspaceChanges) error
}

// Reconciler receives the runner's runner.reconcile.response (KIN-23 resolves UNKNOWN attempts with it).
type Reconciler interface {
	Reconciled(ctx context.Context, runnerID string, r protocol.ReconcileResponse) error
}

// Channel is the Control Plane end of the runner command/event channel (KIN-22).
//
// Commands use a per-runner transactional outbox, the command log: Send writes the command in the caller's
// transaction, so it commits or rolls back with the domain change and the runner sees commands in commit order.
// (The generic messaging outbox delivers concurrently and cannot keep that order.) The session's sender delivers
// the log at least once until the runner acknowledges.
type Channel struct {
	Repo       ChannelRepo
	Executions Executions
	Reconciler Reconciler // optional
	// Scope, if set, checks every workspace.changes report of a current attempt (KIN-51).
	Scope ScopeChecker
	// Prepare, if set, transforms a command's stored body right before it is sent on the runner's authenticated
	// session (KIN-30 opens sealed proxy passwords there, so they never sit in the log in plaintext). On an error
	// the stored body is sent: the stream must not stall.
	Prepare func(ctx context.Context, runnerID string, q domain.QueuedCommand) (json.RawMessage, error)
	Tx      TxRunner
	// Instance identifies this Control Plane process (random per start; defaults to NewID()). It qualifies the
	// command epoch on the wire (domain.Channel.WireEpoch), so a restored database never reuses seq numbers.
	Instance string
	Now      func() time.Time
	NewID    func() string
	Logger   *slog.Logger
	// Window bounds unacknowledged commands in flight (default 32), AckTimeout re-sends from the last ACK when
	// nothing was acknowledged for that long (default 30s). PollInterval is how often a session looks for commands
	// committed by other transactions (default 1s; a Send also wakes it).
	Window       int
	AckTimeout   time.Duration
	PollInterval time.Duration
	// MaxStoredOutput bounds all stored output (default domain.DefaultRetention().MaxOutputBytes); output beyond
	// it is acknowledged and recorded without its body until the retention frees space (KIN-52).
	MaxStoredOutput int64
	// Outbox, if set, raises the Control Plane alert alerts.runner_output_full while output is dropped for that
	// limit (at most once a minute) and alerts.runner_output_recovered when output is stored again (KIN-59).
	Outbox Enqueuer

	once sync.Once
	// Output storage state (KIN-59), changed only after the event's transaction committed.
	fullWarn time.Time // last "output storage full" warning/alert (at most one per minute)
	lastDrop time.Time // last output dropped for the total limit
	// spaceKnown: no recovery is pending (output was stored again after the last drop). A fresh Control Plane does
	// not know whether an earlier run left the alert open; its first stored output resolves it (a no-op if none).
	spaceKnown bool
	mu         sync.Mutex
	wakes      map[string]chan struct{} // runnerID → the live session's sender
}

// commandBatch: commands loaded per read; with ≤64 KiB per command it bounds a sender's memory to ~256 KiB.
const commandBatch = 4

func (c *Channel) init() {
	c.once.Do(func() {
		c.wakes = map[string]chan struct{}{}
		if c.Logger == nil {
			c.Logger = slog.New(slog.DiscardHandler)
		}
		if c.Window <= 0 {
			c.Window = 32
		}
		if c.AckTimeout <= 0 {
			c.AckTimeout = 30 * time.Second
		}
		if c.PollInterval <= 0 {
			c.PollInterval = time.Second
		}
		if c.Instance == "" {
			c.Instance = c.NewID()
		}
		if c.MaxStoredOutput <= 0 {
			c.MaxStoredOutput = domain.DefaultRetention().MaxOutputBytes
		}
	})
}

func (c *Channel) wake(runnerID string) {
	c.mu.Lock()
	ch := c.wakes[runnerID]
	c.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Send appends a command to the runner's log. Call it inside the use case's transaction; id must be unique per
// runner and deterministic for the domain change: a repeated use case finds the id and appends nothing.
func (c *Channel) Send(ctx context.Context, runnerID, id, kind string, body any) error {
	c.init()
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", kind, err)
	}
	probe := protocol.Command{Epoch: "e", Seq: 1, ID: id, Kind: kind, Body: raw}
	if err := probe.Validate(); err != nil {
		return err
	}
	err = c.Tx.InTx(ctx, func(ctx context.Context) error {
		if err := c.Repo.EnsureChannel(ctx, runnerID, c.NewID()); err != nil {
			return err
		}
		_, err := c.Repo.AppendCommand(ctx, runnerID, domain.QueuedCommand{ID: id, Kind: kind, Body: raw}, c.Now())
		return err
	})
	if err != nil {
		return fmt.Errorf("queueing runner command: %w", err)
	}
	c.wake(runnerID) // may run before the caller commits; the session's poll picks the command up then
	return nil
}

// Serve runs for the lifetime of a runner session: it asks the runner to reconcile, then streams the runner's
// unacknowledged commands in Seq order, at most Window in flight, re-sending from the last ACK after a reconnect
// and when nothing was acknowledged for AckTimeout.
func (c *Channel) Serve(ctx context.Context, s Session) {
	c.init()
	wake := make(chan struct{}, 1)
	c.mu.Lock()
	c.wakes[s.RunnerID] = wake
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.wakes[s.RunnerID] == wake {
			delete(c.wakes, s.RunnerID)
		}
		c.mu.Unlock()
	}()
	if err := s.Send(ctx, protocol.TypeReconcile, protocol.Reconcile{RequestID: c.NewID()}); err != nil {
		return
	}
	if err := c.streamCommands(ctx, s, wake); err != nil && ctx.Err() == nil {
		c.Logger.Warn("runner command stream stopped", "runner", s.RunnerID, "err", err)
	}
}

func (c *Channel) streamCommands(ctx context.Context, s Session, wake chan struct{}) error {
	var sent, progress int64 = -1, -1
	progressAt := c.Now()
	tick := time.NewTicker(c.PollInterval)
	defer tick.Stop()
	for {
		st, ok, err := c.Repo.Channel(ctx, s.RunnerID)
		if err != nil {
			return err
		}
		if ok {
			if st.CommandAcked != progress {
				progress, progressAt = st.CommandAcked, c.Now()
			}
			if sent < st.CommandAcked {
				sent = st.CommandAcked
			}
			if sent > st.CommandAcked && c.Now().Sub(progressAt) >= c.AckTimeout {
				c.Logger.Warn("runner commands not acknowledged in time; re-sending", "runner", s.RunnerID,
					"from", st.CommandAcked+1)
				sent, progressAt = st.CommandAcked, c.Now()
			}
			if free := c.Window - int(sent-st.CommandAcked); free > 0 && sent < st.CommandSeq {
				cmds, err := c.Repo.CommandsAfter(ctx, s.RunnerID, sent, min(free, commandBatch))
				if err != nil {
					return err
				}
				for _, q := range cmds {
					body := q.Body
					if c.Prepare != nil {
						if b, err := c.Prepare(ctx, s.RunnerID, q); err != nil {
							c.Logger.Warn("preparing runner command failed; sending it as stored", "runner", s.RunnerID,
								"command", q.ID, "kind", q.Kind, "err", err)
						} else {
							body = b
						}
					}
					if err := s.Send(ctx, protocol.TypeCommand, protocol.Command{Epoch: st.WireEpoch(c.Instance), Seq: q.Seq,
						Base: st.CommandAcked, ID: q.ID, Kind: q.Kind, Body: body}); err != nil {
						return err
					}
					sent = q.Seq
				}
				if len(cmds) > 0 {
					continue
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-wake:
		case <-tick.C:
		}
	}
}

// Types are the message types Handle takes (register them on the gateway).
func (c *Channel) Types() []string {
	return []string{protocol.TypeCommandAck, protocol.TypeEvent, protocol.TypeReconcileResponse}
}

func strictDecode(env protocol.Envelope, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(env.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %s", protocol.ErrChannelField, env.Type)
	}
	return nil
}

// Handle processes one channel message of the session. It runs in the session's read loop, so a slow commit
// slows the runner down (TCP backpressure) instead of buffering. An error closes the connection (protocol error).
func (c *Channel) Handle(ctx context.Context, s Session, env protocol.Envelope) error {
	c.init()
	switch env.Type {
	case protocol.TypeCommandAck:
		var a protocol.Ack
		if err := strictDecode(env, &a); err != nil {
			return err
		}
		return c.commandAck(ctx, s, a)
	case protocol.TypeEvent:
		var e protocol.Event
		if err := strictDecode(env, &e); err != nil {
			return err
		}
		if err := e.Validate(); err != nil {
			return err
		}
		return c.event(ctx, s, e)
	case protocol.TypeReconcileResponse:
		var r protocol.ReconcileResponse
		if err := strictDecode(env, &r); err != nil {
			return err
		}
		c.Logger.Info("runner reconciled", "runner", s.RunnerID, "attempts", len(r.Attempts), "spool_acked", r.Spool.Acked,
			"spool_last", r.Spool.Last, "spool_bytes", r.Spool.Bytes, "output_dropped_bytes", r.Spool.DroppedBytes)
		if c.Reconciler != nil {
			if err := c.Reconciler.Reconciled(ctx, s.RunnerID, r); err != nil {
				c.Logger.Error("handling runner reconciliation", "runner", s.RunnerID, "err", err)
			}
		}
		return nil
	}
	return fmt.Errorf("%w: unexpected %s", protocol.ErrChannelField, env.Type)
}

func (c *Channel) commandAck(ctx context.Context, s Session, a protocol.Ack) error {
	st, ok, err := c.Repo.Channel(ctx, s.RunnerID)
	if err != nil {
		return err
	}
	if !ok || a.Epoch != st.WireEpoch(c.Instance) || a.Seq <= st.CommandAcked {
		return nil // another epoch's or an old ACK: nothing to do
	}
	if a.Seq > st.CommandSeq {
		return fmt.Errorf("%w: ACK of command %d never sent", protocol.ErrChannelField, a.Seq)
	}
	if err := c.Repo.AckCommands(ctx, s.RunnerID, a.Seq, c.Now()); err != nil {
		return err
	}
	c.wake(s.RunnerID)
	return nil
}

// event applies one event exactly once per spool Seq (go-back-N: a gap is dropped and re-sent by the runner) and
// once per attempt event, checks the fencing token and acknowledges only after the commit.
func (c *Channel) event(ctx context.Context, s Session, e protocol.Event) error {
	var ack int64 = -1
	var sig storageSignal
	// KIN-77: secrets are masked before the event is stored, counted or passed to the workflow — outside the
	// transaction, so the single SQLite writer is not held while text is scanned.
	e.Body = redactBody(e.Kind, e.Body)
	err := c.Tx.InTx(ctx, func(ctx context.Context) error {
		sig = storageSignal{}
		st, _, err := c.Repo.Channel(ctx, s.RunnerID)
		if err != nil {
			return err
		}
		cur := protocol.Cursor{Epoch: st.EventEpoch, Last: st.EventLast}
		v, next := cur.Receive(e.Epoch, e.Seq, e.Base)
		if v == protocol.Invalid {
			return fmt.Errorf("%w: event seq", protocol.ErrChannelField)
		}
		if v == protocol.Apply {
			if err := c.applyEvent(ctx, s.RunnerID, e, &sig); err != nil {
				return err
			}
		}
		if next != cur {
			if err := c.Repo.EnsureChannel(ctx, s.RunnerID, c.NewID()); err != nil {
				return err
			}
			if err := c.Repo.SaveEventCursor(ctx, s.RunnerID, next.Epoch, next.Last); err != nil {
				return err
			}
		}
		if v != protocol.Gap {
			ack = next.Last
		}
		return nil
	})
	if errors.Is(err, protocol.ErrChannelField) {
		return err
	}
	if err != nil {
		c.Logger.Warn("runner event not recorded; the runner will send it again", "runner", s.RunnerID, "seq", e.Seq, "err", err)
		return nil
	}
	c.committed(sig)
	if ack > 0 {
		return s.Send(ctx, protocol.TypeEventAck, protocol.Ack{Epoch: e.Epoch, Seq: ack})
	}
	return nil
}

func (c *Channel) applyEvent(ctx context.Context, runnerID string, e protocol.Event, sig *storageSignal) error {
	seen, err := c.Repo.EventSeen(ctx, runnerID, e.Step, e.Fence, e.AttemptSeq)
	if err != nil || seen {
		return err // seen: the same attempt event under another spool epoch
	}
	rec := domain.AttemptEvent{RunnerID: runnerID, Step: e.Step, Fence: e.Fence, AttemptSeq: e.AttemptSeq, Kind: e.Kind,
		Body: e.Body, Outcome: domain.OutcomeApplied, RunnerAt: time.UnixMilli(e.At).UTC(), ReceivedAt: c.Now().UTC()}
	err = c.toWorkflow(ctx, runnerID, e)
	if err == nil && (e.Kind == protocol.EventOutput || e.Kind == protocol.EventOutputDropped) {
		limits := domain.OutputLimits{Attempt: domain.MaxAttemptOutput, Total: c.MaxStoredOutput}
		v, aerr := c.Repo.AddOutput(ctx, e.Step, e.Fence, int64(len(e.Body)), limits, rec.ReceivedAt)
		if aerr != nil {
			return aerr
		}
		if v != domain.OutputStored {
			rec.Body, rec.Reason = nil, v.Reason()
		}
		switch v {
		case domain.OutputOverTotal:
			if err := c.storageFull(ctx, runnerID, e.Step, rec.ReceivedAt, sig); err != nil {
				return err
			}
		case domain.OutputStored:
			if err := c.storageFreed(ctx, rec.ReceivedAt, sig); err != nil {
				return err
			}
		}
	}
	switch {
	case errors.Is(err, domain.ErrStaleAttempt):
		rec.Outcome, rec.Reason = domain.OutcomeRejected, protocol.CancelStaleFence
		// The runner must stop a process whose token was superseded (same transaction: durable with the record).
		if err := c.Send(ctx, runnerID, protocol.CancelCommandID(e.Step, e.Fence), protocol.CommandCancel,
			protocol.Cancel{Step: e.Step, Fence: e.Fence, Reason: protocol.CancelStaleFence}); err != nil {
			return err
		}
		c.Logger.Warn("stale runner event rejected", "runner", runnerID, "step", e.Step, "fence", e.Fence, "kind", e.Kind)
	case errors.Is(err, domain.ErrEventRejected):
		rec.Outcome, rec.Reason = domain.OutcomeRejected, truncate(err.Error(), protocol.MaxReasonLen)
		c.Logger.Warn("runner event rejected", "runner", runnerID, "step", e.Step, "fence", e.Fence, "kind", e.Kind, "err", err)
	case err != nil:
		return err
	}
	if rec.Outcome == domain.OutcomeRejected {
		rec.Body = nil // a rejected event keeps no payload: a runner cannot store data under attempts it does not hold
	}
	return c.Repo.RecordEvent(ctx, rec)
}

// storageSignal is what an event's transaction observed about the output storage; it is applied to the channel's
// state only after the commit, so a rolled-back alert is raised again when the runner re-sends the event.
type storageSignal struct {
	at        time.Time
	runnerID  string
	step      string
	dropped   bool // output was dropped for the total limit
	alerted   bool // ... and alerts.runner_output_full was enqueued
	recovered bool // alerts.runner_output_recovered was enqueued
}

// storageFull alerts that output is being dropped because the output storage is full, at most once a minute.
func (c *Channel) storageFull(ctx context.Context, runnerID, step string, now time.Time, sig *storageSignal) error {
	c.mu.Lock()
	quiet := !c.fullWarn.IsZero() && now.Sub(c.fullWarn) < time.Minute
	c.mu.Unlock()
	sig.at, sig.runnerID, sig.step, sig.dropped = now, runnerID, step, true
	if quiet {
		return nil
	}
	if c.Outbox != nil {
		if err := c.alert(ctx, TopicRunnerOutputFull, "runner-output/full/"+strconv.FormatInt(now.UnixMilli(), 10),
			map[string]any{"limitBytes": c.MaxStoredOutput, "runnerId": runnerID, "stepId": step}); err != nil {
			return err
		}
	}
	sig.alerted = true
	return nil
}

// storageFreed resolves the "output storage full" alert once output is stored again and nothing was dropped for a
// minute: while the storage is nearly full, a small chunk may fit between dropped big ones.
func (c *Channel) storageFreed(ctx context.Context, now time.Time, sig *storageSignal) error {
	if c.Outbox == nil {
		return nil
	}
	c.mu.Lock()
	pending := !c.spaceKnown && (c.lastDrop.IsZero() || now.Sub(c.lastDrop) >= time.Minute)
	c.mu.Unlock()
	if !pending {
		return nil
	}
	if err := c.alert(ctx, TopicRunnerOutputRecovered, "runner-output/recovered/"+strconv.FormatInt(now.UnixMilli(), 10),
		map[string]any{}); err != nil {
		return err
	}
	sig.recovered = true
	return nil
}

// committed applies what a committed event's transaction observed about the output storage.
func (c *Channel) committed(sig storageSignal) {
	c.mu.Lock()
	if sig.dropped {
		c.lastDrop, c.spaceKnown = sig.at, false
	}
	if sig.alerted {
		c.fullWarn = sig.at
	}
	if sig.recovered {
		c.spaceKnown = true
	}
	c.mu.Unlock()
	if sig.alerted {
		c.Logger.Warn("runner output storage full: output of running attempts is dropped until retention frees space",
			"runner", sig.runnerID, "step", sig.step, "limit_bytes", c.MaxStoredOutput)
	}
}

func (c *Channel) alert(ctx context.Context, topic, key string, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := c.Outbox.Enqueue(ctx, messaging.Message{Adapter: AdapterAlerts, Topic: topic, Key: key, Payload: b}); err != nil {
		return fmt.Errorf("enqueueing %s: %w", topic, err)
	}
	return nil
}

func (c *Channel) toWorkflow(ctx context.Context, runnerID string, e protocol.Event) error {
	at := time.UnixMilli(e.At).UTC()
	switch e.Kind {
	case protocol.EventStarted:
		var s protocol.Started
		_ = json.Unmarshal(e.Body, &s) // an unreadable body only loses the agent's name
		return c.Executions.StartAttempt(ctx, runnerID, e.Step, e.Fence, at, s.Agent)
	case protocol.EventCompleted:
		return c.Executions.CompleteAttempt(ctx, runnerID, e.Step, e.Fence, at)
	case protocol.EventFailed:
		var f protocol.Failure
		_ = json.Unmarshal(e.Body, &f)
		return c.Executions.FailAttempt(ctx, runnerID, e.Step, e.Fence, truncate(f.Reason, protocol.MaxReasonLen), at)
	}
	if err := c.Executions.CheckAttempt(ctx, runnerID, e.Step, e.Fence); err != nil || e.Kind != protocol.EventWorkspaceChanges ||
		c.Scope == nil {
		return err
	}
	var ch protocol.WorkspaceChanges
	if err := json.Unmarshal(e.Body, &ch); err != nil {
		// An unreadable report lists no file the check could clear: checked as truncated, it stops the step.
		ch = protocol.WorkspaceChanges{Truncated: true}
	}
	return c.Scope.CheckChanges(ctx, runnerID, e.Step, e.Fence, ch)
}

// redactBody masks secrets in what an event may carry from the attempt's process (KIN-77): the output data (line
// by line, the lines kept) and a failure reason. The runner masks first, with the secret values it holds and a
// window across writes; this is the Control Plane's own best-effort pass for whatever runner sent the event. It is
// stateless per chunk: the runner cuts chunks at line boundaries, so only a line longer than a chunk (or a PEM key
// spread over chunks) is redacted in parts. An output body that does not decode is not kept (it could not be redacted or shown).
func redactBody(kind string, body json.RawMessage) json.RawMessage {
	var v any
	switch kind {
	case protocol.EventOutput:
		var o protocol.Output
		if body == nil || json.Unmarshal(body, &o) != nil {
			return nil
		}
		red := redact.Lines(o.Data)
		if red == o.Data {
			return body
		}
		o.Data = red
		v = o
	case protocol.EventFailed:
		var f protocol.Failure
		if body == nil || json.Unmarshal(body, &f) != nil {
			return nil // nothing that could not be redacted is kept; the attempt fails without a reason
		}
		red := redact.Text(f.Reason)
		if red == f.Reason {
			return body
		}
		f.Reason = red
		v = f
	default:
		return body
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !isRuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
