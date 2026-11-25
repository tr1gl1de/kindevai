package main

import (
	"testing"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
)

// KIN-79: an assignment for an AI CLI carries the CLI run; anything else runs the mock agent.
func TestAssignBodyCarriesTheCLIRun(t *testing.T) {
	t.Parallel()
	a := workflowapp.Assignment{StepID: "s1", StepKey: "api", Fence: 2, Agent: "claude", Model: "sonnet", Role: "implementer",
		Prompt: "# Task"}
	got := assignBody(a, []string{})
	if got.Adapter != protocol.AdapterClaude || got.Scenario != "" || got.Model != "sonnet" || got.Role != "implementer" ||
		got.Prompt != "# Task" {
		t.Fatalf("assign %+v", got)
	}
	for _, agent := range []string{"", "mock", "zcode"} {
		a.Agent = agent
		if got := assignBody(a, nil); got.Adapter != protocol.AdapterMock || got.Prompt != "" || got.Scenario != protocol.ScenarioSuccess {
			t.Fatalf("%q: assign %+v", agent, got)
		}
	}
}
