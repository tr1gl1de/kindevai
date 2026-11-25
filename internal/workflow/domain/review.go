package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
)

// The implementation and review loop of a plan step (KIN-35, KB KIN-A-15 "Implementation and review steps"). A step
// whose implementer runs on an AI CLI goes through phases on one work branch (feature/<task>--<step>):
//
//	implement (CLI, then the repository's test command run by the runner) → review (another CLI, read-only, on the
//	diff base..head) → approve: SUCCEEDED | changes_requested: a fix round (implement again with the findings) …
//
// at most MaxFixRounds fix rounds; a review that still requests changes after the last one fails the step. The
// runner that holds the branch keeps the step (PinnedRunner): the branch lives in its repository cache until it is
// pushed (KIN-36). Test evidence is what the runner measured (exit code, output tail), never the model's claim.

// Step phases.
const (
	PhaseImplement = ""
	PhaseReview    = "review"
)

// MaxFixRounds bounds the fix rounds after a review requested changes (owner decision in KIN-35: 2).
const MaxFixRounds = 2

// Bounds of the artifacts kept on an attempt.
const (
	MaxAgentResult    = 16 << 10 // bytes of the CLI's final answer kept with the attempt
	MaxFindings       = 20
	maxFindingLen     = 500 // runes of a finding's message
	maxVerdictSummary = 2000
	maxHandoffSummary = 4000
	maxHandoffItem    = 500
	MaxTestOutput     = 4 << 10 // bytes of the test output's tail kept as evidence
	maxTestCommand    = 500
	MaxChangedFiles   = 200
)

// TestRun is the runner's objective test evidence for an attempt: the project's test command run in the worktree
// after the CLI ended.
type TestRun struct {
	Command    string
	ExitCode   int
	TimedOut   bool
	DurationMS int64
	Output     string // the output's tail, redacted on the runner and the Control Plane
}

// Passed reports a run that exited 0 in time.
func (r TestRun) Passed() bool { return !r.TimedOut && r.ExitCode == 0 }

// Test evidence statuses of an implementer attempt.
const (
	TestsPassed        = "passed"
	TestsFailed        = "failed"
	TestsMissing       = "missing"        // a test command was sent but the runner reported no result
	TestsNotConfigured = "not_configured" // the repository has no test command: a visible gap, not a pass
)

// WorkChanges is what an attempt's final workspace.changes reported: the work branch's base and head and the
// changed paths (at most MaxChangedFiles listed; Total counts all).
type WorkChanges struct {
	BaseSHA string
	HeadSHA string
	Files   []string
	Total   int
}

// StepClaim is what the scheduler decided for a claim: the AI CLI, whether the step runs the review loop (a
// routed implementer step on an AI CLI) and the repository's test command ("" when none is configured).
type StepClaim struct {
	Agent       AgentChoice
	Review      bool
	TestCommand string
}

// ClaimStep is ClaimWith with the review loop's decisions: the attempt's role and test command follow the step's
// phase (a review sends no test command).
func (e *Execution) ClaimStep(stepID, runnerID string, c StepClaim, now time.Time, ttl time.Duration) (Lease, error) {
	s := e.Step(stepID)
	if s != nil && s.PinnedRunner != "" && s.PinnedRunner != runnerID && e.State == ExecutionRunning {
		return Lease{}, fmt.Errorf("step %s is pinned to runner %s: %w", s.Key, s.PinnedRunner, ErrInvalidTransition)
	}
	l, err := e.ClaimWith(stepID, runnerID, c.Agent, now, ttl)
	if err != nil {
		return Lease{}, err
	}
	s = e.Step(stepID)
	s.Review = s.Review || c.Review
	s.TestCommand = ""
	if s.Review && s.Phase == PhaseImplement {
		// Never cut: a prefix of a shell command is another command. The registry bounds it; a longer one is refused
		// by the runner and the attempt fails with that reason.
		s.TestCommand = strings.TrimSpace(c.TestCommand)
	}
	return l, nil
}

