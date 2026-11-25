package domain

// AI CLI sessions (KIN-32, KB KIN-A-16/KIN-A-21): what the runner asks of a Claude Code or Codex CLI run, the
// permission mode it may use, the events parsed from the CLI's machine-readable output and how a run ends.

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// The AI CLIs the runner drives (ZCode is post-MVP, KB KIN-A-11).
const (
	AgentClaude = "claude"
	AgentCodex  = "codex"
)

var (
	// ErrInvalidAgentRequest: a malformed CLI run request.
	ErrInvalidAgentRequest = errors.New("invalid agent request")
	// ErrBypassWithoutSandbox: the permission bypass was asked for outside the verified strict-egress sandbox (owner
	// decision 24.11.2026, KB KIN-A-21: the CLI is refused, never run on the host with the bypass).
	ErrBypassWithoutSandbox = errors.New("permission bypass refused: the CLI does not run inside the verified strict-egress sandbox")
)

// MaxAgentTimeout bounds one CLI run.
const MaxAgentTimeout = 24 * time.Hour

// AgentRequest is one CLI run. Every value that reaches the CLI's argv is validated here; the prompt never does (it
// is read by the CLI from stdin, from a file outside the workspace).
type AgentRequest struct {
	Agent string
	// Workdir is the attempt's worktree (absolute): the CLI's working directory.
	Workdir string
	// PromptFile is the prompt/context package (absolute), kept outside Workdir so the agent cannot see or alter it.
	PromptFile string
	// Resume is the native session id to continue; empty starts a new session.
	Resume string
	// Model is the CLI's model name or alias; empty: the CLI's default.
	Model string
	// BypassPermissions asks for the CLI's permission bypass (only granted inside the strict-egress sandbox).
	BypassPermissions bool
	// Timeout bounds the run (wall clock); the process group is stopped when it passes.
	Timeout time.Duration
	// Confined: the run happens in the strict egress sandbox, where the CLI sees only copies of its login files and
	// the runner's token (KIN-86); without one it cannot log in, so the run is refused before it starts.
	Confined bool
}

