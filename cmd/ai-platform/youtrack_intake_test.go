package main

import (
	"crypto/rand"
	"path/filepath"
	"testing"
	"time"

	integrationsdb "github.com/tr1gl1de/kindevai/internal/integrations/adapters/sqlite"
	integrationsapp "github.com/tr1gl1de/kindevai/internal/integrations/app"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	projectsdb "github.com/tr1gl1de/kindevai/internal/projects/adapters/sqlite"
	projectsapp "github.com/tr1gl1de/kindevai/internal/projects/app"
	workflowdb "github.com/tr1gl1de/kindevai/internal/workflow/adapters/sqlite"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
)

// The intake's workflow adapter creates real tasks, finds them by issue and follows title changes.
func TestIntakeTasksAdapterOnTheWorkflowContext(t *testing.T) {
	t.Parallel()
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if _, err := db.Migrate(t.Context(), d, db.Migrations()); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"INSERT INTO users(id, login, password_hash, platform_role, created_at) VALUES ('u1', 'owner', 'h', 'owner', 'x')",
		"INSERT INTO projects(id, slug, name, created_at) VALUES ('p1', 'alpha', 'Alpha', 'x')",
	} {
		if _, err := d.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	integ := &integrationsapp.Service{Store: integrationsdb.New(d)}
	tasks := intakeTasks{&workflowapp.Service{Repo: workflowdb.New(d), Tx: db.Transactor{DB: d}, Now: time.Now, NewID: rand.Text,
		Projects: workflowProjects{projects: &projectsapp.Service{Repo: projectsdb.New(d)}, integrations: integ}}}
	ctx := t.Context()
	if _, found, err := tasks.TaskForIssue(ctx, "p1", "KIN-1"); err != nil || found {
		t.Fatalf("missing: %v %v", found, err)
	}
	id, err := tasks.CreateIssueTask(ctx, "p1", "KIN-1", "KIN-1: Login", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if got, found, err := tasks.TaskForIssue(ctx, "p1", "KIN-1"); err != nil || !found || got != id {
		t.Fatalf("found %q %v %v", got, found, err)
	}
	if changed, err := tasks.RetitleTask(ctx, id, "KIN-1: Login page"); err != nil || !changed {
		t.Fatalf("retitle %v %v", changed, err)
	}
	if _, err := tasks.CreateIssueTask(ctx, "missing", "KIN-2", "KIN-2: x", "u1"); err == nil {
		t.Fatal("task for an unknown project")
	}
}
