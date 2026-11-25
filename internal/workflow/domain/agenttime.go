package domain

import (
	"regexp"
	"time"
)

// Agent time accounting (KIN-25, owner requirement of 23.11.2026, KIN-A-14): an attempt's work period is the time
// its CLI process ran, from the runner's execution.started event to its execution.completed/failed event, both on
// the runner's clock (one clock for both ends, so clock skew does not change a duration and a late delivery after
// a reconnect does not inflate it). Waiting, blocking and approvals are not agent time. Each end is recorded once:
// a redelivered event never changes it. A period whose end is unknown (the process was lost, or its end has not
// arrived yet after a cancellation) or whose time could not be recorded is shown as incomplete, never as a
// successful record.

// WorkStatus says how far an attempt's (or a sum's) agent time is known.
type WorkStatus string

const (
	WorkNone       WorkStatus = "none"       // no CLI process ran (e.g. the runner refused the assignment)
	WorkRunning    WorkStatus = "running"    // the process runs; the time counts up to now
	WorkComplete   WorkStatus = "complete"   // start and end recorded
	WorkIncomplete WorkStatus = "incomplete" // a period is missing an end or a usable time
)

// MinWorkTime and MaxWorkSkew bound a usable runner event time: older than MinWorkTime or later than the Control
// Plane's clock plus MaxWorkSkew is not recorded and makes the period incomplete.
var MinWorkTime = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

const MaxWorkSkew = 24 * time.Hour

// MaxWorkPeriod bounds one attempt's work period: a longer one (a runner clock jump, or a runner reporting
// implausible times) is not counted and makes the period incomplete.
const MaxWorkPeriod = 24 * time.Hour

var agentRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,63}$`)

func usableWorkTime(at, now time.Time) bool {
	return !at.Before(MinWorkTime) && !at.After(now.Add(MaxWorkSkew))
}

// attemptFor returns the attempt of the step that runnerID ran under fence, in any execution state (an end may be
// reported after the execution closed).
func (e *Execution) attemptFor(stepID, runnerID string, fence int64) *Attempt {
	s := e.Step(stepID)
	if s == nil {
		return nil
	}
	for i := range s.Attempts {
		if a := &s.Attempts[i]; a.Fence == fence && a.RunnerID == runnerID {
			return a
		}
	}
	return nil
}

// MarkWorkStart records when the runner started the attempt's CLI process (at, runner clock) and which agent
// (CLI adapter) ran it. Only the first mark counts; it reports whether the attempt changed. An unusable time
// marks the period incomplete.
func (e *Execution) MarkWorkStart(stepID, runnerID string, fence int64, agent string, at, now time.Time) bool {
	a := e.attemptFor(stepID, runnerID, fence)
	if a == nil || a.WorkStart != nil || a.WorkIncomplete {
		return false
	}
	// The CLI the Control Plane assigned stays authoritative (KIN-33): a runner's report fills it only when the
	// attempt has none (no routing).
	if a.Agent == "" && agentRe.MatchString(agent) {
		a.Agent = agent
	}
	if !usableWorkTime(at, now) {
		a.WorkIncomplete = true
		return true
	}
	at = at.UTC()
	a.WorkStart = &at
	return true
}

// MarkWorkEnd records when the attempt's process ended (at, runner clock). It needs a recorded start, counts only
// once and is accepted after the step or execution closed (a cancelled attempt's process reports its end later).
func (e *Execution) MarkWorkEnd(stepID, runnerID string, fence int64, at, now time.Time) bool {
	a := e.attemptFor(stepID, runnerID, fence)
	if a == nil || a.WorkStart == nil || a.WorkEnd != nil || a.WorkIncomplete {
		return false
	}
	if !usableWorkTime(at, now) {
		a.WorkIncomplete = true
		return true
	}
	at = at.UTC()
	a.WorkEnd = &at
	return true
}

// Work returns the attempt's agent time status and duration (a running period counts up to now).
func (a Attempt) Work(now time.Time) (WorkStatus, time.Duration) {
	running := a.State == AttemptRunning || a.State == AttemptUnknown
	switch {
	case a.WorkIncomplete:
		return WorkIncomplete, 0
	case a.WorkStart == nil && a.WorkEnd == nil && !running:
		return WorkNone, 0
	case a.WorkStart == nil:
		return WorkIncomplete, 0 // a started attempt whose start was never recorded
	case a.WorkEnd == nil && running:
		return WorkRunning, min(max(0, now.Sub(*a.WorkStart)), MaxWorkPeriod)
	case a.WorkEnd == nil, a.WorkEnd.Before(*a.WorkStart), a.WorkEnd.Sub(*a.WorkStart) > MaxWorkPeriod:
		return WorkIncomplete, 0
	}
	return WorkComplete, a.WorkEnd.Sub(*a.WorkStart)
}

// AgentTime is a sum of agent time: the total of the known periods and the combined status.
type AgentTime struct {
	Total  time.Duration
	Status WorkStatus
}

var workRank = map[WorkStatus]int{"": 0, WorkNone: 1, WorkComplete: 2, WorkRunning: 3, WorkIncomplete: 4}

// Add adds one period: an incomplete part makes the sum incomplete, else a running part makes it running.
func (t *AgentTime) Add(s WorkStatus, d time.Duration) {
	t.Total += d
	if workRank[s] > workRank[t.Status] {
		t.Status = s
	}
}

// Merge adds another sum.
func (t *AgentTime) Merge(o AgentTime) { t.Add(o.Status, o.Total) }

// AgentTime sums the step's attempts.
func (s Step) AgentTime(now time.Time) AgentTime {
	t := AgentTime{Status: WorkNone}
	for _, a := range s.Attempts {
		t.Add(a.Work(now))
	}
	return t
}

// AgentTime sums the execution's steps: agent time adds parallel work, so it can exceed the calendar duration.
func (e Execution) AgentTime(now time.Time) AgentTime {
	t := AgentTime{Status: WorkNone}
	for _, s := range e.Steps {
		t.Merge(s.AgentTime(now))
	}
	return t
}

func clonePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}
