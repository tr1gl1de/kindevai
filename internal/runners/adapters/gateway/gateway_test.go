package gateway

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	auditdb "github.com/tr1gl1de/kindevai/internal/audit/adapters/sqlite"
	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/httpx"
	"github.com/tr1gl1de/kindevai/internal/platform/ratelimit"
	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runners/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/runners/app"
)

var owner = auth.Principal{UserID: "owner1", Login: "owner", Owner: true}

type projects struct{}

func (projects) Lookup(_ context.Context, id string) (app.ProjectRef, error) {
	return app.ProjectRef{ID: id, Name: id}, nil
}

type env struct {
	d      *sql.DB
	h      *Handler
	svc    *app.Service
	hub    *app.Hub
	srv    *httptest.Server
	url    string
	runner string
	key    ed25519.PrivateKey
}

type opts struct {
	cfg      Config
	messages map[string]MessageFunc
	sessions []SessionFunc
	limiter  *ratelimit.Limiter
	server   func(*http.Server) // adjust the HTTP server before it starts
}

// newEnv serves the gateway over a real SQLite database with one enrolled runner.
func newEnv(t *testing.T, o opts) *env {
	t.Helper()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(t.Context(), d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO users(id, login, password_hash, platform_role, created_at) VALUES ('owner1', 'owner', 'h', 'owner', 'x')",
		"INSERT INTO projects(id, slug, name, created_at) VALUES ('p1', 'alpha', 'Alpha', 'x')",
	} {
		if _, err := d.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	repo := sqlite.New(d)
	hub := app.NewHub()
	svc := &app.Service{Repo: repo, Projects: projects{}, Tx: db.Transactor{DB: d},
		Audit: &auditapp.Service{Repo: auditdb.New(d), Now: time.Now}, Rand: rand.Reader, Now: time.Now, NewID: rand.Text,
		Sessions: hub}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, tok, err := svc.CreateToken(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.Enroll(t.Context(), app.EnrollRequest{Token: tok, PublicKey: base64.StdEncoding.EncodeToString(pub),
		Hostname: "h", OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{
		Gateway: &app.Gateway{Runners: repo, Hub: hub, Rand: rand.Reader, Now: time.Now, NewID: rand.Text},
		Limiter: o.limiter, Config: o.cfg, Messages: o.messages, Sessions: o.sessions,
	}
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewUnstartedServer(mux)
	if o.server != nil {
		o.server(srv.Config)
	}
	srv.Config.RegisterOnShutdown(hub.Shutdown)
	srv.Start()
	t.Cleanup(func() { hub.Shutdown(); srv.Close() })
	return &env{d: d, h: h, svc: svc, hub: hub, srv: srv, url: "ws" + strings.TrimPrefix(srv.URL, "http") + protocol.ConnectPath,
		runner: res.Runner.ID, key: priv}
}

func (e *env) approve(t *testing.T) {
	t.Helper()
	if _, err := e.svc.Approve(t.Context(), owner, e.runner, []string{"p1"}); err != nil {
		t.Fatal(err)
	}
}

func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	c, resp, err := websocket.Dial(t.Context(), url, &websocket.DialOptions{Subprotocols: []string{protocol.Subprotocol}})
	if err != nil {
		t.Fatalf("dial: %v (%v)", err, resp)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

func readEnv(t *testing.T, c *websocket.Conn) (protocol.Envelope, error) {
	t.Helper()
	_, b, err := c.Read(t.Context())
	if err != nil {
		return protocol.Envelope{}, err
	}
	var e protocol.Envelope
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatal(err)
	}
	return e, nil
}

func send(t *testing.T, c *websocket.Conn, typ string, payload any) {
	t.Helper()
	raw, _ := json.Marshal(payload)
	b, _ := json.Marshal(protocol.Envelope{Type: typ, Payload: raw})
	if err := c.Write(t.Context(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func challenge(t *testing.T, c *websocket.Conn) protocol.Challenge {
	t.Helper()
	e, err := readEnv(t, c)
	if err != nil || e.Type != protocol.TypeChallenge {
		t.Fatalf("first frame %+v, %v", e, err)
	}
	var ch protocol.Challenge
	if err := json.Unmarshal(e.Payload, &ch); err != nil {
		t.Fatal(err)
	}
	return ch
}

func authMsg(t *testing.T, key ed25519.PrivateKey, runnerID string, ch protocol.Challenge) protocol.Authenticate {
	t.Helper()
	msg, err := protocol.AuthTranscript(protocol.AuthFields{RunnerID: runnerID, Audience: ch.Audience, Nonce: ch.Nonce,
		ExpiresAt: ch.ExpiresAt, ConnectionID: ch.ConnectionID})
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Authenticate{Version: protocol.ProtocolVersion, RunnerID: runnerID,
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, msg))}
}

// closeCode reads until the connection ends and returns the close code the server sent.
func closeCode(t *testing.T, c *websocket.Conn) int {
	t.Helper()
	for {
		if _, err := readEnv(t, c); err != nil {
			return int(websocket.CloseStatus(err))
		}
	}
}

// connect performs the whole handshake and returns the open, authorized connection.
func (e *env) connect(t *testing.T) (*websocket.Conn, protocol.Authorized) {
	t.Helper()
	c := dial(t, e.url)
	send(t, c, protocol.TypeAuthenticate, authMsg(t, e.key, e.runner, challenge(t, c)))
	m, err := readEnv(t, c)
	if err != nil || m.Type != protocol.TypeAuthorized {
		t.Fatalf("after authenticate: %+v %v (close %d)", m, err, websocket.CloseStatus(err))
	}
	var a protocol.Authorized
	_ = json.Unmarshal(m.Payload, &a)
	return c, a
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestChallengeResponseAuthorizesApprovedRunner(t *testing.T) {
	t.Parallel()
	e := newEnv(t, opts{})
	e.approve(t)
	c := dial(t, e.url)
	if c.Subprotocol() != protocol.Subprotocol {
		t.Fatalf("subprotocol %q", c.Subprotocol())
	}
	ch := challenge(t, c)
	nonce, err := base64.RawURLEncoding.DecodeString(ch.Nonce)
	if err != nil || len(nonce) != 32 || ch.Version != protocol.ProtocolVersion || ch.ConnectionID == "" ||
		ch.Audience != protocol.Audience(false, e.srv.Listener.Addr().String()) ||
		time.Until(time.Unix(ch.ExpiresAt, 0)) <= 0 {
		t.Fatalf("challenge %+v", ch)
	}
	send(t, c, protocol.TypeAuthenticate, authMsg(t, e.key, e.runner, ch))
	m, err := readEnv(t, c)
	if err != nil || m.Type != protocol.TypeAuthorized {
		t.Fatalf("got %+v, %v", m, err)
	}
	var a protocol.Authorized
	if err := json.Unmarshal(m.Payload, &a); err != nil || a.RunnerID != e.runner || a.SessionID != ch.ConnectionID {
		t.Fatalf("authorized %+v", a)
	}
	if !e.hub.Online(e.runner) {
		t.Fatal("runner not online")
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
	waitFor(t, "session cleanup", func() bool { return !e.hub.Online(e.runner) })
}

func TestImpostorAndReplayAreRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t, opts{})
	e.approve(t)
	_, impostor, _ := ed25519.GenerateKey(rand.Reader)
	c := dial(t, e.url)
	send(t, c, protocol.TypeAuthenticate, authMsg(t, impostor, e.runner, challenge(t, c)))
	if code := closeCode(t, c); code != protocol.CloseAuthFailed {
		t.Fatalf("impostor close %d", code)
	}

	// Capture a valid answer and replay it on a fresh connection.
	c1 := dial(t, e.url)
	good := authMsg(t, e.key, e.runner, challenge(t, c1))
	send(t, c1, protocol.TypeAuthenticate, good)
	if m, err := readEnv(t, c1); err != nil || m.Type != protocol.TypeAuthorized {
		t.Fatalf("legit runner: %+v %v", m, err)
	}
	c2 := dial(t, e.url)
	challenge(t, c2)
	send(t, c2, protocol.TypeAuthenticate, good)
	if code := closeCode(t, c2); code != protocol.CloseAuthFailed {
		t.Fatalf("replay close %d", code)
	}
}

func TestUnapprovedRunnersAreRejectedWithValidSignature(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		setup func(t *testing.T, e *env)
		code  int
	}{
		"pending": {func(*testing.T, *env) {}, protocol.CloseNotApproved},
		"disabled": {func(t *testing.T, e *env) {
			e.approve(t)
			_, _ = e.svc.Disable(t.Context(), owner, e.runner)
		}, protocol.CloseNotApproved},
		"revoked": {func(t *testing.T, e *env) { _, _ = e.svc.Revoke(t.Context(), owner, e.runner) }, protocol.CloseRevoked},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, opts{})
			tc.setup(t, e)
			c := dial(t, e.url)
			send(t, c, protocol.TypeAuthenticate, authMsg(t, e.key, e.runner, challenge(t, c)))
			if code := closeCode(t, c); code != tc.code {
				t.Fatalf("close %d, want %d", code, tc.code)
			}
		})
	}
}

func TestRevokeClosesLiveSessionWithinOneSecondAndBlocksReconnect(t *testing.T) {
	t.Parallel()
	e := newEnv(t, opts{})
	e.approve(t)
	c, _ := e.connect(t)
	start := time.Now()
	if _, err := e.svc.Revoke(t.Context(), owner, e.runner); err != nil {
		t.Fatal(err)
	}
	if code := closeCode(t, c); code != protocol.CloseRevoked {
		t.Fatalf("close %d", code)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("revocation took %v", d)
	}
	c2 := dial(t, e.url)
	send(t, c2, protocol.TypeAuthenticate, authMsg(t, e.key, e.runner, challenge(t, c2)))
	if code := closeCode(t, c2); code != protocol.CloseRevoked {
		t.Fatalf("reconnect close %d", code)
	}
}

func TestDisableClosesLiveSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t, opts{})
	e.approve(t)
	c, _ := e.connect(t)
	if _, err := e.svc.Disable(t.Context(), owner, e.runner); err != nil {
		t.Fatal(err)
	}
	if code := closeCode(t, c); code != protocol.CloseNotApproved {
		t.Fatalf("close %d", code)
	}
}

