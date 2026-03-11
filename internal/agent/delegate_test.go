package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/agentapi"
	"github.com/EMSERO/gopherclaw/internal/taskqueue"
	"github.com/EMSERO/gopherclaw/internal/tools"
)

func TestDelegateToolName(t *testing.T) {
	tool := &DelegateTool{Logger: zap.NewNop().Sugar()}
	if tool.Name() != "delegate" {
		t.Errorf("expected name 'delegate', got %q", tool.Name())
	}
}

func TestDelegateToolSchema(t *testing.T) {
	tool := &DelegateTool{Logger: zap.NewNop().Sugar()}
	var m map[string]any
	if err := json.Unmarshal(tool.Schema(), &m); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	props, ok := m["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected 'properties' in schema")
	}
	for _, field := range []string{"agent_id", "message", "session_id"} {
		if _, ok := props[field]; !ok {
			t.Errorf("schema missing field %q", field)
		}
	}
}

func TestDelegateToolInvalidJSON(t *testing.T) {
	tool := &DelegateTool{Agents: map[string]Chatter{}, MaxDepth: 5, Logger: zap.NewNop().Sugar()}
	result := tool.Run(context.Background(), "not-json")
	if !strings.Contains(result, "invalid arguments") {
		t.Errorf("expected 'invalid arguments', got %q", result)
	}
}

func TestDelegateToolMissingAgentID(t *testing.T) {
	tool := &DelegateTool{Agents: map[string]Chatter{}, MaxDepth: 5, Logger: zap.NewNop().Sugar()}
	args, _ := json.Marshal(map[string]string{"message": "hello"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "agent_id") {
		t.Errorf("expected agent_id error, got %q", result)
	}
}

func TestDelegateToolMissingMessage(t *testing.T) {
	tool := &DelegateTool{Agents: map[string]Chatter{}, MaxDepth: 5, Logger: zap.NewNop().Sugar()}
	args, _ := json.Marshal(map[string]string{"agent_id": "sub"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "message") {
		t.Errorf("expected message error, got %q", result)
	}
}

func TestDelegateToolUnknownAgent(t *testing.T) {
	tool := &DelegateTool{Agents: map[string]Chatter{}, MaxDepth: 5, Logger: zap.NewNop().Sugar()}
	args, _ := json.Marshal(map[string]string{"agent_id": "missing", "message": "hello"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "not found") {
		t.Errorf("expected 'not found', got %q", result)
	}
}

func TestDelegateToolMaxDepth(t *testing.T) {
	tool := &DelegateTool{Agents: map[string]Chatter{"sub": NewCLIAgent("sub", "echo", nil, 0)}, MaxDepth: 3, Logger: zap.NewNop().Sugar()}
	ctx := context.WithValue(context.Background(), delegateDepthKey{}, 3)
	args, _ := json.Marshal(map[string]string{"agent_id": "sub", "message": "hello"})
	result := tool.Run(ctx, string(args))
	if !strings.Contains(result, "recursion limit") {
		t.Errorf("expected recursion limit error, got %q", result)
	}
}

func TestDelegateToolDepthBelowLimit(t *testing.T) {
	// At depth limit-1, should pass the depth check and fail on agent lookup instead.
	tool := &DelegateTool{Agents: map[string]Chatter{}, MaxDepth: 2, Logger: zap.NewNop().Sugar()}
	ctx := context.WithValue(context.Background(), delegateDepthKey{}, 1)
	args, _ := json.Marshal(map[string]string{"agent_id": "nobody", "message": "hi"})
	result := tool.Run(ctx, string(args))
	if strings.Contains(result, "recursion limit") {
		t.Errorf("should not hit recursion limit at depth 1 with maxDepth 2, got %q", result)
	}
	if !strings.Contains(result, "not found") {
		t.Errorf("expected 'not found' past depth check, got %q", result)
	}
}

// mockChatter is a simple Chatter for testing that returns a fixed response.
type mockChatter struct {
	response string
	err      error
	// records calls
	mu    sync.Mutex
	calls []mockCall
}

type mockCall struct {
	SessionKey string
	Message    string
}

func (m *mockChatter) Chat(_ context.Context, sessionKey, message string) (Response, error) {
	m.mu.Lock()
	m.calls = append(m.calls, mockCall{SessionKey: sessionKey, Message: message})
	m.mu.Unlock()
	if m.err != nil {
		return Response{}, m.err
	}
	return Response{Text: m.response}, nil
}

func (m *mockChatter) getCalls() []mockCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]mockCall, len(m.calls))
	copy(out, m.calls)
	return out
}

