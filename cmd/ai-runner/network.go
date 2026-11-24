package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/app"
	"github.com/tr1gl1de/kindevai/internal/runner/egress/adapters/configstore"
	"github.com/tr1gl1de/kindevai/internal/runner/egress/adapters/httpprobe"
	egress "github.com/tr1gl1de/kindevai/internal/runner/egress/app"
	"github.com/tr1gl1de/kindevai/internal/runner/platform/datadir"
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
