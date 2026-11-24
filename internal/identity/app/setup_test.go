package app

import (
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/identity/domain"
)

// fakeRepo is an in-memory SetupRepository; InTx serialises like a write transaction and rolls back on error.
type fakeRepo struct {
	mu       sync.Mutex
	writeTxs int
	state    domain.SetupState
	users    []domain.User
	name     domain.PlatformName
}

func (r *fakeRepo) InTx(ctx context.Context, fn func(SetupTx) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writeTxs++
	state, users, name := r.state, r.users, r.name
	if err := fn(&fakeTx{r}); err != nil {
		r.state, r.users, r.name = state, users, name
		return err
	}
	return nil
}

func (r *fakeRepo) State(context.Context) (domain.SetupState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state, nil
}

type fakeTx struct{ r *fakeRepo }

func (t *fakeTx) SetupState(context.Context) (domain.SetupState, error) { return t.r.state, nil }
func (t *fakeTx) SaveSetupToken(_ context.Context, tok domain.SetupToken) error {
	t.r.state.Token = &tok
	return nil
}
func (t *fakeTx) CompleteSetup(_ context.Context, u domain.User, n domain.PlatformName, _ time.Time) error {
	t.r.users = append(t.r.users, u)
	t.r.name = n
	t.r.state = domain.SetupState{Completed: true}
	return nil
}

type fakeHasher struct{ calls int }

func (h *fakeHasher) Hash(_ context.Context, pw string) (string, error) {
	h.calls++
	return "hashed:" + pw, nil
}

type fakeChannel struct {
	published []string
	cleared   int
	fail      error
}

func (c *fakeChannel) Publish(plain string) error {
	if c.fail != nil {
		return c.fail
	}
	c.published = append(c.published, plain)
	return nil
}

func (c *fakeChannel) Clear() error {
	c.cleared++
	return c.fail
}
func (c *fakeChannel) last() string { return c.published[len(c.published)-1] }

type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

type fixture struct {
	svc    *SetupService
	repo   *fakeRepo
	hasher *fakeHasher
	ch     *fakeChannel
	clock  *clock
}

func newFixture() *fixture {
	f := &fixture{repo: &fakeRepo{}, hasher: &fakeHasher{}, ch: &fakeChannel{},
		clock: &clock{time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)}}
	f.svc = &SetupService{Repo: f.repo, Hasher: f.hasher, Channel: f.ch, Now: f.clock.Now, Rand: rand.Reader,
		NewID: func() string { return "user-1" }}
	return f
}

func validInput(token string) CompleteSetupInput {
	return CompleteSetupInput{Token: token, Login: " Owner ", Password: "correct horse battery", PlatformName: "Acme AI"}
}

func TestPrepareIssuesTokenAndCompleteCreatesOwner(t *testing.T) {
	t.Parallel()
	f := newFixture()
	required, err := f.svc.Prepare(t.Context())
	if err != nil || !required {
		t.Fatalf("Prepare = %v, %v", required, err)
	}
	if len(f.ch.published) != 1 || f.repo.state.Token == nil {
		t.Fatal("token not published or not stored")
	}
	u, err := f.svc.Complete(t.Context(), validInput(f.ch.last()))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if u.Login != "owner" || u.Role != domain.RoleOwner || u.PasswordHash != "hashed:correct horse battery" || u.ID != "user-1" {
		t.Fatalf("owner = %+v", u)
	}
	if !f.repo.state.Completed || f.repo.name != "Acme AI" || len(f.repo.users) != 1 {
		t.Fatalf("repo = %+v", f.repo)
	}
	if f.ch.cleared == 0 {
		t.Fatal("token channel not cleared after setup")
	}
	if required, _ := f.svc.Status(t.Context()); required {
		t.Fatal("setup still required after completion")
	}
}

func TestCompleteRejectsBadTokensWithoutHashing(t *testing.T) {
	t.Parallel()
	f := newFixture()
	_, _ = f.svc.Prepare(t.Context())
	good := f.ch.last()
	for _, tok := range []string{"", "nope", good + "x"} {
		if _, err := f.svc.Complete(t.Context(), validInput(tok)); !errors.Is(err, domain.ErrInvalidSetupToken) {
			t.Fatalf("token %q: err = %v", tok, err)
		}
	}
	if f.hasher.calls != 0 {
		t.Fatal("password hashed before the token was verified")
	}
}

