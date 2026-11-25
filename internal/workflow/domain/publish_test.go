package domain

import (
	"errors"
	"strings"
	"testing"
)

// approved runs the step's implementation (base1..head1) and an approving review on r1.
func approved(t *testing.T) (Execution, string) {
	t.Helper()
	e, id := reviewExecution(t)
	implement(t, &e, id)
	reviewWith(t, &e, id, `{"verdict":"approve","summary":"fine","findings":[]}`)
	return e, id
}

func publishClaim() StepClaim { return StepClaim{Agent: AgentChoice{Agent: AgentPublish}} }

// KIN-36: an approved step with work publishes it on the runner that holds the branch before it succeeds; the
// publish attempt must report the push of the reviewed head to the step's own feature branch.
func TestApprovedWorkIsPublishedBeforeTheStepSucceeds(t *testing.T) {
	t.Parallel()
	e, id := approved(t)
	s := e.Step(id)
	if s.State != StepReady || s.Phase != PhasePublish || s.PinnedRunner != "r1" || e.State != ExecutionRunning {
		t.Fatalf("after approval: state %s phase %q pinned %q execution %s", s.State, s.Phase, s.PinnedRunner, e.State)
	}
	if b := e.WorkBranch(id); b != "feature/t1--s1" {
		t.Fatalf("work branch %q", b)
	}
	f := runPhase(t, &e, id, "r1", publishClaim())
	if a := e.Step(id).Attempts[2]; a.Role != StepRolePublisher {
		t.Fatalf("publish attempt role %q", a.Role)
	}
	if err := e.RecordPush(id, "r1", f, Push{Repository: "api", Branch: "feature/t1--s1", HeadSHA: "head1"}); err != nil {
		t.Fatal(err)
	}
	if err := e.Succeed(id, "r1", f, t0); err != nil {
		t.Fatal(err)
	}
	s = e.Step(id)
	if s.State != StepSucceeded || e.State != ExecutionSucceeded {
		t.Fatalf("after publish: step %s execution %s", s.State, e.State)
	}
	p, ok := s.Published()
	if !ok || p.Branch != "feature/t1--s1" || p.HeadSHA != "head1" || p.BaseSHA != "base1" || p.Repository != "api" {
		t.Fatalf("published %+v %v", p, ok)
	}
}

// KIN-36: a publish attempt that pushed nothing, or another commit or branch than the reviewed one, fails (and is
// retried within the budget); the step never succeeds on it.
func TestAPublishWithoutTheReviewedPushFails(t *testing.T) {
	t.Parallel()
	for name, push := range map[string]*Push{
		"no push":      nil,
		"other head":   {Repository: "api", Branch: "feature/t1--s1", HeadSHA: "head2"},
		"other branch": {Repository: "api", Branch: "feature/t1--s9", HeadSHA: "head1"},
	} {
		e, id := approved(t)
		f := runPhase(t, &e, id, "r1", publishClaim())
		if push != nil {
			if err := e.RecordPush(id, "r1", f, *push); err != nil {
				t.Fatal(err)
			}
		}
		if err := e.Succeed(id, "r1", f, t0); err != nil {
			t.Fatal(err)
		}
		s := e.Step(id)
		if a := s.Attempts[2]; s.State == StepSucceeded || a.State != AttemptFailed || !strings.Contains(a.Error, "push") {
			t.Errorf("%s: step %s attempt %+v", name, s.State, a)
		}
		if _, ok := s.Published(); ok {
			t.Errorf("%s: published", name)
		}
	}
}

// KIN-36: a push report naming main/master is refused in the application path and not stored.
func TestAPushToAProtectedBranchIsRefused(t *testing.T) {
	t.Parallel()
	for _, b := range []string{"main", "master", "refs/heads/main", "Master"} {
		e, id := approved(t)
		f := runPhase(t, &e, id, "r1", publishClaim())
		if err := e.RecordPush(id, "r1", f, Push{Repository: "api", Branch: b, HeadSHA: "head1"}); !errors.Is(err, ErrProtectedBranch) {
			t.Errorf("%s: %v", b, err)
		}
		if a := e.Step(id).Attempts[2]; a.PushedBranch != "" {
			t.Errorf("%s stored", b)
		}
	}
}

// An approval of a step that changed nothing has nothing to publish: the step succeeds at once.
func TestApprovedStepWithoutChangesSucceedsWithoutPublishing(t *testing.T) {
	t.Parallel()
	e, id := reviewExecution(t)
	f := runPhase(t, &e, id, "r1", implClaim)
	passTests(t, &e, id, "r1", f, 0)
	e.RecordChanges(id, "r1", f, WorkChanges{BaseSHA: "base1", HeadSHA: "base1"})
	if err := e.Succeed(id, "r1", f, t0); err != nil {
		t.Fatal(err)
	}
	reviewWith(t, &e, id, `{"verdict":"approve","summary":"nothing to do","findings":[]}`)
	if s := e.Step(id); s.State != StepSucceeded || e.State != ExecutionSucceeded {
		t.Fatalf("step %s execution %s", s.State, e.State)
	}
}
