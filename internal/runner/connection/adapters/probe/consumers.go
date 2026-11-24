package probe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
)

// Space consumers (KIN-55, KB KIN-A-26): the runner measures what takes space in runner-data and in the container
// engine. It runs only when the operator allowed it (ai-runner run --measure-space) and under I/O limits, so a
// measurement never slows coding agents down: the walk is paced, capped in entries and time, reads metadata only
// (lstat), never follows symlinks and never leaves the filesystem of the directory it measures.

// Limits bound one measurement; zero fields take the defaults.
type Limits struct {
	MaxEntries       int64         // all categories together (default 1 000 000)
	EntriesPerSecond int           // pacing of the walk (default 2 000)
	Budget           time.Duration // wall time of the walk (default 10 min)
}

func (l Limits) withDefaults() Limits {
	if l.MaxEntries <= 0 {
		l.MaxEntries = 1_000_000
	}
	if l.EntriesPerSecond <= 0 {
		l.EntriesPerSecond = 2_000
	}
	if l.Budget <= 0 {
		l.Budget = 10 * time.Minute
	}
	return l
}

// Pacing: a pause of pauseEvery after each tenth of the per-second allowance.
const pauseEvery = 100 * time.Millisecond

// dockerTimeout bounds `docker system df`; maxDFBytes bounds its output.
const (
	dockerTimeout = time.Minute
	maxDFBytes    = 64 << 10
)

// consumerDirs maps runner-data subdirectories to consumer categories, in report order.
var consumerDirs = []struct{ dir, category string }{
	{"repos", protocol.ConsumerRepos},
	{"workspaces", protocol.ConsumerWorkspaces},
	{"sessions", protocol.ConsumerSessions},
	{"events", protocol.ConsumerSpool},
}

// SpaceMeter measures space consumers. Function fields are test seams; nil ones use the real machine.
type SpaceMeter struct {
	DataDir  string
	Limits   Limits
	LookPath func(name string) (string, error)
	Run      func(ctx context.Context, path string, args ...string) ([]byte, error)
	Sleep    func(ctx context.Context, d time.Duration) error
	Now      func() time.Time
}

// NewSpaceMeter measures runner-data with the default limits.
func NewSpaceMeter(dataDir string) *SpaceMeter { return &SpaceMeter{DataDir: dataDir} }

func (m *SpaceMeter) init() {
	if m.LookPath == nil {
		m.LookPath = exec.LookPath
	}
	if m.Run == nil {
		m.Run = runDF
	}
	if m.Sleep == nil {
		m.Sleep = sleep
	}
	if m.Now == nil {
		m.Now = time.Now
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Measure walks the runner-data categories that exist and asks docker (when found) for its caches. A category the
// limits cut short is reported as Partial.
func (m *SpaceMeter) Measure(ctx context.Context) protocol.SpaceConsumers {
	m.init()
	l := m.Limits.withDefaults()
	w := &walker{m: m, ctx: ctx, l: l, deadline: m.Now().Add(l.Budget), batch: max(1, l.EntriesPerSecond/10)}
	out := protocol.SpaceConsumers{Items: []protocol.SpaceConsumer{}}
	for _, c := range consumerDirs {
		root := filepath.Join(m.DataDir, c.dir)
		fi, err := os.Lstat(root)
		if err != nil || !fi.IsDir() {
			continue
		}
		bytes, entries, complete := w.walk(root, fi)
		out.Items = append(out.Items, protocol.SpaceConsumer{Category: c.category, Bytes: bytes, Entries: entries, Partial: !complete})
	}
	if c, ok := m.containerCache(ctx); ok {
		out.Items = append(out.Items, c)
	}
	return out
}

type walker struct {
	m          *SpaceMeter
	ctx        context.Context
	l          Limits
	deadline   time.Time
	batch      int
	seen       int64 // entries of every category so far
	sincePause int
	inodes     map[[2]uint64]struct{} // hard-linked files already counted (git object stores, worktrees)
}

// maxInodes bounds the hard-link set; beyond it further links are counted again (the result is approximate).
const maxInodes = 100_000

var errStop = errors.New("measurement limit reached")

// walk sums the disk usage below root; complete is false when a limit or cancellation stopped it.
func (w *walker) walk(root string, rootInfo fs.FileInfo) (total, entries int64, complete bool) {
	dev, _ := device(rootInfo)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil { // unreadable entry: skip it, keep measuring the rest
			if d != nil && d.IsDir() && path != root {
				return fs.SkipDir
			}
			return nil
		}
		if err := w.step(); err != nil {
			return err
		}
		fi, err := d.Info() // lstat: symlinks are measured as links, never followed
		if err != nil {
			return nil
		}
		if d.IsDir() && path != root {
			if dv, ok := device(fi); ok && dv != dev {
				return fs.SkipDir // another filesystem mounted inside runner-data
			}
		}
		entries++
		if !w.counted(fi) {
			total += usage(fi)
		}
		return nil
	})
	return total, entries, err == nil
}

