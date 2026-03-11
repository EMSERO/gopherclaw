package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/config"
	"github.com/EMSERO/gopherclaw/internal/eidetic"
	"github.com/EMSERO/gopherclaw/internal/hooks"
	"github.com/EMSERO/gopherclaw/internal/models"
	"github.com/EMSERO/gopherclaw/internal/session"
)

// ---------------------------------------------------------------------------
// isContextOverflow — cover all error string patterns
// ---------------------------------------------------------------------------

func TestIsContextOverflowAllPatterns(t *testing.T) {
	patterns := []string{
		"context_length_exceeded",
		"maximum context length",
		"token limit reached",
		"too many tokens in request",
		"context window full",
		"prompt is too long for this model",
		"request too large to process",
	}
	for _, p := range patterns {
		if !isContextOverflow(fmt.Errorf("error: %s", p)) {
			t.Errorf("expected isContextOverflow to match pattern %q", p)
		}
	}
	// Non-overflow errors
	if isContextOverflow(fmt.Errorf("rate limit exceeded")) {
		t.Error("rate limit should not be context overflow")
	}
	if isContextOverflow(nil) {
		t.Error("nil error should not be context overflow")
	}
}

// ---------------------------------------------------------------------------
// ModelHealth — cover agent.go:270
// ---------------------------------------------------------------------------

func TestModelHealth(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	router := makeRouter()
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)

	health := ag.ModelHealth()
	if health == nil {
		t.Error("ModelHealth should not return nil")
	}
	// Should have at least the primary model
	if len(health) == 0 {
		t.Error("expected at least one model in health status")
	}
}

// ---------------------------------------------------------------------------
// GetConfig — cover agent.go:242
// ---------------------------------------------------------------------------

func TestGetConfig(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, "", nil)

	got := ag.GetConfig()
	if got != cfg {
		t.Error("GetConfig should return the current config")
	}
}

// ---------------------------------------------------------------------------
// embed — cover agent.go:317 (nil client path + error path)
// ---------------------------------------------------------------------------

func TestEmbedNilClient(t *testing.T) {
	ag := &Agent{logger: zap.NewNop().Sugar()}
	// No embeddings client set — should return nil
	vec := ag.embed(context.Background(), "test text")
	if vec != nil {
		t.Error("embed with nil client should return nil")
	}
}

// ---------------------------------------------------------------------------
// SetEmbeddings / getEmbeddings — cover agent.go:301, 308
// ---------------------------------------------------------------------------

func TestSetGetEmbeddings(t *testing.T) {
	ag := &Agent{logger: zap.NewNop().Sugar()}
	if ag.getEmbeddings() != nil {
		t.Error("expected nil embeddings initially")
	}
	// SetEmbeddings with nil should be safe
	ag.SetEmbeddings(nil)
	if ag.getEmbeddings() != nil {
		t.Error("expected nil after SetEmbeddings(nil)")
	}
}

// ---------------------------------------------------------------------------
// appendToEidetic — cover agent.go:343 (nil client, semaphore full)
// ---------------------------------------------------------------------------

// mockEideticClient implements eidetic.Client for testing.
type mockEideticClient struct {
	appendCalled chan struct{}
	appendErr    error
	recentResult []eidetic.MemoryEntry
}

func (m *mockEideticClient) AppendMemory(_ context.Context, _ eidetic.AppendRequest) error {
	if m.appendCalled != nil {
		select {
		case m.appendCalled <- struct{}{}:
		default:
		}
	}
	return m.appendErr
}

func (m *mockEideticClient) SearchMemory(_ context.Context, _ eidetic.SearchRequest) ([]eidetic.MemoryEntry, error) {
	return nil, nil
}

func (m *mockEideticClient) GetRecent(_ context.Context, _ string, _ int) ([]eidetic.MemoryEntry, error) {
	return m.recentResult, nil
}

func (m *mockEideticClient) Health(_ context.Context) error { return nil }

func TestAppendToEideticNilClient(t *testing.T) {
	cfg := newTestConfig()
	ag := &Agent{
		cfg:        cfg,
		def:        cfg.DefaultAgent(),
		logger:     zap.NewNop().Sugar(),
		eideticSem: make(chan struct{}, 8),
	}
	// No eidetic client — should return immediately (no panic)
	ag.appendToEidetic("session:1", "user text", "assistant text")
}

