package domain

import (
	"strings"
	"testing"
	"time"
)

// KIN-33: routing picks, in chain order, the first CLI that an eligible runner has and is not cooling down on.
func TestPickRoute(t *testing.T) {
	t.Parallel()
	runner := func(id string, tools ...string) RunnerCandidate {
		return RunnerCandidate{ID: id, Tools: append([]string{"git"}, tools...), DiskHeavyAllowed: true}
	}
	until := t0.Add(30 * time.Minute)
	cool := func(pairs ...string) func(string, string) (time.Time, bool) {
		return func(r, a string) (time.Time, bool) {
			for i := 0; i < len(pairs); i += 2 {
				if pairs[i] == r && pairs[i+1] == a {
					return until, true
				}
			}
			return time.Time{}, false
		}
	}
	claude, codex := AgentChoice{Agent: "claude", Model: "opus"}, AgentChoice{Agent: "codex"}
	tests := []struct {
		name       string
		cands      []RunnerCandidate
		chain      []AgentChoice
		cooling    func(string, string) (time.Time, bool)
		held       map[string]int
		wantRunner string
		wantAgent  string
		wantCode   WaitReason
		wantDetail string
	}{
		{"primary on the least loaded runner with it", []RunnerCandidate{runner("r1", "claude"), runner("r2", "claude", "codex")},
			[]AgentChoice{claude, codex}, nil, map[string]int{"r1": 2}, "r2", "claude", "", ""},
		{"runner without the CLI is skipped", []RunnerCandidate{runner("r1", "codex"), runner("r2", "claude")},
			[]AgentChoice{claude}, nil, nil, "r2", "claude", "", ""},
		{"fallback when no runner has the primary", []RunnerCandidate{runner("r1", "codex")},
			[]AgentChoice{claude, codex}, nil, nil, "r1", "codex", "", ""},
		{"cooling runner is skipped for that CLI only", []RunnerCandidate{runner("r1", "claude", "codex")},
			[]AgentChoice{claude, codex}, cool("r1", "claude"), nil, "r1", "codex", "", ""},
		{"every runner with the CLI cools down", []RunnerCandidate{runner("r1", "claude"), runner("r2")},
			[]AgentChoice{claude}, cool("r1", "claude"), nil, "", "", WaitCLILimit, "claude: usage limit on every runner"},
		{"no runner has any CLI of the chain", []RunnerCandidate{runner("r1")},
			[]AgentChoice{claude, codex}, nil, nil, "", "", WaitRunner, "claude, codex"},
		{"no runners", nil, []AgentChoice{claude}, nil, nil, "", "", WaitRunner, "no approved, online runner"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r, c, reason, ok := PickRoute(tt.cands, nil, tt.held, tt.chain, tt.cooling)
			if ok != (tt.wantRunner != "") || r.ID != tt.wantRunner || c.Agent != tt.wantAgent {
				t.Fatalf("PickRoute = %s/%s ok=%v, want %s/%s", r.ID, c.Agent, ok, tt.wantRunner, tt.wantAgent)
			}
			if ok {
				return
			}
			if reason.Code != tt.wantCode || !strings.Contains(reason.Detail, tt.wantDetail) {
				t.Fatalf("reason %+v, want %s containing %q", reason, tt.wantCode, tt.wantDetail)
			}
		})
	}
}

func TestClaimRecordsTheAgent(t *testing.T) {
	t.Parallel()
	e := newExecution(t, 2)
	s := e.StepByKey("backend")
	if _, err := e.ClaimWith(s.ID, "r1", AgentChoice{Agent: "codex", Model: "gpt-5"}, t0, ttl); err != nil {
		t.Fatal(err)
	}
	s = e.StepByKey("backend")
	if s.Agent != "codex" || s.Model != "gpt-5" {
		t.Fatalf("claimed step agent %q model %q", s.Agent, s.Model)
	}
	if err := e.Start(s.ID, "r1", s.Fence, t0, ttl); err != nil {
		t.Fatal(err)
	}
	if a := e.StepByKey("backend").Attempts[0]; a.Agent != "codex" {
		t.Fatalf("attempt agent %q, want the assigned codex", a.Agent)
	}
}

// KIN-33: the Owner's emergency stop takes every attempt of a CLI away: claimed steps return to READY, running
// attempts are cancelled without using the retry budget and their steps wait for an agent (routed again).
func TestReleaseAgent(t *testing.T) {
	t.Parallel()
	e := newExecution(t, 2)
	b, l := e.StepByKey("backend"), e.StepByKey("lint")
	if _, err := e.ClaimWith(b.ID, "r1", AgentChoice{Agent: "claude"}, t0, ttl); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ClaimWith(l.ID, "r2", AgentChoice{Agent: "codex"}, t0, ttl); err != nil {
		t.Fatal(err)
	}
	b = e.StepByKey("backend")
	if err := e.Start(b.ID, "r1", b.Fence, t0, ttl); err != nil {
		t.Fatal(err)
	}
	got := e.ReleaseAgent("claude", "emergency stop", t0)
	if len(got) != 1 || got[0].StepID != b.ID || got[0].RunnerID != "r1" {
		t.Fatalf("released %+v", got)
	}
	b = e.StepByKey("backend")
	if b.State != StepWaiting || b.WaitReason != WaitAgent || b.RunnerID != "" || b.Attempts[0].State != AttemptCancelled || b.countedAttempts() != 0 {
		t.Fatalf("backend after release: %+v", b)
	}
	if e.StepByKey("lint").State != StepClaimed {
		t.Fatal("a step of another CLI was released")
	}
}

func TestWaitAgentIsAWaitReason(t *testing.T) {
	t.Parallel()
	if !WaitAgent.Valid() {
		t.Fatal("agent is not a valid wait reason")
	}
}
