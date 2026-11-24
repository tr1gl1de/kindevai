package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/domain"
)

// fakeDialer answers each Dial from a script: an error, or a link that ends with the given error.
type fakeDialer struct {
	mu      sync.Mutex
	script  []dialResult
	answers []Answer
	cancel  context.CancelFunc // called when the script runs out
}

type dialResult struct {
	err     error
	linkErr error
	ch      *Challenge
}

type fakeLink struct{ err error }

func (l fakeLink) Wait(context.Context) error { return l.err }

func (d *fakeDialer) Dial(ctx context.Context, _ domain.ServerURL, answer func(Challenge) (Answer, error)) (Link, error) {
	d.mu.Lock()
	if len(d.script) == 0 {
		d.mu.Unlock()
		d.cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	r := d.script[0]
	d.script = d.script[1:]
	d.mu.Unlock()
	if r.ch != nil {
		a, err := answer(*r.ch)
		if err != nil {
			return nil, err
		}
		d.mu.Lock()
		d.answers = append(d.answers, a)
		d.mu.Unlock()
	}
	if r.err != nil {
		return nil, r.err
	}
	return fakeLink{r.linkErr}, nil
}

type connFixture struct {
	c      *Connector
	d      *fakeDialer
	sleeps []time.Duration
	e      domain.Enrollment
	id     domain.Identity
	now    time.Time
}

func newConnFixture(t *testing.T, script ...dialResult) (*connFixture, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	srv, _ := domain.ParseServerURL("https://cp.example.com")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	id, _ := domain.NewIdentity(priv)
	f := &connFixture{d: &fakeDialer{script: script, cancel: cancel}, id: id, now: time.Unix(1_800_000_000, 0),
		e: domain.Enrollment{Server: srv, RunnerID: "r1", Fingerprint: id.Fingerprint()}}
	f.c = &Connector{
		Dialer: f.d,
		Now:    func() time.Time { return f.now },
		Sleep: func(ctx context.Context, d time.Duration) error {
			f.sleeps = append(f.sleeps, d)
			f.now = f.now.Add(d)
			return ctx.Err()
		},
		Rand: func() float64 { return 0 },
	}
	return f, ctx
}

func challengeFor(e domain.Enrollment) *Challenge {
	return &Challenge{Version: protocol.ProtocolVersion, Nonce: base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		ExpiresAt: 1_800_000_030, Audience: "https://cp.example.com", ConnectionID: "c1"}
}

func TestConnectorSignsTheCanonicalTranscript(t *testing.T) {
	t.Parallel()
	f, ctx := newConnFixture(t)
	ch := challengeFor(f.e)
	f.d.script = []dialResult{{ch: ch, linkErr: errors.New("dropped")}}
	if err := f.c.Run(ctx, f.e, f.id); err != nil {
		t.Fatal(err)
	}
	if len(f.d.answers) != 1 {
		t.Fatalf("answers %d", len(f.d.answers))
	}
	a := f.d.answers[0]
	msg, _ := protocol.AuthTranscript(protocol.AuthFields{RunnerID: "r1", Audience: ch.Audience, Nonce: ch.Nonce,
		ExpiresAt: ch.ExpiresAt, ConnectionID: ch.ConnectionID})
	if a.RunnerID != "r1" || a.Fingerprint != f.id.Fingerprint() || !ed25519.Verify(f.id.PublicKey(), msg, a.Signature) {
		t.Fatalf("answer %+v does not sign the transcript", a)
	}
}

func TestConnectorRefusesForeignAudienceAndVersion(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		mut  func(*Challenge)
		want error
	}{
		"relayed from another server": {func(c *Challenge) { c.Audience = "https://evil.example.com" }, domain.ErrAudienceMismatch},
		"newer protocol":              {func(c *Challenge) { c.Version = 2 }, domain.ErrUnsupportedProtocol},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, ctx := newConnFixture(t)
			ch := challengeFor(f.e)
			tc.mut(ch)
			f.d.script = []dialResult{{ch: ch}}
			if err := f.c.Run(ctx, f.e, f.id); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if len(f.d.answers) != 0 {
				t.Fatal("signed a challenge it should refuse")
			}
		})
	}
}