func TestAppendToEideticSuccess(t *testing.T) {
	cfg := newTestConfig()
	doneCh := make(chan struct{}, 1)
	ec := &mockEideticClient{appendCalled: doneCh}

	ag := &Agent{
		cfg:        cfg,
		def:        cfg.DefaultAgent(),
		logger:     zap.NewNop().Sugar(),
		eideticSem: make(chan struct{}, 8),
		eideticC:   ec,
	}

	ag.appendToEidetic("session:1", "hello", "hi there")

	select {
	case <-doneCh:
		// success — AppendMemory was called
	case <-time.After(5 * time.Second):
		t.Error("timed out waiting for AppendMemory to be called")
	}
}

func TestAppendToEideticSemFull(t *testing.T) {
	cfg := newTestConfig()
	ec := &mockEideticClient{appendCalled: make(chan struct{}, 1)}

	ag := &Agent{
		cfg:        cfg,
		def:        cfg.DefaultAgent(),
		logger:     zap.NewNop().Sugar(),
		eideticSem: make(chan struct{}, 1), // capacity 1
		eideticC:   ec,
	}

	// Fill the semaphore
	ag.eideticSem <- struct{}{}

	// This should be skipped (semaphore full)
	ag.appendToEidetic("session:1", "user", "assistant")

	// Brief wait to ensure no goroutine was spawned
	time.Sleep(50 * time.Millisecond)

	select {
	case <-ec.appendCalled:
		t.Error("AppendMemory should NOT have been called with full semaphore")
	default:
		// Expected — call was skipped
	}

	// Clean up
	<-ag.eideticSem
}

func TestAppendToEideticAppendError(t *testing.T) {
	cfg := newTestConfig()
	doneCh := make(chan struct{}, 1)
	ec := &mockEideticClient{
		appendCalled: doneCh,
		appendErr:    fmt.Errorf("network error"),
	}

	ag := &Agent{
		cfg:        cfg,
		def:        cfg.DefaultAgent(),
		logger:     zap.NewNop().Sugar(),
		eideticSem: make(chan struct{}, 8),
		eideticC:   ec,
	}

	ag.appendToEidetic("session:1", "hello", "hi")

	select {
	case <-doneCh:
		// AppendMemory was called (even though it returned an error, that's fine)
	case <-time.After(5 * time.Second):
		t.Error("timed out waiting for AppendMemory call")
	}
}

// ---------------------------------------------------------------------------
// eideticAgentID — cover the configured override path
// ---------------------------------------------------------------------------

func TestEideticAgentIDOverride(t *testing.T) {
	cfg := newTestConfig()
	cfg.Eidetic.AgentID = "custom-agent-id"
	ag := &Agent{
		cfg:    cfg,
		def:    cfg.DefaultAgent(),
		logger: zap.NewNop().Sugar(),
	}

	if id := ag.eideticAgentID(); id != "custom-agent-id" {
		t.Errorf("expected 'custom-agent-id', got %q", id)
	}
}

func TestEideticAgentIDDefault(t *testing.T) {
	cfg := newTestConfig()
	ag := &Agent{
		cfg:    cfg,
		def:    cfg.DefaultAgent(),
		logger: zap.NewNop().Sugar(),
	}

	if id := ag.eideticAgentID(); id != "main" {
		t.Errorf("expected 'main' (def.ID), got %q", id)
	}
}

// ---------------------------------------------------------------------------
// ChatWithImages — cover agent.go:579
// ---------------------------------------------------------------------------

