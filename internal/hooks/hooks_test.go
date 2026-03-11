package hooks

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestNew(t *testing.T) {
	b := New(zap.NewNop().Sugar())
	if b == nil {
		t.Fatal("New returned nil")
	}
	if b.HasHandlers(BeforeToolCall) {
		t.Error("fresh bus should have no handlers")
	}
}

func TestOnAndEmit(t *testing.T) {
	b := New(zap.NewNop().Sugar())
	var called atomic.Int32
	b.On(BeforeToolCall, "test", func(_ context.Context, e Event) error {
		called.Add(1)
		if e.ToolName != "exec" {
			t.Errorf("ToolName = %q, want %q", e.ToolName, "exec")
		}
		return nil
	})

	b.Emit(context.Background(), Event{Type: BeforeToolCall, ToolName: "exec"})
	if called.Load() != 1 {
		t.Errorf("handler called %d times, want 1", called.Load())
	}
}

func TestMultipleHandlersSameEvent(t *testing.T) {
	b := New(zap.NewNop().Sugar())
	var order []int
	var mu sync.Mutex
	for i := range 3 {
		i := i
		b.On(SessionCreated, "h"+string(rune('0'+i)), func(_ context.Context, _ Event) error {
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			return nil
		})
	}

	b.Emit(context.Background(), Event{Type: SessionCreated})
	if len(order) != 3 {
		t.Fatalf("got %d calls, want 3", len(order))
	}
	for i, v := range order {
		if v != i {
			t.Errorf("order[%d] = %d, want %d", i, v, i)
		}
	}
}

func TestOff(t *testing.T) {
	b := New(zap.NewNop().Sugar())
	var called atomic.Int32
	b.On(AfterToolCall, "keep", func(_ context.Context, _ Event) error {
		called.Add(1)
		return nil
	})
	b.On(AfterToolCall, "remove", func(_ context.Context, _ Event) error {
		called.Add(10)
		return nil
	})

	b.Off(AfterToolCall, "remove")
	b.Emit(context.Background(), Event{Type: AfterToolCall})
	if called.Load() != 1 {
		t.Errorf("called = %d, want 1 (only keep handler)", called.Load())
	}
}

func TestEmitErrorDoesNotAbort(t *testing.T) {
	b := New(zap.NewNop().Sugar())
	var secondCalled atomic.Bool
	b.On(BeforeMessageSend, "fail", func(_ context.Context, _ Event) error {
		return errors.New("boom")
	})
	b.On(BeforeMessageSend, "ok", func(_ context.Context, _ Event) error {
		secondCalled.Store(true)
		return nil
	})

	b.Emit(context.Background(), Event{Type: BeforeMessageSend})
	if !secondCalled.Load() {
		t.Error("second handler should still run after first errors")
	}
}

func TestEmitAsync(t *testing.T) {
	b := New(zap.NewNop().Sugar())
	var called atomic.Int32
	for range 5 {
		b.On(AfterMessageSend, "async", func(_ context.Context, _ Event) error {
			called.Add(1)
			return nil
		})
	}

	b.EmitAsync(context.Background(), Event{Type: AfterMessageSend})
	if called.Load() != 5 {
		t.Errorf("called = %d, want 5", called.Load())
	}
}

func TestEmitAsyncContextCancel(t *testing.T) {
	b := New(zap.NewNop().Sugar())
	var started atomic.Int32
	b.On(GatewayStopped, "slow", func(ctx context.Context, _ Event) error {
		started.Add(1)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return nil
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	b.EmitAsync(ctx, Event{Type: GatewayStopped})
	if started.Load() != 1 {
		t.Error("handler should still be invoked even if ctx is cancelled")
	}
}

func TestTimestampAutoFilled(t *testing.T) {
	b := New(zap.NewNop().Sugar())
	var got time.Time
	b.On(SessionReset, "ts", func(_ context.Context, e Event) error {
		got = e.Timestamp
		return nil
	})

	before := time.Now()
	b.Emit(context.Background(), Event{Type: SessionReset})
	if got.Before(before) {
		t.Error("auto-filled timestamp should be >= now")
	}
}

func TestTimestampPreserved(t *testing.T) {
	b := New(zap.NewNop().Sugar())
	var got time.Time
	b.On(SessionReset, "ts", func(_ context.Context, e Event) error {
		got = e.Timestamp
		return nil
	})

	explicit := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	b.Emit(context.Background(), Event{Type: SessionReset, Timestamp: explicit})
	if !got.Equal(explicit) {
		t.Errorf("timestamp = %v, want %v", got, explicit)
	}
}

func TestHandlerCount(t *testing.T) {
	b := New(zap.NewNop().Sugar())
	if b.HandlerCount(BeforePromptBuild) != 0 {
		t.Fatal("expected 0")
	}
	b.On(BeforePromptBuild, "a", func(_ context.Context, _ Event) error { return nil })
	b.On(BeforePromptBuild, "b", func(_ context.Context, _ Event) error { return nil })
	if n := b.HandlerCount(BeforePromptBuild); n != 2 {
		t.Errorf("HandlerCount = %d, want 2", n)
	}
}

func TestEmitNoHandlers(t *testing.T) {
	b := New(zap.NewNop().Sugar())
	b.Emit(context.Background(), Event{Type: GatewayStarted})
	b.EmitAsync(context.Background(), Event{Type: GatewayStarted})
}

func TestConcurrentOnAndEmit(t *testing.T) {
	b := New(zap.NewNop().Sugar())
	var wg sync.WaitGroup
	wg.Add(20)
	for range 10 {
		go func() {
			defer wg.Done()
			b.On(BeforeToolCall, "c", func(_ context.Context, _ Event) error { return nil })
		}()
		go func() {
			defer wg.Done()
			b.Emit(context.Background(), Event{Type: BeforeToolCall})
		}()
	}
	wg.Wait()
}

func TestEventData(t *testing.T) {
	b := New(zap.NewNop().Sugar())
	var got map[string]any
	b.On(AfterToolCall, "data", func(_ context.Context, e Event) error {
		got = e.Data
		return nil
	})

	b.Emit(context.Background(), Event{
		Type: AfterToolCall,
		Data: map[string]any{"result": "ok", "duration_ms": 42},
	})

	if got["result"] != "ok" {
		t.Errorf("Data[result] = %v, want ok", got["result"])
	}
}
