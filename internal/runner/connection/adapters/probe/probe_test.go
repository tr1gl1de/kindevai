package probe

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tr1gl1de/kindevai/internal/platform/diskusage"
	"github.com/tr1gl1de/kindevai/internal/protocol"
)

func fakeProbe(wsl bool) *Probe {
	return &Probe{
		DataDir: "/home/u/.local/share/kindevai-runner",
		GOOS:    "linux", GOARCH: "amd64",
		LookPath: func(name string) (string, error) {
			if name == "codex" {
				return "", errors.New("not found")
			}
			return "/usr/bin/" + name, nil
		},
		Run: func(_ context.Context, path string, _ ...string) ([]byte, error) {
			switch path {
			case "/usr/bin/claude":
				return []byte("1.0.3 (Claude Code)\nmore\n"), nil
			case "/usr/bin/git":
				return []byte("git version 2.43.0\x1b[0m\n"), nil
			case "/usr/bin/docker":
				return nil, errors.New("exit status 1")
			}
			return []byte(strings.Repeat("x", 500)), nil
		},
		IsWSL: func() bool { return wsl },
		Stat: func(p string) (diskusage.Usage, error) {
			if p == "/mnt/c" {
				return diskusage.Usage{MountPoint: "/mnt/c", TotalBytes: 500, AvailableBytes: 100, FreeBytes: 100}, nil
			}
			return diskusage.Usage{MountPoint: "/", TotalBytes: 100, AvailableBytes: 40, FreeBytes: 45}, nil
		},
	}
}

func TestCapabilitiesListFoundToolsWithSanitizedVersions(t *testing.T) {
	t.Parallel()
	c := fakeProbe(true).Capabilities(t.Context())
	if c.OS != "linux" || c.Arch != "amd64" || !c.WSL || c.StrictEgress {
		t.Fatalf("capabilities = %+v", c)
	}
	want := map[string]string{"claude": "1.0.3 (Claude Code)", "git": "git version 2.43.0?[0m", "docker": ""}
	if len(c.Tools) != len(want) {
		t.Fatalf("tools = %+v", c.Tools)
	}
	for _, tl := range c.Tools {
		if v, ok := want[tl.Name]; !ok || v != tl.Version {
			t.Errorf("tool %s version %q, want %q", tl.Name, tl.Version, v)
		}
	}
}

func TestStorageReportsWorkspacesAndTheWindowsVolumeUnderWSL(t *testing.T) {
	t.Parallel()
	s := fakeProbe(true).Storage(t.Context())
	if len(s.Volumes) != 2 || s.Volumes[0].Role != protocol.VolumeWorkspaces || s.Volumes[0].AvailableBytes != 40 ||
		s.Volumes[0].Path != "/home/u/.local/share/kindevai-runner" || s.Volumes[1].Role != protocol.VolumeWindowsHost ||
		s.Volumes[1].ID != "/mnt/c" {
		t.Fatalf("storage = %+v", s)
	}
	if s := fakeProbe(false).Storage(t.Context()); len(s.Volumes) != 1 {
		t.Fatalf("storage without WSL = %+v", s)
	}
}

func TestVersionLineIsBounded(t *testing.T) {
	t.Parallel()
	if v := versionLine([]byte(strings.Repeat("é", 300))); len(v) > 128 || !strings.HasPrefix(v, "é") {
		t.Fatalf("version %q (%d bytes)", v, len(v))
	}
}

func TestRealProbeOnThisMachine(t *testing.T) {
	t.Parallel()
	p := New(t.TempDir())
	if s := p.Storage(t.Context()); len(s.Volumes) == 0 || s.Volumes[0].TotalBytes <= 0 {
		t.Fatalf("storage = %+v", s)
	}
	if c := p.Capabilities(t.Context()); c.OS == "" || c.Tools == nil {
		t.Fatalf("capabilities = %+v", c)
	}
}
