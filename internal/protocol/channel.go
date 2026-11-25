package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Command/event channel (KIN-22, KB KIN-A-18). After runner.authorized both directions carry sequenced streams
// with application ACKs (at-least-once transfer, idempotent processing):
//
//   - Commands, Control Plane → runner: taken from the durable outbox into a per-runner log that assigns a
//     monotonic Seq; re-sent until the runner acknowledges them (after a reconnect and on an ACK timeout).
//   - Events, runner → Control Plane: written to the runner's bounded on-disk spool first (fsync), sent in spool
//     order and deleted only once the Control Plane acknowledged them after its commit.
//
// Both streams use the same receive rule (Cursor): a stream is identified by an epoch (a random id of the sender's
// log; a new one restarts numbering), the receiver applies only the next Seq, ignores duplicates and drops a gap
// (the sender re-sends from the last ACK: go-back-N). ACKs are cumulative. Base is the sender's last acknowledged
// Seq: a receiver whose state is behind it (restored from a backup, data dir wiped) jumps forward instead of
// waiting forever for messages that no longer exist.
const (
	TypeCommand           = "runner.command"            // server → runner: Command
	TypeCommandAck        = "runner.command.ack"        // runner → server: Ack (cumulative)
	TypeEvent             = "runner.event"              // runner → server: Event
	TypeEventAck          = "runner.event.ack"          // server → runner: Ack (cumulative)
	TypeReconcile         = "runner.reconcile"          // server → runner: Reconcile, sent when a session opens
	TypeReconcileResponse = "runner.reconcile.response" // runner → server: ReconcileResponse
)

// Command kinds. Commands are typed, policy-checked step assignments resolved by runner adapters, never shell
// strings. A runner acknowledges a kind it does not implement (and logs it), so new kinds can be added (KIN-43
// plan.discuss, KIN-51 approvals) without stalling the stream on older runners.
const (
	CommandAssign = "execution.assign" // body: Assign (KIN-23)
	CommandCancel = "execution.cancel" // body: Cancel
)

// Event kinds. Every event belongs to one attempt (Step + Fence); AttemptSeq numbers the attempt's events from 1.
const (
	EventAccepted      = "execution.accepted"       // the runner took the assignment
	EventStarted       = "execution.started"        // the process was launched
	EventOutput        = "execution.output"         // body: Output
	EventOutputDropped = "execution.output.dropped" // body: OutputDropped — the spool was full
	EventCompleted     = "execution.completed"      // terminal: success
	EventFailed        = "execution.failed"         // terminal, body: Failure
	// Workspace events (KIN-29), sent before the attempt's process starts and before its terminal event.
	EventWorkspacePrepared = "workspace.prepared" // body: WorkspacePrepared
	EventWorkspaceChanges  = "workspace.changes"  // body: WorkspaceChanges
	// EventWorkspacePreparing reports that the runner is preparing the attempt's workspace (KIN-71), when it starts
	// and periodically while it lasts; the Control Plane extends the claim's lease with it.
	EventWorkspacePreparing = "workspace.preparing" // body: WorkspacePreparing
	// EventAgent is one typed event of the attempt's AI CLI (KIN-79): its session, text, tool use, result or
	// failure, parsed from the CLI's output (the raw output goes as execution.output).
	EventAgent = "execution.agent" // body: AgentEvent
	// EventTests is the runner's test evidence (KIN-35): the assignment's TestCommand run in the worktree after the
	// AI CLI ended, sent before the attempt's final workspace.changes and terminal event.
	EventTests = "execution.tests" // body: TestResult
	// EventWorkspacePushed reports that a publish attempt (KIN-36) pushed the step's work branch, before its
	// terminal event.
	EventWorkspacePushed = "workspace.pushed" // body: WorkspacePushed
)

// TerminalEvent reports whether kind ends an attempt.
func TerminalEvent(kind string) bool { return kind == EventCompleted || kind == EventFailed }

var eventKinds = map[string]bool{EventAccepted: true, EventStarted: true, EventOutput: true, EventOutputDropped: true,
	EventCompleted: true, EventFailed: true, EventWorkspacePrepared: true, EventWorkspaceChanges: true, EventAgent: true,
	EventWorkspacePreparing: true, EventTests: true, EventWorkspacePushed: true}

// Limits of the channel. MaxOutputChunk raw bytes of output escape to at most 6× in JSON (\u00XX), so an output
// event stays below MaxMessageBytes.
const (
	MaxIDLen       = 128
	MaxBodyBytes   = 56 << 10
	MaxOutputChunk = 8 << 10
	MaxReasonLen   = 1024
)

