package app

import (
	"context"
	"errors"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/platform/auth"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

type fakeAccess map[string]map[string]string // user → project → role

func (f fakeAccess) Visible(_ context.Context, p auth.Principal) (map[string]string, error) {
	return f[p.UserID], nil
}

var (
	owner  = auth.Principal{UserID: "owner", Owner: true}
	admin  = auth.Principal{UserID: "admin"}
	member = auth.Principal{UserID: "member"}
	other  = auth.Principal{UserID: "other"}
)

func accessEnv() schedEnv {
	e := newSchedEnv()
	e.svc.Projects = fakeProjects{"p1": {"acme/app"}, "p2": {}}
	e.svc.Access = fakeAccess{
		"owner":  {"p1": RoleOwner, "p2": RoleOwner},
		"admin":  {"p1": RoleProjectAdmin},
		"member": {"p1": "member"},
	}
	return e
}

// task → mock plan → L1 READY → the scheduler starts it.
func TestRunMockPlansAndTheSchedulerStartsIt(t *testing.T) {
	t.Parallel()
	e := accessEnv()
	ctx := t.Context()
	task, err := e.svc.CreateTaskAs(ctx, admin, "p1", "Try the pipeline")
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.RunMock(ctx, admin, task.ID, "flaky")
	if err != nil || got.State != domain.TaskReady || got.PlanRevision != 1 {
		t.Fatalf("RunMock = %+v, %v", got, err)
	}
	rev, _ := e.repo.Revision(ctx, task.ID, 1)
	if s := rev.Plan().Steps; len(s) != 1 || s[0].Key != "flaky" || s[0].Repository != "acme/app" || rev.CreatedBy() != MockPlanner {
		t.Fatalf("mock plan %+v by %s", s, rev.CreatedBy())
	}
	e.pool.set("p1", Candidate{ID: "r1", DiskHeavyAllowed: true})
	e.schedule(t)
	if c := e.disp.take(); len(c) != 1 || c[0] != "assign r1 flaky/1 p1 acme/app" {
		t.Fatalf("commands %v", c)
	}
	d, err := e.svc.TaskDetail(ctx, member, task.ID)
	if err != nil || d.Task.State != domain.TaskRunning || d.Execution == nil || d.Revision == nil || d.CanManage {
		t.Fatalf("detail %+v, %v", d, err)
	}
}

func TestTaskAPIAccess(t *testing.T) {
	t.Parallel()
	e := accessEnv()
	ctx := t.Context()
	task, err := e.svc.CreateTaskAs(ctx, owner, "p1", "T")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateTaskAs(ctx, member, "p1", "T"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member create: %v", err)
	}
	if _, err := e.svc.CreateTaskAs(ctx, other, "p1", "T"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("outsider create: %v", err)
	}
	if _, err := e.svc.TaskDetail(ctx, other, task.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("outsider detail: %v", err)
	}
	if _, err := e.svc.RunMock(ctx, member, task.ID, "success"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member run: %v", err)
	}
	var ve *domain.ValidationError
	if _, err := e.svc.RunMock(ctx, owner, task.ID, "rm -rf"); !errors.As(err, &ve) || ve.Code != "scenario_unknown" {
		t.Fatalf("bad scenario: %v", err)
	}
	t2, _ := e.svc.CreateTaskAs(ctx, owner, "p2", "No repos")
	if _, err := e.svc.RunMock(ctx, owner, t2.ID, "success"); !errors.As(err, &ve) || ve.Code != "repository_required" {
		t.Fatalf("no repository: %v", err)
	}
	if l, err := e.svc.ListTasks(ctx, member, TaskQuery{}); err != nil || len(l.Tasks) != 1 {
		t.Fatalf("member list %d, %v", len(l.Tasks), err)
	}
	if l, _ := e.svc.ListTasks(ctx, owner, TaskQuery{ProjectID: "p2"}); len(l.Tasks) != 1 || l.Tasks[0].Task.ID != t2.ID {
		t.Fatalf("owner list p2 %+v", l)
	}
	if l, _ := e.svc.ListTasks(ctx, other, TaskQuery{}); len(l.Tasks) != 0 {
		t.Fatalf("outsider list %+v", l)
	}
	if _, err := e.svc.CancelTaskAs(ctx, member, task.ID, "x"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member cancel: %v", err)
	}
	if got, err := e.svc.CancelTaskAs(ctx, admin, task.ID, "not needed"); err != nil || got.State != domain.TaskCancelled {
		t.Fatalf("admin cancel %+v, %v", got, err)
	}
}
