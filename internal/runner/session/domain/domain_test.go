package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
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
		{"unknown adapter", func(a *Assignment) { a.Adapter = "zcode" }, allow, ErrUnknownAdapter},
		{"claude with a prompt", cli(AgentClaude, nil), allow, nil},
		{"codex with model and resume", cli(AgentCodex, func(a *Assignment) { a.Model, a.Resume = "gpt-5-codex", "0199a-b2" }), allow, nil},
		{"cli without a prompt", cli(AgentClaude, func(a *Assignment) { a.Prompt = "" }), allow, ErrInvalidAssignment},
		{"cli prompt too large", cli(AgentClaude, func(a *Assignment) { a.Prompt = strings.Repeat("x", MaxPrompt+1) }), allow, ErrInvalidAssignment},
		{"cli with a scenario", cli(AgentClaude, func(a *Assignment) { a.Scenario = "success" }), allow, ErrInvalidAssignment},
		{"model with shell characters", cli(AgentClaude, func(a *Assignment) { a.Model = "x; rm -rf /" }), allow, ErrInvalidAssignment},
		{"model that looks like a flag", cli(AgentClaude, func(a *Assignment) { a.Model = "--dangerously-skip-permissions" }), allow, ErrInvalidAssignment},
		{"resume that looks like a flag", cli(AgentClaude, func(a *Assignment) { a.Resume = "-x" }), allow, ErrInvalidAssignment},
		{"timeout over the maximum", cli(AgentClaude, func(a *Assignment) { a.Timeout = MaxAgentTimeout + 1 }), allow, ErrInvalidAssignment},
		{"negative timeout", cli(AgentClaude, func(a *Assignment) { a.Timeout = -1 }), allow, ErrInvalidAssignment},
		{"bad role", cli(AgentClaude, func(a *Assignment) { a.Role = "a b" }), allow, ErrInvalidAssignment},
		{"implementer with a test command", cli(AgentClaude, func(a *Assignment) { a.TestCommand = "make test && go vet ./..." }), allow, nil},
		{"multi-line test command", cli(AgentClaude, func(a *Assignment) { a.TestCommand = "make test\nrm -rf /" }), allow, ErrInvalidAssignment},
		{"test command too long", cli(AgentClaude, func(a *Assignment) { a.TestCommand = strings.Repeat("x", MaxTestCommand+1) }), allow, ErrInvalidAssignment},
		{"implementer continuing a published branch (KIN-106)", cli(AgentClaude, func(a *Assignment) { a.Base, a.BranchHead = "dev", "abc123" }), allow, nil},
		{"branch head that is no sha", cli(AgentClaude, func(a *Assignment) { a.Base, a.BranchHead = "dev", "--upload-pack=x" }), allow, ErrInvalidAssignment},
		{"branch head without a base", cli(AgentClaude, func(a *Assignment) { a.Base, a.BranchHead = "", "abc123" }), allow, ErrInvalidAssignment},
		{"reviewer with a branch head", cli(AgentClaude, func(a *Assignment) { a.Base, a.Role, a.BranchHead = "dev", RoleReviewer, "abc123" }), allow, ErrInvalidAssignment},
		{"mock agent with a branch head", func(a *Assignment) { a.Base, a.BranchHead = "dev", "abc123" }, allow, ErrInvalidAssignment},
		{"reviewer with a test command", cli(AgentClaude, func(a *Assignment) { a.Role, a.TestCommand = RoleReviewer, "make test" }), allow, ErrInvalidAssignment},
		{"reviewer with a diff range", cli(AgentClaude, func(a *Assignment) { a.Role, a.DiffBase, a.DiffHead = RoleReviewer, "abc123", "def456" }), allow, nil},
		{"half a diff range", cli(AgentClaude, func(a *Assignment) { a.DiffBase = "abc123" }), allow, ErrInvalidAssignment},
		{"diff range that is no sha", cli(AgentClaude, func(a *Assignment) { a.DiffBase, a.DiffHead = "--output=/x", "def456" }), allow, ErrInvalidAssignment},
		{"mock with a test command", func(a *Assignment) { a.TestCommand = "make test" }, allow, ErrInvalidAssignment},
		{"mock with CLI fields", func(a *Assignment) { a.Prompt = "x" }, allow, ErrInvalidAssignment},
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

func cli(agent string, more func(a *Assignment)) func(a *Assignment) {
	return func(a *Assignment) {
		a.Adapter, a.Scenario, a.Prompt = agent, "", "Fix the bug"
		if more != nil {
			more(a)
		}
	}
}

func TestAgentTimeoutDefault(t *testing.T) {
	t.Parallel()
	a := valid()
	cli(AgentClaude, nil)(&a)
	if a.AgentTimeout() != DefaultAgentTimeout {
		t.Fatal(a.AgentTimeout())
	}
	a.Timeout = time.Minute
	if a.AgentTimeout() != time.Minute {
		t.Fatal(a.AgentTimeout())
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
