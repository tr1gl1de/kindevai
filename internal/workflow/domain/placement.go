package domain

import (
	"fmt"
	"slices"
	"strings"
	"time"
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

// AgentChoice is an AI CLI with its model ("" = the CLI's default) that routing may run a step with (KIN-33).
type AgentChoice struct {
	Agent string
	Model string
}

// PickRoute chooses the CLI and runner for a step (KIN-33, KB KIN-A-15 routing: … → Role binding → Runner
// capabilities → Session availability): the first choice of the permitted chain for which an eligible runner
// (PickRunner: capabilities, disk, least load) has the CLI installed and is not cooling down on it after a usage
// limit (cli_limit). When nothing fits, the reason says why: every runner with a CLI of the chain cools down
// (cli_limit, with the earliest reset) or no runner has any of them (runner).
func PickRoute(cands []RunnerCandidate, caps []string, held map[string]int, chain []AgentChoice,
	cooling func(runnerID, agent string) (time.Time, bool)) (RunnerCandidate, AgentChoice, Reason, bool) {
	if cooling == nil {
		cooling = func(string, string) (time.Time, bool) { return time.Time{}, false }
	}
	var limited []string
	var reset time.Time
	for _, c := range chain {
		var ok []RunnerCandidate
		for _, r := range cands {
			if !r.Eligible(append(slices.Clip(caps), c.Agent)) {
				continue
			}
			if until, cool := cooling(r.ID, c.Agent); cool {
				if !slices.Contains(limited, c.Agent) {
					limited = append(limited, c.Agent)
				}
				if reset.IsZero() || until.Before(reset) {
					reset = until
				}
				continue
			}
			ok = append(ok, r)
		}
		if r, found := PickRunner(ok, caps, held); found {
			return r, c, Reason{}, true
		}
	}
	if len(limited) > 0 {
		return RunnerCandidate{}, AgentChoice{}, Reason{Code: WaitCLILimit, Detail: fmt.Sprintf(
			"waiting for a CLI limit to reset: %s: usage limit on every runner that has it, earliest reset %s",
			strings.Join(limited, ", "), reset.UTC().Format("2006-01-02 15:04 UTC"))}, false
	}
	agents := make([]string, 0, len(chain))
	for _, c := range chain {
		if !slices.Contains(agents, c.Agent) {
			agents = append(agents, c.Agent)
		}
	}
	return RunnerCandidate{}, AgentChoice{}, Reason{Code: WaitRunner, Detail: fmt.Sprintf(
		"waiting for a runner: no approved, online runner of this project has a permitted CLI (%s) with the step's capabilities and disk space",
		strings.Join(agents, ", "))}, false
}
