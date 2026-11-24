package domain

import (
	"errors"
	"strings"
	"testing"
)

func valid() Assignment {
	return Assignment{Step: "STEP1", Fence: 1, Execution: "EX1", Task: "T1", Project: "P1", Repository: "acme/app",
		Scope: []string{"src/", "docs/a.md"}, Adapter: AdapterMock, Scenario: "success"}
}

func TestCheckAssignment(t *testing.T) {
	t.Parallel()
	allow := Policy{Projects: []string{"P1"}}
	cases := []struct {
		name   string
		mutate func(a *Assignment)
		policy Policy
		want   error
	}{
		{"valid", func(*Assignment) {}, allow, nil},
		{"no allowlist accepts any project", func(a *Assignment) { a.Project = "P9" }, Policy{}, nil},
		{"foreign project", func(a *Assignment) { a.Project = "P2" }, allow, ErrProjectNotAllowed},
		{"unknown adapter", func(a *Assignment) { a.Adapter = "claude" }, allow, ErrUnknownAdapter},
		{"unknown scenario", func(a *Assignment) { a.Scenario = "rm -rf /" }, allow, ErrInvalidAssignment},
		{"empty scenario means success", func(a *Assignment) { a.Scenario = "" }, allow, nil},
		{"step id with path separator", func(a *Assignment) { a.Step = "../x" }, allow, ErrInvalidAssignment},
		{"no fence", func(a *Assignment) { a.Fence = 0 }, allow, ErrInvalidAssignment},
		{"no project", func(a *Assignment) { a.Project = "" }, allow, ErrInvalidAssignment},
		{"repository not owner/name", func(a *Assignment) { a.Repository = "app" }, allow, ErrInvalidAssignment},
		{"repository traversal", func(a *Assignment) { a.Repository = "../etc" }, allow, ErrInvalidAssignment},
		{"absolute scope", func(a *Assignment) { a.Scope = []string{"/etc/passwd"} }, allow, ErrInvalidAssignment},
		{"scope escapes", func(a *Assignment) { a.Scope = []string{"src/../../x"} }, allow, ErrInvalidAssignment},
		{"too many scope paths", func(a *Assignment) { a.Scope = make([]string, MaxScopePaths+1) }, allow, ErrInvalidAssignment},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			a := valid()
			c.mutate(&a)
			err := CheckAssignment(a, c.policy)
			if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("CheckAssignment = %v, want %v", err, c.want)
			}
		})
	}
}

func TestScenarioOf(t *testing.T) {
	t.Parallel()
	if s := valid().ScenarioOrDefault(); s != "success" {
		t.Fatal(s)
	}
	a := valid()
	a.Scenario = ""
	if s := a.ScenarioOrDefault(); s != ScenarioSuccess {
		t.Fatal(s)
	}
}

func TestOutcome(t *testing.T) {
	t.Parallel()
	cases := []struct {
		exit      Exit
		cancelled string
		ok        bool
		reason    string
	}{
		{Exit{Code: 0}, "", true, ""},
		{Exit{Code: 3}, "", false, "exit status 3"},
		{Exit{Signaled: true}, "", false, "killed by a signal"},
		{Exit{Lost: true}, "", false, "process lost"},
		{Exit{Signaled: true}, "stale_fence", false, "cancelled: stale_fence"},
		{Exit{Code: 0}, "cancelled", false, "cancelled: cancelled"},
	}
	for _, c := range cases {
		ok, reason := Outcome(c.exit, c.cancelled)
		if ok != c.ok || !strings.Contains(reason, c.reason) {
			t.Errorf("Outcome(%+v, %q) = %v %q", c.exit, c.cancelled, ok, reason)
		}
	}
}
