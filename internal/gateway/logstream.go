package gateway

import (
	"strings"
	"sync"
)

// LogBroadcaster is a zapcore.WriteSyncer that fans log lines out to SSE clients.
// Slow clients that can't keep up have messages dropped (non-blocking send).
type LogBroadcaster struct {
	mu      sync.Mutex
	clients map[chan string]struct{}
}

// NewLogBroadcaster creates a new broadcaster.
func NewLogBroadcaster() *LogBroadcaster {
	return &LogBroadcaster{
		clients: make(map[chan string]struct{}),
	}
}

// Write implements io.Writer (used as a zap WriteSyncer).
// Each call fans the log line out to all subscribed clients.
func (lb *LogBroadcaster) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	if line == "" {
		return len(p), nil
	}

	lb.mu.Lock()
	defer lb.mu.Unlock()

	for ch := range lb.clients {
		// Non-blocking send — drop if client is too slow.
		select {
		case ch <- line:
		default:
		}
	}
	return len(p), nil
}

// Sync implements zapcore.WriteSyncer (no-op).
func (lb *LogBroadcaster) Sync() error { return nil }

// Subscribe returns a buffered channel that receives log lines.
func (lb *LogBroadcaster) Subscribe() chan string {
	ch := make(chan string, 256)
	lb.mu.Lock()
	lb.clients[ch] = struct{}{}
	lb.mu.Unlock()
	return ch
}

// Unsubscribe removes a client channel and closes it.
func (lb *LogBroadcaster) Unsubscribe(ch chan string) {
	lb.mu.Lock()
	delete(lb.clients, ch)
	lb.mu.Unlock()
	close(ch)
}
