package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

type fakeProc struct {
	pid      int
	exit     chan struct{} // closed when the process ended (every Wait returns)
	result   domain.Exit
	once     sync.Once
	mu       sync.Mutex
	signals  []bool
	termExit bool // exits on SIGTERM
}

func (p *fakeProc) PID() int  { return p.pid }
func (p *fakeProc) PGID() int { return p.pid }

func (p *fakeProc) StartTime() string { return fmt.Sprintf("start-%d", p.pid) }
func (p *fakeProc) Wait() domain.Exit {
	<-p.exit
	return p.result
}
func (p *fakeProc) end(e domain.Exit) { p.once.Do(func() { p.result = e; close(p.exit) }) }
func (p *fakeProc) Signal(kill bool) error {
	p.mu.Lock()
	p.signals = append(p.signals, kill)
	p.mu.Unlock()
	if kill || p.termExit {
		go p.end(domain.Exit{Signaled: true})
	}
	return nil
}
func (p *fakeProc) sent() []bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.signals)
}

type fakeLauncher struct {
	mu       sync.Mutex
	launched []LaunchSpec
	procs    []*fakeProc
	adopted  map[int]*fakeProc
	termExit bool
}

func (l *fakeLauncher) Launch(_ context.Context, s LaunchSpec) (Process, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p := &fakeProc{pid: 100 + len(l.procs), exit: make(chan struct{}), termExit: l.termExit}
	l.launched = append(l.launched, s)
	l.procs = append(l.procs, p)
	_, _ = io.WriteString(s.Stdout, "hello\n")
	return p, nil
}

func (l *fakeLauncher) Adopt(r domain.Record, _ string) Process {
	l.mu.Lock()
	defer l.mu.Unlock()
	if p, ok := l.adopted[r.PID]; ok {
		return p
	}
	p := &fakeProc{pid: r.PID, exit: make(chan struct{})}
	p.end(domain.Exit{Lost: true})
	return p
}

func (l *fakeLauncher) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.launched)
}

func (l *fakeLauncher) proc(i int) *fakeProc {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.procs[i]
}

type memJournal struct {
	mu   sync.Mutex
	recs map[string]domain.Record
	done map[string]bool
}

func (j *memJournal) Finish(step string, fence int64) error {
	_ = j.Delete(step, fence)
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.done == nil {
		j.done = map[string]bool{}
	}
	j.done[jkey(step, fence)] = true
	return nil
}

func (j *memJournal) Finished(step string, fence int64) (bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.done[jkey(step, fence)], nil
}

func jkey(step string, fence int64) string { return fmt.Sprintf("%s-%d", step, fence) }

func (j *memJournal) Prepare(step string, fence int64) (string, error) {
	return "/work/" + jkey(step, fence), nil
}
func (j *memJournal) Save(r domain.Record) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.recs == nil {
		j.recs = map[string]domain.Record{}
	}
	j.recs[jkey(r.Step, r.Fence)] = r
	return nil
}
func (j *memJournal) Delete(step string, fence int64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.recs, jkey(step, fence))
	return nil
}
func (j *memJournal) List() ([]domain.Record, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []domain.Record
	for _, r := range j.recs {
		out = append(out, r)
	}
	return out, nil
}

type event struct {
	step  string
	fence int64
	seq   int64
	kind  string
	body  string
}

type fakeEvents struct {
	mu   sync.Mutex
	evs  []event
	fail bool // Emit fails (a full disk)
}

type fakeStream struct {
	ev      *fakeEvents
	step    string
	fence   int64
	mu      sync.Mutex
	seq     int64
	res     int64
	reserve func(int64) error
}

func (e *fakeEvents) Attempt(step string, fence, after int64, reserve func(int64) error) Stream {
	return &fakeStream{ev: e, step: step, fence: fence, seq: after, res: after, reserve: reserve}
}

func (s *fakeStream) Emit(kind string, body any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reserve != nil && s.seq+1 > s.res {
		if err := s.reserve(s.seq + 10); err != nil {
			return err
		}
		s.res = s.seq + 10
	}
	s.ev.mu.Lock()
	if s.ev.fail {
		s.ev.mu.Unlock()
		return errors.New("disk full")
	}
	s.ev.mu.Unlock()
	s.seq++
	b, _ := json.Marshal(body)
	s.ev.mu.Lock()
	s.ev.evs = append(s.ev.evs, event{s.step, s.fence, s.seq, kind, string(b)})
	s.ev.mu.Unlock()
	return nil
}

