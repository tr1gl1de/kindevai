package app

import (
	"context"
	"sync"
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
	// Meter measures space consumers (KIN-55); nil unless the operator allowed it (--measure-space). It runs at
	// most every MeterEvery (default 6 h) across reconnects, and its result rides on the next runner.storage.
	Meter      func(ctx context.Context) protocol.SpaceConsumers
	MeterEvery time.Duration

	mu      sync.Mutex
	nextAt  time.Time // when the meter may run next; zero: at once
	pending *protocol.SpaceConsumers
}

// MeterRetry is the wait after a measurement cut short by the link going down, so a flapping link does not
// restart a full scan on every reconnect.
const MeterRetry = 30 * time.Minute

// DefaultMeterEvery is how often space consumers are measured when allowed.
const DefaultMeterEvery = 6 * time.Hour

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

var _ LinkHandler = (*Reporter)(nil)

// Serve runs Report on every authorized link (LinkHandler).
func (r *Reporter) Serve(ctx context.Context, c Conn) error { return r.Report(ctx, c) }

// Handle takes no Control Plane messages: telemetry is runner → server only.
func (r *Reporter) Handle(context.Context, Conn, protocol.Envelope) (bool, error) { return false, nil }

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
	measured := make(chan struct{}, 1)
	if r.Meter != nil {
		go r.meter(ctx, measured)
	}
	for {
		c := r.take()
		err := send(protocol.TypeStorage, func() any { s := r.Probe.Storage(ctx); s.Consumers = c; return s })
		if err != nil || ctx.Err() != nil {
			r.putBack(c) // not delivered: the next link sends it
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-st.C:
		case <-measured:
		}
	}
}

// meter measures when due (MeterEvery after the last complete measurement, MeterRetry after an interrupted one)
// and signals measured.
func (r *Reporter) meter(ctx context.Context, measured chan<- struct{}) {
	every := r.MeterEvery
	if every <= 0 {
		every = DefaultMeterEvery
	}
	now := r.Now
	if now == nil {
		now = time.Now
	}
	for {
		r.mu.Lock()
		wait := time.Duration(0)
		if !r.nextAt.IsZero() {
			wait = r.nextAt.Sub(now())
		}
		r.mu.Unlock()
		if wait > 0 {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
		r.mu.Lock()
		r.nextAt = now().Add(MeterRetry)
		r.mu.Unlock()
		c := r.Meter(ctx)
		if ctx.Err() != nil {
			return // cut short by the link going down: retried after MeterRetry
		}
		r.mu.Lock()
		r.nextAt, r.pending = now().Add(every), &c
		r.mu.Unlock()
		select {
		case measured <- struct{}{}:
		default:
		}
	}
}

// take returns the measurement waiting to be sent, if any.
func (r *Reporter) take() *protocol.SpaceConsumers {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.pending
	r.pending = nil
	return c
}

// putBack keeps an undelivered measurement unless a newer one arrived meanwhile.
func (r *Reporter) putBack(c *protocol.SpaceConsumers) {
	if c == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil {
		r.pending = c
	}
}
