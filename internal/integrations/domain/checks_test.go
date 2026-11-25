package domain

import (
	"strings"
	"testing"
)

// KIN-37: a commit's CI combines all its check runs: none or one still running is pending, any failed, timed out,
// cancelled or action_required one fails it (named in the detail), and success/neutral/skipped pass.
func TestCombineChecks(t *testing.T) {
	t.Parallel()
	done := func(name, c string) CheckRun { return CheckRun{Name: name, Status: "completed", Conclusion: c} }
	for name, tc := range map[string]struct {
		runs   []CheckRun
		state  string
		detail string
	}{
		"none":    {nil, CIStatePending, ""},
		"running": {[]CheckRun{done("lint", "success"), {Name: "test", Status: "in_progress"}}, CIStatePending, ""},
		"green":   {[]CheckRun{done("lint", "success"), done("docs", "skipped"), done("x", "neutral")}, CIStatePassed, ""},
		"red":     {[]CheckRun{done("lint", "success"), done("test", "failure"), {Name: "e2e", Status: "queued"}}, CIStateFailed, "test (failure)"},
		"timeout": {[]CheckRun{done("build", "timed_out"), done("vet", "cancelled")}, CIStateFailed, "build (timed_out), vet (cancelled)"},
	} {
		state, detail := CombineChecks(tc.runs)
		if state != tc.state || !strings.Contains(detail, tc.detail) {
			t.Errorf("%s: %s %q", name, state, detail)
		}
	}
}
