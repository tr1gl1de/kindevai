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
	"unicode/utf8"

	"github.com/tr1gl1de/kindevai/internal/protocol"
)

// Conn is a Link that exchanges messages: Send writes one envelope (safe for concurrent use); the handler set
// before Wait receives every message of the Control Plane, and an error from it closes the link (protocol error).
type Conn interface {
	Link
	Send(ctx context.Context, typ string, payload any) error
	SetHandler(func(ctx context.Context, env protocol.Envelope) error)
}

// LinkHandler is a feature running on every authorized link (the KIN-22 channel; KIN-20 heartbeats).
type LinkHandler interface {
	// Serve runs while the link is up; ctx ends when it goes down.
	Serve(ctx context.Context, c Conn) error
	// Handle gets a message from the Control Plane; ok=false means the type is not this handler's.
	Handle(ctx context.Context, c Conn, env protocol.Envelope) (ok bool, err error)
}

// CommandCursor persists the position in the Control Plane's command stream (adapters/spool.CursorFile).
type CommandCursor interface {
	Load() (protocol.Cursor, error)
	Save(protocol.Cursor) error
}

// CommandFunc applies one command. It is called once per command in Seq order (a command re-delivered after a
// lost ACK is filtered out by Seq; handlers must still be idempotent by Command.ID, e.g. across a wiped data dir).
// A kind it does not implement must be logged and return nil. Any error leaves the command unacknowledged: the
// Control Plane re-sends it after its ACK timeout.
type CommandFunc func(ctx context.Context, cmd protocol.Command) error

// Processes reports the attempts with a live process (session context, KIN-23).
type Processes interface {
	Attempts() []protocol.AttemptStatus
}

// Channel is the runner end of the command/event channel (KIN-22): it applies commands in order exactly once per
// Seq and streams the event spool to the Control Plane from the last ACK, keeping at most Window events
// (WindowBytes) unacknowledged in flight.
type Channel struct {
	Spool     Spool
	Cursor    CommandCursor
	Commands  CommandFunc
	Processes Processes // nil: none
	Now       func() time.Time
	Logger    *slog.Logger
	// Window and WindowBytes bound unacknowledged events in flight (defaults 64 and 1 MiB); AckTimeout without
	// progress re-sends from the last ACK (default 30s).
	Window      int
	WindowBytes int
	AckTimeout  time.Duration

	once    sync.Once
	wake    chan struct{}
	mu      sync.Mutex // serializes command handling and the cursor
	cursor  *protocol.Cursor
	dropped atomic.Int64
}

var _ LinkHandler = (*Channel)(nil)

func (c *Channel) init() {
	c.once.Do(func() {
		c.wake = make(chan struct{}, 1)
		if c.Now == nil {
			c.Now = time.Now
		}
		if c.Logger == nil {
			c.Logger = slog.New(slog.DiscardHandler)
		}
		if c.Window <= 0 {
			c.Window = 64
		}
		if c.WindowBytes <= 0 {
			c.WindowBytes = 1 << 20
		}
		if c.AckTimeout <= 0 {
			c.AckTimeout = 30 * time.Second
		}
	})
}

func (c *Channel) notify() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Emit appends an event to the spool (durable when it returns) and wakes the sender.
func (c *Channel) Emit(e SpooledEvent) (int64, error) {
	c.init()
	if e.At == 0 {
		e.At = c.Now().UnixMilli()
	}
	seq, err := c.Spool.Append(e)
	if err != nil {
		return 0, err
	}
	c.notify()
	return seq, nil
}

// Serve streams unacknowledged events over c until ctx ends or sending fails.
func (c *Channel) Serve(ctx context.Context, conn Conn) error {
	c.init()
	st := c.Spool.State()
	sent := st.Acked
	progress := st.Acked
	timer := time.NewTimer(c.AckTimeout)
	defer timer.Stop()
	for {
		st = c.Spool.State()
		if st.Acked > sent {
			sent = st.Acked
		}
		if free := c.Window - int(sent-st.Acked); free > 0 && sent < st.Last {
			evs, err := c.Spool.Read(sent, free, c.WindowBytes)
			if err != nil {
				return fmt.Errorf("reading the event spool: %w", err)
			}
			for _, e := range evs {
				if err := conn.Send(ctx, protocol.TypeEvent, protocol.Event{Epoch: st.Epoch, Seq: e.Seq, Base: st.Acked,
					Step: e.Step, Fence: e.Fence, AttemptSeq: e.AttemptSeq, Kind: e.Kind, At: e.At, Body: e.Body}); err != nil {
					return fmt.Errorf("sending event %d: %w", e.Seq, err)
				}
				sent = e.Seq
			}
			if len(evs) > 0 {
				continue
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-c.wake:
		case <-timer.C:
			st = c.Spool.State()
			if sent > st.Acked && st.Acked == progress {
				c.Logger.Warn("events not acknowledged in time; re-sending", "from", st.Acked+1, "to", sent)
				sent = st.Acked
			}
			progress = st.Acked
			timer.Reset(c.AckTimeout)
		}
	}
}

func decode(env protocol.Envelope, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(env.Payload)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %s: %w", protocol.ErrChannelField, env.Type, err)
	}
	return nil
}

