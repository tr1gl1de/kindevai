package nsbox

import (
	"encoding/json"
	"errors"
	"slices"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	egress "github.com/tr1gl1de/kindevai/internal/runner/egress/app"
)

// Wrapping a command (for launchers that start a plain command line, like the CLI adapters of KIN-32): Wrap
// returns `<Program> <RunArgs...>` with the run's description in the environment variable RunEnv. That process,
// `ai-runner sandbox-run` (RunMain), runs on the host: it starts the run's proxy adapter, builds the sandbox like
// Box does, waits for the verdict and, once verified, relays the agent's stdin, output, signals and exit status.
// An unverified sandbox ends it with ExitUnverified before the agent ever starts.

// RunEnv carries the run's description to `ai-runner sandbox-run`. It holds the upstream proxy credentials:
// only that host process sees it (the sandbox has its own PID namespace and never gets this variable).
const RunEnv = "KINDEV_SANDBOX_RUN"

// RunSpec is the description of a wrapped run.
type RunSpec struct {
	Request    egress.BoxRequest `json:"request"`
	Argv       []string          `json:"argv"`
	Env        []string          `json:"env"`
	Dir        string            `json:"dir"`
	HelperArgs []string          `json:"helperArgs"` // the inner helper: `<self> <HelperArgs...>`
	HelperEnv  []string          `json:"helperEnv"`
	DataDir    string            `json:"dataDir"`
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
		DataDir: m.DataDir, Sockets: m.Sockets}
	b, err := json.Marshal(rs)
	if err != nil {
		return "", nil, nil, err
	}
	args := m.RunArgs
	if args == nil {
		args = []string{"sandbox-run"}
	}
	return m.Program, slices.Clone(args), append(slices.Clone(m.Env), RunEnv+"="+string(b)), nil
}

// profileStrict reports a strict profile with a proxy.
func profileStrict(p protocol.NetworkProfile) bool {
	return p.Enforcement == "strict" && p.Proxy != nil
}
