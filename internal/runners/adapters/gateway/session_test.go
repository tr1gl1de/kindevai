package gateway

import (
	"context"
	"encoding/json"
	"testing"

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
