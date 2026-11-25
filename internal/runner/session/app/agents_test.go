package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/session/domain"
)

// fakeAgents prepares fake CLI commands and replays scripted output for the follower.
type fakeAgents struct {
	mu        sync.Mutex
	prepared  []domain.AgentRequest
	prompts   []string
	perms     []domain.Permissions
	follows   []AgentFollow
	events    []domain.AgentEvent // what the CLI's output parses into
	stdout    string
	result    domain.AgentResult
	err       error // Prepare fails
	followErr error // Follow fails
}

func (f *fakeAgents) Prepare(dir string, req domain.AgentRequest, prompt string, p domain.Permissions) (AgentCommand, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return AgentCommand{}, f.err
	}
	if req.Workdir == "" {
		req.Workdir = dir + "/work"
	}
	req.PromptFile = dir + "/prompt.md"
	f.prepared, f.prompts, f.perms = append(f.prepared, req), append(f.prompts, prompt), append(f.perms, p)
	return AgentCommand{Path: "/usr/bin/" + req.Agent, Args: []string{"-p"}, Dir: req.Workdir, PromptFile: req.PromptFile,
		Timeout: req.Timeout}, nil
}

type fakeFollower struct {
	f *fakeAgents
	h AgentFollow
}

func (f *fakeAgents) Follow(_, _ string, h AgentFollow) (AgentFollower, error) {
	if f.followErr != nil {
		return nil, f.followErr
	}
	f.mu.Lock()
	f.follows = append(f.follows, h)
	f.mu.Unlock()
	return &fakeFollower{f, h}, nil
}

// Finish sends the scripted events and output (only what lies beyond From, as the real follower does).
func (ff *fakeFollower) Finish(exit domain.Exit, cancelled string) domain.AgentResult {
	f := ff.f
	f.mu.Lock()
	evs, out, res := f.events, f.stdout, f.result
	f.mu.Unlock()
	if ff.h.From.Stdout == 0 {
		for _, e := range evs {
			ff.h.OnEvent(e)
		}
		_, _ = io.WriteString(ff.h.Stdout, out)
	}
	if ff.h.Progress != nil {
		ff.h.Progress(domain.OutputOffsets{Stdout: int64(len(out))})
	}
	res.Exit = exit
	if cancelled != "" {
		res.Failure = &domain.AgentFailure{Code: domain.FailureCancelled, Message: cancelled}
	}
	return res
}

func cliAssignment(agent string) domain.Assignment {
	a := assignment("P1")
	a.Adapter, a.Scenario, a.Prompt, a.Model = agent, "", "Fix the bug", "sonnet"
	return a
}

func newCLIExecutor() (*Executor, *fakeLauncher, *memJournal, *fakeEvents, *fakeAgents) {
	x, l, j, ev := newExecutor(domain.Policy{})
	ag := &fakeAgents{result: domain.AgentResult{Session: "sess-1", Model: "claude-sonnet", Version: "2.1.0",
		Usage: domain.Usage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 5, CostUSD: 0.25, Turns: 3}}}
	x.Agents = ag
	return x, l, j, ev, ag
}

func (e *fakeEvents) ofKind(kind string) []event {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []event
	for _, v := range e.evs {
		if v.kind == kind {
			out = append(out, v)
		}
	}
	return out
}

