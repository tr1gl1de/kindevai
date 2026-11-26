package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	integrationsapp "github.com/tr1gl1de/kindevai/internal/integrations/app"
	integrations "github.com/tr1gl1de/kindevai/internal/integrations/domain"
	networkhttp "github.com/tr1gl1de/kindevai/internal/network/adapters/httpapi"
	networkdb "github.com/tr1gl1de/kindevai/internal/network/adapters/sqlite"
	networkapp "github.com/tr1gl1de/kindevai/internal/network/app"
	network "github.com/tr1gl1de/kindevai/internal/network/domain"
	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	projectsdb "github.com/tr1gl1de/kindevai/internal/projects/adapters/sqlite"
	projectsapp "github.com/tr1gl1de/kindevai/internal/projects/app"
	projects "github.com/tr1gl1de/kindevai/internal/projects/domain"
	"github.com/tr1gl1de/kindevai/internal/protocol"
	runnersgw "github.com/tr1gl1de/kindevai/internal/runners/adapters/gateway"
	runnersdb "github.com/tr1gl1de/kindevai/internal/runners/adapters/sqlite"
	runnersapp "github.com/tr1gl1de/kindevai/internal/runners/app"
	runners "github.com/tr1gl1de/kindevai/internal/runners/domain"
	"github.com/tr1gl1de/kindevai/internal/server"
)

// newNetwork wires the network context (KIN-30): profiles with proxy passwords in the integrations vault,
// configuration delivered over the runner command channel, Test Network results from runner sessions.
func newNetwork(inst *bootstrap.Installation, audit *auditModule, integ *integrationsModule, ch *runnersapp.Channel,
	logger *slog.Logger) (*networkapp.Service, server.Module) {
	svc := &networkapp.Service{
		Repo:     networkdb.New(inst.DB),
		Secrets:  networkSecrets{integ.Service.Vault},
		Cipher:   inst.Box,
		Runners:  networkRunners{runnersdb.New(inst.DB)},
		Projects: networkProjects{&projectsapp.Service{Repo: projectsdb.New(inst.DB)}},
		Commands: ch,
		Tx:       db.Transactor{DB: inst.DB},
		Audit:    audit.Service,
		YouTrackURL: func(ctx context.Context) string {
			s, err := integ.Service.YouTrackSettings(ctx)
			if err != nil {
				return ""
			}
			return s.BaseURL
		},
		Now: time.Now,
		// Lower-case: the id is part of the vault slot name "network_profile.<id>.password".
		NewID:  func() string { return strings.ToLower(rand.Text()) },
		Logger: logger,
	}
	ch.Prepare = func(ctx context.Context, runnerID string, q runners.QueuedCommand) (json.RawMessage, error) {
		return svc.PrepareCommand(ctx, runnerID, q.ID, q.Kind, q.Body)
	}
	return svc, &networkhttp.Handler{Service: svc, Logger: logger}
}

// attachNetwork accepts Test Network results on runner sessions and re-delivers the configuration when a session
// opens (a runner approved after the last change has none yet).
func attachNetwork(h *runnersgw.Handler, svc *networkapp.Service, logger *slog.Logger) {
	if h.Messages == nil {
		h.Messages = map[string]runnersgw.MessageFunc{}
	}
	h.Messages[protocol.TypeNetworkTestResult] = func(ctx context.Context, s runnersapp.Session, env protocol.Envelope) error {
		var r protocol.NetworkTestResult
		if err := json.Unmarshal(env.Payload, &r); err != nil || r.TestID == "" || len(r.TestID) > protocol.MaxIDLen {
			return fmt.Errorf("%w: %s", protocol.ErrChannelField, env.Type)
		}
		if err := svc.RecordResult(ctx, s.RunnerID, r); err != nil {
			logger.Error("recording network test result", "runner", s.RunnerID, "err", err)
		}
		return nil
	}
	h.Sessions = append(h.Sessions, func(ctx context.Context, s runnersapp.Session) {
		if err := svc.PushConfig(ctx, s.RunnerID); err != nil && ctx.Err() == nil {
			logger.Error("queueing runner network configuration", "runner", s.RunnerID, "err", err)
		}
	})
}

// networkSecrets keeps proxy passwords in the integrations credentials vault (KIN-26).
type networkSecrets struct{ v *integrationsapp.Vault }

func (s networkSecrets) Put(ctx context.Context, name, value, by string) error {
	n, err := integrations.NewSecretName(name)
	if err != nil {
		return err
	}
	sec, err := integrations.NewSecret(value)
	if err != nil {
		return err
	}
	return s.v.Put(ctx, n, sec, by)
}

func (s networkSecrets) Get(ctx context.Context, name string) (string, bool, error) {
	sec, err := s.v.Credential(ctx, integrations.SecretName(name))
	if errors.Is(err, integrations.ErrSecretNotSet) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return sec.Reveal(), true, nil
}

func (s networkSecrets) IsSet(ctx context.Context, name string) (bool, error) {
	m, err := s.v.Meta(ctx, integrations.SecretName(name))
	return m.Set, err
}

func (s networkSecrets) Delete(ctx context.Context, name string) error {
	return s.v.Delete(ctx, integrations.SecretName(name))
}

// networkRunners gives the network context the runners it configures.
type networkRunners struct{ repo *runnersdb.Repository }

func runnerRef(r runners.Runner) networkapp.RunnerRef {
	return networkapp.RunnerRef{ID: r.ID, Name: r.Metadata.Hostname, Projects: r.ProjectIDs,
		Active: r.Status == runners.StatusApproved || r.Status == runners.StatusDisabled}
}

func (n networkRunners) Runner(ctx context.Context, id string) (networkapp.RunnerRef, error) {
	r, err := n.repo.Runner(ctx, id)
	if errors.Is(err, runners.ErrNotFound) {
		return networkapp.RunnerRef{}, network.ErrNotFound
	}
	if err != nil {
		return networkapp.RunnerRef{}, err
	}
	return runnerRef(r), nil
}

func (n networkRunners) Runners(ctx context.Context) ([]networkapp.RunnerRef, error) {
	rs, err := n.repo.Runners(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]networkapp.RunnerRef, 0, len(rs))
	for _, r := range rs {
		out = append(out, runnerRef(r))
	}
	return out, nil
}

// networkProjects checks project targets of assignments.
type networkProjects struct{ svc *projectsapp.Service }

func (p networkProjects) Exists(ctx context.Context, id string) error {
	_, err := p.svc.Lookup(ctx, id)
	if errors.Is(err, projects.ErrNotFound) {
		return network.ErrNotFound
	}
	return err
}
