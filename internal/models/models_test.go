package models

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/config"
	"github.com/EMSERO/gopherclaw/internal/taskqueue"
)

func testLogger() *zap.SugaredLogger { return zap.NewNop().Sugar() }

func TestModelID(t *testing.T) {
	tests := []struct {
		input, expected string
	}{
		{"github-copilot/claude-sonnet-4.6", "claude-sonnet-4.6"},
		{"github-copilot/gpt-4.1", "gpt-4.1"},
		{"claude-sonnet-4.6", "claude-sonnet-4.6"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := ModelID(tt.input); got != tt.expected {
			t.Errorf("ModelID(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestSplitModel(t *testing.T) {
	tests := []struct {
		input     string
		wantProv  string
		wantModel string
	}{
		{"github-copilot/claude-sonnet-4.6", "github-copilot", "claude-sonnet-4.6"},
		{"anthropic/claude-opus-4-6", "anthropic", "claude-opus-4-6"},
		{"openai/gpt-4o", "openai", "gpt-4o"},
		{"no-prefix", "github-copilot", "no-prefix"},
		{"", "github-copilot", ""},
	}
	for _, tt := range tests {
		prov, model := splitModel(tt.input)
		if prov != tt.wantProv || model != tt.wantModel {
			t.Errorf("splitModel(%q) = (%q, %q), want (%q, %q)", tt.input, prov, model, tt.wantProv, tt.wantModel)
		}
	}
}

func TestModelRouterPrimary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := openai.ChatCompletionResponse{
			ID:    "test-id",
			Model: "test-model",
			Choices: []openai.ChatCompletionChoice{{
				Index: 0,
				Message: openai.ChatCompletionMessage{
					Role:    "assistant",
					Content: "Hello from primary!",
				},
				FinishReason: "stop",
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			http.Error(w, err.Error(), 500)
		}
	}))
	defer srv.Close()

	cfg := openai.DefaultConfig("test-token")
	cfg.BaseURL = srv.URL
	client := openai.NewClientWithConfig(cfg)
	providers := map[string]Provider{"github-copilot": &openaiProvider{client: client}}

	router := NewRouter(testLogger(), providers, "github-copilot/primary-model", []string{"github-copilot/fallback-model"})

	req := openai.ChatCompletionRequest{
		Model: "github-copilot/primary-model",
		Messages: []openai.ChatCompletionMessage{
			{Role: "user", Content: "hello"},
		},
	}

	resp, err := router.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(resp.Choices) == 0 {
		t.Fatal("no choices")
	}
	if resp.Choices[0].Message.Content != "Hello from primary!" {
		t.Errorf("unexpected content: %s", resp.Choices[0].Message.Content)
	}
}

func TestModelRouterFallback(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++

		var req openai.ChatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}

		// First call (primary-model) fails, second call (fallback-model) succeeds.
		if req.Model == "primary-model" {
			http.Error(w, "primary model unavailable", http.StatusServiceUnavailable)
			return
		}

		resp := openai.ChatCompletionResponse{
			ID:    "fallback-id",
			Model: req.Model,
			Choices: []openai.ChatCompletionChoice{{
				Message: openai.ChatCompletionMessage{
					Role:    "assistant",
					Content: "Hello from fallback!",
				},
				FinishReason: "stop",
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			http.Error(w, err.Error(), 500)
		}
	}))
	defer srv.Close()

	cfg := openai.DefaultConfig("test-token")
	cfg.BaseURL = srv.URL
	client := openai.NewClientWithConfig(cfg)
	providers := map[string]Provider{"github-copilot": &openaiProvider{client: client}}

	router := NewRouter(testLogger(), providers, "github-copilot/primary-model", []string{"github-copilot/fallback-model"})

	req := openai.ChatCompletionRequest{
		Messages: []openai.ChatCompletionMessage{
			{Role: "user", Content: "hello"},
		},
	}

	resp, err := router.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(resp.Choices) == 0 {
		t.Fatal("no choices")
	}
	if resp.Choices[0].Message.Content != "Hello from fallback!" {
		t.Errorf("expected fallback response, got: %s", resp.Choices[0].Message.Content)
	}
	if callCount < 2 {
		t.Errorf("expected at least 2 calls (primary + fallback), got %d", callCount)
	}
}

func TestModelList(t *testing.T) {
	router := &Router{
		primary:   "github-copilot/main",
		fallbacks: []string{"github-copilot/fb1", "github-copilot/fb2"},
	}

	// No override — returns primary + fallbacks
	list := router.modelList("")
	if len(list) != 3 {
		t.Errorf("expected 3 models, got %d", len(list))
	}
	if list[0] != "github-copilot/main" {
		t.Errorf("expected primary first, got %s", list[0])
	}

	// Override with different model
	list = router.modelList("different-model")
	if len(list) != 1 {
		t.Errorf("expected 1 model with override, got %d", len(list))
	}
	if list[0] != "different-model" {
		t.Errorf("expected override model, got %s", list[0])
	}

	// Override with same as primary
	list = router.modelList("github-copilot/main")
	if len(list) != 3 {
		t.Errorf("expected 3 models when override matches primary, got %d", len(list))
	}
}

func TestStreamAll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i, chunk := range []string{"Hello", " ", "World"} {
			data := openai.ChatCompletionStreamResponse{
				ID: "stream-id",
				Choices: []openai.ChatCompletionStreamChoice{{
					Index: 0,
					Delta: openai.ChatCompletionStreamChoiceDelta{
						Content: chunk,
					},
				}},
			}
			jsonData, _ := json.Marshal(data)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", jsonData)
			_ = i
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	cfg := openai.DefaultConfig("test-token")
	cfg.BaseURL = srv.URL
	client := openai.NewClientWithConfig(cfg)

	stream, err := client.CreateChatCompletionStream(context.Background(), openai.ChatCompletionRequest{
		Model:    "test",
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hi"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("CreateStream: %v", err)
	}

	result, err := StreamAll(stream)
	if err != nil {
		t.Fatalf("StreamAll: %v", err)
	}
	if result != "Hello World" {
		t.Errorf("expected 'Hello World', got %q", result)
	}
}

func TestDefaultBaseURL(t *testing.T) {
	cases := []struct {
		name     string
		expected string
	}{
		{"openai", "https://api.openai.com/v1"},
		{"groq", "https://api.groq.com/openai/v1"},
		{"openrouter", "https://openrouter.ai/api/v1"},
		{"ollama", "http://localhost:11434/v1"},
		{"unknown", ""},
	}
	for _, tc := range cases {
		got := DefaultBaseURL(tc.name)
		if got != tc.expected {
			t.Errorf("DefaultBaseURL(%q) = %q, want %q", tc.name, got, tc.expected)
		}
	}
}

// mockProvider is a simple Provider for testing.
type mockProvider struct {
	chatResponse openai.ChatCompletionResponse
	chatErr      error
}

func (m *mockProvider) Chat(_ context.Context, _ openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	return m.chatResponse, m.chatErr
}

func (m *mockProvider) ChatStream(_ context.Context, _ openai.ChatCompletionRequest) (Stream, error) {
	return nil, fmt.Errorf("not implemented")
}

func TestProviderFor(t *testing.T) {
	mock := &mockProvider{}
	providers := map[string]Provider{"test-provider": mock}

	p, modelID, err := ProviderFor(providers, "test-provider/my-model")
	if err != nil {
		t.Fatalf("ProviderFor: %v", err)
	}
	if p != mock {
		t.Error("wrong provider returned")
	}
	if modelID != "my-model" {
		t.Errorf("expected model 'my-model', got %q", modelID)
	}

	_, _, err = ProviderFor(providers, "unknown-provider/model")
	if err == nil {
		t.Error("expected error for unknown provider")
	}
}

func TestProviderNames(t *testing.T) {
	providers := map[string]Provider{
		"a": &mockProvider{},
		"b": &mockProvider{},
	}
	names := ProviderNames(providers)
	if len(names) != 2 {
		t.Errorf("expected 2 names, got %d", len(names))
	}
}

func TestRouterTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{
				Message: openai.ChatCompletionMessage{Role: "assistant", Content: "ok"},
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	cfg := openai.DefaultConfig("test-token")
	cfg.BaseURL = srv.URL
	client := openai.NewClientWithConfig(cfg)
	providers := map[string]Provider{"test": &openaiProvider{client: client}}

	router := NewRouter(testLogger(), providers, "test/model", nil)
	router.Timeout = 5 * time.Second

	req := openai.ChatCompletionRequest{
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hi"}},
	}
	resp, err := router.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Choices[0].Message.Content != "ok" {
		t.Errorf("unexpected content: %s", resp.Choices[0].Message.Content)
	}
}

func TestRouterAllFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fail", 500)
	}))
	defer srv.Close()

	cfg := openai.DefaultConfig("test-token")
	cfg.BaseURL = srv.URL
	client := openai.NewClientWithConfig(cfg)
	providers := map[string]Provider{"test": &openaiProvider{client: client}}

	router := NewRouter(testLogger(), providers, "test/model", []string{"test/fallback"})
	router.Timeout = 2 * time.Second

	req := openai.ChatCompletionRequest{
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hi"}},
	}
	_, err := router.Chat(context.Background(), req)
	if err == nil {
		t.Error("expected error when all models fail")
	}
	if !strings.Contains(err.Error(), "all models failed") {
		t.Errorf("expected 'all models failed', got: %v", err)
	}
}

func TestRouterStreamAllFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fail", 500)
	}))
	defer srv.Close()

	cfg := openai.DefaultConfig("test-token")
	cfg.BaseURL = srv.URL
	client := openai.NewClientWithConfig(cfg)
	providers := map[string]Provider{"test": &openaiProvider{client: client}}

	router := NewRouter(testLogger(), providers, "test/model", nil)

	req := openai.ChatCompletionRequest{
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hi"}},
		Stream:   true,
	}
	_, err := router.ChatStream(context.Background(), req)
	if err == nil {
		t.Error("expected error when stream fails")
	}
}

func TestModelContext(t *testing.T) {
	router := &Router{Timeout: 120 * time.Second}

	// Primary with fallbacks — should cap at 60s
	ctx, cancel := router.modelContext(context.Background(), 0, 2)
	defer cancel()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected deadline")
	}
	remaining := time.Until(dl)
	if remaining > 61*time.Second {
		t.Errorf("expected primary timeout capped at 60s, got ~%v", remaining.Truncate(time.Second))
	}

	// Single model — use full timeout
	ctx2, cancel2 := router.modelContext(context.Background(), 0, 1)
	defer cancel2()
	dl2, _ := ctx2.Deadline()
	remaining2 := time.Until(dl2)
	if remaining2 < 100*time.Second {
		t.Errorf("expected full timeout ~120s, got ~%v", remaining2.Truncate(time.Second))
	}

	// Default timeout (Timeout=0)
	router2 := &Router{}
	ctx3, cancel3 := router2.modelContext(context.Background(), 0, 1)
	defer cancel3()
	dl3, _ := ctx3.Deadline()
	remaining3 := time.Until(dl3)
	if remaining3 < 100*time.Second {
		t.Errorf("expected default timeout ~120s, got ~%v", remaining3.Truncate(time.Second))
	}
}

func TestRouterStreamFallbackOnHang(t *testing.T) {
	// Primary server hangs; fallback responds immediately.
	hangDone := make(chan struct{})
	hangSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-hangDone:
		}
	}))
	defer func() { close(hangDone); hangSrv.Close() }()

	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer okSrv.Close()

	hangCfg := openai.DefaultConfig("t")
	hangCfg.BaseURL = hangSrv.URL
	okCfg := openai.DefaultConfig("t")
	okCfg.BaseURL = okSrv.URL

	providers := map[string]Provider{
		"hang": &openaiProvider{client: openai.NewClientWithConfig(hangCfg)},
		"ok":   &openaiProvider{client: openai.NewClientWithConfig(okCfg)},
	}

	router := NewRouter(testLogger(), providers, "hang/model", []string{"ok/model"})
	router.StreamConnectTTL = 1 * time.Second // short for test

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	stream, err := router.ChatStream(ctx, openai.ChatCompletionRequest{
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hi"}},
		Stream:   true,
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected fallback to succeed, got: %v", err)
	}
	defer stream.Close()

	// Should have fallen back within ~1s (the connect timeout), not 10s.
	if elapsed > 3*time.Second {
		t.Errorf("expected fallback within ~1s, took %v", elapsed.Truncate(time.Millisecond))
	}

	text, err := StreamAll(stream)
	if err != nil {
		t.Fatalf("StreamAll: %v", err)
	}
	if text != "ok" {
		t.Errorf("expected 'ok' from fallback, got %q", text)
	}
}

func TestConvertToAnthropicMessages(t *testing.T) {
	msgs := []openai.ChatCompletionMessage{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi there"},
		{Role: "user", Content: "Use a tool"},
		{Role: "assistant", Content: "", ToolCalls: []openai.ToolCall{{
			ID:       "tc1",
			Type:     openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: "exec", Arguments: `{"cmd":"ls"}`},
		}}},
		{Role: "tool", ToolCallID: "tc1", Content: "file1\nfile2"},
	}

	system, result, err := convertToAnthropicMessages(msgs)
	if err != nil {
		t.Fatalf("convertToAnthropicMessages: %v", err)
	}
	if system != "You are helpful." {
		t.Errorf("expected system prompt, got %q", system)
	}
	if len(result) != 5 {
		t.Errorf("expected 5 messages, got %d", len(result))
	}
}

func TestConvertToAnthropicMessages_ContentWithToolCalls(t *testing.T) {
	// Test assistant message with BOTH text content AND tool calls.
	msgs := []openai.ChatCompletionMessage{
		{Role: "user", Content: "Do something"},
		{Role: "assistant", Content: "Let me use a tool for that.", ToolCalls: []openai.ToolCall{{
			ID:       "tc1",
			Type:     openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: "exec", Arguments: `{"cmd":"ls"}`},
		}}},
		{Role: "tool", ToolCallID: "tc1", Content: "output"},
		{Role: "user", Content: "Thanks"},
	}

	system, result, err := convertToAnthropicMessages(msgs)
	if err != nil {
		t.Fatalf("convertToAnthropicMessages: %v", err)
	}
	if system != "" {
		t.Errorf("expected empty system, got %q", system)
	}
	// user, assistant(with content+tools), tool→user(flush), user
	if len(result) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(result))
	}
}

