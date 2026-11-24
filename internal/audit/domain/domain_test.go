package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var at = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func TestNewEvent(t *testing.T) {
	t.Parallel()
	user := Actor{Type: ActorUser, ID: "u1", Label: "owner"}
	ok, err := NewEvent(at, user, "invitation.created", "p1", "invitation:i1", map[string]string{"role": "member"})
	if err != nil || ok.Kind != "invitation.created" || ok.Details["role"] != "member" {
		t.Fatalf("NewEvent = %+v, %v", ok, err)
	}
	long := strings.Repeat("x", MaxDetailLen+10)
	e, err := NewEvent(at, Actor{Type: ActorSystem}, "runner.revoked", "", "", map[string]string{"reason": long})
	if err != nil || len(e.Details["reason"]) > MaxDetailLen || !strings.HasSuffix(e.Details["reason"], "…") {
		t.Fatalf("long detail not truncated: %v", err)
	}
	if e.Actor.Label != "system" {
		t.Fatalf("system actor label = %q", e.Actor.Label)
	}

	tooMany := map[string]string{}
	for i := range MaxDetails + 1 {
		tooMany[strings.Repeat("k", i+1)] = "v"
	}
	for _, tc := range []struct {
		name    string
		actor   Actor
		kind    string
		details map[string]string
	}{
		{"no kind", user, "", nil},
		{"kind without namespace", user, "created", nil},
		{"kind with spaces", user, "user created", nil},
		{"upper-case kind", user, "User.Created", nil},
		{"unknown actor type", Actor{Type: "robot", ID: "x"}, "user.created", nil},
		{"user actor without id", Actor{Type: ActorUser}, "user.created", nil},
		{"bad detail key", user, "user.created", map[string]string{"a b": "v"}},
		{"too many details", user, "user.created", tooMany},
	} {
		if _, err := NewEvent(at, tc.actor, tc.kind, "", "", tc.details); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("%s: err = %v, want ErrInvalidEvent", tc.name, err)
		}
	}
	if _, err := NewEvent(time.Time{}, user, "user.created", "", "", nil); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("zero time: err = %v", err)
	}
}

func TestNewFilter(t *testing.T) {
	t.Parallel()
	f, err := NewFilter(Filter{})
	if err != nil || f.Limit != DefaultPageSize {
		t.Fatalf("default = %+v, %v", f, err)
	}
	if f, _ := NewFilter(Filter{Limit: 10_000}); f.Limit != MaxPageSize {
		t.Fatalf("limit not capped: %d", f.Limit)
	}
	for _, bad := range []Filter{
		{Limit: -1},
		{Before: -5},
		{From: at, To: at.Add(-time.Hour)},
		{Kind: "not a kind"},
		{Actor: strings.Repeat("a", 200)},
	} {
		if _, err := NewFilter(bad); !errors.Is(err, ErrInvalidFilter) {
			t.Errorf("NewFilter(%+v) = %v, want ErrInvalidFilter", bad, err)
		}
	}
}
