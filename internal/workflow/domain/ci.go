package domain

import (
	"errors"
	"fmt"
	"time"
)

// The CI fix loop and the policy-gated merge into the integration branch (KIN-37, KB KIN-A-20). After a step's
// reviewed work is published and its pull request is open (KIN-36) the task is VERIFYING and the Control Plane
// follows CI on the pull request's head:
//
//	CI passed → the merge gate: integrate_dev allowed (policy for L0/L1, the Owner's approval for L2/L3) and every
//	            earlier artifact of the change set merged → merge commit with the expected head; all merged → COMPLETED
//	CI failed → a CI fix round: the execution reopens, the step implements again on the runner that holds its branch
//	            (told why CI failed), is reviewed and pushed again as a new head (fast-forward), and CI runs again;
//	            after MaxCIFixRounds the task is BLOCKED (reason ci) for the Owner.
//
// main/master are never a merge base (ErrProtectedBranch, checked here and in the GitHub transport).

// MaxCIFixRounds bounds the CI fix rounds of a step's published branch (owner decision in KIN-37: 3). They are
// counted apart from the attempt budget and the review's fix rounds.
const MaxCIFixRounds = 3

// maxCIFeedback bounds the CI failure text kept for the fix round (runes).
const maxCIFeedback = 4000

// ErrCIBudgetExhausted: CI failed again after the last CI fix round.
var ErrCIBudgetExhausted = errors.New("CI still fails after the last CI fix round")

// ReopenForCI starts a CI fix round of a published step of a succeeded execution: the execution runs again and the
// step implements again from its work branch on the runner that holds it (PinnedRunner stays), with fresh attempt
// and review budgets and the CI failure as feedback. Its next publish pushes the new head to the same branch.
func (e *Execution) ReopenForCI(stepID, feedback string, now time.Time) error {
	s := e.Step(stepID)
	if s == nil {
		return fmt.Errorf("step %s: %w", stepID, ErrNotFound)
	}
	if e.State != ExecutionSucceeded || s.State != StepSucceeded || !s.Review {
		return fmt.Errorf("execution %s is %s, step %s: CI fix round: %w", e.ID, e.State, s.State, ErrInvalidTransition)
	}
	if _, ok := s.Published(); !ok {
		return fmt.Errorf("step %s published nothing: CI fix round: %w", s.Key, ErrInvalidTransition)
	}
	if s.CIRound >= MaxCIFixRounds {
		return fmt.Errorf("step %s after %d CI fix rounds: %w", s.Key, s.CIRound, ErrCIBudgetExhausted)
	}
	s.CIRound++
	s.CIFeedback = cutUTF8(feedback, maxCIFeedback)
	s.Phase, s.Round, s.AttemptBase, s.RunnerID, s.Reason = PhaseImplement, 0, len(s.Attempts), "", ""
	s.move(StepReady)
	e.State, e.Reason, e.FinishedAt = ExecutionRunning, "", nil
	return nil
}

// ciFeedback is what the implementer of a CI fix round is told ("" outside one).
func (s *Step) ciFeedback() string {
	if s.Phase != PhaseImplement || s.Round != 0 || s.CIRound == 0 || s.CIFeedback == "" {
		return ""
	}
	return fmt.Sprintf("CI failed on the published branch (CI fix round %d of %d). Fix the cause so the checks pass:\n%s\n",
		s.CIRound, MaxCIFixRounds, s.CIFeedback)
}

// CIState is where CI stands on an artifact's head.
type CIState string

// CI states ("" before the first result).
const (
	CIPending CIState = "pending" // checks are queued or running
	CIPassed  CIState = "passed"  // every check on the head concluded success, neutral or skipped
	CIFailed  CIState = "failed"  // a check on the head failed, timed out, was cancelled or needs action
)

// MergeState is where an artifact's merge into the integration branch stands ("" before the merge gate ran).
type MergeState string

// Merge states.
const (
	MergeWaiting MergeState = "waiting" // CI passed; the gate holds it (integrate_dev not approved, an earlier repo)
	MergeMerged  MergeState = "merged"
	MergeFailed  MergeState = "failed" // the merge was refused (head moved, not mergeable, protected base)
)

// NoCIBlockAfter is how long a head may have no check run at all before its task is BLOCKED (reason ci) instead of
// waiting forever: a repository without CI is never merged automatically (KIN-105).
const NoCIBlockAfter = 30 * time.Minute

// RecordCI stores the CI result of head; a result of another head than the artifact's current one, or of an
// artifact whose pull request is not open, is ignored. It reports whether the artifact changed.
func (a *GitArtifact) RecordCI(head string, st CIState, detail string, now time.Time) bool {
	return a.recordCI(head, st, detail, false, now)
}

// RecordNoChecks stores that head has no check run at all (pending): the no-CI window starts with the first such
// result of the head and keeps running while none appears.
func (a *GitArtifact) RecordNoChecks(head string, now time.Time) bool {
	return a.recordCI(head, CIPending, "", true, now)
}

