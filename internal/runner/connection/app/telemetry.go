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

// Reporter sends the runner's telemetry over an authorized link: capabilities and storage right after
// authorization, then a heartbeat every HeartbeatEvery and storage every StorageEvery.
type Reporter struct {
	Probe          Prober
	HeartbeatEvery time.Duration // default protocol.HeartbeatIntervalSeconds
	StorageEvery   time.Duration // default protocol.StorageIntervalSeconds
	Now            func() time.Time
}

// Report runs until ctx ends (nil) or a send fails (the link is gone; Wait reports why).
func (r *Reporter) Report(ctx context.Context, s Sender) error {
	hbEvery, stEvery, now := r.HeartbeatEvery, r.StorageEvery, r.Now
	if hbEvery <= 0 {
		hbEvery = protocol.HeartbeatIntervalSeconds * time.Second
	}
	if stEvery <= 0 {
		stEvery = protocol.StorageIntervalSeconds * time.Second
	}
	if now == nil {
		now = time.Now
	}
	send := func(typ string, payload any) error {
		if err := s.Send(ctx, typ, payload); err != nil && ctx.Err() == nil {
			return err
		}
		return nil
	}
	heartbeat := func() error {
		return send(protocol.TypeHeartbeat, protocol.Heartbeat{SentAt: now().UTC().Format(time.RFC3339)})
	}
	if err := send(protocol.TypeCapabilities, r.Probe.Capabilities(ctx)); err != nil {
		return err
	}
	if err := send(protocol.TypeStorage, r.Probe.Storage(ctx)); err != nil {
		return err
	}
	if err := heartbeat(); err != nil {
		return err
	}
	hb, st := time.NewTicker(hbEvery), time.NewTicker(stEvery)
	defer hb.Stop()
	defer st.Stop()
	for {
		var err error
		select {
		case <-ctx.Done():
			return nil
		case <-hb.C:
			err = heartbeat()
		case <-st.C:
			err = send(protocol.TypeStorage, r.Probe.Storage(ctx))
		}
		if err != nil {
			return err
		}
	}
}
