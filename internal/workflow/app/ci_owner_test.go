package app

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/messaging"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// ownedCIEnv is a CI env whose project p1 has an Owner, a project admin and a member.
func ownedCIEnv() ciEnv {
	e := newCIEnv()
	e.svc.Access = fakeAccess{
		"owner":  {"p1": RoleOwner},
		"admin":  {"p1": RoleProjectAdmin},
		"member": {"p1": "member"},
	}
	return e
}

// reconcile runs one CI reconciliation pass and delivers what it queued.
func (e ciEnv) reconcile(t *testing.T) {
	t.Helper()
	if err := e.svc.ReconcileCI(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := e.drain(t); err != nil {
		t.Fatal(err)
	}
}

// refused takes the task's green pull request into a refused merge (BLOCKED, reason ci).
func (e ciEnv) refused(t *testing.T) (domain.Task, domain.GitArtifact) {
	t.Helper()
	e.ci.mu.Lock()
	e.ci.mergeErr = messaging.Permanent(errors.New("GitHub refused the merge (HTTP 405)"))
	e.ci.mu.Unlock()
	task, a := e.opened(t)
	e.ci.set("h1", CIStatus{State: domain.CIPassed})
	e.ciEvent(t, a, "green")
	if tk, _ := e.state(t, task.ID); tk.State != domain.TaskBlocked || tk.BlockReason != domain.WaitCI {
		t.Fatalf("refused merge: task %s", tk.State)
	}
	e.ci.mu.Lock()
	e.ci.mergeErr = nil
	e.ci.mu.Unlock()
	return task, a
}

// Acceptance (KIN-105): a repository without CI never auto-merges: its head shows "no check runs" and after
// NoCIBlockAfter without any the task is BLOCKED (reason ci) for the Owner, audited; the Owner's resume starts a new
// window, and check runs that appear later lead to the normal merge.
func TestARepositoryWithoutCIBlocksTheTaskInsteadOfMerging(t *testing.T) {
	t.Parallel()
	e := ownedCIEnv()
	task, _ := e.opened(t)
	e.ci.set("h1", CIStatus{State: domain.CIPending, NoChecks: true})
	e.reconcile(t)
	a := e.artifact(t, task.ID)
	if a.CI != domain.CIPending || a.NoChecksSince.IsZero() {
		t.Fatalf("no check runs not shown: %+v", a)
	}
	e.clock.Add(domain.NoCIBlockAfter - time.Minute)
	e.reconcile(t)
	if tk, _ := e.state(t, task.ID); tk.State != domain.TaskVerifying {
		t.Fatalf("blocked inside the window: %s", tk.State)
	}
	e.clock.Add(2 * time.Minute)
	e.reconcile(t)
	tk, _ := e.state(t, task.ID)
	if tk.State != domain.TaskBlocked || tk.BlockReason != domain.WaitCI || !strings.Contains(tk.Reason.Detail, "no CI check runs") {
		t.Fatalf("after the window: %s %s %q", tk.State, tk.BlockReason, tk.Reason)
	}
	if len(e.ci.merged()) != 0 || !slices.Contains(e.audit.kinds(), KindCIMissing) {
		t.Fatalf("merges %v audit %v", e.ci.merged(), e.audit.kinds())
	}
	e.clock.Add(time.Minute)
	e.reconcile(t) // still no CI: the blocked task stays blocked, nothing merged
	if tk, _ := e.state(t, task.ID); tk.State != domain.TaskBlocked || len(e.ci.merged()) != 0 {
		t.Fatalf("blocked no-CI task: %s merges %v", tk.State, e.ci.merged())
	}
	if _, err := e.svc.ResumeCIAs(t.Context(), owner, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.drain(t); err != nil {
		t.Fatal(err)
	}
	if tk, _ := e.state(t, task.ID); tk.State != domain.TaskVerifying {
		t.Fatalf("resume did not start a new window: %s", tk.State)
	}
	e.ci.set("h1", CIStatus{State: domain.CIPassed})
	e.clock.Add(time.Minute) // the next reconciliation window
	e.reconcile(t)
	if tk, _ := e.state(t, task.ID); tk.State != domain.TaskCompleted || len(e.ci.merged()) != 1 {
		t.Fatalf("CI added later: %s merges %v", tk.State, e.ci.merged())
	}
}

// Acceptance (KIN-105): the Owner or a project admin resumes a task BLOCKED on CI: it is VERIFYING again and CI is
// checked at once, so a refused merge whose CI is green is retried; a member may not, and other tasks are refused.
func TestOwnerResumesATaskBlockedOnCI(t *testing.T) {
	t.Parallel()
	e := ownedCIEnv()
	task, _ := e.refused(t)
	if _, err := e.svc.ResumeCIAs(t.Context(), member, task.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member resume: %v", err)
	}
	got, err := e.svc.ResumeCIAs(t.Context(), admin, task.ID)
	if err != nil || got.State != domain.TaskVerifying {
		t.Fatalf("resume: %+v %v", got.State, err)
	}
	if err := e.drain(t); err != nil {
		t.Fatal(err)
	}
	if tk, _ := e.state(t, task.ID); tk.State != domain.TaskCompleted || len(e.ci.merged()) != 2 {
		t.Fatalf("after resume: %s merges %v", tk.State, e.ci.merged())
	}
	if !slices.Contains(e.audit.kinds(), KindCIResumed) {
		t.Fatalf("audit %v", e.audit.kinds())
	}
	if _, err := e.svc.ResumeCIAs(t.Context(), owner, task.ID); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("resume of a completed task: %v", err)
	}
}

// Acceptance (KIN-105): "Retry merge" clears a refused merge and runs the merge gate again (integrate_dev and green
// CI still required); it needs a refused merge, and a member may not.
func TestOwnerRetriesARefusedMerge(t *testing.T) {
	t.Parallel()
	e := ownedCIEnv()
	task, _ := e.refused(t)
	if _, err := e.svc.RetryMergeAs(t.Context(), member, task.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member retry: %v", err)
	}
	e.gate.set(false)
	got, err := e.svc.RetryMergeAs(t.Context(), owner, task.ID)
	if err != nil || got.State != domain.TaskVerifying {
		t.Fatalf("retry: %s %v", got.State, err)
	}
	if err := e.drain(t); err != nil {
		t.Fatal(err)
	}
	if a := e.artifact(t, task.ID); a.Merge != domain.MergeWaiting || len(e.ci.merged()) != 1 {
		t.Fatalf("the gate was skipped: %+v merges %v", a, e.ci.merged())
	}
	e.gate.set(true)
	e.clock.Add(time.Minute)
	e.reconcile(t)
	if tk, _ := e.state(t, task.ID); tk.State != domain.TaskCompleted || !slices.Contains(e.audit.kinds(), KindMergeRetried) {
		t.Fatalf("after approval: %s audit %v", tk.State, e.audit.kinds())
	}
	if _, err := e.svc.RetryMergeAs(t.Context(), owner, task.ID); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("retry without a refused merge: %v", err)
	}
}

// Review (KIN-105): a task blocked on CI with no open, unmerged pull request of its current run is not resumed
// (nothing would move a VERIFYING task on); the Owner cancels or retries it instead.
func TestResumeNeedsAnOpenPullRequest(t *testing.T) {
	t.Parallel()
	e := ownedCIEnv()
	task, a := e.opened(t)
	a.Merged("m1", e.clock.Now())
	if err := e.sets.SaveArtifact(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.BlockTask(t.Context(), task.ID, domain.WaitCI, "merge failed"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ResumeCIAs(t.Context(), owner, task.ID); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("resume without a pull request: %v", err)
	}
	if tk, _ := e.state(t, task.ID); tk.State != domain.TaskBlocked {
		t.Fatalf("task %s", tk.State)
	}
}
