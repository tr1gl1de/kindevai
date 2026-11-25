package app

import (
	"errors"
	"io"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// AI CLI attempts (KIN-79): an assignment with adapter claude or codex runs the CLI through the Agents port. The
// CLI's output goes to files in the attempt's state directory, so a restarted runner reads on where the last one
// stopped (the process itself is adopted like any attempt's).

// AgentCommand is the CLI to run for an attempt: what the Launcher starts (in the attempt's sandbox, if any) with
// PromptFile on stdin, its output in the attempt's output files and Timeout enforced next to it (it holds across a
// runner restart).
type AgentCommand struct {
	Path       string
	Args       []string
	Env        []string
	Dir        string // the worktree
	PromptFile string // in the attempt's state directory, outside Dir
	Timeout    time.Duration
}

// AgentFollow says where a follower sends what the CLI writes.
type AgentFollow struct {
	// From is the output already reported (after a restart): read again into the run's result, not sent again.
	From domain.OutputOffsets
	// OnEvent receives each parsed event beyond From, in order.
	OnEvent func(domain.AgentEvent)
	// Stdout and Stderr receive the raw output beyond From (the attempt's output streams, with backpressure).
	Stdout, Stderr io.Writer
	// Progress receives the offsets reported so far, now and then (to persist them for a restart).
	Progress func(domain.OutputOffsets)
}

// AgentFollower follows one attempt's CLI output.
type AgentFollower interface {
	// Finish reads what is left once the process ended and returns the run's result (exit is how the attempt's
	// process ended, cancelled the executor's cancel reason).
	Finish(exit domain.Exit, cancelled string) domain.AgentResult
}

// Agents runs the AI CLIs (the cliagent adapters).
type Agents interface {
	// Prepare writes the prompt into the attempt's state directory dir, validates the request (req.PromptFile is
	// set by Prepare; an empty req.Workdir gets a working directory under dir) and returns the CLI's command in
	// permission mode p.
	Prepare(dir string, req domain.AgentRequest, prompt string, p domain.Permissions) (AgentCommand, error)
	// Follow starts following the output files of the attempt's CLI in dir.
	Follow(agent, dir string, h AgentFollow) (AgentFollower, error)
}

// ErrNoAgent: the runner has no CLI of the assigned agent.
var ErrNoAgent = errors.New("this runner has no such AI CLI")
