package domain

import (
	"bytes"
	"testing"
	"time"
)

func TestNewSession(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	plain, s, err := NewSession(bytes.NewReader(bytes.Repeat([]byte{3}, 64)), "user-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) < 40 || bytes.Equal(s.TokenHash, []byte(plain)) {
		t.Fatalf("plain %q / hash not derived", plain)
	}
	if !bytes.Equal(HashSessionToken(plain), s.TokenHash) {
		t.Fatal("HashSessionToken does not match stored hash")
	}
	if s.UserID != "user-1" || !s.CreatedAt.Equal(now) || !s.LastSeenAt.Equal(now) {
		t.Fatalf("session = %+v", s)
	}
}

func TestSessionValidity(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)
	s := Session{CreatedAt: start, LastSeenAt: start}
	tests := []struct {
		name     string
		lastSeen time.Time
		at       time.Time
		valid    bool
	}{
		{"fresh", start, start.Add(time.Minute), true},
		{"just before idle timeout", start, start.Add(SessionIdleTTL - time.Second), true},
		{"idle timeout", start, start.Add(SessionIdleTTL), false},
		{"active but past absolute lifetime", start.Add(SessionMaxAge - time.Hour), start.Add(SessionMaxAge), false},
		{"active within absolute lifetime", start.Add(6 * 24 * time.Hour), start.Add(6*24*time.Hour + time.Hour), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := s
			s.LastSeenAt = tt.lastSeen
			if got := s.Valid(tt.at); got != tt.valid {
				t.Fatalf("Valid = %v, want %v", got, tt.valid)
			}
		})
	}
	if s.NeedsTouch(start.Add(30*time.Second)) || !s.NeedsTouch(start.Add(2*time.Minute)) {
		t.Fatal("NeedsTouch must throttle last-seen updates to once a minute")
	}
	if got := s.ExpiresAt(start); !got.Equal(start.Add(SessionIdleTTL)) {
		t.Fatalf("ExpiresAt = %v", got)
	}
}