func (s *fakeStream) Output(stream string) io.Writer {
	return writerFunc(func(p []byte) (int, error) { return len(p), s.Emit(protocol.EventOutput, string(p)) })
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func (e *fakeEvents) kinds() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, v := range e.evs {
		out = append(out, v.kind)
	}
	return out
}

func (e *fakeEvents) last() event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.evs[len(e.evs)-1]
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func assignment(project string) domain.Assignment {
	return domain.Assignment{Step: "S1", Fence: 1, Execution: "E1", Task: "T1", Project: project, Repository: "acme/app",
		Adapter: domain.AdapterMock, Scenario: domain.ScenarioSuccess}
}

func newExecutor(policy domain.Policy) (*Executor, *fakeLauncher, *memJournal, *fakeEvents) {
	l, j, ev := &fakeLauncher{}, &memJournal{}, &fakeEvents{}
	return &Executor{Launcher: l, Journal: j, Events: ev, Policy: policy, Grace: 20 * time.Millisecond}, l, j, ev
}

func TestAssignRunsTheAttemptAndReportsItsEnd(t *testing.T) {
	t.Parallel()
	x, l, j, ev := newExecutor(domain.Policy{Projects: []string{"P1"}})
	if err := x.Assign(t.Context(), assignment("P1")); err != nil {
		t.Fatal(err)
	}
	if got := x.Attempts(); len(got) != 1 || !got[0].Running || got[0].PID != 100 || got[0].Step != "S1" {
		t.Fatalf("attempts %+v", got)
	}
	// KIN-64: the journal identifies the process by pid and start time, so a restart never adopts a reused pid.
	if recs, _ := j.List(); len(recs) != 1 || recs[0].PID != 100 || recs[0].Start != "start-100" {
		t.Fatalf("journal %+v", recs)
	}
	if s := l.launched[0]; s.Scenario != domain.ScenarioSuccess || s.Attempt != 1 || s.Dir != "/work/S1-1" {
		t.Fatalf("launch %+v", s)
	}
	l.proc(0).end(domain.Exit{Code: 0})
	waitFor(t, "completed", func() bool { return slices.Contains(ev.kinds(), protocol.EventCompleted) })
	if k := ev.kinds(); !slices.Equal(k, []string{protocol.EventAccepted, protocol.EventOutput, protocol.EventStarted, protocol.EventCompleted}) {
		t.Fatalf("events %v", k)
	}
	waitFor(t, "journal cleared", func() bool { r, _ := j.List(); return len(r) == 0 && len(x.Attempts()) == 0 })
}

func TestFailedProcessReportsTheReason(t *testing.T) {
	t.Parallel()
	x, l, _, ev := newExecutor(domain.Policy{})
	if err := x.Assign(t.Context(), assignment("P1")); err != nil {
		t.Fatal(err)
	}
	l.proc(0).end(domain.Exit{Code: 2})
	waitFor(t, "failed", func() bool { return slices.Contains(ev.kinds(), protocol.EventFailed) })
	if e := ev.last(); !strings.Contains(e.body, "exit status 2") {
		t.Fatalf("failure %+v", e)
	}
}

// A repeated assignment (re-delivered command, or a command applied before a restart) never starts a second
// process.
func TestRepeatedAssignmentStartsNoSecondProcess(t *testing.T) {
	t.Parallel()
	x, l, _, _ := newExecutor(domain.Policy{})
	for range 3 {
		if err := x.Assign(t.Context(), assignment("P1")); err != nil {
			t.Fatal(err)
		}
	}
	if n := l.count(); n != 1 {
		t.Fatalf("%d processes launched", n)
	}
	x.KillAll()
}

// The runner checks the assignment's scope itself: an approved runner refuses another project's work even when
// the Control Plane assigns it.
func TestForeignProjectIsRefusedByTheRunner(t *testing.T) {
	t.Parallel()
	x, l, j, ev := newExecutor(domain.Policy{Projects: []string{"P1"}})
	if err := x.Assign(t.Context(), assignment("P2")); err != nil {
		t.Fatal(err)
	}
	if l.count() != 0 {
		t.Fatal("a foreign project's attempt was launched")
	}
	if k := ev.kinds(); !slices.Equal(k, []string{protocol.EventFailed}) || !strings.Contains(ev.last().body, "not allowed") {
		t.Fatalf("events %v %+v", k, ev.last())
	}
	if r, _ := j.List(); len(r) != 0 {
		t.Fatalf("journal %v", r)
	}
}