// Handle processes the channel's messages from the Control Plane.
func (c *Channel) Handle(ctx context.Context, conn Conn, env protocol.Envelope) (bool, error) {
	c.init()
	switch env.Type {
	case protocol.TypeCommand:
		var cmd protocol.Command
		if err := decode(env, &cmd); err != nil {
			return true, err
		}
		if err := cmd.Validate(); err != nil {
			return true, err
		}
		return true, c.command(ctx, conn, cmd)
	case protocol.TypeEventAck:
		var a protocol.Ack
		if err := decode(env, &a); err != nil {
			return true, err
		}
		if a.Epoch == c.Spool.State().Epoch && a.Seq > 0 {
			if err := c.Spool.Ack(a.Seq); err != nil {
				c.Logger.Error("recording event ACK", "err", err)
			}
			c.notify()
		}
		return true, nil
	case protocol.TypeReconcile:
		var r protocol.Reconcile
		if err := decode(env, &r); err != nil {
			return true, err
		}
		return true, conn.Send(ctx, protocol.TypeReconcileResponse, c.reconcile(r.RequestID))
	}
	return false, nil
}

func (c *Channel) command(ctx context.Context, conn Conn, cmd protocol.Command) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cursor == nil {
		cur, err := c.Cursor.Load()
		if err != nil {
			return fmt.Errorf("loading the command cursor: %w", err)
		}
		c.cursor = &cur
	}
	v, next := c.cursor.Receive(cmd.Epoch, cmd.Seq, cmd.Base)
	switch v {
	case protocol.Invalid:
		return fmt.Errorf("%w: command seq", protocol.ErrChannelField)
	case protocol.Gap:
		c.Logger.Debug("command out of order dropped", "seq", cmd.Seq, "expected", next.Last+1)
		return c.save(next)
	case protocol.Duplicate:
		if err := c.save(next); err != nil {
			return err
		}
	case protocol.Apply:
		if err := c.Commands(ctx, cmd); err != nil {
			c.Logger.Warn("command not applied; the Control Plane will re-send it", "id", cmd.ID, "kind", cmd.Kind, "err", err)
			return nil
		}
		if err := c.save(next); err != nil {
			return err
		}
	}
	return conn.Send(ctx, protocol.TypeCommandAck, protocol.Ack{Epoch: c.cursor.Epoch, Seq: c.cursor.Last})
}

func (c *Channel) save(next protocol.Cursor) error {
	if next == *c.cursor {
		return nil
	}
	if err := c.Cursor.Save(next); err != nil {
		return fmt.Errorf("saving the command cursor: %w", err)
	}
	*c.cursor = next
	return nil
}

func (c *Channel) reconcile(requestID string) protocol.ReconcileResponse {
	var attempts []protocol.AttemptStatus
	if c.Processes != nil {
		attempts = c.Processes.Attempts()
	}
	for _, p := range c.Spool.Pending() {
		i := -1
		for j, a := range attempts {
			if a.Step == p.Step && a.Fence == p.Fence {
				i = j
			}
		}
		if i < 0 {
			attempts = append(attempts, protocol.AttemptStatus{Step: p.Step, Fence: p.Fence})
			i = len(attempts) - 1
		}
		attempts[i].Result, attempts[i].ResultSeq = p.Kind, p.Seq
	}
	if attempts == nil {
		attempts = []protocol.AttemptStatus{}
	}
	st := c.Spool.State()
	c.mu.Lock()
	var cmds protocol.Ack
	if c.cursor != nil {
		cmds = protocol.Ack{Epoch: c.cursor.Epoch, Seq: c.cursor.Last}
	} else if cur, err := c.Cursor.Load(); err == nil {
		cmds = protocol.Ack{Epoch: cur.Epoch, Seq: cur.Last}
	}
	c.mu.Unlock()
	return protocol.ReconcileResponse{RequestID: requestID, Attempts: attempts, Commands: cmds,
		Spool: protocol.SpoolOffsets{Epoch: st.Epoch, Acked: st.Acked, Last: st.Last, Bytes: st.Bytes,
			LimitBytes: st.LimitBytes, DroppedBytes: c.dropped.Load()}}
}

