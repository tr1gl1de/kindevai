package domain

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNewLogin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"admin", "admin", true},
		{"  Max.Ivanov_1 ", "max.ivanov_1", true},
		{"ab", "", false},
		{strings.Repeat("a", 65), "", false},
		{"bad login", "", false},
		{"имя", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got, err := NewLogin(tt.in)
			if tt.ok != (err == nil) {
				t.Fatalf("err = %v, ok want %v", err, tt.ok)
			}
			if tt.ok && string(got) != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
			var ve *ValidationError
			if !tt.ok && (!errors.As(err, &ve) || ve.Field != "login") {
				t.Fatalf("want ValidationError on login, got %v", err)
			}
		})
	}
}

func TestValidatePassword(t *testing.T) {
	t.Parallel()
	login := Login("owner")
	tests := []struct {
		name, pw string
		ok       bool
	}{
		{"good", "correct horse battery", true},
		{"unicode counts runes", "пароль-пароль", true},
		{"too short", "short1", false},
		{"too long", strings.Repeat("x", 257), false},
		{"equals login", "owner", false},
		{"login padded", "ownerownerowner", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidatePassword(tt.pw, login)
			if tt.ok != (err == nil) {
				t.Fatalf("err = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func TestNewPlatformName(t *testing.T) {
	t.Parallel()
	if n, err := NewPlatformName("  Моя платформа "); err != nil || n != "Моя платформа" {
		t.Fatalf("got %q, %v", n, err)
	}
	for _, bad := range []string{"", "   ", strings.Repeat("я", 81), "a\x00b", "line\nbreak"} {
		if _, err := NewPlatformName(bad); err == nil {
			t.Fatalf("NewPlatformName(%q) accepted", bad)
		}
	}
}

func TestSetupToken(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	plain, tok, err := NewSetupToken(bytes.NewReader(bytes.Repeat([]byte{7}, 64)), now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) < 40 || strings.Contains(string(tok.Hash), plain) {
		t.Fatalf("plain %q too short or hash contains plaintext", plain)
	}
	if !tok.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("ExpiresAt = %v", tok.ExpiresAt)
	}
	tests := []struct {
		name  string
		plain string
		at    time.Time
		want  error
	}{
		{"valid", plain, now.Add(time.Minute), nil},
		{"wrong", plain + "x", now, ErrInvalidSetupToken},
		{"empty", "", now, ErrInvalidSetupToken},
		{"expired", plain, now.Add(time.Hour), ErrInvalidSetupToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tok.Verify(tt.plain, tt.at); !errors.Is(err, tt.want) {
				t.Fatalf("Verify = %v, want %v", err, tt.want)
			}
		})
	}
	if !tok.Expired(now.Add(2 * time.Hour)) {
		t.Fatal("Expired = false after TTL")
	}
}

func TestSetupStateVerify(t *testing.T) {
	t.Parallel()
	now := time.Now()
	plain, tok, _ := NewSetupToken(bytes.NewReader(bytes.Repeat([]byte{1}, 64)), now, time.Hour)
	tests := []struct {
		name  string
		state SetupState
		want  error
	}{
		{"completed", SetupState{Completed: true, Token: &tok}, ErrSetupCompleted},
		{"no token", SetupState{}, ErrInvalidSetupToken},
		{"ok", SetupState{Token: &tok}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.state.Verify(plain, now); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}
