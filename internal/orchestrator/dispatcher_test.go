package orchestrator

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// --- Mock Chatter implementations ---

// echoChatter returns the message as output.
type echoChatter struct {
	delay time.Duration
}

func (c *echoChatter) Chat(_ context.Context, _ string, message string) (ChatResponse, error) {
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	return ChatResponse{Text: "echo: " + message}, nil
}

// failChatter always returns an error.
type failChatter struct{}

func (c *failChatter) Chat(_ context.Context, _ string, _ string) (ChatResponse, error) {
	return ChatResponse{}, fmt.Errorf("agent failed")
}

// recordingChatter records call order and timing.
type recordingChatter struct {
	mu       sync.Mutex
	calls    []string // task messages in call order
	delay    time.Duration
	started  []time.Time
	finished []time.Time
}

func (c *recordingChatter) Chat(_ context.Context, _ string, message string) (ChatResponse, error) {
	c.mu.Lock()
	c.started = append(c.started, time.Now())
	c.mu.Unlock()

	if c.delay > 0 {
		time.Sleep(c.delay)
	}

	c.mu.Lock()
	c.calls = append(c.calls, message)
	c.finished = append(c.finished, time.Now())
	c.mu.Unlock()
	return ChatResponse{Text: "done: " + message}, nil
}

// slowChatter blocks until context is cancelled.
type slowChatter struct{}

func (c *slowChatter) Chat(ctx context.Context, _ string, _ string) (ChatResponse, error) {
	<-ctx.Done()
	return ChatResponse{}, ctx.Err()
}

// concurrencyChatter tracks max concurrent calls.
type concurrencyChatter struct {
	active   atomic.Int32
	maxSeen  atomic.Int32
	delay    time.Duration
}

func (c *concurrencyChatter) Chat(_ context.Context, _ string, message string) (ChatResponse, error) {
	cur := c.active.Add(1)
	for {
		old := c.maxSeen.Load()
		if cur <= old || c.maxSeen.CompareAndSwap(old, cur) {
			break
		}
	}
	time.Sleep(c.delay)
	c.active.Add(-1)
	return ChatResponse{Text: "ok"}, nil
}

// --- Tests ---

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestParallelExecution(t *testing.T) {
	rec := &recordingChatter{delay: 50 * time.Millisecond}
	d := &Dispatcher{
		Agents:        map[string]Chatter{"agent-a": rec},
		MaxConcurrent: 5,
	}

	graph := TaskGraph{Tasks: []Task{
		{ID: "t1", AgentID: "agent-a", Message: "task1", Blocking: true},
		{ID: "t2", AgentID: "agent-a", Message: "task2", Blocking: true},
		{ID: "t3", AgentID: "agent-a", Message: "task3", Blocking: true},
	}}

	start := time.Now()
	rs, err := d.Execute(context.Background(), graph, NoOpProgress)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rs.Tasks) != 3 {
		t.Fatalf("expected 3 results, got %d", len(rs.Tasks))
	}
	for _, r := range rs.Tasks {
		if r.Status != TaskStatusSuccess {
			t.Errorf("task %s: expected success, got %s", r.ID, r.Status)
		}
	}
	// Three 50ms tasks running in parallel should complete in well under 3x
	if elapsed > 200*time.Millisecond {
		t.Errorf("parallel tasks took %v (expected <200ms); may not be running concurrently", elapsed)
	}
}

func TestDependencyOrdering(t *testing.T) {
	rec := &recordingChatter{delay: 10 * time.Millisecond}
	d := &Dispatcher{
		Agents:        map[string]Chatter{"agent-a": rec},
		MaxConcurrent: 5,
	}

	graph := TaskGraph{Tasks: []Task{
		{ID: "t1", AgentID: "agent-a", Message: "first", Blocking: true},
		{ID: "t2", AgentID: "agent-a", Message: "second", DependsOn: []string{"t1"}, Blocking: true},
		{ID: "t3", AgentID: "agent-a", Message: "third", DependsOn: []string{"t2"}, Blocking: true},
	}}

	rs, err := d.Execute(context.Background(), graph, NoOpProgress)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify all succeeded
	for _, r := range rs.Tasks {
		if r.Status != TaskStatusSuccess {
			t.Errorf("task %s: expected success, got %s", r.ID, r.Status)
		}
	}

	// Verify ordering: t1 finished before t2 started, t2 finished before t3 started
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.calls) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(rec.calls))
	}
	if rec.calls[0] != "first" || rec.calls[1] != "second" || rec.calls[2] != "third" {
		t.Errorf("unexpected call order: %v", rec.calls)
	}
}

