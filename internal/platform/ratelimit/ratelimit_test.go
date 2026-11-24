package ratelimit

import (
	"fmt"
	"testing"
	"time"
)

func TestLimiterBurstAndRefill(t *testing.T) {
	t.Parallel()
	now := time.Unix(0, 0)
	l := New(Config{Burst: 3, Every: time.Minute, MaxKeys: 100}, func() time.Time { return now })
	for i := range 3 {
		if !l.Allow("a") {
			t.Fatalf("request %d denied within burst", i)
		}
	}
	if l.Allow("a") {
		t.Fatal("4th request allowed")
	}
	if !l.Allow("b") {
		t.Fatal("other key affected")
	}
	now = now.Add(time.Minute)
	if !l.Allow("a") {
		t.Fatal("token not refilled after interval")
	}
	if l.Allow("a") {
		t.Fatal("refilled more than one token")
	}
}

func TestLimiterBoundsMemory(t *testing.T) {
	t.Parallel()
	now := time.Unix(0, 0)
	l := New(Config{Burst: 1, Every: time.Hour, MaxKeys: 10}, func() time.Time { return now })
	for i := range 1000 {
		l.Allow(fmt.Sprint(i))
	}
	if n := l.Len(); n > 10 {
		t.Fatalf("tracked keys = %d, want <= 10", n)
	}
}
