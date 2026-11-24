package domain

import (
	"net/netip"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// TestStatus is the state of a Test Network run.
type TestStatus string

const (
	TestPending  TestStatus = "pending"   // sent to the runner, no result yet
	TestOK       TestStatus = "ok"        // proxy, egress IP and every required endpoint passed
	TestFailed   TestStatus = "failed"    // see Code / Detail and the checks
	TestTimedOut TestStatus = "timed_out" // derived: pending for longer than TestTimeout (runner offline or busy)
)

// TestTimeout is how long a test may stay pending before it is shown as timed out.
const TestTimeout = 3 * time.Minute

// Failure codes the Control Plane sets itself (the runner reports its own, e.g. proxy_unreachable,
// proxy_auth_failed, diagnostic_failed).
const (
	CodeIPMismatch          = "egress_ip_mismatch"
	CodeEndpointUnreachable = "endpoint_unreachable"
	CodeFailed              = "failed"
)

// Limits on what a runner may report (its result is untrusted input).
const (
	MaxChecks      = 16
	MaxDetailRunes = 500
)

// Expected are the egress IPs a test compares against (a snapshot of the profile at request time).
type Expected struct {
	IPv4 string
	IPv6 string
}

// Check is the reachability of one required endpoint through the profile's proxy.
type Check struct {
	Name      string // anthropic, openai, github, github_api, youtrack, …
	Host      string
	OK        bool
	Code      string // failure code ("" when OK)
	LatencyMs int64
}

// Result is what a runner reports for a test.
type Result struct {
	OK           bool
	Code         string
	Detail       string
	ObservedIPv4 string
	ObservedIPv6 string
	Checks       []Check
	DurationMs   int64
}

// Test is one Test Network run of a profile on a runner.
type Test struct {
	ID           string
	RunnerID     string
	ProfileID    string // "" = the runner's effective profile
	RequestedBy  string
	RequestedAt  time.Time
	Expected     Expected
	Status       TestStatus
	Code         string
	Detail       string
	ObservedIPv4 string
	ObservedIPv6 string
	Checks       []Check
	DurationMs   int64
	CompletedAt  *time.Time
}

// NewTest starts a pending test.
func NewTest(id, runnerID, profileID, by string, exp Expected, now time.Time) Test {
	return Test{ID: id, RunnerID: runnerID, ProfileID: profileID, RequestedBy: by, RequestedAt: now.UTC(),
		Expected: exp, Status: TestPending}
}

// StatusAt is the status as shown at now: a pending test older than TestTimeout is timed out.
func (t Test) StatusAt(now time.Time) TestStatus {
	if t.Status == TestPending && !now.Before(t.RequestedAt.Add(TestTimeout)) {
		return TestTimedOut
	}
	return t.Status
}

var codeRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func cleanCode(s string) string {
	if s == "" || codeRE.MatchString(s) {
		return s
	}
	return "invalid"
}

func cleanText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return strings.TrimSpace(s)
}

func cleanIP(s string, v4 bool) string {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil || a.Zone() != "" {
		return ""
	}
	a = a.Unmap()
	if a.Is4() != v4 {
		return ""
	}
	return a.String()
}

// Complete records a runner's result. Every field is sanitized (bounded, no control characters, codes in the
// stable format) and the verdict is recomputed: an egress IP other than the expected one or an unreachable
// required endpoint fails the test whatever the runner claims.
func (t *Test) Complete(r Result, now time.Time) error {
	if t.Status != TestPending {
		return ErrTestFinished
	}
	t.Code, t.Detail = cleanCode(r.Code), cleanText(r.Detail, MaxDetailRunes)
	t.ObservedIPv4, t.ObservedIPv6 = cleanIP(r.ObservedIPv4, true), cleanIP(r.ObservedIPv6, false)
	t.DurationMs = max(r.DurationMs, 0)
	t.Checks = nil
	for i, c := range r.Checks {
		if i == MaxChecks {
			break
		}
		t.Checks = append(t.Checks, Check{Name: cleanText(c.Name, 32), Host: cleanText(c.Host, 253), OK: c.OK && c.Code == "",
			Code: cleanCode(c.Code), LatencyMs: max(c.LatencyMs, 0)})
	}
	ok := r.OK && t.Code == ""
	switch {
	case !ok:
		if t.Code == "" {
			t.Code = CodeFailed
		}
	case t.Expected.IPv4 != "" && t.ObservedIPv4 != t.Expected.IPv4,
		t.Expected.IPv6 != "" && t.ObservedIPv6 != t.Expected.IPv6:
		ok, t.Code = false, CodeIPMismatch
	default:
		for _, c := range t.Checks {
			if !c.OK {
				ok, t.Code = false, CodeEndpointUnreachable
				break
			}
		}
	}
	t.Status = TestFailed
	if ok {
		t.Status = TestOK
	}
	at := now.UTC()
	t.CompletedAt = &at
	return nil
}
