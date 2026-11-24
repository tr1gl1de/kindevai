package sqlite_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runners/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/runners/app"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

func withRunner(t *testing.T) (*sql.DB, *sqlite.Repository) {
	t.Helper()
	d := testDB(t)
	r := sqlite.New(d)
	tok, _, _ := domain.NewEnrollmentToken(rand.Reader, "t1", "owner1", t0)
	if err := r.CreateToken(t.Context(), tok); err != nil {
		t.Fatal(err)
	}
	key, _ := newKey(t)
	if err := r.CreateRunner(t.Context(), domain.Enroll("r1", tok, key, domain.Metadata{Hostname: "h", OS: "linux", Arch: "amd64"}, t0)); err != nil {
		t.Fatal(err)
	}
	return d, r
}

func TestCommandLog(t *testing.T) {
	t.Parallel()
	d, r := withRunner(t)
	ctx := t.Context()
	tx := db.Transactor{DB: d}
	if _, ok, err := r.Channel(ctx, "r1"); ok || err != nil {
		t.Fatalf("channel before any command: %v %v", ok, err)
	}
	if err := r.EnsureChannel(ctx, "r1", "ep1"); err != nil {
		t.Fatal(err)
	}
	_ = r.EnsureChannel(ctx, "r1", "ep2") // exists: the epoch stays
	appendCmd := func(id string) int64 {
		var seq int64
		if err := tx.InTx(ctx, func(ctx context.Context) error {
			var err error
			seq, err = r.AppendCommand(ctx, "r1", domain.QueuedCommand{ID: id, Kind: "execution.cancel", Body: []byte(`{"a":1}`)}, t0)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return seq
	}
	if a, b, again, c := appendCmd("x"), appendCmd("y"), appendCmd("x"), appendCmd("z"); a != 1 || b != 2 || again != 1 || c != 3 {
		t.Fatalf("seqs %d %d %d %d", a, b, again, c)
	}
	cmds, err := r.CommandsAfter(ctx, "r1", 1, 10)
	if err != nil || len(cmds) != 2 || cmds[0].Seq != 2 || cmds[0].ID != "y" || string(cmds[0].Body) != `{"a":1}` {
		t.Fatalf("CommandsAfter = %+v, %v", cmds, err)
	}
	if err := r.AckCommands(ctx, "r1", 2, t0); err != nil {
		t.Fatal(err)
	}
	_ = r.AckCommands(ctx, "r1", 1, t0)  // never moves back
	_ = r.AckCommands(ctx, "r1", 99, t0) // never past the log
	st, _, _ := r.Channel(ctx, "r1")
	if st != (domain.Channel{CommandEpoch: "ep1", CommandSeq: 3, CommandAcked: 2}) {
		t.Fatalf("channel %+v", st)
	}
	// An acknowledged command still dedups a re-delivered outbox message until the retention ends.
	if appendCmd("y") != 2 {
		t.Fatal("acknowledged command appended again")
	}
	_ = r.AckCommands(ctx, "r1", 3, t0.Add(domain.AckedCommandRetention+time.Hour)) // purges x, y (acked at t0)
	if appendCmd("x") != 4 {
		t.Fatal("purged command id should be new again")
	}
}

func TestEventCursorAndRecords(t *testing.T) {
	t.Parallel()
	_, r := withRunner(t)
	ctx := t.Context()
	_ = r.EnsureChannel(ctx, "r1", "ep")
	if err := r.SaveEventCursor(ctx, "r1", "sp", 7); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := r.Channel(ctx, "r1"); st.EventEpoch != "sp" || st.EventLast != 7 {
		t.Fatalf("cursor %+v", st)
	}
	ev := domain.AttemptEvent{RunnerID: "r1", Step: "s", Fence: 2, AttemptSeq: 1, Kind: "execution.output", Body: []byte(`{}`),
		Outcome: domain.OutcomeApplied, RunnerAt: t0, ReceivedAt: t0}
	if err := r.RecordEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	if seen, _ := r.EventSeen(ctx, "s", 2, 1); !seen {
		t.Fatal("not seen")
	}
	if seen, _ := r.EventSeen(ctx, "s", 3, 1); seen {
		t.Fatal("another fence seen")
	}
	if r.RecordEvent(ctx, ev) == nil {
		t.Fatal("an attempt event was recorded twice")
	}
	if ok, _ := r.AddOutput(ctx, "s", 2, 60, 100); !ok {
		t.Fatal("first output refused")
	}
	if ok, _ := r.AddOutput(ctx, "s", 2, 50, 100); ok {
		t.Fatal("output over the cap accepted")
	}
	if ok, _ := r.AddOutput(ctx, "s", 2, 40, 100); !ok {
		t.Fatal("output up to the cap refused")
	}
}

// okWorkflow accepts every event in order (the fencing rules are covered by the app tests).
type okWorkflow struct {
	mu    sync.Mutex
	calls []string
}

func (w *okWorkflow) rec(s string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, s)
	return nil
}
func (w *okWorkflow) CheckAttempt(context.Context, string, string, int64) error {
	return w.rec("check")
}
func (w *okWorkflow) StartAttempt(context.Context, string, string, int64) error {
	return w.rec("start")
}
func (w *okWorkflow) CompleteAttempt(context.Context, string, string, int64) error {
	return w.rec("complete")
}
func (w *okWorkflow) FailAttempt(context.Context, string, string, int64, string) error {
	return w.rec("fail")
}

// A Control Plane restart in the middle of an event stream loses nothing and applies nothing twice: ACKs are sent
// only after the commit, the cursor is durable, and the runner re-sends everything after its last ACK.
func TestControlPlaneRestartDuringEventStream(t *testing.T) {
	t.Parallel()
	d, r := withRunner(t)
	wf := &okWorkflow{}
	newChannel := func() *app.Channel {
		return &app.Channel{Repo: r, Executions: wf, Outbox: messaging.NewOutbox(d, messaging.Config{}),
			Tx: db.Transactor{DB: d}, Now: time.Now, NewID: rand.Text}
	}
	kinds := []string{protocol.EventAccepted, protocol.EventStarted, protocol.EventOutput, protocol.EventOutput,
		protocol.EventOutput, protocol.EventOutput, protocol.EventCompleted}
	events := make([]protocol.Event, len(kinds))
	for i, k := range kinds {
		var body json.RawMessage
		if k == protocol.EventOutput {
			body, _ = json.Marshal(protocol.Output{Stream: "stdout", Data: fmt.Sprint("line ", i)})
		}
		events[i] = protocol.Event{Epoch: "spool", Seq: int64(i + 1), Step: "s1", Fence: 3, AttemptSeq: int64(i + 1),
			Kind: k, At: t0.UnixMilli(), Body: body}
	}
	var runnerAcked int64
	send := func(ch *app.Channel, upTo int) {
		var mu sync.Mutex
		var lastAck int64
		sess := app.Session{ID: "c", RunnerID: "r1", Detach: func() {}, Send: func(_ context.Context, typ string, p any) error {
			if typ == protocol.TypeEventAck {
				mu.Lock()
				lastAck = p.(protocol.Ack).Seq
				mu.Unlock()
			}
			return nil
		}}
		for _, ev := range events[runnerAcked:upTo] {
			ev.Base = runnerAcked
			b, _ := json.Marshal(ev)
			if err := ch.Handle(t.Context(), sess, protocol.Envelope{Type: protocol.TypeEvent, Payload: b}); err != nil {
				t.Fatal(err)
			}
		}
		runnerAcked = max(runnerAcked, lastAck-1) // the last ACK is lost in the crash
	}
	send(newChannel(), 4) // the Control Plane crashes after committing four events
	send(newChannel(), 7) // a new process; the runner re-sends from its last ACK
	if !slices.Equal(wf.calls, []string{"check", "start", "check", "check", "check", "check", "complete"}) {
		t.Fatalf("workflow calls %v", wf.calls)
	}
	var n int
	if err := d.QueryRowContext(t.Context(), `SELECT count(*) FROM runner_attempt_events WHERE step_id = 's1'`).Scan(&n); err != nil || n != 7 {
		t.Fatalf("recorded %d events, %v", n, err)
	}
}
