// Package domain is the runner's session context (KIN-23): the assignments it accepts from the Control Plane, the
// attempts it runs as child processes and how an attempt's end is reported. Pure Go, no I/O.
package domain

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	// ErrInvalidAssignment: a malformed assignment (ids, repository or scope).
	ErrInvalidAssignment = errors.New("invalid assignment")
	// ErrProjectNotAllowed: the runner's local project allowlist does not contain the assignment's project. The
	// runner checks this itself, so even an approved runner refuses another project's work.
	ErrProjectNotAllowed = errors.New("project is not allowed on this runner")
	// ErrUnknownAdapter: the runner has no adapter of that name.
	ErrUnknownAdapter = errors.New("unknown agent adapter")
)

// AdapterMock is the mock agent (no real CLI); AgentClaude and AgentCodex run the AI CLIs (KIN-32/KIN-79).
const AdapterMock = "mock"

// MaxPrompt bounds an assignment's prompt/context package (protocol.MaxPromptBytes).
const MaxPrompt = 32 << 10

// DefaultAgentTimeout bounds a CLI run whose assignment names no timeout.
const DefaultAgentTimeout = 2 * time.Hour

// Mock scenarios (same names as on the wire).
const (
	ScenarioSuccess = "success"
	ScenarioFail    = "fail"
	ScenarioFlaky   = "flaky"
	ScenarioCrash   = "crash"
	ScenarioHang    = "hang"
	ScenarioGate    = "gate"
	ScenarioScope   = "scope"
)

var scenarios = []string{ScenarioSuccess, ScenarioFail, ScenarioFlaky, ScenarioCrash, ScenarioHang, ScenarioGate, ScenarioScope}

// MaxScopePaths bounds an assignment's scope.
const MaxScopePaths = 64

// Assignment is one attempt the Control Plane asks the runner to run (execution.assign).
type Assignment struct {
	Step       string
	Fence      int64
	Execution  string
	Task       string
	Project    string
	Repository string
	Scope      []string
	Adapter    string
	Scenario   string
	// Base is the branch the attempt's worktree starts from (KIN-29); empty: an empty working directory.
	Base string
	// Token is the short-lived git credential for Repository (KIN-28/KIN-29), held in memory only.
	Token Secret
	// The AI CLI run (KIN-79; Adapter AgentClaude or AgentCodex): the model, the role the step runs as, the
	// prompt/context package, the native session id to resume, the permission bypass request and the run's timeout
	// (0: DefaultAgentTimeout).
	Model             string
	Role              string
	Prompt            string
	Resume            string
	BypassPermissions bool
	Timeout           time.Duration
}

// CLI reports an assignment run by an AI CLI (not the mock agent).
func (a Assignment) CLI() bool { return a.Adapter == AgentClaude || a.Adapter == AgentCodex }

// AgentTimeout is the CLI run's timeout.
func (a Assignment) AgentTimeout() time.Duration {
	if a.Timeout == 0 {
		return DefaultAgentTimeout
	}
	return a.Timeout
}

// Secret is a credential that never formats in clear (logs, errors, JSON).
type Secret struct{ v string }

// NewSecret wraps s.
func NewSecret(s string) Secret { return Secret{s} }

// Reveal returns the credential.
func (s Secret) Reveal() string { return s.v }

// String redacts the credential.
func (Secret) String() string { return "[redacted]" }

// GoString redacts the credential.
func (Secret) GoString() string { return "[redacted]" }

// MarshalJSON redacts the credential.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// Policy is the runner's local policy for assignments.
type Policy struct {
	// Projects is the allowlist of project ids (ai-runner run --projects). Empty: every project the Control Plane
	// granted the runner.
	Projects []string
}

var (
	idRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
)

func bad(field string) error { return fmt.Errorf("%w: %s", ErrInvalidAssignment, field) }

// CheckAssignment validates a and applies the runner's policy.
func CheckAssignment(a Assignment, p Policy) error {
	for name, v := range map[string]string{"step": a.Step, "execution": a.Execution, "task": a.Task, "project": a.Project} {
		if !idRe.MatchString(v) {
			return bad(name)
		}
	}
	if a.Fence < 1 {
		return bad("fence")
	}
	if !repoRe.MatchString(a.Repository) || strings.Contains(a.Repository, "..") {
		return bad("repository")
	}
	if len(a.Scope) > MaxScopePaths {
		return bad("scope")
	}
	for _, s := range a.Scope {
		if !scopePath(s) {
			return bad("scope")
		}
	}
	switch {
	case a.CLI():
		if err := checkAgent(a); err != nil {
			return err
		}
	case a.Adapter != AdapterMock:
		return fmt.Errorf("%w: %q", ErrUnknownAdapter, a.Adapter)
	case a.Model != "" || a.Prompt != "" || a.Resume != "" || a.BypassPermissions || a.Timeout != 0:
		return bad("agent fields for the mock agent")
	case a.Scenario != "" && !slices.Contains(scenarios, a.Scenario):
		return bad("scenario")
	}
	if a.Role != "" && !idRe.MatchString(a.Role) {
		return bad("role")
	}
	if len(p.Projects) > 0 && !slices.Contains(p.Projects, a.Project) {
		return fmt.Errorf("%w: %s", ErrProjectNotAllowed, a.Project)
	}
	return nil
}

