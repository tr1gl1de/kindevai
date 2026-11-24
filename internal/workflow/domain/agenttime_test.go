package domain

import (
	"testing"
	"time"
)

// runningAttempt claims and starts the first step of a fresh execution for r1 and returns its id and token.
func runningAttempt(t *testing.T) (Execution, string, int64) {
	t.Helper()
	e := newExecution(t, 2)
	s := e.StepByKey("lint")
	l, err := e.Claim(s.ID, "r1", t0, ttl)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(s.ID, "r1", l.Fence, t0, ttl); err != nil {
		t.Fatal(err)
	}
	return e, s.ID, l.Fence
}

func attemptWork(t *testing.T, e *Execution, stepID string, now time.Time) (WorkStatus, time.Duration) {
	t.Helper()
	s := e.Step(stepID)
	return s.Attempts[len(s.Attempts)-1].Work(now)
}

func TestAgentTimeComesFromTheRunnersStartAndEnd(t *testing.T) {
	t.Parallel()
	e, id, fence := runningAttempt(t)
	start := t0.Add(2 * time.Second) // runner clock
	if !e.MarkWorkStart(id, "r1", fence, "mock", start, t0) {
		t.Fatal("start not recorded")
	}
	if st, d := attemptWork(t, &e, id, t0.Add(10*time.Second)); st != WorkRunning || d != 8*time.Second {
		t.Fatalf("running work = %s %v, want running 8s", st, d)
	}
	if err := e.Succeed(id, "r1", fence, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !e.MarkWorkEnd(id, "r1", fence, start.Add(45*time.Second), t0) {
		t.Fatal("end not recorded")
	}
	a := e.Step(id).Attempts[0]
	if a.Agent != "mock" || !a.WorkStart.Equal(start) || !a.WorkEnd.Equal(start.Add(45*time.Second)) {
		t.Fatalf("attempt = %+v", a)
	}
	if st, d := a.Work(t0.Add(time.Hour)); st != WorkComplete || d != 45*time.Second {
		t.Fatalf("work = %s %v, want complete 45s", st, d)
	}
}

func TestRepeatedWorkMarksDoNotDoubleTheTime(t *testing.T) {
	t.Parallel()
	e, id, fence := runningAttempt(t)
	e.MarkWorkStart(id, "r1", fence, "mock", t0, t0)
	if e.MarkWorkStart(id, "r1", fence, "other", t0.Add(time.Minute), t0) {
		t.Error("second start changed the attempt")
	}
	e.MarkWorkEnd(id, "r1", fence, t0.Add(10*time.Second), t0)
	if e.MarkWorkEnd(id, "r1", fence, t0.Add(time.Hour), t0) {
		t.Error("second end changed the attempt")
	}
	if st, d := attemptWork(t, &e, id, t0.Add(2*time.Hour)); st != WorkComplete || d != 10*time.Second {
		t.Fatalf("work = %s %v, want complete 10s", st, d)
	}
	if a := e.Step(id).Attempts[0]; a.Agent != "mock" {
		t.Errorf("agent = %q", a.Agent)
	}
}

func TestWorkMarksNeedTheAttemptsRunnerAndToken(t *testing.T) {
	t.Parallel()
	e, id, fence := runningAttempt(t)
	if e.MarkWorkStart(id, "r2", fence, "mock", t0, t0) || e.MarkWorkStart(id, "r1", fence+1, "mock", t0, t0) ||
		e.MarkWorkStart("nope", "r1", fence, "mock", t0, t0) {
		t.Fatal("a mark from another runner, token or step was recorded")
	}
	if st, _ := attemptWork(t, &e, id, t0); st != WorkIncomplete {
		t.Fatalf("started attempt without a recorded start = %s, want incomplete", st)
	}
}

func TestAnEndReportedAfterCancellationClosesThePeriod(t *testing.T) {
	t.Parallel()
	e, id, fence := runningAttempt(t)
	e.MarkWorkStart(id, "r1", fence, "mock", t0, t0)
	if err := e.Cancel("stop", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if st, _ := attemptWork(t, &e, id, t0.Add(time.Minute)); st != WorkIncomplete {
		t.Fatalf("cancelled attempt without an end = %s, want incomplete", st)
	}
	if !e.MarkWorkEnd(id, "r1", fence, t0.Add(65*time.Second), t0) {
		t.Fatal("the runner's late end was not recorded")
	}
	if st, d := attemptWork(t, &e, id, t0.Add(time.Hour)); st != WorkComplete || d != 65*time.Second {
		t.Fatalf("work = %s %v, want complete 65s", st, d)
	}
}

func TestALostProcessLeavesTheTimeIncomplete(t *testing.T) {
	t.Parallel()
	e, id, fence := runningAttempt(t)
	e.MarkWorkStart(id, "r1", fence, "mock", t0, t0)
	e.ExpireLeases(t0.Add(2 * ttl))
	if st, _ := attemptWork(t, &e, id, t0.Add(2*ttl)); st != WorkRunning {
		t.Fatalf("unknown attempt = %s, want running until reconciled", st)
	}
	if err := e.Reconcile(id, ProcessGone, t0.Add(3*ttl), ttl, 2); err != nil {
		t.Fatal(err)
	}
	if st, d := attemptWork(t, &e, id, t0.Add(time.Hour)); st != WorkIncomplete || d != 0 {
		t.Fatalf("lost attempt = %s %v, want incomplete 0", st, d)
	}
}

func TestAnUnusableEventTimeMarksTheTimeIncomplete(t *testing.T) {
	t.Parallel()
	for name, at := range map[string]time.Time{
		"zero":    {},
		"ancient": time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC),
		"future":  t0.Add(48 * time.Hour),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, id, fence := runningAttempt(t)
			if !e.MarkWorkStart(id, "r1", fence, "mock", at, t0) {
				t.Fatal("the failed mark was not recorded")
			}
			e.MarkWorkEnd(id, "r1", fence, t0.Add(time.Second), t0)
			if st, _ := attemptWork(t, &e, id, t0); st != WorkIncomplete {
				t.Fatalf("work = %s, want incomplete", st)
			}
		})
	}
}

