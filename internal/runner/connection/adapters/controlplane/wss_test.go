package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/app"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/domain"
)

// fakeCP speaks the server side of the handshake; after authorizing it runs then.
func fakeCP(t *testing.T, authCode int, then func(c *websocket.Conn)) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/base"+protocol.ConnectPath {
			http.NotFound(w, r)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{protocol.Subprotocol}})
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := context.Background()
		send := func(typ string, v any) {
			raw, _ := json.Marshal(v)
			b, _ := json.Marshal(protocol.Envelope{Type: typ, Payload: raw})
			_ = c.Write(ctx, websocket.MessageText, b)
		}
		send(protocol.TypeChallenge, protocol.Challenge{Version: 1, Nonce: "bm9uY2U", ExpiresAt: 1, Audience: "x", ConnectionID: "c1"})
		_, b, err := c.Read(ctx)
		var env protocol.Envelope
		var a protocol.Authenticate
		if err != nil || json.Unmarshal(b, &env) != nil || env.Type != protocol.TypeAuthenticate ||
			json.Unmarshal(env.Payload, &a) != nil || a.RunnerID != "r1" || a.Signature != "c2ln" || a.Version != 1 {
			_ = c.Close(protocol.CloseProtocolError, "bad authenticate")
			return
		}
		if authCode != 0 {
			_ = c.Close(websocket.StatusCode(authCode), "no")
			return
		}
		send(protocol.TypeAuthorized, protocol.Authorized{RunnerID: "r1", SessionID: "c1"})
		then(c)
	}
}

func answer(ch app.Challenge) (app.Answer, error) {
	if ch.ConnectionID != "c1" || ch.Nonce != "bm9uY2U" {
		return app.Answer{}, errors.New("challenge not passed through")
	}
	return app.Answer{RunnerID: "r1", Signature: []byte("sig")}, nil
}

func TestDialAuthorizesAndMapsRevocation(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := serve(t, fakeCP(t, 0, func(c *websocket.Conn) {
		<-release
		_ = c.Close(protocol.CloseRevoked, "runner revoked")
	}))
	l, err := New().Dial(t.Context(), srv, answer)
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := l.Wait(t.Context()); !errors.Is(err, domain.ErrRunnerRevoked) {
		t.Fatalf("Wait = %v", err)
	}
}

func TestDialMapsHandshakeCloseCodes(t *testing.T) {
	t.Parallel()
	for code, want := range map[int]error{
		protocol.CloseNotApproved: domain.ErrNotApproved, protocol.CloseAuthFailed: domain.ErrAuthRejected,
		protocol.CloseRevoked: domain.ErrRunnerRevoked, protocol.CloseUnsupportedVersion: domain.ErrUnsupportedProtocol,
	} {
		t.Run(want.Error(), func(t *testing.T) {
			t.Parallel()
			srv := serve(t, fakeCP(t, code, nil))
			if _, err := New().Dial(t.Context(), srv, answer); !errors.Is(err, want) {
				t.Fatalf("err = %v, want %v", err, want)
			}
		})
	}
}

func TestDialMapsHTTPRefusals(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		status int
		code   string
		want   error
	}{
		"plain ws from remote": {http.StatusForbidden, protocol.ErrCodePlainRemote, domain.ErrPlainHTTP},
		"unsupported protocol": {http.StatusUpgradeRequired, protocol.ErrCodeUnsupportedProto, domain.ErrUnsupportedProtocol},
		"busy":                 {http.StatusServiceUnavailable, protocol.ErrCodeBusy, ErrUnexpected},
		"redirect":             {http.StatusFound, "", ErrUnexpected},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.status == http.StatusFound {
					http.Redirect(w, r, "https://elsewhere.example/", http.StatusFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(protocol.Error{Error: tc.code})
			})
			_, err := New().Dial(t.Context(), srv, answer)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if domain.Fatal(err) == errors.Is(tc.want, ErrUnexpected) {
				t.Fatalf("fatal = %v for %v", domain.Fatal(err), err)
			}
		})
	}
}

func TestDialStopsOnTLSVerificationFailure(t *testing.T) {
	t.Parallel()
	var authorized atomic.Bool
	ts := httptest.NewTLSServer(fakeCP(t, 0, func(*websocket.Conn) { authorized.Store(true) }))
	t.Cleanup(ts.Close)
	srv, err := domain.ParseServerURL(ts.URL + "/base")
	if err != nil {
		t.Fatal(err)
	}
	_, err = New().Dial(t.Context(), srv, answer)
	if !errors.Is(err, domain.ErrTLSVerification) || !domain.Fatal(err) {
		t.Fatalf("self-signed server: %v", err)
	}
	// Trusting the test CA makes the same server work: the failure above is about trust only.
	c := New()
	c.rootCAs = ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	l, err := c.Dial(t.Context(), srv, answer)
	if err != nil {
		t.Fatalf("trusted wss: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := l.Wait(ctx); err != nil {
		t.Fatalf("Wait after cancel = %v", err)
	}
}

func TestLinkClosesOnUnexpectedMessage(t *testing.T) {
	t.Parallel()
	srv := serve(t, fakeCP(t, 0, func(c *websocket.Conn) {
		b, _ := json.Marshal(protocol.Envelope{Type: "runner.surprise"})
		_ = c.Write(context.Background(), websocket.MessageText, b)
		_, _, _ = c.Read(context.Background())
	}))
	l, err := New().Dial(t.Context(), srv, answer)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Wait(t.Context()); !errors.Is(err, ErrUnexpected) || domain.Fatal(err) || !strings.Contains(err.Error(), "runner.surprise") {
		t.Fatalf("Wait = %v", err)
	}
}

// The management channel never uses HTTP(S)_PROXY from the environment (that is the AI CLIs' egress proxy).
func TestWebSocketIgnoresProxyEnvironment(t *testing.T) {
	t.Parallel()
	srv, _ := domain.ParseServerURL("https://cp.example.com")
	tr := New().wsHTTP(srv).Transport.(*http.Transport)
	if tr.Proxy != nil || tr.TLSClientConfig.InsecureSkipVerify || tr.TLSClientConfig.MinVersion < 0x0303 {
		t.Fatalf("transport %+v", tr)
	}
}