func TestProtocolViolationsClose(t *testing.T) {
	t.Parallel()
	e := newEnv(t, opts{cfg: Config{AuthTimeout: 200 * time.Millisecond}})
	e.approve(t)
	cases := map[string]struct {
		act  func(t *testing.T, c *websocket.Conn)
		code int
	}{
		"oversized frame": {func(t *testing.T, c *websocket.Conn) {
			challenge(t, c)
			_ = c.Write(t.Context(), websocket.MessageText, make([]byte, protocol.MaxMessageBytes+1))
		}, int(websocket.StatusMessageTooBig)},
		"not json": {func(t *testing.T, c *websocket.Conn) {
			challenge(t, c)
			_ = c.Write(t.Context(), websocket.MessageText, []byte("hello"))
		}, protocol.CloseProtocolError},
		"binary frame": {func(t *testing.T, c *websocket.Conn) {
			challenge(t, c)
			_ = c.Write(t.Context(), websocket.MessageBinary, []byte(`{"type":"runner.authenticate"}`))
		}, protocol.CloseProtocolError},
		"wrong first message": {func(t *testing.T, c *websocket.Conn) {
			challenge(t, c)
			send(t, c, "runner.heartbeat", map[string]int{})
		}, protocol.CloseProtocolError},
		"unknown field": {func(t *testing.T, c *websocket.Conn) {
			challenge(t, c)
			send(t, c, protocol.TypeAuthenticate, map[string]any{"version": 1, "runnerId": "x", "extra": 1})
		}, protocol.CloseProtocolError},
		"unsupported version": {func(t *testing.T, c *websocket.Conn) {
			a := authMsg(t, e.key, e.runner, challenge(t, c))
			a.Version = 2
			send(t, c, protocol.TypeAuthenticate, a)
		}, protocol.CloseUnsupportedVersion},
		"authentication timeout": {func(t *testing.T, c *websocket.Conn) { challenge(t, c) }, protocol.CloseAuthTimeout},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := dial(t, e.url)
			tc.act(t, c)
			if code := closeCode(t, c); code != tc.code {
				t.Fatalf("close %d, want %d", code, tc.code)
			}
		})
	}
}

