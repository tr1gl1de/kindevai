// Package cliagent runs the AI CLIs (Claude Code, Codex) headless on the runner (KIN-32, KB KIN-A-16/KIN-A-21): it
// builds the command line from a validated request (the prompt is read by the CLI from stdin, from a file outside
// the workspace — never argv, never a shell), runs it with a minimal environment in its own process group, enforces
// the timeout (SIGTERM, then SIGKILL to the group), parses the CLI's machine-readable stream into the common event
// model (domain.AgentEvent) and folds it into the run's result: native session id, usage and a typed failure
// (cli_limit for rate limits and logouts). The permission bypass is used only inside the verified strict-egress
// sandbox (Sandbox, KIN-31). Limits here are the wall-clock timeout, the parsed line size, the stderr tail and
// the event field sizes; memory, CPU, process count and disk quotas are the sandbox's (cgroups of KIN-31), and the
// raw output volume is bounded by the attempt's output spool (KIN-22), which drops output once it is full.
package cliagent

import (
	"fmt"

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
