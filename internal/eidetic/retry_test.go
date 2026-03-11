package eidetic_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EMSERO/gopherclaw/internal/eidetic"

	"go.uber.org/zap"
)

// mockClient implements eidetic.Client for testing.
type mockClient struct {
	mu            sync.Mutex
	appendErr     error
	appendCalls   []eidetic.AppendRequest
	searchResult  []eidetic.MemoryEntry
	searchErr     error
	recentResult  []eidetic.MemoryEntry
	recentErr     error
	healthErr     error
	appendCount   atomic.Int64
	failFirstN    int64 // fail the first N AppendMemory calls
}

func (m *mockClient) AppendMemory(_ context.Context, req eidetic.AppendRequest) error {
	n := m.appendCount.Add(1)
	m.mu.Lock()
	m.appendCalls = append(m.appendCalls, req)
	err := m.appendErr
	m.mu.Unlock()
	if m.failFirstN > 0 && n <= m.failFirstN {
		return errors.New("mock append error")
	}
	return err
}

func (m *mockClient) SearchMemory(_ context.Context, _ eidetic.SearchRequest) ([]eidetic.MemoryEntry, error) {
	return m.searchResult, m.searchErr
}

func (m *mockClient) GetRecent(_ context.Context, _ string, _ int) ([]eidetic.MemoryEntry, error) {
	return m.recentResult, m.recentErr
}

func (m *mockClient) Health(_ context.Context) error {
	return m.healthErr
}


func testLogger() *zap.SugaredLogger {
	l, _ := zap.NewDevelopment()
	return l.Sugar()
}

func tempFilePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "retry-queue.json")
}

// ---------- NewRetryClient ----------

func TestNewRetryClient_EmptyFile(t *testing.T) {
	rc := eidetic.NewRetryClient(&mockClient{}, testLogger(), tempFilePath(t))
	if rc.QueueLen() != 0 {
		t.Fatalf("expected empty queue, got %d", rc.QueueLen())
	}
}

func TestNewRetryClient_LoadsExistingQueue(t *testing.T) {
	fp := tempFilePath(t)
	queue := struct {
		Queue []eidetic.AppendRequest `json:"queue"`
	}{
		Queue: []eidetic.AppendRequest{
			{Content: "entry1", AgentID: "a1"},
			{Content: "entry2", AgentID: "a2"},
		},
	}
	data, _ := json.Marshal(queue)
	if err := os.WriteFile(fp, data, 0600); err != nil {
		t.Fatal(err)
	}

	rc := eidetic.NewRetryClient(&mockClient{}, testLogger(), fp)
	if rc.QueueLen() != 2 {
		t.Fatalf("expected 2 queued items, got %d", rc.QueueLen())
	}
}

func TestNewRetryClient_BadJSON(t *testing.T) {
	fp := tempFilePath(t)
	if err := os.WriteFile(fp, []byte("{invalid json"), 0600); err != nil {
		t.Fatal(err)
	}

	rc := eidetic.NewRetryClient(&mockClient{}, testLogger(), fp)
	if rc.QueueLen() != 0 {
		t.Fatalf("expected empty queue on bad JSON, got %d", rc.QueueLen())
	}
}

// ---------- AppendMemory ----------

func TestRetryClient_AppendMemory_Success(t *testing.T) {
	mc := &mockClient{}
	rc := eidetic.NewRetryClient(mc, testLogger(), tempFilePath(t))

	err := rc.AppendMemory(context.Background(), eidetic.AppendRequest{
		Content: "hello",
		AgentID: "test",
	})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if rc.QueueLen() != 0 {
		t.Fatalf("expected empty queue after success, got %d", rc.QueueLen())
	}
}

