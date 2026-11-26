package domain

import (
	"fmt"
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

// KIN-106: the failed checks' output title and summary are what a CI fix round is told beyond the names, each
// bounded and sharing the budget; passing checks add nothing; the detail (audit, block reason, API) keeps the names.
func TestFailedOutputCarriesTheFailedChecksOutput(t *testing.T) {
	t.Parallel()
	runs := []CheckRun{
		{Name: "lint", Status: "completed", Conclusion: "success", Title: "all good", Summary: "nothing"},
		{Name: "test", Status: "completed", Conclusion: "failure", Title: "2 tests failed",
			Summary: "--- FAIL: TestLogin\n    login_test.go:12: want 200, got 500"},
		{Name: "build", Status: "completed", Conclusion: "failure", Title: strings.Repeat("t", 500),
			Summary: strings.Repeat("s", 5000)},
	}
	if st, detail := CombineChecks(runs, len(runs)); st != CIStateFailed || detail != "failed checks: test (failure), build (failure)" {
		t.Fatalf("%s %q", st, detail)
	}
	out := FailedOutput(runs)
	for _, want := range []string{"### test (failure): 2 tests failed", "--- FAIL: TestLogin", "login_test.go:12: want 200, got 500",
		"### build (failure): ttt"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "all good") || strings.Contains(out, strings.Repeat("t", maxCheckTitle+1)) ||
		strings.Contains(out, strings.Repeat("s", maxCheckSummary+1)) || len(out) > maxCIOutput {
		t.Fatalf("unbounded output (%d bytes)", len(out))
	}
	// Many failures share the budget: each one is still named in the output.
	var many []CheckRun
	for i := range 40 {
		many = append(many, CheckRun{Name: fmt.Sprintf("job-%02d", i), Status: "completed", Conclusion: "failure",
			Title: "failed", Summary: strings.Repeat("x", 900)})
	}
	if out := FailedOutput(many); len(out) > maxCIOutput || !strings.Contains(out, "### job-00") || !strings.Contains(out, "### job-15") {
		t.Fatalf("shared budget: %d bytes", len(out))
	}
	if FailedOutput(runs[:1]) != "" {
		t.Fatal("output of a passing check")
	}
}
