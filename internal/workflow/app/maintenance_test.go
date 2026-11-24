package app

import (
	"context"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

type pausedFlag struct{ on bool }

func (p *pausedFlag) SchedulingPaused(context.Context) (bool, error) { return p.on, nil }

// KIN-41: in maintenance mode the scheduler issues no new steps (no task starts, no step is claimed); once the
// mode is off the waiting work is assigned as usual.
func TestMaintenancePausesNewAssignments(t *testing.T) {
	t.Parallel()
	e := newSchedEnv()
	pause := &pausedFlag{on: true}
	e.svc.Pause = pause
	task := e.task(t, onlyStep("git"))
	e.pool.set("p1", Candidate{ID: "r-ok", Tools: []string{"git"}, DiskHeavyAllowed: true})
	e.schedule(t)
	got, err := e.repo.Task(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.TaskReady {
		t.Fatalf("task started in maintenance: %s", got.State)
	}
	if c := e.disp.take(); len(c) != 0 {
		t.Fatalf("commands in maintenance %v", c)
	}
	pause.on = false
	e.schedule(t)
	if c := e.disp.take(); len(c) != 1 {
		t.Fatalf("after maintenance commands %v", c)
	}
}