func TestAnEndBeforeTheStartIsIncomplete(t *testing.T) {
	t.Parallel()
	e, id, fence := runningAttempt(t)
	e.MarkWorkStart(id, "r1", fence, "mock", t0.Add(time.Minute), t0)
	e.MarkWorkEnd(id, "r1", fence, t0, t0)
	if st, d := attemptWork(t, &e, id, t0); st != WorkIncomplete || d != 0 {
		t.Fatalf("work = %s %v, want incomplete 0", st, d)
	}
}

func TestALaunchFailureHasNoAgentTime(t *testing.T) {
	t.Parallel()
	e := newExecution(t, 2)
	s := e.StepByKey("lint")
	l, _ := e.Claim(s.ID, "r1", t0, ttl)
	if err := e.Fail(s.ID, "r1", l.Fence, "assignment refused", t0, 2); err != nil {
		t.Fatal(err)
	}
	if e.MarkWorkEnd(s.ID, "r1", l.Fence, t0, t0) {
		t.Error("an end without a start was recorded")
	}
	if st, d := attemptWork(t, &e, s.ID, t0); st != WorkNone || d != 0 {
		t.Fatalf("work = %s %v, want none", st, d)
	}
}

func TestAgentTimeAddsUpAttemptsStepsAndExecutions(t *testing.T) {
	t.Parallel()
	e, id, fence := runningAttempt(t)
	e.MarkWorkStart(id, "r1", fence, "mock", t0, t0)
	if err := e.Fail(id, "r1", fence, "boom", t0.Add(time.Minute), 2); err != nil {
		t.Fatal(err)
	}
	e.MarkWorkEnd(id, "r1", fence, t0.Add(30*time.Second), t0)
	l, err := e.Claim(id, "r2", t0.Add(time.Minute), ttl)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(id, "r2", l.Fence, t0.Add(time.Minute), ttl); err != nil {
		t.Fatal(err)
	}
	e.MarkWorkStart(id, "r2", l.Fence, "mock", t0.Add(time.Minute), t0)
	now := t0.Add(80 * time.Second)
	if got := e.Step(id).AgentTime(now); got.Status != WorkRunning || got.Total != 50*time.Second {
		t.Fatalf("step time = %+v, want running 50s", got)
	}
	e.MarkWorkEnd(id, "r2", l.Fence, t0.Add(100*time.Second), t0)
	if err := e.Succeed(id, "r2", l.Fence, t0.Add(100*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := e.AgentTime(now); got.Status != WorkComplete || got.Total != 70*time.Second {
		t.Fatalf("execution time = %+v, want complete 70s", got)
	}
	var sum AgentTime
	sum.Add(WorkIncomplete, 0)
	sum.Add(WorkComplete, time.Second)
	if sum.Status != WorkIncomplete || sum.Total != time.Second {
		t.Fatalf("sum = %+v, an incomplete part makes the total incomplete", sum)
	}
}

func TestAgentNamesFromTheRunnerAreSanitized(t *testing.T) {
	t.Parallel()
	e, id, fence := runningAttempt(t)
	e.MarkWorkStart(id, "r1", fence, "<script>\x00", t0, t0)
	if a := e.Step(id).Attempts[0]; a.Agent != "" {
		t.Fatalf("agent = %q, want it dropped", a.Agent)
	}
}

func TestCloneCopiesWorkPeriods(t *testing.T) {
	t.Parallel()
	e, id, fence := runningAttempt(t)
	e.MarkWorkStart(id, "r1", fence, "mock", t0, t0)
	c := e.Clone()
	*c.Step(id).Attempts[0].WorkStart = t0.Add(time.Hour)
	if !e.Step(id).Attempts[0].WorkStart.Equal(t0) {
		t.Fatal("clone shares the work start")
	}
}

func TestAnImplausiblyLongPeriodIsIncomplete(t *testing.T) {
	t.Parallel()
	e, id, fence := runningAttempt(t)
	e.MarkWorkStart(id, "r1", fence, "mock", t0.Add(-MaxWorkPeriod-time.Hour), t0)
	e.MarkWorkEnd(id, "r1", fence, t0, t0)
	if st, d := attemptWork(t, &e, id, t0); st != WorkIncomplete || d != 0 {
		t.Fatalf("work = %s %v, want incomplete 0", st, d)
	}
}
