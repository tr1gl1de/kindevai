package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tr1gl1de/kindevai/internal/protocol"
)

// memSpool is an in-memory Spool with a byte limit on output.
type memSpool struct {
	mu     sync.Mutex
	epoch  string
	events []SpooledEvent
	acked  int64
	last   int64
	limit  int
}

func (s *memSpool) Append(e SpooledEvent) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.limit > 0 && e.Kind == protocol.EventOutput && s.size()+len(e.Body) > s.limit {
		return 0, ErrSpoolFull
	}
	s.last++
	e.Seq = s.last
	s.events = append(s.events, e)
	return e.Seq, nil
}

func (s *memSpool) size() int {
	n := 0
	for _, e := range s.events {
		n += len(e.Body)
	}
	return n
}

func (s *memSpool) Read(after int64, n, maxBytes int) ([]SpooledEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []SpooledEvent
	for _, e := range s.events {
		if e.Seq > after && len(out) < n {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *memSpool) Ack(seq int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq > s.acked {
		s.acked = min(seq, s.last)
	}
	s.events = slices.DeleteFunc(s.events, func(e SpooledEvent) bool { return e.Seq <= s.acked })
	return nil
}

func (s *memSpool) State() SpoolState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SpoolState{Epoch: s.epoch, Acked: s.acked, Last: s.last, Bytes: int64(s.size()), LimitBytes: int64(s.limit)}
}

func (s *memSpool) Pending() []PendingResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []PendingResult
	for _, e := range s.events {
		if protocol.TerminalEvent(e.Kind) {
			out = append(out, PendingResult{Step: e.Step, Fence: e.Fence, Kind: e.Kind, Seq: e.Seq})
		}
	}
	return out
}

type memCursor struct {
	mu  sync.Mutex
	cur protocol.Cursor
}

func (m *memCursor) Load() (protocol.Cursor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cur, nil
}
func (m *memCursor) Save(c protocol.Cursor) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cur = c
	return nil
}

// sentMsg is a message the runner sent.
type sentMsg struct {
	typ     string
	payload any
}

type fakeConn struct {
	out chan sentMsg
}

func newConn() *fakeConn { return &fakeConn{out: make(chan sentMsg, 1024)} }

func (c *fakeConn) Wait(ctx context.Context) error                            { <-ctx.Done(); return nil }
func (c *fakeConn) SetHandler(func(context.Context, protocol.Envelope) error) {}
func (c *fakeConn) Send(_ context.Context, typ string, payload any) error {
	c.out <- sentMsg{typ, payload}
	return nil
}

func (c *fakeConn) next(t *testing.T) sentMsg {
	t.Helper()
	select {
	case m := <-c.out:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("nothing sent")
		return sentMsg{}
	}
}

func (c *fakeConn) quiet(t *testing.T) {
	t.Helper()
	select {
	case m := <-c.out:
		t.Fatalf("unexpected %s %+v", m.typ, m.payload)
	case <-time.After(30 * time.Millisecond):
	}
}

func env(t *testing.T, typ string, payload any) protocol.Envelope {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Envelope{Type: typ, Payload: b}
}

func cmd(epoch string, seq, base int64) protocol.Command {
	return protocol.Command{Epoch: epoch, Seq: seq, Base: base, ID: "cmd-" + string(rune('a'+seq)), Kind: protocol.CommandCancel,
		Body: json.RawMessage(`{"step":"s","fence":1,"reason":"x"}`)}
}

type applied struct {
	mu  sync.Mutex
	ids []int64
	err error
}

func (a *applied) fn(_ context.Context, c protocol.Command) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	a.ids = append(a.ids, c.Seq)
	return nil
}

func (a *applied) seqs() []int64 { a.mu.Lock(); defer a.mu.Unlock(); return slices.Clone(a.ids) }

