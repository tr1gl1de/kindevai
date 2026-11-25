package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/app"
	"github.com/tr1gl1de/kindevai/internal/runner/egress/adapters/configstore"
	"github.com/tr1gl1de/kindevai/internal/runner/egress/adapters/httpprobe"
	"github.com/tr1gl1de/kindevai/internal/runner/egress/adapters/nsbox"
	egress "github.com/tr1gl1de/kindevai/internal/runner/egress/app"
	"github.com/tr1gl1de/kindevai/internal/runner/platform/datadir"
	"github.com/tr1gl1de/kindevai/internal/runner/session/adapters/journal"
	sessionapp "github.com/tr1gl1de/kindevai/internal/runner/session/app"
	sessiondomain "github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// networkConfigPath is where the Control Plane's network configuration is kept (0600: it holds proxy passwords).
func networkConfigPath(d *datadir.Dir) string {
	return filepath.Join(d.Path(), "network", "config.json")
}

// networkAgent handles runner.config.update and network.test (KIN-30).
func networkAgent(d *datadir.Dir, logger *slog.Logger) *egress.Agent {
	return &egress.Agent{Store: configstore.File{Path: networkConfigPath(d)}, Prober: &httpprobe.Prober{}, Logger: logger}
}

// withNetwork routes network commands to the agent and everything else to next.
func withNetwork(a *egress.Agent, next app.CommandFunc) app.CommandFunc {
	return func(ctx context.Context, c protocol.Command) error {
		if err := a.Command(ctx, c); !errors.Is(err, egress.ErrNotNetworkCommand) {
			return err
		}
		return next(ctx, c)
	}
}

// networkLink sends Test Network results on every authorized link.
type networkLink struct{ a *egress.Agent }

var _ app.LinkHandler = networkLink{}

func (n networkLink) Serve(ctx context.Context, c app.Conn) error { return n.a.Serve(ctx, c) }

func (networkLink) Handle(context.Context, app.Conn, protocol.Envelope) (bool, error) {
	return false, nil
}

// testNetwork runs the preflight locally with the runner's default network profile, as delivered by the Control
// Plane (works while the daemon runs).
func testNetwork(ctx context.Context, args []string, e *env) error {
	fs, dataDir := flags("test-network", e)
	if err := fs.Parse(args); err != nil {
		return err
	}
	d, err := datadir.Inspect(*dataDir)
	if err != nil {
		return err
	}
	cu, ok, err := configstore.File{Path: networkConfigPath(d)}.Load()
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(e.stdout, "no network configuration received from the Control Plane yet; testing a direct connection")
		cu.Network.Default = protocol.NetworkProfile{Enforcement: "off", DNS: "system", DiagnosticURL: "https://api64.ipify.org"}
	}
	r := (&httpprobe.Prober{}).Probe(ctx, cu.Network.Default, cu.Network.Endpoints)
	printNetworkResult(e.stdout, cu.Network.Default, r)
	if exe, err := os.Executable(); err == nil {
		printStrictEgress(ctx, e.stdout, strictEgress(d, exe, slog.New(slog.DiscardHandler)))
	}
	if !r.OK {
		return fmt.Errorf("network test failed: %s", r.Code)
	}
	return nil
}

