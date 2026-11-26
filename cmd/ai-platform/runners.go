package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/platform/db"
	"github.com/tr1gl1de/kindevai/internal/platform/diskusage"
	"github.com/tr1gl1de/kindevai/internal/platform/ratelimit"
	"github.com/tr1gl1de/kindevai/internal/protocol"
	runnersgw "github.com/tr1gl1de/kindevai/internal/runners/adapters/gateway"
	runnersdb "github.com/tr1gl1de/kindevai/internal/runners/adapters/sqlite"
	runnersapp "github.com/tr1gl1de/kindevai/internal/runners/app"
	runners "github.com/tr1gl1de/kindevai/internal/runners/domain"
)

// newRunnerGateway wires the runner WebSocket endpoint (KIN-19). hub is shared with the runners service, so
// disable/revoke close live connections; plainPeers are extra "this host" peers for plain ws://.
func newRunnerGateway(inst *bootstrap.Installation, hub *runnersapp.Hub, tel *runnersapp.Telemetry, plainPeers []netip.Prefix,
	audiences []string, logger *slog.Logger) *runnersgw.Handler {
	return &runnersgw.Handler{
		Gateway: &runnersapp.Gateway{Runners: runnersdb.New(inst.DB), Hub: hub, Rand: rand.Reader, Now: time.Now,
			NewID: rand.Text, Logger: logger},
		// Reconnects back off from 1s to 60s, so 30 attempts a minute per address leaves room for NAT-shared runners.
		Limiter:  ratelimit.New(ratelimit.Config{Burst: 30, Every: 2 * time.Second, MaxKeys: 10_000}, time.Now),
		Config:   runnersgw.Config{PlainPeers: plainPeers, Audiences: audiences},
		Messages: runnersgw.TelemetryMessages(tel, time.Now, logger), // KIN-20 heartbeat, capabilities, storage
		Logger:   logger,
	}
}

// newTelemetry wires runner liveness, capabilities and storage metrics (KIN-20); alerts go through the outbox.
func newTelemetry(inst *bootstrap.Installation, msg *messagingModule, logger *slog.Logger) *runnersapp.Telemetry {
	repo := runnersdb.New(inst.DB)
	return &runnersapp.Telemetry{Repo: repo, Runners: repo, Tx: db.Transactor{DB: inst.DB}, Outbox: msg.Outbox, Now: time.Now,
		Logger: logger}
}

// startTelemetry runs the unreachable sweep, the Control Plane's own storage metrics (its data directory) and the
// history purge until ctx ends; the returned stop waits for the loop.
func startTelemetry(ctx context.Context, tel *runnersapp.Telemetry, dataDir string) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tel.Run(ctx, func() ([]runners.Volume, error) {
			u, err := diskusage.Stat(dataDir)
			if err != nil {
				return nil, err
			}
			v, err := runners.NewVolume(u.MountPoint, runners.RoleData, dataDir, u.TotalBytes, u.AvailableBytes, u.FreeBytes, time.Now())
			if err != nil {
				return nil, err
			}
			return []runners.Volume{v}, nil
		})
	}()
	return func() { cancel(); <-done }
}

// parseAudiences turns --public-url into the runner audiences (normalized origins); empty means "from the request".
func parseAudiences(s string) ([]string, error) {
	var out []string
	for f := range strings.SplitSeq(s, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		u, err := url.Parse(f)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, fmt.Errorf("public-url %q: want an absolute http(s) origin", f)
		}
		out = append(out, protocol.Audience(u.Scheme == "https", u.Host))
	}
	return out, nil
}