// Commands arrive dropped, duplicated and reordered; each is applied exactly once, in order, and ACKed cumulatively.
func TestCommandsAppliedOnceInOrderDespiteDropDupReorder(t *testing.T) {
	t.Parallel()
	var got applied
	cur := &memCursor{}
	ch := &Channel{Spool: &memSpool{epoch: "sp"}, Cursor: cur, Commands: got.fn}
	conn := newConn()
	ctx := t.Context()
	// The CP's view: base is what it saw ACKed. Sequence on the wire: 1, 3 (2 lost), 1 (dup), 2 (resend), 3, 3.
	wire := []protocol.Command{cmd("e1", 1, 0), cmd("e1", 3, 1), cmd("e1", 1, 0), cmd("e1", 2, 1), cmd("e1", 3, 1),
		cmd("e1", 3, 2)}
	var acks []int64
	for _, c := range wire {
		ok, err := ch.Handle(ctx, conn, env(t, protocol.TypeCommand, c))
		if !ok || err != nil {
			t.Fatalf("Handle(%d) = %v, %v", c.Seq, ok, err)
		}
		select {
		case m := <-conn.out:
			acks = append(acks, m.payload.(protocol.Ack).Seq)
		default:
		}
	}
	if s := got.seqs(); !slices.Equal(s, []int64{1, 2, 3}) {
		t.Fatalf("applied %v, want 1 2 3 once each", s)
	}
	if !slices.Equal(acks, []int64{1, 1, 2, 3, 3}) {
		t.Fatalf("acks %v", acks)
	}
	if c, _ := cur.Load(); c != (protocol.Cursor{Epoch: "e1", Last: 3}) {
		t.Fatalf("cursor %+v", c)
	}
	// A restarted runner keeps its position: the cursor is persisted, a re-sent command is not applied again.
	ch2 := &Channel{Spool: &memSpool{epoch: "sp"}, Cursor: cur, Commands: got.fn}
	if _, err := ch2.Handle(ctx, conn, env(t, protocol.TypeCommand, cmd("e1", 3, 2))); err != nil {
		t.Fatal(err)
	}
	if s := got.seqs(); len(s) != 3 {
		t.Fatalf("re-applied after restart: %v", s)
	}
}

func TestCommandHandlerErrorLeavesCommandUnacked(t *testing.T) {
	t.Parallel()
	got := applied{err: errors.New("busy")}
	ch := &Channel{Spool: &memSpool{epoch: "sp"}, Cursor: &memCursor{}, Commands: got.fn}
	conn := newConn()
	if _, err := ch.Handle(t.Context(), conn, env(t, protocol.TypeCommand, cmd("e1", 1, 0))); err != nil {
		t.Fatal(err)
	}
	conn.quiet(t)
	got.mu.Lock()
	got.err = nil
	got.mu.Unlock()
	_, _ = ch.Handle(t.Context(), conn, env(t, protocol.TypeCommand, cmd("e1", 1, 0)))
	if m := conn.next(t); m.payload.(protocol.Ack).Seq != 1 {
		t.Fatalf("ack %+v", m.payload)
	}
}

func TestInvalidCommandIsAProtocolError(t *testing.T) {
	t.Parallel()
	ch := &Channel{Spool: &memSpool{epoch: "sp"}, Cursor: &memCursor{}, Commands: (&applied{}).fn}
	for name, e := range map[string]protocol.Envelope{
		"unknown field": {Type: protocol.TypeCommand, Payload: json.RawMessage(`{"epoch":"e","seq":1,"id":"a","kind":"a.b","x":1}`)},
		"zero seq":      env(t, protocol.TypeCommand, cmd("e1", 0, 0)),
		"bad ack":       {Type: protocol.TypeEventAck, Payload: json.RawMessage(`[]`)},
	} {
		if _, err := ch.Handle(t.Context(), newConn(), e); !errors.Is(err, protocol.ErrChannelField) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if ok, _ := ch.Handle(t.Context(), newConn(), protocol.Envelope{Type: "runner.heartbeat.ack"}); ok {
		t.Error("took a message type that is not the channel's")
	}
}

func eventSeqs(t *testing.T, conn *fakeConn, n int) []protocol.Event {
	t.Helper()
	var out []protocol.Event
	for range n {
		m := conn.next(t)
		if m.typ != protocol.TypeEvent {
			t.Fatalf("sent %s", m.typ)
		}
		out = append(out, m.payload.(protocol.Event))
	}
	return out
}

func serve(t *testing.T, ch *Channel, conn Conn) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = ch.Serve(ctx, conn) }()
	stop = func() { cancel(); <-done }
	t.Cleanup(stop)
	return stop
}