func TestBlockingFailureCancelsDependents(t *testing.T) {
	d := &Dispatcher{
		Agents: map[string]Chatter{
			"good":  &echoChatter{},
			"bad":   &failChatter{},
		},
		MaxConcurrent: 5,
	}

	graph := TaskGraph{Tasks: []Task{
		{ID: "t1", AgentID: "bad", Message: "will fail", Blocking: true},
		{ID: "t2", AgentID: "good", Message: "depends on t1", DependsOn: []string{"t1"}, Blocking: true},
		{ID: "t3", AgentID: "good", Message: "depends on t2", DependsOn: []string{"t2"}, Blocking: true},
	}}

	rs, err := d.Execute(context.Background(), graph, NoOpProgress)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r1 := rs.ByID("t1")
	if r1.Status != TaskStatusFailed {
		t.Errorf("t1: expected failed, got %s", r1.Status)
	}
	r2 := rs.ByID("t2")
	if r2.Status != TaskStatusCancelled {
		t.Errorf("t2: expected cancelled, got %s", r2.Status)
	}
	r3 := rs.ByID("t3")
	if r3.Status != TaskStatusCancelled {
		t.Errorf("t3: expected cancelled, got %s", r3.Status)
	}
}

func TestNonBlockingFailureContinues(t *testing.T) {
	d := &Dispatcher{
		Agents: map[string]Chatter{
			"good": &echoChatter{},
			"bad":  &failChatter{},
		},
		MaxConcurrent: 5,
	}

	graph := TaskGraph{Tasks: []Task{
		{ID: "t1", AgentID: "bad", Message: "will fail", Blocking: false},
		{ID: "t2", AgentID: "good", Message: "independent", Blocking: true},
	}}

	rs, err := d.Execute(context.Background(), graph, NoOpProgress)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r1 := rs.ByID("t1")
	if r1.Status != TaskStatusFailed {
		t.Errorf("t1: expected failed, got %s", r1.Status)
	}
	r2 := rs.ByID("t2")
	if r2.Status != TaskStatusSuccess {
		t.Errorf("t2: expected success, got %s", r2.Status)
	}
}

func TestCycleDetection(t *testing.T) {
	d := &Dispatcher{
		Agents:        map[string]Chatter{"agent-a": &echoChatter{}},
		MaxConcurrent: 5,
	}

	graph := TaskGraph{Tasks: []Task{
		{ID: "t1", AgentID: "agent-a", Message: "a", DependsOn: []string{"t2"}, Blocking: true},
		{ID: "t2", AgentID: "agent-a", Message: "b", DependsOn: []string{"t1"}, Blocking: true},
	}}

	_, err := d.Execute(context.Background(), graph, NoOpProgress)
	if err == nil {
		t.Fatal("expected cycle detection error, got nil")
	}
	if !containsString(err.Error(), "cycle") {
		t.Errorf("expected cycle error, got: %v", err)
	}
}

func TestTimeout(t *testing.T) {
	d := &Dispatcher{
		Agents:        map[string]Chatter{"slow": &slowChatter{}},
		MaxConcurrent: 5,
	}

	graph := TaskGraph{Tasks: []Task{
		{ID: "t1", AgentID: "slow", Message: "will timeout", Blocking: true, TimeoutSeconds: 1},
	}}

	start := time.Now()
	rs, err := d.Execute(context.Background(), graph, NoOpProgress)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r1 := rs.ByID("t1")
	if r1.Status != TaskStatusTimeout {
		t.Errorf("t1: expected timeout, got %s (error: %s)", r1.Status, r1.Error)
	}
	if elapsed > 3*time.Second {
		t.Errorf("timeout took %v, expected ~1s", elapsed)
	}
}

