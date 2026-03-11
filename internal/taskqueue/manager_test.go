package taskqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	return New(zap.NewNop().Sugar(), filepath.Join(dir, "tasks.json"), Config{
		MaxConcurrent:    3,
		ResultRetention:  time.Minute,
		ProgressThrottle: 50 * time.Millisecond,
	})
}

func TestSubmitAndComplete(t *testing.T) {
	m := testManager(t)
	defer m.Shutdown()

	done := make(chan struct{})
	id := m.Submit("sess", "agent1", "hello", func(ctx context.Context) (string, error) {
		defer close(done)
		return "result!", nil
	}, SubmitOpts{})

	<-done
	time.Sleep(50 * time.Millisecond) // let state update

	tasks := m.List()
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].ID != id {
		t.Errorf("ID = %q, want %q", tasks[0].ID, id)
	}
	if tasks[0].Status != StatusSuccess {
		t.Errorf("status = %q, want %q", tasks[0].Status, StatusSuccess)
	}
	if tasks[0].Result != "result!" {
		t.Errorf("result = %q, want %q", tasks[0].Result, "result!")
	}
	if tasks[0].DurationMs < 0 {
		t.Error("expected non-negative duration")
	}
}

func TestSubmitFailure(t *testing.T) {
	m := testManager(t)
	defer m.Shutdown()

	done := make(chan struct{})
	m.Submit("sess", "agent1", "fail", func(ctx context.Context) (string, error) {
		defer close(done)
		return "", fmt.Errorf("something broke")
	}, SubmitOpts{})

	<-done
	time.Sleep(50 * time.Millisecond)

	tasks := m.List()
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].Status != StatusFailed {
		t.Errorf("status = %q, want %q", tasks[0].Status, StatusFailed)
	}
	if tasks[0].Error != "something broke" {
		t.Errorf("error = %q, want %q", tasks[0].Error, "something broke")
	}
}

func TestSubmitPanic(t *testing.T) {
	m := testManager(t)
	defer m.Shutdown()

	done := make(chan struct{})
	m.Submit("sess", "agent1", "panic", func(ctx context.Context) (string, error) {
		defer close(done)
		panic("test panic!")
	}, SubmitOpts{})

	<-done
	time.Sleep(50 * time.Millisecond)

	tasks := m.List()
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].Status != StatusFailed {
		t.Errorf("status = %q, want %q", tasks[0].Status, StatusFailed)
	}
}

func TestCancelRunningTask(t *testing.T) {
	m := testManager(t)
	defer m.Shutdown()

	started := make(chan struct{})
	id := m.Submit("sess", "agent1", "long task", func(ctx context.Context) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}, SubmitOpts{})

	<-started

	if err := m.Cancel(id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	tasks := m.List()
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasks))
	}
	if tasks[0].Status != StatusCancelled {
		t.Errorf("status = %q, want %q", tasks[0].Status, StatusCancelled)
	}
}

func TestCancelNotFound(t *testing.T) {
	m := testManager(t)
	defer m.Shutdown()

	err := m.Cancel("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent task")
	}
}

func TestCancelAll(t *testing.T) {
	m := testManager(t)
	defer m.Shutdown()

	started := make(chan struct{}, 3)
	for i := range 3 {
		sess := "sess-a"
		if i == 2 {
			sess = "sess-b"
		}
		m.Submit(sess, "agent1", fmt.Sprintf("task %d", i), func(ctx context.Context) (string, error) {
			started <- struct{}{}
			<-ctx.Done()
			return "", ctx.Err()
		}, SubmitOpts{})
	}

	// Wait for all to start
	for range 3 {
		<-started
	}

	n := m.CancelAll("sess-a")
	if n != 2 {
		t.Errorf("CancelAll returned %d, want 2", n)
	}

	time.Sleep(50 * time.Millisecond)

	// sess-b task should still be running
	if m.RunningCount() != 1 {
		t.Errorf("RunningCount = %d, want 1", m.RunningCount())
	}
}

func TestConcurrencySemaphore(t *testing.T) {
	m := testManager(t) // maxConcurrent = 3
	defer m.Shutdown()

	var maxConcurrent atomic.Int32
	var current atomic.Int32
	var wg sync.WaitGroup

	for range 6 {
		wg.Add(1)
		m.Submit("sess", "agent1", "concurrent", func(ctx context.Context) (string, error) {
			defer wg.Done()
			n := current.Add(1)
			for {
				old := maxConcurrent.Load()
				if n > old {
					if maxConcurrent.CompareAndSwap(old, n) {
						break
					}
				} else {
					break
				}
			}
			time.Sleep(50 * time.Millisecond)
			current.Add(-1)
			return "ok", nil
		}, SubmitOpts{})
	}

	wg.Wait()

	if got := maxConcurrent.Load(); got > 3 {
		t.Errorf("max concurrent = %d, want <= 3", got)
	}
}

