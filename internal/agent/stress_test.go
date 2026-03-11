package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/agentapi"
	"github.com/EMSERO/gopherclaw/internal/taskqueue"
	"github.com/EMSERO/gopherclaw/internal/tools"
)

// slowChatter responds after a short delay to simulate real work.
type slowChatter struct {
	delay    time.Duration
	response string
	mu       sync.Mutex
	calls    int
}

func (s *slowChatter) Chat(ctx context.Context, sessionKey, message string) (Response, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return Response{Text: s.response}, nil
}


// TestStress_MultipleAsyncDelegatesSimultaneous exercises multiple async
// delegates completing at the same time for the same parent session.
func TestStress_MultipleAsyncDelegatesSimultaneous(t *testing.T) {
	t.Parallel()

	const delegateCount = 50
	announcer := &mockAnnouncer{}
	mgr := taskqueue.New(zap.NewNop().Sugar(), filepath.Join(t.TempDir(), "tasks.json"), taskqueue.Config{
		MaxConcurrent: 20,
	})
	defer mgr.Shutdown()

	agents := make(map[string]Chatter)
	for i := 0; i < delegateCount; i++ {
		id := fmt.Sprintf("sub-%d", i)
		agents[id] = &slowChatter{delay: time.Millisecond, response: fmt.Sprintf("result-%d", i)}
	}

	tool := &DelegateTool{
		Agents:                agents,
		AsyncAgents:           make(map[string]bool),
		MaxDepth:              5,
		Announcers:            []Announcer{announcer},
		TaskMgr:               mgr,
		Logger: zap.NewNop().Sugar(),
	}
	// Mark all agents as async.
	for id := range agents {
		tool.AsyncAgents[id] = true
	}

	parentSessionKey := "parent:stress:session"
	ctx := context.WithValue(context.Background(), tools.SessionKeyContextKey{}, parentSessionKey)

	var wg sync.WaitGroup
	for i := 0; i < delegateCount; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			agentID := fmt.Sprintf("sub-%d", n)
			sessionKey := fmt.Sprintf("subagent:%s:stress-%d", agentID, n)
			result := tool.runAsync(ctx, agents[agentID], agentID, sessionKey, fmt.Sprintf("task-%d", n), "")
			if result == "" {
				t.Errorf("empty result from runAsync for sub-%d", n)
			}
		}(i)
	}
	wg.Wait()

	// Wait for all announcements.
	deadline := time.After(10 * time.Second)
	for {
		entries := announcer.getEntries()
		if len(entries) >= delegateCount {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out: got %d/%d announcements", len(announcer.getEntries()), delegateCount)
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}

	entries := announcer.getEntries()
	if len(entries) != delegateCount {
		t.Errorf("expected %d announcements, got %d", delegateCount, len(entries))
	}

	// Verify all announcements target the parent session.
	for _, e := range entries {
		if e.SessionKey != parentSessionKey {
			t.Errorf("announcement for wrong session: %q", e.SessionKey)
		}
	}
}

// TestStress_AnnounceAsyncResultConcurrent exercises announceAsyncResult being
// called concurrently from many goroutines.
func TestStress_AnnounceAsyncResultConcurrent(t *testing.T) {
	t.Parallel()

	const goroutines = 80
	announcer := &mockAnnouncer{}

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			announceAsyncResult(announceParams{
				agentID:          fmt.Sprintf("agent-%d", n),
				parentSessionKey: "parent:concurrent",
				result:           fmt.Sprintf("result-%d", n),
				err:              nil,
				mainAgentID:      "", // no main agent → raw announce
				agents:           map[string]Chatter{},
				announcers:       []Announcer{announcer},
				logger:           zap.NewNop().Sugar(),
				notifPrefix:      "Test",
				maxRetries:       0,
				baseBackoffMs:    0,
			})
		}(i)
	}

	wg.Wait()

	entries := announcer.getEntries()
	if len(entries) != goroutines {
		t.Errorf("expected %d announcements, got %d", goroutines, len(entries))
	}
}