// Events are sent in spool order with the base the runner saw ACKed; after a reconnect everything after the last
// ACK is sent again.
func TestEventsResentFromLastAckAfterReconnect(t *testing.T) {
	t.Parallel()
	sp := &memSpool{epoch: "sp1"}
	ch := &Channel{Spool: sp, Cursor: &memCursor{}, Commands: (&applied{}).fn, AckTimeout: time.Hour}
	at := ch.Attempt("step-1", 4)
	for _, k := range []string{protocol.EventAccepted, protocol.EventStarted, protocol.EventCompleted} {
		if err := at.Emit(k, nil); err != nil {
			t.Fatal(err)
		}
	}
	conn := newConn()
	stop := serve(t, ch, conn)
	evs := eventSeqs(t, conn, 3)
	for i, e := range evs {
		if e.Seq != int64(i+1) || e.AttemptSeq != int64(i+1) || e.Step != "step-1" || e.Fence != 4 || e.Epoch != "sp1" || e.Base != 0 {
			t.Fatalf("event %d = %+v", i, e)
		}
	}
	if _, err := ch.Handle(t.Context(), conn, env(t, protocol.TypeEventAck, protocol.Ack{Epoch: "sp1", Seq: 1})); err != nil {
		t.Fatal(err)
	}
	conn.quiet(t)
	stop() // the link drops

	conn2 := newConn()
	serve(t, ch, conn2)
	evs = eventSeqs(t, conn2, 2)
	if evs[0].Seq != 2 || evs[1].Seq != 3 || evs[0].Base != 1 {
		t.Fatalf("resent %+v", evs)
	}
	// An ACK of another epoch (a stale session) changes nothing.
	_, _ = ch.Handle(t.Context(), conn2, env(t, protocol.TypeEventAck, protocol.Ack{Epoch: "other", Seq: 3}))
	if sp.State().Acked != 1 {
		t.Fatalf("acked %d", sp.State().Acked)
	}
}

func TestEventsResentAfterAckTimeout(t *testing.T) {
	t.Parallel()
	ch := &Channel{Spool: &memSpool{epoch: "sp"}, Cursor: &memCursor{}, Commands: (&applied{}).fn, AckTimeout: 20 * time.Millisecond}
	_ = ch.Attempt("s", 1).Emit(protocol.EventStarted, nil)
	conn := newConn()
	serve(t, ch, conn)
	first := eventSeqs(t, conn, 1)[0]
	again := eventSeqs(t, conn, 1)[0] // no ACK: re-sent
	if first.Seq != 1 || again.Seq != 1 {
		t.Fatalf("first %d again %d", first.Seq, again.Seq)
	}
}

// At most Window events are in flight; ACKs open the window.
func TestWindowBoundsUnackedEvents(t *testing.T) {
	t.Parallel()
	ch := &Channel{Spool: &memSpool{epoch: "sp"}, Cursor: &memCursor{}, Commands: (&applied{}).fn, Window: 3, AckTimeout: time.Hour}
	at := ch.Attempt("s", 1)
	for range 7 {
		_ = at.Emit(protocol.EventStarted, nil)
	}
	conn := newConn()
	serve(t, ch, conn)
	eventSeqs(t, conn, 3)
	conn.quiet(t)
	_, _ = ch.Handle(t.Context(), conn, env(t, protocol.TypeEventAck, protocol.Ack{Epoch: "sp", Seq: 2}))
	if evs := eventSeqs(t, conn, 2); evs[0].Seq != 4 || evs[1].Seq != 5 {
		t.Fatalf("after ack sent %d %d", evs[0].Seq, evs[1].Seq)
	}
	conn.quiet(t)
}

func TestOutputIsChunkedAndOrdered(t *testing.T) {
	t.Parallel()
	sp := &memSpool{epoch: "sp"}
	ch := &Channel{Spool: sp, Cursor: &memCursor{}, Commands: (&applied{}).fn}
	at := ch.Attempt("s", 2)
	data := strings.Repeat("я", protocol.MaxOutputChunk) // 2 bytes per rune: chunks must not split one
	w := at.Output("stdout")
	if n, err := w.Write([]byte(data)); err != nil || n != len(data) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	evs, _ := sp.Read(0, 100, 1<<30)
	var joined strings.Builder
	var off int64
	for i, e := range evs {
		var o protocol.Output
		_ = json.Unmarshal(e.Body, &o)
		if len(o.Data) > protocol.MaxOutputChunk || !utf8.ValidString(o.Data) || o.Offset != off || e.AttemptSeq != int64(i+1) {
			t.Fatalf("chunk %d: %d bytes, offset %d (want %d), attempt seq %d", i, len(o.Data), o.Offset, off, e.AttemptSeq)
		}
		off += int64(len(o.Data))
		joined.WriteString(o.Data)
	}
	if joined.String() != data || len(evs) < 2 {
		t.Fatalf("reassembled %d bytes in %d chunks", joined.Len(), len(evs))
	}
}