func printNetworkResult(w io.Writer, p protocol.NetworkProfile, r protocol.NetworkTestResult) {
	proxy := "direct"
	if p.Proxy != nil {
		proxy = fmt.Sprintf("%s://%s:%d", p.Proxy.Scheme, p.Proxy.Host, p.Proxy.Port) // never the credentials
	}
	fmt.Fprintf(w, "profile:     %s (enforcement %s, proxy %s)\n", p.Name, p.Enforcement, proxy)
	fmt.Fprintf(w, "egress IPv4: %s (expected %s)\n", orDash(r.ObservedIPv4), orDash(p.ExpectedIPv4))
	fmt.Fprintf(w, "egress IPv6: %s (expected %s)\n", orDash(r.ObservedIPv6), orDash(p.ExpectedIPv6))
	for _, c := range r.Checks {
		state := "ok"
		if !c.OK {
			state = "FAILED " + c.Code
		}
		fmt.Fprintf(w, "  %-12s %-32s %s (%d ms)\n", c.Name, c.Host, state, c.LatencyMs)
	}
	if r.OK {
		fmt.Fprintln(w, "result:      ok")
	} else {
		fmt.Fprintf(w, "result:      FAILED %s %s\n", r.Code, r.Detail)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// strictEgress wires strict egress (KIN-31): sandboxes are built by re-executing this binary as
// `ai-runner sandbox-exec` in new namespaces, with the profiles the Control Plane delivered.
func strictEgress(d *datadir.Dir, exe string, logger *slog.Logger) *egress.Sandboxes {
	return &egress.Sandboxes{
		Config: configstore.File{Path: networkConfigPath(d)}.Load,
		Boxes: &nsbox.Manager{Program: exe, Args: []string{"sandbox-exec"}, DataDir: d.Path(), Logger: logger,
			InUse: journalledSandboxes(journal.Dir{Path: attemptsDir(d.Path())})},
		Logger: logger,
	}
}

// journalledSandboxes lists the adapter sockets of the attempts in the journal j: a sweep keeps their directories,
// so a restarted runner can reattach them (KIN-100), also when `ai-runner network` runs its self-test meanwhile.
func journalledSandboxes(j journal.Dir) func() ([]string, error) {
	return func() ([]string, error) {
		recs, err := j.List()
		if err != nil {
			return nil, err
		}
		var socks []string
		for _, r := range recs {
			if r.Sandbox != nil && r.Sandbox.Endpoint != "" {
				socks = append(socks, r.Sandbox.Endpoint)
			}
		}
		return socks, nil
	}
}

// confiner gives the session context the egress context's sandboxes. sessions, when set, returns the CLI session
// store of an AI CLI attempt's task, bound into the agent's HOME so a later attempt can resume (KIN-86).
type confiner struct {
	s        *egress.Sandboxes
	sessions func(sessiondomain.Assignment) ([]egress.HomeBind, error)
}

var _ sessionapp.Confiner = confiner{}

func (c confiner) Confine(ctx context.Context, a sessiondomain.Assignment, dir, workdir string) (sessionapp.Sandbox, error) {
	r := egress.BoxRequest{Dir: dir, Workdir: workdir}
	if c.sessions != nil && a.CLI() {
		// Only a sandboxed run uses the store (on the host the CLI keeps its sessions in its own home).
		if _, strict, err := c.s.StrictProfile(ctx, a.Project, a.Adapter); err == nil && strict {
			binds, err := c.sessions(a)
			if err != nil {
				return nil, err
			}
			r.HomeBinds = binds
		}
	}
	b, err := c.s.Confine(ctx, a.Project, a.Adapter, r)
	if err != nil || b == nil {
		return nil, err
	}
	sb, ok := b.(sessionapp.Sandbox)
	if !ok {
		b.Release()
		return nil, errors.New("the sandbox cannot launch processes")
	}
	return sb, nil
}

// Reattach serves the sandbox of an attempt adopted after a restart again (KIN-81), with the profile of its project
// and CLI role.
func (c confiner) Reattach(ctx context.Context, rec sessiondomain.Record, dir string) (sessionapp.Sandbox, error) {
	if rec.Sandbox == nil {
		return nil, errors.New("the attempt has no sandbox")
	}
	b, err := c.s.Reattach(ctx, rec.Project, rec.Sandbox.Role, dir, rec.Sandbox.Endpoint, rec.Sandbox.Profile)
	if err != nil {
		return nil, err
	}
	sb, ok := b.(sessionapp.Sandbox)
	if !ok {
		b.Release()
		return nil, errors.New("the sandbox cannot be monitored")
	}
	return sb, nil
}

// printStrictEgress reports whether this machine can enforce strict egress, and why not.
func printStrictEgress(ctx context.Context, w io.Writer, s *egress.Sandboxes) {
	if ok, why := s.Supported(ctx); ok {
		fmt.Fprintln(w, "strict egress: available (the sandbox self-test verified)")
	} else {
		fmt.Fprintf(w, "strict egress: unavailable: %s\n", why)
	}
}
