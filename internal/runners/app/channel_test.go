package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

// memChannelRepo is an in-memory ChannelRepo.
type memChannelRepo struct {
	mu       sync.Mutex
	channels map[string]*domain.Channel
	commands map[string][]domain.QueuedCommand
	events   map[string]domain.AttemptEvent
	output   map[string]int64
	// recordErr fails RecordEvent (the event's transaction rolls back).
	recordErr error
}

func newMemChannelRepo() *memChannelRepo {
	return &memChannelRepo{channels: map[string]*domain.Channel{}, commands: map[string][]domain.QueuedCommand{},
		events: map[string]domain.AttemptEvent{}, output: map[string]int64{}}
}

func evKey(step string, fence, seq int64) string { return fmt.Sprintf("%s/%d/%d", step, fence, seq) }

func (r *memChannelRepo) EnsureChannel(_ context.Context, runnerID, epoch string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.channels[runnerID] == nil {
		r.channels[runnerID] = &domain.Channel{CommandEpoch: epoch}
	}
	return nil
}

func (r *memChannelRepo) Channel(_ context.Context, runnerID string) (domain.Channel, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.channels[runnerID]
	if c == nil {
		return domain.Channel{}, false, nil
	}
	return *c, true, nil
}

func (r *memChannelRepo) AppendCommand(_ context.Context, runnerID string, q domain.QueuedCommand, _ time.Time) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.commands[runnerID] {
		if x.ID == q.ID {
			return x.Seq, nil
		}
	}
	c := r.channels[runnerID]
	c.CommandSeq++
	q.Seq = c.CommandSeq
	r.commands[runnerID] = append(r.commands[runnerID], q)
	return q.Seq, nil
}

func (r *memChannelRepo) CommandsAfter(_ context.Context, runnerID string, after int64, limit int) ([]domain.QueuedCommand, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.QueuedCommand
	for _, q := range r.commands[runnerID] {
		if q.Seq > after && len(out) < limit {
			out = append(out, q)
		}
	}
	return out, nil
}

func (r *memChannelRepo) AckCommands(_ context.Context, runnerID string, seq int64, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.channels[runnerID]; c != nil && seq > c.CommandAcked {
		c.CommandAcked = seq
	}
	return nil
}

func (r *memChannelRepo) SaveEventCursor(_ context.Context, runnerID, epoch string, last int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.channels[runnerID]
	c.EventEpoch, c.EventLast = epoch, last
	return nil
}

func (r *memChannelRepo) EventSeen(_ context.Context, runnerID, step string, fence, seq int64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.events[evKey(step, fence, seq)]
	return ok && e.RunnerID == runnerID, nil
}

func (r *memChannelRepo) RecordEvent(_ context.Context, e domain.AttemptEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recordErr != nil {
		return r.recordErr
	}
	k := evKey(e.Step, e.Fence, e.AttemptSeq)
	if _, ok := r.events[k]; ok {
		return errors.New("duplicate event row")
	}
	r.events[k] = e
	return nil
}

func (r *memChannelRepo) AddOutput(_ context.Context, step string, fence, n int64, l domain.OutputLimits, _ time.Time) (domain.OutputVerdict, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := fmt.Sprintf("%s/%d", step, fence)
	var total int64
	for _, b := range r.output {
		total += b
	}
	v := l.Verdict(r.output[k], total, n)
	if v == domain.OutputStored {
		r.output[k] += n
	}
	return v, nil
}

type passTx struct{}

func (passTx) InTx(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) }

// fakeWorkflow emulates the workflow's fencing: one current token per step and a small state machine.
type fakeWorkflow struct {
	mu      sync.Mutex
	fence   map[string]int64
	state   map[string]string
	calls   []string
	marks   []string
	failing error
}

func newFakeWorkflow() *fakeWorkflow {
	return &fakeWorkflow{fence: map[string]int64{}, state: map[string]string{}}
}

func (w *fakeWorkflow) op(name, step string, fence int64, from, to string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failing != nil {
		return w.failing
	}
	if w.fence[step] != fence {
		return domain.ErrStaleAttempt
	}
	if from != "" && w.state[step] != from {
		return fmt.Errorf("%s from %q: %w", name, w.state[step], domain.ErrEventRejected)
	}
	if to != "" {
		w.state[step] = to
	}
	w.calls = append(w.calls, name)
	return nil
}

