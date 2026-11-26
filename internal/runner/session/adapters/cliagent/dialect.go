// Package cliagent runs the AI CLIs (Claude Code, Codex) headless on the runner (KIN-32, KB KIN-A-16/KIN-A-21): it
// builds the command line from a validated request (the prompt is read by the CLI from stdin, from a file outside
// the workspace — never argv, never a shell) with a minimal environment (Adapter.Command). The daemon runs it as
// `ai-runner agent-exec` (ExecMain, KIN-79) in the attempt's process group, inside the strict egress sandbox when
// the profile is strict (KIN-31); agent-exec enforces the timeout (SIGTERM, then SIGKILL to the group) and writes
// the output to files that Agents.Follow parses into the common event model (domain.AgentEvent) and folds into
// the run's result: native session id, usage and a typed failure (cli_limit for rate limits and logouts). The
// session executor chooses the permission bypass only for a run inside the verified sandbox. Limits here are the
// wall-clock timeout, the parsed line size, the stderr tail, the output file size and the event field sizes;
// memory, CPU, process count and disk quotas are the sandbox's.
package cliagent

import (
	"fmt"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// Dialect is one CLI's command line and output format.
type Dialect interface {
	Name() string
	// Args is the command line (without the program) for a checked request in permission mode p.
	Args(r domain.AgentRequest, p domain.Permissions) []string
	// Env is the CLI's own fixed environment additions (KEY=VALUE).
	Env() []string
	// Parser parses one run's output.
	Parser() Parser
	// SessionDir is where, relative to HOME, the CLI keeps the sessions it resumes (KIN-86).
	SessionDir() string
	// LoggedIn reports a login the CLI can use from home and the operator's env alone: what a sandboxed run has.
	LoggedIn(home string, env []string, now time.Time) bool
	// LoginEnv is the environment that logs the CLI in from the runner's own store in home (none when nothing is
	// stored); secrets are the values to mask in the output.
	LoginEnv(home string) (env, secrets []string, err error)
}

// Parser turns one line of a CLI's JSONL output into events (none for lines that are not events).
type Parser interface {
	Parse(line []byte) []domain.AgentEvent
}

// DialectFor returns the dialect of an agent name.
func DialectFor(agent string) (Dialect, error) {
	switch agent {
	case domain.AgentClaude:
		return Claude{}, nil
	case domain.AgentCodex:
		return Codex{}, nil
	}
	return nil, fmt.Errorf("%w: unknown agent %q", domain.ErrInvalidAgentRequest, agent)
}
