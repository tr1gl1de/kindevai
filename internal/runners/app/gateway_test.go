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
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

// gwFixture is a gateway over the service fixture with one enrolled runner whose private key the test holds.
type gwFixture struct {
	*fixture
	gw     *Gateway
	runner domain.Runner
	key    ed25519.PrivateKey
}

func newGateway(t *testing.T) *gwFixture {
	t.Helper()
	f := newFixture(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	r := f.enrolled(t, base64.StdEncoding.EncodeToString(pub))
	hub := NewHub()
	f.svc.Sessions = hub
	gw := &Gateway{Runners: f.repo, Hub: hub, Rand: rand.Reader, Now: func() time.Time { return f.now }, NewID: f.svc.NewID}
	return &gwFixture{fixture: f, gw: gw, runner: r, key: priv}
}

func (g *gwFixture) approve(t *testing.T) {
	t.Helper()
	if _, err := g.svc.Approve(t.Context(), owner, g.runner.ID, []string{"p1"}); err != nil {
		t.Fatal(err)
	}
}

func sign(t *testing.T, key ed25519.PrivateKey, runnerID string, ch *domain.Challenge) Credentials {
	t.Helper()
	msg, err := protocol.AuthTranscript(protocol.AuthFields{RunnerID: runnerID, Audience: ch.Audience,
		Nonce: base64.RawURLEncoding.EncodeToString(ch.Nonce), ExpiresAt: ch.ExpiresAt.Unix(), ConnectionID: ch.ConnectionID})
	if err != nil {
		t.Fatal(err)
	}
	return Credentials{RunnerID: runnerID, Signature: ed25519.Sign(key, msg)}
}

// closer records how the hub closed a session.
type closer struct {
	mu      sync.Mutex
	reasons []DisconnectReason
}

func (c *closer) close(r DisconnectReason) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reasons = append(c.reasons, r)
}

func (c *closer) got() []DisconnectReason {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]DisconnectReason(nil), c.reasons...)
}

func TestGatewayAuthorizesApprovedRunnerWithValidSignature(t *testing.T) {
	t.Parallel()
	g := newGateway(t)
	g.approve(t)
	ch, err := g.gw.NewChallenge("https://cp")
	if err != nil {
		t.Fatal(err)
	}
	s, err := g.gw.Authenticate(t.Context(), ch, sign(t, g.key, g.runner.ID, ch), (&closer{}).close)
	if err != nil {
		t.Fatal(err)
	}
	if s.RunnerID != g.runner.ID || s.ID != ch.ConnectionID || !g.gw.Hub.Online(g.runner.ID) {
		t.Fatalf("session %+v", s)
	}
	if r, _ := g.repo.Runner(t.Context(), g.runner.ID); r.LastSeenAt == nil || !r.LastSeenAt.Equal(g.now) {
		t.Fatalf("lastSeenAt = %v", r.LastSeenAt)
	}
	s.Detach()
	if g.gw.Hub.Online(g.runner.ID) {
		t.Fatal("detached session still online")
	}
}

func TestGatewayRejectsImpostorsAndReplays(t *testing.T) {
	t.Parallel()
	g := newGateway(t)
	g.approve(t)
	_, impostor, _ := ed25519.GenerateKey(rand.Reader)

	ch, _ := g.gw.NewChallenge("https://cp")
	if _, err := g.gw.Authenticate(t.Context(), ch, sign(t, impostor, g.runner.ID, ch), (&closer{}).close); !errors.Is(err, domain.ErrBadSignature) {
		t.Fatalf("impostor = %v", err)
	}
	ch2, _ := g.gw.NewChallenge("https://cp")
	if _, err := g.gw.Authenticate(t.Context(), ch2, sign(t, g.key, "unknown", ch2), (&closer{}).close); !errors.Is(err, domain.ErrBadSignature) {
		t.Fatalf("unknown runner = %v", err)
	}

	// A valid signature captured on one connection is useless on another (different nonce and connection id)
	// and on the same one a second time.
	ch3, _ := g.gw.NewChallenge("https://cp")
	good := sign(t, g.key, g.runner.ID, ch3)
	if _, err := g.gw.Authenticate(t.Context(), ch3, good, (&closer{}).close); err != nil {
		t.Fatal(err)
	}
	if _, err := g.gw.Authenticate(t.Context(), ch3, good, (&closer{}).close); !errors.Is(err, domain.ErrChallengeUsed) {
		t.Fatalf("replay on the same connection = %v", err)
	}
	ch4, _ := g.gw.NewChallenge("https://cp")
	if _, err := g.gw.Authenticate(t.Context(), ch4, good, (&closer{}).close); !errors.Is(err, domain.ErrBadSignature) {
		t.Fatalf("replay on a new connection = %v", err)
	}
	// Signed for another server: the audience is part of the transcript.
	ch5, _ := g.gw.NewChallenge("https://cp")
	other := *ch5
	other.Audience = "https://evil"
	if _, err := g.gw.Authenticate(t.Context(), ch5, sign(t, g.key, g.runner.ID, &other), (&closer{}).close); !errors.Is(err, domain.ErrBadSignature) {
		t.Fatalf("other audience = %v", err)
	}
	// A wrong public key reference fails even with a valid signature.
	ch6, _ := g.gw.NewChallenge("https://cp")
	cred := sign(t, g.key, g.runner.ID, ch6)
	cred.Fingerprint = "SHA256:other"
	if _, err := g.gw.Authenticate(t.Context(), ch6, cred, (&closer{}).close); !errors.Is(err, domain.ErrBadSignature) {
		t.Fatalf("wrong fingerprint = %v", err)
	}
}