func TestChatWithImagesRealRouter(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	router := makeRouter(openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{{
			Message: openai.ChatCompletionMessage{
				Role:    "assistant",
				Content: "I see the image.",
			},
		}},
		Usage: openai.Usage{PromptTokens: 100, CompletionTokens: 15},
	})

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)

	imageURLs := []string{"data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="}
	resp, err := ag.ChatWithImages(context.Background(), "test:images", "describe this", imageURLs)
	if err != nil {
		t.Fatalf("ChatWithImages: %v", err)
	}
	if resp.Text != "I see the image." {
		t.Errorf("expected 'I see the image.', got %q", resp.Text)
	}
	if resp.Usage.InputTokens != 100 {
		t.Errorf("expected 100 input tokens, got %d", resp.Usage.InputTokens)
	}

	// Verify history was persisted with ImageURLs
	h, err := sm.GetHistory("test:images")
	if err != nil {
		t.Fatal(err)
	}
	if len(h) < 2 {
		t.Fatalf("expected at least 2 messages, got %d", len(h))
	}
	if h[0].Role != "user" || h[0].Content != "describe this" {
		t.Errorf("first message: got role=%q content=%q", h[0].Role, h[0].Content)
	}
}

func TestChatWithImagesSemExhausted(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	ag := &Agent{
		cfg:        cfg,
		def:        cfg.DefaultAgent(),
		sessions:   sm,
		toolMap:    make(map[string]Tool),
		sem:        make(chan struct{}, 1),
		logger:     zap.NewNop().Sugar(),
		eideticSem: make(chan struct{}, 8),
	}
	ag.sem <- struct{}{} // fill semaphore

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := ag.ChatWithImages(ctx, "test:images-sem", "hello", []string{"img"})
	if err == nil {
		t.Error("expected error when semaphore exhausted")
	}
	if !strings.Contains(err.Error(), "max concurrent") {
		t.Errorf("expected 'max concurrent' error, got %q", err.Error())
	}
	<-ag.sem
}

func TestChatWithImagesContextOverflowRetry(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	// Pre-populate long history to set up the context overflow path
	key := "test:images-overflow"
	now := time.Now().UnixMilli()
	var msgs []session.Message
	for i := range 5 {
		msgs = append(msgs, session.Message{Role: "user", Content: fmt.Sprintf("q%d", i), TS: now})
		msgs = append(msgs, session.Message{Role: "assistant", Content: fmt.Sprintf("a%d", i), TS: now})
	}
	_ = sm.AppendMessages(key, msgs)

	// First call: context overflow error, second call: success
	callN := 0
	errProvider := &contextOverflowProvider{callN: &callN}
	providers := map[string]models.Provider{"test": errProvider}
	router := models.NewRouter(testLogger(), providers, "test/test-model", nil)

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)

	resp, err := ag.ChatWithImages(context.Background(), key, "describe image", []string{"data:image/png;base64,abc"})
	if err != nil {
		t.Fatalf("ChatWithImages: %v", err)
	}
	if resp.Text != "recovered response" {
		t.Errorf("expected 'recovered response', got %q", resp.Text)
	}
}

// contextOverflowProvider returns a context overflow error on the first call,
// then succeeds on subsequent calls.
type contextOverflowProvider struct {
	callN *int
}

func (p *contextOverflowProvider) Chat(_ context.Context, _ openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	*p.callN++
	if *p.callN == 1 {
		return openai.ChatCompletionResponse{}, fmt.Errorf("context_length_exceeded: prompt too long")
	}
	return openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{{
			Message: openai.ChatCompletionMessage{
				Role:    "assistant",
				Content: "recovered response",
			},
		}},
	}, nil
}

func (p *contextOverflowProvider) ChatStream(_ context.Context, _ openai.ChatCompletionRequest) (models.Stream, error) {
	return nil, fmt.Errorf("not implemented")
}

// ---------------------------------------------------------------------------
// mergeSummaries — cover compaction.go:333
// ---------------------------------------------------------------------------

func TestMergeSummaries(t *testing.T) {
	mock := &mockRouter{
		responses: []openai.ChatCompletionResponse{{
			Choices: []openai.ChatCompletionChoice{{
				Message: openai.ChatCompletionMessage{
					Role:    "assistant",
					Content: "Merged summary of all segments.",
				},
			}},
		}},
	}

	summaries := []string{
		"Summary of segment 1: user asked about Go.",
		"Summary of segment 2: discussed testing strategies.",
	}

	result, err := mergeSummaries(context.Background(), mock, "test-model", summaries)
	if err != nil {
		t.Fatalf("mergeSummaries: %v", err)
	}
	if result != "Merged summary of all segments." {
		t.Errorf("expected merged summary, got %q", result)
	}
}

