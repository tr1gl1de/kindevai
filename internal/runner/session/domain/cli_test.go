package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func validRequest() AgentRequest {
	return AgentRequest{Agent: AgentClaude, Workdir: "/data/workspaces/acme/app/T1--S1",
		PromptFile: "/data/attempts/S1-1/prompt.md", Timeout: time.Hour}
}

func TestAgentRequestCheck(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(r *AgentRequest)
		ok     bool
	}{
		{"valid", func(*AgentRequest) {}, true},
		{"codex", func(r *AgentRequest) { r.Agent = AgentCodex }, true},
		{"resume a native session", func(r *AgentRequest) { r.Resume = "8bc06756-ac80-4061-889c-c0ee7d40571c" }, true},
		{"model alias", func(r *AgentRequest) { r.Model = "claude-sonnet-4.5" }, true},
		{"unknown agent", func(r *AgentRequest) { r.Agent = "zcode" }, false},
		{"relative workdir", func(r *AgentRequest) { r.Workdir = "app" }, false},
		{"relative prompt file", func(r *AgentRequest) { r.PromptFile = "prompt.md" }, false},
		{"prompt file inside the workspace", func(r *AgentRequest) { r.PromptFile = r.Workdir + "/prompt.md" }, false},
		{"prompt file is the workspace", func(r *AgentRequest) { r.PromptFile = r.Workdir }, false},
		{"prompt file escapes back into the workspace", func(r *AgentRequest) {
			r.PromptFile = "/data/attempts/../workspaces/acme/app/T1--S1/p.md"
		}, false},
		{"session id that is a flag", func(r *AgentRequest) { r.Resume = "--dangerously-skip-permissions" }, false},
		{"session id with spaces", func(r *AgentRequest) { r.Resume = "a b" }, false},
		{"model that is a flag", func(r *AgentRequest) { r.Model = "-p" }, false},
		{"model with a shell metacharacter", func(r *AgentRequest) { r.Model = "opus;rm" }, false},
		{"no timeout", func(r *AgentRequest) { r.Timeout = 0 }, false},
		{"timeout beyond the maximum", func(r *AgentRequest) { r.Timeout = MaxAgentTimeout + time.Second }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := validRequest()
			c.mutate(&r)
			err := r.Check()
			if c.ok != (err == nil) {
				t.Fatalf("Check() = %v, want ok=%v", err, c.ok)
			}
			if err != nil && !errors.Is(err, ErrInvalidAgentRequest) {
				t.Fatalf("error %v is not ErrInvalidAgentRequest", err)
			}
		})
	}
}

// Owner decision 24.11.2026 (KB KIN-A-21): the permission bypass runs only inside the verified strict-egress sandbox.
func TestChoosePermissions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		bypass, strict bool
		want           Permissions
		err            error
	}{
		{"safe mode on the host", false, false, PermissionsSafe, nil},
		{"safe mode in the sandbox", false, true, PermissionsSafe, nil},
		{"bypass in the verified sandbox", true, true, PermissionsBypass, nil},
		{"bypass without the sandbox is refused", true, false, "", ErrBypassWithoutSandbox},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := ChoosePermissions(c.bypass, c.strict)
			if got != c.want || !errors.Is(err, c.err) {
				t.Fatalf("ChoosePermissions(%v, %v) = %q, %v; want %q, %v", c.bypass, c.strict, got, err, c.want, c.err)
			}
		})
	}
}

func TestClassifyCLIError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		msg   string
		code  FailureCode
		cause string
	}{
		{"Not logged in · Please run /login", FailureCLILimit, CauseAuth},
		{"Invalid API key · Please run /login", FailureCLILimit, CauseAuth},
		{"OAuth token has expired. Please obtain a new token or refresh your existing token.", FailureCLILimit, CauseAuth},
		{"unexpected status 401 Unauthorized: Your access token could not be refreshed. Please log out and sign in again.",
			FailureCLILimit, CauseAuth},
		{"Claude AI usage limit reached|1795575600", FailureCLILimit, CauseRateLimit},
		{"You've hit your limit · resets 3pm (Europe/Moscow)", FailureCLILimit, CauseRateLimit},
		{"You've hit your usage limit. Upgrade to Pro or try again in 2 hours 5 minutes.", FailureCLILimit, CauseRateLimit},
		{"stream error: exceeded retry limit, last status: 429 Too Many Requests", FailureCLILimit, CauseRateLimit},
		{"API Error: 500 Internal server error", FailureCLI, ""},
		{"error_max_turns", FailureCLI, ""},
		{"", FailureCLI, ""},
		{"panic: authentication module crashed while compiling", FailureCLI, ""},
		{"read /work/401.txt: line 429: unexpected token", FailureCLI, ""},
		{"error: the rate limiter test failed", FailureCLI, ""},
	}
	for _, c := range cases {
		t.Run(c.msg, func(t *testing.T) {
			t.Parallel()
			code, cause := ClassifyCLIError(c.msg)
			if code != c.code || cause != c.cause {
				t.Fatalf("ClassifyCLIError(%q) = %q, %q; want %q, %q", c.msg, code, cause, c.code, c.cause)
			}
		})
	}
}

