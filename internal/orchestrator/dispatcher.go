package orchestrator

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/EMSERO/gopherclaw/internal/agentapi"
)

// Chatter is an alias for agentapi.Chatter.
type Chatter = agentapi.Chatter

// ChatResponse is an alias for agentapi.Response (kept for backward compatibility).
type ChatResponse = agentapi.Response

// Limiter provides a concurrency-limiting semaphore. When set on Dispatcher,
// it replaces the internal channel-based semaphore for global concurrency control.
type Limiter interface {
	Acquire(ctx context.Context) error
	Release()
}

// Dispatcher executes a validated TaskGraph by running subtasks in goroutines,
// respecting dependencies, enforcing max concurrency, and handling partial failures.
type Dispatcher struct {
	// Agents maps agent ID → Chatter.
	Agents map[string]Chatter
	// MaxConcurrent limits simultaneous subtasks (default 5).
	MaxConcurrent int
	// Limiter replaces the internal semaphore when set (for global concurrency control).
	Limiter Limiter
}

// Execute validates the graph, topologically sorts it, then runs all tasks
// using goroutines with dependency tracking and a semaphore for max concurrency.
// progress is called after each task completes; pass NoOpProgress to disable.
func (d *Dispatcher) Execute(ctx context.Context, graph TaskGraph, progress ProgressFunc) (ResultSet, error) {
	if progress == nil {
		progress = NoOpProgress
	}
	maxC := d.MaxConcurrent
	if maxC <= 0 {
		maxC = 5
	}

	// Validate
	if err := d.validate(graph); err != nil {
		return ResultSet{}, err
	}

	// Build lookup
	taskMap := make(map[string]*Task, len(graph.Tasks))
	for i := range graph.Tasks {
		taskMap[graph.Tasks[i].ID] = &graph.Tasks[i]
	}

	// Cycle detection via Kahn's algorithm
	order, err := topoSort(graph.Tasks)
	if err != nil {
		return ResultSet{}, err
	}

	// Generate dispatch ID for session key namespacing
	dispatchID := randomHex(8)

	// Result tracking
	var mu sync.Mutex
	results := make(map[string]*TaskResult, len(graph.Tasks))
	outputs := make(map[string]string, len(graph.Tasks)) // for interpolation
	for _, t := range graph.Tasks {
		results[t.ID] = &TaskResult{
			ID:      t.ID,
			AgentID: t.AgentID,
			Status:  TaskStatusPending,
		}
	}

	// Dependency completion tracking: each task has a channel that is closed
	// when the task finishes (regardless of outcome).
	done := make(map[string]chan struct{}, len(graph.Tasks))
	for _, t := range graph.Tasks {
		done[t.ID] = make(chan struct{})
	}

	// Semaphore for max concurrency (used when no external Limiter is set)
	var sem chan struct{}
	if d.Limiter == nil {
		sem = make(chan struct{}, maxC)
	}

	// Completed counter for progress
	var completed atomic.Int32
	_ = int32(len(graph.Tasks)) // total; available for future progress reporting

	// Use errgroup for goroutine lifecycle. We manage cancellation ourselves
	// rather than using errgroup.WithContext, because we want fine-grained
	// control: only blocking failures should cancel outstanding work.
	cancelCtx, cancelFn := context.WithCancel(ctx)
	defer cancelFn()

	eg, egCtx := errgroup.WithContext(cancelCtx)

	// cancelled tracks tasks whose upstream blocking dependency failed
	cancelled := make(map[string]bool)
	var cancelMu sync.Mutex

	// markCancelled recursively cancels all tasks that transitively depend on taskID.
	// MUST be called while cancelMu is held.
	var markCancelled func(failedID string)
	markCancelled = func(failedID string) {
		for _, t := range graph.Tasks {
			for _, dep := range t.DependsOn {
				if dep == failedID {
					if !cancelled[t.ID] {
						cancelled[t.ID] = true
						markCancelled(t.ID)
					}
				}
			}
		}
	}

	// Schedule tasks in topological order
	for _, taskID := range order {
		task := taskMap[taskID]

		eg.Go(func() error {
			// Wait for all dependencies to complete
			for _, depID := range task.DependsOn {
				select {
				case <-done[depID]:
				case <-egCtx.Done():
					// Context cancelled — mark as cancelled if not already
					mu.Lock()
					r := results[taskID]
					if r.Status == TaskStatusPending {
						r.Status = TaskStatusCancelled
						r.Error = "context cancelled"
					}
					mu.Unlock()
					close(done[taskID])
					return nil
				}
			}

			// Check if this task was cancelled due to upstream failure
			cancelMu.Lock()
			isCancelled := cancelled[taskID]
			cancelMu.Unlock()
			if isCancelled {
				mu.Lock()
				r := results[taskID]
				r.Status = TaskStatusCancelled
				// Find which dependency failed
				for _, depID := range task.DependsOn {
					depR := results[depID]
					if depR.Status == TaskStatusFailed || depR.Status == TaskStatusTimeout || depR.Status == TaskStatusCancelled {
						r.Error = fmt.Sprintf("upstream task %s failed", depID)
						break
					}
				}
				if r.Error == "" {
					r.Error = "upstream task failed"
				}
				mu.Unlock()
				progress(taskID, task.AgentID, TaskStatusCancelled)
				completed.Add(1)
				close(done[taskID])
				return nil
			}

			// Check if non-blocking dependencies failed (they don't cancel us,
			// but we need their output status for interpolation)

			// Acquire semaphore (external Limiter or internal channel)
			if d.Limiter != nil {
				if err := d.Limiter.Acquire(egCtx); err != nil {
					mu.Lock()
					r := results[taskID]
					if r.Status == TaskStatusPending {
						r.Status = TaskStatusCancelled
						r.Error = "context cancelled"
					}
					mu.Unlock()
					close(done[taskID])
					return nil
				}
			} else {
				select {
				case sem <- struct{}{}:
				case <-egCtx.Done():
					mu.Lock()
					r := results[taskID]
					if r.Status == TaskStatusPending {
						r.Status = TaskStatusCancelled
						r.Error = "context cancelled"
					}
					mu.Unlock()
					close(done[taskID])
					return nil
				}
			}

			// Interpolate message with upstream outputs
			mu.Lock()
			msg := Interpolate(task.Message, outputs, 0)
			results[taskID].Status = TaskStatusRunning
			mu.Unlock()

			// Per-task timeout
			var taskCtx context.Context
			var taskCancel context.CancelFunc
			if task.TimeoutSeconds > 0 {
				taskCtx, taskCancel = context.WithTimeout(egCtx, time.Duration(task.TimeoutSeconds)*time.Second)
			} else {
				taskCtx, taskCancel = context.WithCancel(egCtx)
			}

			sessionKey := fmt.Sprintf("orchestrator:%s:%s", dispatchID, taskID)
			start := time.Now()
			resp, chatErr := d.Agents[task.AgentID].Chat(taskCtx, sessionKey, msg)
			elapsed := time.Since(start)

			taskCancel()
			if d.Limiter != nil {
				d.Limiter.Release()
			} else {
				<-sem
			}

			mu.Lock()
			r := results[taskID]
			r.DurationMs = elapsed.Milliseconds()
			r.Duration = elapsed

			if chatErr != nil {
				if taskCtx.Err() == context.DeadlineExceeded && task.TimeoutSeconds > 0 {
					r.Status = TaskStatusTimeout
					r.Error = fmt.Sprintf("timeout after %ds", task.TimeoutSeconds)
				} else {
					r.Status = TaskStatusFailed
					r.Error = chatErr.Error()
				}
			} else {
				r.Status = TaskStatusSuccess
				r.Output = resp.Text
				outputs[taskID] = resp.Text
			}
			mu.Unlock()

			progress(taskID, task.AgentID, r.Status)
			completed.Add(1)

			// Handle blocking failure: cancel downstream dependents
			if r.Status != TaskStatusSuccess && task.Blocking {
				cancelMu.Lock()
				markCancelled(taskID)
				cancelMu.Unlock()
				// Cancel the context so in-flight tasks receive cancellation
				cancelFn()
			}

			close(done[taskID])
			return nil
		})
	}

	// Wait for all goroutines
	_ = eg.Wait()

	// Build ordered result set
	rs := ResultSet{Tasks: make([]TaskResult, 0, len(graph.Tasks))}
	for _, t := range graph.Tasks {
		rs.Tasks = append(rs.Tasks, *results[t.ID])
	}
	return rs, nil
}

