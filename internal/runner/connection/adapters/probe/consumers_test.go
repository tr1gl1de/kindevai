package probe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
)

func write(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, n), 0o600); err != nil {
		t.Fatal(err)
	}
}

func byCategory(s protocol.SpaceConsumers) map[string]protocol.SpaceConsumer {
	out := map[string]protocol.SpaceConsumer{}
	for _, c := range s.Items {
		out[c.Category] = c
	}
	return out
}

func noDocker(string) (string, error) { return "", errors.New("not found") }

func TestSpaceMeterMeasuresRunnerDataCategories(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "repos", "a.git", "pack"), 64<<10)
	write(t, filepath.Join(dir, "workspaces", "w1", "src", "main.go"), 8<<10)
	write(t, filepath.Join(dir, "workspaces", "w2", "big.bin"), 256<<10)
	write(t, filepath.Join(dir, "sessions", "s1.jsonl"), 1<<10)
	write(t, filepath.Join(dir, "events", "seg-1"), 4<<10)
	write(t, filepath.Join(dir, "keys", "runner.key"), 1<<10) // never measured: not a consumer category
	outside := t.TempDir()
	write(t, filepath.Join(outside, "huge"), 1<<20)
	if err := os.Symlink(outside, filepath.Join(dir, "workspaces", "link")); err != nil {
		t.Fatal(err)
	}

	m := &SpaceMeter{DataDir: dir, LookPath: noDocker}
	got := byCategory(m.Measure(t.Context()))
	if len(got) != 4 {
		t.Fatalf("categories = %+v", got)
	}
	ws := got[protocol.ConsumerWorkspaces]
	if ws.Bytes < 264<<10 || ws.Bytes >= 1<<20 || ws.Entries < 6 || ws.Partial {
		t.Fatalf("workspaces = %+v (the symlink must not be followed)", ws)
	}
	if r := got[protocol.ConsumerRepos]; r.Bytes < 64<<10 || r.Entries != 3 {
		t.Fatalf("repos = %+v", r)
	}
	if s := got[protocol.ConsumerSpool]; s.Bytes < 4<<10 {
		t.Fatalf("spool = %+v", s)
	}
	if _, ok := got[protocol.ConsumerContainerCache]; ok {
		t.Fatal("container cache reported without docker")
	}
}

func TestSpaceMeterStopsAtItsIOLimits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for i := range 50 {
		write(t, filepath.Join(dir, "workspaces", "w", strings.Repeat("f", i+1)), 10)
	}
	t.Run("entry limit makes the result partial", func(t *testing.T) {
		t.Parallel()
		m := &SpaceMeter{DataDir: dir, LookPath: noDocker, Limits: Limits{MaxEntries: 20}}
		ws := byCategory(m.Measure(t.Context()))[protocol.ConsumerWorkspaces]
		if !ws.Partial || ws.Entries != 20 {
			t.Fatalf("workspaces = %+v", ws)
		}
	})
	t.Run("the walk is paced", func(t *testing.T) {
		t.Parallel()
		var pauses int
		m := &SpaceMeter{DataDir: dir, LookPath: noDocker, Limits: Limits{EntriesPerSecond: 100},
			Sleep: func(ctx context.Context, d time.Duration) error {
				if d != 100*time.Millisecond {
					t.Errorf("pause %v", d)
				}
				pauses++
				return ctx.Err()
			}}
		m.Measure(t.Context())
		if pauses < 4 { // ≥ 52 entries in batches of 10
			t.Fatalf("pauses = %d", pauses)
		}
	})
	t.Run("time budget makes the result partial", func(t *testing.T) {
		t.Parallel()
		now := time.Unix(0, 0)
		m := &SpaceMeter{DataDir: dir, LookPath: noDocker, Limits: Limits{Budget: time.Second},
			Now: func() time.Time { now = now.Add(100 * time.Millisecond); return now }}
		if ws := byCategory(m.Measure(t.Context()))[protocol.ConsumerWorkspaces]; !ws.Partial {
			t.Fatalf("workspaces = %+v", ws)
		}
	})
	t.Run("cancellation stops the walk", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		m := &SpaceMeter{DataDir: dir, LookPath: noDocker}
		for _, c := range m.Measure(ctx).Items {
			if !c.Partial {
				t.Fatalf("%+v measured after cancellation", c)
			}
		}
	})
}

func TestSpaceMeterReadsContainerCachesFromDockerSystemDF(t *testing.T) {
	t.Parallel()
	out := `{"Active":"2","Reclaimable":"1.2GB (40%)","Size":"3.5GB","TotalCount":"12","Type":"Images"}
{"Active":"1","Reclaimable":"0B (0%)","Size":"12.5kB","TotalCount":"3","Type":"Containers"}
{"Active":"1","Reclaimable":"0B","Size":"512MB","TotalCount":"4","Type":"Local Volumes"}
{"Active":"0","Reclaimable":"2GB","Size":"2GB","TotalCount":"40","Type":"Build Cache"}
`
	var args []string
	m := &SpaceMeter{DataDir: t.TempDir(), LookPath: func(string) (string, error) { return "/usr/bin/docker", nil },
		Run: func(_ context.Context, path string, a ...string) ([]byte, error) {
			args = append([]string{path}, a...)
			return []byte(out), nil
		}}
	cc, ok := byCategory(m.Measure(t.Context()))[protocol.ConsumerContainerCache]
	if !ok || cc.Bytes != 3_500_000_000+12_500+512_000_000+2_000_000_000 || cc.Entries != 59 {
		t.Fatalf("container cache = %+v", cc)
	}
	if strings.Join(args, " ") != "/usr/bin/docker system df --format {{json .}}" {
		t.Fatalf("command = %q", args)
	}
	m.Run = func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("daemon not running") }
	if _, ok := byCategory(m.Measure(t.Context()))[protocol.ConsumerContainerCache]; ok {
		t.Fatal("a failing docker reported a size")
	}
}

func TestParseDockerSize(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]int64{"0B": 0, "512B": 512, "12.5kB": 12_500, "3.5GB": 3_500_000_000, "1TB": 1e12,
		"1.5MB": 1_500_000} {
		if got, ok := parseDockerSize(in); !ok || got != want {
			t.Errorf("%q = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "GB", "-1GB", "1XB", "1e400GB"} {
		if _, ok := parseDockerSize(in); ok {
			t.Errorf("%q parsed", in)
		}
	}
}

// Review finding: hard links (git object stores shared with worktrees) are counted once.
func TestSpaceMeterCountsHardLinkedFilesOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "repos", "a.git", "objects", "pack"), 512<<10)
	if err := os.MkdirAll(filepath.Join(dir, "workspaces", "w1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(dir, "repos", "a.git", "objects", "pack"), filepath.Join(dir, "workspaces", "w1", "pack")); err != nil {
		t.Fatal(err)
	}
	got := byCategory((&SpaceMeter{DataDir: dir, LookPath: noDocker}).Measure(t.Context()))
	if r := got[protocol.ConsumerRepos]; r.Bytes < 512<<10 {
		t.Fatalf("repos = %+v", r)
	}
	if ws := got[protocol.ConsumerWorkspaces]; ws.Bytes >= 512<<10 || ws.Entries != 3 {
		t.Fatalf("workspaces counted the hard link again: %+v", ws)
	}
}