// execution.cancel stops the whole process group: SIGTERM first, SIGKILL after the grace period.
func TestCancelTerminatesThenKillsTheProcessGroup(t *testing.T) {
	t.Parallel()
	x, l, _, ev := newExecutor(domain.Policy{})
	if err := x.Assign(t.Context(), assignment("P1")); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(protocol.Cancel{Step: "S1", Fence: 1, Reason: protocol.CancelCancelled})
	if err := x.Command(t.Context(), protocol.Command{ID: "c", Kind: protocol.CommandCancel, Body: body}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "failed", func() bool { return slices.Contains(ev.kinds(), protocol.EventFailed) })
	if s := l.proc(0).sent(); !slices.Equal(s, []bool{false, true}) {
		t.Fatalf("signals %v", s)
	}
	if e := ev.last(); !strings.Contains(e.body, "cancelled") {
		t.Fatalf("failure %+v", e)
	}
	x.Cancel("S1", 1, "again") // ended: ignored
	x.Cancel("nope", 9, "")
}

func TestNewerTokenSupersedesARunningAttempt(t *testing.T) {
	t.Parallel()
	x, l, _, _ := newExecutor(domain.Policy{})
	l.termExit = true
	if err := x.Assign(t.Context(), assignment("P1")); err != nil {
		t.Fatal(err)
	}
	a := assignment("P1")
	a.Fence = 2
	if err := x.Assign(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if s := l.proc(0).sent(); len(s) == 0 || s[0] {
		t.Fatalf("old attempt signals %v", s)
	}
	waitFor(t, "one attempt left", func() bool { return len(x.Attempts()) == 1 && x.Attempts()[0].Fence == 2 })
	x.KillAll()
}

// After a runner restart a still-running process is adopted from the journal (no second launch) and its end is
// reported with event numbers after the reserved ones.
func TestRecoverAdoptsALiveProcess(t *testing.T) {
	t.Parallel()
	x, l, j, ev := newExecutor(domain.Policy{})
	if err := x.Assign(t.Context(), assignment("P1")); err != nil {
		t.Fatal(err)
	}
	x.Close() // the runner stops; the process keeps running
	live := l.proc(0)
	recs, _ := j.List()
	if len(recs) != 1 || recs[0].PID != live.pid || recs[0].Reserved == 0 {
		t.Fatalf("journal %+v", recs)
	}
	before := len(ev.kinds())

	l2 := &fakeLauncher{adopted: map[int]*fakeProc{live.pid: live}}
	x2 := &Executor{Launcher: l2, Journal: j, Events: ev}
	if err := x2.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if a := x2.Attempts(); len(a) != 1 || a[0].PID != live.pid {
		t.Fatalf("adopted %+v", a)
	}
	if err := x2.Assign(t.Context(), assignment("P1")); err != nil || l2.count() != 0 {
		t.Fatalf("re-assignment after restart launched %d (%v)", l2.count(), err)
	}
	live.end(domain.Exit{Code: 0})
	waitFor(t, "completed", func() bool { return len(ev.kinds()) > before })
	if e := ev.last(); e.kind != protocol.EventCompleted || e.seq <= recs[0].Reserved {
		t.Fatalf("after adoption %+v (reserved %d)", e, recs[0].Reserved)
	}
	waitFor(t, "journal cleared", func() bool { r, _ := j.List(); return len(r) == 0 })
}

func TestRecoverReportsDeadAndUnlaunchedAttempts(t *testing.T) {
	t.Parallel()
	j, ev := &memJournal{}, &fakeEvents{}
	_ = j.Save(domain.Record{Step: "gone", Fence: 1, PID: 999, PGID: 999, Reserved: 10})
	_ = j.Save(domain.Record{Step: "never", Fence: 2})
	x := &Executor{Launcher: &fakeLauncher{}, Journal: j, Events: ev}
	if err := x.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both reported", func() bool { r, _ := j.List(); return len(r) == 0 })
	ev.mu.Lock()
	defer ev.mu.Unlock()
	reasons := map[string]string{}
	for _, e := range ev.evs {
		if e.kind == protocol.EventFailed {
			reasons[e.step] = e.body
		}
	}
	if !strings.Contains(reasons["gone"], "process lost") || !strings.Contains(reasons["never"], "launch interrupted") {
		t.Fatalf("reasons %v", reasons)
	}
}

func TestUnknownCommandKindIsAcknowledged(t *testing.T) {
	t.Parallel()
	x, _, _, _ := newExecutor(domain.Policy{})
	if err := x.Command(t.Context(), protocol.Command{ID: "x", Kind: "plan.discuss"}); err != nil {
		t.Fatal(err)
	}
	if err := x.Command(t.Context(), protocol.Command{ID: "y", Kind: protocol.CommandAssign, Body: []byte(`{`)}); err != nil {
		t.Fatal(err)
	}
}

// A finished attempt's assignment re-delivered later (lost ACK, restored cursor) never runs again.
func TestFinishedAttemptIsNotRunAgain(t *testing.T) {
	t.Parallel()
	x, l, j, _ := newExecutor(domain.Policy{})
	if err := x.Assign(t.Context(), assignment("P1")); err != nil {
		t.Fatal(err)
	}
	l.proc(0).end(domain.Exit{Code: 0})
	waitFor(t, "finished", func() bool { done, _ := j.Finished("S1", 1); return done })
	if err := x.Assign(t.Context(), assignment("P1")); err != nil || l.count() != 1 {
		t.Fatalf("re-delivered assignment launched %d (%v)", l.count(), err)
	}
}

// A newer token waits until the older attempt of the step has ended: never two processes per step.
func TestSupersededAttemptEndsBeforeTheNewOneStarts(t *testing.T) {
	t.Parallel()
	x, l, _, _ := newExecutor(domain.Policy{})
	if err := x.Assign(t.Context(), assignment("P1")); err != nil {
		t.Fatal(err)
	}
	old := l.proc(0) // ignores SIGTERM: ends only with SIGKILL after the grace period
	a := assignment("P1")
	a.Fence = 2
	if err := x.Assign(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	select {
	case <-old.exit:
	default:
		t.Fatal("the new attempt started while the old one still ran")
	}
	if s := old.sent(); !slices.Equal(s, []bool{false, true}) || l.count() != 2 {
		t.Fatalf("signals %v, launches %d", s, l.count())
	}
	x.KillAll()
}

// Until the end of an attempt is reported it stays listed, with its result, so reconciliation never mistakes it
// for a lost process.
func TestUnreportedEndIsListedWithItsResult(t *testing.T) {
	t.Parallel()
	x, l, _, ev := newExecutor(domain.Policy{})
	if err := x.Assign(t.Context(), assignment("P1")); err != nil {
		t.Fatal(err)
	}
	ev.mu.Lock()
	ev.fail = true
	ev.mu.Unlock()
	l.proc(0).end(domain.Exit{Code: 0})
	waitFor(t, "result listed", func() bool {
		a := x.Attempts()
		return len(a) == 1 && !a[0].Running && a[0].Result == protocol.EventCompleted
	})
	ev.mu.Lock()
	ev.fail = false
	ev.mu.Unlock()
	waitFor(t, "reported", func() bool { return len(x.Attempts()) == 0 })
}

// KIN-63: a disabled or revoked runner stops every attempt itself (SIGTERM, SIGKILL after the grace period) and
// StopAll returns only once the processes ended, so a revoked runner never exits leaving them behind.
func TestStopAllTerminatesEveryAttemptAndWaits(t *testing.T) {
	t.Parallel()
	x, l, _, ev := newExecutor(domain.Policy{})
	for _, step := range []string{"S1", "S2"} {
		a := assignment("P1")
		a.Step = step
		if err := x.Assign(t.Context(), a); err != nil {
			t.Fatal(err)
		}
	}
	x.StopAll(protocol.CancelRunnerRevoked)
	for i := range 2 {
		select {
		case <-l.proc(i).exit:
		default:
			t.Fatalf("process %d still running after StopAll", i)
		}
		if s := l.proc(i).sent(); !slices.Equal(s, []bool{false, true}) {
			t.Fatalf("process %d signals %v", i, s)
		}
	}
	waitFor(t, "both failed", func() bool { return len(x.Attempts()) == 0 })
	if e := ev.last(); e.kind != protocol.EventFailed || !strings.Contains(e.body, protocol.CancelRunnerRevoked) {
		t.Fatalf("last event %+v", e)
	}
	x.StopAll(protocol.CancelRunnerRevoked) // nothing left: returns at once
}