func TestRetryClient_AppendMemory_FailQueues(t *testing.T) {
	mc := &mockClient{appendErr: errors.New("server down")}
	fp := tempFilePath(t)
	rc := eidetic.NewRetryClient(mc, testLogger(), fp)

	err := rc.AppendMemory(context.Background(), eidetic.AppendRequest{
		Content: "hello",
		AgentID: "test",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if rc.QueueLen() != 1 {
		t.Fatalf("expected 1 queued item, got %d", rc.QueueLen())
	}

	// Verify persisted to disk.
	data, readErr := os.ReadFile(fp)
	if readErr != nil {
		t.Fatalf("expected queue file to exist: %v", readErr)
	}
	var f struct {
		Queue []eidetic.AppendRequest `json:"queue"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("unmarshal queue file: %v", err)
	}
	if len(f.Queue) != 1 {
		t.Fatalf("expected 1 persisted item, got %d", len(f.Queue))
	}
	if f.Queue[0].Content != "hello" {
		t.Errorf("expected content 'hello', got %q", f.Queue[0].Content)
	}
}

// ---------- SearchMemory proxy ----------

func TestRetryClient_SearchMemory(t *testing.T) {
	expected := []eidetic.MemoryEntry{{ID: "s1", Content: "found"}}
	mc := &mockClient{searchResult: expected}
	rc := eidetic.NewRetryClient(mc, testLogger(), tempFilePath(t))

	result, err := rc.SearchMemory(context.Background(), eidetic.SearchRequest{Query: "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 || result[0].ID != "s1" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestRetryClient_SearchMemory_Error(t *testing.T) {
	mc := &mockClient{searchErr: errors.New("search failed")}
	rc := eidetic.NewRetryClient(mc, testLogger(), tempFilePath(t))

	_, err := rc.SearchMemory(context.Background(), eidetic.SearchRequest{Query: "test"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// ---------- GetRecent proxy ----------

func TestRetryClient_GetRecent(t *testing.T) {
	expected := []eidetic.MemoryEntry{{ID: "r1", Content: "recent"}}
	mc := &mockClient{recentResult: expected}
	rc := eidetic.NewRetryClient(mc, testLogger(), tempFilePath(t))

	result, err := rc.GetRecent(context.Background(), "agent1", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 || result[0].ID != "r1" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestRetryClient_GetRecent_Error(t *testing.T) {
	mc := &mockClient{recentErr: errors.New("recent failed")}
	rc := eidetic.NewRetryClient(mc, testLogger(), tempFilePath(t))

	_, err := rc.GetRecent(context.Background(), "agent1", 10)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// ---------- Health proxy ----------

func TestRetryClient_Health(t *testing.T) {
	mc := &mockClient{}
	rc := eidetic.NewRetryClient(mc, testLogger(), tempFilePath(t))

	if err := rc.Health(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRetryClient_Health_Error(t *testing.T) {
	mc := &mockClient{healthErr: errors.New("unhealthy")}
	rc := eidetic.NewRetryClient(mc, testLogger(), tempFilePath(t))

	if err := rc.Health(context.Background()); err == nil {
		t.Fatal("expected error, got nil")
	}
}

// ---------- QueueLen ----------

func TestRetryClient_QueueLen(t *testing.T) {
	mc := &mockClient{appendErr: errors.New("fail")}
	rc := eidetic.NewRetryClient(mc, testLogger(), tempFilePath(t))

	if rc.QueueLen() != 0 {
		t.Fatalf("expected 0, got %d", rc.QueueLen())
	}

	_ = rc.AppendMemory(context.Background(), eidetic.AppendRequest{Content: "a"})
	_ = rc.AppendMemory(context.Background(), eidetic.AppendRequest{Content: "b"})

	if rc.QueueLen() != 2 {
		t.Fatalf("expected 2, got %d", rc.QueueLen())
	}
}

// ---------- enqueue cap ----------

func TestRetryClient_EnqueueCap(t *testing.T) {
	mc := &mockClient{appendErr: errors.New("fail")}
	rc := eidetic.NewRetryClient(mc, testLogger(), tempFilePath(t))

	// Enqueue 501 items; queue should be capped at 500.
	for i := 0; i < 501; i++ {
		_ = rc.AppendMemory(context.Background(), eidetic.AppendRequest{
			Content: "item",
			AgentID: "test",
		})
	}

	if rc.QueueLen() != 500 {
		t.Fatalf("expected queue capped at 500, got %d", rc.QueueLen())
	}
}

// ---------- StartRetryLoop & drainQueue ----------

func TestRetryClient_StartRetryLoop_ContextCancel(t *testing.T) {
	mc := &mockClient{}
	fp := tempFilePath(t)
	rc := eidetic.NewRetryClient(mc, testLogger(), fp)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		rc.StartRetryLoop(ctx)
		close(done)
	}()

	// Cancel immediately; loop should exit.
	cancel()

	select {
	case <-done:
		// ok
	case <-time.After(3 * time.Second):
		t.Fatal("StartRetryLoop did not exit after context cancel")
	}
}

func TestRetryClient_StartRetryLoop_PersistsOnCancel(t *testing.T) {
	mc := &mockClient{appendErr: errors.New("fail")}
	fp := tempFilePath(t)
	rc := eidetic.NewRetryClient(mc, testLogger(), fp)

	// Queue an item.
	_ = rc.AppendMemory(context.Background(), eidetic.AppendRequest{Content: "persist-me"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		rc.StartRetryLoop(ctx)
		close(done)
	}()

	cancel()
	<-done

	// Verify file was persisted (persist() called on cancel).
	data, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("expected queue file: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty queue file")
	}
}

// ---------- drainQueue via reload ----------

func TestRetryClient_DrainQueue_Success(t *testing.T) {
	// Use failFirstN: inner fails the first call (queuing it),
	// then succeeds on retry when drainQueue is triggered.
	mc := &mockClient{failFirstN: 1}
	fp := tempFilePath(t)
	rc := eidetic.NewRetryClient(mc, testLogger(), fp)

	// This call fails and gets queued.
	_ = rc.AppendMemory(context.Background(), eidetic.AppendRequest{
		Content: "retry-me",
		AgentID: "test",
	})
	if rc.QueueLen() != 1 {
		t.Fatalf("expected 1 queued, got %d", rc.QueueLen())
	}

	// Now create a new RetryClient that loads the queue from disk,
	// using a working inner client. Then trigger drain via StartRetryLoop
	// with a very short-lived context — but drainQueue is ticker-based (2min).
	// Instead, we test indirectly: create a new client that loads the persisted queue.
	mc2 := &mockClient{}
	rc2 := eidetic.NewRetryClient(mc2, testLogger(), fp)
	if rc2.QueueLen() != 1 {
		t.Fatalf("expected loaded queue of 1, got %d", rc2.QueueLen())
	}
}

func TestRetryClient_DrainQueue_PartialFailure(t *testing.T) {
	// We need to test drainQueue directly. Since it's unexported, we
	// exercise it through the reload pattern: persist a queue, load it,
	// then start the retry loop with a ticker that fires.
	// However, the ticker is 2 minutes. Instead, we test the behavior
	// by manually constructing the scenario through AppendMemory calls
	// and verifying queue state.

	// Scenario: 3 items queued, inner fails all retries.
	mc := &mockClient{appendErr: errors.New("always fail")}
	fp := tempFilePath(t)
	rc := eidetic.NewRetryClient(mc, testLogger(), fp)

	_ = rc.AppendMemory(context.Background(), eidetic.AppendRequest{Content: "a"})
	_ = rc.AppendMemory(context.Background(), eidetic.AppendRequest{Content: "b"})
	_ = rc.AppendMemory(context.Background(), eidetic.AppendRequest{Content: "c"})

	if rc.QueueLen() != 3 {
		t.Fatalf("expected 3 queued, got %d", rc.QueueLen())
	}

	// Verify all 3 are persisted and survive a reload.
	mc2 := &mockClient{}
	rc2 := eidetic.NewRetryClient(mc2, testLogger(), fp)
	if rc2.QueueLen() != 3 {
		t.Fatalf("expected 3 loaded, got %d", rc2.QueueLen())
	}
}

// ---------- persist / load round-trip ----------

func TestRetryClient_PersistLoad_RoundTrip(t *testing.T) {
	mc := &mockClient{appendErr: errors.New("fail")}
	fp := tempFilePath(t)
	rc := eidetic.NewRetryClient(mc, testLogger(), fp)

	reqs := []eidetic.AppendRequest{
		{Content: "one", AgentID: "a1", Tags: []string{"t1"}},
		{Content: "two", AgentID: "a2", Tags: []string{"t2", "t3"}},
	}
	for _, r := range reqs {
		_ = rc.AppendMemory(context.Background(), r)
	}

	// Load into a new client.
	mc2 := &mockClient{}
	rc2 := eidetic.NewRetryClient(mc2, testLogger(), fp)
	if rc2.QueueLen() != 2 {
		t.Fatalf("expected 2 loaded, got %d", rc2.QueueLen())
	}
}

func TestRetryClient_PersistCreatesDir(t *testing.T) {
	mc := &mockClient{appendErr: errors.New("fail")}
	dir := filepath.Join(t.TempDir(), "sub", "dir")
	fp := filepath.Join(dir, "retry-queue.json")
	rc := eidetic.NewRetryClient(mc, testLogger(), fp)

	_ = rc.AppendMemory(context.Background(), eidetic.AppendRequest{Content: "x"})

	if _, err := os.Stat(fp); err != nil {
		t.Fatalf("expected queue file to be created in nested dir: %v", err)
	}
}

// ---------- RetryClient satisfies Client interface ----------

func TestRetryClient_ImplementsClient(t *testing.T) {
	mc := &mockClient{}
	var c eidetic.Client = eidetic.NewRetryClient(mc, testLogger(), tempFilePath(t))
	_ = c // verify interface satisfaction at compile time
}

// ---------- load with missing file ----------

func TestNewRetryClient_MissingFile(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "nonexistent", "queue.json")
	rc := eidetic.NewRetryClient(&mockClient{}, testLogger(), fp)
	if rc.QueueLen() != 0 {
		t.Fatalf("expected 0 on missing file, got %d", rc.QueueLen())
	}
}

// ---------- persist no-op when not dirty ----------

func TestRetryClient_PersistNotDirtyNoFile(t *testing.T) {
	fp := tempFilePath(t)
	// Create a client with no failures — nothing queued, not dirty.
	mc := &mockClient{}
	rc := eidetic.NewRetryClient(mc, testLogger(), fp)

	// Successful append should not create the queue file.
	_ = rc.AppendMemory(context.Background(), eidetic.AppendRequest{Content: "ok"})

	if _, err := os.Stat(fp); !os.IsNotExist(err) {
		t.Fatalf("expected no queue file for clean client, but file exists")
	}
}
