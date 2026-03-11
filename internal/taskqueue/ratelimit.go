package taskqueue

import (
	"context"
	"sync"
	"time"
)

// RateLimiter implements a token-bucket algorithm for rate limiting API calls.
type RateLimiter struct {
	mu       sync.Mutex
	tokens   float64
	maxBurst float64
	rate     float64 // tokens per second
	lastTime time.Time
}

// NewRateLimiter creates a rate limiter.
// rate: requests per second, burst: max concurrent burst.
func NewRateLimiter(rate float64, burst int) *RateLimiter {
	return &RateLimiter{
		tokens:   float64(burst),
		maxBurst: float64(burst),
		rate:     rate,
		lastTime: time.Now(),
	}
}

// Wait blocks until a token is available or ctx is cancelled.
func (rl *RateLimiter) Wait(ctx context.Context) error {
	for {
		rl.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(rl.lastTime).Seconds()
		rl.tokens += elapsed * rl.rate
		if rl.tokens > rl.maxBurst {
			rl.tokens = rl.maxBurst
		}
		rl.lastTime = now

		if rl.tokens >= 1 {
			rl.tokens--
			rl.mu.Unlock()
			return nil
		}

		// Calculate wait time for next token
		deficit := 1 - rl.tokens
		wait := time.Duration(deficit / rl.rate * float64(time.Second))
		rl.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
			// Try again
		}
	}
}
