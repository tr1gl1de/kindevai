// Package probe describes the runner's machine for runner.capabilities and runner.storage (KIN-20): OS/arch,
// WSL2, the AI and build CLIs found on PATH with their versions, and the disk metrics of runner-data (plus the
// Windows volume holding the WSL2 disk image when it is visible).
package probe

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tr1gl1de/kindevai/internal/platform/diskusage"
	"github.com/tr1gl1de/kindevai/internal/protocol"
)

// Tools is what the runner looks for: the AI CLIs and the tools they need.
var Tools = []string{"claude", "codex", "git", "docker"}

const (
	versionTimeout  = 5 * time.Second
	maxVersionBytes = 128
	maxOutputBytes  = 4 << 10
	// windowsVolume is where WSL2 mounts C:, which holds %LOCALAPPDATA% and so, by default, the distro's ext4.vhdx.
	windowsVolume = "/mnt/c"
)

// Probe implements app.Prober. The function fields are seams for tests; New fills them for the real machine.
type Probe struct {
	DataDir      string
	GOOS, GOARCH string
	LookPath     func(name string) (string, error)
	Run          func(ctx context.Context, path string, args ...string) ([]byte, error)
	IsWSL        func() bool
	Stat         func(path string) (diskusage.Usage, error)
	// StrictEgress reports whether the strict egress sandbox verified on this machine (KIN-31); nil: false.
	StrictEgress func(ctx context.Context) bool
	// MaxParallelSteps is the parallel-step limit the operator set (ai-runner run --max-parallel-steps, KIN-76);
	// 0: none.
	MaxParallelSteps int
}

// New probes this machine; dataDir is runner-data (repos, worktrees, sessions live on its filesystem).
func New(dataDir string) *Probe {
	return &Probe{DataDir: dataDir, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, LookPath: exec.LookPath, Run: run,
		IsWSL: isWSL, Stat: diskusage.Stat}
}

// run executes a tool's version command with a timeout and bounded output. The command line is fixed (the tool
// found on PATH and "--version"); nothing from the Control Plane or a task reaches it.
func run(ctx context.Context, path string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...) //nolint:gosec // G204: a fixed tool name resolved on PATH
	cmd.Stdin = nil
	var out limitedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	return out.Bytes(), err
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := maxOutputBytes - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func isWSL() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	_, err := os.Stat("/proc/sys/fs/binfmt_misc/WSLInterop")
	return err == nil
}

// versionLine is the first line of a version output: control characters replaced, at most maxVersionBytes.
func versionLine(out []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(strings.ToValidUTF8(string(out), "")), "\n")
	line = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, strings.TrimSpace(line))
	for len(line) > maxVersionBytes {
		_, size := utf8.DecodeLastRuneInString(line)
		line = line[:len(line)-size]
	}
	return line
}

// Capabilities reports the platform and the tools found. A tool whose version command fails is still reported
// (it exists), with an empty version.
func (p *Probe) Capabilities(ctx context.Context) protocol.Capabilities {
	c := protocol.Capabilities{OS: p.GOOS, Arch: p.GOARCH, WSL: p.IsWSL(), Tools: []protocol.Tool{},
		MaxParallelSteps: p.MaxParallelSteps}
	if p.StrictEgress != nil {
		c.StrictEgress = p.StrictEgress(ctx)
	}
	for _, name := range Tools {
		path, err := p.LookPath(name)
		if err != nil {
			continue
		}
		t := protocol.Tool{Name: name}
		if out, err := p.Run(ctx, path, "--version"); err == nil {
			t.Version = versionLine(out)
		}
		c.Tools = append(c.Tools, t)
	}
	return c
}

// Storage measures runner-data and, under WSL2, the Windows volume. A volume that cannot be measured is left out.
func (p *Probe) Storage(context.Context) protocol.Storage {
	s := protocol.Storage{Volumes: []protocol.Volume{}}
	add := func(role, path string) {
		u, err := p.Stat(path)
		if err != nil || u.TotalBytes <= 0 {
			return
		}
		s.Volumes = append(s.Volumes, protocol.Volume{ID: u.MountPoint, Role: role, Path: path, TotalBytes: u.TotalBytes,
			AvailableBytes: u.AvailableBytes, FreeBytes: u.FreeBytes})
	}
	add(protocol.VolumeWorkspaces, p.DataDir)
	if p.IsWSL() {
		add(protocol.VolumeWindowsHost, windowsVolume)
	}
	return s
}
