package domain

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestStepPromptCarriesTheTaskPlanAndScope(t *testing.T) {
	t.Parallel()
	task := Task{Title: "Fix login", YouTrackIssue: "KIN-7"}
	plan := Plan{Summary: "Repair the session check"}
	step := PlanStep{Key: "api", Title: "Patch the handler", Repository: "acme/app", Scope: []string{"internal/auth/"}}
	p := StepPrompt(task, plan, step)
	for _, want := range []string{"Fix login", "KIN-7", "Repair the session check", "Patch the handler", "acme/app", "internal/auth/"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, p)
		}
	}
	plan.Summary = strings.Repeat("é", MaxPromptBytes)
	if p := StepPrompt(task, plan, step); len(p) > MaxPromptBytes || !utf8.ValidString(p) || !strings.Contains(p, "Patch the handler") {
		t.Fatalf("bounded prompt is %d bytes", len(p))
	}
}
