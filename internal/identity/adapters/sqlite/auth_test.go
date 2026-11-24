package sqlite

import (
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/identity/domain"
)

func insertUser(t *testing.T, r *UserRepository, u domain.User) {
	t.Helper()
	if _, err := r.db.ExecContext(t.Context(),
		"INSERT INTO users(id, login, password_hash, platform_role, created_at, disabled_at) VALUES (?, ?, ?, ?, ?, ?)",
		u.ID, string(u.Login), u.PasswordHash, string(u.Role), u.CreatedAt.UTC().Format(time.RFC3339Nano),
		map[bool]any{true: "2026-01-01T00:00:00Z", false: nil}[u.Disabled]); err != nil {
		t.Fatal(err)
	}
}

func TestUserRepository(t *testing.T) {
	t.Parallel()
	r := NewUserRepository(testDB(t))
	created := time.Date(2026, 11, 24, 8, 0, 0, 0, time.UTC)
	insertUser(t, r, domain.User{ID: "u1", Login: "owner", PasswordHash: "h", Role: domain.RoleOwner, CreatedAt: created})
	insertUser(t, r, domain.User{ID: "u2", Login: "old", PasswordHash: "h", Role: domain.RoleUser, CreatedAt: created, Disabled: true})
	u, err := r.ByLogin(t.Context(), "owner")
	if err != nil || u.ID != "u1" || u.Role != domain.RoleOwner || !u.CreatedAt.Equal(created) || u.Disabled {
		t.Fatalf("ByLogin = %+v, %v", u, err)
	}
	if u, err := r.ByID(t.Context(), "u2"); err != nil || !u.Disabled {
		t.Fatalf("ByID disabled = %+v, %v", u, err)
	}
	if _, err := r.ByLogin(t.Context(), "nobody"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	if _, err := r.ByID(t.Context(), "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing id = %v", err)
	}
}

func TestSessionRepository(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	users := NewUserRepository(d)
	insertUser(t, users, domain.User{ID: "u1", Login: "owner", PasswordHash: "h", Role: domain.RoleOwner, CreatedAt: time.Now()})
	r := NewSessionRepository(d)
	now := time.Date(2026, 11, 24, 8, 0, 0, 0, time.UTC)
	_, s, _ := domain.NewSession(rand.Reader, "u1", now)
	if err := r.Create(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	got, err := r.ByTokenHash(t.Context(), s.TokenHash)
	if err != nil || got.UserID != "u1" || !got.CreatedAt.Equal(now) || !got.LastSeenAt.Equal(now) {
		t.Fatalf("ByTokenHash = %+v, %v", got, err)
	}
	later := now.Add(time.Hour)
	if err := r.Touch(t.Context(), s.TokenHash, later); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.ByTokenHash(t.Context(), s.TokenHash); !got.LastSeenAt.Equal(later) {
		t.Fatalf("LastSeenAt = %v", got.LastSeenAt)
	}
	_, old, _ := domain.NewSession(rand.Reader, "u1", now.Add(-domain.SessionMaxAge))
	if err := r.Create(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteExpired(t.Context(), later); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ByTokenHash(t.Context(), old.TokenHash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired session survived purge: %v", err)
	}
	if _, err := r.ByTokenHash(t.Context(), s.TokenHash); err != nil {
		t.Fatalf("live session purged: %v", err)
	}
	if err := r.Delete(t.Context(), s.TokenHash); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ByTokenHash(t.Context(), s.TokenHash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

// Timestamps are compared as text in SQL, so they must have a fixed width: "…:00Z" vs "…:00.5Z" would
// otherwise sort the wrong way round.
func TestDeleteExpiredWithinTheSameSecond(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	insertUser(t, NewUserRepository(d), domain.User{ID: "u1", Login: "owner", PasswordHash: "h", Role: domain.RoleOwner, CreatedAt: time.Now()})
	r := NewSessionRepository(d)
	expiry := time.Date(2026, 11, 24, 8, 0, 0, 0, time.UTC)
	_, s, _ := domain.NewSession(rand.Reader, "u1", expiry.Add(-domain.SessionIdleTTL)) // expires exactly at a whole second
	if err := r.Create(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteExpired(t.Context(), expiry.Add(-500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ByTokenHash(t.Context(), s.TokenHash); err != nil {
		t.Fatalf("session purged 0.5s before expiry: %v", err)
	}
	if err := r.DeleteExpired(t.Context(), expiry.Add(500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ByTokenHash(t.Context(), s.TokenHash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("session survived 0.5s after expiry: %v", err)
	}
}
