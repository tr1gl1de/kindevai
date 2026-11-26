package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
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

func TestRetentionFlags(t *testing.T) {
	t.Parallel()
	parse := func(args ...string) (runners.RetentionPolicy, error) {
		fset := flag.NewFlagSet("t", flag.ContinueOnError)
		fset.SetOutput(io.Discard)
		f := addRetentionFlags(fset, func(string) string { return "" })
		if err := fset.Parse(args); err != nil {
			return runners.RetentionPolicy{}, err
		}
		return f.policy()
	}
	if p, err := parse(); err != nil || p != runners.DefaultRetention() {
		t.Fatalf("defaults %+v %v", p, err)
	}
	p, err := parse("--runner-output-retention-days", "3", "--runner-event-retention-days", "7", "--runner-output-limit-mb", "256")
	if err != nil || p.Output != 3*runners.Day || p.Events != 7*runners.Day || p.MaxOutputBytes != 256<<20 {
		t.Fatalf("set %+v %v", p, err)
	}
	for _, args := range [][]string{
		{"--runner-output-retention-days", "x"},
		{"--runner-output-retention-days", "0"},
		{"--runner-event-retention-days", "3"}, // shorter than output (14)
		{"--runner-output-limit-mb", "8"},      // below one attempt (16 MiB)
		{"--runner-output-limit-mb", "99999999999999"},
	} {
		if _, err := parse(args...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	fset := flag.NewFlagSet("t", flag.ContinueOnError)
	env := map[string]string{"KINDEV_RUNNER_EVENT_RETENTION_DAYS": "60"}
	f := addRetentionFlags(fset, func(k string) string { return env[k] })
	_ = fset.Parse(nil)
	if p, err := f.policy(); err != nil || p.Events != 60*runners.Day {
		t.Fatalf("from the environment %+v %v", p, err)
	}
}