func TestMaxConcurrency(t *testing.T) {
	cc := &concurrencyChatter{delay: 100 * time.Millisecond}
	d := &Dispatcher{
		Agents:        map[string]Chatter{"agent-a": cc},
		MaxConcurrent: 2,
	}

	graph := TaskGraph{Tasks: []Task{
		{ID: "t1", AgentID: "agent-a", Message: "a", Blocking: true},
		{ID: "t2", AgentID: "agent-a", Message: "b", Blocking: true},
		{ID: "t3", AgentID: "agent-a", Message: "c", Blocking: true},
		{ID: "t4", AgentID: "agent-a", Message: "d", Blocking: true},
		{ID: "t5", AgentID: "agent-a", Message: "e", Blocking: true},
	}}

	rs, err := d.Execute(context.Background(), graph, NoOpProgress)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, r := range rs.Tasks {
		if r.Status != TaskStatusSuccess {
			t.Errorf("task %s: expected success, got %s", r.ID, r.Status)
		}
	}
	maxSeen := cc.maxSeen.Load()
	if maxSeen > 2 {
		t.Errorf("max concurrent: expected <=2, saw %d", maxSeen)
	}
}

func TestValidation_EmptyGraph(t *testing.T) {
	d := &Dispatcher{
		Agents:        map[string]Chatter{"a": &echoChatter{}},
		MaxConcurrent: 5,
	}
	_, err := d.Execute(context.Background(), TaskGraph{}, NoOpProgress)
	if err == nil {
		t.Fatal("expected error for empty graph")
	}
}