func (a *GitArtifact) recordCI(head string, st CIState, detail string, noChecks bool, now time.Time) bool {
	if a.State != ArtifactOpen || head == "" || head != a.HeadSHA || a.Merge == MergeMerged {
		return false
	}
	detail = cutUTF8(detail, maxCIFeedback)
	var since time.Time
	if noChecks {
		if since = a.NoChecksSince; since.IsZero() || a.CIHeadSHA != head {
			since = now
		}
	}
	if a.CI == st && a.CIHeadSHA == head && a.CIDetail == detail && a.NoChecksSince.Equal(since) {
		return false
	}
	a.CI, a.CIHeadSHA, a.CIDetail, a.NoChecksSince, a.UpdatedAt = st, head, detail, since, now
	return true
}

// NoChecksFor is how long the current head has had no check run at all (0: it has some, or no result yet).
func (a *GitArtifact) NoChecksFor(now time.Time) time.Duration {
	if a.NoChecksSince.IsZero() || a.CI != CIPending || a.CIHeadSHA != a.HeadSHA || a.HeadSHA == "" {
		return 0
	}
	return now.Sub(a.NoChecksSince)
}

// RestartNoChecksWindow forgets when the no-CI window started (the Owner resumed the task): the next result
// without check runs starts a new one. It reports whether the artifact changed.
func (a *GitArtifact) RestartNoChecksWindow() bool {
	if a.NoChecksSince.IsZero() {
		return false
	}
	a.NoChecksSince = time.Time{}
	return true
}

// RetryMerge clears a refused merge so the merge gate runs again (the Owner's "Retry merge"); it reports whether
// there was one.
func (a *GitArtifact) RetryMerge(now time.Time) bool {
	if a.Merge != MergeFailed {
		return false
	}
	a.Merge, a.MergeError, a.UpdatedAt = "", "", now
	return true
}

// CIPassedOnHead reports CI passed on the artifact's current head.
func (a *GitArtifact) CIPassedOnHead() bool {
	return a.State == ArtifactOpen && a.CI == CIPassed && a.CIHeadSHA == a.HeadSHA && a.HeadSHA != ""
}

// CheckMergeable says why the artifact may not be merged now ("" error: it may): its pull request must be open
// into a base other than main/master, CI must have passed on its head, and it must not be merged already.
func (a *GitArtifact) CheckMergeable() error {
	switch {
	case IsProtectedBranch(a.PRBase):
		return fmt.Errorf("merge into %q: %w", a.PRBase, ErrProtectedBranch)
	case a.Merge == MergeMerged:
		return fmt.Errorf("pull request #%d is merged already: %w", a.PRNumber, ErrInvalidTransition)
	case a.State != ArtifactOpen || a.PRNumber <= 0 || a.PRBase == "":
		return fmt.Errorf("no open pull request for %s: %w", a.Branch, ErrInvalidTransition)
	case !a.CIPassedOnHead():
		return fmt.Errorf("CI has not passed on %s: %w", a.HeadSHA, ErrInvalidTransition)
	}
	return nil
}

// MergeHeld records that the gate holds the merge (why: the reason shown).
func (a *GitArtifact) MergeHeld(why string, now time.Time) bool {
	why = reasonText(why)
	if a.Merge == MergeWaiting && a.MergeError == why {
		return false
	}
	a.Merge, a.MergeError, a.UpdatedAt = MergeWaiting, why, now
	return true
}

// Merged records the merge commit sha.
func (a *GitArtifact) Merged(sha string, now time.Time) {
	a.Merge, a.MergeSHA, a.MergeError, a.UpdatedAt = MergeMerged, cutUTF8(sha, 64), "", now
}

// MergeFailed records why the merge was refused.
func (a *GitArtifact) MergeFailed(reason string, now time.Time) {
	a.Merge, a.MergeError, a.UpdatedAt = MergeFailed, reasonText(reason), now
}

// NextMerge is the artifact of an execution's change set (in order) to merge next: the first one not merged yet,
// when it is mergeable. A failed merge stops the rest (stop says which); an earlier artifact not ready yet (CI
// pending, pull request opening) holds the later ones (nil, "").
func NextMerge(arts []GitArtifact) (next *GitArtifact, stop string) {
	for i := range arts {
		a := &arts[i]
		switch {
		case a.Merge == MergeMerged:
			continue
		case a.Merge == MergeFailed:
			return nil, fmt.Sprintf("the merge of %s (%s) failed: %s", a.Repository, a.Branch, a.MergeError)
		case a.State == ArtifactFailed:
			return nil, fmt.Sprintf("the pull request of %s (%s) could not be opened: %s", a.Repository, a.Branch, a.Error)
		case a.CheckMergeable() != nil:
			return nil, ""
		}
		return a, ""
	}
	return nil, ""
}

// AllMerged reports a non-empty change set whose every artifact is merged.
func AllMerged(arts []GitArtifact) bool {
	for _, a := range arts {
		if a.Merge != MergeMerged {
			return false
		}
	}
	return len(arts) > 0
}