// step enforces the limits before each entry.
func (w *walker) step() error {
	if w.ctx.Err() != nil || w.seen >= w.l.MaxEntries || !w.m.Now().Before(w.deadline) {
		return errStop
	}
	w.seen++
	w.sincePause++
	if w.sincePause >= w.batch {
		w.sincePause = 0
		if err := w.m.Sleep(w.ctx, pauseEvery); err != nil {
			return errStop
		}
	}
	return nil
}

// counted reports whether fi is a hard link to a file already measured (and remembers it otherwise).
func (w *walker) counted(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || fi.IsDir() || st.Nlink < 2 {
		return false
	}
	key := [2]uint64{uint64(st.Dev), st.Ino} //nolint:unconvert,gosec // int32 on darwin, uint64 on linux
	if _, ok := w.inodes[key]; ok {
		return true
	}
	if w.inodes == nil {
		w.inodes = map[[2]uint64]struct{}{}
	}
	if len(w.inodes) < maxInodes {
		w.inodes[key] = struct{}{}
	}
	return false
}

func device(fi fs.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true //nolint:unconvert,gosec // int32 on darwin, uint64 on linux
}

// usage is the space the entry takes on disk (allocated blocks), falling back to its size.
func usage(fi fs.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Blocks > 0 {
		return st.Blocks * 512
	}
	if fi.Mode().IsRegular() {
		return fi.Size()
	}
	return 0
}

// containerCache sums every row of `docker system df` (images, containers, local volumes, build cache).
func (m *SpaceMeter) containerCache(ctx context.Context) (protocol.SpaceConsumer, bool) {
	path, err := m.LookPath("docker")
	if err != nil {
		return protocol.SpaceConsumer{}, false
	}
	out, err := m.Run(ctx, path, "system", "df", "--format", "{{json .}}")
	if err != nil {
		return protocol.SpaceConsumer{}, false
	}
	c := protocol.SpaceConsumer{Category: protocol.ConsumerContainerCache}
	rows := 0
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		var row struct{ Size, TotalCount string }
		if json.Unmarshal(sc.Bytes(), &row) != nil {
			continue
		}
		size, ok := parseDockerSize(row.Size)
		if !ok {
			continue
		}
		n, _ := strconv.ParseInt(row.TotalCount, 10, 64)
		c.Bytes += size
		c.Entries += max(n, 0)
		rows++
	}
	return c, rows > 0
}

// runDF runs `docker system df` with a timeout and bounded output. The command line is fixed; nothing from the
// Control Plane or a task reaches it.
func runDF(ctx context.Context, path string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, dockerTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...) //nolint:gosec // G204: a fixed tool name resolved on PATH
	var out boundedBuffer
	cmd.Stdout = &out
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	return out.Bytes(), err
}

type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := maxDFBytes - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// dockerUnits are the decimal units of docker's human-readable sizes (go-units HumanSize).
var dockerUnits = []struct {
	suffix string
	mult   float64
}{{"kB", 1e3}, {"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12}, {"PB", 1e15}, {"B", 1}}

// parseDockerSize parses "3.5GB", "12.5kB" or "0B".
func parseDockerSize(s string) (int64, bool) {
	for _, u := range dockerUnits {
		num, ok := strings.CutSuffix(s, u.suffix)
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(num, 64)
		if err != nil || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) || f*u.mult >= math.MaxInt64 {
			return 0, false
		}
		return int64(math.Round(f * u.mult)), true
	}
	return 0, false
}