func TestOnCompleteCallback(t *testing.T) {
	m := testManager(t)
	defer m.Shutdown()

	callbackDone := make(chan struct{})
	var callbackResult string
	var callbackErr error

	m.Submit("sess", "agent1", "callback", func(ctx context.Context) (string, error) {
		return "callback-result", nil
	}, SubmitOpts{
		OnComplete: func(result string, err error) {
			callbackResult = result
			callbackErr = err
			close(callbackDone)
		},
	})

	<-callbackDone

	if callbackResult != "callback-result" {
		t.Errorf("callback result = %q, want %q", callbackResult, "callback-result")
	}
	if callbackErr != nil {
		t.Errorf("callback err = %v, want nil", callbackErr)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "tasks.json")

	// Create manager, submit and complete a task
	m1 := New(zap.NewNop().Sugar(), filePath, Config{MaxConcurrent: 3, ResultRetention: time.Hour})
	done := make(chan struct{})
	m1.Submit("sess", "agent1", "persist-test", func(ctx context.Context) (string, error) {
		defer close(done)
		return "persisted-result", nil
	}, SubmitOpts{})
	<-done
	time.Sleep(50 * time.Millisecond)
	m1.Shutdown()

	// Create new manager from same file
	m2 := New(zap.NewNop().Sugar(), filePath, Config{MaxConcurrent: 3, ResultRetention: time.Hour})
	defer m2.Shutdown()

	tasks := m2.List()
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task after reload, got %d", len(tasks))
	}
	if tasks[0].Status != StatusSuccess {
		t.Errorf("status = %q, want %q", tasks[0].Status, StatusSuccess)
	}
	if tasks[0].Result != "persisted-result" {
		t.Errorf("result = %q, want %q", tasks[0].Result, "persisted-result")
	}
}

func TestInterruptedTasksMarkedFailed(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "tasks.json")

	// Simulate a running task persisted to disk
	f := taskFile{
		Version: 1,
		Tasks: []*TaskRecord{
			{ID: "abc", Status: StatusRunning, AgentID: "x", CreatedAtMs: 1000},
			{ID: "def", Status: StatusPending, AgentID: "y", CreatedAtMs: 2000},
			{ID: "ghi", Status: StatusSuccess, AgentID: "z", CreatedAtMs: 3000, Result: "done"},
		},
	}
	data, _ := json.MarshalIndent(f, "", "  ")
	_ = os.WriteFile(filePath, data, 0600)

	m := New(zap.NewNop().Sugar(), filePath, Config{MaxConcurrent: 3})
	defer m.Shutdown()

	tasks := m.List()
	if len(tasks) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(tasks))
	}

	statusMap := make(map[string]TaskStatus)
	for _, task := range tasks {
		statusMap[task.ID] = task.Status
	}

	if statusMap["abc"] != StatusFailed {
		t.Errorf("running task should be failed, got %q", statusMap["abc"])
	}
	if statusMap["def"] != StatusFailed {
		t.Errorf("pending task should be failed, got %q", statusMap["def"])
	}
	if statusMap["ghi"] != StatusSuccess {
		t.Errorf("success task should stay success, got %q", statusMap["ghi"])
	}
}

func TestListForSession(t *testing.T) {
	m := testManager(t)
	defer m.Shutdown()

	var wg sync.WaitGroup
	wg.Add(2)
	m.Submit("sess-a", "agent1", "task-a", func(ctx context.Context) (string, error) {
		defer wg.Done()
		return "a", nil
	}, SubmitOpts{})
	m.Submit("sess-b", "agent1", "task-b", func(ctx context.Context) (string, error) {
		defer wg.Done()
		return "b", nil
	}, SubmitOpts{})

	wg.Wait()
	time.Sleep(50 * time.Millisecond)

	sessA := m.ListForSession("sess-a")
	if len(sessA) != 1 {
		t.Errorf("sess-a tasks = %d, want 1", len(sessA))
	}
	sessB := m.ListForSession("sess-b")
	if len(sessB) != 1 {
		t.Errorf("sess-b tasks = %d, want 1", len(sessB))
	}
}

func TestMessageTruncation(t *testing.T) {
	m := testManager(t)
	defer m.Shutdown()

	longMsg := ""
	for range 300 {
		longMsg += "x"
	}
	done := make(chan struct{})
	m.Submit("sess", "agent1", longMsg, func(ctx context.Context) (string, error) {
		defer close(done)
		return "ok", nil
	}, SubmitOpts{})

	<-done
	time.Sleep(50 * time.Millisecond)

	tasks := m.List()
	if len(tasks[0].Message) != 200 {
		t.Errorf("message length = %d, want 200", len(tasks[0].Message))
	}
}

func TestLimiterInterface(t *testing.T) {
	m := testManager(t) // maxConcurrent = 3
	defer m.Shutdown()

	// Acquire 3 slots
	for range 3 {
		if err := m.Acquire(context.Background()); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
	}

	// 4th should block, verify with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := m.Acquire(ctx)
	if err == nil {
		t.Error("expected timeout on 4th Acquire")
	}

	// Release one, acquire should succeed
	m.Release()

	if err := m.Acquire(context.Background()); err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}

	// Clean up
	for range 3 {
		m.Release()
	}
}

