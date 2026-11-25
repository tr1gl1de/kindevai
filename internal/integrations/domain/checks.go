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
	// CIStateNone: the commit has no check run at all (a repository without CI, KIN-105); still not green.
	CIStateNone = "none"
)

// CheckRun is one check run on a commit as GitHub reports it; Title and Summary are its output (KIN-106,
// untrusted text written by the CI).
type CheckRun struct {
	Name       string
	Status     string // queued | in_progress | completed | waiting | requested | pending
	Conclusion string // success | failure | neutral | cancelled | skipped | timed_out | action_required | stale | ...
	Title      string
	Summary    string
}

// maxCIDetail bounds the CI detail (the failed checks' names, bytes); maxCIOutput the failed checks' output told to a
// CI fix round, maxCheckTitle and maxCheckSummary one check's share of it at most (KIN-106).
const (
	maxCIDetail     = 2000
	maxCIOutput     = 3000
	maxCheckTitle   = 200
	maxCheckSummary = 1000
)

// CombineChecks is the CI state of a commit from its latest check runs (total: how many GitHub has, maybe more than
// were read): none when the commit has no check run at all; pending while none was read, one is not completed or
// some were not read (never green on a partial list); passed when every one concluded success, neutral or skipped;
// failed otherwise, with the failed checks named in detail ("name (conclusion)", bounded).
func CombineChecks(runs []CheckRun, total int) (state, detail string) {
	if len(runs) == 0 {
		if total == 0 {
			return CIStateNone, ""
		}
		return CIStatePending, ""
	}
	var failed []string
	pending := false
	for _, r := range runs {
		switch {
		case r.Status != "completed":
			pending = true
		case passed(r):
		default:
			failed = append(failed, checkName(r))
		}
	}
	switch {
	case len(failed) > 0:
		return CIStateFailed, truncate("failed checks: "+strings.Join(failed, ", "), maxCIDetail)
	case pending || total > len(runs):
		return CIStatePending, ""
	}
	return CIStatePassed, ""
}

// FailedOutput is what a CI fix round is told about the failed checks beyond their names (KIN-106): each one's
// output title and summary (untrusted text the CI wrote), the budget shared among them, all bounded. Only the
// implementer gets it: audit entries, block reasons and the API keep the names (a summary may echo log lines).
func FailedOutput(runs []CheckRun) string {
	var failed []CheckRun
	for _, r := range runs {
		if r.Status == "completed" && !passed(r) && (strings.TrimSpace(r.Title) != "" || strings.TrimSpace(r.Summary) != "") {
			failed = append(failed, r)
		}
	}
	if len(failed) == 0 {
		return ""
	}
	share := max(maxCIOutput/len(failed), 160)
	var b strings.Builder
	for _, r := range failed {
		part := "### " + checkName(r)
		if title := truncate(strings.TrimSpace(r.Title), maxCheckTitle); title != "" {
			part += ": " + title
		}
		if summary := truncate(strings.TrimSpace(r.Summary), maxCheckSummary); summary != "" {
			part += "\n" + summary
		}
		b.WriteString(truncate(part, share) + "\n\n")
	}
	return truncate(strings.TrimSpace(b.String()), maxCIOutput)
}

func passed(r CheckRun) bool {
	return r.Conclusion == "success" || r.Conclusion == "neutral" || r.Conclusion == "skipped"
}

func checkName(r CheckRun) string {
	return fmt.Sprintf("%s (%s)", truncate(strings.TrimSpace(r.Name), 100), truncate(r.Conclusion, 30))
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
