package sqlite_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	auditdb "github.com/tr1gl1de/kindevai/internal/audit/adapters/sqlite"
	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/runners/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/runners/app"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

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
	for _, q := range []string{
		"INSERT INTO users(id, login, password_hash, platform_role, created_at) VALUES ('owner1', 'owner', 'h', 'owner', 'x')",
		"INSERT INTO projects(id, slug, name, created_at) VALUES ('p1', 'alpha', 'Alpha', 'x'), ('p2', 'beta', 'Beta', 'x')",
	} {
		if _, err := d.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func newKey(t *testing.T) (domain.PublicKey, string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s := base64.StdEncoding.EncodeToString(pub)
	k, err := domain.ParsePublicKey(s)
	if err != nil {
		t.Fatal(err)
	}
	return k, s
}

func TestTokenAndRunnerRoundTrip(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	r := sqlite.New(d)
	ctx := t.Context()
	tok, _, err := domain.NewEnrollmentToken(rand.Reader, "t1", "owner1", t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CreateToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	got, err := r.TokenByHash(ctx, tok.TokenHash)
	if err != nil || got.ID != "t1" || !got.ExpiresAt.Equal(tok.ExpiresAt) || got.State(t0) != domain.TokenPending {
		t.Fatalf("TokenByHash = %+v, %v", got, err)
	}
	if _, err := r.TokenByHash(ctx, make([]byte, 32)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown hash = %v", err)
	}

	key, _ := newKey(t)
	run := domain.Enroll("r1", tok, key, domain.Metadata{Hostname: "h", OS: "linux", Arch: "amd64", Version: "1"}, t0)
	if err := r.CreateRunner(ctx, run); err != nil {
		t.Fatal(err)
	}
	dup := run
	dup.ID = "r2"
	if err := r.CreateRunner(ctx, dup); !errors.Is(err, domain.ErrKeyEnrolled) {
		t.Fatalf("duplicate key = %v", err)
	}
	if ok, err := r.ConsumeToken(ctx, "t1", "r1", t0); !ok || err != nil {
		t.Fatalf("consume = %v, %v", ok, err)
	}
	if ok, _ := r.ConsumeToken(ctx, "t1", "r1", t0); ok {
		t.Fatal("a token is consumed once")
	}
	if ok, _ := r.RevokeToken(ctx, "t1", t0); ok {
		t.Fatal("a used token cannot be revoked")
	}
	if got, _ := r.TokenByID(ctx, "t1"); got.RunnerID != "r1" || got.State(t0) != domain.TokenUsed {
		t.Fatalf("consumed token = %+v", got)
	}

	if err := run.Approve([]string{"p2", "p1"}, "owner1", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveRunner(ctx, run); err != nil {
		t.Fatal(err)
	}
	loaded, err := r.Runner(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != domain.StatusApproved || loaded.ApprovedBy != "owner1" || loaded.ApprovedAt == nil ||
		len(loaded.ProjectIDs) != 2 || loaded.ProjectIDs[0] != "p1" || string(loaded.PublicKey) != string(key) ||
		loaded.Fingerprint != key.Fingerprint() || loaded.Metadata != run.Metadata || loaded.CreatedBy != "owner1" {
		t.Fatalf("loaded = %+v", loaded)
	}
	bad := loaded
	bad.ProjectIDs = []string{"nope"}
	// Inside the use case's transaction a failed save leaves the old permissions in place.
	if err := (db.Transactor{DB: d}).InTx(ctx, func(ctx context.Context) error { return r.SaveRunner(ctx, bad) }); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown project = %v", err)
	}
	if _, err := r.Runner(ctx, "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing runner = %v", err)
	}
	all, err := r.Runners(ctx)
	if err != nil || len(all) != 1 || len(all[0].ProjectIDs) != 2 {
		t.Fatalf("Runners = %+v, %v", all, err)
	}
}

func service(t *testing.T, d *sql.DB, now func() time.Time) *app.Service {
	t.Helper()
	audit := &auditapp.Service{Repo: auditdb.New(d), Now: now}
	var mu sync.Mutex
	n := 0
	return &app.Service{
		Repo: sqlite.New(d), Projects: projects{d}, Tx: db.Transactor{DB: d}, Audit: audit, Rand: rand.Reader, Now: now,
		NewID: func() string {
			mu.Lock()
			defer mu.Unlock()
			n++
			return "id" + string(rune('a'+n))
		},
	}
}

// projects reads the real projects table through db.Conn, like the projects context does, so a lookup made
// outside the use case's transaction would deadlock on the single connection (regression: KIN-17 review).
type projects struct{ d *sql.DB }

func (p projects) Lookup(ctx context.Context, id string) (app.ProjectRef, error) {
	var name string
	err := db.Conn(ctx, p.d).QueryRowContext(ctx, "SELECT name FROM projects WHERE id = ?", id).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return app.ProjectRef{}, domain.ErrNotFound
	}
	return app.ProjectRef{ID: id, Name: name}, err
}

var owner = auth.Principal{UserID: "owner1", Login: "owner", Owner: true}

// Two (or more) runners racing with one token: exactly one is registered, the others are told it is used.
func TestConcurrentEnrollmentsWithOneTokenExactlyOneSucceeds(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	svc := service(t, d, func() time.Time { return time.Now() })
	_, plain, err := svc.CreateToken(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	const racers = 8
	errs := make([]error, racers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range racers {
		_, key := newKey(t)
		wg.Go(func() {
			<-start
			_, errs[i] = svc.Enroll(t.Context(), app.EnrollRequest{Token: plain, PublicKey: key, Hostname: "h", OS: "linux", Arch: "amd64"})
		})
	}
	close(start)
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, domain.ErrTokenUsed):
			t.Errorf("loser error = %v, want ErrTokenUsed", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d enrollments succeeded, want exactly 1", ok)
	}
	var n int
	if err := d.QueryRowContext(t.Context(), "SELECT count(*) FROM runners").Scan(&n); err != nil || n != 1 {
		t.Fatalf("runners in the database = %d, %v", n, err)
	}
}

// The audit events commit with the action and never carry the token.
func TestEnrollmentAuditIsStoredWithoutTheToken(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	svc := service(t, d, func() time.Time { return t0 })
	_, plain, err := svc.CreateToken(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	_, key := newKey(t)
	res, err := svc.Enroll(t.Context(), app.EnrollRequest{Token: plain, PublicKey: key, Hostname: "h", OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Approve(t.Context(), owner, res.Runner.ID, []string{"p1"}); err != nil {
		t.Fatal(err)
	}
	rows, err := d.QueryContext(t.Context(), "SELECT kind, actor_type, coalesce(subject, ''), coalesce(details, '') FROM audit_events ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var kind, actor, subject, details string
		if err := rows.Scan(&kind, &actor, &subject, &details); err != nil {
			t.Fatal(err)
		}
		if kind == app.KindEnrolled && actor != "runner" {
			t.Errorf("enrolled actor = %s, want runner", actor)
		}
		for _, s := range []string{subject, details} {
			if strings.Contains(s, plain) {
				t.Fatalf("audit %s contains the token", kind)
			}
		}
		kinds = append(kinds, kind)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []string{app.KindTokenCreated, app.KindEnrolled, app.KindApproved}; strings.Join(kinds, " ") != strings.Join(want, " ") {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
}