func TestConvertToAnthropicMessages_EmptyToolArguments(t *testing.T) {
	// Test assistant tool call with empty/missing arguments.
	msgs := []openai.ChatCompletionMessage{
		{Role: "user", Content: "Help"},
		{Role: "assistant", ToolCalls: []openai.ToolCall{{
			ID:       "tc1",
			Type:     openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: "status", Arguments: ""},
		}}},
		{Role: "tool", ToolCallID: "tc1", Content: "ok"},
	}

	_, result, err := convertToAnthropicMessages(msgs)
	if err != nil {
		t.Fatalf("convertToAnthropicMessages: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(result))
	}
}

func TestConvertToAnthropicMessages_ToolFlushOnUser(t *testing.T) {
	// Tool results followed by another user message exercises flush in user case.
	msgs := []openai.ChatCompletionMessage{
		{Role: "user", Content: "Use tools"},
		{Role: "assistant", ToolCalls: []openai.ToolCall{
			{ID: "tc1", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "a", Arguments: `{}`}},
			{ID: "tc2", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "b", Arguments: `{}`}},
		}},
		{Role: "tool", ToolCallID: "tc1", Content: "res1"},
		{Role: "tool", ToolCallID: "tc2", Content: "res2"},
		{Role: "user", Content: "Now what?"},
	}

	_, result, err := convertToAnthropicMessages(msgs)
	if err != nil {
		t.Fatalf("convertToAnthropicMessages: %v", err)
	}
	// user, assistant, tool_results→user(flush), user
	if len(result) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(result))
	}
}

func TestConvertToAnthropicTools(t *testing.T) {
	tools := []openai.Tool{
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "test",
				Description: "A test tool",
				Parameters:  json.RawMessage(`{"type":"object"}`),
			},
		},
		{Type: openai.ToolTypeFunction}, // nil Function — should be skipped
	}
	result := convertToAnthropicTools(tools)
	if len(result) != 1 {
		t.Errorf("expected 1 tool (nil Function skipped), got %d", len(result))
	}
	if result[0].Name != "test" {
		t.Errorf("expected name 'test', got %q", result[0].Name)
	}
}

func TestConvertToAnthropicToolsEmptySchema(t *testing.T) {
	tools := []openai.Tool{{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:       "bare",
			Parameters: nil,
		},
	}}
	result := convertToAnthropicTools(tools)
	if len(result) != 1 {
		t.Fatal("expected 1 tool")
	}
	if string(result[0].InputSchema) != `{"type":"object","properties":{}}` {
		t.Errorf("expected default schema, got %s", result[0].InputSchema)
	}
}

func TestConvertResponse(t *testing.T) {
	p := &anthropicProvider{}
	ar := anthropicResponse{
		ID:         "msg_123",
		Model:      "claude-sonnet",
		StopReason: "tool_use",
		Content: []anthropicBlock{
			{Type: "text", Text: "Let me use a tool."},
			{Type: "tool_use", ID: "tc1", Name: "exec", Input: json.RawMessage(`{"cmd":"ls"}`)},
		},
		Usage: struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		}{InputTokens: 100, OutputTokens: 50},
	}

	resp := p.convertResponse(ar)
	if resp.ID != "msg_123" {
		t.Errorf("expected ID msg_123, got %s", resp.ID)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(resp.Choices))
	}
	if resp.Choices[0].Message.Content != "Let me use a tool." {
		t.Errorf("wrong content: %s", resp.Choices[0].Message.Content)
	}
	if len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.Choices[0].Message.ToolCalls))
	}
	if resp.Choices[0].FinishReason != openai.FinishReasonToolCalls {
		t.Errorf("expected tool_calls finish reason, got %s", resp.Choices[0].FinishReason)
	}
	if resp.Usage.PromptTokens != 100 || resp.Usage.CompletionTokens != 50 {
		t.Errorf("wrong usage: %+v", resp.Usage)
	}
}

func TestConvertResponseStopReasons(t *testing.T) {
	p := &anthropicProvider{}
	cases := []struct {
		stopReason string
		expected   openai.FinishReason
	}{
		{"end_turn", openai.FinishReasonStop},
		{"tool_use", openai.FinishReasonToolCalls},
		{"max_tokens", openai.FinishReasonLength},
	}
	for _, tc := range cases {
		ar := anthropicResponse{StopReason: tc.stopReason, Content: []anthropicBlock{{Type: "text", Text: "x"}}}
		resp := p.convertResponse(ar)
		if resp.Choices[0].FinishReason != tc.expected {
			t.Errorf("stopReason %q: expected %s, got %s", tc.stopReason, tc.expected, resp.Choices[0].FinishReason)
		}
	}
}

func TestConvertResponseEmptyToolInput(t *testing.T) {
	p := &anthropicProvider{}
	ar := anthropicResponse{
		Content: []anthropicBlock{
			{Type: "tool_use", ID: "tc1", Name: "exec", Input: nil},
		},
	}
	resp := p.convertResponse(ar)
	tc := resp.Choices[0].Message.ToolCalls[0]
	if tc.Function.Arguments != "{}" {
		t.Errorf("expected '{}' for nil input, got %q", tc.Function.Arguments)
	}
}

func TestBuildRequestThinking(t *testing.T) {
	p := &anthropicProvider{
		apiKey:   "test",
		thinking: ThinkingConfig{Enabled: true, BudgetTokens: 4096},
	}
	req := openai.ChatCompletionRequest{
		Model:    "claude-sonnet",
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hi"}},
	}
	ar, err := p.buildRequest(req, false)
	if err != nil {
		t.Fatal(err)
	}
	if ar.Thinking == nil {
		t.Fatal("expected thinking config")
	}
	if ar.Thinking.BudgetTokens != 4096 {
		t.Errorf("expected budget 4096, got %d", ar.Thinking.BudgetTokens)
	}
	if ar.MaxTokens <= 4096 {
		t.Errorf("expected maxTokens > budget, got %d", ar.MaxTokens)
	}
}

func TestBuildRequestThinkingDefaultBudget(t *testing.T) {
	p := &anthropicProvider{
		apiKey:   "test",
		thinking: ThinkingConfig{Enabled: true, BudgetTokens: 0},
	}
	req := openai.ChatCompletionRequest{
		Model:    "claude-sonnet",
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hi"}},
	}
	ar, err := p.buildRequest(req, false)
	if err != nil {
		t.Fatal(err)
	}
	if ar.Thinking.BudgetTokens != 8192 {
		t.Errorf("expected default budget 8192, got %d", ar.Thinking.BudgetTokens)
	}
}

func TestBuildRequestNoThinking(t *testing.T) {
	p := &anthropicProvider{apiKey: "test"}
	req := openai.ChatCompletionRequest{
		Model:    "claude-sonnet",
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hi"}},
	}
	ar, err := p.buildRequest(req, false)
	if err != nil {
		t.Fatal(err)
	}
	if ar.Thinking != nil {
		t.Error("expected no thinking config")
	}
}

func TestBuildRequestWithTools(t *testing.T) {
	p := &anthropicProvider{apiKey: "test"}
	req := openai.ChatCompletionRequest{
		Model:    "claude-sonnet",
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hi"}},
		Tools: []openai.Tool{{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:       "exec",
				Parameters: json.RawMessage(`{"type":"object"}`),
			},
		}},
	}
	ar, err := p.buildRequest(req, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(ar.Tools) != 1 {
		t.Errorf("expected 1 tool, got %d", len(ar.Tools))
	}
	if !ar.Stream {
		t.Error("expected stream=true")
	}
}

