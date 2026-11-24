package app

import (
	"context"
	"crypto/rand"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/identity/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
)

// memAccounts is an in-memory AccountRepository + UserRepository.
type memAccounts struct {
	mu    sync.Mutex
	users map[string]domain.User
}

func (m *memAccounts) ByLogin(_ context.Context, l domain.Login) (domain.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if u.Login == l {
			return u, nil
		}
	}
	return domain.User{}, domain.ErrNotFound
}

func (m *memAccounts) ByID(_ context.Context, id string) (domain.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return domain.User{}, domain.ErrNotFound
	}
	return u, nil
}

func (m *memAccounts) Create(_ context.Context, u domain.User) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.users {
		if x.Login == u.Login {
			return domain.ErrLoginTaken
		}
	}
	m.users[u.ID] = u
	return nil
}

func (m *memAccounts) List(context.Context) ([]domain.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.User
	for _, u := range m.users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Login < out[j].Login })
	return out, nil
}

func (m *memAccounts) SetDisabled(_ context.Context, id string, disabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return domain.ErrNotFound
	}
	u.Disabled = disabled
	m.users[id] = u
	return nil
}

func (m *memAccounts) SetPassword(_ context.Context, id, hash string, mustChange bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return domain.ErrNotFound
	}
	u.PasswordHash, u.MustChangePassword = hash, mustChange
	m.users[id] = u
	return nil
}

type revoked struct {
	mu    sync.Mutex
	all   []string
	keeps []string
}

func (r *revoked) DeleteByUser(_ context.Context, userID string, keep []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.all = append(r.all, userID)
	r.keeps = append(r.keeps, string(keep))
	return nil
}

type hasher struct{}

func (hasher) Hash(_ context.Context, pw string) (string, error) { return "h:" + pw, nil }
func (hasher) Verify(_ context.Context, h, pw string) (bool, error) {
	return h == "h:"+pw, nil
}

var (
	ownerP = auth.Principal{UserID: "u-owner", Login: "owner", Owner: true}
	bobP   = auth.Principal{UserID: "u-bob", Login: "bob"}
)

func accountsFixture() (*AccountsService, *memAccounts, *revoked) {
	repo := &memAccounts{users: map[string]domain.User{
		"u-owner": {ID: "u-owner", Login: "owner", Role: domain.RoleOwner, PasswordHash: "h:owner password 1"},
		"u-bob":   {ID: "u-bob", Login: "bob", Role: domain.RoleUser, PasswordHash: "h:bob password 12"},
	}}
	rev := &revoked{}
	n := 0
	return &AccountsService{
		Accounts: repo, Sessions: rev, Hasher: hasher{}, Rand: rand.Reader,
		Now:   func() time.Time { return time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC) },
		NewID: func() string { n++; return "u-new" + strings.Repeat("x", n) },
	}, repo, rev
}

func TestOwnerCreatesUserWithTemporaryPassword(t *testing.T) {
	t.Parallel()
	s, repo, _ := accountsFixture()
	u, temp, err := s.CreateUser(t.Context(), ownerP, " Carol ")
	if err != nil {
		t.Fatal(err)
	}
	if u.Login != "carol" || u.Role != domain.RoleUser || !u.MustChangePassword || temp == "" {
		t.Fatalf("user = %+v temp=%q", u, temp)
	}
	stored := repo.users[u.ID]
	if stored.PasswordHash != "h:"+temp || strings.Contains(stored.PasswordHash, "carol") {
		t.Fatal("temporary password not stored as a hash")
	}
	if _, _, err := s.CreateUser(t.Context(), ownerP, "carol"); !errors.Is(err, domain.ErrLoginTaken) {
		t.Fatalf("duplicate login = %v", err)
	}
	var ve *domain.ValidationError
	if _, _, err := s.CreateUser(t.Context(), ownerP, "x"); !errors.As(err, &ve) {
		t.Fatalf("bad login = %v", err)
	}
}

func TestOnlyOwnerManagesAccounts(t *testing.T) {
	t.Parallel()
	s, _, _ := accountsFixture()
	if _, _, err := s.CreateUser(t.Context(), bobP, "eve"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("create = %v", err)
	}
	if _, err := s.ListUsers(t.Context(), bobP); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("list = %v", err)
	}
	if err := s.SetDisabled(t.Context(), bobP, "u-owner", true); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("disable = %v", err)
	}
	if _, err := s.ResetPassword(t.Context(), bobP, "u-owner"); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("reset = %v", err)
	}
}

func TestDisableRevokesSessionsAndOwnerIsProtected(t *testing.T) {
	t.Parallel()
	s, repo, rev := accountsFixture()
	if err := s.SetDisabled(t.Context(), ownerP, "u-bob", true); err != nil {
		t.Fatal(err)
	}
	if !repo.users["u-bob"].Disabled || len(rev.all) != 1 || rev.all[0] != "u-bob" || rev.keeps[0] != "" {
		t.Fatalf("disabled=%v revoked=%v", repo.users["u-bob"].Disabled, rev.all)
	}
	if err := s.SetDisabled(t.Context(), ownerP, "u-bob", false); err != nil || repo.users["u-bob"].Disabled {
		t.Fatalf("enable = %v", err)
	}
	var ve *domain.ValidationError
	if err := s.SetDisabled(t.Context(), ownerP, "u-owner", true); !errors.As(err, &ve) {
		t.Fatalf("disabling the owner = %v", err)
	}
	if err := s.SetDisabled(t.Context(), ownerP, "nope", true); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown user = %v", err)
	}
}

func TestResetPassword(t *testing.T) {
	t.Parallel()
	s, repo, rev := accountsFixture()
	temp, err := s.ResetPassword(t.Context(), ownerP, "u-bob")
	if err != nil || temp == "" {
		t.Fatal(err)
	}
	if u := repo.users["u-bob"]; u.PasswordHash != "h:"+temp || !u.MustChangePassword {
		t.Fatalf("user = %+v", u)
	}
	if len(rev.all) != 1 {
		t.Fatal("sessions not revoked on reset")
	}
}

func TestChangeOwnPassword(t *testing.T) {
	t.Parallel()
	s, repo, rev := accountsFixture()
	repo.users["u-bob"] = func() domain.User { u := repo.users["u-bob"]; u.MustChangePassword = true; return u }()
	if err := s.ChangePassword(t.Context(), bobP, "wrong", "new password 123", []byte("cur")); !errors.Is(err, domain.ErrInvalidCredentials) {
		t.Fatalf("wrong current = %v", err)
	}
	var ve *domain.ValidationError
	if err := s.ChangePassword(t.Context(), bobP, "bob password 12", "short", []byte("cur")); !errors.As(err, &ve) {
		t.Fatalf("weak new = %v", err)
	}
	if err := s.ChangePassword(t.Context(), bobP, "bob password 12", "bob password 12", []byte("cur")); !errors.As(err, &ve) || ve.Code != "password_reused" {
		t.Fatalf("same password = %v", err)
	}
	if err := s.ChangePassword(t.Context(), bobP, "bob password 12", "new password 123", []byte("cur")); err != nil {
		t.Fatal(err)
	}
	if u := repo.users["u-bob"]; u.PasswordHash != "h:new password 123" || u.MustChangePassword {
		t.Fatalf("user = %+v", u)
	}
	if len(rev.all) != 1 || rev.keeps[0] != "cur" {
		t.Fatalf("other sessions not revoked while keeping the current one: %v %v", rev.all, rev.keeps)
	}
}