func (w *fakeWorkflow) CheckAttempt(_ context.Context, _, step string, fence int64) error {
	return w.op("check", step, fence, "", "")
}
func (w *fakeWorkflow) StartAttempt(_ context.Context, _, step string, fence int64, at time.Time, agent string) error {
	w.mark("start", at, agent)
	return w.op("start", step, fence, "claimed", "running")
}
func (w *fakeWorkflow) CompleteAttempt(_ context.Context, _, step string, fence int64, at time.Time) error {
	w.mark("complete", at, "")
	return w.op("complete", step, fence, "running", "done")
}
func (w *fakeWorkflow) FailAttempt(_ context.Context, _, step string, fence int64, _ string, at time.Time) error {
	w.mark("fail", at, "")
	return w.op("fail", step, fence, "running", "failed")
}

// mark records the event time (and agent) the workflow got.
func (w *fakeWorkflow) mark(name string, at time.Time, agent string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.marks = append(w.marks, fmt.Sprintf("%s %d %s", name, at.UnixMilli(), agent))
}

func (w *fakeWorkflow) history() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.calls)
}

type sent struct {
	typ     string
	payload any
}

// fakeSession records what the Control Plane sends to the runner.
type fakeSession struct {
	out chan sent
}

func newSession(runnerID string) (Session, *fakeSession) {
	f := &fakeSession{out: make(chan sent, 4096)}
	return Session{ID: "sess", RunnerID: runnerID, Detach: func() {}, Send: func(_ context.Context, typ string, p any) error {
		f.out <- sent{typ, p}
		return nil
	}}, f
}

func (f *fakeSession) next(t *testing.T) sent {
	t.Helper()
	select {
	case m := <-f.out:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("nothing sent")
		return sent{}
	}
}

func (f *fakeSession) quiet(t *testing.T) {
	t.Helper()
	select {
	case m := <-f.out:
		t.Fatalf("unexpected %s %+v", m.typ, m.payload)
	case <-time.After(30 * time.Millisecond):
	}
}

func (f *fakeSession) drain() []sent {
	var out []sent
	for {
		select {
		case m := <-f.out:
			out = append(out, m)
		default:
			return out
		}
	}
}

type channelEnv struct {
	repo *memChannelRepo
	wf   *fakeWorkflow
	ch   *Channel
}

func newChannelEnv() *channelEnv {
	e := &channelEnv{repo: newMemChannelRepo(), wf: newFakeWorkflow()}
	n := 0
	var mu sync.Mutex
	e.ch = &Channel{Repo: e.repo, Executions: e.wf, Tx: passTx{}, Now: func() time.Time { return time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC) },
		NewID: func() string { mu.Lock(); defer mu.Unlock(); n++; return fmt.Sprintf("id%d", n) }, AckTimeout: time.Hour}
	return e
}

func (e *channelEnv) commands(runnerID string) []domain.QueuedCommand {
	e.repo.mu.Lock()
	defer e.repo.mu.Unlock()
	return slices.Clone(e.repo.commands[runnerID])
}

func handle(t *testing.T, ch *Channel, s Session, typ string, payload any) error {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return ch.Handle(t.Context(), s, protocol.Envelope{Type: typ, Payload: b})
}

func runServe(t *testing.T, ch *Channel, s Session) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); ch.Serve(ctx, s) }()
	stop = func() { cancel(); <-done }
	t.Cleanup(stop)
	return stop
}

func commandsSent(t *testing.T, f *fakeSession, n int) []protocol.Command {
	t.Helper()
	var out []protocol.Command
	for len(out) < n {
		m := f.next(t)
		if m.typ == protocol.TypeReconcile {
			continue
		}
		if m.typ != protocol.TypeCommand {
			t.Fatalf("sent %s", m.typ)
		}
		out = append(out, m.payload.(protocol.Command))
	}
	return out
}