// mockAnnouncer records announcements for verification.
type mockAnnouncer struct {
	mu      sync.Mutex
	entries []mockAnnouncement
}

type mockAnnouncement struct {
	SessionKey string
	Text       string
}

func (m *mockAnnouncer) AnnounceToSession(sessionKey, text string) {
	m.mu.Lock()
	m.entries = append(m.entries, mockAnnouncement{SessionKey: sessionKey, Text: text})
	m.mu.Unlock()
}

func (m *mockAnnouncer) getEntries() []mockAnnouncement {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]mockAnnouncement, len(m.entries))
	copy(out, m.entries)
	return out
}

func TestAsyncDelegateAnnouncesRawResult(t *testing.T) {
	subAgent := &mockChatter{response: "subagent output"}
	announcer := &mockAnnouncer{}
	mgr := taskqueue.New(testLogger(), filepath.Join(t.TempDir(), "tasks.json"), taskqueue.Config{MaxConcurrent: 5})

	tool := &DelegateTool{
		Agents:     map[string]Chatter{"cli-sub": subAgent},
		MaxDepth:   5,
		Announcers: []Announcer{announcer},
		TaskMgr:    mgr,
		Logger:     zap.NewNop().Sugar(),
	}

	ctx := context.WithValue(context.Background(), tools.SessionKeyContextKey{}, "agent:main:telegram:12345")
	result := tool.runAsync(ctx, subAgent, "cli-sub", "subagent:cli-sub:test", "do something", "")

	if !strings.Contains(result, "Spawned") {
		t.Fatalf("expected 'Spawned' acknowledgment, got %q", result)
	}

	deadline := time.After(5 * time.Second)
	for {
		entries := announcer.getEntries()
		if len(entries) >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for announcements, got %d", len(announcer.getEntries()))
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Verify: subagent was called
	subCalls := subAgent.getCalls()
	if len(subCalls) != 1 {
		t.Fatalf("expected 1 subagent call, got %d", len(subCalls))
	}
	if subCalls[0].Message != "do something" {
		t.Errorf("subagent got wrong message: %q", subCalls[0].Message)
	}

	// Verify: raw result announced directly
	entries := announcer.getEntries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 announcement, got %d", len(entries))
	}
	if !strings.Contains(entries[0].Text, "subagent output") {
		t.Errorf("expected raw subagent result in announcement, got %q", entries[0].Text)
	}
	if !strings.Contains(entries[0].Text, "cli-sub result") {
		t.Errorf("expected agent ID in announcement, got %q", entries[0].Text)
	}
}

func TestAsyncDelegateAnnouncesError(t *testing.T) {
	subAgent := &mockChatter{err: context.DeadlineExceeded}
	announcer := &mockAnnouncer{}
	mgr := taskqueue.New(testLogger(), filepath.Join(t.TempDir(), "tasks.json"), taskqueue.Config{MaxConcurrent: 5})

	tool := &DelegateTool{
		Agents:     map[string]Chatter{"cli-sub": subAgent},
		MaxDepth:   5,
		Announcers: []Announcer{announcer},
		TaskMgr:    mgr,
		Logger:     zap.NewNop().Sugar(),
	}

	ctx := context.WithValue(context.Background(), tools.SessionKeyContextKey{}, "agent:main:telegram:99")
	tool.runAsync(ctx, subAgent, "cli-sub", "subagent:cli-sub:test", "task", "")

	deadline := time.After(5 * time.Second)
	for {
		entries := announcer.getEntries()
		if len(entries) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for announcement")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	entries := announcer.getEntries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 announcement, got %d", len(entries))
	}
	if !strings.Contains(entries[0].Text, "error") {
		t.Errorf("expected error in announcement, got %q", entries[0].Text)
	}
	if !strings.Contains(entries[0].Text, "cli-sub") {
		t.Errorf("expected agent ID in announcement, got %q", entries[0].Text)
	}
}

