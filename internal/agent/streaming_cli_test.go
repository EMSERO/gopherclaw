package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestStreamingCLIAgent_ParseStreamEvents verifies the NDJSON event parsing.
func TestStreamingCLIAgent_ParseStreamEvents(t *testing.T) {
	logger := zap.NewNop().Sugar()
	s := &StreamingCLIAgent{logger: logger}

	t.Run("text_delta", func(t *testing.T) {
		var text strings.Builder
		var toolName string
		var chunks []string
		cb := &StreamCallbacks{
			OnChunk: func(t string) { chunks = append(chunks, t) },
		}

		ev := streamEvent{Type: "content_block_delta"}
		delta, _ := json.Marshal(textDelta{Type: "text_delta", Text: "Hello"})
		ev.Delta = delta
		raw, _ := json.Marshal(ev)
		s.handleStreamEvent(raw, &text, &toolName, cb)

		if text.String() != "Hello" {
			t.Errorf("expected text 'Hello', got %q", text.String())
		}
		if len(chunks) != 1 || chunks[0] != "Hello" {
			t.Errorf("expected 1 chunk 'Hello', got %v", chunks)
		}
	})

	t.Run("tool_use_start_stop", func(t *testing.T) {
		var text strings.Builder
		var toolName string
		var startedTools []string
		var stoppedTools []string
		cb := &StreamCallbacks{
			OnToolStart: func(name, _ string) { startedTools = append(startedTools, name) },
			OnToolDone:  func(name, _ string, _ error) { stoppedTools = append(stoppedTools, name) },
		}

		block, _ := json.Marshal(contentBlock{Type: "tool_use", Name: "browser_navigate"})
		ev := streamEvent{Type: "content_block_start", ContentBlock: block}
		raw, _ := json.Marshal(ev)
		s.handleStreamEvent(raw, &text, &toolName, cb)

		if len(startedTools) != 1 || startedTools[0] != "browser_navigate" {
			t.Errorf("expected tool start 'browser_navigate', got %v", startedTools)
		}

		ev2 := streamEvent{Type: "content_block_stop"}
		raw2, _ := json.Marshal(ev2)
		s.handleStreamEvent(raw2, &text, &toolName, cb)

		if len(stoppedTools) != 1 || stoppedTools[0] != "browser_navigate" {
			t.Errorf("expected tool stop 'browser_navigate', got %v", stoppedTools)
		}
	})

	t.Run("nil_callback", func(t *testing.T) {
		var text strings.Builder
		var toolName string
		delta, _ := json.Marshal(textDelta{Type: "text_delta", Text: "no panic"})
		ev := streamEvent{Type: "content_block_delta", Delta: delta}
		raw, _ := json.Marshal(ev)
		s.handleStreamEvent(raw, &text, &toolName, nil)
		if text.String() != "no panic" {
			t.Errorf("expected 'no panic', got %q", text.String())
		}
	})
}

// TestStreamingCLIAgent_ExtractAssistantText tests assistant message text extraction.
func TestStreamingCLIAgent_ExtractAssistantText(t *testing.T) {
	logger := zap.NewNop().Sugar()
	s := &StreamingCLIAgent{logger: logger}

	msg := `{"content":[{"type":"text","text":"Hello world"},{"type":"tool_use","id":"t1","name":"foo"},{"type":"text","text":"More text"}]}`
	result := s.extractAssistantText(json.RawMessage(msg))
	if result != "Hello world\nMore text" {
		t.Errorf("expected 'Hello world\\nMore text', got %q", result)
	}

	if s.extractAssistantText(nil) != "" {
		t.Error("expected empty string for nil input")
	}
}