func TestConnectorReconnectsWithGrowingJitteredBackoff(t *testing.T) {
	t.Parallel()
	f, ctx := newConnFixture(t,
		dialResult{err: errors.New("connection refused")},
		dialResult{err: domain.ErrNotApproved},
		dialResult{err: domain.ErrAuthRejected},
		dialResult{linkErr: errors.New("dropped right away")},
	)
	if err := f.c.Run(ctx, f.e, f.id); err != nil {
		t.Fatalf("Run = %v, want nil after cancellation", err)
	}
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second}
	if len(f.sleeps) != len(want) {
		t.Fatalf("sleeps %v, want %v", f.sleeps, want)
	}
	for i := range want {
		if f.sleeps[i] != want[i] {
			t.Fatalf("sleeps %v, want %v", f.sleeps, want)
		}
	}
}

// stableLink stays up for a while (advancing the fake clock) before it drops.
type stableLink struct {
	f    *connFixture
	for_ time.Duration
}

func (l stableLink) Wait(context.Context) error {
	l.f.now = l.f.now.Add(l.for_)
	return errors.New("dropped")
}

type stableDialer struct {
	f     *connFixture
	n     int
	limit int
	stop  context.CancelFunc
}

func (d *stableDialer) Dial(ctx context.Context, _ domain.ServerURL, _ func(Challenge) (Answer, error)) (Link, error) {
	d.n++
	if d.n > d.limit {
		d.stop()
		return nil, ctx.Err()
	}
	return stableLink{f: d.f, for_: 10 * time.Minute}, nil
}

func TestConnectorResetsBackoffAfterAStableSession(t *testing.T) {
	t.Parallel()
	f, ctx := newConnFixture(t)
	ctx, cancel := context.WithCancel(ctx)
	f.c.Dialer = &stableDialer{f: f, limit: 3, stop: cancel}
	if err := f.c.Run(ctx, f.e, f.id); err != nil {
		t.Fatal(err)
	}
	for _, s := range f.sleeps {
		if s != 500*time.Millisecond {
			t.Fatalf("backoff not reset after stable sessions: %v", f.sleeps)
		}
	}
}

func TestConnectorStopsOnFatalErrors(t *testing.T) {
	t.Parallel()
	for _, fatal := range []error{domain.ErrRunnerRevoked, domain.ErrTLSVerification, domain.ErrUnsupportedProtocol, domain.ErrPlainHTTP} {
		t.Run(fatal.Error(), func(t *testing.T) {
			t.Parallel()
			f, ctx := newConnFixture(t, dialResult{err: fatal}, dialResult{err: errors.New("must not be reached")})
			if err := f.c.Run(ctx, f.e, f.id); !errors.Is(err, fatal) {
				t.Fatalf("Run = %v, want %v", err, fatal)
			}
			if len(f.sleeps) != 0 {
				t.Fatal("retried a fatal error")
			}
		})
	}
	f, ctx := newConnFixture(t, dialResult{linkErr: domain.ErrRunnerRevoked})
	if err := f.c.Run(ctx, f.e, f.id); !errors.Is(err, domain.ErrRunnerRevoked) {
		t.Fatalf("revocation of a live session = %v", err)
	}
}

// KIN-63 fail-closed: told that it was disabled (4403) or revoked (4410), on the live link or at the handshake, the
// runner stops its attempts itself, without waiting for an execution.cancel; other closes leave them running.
func TestDisabledOrRevokedRunnerHaltsItsAttempts(t *testing.T) {
	t.Parallel()
	f, ctx := newConnFixture(t,
		dialResult{linkErr: errors.New("connection reset")},
		dialResult{linkErr: domain.ErrNotApproved},
		dialResult{err: domain.ErrNotApproved},
		dialResult{err: domain.ErrRunnerRevoked})
	var halts []string
	f.c.Halt = func(reason string) { halts = append(halts, reason) }
	if err := f.c.Run(ctx, f.e, f.id); !errors.Is(err, domain.ErrRunnerRevoked) {
		t.Fatalf("run = %v", err)
	}
	want := []string{protocol.CancelRunnerDisabled, protocol.CancelRunnerDisabled, protocol.CancelRunnerRevoked}
	if len(halts) != len(want) || halts[0] != want[0] || halts[1] != want[1] || halts[2] != want[2] {
		t.Fatalf("halts = %v, want %v", halts, want)
	}
}
