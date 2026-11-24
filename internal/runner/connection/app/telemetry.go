package app

import (
	"context"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
)

// Sender sends one message on an authorized link (KIN-20 telemetry). It is safe to call concurrently with Wait.
type Sender interface {
	Send(ctx context.Context, typ string, payload any) error
}

// Prober describes this machine: capabilities (OS/arch, CLIs found and their versions) and storage metrics.
type Prober interface {
	Capabilities(ctx context.Context) protocol.Capabilities
	Storage(ctx context.Context) protocol.Storage
}

// Reporter sends the runner's telemetry over an authorized link: a heartbeat at once and every HeartbeatEvery;
// capabilities, then storage at once and every StorageEvery. Probes run apart from the heartbeat, so a slow or
// hung probe (a stale /mnt/c, a CLI that does not answer --version) never delays heartbeats or blocks stopping.
type Reporter struct {
	Probe          Prober
	HeartbeatEvery time.Duration // default protocol.HeartbeatIntervalSeconds
	StorageEvery   time.Duration // default protocol.StorageIntervalSeconds
	Now            func() time.Time
}

// Report runs until ctx ends (nil) or a send fails (the link is gone; Wait reports why). It returns promptly on
// cancellation even while a probe is still running.
func (r *Reporter) Report(ctx context.Context, s Sender) error {
	hbEvery, now := r.HeartbeatEvery, r.Now
	if hbEvery <= 0 {
		hbEvery = protocol.HeartbeatIntervalSeconds * time.Second
	}
	if now == nil {
		now = time.Now
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	beat := func() error {
		if err := s.Send(ctx, protocol.TypeHeartbeat, protocol.Heartbeat{SentAt: now().UTC().Format(time.RFC3339)}); err != nil && ctx.Err() == nil {
			return err
		}
		return nil
	}
	if err := beat(); err != nil { // liveness first, before any probe
		return err
	}
	probeErr := make(chan error, 1)
	go func() { probeErr <- r.probes(ctx, s) }()
	hb := time.NewTicker(hbEvery)
	defer hb.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-probeErr:
			if err != nil {
				return err
			}
			probeErr = nil // probes stopped cleanly (ctx ended); keep beating until it is noticed
		case <-hb.C:
			if err := beat(); err != nil {
				return err
			}
		}
	}
}

// probes sends capabilities and storage; each probe runs in its own goroutine so cancellation is never blocked.
func (r *Reporter) probes(ctx context.Context, s Sender) error {
	stEvery := r.StorageEvery
	if stEvery <= 0 {
		stEvery = protocol.StorageIntervalSeconds * time.Second
	}
	send := func(typ string, probe func() any) error {
		res := make(chan any, 1)
		go func() { res <- probe() }()
		select {
		case <-ctx.Done():
			return nil
		case payload := <-res:
			if err := s.Send(ctx, typ, payload); err != nil && ctx.Err() == nil {
				return err
			}
			return nil
		}
	}
	if err := send(protocol.TypeCapabilities, func() any { return r.Probe.Capabilities(ctx) }); err != nil {
		return err
	}
	st := time.NewTicker(stEvery)
	defer st.Stop()
	for {
		if err := send(protocol.TypeStorage, func() any { return r.Probe.Storage(ctx) }); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-st.C:
		}
	}
}
