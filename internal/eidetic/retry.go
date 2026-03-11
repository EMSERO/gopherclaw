package eidetic

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
)

// RetryClient wraps a Client and queues failed AppendMemory calls for
// background retry.  The queue is persisted to disk so entries survive
// restarts.  Search and health calls are proxied through unchanged.
type RetryClient struct {
	inner  Client
	logger *zap.SugaredLogger

	mu       sync.Mutex
	queue    []AppendRequest
	filePath string // e.g. ~/.gopherclaw/eidetic-retry-queue.json
	dirty    bool
}

// NewRetryClient wraps inner with a failure queue persisted at filePath.
// Call StartRetryLoop in a goroutine to process the queue.
func NewRetryClient(inner Client, logger *zap.SugaredLogger, filePath string) *RetryClient {
	rc := &RetryClient{
		inner:    inner,
		logger:   logger,
		filePath: filePath,
	}
	rc.load()
	return rc
}

// AppendMemory tries the inner client first; on failure, queues the request.
func (rc *RetryClient) AppendMemory(ctx context.Context, req AppendRequest) error {
	err := rc.inner.AppendMemory(ctx, req)
	if err != nil {
		rc.enqueue(req)
		rc.logger.Warnf("eidetic: append failed, queued for retry (%d pending): %v", rc.QueueLen(), err)
	}
	return err
}

// SearchMemory proxies through to the inner client.
func (rc *RetryClient) SearchMemory(ctx context.Context, req SearchRequest) ([]MemoryEntry, error) {
	return rc.inner.SearchMemory(ctx, req)
}

// GetRecent proxies through to the inner client.
func (rc *RetryClient) GetRecent(ctx context.Context, agentID string, limit int) ([]MemoryEntry, error) {
	return rc.inner.GetRecent(ctx, agentID, limit)
}

// Health proxies through to the inner client.
func (rc *RetryClient) Health(ctx context.Context) error {
	return rc.inner.Health(ctx)
}

// QueueLen returns the number of pending retry items.
func (rc *RetryClient) QueueLen() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.queue)
}

// StartRetryLoop drains the queue periodically until ctx is cancelled.
func (rc *RetryClient) StartRetryLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			rc.persist()
			return
		case <-ticker.C:
			rc.drainQueue(ctx)
		}
	}
}

func (rc *RetryClient) enqueue(req AppendRequest) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	// Cap queue to prevent unbounded growth.
	const maxQueue = 500
	if len(rc.queue) >= maxQueue {
		// Drop oldest entry.
		rc.queue = rc.queue[1:]
	}
	rc.queue = append(rc.queue, req)
	rc.dirty = true
	rc.persist()
}

func (rc *RetryClient) drainQueue(ctx context.Context) {
	rc.mu.Lock()
	if len(rc.queue) == 0 {
		rc.mu.Unlock()
		return
	}
	// Take a snapshot and clear.
	batch := make([]AppendRequest, len(rc.queue))
	copy(batch, rc.queue)
	rc.queue = rc.queue[:0]
	rc.dirty = true
	rc.mu.Unlock()

	var failed []AppendRequest
	for _, req := range batch {
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := rc.inner.AppendMemory(rctx, req); err != nil {
			failed = append(failed, req)
		}
		cancel()
	}

	if len(failed) > 0 {
		rc.mu.Lock()
		// Prepend failed items back (they're older).
		rc.queue = append(failed, rc.queue...)
		rc.dirty = true
		rc.mu.Unlock()
		rc.logger.Warnf("eidetic: retry queue: %d/%d still failing", len(failed), len(batch))
	} else if len(batch) > 0 {
		rc.logger.Infof("eidetic: retry queue drained %d entries successfully", len(batch))
	}
	rc.persist()
}

// ---- persistence ----

type retryFile struct {
	Queue []AppendRequest `json:"queue"`
}

func (rc *RetryClient) load() {
	data, err := os.ReadFile(rc.filePath)
	if err != nil {
		return
	}
	var f retryFile
	if err := json.Unmarshal(data, &f); err != nil {
		rc.logger.Warnf("eidetic: retry queue parse error: %v", err)
		return
	}
	rc.queue = f.Queue
	if len(rc.queue) > 0 {
		rc.logger.Infof("eidetic: loaded %d entries from retry queue", len(rc.queue))
	}
}

func (rc *RetryClient) persist() {
	if !rc.dirty {
		return
	}
	rc.dirty = false

	f := retryFile{Queue: rc.queue}
	data, err := json.Marshal(f)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(rc.filePath), 0700)
	_ = os.WriteFile(rc.filePath, data, 0600)
}