func TestPrune(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "tasks.json")

	m := New(zap.NewNop().Sugar(), filePath, Config{
		MaxConcurrent:   3,
		ResultRetention: 50 * time.Millisecond,
	})
	defer m.Shutdown()

	done := make(chan struct{})
	m.Submit("sess", "agent1", "old task", func(ctx context.Context) (string, error) {
		defer close(done)
		return "old", nil
	}, SubmitOpts{})

	<-done
	time.Sleep(100 * time.Millisecond) // exceed retention

	m.prune()

	if len(m.List()) != 0 {
		t.Errorf("expected 0 tasks after prune, got %d", len(m.List()))
	}
}

// --- mock Announcer ---

type mockAnnouncer struct {
	mu       sync.Mutex
	messages []announcerMsg
}

type announcerMsg struct {
	sessionKey string
	text       string
}

func (a *mockAnnouncer) AnnounceToSession(sessionKey, text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.messages = append(a.messages, announcerMsg{sessionKey: sessionKey, text: text})
}

func (a *mockAnnouncer) getMessages() []announcerMsg {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]announcerMsg, len(a.messages))
	copy(out, a.messages)
	return out
}

// --- AddAnnouncer tests ---

func TestAddAnnouncer(t *testing.T) {
	m := testManager(t)
	defer m.Shutdown()

	a1 := &mockAnnouncer{}
	a2 := &mockAnnouncer{}

	m.AddAnnouncer(a1)
	m.AddAnnouncer(a2)

	m.mu.Lock()
	count := len(m.announcers)
	m.mu.Unlock()

	if count != 2 {
		t.Errorf("expected 2 announcers, got %d", count)
	}
}

// --- AnnounceProgress tests ---

func TestAnnounceProgress(t *testing.T) {
	m := testManager(t)
	defer m.Shutdown()

	a := &mockAnnouncer{}
	m.AddAnnouncer(a)

	// First call should go through.
	m.AnnounceProgress("sess-1", "50% done")

	msgs := a.getMessages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].sessionKey != "sess-1" {
		t.Errorf("session key = %q, want %q", msgs[0].sessionKey, "sess-1")
	}
	if msgs[0].text != "50% done" {
		t.Errorf("text = %q, want %q", msgs[0].text, "50% done")
	}

	// Second call within throttle window should be suppressed.
	m.AnnounceProgress("sess-1", "60% done")
	msgs = a.getMessages()
	if len(msgs) != 1 {
		t.Errorf("expected 1 message (throttled), got %d", len(msgs))
	}

	// Different session should go through.
	m.AnnounceProgress("sess-2", "10% done")
	msgs = a.getMessages()
	if len(msgs) != 2 {
		t.Errorf("expected 2 messages, got %d", len(msgs))
	}

	// Wait for throttle window to pass, then same session should work.
	time.Sleep(60 * time.Millisecond) // ProgressThrottle is 50ms in testManager
	m.AnnounceProgress("sess-1", "90% done")
	msgs = a.getMessages()
	if len(msgs) != 3 {
		t.Errorf("expected 3 messages after throttle expired, got %d", len(msgs))
	}
}

// --- Announce tests ---

func TestAnnounce(t *testing.T) {
	m := testManager(t)
	defer m.Shutdown()

	a := &mockAnnouncer{}
	m.AddAnnouncer(a)

	// Announce is unthrottled, so multiple calls should all go through.
	m.Announce("sess-1", "Task complete: result A")
	m.Announce("sess-1", "Task complete: result B")
	m.Announce("sess-2", "Task complete: result C")

	msgs := a.getMessages()
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	if msgs[0].text != "Task complete: result A" {
		t.Errorf("msg[0] text = %q, want %q", msgs[0].text, "Task complete: result A")
	}
	if msgs[1].text != "Task complete: result B" {
		t.Errorf("msg[1] text = %q, want %q", msgs[1].text, "Task complete: result B")
	}
	if msgs[2].sessionKey != "sess-2" {
		t.Errorf("msg[2] sessionKey = %q, want %q", msgs[2].sessionKey, "sess-2")
	}
}

func TestAnnounceMultipleAnnouncers(t *testing.T) {
	m := testManager(t)
	defer m.Shutdown()

	a1 := &mockAnnouncer{}
	a2 := &mockAnnouncer{}
	m.AddAnnouncer(a1)
	m.AddAnnouncer(a2)

	m.Announce("sess-1", "broadcast msg")

	msgs1 := a1.getMessages()
	msgs2 := a2.getMessages()
	if len(msgs1) != 1 || len(msgs2) != 1 {
		t.Errorf("expected both announcers to get 1 message, got %d and %d", len(msgs1), len(msgs2))
	}
}

// --- StartPruneLoop tests ---

func TestStartPruneLoop(t *testing.T) {
	m := testManager(t)
	defer m.Shutdown()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		m.StartPruneLoop(ctx)
		close(done)
	}()

	// Cancel immediately; the loop should exit.
	cancel()

	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("StartPruneLoop did not exit after context cancellation")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("short string: got %q, want %q", got, "hello")
	}
	if got := truncate("hello world", 5); got != "hello" {
		t.Errorf("long string: got %q, want %q", got, "hello")
	}
}
