package app

import (
	"errors"
	"testing"
	"time"

	auditapp "github.com/tr1gl1de/kindevai/internal/audit/app"
	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

// noCIBlocked takes the task's pull request without any check run past NoCIBlockAfter: BLOCKED (reason ci).
func (e ciEnv) noCIBlocked(t *testing.T) domain.Task {
	t.Helper()
	task, _ := e.opened(t)
	e.ci.set("h1", CIStatus{State: domain.CIPending, NoChecks: true})
	e.reconcile(t)
	e.clock.Add(domain.NoCIBlockAfter + time.Minute)
	e.reconcile(t)
	if tk, _ := e.state(t, task.ID); tk.State != domain.TaskBlocked || tk.BlockReason != domain.WaitCI {
		t.Fatalf("no CI: task %s %s", tk.State, tk.BlockReason)
	}
	return task
}

func (e ciEnv) auditEntry(kind string) (auditapp.Entry, bool) {
	e.audit.mu.Lock()
	defer e.audit.mu.Unlock()
	for _, x := range e.audit.entries {
		if x.Kind == kind {
			return x, true
		}
	}
	return auditapp.Entry{}, false
}

// Acceptance (KIN-113): the Owner explicitly merges a task whose pull request has no CI check at all; the action
// is audited as the Owner's, still goes through the merge gate (integrate_dev held → not merged; a later
// reconciliation neither merges past the gate nor blocks the task again) and the merge record says CI was skipped.
// A project admin or a member may not: approving merges is the Owner's.
func TestOwnerMergesATaskWithoutCI(t *testing.T) {
	t.Parallel()
	e := ownedCIEnv()
	task := e.noCIBlocked(t)
	if _, err := e.svc.MergeWithoutCIAs(t.Context(), admin, task.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("admin: %v", err)
	}
	if _, err := e.svc.MergeWithoutCIAs(t.Context(), member, task.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member: %v", err)
	}
	e.gate.set(false)
	got, err := e.svc.MergeWithoutCIAs(t.Context(), owner, task.ID)
	if err != nil || got.State != domain.TaskVerifying {
		t.Fatalf("merge without CI: %s %v", got.State, err)
	}
	if err := e.drain(t); err != nil {
		t.Fatal(err)
	}
	if a := e.artifact(t, task.ID); a.Merge != domain.MergeWaiting || len(e.ci.merged()) != 0 {
		t.Fatalf("integrate_dev skipped: %+v merges %v", a, e.ci.merged())
	}
	e.clock.Add(domain.NoCIBlockAfter)
	e.reconcile(t)
	if tk, _ := e.state(t, task.ID); tk.State != domain.TaskVerifying || len(e.ci.merged()) != 0 {
		t.Fatalf("held waiver: %s merges %v", tk.State, e.ci.merged())
	}
	e.gate.set(true)
	e.clock.Add(time.Minute)
	e.reconcile(t)
	if tk, _ := e.state(t, task.ID); tk.State != domain.TaskCompleted || len(e.ci.merged()) != 1 {
		t.Fatalf("after approval: %s merges %v", tk.State, e.ci.merged())
	}
	act, ok := e.auditEntry(KindMergedWithoutCI)
	if !ok || act.Actor.ID != owner.UserID {
		t.Fatalf("audit %v: %+v", e.audit.kinds(), act)
	}
	if m, ok := e.auditEntry(KindMerged); !ok || m.Details["ciWaived"] != "true" {
		t.Fatalf("merge record: %+v", m)
	}
}

// Acceptance (KIN-113): "merge without CI" is for a pull request with no check run only: red CI or running checks
// refuse it and the task stays blocked; so does a task that is not blocked on CI.
func TestMergeWithoutCIIsOnlyForPullRequestsWithoutChecks(t *testing.T) {
	t.Parallel()
	for _, st := range []CIStatus{{State: domain.CIFailed, Detail: "lint"}, {State: domain.CIPending}} {
		e := ownedCIEnv()
		task := e.noCIBlocked(t)
		e.ci.set("h1", st)
		e.clock.Add(time.Minute)
		e.reconcile(t)
		if _, err := e.svc.MergeWithoutCIAs(t.Context(), owner, task.ID); !errors.Is(err, domain.ErrInvalidTransition) {
			t.Fatalf("%s CI: %v", st.State, err)
		}
		if tk, _ := e.state(t, task.ID); tk.State != domain.TaskBlocked || len(e.ci.merged()) != 0 {
			t.Fatalf("%s CI: task %s merges %v", st.State, tk.State, e.ci.merged())
		}
	}
	e := ownedCIEnv()
	task, _ := e.opened(t)
	if _, err := e.svc.MergeWithoutCIAs(t.Context(), owner, task.ID); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("verifying task: %v", err)
	}
}

// Acceptance (KIN-113): the waiver holds for the head without check runs only: CI that appears before the merge
// (read again right before it) is followed instead, here red → a CI fix round, nothing merged.
func TestMergeWithoutCIFollowsCIThatAppearsBeforeTheMerge(t *testing.T) {
	t.Parallel()
	e := ownedCIEnv()
	task := e.noCIBlocked(t)
	if _, err := e.svc.MergeWithoutCIAs(t.Context(), owner, task.ID); err != nil {
		t.Fatal(err)
	}
	e.ci.set("h1", CIStatus{State: domain.CIFailed, Detail: "tests"})
	if err := e.drain(t); err != nil {
		t.Fatal(err)
	}
	if len(e.ci.merged()) != 0 {
		t.Fatalf("merged despite red CI: %v", e.ci.merged())
	}
	if tk, _ := e.state(t, task.ID); tk.State != domain.TaskRunning {
		t.Fatalf("red CI after the waiver: task %s", tk.State)
	}
}

// Review (KIN-113): a refused merge is retried, not merged without CI; a refused project admin waives nothing and
// leaves no audit entry.
func TestMergeWithoutCIRefusesARefusedMergeAndAProjectAdmin(t *testing.T) {
	t.Parallel()
	e := ownedCIEnv()
	task, _ := e.refused(t)
	if _, err := e.svc.MergeWithoutCIAs(t.Context(), owner, task.ID); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("refused merge: %v", err)
	}
	f := ownedCIEnv()
	blocked := f.noCIBlocked(t)
	if _, err := f.svc.MergeWithoutCIAs(t.Context(), admin, blocked.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("admin: %v", err)
	}
	if a := f.artifact(t, blocked.ID); a.CIWaivedHead != "" {
		t.Fatalf("admin waived: %+v", a)
	}
	if _, ok := f.auditEntry(KindMergedWithoutCI); ok {
		t.Fatal("admin's refused action audited as done")
	}
}