func TestMergeSummariesError(t *testing.T) {
	mock := &mockRouter{responses: nil} // will error

	_, err := mergeSummaries(context.Background(), mock, "test-model", []string{"a", "b"})
	if err == nil {
		t.Error("expected error from mergeSummaries when router fails")
	}
}

func TestMergeSummariesEmptyChoices(t *testing.T) {
	mock := &mockRouter{
		responses: []openai.ChatCompletionResponse{{
			Choices: []openai.ChatCompletionChoice{}, // empty
		}},
	}

	_, err := mergeSummaries(context.Background(), mock, "test-model", []string{"a", "b"})
	if err == nil {
		t.Error("expected error from mergeSummaries with empty choices")
	}
	if !strings.Contains(err.Error(), "empty response") {
		t.Errorf("expected 'empty response' error, got %q", err.Error())
	}
}

// ---------------------------------------------------------------------------
// ContextWindowError.Error() — cover compaction.go:365
// ---------------------------------------------------------------------------

func TestContextWindowErrorString(t *testing.T) {
	e := &ContextWindowError{ModelTokens: 8000, Minimum: 16000}
	msg := e.Error()
	if !strings.Contains(msg, "8000") || !strings.Contains(msg, "16000") {
		t.Errorf("unexpected error message: %q", msg)
	}
}

// ---------------------------------------------------------------------------
// NewToolLoopDetector — cover defaults for all zero fields (loopdetect.go:92)
// ---------------------------------------------------------------------------

func TestNewToolLoopDetectorDefaults(t *testing.T) {
	tld := NewToolLoopDetector(ToolLoopDetectionConfig{})

	if tld.cfg.HistorySize != 30 {
		t.Errorf("expected default HistorySize 30, got %d", tld.cfg.HistorySize)
	}
	if tld.cfg.WarningThreshold != 10 {
		t.Errorf("expected default WarningThreshold 10, got %d", tld.cfg.WarningThreshold)
	}
	if tld.cfg.CriticalThreshold != 20 {
		t.Errorf("expected default CriticalThreshold 20, got %d", tld.cfg.CriticalThreshold)
	}
	if tld.cfg.GlobalCircuitBreakerThreshold != 30 {
		t.Errorf("expected default GlobalCircuitBreakerThreshold 30, got %d", tld.cfg.GlobalCircuitBreakerThreshold)
	}
}

func TestNewToolLoopDetectorCustomValues(t *testing.T) {
	tld := NewToolLoopDetector(ToolLoopDetectionConfig{
		HistorySize:                   50,
		WarningThreshold:              5,
		CriticalThreshold:             10,
		GlobalCircuitBreakerThreshold: 15,
	})

	if tld.cfg.HistorySize != 50 {
		t.Errorf("expected HistorySize 50, got %d", tld.cfg.HistorySize)
	}
	if tld.cfg.WarningThreshold != 5 {
		t.Errorf("expected WarningThreshold 5, got %d", tld.cfg.WarningThreshold)
	}
}

// ---------------------------------------------------------------------------
// buildSystemPrompt — cover eidetic integration paths (prompt.go:102)
// ---------------------------------------------------------------------------

func TestBuildSystemPromptWithEidetic(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.UserTimezone = "UTC"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, "", nil)

	// Set eidetic client to activate the eidetic rules block
	ec := &mockEideticClient{
		recentResult: []eidetic.MemoryEntry{
			{
				Content:   "User likes Go",
				Timestamp: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
			},
		},
	}
	ag.SetEidetic(ec)

	prompt, entries := ag.buildSystemPrompt()

	// Should contain eidetic-specific rules
	if !strings.Contains(prompt, "eidetic_search") {
		t.Error("expected eidetic search instruction in prompt")
	}

	// Should contain recent memory entries
	if !strings.Contains(prompt, "## Recent Memory") {
		t.Error("expected Recent Memory section")
	}
	if !strings.Contains(prompt, "User likes Go") {
		t.Error("expected recent memory content in prompt")
	}

	if len(entries) != 1 {
		t.Errorf("expected 1 recent entry, got %d", len(entries))
	}
}

