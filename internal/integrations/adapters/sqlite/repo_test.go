package sqlite_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	auditdb "github.com/tr1gl1de/kindevai/internal/audit/adapters/sqlite"
	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	"github.com/tr1gl1de/kindevai/internal/platform/secrets"
)

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 123456789, time.UTC)

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

func TestSecrets(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	r := sqlite.New(d)
	ctx := t.Context()
	if _, err := r.SecretCiphertext(ctx, domain.SecretYouTrackToken); !errors.Is(err, domain.ErrSecretNotSet) {
		t.Fatalf("missing: %v", err)
	}
	if m, err := r.SecretMeta(ctx, domain.SecretYouTrackToken); err != nil || m.Set {
		t.Fatalf("missing meta: %+v %v", m, err)
	}
	if err := r.PutSecret(ctx, domain.SecretYouTrackToken, []byte{1, 2}, "owner1", t0); err != nil {
		t.Fatal(err)
	}
	if err := r.PutSecret(ctx, domain.SecretYouTrackToken, []byte{3, 4}, "owner1", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	ct, err := r.SecretCiphertext(ctx, domain.SecretYouTrackToken)
	if err != nil || string(ct) != "\x03\x04" {
		t.Fatalf("ciphertext %v %v", ct, err)
	}
	m, err := r.SecretMeta(ctx, domain.SecretYouTrackToken)
	if err != nil || !m.Set || !m.UpdatedAt.Equal(t0.Add(time.Hour)) || m.UpdatedBy != "owner1" {
		t.Fatalf("meta %+v %v", m, err)
	}
	if err := r.DeleteSecret(ctx, domain.SecretYouTrackToken); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SecretCiphertext(ctx, domain.SecretYouTrackToken); !errors.Is(err, domain.ErrSecretNotSet) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestYouTrackConnection(t *testing.T) {
	t.Parallel()
	r := sqlite.New(testDB(t))
	ctx := t.Context()
	if _, err := r.YouTrack(ctx); !errors.Is(err, domain.ErrNotConfigured) {
		t.Fatalf("missing: %v", err)
	}
	c := domain.NewYouTrackConnection("https://a.youtrack.cloud", t0)
	if err := r.SaveYouTrack(ctx, c); err != nil {
		t.Fatal(err)
	}
	c.StartCheck("c1", t0)
	c.FinishCheck("c1", domain.CheckResult{Code: "unauthorized", Detail: "HTTP 401"}, t0.Add(time.Second))
	if err := r.SaveYouTrack(ctx, c); err != nil {
		t.Fatal(err)
	}
	got, err := r.YouTrack(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseURL != c.BaseURL || got.Check.Status != domain.CheckFailed || got.Check.ID != "c1" || got.Check.Code != "unauthorized" ||
		got.Check.Detail != "HTTP 401" || !got.Check.RequestedAt.Equal(t0) || !got.Check.CheckedAt.Equal(t0.Add(time.Second)) {
		t.Fatalf("got %+v", got)
	}
	if err := r.DeleteYouTrack(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.YouTrack(ctx); !errors.Is(err, domain.ErrNotConfigured) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestProjectBindingAndRepositories(t *testing.T) {
	t.Parallel()
	r := sqlite.New(testDB(t))
	ctx := t.Context()
	if sn, err := r.ProjectYouTrack(ctx, "p1"); err != nil || sn != "" {
		t.Fatalf("unbound: %q %v", sn, err)
	}
	if err := r.SetProjectYouTrack(ctx, "p1", "KIN", t0); err != nil {
		t.Fatal(err)
	}
	if err := r.SetProjectYouTrack(ctx, "p1", "WEB", t0); err != nil {
		t.Fatal(err)
	}
	if err := r.SetProjectYouTrack(ctx, "p2", "WEB", t0); !errors.Is(err, domain.ErrShortNameTaken) {
		t.Fatalf("taken: %v", err)
	}
	if err := r.SetProjectYouTrack(ctx, "missing", "ZZZ", t0); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
	if sn, _ := r.ProjectYouTrack(ctx, "p1"); sn != "WEB" {
		t.Fatalf("bound %q", sn)
	}
	if err := r.SetProjectYouTrack(ctx, "p1", "", t0); err != nil {
		t.Fatal(err)
	}
	if sn, _ := r.ProjectYouTrack(ctx, "p1"); sn != "" {
		t.Fatalf("unbind left %q", sn)
	}

	repo, _ := domain.NewRepository("r1", "p1", "Acme", "web", "", t0)
	if err := r.CreateRepository(ctx, repo); err != nil {
		t.Fatal(err)
	}
	dup, _ := domain.NewRepository("r2", "p2", "acme", "WEB", "", t0)
	if err := r.CreateRepository(ctx, dup); !errors.Is(err, domain.ErrRepositoryExists) {
		t.Fatalf("duplicate: %v", err)
	}
	orphan, _ := domain.NewRepository("r3", "missing", "acme", "api", "", t0)
	if err := r.CreateRepository(ctx, orphan); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
	if err := repo.SetIntegrationBranch("next", t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := r.SaveRepository(ctx, repo); err != nil {
		t.Fatal(err)
	}
	got, err := r.Repository(ctx, "p1", "r1")
	if err != nil || got.IntegrationBranch != "next" || got.Owner != "Acme" || !got.UpdatedAt.Equal(t0.Add(time.Hour)) || !got.CreatedAt.Equal(t0) {
		t.Fatalf("got %+v %v", got, err)
	}
	if _, err := r.Repository(ctx, "p2", "r1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("other project: %v", err)
	}
	if rs, err := r.Repositories(ctx, "p1"); err != nil || len(rs) != 1 {
		t.Fatalf("list %v %v", rs, err)
	}
	if err := r.DeleteRepository(ctx, "p2", "r1"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("delete from other project: %v", err)
	}
	if err := r.DeleteRepository(ctx, "p1", "r1"); err != nil {
		t.Fatal(err)
	}
	if rs, _ := r.Repositories(ctx, "p1"); len(rs) != 0 {
		t.Fatalf("after delete %v", rs)
	}
}

type projects struct{}

func (projects) Lookup(_ context.Context, id string) (app.ProjectRef, error) {
	if id == "p1" {
		return app.ProjectRef{ID: "p1", Name: "Alpha"}, nil
	}
	return app.ProjectRef{}, domain.ErrNotFound
}

// TestTokenEncryptedAtRestWithAuditInSameTx runs the use case on SQLite: the database file holds only ciphertext,
// and the audit event and outbox message commit with the change.
func TestTokenEncryptedAtRestWithAuditInSameTx(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	var k secrets.Key
	_, _ = rand.Read(k[:])
	box, _ := secrets.NewBox(k)
	repo := sqlite.New(d)
	audit := &auditapp.Service{Repo: auditdb.New(d), Now: time.Now}
	svc := &app.Service{
		Vault: &app.Vault{Store: repo, Cipher: box, Now: time.Now}, Store: repo, Projects: projects{},
		Tx: db.Transactor{DB: d}, Audit: audit, Outbox: messaging.NewOutbox(d, messaging.Config{}),
		Now: time.Now, NewID: rand.Text,
	}
	owner := auth.Principal{UserID: "owner1", Login: "owner", Owner: true}
	const tok = "perm:b3duZXI=.s3cr3tT0ken.AbCdEf123456"
	if _, err := svc.ConfigureYouTrack(t.Context(), owner, "https://a.youtrack.cloud", tok); err != nil {
		t.Fatal(err)
	}
	var ct []byte
	if err := d.QueryRowContext(t.Context(), "SELECT ciphertext FROM integration_secrets WHERE name = 'youtrack.token'").Scan(&ct); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ct), "s3cr3t") {
		t.Fatal("token stored in plaintext")
	}
	if _, err := svc.CheckYouTrack(t.Context(), owner); err != nil {
		t.Fatal(err)
	}
	var events, msgs int
	_ = d.QueryRowContext(t.Context(), "SELECT count(*) FROM audit_events").Scan(&events)
	_ = d.QueryRowContext(t.Context(), "SELECT count(*) FROM outbox_events").Scan(&msgs)
	if events != 2 || msgs != 1 {
		t.Fatalf("audit events %d, outbox messages %d", events, msgs)
	}
	s, err := svc.YouTrackSettings(t.Context())
	if err != nil || s.Token.Reveal() != tok {
		t.Fatalf("settings: %v", err)
	}
}