func TestCLIAssignmentRunsTheCLIWithThePromptOutsideTheWorktree(t *testing.T) {
	t.Parallel()
	x, l, _, ev, ag := newCLIExecutor()
	a := cliAssignment(domain.AgentCodex)
	a.Resume, a.Timeout = "sess-0", time.Minute
	if err := x.Assign(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if len(ag.prepared) != 1 {
		t.Fatalf("prepared %d CLI runs", len(ag.prepared))
	}
	req := ag.prepared[0]
	if req.Agent != domain.AgentCodex || req.Model != "sonnet" || req.Resume != "sess-0" || req.Timeout != time.Minute ||
		ag.prompts[0] != "Fix the bug" || ag.perms[0] != domain.PermissionsSafe {
		t.Fatalf("request %+v prompt %q perms %v", req, ag.prompts[0], ag.perms[0])
	}
	spec := l.launched[0]
	if spec.Agent == nil || spec.Agent.PromptFile != "/work/S1-1/prompt.md" || spec.Workdir != "/work/S1-1/work" {
		t.Fatalf("launch spec %+v", spec)
	}
	started := ev.ofKind(protocol.EventStarted)
	if len(started) != 1 || !strings.Contains(started[0].body, `"agent":"codex"`) {
		t.Fatalf("started %+v", started)
	}
	l.proc(0).end(domain.Exit{})
	waitFor(t, "completed", func() bool { return len(ev.ofKind(protocol.EventCompleted)) == 1 })
}

func TestCLIEventsAndUsageReachTheControlPlane(t *testing.T) {
	t.Parallel()
	x, l, _, ev, ag := newCLIExecutor()
	x.KnownSecrets = func() []string { return []string{"hunter2-proxy-pass"} }
	ag.events = []domain.AgentEvent{
		{Kind: domain.EventSession, Session: "sess-1", Model: "claude-sonnet", Version: "2.1.0"},
		{Kind: domain.EventText, Text: "the proxy password is hunter2-proxy-pass " + strings.Repeat("x", 20<<10)},
		{Kind: domain.EventToolUse, Tool: "Edit", Input: `{"file":"a.go"}`},
		{Kind: domain.EventResult, Text: "done", Usage: &domain.Usage{InputTokens: 100}},
	}
	ag.stdout = `{"type":"result"}` + "\n"
	if err := x.Assign(t.Context(), cliAssignment(domain.AgentClaude)); err != nil {
		t.Fatal(err)
	}
	l.proc(0).end(domain.Exit{})
	waitFor(t, "completed", func() bool { return len(ev.ofKind(protocol.EventCompleted)) == 1 })
	agent := ev.ofKind(protocol.EventAgent)
	if len(agent) != 4 {
		t.Fatalf("agent events %+v", agent)
	}
	var text protocol.AgentEvent
	if err := json.Unmarshal([]byte(agent[1].body), &text); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text.Text, "hunter2-proxy-pass") || len(text.Text) > protocol.MaxAgentText+8 {
		t.Fatalf("text event not masked or not bounded: %d bytes", len(text.Text))
	}
	if !strings.Contains(agent[2].body, `"tool":"Edit"`) || !strings.Contains(agent[0].body, `"version":"2.1.0"`) {
		t.Fatalf("events %+v", agent)
	}
	var done protocol.Completed
	if err := json.Unmarshal([]byte(ev.ofKind(protocol.EventCompleted)[0].body), &done); err != nil {
		t.Fatal(err)
	}
	if done.Agent == nil || done.Agent.Session != "sess-1" || done.Agent.Version != "2.1.0" || done.Agent.Usage == nil ||
		done.Agent.Usage.InputTokens != 100 || done.Agent.Usage.CostUSD != 0.25 || done.Agent.Usage.Turns != 3 {
		t.Fatalf("completed %+v", done.Agent)
	}
	if out := ev.ofKind(protocol.EventOutput); len(out) == 0 {
		t.Fatal("the raw output was not streamed")
	}
	// The agent events come before the terminal event.
	kinds := ev.kinds()
	if kinds[len(kinds)-1] != protocol.EventCompleted {
		t.Fatalf("kinds %v", kinds)
	}
}

