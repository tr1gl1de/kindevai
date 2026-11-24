package domain

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func TestNewEnrollmentTokenIs256BitsAndOnlyTheHashIsKept(t *testing.T) {
	t.Parallel()
	tok, plain, err := NewEnrollmentToken(bytes.NewReader(bytes.Repeat([]byte{7}, 32)), "t1", "owner1", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 43 { // 32 bytes, unpadded base64url
		t.Fatalf("token length %d, want 43", len(plain))
	}
	if !bytes.Equal(tok.TokenHash, HashEnrollmentToken(plain)) || len(tok.TokenHash) != 32 {
		t.Fatal("only the SHA-256 of the plaintext is kept")
	}
	if !tok.ExpiresAt.Equal(now.Add(10*time.Minute)) || tok.CreatedBy != "owner1" || tok.ID != "t1" {
		t.Fatalf("token = %+v", tok)
	}
	if _, _, err := NewEnrollmentToken(bytes.NewReader(nil), "t1", "owner1", now); err == nil {
		t.Fatal("short randomness must fail")
	}
}

func TestEnrollmentTokenState(t *testing.T) {
	t.Parallel()
	past := now.Add(-time.Minute)
	base := EnrollmentToken{CreatedAt: now.Add(-5 * time.Minute), ExpiresAt: now.Add(5 * time.Minute)}
	used, revoked, expired, usedLate := base, base, base, base
	used.ConsumedAt, used.RunnerID = &past, "r1"
	revoked.RevokedAt = &past
	expired.ExpiresAt = now
	usedLate.ConsumedAt, usedLate.ExpiresAt = &past, past
	tests := []struct {
		name string
		tok  EnrollmentToken
		want TokenState
		err  error
	}{
		{"pending", base, TokenPending, nil},
		{"used", used, TokenUsed, ErrTokenUsed},
		{"revoked", revoked, TokenRevoked, ErrTokenRevoked},
		{"expired at the deadline", expired, TokenExpired, ErrTokenExpired},
		{"used wins over expired", usedLate, TokenUsed, ErrTokenUsed},
	}
	for _, tt := range tests {
		if got := tt.tok.State(now); got != tt.want {
			t.Errorf("%s: State = %s, want %s", tt.name, got, tt.want)
		}
		if err := tt.tok.Usable(now); !errors.Is(err, tt.err) {
			t.Errorf("%s: Usable = %v, want %v", tt.name, err, tt.err)
		}
	}
}

// The fingerprint matches `ssh-keygen -lf` for the same key in OpenSSH format, so the Owner can compare it with
// what the runner machine prints.
func TestParsePublicKeyAndOpenSSHFingerprint(t *testing.T) {
	t.Parallel()
	k, err := ParsePublicKey("2hznQhRgvaYtn8K3hLubpx7uDIisg76parXO5pADfQA=")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := k.Fingerprint(), "SHA256:WdYR/VIHb2knhxnmd3MKU6WpNAryvOmk4PylABREeIs"; got != want {
		t.Fatalf("Fingerprint = %s, want %s", got, want)
	}
	if k.String() != "2hznQhRgvaYtn8K3hLubpx7uDIisg76parXO5pADfQA=" {
		t.Fatalf("String = %s", k.String())
	}
	bad := []string{
		"",
		"not base64!",
		base64.StdEncoding.EncodeToString(make([]byte, 31)),
		base64.StdEncoding.EncodeToString(make([]byte, 33)),
		base64.StdEncoding.EncodeToString(make([]byte, 32)), // all zeros is not a usable key
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINoc50IUYL2mLZ/Ct4S7m6ce7gyIrIO+qWq1zuaQA30A",
	}
	for _, s := range bad {
		var ve *ValidationError
		if _, err := ParsePublicKey(s); !errors.As(err, &ve) || ve.Field != "publicKey" {
			t.Errorf("ParsePublicKey(%q) = %v, want publicKey validation error", s, err)
		}
	}
}

func TestNewMetadata(t *testing.T) {
	t.Parallel()
	m, err := NewMetadata(" build-box.local ", "linux", "amd64", "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if m != (Metadata{Hostname: "build-box.local", OS: "linux", Arch: "amd64", Version: "0.1.0"}) {
		t.Fatalf("metadata = %+v", m)
	}
	if m, err := NewMetadata("h", "darwin", "arm64", ""); err != nil || m.Version != "" {
		t.Fatalf("version is optional: %+v, %v", m, err)
	}
	bad := []struct{ host, os, arch, version, field string }{
		{"", "linux", "amd64", "1", "hostname"},
		{strings.Repeat("a", 254), "linux", "amd64", "1", "hostname"},
		{"bad\nhost", "linux", "amd64", "1", "hostname"},
		{"h", "", "amd64", "1", "os"},
		{"h", "Linux 6.1", "amd64", "1", "os"},
		{"h", "linux", "", "1", "arch"},
		{"h", "linux", "amd64", strings.Repeat("1", 65), "version"},
		{"h", "linux", "amd64", "v1\x00", "version"},
	}
	for _, b := range bad {
		var ve *ValidationError
		if _, err := NewMetadata(b.host, b.os, b.arch, b.version); !errors.As(err, &ve) || ve.Field != b.field {
			t.Errorf("NewMetadata(%q,%q,%q,%q) = %v, want %s error", b.host, b.os, b.arch, b.version, err, b.field)
		}
	}
}

func pendingRunner(t *testing.T) Runner {
	t.Helper()
	k, err := ParsePublicKey("2hznQhRgvaYtn8K3hLubpx7uDIisg76parXO5pADfQA=")
	if err != nil {
		t.Fatal(err)
	}
	tok := EnrollmentToken{ID: "t1", CreatedBy: "owner1", ExpiresAt: now.Add(time.Minute)}
	return Enroll("r1", tok, k, Metadata{Hostname: "h", OS: "linux", Arch: "amd64"}, now)
}

func TestEnrollCreatesAPendingRunnerOwnedByTheTokenIssuer(t *testing.T) {
	t.Parallel()
	r := pendingRunner(t)
	if r.Status != StatusPending || r.CreatedBy != "owner1" || r.EnrollmentTokenID != "t1" || !r.EnrolledAt.Equal(now) {
		t.Fatalf("runner = %+v", r)
	}
	if r.Fingerprint != "SHA256:WdYR/VIHb2knhxnmd3MKU6WpNAryvOmk4PylABREeIs" {
		t.Fatalf("fingerprint = %s", r.Fingerprint)
	}
	if r.MayConnect() || r.MayRun("p1") {
		t.Fatal("a pending runner may not connect or run anything")
	}
}

func TestRunnerLifecycle(t *testing.T) {
	t.Parallel()
	later := now.Add(time.Hour)
	r := pendingRunner(t)
	if err := r.Approve(nil, "owner1", later); !isValidation(err, "projectIds", "projects_required") {
		t.Fatalf("approve without projects = %v", err)
	}
	if err := r.Approve([]string{"p1", "p1", "p2"}, "owner1", later); err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusApproved || r.ApprovedBy != "owner1" || !r.ApprovedAt.Equal(later) || len(r.ProjectIDs) != 2 {
		t.Fatalf("approved runner = %+v", r)
	}
	if !r.MayConnect() || !r.MayRun("p1") || r.MayRun("p3") {
		t.Fatal("an approved runner runs only its projects")
	}
	if err := r.Approve([]string{"p1"}, "owner1", later); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("approving twice = %v", err)
	}
	if err := r.Enable(later); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("enabling an approved runner = %v", err)
	}
	if err := r.Disable(later); err != nil || r.Status != StatusDisabled || r.MayConnect() || r.MayRun("p1") {
		t.Fatalf("disable = %v, %+v", err, r)
	}
	if err := r.SetProjects([]string{"p3"}); err != nil || r.MayRun("p1") {
		t.Fatalf("projects of a disabled runner can change: %v", err)
	}
	if err := r.Enable(later); err != nil || r.Status != StatusApproved || !r.MayRun("p3") {
		t.Fatalf("enable = %v, %+v", err, r)
	}
	if err := r.SetProjects(nil); !isValidation(err, "projectIds", "projects_required") {
		t.Fatalf("empty project list = %v", err)
	}
	if err := r.Revoke("owner1", later); err != nil || r.Status != StatusRevoked || r.RevokedAt == nil || r.MayConnect() {
		t.Fatalf("revoke = %v, %+v", err, r)
	}
	for name, op := range map[string]func() error{
		"approve":  func() error { return r.Approve([]string{"p1"}, "owner1", later) },
		"disable":  func() error { return r.Disable(later) },
		"enable":   func() error { return r.Enable(later) },
		"revoke":   func() error { return r.Revoke("owner1", later) },
		"projects": func() error { return r.SetProjects([]string{"p1"}) },
	} {
		if err := op(); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s after revoke = %v, want ErrInvalidTransition", name, err)
		}
	}
}

func TestPendingRunnerCanBeRevokedButNotDisabled(t *testing.T) {
	t.Parallel()
	r := pendingRunner(t)
	if err := r.Disable(now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("disable pending = %v", err)
	}
	if err := r.SetProjects([]string{"p1"}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("projects of a pending runner are set by approval: %v", err)
	}
	if err := r.Revoke("owner1", now); err != nil || r.Status != StatusRevoked {
		t.Fatalf("revoke pending = %v", err)
	}
}

func isValidation(err error, field, code string) bool {
	var ve *ValidationError
	return errors.As(err, &ve) && ve.Field == field && ve.Code == code
}
