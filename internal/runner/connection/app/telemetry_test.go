package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/connection/domain"
)

type recSender struct {
	mu    sync.Mutex
	types []string
	fail  error
}

func (s *recSender) Send(_ context.Context, typ string, _ any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.types = append(s.types, typ)
	return nil
}

func (s *recSender) count(typ string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, t := range s.types {
		if t == typ {
			n++
		}
	}
	return n
}

type fakeProbe struct{ storageCalls atomic.Int64 }

func (p *fakeProbe) Capabilities(context.Context) protocol.Capabilities {
	return protocol.Capabilities{OS: "linux", Arch: "amd64"}
}

func (p *fakeProbe) Storage(context.Context) protocol.Storage {
	p.storageCalls.Add(1)
	return protocol.Storage{Volumes: []protocol.Volume{{ID: "/", Role: protocol.VolumeWorkspaces, TotalBytes: 10}}}
}

func TestReporterSendsCapabilitiesStorageThenPeriodicHeartbeats(t *testing.T) {
	t.Parallel()
	s := &recSender{}
	r := &Reporter{Probe: &fakeProbe{}, HeartbeatEvery: time.Millisecond, StorageEvery: time.Hour}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Report(ctx, s) }()
	deadline := time.Now().Add(5 * time.Second)
	for s.count(protocol.TypeHeartbeat) < 3 || s.count(protocol.TypeStorage) < 1 { // probes run apart from heartbeats
		if time.Now().After(deadline) {
			t.Fatal("no periodic heartbeats")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Report after cancel = %v", err)
	}
	s.mu.Lock()
	first := s.types[0]
	s.mu.Unlock()
	if first != protocol.TypeHeartbeat {
		t.Fatalf("first message = %v, want a heartbeat before any probe", first)
	}
	if s.count(protocol.TypeStorage) != 1 || s.count(protocol.TypeCapabilities) != 1 {
		t.Fatalf("messages = %v", s.types)
	}
}

// A hung probe (e.g. a stale /mnt/c) neither delays heartbeats nor blocks stopping.
type hungProbe struct{ release chan struct{} }

func (p hungProbe) Capabilities(context.Context) protocol.Capabilities {
	<-p.release
	return protocol.Capabilities{}
}

func (p hungProbe) Storage(context.Context) protocol.Storage { return protocol.Storage{} }

func TestHungProbeDoesNotStopHeartbeatsOrShutdown(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	defer close(release)
	s := &recSender{}
	r := &Reporter{Probe: hungProbe{release: release}, HeartbeatEvery: time.Millisecond}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Report(ctx, s) }()
	deadline := time.Now().Add(5 * time.Second)
	for s.count(protocol.TypeHeartbeat) < 3 {
		if time.Now().After(deadline) {
			t.Fatal("heartbeats stopped behind a hung probe")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Report did not return while a probe hung")
	}
}

func TestReporterStopsOnSendError(t *testing.T) {
	t.Parallel()
	boom := errors.New("closed")
	r := &Reporter{Probe: &fakeProbe{}}
	if err := r.Report(t.Context(), &recSender{fail: boom}); !errors.Is(err, boom) {
		t.Fatalf("Report = %v", err)
	}
}

type sendingLink struct {
	recSender
	wait chan struct{}
}

func (l *sendingLink) SetHandler(func(context.Context, protocol.Envelope) error) {}

func (l *sendingLink) Wait(ctx context.Context) error {
	select {
	case <-l.wait:
	case <-ctx.Done():
	}
	return errors.New("closed")
}

// linkDialer returns link once, then ends the run.
type linkDialer struct {
	link   *sendingLink
	cancel context.CancelFunc
	used   *bool
}

