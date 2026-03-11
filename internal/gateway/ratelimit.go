package gateway

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ipLimiter tracks request timestamps per IP for rate limiting.
type ipLimiter struct {
	mu         sync.Mutex
	clients    map[string]*bucket
	rps        float64
	burst      int
	maxClients int // cap on tracked IPs to prevent memory exhaustion under IP-spray attacks
	done       chan struct{}
	wg         sync.WaitGroup
}

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

func newIPLimiter(rps float64, burst int) *ipLimiter {
	if burst <= 0 {
		burst = max(1, int(rps))
	}
	l := &ipLimiter{
		clients:    make(map[string]*bucket),
		rps:        rps,
		burst:      burst,
		maxClients: 10000, // prevent unbounded growth under IP-spray attacks
		done:       make(chan struct{}),
	}
	l.wg.Add(1)
	go l.cleanup()
	return l
}

// allow returns true if the request from ip is within the rate limit.
func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	b, ok := l.clients[ip]
	if !ok {
		// Reject new clients when at capacity to prevent memory exhaustion.
		if len(l.clients) >= l.maxClients {
			return false
		}
		b = &bucket{tokens: float64(l.burst), lastSeen: now}
		l.clients[ip] = b
	}

	// Refill tokens based on elapsed time.
	elapsed := now.Sub(b.lastSeen).Seconds()
	b.tokens += elapsed * l.rps
	if b.tokens > float64(l.burst) {
		b.tokens = float64(l.burst)
	}
	b.lastSeen = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// cleanup evicts stale entries every 60 seconds.
func (l *ipLimiter) cleanup() {
	defer l.wg.Done()
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-l.done:
			return
		case <-ticker.C:
			l.mu.Lock()
			cutoff := time.Now().Add(-5 * time.Minute)
			for ip, b := range l.clients {
				if b.lastSeen.Before(cutoff) {
					delete(l.clients, ip)
				}
			}
			l.mu.Unlock()
		}
	}
}

// rateLimitMiddleware returns a chi-compatible middleware that enforces per-IP
// rate limiting. If rps <= 0, no limiting is applied (pass-through).
func rateLimitMiddleware(rps float64, burst int) func(http.Handler) http.Handler {
	if rps <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	limiter := newIPLimiter(rps, burst)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := extractIP(r)
			if !limiter.allow(ip) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":"rate limit exceeded"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// extractIP returns the client IP from X-Forwarded-For, X-Real-IP, or RemoteAddr.
func extractIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// First IP in the chain is the original client.
		if before, _, ok := strings.Cut(xff, ","); ok {
			return strings.TrimSpace(before)
		}
		return xff
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}
