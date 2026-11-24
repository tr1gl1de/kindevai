package sqlite_test

import (
	"errors"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/integrations/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/integrations/domain"
)

func TestGitHubAppRoundTrip(t *testing.T) {
	t.Parallel()
	r := sqlite.New(testDB(t))
	ctx := t.Context()
	if _, err := r.GitHubApp(ctx); !errors.Is(err, domain.ErrNotConfigured) {
		t.Fatalf("missing app: %v", err)
	}
	a := domain.NewGitHubApp(42, t0)
	a.Check.Start("c1", t0)
	a.Check.Finish("c1", domain.CheckResult{OK: true, Detail: "kindev-bot"}, t0)
	a.Delivered("ping", t0)
	if err := r.SaveGitHubApp(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := r.GitHubApp(ctx)
	if err != nil || got.AppID != 42 || got.Check.Status != domain.CheckOK || got.Check.Detail != "kindev-bot" ||
		got.LastEvent != "ping" || got.LastDeliveryAt == nil || !got.LastDeliveryAt.Equal(t0) || !got.UpdatedAt.Equal(t0) {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if err := r.DeleteGitHubApp(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GitHubApp(ctx); !errors.Is(err, domain.ErrNotConfigured) {
		t.Fatalf("deleted app: %v", err)
	}
}

func TestRepositoryGitHubState(t *testing.T) {
	t.Parallel()
	r := sqlite.New(testDB(t))
	ctx := t.Context()
	repo, _ := domain.NewRepository("r1", "p1", "Acme", "API", "", t0)
	if err := r.CreateRepository(ctx, repo); err != nil {
		t.Fatal(err)
	}
	other, _ := domain.NewRepository("r2", "p2", "acme", "web", "", t0)
	if err := r.CreateRepository(ctx, other); err != nil {
		t.Fatal(err)
	}
	got, err := r.RepositoryByFullName(ctx, "acme/api")
	if err != nil || got.ID != "r1" || got.GitHub.Protection != domain.ProtectionUnknown || got.GitHub.Sync.Status != domain.CheckNone {
		t.Fatalf("by full name: %+v %v", got, err)
	}
	got.StartSync("s1", t0)
	got.FinishSync("s1", domain.SyncResult{CheckResult: domain.CheckResult{OK: true}, State: domain.RepoState{InstallationID: 9,
		DefaultBranch: "main", Branches: []domain.BranchProtection{{Branch: "main", Exists: true}, {Branch: "master", Exists: true}}}}, t0)
	if err := r.SaveRepository(ctx, got); err != nil {
		t.Fatal(err)
	}
	back, err := r.RepositoryByID(ctx, "r1")
	if err != nil || back.GitHub.InstallationID != 9 || back.GitHub.DefaultBranch != "main" ||
		back.GitHub.Protection != domain.ProtectionUnprotected || len(back.GitHub.Unprotected) != 2 ||
		back.GitHub.Sync.Status != domain.CheckOK || back.GitHub.Sync.CheckedAt == nil || back.IntegrationBranch != "dev" {
		t.Fatalf("by id: %+v %v", back, err)
	}
	all, err := r.AllRepositories(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("all: %d %v", len(all), err)
	}
	for _, name := range []string{"acme/none", "acme", ""} {
		if _, err := r.RepositoryByFullName(ctx, name); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%q: %v", name, err)
		}
	}
}
