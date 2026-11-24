package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

type fakeNotifier struct {
	mu       sync.Mutex
	finished []string // id:state
	fail     error
}

func (f *fakeNotifier) TaskFinished(_ context.Context, t domain.Task) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.finished = append(f.finished, t.ID+":"+string(t.State))
	return nil
}

// failTask runs the task's first step into a final failure (all attempts fail).
func (e env) failTask(t *testing.T, task domain.Task) error {
	t.Helper()
	ctx := t.Context()
	ex, err := e.svc.StartExecution(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	id := ex.StepByKey("backend").ID
	var last error
	for range DefaultMaxAttempts {
		lease, err := e.svc.Claim(ctx, id, "r1")
		if err != nil {
			t.Fatal(err)
		}
		_ = e.svc.StartStep(ctx, id, "r1", lease.Fence)
		last = e.svc.FailStep(ctx, id, "r1", lease.Fence, "exit 1")
	}
	return last
}

func TestFailedTaskNotifiesOnce(t *testing.T) {
	t.Parallel()
	e := newEnv()
	n := &fakeNotifier{}
	e.svc.Notifier = n
	task := e.readyTask(t)
	if err := e.failTask(t, task); err != nil {
		t.Fatal(err)
	}
	if len(n.finished) != 1 || n.finished[0] != task.ID+":FAILED" {
		t.Fatalf("finished = %v", n.finished)
	}
}

func TestCompletedTaskNotifiesButOtherTransitionsDoNot(t *testing.T) {
	t.Parallel()
	e := newEnv()
	n := &fakeNotifier{}
	e.svc.Notifier = n
	task := e.readyTask(t)
	ctx := t.Context()
	if _, err := e.svc.BlockTask(ctx, task.ID, domain.WaitRunner, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ResumeTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if len(n.finished) != 0 {
		t.Fatalf("finished after block/resume = %v", n.finished)
	}
	// VERIFYING -> COMPLETED goes through updateTask, the path every task use case shares.
	for _, to := range []domain.TaskState{domain.TaskRunning, domain.TaskVerifying, domain.TaskCompleted} {
		if _, err := e.svc.updateTask(ctx, task.ID, func(_ context.Context, tk *domain.Task, now time.Time) error {
			return tk.Transition(to, domain.Reason{}, now)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if len(n.finished) != 1 || n.finished[0] != task.ID+":COMPLETED" {
		t.Fatalf("finished = %v", n.finished)
	}
}

func TestCancelledTaskDoesNotNotify(t *testing.T) {
	t.Parallel()
	e := newEnv()
	n := &fakeNotifier{}
	e.svc.Notifier = n
	task := e.readyTask(t)
	if _, err := e.svc.CancelTask(t.Context(), task.ID, "not needed"); err != nil {
		t.Fatal(err)
	}
	if len(n.finished) != 0 {
		t.Fatalf("finished = %v", n.finished)
	}
}

func TestNotifierFailureFailsTheTransition(t *testing.T) {
	t.Parallel()
	e := newEnv()
	boom := errors.New("outbox down")
	e.svc.Notifier = &fakeNotifier{fail: boom}
	task := e.readyTask(t)
	if err := e.failTask(t, task); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the notifier's error", err)
	}
}
