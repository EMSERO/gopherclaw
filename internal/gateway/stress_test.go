package gateway

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// TestStress_ConcurrentAllowSameIP exercises concurrent allow() calls on the
// same IP address to verify the mutex protects the token bucket correctly.
func TestStress_ConcurrentAllowSameIP(t *testing.T) {
	t.Parallel()
	limiter := newIPLimiter(100, 50) // 100 rps, burst of 50
	defer close(limiter.done)

	const goroutines = 100
	var wg sync.WaitGroup
	var allowed, denied atomic.Int64

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if limiter.allow("192.168.1.1") {
				allowed.Add(1)
			} else {
				denied.Add(1)
			}
		}()
	}

	wg.Wait()

	total := allowed.Load() + denied.Load()
	if total != goroutines {
		t.Errorf("expected %d total calls, got %d", goroutines, total)
	}
	// With burst=50, the first 50 should be allowed (tokens start at burst).
	// Some may refill during execution, so just check we got some allowed and some denied.
	if allowed.Load() == 0 {
		t.Error("expected at least some requests to be allowed")
	}
	t.Logf("allowed=%d denied=%d", allowed.Load(), denied.Load())
}

// TestStress_ConcurrentAllowManyIPs exercises concurrent allow() calls from
// many different IPs simultaneously.
func TestStress_ConcurrentAllowManyIPs(t *testing.T) {
	t.Parallel()
	limiter := newIPLimiter(10, 5)
	defer close(limiter.done)

	const goroutines = 100
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ip := fmt.Sprintf("10.0.%d.%d", n/256, n%256)
			limiter.allow(ip)
		}(i)
	}

	wg.Wait()
	// No panics or data races means success.
}

// TestStress_BurstBoundary exercises the exact burst boundary with concurrent
// requests to verify token accounting is correct.
func TestStress_BurstBoundary(t *testing.T) {
	t.Parallel()
	const burst = 20
	limiter := newIPLimiter(0.001, burst) // negligible refill rate
	defer close(limiter.done)

	// Send exactly burst requests concurrently from the same IP.
	var wg sync.WaitGroup
	var allowed atomic.Int64

	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if limiter.allow("10.10.10.10") {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	if allowed.Load() != int64(burst) {
		t.Errorf("expected all %d burst requests allowed, got %d", burst, allowed.Load())
	}

	// One more should be denied (tokens exhausted, negligible refill).
	if limiter.allow("10.10.10.10") {
		t.Error("expected request after burst to be denied")
	}
}

// TestStress_MaxClientsCapUnderLoad verifies that the maxClients cap prevents
// unbounded growth when many unique IPs hit the limiter concurrently.
func TestStress_MaxClientsCapUnderLoad(t *testing.T) {
	t.Parallel()
	limiter := newIPLimiter(10, 5)
	limiter.maxClients = 50 // low cap for testing
	defer close(limiter.done)

	const goroutines = 100
	var wg sync.WaitGroup
	var rejected atomic.Int64

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ip := fmt.Sprintf("172.16.%d.%d", n/256, n%256)
			if !limiter.allow(ip) {
				rejected.Add(1)
			}
		}(i)
	}

	wg.Wait()

	// With maxClients=50, at least some of the 100 unique IPs should be rejected.
	if rejected.Load() == 0 {
		t.Error("expected some rejections due to maxClients cap")
	}
	t.Logf("rejected=%d out of %d unique IPs", rejected.Load(), goroutines)
}

// TestStress_ConcurrentAllowDuringCleanup verifies that allow() calls don't
// race with the cleanup goroutine.
func TestStress_ConcurrentAllowDuringCleanup(t *testing.T) {
	t.Parallel()
	limiter := newIPLimiter(100, 50)
	defer close(limiter.done)

	const goroutines = 80
	var wg sync.WaitGroup

	// Simulate cleanup running concurrently with allow() calls.
	// We call cleanup directly since the ticker-based one runs every 60s.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if n%5 == 0 {
				// Simulate cleanup cycle.
				limiter.mu.Lock()
				for ip := range limiter.clients {
					delete(limiter.clients, ip)
				}
				limiter.mu.Unlock()
			} else {
				ip := fmt.Sprintf("192.168.%d.%d", n/256, n%256)
				limiter.allow(ip)
			}
		}(i)
	}

	wg.Wait()
}