// Command is the payload of runner.command. ID is the command id (unique per runner; the runner's handlers are
// idempotent by it), Seq its position in the runner's command log.
type Command struct {
	Epoch string          `json:"epoch"`
	Seq   int64           `json:"seq"`
	Base  int64           `json:"base"` // the last Seq the runner acknowledged, as the Control Plane knows it
	ID    string          `json:"id"`
	Kind  string          `json:"kind"`
	Body  json.RawMessage `json:"body,omitempty"`
}

// Ack is the payload of runner.command.ack and runner.event.ack: every message of the epoch up to Seq is done.
type Ack struct {
	Epoch string `json:"epoch"`
	Seq   int64  `json:"seq"`
}

// Event is the payload of runner.event. Seq/Base are the spool's (transport), AttemptSeq the attempt's (domain;
// the Control Plane stores each (Step, Fence, AttemptSeq) once). Fence is the attempt's fencing token: an event
// whose token is no longer current is rejected, so a stale runner never overwrites state.
type Event struct {
	Epoch      string          `json:"epoch"`
	Seq        int64           `json:"seq"`
	Base       int64           `json:"base"`
	Step       string          `json:"step"`
	Fence      int64           `json:"fence"`
	AttemptSeq int64           `json:"attemptSeq"`
	Kind       string          `json:"kind"`
	At         int64           `json:"at"` // unix milliseconds on the runner
	Body       json.RawMessage `json:"body,omitempty"`
}

// Assign is the body of execution.assign (KIN-23): the attempt to run as typed fields, never a shell string. The
// runner checks it again before launching (project allowlist, known adapter, repository and scope format), so a
// compromised or buggy Control Plane cannot make it run another project's work.
type Assign struct {
	Step       string   `json:"step"`
	Fence      int64    `json:"fence"`
	Execution  string   `json:"execution"`
	Task       string   `json:"task"`
	Project    string   `json:"project"`
	Repository string   `json:"repository"` // "owner/name"
	Scope      []string `json:"scope"`      // repository-relative paths the step may change
	// Adapter is the agent to run: AdapterMock, or the AI CLI routing chose (KIN-33: "claude", "codex"; run by the
	// CLI adapters, KIN-79).
	Adapter  string `json:"adapter"`
	Scenario string `json:"scenario,omitempty"`
	// Model is the CLI's model routing chose from the role binding ("" = the CLI's default); Role the step's
	// platform role (planner, implementer, reviewer), informational (KIN-33).
	Model string `json:"model,omitempty"`
	Role  string `json:"role,omitempty"`
	// The CLI run (KIN-79; adapters claude and codex only). Prompt is the prompt/context package: the runner writes it
	// to a file in the attempt's state directory, outside the worktree, and the CLI reads it on stdin. Resume is the
	// native session id to continue (empty: a new session). BypassPermissions asks for the CLI's permission bypass,
	// which the runner grants only inside the verified strict egress sandbox (KIN-31) and refuses elsewhere.
	// TimeoutSec bounds the run (0: the runner's default).
	Prompt            string `json:"prompt,omitempty"`
	Resume            string `json:"resume,omitempty"`
	BypassPermissions bool   `json:"bypassPermissions,omitempty"`
	TimeoutSec        int64  `json:"timeoutSec,omitempty"`
	// The review loop (KIN-35). TestCommand is the repository's test command: after the CLI ended successfully the
	// runner runs it with /bin/sh in the worktree, inside the attempt's confinement, and reports execution.tests.
	// DiffBase..DiffHead is the work a reviewer (Role reviewer) reviews: the runner adds the bounded diff to its
	// prompt and runs the CLI read-only.
	TestCommand string `json:"testCommand,omitempty"`
	DiffBase    string `json:"diffBase,omitempty"`
	DiffHead    string `json:"diffHead,omitempty"`
	// Base is the branch the attempt's worktree starts from (the repository's integration branch, KIN-26/KIN-29).
	// Empty: the attempt runs in an empty directory (no registered repository).
	Base string `json:"base,omitempty"`
	// Credential is added by the Control Plane right before sending (Channel.Prepare), never stored in its command
	// log: a short-lived installation token for Repository only (KIN-28).
	Credential *GitCredential `json:"credential,omitempty"`
}

