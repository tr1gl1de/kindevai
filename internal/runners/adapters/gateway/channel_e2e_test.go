package gateway

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/adapters/controlplane"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/adapters/spool"
	runnerapp "github.com/tr1gl1de/kindevai/internal/runner/connection/app"
	runnerdomain "github.com/tr1gl1de/kindevai/internal/runner/connection/domain"
	"github.com/tr1gl1de/kindevai/internal/runners/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/runners/app"
)

type acceptAll struct {
	mu    sync.Mutex
	calls []string
}

func (a *acceptAll) rec(s string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, s)
	return nil
}
func (a *acceptAll) CheckAttempt(context.Context, string, string, int64) error {
	return a.rec("check")
}
func (a *acceptAll) StartAttempt(context.Context, string, string, int64, time.Time, string) error {
	return a.rec("start")
}
func (a *acceptAll) CompleteAttempt(context.Context, string, string, int64, time.Time) error {
	return a.rec("complete")
}
func (a *acceptAll) FailAttempt(context.Context, string, string, int64, string, time.Time) error {
	return a.rec("fail")
}
func (a *acceptAll) history() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.calls)
}

// End to end over the real WebSocket: events spooled while the runner was offline reach the Control Plane after
// it connects and are deleted from the spool once acknowledged; commands are applied by the runner once each, in
// commit order, and acknowledged.
func TestChannelEndToEnd(t *testing.T) {
	t.Parallel()
	e := newEnv(t, opts{})
	e.approve(t)
	ctx := t.Context()
	wf := &acceptAll{}
	cp := &app.Channel{Repo: sqlite.New(e.d), Executions: wf, Tx: db.Transactor{DB: e.d}, Now: time.Now,
		NewID:        func() string { return strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "") },
		PollInterval: 10 * time.Millisecond}
	e.h.Messages = map[string]MessageFunc{}
	for _, typ := range cp.Types() {
		e.h.Messages[typ] = func(ctx context.Context, s app.Session, env protocol.Envelope) error { return cp.Handle(ctx, s, env) }
	}
	e.h.Sessions = []SessionFunc{cp.Serve}

	// The runner, offline: its attempt's events go to the spool.
	dir := filepath.Join(t.TempDir(), "events")
	sp, err := spool.Open(spool.Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sp.Close() })
	var mu sync.Mutex
	var applied []string
	rch := &runnerapp.Channel{Spool: sp, Cursor: spool.CursorFile{Path: filepath.Join(dir, "commands.json")},
		Commands: func(_ context.Context, c protocol.Command) error {
			mu.Lock()
			defer mu.Unlock()
			applied = append(applied, c.ID)
			return nil
		}}
	at := rch.Attempt("step-1", 1)
	_ = at.Emit(protocol.EventAccepted, nil)
	_ = at.Emit(protocol.EventStarted, nil)
	if _, err := at.Output("stdout").Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	_ = at.Emit(protocol.EventCompleted, nil)

	// Commands queued for it on the Control Plane, each in its own transaction.
	for _, id := range []string{"c1", "c2", "c3", "c4"} {
		if err := (db.Transactor{DB: e.d}).InTx(ctx, func(ctx context.Context) error {
			return cp.Send(ctx, e.runner, id, protocol.CommandCancel, protocol.Cancel{Step: "s", Fence: 1, Reason: "test"})
		}); err != nil {
			t.Fatal(err)
		}
	}

	server, err := runnerdomain.ParseServerURL("http" + strings.TrimPrefix(e.srv.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	id, err := runnerdomain.NewIdentity(e.key)
	if err != nil {
		t.Fatal(err)
	}
	conn := &runnerapp.Connector{Dialer: controlplane.New(), Handlers: []runnerapp.LinkHandler{rch}}
	rctx, stopRunner := context.WithCancel(ctx)
	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		_ = conn.Run(rctx, runnerdomain.Enrollment{Server: server, RunnerID: e.runner}, id)
	}()
	t.Cleanup(func() { stopRunner(); <-runnerDone })

	waitFor(t, "events acknowledged and commands applied", func() bool {
		mu.Lock()
		n := len(applied)
		mu.Unlock()
		st, _, _ := sqlite.New(e.d).Channel(ctx, e.runner)
		return sp.State().Acked == 4 && n == 4 && st.CommandAcked == 4
	})
	if h := wf.history(); !slices.Equal(h, []string{"check", "start", "check", "complete"}) {
		t.Fatalf("workflow saw %v", h)
	}
	if !slices.Equal(applied, []string{"c1", "c2", "c3", "c4"}) {
		t.Fatalf("runner applied %v", applied)
	}
	if st := sp.State(); st.Bytes != 0 || len(sp.Pending()) != 0 {
		t.Fatalf("spool after ACK %+v pending %v", st, sp.Pending())
	}
	var stored string
	if err := e.d.QueryRowContext(ctx, `SELECT body FROM runner_attempt_events WHERE step_id = 'step-1' AND attempt_seq = 3`).Scan(&stored); err != nil || !strings.Contains(stored, `hello\n`) {
		t.Fatalf("stored output %q, %v", stored, err)
	}
}
