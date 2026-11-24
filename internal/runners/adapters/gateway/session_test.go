package gateway

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runners/app"
)

// Session features (the KIN-22 channel) run while a session is open and push messages to the runner; message
// handlers can answer on the same session.
func TestSessionFeaturesPushAndAnswer(t *testing.T) {
	t.Parallel()
	ended := make(chan struct{})
	e := newEnv(t, opts{
		sessions: []SessionFunc{func(ctx context.Context, s app.Session) {
			_ = s.Send(ctx, protocol.TypeReconcile, protocol.Reconcile{RequestID: "rq-" + s.RunnerID})
			<-ctx.Done()
			close(ended)
		}},
		messages: map[string]MessageFunc{protocol.TypeEvent: func(ctx context.Context, s app.Session, env protocol.Envelope) error {
			return s.Send(ctx, protocol.TypeEventAck, protocol.Ack{Epoch: "sp", Seq: 1})
		}},
	})
	e.approve(t)
	c, _ := e.connect(t)
	env, err := readEnv(t, c)
	var r protocol.Reconcile
	if err != nil || env.Type != protocol.TypeReconcile || json.Unmarshal(env.Payload, &r) != nil || r.RequestID != "rq-"+e.runner {
		t.Fatalf("pushed %+v, %v", env, err)
	}
	send(t, c, protocol.TypeEvent, map[string]int{})
	env, err = readEnv(t, c)
	if err != nil || env.Type != protocol.TypeEventAck {
		t.Fatalf("answer %+v, %v", env, err)
	}
	e.hub.Disconnect(e.runner, app.ReasonDisabled)
	<-ended // the feature stops with the session
}

// A session closed while a message to the runner is still being written keeps the connection until the close
// frame is out: the runner receives the whole message and then the close code, never an abrupt TCP teardown
// (KIN-54 — a cancelled write context made the library drop the connection, so Shutdown right after Authorized
// surfaced as close -1).
func TestClosingASessionDoesNotCutAWriteInFlight(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 1<<20) // several times what the loopback socket buffers hold, so the write blocks; small
	// enough to be read well within closeGrace
	e := newEnv(t, opts{sessions: []SessionFunc{func(ctx context.Context, s app.Session) {
		_ = s.Send(ctx, protocol.TypeReconcile, protocol.Reconcile{RequestID: big})
	}}})
	e.approve(t)
	c, _ := e.connect(t)
	c.SetReadLimit(-1)
	_, r, err := c.Reader(t.Context()) // the frame header arrived: the server is in the middle of writing it
	if err != nil {
		t.Fatal(err)
	}
	e.hub.Disconnect(e.runner, app.ReasonRevoked)
	b, err := io.ReadAll(r)
	var env protocol.Envelope
	if err != nil || json.Unmarshal(b, &env) != nil || env.Type != protocol.TypeReconcile {
		t.Fatalf("message cut off after %d bytes: %v (close %d)", len(b), err, websocket.CloseStatus(err))
	}
	if code := closeCode(t, c); code != protocol.CloseRevoked {
		t.Fatalf("close %d, want %d", code, protocol.CloseRevoked)
	}
}
