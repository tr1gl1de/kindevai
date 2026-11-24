package sqlite_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/approvals/adapters/sqlite"
	"github.com/tr1gl1de/kindevai/internal/approvals/app"
	"github.com/tr1gl1de/kindevai/internal/approvals/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
)

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

var hash1 = strings.Repeat("a", 64)

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
		"INSERT INTO users(id, login, password_hash, platform_role, created_at) VALUES ('u1', 'owner', 'h', 'owner', 'x')",
		"INSERT INTO projects(id, slug, name, created_at) VALUES ('p1', 'alpha', 'Alpha', 'x'), ('p2', 'beta', 'Beta', 'x')",
		`INSERT INTO tasks(id, project_id, title, state, plan_revision, created_at, updated_at, version)
		 VALUES ('t1', 'p1', 'Login', 'AWAITING_APPROVAL', 1, 'x', 'x', 1), ('t2', 'p2', 'Other', 'AWAITING_APPROVAL', 1, 'x', 'x', 1)`,
		`INSERT INTO task_plan_revisions(task_id, revision, plan_json, plan_hash, risk, created_at)
		 VALUES ('t1', 1, '{}', '` + hash1 + `', 'L2', 'x'), ('t2', 1, '{}', '` + hash1 + `', 'L2', 'x')`,
	} {
		if _, err := d.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func newApproval(t *testing.T, id, task, project string, scope domain.Scope, at time.Time) domain.Approval {
	t.Helper()
	a, err := domain.NewRequest(domain.Request{ID: id, ProjectID: project, TaskID: task, TaskTitle: "Login",
		Revision: 1, PlanHash: hash1, Scope: scope, Reason: domain.ReasonPlan, Risk: domain.RiskL2,
		ProposedRisk: domain.RiskL1, Paths: []string{"b", "a"}, Findings: []domain.Finding{{Category: domain.CategoryMigrations,
			Risk: domain.RiskL2, Pattern: "**/*.sql", Repository: "api", Path: "db/1.sql"}}}, at)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestApprovalRoundTripAndImmutability(t *testing.T) {
	t.Parallel()
	d := testDB(t)
	repo := sqlite.New(d)
	ctx := t.Context()
	a := newApproval(t, "a1", "t1", "p1", domain.ScopeExecute, t0)
	if err := repo.Create(ctx, a); err != nil {
		t.Fatal(err)
	}
	dup := newApproval(t, "a2", "t1", "p1", domain.ScopeExecute, t0)
	if err := repo.Create(ctx, dup); !errors.Is(err, app.ErrDuplicatePending) {
		t.Fatalf("second pending execute approval = %v", err)
	}
	if err := repo.Create(ctx, newApproval(t, "a3", "t1", "p1", domain.ScopeIntegrateDev, t0.Add(time.Second))); err != nil {
		t.Fatalf("integrate_dev is a separate request: %v", err)
	}
	got, err := repo.Get(ctx, "a1")
	if err != nil || got.ProposedRisk != domain.RiskL1 || !slices.Equal(got.Paths, []string{"a", "b"}) ||
		len(got.Findings) != 1 || got.Findings[0].Pattern != "**/*.sql" || !got.RequestedAt.Equal(t0) {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	if err := got.Decide(domain.Decision{Approver: domain.Approver{ID: "u1", Login: "owner"}, Approve: true,
		Revision: 1, PlanHash: hash1, Comment: "ok"}, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := repo.Save(ctx, got); err != nil {
		t.Fatal(err)
	}
	again, _ := repo.Get(ctx, "a1")
	if again.State != domain.StateApproved || again.ApproverID != "u1" || again.Comment != "ok" || again.DecidedAt == nil {
		t.Fatalf("decided = %+v", again)
	}
	if err := repo.Save(ctx, again); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("saving a decided approval = %v", err)
	}
	if _, err := d.ExecContext(ctx, "UPDATE approvals SET comment = 'x' WHERE id = 'a1'"); err == nil {
		t.Fatal("the trigger must keep decided approvals immutable")
	}
	if _, err := repo.Get(ctx, "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown = %v", err)
	}
	// After the decision another execute request (e.g. a scope extension) may be pending again.
	if err := repo.Create(ctx, dup); err != nil {
		t.Fatal(err)
	}
	ts, _ := repo.ForTask(ctx, "t1")
	if len(ts) != 3 || ts[0].ID != "a1" {
		t.Fatalf("for task = %+v", ts)
	}
}

func TestListFiltersByProjectAndState(t *testing.T) {
	t.Parallel()
	repo := sqlite.New(testDB(t))
	ctx := t.Context()
	for i, a := range []domain.Approval{
		newApproval(t, "a1", "t1", "p1", domain.ScopeExecute, t0),
		newApproval(t, "a2", "t2", "p2", domain.ScopeExecute, t0.Add(time.Second)),
		newApproval(t, "a3", "t1", "p1", domain.ScopeIntegrateDev, t0.Add(2*time.Second)),
	} {
		if err := repo.Create(ctx, a); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	ids := func(as []domain.Approval) []string {
		var out []string
		for _, a := range as {
			out = append(out, a.ID)
		}
		return out
	}
	all, _ := repo.List(ctx, app.Filter{AllProjects: true})
	if !slices.Equal(ids(all), []string{"a3", "a2", "a1"}) {
		t.Fatalf("all = %v", ids(all))
	}
	p1, _ := repo.List(ctx, app.Filter{ProjectIDs: []string{"p1"}})
	if !slices.Equal(ids(p1), []string{"a3", "a1"}) {
		t.Fatalf("p1 = %v", ids(p1))
	}
	none, _ := repo.List(ctx, app.Filter{})
	if len(none) != 0 {
		t.Fatalf("no projects = %v", ids(none))
	}
	decided, _ := repo.List(ctx, app.Filter{AllProjects: true, States: []domain.State{domain.StateApproved}})
	if len(decided) != 0 {
		t.Fatalf("approved = %v", ids(decided))
	}
}

func TestPolicyRoundTrip(t *testing.T) {
	t.Parallel()
	repo := sqlite.New(testDB(t))
	ctx := t.Context()
	if _, ok, err := repo.Policy(ctx, "p1"); ok || err != nil {
		t.Fatalf("no policy yet: ok=%v err=%v", ok, err)
	}
	p, _ := domain.NewPolicy([]domain.Rule{{Category: domain.CategorySecurity, Risk: domain.RiskL3, Patterns: []string{"**/auth/**"}}})
	for range 2 { // insert, then replace
		if err := repo.SavePolicy(ctx, "p1", p, "u1", t0); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := repo.Policy(ctx, "p1")
	if err != nil || !ok || got.Rule(domain.CategorySecurity).Risk != domain.RiskL3 || len(got.Rules) != len(domain.AllCategories) {
		t.Fatalf("policy = %+v, %v, %v", got, ok, err)
	}
	if err := repo.SavePolicy(ctx, "p9", p, "u1", t0); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown project = %v", err)
	}
}