func TestCLILimitFailsWithItsCodeCauseAndResetTime(t *testing.T) {
	t.Parallel()
	x, l, _, ev, ag := newCLIExecutor()
	resets := time.Date(2026, 11, 25, 18, 0, 0, 0, time.UTC)
	ag.result.Failure = &domain.AgentFailure{Code: domain.FailureCLILimit, Cause: domain.CauseRateLimit,
		Message: "usage limit reached", ResetsAt: resets}
	if err := x.Assign(t.Context(), cliAssignment(domain.AgentClaude)); err != nil {
		t.Fatal(err)
	}
	l.proc(0).end(domain.Exit{Code: 1})
	waitFor(t, "failed", func() bool { return len(ev.ofKind(protocol.EventFailed)) == 1 })
	var f protocol.Failure
	if err := json.Unmarshal([]byte(ev.ofKind(protocol.EventFailed)[0].body), &f); err != nil {
		t.Fatal(err)
	}
	if f.Code != protocol.FailureCLILimit || f.Cause != "rate_limit" || f.ResetsAt != resets.UnixMilli() ||
		!strings.HasPrefix(f.Reason, "cli_limit: rate_limit") || f.Agent == nil || f.Agent.Session != "sess-1" {
		t.Fatalf("failure %+v", f)
	}
}

func TestCLICancelIsReportedAsBefore(t *testing.T) {
	t.Parallel()
	x, l, _, ev, _ := newCLIExecutor()
	l.termExit = true
	if err := x.Assign(t.Context(), cliAssignment(domain.AgentClaude)); err != nil {
		t.Fatal(err)
	}
	x.Cancel("S1", 1, protocol.CancelCancelled)
	waitFor(t, "failed", func() bool { return len(ev.ofKind(protocol.EventFailed)) == 1 })
	var f protocol.Failure
	_ = json.Unmarshal([]byte(ev.ofKind(protocol.EventFailed)[0].body), &f)
	if f.Reason != "cancelled: cancelled" || f.Code != protocol.FailureCancelled {
		t.Fatalf("failure %+v", f)
	}
}