func TestBuildSystemPromptEideticNoEntries(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, "", nil)

	ec := &mockEideticClient{recentResult: nil}
	ag.SetEidetic(ec)

	prompt, entries := ag.buildSystemPrompt()
	// Should have eidetic rules but no Recent Memory section
	if !strings.Contains(prompt, "eidetic_search") {
		t.Error("expected eidetic instruction even with no recent entries")
	}
	if strings.Contains(prompt, "## Recent Memory") {
		t.Error("should not have Recent Memory section with no entries")
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

// ---------------------------------------------------------------------------
// agentLoop — cover multi-detector loop detection and context overflow mid-loop
// ---------------------------------------------------------------------------

func TestAgentLoopMultiDetectorLoopDetection(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	cfg.Agents.Defaults.ToolLoopDetection = config.ToolLoopDetectionConfig{
		Enabled:                       true,
		HistorySize:                   30,
		WarningThreshold:              2,
		CriticalThreshold:             3,
		GlobalCircuitBreakerThreshold: 30,
	}
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	mt := &mockTool{name: "repeat", result: "same"}

	sameToolCall := openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{{
			Message: openai.ChatCompletionMessage{
				Role: "assistant",
				ToolCalls: []openai.ToolCall{{
					ID:   "tc_multi",
					Type: openai.ToolTypeFunction,
					Function: openai.FunctionCall{
						Name:      "repeat",
						Arguments: `{"x":"same"}`,
					},
				}},
			},
		}},
	}

	router := makeRouter(sameToolCall, sameToolCall, sameToolCall, sameToolCall)
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", []Tool{mt})

	resp, err := ag.Chat(context.Background(), "test:multi-detector", "go")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !resp.Stopped {
		t.Error("expected multi-detector to stop the conversation")
	}
	if !strings.Contains(resp.Text, "Loop detected") {
		t.Errorf("expected 'Loop detected' in text, got %q", resp.Text)
	}
}

func TestAgentLoopContextOverflowMidLoop(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	cfg.Agents.Defaults.ContextPruning.KeepLastAssistants = 1
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	// Call sequence:
	// 1st: tool call (agentLoop iter 0)
	// 2nd: context overflow (agentLoop iter 1) → triggers forceSoftTrim
	//      forceSoftTrim won't have enough assistants to summarize (returns as-is)
	// 3rd: retry after compact — final text response
	callN := 0
	fp := &midLoopOverflowProvider{
		callN: &callN,
		responses: []openai.ChatCompletionResponse{
			// 1st: tool call
			{Choices: []openai.ChatCompletionChoice{{
				Message: openai.ChatCompletionMessage{
					Role: "assistant",
					ToolCalls: []openai.ToolCall{{
						ID: "tc1", Type: openai.ToolTypeFunction,
						Function: openai.FunctionCall{Name: "check", Arguments: `{}`},
					}},
				},
			}}},
			// 2nd: context overflow (returned as error, not used)
			{},
			// 3rd: retry after compact — final text
			{Choices: []openai.ChatCompletionChoice{{
				Message: openai.ChatCompletionMessage{Role: "assistant", Content: "Recovered after overflow."},
			}}},
		},
	}
	providers := map[string]models.Provider{"test": fp}
	router := models.NewRouter(testLogger(), providers, "test/test-model", nil)

	mt := &mockTool{name: "check", result: "ok"}
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", []Tool{mt})

	resp, err := ag.Chat(context.Background(), "test:mid-loop-overflow", "go")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	// The overflow recovery compacts and retries; final response should be present
	if !strings.Contains(resp.Text, "Recovered after overflow.") {
		t.Errorf("expected 'Recovered after overflow.' in response, got %q", resp.Text)
	}
}

type midLoopOverflowProvider struct {
	callN     *int
	responses []openai.ChatCompletionResponse
}