func TestUnknownMessageAfterAuthorizationCloses(t *testing.T) {
	t.Parallel()
	e := newEnv(t, opts{})
	e.approve(t)
	c, _ := e.connect(t)
	send(t, c, "runner.nonsense", map[string]int{})
	if code := closeCode(t, c); code != protocol.CloseProtocolError {
		t.Fatalf("close %d", code)
	}
}

func TestKeepaliveClosesUnresponsivePeerAndServerTimeoutsDoNotApply(t *testing.T) {
	t.Parallel()
	e := newEnv(t, opts{
		cfg:    Config{PingInterval: 50 * time.Millisecond, PongTimeout: time.Second},
		server: func(s *http.Server) { s.ReadTimeout, s.WriteTimeout = 100*time.Millisecond, 100*time.Millisecond },
	})
	e.approve(t)
	c, _ := e.connect(t)
	// While the runner reads (and so answers pings) the session outlives the HTTP server's timeouts.
	go func() { _, _, _ = c.Read(context.WithoutCancel(t.Context())) }() // ends when the test closes c
	<-time.After(400 * time.Millisecond)
	if !e.hub.Online(e.runner) {
		t.Fatal("session ended early")
	}
	// A fresh connection that never reads cannot answer pings: the server drops it.
	c2, _ := e.connect(t)
	_ = c2
	waitFor(t, "keepalive to drop the silent runner", func() bool { return !e.hub.Online(e.runner) })
}

