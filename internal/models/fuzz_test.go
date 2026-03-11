package models

import (
	"encoding/json"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

func FuzzConvertToAnthropicMessages(f *testing.F) {
	// Helper to serialize a message slice to JSON for the seed corpus.
	addSeed := func(msgs []openai.ChatCompletionMessage) {
		data, err := json.Marshal(msgs)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}

	// Single system message.
	addSeed([]openai.ChatCompletionMessage{
		{Role: "system", Content: "You are a helpful assistant."},
	})

	// System + user + assistant.
	addSeed([]openai.ChatCompletionMessage{
		{Role: "system", Content: "Be helpful."},
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi there!"},
	})

	// User message only.
	addSeed([]openai.ChatCompletionMessage{
		{Role: "user", Content: "What is 2+2?"},
	})

	// Assistant with tool calls.
	addSeed([]openai.ChatCompletionMessage{
		{Role: "user", Content: "Search for cats"},
		{Role: "assistant", Content: "Let me search.", ToolCalls: []openai.ToolCall{
			{
				ID:   "call_123",
				Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{
					Name:      "web_search",
					Arguments: `{"query":"cats"}`,
				},
			},
		}},
		{Role: "tool", ToolCallID: "call_123", Content: "Found 10 results about cats."},
		{Role: "assistant", Content: "I found some results."},
	})

	// Multiple tool results batched.
	addSeed([]openai.ChatCompletionMessage{
		{Role: "user", Content: "Do two things"},
		{Role: "assistant", ToolCalls: []openai.ToolCall{
			{ID: "c1", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "tool_a", Arguments: "{}"}},
			{ID: "c2", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "tool_b", Arguments: "{}"}},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "result a"},
		{Role: "tool", ToolCallID: "c2", Content: "result b"},
		{Role: "assistant", Content: "Done."},
	})

	// Empty content fields.
	addSeed([]openai.ChatCompletionMessage{
		{Role: "user", Content: ""},
		{Role: "assistant", Content: ""},
	})

	// Empty slice.
	addSeed([]openai.ChatCompletionMessage{})

	// Tool call with empty arguments.
	addSeed([]openai.ChatCompletionMessage{
		{Role: "assistant", ToolCalls: []openai.ToolCall{
			{ID: "c1", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "noop", Arguments: ""}},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "ok"},
	})

	// Assistant with content AND tool calls.
	addSeed([]openai.ChatCompletionMessage{
		{Role: "assistant", Content: "Thinking...", ToolCalls: []openai.ToolCall{
			{ID: "c1", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "think", Arguments: `{"thought":"hmm"}`}},
		}},
		{Role: "tool", ToolCallID: "c1", Content: "thought recorded"},
	})

	// Multiple system messages (last one wins in the loop).
	addSeed([]openai.ChatCompletionMessage{
		{Role: "system", Content: "First system"},
		{Role: "system", Content: "Second system"},
		{Role: "user", Content: "Hi"},
	})

	f.Fuzz(func(t *testing.T, data []byte) {
		var msgs []openai.ChatCompletionMessage
		if err := json.Unmarshal(data, &msgs); err != nil {
			// Invalid JSON — skip.
			return
		}

		system, result, err := convertToAnthropicMessages(msgs)
		if err != nil {
			// Conversion errors (e.g. json.Marshal failures) are acceptable.
			return
		}

		// Invariants:
		// 1. No result message should have an empty role.
		for i, m := range result {
			if m.Role == "" {
				t.Fatalf("result[%d] has empty role", i)
			}
			// 2. Role must be "user" or "assistant" (Anthropic format).
			if m.Role != "user" && m.Role != "assistant" {
				t.Fatalf("result[%d] has unexpected role %q", i, m.Role)
			}
			// 3. Content must be valid JSON (it's always marshaled as a block array).
			if !json.Valid(m.Content) {
				t.Fatalf("result[%d] has invalid JSON content", i)
			}
		}

		// 4. System should only be set if there was a system message in input.
		hasSystem := false
		for _, m := range msgs {
			if m.Role == "system" && m.Content != "" {
				hasSystem = true
			}
		}
		if !hasSystem && system != "" {
			t.Fatalf("system = %q but no system message in input", system)
		}
	})
}