// Agent adapters a runner may be assigned: the mock agent and the AI CLIs (KIN-32/KIN-79).
const (
	AdapterMock   = "mock"
	AdapterClaude = "claude"
	AdapterCodex  = "codex"
	// AdapterPublish pushes the step's reviewed work branch (KIN-36; Role RolePublisher, DiffHead the commit): no
	// process runs. The runner reports workspace.pushed before execution.completed.
	AdapterPublish = "publish"
	RolePublisher  = "publisher"
)

// CLIAdapter reports an adapter that runs an AI CLI.
func CLIAdapter(adapter string) bool { return adapter == AdapterClaude || adapter == AdapterCodex }

// MaxPromptBytes bounds Assign.Prompt (the command body must stay below MaxBodyBytes).
const MaxPromptBytes = 32 << 10

// Mock scenarios (AdapterMock): deterministic behaviours of the mock agent process.
const (
	ScenarioSuccess = "success" // prints a few lines, exits 0
	ScenarioFail    = "fail"    // exits 1 on every attempt
	ScenarioFlaky   = "flaky"   // fails on the first attempt (fence 1), succeeds on a retry
	ScenarioCrash   = "crash"   // kills itself with SIGKILL (no result)
	ScenarioHang    = "hang"    // runs until it is cancelled
	ScenarioGate    = "gate"    // waits until the file "release" appears in its working directory, then exits 0
	// ScenarioScope changes files in its worktree (KIN-51): the first attempt writes mock/scope.txt and outside.txt
	// (beyond the mock plan's scope "mock") and waits like gate; a later attempt writes mock/scope.txt and exits 0.
	ScenarioScope = "scope"
)

// MockScenarios lists every scenario.
var MockScenarios = []string{ScenarioSuccess, ScenarioFail, ScenarioFlaky, ScenarioCrash, ScenarioHang, ScenarioGate,
	ScenarioScope}

// Cancel is the body of execution.cancel: the runner stops the attempt's whole process group. Reason
// "stale_fence": the Control Plane rejected the attempt's events because a newer token exists; "cancelled": the
// task or execution was cancelled.
type Cancel struct {
	Step   string `json:"step"`
	Fence  int64  `json:"fence"`
	Reason string `json:"reason"`
}

// CancelStaleFence is Cancel.Reason for an attempt whose token was superseded.
const CancelStaleFence = "stale_fence"

// CancelCancelled is Cancel.Reason for an attempt of a cancelled task or execution.
const CancelCancelled = "cancelled"

// CancelRunnerDisabled is Cancel.Reason for the attempts of a runner the Owner disabled (KIN-63). CancelRunnerRevoked
// is never sent (a revoked runner never connects again): it is the reason a revoked runner stops its attempts with
// itself, on close code 4410.
const (
	CancelRunnerDisabled = "runner_disabled"
	CancelRunnerRevoked  = "runner_revoked"
)

// CancelScopeExceeded is Cancel.Reason for an attempt that changed files beyond its approved scope (KIN-51): the
// step waits for a new approval.
const CancelScopeExceeded = "scope_exceeded"

// AssignCommandID and CancelCommandID are the deterministic command ids of an attempt's assignment and
// cancellation: a repeated use case appends nothing, and the stale-fence cancel and a task cancel share one.
func AssignCommandID(step string, fence int64) string {
	return fmt.Sprintf("assign/%s/%d", step, fence)
}

// CancelCommandID: see AssignCommandID.
func CancelCommandID(step string, fence int64) string {
	return fmt.Sprintf("cancel/%s/%d", step, fence)
}

// Output is the body of execution.output: a chunk of a process stream at byte Offset of that stream. Offsets count
// the redacted stream the runner sends (KIN-77) and are informational: the Control Plane may mask the data again, so
// a stored chunk's length need not match the next chunk's offset.
type Output struct {
	Stream string `json:"stream"` // stdout | stderr
	Offset int64  `json:"offset"`
	Data   string `json:"data"`
}

// OutputDropped is the body of execution.output.dropped: output lost because the spool was full (overflow policy:
// output is dropped and counted, lifecycle events are kept).
type OutputDropped struct {
	Bytes  int64 `json:"bytes"`
	Chunks int64 `json:"chunks"`
}

// Started is the body of execution.started (KIN-25): the agent (CLI adapter, e.g. "mock") that runs the attempt.
// The Control Plane counts the attempt's agent time from this event's time to its terminal event's time.
type Started struct {
	Agent string `json:"agent,omitempty"`
}