func TestRouterStreamFallback(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var req openai.ChatCompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		if req.Model == "primary-model" {
			http.Error(w, "fail", 500)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		data := openai.ChatCompletionStreamResponse{
			Choices: []openai.ChatCompletionStreamChoice{{
				Delta: openai.ChatCompletionStreamChoiceDelta{Content: "fallback"},
			}},
		}
		jsonData, _ := json.Marshal(data)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", jsonData)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg := openai.DefaultConfig("test-token")
	cfg.BaseURL = srv.URL
	client := openai.NewClientWithConfig(cfg)
	providers := map[string]Provider{"test": &openaiProvider{client: client}}

	router := NewRouter(testLogger(), providers, "test/primary-model", []string{"test/fallback-model"})

	req := openai.ChatCompletionRequest{
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hi"}},
		Stream:   true,
	}
	stream, err := router.ChatStream(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	result, err := StreamAll(stream)
	if err != nil {
		t.Fatalf("StreamAll: %v", err)
	}
	if result != "fallback" {
		t.Errorf("expected 'fallback', got %q", result)
	}
	if callCount < 2 {
		t.Errorf("expected at least 2 calls, got %d", callCount)
	}
}

func TestModelIDWithMultipleSlashes(t *testing.T) {
	if got := ModelID("provider/model/with/slashes"); got != "model/with/slashes" {
		t.Errorf("ModelID with multiple slashes: got %q", got)
	}
}

func TestNewAnthropicProvider(t *testing.T) {
	p := newAnthropicProvider("test-key", ThinkingConfig{Enabled: true, BudgetTokens: 4096})
	if p.apiKey != "test-key" {
		t.Errorf("expected apiKey 'test-key', got %q", p.apiKey)
	}
	if !p.thinking.Enabled {
		t.Error("expected thinking enabled")
	}
	if p.client == nil {
		t.Error("expected non-nil client")
	}
}

// rewriteTransport intercepts all requests and redirects them to a target URL.
type rewriteTransport struct {
	base   http.RoundTripper
	target string
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	targetURL, _ := url.Parse(rt.target)
	req.URL.Scheme = targetURL.Scheme
	req.URL.Host = targetURL.Host
	base := rt.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

func TestAnthropicChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("missing or wrong x-api-key: %q", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("wrong anthropic-version: %q", r.Header.Get("anthropic-version"))
		}

		resp := anthropicResponse{
			ID:         "msg_test",
			Model:      "claude-sonnet",
			StopReason: "end_turn",
			Content: []anthropicBlock{
				{Type: "text", Text: "Hello from Anthropic!"},
			},
			Usage: struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			}{InputTokens: 10, OutputTokens: 5},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	p := &anthropicProvider{
		apiKey: "test-key",
		client: &http.Client{Transport: &rewriteTransport{target: srv.URL}},
	}

	req := openai.ChatCompletionRequest{
		Model:    "claude-sonnet",
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hello"}},
	}
	resp, err := p.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(resp.Choices) == 0 {
		t.Fatal("no choices")
	}
	if resp.Choices[0].Message.Content != "Hello from Anthropic!" {
		t.Errorf("unexpected content: %s", resp.Choices[0].Message.Content)
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 5 {
		t.Errorf("wrong usage: %+v", resp.Usage)
	}
}

func TestAnthropicChatError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		_ = json.NewEncoder(w).Encode(anthropicAPIError{
			Type: "error",
			Error: struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			}{Type: "rate_limit_error", Message: "rate limited"},
		})
	}))
	defer srv.Close()

	p := &anthropicProvider{
		apiKey: "test-key",
		client: &http.Client{Transport: &rewriteTransport{target: srv.URL}},
	}

	req := openai.ChatCompletionRequest{
		Model:    "claude-sonnet",
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hello"}},
	}
	_, err := p.Chat(context.Background(), req)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("expected rate limit error, got: %v", err)
	}
}

func TestAnthropicChatNonJSONError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		_, _ = w.Write([]byte("Bad Gateway"))
	}))
	defer srv.Close()

	p := &anthropicProvider{
		apiKey: "test-key",
		client: &http.Client{Transport: &rewriteTransport{target: srv.URL}},
	}

	req := openai.ChatCompletionRequest{
		Model:    "claude-sonnet",
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hello"}},
	}
	_, err := p.Chat(context.Background(), req)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("expected 502 error, got: %v", err)
	}
}

func TestAnthropicChatStreamSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)

		fmt.Fprint(w, "event: content_block_start\n")
		fmt.Fprint(w, `data: {"index":0,"content_block":{"type":"text"}}`+"\n\n")

		fmt.Fprint(w, "event: content_block_delta\n")
		fmt.Fprint(w, `data: {"index":0,"delta":{"type":"text_delta","text":"Hello "}}`+"\n\n")

		fmt.Fprint(w, "event: content_block_delta\n")
		fmt.Fprint(w, `data: {"index":0,"delta":{"type":"text_delta","text":"World"}}`+"\n\n")

		fmt.Fprint(w, "event: message_stop\n")
		fmt.Fprint(w, "data: {}\n\n")
	}))
	defer srv.Close()

	p := &anthropicProvider{
		apiKey: "test-key",
		client: &http.Client{Transport: &rewriteTransport{target: srv.URL}},
	}

	req := openai.ChatCompletionRequest{
		Model:    "claude-sonnet",
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hello"}},
		Stream:   true,
	}
	stream, err := p.ChatStream(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer stream.Close()

	var content strings.Builder
	for {
		chunk, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("Recv: %v", err)
		}
		if len(chunk.Choices) > 0 {
			content.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	if content.String() != "Hello World" {
		t.Errorf("expected 'Hello World', got %q", content.String())
	}
}

func TestAnthropicChatStreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"type":"error","error":{"type":"server_error","message":"internal error"}}`)
	}))
	defer srv.Close()

	p := &anthropicProvider{
		apiKey: "test-key",
		client: &http.Client{Transport: &rewriteTransport{target: srv.URL}},
	}

	req := openai.ChatCompletionRequest{
		Model:    "claude-sonnet",
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hello"}},
	}
	_, err := p.ChatStream(context.Background(), req)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "internal error") {
		t.Errorf("expected 'internal error', got: %v", err)
	}
}

func TestAnthropicStreamToolUse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)

		fmt.Fprint(w, "event: content_block_start\n")
		fmt.Fprint(w, `data: {"index":0,"content_block":{"type":"text"}}`+"\n\n")
		fmt.Fprint(w, "event: content_block_delta\n")
		fmt.Fprint(w, `data: {"index":0,"delta":{"type":"text_delta","text":"Let me help."}}`+"\n\n")

		fmt.Fprint(w, "event: content_block_start\n")
		fmt.Fprint(w, `data: {"index":1,"content_block":{"type":"tool_use","id":"tc1","name":"exec"}}`+"\n\n")
		fmt.Fprint(w, "event: content_block_delta\n")
		fmt.Fprint(w, `data: {"index":1,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":"}}`+"\n\n")
		fmt.Fprint(w, "event: content_block_delta\n")
		fmt.Fprint(w, `data: {"index":1,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}`+"\n\n")

		fmt.Fprint(w, "event: message_stop\n")
		fmt.Fprint(w, "data: {}\n\n")
	}))
	defer srv.Close()

	p := &anthropicProvider{
		apiKey: "test-key",
		client: &http.Client{Transport: &rewriteTransport{target: srv.URL}},
	}

	req := openai.ChatCompletionRequest{
		Model:    "claude-sonnet",
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "run ls"}},
		Stream:   true,
	}
	stream, err := p.ChatStream(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer stream.Close()

	var content strings.Builder
	var toolCalls []openai.ToolCall
	for {
		chunk, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("Recv: %v", err)
		}
		if len(chunk.Choices) > 0 {
			d := chunk.Choices[0].Delta
			content.WriteString(d.Content)
			for _, tc := range d.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}
				for len(toolCalls) <= idx {
					toolCalls = append(toolCalls, openai.ToolCall{})
				}
				if tc.ID != "" {
					toolCalls[idx].ID = tc.ID
				}
				if tc.Function.Name != "" {
					toolCalls[idx].Function.Name = tc.Function.Name
				}
				toolCalls[idx].Function.Arguments += tc.Function.Arguments
			}
		}
	}
	if content.String() != "Let me help." {
		t.Errorf("expected 'Let me help.', got %q", content.String())
	}
	if len(toolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(toolCalls))
	}
	if toolCalls[0].Function.Arguments != `{"cmd":"ls"}` {
		t.Errorf("wrong args: %q", toolCalls[0].Function.Arguments)
	}
}

func TestSetHeaders(t *testing.T) {
	p := &anthropicProvider{apiKey: "my-key"}
	req, _ := http.NewRequest("POST", "http://example.com", nil)
	p.setHeaders(req, false)
	if req.Header.Get("x-api-key") != "my-key" {
		t.Errorf("wrong x-api-key")
	}
	if req.Header.Get("accept") != "" {
		t.Error("non-streaming should not set accept")
	}

	req2, _ := http.NewRequest("POST", "http://example.com", nil)
	p.setHeaders(req2, true)
	if req2.Header.Get("accept") != "text/event-stream" {
		t.Errorf("streaming should set accept=text/event-stream")
	}
}

type mockTokenProvider struct {
	token  string
	apiURL string
	err    error
}

func (m *mockTokenProvider) GetToken(_ context.Context) (string, error) {
	return m.token, m.err
}
func (m *mockTokenProvider) APIURL() string { return m.apiURL }

func TestCopilotTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("wrong auth: %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Copilot-Integration-Id") != "vscode-chat" {
			t.Errorf("wrong Copilot-Integration-Id")
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	ct := &copilotTransport{
		base:     srv.Client().Transport,
		provider: &mockTokenProvider{token: "test-token", apiURL: srv.URL},
	}
	req, _ := http.NewRequest("GET", srv.URL+"/test", nil)
	resp, err := ct.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestCopilotTransportTokenError(t *testing.T) {
	ct := &copilotTransport{
		provider: &mockTokenProvider{err: fmt.Errorf("no token")},
	}
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	_, err := ct.RoundTrip(req)
	if err == nil || !strings.Contains(err.Error(), "no token") {
		t.Errorf("expected token error, got: %v", err)
	}
}

func TestBuildProviders(t *testing.T) {
	tp := &mockTokenProvider{token: "test", apiURL: "http://localhost"}
	cfg := &config.Root{
		Providers: map[string]*config.ProviderConfig{
			"anthropic": {APIKey: "ant-key"},
			"openai":    {APIKey: "oai-key"},
			"custom":    {APIKey: "key", BaseURL: "http://custom.example.com/v1"},
		},
	}
	providers := BuildProviders(testLogger(), cfg, tp)
	if _, ok := providers["github-copilot"]; !ok {
		t.Error("missing github-copilot")
	}
	if _, ok := providers["anthropic"]; !ok {
		t.Error("missing anthropic")
	}
	if _, ok := providers["openai"]; !ok {
		t.Error("missing openai")
	}
	if _, ok := providers["custom"]; !ok {
		t.Error("missing custom")
	}
	if len(providers) != 4 {
		t.Errorf("expected 4 providers, got %d", len(providers))
	}
}

func TestBuildProvidersNilConfig(t *testing.T) {
	tp := &mockTokenProvider{token: "test", apiURL: "http://localhost"}
	cfg := &config.Root{Providers: map[string]*config.ProviderConfig{"nil": nil}}
	providers := BuildProviders(testLogger(), cfg, tp)
	if _, ok := providers["nil"]; ok {
		t.Error("nil config should be skipped")
	}
}

func TestBuildProvidersUnknownFallback(t *testing.T) {
	tp := &mockTokenProvider{token: "test", apiURL: "http://localhost"}
	cfg := &config.Root{Providers: map[string]*config.ProviderConfig{"unknown": {APIKey: "k"}}}
	providers := BuildProviders(testLogger(), cfg, tp)
	if _, ok := providers["unknown"]; !ok {
		t.Error("unknown provider should still be registered")
	}
}

func TestNewHTTP1Transport(t *testing.T) {
	tr := newHTTP1Transport()
	if tr.ForceAttemptHTTP2 {
		t.Error("expected HTTP/2 disabled")
	}
}

func TestNewCopilotProvider(t *testing.T) {
	tp := &mockTokenProvider{token: "test", apiURL: "http://localhost"}
	p := NewCopilotProvider(tp)
	if p == nil {
		t.Fatal("expected non-nil provider")
	}
}

func TestNewCopilotClient(t *testing.T) {
	tp := &mockTokenProvider{token: "test", apiURL: "http://localhost"}
	c := NewCopilotClient(tp)
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestAnthropicStreamClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "event: message_stop\ndata: {}\n\n")
	}))
	defer srv.Close()

	p := &anthropicProvider{
		apiKey: "test-key",
		client: &http.Client{Transport: &rewriteTransport{target: srv.URL}},
	}
	req := openai.ChatCompletionRequest{
		Model:    "claude-sonnet",
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hi"}},
	}
	stream, err := p.ChatStream(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestAnthropicStreamRecvAfterDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "event: message_stop\ndata: {}\n\n")
	}))
	defer srv.Close()

	p := &anthropicProvider{
		apiKey: "test-key",
		client: &http.Client{Transport: &rewriteTransport{target: srv.URL}},
	}
	req := openai.ChatCompletionRequest{
		Model:    "claude-sonnet",
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hi"}},
	}
	stream, err := p.ChatStream(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer stream.Close()

	_, err = stream.Recv()
	if err != io.EOF {
		t.Errorf("expected EOF, got %v", err)
	}
	_, err = stream.Recv()
	if err != io.EOF {
		t.Errorf("expected EOF on second recv, got %v", err)
	}
}

func TestProcessEventUnknown(t *testing.T) {
	s := &anthropicStream{blockType: make(map[int]string), blockToolIdx: make(map[int]int)}
	_, hasChunk, isDone, err := s.processEvent("ping", `{}`)
	if err != nil || hasChunk || isDone {
		t.Error("unknown event should produce no output")
	}
}

// ---------------------------------------------------------------------------
// processEvent — message_start / message_delta
// ---------------------------------------------------------------------------

func TestProcessEvent_MessageStart(t *testing.T) {
	s := &anthropicStream{blockType: make(map[int]string), blockToolIdx: make(map[int]int)}
	_, hasChunk, isDone, err := s.processEvent("message_start", `{"message":{"usage":{"input_tokens":42}}}`)
	if err != nil || hasChunk || isDone {
		t.Error("message_start should produce no output")
	}
	if s.inputTokens != 42 {
		t.Errorf("inputTokens: got %d, want 42", s.inputTokens)
	}
}

func TestProcessEvent_MessageStartBadJSON(t *testing.T) {
	s := &anthropicStream{blockType: make(map[int]string), blockToolIdx: make(map[int]int)}
	_, hasChunk, isDone, err := s.processEvent("message_start", `{bad json`)
	if err != nil || hasChunk || isDone {
		t.Error("message_start with bad JSON should be silent")
	}
	if s.inputTokens != 0 {
		t.Errorf("inputTokens should remain 0 on bad JSON, got %d", s.inputTokens)
	}
}

func TestProcessEvent_MessageDelta(t *testing.T) {
	s := &anthropicStream{blockType: make(map[int]string), blockToolIdx: make(map[int]int)}
	_, hasChunk, isDone, err := s.processEvent("message_delta", `{"usage":{"output_tokens":55}}`)
	if err != nil || hasChunk || isDone {
		t.Error("message_delta should produce no output")
	}
	if s.outputTokens != 55 {
		t.Errorf("outputTokens: got %d, want 55", s.outputTokens)
	}
}

func TestProcessEvent_MessageDeltaBadJSON(t *testing.T) {
	s := &anthropicStream{blockType: make(map[int]string), blockToolIdx: make(map[int]int)}
	_, hasChunk, isDone, err := s.processEvent("message_delta", `not json`)
	if err != nil || hasChunk || isDone {
		t.Error("message_delta with bad JSON should be silent")
	}
	if s.outputTokens != 0 {
		t.Errorf("outputTokens should remain 0 on bad JSON, got %d", s.outputTokens)
	}
}

func TestProcessEvent_MessageStop(t *testing.T) {
	s := &anthropicStream{blockType: make(map[int]string), blockToolIdx: make(map[int]int)}
	_, hasChunk, isDone, err := s.processEvent("message_stop", `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasChunk {
		t.Error("message_stop should not produce a chunk")
	}
	if !isDone {
		t.Error("message_stop should set isDone=true")
	}
}

