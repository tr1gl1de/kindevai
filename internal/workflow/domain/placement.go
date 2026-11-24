package domain

import (
	"slices"
	"strings"
)

// RunnerCandidate is a runner the scheduler may give a project's steps to: the runners context already checked it is
// APPROVED, granted the project and online (connected, not UNREACHABLE).
type RunnerCandidate struct {
	ID    string
	Tools []string // capability names the runner reported (e.g. "git", "claude")
	// DiskHeavyAllowed is false while one of the runner's volumes is critical (KIN-20): no new step goes there,
	// because every step works in a git worktree on the runner's disk.
	DiskHeavyAllowed bool
}

// Eligible reports whether the runner may take a step needing caps: it has every capability and disk headroom.
func (c RunnerCandidate) Eligible(caps []string) bool {
	return c.DiskHeavyAllowed && !slices.ContainsFunc(caps, func(x string) bool { return !slices.Contains(c.Tools, x) })
}

// PickRunner chooses the runner for a step needing caps (KIN-65): among the eligible candidates the one holding the
// fewest steps (held: CLAIMED, RUNNING or unknown attempts per runner id, including assignments made earlier in the
// same pass), ties broken by the smallest id. The choice is deterministic and does not depend on the candidates'
// order; an ineligible runner is never picked however idle it is.
func PickRunner(cands []RunnerCandidate, caps []string, held map[string]int) (RunnerCandidate, bool) {
	var best RunnerCandidate
	found := false
	for _, c := range cands {
		if !c.Eligible(caps) {
			continue
		}
		if !found || held[c.ID] < held[best.ID] || held[c.ID] == held[best.ID] && strings.Compare(c.ID, best.ID) < 0 {
			best, found = c, true
		}
	}
	return best, found
}