// TestStreamingCLIAgent_ReadResponse tests the full readResponse flow with mock events.
func TestStreamingCLIAgent_ReadResponse(t *testing.T) {
	logger := zap.NewNop().Sugar()

	events := strings.Join([]string{
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello world"}]}}`,
		`{"type":"result","subtype":"success","result":"Hello world","is_error":false,"usage":{"input_tokens":10,"output_tokens":5},"duration_ms":100}`,
	}, "\n") + "\n"

	sess := &cliSession{
		stdout: bufio.NewScanner(strings.NewReader(events)),
	}

	s := &StreamingCLIAgent{
		logger: logger,
		usage:  NewUsageTracker(),
	}

	var chunks []string
	cb := &StreamCallbacks{
		OnChunk: func(text string) { chunks = append(chunks, text) },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := s.readResponse(ctx, "test-session", sess, cb)
	if err != nil {
		t.Fatalf("readResponse: %v", err)
	}

	if result.Text != "Hello world" {
		t.Errorf("expected 'Hello world', got %q", result.Text)
	}
	if len(chunks) != 2 {
		t.Errorf("expected 2 chunks, got %d: %v", len(chunks), chunks)
	} else {
		if chunks[0] != "Hello " || chunks[1] != "world" {
			t.Errorf("unexpected chunks: %v", chunks)
		}
	}

	// Verify usage was tracked.
	u, calls := s.usage.GetSession("test-session")
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
	if u.Input != 10 || u.Output != 5 {
		t.Errorf("expected input=10 output=5, got input=%d output=%d", u.Input, u.Output)
	}
}

// TestStreamingCLIAgent_ReadResponse_Error tests error handling in readResponse.
func TestStreamingCLIAgent_ReadResponse_Error(t *testing.T) {
	logger := zap.NewNop().Sugar()

	events := strings.Join([]string{
		`{"type":"result","subtype":"error_max_turns","is_error":true,"errors":["max turns exceeded"]}`,
	}, "\n") + "\n"

	sess := &cliSession{
		stdout: bufio.NewScanner(strings.NewReader(events)),
	}

	s := &StreamingCLIAgent{
		logger: logger,
		usage:  NewUsageTracker(),
	}

	ctx := context.Background()
	_, err := s.readResponse(ctx, "err-session", sess, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "max turns exceeded") {
		t.Errorf("expected 'max turns exceeded' in error, got: %v", err)
	}
}

// TestStreamingCLIAgent_ReadResponse_AssistantError tests assistant-level error handling.
func TestStreamingCLIAgent_ReadResponse_AssistantError(t *testing.T) {
	logger := zap.NewNop().Sugar()

	events := strings.Join([]string{
		`{"type":"assistant","error":"authentication_failed","message":{"content":[]}}`,
	}, "\n") + "\n"

	sess := &cliSession{
		stdout: bufio.NewScanner(strings.NewReader(events)),
	}

	s := &StreamingCLIAgent{
		logger: logger,
		usage:  NewUsageTracker(),
	}

	ctx := context.Background()
	_, err := s.readResponse(ctx, "auth-err", sess, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "authentication_failed") {
		t.Errorf("expected 'authentication_failed' in error, got: %v", err)
	}
}

// TestStreamingCLIAgent_ResolveModel tests model resolution logic.
func TestStreamingCLIAgent_ResolveModel(t *testing.T) {
	s := &StreamingCLIAgent{
		args:          []string{"-p", "--model", "sonnet", "--verbose"},
		sessionModels: make(map[string]string),
	}

	if m := s.ResolveModel(""); m != "sonnet" {
		t.Errorf("expected 'sonnet', got %q", m)
	}

	s.SetSessionModel("s1", "opus")
	if m := s.ResolveModel("s1"); m != "opus" {
		t.Errorf("expected 'opus', got %q", m)
	}

	s.ClearSessionModel("s1")
	if m := s.ResolveModel("s1"); m != "sonnet" {
		t.Errorf("expected 'sonnet' after clear, got %q", m)
	}
}

// TestStreamingCLIAgent_ModelHealth tests the ModelHealth stub.
func TestStreamingCLIAgent_ModelHealth(t *testing.T) {
	s := &StreamingCLIAgent{
		args:          []string{"--model", "opus"},
		sessionModels: make(map[string]string),
	}

	health := s.ModelHealth()
	if len(health) != 1 {
		t.Fatalf("expected 1 health entry, got %d", len(health))
	}
	if health[0].Provider != "claude-cli" {
		t.Errorf("expected provider 'claude-cli', got %q", health[0].Provider)
	}
	if !health[0].Available {
		t.Error("expected available=true")
	}
}

// TestStreamingCLIAgent_Compact tests that Compact reaps the session.
func TestStreamingCLIAgent_Compact(t *testing.T) {
	s := &StreamingCLIAgent{
		logger:        zap.NewNop().Sugar(),
		sessions:      make(map[string]*cliSession),
		usage:         NewUsageTracker(),
		sessionModels: make(map[string]string),
	}

	// Add a fake session entry.
	s.mu.Lock()
	s.sessions["test"] = &cliSession{
		cancel: func() {},
		stdin:  nopWriteCloser{},
		cmd:    nil, // skip kill
	}
	s.mu.Unlock()

	err := s.Compact(context.Background(), "test", "")
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}

	s.mu.Lock()
	_, exists := s.sessions["test"]
	s.mu.Unlock()
	if exists {
		t.Error("expected session to be reaped after Compact")
	}
}

// nopWriteCloser is a no-op WriteCloser for testing.
type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                 { return nil }
