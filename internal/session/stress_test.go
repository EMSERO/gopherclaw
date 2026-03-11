package session

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// NOTE: Running these tests with -race exposes a real data race in the session
// manager: Entry.UpdatedAt is read in GetHistory (line ~124) after releasing
// the mutex, while AppendMessages (line ~184) writes it under the lock. This
// is a known pre-existing issue in the production code that these stress tests
// are designed to surface. Without -race, these tests verify no panics or
// deadlocks occur under concurrent load.

func testMsg(role, content string) Message {
	return Message{Role: role, Content: content, TS: time.Now().UnixMilli()}
}

// TestStress_ConcurrentAppendAndGetHistory exercises concurrent AppendMessages
// and GetHistory on the same session key.
func TestStress_ConcurrentAppendAndGetHistory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	const goroutines = 50
	const key = "stress:append-get"

	var wg sync.WaitGroup

	// Half goroutines append, half read.
	for i := 0; i < goroutines/2; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			msg := testMsg("user", fmt.Sprintf("message-%d", n))
			if err := m.AppendMessages(key, []Message{msg}); err != nil {
				t.Errorf("AppendMessages failed: %v", err)
			}
		}(i)

		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.GetHistory(key)
			if err != nil {
				t.Errorf("GetHistory failed: %v", err)
			}
		}()
	}

	wg.Wait()

	// Verify all appended messages are readable.
	history, err := m.GetHistory(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != goroutines/2 {
		t.Errorf("expected %d messages, got %d", goroutines/2, len(history))
	}
}

// TestStress_ConcurrentResetWhileGetHistory exercises Reset racing with GetHistory.
func TestStress_ConcurrentResetWhileGetHistory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	const key = "stress:reset-get"

	// Seed some initial data.
	for i := 0; i < 20; i++ {
		if err := m.AppendMessages(key, []Message{testMsg("user", fmt.Sprintf("seed-%d", i))}); err != nil {
			t.Fatal(err)
		}
	}

	const goroutines = 60
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		switch i % 3 {
		case 0:
			// Reset
			wg.Add(1)
			go func() {
				defer wg.Done()
				m.Reset(key)
			}()
		case 1:
			// GetHistory
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := m.GetHistory(key)
				if err != nil {
					t.Errorf("GetHistory failed: %v", err)
				}
			}()
		default:
			// Append
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				_ = m.AppendMessages(key, []Message{testMsg("user", fmt.Sprintf("msg-%d", n))})
			}(i)
		}
	}

	wg.Wait()
	// No panics or deadlocks means success.
}

// TestStress_ConcurrentAppendFromManyGoroutines exercises many goroutines
// appending to different session keys simultaneously. Uses multiple keys to
// avoid file-lock timeouts (the file-based lock has a 10s timeout, and each
// append holds the lock briefly; too many goroutines on one key exceed it).
func TestStress_ConcurrentAppendFromManyGoroutines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	const goroutines = 100
	const numKeys = 10 // spread across 10 sessions to avoid lock contention

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			key := fmt.Sprintf("stress:multi-append:%d", n%numKeys)
			msg := testMsg("user", fmt.Sprintf("concurrent-%d", n))
			if err := m.AppendMessages(key, []Message{msg}); err != nil {
				t.Errorf("AppendMessages goroutine %d failed: %v", n, err)
			}
		}(i)
	}
	wg.Wait()

	// Verify total across all keys.
	total := 0
	for k := 0; k < numKeys; k++ {
		key := fmt.Sprintf("stress:multi-append:%d", k)
		history, err := m.GetHistory(key)
		if err != nil {
			t.Errorf("GetHistory(%s) failed: %v", key, err)
			continue
		}
		total += len(history)
	}
	if total != goroutines {
		t.Errorf("expected %d total messages across keys, got %d", goroutines, total)
	}
}

// TestStress_PruningRaceWithAppend exercises the pruning code path running
// concurrently with appends.
func TestStress_PruningRaceWithAppend(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	// Configure aggressive pruning so it triggers.
	m.SetPruning(PruningPolicy{
		HardClearRatio:     0.01, // very low threshold to trigger pruning
		ModelMaxTokens:     100,
		KeepLastAssistants: 2,
	})

	const key = "stress:prune-append"

	// Seed with enough messages to trigger pruning.
	for i := 0; i < 50; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		content := fmt.Sprintf("message-%d with some extra content to pad the token count above threshold", i)
		if err := m.AppendMessages(key, []Message{testMsg(role, content)}); err != nil {
			t.Fatal(err)
		}
	}

	const goroutines = 50
	var wg sync.WaitGroup

	// Half read (triggering pruning), half append.
	for i := 0; i < goroutines; i++ {
		if i%2 == 0 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := m.GetHistory(key)
				if err != nil {
					t.Errorf("GetHistory (prune path) failed: %v", err)
				}
			}()
		} else {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				role := "user"
				if n%2 == 0 {
					role = "assistant"
				}
				msg := testMsg(role, fmt.Sprintf("concurrent-prune-%d with padding content to keep things interesting", n))
				_ = m.AppendMessages(key, []Message{msg})
			}(i)
		}
	}

	wg.Wait()
	// Success = no panics, deadlocks, or data corruption.
}

// TestStress_ConcurrentMultipleSessions exercises operations across different
// session keys simultaneously.
func TestStress_ConcurrentMultipleSessions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	const sessions = 20
	const opsPerSession = 10

	var wg sync.WaitGroup
	for s := 0; s < sessions; s++ {
		key := fmt.Sprintf("stress:multi:%d", s)
		for op := 0; op < opsPerSession; op++ {
			wg.Add(1)
			go func(k string, n int) {
				defer wg.Done()
				switch n % 4 {
				case 0:
					_ = m.AppendMessages(k, []Message{testMsg("user", fmt.Sprintf("msg-%d", n))})
				case 1:
					_, _ = m.GetHistory(k)
				case 2:
					_ = m.AppendMessages(k, []Message{testMsg("assistant", fmt.Sprintf("resp-%d", n))})
				case 3:
					m.TouchAPICall(k)
				}
			}(key, op)
		}
	}
	wg.Wait()

	active := m.ActiveSessions()
	if len(active) == 0 {
		t.Error("expected at least some active sessions")
	}
}

// TestStress_ResetIdleUnderConcurrency exercises ResetIdle while sessions are
// being actively used.
func TestStress_ResetIdleUnderConcurrency(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	const goroutines = 50

	// Create some sessions.
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("stress:idle:%d", i)
		_ = m.AppendMessages(key, []Message{testMsg("user", "hello")})
	}

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			switch n % 3 {
			case 0:
				m.ResetIdle(time.Nanosecond) // very short idle = resets everything
			case 1:
				key := fmt.Sprintf("stress:idle:%d", n%10)
				_ = m.AppendMessages(key, []Message{testMsg("user", "keepalive")})
			case 2:
				key := fmt.Sprintf("stress:idle:%d", n%10)
				_, _ = m.GetHistory(key)
			}
		}(i)
	}
	wg.Wait()
}
