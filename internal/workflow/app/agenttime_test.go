package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// mockRunning plans a task with the mock planner and lets the scheduler assign it to r1; it returns the task and
// the claimed step with its token.
func mockRunning(t *testing.T, e schedEnv, scenario string) (domain.Task, string, int64) {
	t.Helper()
	ctx := t.Context()
	task, err := e.svc.CreateTaskAs(ctx, admin, "p1", "Timed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.RunMock(ctx, admin, task.ID, scenario); err != nil {
		t.Fatal(err)
	}
	e.pool.set("p1", Candidate{ID: "r1", DiskHeavyAllowed: true})
	e.schedule(t)
	_, ex := e.state(t, task.ID)
	s := ex.Steps[0]
	return task, s.ID, s.Fence
}

// Periods come from the started/completed events on the runner's clock; a redelivered event does not double them;
// the task shows agent time and calendar duration separately, with the agent and the run.
func TestAgentTimeIsRecordedFromRunnerEvents(t *testing.T) {
	t.Parallel()
	e := accessEnv()
	ctx := t.Context()
	task, step, fence := mockRunning(t, e, "success")
	runnerAt := t0.Add(-time.Hour) // the runner's clock is an hour behind: durations stay right
	if err := e.svc.StartStepAt(ctx, step, "r1", fence, runnerAt, "mock"); err != nil {
		t.Fatal(err)
	}
	e.clock.Add(time.Minute)
	if err := e.svc.CompleteStepAt(ctx, step, "r1", fence, runnerAt.Add(40*time.Second)); err != nil {
		t.Fatal(err)
	}
	// a redelivered completion is rejected by the step's state and changes nothing
	if err := e.svc.CompleteStepAt(ctx, step, "r1", fence, runnerAt.Add(time.Hour)); err == nil {
		t.Fatal("a second completion was accepted")
	}
	d, err := e.svc.TaskDetail(ctx, member, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	a := d.Execution.Steps[0].Attempts[0]
	if a.Agent != "mock" || a.RunnerID != "r1" || !a.WorkStart.Equal(runnerAt) {
		t.Fatalf("attempt %+v", a)
	}
	if d.Time.Agent != (domain.AgentTime{Total: 40 * time.Second, Status: domain.WorkComplete}) {
		t.Fatalf("agent time %+v", d.Time.Agent)
	}
	if d.Time.Calendar != time.Minute || d.Time.Running {
		t.Fatalf("calendar %v running %v, want 1m from the execution's start to its end", d.Time.Calendar, d.Time.Running)
	}
}

// A process that ends after its task was cancelled reports its end later: the period closes; until then (and for
// a lost process) the time is incomplete, never shown as a complete record.
func TestALateEndAfterCancellationCompletesTheTime(t *testing.T) {
	t.Parallel()
	e := accessEnv()
	ctx := t.Context()
	task, step, fence := mockRunning(t, e, "hang")
	if err := e.svc.StartStepAt(ctx, step, "r1", fence, t0, "mock"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CancelTaskAs(ctx, admin, task.ID, "stop"); err != nil {
		t.Fatal(err)
	}
	if d, _ := e.svc.TaskDetail(ctx, admin, task.ID); d.Time.Agent.Status != domain.WorkIncomplete {
		t.Fatalf("before the end arrives: %+v", d.Time.Agent)
	}
	err := e.svc.FailStepAt(ctx, step, "r1", fence, "cancelled", t0.Add(5*time.Second))
	if !errors.Is(err, domain.ErrStaleFence) {
		t.Fatalf("late failure = %v, want stale (the runner is told to stop)", err)
	}
	d, _ := e.svc.TaskDetail(ctx, admin, task.ID)
	if d.Time.Agent != (domain.AgentTime{Total: 5 * time.Second, Status: domain.WorkComplete}) {
		t.Fatalf("after the late end: %+v", d.Time.Agent)
	}
}

func TestListTasksFiltersByStateAndSumsAgentTime(t *testing.T) {
	t.Parallel()
	e := accessEnv()
	ctx := t.Context()
	done, step, fence := mockRunning(t, e, "success")
	if err := e.svc.StartStepAt(ctx, step, "r1", fence, t0, "mock"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.CompleteStepAt(ctx, step, "r1", fence, t0.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	idle, err := e.svc.CreateTaskAs(ctx, admin, "p1", "Idle")
	if err != nil {
		t.Fatal(err)
	}
	all, err := e.svc.ListTasks(ctx, member, TaskQuery{ProjectID: "p1"})
	if err != nil || len(all.Tasks) != 2 {
		t.Fatalf("list %+v, %v", all, err)
	}
	if all.Total != (domain.AgentTime{Total: 30 * time.Second, Status: domain.WorkComplete}) {
		t.Fatalf("project total %+v", all.Total)
	}
	for _, it := range all.Tasks {
		if it.Task.ID == idle.ID && it.Time.Agent.Status != domain.WorkNone {
			t.Errorf("idle task time %+v", it.Time)
		}
		if it.Task.ID == done.ID && it.Time.Agent.Total != 30*time.Second {
			t.Errorf("done task time %+v", it.Time)
		}
	}
	created, err := e.svc.ListTasks(ctx, member, TaskQuery{States: []domain.TaskState{domain.TaskCreated}})
	if err != nil || len(created.Tasks) != 1 || created.Tasks[0].Task.ID != idle.ID || created.Total.Total != 0 {
		t.Fatalf("created only %+v, %v", created, err)
	}
	if l, _ := e.svc.ListTasks(ctx, other, TaskQuery{}); len(l.Tasks) != 0 {
		t.Fatalf("outsider list %+v", l)
	}
	if l, err := e.svc.ListTasks(ctx, member, TaskQuery{ProjectID: "p2"}); err != nil || len(l.Tasks) != 0 {
		t.Fatalf("member on a project they cannot see: %+v, %v", l, err)
	}
}

// Retry plans a FAILED task again: the current plan becomes a new revision that passes policy (and approvals)
// again. Only the Owner or a project admin may retry; only a FAILED task can be retried.
func TestRetryPlansAFailedTaskAgain(t *testing.T) {
	t.Parallel()
	e := accessEnv()
	ctx := t.Context()
	task, step, fence := mockRunning(t, e, "fail")
	if _, err := e.svc.RetryTask(ctx, admin, task.ID); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("retry of a running task = %v", err)
	}
	for range 2 {
		if err := e.svc.StartStepAt(ctx, step, "r1", fence, t0, "mock"); err != nil {
			t.Fatal(err)
		}
		if err := e.svc.FailStepAt(ctx, step, "r1", fence, "exit 1", t0.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		e.schedule(t)
		_, ex := e.state(t, task.ID)
		fence = ex.Steps[0].Fence
	}
	if got, _ := e.repo.Task(ctx, task.ID); got.State != domain.TaskFailed {
		t.Fatalf("task %s, want FAILED", got.State)
	}
	if _, err := e.svc.RetryTask(ctx, member, task.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member retry = %v", err)
	}
	if _, err := e.svc.RetryTask(ctx, other, task.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("outsider retry = %v", err)
	}
	got, err := e.svc.RetryTask(ctx, admin, task.ID)
	if err != nil || got.State != domain.TaskReady || got.PlanRevision != 2 || got.Reason != (domain.Reason{}) {
		t.Fatalf("retry = %+v, %v", got, err)
	}
	rev, _ := e.repo.Revision(ctx, task.ID, 2)
	if rev.CreatedBy() != "admin" || len(rev.Plan().Steps) != 1 || rev.Plan().Steps[0].Key != "fail" {
		t.Fatalf("revision 2 by %s: %+v", rev.CreatedBy(), rev.Plan())
	}
}

type fakeLog struct{ n int64 }

func (f fakeLog) AttemptEvents(_ context.Context, _ string, _ int64, after int64, limit int) ([]AttemptEvent, error) {
	var out []AttemptEvent
	for seq := after + 1; seq <= f.n && len(out) < limit; seq++ {
		out = append(out, AttemptEvent{Seq: seq, Kind: "execution.output", Data: "x"})
	}
	return out, nil
}

func (fakeLog) AttemptWorkspace(_ context.Context, step string, fence int64) (AttemptWorkspace, error) {
	return AttemptWorkspace{Prepared: &WorkspacePrepared{Branch: "feature/" + step, Base: "dev", BaseSHA: "abc"},
		Changes: &WorkspaceChanges{HeadSHA: "def", Files: []ChangedFile{{Path: "a.go", Status: "M"}}, FilesTotal: int(fence)}}, nil
}

// Members read an attempt's events in bounded pages after a cursor; a step or attempt of another task, or a task
// they cannot see, is not found.
func TestAttemptEventsArePagedAndScopedToTheTask(t *testing.T) {
	t.Parallel()
	e := accessEnv()
	e.svc.Log = fakeLog{n: MaxEventPage + 5}
	ctx := t.Context()
	task, step, fence := mockRunning(t, e, "success")
	if err := e.svc.StartStepAt(ctx, step, "r1", fence, t0, "mock"); err != nil {
		t.Fatal(err)
	}
	p1, err := e.svc.AttemptEvents(ctx, member, task.ID, step, fence, 0)
	if err != nil || len(p1.Events) != MaxEventPage || !p1.More || p1.Next != MaxEventPage {
		t.Fatalf("page 1: %d more=%v next=%d %v", len(p1.Events), p1.More, p1.Next, err)
	}
	p2, err := e.svc.AttemptEvents(ctx, member, task.ID, step, fence, p1.Next)
	if err != nil || len(p2.Events) != 5 || p2.More || p2.Next != MaxEventPage+5 {
		t.Fatalf("page 2: %d more=%v next=%d %v", len(p2.Events), p2.More, p2.Next, err)
	}
	p3, _ := e.svc.AttemptEvents(ctx, member, task.ID, step, fence, p2.Next)
	if len(p3.Events) != 0 || p3.Next != p2.Next {
		t.Fatalf("page 3 %+v", p3)
	}
	otherTask, _ := e.svc.CreateTaskAs(ctx, admin, "p1", "Other")
	for name, call := range map[string]func() error{
		"outsider":      func() error { _, err := e.svc.AttemptEvents(ctx, other, task.ID, step, fence, 0); return err },
		"another task":  func() error { _, err := e.svc.AttemptEvents(ctx, member, otherTask.ID, step, fence, 0); return err },
		"unknown fence": func() error { _, err := e.svc.AttemptEvents(ctx, member, task.ID, step, fence+1, 0); return err },
		"unknown step":  func() error { _, err := e.svc.AttemptEvents(ctx, member, task.ID, "nope", fence, 0); return err },
	} {
		if err := call(); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s: %v, want not found", name, err)
		}
	}
}

// Whoever may see the task reads an attempt's latest workspace reports (KIN-70); a step or attempt of another task,
// or a task they cannot see, is not found; without a recorded log there are no reports.
func TestAttemptWorkspaceIsScopedToTheTask(t *testing.T) {
	t.Parallel()
	e := accessEnv()
	ctx := t.Context()
	task, step, fence := mockRunning(t, e, "success")
	if err := e.svc.StartStepAt(ctx, step, "r1", fence, t0, "mock"); err != nil {
		t.Fatal(err)
	}
	if w, err := e.svc.AttemptWorkspace(ctx, member, task.ID, step, fence); err != nil || w.Prepared != nil || w.Changes != nil {
		t.Fatalf("without a log %+v %v", w, err)
	}
	e.svc.Log = fakeLog{}
	w, err := e.svc.AttemptWorkspace(ctx, member, task.ID, step, fence)
	if err != nil || w.Prepared == nil || w.Prepared.Branch != "feature/"+step || w.Changes == nil ||
		w.Changes.FilesTotal != int(fence) || len(w.Changes.Files) != 1 {
		t.Fatalf("workspace %+v %v", w, err)
	}
	otherTask, _ := e.svc.CreateTaskAs(ctx, admin, "p1", "Other")
	for name, call := range map[string]func() error{
		"outsider":      func() error { _, err := e.svc.AttemptWorkspace(ctx, other, task.ID, step, fence); return err },
		"another task":  func() error { _, err := e.svc.AttemptWorkspace(ctx, member, otherTask.ID, step, fence); return err },
		"unknown fence": func() error { _, err := e.svc.AttemptWorkspace(ctx, member, task.ID, step, fence+1); return err },
		"unknown step":  func() error { _, err := e.svc.AttemptWorkspace(ctx, member, task.ID, "nope", fence); return err },
	} {
		if err := call(); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s: %v, want not found", name, err)
		}
	}
}

// Only the execution's own refusal of a report records a late end; other errors (saving, the task's follow-up)
// are returned unchanged and record nothing.
func TestOnlyARefusedReportRecordsALateEnd(t *testing.T) {
	t.Parallel()
	e := accessEnv()
	ctx := t.Context()
	_, step, fence := mockRunning(t, e, "success")
	if err := e.svc.StartStepAt(ctx, step, "r1", fence, t0, "mock"); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("disk full")
	if err := e.svc.lateWorkEnd(ctx, boom, step, "r1", fence, t0.Add(time.Second)); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if err := e.svc.lateWorkEnd(ctx, domain.ErrInvalidTransition, step, "r1", fence, t0.Add(time.Second)); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("err = %v", err)
	}
	ex, _ := e.repo.ExecutionByStep(ctx, step)
	if a := ex.Step(step).Attempts[0]; a.WorkEnd != nil {
		t.Fatalf("an unrelated error recorded an end: %+v", a)
	}
}