func TestTranscriptResult(t *testing.T) {
	t.Parallel()
	usage := Usage{InputTokens: 4, OutputTokens: 131, CacheReadTokens: 32918, CacheWriteTokens: 14183, CostUSD: 0.003, Turns: 2}
	session := AgentEvent{Kind: EventSession, Session: "s-1", Model: "m", Version: "2.1.331"}
	limit := AgentEvent{Kind: EventFailure, Failure: &AgentFailure{Code: FailureCLILimit, Cause: CauseRateLimit,
		Message: "You've hit your limit"}}
	cases := []struct {
		name      string
		events    []AgentEvent
		end       End
		ok        bool
		code      FailureCode
		cause     string
		reasonHas string
	}{
		{"success", []AgentEvent{session, {Kind: EventText, Text: "hi"}, {Kind: EventResult, Text: "done", Usage: &usage}},
			End{}, true, "", "", ""},
		{"no result event", []AgentEvent{session}, End{}, false, FailureCLI, "", "no result"},
		{"exit status without a reported error", []AgentEvent{session}, End{Exit: Exit{Code: 2}, StderrTail: "boom"},
			false, FailureCLI, "", "exit status 2: boom"},
		{"rate limit in stderr", []AgentEvent{session}, End{Exit: Exit{Code: 1}, StderrTail: "429 Too Many Requests"},
			false, FailureCLILimit, CauseRateLimit, "cli_limit"},
		{"reported limit wins over a clean exit", []AgentEvent{session, limit, {Kind: EventResult, Text: "x"}},
			End{}, false, FailureCLILimit, CauseRateLimit, "cli_limit: rate_limit: You've hit your limit"},
		{"timeout wins", []AgentEvent{session, limit}, End{Exit: Exit{Signaled: true}, TimedOut: true},
			false, FailureTimeout, "", "timed out"},
		{"cancel wins over everything", []AgentEvent{session}, End{Exit: Exit{Signaled: true}, TimedOut: true, Cancelled: "runner_disabled"},
			false, FailureCancelled, "", "cancelled: runner_disabled"},
		{"killed by a signal", []AgentEvent{session}, End{Exit: Exit{Signaled: true}}, false, FailureCLI, "", "signal"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var tr Transcript
			for _, e := range c.events {
				tr.Apply(e)
			}
			res := tr.Result(c.end)
			if res.Session != "s-1" || res.Version != "2.1.331" {
				t.Fatalf("session/version = %q/%q", res.Session, res.Version)
			}
			ok, reason := res.Outcome()
			if ok != c.ok {
				t.Fatalf("Outcome() ok = %v (%s), want %v", ok, reason, c.ok)
			}
			if c.ok {
				if res.Text != "done" || res.Usage != usage {
					t.Fatalf("result = %+v", res)
				}
				return
			}
			if res.Failure == nil || res.Failure.Code != c.code || res.Failure.Cause != c.cause {
				t.Fatalf("failure = %+v, want %s/%s", res.Failure, c.code, c.cause)
			}
			if !strings.Contains(reason, c.reasonHas) {
				t.Fatalf("reason %q does not contain %q", reason, c.reasonHas)
			}
		})
	}
}

func TestTruncateKeepsValidUTF8(t *testing.T) {
	t.Parallel()
	s := strings.Repeat("я", 10) // 2 bytes each
	got := Truncate(s, 5)
	if got != "яя…" {
		t.Fatalf("Truncate = %q", got)
	}
	if Truncate("abc", 5) != "abc" {
		t.Fatal("short strings are kept")
	}
}
