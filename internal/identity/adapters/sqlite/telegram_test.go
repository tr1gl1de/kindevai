package sqlite

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/identity/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
)

func TestTelegramIdentityUniqueness(t *testing.T) {
	t.Parallel()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(t.Context(), d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	users := NewUserRepository(d)
	for _, id := range []string{"u1", "u2"} {
		if err := users.Create(t.Context(), domain.User{ID: id, Login: domain.Login(id + "x"), PasswordHash: "h", Role: domain.RoleUser, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	r := NewTelegramRepository(d)
	now := time.Now()
	if err := r.CreateIdentity(t.Context(), domain.TelegramIdentity{UserID: "u1", TelegramID: 42, LinkedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateIdentity(t.Context(), domain.TelegramIdentity{UserID: "u2", TelegramID: 42, LinkedAt: now}); !errors.Is(err, domain.ErrTelegramTaken) {
		t.Fatalf("same Telegram ID: %v, want ErrTelegramTaken", err)
	}
	if err := r.CreateIdentity(t.Context(), domain.TelegramIdentity{UserID: "u1", TelegramID: 43, LinkedAt: now}); !errors.Is(err, domain.ErrUserHasTelegram) {
		t.Fatalf("same user: %v, want ErrUserHasTelegram", err)
	}
	if err := r.CreateIdentity(t.Context(), domain.TelegramIdentity{UserID: "nobody", TelegramID: 44, LinkedAt: now}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown user: %v, want ErrNotFound", err)
	}
}

func TestOldLinkRequestsArePurged(t *testing.T) {
	t.Parallel()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(t.Context(), d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	if err := NewUserRepository(d).Create(t.Context(), domain.User{ID: "u1", Login: "alice", PasswordHash: "h", Role: domain.RoleUser, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	r := NewTelegramRepository(d)
	now := time.Now()
	for i, created := range []time.Time{now.Add(-48 * time.Hour), now} {
		req := domain.LinkRequest{ID: string(rune('a' + i)), TokenHash: domain.HashLinkToken(string(rune('a' + i))), UserID: "u1",
			SessionHash: []byte("s"), CreatedAt: created, ExpiresAt: created.Add(domain.LinkRequestTTL)}
		if err := r.CreateLinkRequest(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.DeleteLinkRequestsBefore(t.Context(), now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if l, err := r.LatestLinkRequest(t.Context(), "u1"); err != nil || l.ID != "b" {
		t.Fatalf("latest = %+v, %v", l, err)
	}
	var n int
	_ = d.QueryRowContext(t.Context(), "SELECT count(*) FROM telegram_link_requests").Scan(&n)
	if n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
}
