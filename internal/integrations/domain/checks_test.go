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
		"none":    {nil, CIStateNone, ""},
		"running": {[]CheckRun{done("lint", "success"), {Name: "test", Status: "in_progress"}}, CIStatePending, ""},
		"green":   {[]CheckRun{done("lint", "success"), done("docs", "skipped"), done("x", "neutral")}, CIStatePassed, ""},
		"red":     {[]CheckRun{done("lint", "success"), done("test", "failure"), {Name: "e2e", Status: "queued"}}, CIStateFailed, "test (failure)"},
		"timeout": {[]CheckRun{done("build", "timed_out"), done("vet", "cancelled")}, CIStateFailed, "build (timed_out), vet (cancelled)"},
	} {
		state, detail := CombineChecks(tc.runs, len(tc.runs))
		if state != tc.state || !strings.Contains(detail, tc.detail) {
			t.Errorf("%s: %s %q", name, state, detail)
		}
	}
	// KIN-105: no check run read while GitHub reports some is pending, not "no CI".
	if st, _ := CombineChecks(nil, 3); st != CIStatePending {
		t.Errorf("unread runs: %s", st)
	}
	// Review: a partial page of green runs is never green; a failure on it still fails.
	green := []CheckRun{done("lint", "success")}
	if st, _ := CombineChecks(green, 150); st != CIStatePending {
		t.Errorf("partial green: %s", st)
	}
	if st, _ := CombineChecks([]CheckRun{done("lint", "failure")}, 150); st != CIStateFailed {
		t.Errorf("partial red: %s", st)
	}
}