func TestHTTPChecksBeforeUpgrade(t *testing.T) {
	t.Parallel()
	e := newEnv(t, opts{limiter: ratelimit.New(ratelimit.Config{Burst: 3, Every: time.Hour, MaxKeys: 10}, time.Now)})
	httpURL := e.srv.URL + protocol.ConnectPath
	status := func(h http.Header) (int, string) {
		t.Helper()
		_, resp, err := websocket.Dial(t.Context(), httpURL, &websocket.DialOptions{HTTPHeader: h,
			Subprotocols: map[bool][]string{true: nil, false: {protocol.Subprotocol}}[h.Get("X-No-Proto") != ""]})
		if err == nil || resp == nil {
			t.Fatalf("dial succeeded: %v", err)
		}
		var body protocol.Error
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body.Error
	}
	if code, msg := status(http.Header{"X-No-Proto": {"1"}}); code != http.StatusUpgradeRequired || msg != protocol.ErrCodeUnsupportedProto {
		t.Fatalf("no subprotocol: %d %s", code, msg)
	}
	if code, msg := status(http.Header{"Origin": {"https://evil.example"}}); code != http.StatusForbidden || msg != protocol.ErrCodeBrowserNotAllowed {
		t.Fatalf("browser origin: %d %s", code, msg)
	}
	// Refused requests above did not spend the per-address budget (burst 3); well-formed attempts do.
	for range 3 {
		dial(t, e.url)
	}
	if code, _ := status(nil); code != http.StatusTooManyRequests {
		t.Fatalf("rate limit: %d", code)
	}
}

func TestConnectionLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t, opts{cfg: Config{MaxConnections: 1}})
	dial(t, e.url)
	_, resp, err := websocket.Dial(t.Context(), e.url, &websocket.DialOptions{Subprotocols: []string{protocol.Subprotocol}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second connection: %v %v", resp, err)
	}
}

