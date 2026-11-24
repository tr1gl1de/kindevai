package app

import (
	"errors"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

func TestCheckFence(t *testing.T) {
	t.Parallel()
	e := newEnv()
	ctx := t.Context()
	task := e.readyTask(t)
	ex, _ := e.svc.StartExecution(ctx, task.ID)
	id := ex.StepByKey("backend").ID
	l, _ := e.svc.Claim(ctx, id, "r1")
	before, _ := e.repo.Execution(ctx, ex.ID)
	if err := e.svc.CheckFence(ctx, id, "r1", l.Fence); err != nil {
		t.Fatalf("holder = %v", err)
	}
	if err := e.svc.CheckFence(ctx, id, "r2", l.Fence); !errors.Is(err, domain.ErrStaleFence) {
		t.Fatalf("other runner = %v", err)
	}
	if err := e.svc.CheckFence(ctx, "nope", "r1", 1); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown step = %v", err)
	}
	if after, _ := e.repo.Execution(ctx, ex.ID); after.Version != before.Version {
		t.Fatal("a check saved the execution")
	}
}
