package domain

import (
	"errors"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func newTask(t *testing.T) Task {
	t.Helper()
	task, err := NewTask("t1", "p1", "Add login page", "KIN-7", "u1", t0)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestNewTaskValidatesInputAndKeepsTheYouTrackReference(t *testing.T) {
	t.Parallel()
	task := newTask(t)
	if task.State != TaskCreated || task.YouTrackIssue != "KIN-7" || task.PlanRevision != 0 || task.ProjectID != "p1" {
		t.Fatalf("task = %+v", task)
	}
	noIssue, err := NewTask("t2", "p1", "  Manual task  ", "", "u1", t0)
	if err != nil || noIssue.YouTrackIssue != "" || noIssue.Title != "Manual task" {
		t.Fatalf("task without an issue = %+v, %v", noIssue, err)
	}
	tests := []struct {
		name, project, title, issue, field string
	}{
		{"empty title", "p1", " ", "", "title"},
		{"long title", "p1", strings.Repeat("x", 201), "", "title"},
		{"control characters", "p1", "a\x00b", "", "title"},
		{"lowercase issue", "p1", "x", "kin-7", "youtrackIssue"},
		{"issue without number", "p1", "x", "KIN-", "youtrackIssue"},
		{"no project", "", "x", "", "projectId"},
	}
	for _, tt := range tests {
		_, err := NewTask("t", tt.project, tt.title, tt.issue, "u1", t0)
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Field != tt.field {
			t.Errorf("%s: err = %v, want a validation error on %s", tt.name, err, tt.field)
		}
	}
}

// taskTable is the Task FSM from KIN-A-14 with the KIN-21 decisions for BLOCKED/FAILED/CANCELLED. Leaving
// BLOCKED other than to FAILED/CANCELLED is Resume, which returns to the state the task was blocked in.
var taskTable = map[TaskState][]TaskState{
	TaskCreated:          {TaskAnalyzing, TaskBlocked, TaskCancelled},
	TaskAnalyzing:        {TaskReady, TaskAwaitingApproval, TaskBlocked, TaskFailed, TaskCancelled},
	TaskReady:            {TaskAnalyzing, TaskAwaitingApproval, TaskRunning, TaskBlocked, TaskCancelled},
	TaskAwaitingApproval: {TaskAnalyzing, TaskReady, TaskBlocked, TaskCancelled},
	TaskRunning:          {TaskVerifying, TaskBlocked, TaskFailed, TaskCancelled},
	TaskVerifying:        {TaskRunning, TaskCompleted, TaskBlocked, TaskFailed, TaskCancelled},
	TaskBlocked:          {TaskFailed, TaskCancelled},
	TaskCompleted:        nil,
	TaskFailed:           nil,
	TaskCancelled:        nil,
}

func reasonFor(to TaskState) Reason {
	switch to {
	case TaskBlocked:
		return Reason{Code: WaitRunner}
	case TaskFailed, TaskCancelled:
		return Reason{Detail: "because"}
	}
	return Reason{}
}

func TestTaskTransitionTable(t *testing.T) {
	t.Parallel()
	for _, from := range AllTaskStates {
		for _, to := range AllTaskStates {
			want := slices.Contains(taskTable[from], to)
			if got := TaskTransitionAllowed(from, to); got != want {
				t.Errorf("%s -> %s allowed = %v, want %v", from, to, got, want)
			}
			task := Task{ID: "t", State: from}
			if from == TaskBlocked {
				task.BlockReason, task.BlockedFrom = WaitRunner, TaskRunning
			}
			err := task.Transition(to, reasonFor(to), t0)
			if want && err != nil {
				t.Errorf("%s -> %s: %v", from, to, err)
			}
			if !want && !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("%s -> %s: err = %v, want ErrInvalidTransition", from, to, err)
			}
			if !want && task.State != from {
				t.Errorf("%s -> %s: a rejected transition changed the state to %s", from, to, task.State)
			}
		}
	}
}

func TestTaskTransitionReasons(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		to     TaskState
		reason Reason
		ok     bool
	}{
		{"blocked needs a reason code", TaskBlocked, Reason{}, false},
		{"blocked with an unknown code", TaskBlocked, Reason{Code: "weather"}, false},
		{"blocked on network", TaskBlocked, Reason{Code: WaitNetwork, Detail: "proxy down"}, true},
		{"blocked on cli limit", TaskBlocked, Reason{Code: WaitCLILimit}, true},
		{"failed needs a detail", TaskFailed, Reason{Detail: "  "}, false},
		{"failed takes no code", TaskFailed, Reason{Code: WaitRunner, Detail: "x"}, false},
		{"failed with a detail", TaskFailed, Reason{Detail: "step build failed"}, true},
		{"failed with a huge detail", TaskFailed, Reason{Detail: strings.Repeat("x", 1001)}, false},
		{"cancelled needs a detail", TaskCancelled, Reason{}, false},
		{"ready takes no reason", TaskReady, Reason{Detail: "x"}, false},
	}
	for _, tt := range tests {
		task := Task{ID: "t", State: TaskAnalyzing}
		err := task.Transition(tt.to, tt.reason, t0)
		if tt.ok != (err == nil) {
			t.Errorf("%s: err = %v", tt.name, err)
		}
		if err == nil && (task.Reason.Code != tt.reason.Code || task.Reason.Detail != strings.TrimSpace(tt.reason.Detail)) {
			t.Errorf("%s: reason = %+v", tt.name, task.Reason)
		}
	}
}