// Failure is the body of execution.failed. Code classifies a failure the Control Plane treats specially; for an AI
// CLI run (KIN-79) Cause refines it (FailureCLILimit: rate_limit | auth | billing), ResetsAt is when a rate limit
// resets (unix milliseconds, if the CLI said so) and Agent is what the run reported before it failed.
type Failure struct {
	Reason   string       `json:"reason"`
	Code     string       `json:"code,omitempty"`
	Cause    string       `json:"cause,omitempty"`
	ResetsAt int64        `json:"resetsAt,omitempty"`
	Agent    *AgentReport `json:"agent,omitempty"`
}

// Failure codes of an AI CLI run (KIN-79). FailureCLILimit: the CLI hit its subscription/rate limit or is logged
// out; the step waits (BLOCKED cli_limit) instead of using its retry budget. The others are informational.
const (
	FailureCLILimit  = "cli_limit"
	FailureCLIError  = "cli_error"
	FailureTimeout   = "timeout"
	FailureCancelled = "cancelled"
)

// TestResult is the body of execution.tests: how the test command ended (ExitCode -1 when it was killed or did not
// start), how long it ran and the masked tail of its output (at most MaxTestOutput bytes).
type TestResult struct {
	Command    string `json:"command"`
	ExitCode   int    `json:"exitCode"`
	TimedOut   bool   `json:"timedOut,omitempty"`
	DurationMS int64  `json:"durationMs"`
	Output     string `json:"output,omitempty"`
}

// MaxTestOutput bounds TestResult.Output; MaxTestCommand bounds Assign.TestCommand.
const (
	MaxTestOutput  = 4 << 10
	MaxTestCommand = 500
)

// Completed is the body of execution.completed: what the AI CLI run reported (nil for the mock agent).
type Completed struct {
	Agent *AgentReport `json:"agent,omitempty"`
}

// AgentReport is an AI CLI run's report (KIN-79): its native session id (to resume), model, CLI version and usage.
// Result is the run's final answer (KIN-34: the planner's plan), masked and at most MaxAgentResult bytes, cut
// further so the event body stays within MaxBodyBytes; it is untrusted text.
type AgentReport struct {
	Session string      `json:"session,omitempty"`
	Model   string      `json:"model,omitempty"`
	Version string      `json:"version,omitempty"`
	Usage   *AgentUsage `json:"usage,omitempty"`
	Result  string      `json:"result,omitempty"`
}

// MaxAgentResult bounds AgentReport.Result (bytes).
const MaxAgentResult = 24 << 10

// AgentUsage is the token usage and cost of a run as the CLI reported it (Codex reports no cost).
type AgentUsage struct {
	InputTokens      int64   `json:"inputTokens"`
	OutputTokens     int64   `json:"outputTokens"`
	CacheReadTokens  int64   `json:"cacheReadTokens"`
	CacheWriteTokens int64   `json:"cacheWriteTokens"`
	CostUSD          float64 `json:"costUsd"`
	Turns            int     `json:"turns"`
	DurationMS       int64   `json:"durationMs"`
}

// Bounds of an AgentEvent's text fields: an event is a summary (the raw output is streamed separately) and its
// body stays below MaxBodyBytes even when every byte escapes to \u00XX.
const (
	MaxAgentText  = 6 << 10
	MaxAgentInput = 1 << 10
)

// AgentEvent is the body of execution.agent: one typed event of the AI CLI (kind session | text | tool_use |
// usage | result | failure | retrying).
type AgentEvent struct {
	Kind     string      `json:"kind"`
	Session  string      `json:"session,omitempty"`
	Model    string      `json:"model,omitempty"`
	Version  string      `json:"version,omitempty"`
	Text     string      `json:"text,omitempty"`
	Tool     string      `json:"tool,omitempty"`
	Input    string      `json:"input,omitempty"`
	Usage    *AgentUsage `json:"usage,omitempty"`
	Code     string      `json:"code,omitempty"` // failure: its code, cause and reset time
	Cause    string      `json:"cause,omitempty"`
	Message  string      `json:"message,omitempty"`
	ResetsAt int64       `json:"resetsAt,omitempty"`
}

// FailureNetwork: the attempt did not run because its strict egress sandbox could not be built or verified
// (KIN-31, fail-closed). The step waits for the network (WAITING, reason network) instead of using its retry
// budget.
const FailureNetwork = "network"

// Reconcile is the payload of runner.reconcile: the Control Plane asks for the runner's process and spool state
// whenever a session opens (a lost heartbeat means "unknown", never "stopped": reconcile before any retry).
type Reconcile struct {
	RequestID string `json:"requestId"`
}

