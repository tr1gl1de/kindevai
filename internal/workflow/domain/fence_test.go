package domain

import (
	"errors"
	"testing"
)

// CheckHolder is the fencing check for runner reports that change no state (KIN-22: accepted, output events).
func TestCheckHolder(t *testing.T) {
	t.Parallel()
	e := newExecution(t, 2)
	id := e.StepByKey("backend").ID
	if err := e.CheckHolder(id, "r1", 1); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("unclaimed step = %v", err)
	}
	l1, _ := e.Claim(id, "r1", t0, ttl)
	if err := e.CheckHolder(id, "r1", l1.Fence); err != nil {
		t.Fatalf("holder = %v", err)
	}
	for name, c := range map[string]struct {
		runner string
		fence  int64
	}{"other runner": {"r2", l1.Fence}, "old token": {"r1", l1.Fence - 1}, "future token": {"r1", l1.Fence + 1}} {
		if err := e.CheckHolder(id, c.runner, c.fence); !errors.Is(err, ErrStaleFence) {
			t.Errorf("%s = %v", name, err)
		}
	}
	if err := e.CheckHolder("nope", "r1", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown step = %v", err)
	}
	_ = e.Cancel("stop", t0)
	if err := e.CheckHolder(id, "r1", l1.Fence); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("cancelled execution = %v", err)
	}
}
