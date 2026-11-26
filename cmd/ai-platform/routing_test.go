package main

import (
	"slices"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	workflowapp "github.com/tr1gl1de/kindevai/internal/workflow/app"
)

// KIN-33 / KIN-79 contract: execution.assign names the routed CLI in adapter, with model and role; mock plans (and
// assignments without routing) run the mock agent with the step's scenario.
func TestAssignBodyCarriesTheRoutedCLI(t *testing.T) {
	t.Parallel()
	a := workflowapp.Assignment{StepID: "s1", StepKey: "impl", Fence: 2, ExecutionID: "e1", TaskID: "t1",
		ProjectID: "p1", Repository: "o/r", Agent: "codex", Model: "gpt-5", Role: "implementer"}
	got := assignBody(a)
	if got.Adapter != "codex" || got.Model != "gpt-5" || got.Role != "implementer" || got.Scenario != "" ||
		got.Scope == nil {
		t.Fatalf("routed body %+v", got)
	}
	for _, agent := range []string{"", workflowapp.AgentMock} {
		a := workflowapp.Assignment{StepID: "s1", StepKey: protocol.ScenarioFlaky, Agent: agent, Model: "x", Role: "implementer"}
		got := assignBody(a)
		if got.Adapter != protocol.AdapterMock || got.Scenario != protocol.ScenarioFlaky || got.Model != "" {
			t.Fatalf("mock body for agent %q: %+v", agent, got)
		}
	}
	if !slices.Contains(protocol.MockScenarios, mockScenario("unknown")) {
		t.Fatal("unknown step key has no scenario")
	}
}
