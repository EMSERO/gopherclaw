package taskqueue

import "context"

// Limiter provides a concurrency-limiting semaphore.
// Manager implements this; the orchestrator Dispatcher can use it
// instead of its own internal semaphore for global concurrency control.
type Limiter interface {
	Acquire(ctx context.Context) error
	Release()
}
