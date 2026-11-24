package gateway

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runners/domain"
)

type sink struct {
	mu    sync.Mutex
	beats []string
	caps  []domain.Capabilities
	vols  [][]domain.Volume
}

func (s *sink) Heartbeat(_ context.Context, id, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beats = append(s.beats, id)
	return nil
}

func (s *sink) Capabilities(_ context.Context, _, _ string, c domain.Capabilities) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.caps = append(s.caps, c)
	return nil
}

func (s *sink) Storage(_ context.Context, _, _ string, vs []domain.Volume) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vols = append(s.vols, vs)
	return nil
}

func (s *sink) count() (int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.beats), len(s.caps), len(s.vols)
}

func TestTelemetryMessagesReachTheSinkStampedWithServerTime(t *testing.T) {
	t.Parallel()
	s := &sink{}
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	e := newEnv(t, opts{messages: TelemetryMessages(s, func() time.Time { return now }, nil)})
	e.approve(t)
	c, _ := e.connect(t)
	send(t, c, protocol.TypeHeartbeat, map[string]any{"sentAt": "1999-01-01T00:00:00Z", "futureField": 1})
	send(t, c, protocol.TypeCapabilities, protocol.Capabilities{OS: "linux", Arch: "amd64", WSL: true,
		Tools: []protocol.Tool{{Name: "claude", Version: "1.0.3 (Claude Code)"}, {Name: "git", Version: "git version 2.43.0"}}})
	send(t, c, protocol.TypeStorage, protocol.Storage{Volumes: []protocol.Volume{{ID: "/", Role: protocol.VolumeWorkspaces,
		Path: "/home/u/.local/share/kindevai-runner", TotalBytes: 100, AvailableBytes: 40, FreeBytes: 45}}})
	waitFor(t, "telemetry", func() bool { b, cp, v := s.count(); return b == 1 && cp == 1 && v == 1 })
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.beats[0] != e.runner {
		t.Fatalf("heartbeat for %q", s.beats[0])
	}
	if got := s.caps[0]; !got.WSL || len(got.Tools) != 2 || !got.ReportedAt.Equal(now) {
		t.Fatalf("capabilities = %+v", got)
	}
	if got := s.vols[0][0]; got.AvailableBytes != 40 || !got.ReportedAt.Equal(now) || got.Role != domain.RoleWorkspaces {
		t.Fatalf("volume = %+v", got)
	}
}

func TestInvalidTelemetryClosesTheConnection(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		typ     string
		payload any
	}{
		"capabilities with a bad tool": {protocol.TypeCapabilities, protocol.Capabilities{OS: "linux", Arch: "amd64",
			Tools: []protocol.Tool{{Name: "x; rm", Version: ""}}}},
		"storage with available above total": {protocol.TypeStorage, protocol.Storage{Volumes: []protocol.Volume{{ID: "/",
			Role: protocol.VolumeWorkspaces, TotalBytes: 1, AvailableBytes: 2}}}},
		"storage claiming the control plane's role": {protocol.TypeStorage, protocol.Storage{Volumes: []protocol.Volume{{ID: "/",
			Role: protocol.VolumeData, TotalBytes: 2, AvailableBytes: 1}}}},
		"too many volumes": {protocol.TypeStorage, protocol.Storage{Volumes: make([]protocol.Volume, 9)}},
		"not an object":    {protocol.TypeHeartbeat, "beat"},
		"long version": {protocol.TypeCapabilities, protocol.Capabilities{OS: "linux", Arch: "amd64",
			Tools: []protocol.Tool{{Name: "git", Version: strings.Repeat("9", 200)}}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := &sink{}
			e := newEnv(t, opts{messages: TelemetryMessages(s, time.Now, nil)})
			e.approve(t)
			c, _ := e.connect(t)
			send(t, c, tc.typ, tc.payload)
			if code := closeCode(t, c); code != protocol.CloseProtocolError {
				t.Fatalf("close %d", code)
			}
		})
	}
}
