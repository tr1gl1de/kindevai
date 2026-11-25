package nsbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	egress "github.com/tr1gl1de/kindevai/internal/runner/egress/app"
)

// Wrapping a command (for launchers that start a plain command line, like the CLI adapters of KIN-32): Wrap
// writes the run's description to an owner-only file in the attempt's state directory and returns
// `<Program> <RunArgs...>` with that file's path in the environment variable RunEnv. That process,
// `ai-runner sandbox-run` (RunMain), runs on the host: it starts the run's proxy adapter, builds the sandbox like
// Box does, waits for the verdict and, once verified, relays the agent's stdin, output, signals and exit status.
// An unverified sandbox ends it with ExitUnverified, and UnverifiedMarker on stderr, before the agent ever starts.

// RunEnv carries the path of the run's description to `ai-runner sandbox-run`, which reads and deletes the file
// first thing. The description holds the upstream proxy credentials and the CLI's environment, so it never
// travels in an environment variable (readable in /proc/<pid>/environ) or the command line.
const RunEnv = "KINDEV_SANDBOX_RUN"

// UnverifiedMarker is the stderr line `sandbox-run` writes before exiting with ExitUnverified: the launcher tells
// "the sandbox did not verify" (the step waits for the network) from a CLI that happened to exit with that code.
const UnverifiedMarker = "kindev: strict egress sandbox not verified"

// RunSpec is the description of a wrapped run.
type RunSpec struct {
	Request    egress.BoxRequest `json:"request"`
	Argv       []string          `json:"argv"`
	Env        []string          `json:"env"`
	Dir        string            `json:"dir"`
	HelperArgs []string          `json:"helperArgs"` // the inner helper: `<self> <HelperArgs...>`
	HelperEnv  []string          `json:"helperEnv"`
	DataDir    string            `json:"dataDir"`
	Home       string            `json:"home"`
	Sockets    []string          `json:"sockets,omitempty"`
}

// Wrap returns the command line and environment that run argv (with env, in dir) in a strict egress sandbox for
// the profile of r. RunArgs start `ai-runner sandbox-run` (default "sandbox-run").
func (m *Manager) Wrap(r egress.BoxRequest, argv, env []string, dir string) (string, []string, []string, error) {
	if len(argv) == 0 {
		return "", nil, nil, errors.New("no command")
	}
	if !profileStrict(r.Profile) {
		return "", nil, nil, errors.New("not a strict profile with a proxy")
	}
	rs := RunSpec{Request: r, Argv: argv, Env: env, Dir: dir, HelperArgs: m.Args, HelperEnv: m.Env,
		DataDir: m.DataDir, Home: m.home(), Sockets: m.Sockets}
	b, err := json.Marshal(rs)
	if err != nil {
		return "", nil, nil, err
	}
	f, err := os.CreateTemp(r.Dir, "sandbox-run-*.json") // 0600, in the runner's own attempt directory
	if err != nil {
		return "", nil, nil, fmt.Errorf("writing the sandbox run description: %w", err)
	}
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(f.Name())
		return "", nil, nil, fmt.Errorf("writing the sandbox run description: %w", werr)
	}
	args := m.RunArgs
	if args == nil {
		args = []string{"sandbox-run"}
	}
	return m.Program, slices.Clone(args), append(slices.Clone(m.Env), RunEnv+"="+f.Name()), nil
}

// profileStrict reports a strict profile with a proxy.
func profileStrict(p protocol.NetworkProfile) bool {
	return p.Enforcement == "strict" && p.Proxy != nil
}
