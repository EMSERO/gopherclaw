package reasoning

import (
	"testing"
	"time"

	"go.uber.org/zap"
)

func testLogger() *zap.SugaredLogger { return zap.NewNop().Sugar() }

func TestTriggerCoalesces(t *testing.T) {
	l := New(time.Minute, nil, nil, nil, testLogger())

	// Fire multiple triggers rapidly — they should coalesce.
	for i := 0; i < 10; i++ {
		l.Trigger()
	}

	// Channel should have exactly 1 message (buffered 1).
	select {
	case <-l.triggerCh:
	default:
		t.Error("expected a trigger in the channel")
	}

	// Channel should now be empty.
	select {
	case <-l.triggerCh:
		t.Error("expected channel to be empty after drain")
	default:
	}
}

func TestTriggerNonBlocking(t *testing.T) {
	l := New(time.Minute, nil, nil, nil, testLogger())

	// Fill the channel.
	l.Trigger()

	// Second trigger should not block.
	done := make(chan struct{})
	go func() {
		l.Trigger()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("Trigger() blocked when channel was full")
	}
}

func TestAddDeliverer(t *testing.T) {
	l := New(time.Minute, nil, nil, nil, testLogger())
	if len(l.deliverers) != 0 {
		t.Fatalf("expected 0 deliverers, got %d", len(l.deliverers))
	}

	l.AddDeliverer(&mockDeliverer{})
	if len(l.deliverers) != 1 {
		t.Errorf("expected 1 deliverer, got %d", len(l.deliverers))
	}
}

func TestFormatSurfaceNotification(t *testing.T) {
	cases := []struct {
		surfaceType string
		content     string
		wantIcon    string
	}{
		{"insight", "test insight", "💡"},
		{"warning", "something broke", "⚠️"},
		{"question", "what do you think", "❓"},
		{"reminder", "don't forget", "⏰"},
		{"connection", "A relates to B", "🔗"},
	}

	for _, tc := range cases {
		t.Run(tc.surfaceType, func(t *testing.T) {
			result := formatSurfaceNotification(tc.surfaceType, tc.content)
			if result != tc.wantIcon+" ["+tc.surfaceType+"] "+tc.content {
				t.Errorf("got %q", result)
			}
		})
	}
}

// mockDeliverer records SendToAllPaired calls.
type mockDeliverer struct {
	sent []string
}

func (m *mockDeliverer) SendToAllPaired(text string) {
	m.sent = append(m.sent, text)
}