func TestPlainWSOnlyFromLoopback(t *testing.T) {
	t.Parallel()
	trusted, _ := httpx.ParseTrustedProxies("172.29.240.0/24")
	h := &Handler{Config: Config{PlainPeers: []netip.Prefix{netip.MustParsePrefix("172.29.241.1/32")}}}
	cases := []struct {
		name, peer string
		headers    map[string]string
		tls        bool
		ok         bool
	}{
		{"loopback v4", "127.0.0.1:5000", nil, false, true},
		{"loopback v6", "[::1]:5000", nil, false, true},
		{"remote plain", "203.0.113.7:5000", nil, false, false},
		{"configured local peer (docker gateway)", "172.29.241.1:5000", nil, false, true},
		{"loopback proxy forwarding a remote client", "127.0.0.1:5000", map[string]string{"X-Forwarded-For": "203.0.113.7"}, false, false},
		{"remote with direct TLS", "203.0.113.7:5000", nil, true, true},
		{"trusted proxy terminating TLS", "172.29.240.5:5000", map[string]string{"X-Forwarded-For": "203.0.113.7", "X-Forwarded-Proto": "https"}, false, true},
		{"trusted proxy over plain http", "172.29.240.5:5000", map[string]string{"X-Forwarded-For": "203.0.113.7", "X-Forwarded-Proto": "http"}, false, false},
		{"untrusted peer claiming https", "203.0.113.7:5000", map[string]string{"X-Forwarded-Proto": "https"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, protocol.ConnectPath, nil)
			r.RemoteAddr = tc.peer
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			var err error
			trusted.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { err = h.transportAllowed(r) })).
				ServeHTTP(httptest.NewRecorder(), r)
			if (err == nil) != tc.ok {
				t.Fatalf("allowed = %v, want %v", err == nil, tc.ok)
			}
		})
	}
	// The refusal is an explicit HTTP 403 with a clear code, before any upgrade.
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, protocol.ConnectPath, nil)
	r.RemoteAddr = "203.0.113.7:5000"
	w := httptest.NewRecorder()
	h.connect(w, r)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), protocol.ErrCodePlainRemote) ||
		!strings.Contains(w.Body.String(), "wss://") {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
}

func TestShutdownClosesSessions(t *testing.T) {
	t.Parallel()
	e := newEnv(t, opts{})
	e.approve(t)
	c, _ := e.connect(t)
	e.hub.Shutdown()
	if code := closeCode(t, c); code != int(websocket.StatusGoingAway) {
		t.Fatalf("close %d", code)
	}
}

// A revoked runner that ignores the close frame is cut off anyway: nothing it sends afterwards is dispatched
// and its TCP connection is gone within the close grace period.
func TestRevokeCutsOffAnUncooperativeRunner(t *testing.T) {
	t.Parallel()
	var handled atomic.Int32
	e := newEnv(t, opts{messages: map[string]MessageFunc{"runner.test": func(context.Context, app.Session, protocol.Envelope) error {
		handled.Add(1)
		return nil
	}}})
	e.approve(t)
	c, _ := e.connect(t)
	send(t, c, "runner.test", map[string]int{}) // dispatched while approved
	waitFor(t, "the message to be handled", func() bool { return handled.Load() == 1 })
	if _, err := e.svc.Revoke(t.Context(), owner, e.runner); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	send(t, c, "runner.test", map[string]int{}) // the runner never reads, so it never answers the close frame
	waitFor(t, "the connection handler to end", func() bool { return e.h.Live() == 0 })
	if d := time.Since(start); d > closeGrace+time.Second {
		t.Fatalf("connection lingered %v", d)
	}
	if n := handled.Load(); n != 1 {
		t.Fatalf("revoked runner's message was dispatched (%d)", n)
	}
}

