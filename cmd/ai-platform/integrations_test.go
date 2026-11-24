package main

import (
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	integrationsdb "github.com/tr1gl1de/kindevai/internal/integrations/adapters/sqlite"
	integrationsapp "github.com/tr1gl1de/kindevai/internal/integrations/app"
	integrations "github.com/tr1gl1de/kindevai/internal/integrations/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	projectsdb "github.com/tr1gl1de/kindevai/internal/projects/adapters/sqlite"
	projectsapp "github.com/tr1gl1de/kindevai/internal/projects/app"
	workflow "github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// The workflow's plan validation reads a project's repositories from the integrations registry (KIN-21 port).
func TestWorkflowRepositoriesFromRegistry(t *testing.T) {
	t.Parallel()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(t.Context(), d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(t.Context(), "INSERT INTO projects(id, slug, name, created_at) VALUES ('p1', 'alpha', 'Alpha', '2026-11-24T12:00:00Z'), ('p2', 'beta', 'Beta', '2026-11-24T12:00:00Z')"); err != nil {
		t.Fatal(err)
	}
	repo := integrationsdb.New(d)
	for i, r := range [][3]string{{"p1", "acme", "web"}, {"p1", "acme", "api"}, {"p2", "acme", "other"}} {
		rr, err := integrations.NewRepository(string(rune('a'+i)), r[0], r[1], r[2], "", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.CreateRepository(t.Context(), rr); err != nil {
			t.Fatal(err)
		}
	}
	w := workflowRepositories{projects: &projectsapp.Service{Repo: projectsdb.New(d)}, repos: &integrationsapp.Service{Store: repo}}
	got, err := w.Repositories(t.Context(), "p1")
	if err != nil || !slices.Equal(got, []string{"acme/api", "acme/web"}) {
		t.Fatalf("p1: %v %v", got, err)
	}
	if got, err := w.Repositories(t.Context(), "p2"); err != nil || !slices.Equal(got, []string{"acme/other"}) {
		t.Fatalf("p2: %v %v", got, err)
	}
	if _, err := w.Repositories(t.Context(), "nope"); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
}

// The webhook endpoint is reachable without a session (its signature authenticates it) and refuses unsigned posts.
func TestGitHubWebhookIsPublicButSigned(t *testing.T) {
	t.Parallel()
	base, stop := startServe(t, t.TempDir())
	defer stop()
	code, body := call(t, http.MethodPost, base+"/api/webhooks/github", `{"zen":"x"}`)
	if code != http.StatusUnauthorized || !strings.Contains(body, "bad_signature") {
		t.Fatalf("webhook without app: %d %s", code, body)
	}
}

// The GitHub API URL carries App JWTs and tokens: only https, or http to this machine (a test double).
func TestServeRejectsBadGitHubAPIURL(t *testing.T) {
	t.Parallel()
	for _, u := range []string{"http://github.example.com", "ftp://x", "https://user:pw@ghe.example.com/api/v3"} {
		err := run(t.Context(), []string{"serve", "--addr", "127.0.0.1:0", "--data-dir", t.TempDir(), "--github-api-url", u}, io.Discard, io.Discard, nil)
		if err == nil || !strings.Contains(err.Error(), "--github-api-url") {
			t.Errorf("%s: err = %v", u, err)
		}
	}
}
