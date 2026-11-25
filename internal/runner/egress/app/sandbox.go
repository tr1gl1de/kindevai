package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/egress/domain"
)

// Strict egress (KIN-31, KB KIN-A-22): an attempt whose effective profile is strict runs in a sandbox built by
// Boxes, or not at all.

// CLI roles a scoped profile may name (KIN-30).
var roles = []string{"claude", "codex", "zcode"}

// ProfileFor picks the profile that applies to a step of project run by the CLI role: the scoped entry for
// (project, role), then (project, ""), then ("", role), else the default.
func ProfileFor(c protocol.NetworkConfig, project, role string) protocol.NetworkProfile {
	if !slices.Contains(roles, role) {
		role = ""
	}
	find := func(p, r string) (protocol.NetworkProfile, bool) {
		for _, s := range c.Scoped {
			if s.Project == p && s.Role == r {
				return s.Profile, true
			}
		}
		return protocol.NetworkProfile{}, false
	}
	var keys [][2]string
	if project != "" && role != "" {
		keys = append(keys, [2]string{project, role})
	}
	if project != "" {
		keys = append(keys, [2]string{project, ""})
	}
	if role != "" {
		keys = append(keys, [2]string{"", role})
	}
	for _, k := range keys {
		if p, ok := find(k[0], k[1]); ok {
			return p
		}
	}
	return c.Default
}

// BoxRequest is what a sandbox is built for.
type BoxRequest struct {
	Dir     string // the attempt's state directory (owner-only; holds the adapter's socket)
	Workdir string // the attempt's worktree ("" = Dir)
	Profile protocol.NetworkProfile
	// HomeBinds are host directories bound read-write below the agent's private HOME (KIN-86: the CLI's session
	// store of the task, so a later attempt can resume the session); the host's own CLI login stays a copy.
	HomeBinds []HomeBind
}

// HomeBind binds the host directory Source at Rel (a clean relative path) below the agent's HOME.
type HomeBind struct {
	Source string
	Rel    string
}

// Box is one attempt's sandbox, as the session context launches through it.
type Box interface {
	Release()
}

// Boxes builds sandboxes (the namespace adapter) and says whether this host can.
type Boxes interface {
	Open(ctx context.Context, r BoxRequest) (Box, error)
	// Support runs the platform checks and the self-test sandbox: ok, or why not.
	Support(ctx context.Context) (bool, string)
}

// ErrUnsupported: the profile is strict and this runner cannot enforce it (fail-closed: the attempt does not run).
var ErrUnsupported = errors.New(domain.CodeUnsupported)

// Sandboxes decides per attempt whether to confine it and reports the strict_egress capability.
type Sandboxes struct {
	Config func() (protocol.ConfigUpdate, bool, error)
	Boxes  Boxes
	Logger *slog.Logger
	// MaxAge is how long a support verdict is reused (default 10 minutes).
	MaxAge time.Duration
	Now    func() time.Time

	mu      sync.Mutex
	checked time.Time
	ok      bool
	why     string
}

func (s *Sandboxes) logger() *slog.Logger {
	if s.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Logger
}

func (s *Sandboxes) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

// Supported reports whether strict egress can be enforced here (the runner.capabilities strictEgress flag): only
// after the self-test sandbox verified. A verdict is reused for MaxAge.
func (s *Sandboxes) Supported(ctx context.Context) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	maxAge := s.MaxAge
	if maxAge <= 0 {
		maxAge = 10 * time.Minute
	}
	if s.checked.IsZero() || s.now().Sub(s.checked) >= maxAge {
		s.ok, s.why = s.Boxes.Support(ctx)
		s.checked = s.now()
		if s.ok {
			s.logger().Info("strict egress available: the self-test sandbox verified")
		} else {
			s.logger().Warn("strict egress unavailable; steps with a strict network profile will not run here", "reason", s.why)
		}
	}
	return s.ok, s.why
}

// StrictProfile returns the profile of a run of project by role and whether it is strict. An error means the run
// must not start (strict but unsupported here, or no configuration to prove it is not strict: fail-closed).
func (s *Sandboxes) StrictProfile(ctx context.Context, project, role string) (protocol.NetworkProfile, bool, error) {
	cu, ok, err := s.Config()
	if err != nil {
		return protocol.NetworkProfile{}, false, fmt.Errorf("reading the network configuration: %w", err)
	}
	if !ok {
		// The Control Plane sends the configuration when the session opens, before any assignment: without it the
		// runner cannot prove the profile is not strict (fail-closed).
		return protocol.NetworkProfile{}, false, errors.New("no network configuration received from the Control Plane yet")
	}
	p := ProfileFor(cu.Network, project, role)
	if p.Enforcement != domain.EnforcementStrict {
		return p, false, nil
	}
	if ok, why := s.Supported(ctx); !ok {
		return p, true, fmt.Errorf("%w: %s", ErrUnsupported, why)
	}
	return p, true, nil
}

// Confine returns the sandbox for an attempt of project run by role (r without its profile), or nil when its
// profile is not strict. An error means the attempt must not run (strict, but no verified sandbox): the step waits
// for the network.
func (s *Sandboxes) Confine(ctx context.Context, project, role string, r BoxRequest) (Box, error) {
	p, strict, err := s.StrictProfile(ctx, project, role)
	if err != nil || !strict {
		return nil, err
	}
	r.Profile = p
	b, err := s.Boxes.Open(ctx, r)
	if err != nil {
		return nil, fmt.Errorf("building the strict egress sandbox: %w", err)
	}
	return b, nil
}