func (p *midLoopOverflowProvider) Chat(_ context.Context, _ openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	idx := *p.callN
	*p.callN++
	if idx >= len(p.responses) {
		return openai.ChatCompletionResponse{}, fmt.Errorf("no more responses")
	}
	// 2nd call is context overflow
	if idx == 1 {
		return openai.ChatCompletionResponse{}, fmt.Errorf("context_length_exceeded")
	}
	return p.responses[idx], nil
}

func (p *midLoopOverflowProvider) ChatStream(_ context.Context, _ openai.ChatCompletionRequest) (models.Stream, error) {
	return nil, fmt.Errorf("not implemented")
}

// ---------------------------------------------------------------------------
// agentLoop — cover OnToolStart/OnToolDone/OnIterationText callbacks
// ---------------------------------------------------------------------------

func TestAgentLoopStreamCallbacks(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	mt := &mockTool{name: "greet", result: "hello!"}

	router := makeRouter(
		// First: tool call with some text content
		openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{
				Message: openai.ChatCompletionMessage{
					Role:    "assistant",
					Content: "Let me greet you.",
					ToolCalls: []openai.ToolCall{{
						ID:   "tc1",
						Type: openai.ToolTypeFunction,
						Function: openai.FunctionCall{
							Name:      "greet",
							Arguments: `{"name":"user"}`,
						},
					}},
				},
			}},
		},
		// Second: final text
		openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{
				Message: openai.ChatCompletionMessage{
					Role:    "assistant",
					Content: "Done!",
				},
			}},
		},
	)

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", []Tool{mt})

	var toolStarts []string
	var toolDones []string
	var iterTexts []string

	cb := &StreamCallbacks{
		OnToolStart: func(name, args string) {
			toolStarts = append(toolStarts, name)
		},
		OnToolDone: func(name, result string, err error) {
			toolDones = append(toolDones, name)
		},
		OnIterationText: func(text string) {
			iterTexts = append(iterTexts, text)
		},
	}

	msgs := []openai.ChatCompletionMessage{{Role: "user", Content: "hi"}}
	resp, _, err := ag.agentLoop(context.Background(), "test:callbacks", "sys", msgs, false, ag.fullModelCaller(), cb)
	if err != nil {
		t.Fatalf("agentLoop: %v", err)
	}
	if !strings.Contains(resp.Text, "Done!") {
		t.Errorf("expected 'Done!' in response, got %q", resp.Text)
	}
	if len(toolStarts) != 1 || toolStarts[0] != "greet" {
		t.Errorf("expected OnToolStart for 'greet', got %v", toolStarts)
	}
	if len(toolDones) != 1 || toolDones[0] != "greet" {
		t.Errorf("expected OnToolDone for 'greet', got %v", toolDones)
	}
	if len(iterTexts) != 1 || iterTexts[0] != "Let me greet you." {
		t.Errorf("expected OnIterationText 'Let me greet you.', got %v", iterTexts)
	}
}

// ---------------------------------------------------------------------------
// agentLoop — cover max iterations reached (no delegate tool)
// ---------------------------------------------------------------------------

func TestAgentLoopMaxIterations(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	cfg.Agents.Defaults.MaxIterations = 3
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	mt := &mockTool{name: "looper", result: "ok"}

	loopResp := openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{{
			Message: openai.ChatCompletionMessage{
				Role: "assistant",
				ToolCalls: []openai.ToolCall{{
					ID:   "tc_loop",
					Type: openai.ToolTypeFunction,
					Function: openai.FunctionCall{
						Name:      "looper",
						Arguments: `{"i":1}`,
					},
				}},
			},
		}},
	}
	// Different arguments on each call to avoid loop detection
	loopResp2 := openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{{
			Message: openai.ChatCompletionMessage{
				Role: "assistant",
				ToolCalls: []openai.ToolCall{{
					ID:   "tc_loop2",
					Type: openai.ToolTypeFunction,
					Function: openai.FunctionCall{
						Name:      "looper",
						Arguments: `{"i":2}`,
					},
				}},
			},
		}},
	}
	loopResp3 := openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{{
			Message: openai.ChatCompletionMessage{
				Role: "assistant",
				ToolCalls: []openai.ToolCall{{
					ID:   "tc_loop3",
					Type: openai.ToolTypeFunction,
					Function: openai.FunctionCall{
						Name:      "looper",
						Arguments: `{"i":3}`,
					},
				}},
			},
		}},
	}

	router := makeRouter(loopResp, loopResp2, loopResp3)
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", []Tool{mt})

	resp, err := ag.Chat(context.Background(), "test:max-iter", "go")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !resp.Stopped {
		t.Error("expected Stopped=true when max iterations reached")
	}
	if !strings.Contains(resp.Text, "Maximum iterations reached") {
		t.Errorf("expected 'Maximum iterations reached', got %q", resp.Text)
	}
}