// KIN-77: chunks end at a line (else a word) boundary when one fits, so the Control Plane redacts whole lines and
// a secret is never cut in two by the chunking.
func TestOutputChunksEndAtLineBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, data, sep string }{
		{"lines", strings.Repeat("token: ghp_Ab1Cd2Ef3Gh4Ij5Kl6Mn7Op8 line\n", 3*protocol.MaxOutputChunk/40), "\n"},
		{"one long line", strings.Repeat("word ", 3*protocol.MaxOutputChunk/5), " "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sp := &memSpool{epoch: "sp"}
			ch := &Channel{Spool: sp, Cursor: &memCursor{}, Commands: (&applied{}).fn}
			w := ch.Attempt("s", 2).Output("stdout")
			if n, err := w.Write([]byte(tc.data)); err != nil || n != len(tc.data) {
				t.Fatalf("Write = %d, %v", n, err)
			}
			evs, _ := sp.Read(0, 100, 1<<30)
			var joined strings.Builder
			for i, e := range evs {
				var o protocol.Output
				_ = json.Unmarshal(e.Body, &o)
				if len(o.Data) > protocol.MaxOutputChunk || !strings.HasSuffix(o.Data, tc.sep) {
					t.Fatalf("chunk %d of %d: %d bytes ending %q", i, len(evs), len(o.Data), o.Data[max(0, len(o.Data)-10):])
				}
				joined.WriteString(o.Data)
			}
			if joined.String() != tc.data || len(evs) < 3 {
				t.Fatalf("reassembled %d bytes in %d chunks", joined.Len(), len(evs))
			}
		})
	}
}

// Overflow policy: when the spool is full output is dropped and counted; the loss is reported by
// execution.output.dropped before the attempt's next event, and the process's writes never fail.
func TestFullSpoolDropsOutputAndReportsIt(t *testing.T) {
	t.Parallel()
	sp := &memSpool{epoch: "sp", limit: 300}
	ch := &Channel{Spool: sp, Cursor: &memCursor{}, Commands: (&applied{}).fn}
	at := ch.Attempt("s", 2)
	w := at.Output("stderr")
	for range 10 {
		if _, err := w.Write([]byte(strings.Repeat("x", 100))); err != nil {
			t.Fatalf("Write failed on a full spool: %v", err)
		}
	}
	if err := at.Emit(protocol.EventCompleted, nil); err != nil {
		t.Fatal(err)
	}
	evs, _ := sp.Read(0, 100, 1<<30)
	kinds := make([]string, 0, len(evs))
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	last := evs[len(evs)-2:]
	if last[0].Kind != protocol.EventOutputDropped || last[1].Kind != protocol.EventCompleted {
		t.Fatalf("kinds %v", kinds)
	}
	var d protocol.OutputDropped
	_ = json.Unmarshal(last[0].Body, &d)
	outputs := int64(len(evs) - 2)
	if d.Bytes != (10-outputs)*100 || d.Chunks != 10-outputs {
		t.Fatalf("dropped %+v after %d stored chunks", d, outputs)
	}
	if got := ch.reconcile("r").Spool.DroppedBytes; got != d.Bytes {
		t.Fatalf("reconcile dropped bytes %d", got)
	}
	for i, e := range evs {
		if e.AttemptSeq != int64(i+1) {
			t.Fatalf("attempt seq gap at %d: %d", i, e.AttemptSeq)
		}
	}
}

type procs []protocol.AttemptStatus

func (p procs) Attempts() []protocol.AttemptStatus { return slices.Clone(p) }

func TestReconcileResponse(t *testing.T) {
	t.Parallel()
	sp := &memSpool{epoch: "sp", limit: 1 << 20}
	cur := &memCursor{cur: protocol.Cursor{Epoch: "ce", Last: 7}}
	ch := &Channel{Spool: sp, Cursor: cur, Commands: (&applied{}).fn,
		Processes: procs{{Step: "run", Fence: 5, PID: 100, PGID: 100, Running: true}}}
	_ = ch.Attempt("done", 3).Emit(protocol.EventStarted, nil)
	_ = ch.Attempt("done", 3).Emit(protocol.EventFailed, protocol.Failure{Reason: "exit 1"})
	_ = sp.Ack(1)
	conn := newConn()
	if _, err := ch.Handle(t.Context(), conn, env(t, protocol.TypeReconcile, protocol.Reconcile{RequestID: "rq"})); err != nil {
		t.Fatal(err)
	}
	m := conn.next(t)
	r := m.payload.(protocol.ReconcileResponse)
	want := []protocol.AttemptStatus{{Step: "run", Fence: 5, PID: 100, PGID: 100, Running: true},
		{Step: "done", Fence: 3, Result: protocol.EventFailed, ResultSeq: 2}}
	if m.typ != protocol.TypeReconcileResponse || r.RequestID != "rq" || !slices.Equal(r.Attempts, want) {
		t.Fatalf("response %s %+v", m.typ, r)
	}
	if r.Spool.Epoch != "sp" || r.Spool.Acked != 1 || r.Spool.Last != 2 || r.Commands != (protocol.Ack{Epoch: "ce", Seq: 7}) {
		t.Fatalf("offsets %+v commands %+v", r.Spool, r.Commands)
	}
}