// ---------------------------------------------------------------------------
// processEvent — content_block_start
// ---------------------------------------------------------------------------

func TestProcessEvent_ContentBlockStartText(t *testing.T) {
	s := &anthropicStream{blockType: make(map[int]string), blockToolIdx: make(map[int]int)}
	_, hasChunk, isDone, err := s.processEvent("content_block_start", `{"index":0,"content_block":{"type":"text"}}`)
	if err != nil || isDone {
		t.Fatal("unexpected error or done")
	}
	if hasChunk {
		t.Error("text block start should not produce a chunk")
	}
	if s.blockType[0] != "text" {
		t.Errorf("blockType[0] = %q, want 'text'", s.blockType[0])
	}
}

func TestProcessEvent_ContentBlockStartBadJSON(t *testing.T) {
	s := &anthropicStream{blockType: make(map[int]string), blockToolIdx: make(map[int]int)}
	_, hasChunk, isDone, err := s.processEvent("content_block_start", `{bad`)
	if err != nil || hasChunk || isDone {
		t.Error("bad JSON content_block_start should be silent")
	}
}

// ---------------------------------------------------------------------------
// processEvent — content_block_delta
// ---------------------------------------------------------------------------

func TestProcessEvent_ContentBlockDeltaEmptyText(t *testing.T) {
	s := &anthropicStream{blockType: make(map[int]string), blockToolIdx: make(map[int]int)}
	_, hasChunk, isDone, err := s.processEvent("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":""}}`)
	if err != nil || isDone {
		t.Fatal("unexpected error or done")
	}
	if hasChunk {
		t.Error("empty text delta should not produce a chunk")
	}
}

func TestProcessEvent_ContentBlockDeltaBadJSON(t *testing.T) {
	s := &anthropicStream{blockType: make(map[int]string), blockToolIdx: make(map[int]int)}
	_, hasChunk, isDone, err := s.processEvent("content_block_delta", `{{{`)
	if err != nil || hasChunk || isDone {
		t.Error("bad JSON content_block_delta should be silent")
	}
}

func TestProcessEvent_ContentBlockDeltaInputJSONNoToolIdx(t *testing.T) {
	s := &anthropicStream{blockType: make(map[int]string), blockToolIdx: make(map[int]int)}
	_, hasChunk, isDone, err := s.processEvent("content_block_delta", `{"index":99,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`)
	if err != nil || isDone {
		t.Fatal("unexpected error or done")
	}
	if hasChunk {
		t.Error("input_json_delta without matching tool index should not produce a chunk")
	}
}

func TestProcessEvent_ContentBlockDeltaUnknownType(t *testing.T) {
	s := &anthropicStream{blockType: make(map[int]string), blockToolIdx: make(map[int]int)}
	_, hasChunk, isDone, err := s.processEvent("content_block_delta", `{"index":0,"delta":{"type":"unknown_delta"}}`)
	if err != nil || isDone {
		t.Fatal("unexpected error or done")
	}
	if hasChunk {
		t.Error("unknown delta type should not produce a chunk")
	}
}

// ---------------------------------------------------------------------------
// anthropicStream.Usage
// ---------------------------------------------------------------------------

func TestAnthropicStreamUsage(t *testing.T) {
	s := &anthropicStream{
		blockType:    make(map[int]string),
		blockToolIdx: make(map[int]int),
		inputTokens:  100,
		outputTokens: 200,
	}
	in, out := s.Usage()
	if in != 100 || out != 200 {
		t.Errorf("Usage() = (%d, %d), want (100, 200)", in, out)
	}
}

// ---------------------------------------------------------------------------
// isThinkingRejection
// ---------------------------------------------------------------------------

func TestIsThinkingRejection_True(t *testing.T) {
	body := `{"type":"error","error":{"type":"invalid_request_error","message":"Thinking is not supported for this model"}}`
	if !isThinkingRejection(400, []byte(body)) {
		t.Error("should detect thinking rejection")
	}
}

func TestIsThinkingRejection_Think(t *testing.T) {
	body := `{"type":"error","error":{"type":"invalid_request_error","message":"This model cannot think"}}`
	if !isThinkingRejection(400, []byte(body)) {
		t.Error("should detect 'think' keyword")
	}
}

func TestIsThinkingRejection_WrongStatus(t *testing.T) {
	body := `{"type":"error","error":{"type":"invalid_request_error","message":"Thinking is not supported"}}`
	if isThinkingRejection(500, []byte(body)) {
		t.Error("should return false for non-400 status")
	}
}

func TestIsThinkingRejection_InvalidJSON(t *testing.T) {
	if isThinkingRejection(400, []byte("not json")) {
		t.Error("should return false for invalid JSON")
	}
}

func TestIsThinkingRejection_UnrelatedError(t *testing.T) {
	body := `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens must be positive"}}`
	if isThinkingRejection(400, []byte(body)) {
		t.Error("should return false for unrelated error message")
	}
}

// ---------------------------------------------------------------------------
// shouldThink
// ---------------------------------------------------------------------------

func TestShouldThink_Disabled(t *testing.T) {
	p := &anthropicProvider{thinking: ThinkingConfig{Enabled: false}}
	if p.shouldThink("claude-sonnet-4-20250514") {
		t.Error("should return false when thinking is disabled and no Level set")
	}
}

func TestShouldThink_LevelEnabled(t *testing.T) {
	p := &anthropicProvider{thinking: ThinkingConfig{Level: "enabled"}}
	if !p.shouldThink("claude-sonnet-4-20250514") {
		t.Error("should return true when Level is 'enabled'")
	}
}

func TestShouldThink_LevelAdaptive(t *testing.T) {
	p := &anthropicProvider{thinking: ThinkingConfig{Level: "adaptive"}}
	if !p.shouldThink("claude-sonnet-4-20250514") {
		t.Error("should return true when Level is 'adaptive'")
	}
}

func TestShouldThink_LevelOff(t *testing.T) {
	p := &anthropicProvider{thinking: ThinkingConfig{Level: "off"}}
	if p.shouldThink("claude-sonnet-4-20250514") {
		t.Error("should return false when Level is 'off'")
	}
}

func TestShouldThink_ElegacyEnabled(t *testing.T) {
	p := &anthropicProvider{thinking: ThinkingConfig{Enabled: true}}
	if !p.shouldThink("claude-3-haiku-20240307") {
		t.Error("should return true when legacy Enabled is true")
	}
}

func TestShouldThink_Claude46Default(t *testing.T) {
	p := &anthropicProvider{thinking: ThinkingConfig{}}
	if !p.shouldThink("claude-4-6-model") {
		t.Error("should default to true for Claude 4-6 models")
	}
}

func TestShouldThink_NonClaudeModel(t *testing.T) {
	p := &anthropicProvider{thinking: ThinkingConfig{}}
	if p.shouldThink("gpt-4") {
		t.Error("should return false for non-Claude model with nothing enabled")
	}
}

// ---------------------------------------------------------------------------
// Router.SetModels
// ---------------------------------------------------------------------------

func TestRouterSetModels(t *testing.T) {
	r := NewRouter(testLogger(), nil, "old-primary", []string{"old-fb1"})

	r.SetModels("new-primary", []string{"new-fb1", "new-fb2"})

	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.primary != "new-primary" {
		t.Errorf("primary: got %q, want 'new-primary'", r.primary)
	}
	if len(r.fallbacks) != 2 || r.fallbacks[0] != "new-fb1" {
		t.Errorf("fallbacks: got %v, want [new-fb1, new-fb2]", r.fallbacks)
	}
}

// ---------------------------------------------------------------------------
// Router.rateLimiter
// ---------------------------------------------------------------------------

func TestRouterRateLimiter(t *testing.T) {
	rl := &taskqueue.RateLimiter{}
	r := NewRouter(testLogger(), nil, "anthropic/claude-opus-4-6", nil)
	r.RateLimiters = map[string]*taskqueue.RateLimiter{"anthropic": rl}

	// provider/model → should find limiter by prefix "anthropic"
	got := r.rateLimiter("anthropic/claude-opus-4-6")
	if got != rl {
		t.Error("expected limiter for prefixed model")
	}

	// bare model name → should do direct lookup (no match here)
	got = r.rateLimiter("gpt-4")
	if got != nil {
		t.Error("expected nil for unregistered bare model")
	}

	// nil map → should return nil
	r2 := NewRouter(testLogger(), nil, "x", nil)
	if r2.rateLimiter("anything") != nil {
		t.Error("expected nil when RateLimiters is empty")
	}
}

// ---------------------------------------------------------------------------
// buildRequest – additional branches
// ---------------------------------------------------------------------------

func TestBuildRequestMaxTokensOverride(t *testing.T) {
	p := &anthropicProvider{}
	req := openai.ChatCompletionRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 2048,
		Messages:  []openai.ChatCompletionMessage{{Role: "user", Content: "hi"}},
	}
	ar, err := p.buildRequest(req, false)
	if err != nil {
		t.Fatal(err)
	}
	if ar.MaxTokens != 2048 {
		t.Errorf("expected MaxTokens=2048, got %d", ar.MaxTokens)
	}
}

func TestBuildRequestThinkingLowMaxToks(t *testing.T) {
	p := &anthropicProvider{
		thinking: ThinkingConfig{Enabled: true, BudgetTokens: 8192},
	}
	req := openai.ChatCompletionRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 1000, // less than budget
		Messages:  []openai.ChatCompletionMessage{{Role: "user", Content: "hi"}},
	}
	ar, err := p.buildRequest(req, false)
	if err != nil {
		t.Fatal(err)
	}
	// maxToks should be bumped to budget + 4096
	if ar.MaxTokens != 8192+4096 {
		t.Errorf("expected MaxTokens=%d, got %d", 8192+4096, ar.MaxTokens)
	}
	if ar.Thinking == nil {
		t.Error("expected Thinking to be set")
	}
}

// ---------------------------------------------------------------------------
// Router.modelList
// ---------------------------------------------------------------------------

func TestRouterModelList(t *testing.T) {
	r := NewRouter(testLogger(), nil, "anthropic/primary", []string{"openai/fb1", "openai/fb2"})

	// No override — returns primary + fallbacks
	got := r.modelList("")
	if len(got) != 3 || got[0] != "anthropic/primary" {
		t.Errorf("no-override: got %v", got)
	}

	// Override matches primary — same result
	got = r.modelList("anthropic/primary")
	if len(got) != 3 {
		t.Errorf("same-as-primary override: got %v", got)
	}

	// Override differs — returns only the override
	got = r.modelList("custom/model")
	if len(got) != 1 || got[0] != "custom/model" {
		t.Errorf("different override: got %v", got)
	}
}

// ---------------------------------------------------------------------------
// convertResponse – text-only and tool_use
// ---------------------------------------------------------------------------

func TestConvertResponseTextOnly(t *testing.T) {
	p := &anthropicProvider{}
	ar := anthropicResponse{
		ID:   "msg_123",
		Role: "assistant",
		Content: []anthropicBlock{
			{Type: "text", Text: "hello world"},
		},
		Model:      "claude-sonnet-4-20250514",
		StopReason: "end_turn",
	}
	ar.Usage.InputTokens = 10
	ar.Usage.OutputTokens = 5

	resp := p.convertResponse(ar)
	if len(resp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(resp.Choices))
	}
	if resp.Choices[0].Message.Content != "hello world" {
		t.Errorf("content: got %q", resp.Choices[0].Message.Content)
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 5 {
		t.Errorf("usage: got prompt=%d completion=%d", resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	}
}

func TestConvertResponseWithToolUse(t *testing.T) {
	p := &anthropicProvider{}
	ar := anthropicResponse{
		Content: []anthropicBlock{
			{Type: "tool_use", ID: "tool_1", Name: "search", Input: json.RawMessage(`{"q":"test"}`)},
		},
	}
	resp := p.convertResponse(ar)
	if len(resp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(resp.Choices))
	}
	if len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.Choices[0].Message.ToolCalls))
	}
	tc := resp.Choices[0].Message.ToolCalls[0]
	if tc.Function.Name != "search" {
		t.Errorf("tool name: got %q", tc.Function.Name)
	}
}

