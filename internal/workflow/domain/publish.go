package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Publishing a step's reviewed work (KIN-36, KB KIN-A-20): after the review approved work that changed something,
// the step's publish phase runs on the runner that holds the work branch: it pushes the reviewed head to the step's
// own feature branch (feature/<task>--<step>, fast-forward only) with a short-lived installation token and
// reports the push. The step succeeds only on a report of exactly that branch and commit; then the Control Plane
// records the publication in the execution's change set and opens (or reuses) the pull request into the
// repository's integration branch through the outbox. main/master are never a push target or a pull request base.

// PhasePublish is the step phase that pushes the reviewed work branch.
const PhasePublish = "publish"

// StepRolePublisher is the role of a publish attempt; AgentPublish its agent (the runner's git push, no AI CLI).
const (
	StepRolePublisher = "publisher"
	AgentPublish      = "publish"
)

// WorkBranchPrefix starts every branch the platform pushes (git flow: feature/*).
const WorkBranchPrefix = "feature/"

// ErrProtectedBranch: main/master are Owner-only (KB KIN-A-20): never pushed to, never a pull request's base.
var ErrProtectedBranch = errors.New("protected branch: the platform never pushes to or opens pull requests into main/master")

// IsProtectedBranch reports main or master (any case, with or without refs/heads/).
func IsProtectedBranch(b string) bool {
	b = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(b), "refs/heads/"))
	return b == "main" || b == "master"
}

// WorkBranch is the branch of a step of the execution, as the runner names it: feature/<task>--<step>.
func (e *Execution) WorkBranch(stepID string) string {
	return WorkBranchPrefix + e.TaskID + "--" + stepID
}

// Push is what a publish attempt reported (workspace.pushed): Branch of Repository now points to HeadSHA.
type Push struct {
	Repository string
	Branch     string
	HeadSHA    string
}

// RecordPush stores a publish attempt's push report. A report naming main/master is ErrProtectedBranch and is not
// stored (the caller audits it and fails the attempt). An attempt the runner does not hold is ErrStaleFence, one
// that is no publish attempt ErrInvalidTransition.
func (e *Execution) RecordPush(stepID, runnerID string, fence int64, p Push) error {
	if IsProtectedBranch(p.Branch) {
		return fmt.Errorf("push to %q: %w", cutUTF8(p.Branch, 200), ErrProtectedBranch)
	}
	a := e.attemptFor(stepID, runnerID, fence)
	if a == nil {
		return fmt.Errorf("attempt %d of step %s on runner %s: %w", fence, stepID, runnerID, ErrStaleFence)
	}
	if a.Role != StepRolePublisher { // only a publish attempt pushes; another's head is its own report
		return fmt.Errorf("attempt %d of step %s is no publish attempt: %w", fence, stepID, ErrInvalidTransition)
	}
	a.PushedBranch, a.HeadSHA = cutUTF8(p.Branch, 200), cutUTF8(p.HeadSHA, 64)
	return nil
}

// Publication is a step's published work: its branch of Repository at HeadSHA, forked from BaseSHA.
type Publication struct {
	Repository string
	Branch     string
	BaseSHA    string
	HeadSHA    string
}

// Published returns the step's publication: its latest succeeded publish attempt.
func (s *Step) Published() (Publication, bool) {
	for i := len(s.Attempts) - 1; i >= s.AttemptBase; i-- {
		if a := s.Attempts[i]; a.Role == StepRolePublisher && a.State == AttemptSucceeded {
			base, _ := s.ReviewedWork()
			return Publication{Repository: s.Repository, Branch: a.PushedBranch, BaseSHA: base, HeadSHA: a.HeadSHA}, true
		}
	}
	return Publication{}, false
}

// PublishedHead is the head the step last pushed to its work branch (its latest succeeded publish attempt, of any
// round: a CI fix round or a runner loss resets the attempt window, not the remote branch) — "" before its first
// publish. A CI fix round's implementer continues from it (KIN-106), on whatever runner it gets.
func (s *Step) PublishedHead() string {
	for i := len(s.Attempts) - 1; i >= 0; i-- {
		if a := s.Attempts[i]; a.Role == StepRolePublisher && a.State == AttemptSucceeded && a.HeadSHA != "" {
			return a.HeadSHA
		}
	}
	return ""
}

// finishPublish decides the end of a publish attempt: it succeeds only on the push of the reviewed head to the
// step's own work branch.
func (e *Execution) finishPublish(s *Step, now time.Time, maxAttempts int) bool {
	a := s.lastAttempt()
	_, head := s.ReviewedWork()
	if want := e.WorkBranch(s.ID); a.PushedBranch != want || a.HeadSHA != head {
		e.failAttempt(s, fmt.Sprintf("the runner reported no push of the reviewed commit %s to %s", head, want), now, maxAttempts)
		return false
	}
	s.finishAttempt(AttemptSucceeded, "", now)
	return true
}

// ArtifactState is where a git artifact's pull request stands.
type ArtifactState string

// Git artifact states.
const (
	ArtifactOpening ArtifactState = "opening" // pushed; the pull request is being opened (outbox)
	ArtifactOpen    ArtifactState = "open"    // the pull request is open (created or reused)
	ArtifactFailed  ArtifactState = "failed"  // the pull request could not be opened; Error says why
)

// GitArtifact (ChangeSet entry) is one published branch of an execution: Repository's Branch at HeadSHA and its
// pull request into the repository's integration branch. An execution's change set holds one per repository and
// branch.
type GitArtifact struct {
	ExecutionID string
	TaskID      string
	StepID      string
	Repository  string
	Branch      string
	BaseSHA     string
	HeadSHA     string
	State       ArtifactState
	PRNumber    int
	PRURL       string
	PRBase      string
	Error       string
	// CI on HeadSHA and the merge into PRBase (KIN-37, ci.go): CIHeadSHA is the head CI was last reported for,
	// CIDetail the failed checks; MergeSHA the merge commit, MergeError why the gate holds or refused it.
	CI        CIState
	CIHeadSHA string
	CIDetail  string
	// NoChecksSince is when CI first found no check run at all on CIHeadSHA (a repository without CI, KIN-105);
	// zero once a check run appeared, the head moved or the Owner resumed the task.
	NoChecksSince time.Time
	// CIWaivedHead is the head the Owner chose to merge without CI (KIN-113): it counts only while that head still
	// has no check run at all.
	CIWaivedHead string
	Merge        MergeState
	MergeSHA     string
	MergeError   string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// NewGitArtifact records step s's publication in e's change set, its pull request still to be opened.
func NewGitArtifact(e *Execution, s *Step, p Publication, now time.Time) GitArtifact {
	return GitArtifact{ExecutionID: e.ID, TaskID: e.TaskID, StepID: s.ID, Repository: p.Repository, Branch: p.Branch,
		BaseSHA: p.BaseSHA, HeadSHA: p.HeadSHA, State: ArtifactOpening, CreatedAt: now, UpdatedAt: now}
}

// Opened records the open pull request (created or reused) into base. A base of main/master is refused.
func (a *GitArtifact) Opened(number int, url, base string, now time.Time) error {
	if IsProtectedBranch(base) {
		return fmt.Errorf("pull request into %q: %w", base, ErrProtectedBranch)
	}
	a.State, a.PRNumber, a.PRURL, a.PRBase, a.Error, a.UpdatedAt = ArtifactOpen, number, url, base, "", now
	return nil
}

// Failed records why the pull request could not be opened.
func (a *GitArtifact) Failed(reason string, now time.Time) {
	a.State, a.Error, a.UpdatedAt = ArtifactFailed, reasonText(reason), now
}
