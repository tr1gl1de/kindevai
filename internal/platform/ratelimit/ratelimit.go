// Package ratelimit is an in-memory per-key token bucket with a bounded number of tracked keys.
package ratelimit

import (
	"slices"
	"sync"
	"time"
)

// Config: each key may make Burst requests at once and regains one every Every. MaxKeys bounds memory.
type Config struct {
	Burst   int
	Every   time.Duration
	MaxKeys int
}

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter is safe for concurrent use.
type Limiter struct {
	cfg     Config
	now     func() time.Time
	mu      sync.Mutex
	buckets map[string]*bucket
}

// New returns a Limiter; now is injected for tests (time.Now in production).
func New(cfg Config, now func() time.Time) *Limiter {
	return &Limiter{cfg: cfg, now: now, buckets: make(map[string]*bucket)}
}

// Allow consumes one token for key and reports whether the request may proceed.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.cfg.MaxKeys {
			l.evict()
		}
		b = &bucket{tokens: float64(l.cfg.Burst), last: now}
		l.buckets[key] = b
	}
	b.tokens = min(float64(l.cfg.Burst), b.tokens+float64(now.Sub(b.last))/float64(l.cfg.Every))
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Peek reports whether key has a token available without consuming it or starting to track the key.
func (l *Limiter) Peek(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		return l.cfg.Burst > 0
	}
	return b.tokens+float64(l.now().Sub(b.last))/float64(l.cfg.Every) >= 1
}

// evict drops the least recently used half of the keys.
func (l *Limiter) evict() {
	type kv struct {
		k string
		t time.Time
	}
	all := make([]kv, 0, len(l.buckets))
	for k, b := range l.buckets {
		all = append(all, kv{k, b.last})
	}
	slices.SortFunc(all, func(a, b kv) int { return a.t.Compare(b.t) })
	for _, e := range all[:len(all)/2+1] {
		delete(l.buckets, e.k)
	}
}

// Len is the number of tracked keys.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
