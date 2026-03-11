package agent

import (
	"context"
	"fmt"
	"testing"

	openai "github.com/sashabaranov/go-openai"

	"github.com/EMSERO/gopherclaw/internal/session"
)

// mockCompactorClient is a test double for the LLM client.
type mockCompactorClient struct {
	response string
	err      error
	calls    int
}

func (m *mockCompactorClient) Chat(_ context.Context, _ openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	m.calls++
	if m.err != nil {
		return openai.ChatCompletionResponse{}, m.err
	}
	return openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{
			{Message: openai.ChatCompletionMessage{Content: m.response}},
		},
	}, nil
}

func TestCompactHistory_Basic(t *testing.T) {
	client := &mockCompactorClient{response: "Summary of conversation"}
	msgs := make([]session.Message, 0)
	// Build a history with 10 exchanges
	for i := range 10 {
		msgs = append(msgs, session.Message{Role: "user", Content: fmt.Sprintf("Question %d with some longer text to ensure tokens", i)})
		msgs = append(msgs, session.Message{Role: "assistant", Content: fmt.Sprintf("Answer %d with detailed explanation text content", i)})
	}

	result, err := CompactHistory(context.Background(), testLogger(), client, "test-model", msgs, 128000, 2)
	if err != nil {
		t.Fatalf("CompactHistory error: %v", err)
	}

	// Should have summary + recent messages
	if len(result) < 3 {
		t.Errorf("expected at least 3 messages (summary + 2 recent), got %d", len(result))
	}
	if result[0].Content == "" || result[0].Role != "assistant" {
		t.Error("first message should be a summary")
	}
	if client.calls == 0 {
		t.Error("expected at least one LLM call")
	}
}

func TestCompactHistory_TooFewMessages(t *testing.T) {
	client := &mockCompactorClient{response: "Summary"}
	msgs := []session.Message{
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi"},
	}

	result, err := CompactHistory(context.Background(), testLogger(), client, "test-model", msgs, 128000, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// With only 2 messages and keepN=2, nothing should be compacted
	if len(result) != 2 {
		t.Errorf("expected original 2 messages, got %d", len(result))
	}
	if client.calls != 0 {
		t.Error("should not have called the LLM")
	}
}

func TestCompactHistory_SummarizationFailure(t *testing.T) {
	client := &mockCompactorClient{err: fmt.Errorf("model error")}
	msgs := make([]session.Message, 0)
	for i := range 10 {
		msgs = append(msgs, session.Message{Role: "user", Content: fmt.Sprintf("Q%d", i)})
		msgs = append(msgs, session.Message{Role: "assistant", Content: fmt.Sprintf("A%d", i)})
	}

	// Should still succeed with fallback note
	result, err := CompactHistory(context.Background(), testLogger(), client, "test-model", msgs, 128000, 2)
	if err != nil {
		// Even if all chunks fail, we get fallback notes
		t.Logf("got expected error path: %v", err)
	}
	// The result should at least not panic
	_ = result
}

func TestStripToolResultDetails(t *testing.T) {
	msgs := []session.Message{
		{Role: "user", Content: "Run a command"},
		{Role: "assistant", Content: "I'll run ls"},
		{Role: "tool", Name: "exec", Content: "total 42\n-rw-r--r-- 1 user user 1234 file.txt\n...lots of output..."},
		{Role: "assistant", Content: "The directory has one file"},
	}

	stripped := stripToolResultDetails(msgs)
	if stripped[2].Content != "[tool result: exec]" {
		t.Errorf("tool result not stripped: %q", stripped[2].Content)
	}
	// Non-tool messages should be unchanged
	if stripped[0].Content != "Run a command" {
		t.Error("user message was modified")
	}
	if stripped[3].Content != "The directory has one file" {
		t.Error("assistant message was modified")
	}
}

func TestFindCompactionSplitPoint(t *testing.T) {
	msgs := []session.Message{
		{Role: "user", Content: "Q1"},
		{Role: "assistant", Content: "A1"},
		{Role: "user", Content: "Q2"},
		{Role: "assistant", Content: "A2"},
		{Role: "user", Content: "Q3"},
		{Role: "assistant", Content: "A3"},
	}

	// keepN=2 should split before the last 2 assistant messages
	split := findCompactionSplitPoint(msgs, 2)
	if split != 2 { // should keep A2, A3 → split at index 2 (Q2)
		t.Errorf("split = %d, want 2", split)
	}

	// keepN=1
	split = findCompactionSplitPoint(msgs, 1)
	if split != 4 { // should keep A3 → split at index 4 (Q3)
		t.Errorf("split = %d, want 4", split)
	}

	// keepN=3 (all assistants) → nothing to compact
	split = findCompactionSplitPoint(msgs, 3)
	if split != 0 {
		t.Errorf("split = %d, want 0 (nothing to compact)", split)
	}
}

func TestAdaptiveChunkRatio(t *testing.T) {
	// Small messages → higher ratio
	small := make([]session.Message, 10)
	for i := range small {
		small[i] = session.Message{Role: "user", Content: "short"}
	}
	r := adaptiveChunkRatio(small, 128000)
	if r < MinChunkRatio || r > BaseChunkRatio {
		t.Errorf("ratio %f out of bounds [%f, %f]", r, MinChunkRatio, BaseChunkRatio)
	}

	// Empty messages
	r = adaptiveChunkRatio(nil, 128000)
	if r != BaseChunkRatio {
		t.Errorf("empty: ratio=%f, want %f", r, BaseChunkRatio)
	}
}

func TestChunkByTokenBudget(t *testing.T) {
	msgs := make([]session.Message, 20)
	for i := range msgs {
		msgs[i] = session.Message{Role: "user", Content: fmt.Sprintf("Message %d with some content", i)}
	}

	// Small budget → many chunks
	chunks := chunkByTokenBudget(msgs, 50, 128000)
	if len(chunks) < 2 {
		t.Errorf("expected multiple chunks with small budget, got %d", len(chunks))
	}

	// Large budget → fewer chunks
	chunks = chunkByTokenBudget(msgs, 100000, 128000)
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk with large budget, got %d", len(chunks))
	}
}

func TestValidateContextWindow(t *testing.T) {
	// Below minimum → error
	err := ValidateContextWindow(testLogger(), 8000)
	if err == nil {
		t.Error("expected error for 8K context window")
	}
	if _, ok := err.(*ContextWindowError); !ok {
		t.Errorf("expected ContextWindowError, got %T", err)
	}

	// At minimum → no error
	err = ValidateContextWindow(testLogger(), 16000)
	if err != nil {
		t.Errorf("unexpected error at exact minimum: %v", err)
	}

	// Zero (unknown) → no error
	err = ValidateContextWindow(testLogger(), 0)
	if err != nil {
		t.Errorf("unexpected error for unknown context: %v", err)
	}

	// Well above → no error
	err = ValidateContextWindow(testLogger(), 128000)
	if err != nil {
		t.Errorf("unexpected error for 128K: %v", err)
	}
}

func TestResolveContextWindow(t *testing.T) {
	tests := []struct {
		perModel, agentDef, want int
	}{
		{200000, 128000, 200000}, // per-model takes precedence
		{0, 64000, 64000},        // agent default fallback
		{0, 0, 128_000},          // global fallback
	}
	for _, tt := range tests {
		got := ResolveContextWindow(tt.perModel, tt.agentDef)
		if got != tt.want {
			t.Errorf("ResolveContextWindow(%d, %d) = %d, want %d", tt.perModel, tt.agentDef, got, tt.want)
		}
	}
}