// TestTaskFSMRandomWalk is the property test: from any reachable state, random transitions are accepted exactly
// when the table allows them, terminal states never change, the reason is set exactly in BLOCKED/FAILED/CANCELLED,
// and Resume returns a blocked task to where it was.
func TestTaskFSMRandomWalk(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(21, 21)) //nolint:gosec // deterministic test input
	for walk := range 500 {
		task := Task{ID: "t", State: TaskCreated}
		for range 30 {
			from := task.State
			if from == TaskBlocked && rng.IntN(3) == 0 {
				back := task.BlockedFrom
				if err := task.Resume(t0); err != nil || task.State != back {
					t.Fatalf("walk %d: resume from %s = %v, state %s", walk, back, err, task.State)
				}
				continue
			}
			to := AllTaskStates[rng.IntN(len(AllTaskStates))]
			err := task.Transition(to, reasonFor(to), t0)
			allowed := slices.Contains(taskTable[from], to)
			switch {
			case allowed && err != nil:
				t.Fatalf("walk %d: %s -> %s rejected: %v", walk, from, to, err)
			case !allowed && err == nil:
				t.Fatalf("walk %d: %s -> %s accepted", walk, from, to)
			case !allowed && task.State != from:
				t.Fatalf("walk %d: rejected %s -> %s changed state", walk, from, to)
			}
			if from.Terminal() && task.State != from {
				t.Fatalf("walk %d: left terminal state %s", walk, from)
			}
			hasReason := task.Reason != (Reason{})
			if wants := task.State == TaskBlocked || task.State == TaskFailed || task.State == TaskCancelled; hasReason != wants {
				t.Fatalf("walk %d: state %s with reason %+v", walk, task.State, task.Reason)
			}
			if task.State == TaskBlocked && (task.BlockedFrom == "" || task.BlockedFrom == TaskBlocked || task.BlockedFrom.Terminal()) {
				t.Fatalf("walk %d: blocked from %q", walk, task.BlockedFrom)
			}
		}
	}
}

func TestResumeOnlyFromBlocked(t *testing.T) {
	t.Parallel()
	task := newTask(t)
	if err := task.Resume(t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("resume of a created task = %v", err)
	}
	if err := task.Transition(TaskBlocked, Reason{Code: WaitApproval}, t0); err != nil {
		t.Fatal(err)
	}
	if err := task.Resume(t0.Add(time.Minute)); err != nil || task.State != TaskCreated || task.BlockedFrom != "" ||
		task.Reason != (Reason{}) || !task.UpdatedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("resume = %v, task %+v", err, task)
	}
}

func TestAcceptPlanRoutesByRisk(t *testing.T) {
	t.Parallel()
	tests := []struct {
		risk Risk
		want TaskState
	}{
		{RiskL0, TaskReady}, {RiskL1, TaskReady}, {RiskL2, TaskAwaitingApproval}, {RiskL3, TaskAwaitingApproval},
	}
	for _, tt := range tests {
		task := Task{ID: "t", State: TaskAnalyzing}
		if err := task.AcceptPlan(1, tt.risk, t0); err != nil || task.State != tt.want || task.PlanRevision != 1 {
			t.Errorf("%s: err %v, task %+v", tt.risk, err, task)
		}
	}
}

func TestAcceptPlanRevisionFromAChatReplacesTheCurrentOne(t *testing.T) {
	t.Parallel()
	task := Task{ID: "t", State: TaskAwaitingApproval, PlanRevision: 1}
	if err := task.AcceptPlan(2, RiskL3, t0); err != nil || task.State != TaskAwaitingApproval || task.PlanRevision != 2 {
		t.Fatalf("err %v, task %+v", err, task)
	}
	if err := task.AcceptPlan(3, RiskL1, t0); err != nil || task.State != TaskReady || task.PlanRevision != 3 {
		t.Fatalf("err %v, task %+v", err, task)
	}
	if err := task.AcceptPlan(3, RiskL1, t0); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("revision numbers only grow: %v", err)
	}
	if err := task.AcceptPlan(4, "L9", t0); err == nil {
		t.Fatal("unknown risk accepted")
	}
	for _, s := range []TaskState{TaskCreated, TaskRunning, TaskVerifying, TaskCompleted, TaskBlocked, TaskCancelled} {
		task := Task{ID: "t", State: s, PlanRevision: 1}
		if err := task.AcceptPlan(2, RiskL0, t0); !errors.Is(err, ErrInvalidTransition) || task.PlanRevision != 1 {
			t.Errorf("%s: accept plan = %v", s, err)
		}
	}
}

func TestApproveBindsToTheCurrentRevision(t *testing.T) {
	t.Parallel()
	task := Task{ID: "t", State: TaskAwaitingApproval, PlanRevision: 2}
	if err := task.Approve(1, t0); !errors.Is(err, ErrStaleRevision) || task.State != TaskAwaitingApproval {
		t.Fatalf("approving an old revision = %v", err)
	}
	if err := task.Approve(2, t0); err != nil || task.State != TaskReady {
		t.Fatalf("approve = %v, state %s", err, task.State)
	}
	if err := task.Approve(2, t0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("approving a ready task = %v", err)
	}
}

func TestRiskLevels(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		in             string
		ok, needsOwner bool
	}{{"L0", true, false}, {"L1", true, false}, {"L2", true, true}, {"L3", true, true}, {"l2", false, false}, {"", false, false}} {
		r, err := ParseRisk(tt.in)
		if tt.ok != (err == nil) {
			t.Errorf("%q: %v", tt.in, err)
			continue
		}
		if err == nil && (r.RequiresApproval() != tt.needsOwner || r.IntegrateDevAutoApproved() == tt.needsOwner) {
			t.Errorf("%q: approval rules wrong", tt.in)
		}
	}
	if !RiskL1.AtLeast(RiskL0) || RiskL0.AtLeast(RiskL1) || !RiskL2.AtLeast(RiskL2) {
		t.Fatal("risk ordering")
	}
}
