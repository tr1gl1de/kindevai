package main

import (
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/tr1gl1de/kindevai/internal/platform/bootstrap"
	"github.com/tr1gl1de/kindevai/internal/platform/ratelimit"
	"github.com/tr1gl1de/kindevai/internal/protocol"
	runnersgw "github.com/tr1gl1de/kindevai/internal/runners/adapters/gateway"
	runnersdb "github.com/tr1gl1de/kindevai/internal/runners/adapters/sqlite"
	runnersapp "github.com/tr1gl1de/kindevai/internal/runners/app"
)

// newRunnerGateway wires the runner WebSocket endpoint (KIN-19). hub is shared with the runners service, so
// disable/revoke close live connections; plainPeers are extra "this host" peers for plain ws://.
func newRunnerGateway(inst *bootstrap.Installation, hub *runnersapp.Hub, plainPeers []netip.Prefix, audiences []string,
	logger *slog.Logger) *runnersgw.Handler {
	return &runnersgw.Handler{
		Gateway: &runnersapp.Gateway{Runners: runnersdb.New(inst.DB), Hub: hub, Rand: rand.Reader, Now: time.Now,
			NewID: rand.Text, Logger: logger},
		// Reconnects back off from 1s to 60s, so 30 attempts a minute per address leaves room for NAT-shared runners.
		Limiter: ratelimit.New(ratelimit.Config{Burst: 30, Every: 2 * time.Second, MaxKeys: 10_000}, time.Now),
		Config:  runnersgw.Config{PlainPeers: plainPeers, Audiences: audiences},
		Logger:  logger,
	}
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