// Attempt returns the event stream of one attempt: it numbers the attempt's events and writes its output.
func (c *Channel) Attempt(step string, fence int64) *Attempt {
	c.init()
	return &Attempt{ch: c, step: step, fence: fence, offsets: map[string]int64{}}
}

// Attempt is the event stream of one attempt (Step + Fence). It is safe for concurrent use (stdout and stderr).
//
// Backpressure: output is written to the spool synchronously in chunks of at most protocol.MaxOutputChunk, so a
// process producing output faster than the disk takes it blocks on its pipe; nothing is buffered in memory beyond
// one chunk. When the spool is full the output is dropped and counted, and an execution.output.dropped event
// reports the loss before the attempt's next event.
type Attempt struct {
	ch      *Channel
	step    string
	fence   int64
	mu      sync.Mutex
	seq     int64
	offsets map[string]int64
	dropped protocol.OutputDropped
}

// Emit appends a lifecycle event (body is marshalled to JSON; nil for none).
func (a *Attempt) Emit(kind string, body any) error {
	var raw json.RawMessage
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding %s: %w", kind, err)
		}
		raw = b
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.flushDropped()
	return a.append(kind, raw)
}

func (a *Attempt) append(kind string, body json.RawMessage) error {
	_, err := a.ch.Emit(SpooledEvent{Step: a.step, Fence: a.fence, AttemptSeq: a.seq + 1, Kind: kind, Body: body})
	if err == nil {
		a.seq++
	}
	return err
}

// flushDropped reports dropped output once there is room again.
func (a *Attempt) flushDropped() {
	if a.dropped.Bytes == 0 {
		return
	}
	b, _ := json.Marshal(a.dropped)
	if a.append(protocol.EventOutputDropped, b) == nil {
		a.dropped = protocol.OutputDropped{}
	}
}

// room reports whether an output event of about n bytes fits under the spool's limit.
func (c *Channel) room(n int) bool {
	st := c.Spool.State()
	return st.LimitBytes <= 0 || st.Bytes+int64(n)+256 <= st.LimitBytes
}

// Output returns a writer for a process stream ("stdout" or "stderr"). Write never fails because of a full spool
// (the output is dropped and counted), so the process is never killed by its pipe; other spool errors are returned.
func (a *Attempt) Output(stream string) io.Writer { return outputWriter{a, stream} }

type outputWriter struct {
	a      *Attempt
	stream string
}

func (w outputWriter) Write(p []byte) (int, error) {
	a := w.a
	a.mu.Lock()
	defer a.mu.Unlock()
	written := 0
	for len(p) > 0 {
		n := min(len(p), protocol.MaxOutputChunk)
		for n < len(p) && n > 0 && !utf8.RuneStart(p[n]) { // keep multi-byte characters whole
			n--
		}
		if n == 0 {
			n = min(len(p), protocol.MaxOutputChunk)
		}
		body, _ := json.Marshal(protocol.Output{Stream: w.stream, Offset: a.offsets[w.stream], Data: string(p[:n])})
		if a.dropped.Bytes > 0 && a.ch.room(len(body)) {
			a.flushDropped() // only once output fits again, so a full spool's reserve is not spent on loss reports
		}
		aerr := ErrSpoolFull // keep order: no output after a loss until the loss itself is reported
		if a.dropped.Bytes == 0 {
			aerr = a.append(protocol.EventOutput, body)
		}
		switch {
		case errors.Is(aerr, ErrSpoolFull):
			a.dropped.Bytes += int64(n)
			a.dropped.Chunks++
			a.ch.dropped.Add(int64(n))
		case aerr != nil:
			return written, aerr
		}
		a.offsets[w.stream] += int64(n)
		written += n
		p = p[n:]
	}
	return written, nil
}