// restartReview starts the step's review loop over (KIN-35): the runner that held its work branch is gone, so the
// next attempt implements again from the base on another runner, with fresh budgets.
func (s *Step) restartReview() {
	s.PinnedRunner, s.Phase, s.Round, s.AttemptBase = "", PhaseImplement, 0, len(s.Attempts)
}

// attemptRole is the role of the step's next attempt: "" outside the review loop.
func (s *Step) attemptRole() string {
	switch {
	case !s.Review:
		return ""
	case s.Phase == PhaseReview:
		return StepRoleReviewer
	}
	return StepRoleImplementer
}

// RecordTests stores the runner's test evidence with the attempt of runnerID under fence (any state: evidence of
// a late attempt is still shown). It reports whether the attempt changed.
func (e *Execution) RecordTests(stepID, runnerID string, fence int64, r TestRun) bool {
	a := e.attemptFor(stepID, runnerID, fence)
	if a == nil {
		return false
	}
	r.Command = cutUTF8(r.Command, maxTestCommand)
	if len(r.Output) > MaxTestOutput {
		r.Output = tailUTF8(r.Output, MaxTestOutput)
	}
	a.Tests = &r
	return true
}

// RecordChanges stores the attempt's final changes report (base, head, files). It reports whether it changed.
func (e *Execution) RecordChanges(stepID, runnerID string, fence int64, c WorkChanges) bool {
	a := e.attemptFor(stepID, runnerID, fence)
	if a == nil {
		return false
	}
	if len(c.Files) > MaxChangedFiles {
		c.Files = c.Files[:MaxChangedFiles]
	}
	a.BaseSHA, a.HeadSHA = cutUTF8(c.BaseSHA, 64), cutUTF8(c.HeadSHA, 64)
	a.Files, a.FilesTotal = slices.Clone(c.Files), max(c.Total, len(c.Files))
	return true
}

// TestStatus is the attempt's test evidence: "" for an attempt that is no implementation of the review loop.
func (a *Attempt) TestStatus() string {
	switch {
	case a.Role != StepRoleImplementer:
		return ""
	case a.Tests != nil && a.Tests.Passed():
		return TestsPassed
	case a.Tests != nil:
		return TestsFailed
	case a.TestCommand != "":
		return TestsMissing
	}
	return TestsNotConfigured
}

// lastImplementation is the latest succeeded implementer attempt: the work the review looks at.
func (s *Step) lastImplementation() *Attempt {
	for i := len(s.Attempts) - 1; i >= s.AttemptBase; i-- {
		if a := &s.Attempts[i]; a.Role == StepRoleImplementer && a.State == AttemptSucceeded {
			return a
		}
	}
	return nil
}

// Author is the AI CLI the Control Plane assigned to the step's latest succeeded implementation ("" before one):
// the reviewer is routed to another CLI when possible (KIN-33 different-from-author).
func (s *Step) Author() string {
	if a := s.lastImplementation(); a != nil {
		return a.Agent
	}
	return ""
}

// ReviewedWork is the base and head of the latest succeeded implementation (empty without a workspace report).
func (s *Step) ReviewedWork() (base, head string) {
	if a := s.lastImplementation(); a != nil {
		return a.BaseSHA, a.HeadSHA
	}
	return "", ""
}

// LastImplementation returns a copy of the latest succeeded implementer attempt.
func (s *Step) LastImplementation() (Attempt, bool) {
	if a := s.lastImplementation(); a != nil {
		return *a, true
	}
	return Attempt{}, false
}

// LastReview returns a copy of the latest reviewer attempt that gave a verdict, and the verdict.
func (s *Step) LastReview() (Attempt, Review, bool) {
	for i := len(s.Attempts) - 1; i >= s.AttemptBase; i-- {
		a := s.Attempts[i]
		if a.Role != StepRoleReviewer || a.State != AttemptSucceeded {
			continue
		}
		if r, err := ParseVerdict(a.Report.Result); err == nil {
			return a, r, true
		}
	}
	return Attempt{}, Review{}, false
}

