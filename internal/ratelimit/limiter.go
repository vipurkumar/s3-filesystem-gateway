// Copyright 2024 s3-filesystem-gateway contributors
// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"sync"
	"time"
)

// Limiter implements a token bucket rate limiter.
type Limiter struct {
	mu       sync.Mutex
	rate     float64
	burst    int
	tokens   float64
	lastTime time.Time
	disabled bool
}

// NewLimiter creates a rate limiter. rate=0 disables limiting.
func NewLimiter(rate float64, burst int) *Limiter {
	if rate <= 0 {
		return &Limiter{disabled: true}
	}
	return &Limiter{
		rate:     rate,
		burst:    burst,
		tokens:   float64(burst),
		lastTime: time.Now(),
	}
}

// Allow returns true if the request is allowed under the rate limit.
func (l *Limiter) Allow() bool {
	if l.disabled {
		return true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(l.lastTime).Seconds()
	l.tokens += elapsed * l.rate
	if l.tokens > float64(l.burst) {
		l.tokens = float64(l.burst)
	}
	l.lastTime = now

	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}