// validate checks the task graph for required fields, duplicate IDs,
// unknown agent IDs, and unknown dependency references.
func (d *Dispatcher) validate(graph TaskGraph) error {
	if len(graph.Tasks) == 0 {
		return fmt.Errorf("task graph is empty")
	}

	ids := make(map[string]bool, len(graph.Tasks))
	for _, t := range graph.Tasks {
		if t.ID == "" {
			return fmt.Errorf("task missing required field: id")
		}
		if t.AgentID == "" {
			return fmt.Errorf("task %q missing required field: agent_id", t.ID)
		}
		if t.Message == "" {
			return fmt.Errorf("task %q missing required field: message", t.ID)
		}
		if ids[t.ID] {
			return fmt.Errorf("duplicate task id: %q", t.ID)
		}
		ids[t.ID] = true

		if _, ok := d.Agents[t.AgentID]; !ok {
			return fmt.Errorf("task %q references unknown agent %q; available: %s",
				t.ID, t.AgentID, agentIDList(d.Agents))
		}
	}

	// Validate dependency references
	for _, t := range graph.Tasks {
		for _, dep := range t.DependsOn {
			if !ids[dep] {
				return fmt.Errorf("task %q depends on unknown task %q", t.ID, dep)
			}
		}
	}

	return nil
}

// topoSort performs a topological sort using Kahn's algorithm.
// Returns the task IDs in execution order, or an error if a cycle is detected.
func topoSort(tasks []Task) ([]string, error) {
	// Build adjacency and in-degree
	inDegree := make(map[string]int, len(tasks))
	dependents := make(map[string][]string, len(tasks))
	for _, t := range tasks {
		if _, ok := inDegree[t.ID]; !ok {
			inDegree[t.ID] = 0
		}
		for _, dep := range t.DependsOn {
			inDegree[t.ID]++
			dependents[dep] = append(dependents[dep], t.ID)
		}
	}

	// Start with zero in-degree nodes
	var queue []string
	for _, t := range tasks {
		if inDegree[t.ID] == 0 {
			queue = append(queue, t.ID)
		}
	}

	var order []string
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		order = append(order, node)

		for _, dep := range dependents[node] {
			inDegree[dep]--
			if inDegree[dep] == 0 {
				queue = append(queue, dep)
			}
		}
	}

	if len(order) != len(tasks) {
		// Find nodes in cycle for a useful error message
		var cycleNodes []string
		for id, deg := range inDegree {
			if deg > 0 {
				cycleNodes = append(cycleNodes, id)
			}
		}
		return nil, fmt.Errorf("cycle detected in task graph involving tasks: %s", strings.Join(cycleNodes, ", "))
	}

	return order, nil
}

func agentIDList(agents map[string]Chatter) string {
	ids := make([]string, 0, len(agents))
	for id := range agents {
		ids = append(ids, id)
	}
	return strings.Join(ids, ", ")
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}
