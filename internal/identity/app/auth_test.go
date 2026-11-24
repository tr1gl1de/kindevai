package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/identity/domain"
)

type fakeUsers struct{ byLogin map[domain.Login]domain.User }

func (f *fakeUsers) ByLogin(_ context.Context, l domain.Login) (domain.User, error) {
	u, ok := f.byLogin[l]
	if !ok {
		return domain.User{}, domain.ErrNotFound
	}
	return u, nil
}

func (f *fakeUsers) ByID(_ context.Context, id string) (domain.User, error) {
	for _, u := range f.byLogin {
		if u.ID == id {
			return u, nil
		}
	}
	return domain.User{}, domain.ErrNotFound
}

type fakeSessions struct {
	mu      sync.Mutex
	byHash  map[string]domain.Session
	touches int
	purged  int
}

func (f *fakeSessions) Create(_ context.Context, s domain.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byHash[string(s.TokenHash)] = s
	return nil
}

func (f *fakeSessions) ByTokenHash(_ context.Context, h []byte) (domain.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.byHash[string(h)]
	if !ok {
		return domain.Session{}, domain.ErrNotFound
	}
	return s, nil
}

func (f *fakeSessions) Touch(_ context.Context, h []byte, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.byHash[string(h)]
	s.LastSeenAt = at
	f.byHash[string(h)] = s
	f.touches++
	return nil
}

func (f *fakeSessions) Delete(_ context.Context, h []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.byHash, string(h))
	return nil
}

func (f *fakeSessions) DeleteExpired(_ context.Context, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, s := range f.byHash {
		if !s.Valid(now) {
			delete(f.byHash, k)
			f.purged++
		}
	}
	return nil
}

type fakeVerifier struct{ dummy int }

func (v *fakeVerifier) Verify(_ context.Context, hash, pw string) (bool, error) {
	return hash == "h:"+pw, nil
}
func (v *fakeVerifier) DummyVerify(context.Context, string) { v.dummy++ }

type authFixture struct {
	svc      *AuthService
	sessions *fakeSessions
	verifier *fakeVerifier
	clock    *clock
}

func newAuthFixture() *authFixture {
	f := &authFixture{
		sessions: &fakeSessions{byHash: map[string]domain.Session{}},
		verifier: &fakeVerifier{},
		clock:    &clock{time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)},
	}
	users := &fakeUsers{byLogin: map[domain.Login]domain.User{
		"owner": {ID: "u1", Login: "owner", PasswordHash: "h:correct horse battery", Role: domain.RoleOwner},
		"gone":  {ID: "u2", Login: "gone", PasswordHash: "h:correct horse battery", Role: domain.RoleUser, Disabled: true},
	}}
	f.svc = &AuthService{Users: users, Sessions: f.sessions, Passwords: f.verifier, Now: f.clock.Now, Rand: rand.Reader}
	return f
}

func TestLoginAndAuthenticate(t *testing.T) {
	t.Parallel()
	f := newAuthFixture()
	tok, u, err := f.svc.Login(t.Context(), " Owner ", "correct horse battery")
	if err != nil || u.ID != "u1" || tok == "" {
		t.Fatalf("Login = %q %+v %v", tok, u, err)
	}
	for k := range f.sessions.byHash {
		if bytes.Contains([]byte(k), []byte(tok)) {
			t.Fatal("plaintext token stored")
		}
	}
	got, err := f.svc.Authenticate(t.Context(), tok)
	if err != nil || got.ID != "u1" {
		t.Fatalf("Authenticate = %+v %v", got, err)
	}
}

func TestLoginFailuresLookAlike(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, login, pw string }{
		{"wrong password", "owner", "wrong password!!"},
		{"unknown login", "nobody", "correct horse battery"},
		{"invalid login format", "x", "correct horse battery"},
		{"disabled user", "gone", "correct horse battery"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newAuthFixture()
			if _, _, err := f.svc.Login(t.Context(), tt.login, tt.pw); !errors.Is(err, domain.ErrInvalidCredentials) {
				t.Fatalf("err = %v", err)
			}
			if len(f.sessions.byHash) != 0 {
				t.Fatal("session created on failed login")
			}
		})
	}
	f := newAuthFixture()
	_, _, _ = f.svc.Login(t.Context(), "nobody", "pw-pw-pw-pw-pw")
	if f.verifier.dummy != 1 {
		t.Fatal("unknown login did not spend a dummy hash (timing leak)")
	}
}

func TestAuthenticateRejects(t *testing.T) {
	t.Parallel()
	f := newAuthFixture()
	tok, _, _ := f.svc.Login(t.Context(), "owner", "correct horse battery")
	if _, err := f.svc.Authenticate(t.Context(), ""); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("empty token: %v", err)
	}
	if _, err := f.svc.Authenticate(t.Context(), tok+"x"); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("unknown token: %v", err)
	}
	f.clock.t = f.clock.t.Add(domain.SessionIdleTTL)
	if _, err := f.svc.Authenticate(t.Context(), tok); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("idle session: %v", err)
	}
	if len(f.sessions.byHash) != 0 {
		t.Fatal("expired session not deleted")
	}
}

func TestAuthenticateTouchesAtMostOncePerMinute(t *testing.T) {
	t.Parallel()
	f := newAuthFixture()
	tok, _, _ := f.svc.Login(t.Context(), "owner", "correct horse battery")
	for range 5 {
		_, _ = f.svc.Authenticate(t.Context(), tok)
	}
	if f.sessions.touches != 0 {
		t.Fatalf("touched %d times within a minute", f.sessions.touches)
	}
	f.clock.t = f.clock.t.Add(2 * time.Minute)
	_, _ = f.svc.Authenticate(t.Context(), tok)
	if f.sessions.touches != 1 {
		t.Fatalf("touches = %d, want 1", f.sessions.touches)
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	t.Parallel()
	f := newAuthFixture()
	tok, _, _ := f.svc.Login(t.Context(), "owner", "correct horse battery")
	if err := f.svc.Logout(t.Context(), tok); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Authenticate(t.Context(), tok); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("after logout: %v", err)
	}
}

func TestLoginPurgesExpiredSessions(t *testing.T) {
	t.Parallel()
	f := newAuthFixture()
	_, _, _ = f.svc.Login(t.Context(), "owner", "correct horse battery")
	f.clock.t = f.clock.t.Add(domain.SessionMaxAge)
	_, _, _ = f.svc.Login(t.Context(), "owner", "correct horse battery")
	if f.sessions.purged != 1 || len(f.sessions.byHash) != 1 {
		t.Fatalf("purged = %d, left = %d", f.sessions.purged, len(f.sessions.byHash))
	}
}

func TestAuthenticateRejectsDisabledUser(t *testing.T) {
	t.Parallel()
	f := newAuthFixture()
	plain, s2, _ := domain.NewSession(rand.Reader, "u2", f.clock.t)
	_ = f.sessions.Create(t.Context(), s2)
	if _, err := f.svc.Authenticate(t.Context(), plain); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatalf("disabled user session: %v", err)
	}
}