// --- Retry / isRetryable tests ---

func TestIsRetryable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context canceled", context.Canceled, false},
		{"context deadline", context.DeadlineExceeded, false},
		{"wrapped context canceled", fmt.Errorf("http: %w", context.Canceled), false},
		{"wrapped context deadline", fmt.Errorf("http: %w", context.DeadlineExceeded), false},
		{"429", fmt.Errorf("status code: 429"), true},
		{"500", fmt.Errorf("status code: 500"), true},
		{"502", fmt.Errorf("status code: 502"), true},
		{"503", fmt.Errorf("status code: 503"), true},
		{"504", fmt.Errorf("status code: 504"), true},
		{"529 overloaded", fmt.Errorf("status code: 529"), true},
		{"rate limit message", fmt.Errorf("rate limit exceeded"), true},
		{"too many requests", fmt.Errorf("Too Many Requests"), true},
		{"overloaded", fmt.Errorf("model is overloaded"), true},
		{"connection reset", fmt.Errorf("connection reset by peer"), true},
		{"connection refused", fmt.Errorf("connection refused"), true},
		{"broken pipe", fmt.Errorf("broken pipe"), true},
		{"eof", fmt.Errorf("unexpected EOF"), true},
		{"normal error", fmt.Errorf("invalid model"), false},
		{"auth 401", fmt.Errorf("status code: 401"), false},
		{"bad request 400", fmt.Errorf("status code: 400"), false},
		{"APIError 429", &APIError{StatusCode: 429, Message: "anthropic 429: rate limited"}, true},
		{"APIError 500", &APIError{StatusCode: 500, Message: "anthropic 500: internal"}, true},
		{"APIError 408", &APIError{StatusCode: 408, Message: "anthropic 408: timeout"}, true},
		{"APIError 502", &APIError{StatusCode: 502, Message: "anthropic 502: bad gateway"}, true},
		{"APIError 504", &APIError{StatusCode: 504, Message: "anthropic 504: gateway timeout"}, true},
		{"APIError 401 not retryable", &APIError{StatusCode: 401, Message: "anthropic 401: unauthorized"}, false},
		{"APIError 400 not retryable", &APIError{StatusCode: 400, Message: "anthropic 400: bad request"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryable(tt.err); got != tt.want {
				t.Errorf("isRetryable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestRetryAfterFromErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want time.Duration
	}{
		{"nil", nil, 0},
		{"plain error", fmt.Errorf("something"), 0},
		{"APIError no retry-after", &APIError{StatusCode: 429, Message: "rate limited"}, 0},
		{"APIError with retry-after", &APIError{StatusCode: 429, RetryAfter: 5 * time.Second, Message: "rate limited"}, 5 * time.Second},
		{"wrapped APIError", fmt.Errorf("wrap: %w", &APIError{StatusCode: 429, RetryAfter: 3 * time.Second, Message: "rate limited"}), 3 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := retryAfterFromErr(tt.err); got != tt.want {
				t.Errorf("retryAfterFromErr() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRetryBackoffRespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled
	if retryBackoff(ctx, 0, 0) {
		t.Error("expected retryBackoff to return false on canceled context")
	}
}

// flakyProvider fails N times with a retryable error, then succeeds.
type flakyProvider struct {
	failCount int
	calls     int
	failErr   error
}

func (fp *flakyProvider) Chat(_ context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	fp.calls++
	if fp.calls <= fp.failCount {
		return openai.ChatCompletionResponse{}, fp.failErr
	}
	return openai.ChatCompletionResponse{
		Model: req.Model,
		Choices: []openai.ChatCompletionChoice{{
			Message: openai.ChatCompletionMessage{Role: "assistant", Content: "ok"},
		}},
	}, nil
}

func (fp *flakyProvider) ChatStream(_ context.Context, req openai.ChatCompletionRequest) (Stream, error) {
	fp.calls++
	if fp.calls <= fp.failCount {
		return nil, fp.failErr
	}
	return &fakeStream{chunks: []string{"ok"}}, nil
}

// fakeStream is a minimal Stream implementation for testing.
type fakeStream struct {
	chunks []string
	idx    int
}

func (fs *fakeStream) Recv() (openai.ChatCompletionStreamResponse, error) {
	if fs.idx >= len(fs.chunks) {
		return openai.ChatCompletionStreamResponse{}, io.EOF
	}
	chunk := fs.chunks[fs.idx]
	fs.idx++
	return openai.ChatCompletionStreamResponse{
		Choices: []openai.ChatCompletionStreamChoice{{
			Delta: openai.ChatCompletionStreamChoiceDelta{Content: chunk},
		}},
	}, nil
}

func (fs *fakeStream) Close() error { return nil }

func TestRouterChatRetriesTransientError(t *testing.T) {
	fp := &flakyProvider{failCount: 2, failErr: fmt.Errorf("status code: 502")}
	providers := map[string]Provider{"test": fp}
	router := NewRouter(testLogger(), providers, "test/model", nil)
	router.MaxRetries = 2

	req := openai.ChatCompletionRequest{
		Model:    "test/model",
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hello"}},
	}
	resp, err := router.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if resp.Choices[0].Message.Content != "ok" {
		t.Errorf("unexpected content: %q", resp.Choices[0].Message.Content)
	}
	if fp.calls != 3 {
		t.Errorf("expected 3 calls (2 failures + 1 success), got %d", fp.calls)
	}
}

func TestRouterChatNoRetryOnNonTransient(t *testing.T) {
	fp := &flakyProvider{failCount: 10, failErr: fmt.Errorf("invalid API key")}
	providers := map[string]Provider{"test": fp}
	router := NewRouter(testLogger(), providers, "test/model", nil)
	router.MaxRetries = 2

	req := openai.ChatCompletionRequest{
		Model:    "test/model",
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hello"}},
	}
	_, err := router.Chat(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for non-transient failure")
	}
	if fp.calls != 1 {
		t.Errorf("expected 1 call (no retries for non-transient), got %d", fp.calls)
	}
}

func TestRouterChatStreamRetriesTransientError(t *testing.T) {
	fp := &flakyProvider{failCount: 1, failErr: fmt.Errorf("status code: 503")}
	providers := map[string]Provider{"test": fp}
	router := NewRouter(testLogger(), providers, "test/model", nil)
	router.MaxRetries = 2

	req := openai.ChatCompletionRequest{
		Model:    "test/model",
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hello"}},
	}
	stream, err := router.ChatStream(context.Background(), req)
	if err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}
	defer stream.Close()
	text, err := StreamAll(stream)
	if err != nil {
		t.Fatalf("StreamAll: %v", err)
	}
	if text != "ok" {
		t.Errorf("unexpected stream content: %q", text)
	}
	if fp.calls != 2 {
		t.Errorf("expected 2 calls (1 failure + 1 success), got %d", fp.calls)
	}
}

func TestRouterChatExhaustsRetries(t *testing.T) {
	fp := &flakyProvider{failCount: 10, failErr: fmt.Errorf("status code: 429")}
	providers := map[string]Provider{"test": fp}
	router := NewRouter(testLogger(), providers, "test/model", nil)
	router.MaxRetries = 2

	req := openai.ChatCompletionRequest{
		Model:    "test/model",
		Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hello"}},
	}
	_, err := router.Chat(context.Background(), req)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	// 1 initial + 2 retries = 3 total
	if fp.calls != 3 {
		t.Errorf("expected 3 calls, got %d", fp.calls)
	}
}
