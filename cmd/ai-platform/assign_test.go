package main

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
)

// KIN-79: an assignment for an AI CLI carries the CLI run; anything else runs the mock agent.
func TestAssignBodyCarriesTheCLIRun(t *testing.T) {
	t.Parallel()
	a := workflowapp.Assignment{StepID: "s1", StepKey: "api", Fence: 2, Agent: "claude", Model: "sonnet", Role: "implementer",
		Prompt: "# Task"}
	got := assignBody(a)
	if got.Adapter != protocol.AdapterClaude || got.Scenario != "" || got.Model != "sonnet" || got.Role != "implementer" ||
		got.Prompt != "# Task" {
		t.Fatalf("assign %+v", got)
	}
	for _, agent := range []string{"", "mock"} {
		a.Agent = agent
		if got := assignBody(a); got.Adapter != protocol.AdapterMock || got.Prompt != "" || got.Scenario != protocol.ScenarioSuccess {
			t.Fatalf("%q: assign %+v", agent, got)
		}
	}
}

func TestAgentSummaryIsOneBoundedLine(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		ev   protocol.AgentEvent
		want string
	}{
		{protocol.AgentEvent{Kind: "session", Session: "s1", Model: "sonnet", Version: "2.1.0"}, "session · s1 · sonnet · v2.1.0"},
		{protocol.AgentEvent{Kind: "tool_use", Tool: "Edit", Input: "{\"a\":\n1}"}, "tool_use · Edit · {\"a\": 1}"},
		{protocol.AgentEvent{Kind: "failure", Code: "cli_limit", Cause: "rate_limit", Message: "limit"}, "failure · cli_limit · rate_limit · limit"},
		{protocol.AgentEvent{Kind: "result", Text: "done", Usage: &protocol.AgentUsage{InputTokens: 5, OutputTokens: 2, Turns: 1, CostUSD: 0.5}},
			"result · done · 5 in / 2 out tokens, 1 turns, $0.5000"},
	} {
		if got := agentSummary(c.ev); got != c.want {
			t.Errorf("%q, want %q", got, c.want)
		}
	}
	long := agentSummary(protocol.AgentEvent{Kind: "text", Text: strings.Repeat("é", 1000)})
	if len(long) > maxAgentSummary+len("…") || !utf8.ValidString(long) {
		t.Fatalf("summary of %d bytes", len(long))
	}
}

// KIN-79: only a CLI's limit cools a runner's CLI down; the mock agent has none.
func TestCLICooldownsIgnoreTheMockAgent(t *testing.T) {
	t.Parallel()
	if err := (cliCooldowns{}).CLILimitHit(t.Context(), "r1", "mock", time.Now().Add(time.Hour), "x"); err != nil {
		t.Fatal(err)
	}
}
