package session

import (
	"encoding/json"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

// FuzzJSONLParsing fuzzes JSONL message deserialization with arbitrary bytes.
// The loadJSONL method is unexported and requires filesystem access, so we
// fuzz the underlying json.Unmarshal of Message structs directly — this is
// the same code path that loadJSONL exercises per line.
func FuzzJSONLParsing(f *testing.F) {
	f.Add([]byte(`{"role":"user","content":"hello","ts":1}`))
	f.Add([]byte(`{"role":"assistant","content":"hi","tool_calls":[{"id":"tc1","type":"function","function":{"name":"exec","arguments":"{}"}}],"ts":2}`))
	f.Add([]byte(`{"role":"tool","content":"result","tool_call_id":"tc1","ts":3}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(``))
	f.Add([]byte(`{"role":"user","content":"` + string(make([]byte, 4096)) + `"}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var msg Message
		// Unmarshal may return an error — that's fine. We just must not panic.
		_ = json.Unmarshal(data, &msg)
	})
}

// FuzzSanitizeOrphans fuzzes SanitizeOrphans with randomly generated Message slices.
func FuzzSanitizeOrphans(f *testing.F) {
	seed1, _ := json.Marshal([]Message{
		{Role: "user", Content: "hi", TS: 1},
		{Role: "assistant", Content: "hello", TS: 2},
	})
	seed2, _ := json.Marshal([]Message{
		{Role: "assistant", ToolCalls: []openai.ToolCall{{ID: "tc1", Type: "function", Function: openai.FunctionCall{Name: "exec", Arguments: "{}"}}}, TS: 1},
		{Role: "tool", ToolCallID: "tc1", Content: "ok", TS: 2},
	})
	seed3, _ := json.Marshal([]Message{
		{Role: "tool", ToolCallID: "orphan", Content: "stale", TS: 1},
		{Role: "assistant", Content: "reply", TS: 2},
	})
	seed4, _ := json.Marshal([]Message{})

	f.Add(seed1)
	f.Add(seed2)
	f.Add(seed3)
	f.Add(seed4)

	f.Fuzz(func(t *testing.T, data []byte) {
		var msgs []Message
		if err := json.Unmarshal(data, &msgs); err != nil {
			return
		}
		cleaned, n := SanitizeOrphans(msgs)
		if n < 0 {
			t.Fatalf("SanitizeOrphans returned negative dropped count: %d", n)
		}
		if len(cleaned)+n != len(msgs) {
			t.Fatalf("SanitizeOrphans: len(cleaned)=%d + dropped=%d != len(input)=%d", len(cleaned), n, len(msgs))
		}
	})
}

// FuzzToOpenAI fuzzes ToOpenAI with randomly generated Message slices.
func FuzzToOpenAI(f *testing.F) {
	seed1, _ := json.Marshal([]Message{
		{Role: "user", Content: "hi", ImageURLs: []string{"data:image/png;base64,abc"}, TS: 1},
		{Role: "assistant", Content: "hello", TS: 2},
	})
	seed2, _ := json.Marshal([]Message{
		{Role: "assistant", ToolCalls: []openai.ToolCall{{ID: "tc1", Type: "function", Function: openai.FunctionCall{Name: "exec", Arguments: "{}"}}}, TS: 1},
		{Role: "tool", ToolCallID: "tc1", Content: "result", TS: 2},
	})
	seed3, _ := json.Marshal([]Message{})

	f.Add(seed1)
	f.Add(seed2)
	f.Add(seed3)

	f.Fuzz(func(t *testing.T, data []byte) {
		var msgs []Message
		if err := json.Unmarshal(data, &msgs); err != nil {
			return
		}
		_ = ToOpenAI(msgs)
	})
}

// FuzzEstimateTokens fuzzes EstimateTokens with arbitrary Message slices.
func FuzzEstimateTokens(f *testing.F) {
	seed1, _ := json.Marshal([]Message{
		{Role: "user", Content: "hello world", TS: 1},
	})
	seed2, _ := json.Marshal([]Message{
		{Role: "assistant", Content: "", ToolCalls: []openai.ToolCall{
			{ID: "tc1", Type: "function", Function: openai.FunctionCall{Name: "long_tool_name", Arguments: `{"key":"value"}`}},
		}, TS: 1},
	})
	seed3, _ := json.Marshal([]Message{})

	f.Add(seed1)
	f.Add(seed2)
	f.Add(seed3)

	f.Fuzz(func(t *testing.T, data []byte) {
		var msgs []Message
		if err := json.Unmarshal(data, &msgs); err != nil {
			return
		}
		n := EstimateTokens(msgs)
		if n < 0 {
			t.Fatalf("EstimateTokens returned negative: %d", n)
		}
	})
}

// FuzzPruneToolResults fuzzes PruneToolResults with random Message slices
// and various keepRecent values.
func FuzzPruneToolResults(f *testing.F) {
	seed1, _ := json.Marshal([]Message{
		{Role: "user", Content: "q1", TS: 1},
		{Role: "assistant", ToolCalls: []openai.ToolCall{{ID: "tc1", Type: "function", Function: openai.FunctionCall{Name: "exec", Arguments: "{}"}}}, TS: 2},
		{Role: "tool", ToolCallID: "tc1", Content: string(make([]byte, 500)), TS: 3},
		{Role: "assistant", Content: "answer1", TS: 4},
		{Role: "user", Content: "q2", TS: 5},
		{Role: "assistant", Content: "answer2", TS: 6},
	})
	seed2, _ := json.Marshal([]Message{})
	seed3, _ := json.Marshal([]Message{
		{Role: "assistant", Content: "only one", TS: 1},
	})

	f.Add(seed1, 2)
	f.Add(seed2, 0)
	f.Add(seed3, 1)
	f.Add(seed1, -1)
	f.Add(seed1, 100)

	f.Fuzz(func(t *testing.T, data []byte, keepRecent int) {
		var msgs []Message
		if err := json.Unmarshal(data, &msgs); err != nil {
			return
		}
		_ = PruneToolResults(msgs, keepRecent)
	})
}

// FuzzPruneKeepLast fuzzes pruneKeepLast with random Message slices
// and various keepAssistants values.
func FuzzPruneKeepLast(f *testing.F) {
	seed1, _ := json.Marshal([]Message{
		{Role: "user", Content: "q1", TS: 1},
		{Role: "assistant", Content: "a1", TS: 2},
		{Role: "user", Content: "q2", TS: 3},
		{Role: "assistant", ToolCalls: []openai.ToolCall{{ID: "tc1", Type: "function", Function: openai.FunctionCall{Name: "exec", Arguments: "{}"}}}, TS: 4},
		{Role: "tool", ToolCallID: "tc1", Content: "result", TS: 5},
		{Role: "assistant", Content: "a2", TS: 6},
		{Role: "user", Content: "q3", TS: 7},
		{Role: "assistant", Content: "a3", TS: 8},
	})
	seed2, _ := json.Marshal([]Message{})
	seed3, _ := json.Marshal([]Message{
		{Role: "user", Content: "only", TS: 1},
	})

	f.Add(seed1, 2)
	f.Add(seed2, 0)
	f.Add(seed3, 1)
	f.Add(seed1, -1)
	f.Add(seed1, 100)

	f.Fuzz(func(t *testing.T, data []byte, keepAssistants int) {
		var msgs []Message
		if err := json.Unmarshal(data, &msgs); err != nil {
			return
		}
		_ = pruneKeepLast(msgs, keepAssistants)
	})
}