// ---------------------------------------------------------------------------
// chatImpl — cover context overflow auto-recovery in chatImpl
// ---------------------------------------------------------------------------

func TestChatImplContextOverflowRetry(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	// Pre-populate history
	key := "test:overflow-retry"
	now := time.Now().UnixMilli()
	_ = sm.AppendMessages(key, []session.Message{
		{Role: "user", Content: "old q", TS: now},
		{Role: "assistant", Content: "old a", TS: now},
	})

	callN := 0
	fp := &contextOverflowProvider{callN: &callN}
	providers := map[string]models.Provider{"test": fp}
	router := models.NewRouter(testLogger(), providers, "test/test-model", nil)

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)

	resp, err := ag.Chat(context.Background(), key, "new question")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Text != "recovered response" {
		t.Errorf("expected 'recovered response', got %q", resp.Text)
	}
}

// ---------------------------------------------------------------------------
// loopdetect.go — stableJSON / sortKeys edge cases
// ---------------------------------------------------------------------------

func TestStableJSONInvalidJSON(t *testing.T) {
	// Invalid JSON should return the raw string unchanged
	raw := "not valid json"
	result := stableJSON(raw)
	if result != raw {
		t.Errorf("expected raw string back for invalid JSON, got %q", result)
	}
}

func TestStableJSONWithArray(t *testing.T) {
	raw := `[3,1,2]`
	result := stableJSON(raw)
	if result != `[3,1,2]` {
		t.Errorf("expected array to be preserved, got %q", result)
	}
}

func TestStableJSONWithNestedMaps(t *testing.T) {
	raw := `{"b":"2","a":{"d":"4","c":"3"}}`
	result := stableJSON(raw)
	// Keys should be sorted
	if !strings.Contains(result, `"a"`) || !strings.Contains(result, `"b"`) {
		t.Errorf("unexpected result: %q", result)
	}
}

// ---------------------------------------------------------------------------
// PollBackoff — cover prune and suggestDelay edge cases
// ---------------------------------------------------------------------------

func TestPollBackoffSuggestDelayBounds(t *testing.T) {
	pb := NewPollBackoff()

	// Record many polls with no output
	for range 10 {
		pb.Record("cmd1", false)
	}

	// Delay should be capped at 60s
	d := pb.Record("cmd1", false)
	if d != 60*time.Second {
		t.Errorf("expected max delay 60s, got %v", d)
	}

	// Reset on new output
	d = pb.Record("cmd1", true)
	if d != 0 {
		t.Errorf("expected 0 delay after new output, got %v", d)
	}
}

// ---------------------------------------------------------------------------
// UpdateConfig
// ---------------------------------------------------------------------------

func TestUpdateConfigCoverage(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	router := makeRouter()
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)

	newCfg := newTestConfig()
	newCfg.Agents.Defaults.Model.Primary = "test/new-model"
	ag.UpdateConfig(newCfg)

	got := ag.GetConfig()
	if got.Agents.Defaults.Model.Primary != "test/new-model" {
		t.Errorf("expected updated model, got %q", got.Agents.Defaults.Model.Primary)
	}
}

// ---------------------------------------------------------------------------
// buildLightSystemPrompt — cover heartbeat path
// ---------------------------------------------------------------------------

func TestBuildLightSystemPromptNoWorkspace(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, "", nil)

	prompt := ag.buildLightSystemPrompt()
	if !strings.Contains(prompt, "Test") {
		t.Error("light prompt should contain identity name")
	}
	if !strings.Contains(prompt, "Current date/time:") {
		t.Error("light prompt should contain date/time")
	}
}

