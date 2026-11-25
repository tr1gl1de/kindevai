package app

import (
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

func (e plannerEnv) autoPlan(t *testing.T, taskID string) AutoPlanOutcome {
	t.Helper()
	got, err := e.svc.AutoStartPlanning(t.Context(), taskID, "admin")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// KIN-85 AC: an intake task is planned automatically — CREATED → ANALYZING with a planning run requested by the
// initiator; the issue's context is read through the outbox (nothing external from the caller).
func TestAutoStartPlanningCreatesARunThroughTheOutbox(t *testing.T) {
	t.Parallel()
	e := newPlannerEnv()
	ob := &alertOutbox{}
	e.svc.Outbox, e.svc.IssueContext = ob, &fakeIssueContext{}
	task := e.newTask(t, "KIN-7")
	if got := e.autoPlan(t, task.ID); got != AutoPlanStarted {
		t.Fatalf("outcome %q", got)
	}
	if got, _ := e.repo.Task(t.Context(), task.ID); got.State != domain.TaskAnalyzing {
		t.Fatalf("task %s", got.State)
	}
	r := e.run(t, task.ID)
	if r.State != domain.PlanningPreparing || r.CreatedBy != "admin" {
		t.Fatalf("run %+v", r)
	}
	if len(ob.msgs) != 1 || ob.msgs[0].Topic != TopicPlanningContext {
		t.Fatalf("outbox %+v", ob.msgs)
	}
	e.deliverContext(t, ob, 1)
	if a := e.assigned(t); a.Role != RolePlanner {
		t.Fatalf("assignment %+v", a)
	}
}

// KIN-85 AC: idempotent — a task that is already planned (or was planned before, or left CREATED) never gets a
// second automatic run.
func TestAutoStartPlanningIsIdempotent(t *testing.T) {
	t.Parallel()
	e := newPlannerEnv()
	task := e.newTask(t, "")
	if got := e.autoPlan(t, task.ID); got != AutoPlanStarted {
		t.Fatalf("first outcome %q", got)
	}
	first := e.run(t, task.ID)
	if got := e.autoPlan(t, task.ID); got != AutoPlanNotNew {
		t.Fatalf("second outcome %q", got)
	}
	if again := e.run(t, task.ID); again.ID != first.ID {
		t.Fatalf("a second run %s was created", again.ID)
	}
	// A run that ended (cancelled) still counts: auto-start is once per task.
	if _, err := e.svc.CancelTask(t.Context(), task.ID, "no"); err != nil {
		t.Fatal(err)
	}
	if got := e.autoPlan(t, task.ID); got != AutoPlanNotNew {
		t.Fatalf("after cancel %q", got)
	}
}

// KIN-85: without the planner step the task simply stays CREATED.
func TestAutoStartPlanningWithoutPlanner(t *testing.T) {
	t.Parallel()
	e := newPlannerEnv()
	task := e.newTask(t, "")
	e.svc.Planning = nil
	if got := e.autoPlan(t, task.ID); got != AutoPlanUnavailable {
		t.Fatalf("outcome %q", got)
	}
	if got, _ := e.repo.Task(t.Context(), task.ID); got.State != domain.TaskCreated {
		t.Fatalf("task %s", got.State)
	}
}

// KIN-85: a project gets at most AutoPlanPerHour planning runs started in a rolling hour by the intake; above it the
// task stays CREATED for a manual start.
func TestAutoStartPlanningIsRateLimitedPerProject(t *testing.T) {
	t.Parallel()
	e := newPlannerEnv()
	e.plans.tasks = e.repo
	for i := range AutoPlanPerHour {
		if got := e.autoPlan(t, e.newTask(t, "").ID); got != AutoPlanStarted {
			t.Fatalf("run %d: %q", i, got)
		}
	}
	over := e.newTask(t, "")
	if got := e.autoPlan(t, over.ID); got != AutoPlanRateLimited {
		t.Fatalf("over the cap: %q", got)
	}
	if got, _ := e.repo.Task(t.Context(), over.ID); got.State != domain.TaskCreated {
		t.Fatalf("task %s", got.State)
	}
	e.clock.Add(time.Hour + time.Second)
	if got := e.autoPlan(t, over.ID); got != AutoPlanStarted {
		t.Fatalf("an hour later: %q", got)
	}
}

// KIN-85: the automatic run obeys routing like a manual one — no permitted CLI (e.g. the Owner's emergency stop)
// BLOCKs the task with the reason, maintenance mode holds it back.
func TestAutoStartedPlanningRespectsRoutingAndMaintenance(t *testing.T) {
	t.Parallel()
	e := newPlannerEnv()
	pause := &pausedFlag{on: true}
	e.svc.Pause = pause
	task := e.newTask(t, "")
	e.autoPlan(t, task.ID)
	if e.schedule(t); len(e.disp.takeAssigned()) != 0 {
		t.Fatal("assigned in maintenance mode")
	}
	pause.on = false
	e.router.set(func(r *fakeRouter) {
		r.chains["planner"], r.blocked = nil, "no permitted AI CLI for role planner: claude stopped"
	})
	e.schedule(t)
	got, _ := e.repo.Task(t.Context(), task.ID)
	if got.State != domain.TaskBlocked || got.BlockReason != domain.WaitAgent {
		t.Fatalf("task %+v", got)
	}
	if len(e.disp.takeAssigned()) != 0 {
		t.Fatal("assigned without a permitted CLI")
	}
}
