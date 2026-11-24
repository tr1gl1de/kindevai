package main

import (
	"errors"
	"fmt"
	"testing"

	runners "github.com/tr1gl1de/kindevai/internal/runners/domain"
	workflow "github.com/tr1gl1de/kindevai/internal/workflow/domain"
)

func TestAttemptErrorMapping(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   error
		want error
	}{
		{fmt.Errorf("x: %w", workflow.ErrStaleFence), runners.ErrStaleAttempt},
		{workflow.ErrLeaseExpired, runners.ErrStaleAttempt},
		{workflow.ErrNotFound, runners.ErrStaleAttempt},
		{workflow.ErrInvalidTransition, runners.ErrEventRejected},
		{workflow.ErrAttemptUnknown, runners.ErrEventRejected},
	} {
		if got := attemptError(c.in); !errors.Is(got, c.want) || !errors.Is(got, c.in) {
			t.Errorf("attemptError(%v) = %v, want %v", c.in, got, c.want)
		}
	}
	transient := workflow.ErrConflict
	if got := attemptError(transient); errors.Is(got, runners.ErrStaleAttempt) || errors.Is(got, runners.ErrEventRejected) {
		t.Errorf("a conflict is transient, got %v", got)
	}
	if attemptError(nil) != nil {
		t.Error("nil")
	}
}
