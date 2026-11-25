package domain

import (
	"errors"
	"fmt"
	"strings"
)

// CI on a pull request's head and its merge (KIN-37, KB KIN-A-20): the CI status of a commit combines all its
// latest check runs; a pull request is merged (merge commit) only while its head is the expected commit and its
// base is the repository's integration branch, never main/master.

var (
	// ErrHeadMoved: the pull request's head is not the commit that was checked (GitHub 409, or a later push).
	ErrHeadMoved = errors.New("the pull request's head moved")
	// ErrNotMergeable: GitHub refuses the merge (conflicts, branch protection, closed pull request).
	ErrNotMergeable = errors.New("the pull request cannot be merged")
)

// CI states of a commit.
const (
	CIStatePending = "pending"
	CIStatePassed  = "passed"
	CIStateFailed  = "failed"
)

// CheckRun is one check run on a commit as GitHub reports it.
type CheckRun struct {
	Name       string
	Status     string // queued | in_progress | completed | waiting | requested | pending
	Conclusion string // success | failure | neutral | cancelled | skipped | timed_out | action_required | stale | ...
}

const maxCIDetail = 2000

// CombineChecks is the CI state of a commit from its latest check runs: pending while there is none or one is not
// completed; passed when every one concluded success, neutral or skipped; failed otherwise, with the failed checks
// named in detail ("name (conclusion)", bounded).
func CombineChecks(runs []CheckRun) (state, detail string) {
	if len(runs) == 0 {
		return CIStatePending, ""
	}
	var failed []string
	pending := false
	for _, r := range runs {
		switch {
		case r.Status != "completed":
			pending = true
		case r.Conclusion == "success" || r.Conclusion == "neutral" || r.Conclusion == "skipped":
		default:
			failed = append(failed, fmt.Sprintf("%s (%s)", truncate(strings.TrimSpace(r.Name), 100), truncate(r.Conclusion, 30)))
		}
	}
	switch {
	case len(failed) > 0:
		return CIStateFailed, truncate("failed checks: "+strings.Join(failed, ", "), maxCIDetail)
	case pending:
		return CIStatePending, ""
	}
	return CIStatePassed, ""
}

// PullRequestState is a pull request as the merge needs it.
type PullRequestState struct {
	Number         int
	State          string // open | closed
	Merged         bool
	MergeCommitSHA string
	HeadRef        string
	HeadSHA        string
	Base           string
}