// ---------------------------------------------------------------------------
// emitHook — cover the hook firing path
// ---------------------------------------------------------------------------

func TestEmitHookNoOp(t *testing.T) {
	cfg := newTestConfig()
	ag := &Agent{
		cfg:    cfg,
		def:    cfg.DefaultAgent(),
		logger: zap.NewNop().Sugar(),
		Hooks:  nil, // no hooks bus
	}
	// Should not panic with nil Hooks
	ag.emitHook(context.Background(), hooks.Event{Type: hooks.BeforePromptBuild})
}

func TestEmitHookWithBus(t *testing.T) {
	cfg := newTestConfig()
	ag := &Agent{
		cfg:    cfg,
		def:    cfg.DefaultAgent(),
		logger: zap.NewNop().Sugar(),
		Hooks:  hooks.New(zap.NewNop().Sugar()),
	}
	// Should set AgentID from def when empty
	ag.emitHook(context.Background(), hooks.Event{Type: hooks.BeforePromptBuild})
}

// ---------------------------------------------------------------------------
// CompactHistory — cover multi-chunk merge path
// ---------------------------------------------------------------------------

func TestCompactHistoryMultiChunkMerge(t *testing.T) {
	// Create enough messages to trigger multiple chunks
	var msgs []session.Message
	now := time.Now().UnixMilli()
	for i := range 20 {
		msgs = append(msgs, session.Message{
			Role:    "user",
			Content: fmt.Sprintf("Long question %d with lots of content to fill up the token budget and force chunking into multiple segments for summarization", i),
			TS:      now,
		})
		msgs = append(msgs, session.Message{
			Role:    "assistant",
			Content: fmt.Sprintf("Long answer %d with lots of content to fill up the token budget and force chunking into multiple segments for summarization too", i),
			TS:      now,
		})
	}

	mock := &mockRouter{
		responses: []openai.ChatCompletionResponse{
			// Chunk 1 summary
			{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Role: "assistant", Content: "Summary chunk 1."}}}},
			// Chunk 2 summary
			{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Role: "assistant", Content: "Summary chunk 2."}}}},
			// Chunk 3 summary (in case there are 3 chunks)
			{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Role: "assistant", Content: "Summary chunk 3."}}}},
			// More chunks in case needed
			{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Role: "assistant", Content: "Summary chunk 4."}}}},
			{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Role: "assistant", Content: "Summary chunk 5."}}}},
			// Merge summary
			{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Role: "assistant", Content: "Final merged summary."}}}},
		},
	}

	result, err := CompactHistory(context.Background(), testLogger(), mock, "test-model", msgs, 128000, 2)
	if err != nil {
		t.Fatalf("CompactHistory: %v", err)
	}
	if len(result) >= len(msgs) {
		t.Errorf("expected compacted result < %d, got %d", len(msgs), len(result))
	}
	if len(result) > 0 && !strings.Contains(result[0].Content, "Compacted") {
		t.Errorf("expected compacted summary, got %q", result[0].Content)
	}
}

// ---------------------------------------------------------------------------
// agentLoop — cover the soft warning nudge (80% of max iterations)
// ---------------------------------------------------------------------------

func TestAgentLoopSoftWarningNudge(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	cfg.Agents.Defaults.MaxIterations = 5 // softWarnAt = 4
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	mt := &mockTool{name: "work", result: "ok"}

	var responses []openai.ChatCompletionResponse
	for i := range 5 {
		responses = append(responses, openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{
				Message: openai.ChatCompletionMessage{
					Role: "assistant",
					ToolCalls: []openai.ToolCall{{
						ID:   fmt.Sprintf("tc_%d", i),
						Type: openai.ToolTypeFunction,
						Function: openai.FunctionCall{
							Name:      "work",
							Arguments: fmt.Sprintf(`{"step":%d}`, i),
						},
					}},
				},
			}},
		})
	}

	router := makeRouter(responses...)
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", []Tool{mt})

	resp, err := ag.Chat(context.Background(), "test:soft-warn", "go")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	// Should hit max iterations
	if !resp.Stopped {
		t.Error("expected Stopped=true")
	}
}