func TestAsyncDelegateNoMainAgent(t *testing.T) {
	subAgent := &mockChatter{response: "result"}
	announcer := &mockAnnouncer{}
	mgr := taskqueue.New(testLogger(), filepath.Join(t.TempDir(), "tasks.json"), taskqueue.Config{MaxConcurrent: 5})

	tool := &DelegateTool{
		Agents:     map[string]Chatter{"cli-sub": subAgent},
		MaxDepth:   5,
		Announcers: []Announcer{announcer},
		TaskMgr:    mgr,
		Logger:     zap.NewNop().Sugar(),
	}

	ctx := context.WithValue(context.Background(), tools.SessionKeyContextKey{}, "session:1")
	tool.runAsync(ctx, subAgent, "cli-sub", "subagent:cli-sub:test", "task", "")

	deadline := time.After(5 * time.Second)
	for {
		entries := announcer.getEntries()
		if len(entries) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Should announce raw result directly
	entries := announcer.getEntries()
	if !strings.Contains(entries[0].Text, "result") {
		t.Errorf("expected raw result, got %q", entries[0].Text)
	}
}

func TestDelegateStatusEmpty(t *testing.T) {
	mgr := taskqueue.New(testLogger(), filepath.Join(t.TempDir(), "tasks.json"), taskqueue.Config{MaxConcurrent: 5})
	tool := &DelegateTool{Agents: map[string]Chatter{}, MaxDepth: 5, TaskMgr: mgr, Logger: zap.NewNop().Sugar()}
	args, _ := json.Marshal(map[string]string{"action": "status"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "No tasks") {
		t.Errorf("expected 'No tasks', got %q", result)
	}
}

func TestDelegateStatusShowsRunning(t *testing.T) {
	mgr := taskqueue.New(testLogger(), filepath.Join(t.TempDir(), "tasks.json"), taskqueue.Config{MaxConcurrent: 5})
	tool := &DelegateTool{
		Agents:  map[string]Chatter{},
		TaskMgr: mgr,
		Logger:  zap.NewNop().Sugar(),
	}

	// Submit a long-running task so it shows as running
	blockCh := make(chan struct{})
	mgr.Submit("session:test", "coding-agent", "fix the bug", func(ctx context.Context) (string, error) {
		<-blockCh
		return "done", nil
	}, taskqueue.SubmitOpts{})
	// Give goroutine time to start
	time.Sleep(50 * time.Millisecond)

	args, _ := json.Marshal(map[string]string{"action": "status"})
	result := tool.Run(context.Background(), string(args))
	close(blockCh)

	if !strings.Contains(result, "1 task(s)") {
		t.Errorf("expected '1 task(s)', got %q", result)
	}
	if !strings.Contains(result, "coding-agent") {
		t.Errorf("expected agent name in status, got %q", result)
	}
	if !strings.Contains(result, "fix the bug") {
		t.Errorf("expected message in status, got %q", result)
	}
}

func TestAsyncDelegateRoutesResultThroughMainAgent(t *testing.T) {
	subAgent := &mockChatter{response: "subagent output"}
	mainAgent := &mockChatter{response: "The coding agent completed successfully. Tests all pass."}
	announcer := &mockAnnouncer{}
	mgr := taskqueue.New(testLogger(), filepath.Join(t.TempDir(), "tasks.json"), taskqueue.Config{MaxConcurrent: 5})

	tool := &DelegateTool{
		Agents: map[string]Chatter{
			"cli-sub": subAgent,
			"main":    mainAgent,
		},
		MaxDepth:    5,
		Announcers:  []Announcer{announcer},
		TaskMgr:     mgr,
		MainAgentID: "main",
		Logger:      zap.NewNop().Sugar(),
	}

	ctx := context.WithValue(context.Background(), tools.SessionKeyContextKey{}, "main:telegram:12345")
	tool.runAsync(ctx, subAgent, "cli-sub", "subagent:cli-sub:test", "run tests", "")

	deadline := time.After(5 * time.Second)
	for {
		entries := announcer.getEntries()
		if len(entries) >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for announcement")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Verify: main agent was called with the notification containing the subagent result
	mainCalls := mainAgent.getCalls()
	if len(mainCalls) != 1 {
		t.Fatalf("expected 1 main agent call, got %d", len(mainCalls))
	}
	if !strings.Contains(mainCalls[0].Message, "Background task completed") {
		t.Errorf("expected notification prefix, got %q", mainCalls[0].Message)
	}
	if !strings.Contains(mainCalls[0].Message, "subagent output") {
		t.Errorf("expected subagent result in notification, got %q", mainCalls[0].Message)
	}

	// Verify: announcement contains the main agent's response, not the raw subagent output
	entries := announcer.getEntries()
	if entries[0].Text != "The coding agent completed successfully. Tests all pass." {
		t.Errorf("expected main agent response in announcement, got %q", entries[0].Text)
	}
}

func TestAsyncDelegateFallsBackOnMainAgentError(t *testing.T) {
	subAgent := &mockChatter{response: "subagent output"}
	mainAgent := &mockChatter{err: fmt.Errorf("model unavailable")}
	announcer := &mockAnnouncer{}
	mgr := taskqueue.New(testLogger(), filepath.Join(t.TempDir(), "tasks.json"), taskqueue.Config{MaxConcurrent: 5})

	tool := &DelegateTool{
		Agents: map[string]Chatter{
			"cli-sub": subAgent,
			"main":    mainAgent,
		},
		MaxDepth:              5,
		Announcers:            []Announcer{announcer},
		TaskMgr:               mgr,
		MainAgentID:           "main",
		Logger:                zap.NewNop().Sugar(),
		AnnounceMaxRetries:    1,
		AnnounceBaseBackoffMs: 1, // minimal backoff for fast tests
	}

	ctx := context.WithValue(context.Background(), tools.SessionKeyContextKey{}, "main:telegram:99")
	tool.runAsync(ctx, subAgent, "cli-sub", "subagent:cli-sub:test", "task", "")

	deadline := time.After(5 * time.Second)
	for {
		entries := announcer.getEntries()
		if len(entries) >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for announcement")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Verify: falls back to raw announcement since main agent errored
	entries := announcer.getEntries()
	if !strings.Contains(entries[0].Text, "subagent output") {
		t.Errorf("expected raw subagent result in fallback, got %q", entries[0].Text)
	}
	if !strings.Contains(entries[0].Text, "cli-sub result") {
		t.Errorf("expected agent ID in fallback, got %q", entries[0].Text)
	}
}

func TestRandomHex(t *testing.T) {
	a := randomHex(8)
	b := randomHex(8)
	if len(a) != 16 { // 8 bytes → 16 hex chars
		t.Errorf("expected 16 hex chars, got %d", len(a))
	}
	if a == b {
		t.Error("two randomHex calls returned the same value")
	}
}

func TestDefaultMaxDelegateDepthIsOne(t *testing.T) {
	if defaultMaxDelegateDepth != 1 {
		t.Errorf("expected defaultMaxDelegateDepth == 1, got %d", defaultMaxDelegateDepth)
	}
}

func TestDelegateToolDefaultMaxDepthBlocksAtOne(t *testing.T) {
	// With MaxDepth=0 (use default), depth 1 should be blocked.
	tool := &DelegateTool{
		Agents:   map[string]Chatter{"sub": &mockChatter{response: "ok"}},
		MaxDepth: 0, // trigger default
		Logger:   zap.NewNop().Sugar(),
	}
	ctx := context.WithValue(context.Background(), delegateDepthKey{}, 1)
	args, _ := json.Marshal(map[string]string{"agent_id": "sub", "message": "hi"})
	result := tool.Run(ctx, string(args))
	if !strings.Contains(result, "recursion limit") {
		t.Errorf("expected recursion limit at depth 1 with default max 1, got %q", result)
	}
}

func TestDelegateToolPersistentSessionMode(t *testing.T) {
	sub := &mockChatter{response: "done"}
	tool := &DelegateTool{
		Agents:   map[string]Chatter{"sub": sub},
		MaxDepth: 5,
		Logger:   zap.NewNop().Sugar(),
	}
	parentKey := "main:telegram:42"
	ctx := context.WithValue(context.Background(), agentapi.SessionKeyContextKey{}, parentKey)

	args, _ := json.Marshal(map[string]any{
		"agent_id": "sub",
		"message":  "task 1",
		"mode":     "persistent",
	})
	tool.Run(ctx, string(args))

	calls := sub.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	expectedKey := fmt.Sprintf("subagent:sub:persistent:%s", parentKey)
	if calls[0].SessionKey != expectedKey {
		t.Errorf("expected session key %q, got %q", expectedKey, calls[0].SessionKey)
	}

	// Second call with persistent mode should use the same key.
	tool.Run(ctx, string(args))
	calls = sub.getCalls()
	if calls[1].SessionKey != expectedKey {
		t.Errorf("second call session key %q != %q", calls[1].SessionKey, expectedKey)
	}
}

func TestDelegateToolEphemeralSessionIsRandom(t *testing.T) {
	sub := &mockChatter{response: "ok"}
	tool := &DelegateTool{
		Agents:   map[string]Chatter{"sub": sub},
		MaxDepth: 5,
		Logger:   zap.NewNop().Sugar(),
	}
	ctx := context.Background()
	args, _ := json.Marshal(map[string]string{"agent_id": "sub", "message": "go"})

	tool.Run(ctx, string(args))
	tool.Run(ctx, string(args))
	calls := sub.getCalls()
	if calls[0].SessionKey == calls[1].SessionKey {
		t.Error("ephemeral sessions should have different keys")
	}
}

func TestSteerMissingTaskID(t *testing.T) {
	tool := &DelegateTool{
		Agents:  map[string]Chatter{"sub": &mockChatter{response: "ok"}},
		TaskMgr: taskqueue.New(testLogger(), filepath.Join(t.TempDir(), "tasks.json"), taskqueue.Config{MaxConcurrent: 5}),
		Logger:  zap.NewNop().Sugar(),
	}
	args, _ := json.Marshal(map[string]any{"action": "steer", "message": "new direction"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "task_id is required") {
		t.Errorf("expected task_id required error, got %q", result)
	}
}

func TestSteerMissingMessage(t *testing.T) {
	tool := &DelegateTool{
		Agents:  map[string]Chatter{"sub": &mockChatter{response: "ok"}},
		TaskMgr: taskqueue.New(testLogger(), filepath.Join(t.TempDir(), "tasks.json"), taskqueue.Config{MaxConcurrent: 5}),
		Logger:  zap.NewNop().Sugar(),
	}
	args, _ := json.Marshal(map[string]any{"action": "steer", "task_id": "abc123"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "message is required") {
		t.Errorf("expected message required error, got %q", result)
	}
}

func TestSteerNotFoundTask(t *testing.T) {
	mgr := taskqueue.New(testLogger(), filepath.Join(t.TempDir(), "tasks.json"), taskqueue.Config{MaxConcurrent: 5})
	tool := &DelegateTool{
		Agents:  map[string]Chatter{"sub": &mockChatter{response: "ok"}},
		TaskMgr: mgr,
		Logger:  zap.NewNop().Sugar(),
	}
	args, _ := json.Marshal(map[string]any{"action": "steer", "task_id": "nonexistent", "message": "go"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "not found") {
		t.Errorf("expected not found error, got %q", result)
	}
}

func TestSteerCancelsAndRespawns(t *testing.T) {
	// Use a slow sub-agent that blocks until cancelled.
	slowSub := &blockingChatter{ch: make(chan struct{})}
	fastSub := &mockChatter{response: "steered result"}
	announcer := &mockAnnouncer{}
	mgr := taskqueue.New(testLogger(), filepath.Join(t.TempDir(), "tasks.json"), taskqueue.Config{MaxConcurrent: 5})

	tool := &DelegateTool{
		Agents:      map[string]Chatter{"slow": slowSub, "fast": fastSub},
		AsyncAgents: map[string]bool{"slow": true, "fast": true},
		MaxDepth:    5,
		Announcers:  []Announcer{announcer},
		TaskMgr:     mgr,
		Logger:      zap.NewNop().Sugar(),
	}

	ctx := context.WithValue(context.Background(), tools.SessionKeyContextKey{}, "parent:session")
	// Spawn a slow task.
	args, _ := json.Marshal(map[string]any{"agent_id": "slow", "message": "slow task"})
	spawnResult := tool.Run(ctx, string(args))
	if !strings.Contains(spawnResult, "Spawned") {
		t.Fatalf("expected Spawned, got %q", spawnResult)
	}

	// Wait for the task to be running.
	time.Sleep(50 * time.Millisecond)

	// Get the task ID from the manager.
	tasks := mgr.List()
	if len(tasks) == 0 {
		t.Fatal("expected at least 1 task")
	}
	taskID := tasks[0].ID

	// Steer it with a new agent and message.
	steerArgs, _ := json.Marshal(map[string]any{
		"action":   "steer",
		"task_id":  taskID,
		"agent_id": "fast",
		"message":  "new direction",
	})
	steerResult := tool.Run(ctx, string(steerArgs))
	if !strings.Contains(steerResult, "Spawned") {
		t.Errorf("expected steer to spawn new task, got %q", steerResult)
	}

	// Unblock the slow agent so it can finish (it was cancelled, so it will error).
	close(slowSub.ch)

	// Wait for the fast agent's result to be announced.
	deadline := time.After(5 * time.Second)
	for {
		entries := announcer.getEntries()
		if len(entries) >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for steered announcement")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// mockChatterRetry fails the first failCount calls then succeeds.
type mockChatterRetry struct {
	mu        sync.Mutex
	failCount int
	called    int
	response  string
}

func (m *mockChatterRetry) Chat(_ context.Context, sessionKey, message string) (Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.called++
	if m.called <= m.failCount {
		return Response{}, fmt.Errorf("temporary error #%d", m.called)
	}
	return Response{Text: m.response}, nil
}

func (m *mockChatterRetry) getCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.called
}

func TestAnnounceAsyncResultRetriesOnFailure(t *testing.T) {
	// Main agent fails twice then succeeds on third attempt.
	mainAgent := &mockChatterRetry{failCount: 2, response: "summarized result"}
	announcer := &mockAnnouncer{}

	announceAsyncResult(announceParams{
		agentID:          "sub",
		parentSessionKey: "parent:session",
		result:           "raw output",
		err:              nil,
		mainAgentID:      "main",
		agents:           map[string]Chatter{"main": mainAgent},
		announcers:       []Announcer{announcer},
		logger:           zap.NewNop().Sugar(),
		notifPrefix:      "Test",
		maxRetries:       2,
		baseBackoffMs:    0,
	})

	// Should have retried and succeeded.
	callCount := mainAgent.getCallCount()
	if callCount != 3 { // 1 initial + 2 retries
		t.Errorf("expected 3 calls (1 + 2 retries), got %d", callCount)
	}
	entries := announcer.getEntries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 announcement, got %d", len(entries))
	}
	if entries[0].Text != "summarized result" {
		t.Errorf("expected summarized result, got %q", entries[0].Text)
	}
}

func TestAnnounceAsyncResultFallsBackAfterAllRetries(t *testing.T) {
	// Main agent always fails.
	mainAgent := &mockChatterRetry{failCount: 999, response: "never"}
	announcer := &mockAnnouncer{}

	announceAsyncResult(announceParams{
		agentID:          "sub",
		parentSessionKey: "parent:session",
		result:           "raw fallback output",
		err:              nil,
		mainAgentID:      "main",
		agents:           map[string]Chatter{"main": mainAgent},
		announcers:       []Announcer{announcer},
		logger:           zap.NewNop().Sugar(),
		notifPrefix:      "Test",
		maxRetries:       1,
		baseBackoffMs:    0,
	})

	// Should have tried 2 times (1 + 1 retry), then fallen back.
	callCount := mainAgent.getCallCount()
	if callCount != 2 {
		t.Errorf("expected 2 calls, got %d", callCount)
	}
	entries := announcer.getEntries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 fallback announcement, got %d", len(entries))
	}
	if !strings.Contains(entries[0].Text, "raw fallback output") {
		t.Errorf("expected raw result in fallback, got %q", entries[0].Text)
	}
}

func TestDelegateToolSchemaIncludesNewFields(t *testing.T) {
	tool := &DelegateTool{Logger: zap.NewNop().Sugar()}
	var m map[string]any
	if err := json.Unmarshal(tool.Schema(), &m); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	props := m["properties"].(map[string]any)
	for _, field := range []string{"model", "mode", "action"} {
		if _, ok := props[field]; !ok {
			t.Errorf("schema missing new field %q", field)
		}
	}
	// Verify mode has enum with persistent.
	modeProp := props["mode"].(map[string]any)
	modeEnum := modeProp["enum"].([]any)
	found := false
	for _, v := range modeEnum {
		if v == "persistent" {
			found = true
		}
	}
	if !found {
		t.Error("mode enum missing 'persistent'")
	}
	// Verify action has steer.
	actionProp := props["action"].(map[string]any)
	actionEnum := actionProp["enum"].([]any)
	found = false
	for _, v := range actionEnum {
		if v == "steer" {
			found = true
		}
	}
	if !found {
		t.Error("action enum missing 'steer'")
	}
}

// blockingChatter blocks until its channel is closed, honouring context cancellation.
type blockingChatter struct {
	ch chan struct{}
}

func (b *blockingChatter) Chat(ctx context.Context, sessionKey, message string) (Response, error) {
	select {
	case <-ctx.Done():
		return Response{}, ctx.Err()
	case <-b.ch:
		return Response{Text: "unblocked"}, nil
	}
}
