package taskqueue

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

func stressManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	return New(zap.NewNop().Sugar(), filepath.Join(dir, "tasks.json"), Config{
		MaxConcurrent:    10,
		ResultRetention:  time.Minute,
		ProgressThrottle: 10 * time.Millisecond,
	})
}

// TestStress_ConcurrentSubmitStatusCancel exercises Submit, List, and Cancel
// operations running simultaneously.
func TestStress_ConcurrentSubmitStatusCancel(t *testing.T) {
	t.Parallel()
	m := stressManager(t)
	defer m.Shutdown()

	const goroutines = 80
	var wg sync.WaitGroup
	var taskIDs sync.Map // stores submitted task IDs

	// Submit tasks that block until cancelled or a short timeout.
	for i := 0; i < goroutines/2; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := m.Submit(
				fmt.Sprintf("session:%d", n%5),
				"agent-test",
				fmt.Sprintf("task-%d", n),
				func(ctx context.Context) (string, error) {
					select {
					case <-ctx.Done():
						return "", ctx.Err()
					case <-time.After(500 * time.Millisecond):
						return fmt.Sprintf("result-%d", n), nil
					}
				},
				SubmitOpts{},
			)
			taskIDs.Store(id, true)
		}(i)
	}

	// Concurrent status checks.
	for i := 0; i < goroutines/4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.List()
			_ = m.RunningCount()
			_ = m.ListForSession("session:0")
		}()
	}

	// Concurrent cancels (some will fail because task already finished; that's fine).
	for i := 0; i < goroutines/4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Try to cancel whatever tasks exist.
			tasks := m.List()
			for _, task := range tasks {
				_ = m.Cancel(task.ID)
			}
		}()
	}

	wg.Wait()
}

// TestStress_ManyTasksCompletingSimultaneously exercises many tasks finishing
// at roughly the same time.
func TestStress_ManyTasksCompletingSimultaneously(t *testing.T) {
	t.Parallel()
	m := stressManager(t)
	defer m.Shutdown()

	const taskCount = 50
	var completed atomic.Int64
	startBarrier := make(chan struct{}) // all tasks wait for this

	var wg sync.WaitGroup
	for i := 0; i < taskCount; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			m.Submit(
				"session:burst",
				"agent-test",
				fmt.Sprintf("burst-task-%d", n),
				func(ctx context.Context) (string, error) {
					<-startBarrier // wait for all tasks to be submitted
					completed.Add(1)
					return fmt.Sprintf("done-%d", n), nil
				},
				SubmitOpts{},
			)
		}(i)
	}

	// Let all tasks start working.
	time.Sleep(50 * time.Millisecond)
	close(startBarrier)

	wg.Wait()

	// Wait for all tasks to complete through the semaphore.
	deadline := time.After(10 * time.Second)
	for {
		tasks := m.List()
		allDone := true
		for _, task := range tasks {
			if task.Status == StatusPending || task.Status == StatusRunning {
				allDone = false
				break
			}
		}
		if allDone {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for all tasks to complete")
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}

	if completed.Load() != taskCount {
		t.Errorf("expected %d completions, got %d", taskCount, completed.Load())
	}
}

// TestStress_ConcurrentOnCompleteCallbacks exercises many tasks with
// OnComplete callbacks finishing simultaneously.
func TestStress_ConcurrentOnCompleteCallbacks(t *testing.T) {
	t.Parallel()
	m := stressManager(t)
	defer m.Shutdown()

	const taskCount = 60
	var callbackCount atomic.Int64
	var callbackMu sync.Mutex
	callbackResults := make(map[string]bool)

	var wg sync.WaitGroup
	for i := 0; i < taskCount; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			resultKey := fmt.Sprintf("result-%d", n)
			m.Submit(
				"session:callbacks",
				"agent-test",
				fmt.Sprintf("callback-task-%d", n),
				func(ctx context.Context) (string, error) {
					return resultKey, nil
				},
				SubmitOpts{
					OnComplete: func(result string, err error) {
						callbackCount.Add(1)
						callbackMu.Lock()
						callbackResults[result] = true
						callbackMu.Unlock()
					},
				},
			)
		}(i)
	}

	wg.Wait()

	// Wait for all callbacks to fire.
	deadline := time.After(10 * time.Second)
	for callbackCount.Load() < taskCount {
		select {
		case <-deadline:
			t.Fatalf("timed out: only %d/%d callbacks fired", callbackCount.Load(), taskCount)
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}

	callbackMu.Lock()
	defer callbackMu.Unlock()
	if len(callbackResults) != taskCount {
		t.Errorf("expected %d unique callback results, got %d", taskCount, len(callbackResults))
	}
}

// TestStress_CancelAllUnderLoad exercises CancelAll while tasks are being
// submitted and running.
func TestStress_CancelAllUnderLoad(t *testing.T) {
	t.Parallel()
	m := stressManager(t)
	defer m.Shutdown()

	const goroutines = 60
	var wg sync.WaitGroup

	sessionKey := "session:cancel-all"

	// Submit long-running tasks.
	for i := 0; i < goroutines/2; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			m.Submit(
				sessionKey,
				"agent-test",
				fmt.Sprintf("long-task-%d", n),
				func(ctx context.Context) (string, error) {
					select {
					case <-ctx.Done():
						return "", ctx.Err()
					case <-time.After(5 * time.Second):
						return "done", nil
					}
				},
				SubmitOpts{},
			)
		}(i)
	}

	// Concurrent CancelAll calls.
	for i := 0; i < goroutines/2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(10 * time.Millisecond) // let some tasks start
			m.CancelAll(sessionKey)
		}()
	}

	wg.Wait()
}

// TestStress_PruneWhileSubmitting exercises task pruning running concurrently
// with submission.
func TestStress_PruneWhileSubmitting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m := New(zap.NewNop().Sugar(), filepath.Join(dir, "tasks.json"), Config{
		MaxConcurrent:   10,
		ResultRetention: time.Nanosecond, // instant expiry for testing
	})
	defer m.Shutdown()

	const goroutines = 50
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if n%3 == 0 {
				m.prune()
			} else {
				m.Submit(
					"session:prune",
					"agent-test",
					fmt.Sprintf("prune-task-%d", n),
					func(ctx context.Context) (string, error) {
						return "ok", nil
					},
					SubmitOpts{},
				)
			}
		}(i)
	}

	wg.Wait()
}

// TestStress_AcquireReleaseSemaphore exercises the Acquire/Release semaphore
// interface under contention.
func TestStress_AcquireReleaseSemaphore(t *testing.T) {
	t.Parallel()
	m := stressManager(t) // MaxConcurrent=10
	defer m.Shutdown()

	const goroutines = 80
	var wg sync.WaitGroup
	var active atomic.Int64
	var maxActive atomic.Int64

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := m.Acquire(ctx); err != nil {
				return // context expired, ok under contention
			}
			cur := active.Add(1)
			// Track max concurrent.
			for {
				old := maxActive.Load()
				if cur <= old || maxActive.CompareAndSwap(old, cur) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			active.Add(-1)
			m.Release()
		}()
	}

	wg.Wait()

	if maxActive.Load() > 10 {
		t.Errorf("max concurrent %d exceeded semaphore limit of 10", maxActive.Load())
	}
}