func TestGatewayRejectsExpiredChallenge(t *testing.T) {
	t.Parallel()
	g := newGateway(t)
	g.approve(t)
	ch, _ := g.gw.NewChallenge("https://cp")
	g.now = g.now.Add(domain.ChallengeTTL)
	if _, err := g.gw.Authenticate(t.Context(), ch, sign(t, g.key, g.runner.ID, ch), (&closer{}).close); !errors.Is(err, domain.ErrChallengeExpired) {
		t.Fatalf("expired = %v", err)
	}
}

func TestGatewayRejectsUnapprovedRunnersEvenWithValidSignature(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		setup func(t *testing.T, g *gwFixture)
		want  error
	}{
		"pending": {func(*testing.T, *gwFixture) {}, domain.ErrNotApproved},
		"disabled": {func(t *testing.T, g *gwFixture) {
			g.approve(t)
			if _, err := g.svc.Disable(t.Context(), owner, g.runner.ID); err != nil {
				t.Fatal(err)
			}
		}, domain.ErrNotApproved},
		"revoked": {func(t *testing.T, g *gwFixture) {
			g.approve(t)
			if _, err := g.svc.Revoke(t.Context(), owner, g.runner.ID); err != nil {
				t.Fatal(err)
			}
		}, domain.ErrRunnerRevoked},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := newGateway(t)
			tc.setup(t, g)
			ch, _ := g.gw.NewChallenge("https://cp")
			if _, err := g.gw.Authenticate(t.Context(), ch, sign(t, g.key, g.runner.ID, ch), (&closer{}).close); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if g.gw.Hub.Online(g.runner.ID) {
				t.Fatal("rejected runner is online")
			}
		})
	}
}

func TestDisableAndRevokeCloseTheLiveSession(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		act  func(ctx context.Context, s *Service, id string) error
		want DisconnectReason
	}{
		"disable": {func(ctx context.Context, s *Service, id string) error {
			_, err := s.Disable(ctx, owner, id)
			return err
		}, ReasonDisabled},
		"revoke": {func(ctx context.Context, s *Service, id string) error { _, err := s.Revoke(ctx, owner, id); return err }, ReasonRevoked},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := newGateway(t)
			g.approve(t)
			ch, _ := g.gw.NewChallenge("https://cp")
			c := &closer{}
			if _, err := g.gw.Authenticate(t.Context(), ch, sign(t, g.key, g.runner.ID, ch), c.close); err != nil {
				t.Fatal(err)
			}
			if err := tc.act(t.Context(), g.svc, g.runner.ID); err != nil {
				t.Fatal(err)
			}
			if got := c.got(); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("close reasons = %v, want [%v]", got, tc.want)
			}
			if g.gw.Hub.Online(g.runner.ID) {
				t.Fatal("still online")
			}
		})
	}
}

func TestNewerConnectionReplacesTheOlder(t *testing.T) {
	t.Parallel()
	g := newGateway(t)
	g.approve(t)
	first, second := &closer{}, &closer{}
	ch, _ := g.gw.NewChallenge("https://cp")
	s1, err := g.gw.Authenticate(t.Context(), ch, sign(t, g.key, g.runner.ID, ch), first.close)
	if err != nil {
		t.Fatal(err)
	}
	ch2, _ := g.gw.NewChallenge("https://cp")
	if _, err := g.gw.Authenticate(t.Context(), ch2, sign(t, g.key, g.runner.ID, ch2), second.close); err != nil {
		t.Fatal(err)
	}
	if got := first.got(); len(got) != 1 || got[0] != ReasonReplaced {
		t.Fatalf("old session closes = %v", got)
	}
	s1.Detach() // the old connection's cleanup must not drop the new session
	if !g.gw.Hub.Online(g.runner.ID) {
		t.Fatal("new session dropped by the old one's detach")
	}
}

// statusFlip revokes the runner right after the gateway's first status read, before the session registers.
type statusFlip struct {
	*memRepo
	once sync.Once
	flip func()
}

func (s *statusFlip) Runner(ctx context.Context, id string) (domain.Runner, error) {
	r, err := s.memRepo.Runner(ctx, id)
	s.once.Do(s.flip)
	return r, err
}

func TestRevocationRacingAnAuthenticationWins(t *testing.T) {
	t.Parallel()
	g := newGateway(t)
	g.approve(t)
	g.gw.Runners = &statusFlip{memRepo: g.repo, flip: func() {
		if _, err := g.svc.Revoke(t.Context(), owner, g.runner.ID); err != nil {
			t.Error(err)
		}
	}}
	ch, _ := g.gw.NewChallenge("https://cp")
	if _, err := g.gw.Authenticate(t.Context(), ch, sign(t, g.key, g.runner.ID, ch), (&closer{}).close); !errors.Is(err, domain.ErrRunnerRevoked) {
		t.Fatalf("err = %v, want ErrRunnerRevoked", err)
	}
	if g.gw.Hub.Online(g.runner.ID) {
		t.Fatal("revoked runner stayed online")
	}
}

func TestHubShutdownClosesAllAndRefusesNew(t *testing.T) {
	t.Parallel()
	g := newGateway(t)
	g.approve(t)
	c := &closer{}
	ch, _ := g.gw.NewChallenge("https://cp")
	if _, err := g.gw.Authenticate(t.Context(), ch, sign(t, g.key, g.runner.ID, ch), c.close); err != nil {
		t.Fatal(err)
	}
	g.gw.Hub.Shutdown()
	if got := c.got(); len(got) != 1 || got[0] != ReasonShutdown {
		t.Fatalf("closes = %v", got)
	}
	ch2, _ := g.gw.NewChallenge("https://cp")
	if _, err := g.gw.Authenticate(t.Context(), ch2, sign(t, g.key, g.runner.ID, ch2), c.close); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("after shutdown = %v", err)
	}
}
