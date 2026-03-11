package orchestrator

// ProgressFunc is called after each task completes (or fails) during dispatch.
// It must be safe to call from multiple goroutines concurrently.
type ProgressFunc func(taskID string, agentID string, status TaskStatus)

// NoOpProgress is a ProgressFunc that does nothing. Used when
// orchestrator.progressUpdates is false (the default).
func NoOpProgress(string, string, TaskStatus) {}
