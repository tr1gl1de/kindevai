package sqlite

import (
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/identity/domain"
)

func TestAccountWrites(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	r := NewUserRepository(d)
	ctx := t.Context()
	now := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	u := domain.User{ID: "u1", Login: "carol", PasswordHash: "h", Role: domain.RoleUser, CreatedAt: now, MustChangePassword: true}
	if err := r.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := r.Create(ctx, domain.User{ID: "u2", Login: "carol", PasswordHash: "h", Role: domain.RoleUser, CreatedAt: now}); !errors.Is(err, domain.ErrLoginTaken) {
		t.Fatalf("duplicate login = %v", err)
	}
	got, err := r.ByID(ctx, "u1")
	if err != nil || !got.MustChangePassword || got.Disabled {
		t.Fatalf("ByID = %+v, %v", got, err)
	}
	if err := r.SetDisabled(ctx, "u1", true); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.ByID(ctx, "u1"); !got.Disabled {
		t.Fatal("not disabled")
	}
	if err := r.SetPassword(ctx, "u1", "h2", false); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.ByID(ctx, "u1"); got.PasswordHash != "h2" || got.MustChangePassword {
		t.Fatalf("after SetPassword = %+v", got)
	}
	if err := r.SetDisabled(ctx, "nope", true); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown user = %v", err)
	}
	if err := r.SetPassword(ctx, "nope", "h", false); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown user = %v", err)
	}
	list, err := r.List(ctx)
	if err != nil || len(list) != 1 || list[0].Login != "carol" {
		t.Fatalf("List = %+v, %v", list, err)
	}
}

func TestDeleteSessionsByUser(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	users := NewUserRepository(d)
	for _, id := range []string{"u1", "u2"} {
		if err := users.Create(t.Context(), domain.User{ID: id, Login: domain.Login("user-" + id), PasswordHash: "h", Role: domain.RoleUser, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	r := NewSessionRepository(d)
	now := time.Now()
	_, a, _ := domain.NewSession(rand.Reader, "u1", now)
	_, b, _ := domain.NewSession(rand.Reader, "u1", now)
	_, other, _ := domain.NewSession(rand.Reader, "u2", now)
	for _, s := range []domain.Session{a, b, other} {
		if err := r.Create(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.DeleteByUser(t.Context(), "u1", a.TokenHash); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ByTokenHash(t.Context(), a.TokenHash); err != nil {
		t.Fatal("kept session was deleted")
	}
	if _, err := r.ByTokenHash(t.Context(), b.TokenHash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("other session of the user survived")
	}
	if _, err := r.ByTokenHash(t.Context(), other.TokenHash); err != nil {
		t.Fatal("another user's session was deleted")
	}
	if err := r.DeleteByUser(t.Context(), "u1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ByTokenHash(t.Context(), a.TokenHash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("DeleteByUser without keep left a session")
	}
}
