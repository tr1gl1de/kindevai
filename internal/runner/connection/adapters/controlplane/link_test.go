package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/coder/websocket"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/app"
)

// An authorized link delivers the Control Plane's messages to the handler and sends the runner's replies.
func TestLinkExchangesMessages(t *testing.T) {
	t.Parallel()
	got := make(chan protocol.Envelope, 1)
	srv := serve(t, fakeCP(t, 0, func(c *websocket.Conn) {
		raw, _ := json.Marshal(protocol.Ack{Epoch: "e", Seq: 3})
		b, _ := json.Marshal(protocol.Envelope{Type: protocol.TypeEventAck, Payload: raw})
		_ = c.Write(context.Background(), websocket.MessageText, b)
		_, b, err := c.Read(context.Background())
		if err == nil {
			var env protocol.Envelope
			_ = json.Unmarshal(b, &env)
			got <- env
		}
		_ = c.Close(websocket.StatusNormalClosure, "done")
	}))
	l, err := New().Dial(t.Context(), srv, answer)
	if err != nil {
		t.Fatal(err)
	}
	conn := l.(app.Conn)
	conn.SetHandler(func(ctx context.Context, env protocol.Envelope) error {
		if env.Type != protocol.TypeEventAck {
			return errors.New("unexpected " + env.Type)
		}
		return conn.Send(ctx, protocol.TypeCommandAck, protocol.Ack{Epoch: "c", Seq: 9})
	})
	_ = l.Wait(t.Context())
	env := <-got
	var a protocol.Ack
	if env.Type != protocol.TypeCommandAck || json.Unmarshal(env.Payload, &a) != nil || a.Seq != 9 {
		t.Fatalf("server got %+v", env)
	}
}

func TestLinkClosesWhenHandlerFails(t *testing.T) {
	t.Parallel()
	code := make(chan websocket.StatusCode, 1)
	srv := serve(t, fakeCP(t, 0, func(c *websocket.Conn) {
		b, _ := json.Marshal(protocol.Envelope{Type: protocol.TypeCommand, Payload: json.RawMessage(`{}`)})
		_ = c.Write(context.Background(), websocket.MessageText, b)
		_, _, err := c.Read(context.Background())
		code <- websocket.CloseStatus(err)
	}))
	l, err := New().Dial(t.Context(), srv, answer)
	if err != nil {
		t.Fatal(err)
	}
	l.(app.Conn).SetHandler(func(context.Context, protocol.Envelope) error { return protocol.ErrChannelField })
	if err := l.Wait(t.Context()); !errors.Is(err, protocol.ErrChannelField) {
		t.Fatalf("Wait = %v", err)
	}
	if c := <-code; c != protocol.CloseProtocolError {
		t.Fatalf("close code %d", c)
	}
}
