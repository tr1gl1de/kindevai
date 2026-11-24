package controlplane

import (
	"encoding/json"
	"testing"

	"github.com/coder/websocket"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/app"
)

func TestLinkSendsTelemetryEnvelopes(t *testing.T) {
	t.Parallel()
	got := make(chan protocol.Envelope, 1)
	srv := serve(t, fakeCP(t, 0, func(c *websocket.Conn) {
		_, b, err := c.Read(t.Context())
		var env protocol.Envelope
		if err == nil && json.Unmarshal(b, &env) == nil {
			got <- env
		}
		_ = c.Close(websocket.StatusNormalClosure, "")
	}))
	l, err := New().Dial(t.Context(), srv, answer)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := l.(app.Sender)
	if !ok {
		t.Fatal("the link cannot send")
	}
	if err := s.Send(t.Context(), protocol.TypeHeartbeat, protocol.Heartbeat{SentAt: "2026-11-24T12:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	env := <-got
	var hb protocol.Heartbeat
	if env.Type != protocol.TypeHeartbeat || json.Unmarshal(env.Payload, &hb) != nil || hb.SentAt != "2026-11-24T12:00:00Z" {
		t.Fatalf("server got %+v", env)
	}
	_ = l.Wait(t.Context())
}
