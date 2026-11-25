package app

import (
	"context"
	"errors"
	"os/exec"

	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// Sandbox confines one attempt's process: the egress context's strict egress sandbox (KIN-31, KB KIN-A-22).
type Sandbox interface {
	// Command returns the command that runs argv (with env, in dir) inside the sandbox; the launcher sets its
	// output and process group handling stays as for any attempt.
	Command(argv, env []string, dir string) (*exec.Cmd, error)
	// Await returns once the started command verified the sandbox (nil: the agent runs) or failed to (the agent
	// never starts).
	Await(ctx context.Context) error
	// Release cuts the sandbox off from the network and removes its files (after the process ended).
	Release()
	// Endpoint is the sandbox's way out (its proxy adapter's socket), kept in the attempt's journal record so a
	// restarted runner can serve it again (KIN-81).
	Endpoint() string
	// Fault delivers why the sandbox lost its verified egress while the attempt runs (KIN-81: a runtime check
	// found another egress IP, or the proxy unreachable); its proxy adapter has already cut every connection. nil
	// when the sandbox is not monitored.
	Fault() <-chan string
	// Summary is a line about the sandbox's runtime worth showing in the attempt's output (connections the
	// allowlist denied); "" when there is none. Read once the process ended.
	Summary() string
}

// Confiner decides whether an attempt runs in a sandbox: nil when its network profile is not strict, an error
// when it is strict and no verified sandbox can be had (the attempt must not run at all: fail-closed).
type Confiner interface {
	Confine(ctx context.Context, a domain.Assignment, dir, workdir string) (Sandbox, error)
	// Reattach serves the sandbox of an attempt adopted after a runner restart again (KIN-81): its proxy adapter
	// lived in the old runner process. An error means it cannot be, and the attempt must be stopped (it has no way
	// out; it never gets another).
	Reattach(ctx context.Context, rec domain.Record, dir string) (Sandbox, error)
}

// ErrNetwork marks a launch refused because the strict egress sandbox was not verified.
var ErrNetwork = errors.New("strict egress sandbox not verified")

// ErrBypassUnconfined: a CLI's permission bypass (`--dangerously-skip-permissions` and the like) was asked for an
// attempt that does not run in a strict egress sandbox (owner decision, KB KIN-A-11/KIN-A-21).
var ErrBypassUnconfined = domain.ErrBypassWithoutSandbox

// PermitBypass says whether the CLI adapter may switch off the CLI's own permission prompts for this launch: only
// inside a strict egress sandbox, whose helper starts the CLI only after its preflight verified (KIN-31). A CLI
// launcher (KIN-32's cliagent) passes its result to domain.ChoosePermissions and refuses the launch on an error.
func (s LaunchSpec) PermitBypass() error {
	if s.Sandbox == nil {
		return ErrBypassUnconfined
	}
	return nil
}