// ReconcileResponse is the payload of runner.reconcile.response.
type ReconcileResponse struct {
	RequestID string `json:"requestId"`
	// Attempts are the attempts the runner knows: running processes and finished ones whose terminal event is
	// still in the spool (Result set).
	Attempts []AttemptStatus `json:"attempts"`
	Spool    SpoolOffsets    `json:"spool"`
	Commands Ack             `json:"commands"` // the last command the runner applied
}

// AttemptStatus is one attempt in a reconcile response.
type AttemptStatus struct {
	Step    string `json:"step"`
	Fence   int64  `json:"fence"`
	PID     int    `json:"pid,omitempty"`
	PGID    int    `json:"pgid,omitempty"`
	Running bool   `json:"running"`
	// Result is the terminal event kind still waiting in the spool (execution.completed|failed), at spool ResultSeq.
	Result    string `json:"result,omitempty"`
	ResultSeq int64  `json:"resultSeq,omitempty"`
}

// SpoolOffsets describe the runner's event spool.
type SpoolOffsets struct {
	Epoch        string `json:"epoch"`
	Acked        int64  `json:"acked"`        // the last Seq the Control Plane acknowledged
	Last         int64  `json:"last"`         // the last Seq written
	Bytes        int64  `json:"bytes"`        // on disk
	LimitBytes   int64  `json:"limitBytes"`   // configured limit
	DroppedBytes int64  `json:"droppedBytes"` // output dropped since the runner started
}

// Verdict is what a receiver does with a sequenced message.
type Verdict int

const (
	Invalid   Verdict = iota // malformed sequence fields: a protocol error
	Apply                    // the next message: apply it, then store the returned cursor and ACK
	Duplicate                // already applied: ACK again, do nothing else
	Gap                      // a message before it is missing: drop it, the sender re-sends from the last ACK
)

func (v Verdict) String() string {
	return [...]string{"invalid", "apply", "duplicate", "gap"}[v]
}

// Cursor is a receiver's position in one stream: the epoch and the last Seq applied.
type Cursor struct {
	Epoch string
	Last  int64
}

// Receive decides what to do with message seq of epoch whose sender last saw base acknowledged, and returns the
// cursor to store (for Apply: once the message is applied; for Duplicate and Gap it may have moved to a new
// epoch or forward to base).
func (c Cursor) Receive(epoch string, seq, base int64) (Verdict, Cursor) {
	if epoch == "" || seq < 1 || base < 0 || base >= seq {
		return Invalid, c
	}
	if epoch != c.Epoch {
		c = Cursor{Epoch: epoch}
	}
	if base > c.Last {
		c.Last = base
	}
	switch {
	case seq <= c.Last:
		return Duplicate, c
	case seq == c.Last+1:
		return Apply, Cursor{Epoch: epoch, Last: seq}
	}
	return Gap, c
}

var kindRE = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// ErrChannelField: a channel message failed validation (the connection is closed with CloseProtocolError).
var ErrChannelField = errors.New("invalid channel message")

func checkID(name, v string) error {
	if v == "" || len(v) > MaxIDLen || strings.IndexFunc(v, func(r rune) bool { return unicode.IsControl(r) || r == ' ' }) >= 0 {
		return fmt.Errorf("%w: %s", ErrChannelField, name)
	}
	return nil
}

// Validate checks the command's fields and size.
func (c Command) Validate() error {
	if err := checkID("epoch", c.Epoch); err != nil {
		return err
	}
	if err := checkID("id", c.ID); err != nil {
		return err
	}
	if !kindRE.MatchString(c.Kind) || len(c.Kind) > MaxIDLen {
		return fmt.Errorf("%w: kind", ErrChannelField)
	}
	if c.Seq < 1 || c.Base < 0 || c.Base >= c.Seq {
		return fmt.Errorf("%w: seq", ErrChannelField)
	}
	if len(c.Body) > MaxBodyBytes {
		return fmt.Errorf("%w: body too large", ErrChannelField)
	}
	return nil
}

// Validate checks the event's fields and size.
func (e Event) Validate() error {
	if err := checkID("epoch", e.Epoch); err != nil {
		return err
	}
	if err := checkID("step", e.Step); err != nil {
		return err
	}
	if !eventKinds[e.Kind] {
		return fmt.Errorf("%w: kind", ErrChannelField)
	}
	if e.Seq < 1 || e.Base < 0 || e.Base >= e.Seq || e.Fence < 1 || e.AttemptSeq < 1 {
		return fmt.Errorf("%w: seq", ErrChannelField)
	}
	if len(e.Body) > MaxBodyBytes {
		return fmt.Errorf("%w: body too large", ErrChannelField)
	}
	return nil
}