// Commands come from the durable (transactional) command log, get a message id and a monotonic seq, and are re-sent until ACKed:
// after a reconnect from the last ACK, and after the ACK timeout.
func TestCommandsSequencedAndResentUntilAcked(t *testing.T) {
	t.Parallel()
	e := newChannelEnv()
	ctx := t.Context()
	for i := range 3 {
		if err := e.ch.Send(ctx, "r1", fmt.Sprintf("c%d", i), protocol.CommandCancel, protocol.Cancel{Step: "s", Fence: 1, Reason: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	_ = e.ch.Send(ctx, "r1", "c0", protocol.CommandCancel, protocol.Cancel{}) // the same use case again: no new command
	if cmds := e.commands("r1"); len(cmds) != 3 || cmds[0].ID != "c0" || cmds[0].Seq != 1 || cmds[2].Seq != 3 {
		t.Fatalf("command log %+v", cmds)
	}
	sess, f := newSession("r1")
	stop := runServe(t, e.ch, sess)
	if m := f.next(t); m.typ != protocol.TypeReconcile || m.payload.(protocol.Reconcile).RequestID == "" {
		t.Fatalf("first message %s %+v, want a reconcile request", m.typ, m.payload)
	}
	cmds := commandsSent(t, f, 3)
	for i, c := range cmds {
		if c.Seq != int64(i+1) || c.ID != fmt.Sprintf("c%d", i) || c.Kind != protocol.CommandCancel || c.Base != 0 || c.Epoch == "" {
			t.Fatalf("command %d = %+v", i, c)
		}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	f.quiet(t)
	if err := handle(t, e.ch, sess, protocol.TypeCommandAck, protocol.Ack{Epoch: cmds[0].Epoch, Seq: 1}); err != nil {
		t.Fatal(err)
	}
	stop() // connection lost

	sess2, f2 := newSession("r1")
	runServe(t, e.ch, sess2)
	again := commandsSent(t, f2, 2)
	if again[0].Seq != 2 || again[1].Seq != 3 || again[0].Base != 1 {
		t.Fatalf("after reconnect %+v", again)
	}
	// A new command reaches the live session at once.
	_ = e.ch.Send(ctx, "r1", "c9", protocol.CommandCancel, protocol.Cancel{})
	if c := commandsSent(t, f2, 1)[0]; c.Seq != 4 || c.ID != "c9" {
		t.Fatalf("live command %+v", c)
	}
	// An ACK for a command never sent is a protocol error; an ACK of another epoch is ignored.
	if err := handle(t, e.ch, sess2, protocol.TypeCommandAck, protocol.Ack{Epoch: cmds[0].Epoch, Seq: 99}); !errors.Is(err, protocol.ErrChannelField) {
		t.Fatalf("ack beyond the log: %v", err)
	}
	if err := handle(t, e.ch, sess2, protocol.TypeCommandAck, protocol.Ack{Epoch: "old", Seq: 4}); err != nil {
		t.Fatal(err)
	}
}

func TestCommandsResentAfterAckTimeout(t *testing.T) {
	t.Parallel()
	e := newChannelEnv()
	e.ch.AckTimeout, e.ch.PollInterval, e.ch.Now = 20*time.Millisecond, 5*time.Millisecond, time.Now
	_ = e.ch.Send(t.Context(), "r1", "c1", protocol.CommandCancel, protocol.Cancel{})
	sess, f := newSession("r1")
	runServe(t, e.ch, sess)
	if a, b := commandsSent(t, f, 1)[0], commandsSent(t, f, 1)[0]; a.Seq != 1 || b.Seq != 1 {
		t.Fatalf("sent %d then %d", a.Seq, b.Seq)
	}
}

func TestCommandWindow(t *testing.T) {
	t.Parallel()
	e := newChannelEnv()
	e.ch.Window = 3
	for i := range 6 {
		_ = e.ch.Send(t.Context(), "r1", fmt.Sprintf("c%d", i), protocol.CommandCancel, protocol.Cancel{})
	}
	sess, f := newSession("r1")
	runServe(t, e.ch, sess)
	cmds := commandsSent(t, f, 3)
	f.quiet(t)
	_ = handle(t, e.ch, sess, protocol.TypeCommandAck, protocol.Ack{Epoch: cmds[0].Epoch, Seq: 2})
	if more := commandsSent(t, f, 2); more[0].Seq != 4 || more[1].Seq != 5 {
		t.Fatalf("after ack %+v", more)
	}
	f.quiet(t)
}

func TestSendRejectsInvalidCommands(t *testing.T) {
	t.Parallel()
	e := newChannelEnv()
	for _, kind := range []string{"", "rm -rf", "Exec"} {
		if err := e.ch.Send(t.Context(), "r1", "c", kind, nil); !errors.Is(err, protocol.ErrChannelField) {
			t.Errorf("kind %q: %v", kind, err)
		}
	}
	if err := e.ch.Send(t.Context(), "r1", "c", protocol.CommandAssign, string(make([]byte, protocol.MaxBodyBytes))); err == nil {
		t.Error("oversized command accepted")
	}
}

// attemptEvents is the event stream of one attempt as the runner spools it.
func attemptEvents(epoch, step string, fence int64) []protocol.Event {
	kinds := []string{protocol.EventAccepted, protocol.EventStarted, protocol.EventOutput, protocol.EventOutput,
		protocol.EventOutput, protocol.EventCompleted}
	out := make([]protocol.Event, 0, len(kinds))
	for i, k := range kinds {
		var body json.RawMessage
		if k == protocol.EventOutput {
			body, _ = json.Marshal(protocol.Output{Stream: "stdout", Offset: int64(i), Data: "x"})
		}
		out = append(out, protocol.Event{Epoch: epoch, Seq: int64(i + 1), Step: step, Fence: fence,
			AttemptSeq: int64(i + 1), Kind: k, At: 1, Body: body})
	}
	return out
}

func acks(f *fakeSession) []int64 {
	var out []int64
	for _, m := range f.drain() {
		if m.typ == protocol.TypeEventAck {
			out = append(out, m.payload.(protocol.Ack).Seq)
		}
	}
	return out
}

// Duplicated and out-of-order events are harmless: every event is applied once, in order (fake transport with
// drop, dup and reorder; the simulated runner re-sends from the last ACK as the real one does).
func TestEventsAppliedOnceDespiteDropDupReorder(t *testing.T) {
	t.Parallel()
	for seed := range uint64(50) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()
			e := newChannelEnv()
			e.wf.fence["s1"], e.wf.state["s1"] = 7, "claimed"
			sess, f := newSession("r1")
			evs := attemptEvents("sp", "s1", 7)
			rng := rand.New(rand.NewPCG(seed, 1))
			var acked int64
			for round := 0; acked < int64(len(evs)); round++ {
				if round > 500 {
					t.Fatalf("no progress, acked %d", acked)
				}
				// The runner sends everything after its ACK; the transport drops, duplicates and swaps messages.
				var wire []protocol.Event
				for _, ev := range evs[acked:] {
					ev.Base = acked
					switch r := rng.Float64(); {
					case r < 0.25: // dropped
					case r < 0.45:
						wire = append(wire, ev, ev)
					default:
						wire = append(wire, ev)
					}
				}
				for i := range wire {
					if j := i + 1; j < len(wire) && rng.Float64() < 0.3 {
						wire[i], wire[j] = wire[j], wire[i]
					}
				}
				for _, ev := range wire {
					if err := handle(t, e.ch, sess, protocol.TypeEvent, ev); err != nil {
						t.Fatal(err)
					}
				}
				for _, a := range acks(f) {
					if rng.Float64() < 0.2 {
						continue // ACK lost
					}
					acked = max(acked, a)
				}
			}
			if h := e.wf.history(); !slices.Equal(h, []string{"check", "start", "check", "check", "check", "complete"}) {
				t.Fatalf("workflow saw %v", h)
			}
			if len(e.repo.events) != len(evs) {
				t.Fatalf("recorded %d events", len(e.repo.events))
			}
			for _, r := range e.repo.events {
				if r.Outcome != domain.OutcomeApplied || r.RunnerID != "r1" {
					t.Fatalf("event %+v", r)
				}
			}
		})
	}
}

// A runner whose spool was recreated starts a new epoch; events already recorded under the old epoch (same attempt
// event) are not applied twice.
func TestNewSpoolEpochAndAttemptDedup(t *testing.T) {
	t.Parallel()
	e := newChannelEnv()
	e.wf.fence["s1"], e.wf.state["s1"] = 7, "claimed"
	sess, f := newSession("r1")
	evs := attemptEvents("old", "s1", 7)[:2]
	for _, ev := range evs {
		_ = handle(t, e.ch, sess, protocol.TypeEvent, ev)
	}
	for _, ev := range attemptEvents("new", "s1", 7)[:3] { // the same attempt events, re-numbered by a new spool
		if err := handle(t, e.ch, sess, protocol.TypeEvent, ev); err != nil {
			t.Fatal(err)
		}
	}
	if h := e.wf.history(); !slices.Equal(h, []string{"check", "start", "check"}) {
		t.Fatalf("workflow saw %v", h)
	}
	if a := acks(f); !slices.Equal(a, []int64{1, 2, 1, 2, 3}) {
		t.Fatalf("acks %v", a)
	}
}

// A stale runner can never overwrite state: an event with a superseded token is rejected (recorded, acknowledged
// so the runner does not retry it) and the runner is told to cancel that attempt.
func TestStaleFenceRejectedAndCancelled(t *testing.T) {
	t.Parallel()
	e := newChannelEnv()
	e.wf.fence["s1"], e.wf.state["s1"] = 8, "running" // a newer claim exists
	sess, f := newSession("r1")
	ev := attemptEvents("sp", "s1", 7)[5] // execution.completed of the old attempt
	ev.Seq, ev.Base = 1, 0
	if err := handle(t, e.ch, sess, protocol.TypeEvent, ev); err != nil {
		t.Fatal(err)
	}
	if h := e.wf.history(); len(h) != 0 || e.wf.state["s1"] != "running" {
		t.Fatalf("stale event changed the workflow: %v %s", h, e.wf.state["s1"])
	}
	if a := acks(f); !slices.Equal(a, []int64{1}) {
		t.Fatalf("acks %v", a)
	}
	rec := e.repo.events[evKey("s1", 7, 6)]
	if rec.Outcome != domain.OutcomeRejected || rec.Reason != protocol.CancelStaleFence || rec.Body != nil {
		t.Fatalf("record %+v", rec)
	}
	cmds := e.commands("r1")
	if len(cmds) != 1 || cmds[0].Kind != protocol.CommandCancel {
		t.Fatalf("commands %+v", cmds)
	}
	c := cmds[0]
	var body protocol.Cancel
	if json.Unmarshal(c.Body, &body) != nil || body != (protocol.Cancel{Step: "s1", Fence: 7, Reason: protocol.CancelStaleFence}) {
		t.Fatalf("cancel %s", c.Body)
	}
	// A result that does not fit the step's state is rejected, acknowledged, and nothing is cancelled.
	e.wf.fence["s2"], e.wf.state["s2"] = 1, "claimed"
	done := protocol.Event{Epoch: "sp", Seq: 2, Base: 1, Step: "s2", Fence: 1, AttemptSeq: 1, Kind: protocol.EventCompleted, At: 1}
	_ = handle(t, e.ch, sess, protocol.TypeEvent, done)
	if rec := e.repo.events[evKey("s2", 1, 1)]; rec.Outcome != domain.OutcomeRejected || len(e.commands("r1")) != 1 {
		t.Fatalf("record %+v", rec)
	}
}

// A transient failure (database busy, conflict) leaves the event unacknowledged and unrecorded; the re-send applies it.
func TestTransientFailureIsNotAcked(t *testing.T) {
	t.Parallel()
	e := newChannelEnv()
	e.wf.fence["s1"], e.wf.state["s1"] = 1, "claimed"
	e.wf.failing = errors.New("database is locked")
	sess, f := newSession("r1")
	ev := attemptEvents("sp", "s1", 1)[1] // started
	ev.Seq, ev.AttemptSeq = 1, 1
	if err := handle(t, e.ch, sess, protocol.TypeEvent, ev); err != nil {
		t.Fatal(err)
	}
	if a := acks(f); len(a) != 0 || len(e.repo.events) != 0 {
		t.Fatalf("acks %v events %d", a, len(e.repo.events))
	}
	e.wf.failing = nil
	_ = handle(t, e.ch, sess, protocol.TypeEvent, ev)
	if a := acks(f); !slices.Equal(a, []int64{1}) || e.wf.state["s1"] != "running" {
		t.Fatalf("acks %v state %s", a, e.wf.state["s1"])
	}
}

func TestInvalidEventsAreProtocolErrors(t *testing.T) {
	t.Parallel()
	e := newChannelEnv()
	sess, _ := newSession("r1")
	for name, env := range map[string]protocol.Envelope{
		"unknown field": {Type: protocol.TypeEvent, Payload: json.RawMessage(`{"epoch":"e","seq":1,"x":1}`)},
		"zero fence":    {Type: protocol.TypeEvent, Payload: json.RawMessage(`{"epoch":"e","seq":1,"step":"s","fence":0,"attemptSeq":1,"kind":"execution.started","at":1}`)},
		"base ≥ seq":    {Type: protocol.TypeEvent, Payload: json.RawMessage(`{"epoch":"e","seq":1,"base":1,"step":"s","fence":1,"attemptSeq":1,"kind":"execution.started","at":1}`)},
		"bad ack":       {Type: protocol.TypeCommandAck, Payload: json.RawMessage(`"x"`)},
	} {
		if err := e.ch.Handle(t.Context(), sess, env); !errors.Is(err, protocol.ErrChannelField) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Output beyond MaxAttemptOutput is acknowledged but not stored: a runner cannot fill the Control Plane's disk.
func TestAttemptOutputIsCapped(t *testing.T) {
	t.Parallel()
	e := newChannelEnv()
	e.wf.fence["s1"], e.wf.state["s1"] = 1, "running"
	e.repo.output["s1/1"] = domain.MaxAttemptOutput - 10
	sess, f := newSession("r1")
	body, _ := json.Marshal(protocol.Output{Stream: "stdout", Data: "more than ten bytes"})
	ev := protocol.Event{Epoch: "sp", Seq: 1, Step: "s1", Fence: 1, AttemptSeq: 1, Kind: protocol.EventOutput, At: 1, Body: body}
	_ = handle(t, e.ch, sess, protocol.TypeEvent, ev)
	rec := e.repo.events[evKey("s1", 1, 1)]
	if rec.Body != nil || rec.Reason != "output_limit" || !slices.Equal(acks(f), []int64{1}) {
		t.Fatalf("record %+v", rec)
	}
}

// Output beyond the Control Plane-wide limit is acknowledged, recorded without its body and logged.
func TestOutputOverStorageLimitIsDropped(t *testing.T) {
	t.Parallel()
	e := newChannelEnv()
	var logs bytes.Buffer
	e.ch.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	e.ch.MaxStoredOutput = domain.MaxAttemptOutput
	e.wf.fence["s1"], e.wf.state["s1"] = 1, "running"
	e.repo.output["old/1"] = domain.MaxAttemptOutput - 5
	sess, f := newSession("r1")
	body, _ := json.Marshal(protocol.Output{Stream: "stdout", Data: "more than five bytes"})
	for i := int64(1); i <= 2; i++ {
		ev := protocol.Event{Epoch: "sp", Seq: i, Step: "s1", Fence: 1, AttemptSeq: i, Kind: protocol.EventOutput, At: 1, Body: body}
		_ = handle(t, e.ch, sess, protocol.TypeEvent, ev)
	}
	rec := e.repo.events[evKey("s1", 1, 1)]
	if rec.Body != nil || rec.Reason != "output_storage_limit" || rec.Outcome != domain.OutcomeApplied || !slices.Equal(acks(f), []int64{1, 2}) {
		t.Fatalf("record %+v, acks %v", rec, acks(f))
	}
	if n := strings.Count(logs.String(), "output storage full"); n != 1 {
		t.Fatalf("logged %d warnings (throttled to one):\n%s", n, logs.String())
	}
}

type recReconciler struct {
	mu  sync.Mutex
	got []protocol.ReconcileResponse
}

func (r *recReconciler) Reconciled(_ context.Context, runnerID string, resp protocol.ReconcileResponse) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if runnerID == "r1" {
		r.got = append(r.got, resp)
	}
	return nil
}

func TestReconcileResponseReachesReconciler(t *testing.T) {
	t.Parallel()
	e := newChannelEnv()
	rec := &recReconciler{}
	e.ch.Reconciler = rec
	sess, _ := newSession("r1")
	resp := protocol.ReconcileResponse{RequestID: "rq", Attempts: []protocol.AttemptStatus{{Step: "s", Fence: 2, PID: 10,
		PGID: 10, Running: true}}, Spool: protocol.SpoolOffsets{Epoch: "sp", Acked: 3, Last: 5}}
	if err := handle(t, e.ch, sess, protocol.TypeReconcileResponse, resp); err != nil {
		t.Fatal(err)
	}
	if len(rec.got) != 1 || rec.got[0].Attempts[0].PID != 10 || rec.got[0].Spool.Last != 5 {
		t.Fatalf("reconciler got %+v", rec.got)
	}
}

// Prepare transforms a command right before it is sent (KIN-30 opens sealed proxy passwords there): the log keeps
// the stored body; a failing Prepare sends the stored body rather than blocking the stream.
func TestCommandsPreparedBeforeSending(t *testing.T) {
	t.Parallel()
	e := newChannelEnv()
	e.ch.Prepare = func(_ context.Context, runnerID string, q domain.QueuedCommand) (json.RawMessage, error) {
		if q.ID == "bad" {
			return nil, errors.New("cannot prepare")
		}
		return json.RawMessage(`{"for":"` + runnerID + `/` + q.ID + `"}`), nil
	}
	_ = e.ch.Send(t.Context(), "r1", "c1", protocol.CommandCancel, protocol.Cancel{Reason: "stored"})
	_ = e.ch.Send(t.Context(), "r1", "bad", protocol.CommandCancel, protocol.Cancel{Reason: "stored"})
	sess, f := newSession("r1")
	runServe(t, e.ch, sess)
	cmds := commandsSent(t, f, 2)
	if string(cmds[0].Body) != `{"for":"r1/c1"}` || !strings.Contains(string(cmds[1].Body), "stored") {
		t.Fatalf("sent %s, %s", cmds[0].Body, cmds[1].Body)
	}
	if !strings.Contains(string(e.commands("r1")[0].Body), "stored") {
		t.Fatal("the command log was changed")
	}
}

// KIN-59: dropping output for the total limit raises a Control Plane alert through the outbox (at most once a
// minute, grouped by the Alert Center); the first output stored afterwards resolves it.
func TestOutputStorageFullAlertsAndRecovers(t *testing.T) {
	t.Parallel()
	e := newChannelEnv()
	ob := &memOutbox{}
	e.ch.Outbox = ob
	e.ch.MaxStoredOutput = domain.MaxAttemptOutput
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	e.ch.Now = func() time.Time { return now }
	e.wf.fence["s1"], e.wf.state["s1"] = 1, "running"
	sess, _ := newSession("r1")
	seq := int64(0)
	output := func(data string) {
		t.Helper()
		seq++
		body, _ := json.Marshal(protocol.Output{Stream: "stdout", Data: data})
		ev := protocol.Event{Epoch: "sp", Seq: seq, Step: "s1", Fence: 1, AttemptSeq: seq, Kind: protocol.EventOutput, At: 1, Body: body}
		if err := handle(t, e.ch, sess, protocol.TypeEvent, ev); err != nil {
			t.Fatal(err)
		}
	}
	e.repo.output["old/1"] = domain.MaxAttemptOutput - 5
	output("more than five bytes")
	output("more than five bytes") // same minute: grouped, not enqueued again
	now = now.Add(time.Minute)
	output("more than five bytes")
	if got := ob.topics(); !slices.Equal(got, []string{TopicRunnerOutputFull, TopicRunnerOutputFull}) {
		t.Fatalf("topics while full = %v", got)
	}
	var p struct {
		LimitBytes int64  `json:"limitBytes"`
		RunnerID   string `json:"runnerId"`
		StepID     string `json:"stepId"`
	}
	if err := json.Unmarshal(ob.msgs[0].Payload, &p); err != nil || p.LimitBytes != domain.MaxAttemptOutput || p.RunnerID != "r1" ||
		p.StepID != "s1" || ob.msgs[0].Adapter != AdapterAlerts {
		t.Fatalf("payload %s %+v %v", ob.msgs[0].Payload, p, err)
	}
	e.repo.mu.Lock()
	delete(e.repo.output, "old/1") // the retention evicted the oldest finished output
	e.repo.mu.Unlock()
	now = now.Add(time.Minute) // a minute without drops
	output("fits")
	output("fits again")
	if got := ob.topics(); !slices.Equal(got, []string{TopicRunnerOutputFull, TopicRunnerOutputFull, TopicRunnerOutputRecovered}) {
		t.Fatalf("topics after space was freed = %v", got)
	}
}

// Review of KIN-59: the alert state changes only after the event's transaction commits, so an alert rolled back
// with its event is raised again when the runner re-sends the event.
func TestOutputStorageAlertSurvivesRollback(t *testing.T) {
	t.Parallel()
	e := newChannelEnv()
	ob := &memOutbox{}
	e.ch.Outbox = ob
	e.ch.MaxStoredOutput = domain.MaxAttemptOutput
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	e.ch.Now = func() time.Time { return now }
	e.wf.fence["s1"], e.wf.state["s1"] = 1, "running"
	e.repo.output["old/1"] = domain.MaxAttemptOutput - 5
	sess, f := newSession("r1")
	body, _ := json.Marshal(protocol.Output{Stream: "stdout", Data: "more than five bytes"})
	ev := protocol.Event{Epoch: "sp", Seq: 1, Step: "s1", Fence: 1, AttemptSeq: 1, Kind: protocol.EventOutput, At: 1, Body: body}
	e.repo.recordErr = errors.New("database is locked")
	_ = handle(t, e.ch, sess, protocol.TypeEvent, ev)
	ob.msgs = nil // rolled back with the event
	e.repo.recordErr = nil
	now = now.Add(time.Second)
	_ = handle(t, e.ch, sess, protocol.TypeEvent, ev)
	if got := ob.topics(); !slices.Equal(got, []string{TopicRunnerOutputFull}) || !slices.Equal(acks(f), []int64{1}) {
		t.Fatalf("topics after the re-sent event = %v, acks %v", got, acks(f))
	}
}

// Review of KIN-59: while the storage is nearly full, a small chunk that still fits does not resolve the alert;
// output is stored again only after a minute without drops.
func TestOutputStorageFlappingDoesNotResolve(t *testing.T) {
	t.Parallel()
	e := newChannelEnv()
	ob := &memOutbox{}
	e.ch.Outbox = ob
	e.ch.MaxStoredOutput = domain.MaxAttemptOutput
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	e.ch.Now = func() time.Time { return now }
	e.wf.fence["s1"], e.wf.state["s1"] = 1, "running"
	e.repo.output["old/1"] = domain.MaxAttemptOutput - 200
	sess, _ := newSession("r1")
	seq := int64(0)
	output := func(n int) {
		t.Helper()
		seq++
		body, _ := json.Marshal(protocol.Output{Stream: "stdout", Data: strings.Repeat("x", n)})
		ev := protocol.Event{Epoch: "sp", Seq: seq, Step: "s1", Fence: 1, AttemptSeq: seq, Kind: protocol.EventOutput, At: 1, Body: body}
		if err := handle(t, e.ch, sess, protocol.TypeEvent, ev); err != nil {
			t.Fatal(err)
		}
	}
	output(500) // dropped
	now = now.Add(10 * time.Second)
	output(1) // fits, but the storage is still full
	now = now.Add(10 * time.Second)
	output(500) // dropped again
	if got := ob.topics(); !slices.Equal(got, []string{TopicRunnerOutputFull}) {
		t.Fatalf("topics while flapping = %v", got)
	}
	now = now.Add(time.Minute)
	output(1)
	if got := ob.topics(); !slices.Equal(got, []string{TopicRunnerOutputFull, TopicRunnerOutputRecovered}) {
		t.Fatalf("topics after a quiet minute = %v", got)
	}
}

// The workflow gets each lifecycle event's time on the runner's clock and the agent from execution.started, so it
// can account the attempt's agent time (KIN-25).
func TestLifecycleEventsCarryTheRunnerTimeAndAgent(t *testing.T) {
	t.Parallel()
	e := newChannelEnv()
	e.wf.fence["s1"], e.wf.state["s1"] = 3, "claimed"
	sess, _ := newSession("r1")
	started, _ := json.Marshal(protocol.Started{Agent: "mock"})
	evs := []protocol.Event{
		{Epoch: "sp", Seq: 1, Step: "s1", Fence: 3, AttemptSeq: 1, Kind: protocol.EventStarted, At: 1_000, Body: started},
		{Epoch: "sp", Seq: 2, Base: 1, Step: "s1", Fence: 3, AttemptSeq: 2, Kind: protocol.EventCompleted, At: 41_000},
	}
	for _, ev := range evs {
		if err := handle(t, e.ch, sess, protocol.TypeEvent, ev); err != nil {
			t.Fatal(err)
		}
	}
	e.wf.mu.Lock()
	defer e.wf.mu.Unlock()
	if !slices.Equal(e.wf.marks, []string{"start 1000 mock", "complete 41000 "}) {
		t.Fatalf("marks %q", e.wf.marks)
	}
}
