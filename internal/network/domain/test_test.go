package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tr1gl1de/kindevai/internal/network/domain"
)

var t0 = time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)

func pending() domain.Test {
	return domain.NewTest("t1", "r1", "p1", "u1", domain.Expected{IPv4: "203.0.113.7"}, t0)
}

func TestCompleteRecordsObservedIPAndChecks(t *testing.T) {
	t.Parallel()
	tt := pending()
	err := tt.Complete(domain.Result{OK: true, ObservedIPv4: "203.0.113.7", DurationMs: 1200,
		Checks: []domain.Check{{Name: "github", Host: "github.com", OK: true, LatencyMs: 80}}}, t0.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if tt.Status != domain.TestOK || tt.ObservedIPv4 != "203.0.113.7" || tt.CompletedAt == nil || len(tt.Checks) != 1 {
		t.Fatalf("test = %+v", tt)
	}
	if err := tt.Complete(domain.Result{OK: true}, t0); !errors.Is(err, domain.ErrTestFinished) {
		t.Fatalf("second result: %v", err)
	}
}

func TestCompleteDoesNotTrustTheRunnerVerdict(t *testing.T) {
	t.Parallel()
	tt := pending()
	// The runner claims success but the observed egress IP is not the expected one.
	if err := tt.Complete(domain.Result{OK: true, ObservedIPv4: "198.51.100.1"}, t0); err != nil {
		t.Fatal(err)
	}
	if tt.Status != domain.TestFailed || tt.Code != domain.CodeIPMismatch {
		t.Fatalf("test = %+v, want failed with %s", tt, domain.CodeIPMismatch)
	}
	// A failed required endpoint fails the test too.
	tt = pending()
	_ = tt.Complete(domain.Result{OK: true, ObservedIPv4: "203.0.113.7", Checks: []domain.Check{{Name: "youtrack", Host: "x.example", Code: "timeout"}}}, t0)
	if tt.Status != domain.TestFailed || tt.Code != domain.CodeEndpointUnreachable {
		t.Fatalf("test = %+v, want failed with %s", tt, domain.CodeEndpointUnreachable)
	}
}

func TestCompleteSanitizesUntrustedFields(t *testing.T) {
	t.Parallel()
	tt := pending()
	checks := make([]domain.Check, 40)
	for i := range checks {
		checks[i] = domain.Check{Name: "n\x1b[31m", Host: strings.Repeat("h", 500), Code: "Bad Code!"}
	}
	_ = tt.Complete(domain.Result{OK: false, Code: "PROXY\nAUTH", Detail: strings.Repeat("é", 2000) + "\x00",
		ObservedIPv4: "not-an-ip", ObservedIPv6: "203.0.113.7", Checks: checks, DurationMs: -5}, t0)
	if tt.Code != "invalid" || len([]rune(tt.Detail)) > domain.MaxDetailRunes || strings.ContainsRune(tt.Detail, 0) {
		t.Fatalf("code %q detail len %d", tt.Code, len([]rune(tt.Detail)))
	}
	if tt.ObservedIPv4 != "" || tt.ObservedIPv6 != "" || tt.DurationMs != 0 {
		t.Fatalf("observed %q %q duration %d", tt.ObservedIPv4, tt.ObservedIPv6, tt.DurationMs)
	}
	if len(tt.Checks) != domain.MaxChecks || strings.ContainsRune(tt.Checks[0].Name, 0x1b) || len(tt.Checks[0].Host) > 253 ||
		tt.Checks[0].Code != "invalid" {
		t.Fatalf("checks = %d %+v", len(tt.Checks), tt.Checks[0])
	}
}

func TestPendingTestTimesOut(t *testing.T) {
	t.Parallel()
	tt := pending()
	if tt.StatusAt(t0.Add(domain.TestTimeout-time.Second)) != domain.TestPending {
		t.Fatal("expired too early")
	}
	if tt.StatusAt(t0.Add(domain.TestTimeout)) != domain.TestTimedOut {
		t.Fatal("not expired after the timeout")
	}
}
