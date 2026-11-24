package app

import (
	"context"
	"errors"
	"slices"
	"sync"
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

type fakeProbe struct{ storageCalls int }

func (p *fakeProbe) Capabilities(context.Context) protocol.Capabilities {
	return protocol.Capabilities{OS: "linux", Arch: "amd64"}
}

func (p *fakeProbe) Storage(context.Context) protocol.Storage {
	p.storageCalls++
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
	for s.count(protocol.TypeHeartbeat) < 3 {
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
	first := slices.Clone(s.types[:3])
	s.mu.Unlock()
	if !slices.Equal(first, []string{protocol.TypeCapabilities, protocol.TypeStorage, protocol.TypeHeartbeat}) {
		t.Fatalf("first messages = %v", first)
	}
	if s.count(protocol.TypeStorage) != 1 || s.count(protocol.TypeCapabilities) != 1 {
		t.Fatalf("messages = %v", s.types)
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
	f.c.Reporter = &Reporter{Probe: &fakeProbe{}, HeartbeatEvery: time.Hour, StorageEvery: time.Hour}
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for link.count(protocol.TypeHeartbeat) < 1 && time.Now().Before(deadline) {
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