// RetryFeedback is what the step's next attempt is told: the findings of the review that asked for this fix round
// and why the previous attempt of the same phase failed.
func (s *Step) RetryFeedback() string {
	var b strings.Builder
	if s.Phase == PhaseImplement && s.Round > 0 {
		if _, r, ok := s.LastReview(); ok {
			b.WriteString(r.Render())
		}
	}
	if a := s.lastAttempt(); a != nil && a.State == AttemptFailed && a.Role == s.attemptRole() && a.Round == s.Round {
		b.WriteString("The previous attempt failed: " + a.Error + "\n")
		if a.Tests != nil && !a.Tests.Passed() && a.Tests.Output != "" {
			b.WriteString("Test output (tail):\n" + a.Tests.Output + "\n")
		}
	}
	return b.String()
}

// finishPhase decides the end of a reported attempt of the review loop (Succeed): it returns done when the step
// succeeded, else the step went on (next phase, fix round, retry) or failed.
func (e *Execution) finishPhase(s *Step, now time.Time, maxAttempts int) (done bool) {
	a := s.lastAttempt()
	if s.Phase == PhaseImplement {
		switch {
		case a.TestCommand != "" && a.Tests == nil:
			e.failAttempt(s, fmt.Sprintf("the runner reported no test result for %s", a.TestCommand), now, maxAttempts)
			return false
		case a.Tests != nil && !a.Tests.Passed():
			e.failAttempt(s, testFailure(*a.Tests), now, maxAttempts)
			return false
		}
		s.finishAttempt(AttemptSucceeded, "", now)
		s.Phase, s.PinnedRunner, s.RunnerID = PhaseReview, s.RunnerID, ""
		s.move(StepReady)
		return false
	}
	if _, head := s.ReviewedWork(); head != "" && a.HeadSHA != "" && a.HeadSHA != head {
		e.failAttempt(s, "the reviewer changed the work branch although it may only read", now, maxAttempts)
		return false
	}
	r, err := ParseVerdict(a.Report.Result)
	if err != nil {
		e.failAttempt(s, "the reviewer's answer is no valid verdict: "+err.Error(), now, maxAttempts)
		return false
	}
	s.finishAttempt(AttemptSucceeded, "", now)
	s.RunnerID = ""
	if r.Verdict == VerdictApprove {
		return true
	}
	if s.Round >= MaxFixRounds {
		msg := fmt.Sprintf("the review still requests changes after %d fix rounds: %s", MaxFixRounds, r.Findings[0].Message)
		s.move(StepFailed)
		s.Reason = reasonText(msg)
		e.finish(ExecutionFailed, reasonText(fmt.Sprintf("step %s failed: %s", s.Key, msg)), now)
		return false
	}
	s.Round++
	s.Phase = PhaseImplement
	s.move(StepReady)
	return false
}

func testFailure(r TestRun) string {
	msg := fmt.Sprintf("tests failed: %s exited with %d", r.Command, r.ExitCode)
	if r.TimedOut {
		msg = fmt.Sprintf("tests failed: %s timed out", r.Command)
	}
	if last := lastLine(r.Output); last != "" {
		msg += ": " + last
	}
	return msg
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return cutUTF8(strings.TrimSpace(lines[len(lines)-1]), 300)
}

func tailUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// Verdict is a reviewer's decision.
type Verdict string

const (
	VerdictApprove          Verdict = "approve"
	VerdictChangesRequested Verdict = "changes_requested"
)

