package main

import (
	"crypto/rand"
	"log/slog"
	"net/netip"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/platform/ratelimit"
	runnersgw "github.com/tr1gl1de/kindevai/internal/runners/adapters/gateway"
	runnersdb "github.com/tr1gl1de/kindevai/internal/runners/adapters/sqlite"
	runnersapp "github.com/tr1gl1de/kindevai/internal/runners/app"
	"github.com/tr1gl1de/kindevai/internal/server"
)

// newRunnerGateway wires the runner WebSocket endpoint (KIN-19). hub is shared with the runners service, so
// disable/revoke close live connections; plainPeers are extra "this host" peers for plain ws://.
func newRunnerGateway(inst *bootstrap.Installation, hub *runnersapp.Hub, plainPeers []netip.Prefix, logger *slog.Logger) server.Module {
	return &runnersgw.Handler{
		Gateway: &runnersapp.Gateway{Runners: runnersdb.New(inst.DB), Hub: hub, Rand: rand.Reader, Now: time.Now,
			NewID: rand.Text, Logger: logger},
		// Reconnects back off from 1s to 60s, so 30 attempts a minute per address leaves room for NAT-shared runners.
		Limiter: ratelimit.New(ratelimit.Config{Burst: 30, Every: 2 * time.Second, MaxKeys: 10_000}, time.Now),
		Config:  runnersgw.Config{PlainPeers: plainPeers},
		Logger:  logger,
	}
}