func TestForeignAudienceIsRefusedWhenOriginsAreConfigured(t *testing.T) {
	t.Parallel()
	e := newEnv(t, opts{cfg: Config{Audiences: []string{"https://cp.example.com"}}})
	_, resp, err := websocket.Dial(t.Context(), e.url, &websocket.DialOptions{Subprotocols: []string{protocol.Subprotocol}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("relay with its own Host: %v %v", resp, err)
	}
	ok := newEnv(t, opts{})
	ok.h.Config.Audiences = []string{protocol.Audience(false, ok.srv.Listener.Addr().String())}
	ok.approve(t)
	ok.connect(t)
}

func TestWaitDrainsConnectionsAfterShutdown(t *testing.T) {
	t.Parallel()
	e := newEnv(t, opts{})
	e.approve(t)
	c, _ := e.connect(t)
	e.hub.Shutdown()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go func() { _ = closeCode(t, c) }() // a cooperative runner answers the close
	if err := e.h.Wait(ctx); err != nil || e.h.Live() != 0 {
		t.Fatalf("Wait = %v, live %d", err, e.h.Live())
	}
}

// stallingHijack holds the connection handler right after the HTTP server handed the connection over, before the
// WebSocket handshake goes on.
type stallingHijack struct {
	http.ResponseWriter
	hijacked, release chan struct{}
}

func (s *stallingHijack) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, brw, err := http.NewResponseController(s.ResponseWriter).Hijack()
	close(s.hijacked)
	<-s.release
	return c, brw, err
}

// awaitHijack returns once the stalled handler has hijacked the connection, or with the dial's error if the dial
// failed first. A successful dial is not a failure: websocket.Accept writes the 101 before it hijacks and net/http
// flushes it inside Hijack, so the runner can finish its dial just before close(hijacked) runs (KIN-94). The
// handler has then already reached Hijack, so waiting for hijacked cannot hang.
func awaitHijack(hijacked <-chan struct{}, dialed <-chan error) error {
	select {
	case <-hijacked:
		return nil
	case err := <-dialed:
		if err != nil {
			return err
		}
		<-hijacked
		return nil
	}
}

// KIN-74: srv.Shutdown stops tracking a connection once it is hijacked, so the gateway must already count it then;
// otherwise Wait (called after Shutdown) returns while that handler still runs and races its WaitGroup.Add.
func TestWaitCountsAConnectionHijackedDuringShutdown(t *testing.T) {
	t.Parallel()
	hijacked, release := make(chan struct{}), make(chan struct{})
	e := newEnv(t, opts{server: func(s *http.Server) {
		next := s.Handler
		s.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(&stallingHijack{ResponseWriter: w, hijacked: hijacked, release: release}, r)
		})
	}})
	var once sync.Once
	unstall := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unstall)
	dialed := make(chan error, 1)
	go func() {
		c, _, err := websocket.Dial(t.Context(), e.url, &websocket.DialOptions{Subprotocols: []string{protocol.Subprotocol}})
		if err == nil {
			_ = c.CloseNow()
		}
		dialed <- err
	}()
	if err := awaitHijack(hijacked, dialed); err != nil {
		t.Fatalf("dial ended before the upgrade: %v", err)
	}
	if err := e.srv.Config.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := e.h.Live(); n != 1 {
		t.Fatalf("live = %d after Shutdown with a hijacked connection, want 1", n)
	}
	unstall()
	if err := e.h.Wait(t.Context()); err != nil || e.h.Live() != 0 {
		t.Fatalf("Wait = %v, live %d", err, e.h.Live())
	}
}

// KIN-94: when the dial has already succeeded by the time the test looks, a select between the two outcomes chose
// at random and failed about half the time. Both outcomes are ready here on every round, so a random pick would fail.
func TestAwaitHijackAcceptsADialThatFinishedFirst(t *testing.T) {
	t.Parallel()
	hijacked := make(chan struct{})
	close(hijacked)
	for range 100 {
		dialed := make(chan error, 1)
		dialed <- nil
		if err := awaitHijack(hijacked, dialed); err != nil {
			t.Fatalf("awaitHijack = %v, want nil", err)
		}
	}
	failed := make(chan error, 1)
	failed <- errors.New("refused")
	if err := awaitHijack(make(chan struct{}), failed); err == nil {
		t.Fatal("a failed dial must be reported")
	}
}
