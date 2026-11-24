//go:build unix

package process

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/runner/session/adapters/mockagent"
	"github.com/tr1gl1de/kindevai/internal/runner/session/app"
	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// The test binary doubles as the mock agent (like `ai-runner mock-agent`).
func TestMain(m *testing.M) {
	if os.Getenv("KINDEV_TEST_MOCK_AGENT") == "1" {
		os.Exit(mockagent.Main(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func launcher(t *testing.T) *Launcher {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &Launcher{Program: exe, Env: []string{"KINDEV_TEST_MOCK_AGENT=1"},
		Poll: 10 * time.Millisecond}
}

func start(t *testing.T, l *Launcher, scenario string, attempt int64) (app.Process, string, *syncBuf) {
	t.Helper()
	dir := t.TempDir()
	out := &syncBuf{}
	p, err := l.Launch(t.Context(), app.LaunchSpec{Dir: dir, Scenario: scenario, Attempt: attempt, Stdout: out, Stderr: out})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Signal(true); p.Wait() })
	return p, dir, out
}

func TestScenarios(t *testing.T) {
	t.Parallel()
	l := launcher(t)
	cases := []struct {
		scenario string
		attempt  int64
		want     domain.Exit
		output   string
	}{
		{"success", 1, domain.Exit{Code: 0}, "done"},
		{"fail", 1, domain.Exit{Code: 1}, "simulated failure"},
		{"flaky", 1, domain.Exit{Code: 1}, "first attempt"},
		{"flaky", 2, domain.Exit{Code: 0}, "succeeded on retry"},
		{"crash", 1, domain.Exit{Signaled: true}, "scenario crash"},
	}
	for _, c := range cases {
		t.Run(c.scenario, func(t *testing.T) {
			t.Parallel()
			p, dir, out := start(t, l, c.scenario, c.attempt)
			if got := p.Wait(); got != c.want {
				t.Fatalf("exit %+v, want %+v", got, c.want)
			}
			if !strings.Contains(out.String(), c.output) {
				t.Fatalf("output %q", out.String())
			}
			if code, ok := mockagent.ReadResult(dir); c.scenario != "crash" && (!ok || code != c.want.Code) {
				t.Fatalf("result file %d %v", code, ok)
			}
		})
	}
}

// Cancel signals the whole process group; a hanging agent dies of SIGTERM.
func TestSignalStopsTheProcessGroup(t *testing.T) {
	t.Parallel()
	p, _, out := start(t, launcher(t), "hang", 1)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "hanging") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if p.PGID() != p.PID() || !Alive(p.PID(), p.PGID()) {
		t.Fatalf("pid %d pgid %d not a live group leader", p.PID(), p.PGID())
	}
	if err := p.Signal(false); err != nil {
		t.Fatal(err)
	}
	if got := p.Wait(); !got.Signaled {
		t.Fatalf("exit %+v", got)
	}
	if Alive(p.PID(), p.PGID()) {
		t.Fatal("process group still alive")
	}
}

// After a runner restart the recorded process is adopted: it is watched until it ends and its result file tells
// how it ended; a process that is gone without a result is lost.
func TestAdopt(t *testing.T) {
	t.Parallel()
	l := launcher(t)
	p, dir, out := start(t, l, "gate", 1)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "waiting for release") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	a := l.Adopt(domain.Record{Step: "s", Fence: 1, PID: p.PID(), PGID: p.PGID()}, dir)
	done := make(chan domain.Exit, 1)
	go func() { done <- a.Wait() }()
	select {
	case e := <-done:
		t.Fatalf("adopted process ended early: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
	if err := os.WriteFile(filepath.Join(dir, mockagent.ReleaseFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p.Wait() // the original parent reaps it (in a real restart that parent is gone)
	if e := <-done; e != (domain.Exit{Code: 0}) {
		t.Fatalf("adopted exit %+v", e)
	}

	gone := l.Adopt(domain.Record{Step: "s", Fence: 2, PID: p.PID(), PGID: p.PGID()}, t.TempDir())
	if e := gone.Wait(); !e.Lost {
		t.Fatalf("gone process %+v", e)
	}
	if err := gone.Signal(true); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchFailsForAMissingProgram(t *testing.T) {
	t.Parallel()
	l := &Launcher{Program: filepath.Join(t.TempDir(), "missing")}
	if _, err := l.Launch(t.Context(), app.LaunchSpec{Dir: t.TempDir(), Scenario: "success", Attempt: 1}); err == nil {
		t.Fatal("launched a missing program")
	}
}

// KIN-64: the launched process's start time is recorded, and it is stable for the life of the process.
func TestStartTimeIdentifiesTheProcess(t *testing.T) {
	t.Parallel()
	p, _, _ := start(t, launcher(t), "hang", 1)
	st := p.StartTime()
	if st == "" {
		t.Fatal("no start time recorded")
	}
	if again, err := StartTime(p.PID()); err != nil || again != st {
		t.Fatalf("start time now %q (%v), at launch %q", again, err, st)
	}
	if !strings.Contains(st, ":") { // boot id : ticks (Linux) or seconds.microseconds (macOS)
		if _, err := os.Stat("/proc/self/stat"); err == nil {
			t.Fatalf("start time %q has no boot id", st)
		}
	}
	if own, err := StartTime(os.Getpid()); err != nil || own == "" || own == st {
		t.Fatalf("own start time %q (%v) vs child %q", own, err, st)
	}
	if _, err := StartTime(1 << 30); err == nil {
		t.Fatal("start time of a missing process")
	}
}

// KIN-64: a recorded pid that now belongs to another process (its start time differs) is not adopted: the attempt
// counts as ended, and a cancel never signals the stranger's process group.
func TestAdoptRejectsAReusedPID(t *testing.T) {
	t.Parallel()
	l := launcher(t)
	stranger, _, out := start(t, l, "hang", 1)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "hanging") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	rec := domain.Record{Step: "s", Fence: 1, PID: stranger.PID(), PGID: stranger.PGID(), Start: "1"} // our process started at tick 1
	a := l.Adopt(rec, t.TempDir())
	if a.StartTime() != "1" {
		t.Fatalf("adopted start time %q", a.StartTime())
	}
	if err := a.Signal(true); err != nil {
		t.Fatal(err)
	}
	if e := a.Wait(); !e.Lost {
		t.Fatalf("reused pid adopted: %+v", e)
	}
	if !Alive(stranger.PID(), stranger.PGID()) {
		t.Fatal("the stranger's process group was signalled")
	}
	// The real process (its start time matches) is adopted as before.
	same := l.Adopt(domain.Record{Step: "s", Fence: 2, PID: stranger.PID(), PGID: stranger.PGID(), Start: stranger.StartTime()}, t.TempDir())
	done := make(chan domain.Exit, 1)
	go func() { done <- same.Wait() }()
	select {
	case e := <-done:
		t.Fatalf("own process not adopted: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}
	if err := same.Signal(true); err != nil {
		t.Fatal(err)
	}
	stranger.Wait()
	if e := <-done; !e.Lost {
		t.Fatalf("killed adopted process: %+v", e)
	}
}

// In a worktree the process works there, while its result stays in the attempt's state directory (it never
// becomes a changed file of the repository).
func TestProcessRunsInTheWorkdirAndLeavesItsResultInTheStateDir(t *testing.T) {
	t.Parallel()
	l := launcher(t)
	state, work := t.TempDir(), t.TempDir()
	out := &syncBuf{}
	p, err := l.Launch(t.Context(), app.LaunchSpec{Dir: state, Workdir: work, Scenario: domain.ScenarioGate, Attempt: 1,
		Stdout: out, Stderr: out})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, mockagent.ReleaseFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if e := p.Wait(); e.Code != 0 {
		t.Fatalf("%+v %s", e, out)
	}
	if code, ok := mockagent.ReadResult(state); !ok || code != 0 {
		t.Fatal("no result in the state dir")
	}
	if ents, _ := os.ReadDir(work); len(ents) != 0 {
		t.Fatalf("files left in the worktree: %v", ents)
	}
}
