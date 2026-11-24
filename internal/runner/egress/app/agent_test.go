package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/egress/app"
)

type memStore struct {
	mu  sync.Mutex
	cfg *protocol.ConfigUpdate
}

func (m *memStore) Load() (protocol.ConfigUpdate, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cfg == nil {
		return protocol.ConfigUpdate{}, false, nil
	}
	return *m.cfg, true, nil
}

func (m *memStore) Save(c protocol.ConfigUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = &c
	return nil
}

type prober struct {
	release chan struct{}
	got     chan protocol.NetworkProfile
}

func (p *prober) Probe(ctx context.Context, np protocol.NetworkProfile, _ []protocol.NetworkEndpoint) protocol.NetworkTestResult {
	p.got <- np
	select {
	case <-p.release:
	case <-ctx.Done():
	}
	return protocol.NetworkTestResult{OK: true, ObservedIPv4: "203.0.113.7"}
}

type sender struct {
	out chan protocol.NetworkTestResult
}

func (s sender) Send(_ context.Context, typ string, payload any) error {
	if typ != protocol.TypeNetworkTestResult {
		return errors.New("unexpected " + typ)
	}
	s.out <- payload.(protocol.NetworkTestResult)
	return nil
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func command(t *testing.T, kind string, body any) protocol.Command {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.Command{ID: kind, Kind: kind, Body: b}
}

func TestConfigUpdateStoredNewestOnlyAndNotLogged(t *testing.T) {
	t.Parallel()
	logs := &lockedBuf{}
	st := &memStore{}
	a := &app.Agent{Store: st, Logger: slog.New(slog.NewTextHandler(logs, nil))}
	cfg := func(rev int64) protocol.ConfigUpdate {
		return protocol.ConfigUpdate{Revision: rev, Network: protocol.NetworkConfig{Default: protocol.NetworkProfile{
			Enforcement: "strict", Proxy: &protocol.NetworkProxy{Scheme: "socks5", Host: "p.example", Port: 1080, Password: "hunter2"}}}}
	}
	for _, rev := range []int64{2, 1, 2} {
		if err := a.Command(t.Context(), command(t, protocol.CommandConfigUpdate, cfg(rev))); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, _ := a.Config()
	if !ok || got.Revision != 2 || got.Network.Default.Proxy.Password != "hunter2" {
		t.Fatalf("stored %+v", got)
	}
	if strings.Contains(logs.String(), "hunter2") || !strings.Contains(logs.String(), "network configuration applied") {
		t.Fatalf("logs: %s", logs.String())
	}
	if err := a.Command(t.Context(), protocol.Command{Kind: protocol.CommandAssign}); !errors.Is(err, app.ErrNotNetworkCommand) {
		t.Fatalf("other kind: %v", err)
	}
}

func TestNetworkTestRunsInBackgroundAndReportsOnTheLink(t *testing.T) {
	t.Parallel()
	p := &prober{release: make(chan struct{}), got: make(chan protocol.NetworkProfile, 2)}
	a := &app.Agent{Store: &memStore{}, Prober: p, Timeout: time.Minute}
	if err := a.Command(t.Context(), command(t, protocol.CommandNetworkTest, protocol.NetworkTest{TestID: "t1",
		Profile: protocol.NetworkProfile{Name: "corp"}})); err != nil {
		t.Fatal(err)
	}
	if np := <-p.got; np.Name != "corp" {
		t.Fatalf("probed %+v", np)
	}
	// A second test while the first runs is answered busy.
	_ = a.Command(t.Context(), command(t, protocol.CommandNetworkTest, protocol.NetworkTest{TestID: "t2"}))
	s := sender{out: make(chan protocol.NetworkTestResult, 4)}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx, s) }()
	t.Cleanup(func() { cancel(); <-done })
	if r := <-s.out; r.TestID != "t2" || r.Code != "busy" {
		t.Fatalf("first result %+v", r)
	}
	close(p.release)
	if r := <-s.out; r.TestID != "t1" || !r.OK || r.ObservedIPv4 != "203.0.113.7" {
		t.Fatalf("result %+v", r)
	}
}
