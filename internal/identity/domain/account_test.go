package domain

import (
	"bytes"
	"crypto/rand"
	"strings"
	"testing"
)

func TestNewTemporaryPassword(t *testing.T) {
	t.Parallel()
	a, err := NewTemporaryPassword(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewTemporaryPassword(rand.Reader)
	if a == b {
		t.Fatal("two temporary passwords are equal")
	}
	if err := ValidatePassword(a, "someone"); err != nil {
		t.Fatalf("temporary password fails the password policy: %v", err)
	}
	if strings.ContainsAny(a, "0O1lI") {
		t.Fatalf("ambiguous characters in %q", a)
	}
	if _, err := NewTemporaryPassword(bytes.NewReader(nil)); err == nil {
		t.Fatal("want error when randomness fails")
	}
}
