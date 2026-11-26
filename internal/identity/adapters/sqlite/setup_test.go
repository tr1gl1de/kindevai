package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditdb "github.com/tr1gl1de/kindevai/internal/audit/adapters/sqlite"
	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/identity/app"
	"github.com/tr1gl1de/kindevai/internal/identity/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(t.Context(), d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	return d
}

type plainHasher struct{}

func (plainHasher) Hash(_ context.Context, pw string) (string, error) { return "h:" + pw, nil }

type memChannel struct {
	mu    sync.Mutex
	token string
}

func (c *memChannel) Publish(p string) error { c.mu.Lock(); c.token = p; c.mu.Unlock(); return nil }
func (c *memChannel) Clear() error           { c.mu.Lock(); c.token = ""; c.mu.Unlock(); return nil }
func (c *memChannel) get() string            { c.mu.Lock(); defer c.mu.Unlock(); return c.token }

var ids atomic.Int64

func service(d *sql.DB, ch *memChannel) *app.SetupService {
	return &app.SetupService{Repo: NewSetupRepository(d), Hasher: plainHasher{}, Channel: ch,
		Audit: &auditapp.Service{Repo: auditdb.New(d), Now: time.Now}, Now: time.Now,
		Rand: rand.Reader, NewID: func() string { return fmt.Sprint("u", ids.Add(1)) }}
}

func input(tok string) app.CompleteSetupInput {
	return app.CompleteSetupInput{Token: tok, Login: "owner", Password: "correct horse battery", PlatformName: "Acme"}
}

func TestSetupPersistsOwnerAndClosesAfterRestart(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	ch := &memChannel{}
	svc := service(d, ch)
	if req, err := svc.Prepare(t.Context()); err != nil || !req {
		t.Fatalf("Prepare = %v, %v", req, err)
	}
	var stored string
	if err := d.QueryRowContext(t.Context(), "SELECT value FROM meta WHERE key = 'setup_token_hash'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == ch.get() {
		t.Fatal("plaintext token stored in the database")
	}
	if _, err := svc.Complete(t.Context(), input(ch.get())); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	var login, role, hash string
	if err := d.QueryRowContext(t.Context(), "SELECT login, platform_role, password_hash FROM users").Scan(&login, &role, &hash); err != nil {
		t.Fatal(err)
	}
	if login != "owner" || role != "owner" || hash != "h:correct horse battery" {
		t.Fatalf("user row = %s %s %s", login, role, hash)
	}
	var name []byte
	if err := d.QueryRowContext(t.Context(), "SELECT value FROM settings WHERE key = 'platform.name'").Scan(&name); err != nil || string(name) != "Acme" {
		t.Fatalf("platform name = %q, %v", name, err)
	}
	var n int
	_ = d.QueryRowContext(t.Context(), "SELECT count(*) FROM meta WHERE key LIKE 'setup_token%'").Scan(&n)
	if n != 0 {
		t.Fatal("setup token left in the database after completion")
	}

	restarted := service(d, &memChannel{}) // same DB, fresh process
	if req, err := restarted.Prepare(t.Context()); err != nil || req {
		t.Fatalf("Prepare after restart = %v, %v", req, err)
	}
	if _, err := restarted.Complete(t.Context(), input("anything")); !errors.Is(err, domain.ErrSetupCompleted) {
		t.Fatalf("Complete after restart err = %v", err)
	}
}

func TestConcurrentCompleteCreatesExactlyOneOwner(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	ch := &memChannel{}
	svc := service(d, ch)
	if _, err := svc.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	tok := ch.get()
	const n = 8
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			in := input(tok)
			in.Login = fmt.Sprintf("owner%d", i)
			_, err := svc.Complete(t.Context(), in)
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, domain.ErrSetupCompleted), errors.Is(err, domain.ErrInvalidSetupToken):
			default:
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	wg.Wait()
	var users int
	_ = d.QueryRowContext(t.Context(), "SELECT count(*) FROM users").Scan(&users)
	if ok.Load() != 1 || users != 1 {
		t.Fatalf("successes = %d, users = %d; want exactly 1", ok.Load(), users)
	}
}

func TestSchemaAllowsOnlyOneOwner(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	ins := "INSERT INTO users(id, login, password_hash, platform_role, created_at) VALUES (?, ?, 'h', ?, 'now')"
	if _, err := d.ExecContext(t.Context(), ins, "1", "a", "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(t.Context(), ins, "2", "b", "owner"); err == nil {
		t.Fatal("second owner inserted")
	}
	if _, err := d.ExecContext(t.Context(), ins, "3", "c", "user"); err != nil {
		t.Fatalf("regular user rejected: %v", err)
	}
}
