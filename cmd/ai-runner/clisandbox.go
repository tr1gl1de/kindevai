package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/egress/adapters/nsbox"
	egress "github.com/tr1gl1de/kindevai/internal/runner/egress/app"
	"github.com/tr1gl1de/kindevai/internal/runner/session/adapters/cliagent"
	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// cliSandbox confines one CLI run of the KIN-32 adapters (cliagent.Sandbox) in the strict egress sandbox of its
// network profile (KIN-31): the CLI's command line becomes `ai-runner sandbox-run`, which exits with
// nsbox.ExitUnverified, never starting the CLI, when the sandbox does not verify.
type cliSandbox struct {
	supported func(context.Context) (bool, string) // egress.Sandboxes.Supported
	boxes     *nsbox.Manager
	profile   protocol.NetworkProfile
	dir       string // the attempt's state directory (kept visible in the sandbox)
}

var (
	_ cliagent.Sandbox = cliSandbox{}
	_ cliagent.Refuser = cliSandbox{}
)

// newCLISandbox returns the sandbox of a CLI run of project by role (the CLI's name): nil when its profile is not
// strict (the CLI runs on the host, safe mode only), an error when it is strict and cannot be confined here.
func newCLISandbox(ctx context.Context, s *egress.Sandboxes, project, role, dir string) (cliagent.Sandbox, error) {
	p, strict, err := s.StrictProfile(ctx, project, role)
	if err != nil || !strict {
		return nil, err
	}
	m, ok := s.Boxes.(*nsbox.Manager)
	if !ok {
		return nil, errors.New("the strict egress sandboxes cannot wrap a command")
	}
	return cliSandbox{supported: s.Supported, boxes: m, profile: p, dir: dir}, nil
}

// StrictEgress reports a runner whose sandbox self-test verified; the run itself starts the CLI only after its own
// preflight verified.
func (c cliSandbox) StrictEgress(ctx context.Context) bool {
	ok, _ := c.supported(ctx)
	return ok
}

// Wrap runs the CLI's command inside the sandbox.
func (c cliSandbox) Wrap(ctx context.Context, cmd cliagent.Command) (cliagent.Command, error) {
	if ok, why := c.supported(ctx); !ok {
		return cliagent.Command{}, fmt.Errorf("%w: %s", egress.ErrUnsupported, why)
	}
	path, args, env, err := c.boxes.Wrap(egress.BoxRequest{Dir: c.dir, Workdir: cmd.Dir, Profile: c.profile},
		append([]string{cmd.Path}, cmd.Args...), cmd.Env, cmd.Dir)
	if err != nil {
		return cliagent.Command{}, err
	}
	return cliagent.Command{Path: path, Args: args, Env: env, Dir: cmd.Dir}, nil
}

// Refused reports a run that `sandbox-run` ended because its sandbox did not verify: exit nsbox.ExitUnverified and
// the refusal it recorded, before the CLI could start, in the runner's data directory, which the confined CLI cannot
// write (KIN-88; a CLI that exits 121 and prints nsbox.UnverifiedMarker is a CLI error). The run fails with code
// network and the step waits for the network (KIN-31, KIN-83).
func (c cliSandbox) Refused(wrapped cliagent.Command, exit domain.Exit) bool {
	recorded := c.boxes.Refused(wrapped.Env) // read once, and removed, whatever the exit
	return recorded && !exit.Signaled && exit.Code == nsbox.ExitUnverified
}