func TestCompleteTwiceFails(t *testing.T) {
	t.Parallel()
	f := newFixture()
	_, _ = f.svc.Prepare(t.Context())
	tok := f.ch.last()
	if _, err := f.svc.Complete(t.Context(), validInput(tok)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Complete(t.Context(), validInput(tok)); !errors.Is(err, domain.ErrSetupCompleted) {
		t.Fatalf("second Complete err = %v", err)
	}
	if len(f.repo.users) != 1 {
		t.Fatalf("users = %d", len(f.repo.users))
	}
}

func TestExpiredTokenIsRejectedAndStatusReissues(t *testing.T) {
	t.Parallel()
	f := newFixture()
	_, _ = f.svc.Prepare(t.Context())
	old := f.ch.last()
	f.clock.t = f.clock.t.Add(domain.SetupTokenTTL)
	if _, err := f.svc.Complete(t.Context(), validInput(old)); !errors.Is(err, domain.ErrInvalidSetupToken) {
		t.Fatalf("expired token err = %v", err)
	}
	if required, err := f.svc.Status(t.Context()); err != nil || !required {
		t.Fatalf("Status = %v, %v", required, err)
	}
	if f.ch.last() == old {
		t.Fatal("no new token issued after expiry")
	}
	if _, err := f.svc.Complete(t.Context(), validInput(f.ch.last())); err != nil {
		t.Fatalf("Complete with reissued token: %v", err)
	}
}

func TestStatusDoesNotReissueValidToken(t *testing.T) {
	t.Parallel()
	f := newFixture()
	_, _ = f.svc.Prepare(t.Context())
	_, _ = f.svc.Status(t.Context())
	if len(f.ch.published) != 1 {
		t.Fatalf("published %d tokens; a valid token must not be rotated by status checks", len(f.ch.published))
	}
}

func TestCompleteValidatesInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		mod   func(*CompleteSetupInput)
		field string
	}{
		{"login", func(in *CompleteSetupInput) { in.Login = "x" }, "login"},
		{"password", func(in *CompleteSetupInput) { in.Password = "short" }, "password"},
		{"name", func(in *CompleteSetupInput) { in.PlatformName = " " }, "platformName"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture()
			_, _ = f.svc.Prepare(t.Context())
			in := validInput(f.ch.last())
			tt.mod(&in)
			_, err := f.svc.Complete(t.Context(), in)
			var ve *domain.ValidationError
			if !errors.As(err, &ve) || ve.Field != tt.field {
				t.Fatalf("err = %v, want validation error on %s", err, tt.field)
			}
			if f.repo.state.Completed || f.hasher.calls != 0 {
				t.Fatal("state changed or password hashed on invalid input")
			}
		})
	}
}

func TestPrepareAfterCompletionClearsChannel(t *testing.T) {
	t.Parallel()
	f := newFixture()
	f.repo.state.Completed = true
	required, err := f.svc.Prepare(t.Context())
	if err != nil || required {
		t.Fatalf("Prepare = %v, %v", required, err)
	}
	if len(f.ch.published) != 0 || f.ch.cleared != 1 {
		t.Fatalf("published=%d cleared=%d", len(f.ch.published), f.ch.cleared)
	}
}

func TestFailedPublishDoesNotKeepAnUndeliveredToken(t *testing.T) {
	t.Parallel()
	f := newFixture()
	f.ch.fail = errors.New("disk full")
	if _, err := f.svc.Prepare(t.Context()); err == nil {
		t.Fatal("want error when the token cannot be delivered")
	}
	if f.repo.state.Token != nil {
		t.Fatal("token stored although it was never delivered")
	}
	f.ch.fail = nil
	if _, err := f.svc.Status(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(f.ch.published) != 1 || f.repo.state.Token == nil {
		t.Fatal("status did not issue a token after the failed delivery")
	}
}

func TestStatusWithValidTokenDoesNotWrite(t *testing.T) {
	t.Parallel()
	f := newFixture()
	_, _ = f.svc.Prepare(t.Context())
	before := f.repo.writeTxs
	for range 5 {
		if _, err := f.svc.Status(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	f.repo.state.Completed = true
	_, _ = f.svc.Status(t.Context())
	if f.repo.writeTxs != before {
		t.Fatalf("status opened %d write transactions", f.repo.writeTxs-before)
	}
}

func TestCompleteAcceptsTokenWithSurroundingWhitespace(t *testing.T) {
	t.Parallel()
	f := newFixture()
	_, _ = f.svc.Prepare(t.Context())
	if _, err := f.svc.Complete(t.Context(), validInput("  "+f.ch.last()+"\n")); err != nil {
		t.Fatalf("Complete with copied newline: %v", err)
	}
}

func TestPrepareAfterCompletionToleratesClearFailure(t *testing.T) {
	t.Parallel()
	f := newFixture()
	f.repo.state.Completed = true
	f.ch.fail = errors.New("read-only file system")
	if required, err := f.svc.Prepare(t.Context()); err != nil || required {
		t.Fatalf("Prepare = %v, %v; a leftover token file must not block start-up", required, err)
	}
}