func (d linkDialer) Dial(ctx context.Context, _ domain.ServerURL, _ func(Challenge) (Answer, error)) (Link, error) {
	if !*d.used {
		*d.used = true
		return d.link, nil
	}
	d.cancel()
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestConnectorRunsTheReporterWhileTheLinkIsUp(t *testing.T) {
	t.Parallel()
	link := &sendingLink{wait: make(chan struct{})}
	f, ctx := newConnFixture(t)
	f.c.Dialer = linkDialer{link: link, cancel: f.d.cancel, used: new(bool)}
	f.c.Handlers = []LinkHandler{&Reporter{Probe: &fakeProbe{}, HeartbeatEvery: time.Hour, StorageEvery: time.Hour}}
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for (link.count(protocol.TypeHeartbeat) < 1 || link.count(protocol.TypeCapabilities) < 1) && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		close(link.wait)
	}()
	if err := f.c.Run(ctx, f.e, f.id); err != nil {
		t.Fatal(err)
	}
	if link.count(protocol.TypeCapabilities) != 1 || link.count(protocol.TypeHeartbeat) != 1 {
		t.Fatalf("sent %v", link.types)
	}
}

// storageSender records the runner.storage payloads.
type storageSender struct {
	mu  sync.Mutex
	got []protocol.Storage
}

func (s *storageSender) Send(_ context.Context, typ string, p any) error {
	if typ == protocol.TypeStorage {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.got = append(s.got, p.(protocol.Storage))
	}
	return nil
}

func (s *storageSender) withConsumers() []protocol.SpaceConsumers {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []protocol.SpaceConsumers
	for _, x := range s.got {
		if x.Consumers != nil {
			out = append(out, *x.Consumers)
		}
	}
	return out
}

func TestReporterSendsSpaceConsumersOnlyWhenAllowedAndNotOnEveryLink(t *testing.T) {
	t.Parallel()
	waitUntil := func(cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatal("timed out")
			}
			time.Sleep(time.Millisecond)
		}
	}
	link := func(r *Reporter, s *storageSender, until func() bool) {
		t.Helper()
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- r.Report(ctx, s) }()
		waitUntil(until)
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}

	// Not allowed: storage reports never carry consumers.
	off := &storageSender{}
	link(&Reporter{Probe: &fakeProbe{}, StorageEvery: time.Millisecond}, off,
		func() bool { off.mu.Lock(); defer off.mu.Unlock(); return len(off.got) >= 3 })
	if len(off.withConsumers()) != 0 {
		t.Fatal("consumers sent without permission")
	}

	// Allowed: measured once, sent with the next storage report right after the measurement.
	var mu sync.Mutex
	measured := 0
	r := &Reporter{Probe: &fakeProbe{}, StorageEvery: time.Hour, MeterEvery: time.Hour,
		Meter: func(context.Context) protocol.SpaceConsumers {
			mu.Lock()
			defer mu.Unlock()
			measured++
			return protocol.SpaceConsumers{Items: []protocol.SpaceConsumer{{Category: protocol.ConsumerSpool, Bytes: 7}}}
		}}
	on := &storageSender{}
	link(r, on, func() bool { return len(on.withConsumers()) == 1 })
	if c := on.withConsumers()[0]; c.Items[0].Bytes != 7 {
		t.Fatalf("consumers = %+v", c)
	}
	// A reconnect within MeterEvery does not measure again.
	again := &storageSender{}
	link(r, again, func() bool { again.mu.Lock(); defer again.mu.Unlock(); return len(again.got) >= 1 })
	mu.Lock()
	defer mu.Unlock()
	if measured != 1 || len(again.withConsumers()) != 0 {
		t.Fatalf("measured %d times, resent %d", measured, len(again.withConsumers()))
	}
}

// Review finding: a scan cut short by the link going down is not restarted on every reconnect.
func TestInterruptedMeasurementIsRetriedOnlyAfterABackoff(t *testing.T) {
	t.Parallel()
	started := make(chan struct{}, 4)
	r := &Reporter{Probe: &fakeProbe{}, StorageEvery: time.Hour, MeterEvery: time.Hour,
		Meter: func(ctx context.Context) protocol.SpaceConsumers {
			started <- struct{}{}
			<-ctx.Done() // a long walk the link outlives
			return protocol.SpaceConsumers{}
		}}
	for i := range 2 {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- r.Report(ctx, &storageSender{}) }()
		if i == 0 {
			<-started
		} else {
			select {
			case <-started:
				t.Fatal("measurement restarted right after a reconnect")
			case <-time.After(50 * time.Millisecond):
			}
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}