func TestValidation_MissingFields(t *testing.T) {
	d := &Dispatcher{
		Agents:        map[string]Chatter{"a": &echoChatter{}},
		MaxConcurrent: 5,
	}

	tests := []struct {
		name string
		task Task
	}{
		{"missing id", Task{AgentID: "a", Message: "m"}},
		{"missing agent_id", Task{ID: "t1", Message: "m"}},
		{"missing message", Task{ID: "t1", AgentID: "a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := d.Execute(context.Background(), TaskGraph{Tasks: []Task{tt.task}}, NoOpProgress)
			if err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestValidation_UnknownAgent(t *testing.T) {
	d := &Dispatcher{
		Agents:        map[string]Chatter{"known": &echoChatter{}},
		MaxConcurrent: 5,
	}
	graph := TaskGraph{Tasks: []Task{
		{ID: "t1", AgentID: "unknown", Message: "m", Blocking: true},
	}}
	_, err := d.Execute(context.Background(), graph, NoOpProgress)
	if err == nil {
		t.Fatal("expected error for unknown agent")
	}
	if !containsString(err.Error(), "unknown agent") {
		t.Errorf("expected unknown agent error, got: %v", err)
	}
}

func TestValidation_UnknownDependency(t *testing.T) {
	d := &Dispatcher{
		Agents:        map[string]Chatter{"a": &echoChatter{}},
		MaxConcurrent: 5,
	}
	graph := TaskGraph{Tasks: []Task{
		{ID: "t1", AgentID: "a", Message: "m", DependsOn: []string{"nonexistent"}, Blocking: true},
	}}
	_, err := d.Execute(context.Background(), graph, NoOpProgress)
	if err == nil {
		t.Fatal("expected error for unknown dependency")
	}
}

func TestOutputInterpolation(t *testing.T) {
	d := &Dispatcher{
		Agents:        map[string]Chatter{"a": &echoChatter{}},
		MaxConcurrent: 5,
	}

	graph := TaskGraph{Tasks: []Task{
		{ID: "t1", AgentID: "a", Message: "hello", Blocking: true},
		{ID: "t2", AgentID: "a", Message: "received: {{t1.output}}", DependsOn: []string{"t1"}, Blocking: true},
	}}

	rs, err := d.Execute(context.Background(), graph, NoOpProgress)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r2 := rs.ByID("t2")
	if r2.Status != TaskStatusSuccess {
		t.Fatalf("t2: expected success, got %s", r2.Status)
	}
	// t1 output is "echo: hello", so t2's message becomes "received: echo: hello"
	// and t2's output is "echo: received: echo: hello"
	expected := "echo: received: echo: hello"
	if r2.Output != expected {
		t.Errorf("t2 output: expected %q, got %q", expected, r2.Output)
	}
}

func TestProgressCallback(t *testing.T) {
	d := &Dispatcher{
		Agents:        map[string]Chatter{"a": &echoChatter{}},
		MaxConcurrent: 5,
	}

	graph := TaskGraph{Tasks: []Task{
		{ID: "t1", AgentID: "a", Message: "hello", Blocking: true},
		{ID: "t2", AgentID: "a", Message: "world", Blocking: true},
	}}

	var mu sync.Mutex
	var updates []string
	progress := func(taskID, agentID string, status TaskStatus) {
		mu.Lock()
		updates = append(updates, fmt.Sprintf("%s:%s:%s", taskID, agentID, status))
		mu.Unlock()
	}

	rs, err := d.Execute(context.Background(), graph, progress)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, r := range rs.Tasks {
		if r.Status != TaskStatusSuccess {
			t.Errorf("task %s: expected success, got %s", r.ID, r.Status)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(updates) != 2 {
		t.Errorf("expected 2 progress updates, got %d: %v", len(updates), updates)
	}
}

func TestContextCancellation(t *testing.T) {
	d := &Dispatcher{
		Agents:        map[string]Chatter{"slow": &slowChatter{}},
		MaxConcurrent: 5,
	}

	graph := TaskGraph{Tasks: []Task{
		{ID: "t1", AgentID: "slow", Message: "will be cancelled", Blocking: true},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	rs, err := d.Execute(ctx, graph, NoOpProgress)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r1 := rs.ByID("t1")
	// Could be failed or cancelled depending on timing
	if r1.Status == TaskStatusSuccess || r1.Status == TaskStatusPending {
		t.Errorf("t1: expected failed/cancelled/timeout, got %s", r1.Status)
	}
}

func TestNonBlockingFailureWithDependents(t *testing.T) {
	// Non-blocking task fails, but its dependent should still run
	// (the dependent won't have interpolated output, but shouldn't be cancelled)
	d := &Dispatcher{
		Agents: map[string]Chatter{
			"good": &echoChatter{},
			"bad":  &failChatter{},
		},
		MaxConcurrent: 5,
	}

	graph := TaskGraph{Tasks: []Task{
		{ID: "t1", AgentID: "bad", Message: "will fail", Blocking: false},
		{ID: "t2", AgentID: "good", Message: "after failure: {{t1.output}}", DependsOn: []string{"t1"}, Blocking: true},
	}}

	rs, err := d.Execute(context.Background(), graph, NoOpProgress)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r1 := rs.ByID("t1")
	if r1.Status != TaskStatusFailed {
		t.Errorf("t1: expected failed, got %s", r1.Status)
	}
	// t2 should still run (non-blocking failure doesn't cancel dependents)
	r2 := rs.ByID("t2")
	if r2.Status != TaskStatusSuccess {
		t.Errorf("t2: expected success, got %s (error: %s)", r2.Status, r2.Error)
	}
}

func TestDuplicateTaskID(t *testing.T) {
	d := &Dispatcher{
		Agents:        map[string]Chatter{"a": &echoChatter{}},
		MaxConcurrent: 5,
	}
	graph := TaskGraph{Tasks: []Task{
		{ID: "t1", AgentID: "a", Message: "m1", Blocking: true},
		{ID: "t1", AgentID: "a", Message: "m2", Blocking: true},
	}}
	_, err := d.Execute(context.Background(), graph, NoOpProgress)
	if err == nil {
		t.Fatal("expected error for duplicate task ID")
	}
}

func TestResultSetFormatAppendix(t *testing.T) {
	rs := ResultSet{Tasks: []TaskResult{
		{ID: "t1", AgentID: "agent-a", Status: TaskStatusSuccess, Output: "hello"},
		{ID: "t2", AgentID: "agent-b", Status: TaskStatusFailed, Error: "boom"},
	}}
	appendix := rs.FormatAppendix()
	if !containsString(appendix, "task: t1") {
		t.Error("appendix missing t1")
	}
	if !containsString(appendix, "hello") {
		t.Error("appendix missing t1 output")
	}
	if !containsString(appendix, "ERROR: boom") {
		t.Error("appendix missing t2 error")
	}
}

// --- Limiter integration tests ---

// chanLimiter is a simple channel-based Limiter for testing.
type chanLimiter struct {
	ch      chan struct{}
	maxSeen atomic.Int32
	current atomic.Int32
}

func newChanLimiter(n int) *chanLimiter {
	return &chanLimiter{ch: make(chan struct{}, n)}
}

func (l *chanLimiter) Acquire(ctx context.Context) error {
	select {
	case l.ch <- struct{}{}:
		cur := l.current.Add(1)
		for {
			old := l.maxSeen.Load()
			if cur > old {
				if l.maxSeen.CompareAndSwap(old, cur) {
					break
				}
			} else {
				break
			}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *chanLimiter) Release() {
	l.current.Add(-1)
	<-l.ch
}

func TestExecuteWithExternalLimiter(t *testing.T) {
	defer goleak.VerifyNone(t)

	limiter := newChanLimiter(2) // allow only 2 concurrent

	recorder := &recordingChatter{delay: 50 * time.Millisecond}
	d := &Dispatcher{
		Agents:        map[string]Chatter{"a": recorder},
		MaxConcurrent: 10, // internal semaphore is large, but limiter caps at 2
		Limiter:       limiter,
	}

	graph := TaskGraph{
		Tasks: []Task{
			{ID: "t1", AgentID: "a", Message: "one", Blocking: false},
			{ID: "t2", AgentID: "a", Message: "two", Blocking: false},
			{ID: "t3", AgentID: "a", Message: "three", Blocking: false},
			{ID: "t4", AgentID: "a", Message: "four", Blocking: false},
		},
	}

	rs, err := d.Execute(context.Background(), graph, NoOpProgress)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// All tasks should succeed
	for _, r := range rs.Tasks {
		if r.Status != TaskStatusSuccess {
			t.Errorf("task %s: status=%s, err=%s", r.ID, r.Status, r.Error)
		}
	}

	// Max concurrent should not exceed the limiter's capacity (2)
	if got := limiter.maxSeen.Load(); got > 2 {
		t.Errorf("max concurrent = %d, want <= 2 (limiter capacity)", got)
	}
}

func TestExecuteWithLimiterContextCancel(t *testing.T) {
	defer goleak.VerifyNone(t)

	limiter := newChanLimiter(1) // only 1 at a time

	slowAgent := &echoChatter{delay: 200 * time.Millisecond}
	d := &Dispatcher{
		Agents:        map[string]Chatter{"a": slowAgent},
		MaxConcurrent: 5,
		Limiter:       limiter,
	}

	graph := TaskGraph{
		Tasks: []Task{
			{ID: "t1", AgentID: "a", Message: "slow", Blocking: true},
			{ID: "t2", AgentID: "a", Message: "queued", Blocking: false},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	rs, err := d.Execute(ctx, graph, NoOpProgress)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// At least one task should be cancelled due to context timeout
	var cancelled int
	for _, r := range rs.Tasks {
		if r.Status == TaskStatusCancelled {
			cancelled++
		}
	}
	if cancelled == 0 {
		t.Error("expected at least one cancelled task due to context timeout")
	}
}

// --- Helpers ---

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstring(s, substr))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// --- ParseTaskGraph tests ---

func TestParseTaskGraph_Valid(t *testing.T) {
	input := `{"tasks": [{"id": "t1", "agent_id": "a1", "message": "do stuff", "blocking": true}]}`
	g, err := ParseTaskGraph([]byte(input))
	if err != nil {
		t.Fatalf("ParseTaskGraph: unexpected error: %v", err)
	}
	if len(g.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(g.Tasks))
	}
	if g.Tasks[0].ID != "t1" {
		t.Errorf("task ID = %q, want %q", g.Tasks[0].ID, "t1")
	}
	if g.Tasks[0].AgentID != "a1" {
		t.Errorf("agent_id = %q, want %q", g.Tasks[0].AgentID, "a1")
	}
	if g.Tasks[0].Message != "do stuff" {
		t.Errorf("message = %q, want %q", g.Tasks[0].Message, "do stuff")
	}
	if !g.Tasks[0].Blocking {
		t.Error("expected blocking = true")
	}
}

func TestParseTaskGraph_WithDependencies(t *testing.T) {
	input := `{
		"tasks": [
			{"id": "t1", "agent_id": "a1", "message": "first", "blocking": true},
			{"id": "t2", "agent_id": "a2", "message": "second", "depends_on": ["t1"], "blocking": false, "timeout_seconds": 30}
		]
	}`
	g, err := ParseTaskGraph([]byte(input))
	if err != nil {
		t.Fatalf("ParseTaskGraph: unexpected error: %v", err)
	}
	if len(g.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(g.Tasks))
	}
	if len(g.Tasks[1].DependsOn) != 1 || g.Tasks[1].DependsOn[0] != "t1" {
		t.Errorf("depends_on = %v, want [t1]", g.Tasks[1].DependsOn)
	}
	if g.Tasks[1].TimeoutSeconds != 30 {
		t.Errorf("timeout_seconds = %d, want 30", g.Tasks[1].TimeoutSeconds)
	}
}

func TestParseTaskGraph_InvalidJSON(t *testing.T) {
	_, err := ParseTaskGraph([]byte(`{invalid json`))
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
	if !containsString(err.Error(), "parse task graph") {
		t.Errorf("expected wrapped error, got: %v", err)
	}
}

func TestParseTaskGraph_EmptyTasks(t *testing.T) {
	g, err := ParseTaskGraph([]byte(`{"tasks": []}`))
	if err != nil {
		t.Fatalf("ParseTaskGraph: unexpected error: %v", err)
	}
	if len(g.Tasks) != 0 {
		t.Errorf("expected 0 tasks, got %d", len(g.Tasks))
	}
}

func TestParseTaskGraph_EmptyObject(t *testing.T) {
	g, err := ParseTaskGraph([]byte(`{}`))
	if err != nil {
		t.Fatalf("ParseTaskGraph: unexpected error: %v", err)
	}
	if g.Tasks != nil {
		t.Errorf("expected nil tasks, got %v", g.Tasks)
	}
}

// --- NoOpProgress test ---

func TestNoOpProgress(t *testing.T) {
	// NoOpProgress should be callable without panic.
	NoOpProgress("task-1", "agent-a", TaskStatusSuccess)
	NoOpProgress("task-2", "agent-b", TaskStatusFailed)
	NoOpProgress("", "", "")
}

// --- ResultSet.ByID edge case ---

func TestResultSetByID_NotFound(t *testing.T) {
	rs := &ResultSet{Tasks: []TaskResult{
		{ID: "t1", Status: TaskStatusSuccess},
	}}
	if got := rs.ByID("nonexistent"); got != nil {
		t.Errorf("expected nil for nonexistent ID, got %+v", got)
	}
}

// --- FormatAppendix edge cases ---

func TestFormatAppendix_CancelledAndOtherStatuses(t *testing.T) {
	rs := ResultSet{Tasks: []TaskResult{
		{ID: "t1", AgentID: "a", Status: TaskStatusCancelled, Error: "dependency failed"},
		{ID: "t2", AgentID: "b", Status: TaskStatusTimeout, Error: "timed out"},
		{ID: "t3", AgentID: "c", Status: TaskStatusPending},
	}}
	appendix := rs.FormatAppendix()
	if !containsString(appendix, "CANCELLED: dependency failed") {
		t.Error("appendix missing CANCELLED status")
	}
	if !containsString(appendix, "ERROR: timed out") {
		t.Error("appendix missing timeout ERROR")
	}
	if !containsString(appendix, "STATUS: pending") {
		t.Error("appendix missing pending STATUS")
	}
}

func TestFormatAppendix_TruncatesLongOutput(t *testing.T) {
	longOutput := ""
	for range 3000 {
		longOutput += "x"
	}
	rs := ResultSet{Tasks: []TaskResult{
		{ID: "t1", AgentID: "a", Status: TaskStatusSuccess, Output: longOutput},
	}}
	appendix := rs.FormatAppendix()
	if !containsString(appendix, "... (truncated)") {
		t.Error("expected truncation marker in appendix for long output")
	}
}