// Everything the runner spools must pass the Control Plane's validation; otherwise one bad record would close the
// link on every reconnect and block all later events behind it.
func TestEmittedEventsAlwaysFitTheWire(t *testing.T) {
	t.Parallel()
	sp := &memSpool{epoch: "sp"}
	ch := &Channel{Spool: sp, Cursor: &memCursor{}, Commands: (&applied{}).fn}
	at := ch.Attempt("step-1", 2)
	if err := at.Emit(protocol.EventFailed, protocol.Failure{Reason: strings.Repeat("e", 100<<10)}); err != nil {
		t.Fatalf("a long failure reason must be truncated, got %v", err)
	}
	if err := at.Emit(protocol.EventStarted, map[string]string{"x": strings.Repeat("a", protocol.MaxBodyBytes)}); err == nil {
		t.Fatal("an oversized body reached the spool")
	}
	for _, bad := range []*Attempt{ch.Attempt("", 1), ch.Attempt("has space", 1), ch.Attempt(strings.Repeat("s", 200), 1), ch.Attempt("s", 0)} {
		if err := bad.Emit(protocol.EventStarted, nil); !errors.Is(err, protocol.ErrChannelField) {
			t.Fatalf("invalid attempt %q/%d spooled: %v", bad.step, bad.fence, err)
		}
	}
	evs, _ := sp.Read(0, 100, 1<<30)
	if len(evs) != 1 {
		t.Fatalf("spooled %d events", len(evs))
	}
	for _, e := range evs {
		ev := protocol.Event{Epoch: "sp", Seq: e.Seq, Step: e.Step, Fence: e.Fence, AttemptSeq: e.AttemptSeq, Kind: e.Kind, At: e.At, Body: e.Body}
		if err := ev.Validate(); err != nil {
			t.Fatalf("spooled event fails validation: %v", err)
		}
		var f protocol.Failure
		_ = json.Unmarshal(e.Body, &f)
		if len(f.Reason) != protocol.MaxReasonLen {
			t.Fatalf("reason %d bytes", len(f.Reason))
		}
	}
}

// A resumed attempt (the runner restarted while its process ran) continues after every event number the old
// stream may have used: numbers are reserved in blocks before use, so none is ever reused.
func TestResumedAttemptNeverReusesEventNumbers(t *testing.T) {
	t.Parallel()
	sp := &memSpool{epoch: "sp"}
	ch := &Channel{Spool: sp, Cursor: &memCursor{}, Commands: (&applied{}).fn}
	var reserved []int64
	reserve := func(upTo int64) error { reserved = append(reserved, upTo); return nil }
	at := ch.ResumeAttempt("s", 1, 0, reserve)
	for range 3 {
		if err := at.Emit(protocol.EventAccepted, nil); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(reserved, []int64{AttemptSeqBlock}) {
		t.Fatalf("reserved %v", reserved)
	}
	// The runner restarts: the new stream starts after the reservation, not after the last event sent.
	again := ch.ResumeAttempt("s", 1, reserved[len(reserved)-1], reserve)
	if err := again.Emit(protocol.EventCompleted, nil); err != nil {
		t.Fatal(err)
	}
	evs, _ := sp.Read(0, 100, 1<<30)
	if got := evs[len(evs)-1].AttemptSeq; got != AttemptSeqBlock+1 {
		t.Fatalf("resumed attempt seq %d", got)
	}
	if !slices.Equal(reserved, []int64{AttemptSeqBlock, 2 * AttemptSeqBlock}) {
		t.Fatalf("reserved %v", reserved)
	}
	// A failing reservation fails the emit instead of risking a reused number.
	failing := ch.ResumeAttempt("s", 2, 0, func(int64) error { return errors.New("disk") })
	if err := failing.Emit(protocol.EventAccepted, nil); err == nil {
		t.Fatal("emit without a reservation succeeded")
	}
}
