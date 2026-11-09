// Package throttle is the login gate's real backoff (lumen §8 rules:
// single user + public console = credential stuffing IS the attack; argon2id
// prices each guess, this makes guesses rare). In-memory per-IP and
// per-account counters with exponential backoff: deliberately per-replica —
// at 2 replicas the effective budget doubles, an acceptable honesty for
// blog scale (a durable variant would be a Postgres table; the ruling is
// recorded, not hidden).
//
// The edge rate-limit middleware (INFRA Phase 3) is the coarse first layer;
// this is the specific one: it knows WHAT an attempt is.
package throttle

import (
	"sync"
	"time"
)

type bucket struct {
	penaltyUntil time.Time
	consecutive  int
}

type Limiter struct {
	mu      sync.Mutex
	byKey   map[string]*bucket
	maxBack time.Duration
	now     func() time.Time
}

// New: maxBack caps the exponential (default 15 min via 0 → internal cap).
func New(maxBack time.Duration) *Limiter {
	if maxBack == 0 {
		maxBack = 15 * time.Minute
	}
	return &Limiter{byKey: map[string]*bucket{}, maxBack: maxBack, now: time.Now}
}

// Allow reports whether key may attempt now, and how long to retry-after.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.byKey[key]
	if b == nil {
		return true, 0
	}
	if d := b.penaltyUntil.Sub(l.now()); d > 0 {
		return false, d
	}
	return true, 0
}

// Fail records a rejected attempt: first 3 free, then 1s,2s,4s… capped.
func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.byKey[key]
	if b == nil {
		b = &bucket{}
		l.byKey[key] = b
	}
	b.consecutive++
	if b.consecutive <= 3 {
		return
	}
	backoff := time.Duration(1<<(b.consecutive-4)) * time.Second
	if backoff > l.maxBack {
		backoff = l.maxBack
	}
	b.penaltyUntil = l.now().Add(backoff)
}

// Success clears an account's history (the stuffing signal resets when the
// real owner shows up).
func (l *Limiter) Success(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byKey, key)
}
