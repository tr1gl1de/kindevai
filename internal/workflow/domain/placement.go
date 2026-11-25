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
	// Disk is the highest alert level of the runner's volumes below critical (KIN-76): a fuller disk ranks lower.
	Disk DiskLevel
	// MaxParallel is the runner's parallel-step limit (KIN-76: declared by the runner); 0 = no limit.
	MaxParallel int
	// RecentFailures counts the runner's attempts that FAILED or were lost (UNKNOWN) within FailureWindow (KIN-76).
	RecentFailures int
}

// DiskLevel ranks a runner's disk for placement (KB KIN-A-26 levels; critical excludes the runner instead).
type DiskLevel int

const (
	DiskOK DiskLevel = iota
	DiskWarning
	DiskHigh
)

// FailureWindow and FailingAttempts decide when a runner is failing (KIN-76, KB KIN-A-14): FailingAttempts or more
// failed or lost attempts within the last FailureWindow. A failing runner stays eligible but ranks after the others.
const (
	FailureWindow   = time.Hour
	FailingAttempts = 2
)

// DetailRunnersAtLimit is the WAITING(runner) detail when only runners at their parallel-step limit could take a step.
const DetailRunnersAtLimit = "waiting for a runner: every runner that can take this step is at its parallel-step limit"

// Eligible reports whether the runner may take a step needing caps: it has every capability and disk headroom.
func (c RunnerCandidate) Eligible(caps []string) bool {
	return c.DiskHeavyAllowed && !slices.ContainsFunc(caps, func(x string) bool { return !slices.Contains(c.Tools, x) })
}

// Full reports whether the runner holds as many steps as its limit allows (held: its held steps).
func (c RunnerCandidate) Full(held int) bool { return c.MaxParallel > 0 && held >= c.MaxParallel }

// Failing reports whether the runner failed recently enough to rank after the others.
func (c RunnerCandidate) Failing() bool { return c.RecentFailures >= FailingAttempts }

// AtLimit reports whether an eligible runner for caps exists but each one of them is at its limit.
func AtLimit(cands []RunnerCandidate, caps []string, held map[string]int) bool {
	full := false
	for _, c := range cands {
		if !c.Eligible(caps) {
			continue
		}
		if !c.Full(held[c.ID]) {
			return false
		}
		full = true
	}
	return full
}

// PickRunner chooses the runner for a step needing caps (KIN-65, KIN-76): among the eligible candidates below their
// parallel-step limit, a runner that is not failing first, then the one whose disk is least full (ok, warning, high),
// then the one holding the fewest steps (held: CLAIMED, RUNNING or unknown attempts per runner id, including
// assignments made earlier in the same pass), ties broken by the smallest id. The choice is deterministic and does
// not depend on the candidates' order; an ineligible or full runner is never picked however idle it is.
func PickRunner(cands []RunnerCandidate, caps []string, held map[string]int) (RunnerCandidate, bool) {
	var best RunnerCandidate
	found := false
	for _, c := range cands {
		if !c.Eligible(caps) || c.Full(held[c.ID]) {
			continue
		}
		if !found || better(c, best, held) {
			best, found = c, true
		}
	}
	return best, found
}

// better orders two eligible runners for placement.
func better(a, b RunnerCandidate, held map[string]int) bool {
	switch {
	case a.Failing() != b.Failing():
		return !a.Failing()
	case a.Disk != b.Disk:
		return a.Disk < b.Disk
	case held[a.ID] != held[b.ID]:
		return held[a.ID] < held[b.ID]
	default:
		return strings.Compare(a.ID, b.ID) < 0
	}
}

// AgentChoice is an AI CLI with its model ("" = the CLI's default) that routing may run a step with (KIN-33).
type AgentChoice struct {
	Agent string
	Model string
}

// Cooling is a runner's CLI held after cli_limit (KIN-79, KIN-83): until when, or (NeedsOwner) until the Owner
// marks it ready again after logging it in or fixing its billing.
type Cooling struct {
	Until      time.Time
	NeedsOwner bool
}

// PickRoute chooses the CLI and runner for a step (KIN-33, KB KIN-A-15 routing: … → Role binding → Runner
// capabilities → Session availability): the first choice of the permitted chain for which an eligible runner
// (PickRunner: capabilities, disk, least load) has the CLI installed and is not held on it after a usage limit or a
// logout (cli_limit). When nothing fits, the reason says why: every runner with a CLI of the chain is held
// (cli_limit: the earliest reset, or the Owner's action when only that lifts them) or no runner has any of them
// (runner).
func PickRoute(cands []RunnerCandidate, caps []string, held map[string]int, chain []AgentChoice,
	cooling func(runnerID, agent string) (Cooling, bool)) (RunnerCandidate, AgentChoice, Reason, bool) {
	if cooling == nil {
		cooling = func(string, string) (Cooling, bool) { return Cooling{}, false }
	}
	var limited, owner []string
	var reset time.Time
	atLimit := false
	for _, c := range chain {
		var ok []RunnerCandidate
		for _, r := range cands {
			if !r.Eligible(append(slices.Clip(caps), c.Agent)) {
				continue
			}
			if h, cool := cooling(r.ID, c.Agent); cool {
				if !slices.Contains(limited, c.Agent) {
					limited = append(limited, c.Agent)
				}
				switch {
				case h.NeedsOwner:
					owner = append(owner, c.Agent+" on runner "+r.ID)
				case reset.IsZero() || h.Until.Before(reset):
					reset = h.Until
				}
				continue
			}
			ok = append(ok, r)
		}
		if r, found := PickRunner(ok, caps, held); found {
			return r, c, Reason{}, true
		}
		atLimit = atLimit || AtLimit(ok, caps, held)
	}
	if len(limited) > 0 && reset.IsZero() {
		return RunnerCandidate{}, AgentChoice{}, Reason{Code: WaitCLILimit, Detail: fmt.Sprintf(
			"waiting for the Owner: %s logged out or refused by billing; log the CLI in on the runner (or fix its billing), then mark it ready again on the Agents page",
			strings.Join(owner, ", "))}, false
	}
	if len(limited) > 0 {
		return RunnerCandidate{}, AgentChoice{}, Reason{Code: WaitCLILimit, Detail: fmt.Sprintf(
			"waiting for a CLI limit to reset: %s: usage limit on every runner that has it, earliest reset %s",
			strings.Join(limited, ", "), reset.UTC().Format("2006-01-02 15:04 UTC"))}, false
	}
	if atLimit {
		return RunnerCandidate{}, AgentChoice{}, Reason{Code: WaitRunner, Detail: DetailRunnersAtLimit}, false
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
