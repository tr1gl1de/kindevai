// Package app is the runner's egress context (KIN-30, KB KIN-A-22): it applies the network configuration the
// Control Plane delivers with runner.config.update (kept in a private file, never logged with its passwords) and
// runs the Test Network preflight for network.test, reporting runner.network.test.result. Confining a CLI's
// traffic to the profile (namespaces, nftables) is KIN-31's; this package provides the profiles it will enforce.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
)

// Prober runs the preflight of a profile: proxy connectivity, the observed egress IP via the profile's diagnostic
// endpoint and the reachability of each endpoint through the proxy. Failures are reported as stable codes, never
// error texts (which could echo a proxy URL).
type Prober interface {
	Probe(ctx context.Context, p protocol.NetworkProfile, eps []protocol.NetworkEndpoint) protocol.NetworkTestResult
}

// Store keeps the applied configuration (owner-only file: it holds proxy passwords).
type Store interface {
	Load() (protocol.ConfigUpdate, bool, error)
	Save(protocol.ConfigUpdate) error
}

// Sender sends one message on the authorized link.
type Sender interface {
	Send(ctx context.Context, typ string, payload any) error
}

// Agent handles the network commands and reports test results on the live link.
type Agent struct {
	Store   Store
	Prober  Prober
	Logger  *slog.Logger
	Timeout time.Duration // per test; default 90s
	// Context bounds background tests (the daemon's lifetime); nil: each test is bounded by Timeout only.
	Context context.Context

	once    sync.Once
	mu      sync.Mutex
	busy    bool
	results chan protocol.NetworkTestResult
}

// MaxQueuedResults bounds results waiting for a link; older ones are dropped (the Control Plane shows them as
// timed out and the Owner repeats the test).
const MaxQueuedResults = 8

func (a *Agent) init() {
	a.once.Do(func() {
		a.results = make(chan protocol.NetworkTestResult, MaxQueuedResults)
		if a.Logger == nil {
			a.Logger = slog.New(slog.DiscardHandler)
		}
		if a.Timeout <= 0 {
			a.Timeout = 90 * time.Second
		}
	})
}

// ErrNotNetworkCommand: the command is not one of this context's kinds.
var ErrNotNetworkCommand = errors.New("not a network command")

// Command applies a network command. It returns ErrNotNetworkCommand for other kinds; a malformed body is logged
// and acknowledged (re-sending it would not help).
func (a *Agent) Command(ctx context.Context, cmd protocol.Command) error {
	a.init()
	switch cmd.Kind {
	case protocol.CommandConfigUpdate:
		var cu protocol.ConfigUpdate
		if err := json.Unmarshal(cmd.Body, &cu); err != nil {
			a.Logger.Warn("malformed network configuration ignored", "command", cmd.ID)
			return nil
		}
		return a.apply(cu)
	case protocol.CommandNetworkTest:
		var nt protocol.NetworkTest
		if err := json.Unmarshal(cmd.Body, &nt); err != nil || nt.TestID == "" {
			a.Logger.Warn("malformed network test ignored", "command", cmd.ID)
			return nil
		}
		a.test(ctx, nt)
		return nil
	}
	return ErrNotNetworkCommand
}

// apply stores every update: commands arrive in the Control Plane's commit order (KIN-22), so the last one is the
// current configuration even when the revision went down (a restored Control Plane database).
func (a *Agent) apply(cu protocol.ConfigUpdate) error {
	sealed := cu.Network.Default.Proxy != nil && cu.Network.Default.Proxy.PasswordSealed != nil
	for _, sc := range cu.Network.Scoped {
		sealed = sealed || (sc.Profile.Proxy != nil && sc.Profile.Proxy.PasswordSealed != nil)
	}
	if sealed {
		a.Logger.Warn("the Control Plane could not deliver a proxy password; proxy authentication will fail until the profile is saved again",
			"revision", cu.Revision)
	}
	if err := a.Store.Save(cu); err != nil {
		return fmt.Errorf("saving network configuration: %w", err) // unacknowledged: re-sent later
	}
	a.Logger.Info("network configuration applied", "revision", cu.Revision, "enforcement", cu.Network.Default.Enforcement,
		"proxy", cu.Network.Default.Proxy != nil, "scoped", len(cu.Network.Scoped))
	return nil
}

// test runs one preflight at a time in the background (the command stream must not wait for the network); a test
// arriving while another runs is answered "busy".
func (a *Agent) test(ctx context.Context, nt protocol.NetworkTest) {
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		a.report(protocol.NetworkTestResult{TestID: nt.TestID, Code: "busy"})
		return
	}
	a.busy = true
	a.mu.Unlock()
	go func() {
		defer func() { a.mu.Lock(); a.busy = false; a.mu.Unlock() }()
		// Bounded by Timeout and the daemon, not by the link: a result produced after a reconnect is still delivered.
		base := a.Context
		if base == nil {
			base = context.WithoutCancel(ctx)
		}
		tctx, cancel := context.WithTimeout(base, a.Timeout)
		defer cancel()
		start := time.Now()
		r := a.Prober.Probe(tctx, nt.Profile, nt.Endpoints)
		r.TestID = nt.TestID
		r.DurationMs = time.Since(start).Milliseconds()
		a.report(r)
	}()
}

func (a *Agent) report(r protocol.NetworkTestResult) {
	for {
		select {
		case a.results <- r:
			return
		default:
		}
		select { // full: drop the oldest
		case <-a.results:
		default:
		}
	}
}

// Serve sends test results while the link is up.
func (a *Agent) Serve(ctx context.Context, s Sender) error {
	a.init()
	for {
		select {
		case <-ctx.Done():
			return nil
		case r := <-a.results:
			if err := s.Send(ctx, protocol.TypeNetworkTestResult, r); err != nil {
				a.report(r) // keep it for the next link
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}

// Config returns the applied configuration (for the local `ai-runner test-network`).
func (a *Agent) Config() (protocol.ConfigUpdate, bool, error) { return a.Store.Load() }
