package taskqueue

import (
	"context"
	"testing"
	"time"
)

func TestRateLimiterBasic(t *testing.T) {
	rl := NewRateLimiter(100, 5) // 100/s, burst 5

	// Should allow burst of 5 immediately
	for i := range 5 {
		if err := rl.Wait(context.Background()); err != nil {
			t.Fatalf("burst request %d failed: %v", i, err)
		}
	}

	// 6th should be delayed (tokens exhausted)
	start := time.Now()
	if err := rl.Wait(context.Background()); err != nil {
		t.Fatalf("delayed request failed: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 5*time.Millisecond {
		t.Errorf("expected delay for 6th request, got %v", elapsed)
	}
}

func TestRateLimiterContextCancel(t *testing.T) {
	rl := NewRateLimiter(1, 1) // 1/s, burst 1

	// Exhaust the burst
	_ = rl.Wait(context.Background())

	// Next should block; cancel via context
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := rl.Wait(ctx)
	if err == nil {
		t.Error("expected context error")
	}
}

func TestRateLimiterRefill(t *testing.T) {
	rl := NewRateLimiter(100, 2) // 100/s, burst 2

	// Exhaust burst
	_ = rl.Wait(context.Background())
	_ = rl.Wait(context.Background())

	// Wait for refill
	time.Sleep(25 * time.Millisecond) // ~2.5 tokens at 100/s

	// Should succeed immediately
	start := time.Now()
	if err := rl.Wait(context.Background()); err != nil {
		t.Fatalf("refill request failed: %v", err)
	}
	if time.Since(start) > 5*time.Millisecond {
		t.Error("refilled request should be immediate")
	}
}