// checkAgent validates a CLI assignment's run fields (the rest is AgentRequest.Check's, before the launch).
func checkAgent(a Assignment) error {
	switch {
	case a.Scenario != "":
		return bad("scenario")
	case strings.TrimSpace(a.Prompt) == "" || len(a.Prompt) > MaxPrompt || !utf8.ValidString(a.Prompt):
		return bad("prompt")
	case a.Model != "" && !modelRe.MatchString(a.Model):
		return bad("model")
	case a.Resume != "" && !sessionRe.MatchString(a.Resume):
		return bad("session id")
	case a.Timeout < 0 || a.Timeout > MaxAgentTimeout:
		return bad("timeout")
	}
	return nil
}

func scopePath(s string) bool {
	if s == "" || len(s) > 512 || strings.HasPrefix(s, "/") || strings.Contains(s, "\\") ||
		strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return false
	}
	c := path.Clean(s)
	return c != ".." && !strings.HasPrefix(c, "../")
}

// ScenarioOrDefault is the mock scenario to run (success when none is given).
func (a Assignment) ScenarioOrDefault() string {
	if a.Scenario == "" {
		return ScenarioSuccess
	}
	return a.Scenario
}

// Exit is how an attempt's process ended.
type Exit struct {
	Code     int
	Signaled bool // killed by a signal
	Lost     bool // adopted after a runner restart and ended without leaving a result
}

// Outcome is the attempt's result: ok (execution.completed) or the failure reason (execution.failed). An attempt
// the runner cancelled always fails with the cancel reason, whatever its exit.
func Outcome(e Exit, cancelled string) (ok bool, reason string) {
	switch {
	case cancelled != "":
		return false, "cancelled: " + cancelled
	case e.Lost:
		return false, "process lost: it ended while the runner was not watching and left no result"
	case e.Signaled:
		return false, "killed by a signal"
	case e.Code != 0:
		return false, fmt.Sprintf("exit status %d", e.Code)
	}
	return true, ""
}

// Record is the on-disk journal entry of a running attempt. It outlives a runner restart, so the runner adopts a
// process that is still alive (or reports its result) instead of starting a second one.
type Record struct {
	Step      string `json:"step"`
	Fence     int64  `json:"fence"`
	Execution string `json:"execution"`
	Project   string `json:"project"`
	Scenario  string `json:"scenario"`
	Workdir   string `json:"workdir,omitempty"` // the attempt's git worktree (KIN-29); empty: its journal directory
	// Preparing: the attempt has a workspace to prepare (KIN-71), in the background after the assignment was
	// acknowledged; with no Workdir yet, a restart interrupted the preparation.
	Preparing bool `json:"preparing,omitempty"`
	PID       int  `json:"pid,omitempty"` // 0 until the process was launched
	PGID      int  `json:"pgid,omitempty"`
	// Start is the process's start time as the kernel reports it (KIN-64): with PID it identifies the process, so
	// a pid reused by another process is never adopted. Empty for records written before KIN-64 or where the
	// system does not report it (adoption then relies on the pid leading PGID).
	Start string `json:"start,omitempty"`
	// Reserved is the highest attempt event sequence number the attempt may have used: a resumed stream continues
	// after it, so no event number is reused (the Control Plane stores each one once).
	Reserved int64 `json:"reserved"`
	// Agent is the AI CLI running the attempt (KIN-79; empty: the mock agent) and Output how far its output files
	// were reported: after a restart they are read again from the start (for the run's result) and reported only
	// beyond these offsets.
	Agent  string        `json:"agent,omitempty"`
	Output OutputOffsets `json:"output,omitzero"`
}

// OutputOffsets are byte offsets in an AI CLI's stdout and stderr files.
type OutputOffsets struct {
	Stdout int64 `json:"stdout,omitempty"`
	Stderr int64 `json:"stderr,omitempty"`
}