// Finding is one review finding. File is repository-relative ("" for a general finding), Line 0 when unknown.
type Finding struct {
	Severity string `json:"severity"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Message  string `json:"message"`
}

// Finding severities.
var findingSeverities = []string{"blocker", "major", "minor"}

// Review is a reviewer's structured verdict (its final answer, parsed strictly).
type Review struct {
	Verdict  Verdict   `json:"verdict"`
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
}

// Render is the verdict as the next implementer attempt reads it.
func (r Review) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "The independent review requested changes: %s\n", r.Summary)
	for _, f := range r.Findings {
		loc := f.File
		if loc != "" && f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		if loc != "" {
			loc = " " + loc
		}
		fmt.Fprintf(&b, "- [%s]%s %s\n", f.Severity, loc, f.Message)
	}
	return b.String()
}

// jsonObject finds the JSON object in a CLI's final answer (optionally in a code fence or among prose).
func jsonObject(answer string) (string, bool) {
	s := strings.TrimSpace(answer)
	start, end := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}')
	if start < 0 || end < start {
		return "", false
	}
	return s[start : end+1], true
}

// ParseVerdict parses a reviewer's final answer strictly: one JSON object {verdict, summary, findings}; unknown
// fields, unknown verdicts or severities, control characters and oversized fields are refused, and
// changes_requested needs at least one finding. A ValidationError says what is wrong.
func ParseVerdict(answer string) (Review, error) {
	obj, ok := jsonObject(answer)
	if !ok {
		return Review{}, invalid("verdict", "verdict_missing")
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(obj)))
	dec.DisallowUnknownFields()
	var r Review
	if err := dec.Decode(&r); err != nil {
		return Review{}, invalid("verdict", "verdict_json")
	}
	r.Summary = strings.TrimSpace(r.Summary)
	switch {
	case r.Verdict != VerdictApprove && r.Verdict != VerdictChangesRequested:
		return Review{}, invalid("verdict", "verdict_unknown")
	case !printable(r.Summary, maxVerdictSummary):
		return Review{}, invalid("summary", "summary_format")
	case len(r.Findings) > MaxFindings:
		return Review{}, invalid("findings", "too_many")
	case r.Verdict == VerdictChangesRequested && len(r.Findings) == 0:
		return Review{}, invalid("findings", "findings_required")
	case r.Verdict == VerdictApprove && slices.ContainsFunc(r.Findings, func(f Finding) bool { return f.Severity == "blocker" }):
		return Review{}, invalid("verdict", "approve_with_blocker")
	}
	for i := range r.Findings {
		f := &r.Findings[i]
		f.Message, f.File = strings.TrimSpace(f.Message), strings.TrimSpace(f.File)
		switch {
		case !slices.Contains(findingSeverities, f.Severity):
			return Review{}, invalid("findings", "severity_unknown")
		case f.Message == "" || !printable(f.Message, maxFindingLen):
			return Review{}, invalid("findings", "message_format")
		case f.Line < 0:
			return Review{}, invalid("findings", "line_format")
		case f.File != "" && (!printable(f.File, 300) || path.IsAbs(f.File) || strings.Contains(f.File, "\n") ||
			slices.Contains(strings.Split(f.File, "/"), "..")):
			return Review{}, invalid("findings", "file_format")
		}
	}
	return r, nil
}

// Handoff is the implementer's handoff artifact (KB KIN-A-15): the model's own summary, documentation impact and
// open questions. The objective parts (base/head, changed files, test evidence) are the runner's reports on the
// attempt.
type Handoff struct {
	Summary       string   `json:"summary"`
	Docs          []string `json:"docs"`
	OpenQuestions []string `json:"openQuestions"`
}

// ParseHandoff reads the implementer's final answer leniently: the JSON object {summary, docs, openQuestions}, or
// the answer's text as the summary. Every field is bounded.
func ParseHandoff(answer string) Handoff {
	var h Handoff
	if obj, ok := jsonObject(answer); ok && json.Unmarshal([]byte(obj), &h) == nil && strings.TrimSpace(h.Summary) != "" {
		h.Summary = boundText(h.Summary, maxHandoffSummary)
		h.Docs, h.OpenQuestions = boundList(h.Docs), boundList(h.OpenQuestions)
		return h
	}
	return Handoff{Summary: boundText(answer, maxHandoffSummary)}
}

func boundText(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func boundList(xs []string) []string {
	var out []string
	for _, x := range xs {
		if x = boundText(x, maxHandoffItem); x != "" && len(out) < maxListItems {
			out = append(out, x)
		}
	}
	return out
}