// TestStress_AnnounceWithMainAgentConcurrent exercises announceAsyncResult
// routing through a main agent concurrently.
func TestStress_AnnounceWithMainAgentConcurrent(t *testing.T) {
	t.Parallel()

	const goroutines = 50
	mainAgent := &mockChatter{response: "summarized"}
	announcer := &mockAnnouncer{}

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			announceAsyncResult(announceParams{
				agentID:          fmt.Sprintf("agent-%d", n),
				parentSessionKey: fmt.Sprintf("parent:%d", n%5),
				result:           fmt.Sprintf("result-%d", n),
				err:              nil,
				mainAgentID:      "main",
				agents:           map[string]Chatter{"main": mainAgent},
				announcers:       []Announcer{announcer},
				logger:           zap.NewNop().Sugar(),
				notifPrefix:      "Stress",
				maxRetries:       1,
				baseBackoffMs:    0,
			})
		}(i)
	}

	wg.Wait()

	entries := announcer.getEntries()
	if len(entries) != goroutines {
		t.Errorf("expected %d announcements, got %d", goroutines, len(entries))
	}

	// Verify main agent was called for each.
	calls := mainAgent.getCalls()
	if len(calls) != goroutines {
		t.Errorf("expected %d main agent calls, got %d", goroutines, len(calls))
	}
}

// TestStress_DelegateToolConcurrentRuns exercises the full DelegateTool.Run
// path with many concurrent synchronous delegate calls.
func TestStress_DelegateToolConcurrentRuns(t *testing.T) {
	t.Parallel()

	const goroutines = 50
	sub := &mockChatter{response: "ok"}

	tool := &DelegateTool{
		Agents:   map[string]Chatter{"sub": sub},
		MaxDepth: 5,
		Logger:   zap.NewNop().Sugar(),
	}

	var wg sync.WaitGroup
	var errors atomic.Int64

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ctx := context.WithValue(context.Background(), agentapi.SessionKeyContextKey{}, fmt.Sprintf("session:%d", n))
			args := fmt.Sprintf(`{"agent_id":"sub","message":"msg-%d"}`, n)
			result := tool.Run(ctx, args)
			if result != "ok" {
				errors.Add(1)
			}
		}(i)
	}

	wg.Wait()

	if errors.Load() > 0 {
		t.Errorf("%d delegate runs returned unexpected results", errors.Load())
	}

	calls := sub.getCalls()
	if len(calls) != goroutines {
		t.Errorf("expected %d sub-agent calls, got %d", goroutines, len(calls))
	}
}

// TestStress_MixedAsyncAndSyncDelegates exercises async and sync delegate calls
// happening concurrently.
func TestStress_MixedAsyncAndSyncDelegates(t *testing.T) {
	t.Parallel()

	const goroutines = 60
	syncSub := &mockChatter{response: "sync-ok"}
	asyncSub := &mockChatter{response: "async-ok"}
	announcer := &mockAnnouncer{}
	mgr := taskqueue.New(zap.NewNop().Sugar(), filepath.Join(t.TempDir(), "tasks.json"), taskqueue.Config{
		MaxConcurrent: 10,
	})
	defer mgr.Shutdown()

	tool := &DelegateTool{
		Agents: map[string]Chatter{
			"sync-sub":  syncSub,
			"async-sub": asyncSub,
		},
		AsyncAgents:           map[string]bool{"async-sub": true},
		MaxDepth:              5,
		Announcers:            []Announcer{announcer},
		TaskMgr:               mgr,
		Logger: zap.NewNop().Sugar(),
	}

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ctx := context.WithValue(context.Background(), tools.SessionKeyContextKey{}, fmt.Sprintf("session:%d", n))
			var args string
			if n%2 == 0 {
				args = fmt.Sprintf(`{"agent_id":"sync-sub","message":"sync-%d"}`, n)
			} else {
				args = fmt.Sprintf(`{"agent_id":"async-sub","message":"async-%d"}`, n)
			}
			tool.Run(ctx, args)
		}(i)
	}

	wg.Wait()

	// Wait for async announcements.
	asyncCount := goroutines / 2
	deadline := time.After(10 * time.Second)
	for {
		entries := announcer.getEntries()
		if len(entries) >= asyncCount {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out: got %d/%d async announcements", len(announcer.getEntries()), asyncCount)
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
}