var (
	sessionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	modelRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/\[\]-]{0,127}$`)
)

func badRequest(field string) error { return fmt.Errorf("%w: %s", ErrInvalidAgentRequest, field) }

// Check validates the request.
func (r AgentRequest) Check() error {
	if r.Agent != AgentClaude && r.Agent != AgentCodex {
		return badRequest("agent")
	}
	if !path.IsAbs(r.Workdir) {
		return badRequest("workdir")
	}
	if !path.IsAbs(r.PromptFile) || Within(r.PromptFile, r.Workdir) {
		return badRequest("prompt file")
	}
	if r.Resume != "" && !sessionRe.MatchString(r.Resume) {
		return badRequest("session id")
	}
	if r.Model != "" && !modelRe.MatchString(r.Model) {
		return badRequest("model")
	}
	if r.Timeout <= 0 || r.Timeout > MaxAgentTimeout {
		return badRequest("timeout")
	}
	return nil
}

// Within reports whether p is dir or inside it (both absolute; lexical).
func Within(p, dir string) bool {
	p, dir = path.Clean(p), path.Clean(dir)
	return p == dir || dir == "/" || strings.HasPrefix(p, dir+"/")
}

// Permissions is how a CLI handles its tool permissions.
type Permissions string

const (
	// PermissionsSafe: the CLI's own permission/approval-safe mode (edits in the workspace, nothing that would
	// prompt; Codex keeps its own sandbox).
	PermissionsSafe Permissions = "safe"
	// PermissionsBypass: --dangerously-skip-permissions / --dangerously-bypass-approvals-and-sandbox.
	PermissionsBypass Permissions = "bypass"
)

// ChoosePermissions decides the run's permission mode: the bypass only when the run is confined by the verified
// strict-egress sandbox (its preflight passed); asking for it elsewhere is refused, never downgraded silently.
func ChoosePermissions(bypass, strictEgress bool) (Permissions, error) {
	switch {
	case !bypass:
		return PermissionsSafe, nil
	case strictEgress:
		return PermissionsBypass, nil
	}
	return "", ErrBypassWithoutSandbox
}

// EventKind is the kind of a parsed CLI event.
type EventKind string

const (
	EventSession  EventKind = "session"  // the native session started (id, model, CLI version)
	EventText     EventKind = "text"     // assistant text
	EventToolUse  EventKind = "tool_use" // the agent called a tool
	EventUsage    EventKind = "usage"    // token usage of a turn (cumulative ones come with EventResult)
	EventResult   EventKind = "result"   // the final result (text and the run's total usage)
	EventFailure  EventKind = "failure"  // the CLI reported an error
	EventRetrying EventKind = "retrying" // the CLI retries an API call
)

// Bounds of event fields: an event is a summary, the raw output is streamed separately.
const (
	MaxEventText    = 16 << 10
	MaxToolInput    = 2 << 10
	MaxErrorMessage = 1 << 10
)

// AgentEvent is one event of the common model both CLIs' output is parsed into.
type AgentEvent struct {
	Kind    EventKind     `json:"kind"`
	Session string        `json:"session,omitempty"`
	Model   string        `json:"model,omitempty"`
	Version string        `json:"version,omitempty"`
	Text    string        `json:"text,omitempty"`
	Tool    string        `json:"tool,omitempty"`
	Input   string        `json:"input,omitempty"` // the tool input as JSON, truncated
	Usage   *Usage        `json:"usage,omitempty"`
	Failure *AgentFailure `json:"failure,omitempty"`
}

// Usage is the token usage and cost of a run (what the CLI reports; Codex reports no cost).
type Usage struct {
	InputTokens      int64   `json:"inputTokens"`
	OutputTokens     int64   `json:"outputTokens"`
	CacheReadTokens  int64   `json:"cacheReadTokens"`
	CacheWriteTokens int64   `json:"cacheWriteTokens"`
	CostUSD          float64 `json:"costUsd"`
	Turns            int     `json:"turns"`
	DurationMS       int64   `json:"durationMs"`
}

// Add accumulates o into u.
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CacheReadTokens += o.CacheReadTokens
	u.CacheWriteTokens += o.CacheWriteTokens
	u.CostUSD += o.CostUSD
	u.Turns += o.Turns
	u.DurationMS += o.DurationMS
}

// FailureCode is the typed outcome of a failed run.
type FailureCode string

const (
	// FailureCLILimit: the CLI hit its subscription/rate limit or is logged out; the step is BLOCKED(cli_limit)
	// until the Owner acts or the limit resets, it is not retried as a code failure.
	FailureCLILimit  FailureCode = "cli_limit"
	FailureCLI       FailureCode = "cli_error" // the CLI failed otherwise
	FailureTimeout   FailureCode = "timeout"   // the run's timeout passed
	FailureCancelled FailureCode = "cancelled" // the run was cancelled
	// FailureNetwork: the run's strict egress sandbox did not verify, the CLI never started (KIN-31); the step
	// waits for the network instead of using its retry budget.
	FailureNetwork FailureCode = "network"
)

// Causes of FailureCLILimit.
const (
	CauseRateLimit = "rate_limit"
	CauseAuth      = "auth"
	CauseBilling   = "billing" // the subscription/billing refuses the request
)

// AgentFailure is why a run failed.
type AgentFailure struct {
	Code     FailureCode `json:"code"`
	Cause    string      `json:"cause,omitempty"`
	Message  string      `json:"message,omitempty"`
	ResetsAt time.Time   `json:"resetsAt,omitzero"` // when a rate limit resets, if the CLI said so
}

// Reason is the failure as one line: "<code>[: <cause>][: <message>]".
func (f AgentFailure) Reason() string {
	parts := []string{string(f.Code)}
	if f.Cause != "" {
		parts = append(parts, f.Cause)
	}
	if f.Message != "" {
		parts = append(parts, f.Message)
	}
	return strings.Join(parts, ": ")
}

var (
	authRe = regexp.MustCompile(`(?i)not logged in|run /login|log ?in again|sign in again|401 unauthori[sz]ed|` +
		`invalid api key|authentication (failed|error|required)|oauth token (has )?expired|token (has )?expired|` +
		`could not be refreshed`)
	rateRe = regexp.MustCompile(`(?i)rate.?limit(ed| reached| exceeded)|usage limit|hit your (usage )?limit|` +
		`limit reached|429 too many requests|status:? 429\b|quota exceeded`)
)

// ClassifyCLIError maps a CLI error message to a failure: logged out or rate/usage limited is cli_limit, anything
// else cli_error. Only error texts are classified, never assistant or tool output.
func ClassifyCLIError(msg string) (FailureCode, string) {
	switch {
	case authRe.MatchString(msg):
		return FailureCLILimit, CauseAuth
	case rateRe.MatchString(msg):
		return FailureCLILimit, CauseRateLimit
	}
	return FailureCLI, ""
}

// Truncate cuts s to at most n bytes on a rune boundary, marking the cut with "…".
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

// End is how the CLI process ended, as the adapter saw it.
type End struct {
	Exit       Exit
	StderrTail string // the end of stderr, for errors the CLI reported only there
	TimedOut   bool
	Cancelled  string // the cancel reason; empty when not cancelled
	// SandboxRefused: the strict egress sandbox did not verify and the CLI never started (the launcher's sandbox
	// recognised its own refusal, KIN-31, on a signal the CLI cannot forge, KIN-88).
	SandboxRefused bool
}

// AgentResult is the outcome of a CLI run.
type AgentResult struct {
	Session string // the native session id (to resume)
	Model   string
	Version string // the CLI's version, as it reported it
	Text    string // the final result text
	Usage   Usage
	Exit    Exit
	Failure *AgentFailure // nil on success
}

// Outcome reports success or the failure reason.
func (r AgentResult) Outcome() (bool, string) {
	if r.Failure == nil {
		return true, ""
	}
	return false, r.Failure.Reason()
}

// Transcript folds a run's events into its result.
type Transcript struct {
	session, model, version string
	text                    string
	usage                   Usage
	resulted                bool
	failure                 *AgentFailure
}

// Apply folds one event in. The first cli_limit failure is kept over other failures (it decides BLOCKED).
func (t *Transcript) Apply(e AgentEvent) {
	switch e.Kind {
	case EventSession:
		t.session = first(t.session, e.Session)
		t.model = first(t.model, e.Model)
		t.version = first(t.version, e.Version)
	case EventResult:
		t.resulted = true
		t.text = e.Text
		if e.Usage != nil {
			t.usage = *e.Usage
		}
	case EventUsage:
		if e.Usage != nil && !t.resulted {
			t.usage.Add(*e.Usage)
		}
	case EventFailure:
		if e.Failure != nil && (t.failure == nil || t.failure.Code != FailureCLILimit) {
			f := *e.Failure
			t.failure = &f
		}
	}
	if e.Session != "" && t.session == "" {
		t.session = e.Session
	}
}

func first(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Result is the run's outcome once the process ended: a cancel decides over a timeout, a timeout over what the
// CLI reported, a reported failure over the exit status.
func (t *Transcript) Result(end End) AgentResult {
	r := AgentResult{Session: t.session, Model: t.model, Version: t.version, Text: t.text, Usage: t.usage, Exit: end.Exit}
	tail := Truncate(strings.TrimSpace(end.StderrTail), MaxErrorMessage)
	switch {
	case end.Cancelled != "":
		r.Failure = &AgentFailure{Code: FailureCancelled, Message: end.Cancelled}
	case end.SandboxRefused:
		r.Failure = &AgentFailure{Code: FailureNetwork, Message: tail}
	case end.TimedOut:
		r.Failure = &AgentFailure{Code: FailureTimeout, Message: "timed out"}
	case t.failure != nil:
		f := *t.failure
		r.Failure = &f
	case end.Exit.Code != 0 || end.Exit.Signaled || end.Exit.Lost:
		_, msg := Outcome(end.Exit, "")
		if tail != "" {
			msg += ": " + tail
		}
		code, cause := ClassifyCLIError(tail)
		r.Failure = &AgentFailure{Code: code, Cause: cause, Message: msg}
	case !t.resulted:
		r.Failure = &AgentFailure{Code: FailureCLI, Message: "no result: the CLI ended without reporting one"}
	}
	return r
}
