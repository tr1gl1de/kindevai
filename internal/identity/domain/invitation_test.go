package domain

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestNewInvitationTokenIs256BitsAndOnlyTheHashIsKept(t *testing.T) {
	t.Parallel()
	rnd := bytes.NewReader(bytes.Repeat([]byte{7}, 32))
	plain, hash, err := NewInvitationToken(rnd)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 43 { // 32 bytes, unpadded base64url
		t.Fatalf("token length %d, want 43", len(plain))
	}
	if !bytes.Equal(hash, HashInvitationToken(plain)) || len(hash) != 32 {
		t.Fatal("hash must be SHA-256 of the plaintext")
	}
	if _, _, err := NewInvitationToken(bytes.NewReader(nil)); err == nil {
		t.Fatal("short randomness must fail")
	}
}

func TestInvitationTTL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		hours int
		want  time.Duration
		ok    bool
	}{
		{0, 72 * time.Hour, true}, // default
		{1, time.Hour, true},
		{168, 7 * 24 * time.Hour, true},
		{169, 0, false},
		{-1, 0, false},
	}
	for _, tt := range tests {
		got, err := InvitationTTL(tt.hours)
		if tt.ok != (err == nil) || got != tt.want {
			t.Errorf("InvitationTTL(%d) = %v, %v; want %v ok=%v", tt.hours, got, err, tt.want, tt.ok)
		}
		var ve *ValidationError
		if !tt.ok && (!errors.As(err, &ve) || ve.Code != "ttl_out_of_range") {
			t.Errorf("InvitationTTL(%d) error = %v, want ttl_out_of_range", tt.hours, err)
		}
	}
}

func TestInvitationState(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute)
	base := Invitation{CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}
	used, revoked, expired := base, base, base
	used.ConsumedAt = &past
	revoked.RevokedAt = &past
	expired.ExpiresAt = now

	tests := []struct {
		name string
		inv  Invitation
		want InvitationState
		err  error
	}{
		{"pending", base, InvitationPending, nil},
		{"used", used, InvitationUsed, ErrInvitationUsed},
		{"revoked", revoked, InvitationRevoked, ErrInvitationRevoked},
		{"expired at the boundary", expired, InvitationExpired, ErrInvitationExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.inv.State(now); got != tt.want {
				t.Fatalf("State = %q, want %q", got, tt.want)
			}
			if err := tt.inv.Usable(now); !errors.Is(err, tt.err) {
				t.Fatalf("Usable = %v, want %v", err, tt.err)
			}
		})
	}
}