func TestPermissionBypassOnlyInsideTheStrictSandbox(t *testing.T) {
	t.Parallel()
	x, l, _, ev, ag := newCLIExecutor()
	a := cliAssignment(domain.AgentClaude)
	a.BypassPermissions = true
	if err := x.Assign(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if l.count() != 0 || len(ag.prepared) != 0 {
		t.Fatal("the CLI ran with the bypass outside the sandbox")
	}
	if f := ev.last(); f.kind != protocol.EventFailed || !strings.Contains(f.body, "permission bypass refused") {
		t.Fatalf("last event %+v", f)
	}

	x2, l2, _, _, ag2 := newCLIExecutor()
	x2.Egress = &fakeConfiner{box: &fakeSandbox{}}
	if err := x2.Assign(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if len(ag2.perms) != 1 || ag2.perms[0] != domain.PermissionsBypass || l2.launched[0].Sandbox == nil {
		t.Fatalf("perms %v", ag2.perms)
	}
	l2.proc(0).end(domain.Exit{})
}

func TestCLIAssignmentWithoutTheCLIFails(t *testing.T) {
	t.Parallel()
	x, l, _, ev, _ := newCLIExecutor()
	x.Agents = nil
	if err := x.Assign(t.Context(), cliAssignment(domain.AgentCodex)); err != nil {
		t.Fatal(err)
	}
	if l.count() != 0 || ev.last().kind != protocol.EventFailed {
		t.Fatalf("launched %d, events %v", l.count(), ev.kinds())
	}
	x3, _, _, ev3, ag3 := newCLIExecutor()
	ag3.err = errors.New("no codex: " + ErrNoAgent.Error())
	if err := x3.Assign(t.Context(), cliAssignment(domain.AgentCodex)); err != nil {
		t.Fatal(err)
	}
	if ev3.last().kind != protocol.EventFailed {
		t.Fatal(ev3.kinds())
	}
}

// A sandboxed CLI without a login (KIN-86) fails before it starts like a logged-out CLI: cli_limit with cause auth,
// so the Control Plane holds the step and alerts the owner (KIN-83) instead of spending the retry budget.
func TestSandboxedCLIWithoutALoginIsHeldForAuth(t *testing.T) {
	t.Parallel()
	x, l, _, ev, ag := newCLIExecutor()
	x.Egress = &fakeConfiner{box: &fakeSandbox{}}
	ag.err = fmt.Errorf("%w: claude has no login", ErrAgentLoggedOut)
	if err := x.Assign(t.Context(), cliAssignment(domain.AgentClaude)); err != nil {
		t.Fatal(err)
	}
	last := ev.last()
	if l.count() != 0 || last.kind != protocol.EventFailed {
		t.Fatalf("launched %d, events %v", l.count(), ev.kinds())
	}
	var f protocol.Failure
	if err := json.Unmarshal([]byte(last.body), &f); err != nil {
		t.Fatal(err)
	}
	if f.Code != protocol.FailureCLILimit || f.Cause != domain.CauseAuth {
		t.Fatalf("failure %+v", f)
	}
	// The executor told the adapter the run is confined (only then is a missing login certain).
	x2, _, _, _, ag2 := newCLIExecutor()
	x2.Egress = &fakeConfiner{box: &fakeSandbox{}}
	a := cliAssignment(domain.AgentClaude)
	if err := x2.Assign(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	if len(ag2.prepared) != 1 || !ag2.prepared[0].Confined {
		t.Fatalf("prepared %+v", ag2.prepared)
	}
}

// After a restart the adopted CLI attempt's output is read on from the recorded offsets and its result reported.
func TestRecoveredCLIAttemptIsFollowedFromItsOffsets(t *testing.T) {
	t.Parallel()
	x, l, j, ev, ag := newCLIExecutor()
	ag.events = []domain.AgentEvent{{Kind: domain.EventText, Text: "already sent"}}
	p := &fakeProc{pid: 42, exit: make(chan struct{})}
	l.adopted = map[int]*fakeProc{42: p}
	_ = j.Save(domain.Record{Step: "S1", Fence: 1, Execution: "E1", Project: "P1", PID: 42, PGID: 42, Agent: domain.AgentClaude,
		Output: domain.OutputOffsets{Stdout: 300, Stderr: 7}})
	if err := x.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(ag.follows) != 1 || ag.follows[0].From.Stdout != 300 || ag.follows[0].From.Stderr != 7 {
		t.Fatalf("follows %+v", ag.follows)
	}
	p.end(domain.Exit{Code: 0})
	waitFor(t, "completed", func() bool { return len(ev.ofKind(protocol.EventCompleted)) == 1 })
	if n := len(ev.ofKind(protocol.EventAgent)); n != 0 {
		t.Fatalf("%d events sent again", n)
	}
	if !strings.Contains(ev.ofKind(protocol.EventCompleted)[0].body, "sess-1") {
		t.Fatal(ev.ofKind(protocol.EventCompleted)[0].body)
	}
}

func TestCLIOutputOffsetsAreRecorded(t *testing.T) {
	t.Parallel()
	x, l, j, ev, ag := newCLIExecutor()
	ag.stdout = "0123456789"
	var saved []domain.Record
	if err := x.Assign(t.Context(), cliAssignment(domain.AgentClaude)); err != nil {
		t.Fatal(err)
	}
	recs, _ := j.List()
	saved = append(saved, recs...)
	if len(saved) != 1 || saved[0].Agent != domain.AgentClaude {
		t.Fatalf("record %+v", saved)
	}
	l.proc(0).end(domain.Exit{})
	waitFor(t, "completed", func() bool { return len(ev.ofKind(protocol.EventCompleted)) == 1 })
}

// Offsets reached after the executor closed are not recorded: that output was not sent, the next runner process
// reads on from the last recorded offsets.
func TestCLIOutputOffsetsAreNotRecordedAfterClose(t *testing.T) {
	t.Parallel()
	x, l, j, _, ag := newCLIExecutor()
	if err := x.Assign(t.Context(), cliAssignment(domain.AgentClaude)); err != nil {
		t.Fatal(err)
	}
	ag.follows[0].Progress(domain.OutputOffsets{Stdout: 5})
	x.Close()
	ag.follows[0].Progress(domain.OutputOffsets{Stdout: 99})
	recs, _ := j.List()
	if len(recs) != 1 || recs[0].Output.Stdout != 5 {
		t.Fatalf("records %+v", recs)
	}
	l.proc(0).end(domain.Exit{})
}

// A CLI attempt whose output cannot be read is stopped and fails: its result would be unknown.
func TestCLIAttemptWithoutItsOutputIsStopped(t *testing.T) {
	t.Parallel()
	x, l, _, ev, ag := newCLIExecutor()
	l.termExit = true
	ag.followErr = errors.New("unknown dialect")
	if err := x.Assign(t.Context(), cliAssignment(domain.AgentClaude)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "failed", func() bool { return len(ev.ofKind(protocol.EventFailed)) == 1 })
	if f := ev.ofKind(protocol.EventFailed)[0]; !strings.Contains(f.body, "cannot read the CLI's output") {
		t.Fatalf("failure %s", f.body)
	}
}

// Fields the CLI reported are bounded (and masked): a huge session id cannot make the terminal event too large.
func TestCLIReportFieldsAreBounded(t *testing.T) {
	t.Parallel()
	x, l, _, ev, ag := newCLIExecutor()
	huge := strings.Repeat("s", 200<<10)
	ag.events = []domain.AgentEvent{{Kind: domain.EventSession, Session: huge, Model: huge, Version: huge}}
	ag.result.Session, ag.result.Model, ag.result.Version = huge, huge, huge
	ag.result.Failure = &domain.AgentFailure{Code: domain.FailureCLILimit, Cause: huge, Message: huge}
	if err := x.Assign(t.Context(), cliAssignment(domain.AgentClaude)); err != nil {
		t.Fatal(err)
	}
	l.proc(0).end(domain.Exit{Code: 1})
	waitFor(t, "failed", func() bool { return len(ev.ofKind(protocol.EventFailed)) == 1 })
	for _, e := range append(ev.ofKind(protocol.EventAgent), ev.ofKind(protocol.EventFailed)...) {
		if len(e.body) > 4<<10 {
			t.Fatalf("%s body of %d bytes", e.kind, len(e.body))
		}
	}
}

// KIN-34: the CLI's final answer (the planner's plan) reaches the Control Plane with execution.completed, masked
// and cut so the event body stays within the channel's limit even when every byte escapes.
func TestCLIFinalAnswerIsReportedMaskedAndBounded(t *testing.T) {
	t.Parallel()
	for name, text := range map[string]string{
		"plan":    `{"summary":"s","token":"hunter2-proxy-pass"}`,
		"escapes": strings.Repeat("\x01", 2*protocol.MaxAgentResult),
		"long":    strings.Repeat(`{"k":"é"}`, protocol.MaxAgentResult),
	} {
		x, l, _, ev, ag := newCLIExecutor()
		x.KnownSecrets = func() []string { return []string{"hunter2-proxy-pass"} }
		ag.result.Text = text
		if err := x.Assign(t.Context(), cliAssignment(domain.AgentClaude)); err != nil {
			t.Fatal(err)
		}
		l.proc(0).end(domain.Exit{})
		waitFor(t, "completed", func() bool { return len(ev.ofKind(protocol.EventCompleted)) == 1 })
		body := ev.ofKind(protocol.EventCompleted)[0].body
		var done protocol.Completed
		if err := json.Unmarshal([]byte(body), &done); err != nil || done.Agent == nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(body) > protocol.MaxBodyBytes || len(done.Agent.Result) > protocol.MaxAgentResult ||
			strings.Contains(done.Agent.Result, "hunter2-proxy-pass") || !utf8.ValidString(done.Agent.Result) {
			t.Fatalf("%s: body %d bytes, result %d bytes", name, len(body), len(done.Agent.Result))
		}
		if name == "plan" && !strings.Contains(done.Agent.Result, `"summary":"s"`) {
			t.Fatalf("plan lost: %q", done.Agent.Result)
		}
	}
}
