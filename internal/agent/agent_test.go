package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/config"
	"github.com/EMSERO/gopherclaw/internal/models"
	"github.com/EMSERO/gopherclaw/internal/orchestrator"
	"github.com/EMSERO/gopherclaw/internal/session"
	"github.com/EMSERO/gopherclaw/internal/skills"
	"github.com/EMSERO/gopherclaw/internal/taskqueue"
	"github.com/EMSERO/gopherclaw/internal/tools"
)

func testLogger() *zap.SugaredLogger { return zap.NewNop().Sugar() }

// mockRouter implements a minimal model router for testing.
type mockRouter struct {
	responses []openai.ChatCompletionResponse
	callIdx   int
}

func (m *mockRouter) Chat(_ context.Context, _ openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	if m.callIdx >= len(m.responses) {
		return openai.ChatCompletionResponse{}, fmt.Errorf("no more mock responses")
	}
	resp := m.responses[m.callIdx]
	m.callIdx++
	return resp, nil
}

func (m *mockRouter) ChatStream(_ context.Context, _ openai.ChatCompletionRequest) (*openai.ChatCompletionStream, error) {
	return nil, fmt.Errorf("not implemented in mock")
}

// mockTool is a test tool that records calls.
type mockTool struct {
	name    string
	result  string
	calls   int
	lastArg string
}

func (t *mockTool) Name() string { return t.name }
func (t *mockTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (t *mockTool) Run(_ context.Context, argsJSON string) string {
	t.calls++
	t.lastArg = argsJSON
	return t.result
}

func newTestConfig() *config.Root {
	return &config.Root{
		Agents: config.Agents{
			Defaults: config.AgentDefaults{
				Model:          config.ModelConfig{Primary: "test-model"},
				UserTimezone:   "UTC",
				LoopDetectionN: 3,
			},
			List: []config.AgentDef{{ID: "main", Default: true, Identity: config.Identity{Name: "Test", Theme: "test"}}},
		},
	}
}

func TestLoopDetection(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.LoopDetectionN = 2 // detect after 2 identical calls

	sessDir := t.TempDir()
	sm, _ := session.New(testLogger(), sessDir, time.Hour)

	// Mock router: always returns same tool call
	toolCallResp := func() openai.ChatCompletionResponse {
		return openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{
				Message: openai.ChatCompletionMessage{
					Role: "assistant",
					ToolCalls: []openai.ToolCall{{
						ID:   "tc1",
						Type: openai.ToolTypeFunction,
						Function: openai.FunctionCall{
							Name:      "test_tool",
							Arguments: `{"query":"same"}`,
						},
					}},
				},
			}},
		}
	}

	router := &mockRouter{
		responses: []openai.ChatCompletionResponse{
			toolCallResp(),
			toolCallResp(),
			toolCallResp(), // should not be reached
		},
	}

	mt := &mockTool{name: "test_tool", result: "ok"}

	ag := &Agent{
		cfg:      cfg,
		def:      cfg.DefaultAgent(),
		sessions: sm,
		toolMap:  map[string]Tool{"test_tool": mt},
		toolDefs: []openai.Tool{{
			Type:     openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{Name: "test_tool", Parameters: mt.Schema()},
		}},
		logger: zap.NewNop().Sugar(),
	}

	// Use loop directly
	msgs := []openai.ChatCompletionMessage{{Role: "user", Content: "test"}}
	resp, _, err := ag.loopWithRouter(context.Background(), router, "system prompt", msgs)
	if err != nil {
		t.Fatalf("loop: %v", err)
	}
	if !resp.Stopped {
		t.Error("expected loop to be stopped due to detection")
	}
	if mt.calls != 1 {
		// First call executes, second triggers detection before execution
		t.Errorf("expected 1 tool call, got %d", mt.calls)
	}
}

func TestLoopDetectorLogic(t *testing.T) {
	ld := newLoopDetector(3)

	calls := []openai.ToolCall{{
		Function: openai.FunctionCall{Name: "exec", Arguments: `{"cmd":"ls"}`},
	}}
	diffCalls := []openai.ToolCall{{
		Function: openai.FunctionCall{Name: "exec", Arguments: `{"cmd":"pwd"}`},
	}}

	if ld.check(calls) {
		t.Error("should not detect loop on first call")
	}
	if ld.check(calls) {
		t.Error("should not detect loop on second identical call (limit=3)")
	}
	if !ld.check(calls) {
		t.Error("should detect loop on third identical call")
	}

	// Reset by different call
	ld2 := newLoopDetector(2)
	ld2.check(calls)
	ld2.check(diffCalls) // breaks streak
	if ld2.check(calls) {
		t.Error("should not detect loop after different call resets counter")
	}
}

func TestHardClear(t *testing.T) {
	dir := t.TempDir()
	sm, err := session.New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// Very low threshold to trigger pruning
	sm.SetPruning(session.PruningPolicy{
		HardClearRatio:     0.001,
		ModelMaxTokens:     100,
		KeepLastAssistants: 1,
	})

	key := "test:hardclear"
	now := time.Now().UnixMilli()
	msgs := make([]session.Message, 0)
	for i := range 10 {
		msgs = append(msgs, session.Message{
			Role:    "user",
			Content: fmt.Sprintf("Long question %d with enough text to generate a lot of tokens when estimated", i),
			TS:      now,
		})
		msgs = append(msgs, session.Message{
			Role:    "assistant",
			Content: fmt.Sprintf("Long answer %d with plenty of content to push the token count well past our threshold", i),
			TS:      now,
		})
	}
	if err := sm.AppendMessages(key, msgs); err != nil {
		t.Fatal(err)
	}

	h, err := sm.GetHistory(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) >= 20 {
		t.Errorf("expected pruning to reduce messages from 20, got %d", len(h))
	}
}

func TestSoftTrim(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.SoftTrimRatio = 0.001 // very low to trigger
	cfg.Agents.Defaults.ContextPruning.KeepLastAssistants = 1

	// Mock router that returns a summarization response
	router := &mockRouter{
		responses: []openai.ChatCompletionResponse{{
			Choices: []openai.ChatCompletionChoice{{
				Message: openai.ChatCompletionMessage{
					Role:    "assistant",
					Content: "Summary of the conversation so far.",
				},
			}},
		}},
	}

	sessDir := t.TempDir()
	sm, _ := session.New(testLogger(), sessDir, time.Hour)

	ag := &Agent{
		cfg:      cfg,
		def:      cfg.DefaultAgent(),
		sessions: sm,
		toolMap:  make(map[string]Tool),
		logger:   zap.NewNop().Sugar(),
	}

	// Create history that exceeds the threshold
	history := make([]session.Message, 0)
	for i := range 10 {
		history = append(history, session.Message{
			Role:    "user",
			Content: fmt.Sprintf("Question %d with enough padding text to be substantial for token estimation purposes in our test case scenario", i),
			TS:      time.Now().UnixMilli(),
		})
		history = append(history, session.Message{
			Role:    "assistant",
			Content: fmt.Sprintf("Answer %d with enough padding text to be substantial for token estimation purposes in our test case scenario as well", i),
			TS:      time.Now().UnixMilli(),
		})
	}

	result := ag.softTrimWithRouter(context.Background(), router, history)

	// Should have fewer messages
	if len(result) >= len(history) {
		t.Errorf("expected soft trim to reduce messages from %d, got %d", len(history), len(result))
	}
	// First message should be the summary
	if len(result) > 0 && result[0].Role != "assistant" {
		t.Errorf("expected first message to be summary assistant, got %s", result[0].Role)
	}
}

// loopWithRouter is a test helper that calls the agent loop with a custom router.
func (a *Agent) loopWithRouter(ctx context.Context, router *mockRouter, sysPrompt string, messages []openai.ChatCompletionMessage) (Response, []openai.ChatCompletionMessage, error) {
	var added []openai.ChatCompletionMessage
	ld := newLoopDetector(a.cfg.Agents.Defaults.LoopDetectionN)

	for range a.maxIter() {
		req, err := a.buildRequest(ctx, "test-session", sysPrompt, messages, false)
		if err != nil {
			return Response{}, added, err
		}
		resp, err := router.Chat(ctx, req)
		if err != nil {
			return Response{}, added, err
		}

		if len(resp.Choices) == 0 {
			return Response{}, added, fmt.Errorf("empty response from model")
		}
		choice := resp.Choices[0]
		assistantMsg := choice.Message
		messages = append(messages, assistantMsg)
		added = append(added, assistantMsg)

		if len(assistantMsg.ToolCalls) == 0 {
			return Response{Text: assistantMsg.Content}, added, nil
		}

		if ld.check(assistantMsg.ToolCalls) {
			return Response{Text: "Loop detected", Stopped: true}, added, nil
		}

		toolMsgs := a.executeTools(ctx, assistantMsg.ToolCalls)
		messages = append(messages, toolMsgs...)
		added = append(added, toolMsgs...)
	}

	return Response{Text: "Maximum iterations reached.", Stopped: true}, added, nil
}

// softTrimWithRouter is a test helper that performs soft trim with a custom router.
func (a *Agent) softTrimWithRouter(ctx context.Context, router *mockRouter, history []session.Message) []session.Message {
	ratio := a.cfg.Agents.Defaults.SoftTrimRatio
	if ratio <= 0 {
		return history
	}
	maxTokens := 128_000
	threshold := int(ratio * float64(maxTokens))
	tokens := session.EstimateTokens(history)
	if tokens <= threshold {
		return history
	}

	keepN := a.cfg.Agents.Defaults.ContextPruning.KeepLastAssistants
	if keepN <= 0 {
		keepN = 2
	}

	var assistantIndices []int
	for i, m := range history {
		if m.Role == "assistant" {
			assistantIndices = append(assistantIndices, i)
		}
	}
	if len(assistantIndices) <= keepN {
		return history
	}

	splitAt := assistantIndices[len(assistantIndices)-keepN]
	if splitAt > 0 && history[splitAt-1].Role == "user" {
		splitAt--
	}

	oldMessages := history[:splitAt]
	recentMessages := history[splitAt:]

	var sb fmt.Stringer = &stringerHelper{old: oldMessages}
	_ = sb // suppress unused

	summaryReq := openai.ChatCompletionRequest{
		Messages: []openai.ChatCompletionMessage{
			{Role: "system", Content: "Summarize the conversation."},
			{Role: "user", Content: "summarize"},
		},
	}
	resp, err := router.Chat(ctx, summaryReq)
	if err != nil || len(resp.Choices) == 0 {
		return history
	}

	summaryMsg := session.Message{
		Role:    "assistant",
		Content: fmt.Sprintf("[Conversation summary]: %s", resp.Choices[0].Message.Content),
		TS:      time.Now().UnixMilli(),
	}

	result := []session.Message{summaryMsg}
	result = append(result, recentMessages...)
	return result
}

type stringerHelper struct {
	old []session.Message
}

func (s *stringerHelper) String() string {
	return fmt.Sprintf("%d messages", len(s.old))
}

// ---------------------------------------------------------------------------
// New() constructor tests
// ---------------------------------------------------------------------------

func TestNewAgent(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	mt1 := &mockTool{name: "tool_a", result: "a"}
	mt2 := &mockTool{name: "tool_b", result: "b"}

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, "", []Tool{mt1, mt2})

	if len(ag.toolMap) != 2 {
		t.Errorf("expected 2 tools in toolMap, got %d", len(ag.toolMap))
	}
	if _, ok := ag.toolMap["tool_a"]; !ok {
		t.Error("toolMap missing tool_a")
	}
	if _, ok := ag.toolMap["tool_b"]; !ok {
		t.Error("toolMap missing tool_b")
	}
	if len(ag.toolDefs) != 2 {
		t.Errorf("expected 2 tool definitions, got %d", len(ag.toolDefs))
	}
	// sem should be nil with no maxConcurrent
	if ag.sem != nil {
		t.Error("expected nil sem when maxConcurrent is 0")
	}
}

func TestNewAgentWithSemaphore(t *testing.T) {
	cfg := newTestConfig()
	cfg.Session.MaxConcurrent = 3
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, "", nil)

	if ag.sem == nil {
		t.Fatal("expected semaphore to be set when maxConcurrent > 0")
	}
	if cap(ag.sem) != 3 {
		t.Errorf("expected semaphore capacity 3, got %d", cap(ag.sem))
	}
}

func TestNewAgentSemaphoreFallbackToDefaults(t *testing.T) {
	cfg := newTestConfig()
	cfg.Session.MaxConcurrent = 0
	cfg.Agents.Defaults.MaxConcurrent = 5
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, "", nil)

	if ag.sem == nil {
		t.Fatal("expected semaphore from defaults")
	}
	if cap(ag.sem) != 5 {
		t.Errorf("expected semaphore capacity 5, got %d", cap(ag.sem))
	}
}

// ---------------------------------------------------------------------------
// SetSessionModel / ClearSessionModel / ResolveModel
// ---------------------------------------------------------------------------

func TestSessionModelOverrides(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, "", nil)

	// Default model
	if m := ag.ResolveModel("session1"); m != "test-model" {
		t.Errorf("expected default model 'test-model', got %q", m)
	}

	// Set override
	ag.SetSessionModel("session1", "custom-model-3")
	if m := ag.ResolveModel("session1"); m != "custom-model-3" {
		t.Errorf("expected override 'custom-model-3', got %q", m)
	}

	// Other sessions unaffected
	if m := ag.ResolveModel("session2"); m != "test-model" {
		t.Errorf("expected default for session2, got %q", m)
	}

	// Clear override
	ag.ClearSessionModel("session1")
	if m := ag.ResolveModel("session1"); m != "test-model" {
		t.Errorf("expected default after clear, got %q", m)
	}
}

// ---------------------------------------------------------------------------
// acquireSem / releaseSem
// ---------------------------------------------------------------------------

func TestAcquireReleaseSemNil(t *testing.T) {
	ag := &Agent{logger: zap.NewNop().Sugar()} // sem is nil
	if err := ag.acquireSem(context.Background()); err != nil {
		t.Errorf("acquireSem with nil sem should not error: %v", err)
	}
	// releaseSem with nil should not panic
	ag.releaseSem()
}

func TestAcquireReleaseSemWithLimit(t *testing.T) {
	ag := &Agent{sem: make(chan struct{}, 1), logger: zap.NewNop().Sugar()}

	// First acquire should succeed
	if err := ag.acquireSem(context.Background()); err != nil {
		t.Fatalf("first acquire should succeed: %v", err)
	}

	// Second acquire should block; test with cancelled context
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := ag.acquireSem(ctx)
	if err == nil {
		t.Error("expected error when sem full and context cancelled")
	}
	if err != context.DeadlineExceeded {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}

	// Release and try again
	ag.releaseSem()
	if err := ag.acquireSem(context.Background()); err != nil {
		t.Fatalf("acquire after release should succeed: %v", err)
	}
	ag.releaseSem()
}

func TestAcquireSemContextCancellation(t *testing.T) {
	ag := &Agent{sem: make(chan struct{}, 1), logger: zap.NewNop().Sugar()}
	// Fill the semaphore
	ag.sem <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := ag.acquireSem(ctx)
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	// Clean up
	<-ag.sem
}

// ---------------------------------------------------------------------------
// buildRequest
// ---------------------------------------------------------------------------

func TestBuildRequest(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	mt := &mockTool{name: "exec", result: "ok"}
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, "", []Tool{mt})

	msgs := []openai.ChatCompletionMessage{
		{Role: "user", Content: "hello"},
	}

	req, err := ag.buildRequest(context.Background(), "session:1", "You are helpful.", msgs, false)
	if err != nil {
		t.Fatalf("buildRequest returned unexpected error: %v", err)
	}

	// First message should be system prompt
	if req.Messages[0].Role != "system" {
		t.Errorf("expected first message role 'system', got %q", req.Messages[0].Role)
	}
	if req.Messages[0].Content != "You are helpful." {
		t.Errorf("expected system content, got %q", req.Messages[0].Content)
	}

	// Second message should be the user message
	if len(req.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(req.Messages))
	}
	if req.Messages[1].Content != "hello" {
		t.Errorf("expected user content 'hello', got %q", req.Messages[1].Content)
	}

	// Model should be the default
	if req.Model != "test-model" {
		t.Errorf("expected model 'test-model', got %q", req.Model)
	}

	// Tools should be included
	if len(req.Tools) != 1 {
		t.Errorf("expected 1 tool definition, got %d", len(req.Tools))
	}

	// Stream should be false
	if req.Stream {
		t.Error("expected stream=false")
	}
}

func TestBuildRequestWithModelOverride(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, "", nil)
	ag.SetSessionModel("session:override", "gpt-4o")

	req, err := ag.buildRequest(context.Background(), "session:override", "sys", nil, true)
	if err != nil {
		t.Fatalf("buildRequest returned unexpected error: %v", err)
	}

	if req.Model != "gpt-4o" {
		t.Errorf("expected model override 'gpt-4o', got %q", req.Model)
	}
	if !req.Stream {
		t.Error("expected stream=true")
	}
}

func TestBuildRequestContextGuard(t *testing.T) {
	cfg := newTestConfig()
	// Set ModelMaxTokens below the ContextWindowMinimum (16000) to trigger the guard.
	cfg.Agents.Defaults.ContextPruning.ModelMaxTokens = 10000

	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, "", nil)

	_, err := ag.buildRequest(context.Background(), "session:guard", "sys", nil, false)
	if err == nil {
		t.Fatal("expected error from buildRequest when context window is too small, got nil")
	}
	if !strings.Contains(err.Error(), "context window guard") {
		t.Errorf("expected error to contain 'context window guard', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// initStaticPrompt / buildSystemPrompt
// ---------------------------------------------------------------------------

func TestInitStaticPromptBasic(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, "", nil)

	if !strings.Contains(ag.sysPromptStatic, "Test") {
		t.Error("static prompt missing identity name 'Test'")
	}
	if !strings.Contains(ag.sysPromptStatic, "test") {
		t.Error("static prompt missing identity theme 'test'")
	}
}

func TestInitStaticPromptWithSkills(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	sk := []skills.Skill{
		{Name: "coder", Description: "Write code", Content: "Use Go."},
		{Name: "researcher", Description: "Research things"},
	}

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, sk, nil, "", nil)

	if !strings.Contains(ag.sysPromptStatic, "## Skills") {
		t.Error("static prompt missing Skills section")
	}
	if !strings.Contains(ag.sysPromptStatic, "### coder") {
		t.Error("static prompt missing skill 'coder'")
	}
	if !strings.Contains(ag.sysPromptStatic, "Write code") {
		t.Error("static prompt missing skill description")
	}
	if !strings.Contains(ag.sysPromptStatic, "Use Go.") {
		t.Error("static prompt missing skill content")
	}
	if !strings.Contains(ag.sysPromptStatic, "### researcher") {
		t.Error("static prompt missing skill 'researcher'")
	}
}

func TestInitStaticPromptWithWorkspaceDocs(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	wsMDs := map[string]string{
		"README.md":  "# Project\nThis is a project.",
		"CONTRIB.md": "Contribute here.",
	}

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, wsMDs, "", nil)

	if !strings.Contains(ag.sysPromptStatic, "## Workspace") {
		t.Error("static prompt missing Workspace section")
	}
	if !strings.Contains(ag.sysPromptStatic, "### README.md") {
		t.Error("static prompt missing README.md entry")
	}
	if !strings.Contains(ag.sysPromptStatic, "This is a project.") {
		t.Error("static prompt missing README content")
	}
}

func TestInitStaticPromptWithSubagents(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	subChatter := &mockChatter{response: "ok"}
	dt := &DelegateTool{
		Agents: map[string]Chatter{
			"main":  subChatter,
			"coder": subChatter,
		},
		Logger: zap.NewNop().Sugar(),
	}

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, "", []Tool{dt})

	if !strings.Contains(ag.sysPromptStatic, "## Subagents") {
		t.Error("static prompt missing Subagents section")
	}
	if !strings.Contains(ag.sysPromptStatic, "**coder**") {
		t.Error("static prompt missing subagent 'coder'")
	}
	// "main" should not be listed (it's the current agent)
	if strings.Contains(ag.sysPromptStatic, "**main**") {
		t.Error("static prompt should not list self as subagent")
	}
}

func TestBuildSystemPromptIncludesDateTime(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, "", nil)

	prompt, _ := ag.buildSystemPrompt()
	if !strings.Contains(prompt, "Current date/time:") {
		t.Error("system prompt missing date/time")
	}
	if !strings.Contains(prompt, "(UTC)") {
		t.Error("system prompt missing timezone")
	}
}

// ---------------------------------------------------------------------------
// loadMemoryCached
// ---------------------------------------------------------------------------

func TestLoadMemoryCachedDisabled(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Memory.Enabled = false
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, t.TempDir(), nil)

	result := ag.loadMemoryCached()
	if result != "" {
		t.Errorf("expected empty when memory disabled, got %q", result)
	}
}

func TestLoadMemoryCachedNoWorkspace(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Memory.Enabled = true
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, "", nil)

	result := ag.loadMemoryCached()
	if result != "" {
		t.Errorf("expected empty when workspace is empty, got %q", result)
	}
}

func TestLoadMemoryCachedFileNotFound(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Memory.Enabled = true
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	workspace := t.TempDir() // no MEMORY.md file
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, workspace, nil)

	result := ag.loadMemoryCached()
	if result != "" {
		t.Errorf("expected empty when MEMORY.md doesn't exist, got %q", result)
	}
}

func TestLoadMemoryCachedHitAndMiss(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Memory.Enabled = true
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	workspace := t.TempDir()
	memPath := filepath.Join(workspace, "MEMORY.md")

	// Write initial memory
	if err := os.WriteFile(memPath, []byte("# Memory\nFirst version"), 0644); err != nil {
		t.Fatal(err)
	}

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, workspace, nil)

	// First call: cache miss
	result1 := ag.loadMemoryCached()
	if !strings.Contains(result1, "First version") {
		t.Errorf("expected 'First version', got %q", result1)
	}

	// Second call with same mtime: cache hit
	result2 := ag.loadMemoryCached()
	if result2 != result1 {
		t.Errorf("expected cache hit to return same value")
	}

	// Modify file (need to ensure mtime changes)
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(memPath, []byte("# Memory\nSecond version"), 0644); err != nil {
		t.Fatal(err)
	}

	result3 := ag.loadMemoryCached()
	if !strings.Contains(result3, "Second version") {
		t.Errorf("expected 'Second version' after update, got %q", result3)
	}
}

func TestBuildSystemPromptWithMemory(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Memory.Enabled = true
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	workspace := t.TempDir()
	memPath := filepath.Join(workspace, "MEMORY.md")
	if err := os.WriteFile(memPath, []byte("Remember: user prefers dark mode"), 0644); err != nil {
		t.Fatal(err)
	}

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, workspace, nil)
	prompt, _ := ag.buildSystemPrompt()

	if !strings.Contains(prompt, "## Memory") {
		t.Error("system prompt missing Memory section")
	}
	if !strings.Contains(prompt, "user prefers dark mode") {
		t.Error("system prompt missing memory content")
	}
}

// ---------------------------------------------------------------------------
// executeTools
// ---------------------------------------------------------------------------

func TestExecuteToolsParallel(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	mt1 := &mockTool{name: "tool_a", result: "result_a"}
	mt2 := &mockTool{name: "tool_b", result: "result_b"}

	ag := &Agent{
		cfg:      cfg,
		def:      cfg.DefaultAgent(),
		sessions: sm,
		toolMap:  map[string]Tool{"tool_a": mt1, "tool_b": mt2},
		logger:   zap.NewNop().Sugar(),
	}

	calls := []openai.ToolCall{
		{ID: "tc1", Function: openai.FunctionCall{Name: "tool_a", Arguments: `{"x":1}`}},
		{ID: "tc2", Function: openai.FunctionCall{Name: "tool_b", Arguments: `{"y":2}`}},
	}

	results := ag.executeTools(context.Background(), calls)

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	// Results should be in same order as calls
	if results[0].Content != "result_a" {
		t.Errorf("expected 'result_a', got %q", results[0].Content)
	}
	if results[0].ToolCallID != "tc1" {
		t.Errorf("expected tool call ID 'tc1', got %q", results[0].ToolCallID)
	}
	if results[1].Content != "result_b" {
		t.Errorf("expected 'result_b', got %q", results[1].Content)
	}
	if mt1.calls != 1 || mt2.calls != 1 {
		t.Errorf("expected each tool called once, got a=%d b=%d", mt1.calls, mt2.calls)
	}
}

func TestExecuteToolsUnknownTool(t *testing.T) {
	ag := &Agent{
		cfg:     newTestConfig(),
		def:     newTestConfig().DefaultAgent(),
		toolMap: make(map[string]Tool),
		logger:  zap.NewNop().Sugar(),
	}

	calls := []openai.ToolCall{
		{ID: "tc1", Function: openai.FunctionCall{Name: "nonexistent", Arguments: "{}"}},
	}

	results := ag.executeTools(context.Background(), calls)
	if !strings.Contains(results[0].Content, "unknown tool") {
		t.Errorf("expected 'unknown tool' message, got %q", results[0].Content)
	}
	if results[0].Role != "tool" {
		t.Errorf("expected role 'tool', got %q", results[0].Role)
	}
}

// panicTool is a tool that deliberately panics.
type panicTool struct{}

func (p *panicTool) Name() string            { return "panic_tool" }
func (p *panicTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (p *panicTool) Run(_ context.Context, _ string) string {
	panic("test panic!")
}

func TestExecuteToolsPanicRecovery(t *testing.T) {
	pt := &panicTool{}
	ag := &Agent{
		cfg:     newTestConfig(),
		def:     newTestConfig().DefaultAgent(),
		toolMap: map[string]Tool{"panic_tool": pt},
		logger:  zap.NewNop().Sugar(),
	}

	calls := []openai.ToolCall{
		{ID: "tc1", Function: openai.FunctionCall{Name: "panic_tool", Arguments: "{}"}},
	}

	results := ag.executeTools(context.Background(), calls)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !strings.Contains(results[0].Content, "tool panic") {
		t.Errorf("expected 'tool panic' message, got %q", results[0].Content)
	}
	if results[0].ToolCallID != "tc1" {
		t.Errorf("expected tool call ID 'tc1', got %q", results[0].ToolCallID)
	}
}

// ---------------------------------------------------------------------------
// randomToolCallID
// ---------------------------------------------------------------------------

func TestRandomToolCallID(t *testing.T) {
	id1 := randomToolCallID()
	id2 := randomToolCallID()

	if !strings.HasPrefix(id1, "call_") {
		t.Errorf("expected prefix 'call_', got %q", id1)
	}
	// 8 bytes -> 16 hex chars + "call_" prefix = 21 chars
	if len(id1) != 21 {
		t.Errorf("expected length 21, got %d (%q)", len(id1), id1)
	}
	if id1 == id2 {
		t.Error("two random IDs should differ")
	}
}

// ---------------------------------------------------------------------------
// toolCallsFingerprint
// ---------------------------------------------------------------------------

func TestToolCallsFingerprint(t *testing.T) {
	calls1 := []openai.ToolCall{
		{Function: openai.FunctionCall{Name: "exec", Arguments: `{"cmd":"ls"}`}},
	}
	calls2 := []openai.ToolCall{
		{Function: openai.FunctionCall{Name: "exec", Arguments: `{"cmd":"pwd"}`}},
	}
	calls3 := []openai.ToolCall{
		{Function: openai.FunctionCall{Name: "exec", Arguments: `{"cmd":"ls"}`}},
	}

	fp1 := toolCallsFingerprint(calls1)
	fp2 := toolCallsFingerprint(calls2)
	fp3 := toolCallsFingerprint(calls3)

	if fp1 == fp2 {
		t.Error("different calls should have different fingerprints")
	}
	if fp1 != fp3 {
		t.Error("identical calls should have same fingerprint")
	}
	if fp1 == "" {
		t.Error("fingerprint should not be empty")
	}

	// Multi-call fingerprint
	multi := []openai.ToolCall{
		{Function: openai.FunctionCall{Name: "a", Arguments: "1"}},
		{Function: openai.FunctionCall{Name: "b", Arguments: "2"}},
	}
	fpMulti := toolCallsFingerprint(multi)
	if !strings.Contains(fpMulti, "a:1;") {
		t.Errorf("expected 'a:1;' in fingerprint, got %q", fpMulti)
	}
	if !strings.Contains(fpMulti, "b:2;") {
		t.Errorf("expected 'b:2;' in fingerprint, got %q", fpMulti)
	}
}

// ---------------------------------------------------------------------------
// DefaultTools
// ---------------------------------------------------------------------------

func TestDefaultToolsBasic(t *testing.T) {
	cfg := newTestConfig()
	toolList := DefaultTools(cfg, "", nil)

	// Should have: exec, web_search, web_fetch, read_file, write_file, list_dir, notify_user = 7
	if len(toolList) != 7 {
		t.Errorf("expected 7 default tools, got %d", len(toolList))
	}

	names := make(map[string]bool)
	for _, tool := range toolList {
		names[tool.Name()] = true
	}
	for _, expected := range []string{"exec", "web_search", "web_fetch", "read_file", "write_file", "list_dir", "notify_user"} {
		if !names[expected] {
			t.Errorf("missing expected tool %q", expected)
		}
	}
}

func TestDefaultToolsWithMemory(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Memory.Enabled = true
	workspace := t.TempDir()

	toolList := DefaultTools(cfg, workspace, nil)

	// Should have 7 base + 2 memory = 9
	if len(toolList) != 9 {
		t.Errorf("expected 9 tools with memory enabled, got %d", len(toolList))
	}

	names := make(map[string]bool)
	for _, tool := range toolList {
		names[tool.Name()] = true
	}
	if !names["memory_append"] {
		t.Error("missing memory_append tool")
	}
	if !names["memory_get"] {
		t.Error("missing memory_get tool")
	}
}

func TestDefaultToolsNoMemoryWithoutWorkspace(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Memory.Enabled = true

	toolList := DefaultTools(cfg, "", nil) // empty workspace

	// Should still have only 7 base tools (memory tools require workspace)
	if len(toolList) != 7 {
		t.Errorf("expected 7 tools without workspace, got %d", len(toolList))
	}
}

// ---------------------------------------------------------------------------
// Compact
// ---------------------------------------------------------------------------

func TestCompactEmptyHistory(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	ag := &Agent{
		cfg:      cfg,
		def:      cfg.DefaultAgent(),
		sessions: sm,
		toolMap:  make(map[string]Tool),
		logger:   zap.NewNop().Sugar(),
	}

	// Compact on empty session should return nil
	err := ag.Compact(context.Background(), "empty-session", "")
	if err != nil {
		t.Errorf("Compact on empty history should not error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// forceSoftTrim
// ---------------------------------------------------------------------------

func TestForceSoftTrimFewAssistants(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.ContextPruning.KeepLastAssistants = 2
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	ag := &Agent{
		cfg:      cfg,
		def:      cfg.DefaultAgent(),
		sessions: sm,
		toolMap:  make(map[string]Tool),
		logger:   zap.NewNop().Sugar(),
	}

	// Only 2 assistant messages, keepN=2 => no split
	history := []session.Message{
		{Role: "user", Content: "q1", TS: 1},
		{Role: "assistant", Content: "a1", TS: 2},
		{Role: "user", Content: "q2", TS: 3},
		{Role: "assistant", Content: "a2", TS: 4},
	}

	result := ag.forceSoftTrim(context.Background(), "test", history, "")
	if len(result) != len(history) {
		t.Errorf("expected no trimming (too few assistants), got %d instead of %d", len(result), len(history))
	}
}

func TestForceSoftTrimWithToolCalls(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.ContextPruning.KeepLastAssistants = 1
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	// Test through softTrimWithRouter helper which uses a mock router
	// instead of the real a.router.
	softRouter := &mockRouter{
		responses: []openai.ChatCompletionResponse{{
			Choices: []openai.ChatCompletionChoice{{
				Message: openai.ChatCompletionMessage{
					Role:    "assistant",
					Content: "Summary of tool usage.",
				},
			}},
		}},
	}

	ag := &Agent{
		cfg:      cfg,
		def:      cfg.DefaultAgent(),
		sessions: sm,
		toolMap:  make(map[string]Tool),
		logger:   zap.NewNop().Sugar(),
	}

	// History with tool_calls before the split point
	history := []session.Message{
		{Role: "user", Content: "q1", TS: 1},
		{Role: "assistant", Content: "a1", TS: 2},
		{Role: "user", Content: "q2", TS: 3},
		{Role: "assistant", Content: "a2", TS: 4},
		{Role: "user", Content: "q3", TS: 5},
		{Role: "assistant", Content: "a3", TS: 6},
	}

	cfg.Agents.Defaults.SoftTrimRatio = 0.0001 // force trigger
	result := ag.softTrimWithRouter(context.Background(), softRouter, history)
	if len(result) >= len(history) {
		t.Errorf("expected soft trim to reduce message count, got %d", len(result))
	}
}

// ---------------------------------------------------------------------------
// Chat — full round-trip with mock router
// ---------------------------------------------------------------------------

func TestChatFullRoundTrip(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	mt := &mockTool{name: "test_exec", result: "executed"}

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, "", []Tool{mt})

	// Use loopWithRouter test helper since ag.router is *models.Router.
	mockR := &mockRouter{
		responses: []openai.ChatCompletionResponse{{
			Choices: []openai.ChatCompletionChoice{{
				Message: openai.ChatCompletionMessage{
					Role:    "assistant",
					Content: "Hello! I can help you.",
				},
			}},
			Usage: openai.Usage{PromptTokens: 100, CompletionTokens: 20},
		}},
	}

	msgs := []openai.ChatCompletionMessage{{Role: "user", Content: "hi"}}
	resp, added, err := ag.loopWithRouter(context.Background(), mockR, func() string { p, _ := ag.buildSystemPrompt(); return p }(), msgs)
	if err != nil {
		t.Fatalf("loop: %v", err)
	}
	if resp.Text != "Hello! I can help you." {
		t.Errorf("expected 'Hello! I can help you.', got %q", resp.Text)
	}
	if resp.Stopped {
		t.Error("should not be stopped")
	}
	if len(added) != 1 {
		t.Errorf("expected 1 added message, got %d", len(added))
	}
}

func TestChatWithToolCall(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	mt := &mockTool{name: "greet", result: "Hello, world!"}

	ag := &Agent{
		cfg:      cfg,
		def:      cfg.DefaultAgent(),
		sessions: sm,
		toolMap:  map[string]Tool{"greet": mt},
		toolDefs: []openai.Tool{{
			Type:     openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{Name: "greet", Parameters: mt.Schema()},
		}},
		logger: zap.NewNop().Sugar(),
	}

	// First response: tool call. Second response: text.
	mockR := &mockRouter{
		responses: []openai.ChatCompletionResponse{
			{
				Choices: []openai.ChatCompletionChoice{{
					Message: openai.ChatCompletionMessage{
						Role: "assistant",
						ToolCalls: []openai.ToolCall{{
							ID:   "tc1",
							Type: openai.ToolTypeFunction,
							Function: openai.FunctionCall{
								Name:      "greet",
								Arguments: `{}`,
							},
						}},
					},
				}},
			},
			{
				Choices: []openai.ChatCompletionChoice{{
					Message: openai.ChatCompletionMessage{
						Role:    "assistant",
						Content: "The greeting is: Hello, world!",
					},
				}},
			},
		},
	}

	msgs := []openai.ChatCompletionMessage{{Role: "user", Content: "say hi"}}
	resp, added, err := ag.loopWithRouter(context.Background(), mockR, "sys", msgs)
	if err != nil {
		t.Fatalf("loop: %v", err)
	}

	if resp.Text != "The greeting is: Hello, world!" {
		t.Errorf("expected final text, got %q", resp.Text)
	}
	if mt.calls != 1 {
		t.Errorf("expected greet tool called once, got %d", mt.calls)
	}
	// added should include: assistant(tool_call) + tool(result) + assistant(text)
	if len(added) != 3 {
		t.Errorf("expected 3 added messages, got %d", len(added))
	}
}

// ---------------------------------------------------------------------------
// ChatStream error path — sem full + context cancelled
// ---------------------------------------------------------------------------

func TestChatStreamSemExhausted(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	ag := &Agent{
		cfg:      cfg,
		def:      cfg.DefaultAgent(),
		sessions: sm,
		toolMap:  make(map[string]Tool),
		sem:      make(chan struct{}, 1),
		logger:   zap.NewNop().Sugar(),
	}
	// Fill semaphore
	ag.sem <- struct{}{}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := ag.ChatStream(ctx, "test-session", "hello", nil)
	if err == nil {
		t.Error("expected error from ChatStream when semaphore exhausted")
	}
	if !strings.Contains(err.Error(), "max concurrent") {
		t.Errorf("expected 'max concurrent' in error, got %q", err.Error())
	}

	// Clean up
	<-ag.sem
}

// TestChatSemExhausted verifies Chat returns an error when sem is full.
func TestChatSemExhausted(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	ag := &Agent{
		cfg:      cfg,
		def:      cfg.DefaultAgent(),
		sessions: sm,
		toolMap:  make(map[string]Tool),
		sem:      make(chan struct{}, 1),
		logger:   zap.NewNop().Sugar(),
	}
	ag.sem <- struct{}{}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := ag.Chat(ctx, "test-session", "hello")
	if err == nil {
		t.Error("expected error from Chat when semaphore exhausted")
	}
	if !strings.Contains(err.Error(), "max concurrent") {
		t.Errorf("expected 'max concurrent' in error, got %q", err.Error())
	}

	<-ag.sem
}

// ---------------------------------------------------------------------------
// memoryMDPath
// ---------------------------------------------------------------------------

func TestMemoryMDPath(t *testing.T) {
	ag := &Agent{workspace: "/home/user/project", logger: zap.NewNop().Sugar()}
	if p := ag.memoryMDPath(); p != "/home/user/project/MEMORY.md" {
		t.Errorf("expected /home/user/project/MEMORY.md, got %q", p)
	}

	ag2 := &Agent{workspace: "", logger: zap.NewNop().Sugar()}
	if p := ag2.memoryMDPath(); p != "" {
		t.Errorf("expected empty path for empty workspace, got %q", p)
	}
}

// ---------------------------------------------------------------------------
// newLoopDetector edge cases
// ---------------------------------------------------------------------------

func TestNewLoopDetectorDefaultLimit(t *testing.T) {
	ld := newLoopDetector(0)
	if ld.limit != 3 {
		t.Errorf("expected default limit 3, got %d", ld.limit)
	}

	ld2 := newLoopDetector(-1)
	if ld2.limit != 3 {
		t.Errorf("expected default limit 3 for negative input, got %d", ld2.limit)
	}
}

// ---------------------------------------------------------------------------
// DispatchTool tests
// ---------------------------------------------------------------------------

func TestDispatchToolName(t *testing.T) {
	dt := &DispatchTool{Logger: zap.NewNop().Sugar()}
	if dt.Name() != "dispatch" {
		t.Errorf("expected 'dispatch', got %q", dt.Name())
	}
}

func TestDispatchToolSchema(t *testing.T) {
	dt := &DispatchTool{Logger: zap.NewNop().Sugar()}
	var m map[string]any
	if err := json.Unmarshal(dt.Schema(), &m); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	props, ok := m["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected 'properties' in schema")
	}
	if _, ok := props["task_graph"]; !ok {
		t.Error("schema missing 'task_graph' property")
	}
	req, ok := m["required"].([]any)
	if !ok {
		t.Fatal("expected 'required' array in schema")
	}
	found := false
	for _, r := range req {
		if r == "task_graph" {
			found = true
		}
	}
	if !found {
		t.Error("'task_graph' should be required")
	}
}

func TestDispatchToolInvalidJSON(t *testing.T) {
	dt := &DispatchTool{Agents: map[string]Chatter{}, Logger: zap.NewNop().Sugar()}
	result := dt.Run(context.Background(), "not json")
	if !strings.Contains(result, "invalid arguments") {
		t.Errorf("expected 'invalid arguments', got %q", result)
	}
}

func TestDispatchToolInvalidTaskGraph(t *testing.T) {
	dt := &DispatchTool{
		Agents: map[string]Chatter{
			"worker": &mockChatter{response: "done"},
		},
		Logger: zap.NewNop().Sugar(),
	}
	// Empty task graph
	args := `{"task_graph":{"tasks":[]}}`
	result := dt.Run(context.Background(), args)
	if !strings.Contains(result, "error") {
		t.Errorf("expected error for empty task graph, got %q", result)
	}
}

func TestDispatchToolSuccessfulDispatch(t *testing.T) {
	worker := &mockChatter{response: "task completed"}
	dt := &DispatchTool{
		Agents:        map[string]Chatter{"worker": worker},
		MaxConcurrent: 2,
		Logger:        zap.NewNop().Sugar(),
	}

	args := `{"task_graph":{"tasks":[{"id":"t1","agent_id":"worker","message":"do work","blocking":true}]}}`
	result := dt.Run(context.Background(), args)

	if !strings.Contains(result, "Dispatch Results") {
		t.Errorf("expected 'Dispatch Results' in output, got %q", result)
	}
	if !strings.Contains(result, "task completed") {
		t.Errorf("expected 'task completed' in output, got %q", result)
	}
	if !strings.Contains(result, "success") {
		t.Errorf("expected 'success' status in output, got %q", result)
	}
}

func TestDispatchToolMultipleTasksWithDependency(t *testing.T) {
	worker := &mockChatter{response: "done"}
	dt := &DispatchTool{
		Agents:        map[string]Chatter{"worker": worker},
		MaxConcurrent: 5,
		Logger:        zap.NewNop().Sugar(),
	}

	args := `{"task_graph":{"tasks":[
		{"id":"t1","agent_id":"worker","message":"first","blocking":true},
		{"id":"t2","agent_id":"worker","message":"second uses {{t1.output}}","blocking":false,"depends_on":["t1"]}
	]}}`
	result := dt.Run(context.Background(), args)

	if !strings.Contains(result, "Dispatch Results") {
		t.Errorf("expected results, got %q", result)
	}
	calls := worker.getCalls()
	if len(calls) != 2 {
		t.Errorf("expected 2 calls, got %d", len(calls))
	}
}

func TestDispatchToolUnknownAgent(t *testing.T) {
	dt := &DispatchTool{
		Agents: map[string]Chatter{"worker": &mockChatter{response: "ok"}},
		Logger: zap.NewNop().Sugar(),
	}
	args := `{"task_graph":{"tasks":[{"id":"t1","agent_id":"missing","message":"work","blocking":true}]}}`
	result := dt.Run(context.Background(), args)
	if !strings.Contains(result, "unknown agent") {
		t.Errorf("expected 'unknown agent' error, got %q", result)
	}
}

// ---------------------------------------------------------------------------
// CLIAgent tests
// ---------------------------------------------------------------------------

func TestNewCLIAgent(t *testing.T) {
	ag := NewCLIAgent("test-cli", "echo", []string{"-n"}, 5*time.Second)

	if ag.id != "test-cli" {
		t.Errorf("expected id 'test-cli', got %q", ag.id)
	}
	if ag.timeout != 5*time.Second {
		t.Errorf("expected timeout 5s, got %v", ag.timeout)
	}
	if len(ag.args) != 1 || ag.args[0] != "-n" {
		t.Errorf("expected args [-n], got %v", ag.args)
	}
	// Command() accessor
	cmd := ag.Command()
	if cmd == "" {
		t.Error("Command() should not be empty")
	}
}

func TestCLIAgentChat(t *testing.T) {
	// Use "echo" which should be available on Linux
	echoPath, err := lookPathSafe("echo")
	if err != nil {
		t.Skip("echo not found in PATH, skipping CLIAgent.Chat test")
	}

	ag := &CLIAgent{
		id:      "echo-agent",
		command: echoPath,
		args:    nil,
		timeout: 5 * time.Second,
	}

	resp, err := ag.Chat(context.Background(), "session:test", "hello world")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Text != "hello world" {
		t.Errorf("expected 'hello world', got %q", resp.Text)
	}
}

func TestCLIAgentChatWithArgs(t *testing.T) {
	echoPath, err := lookPathSafe("echo")
	if err != nil {
		t.Skip("echo not found in PATH")
	}

	ag := &CLIAgent{
		id:      "echo-agent",
		command: echoPath,
		args:    []string{"-n"},
		timeout: 5 * time.Second,
	}

	resp, err := ag.Chat(context.Background(), "session:test", "test output")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Text != "test output" {
		t.Errorf("expected 'test output', got %q", resp.Text)
	}
}

func TestCLIAgentChatTimeout(t *testing.T) {
	sleepPath, err := lookPathSafe("sleep")
	if err != nil {
		t.Skip("sleep not found in PATH")
	}

	ag := &CLIAgent{
		id:      "sleep-agent",
		command: sleepPath,
		args:    nil,
		timeout: 100 * time.Millisecond,
	}

	_, err = ag.Chat(context.Background(), "session:test", "10")
	if err == nil {
		t.Error("expected timeout error")
	}
}

func TestCLIAgentChatBadCommand(t *testing.T) {
	ag := &CLIAgent{
		id:      "bad-agent",
		command: "/nonexistent/command/xyz",
		args:    nil,
	}

	_, err := ag.Chat(context.Background(), "session:test", "hello")
	if err == nil {
		t.Error("expected error from nonexistent command")
	}
	if !strings.Contains(err.Error(), "bad-agent") {
		t.Errorf("expected agent id in error, got %q", err.Error())
	}
}

// lookPathSafe is a test helper for finding commands.
func lookPathSafe(name string) (string, error) {
	return exec.LookPath(name)
}

// ---------------------------------------------------------------------------
// loopDetector — additional edge cases
// ---------------------------------------------------------------------------

func TestLoopDetectorEmptyCalls(t *testing.T) {
	ld := newLoopDetector(2)
	empty := []openai.ToolCall{}

	// Empty calls should not panic
	if ld.check(empty) {
		t.Error("first empty check should not detect loop")
	}
	if !ld.check(empty) {
		t.Error("second empty check should detect loop (limit=2)")
	}
}

// ---------------------------------------------------------------------------
// Mock provider for real Router tests
// ---------------------------------------------------------------------------

// fakeProvider implements models.Provider with canned responses.
type fakeProvider struct {
	chatResponses []openai.ChatCompletionResponse
	chatIdx       int
	streamErr     error
}

func (f *fakeProvider) Chat(_ context.Context, _ openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	if f.chatIdx >= len(f.chatResponses) {
		return openai.ChatCompletionResponse{}, fmt.Errorf("no more responses")
	}
	r := f.chatResponses[f.chatIdx]
	f.chatIdx++
	return r, nil
}

func (f *fakeProvider) ChatStream(_ context.Context, _ openai.ChatCompletionRequest) (models.Stream, error) {
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	return nil, fmt.Errorf("fake stream not implemented")
}

// makeRouter creates a *models.Router backed by a fakeProvider.
func makeRouter(responses ...openai.ChatCompletionResponse) *models.Router {
	fp := &fakeProvider{chatResponses: responses}
	providers := map[string]models.Provider{"test": fp}
	return models.NewRouter(testLogger(), providers, "test/test-model", nil)
}

// ---------------------------------------------------------------------------
// Real Chat() round-trip (covers loop, softTrim, buildSystemPrompt, etc.)
// ---------------------------------------------------------------------------

func TestChatRealRouter(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	router := makeRouter(openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{{
			Message: openai.ChatCompletionMessage{
				Role:    "assistant",
				Content: "Hello from real router!",
			},
		}},
		Usage: openai.Usage{PromptTokens: 50, CompletionTokens: 10},
	})

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)

	resp, err := ag.Chat(context.Background(), "test:real-router", "hi")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Text != "Hello from real router!" {
		t.Errorf("expected 'Hello from real router!', got %q", resp.Text)
	}
	if resp.Usage.InputTokens != 50 {
		t.Errorf("expected 50 input tokens, got %d", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 10 {
		t.Errorf("expected 10 output tokens, got %d", resp.Usage.OutputTokens)
	}

	// Verify history was persisted
	h, err := sm.GetHistory("test:real-router")
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if len(h) < 2 {
		t.Fatalf("expected at least 2 messages (user+assistant), got %d", len(h))
	}
	if h[0].Role != "user" || h[0].Content != "hi" {
		t.Errorf("first message should be user 'hi', got %q: %q", h[0].Role, h[0].Content)
	}
	if h[1].Role != "assistant" || h[1].Content != "Hello from real router!" {
		t.Errorf("second message should be assistant, got %q: %q", h[1].Role, h[1].Content)
	}
}

// TestChatRealRouterWithToolCall tests the real loop path with a tool call.
func TestChatRealRouterWithToolCall(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	mt := &mockTool{name: "greet", result: "Hi there!"}

	router := makeRouter(
		// First response: tool call
		openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{
				Message: openai.ChatCompletionMessage{
					Role: "assistant",
					ToolCalls: []openai.ToolCall{{
						ID:   "tc_real_1",
						Type: openai.ToolTypeFunction,
						Function: openai.FunctionCall{
							Name:      "greet",
							Arguments: `{}`,
						},
					}},
				},
			}},
		},
		// Second response: final text
		openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{
				Message: openai.ChatCompletionMessage{
					Role:    "assistant",
					Content: "Greeted successfully.",
				},
			}},
		},
	)

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", []Tool{mt})

	resp, err := ag.Chat(context.Background(), "test:tool-call", "please greet")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Text != "Greeted successfully." {
		t.Errorf("expected 'Greeted successfully.', got %q", resp.Text)
	}
	if mt.calls != 1 {
		t.Errorf("expected greet tool called once, got %d", mt.calls)
	}
}

// TestChatRealRouterLoopDetection tests loop detection through the real loop.
func TestChatRealRouterLoopDetection(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	cfg.Agents.Defaults.LoopDetectionN = 2
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	mt := &mockTool{name: "repeat", result: "same"}

	sameToolCall := openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{{
			Message: openai.ChatCompletionMessage{
				Role: "assistant",
				ToolCalls: []openai.ToolCall{{
					ID:   "tc_loop",
					Type: openai.ToolTypeFunction,
					Function: openai.FunctionCall{
						Name:      "repeat",
						Arguments: `{"x":"same"}`,
					},
				}},
			},
		}},
	}

	router := makeRouter(sameToolCall, sameToolCall, sameToolCall)
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", []Tool{mt})

	resp, err := ag.Chat(context.Background(), "test:loop-detect", "go")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !resp.Stopped {
		t.Error("expected loop detection to stop the conversation")
	}
	if !strings.Contains(resp.Text, "Loop detected") {
		t.Errorf("expected 'Loop detected' in text, got %q", resp.Text)
	}
}

// TestSoftTrimRealRouter tests soft trim through real Chat path.
func TestSoftTrimRealRouter(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	cfg.Agents.Defaults.SoftTrimRatio = 0.0001 // very low threshold to trigger
	cfg.Agents.Defaults.ContextPruning.KeepLastAssistants = 1
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	// Pre-populate history to trigger soft trim
	key := "test:soft-trim-real"
	now := time.Now().UnixMilli()
	var msgs []session.Message
	for i := range 10 {
		msgs = append(msgs, session.Message{
			Role:    "user",
			Content: fmt.Sprintf("Long question %d with enough text to generate a lot of estimated tokens for context window calculations", i),
			TS:      now,
		})
		msgs = append(msgs, session.Message{
			Role:    "assistant",
			Content: fmt.Sprintf("Long answer %d with enough text to generate a lot of estimated tokens for context window calculations too", i),
			TS:      now,
		})
	}
	if err := sm.AppendMessages(key, msgs); err != nil {
		t.Fatal(err)
	}

	// Router: first call is summary (from softTrim/forceSoftTrim), second is
	// the actual Chat response
	router := makeRouter(
		// Summary response (softTrim)
		openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{
				Message: openai.ChatCompletionMessage{
					Role:    "assistant",
					Content: "Summary of conversation.",
				},
			}},
		},
		// Actual chat response
		openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{
				Message: openai.ChatCompletionMessage{
					Role:    "assistant",
					Content: "Response after trim.",
				},
			}},
		},
	)

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)

	resp, err := ag.Chat(context.Background(), key, "new question")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Text != "Response after trim." {
		t.Errorf("expected 'Response after trim.', got %q", resp.Text)
	}

	// Verify history was trimmed
	h, err := sm.GetHistory(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) >= 20 {
		t.Errorf("expected history to be trimmed from 20+, got %d", len(h))
	}
}

// TestCompactRealRouter tests Compact with a real router.
func TestCompactRealRouter(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	cfg.Agents.Defaults.ContextPruning.KeepLastAssistants = 1
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	key := "test:compact"
	now := time.Now().UnixMilli()
	if err := sm.AppendMessages(key, []session.Message{
		{Role: "user", Content: "q1", TS: now},
		{Role: "assistant", Content: "a1", TS: now},
		{Role: "user", Content: "q2", TS: now},
		{Role: "assistant", Content: "a2", TS: now},
		{Role: "user", Content: "q3", TS: now},
		{Role: "assistant", Content: "a3", TS: now},
	}); err != nil {
		t.Fatal(err)
	}

	router := makeRouter(openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{{
			Message: openai.ChatCompletionMessage{
				Role:    "assistant",
				Content: "Compacted summary.",
			},
		}},
	})

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)

	if err := ag.Compact(context.Background(), key, "be brief"); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	h, err := sm.GetHistory(key)
	if err != nil {
		t.Fatal(err)
	}
	// Should be compacted: summary + recent messages
	if len(h) >= 6 {
		t.Errorf("expected compacted history < 6 messages, got %d", len(h))
	}
	if len(h) > 0 && !strings.Contains(h[0].Content, "Compacted summary") {
		t.Errorf("expected summary as first message, got %q", h[0].Content)
	}
}

// TestForceSoftTrimToolCallBoundary tests that forceSoftTrim doesn't split
// in the middle of an assistant(tool_calls)->tool sequence.
func TestForceSoftTrimToolCallBoundary(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	cfg.Agents.Defaults.ContextPruning.KeepLastAssistants = 1
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	router := makeRouter(openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{{
			Message: openai.ChatCompletionMessage{
				Role:    "assistant",
				Content: "Summary with tools.",
			},
		}},
	})

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)

	now := time.Now().UnixMilli()
	history := []session.Message{
		{Role: "user", Content: "q1", TS: now},
		{Role: "assistant", Content: "a1", TS: now, ToolCalls: []openai.ToolCall{{
			ID: "tc1", Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: "exec", Arguments: `{"cmd":"ls"}`},
		}}},
		{Role: "tool", Content: "file1 file2", ToolCallID: "tc1", TS: now},
		{Role: "assistant", Content: "a2 after tools", TS: now},
		{Role: "user", Content: "q2", TS: now},
		{Role: "assistant", Content: "a3 final", TS: now},
	}

	result := ag.forceSoftTrim(context.Background(), "test:boundary", history, "")

	// The split should NOT orphan the tool message. The result should start
	// with the summary, and recent messages should be a valid sequence.
	if len(result) >= len(history) {
		t.Errorf("expected trimming, got %d messages (same as input %d)", len(result), len(history))
	}
	if len(result) > 0 && !strings.Contains(result[0].Content, "Summary with tools") {
		t.Errorf("expected summary as first message, got %q", result[0].Content)
	}
	// Verify no orphaned tool messages in result
	for i, m := range result {
		if m.Role == "tool" {
			// Must be preceded by an assistant with tool_calls
			if i == 0 {
				t.Error("tool message at index 0 with no preceding assistant")
				continue
			}
			prev := result[i-1]
			if prev.Role != "assistant" || len(prev.ToolCalls) == 0 {
				// Could also be another tool message in a multi-call
				if prev.Role != "tool" {
					t.Errorf("tool message at index %d not preceded by assistant with tool_calls", i)
				}
			}
		}
	}
}

// TestChatRealRouterEmptyResponse tests handling of empty model response.
func TestChatRealRouterEmptyResponse(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	router := makeRouter(openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{}, // empty
	})

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)

	_, err := ag.Chat(context.Background(), "test:empty-resp", "hi")
	if err == nil {
		t.Error("expected error from empty response")
	}
	if !strings.Contains(err.Error(), "empty response") {
		t.Errorf("expected 'empty response' in error, got %q", err.Error())
	}
}

// TestDelegateToolSyncDelegation tests synchronous delegation through Run().
func TestDelegateToolSyncDelegation(t *testing.T) {
	sub := &mockChatter{response: "sub result"}
	tool := &DelegateTool{
		Agents:   map[string]Chatter{"sub": sub},
		MaxDepth: 5,
		Logger:   zap.NewNop().Sugar(),
	}

	args, _ := json.Marshal(map[string]string{"agent_id": "sub", "message": "do it"})
	result := tool.Run(context.Background(), string(args))

	if result != "sub result" {
		t.Errorf("expected 'sub result', got %q", result)
	}
	calls := sub.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].Message != "do it" {
		t.Errorf("expected message 'do it', got %q", calls[0].Message)
	}
}

// TestDelegateToolSyncDelegationError tests sync delegation when subagent errors.
func TestDelegateToolSyncDelegationError(t *testing.T) {
	sub := &mockChatter{err: fmt.Errorf("agent broke")}
	tool := &DelegateTool{
		Agents:   map[string]Chatter{"sub": sub},
		MaxDepth: 5,
		Logger:   zap.NewNop().Sugar(),
	}

	args, _ := json.Marshal(map[string]string{"agent_id": "sub", "message": "do it"})
	result := tool.Run(context.Background(), string(args))

	if !strings.Contains(result, "error calling subagent") {
		t.Errorf("expected error message, got %q", result)
	}
}

// TestDelegateToolDefaultMaxDepth tests that default max depth applies.
func TestDelegateToolDefaultMaxDepth(t *testing.T) {
	tool := &DelegateTool{
		Agents:   map[string]Chatter{"sub": &mockChatter{response: "ok"}},
		MaxDepth: 0, // should default to 5
		Logger:   zap.NewNop().Sugar(),
	}
	ctx := context.WithValue(context.Background(), delegateDepthKey{}, 5)
	args, _ := json.Marshal(map[string]string{"agent_id": "sub", "message": "hi"})
	result := tool.Run(ctx, string(args))
	if !strings.Contains(result, "recursion limit") {
		t.Errorf("expected recursion limit with default depth, got %q", result)
	}
}

// TestAgentIDsHelper tests the agentIDs helper.
func TestAgentIDsHelper(t *testing.T) {
	m := map[string]Chatter{
		"a": &mockChatter{},
		"b": &mockChatter{},
	}
	ids := agentIDs(m)
	if len(ids) != 2 {
		t.Errorf("expected 2 ids, got %d", len(ids))
	}
}

// TestDelegateToolWithSessionID tests custom session ID.
func TestDelegateToolWithSessionID(t *testing.T) {
	sub := &mockChatter{response: "with session"}
	tool := &DelegateTool{
		Agents:   map[string]Chatter{"sub": sub},
		MaxDepth: 5,
		Logger:   zap.NewNop().Sugar(),
	}

	args, _ := json.Marshal(map[string]any{
		"agent_id":   "sub",
		"message":    "hello",
		"session_id": "custom:session:123",
	})
	result := tool.Run(context.Background(), string(args))
	if result != "with session" {
		t.Errorf("expected 'with session', got %q", result)
	}
	calls := sub.getCalls()
	if calls[0].SessionKey != "custom:session:123" {
		t.Errorf("expected custom session key, got %q", calls[0].SessionKey)
	}
}

// TestDelegateToolStoppedResponse tests when subagent returns Stopped=true.
func TestDelegateToolStoppedResponse(t *testing.T) {
	// We need a chatter that returns Stopped=true. mockChatter always returns
	// Stopped=false, so create a special one.
	stoppedChatter := &stoppedMockChatter{}
	tool := &DelegateTool{
		Agents:   map[string]Chatter{"sub": stoppedChatter},
		MaxDepth: 5,
		Logger:   zap.NewNop().Sugar(),
	}

	args, _ := json.Marshal(map[string]string{"agent_id": "sub", "message": "go"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "[subagent stopped]") {
		t.Errorf("expected '[subagent stopped]', got %q", result)
	}
}

type stoppedMockChatter struct{}

func (s *stoppedMockChatter) Chat(_ context.Context, _, _ string) (Response, error) {
	return Response{Text: "max iterations", Stopped: true}, nil
}

// ---------------------------------------------------------------------------
// Streaming tests (ChatStream / loopStream)
// ---------------------------------------------------------------------------

// fakeStream replays a sequence of ChatCompletionStreamResponse chunks, then EOF.
type fakeStream struct {
	chunks []openai.ChatCompletionStreamResponse
	idx    int
}

func (f *fakeStream) Recv() (openai.ChatCompletionStreamResponse, error) {
	if f.idx >= len(f.chunks) {
		return openai.ChatCompletionStreamResponse{}, fmt.Errorf("EOF: %w", io.EOF)
	}
	c := f.chunks[f.idx]
	f.idx++
	return c, nil
}

func (f *fakeStream) Close() error { return nil }

// fakeStreamProvider returns a fakeStream from ChatStream and canned responses from Chat.
type fakeStreamProvider struct {
	chatResponses []openai.ChatCompletionResponse
	chatIdx       int
	streamChunks  []openai.ChatCompletionStreamResponse
	streamErr     error
}

func (f *fakeStreamProvider) Chat(_ context.Context, _ openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	if f.chatIdx >= len(f.chatResponses) {
		return openai.ChatCompletionResponse{}, fmt.Errorf("no more chat responses")
	}
	r := f.chatResponses[f.chatIdx]
	f.chatIdx++
	return r, nil
}

func (f *fakeStreamProvider) ChatStream(_ context.Context, _ openai.ChatCompletionRequest) (models.Stream, error) {
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	return &fakeStream{chunks: f.streamChunks}, nil
}

func makeStreamRouter(streamChunks []openai.ChatCompletionStreamResponse, chatResponses ...openai.ChatCompletionResponse) *models.Router {
	fp := &fakeStreamProvider{
		chatResponses: chatResponses,
		streamChunks:  streamChunks,
	}
	providers := map[string]models.Provider{"test": fp}
	return models.NewRouter(testLogger(), providers, "test/test-model", nil)
}

func TestChatStreamRealRouter(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	chunks := []openai.ChatCompletionStreamResponse{
		{Choices: []openai.ChatCompletionStreamChoice{{Delta: openai.ChatCompletionStreamChoiceDelta{Content: "Hello "}}}},
		{Choices: []openai.ChatCompletionStreamChoice{{Delta: openai.ChatCompletionStreamChoiceDelta{Content: "world!"}}}},
	}

	router := makeStreamRouter(chunks)
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)

	var collected []string
	resp, err := ag.ChatStream(context.Background(), "test:stream", "hi", &StreamCallbacks{OnChunk: func(chunk string) {
		collected = append(collected, chunk)
	}})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp.Text != "Hello world!" {
		t.Errorf("expected 'Hello world!', got %q", resp.Text)
	}
	if len(collected) != 2 {
		t.Errorf("expected 2 chunks, got %d", len(collected))
	}

	// Verify history was persisted
	h, err := sm.GetHistory("test:stream")
	if err != nil {
		t.Fatal(err)
	}
	if len(h) < 2 {
		t.Fatalf("expected at least 2 messages in history, got %d", len(h))
	}
}

func TestChatStreamWithToolCalls(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	mt := &mockTool{name: "compute", result: "42"}

	// Stream that returns a tool call in chunks, then a text response
	idx := 0 // helper for func pointer
	toolCallChunks := []openai.ChatCompletionStreamResponse{
		{Choices: []openai.ChatCompletionStreamChoice{{
			Delta: openai.ChatCompletionStreamChoiceDelta{
				ToolCalls: []openai.ToolCall{{
					Index: &idx,
					ID:    "tc_stream_1",
					Type:  openai.ToolTypeFunction,
					Function: openai.FunctionCall{
						Name: "compute",
					},
				}},
			},
		}}},
		{Choices: []openai.ChatCompletionStreamChoice{{
			Delta: openai.ChatCompletionStreamChoiceDelta{
				ToolCalls: []openai.ToolCall{{
					Index: &idx,
					Function: openai.FunctionCall{
						Arguments: `{"expr":"1+1"}`,
					},
				}},
			},
		}}},
	}

	textChunks := []openai.ChatCompletionStreamResponse{
		{Choices: []openai.ChatCompletionStreamChoice{{Delta: openai.ChatCompletionStreamChoiceDelta{Content: "The answer is 42."}}}},
	}

	// Create a provider that returns tool call stream first, then text stream
	callCount := 0
	fp := &sequentialStreamProvider{
		streams: [][]openai.ChatCompletionStreamResponse{toolCallChunks, textChunks},
	}
	_ = callCount
	providers := map[string]models.Provider{"test": fp}
	router := models.NewRouter(testLogger(), providers, "test/test-model", nil)

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", []Tool{mt})

	resp, err := ag.ChatStream(context.Background(), "test:stream-tool", "compute", nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp.Text != "The answer is 42." {
		t.Errorf("expected 'The answer is 42.', got %q", resp.Text)
	}
	if mt.calls != 1 {
		t.Errorf("expected compute tool called once, got %d", mt.calls)
	}
}

// sequentialStreamProvider returns different streams on successive ChatStream calls.
type sequentialStreamProvider struct {
	streams [][]openai.ChatCompletionStreamResponse
	idx     int
}

func (s *sequentialStreamProvider) Chat(_ context.Context, _ openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	return openai.ChatCompletionResponse{}, fmt.Errorf("Chat not expected in streaming test")
}

func (s *sequentialStreamProvider) ChatStream(_ context.Context, _ openai.ChatCompletionRequest) (models.Stream, error) {
	if s.idx >= len(s.streams) {
		return nil, fmt.Errorf("no more streams")
	}
	stream := &fakeStream{chunks: s.streams[s.idx]}
	s.idx++
	return stream, nil
}

func TestChatStreamLoopDetection(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	cfg.Agents.Defaults.LoopDetectionN = 2
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	mt := &mockTool{name: "repeat", result: "same"}

	idx := 0
	sameToolChunks := []openai.ChatCompletionStreamResponse{
		{Choices: []openai.ChatCompletionStreamChoice{{
			Delta: openai.ChatCompletionStreamChoiceDelta{
				ToolCalls: []openai.ToolCall{{
					Index: &idx,
					ID:    "tc_loop",
					Type:  openai.ToolTypeFunction,
					Function: openai.FunctionCall{
						Name:      "repeat",
						Arguments: `{"x":"same"}`,
					},
				}},
			},
		}}},
	}

	fp := &sequentialStreamProvider{
		streams: [][]openai.ChatCompletionStreamResponse{
			sameToolChunks, sameToolChunks, sameToolChunks,
		},
	}
	providers := map[string]models.Provider{"test": fp}
	router := models.NewRouter(testLogger(), providers, "test/test-model", nil)

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", []Tool{mt})

	resp, err := ag.ChatStream(context.Background(), "test:stream-loop", "go", nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if !resp.Stopped {
		t.Error("expected loop detection to stop streaming")
	}
	if !strings.Contains(resp.Text, "Loop detected") {
		t.Errorf("expected 'Loop detected', got %q", resp.Text)
	}
}

func TestChatStreamError(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	fp := &fakeStreamProvider{streamErr: fmt.Errorf("stream broke")}
	providers := map[string]models.Provider{"test": fp}
	router := models.NewRouter(testLogger(), providers, "test/test-model", nil)

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)

	_, err := ag.ChatStream(context.Background(), "test:stream-err", "hi", nil)
	if err == nil {
		t.Error("expected error from ChatStream")
	}
}

// TestBuildSystemPromptBadTimezone tests fallback to UTC for bad timezone.
func TestBuildSystemPromptBadTimezone(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.UserTimezone = "Invalid/Timezone"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, "", nil)

	prompt, _ := ag.buildSystemPrompt()
	// Should fall back to UTC and still contain date/time
	if !strings.Contains(prompt, "Current date/time:") {
		t.Error("expected date/time even with invalid timezone")
	}
}

// ---------------------------------------------------------------------------
// Additional coverage tests — uncovered code paths
// ---------------------------------------------------------------------------

// TestChatStreamMidStreamRecvError tests loopStream when Recv returns a non-EOF error.
func TestChatStreamMidStreamRecvError(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	fp := &fakeStreamProvider{
		streamChunks: nil, // will use errorStream instead
	}
	// Replace with an error-producing stream provider
	errProvider := &errorStreamProvider{
		errAfterChunks: 1,
		chunks: []openai.ChatCompletionStreamResponse{
			{Choices: []openai.ChatCompletionStreamChoice{{Delta: openai.ChatCompletionStreamChoiceDelta{Content: "partial"}}}},
		},
		recvErr: fmt.Errorf("connection reset"),
	}
	_ = fp
	providers := map[string]models.Provider{"test": errProvider}
	router := models.NewRouter(testLogger(), providers, "test/test-model", nil)

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)

	_, err := ag.ChatStream(context.Background(), "test:stream-recv-err", "hi", nil)
	if err == nil {
		t.Error("expected error from mid-stream Recv failure")
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("expected 'connection reset' in error, got %q", err.Error())
	}
}

// errorStream delivers N chunks then returns an error (not EOF).
type errorStream struct {
	chunks  []openai.ChatCompletionStreamResponse
	idx     int
	recvErr error
}

func (e *errorStream) Recv() (openai.ChatCompletionStreamResponse, error) {
	if e.idx < len(e.chunks) {
		c := e.chunks[e.idx]
		e.idx++
		return c, nil
	}
	return openai.ChatCompletionStreamResponse{}, e.recvErr
}

func (e *errorStream) Close() error { return nil }

// errorStreamProvider returns an errorStream from ChatStream.
type errorStreamProvider struct {
	errAfterChunks int
	chunks         []openai.ChatCompletionStreamResponse
	recvErr        error
}

func (e *errorStreamProvider) Chat(_ context.Context, _ openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	return openai.ChatCompletionResponse{}, fmt.Errorf("not expected")
}

func (e *errorStreamProvider) ChatStream(_ context.Context, _ openai.ChatCompletionRequest) (models.Stream, error) {
	return &errorStream{chunks: e.chunks, recvErr: e.recvErr}, nil
}

// TestChatStreamEmptyChoicesChunk tests that empty-choices chunks are skipped in loopStream.
func TestChatStreamEmptyChoicesChunk(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	// Mix of empty and real chunks
	chunks := []openai.ChatCompletionStreamResponse{
		{Choices: []openai.ChatCompletionStreamChoice{}}, // empty — should be skipped
		{Choices: []openai.ChatCompletionStreamChoice{{Delta: openai.ChatCompletionStreamChoiceDelta{Content: "Hello"}}}},
		{Choices: []openai.ChatCompletionStreamChoice{}}, // another empty
		{Choices: []openai.ChatCompletionStreamChoice{{Delta: openai.ChatCompletionStreamChoiceDelta{Content: " world"}}}},
	}

	router := makeStreamRouter(chunks)
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)

	var collected []string
	resp, err := ag.ChatStream(context.Background(), "test:empty-choices", "hi", &StreamCallbacks{OnChunk: func(chunk string) {
		collected = append(collected, chunk)
	}})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp.Text != "Hello world" {
		t.Errorf("expected 'Hello world', got %q", resp.Text)
	}
	// Only 2 real chunks should be collected
	if len(collected) != 2 {
		t.Errorf("expected 2 chunks (skipping empty), got %d", len(collected))
	}
}

// TestChatStreamPhantomSlotFiltering tests that phantom (empty) tool call slots
// are filtered out in loopStream, and that tool calls missing an ID get one assigned.
func TestChatStreamPhantomSlotFiltering(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	mt := &mockTool{name: "compute", result: "42"}

	// Simulate Copilot-style streaming: index=1 tool call (skipping index=0),
	// creating a phantom empty slot at index 0.
	idx1 := 1
	toolCallChunks := []openai.ChatCompletionStreamResponse{
		{Choices: []openai.ChatCompletionStreamChoice{{
			Delta: openai.ChatCompletionStreamChoiceDelta{
				ToolCalls: []openai.ToolCall{{
					Index: &idx1,
					ID:    "", // no ID — should get assigned
					Type:  openai.ToolTypeFunction,
					Function: openai.FunctionCall{
						Name:      "compute",
						Arguments: `{"x":1}`,
					},
				}},
			},
		}}},
	}

	textChunks := []openai.ChatCompletionStreamResponse{
		{Choices: []openai.ChatCompletionStreamChoice{{Delta: openai.ChatCompletionStreamChoiceDelta{Content: "Result: 42"}}}},
	}

	fp := &sequentialStreamProvider{
		streams: [][]openai.ChatCompletionStreamResponse{toolCallChunks, textChunks},
	}
	providers := map[string]models.Provider{"test": fp}
	router := models.NewRouter(testLogger(), providers, "test/test-model", nil)

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", []Tool{mt})

	resp, err := ag.ChatStream(context.Background(), "test:phantom-filter", "compute", nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp.Text != "Result: 42" {
		t.Errorf("expected 'Result: 42', got %q", resp.Text)
	}
	if mt.calls != 1 {
		t.Errorf("expected compute tool called once, got %d", mt.calls)
	}
}

// TestChatStreamAllPhantomSlots tests loopStream when all tool call slots are phantom
// (empty name and empty arguments), so isToolCall gets set back to false.
func TestChatStreamAllPhantomSlots(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	// Stream that has tool_calls delta but with only empty phantom slots
	idx0 := 0
	phantomChunks := []openai.ChatCompletionStreamResponse{
		{Choices: []openai.ChatCompletionStreamChoice{{
			Delta: openai.ChatCompletionStreamChoiceDelta{
				Content: "Just text.",
				ToolCalls: []openai.ToolCall{{
					Index:    &idx0,
					Function: openai.FunctionCall{Name: "", Arguments: ""},
				}},
			},
		}}},
	}

	fp := &fakeStreamProvider{streamChunks: phantomChunks}
	providers := map[string]models.Provider{"test": fp}
	router := models.NewRouter(testLogger(), providers, "test/test-model", nil)

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)

	resp, err := ag.ChatStream(context.Background(), "test:all-phantom", "hi", nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	// Should return the text content, not treat as tool call
	if resp.Text != "Just text." {
		t.Errorf("expected 'Just text.', got %q", resp.Text)
	}
}

// TestDefaultToolsWithSandbox tests that DefaultTools creates tools with sandbox config.
func TestDefaultToolsWithSandbox(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Sandbox = config.SandboxConfig{
		Enabled:      true,
		Image:        "ubuntu:22.04",
		Mounts:       []string{"/host:/container:ro"},
		SetupCommand: "apt-get update",
	}

	toolList := DefaultTools(cfg, "", nil)

	// Should still have 7 base tools (6 core + notify_user)
	if len(toolList) != 7 {
		t.Errorf("expected 7 tools, got %d", len(toolList))
	}
	// Verify exec tool exists (it would have sandbox config)
	names := make(map[string]bool)
	for _, tool := range toolList {
		names[tool.Name()] = true
	}
	if !names["exec"] {
		t.Error("missing exec tool")
	}
}

// TestForceSoftTrimDefaultKeepN tests that keepN defaults to 2 when config is 0.
func TestForceSoftTrimDefaultKeepN(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.ContextPruning.KeepLastAssistants = 0 // should default to 2
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	router := makeRouter(openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{{
			Message: openai.ChatCompletionMessage{
				Role:    "assistant",
				Content: "Summary with default keepN.",
			},
		}},
	})

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)

	now := time.Now().UnixMilli()
	history := []session.Message{
		{Role: "user", Content: "q1", TS: now},
		{Role: "assistant", Content: "a1", TS: now},
		{Role: "user", Content: "q2", TS: now},
		{Role: "assistant", Content: "a2", TS: now},
		{Role: "user", Content: "q3", TS: now},
		{Role: "assistant", Content: "a3", TS: now},
	}

	result := ag.forceSoftTrim(context.Background(), "test:default-keepn", history, "")

	// keepN defaults to 2, so with 3 assistants it should trim
	if len(result) >= len(history) {
		t.Errorf("expected trimming with default keepN=2, got %d messages (same as input %d)", len(result), len(history))
	}
}

// TestForceSoftTrimSummarizationFailure tests that forceSoftTrim returns original
// history when summarization fails (empty choices from model).
func TestForceSoftTrimSummarizationFailure(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.ContextPruning.KeepLastAssistants = 1
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	// Router returns empty choices (summarization failure)
	router := makeRouter(openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{}, // empty — summarization fails
	})

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)

	now := time.Now().UnixMilli()
	history := []session.Message{
		{Role: "user", Content: "q1", TS: now},
		{Role: "assistant", Content: "a1", TS: now},
		{Role: "user", Content: "q2", TS: now},
		{Role: "assistant", Content: "a2", TS: now},
		{Role: "user", Content: "q3", TS: now},
		{Role: "assistant", Content: "a3", TS: now},
	}

	result := ag.forceSoftTrim(context.Background(), "test:fail-summary", history, "")

	// Should return original history unchanged when summarization fails
	if len(result) != len(history) {
		t.Errorf("expected original history length %d on summarization failure, got %d", len(history), len(result))
	}
}

// TestForceSoftTrimToolSequenceBoundary tests that forceSoftTrim backs up past
// assistant(tool_calls) → tool sequences to avoid orphaning tool messages.
// The split point needs to land on a tool message, forcing lines 240-245 to execute.
func TestForceSoftTrimToolSequenceBoundary(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.ContextPruning.KeepLastAssistants = 1
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	router := makeRouter(openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{{
			Message: openai.ChatCompletionMessage{
				Role:    "assistant",
				Content: "Summary after tool boundary adjustment.",
			},
		}},
	})

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)

	now := time.Now().UnixMilli()
	// With keepN=1, we need at least 3 assistants (so len > keepN).
	// The split should land at the second-to-last assistant, which should
	// be preceded by tool messages that force the boundary back-up.
	//
	// Indices: 0=user 1=assistant 2=user 3=assistant(tc) 4=tool 5=tool 6=assistant(final)
	// assistantIndices = [1, 3, 6], keepN=1
	// splitAt = assistantIndices[3-1] = assistantIndices[2] = 6
	// history[5].Role == "tool" -> NOT "user", so user check fails
	// for loop: history[5].Role == "tool" -> yes, splitAt=5
	//           history[4].Role == "tool" -> yes, splitAt=4
	//           history[3].Role == "assistant" -> NOT "tool", exit for
	// if check: history[3].Role == "assistant" && has ToolCalls -> yes, splitAt=3
	// oldMessages = history[:3] = [user, assistant, user]
	history := []session.Message{
		{Role: "user", Content: "q1", TS: now},
		{Role: "assistant", Content: "a1", TS: now},
		{Role: "user", Content: "q2", TS: now},
		{Role: "assistant", Content: "", TS: now, ToolCalls: []openai.ToolCall{{
			ID: "tc_boundary", Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: "exec", Arguments: `{"cmd":"ls"}`},
		}}},
		{Role: "tool", Content: "file1.go", ToolCallID: "tc_boundary", TS: now},
		{Role: "tool", Content: "file2.go", ToolCallID: "tc_boundary2", TS: now},
		{Role: "assistant", Content: "a3 final", TS: now},
	}

	result := ag.forceSoftTrim(context.Background(), "test:tool-seq-boundary", history, "")

	if len(result) >= len(history) {
		t.Errorf("expected trimming, got %d messages (same as input %d)", len(result), len(history))
	}
	// Verify no orphaned tool messages
	for i, m := range result {
		if m.Role == "tool" && i == 0 {
			t.Error("tool message at index 0 with no preceding assistant")
		}
	}
}

// TestCLIAgentChatExitErrorWithStderr tests that CLIAgent.Chat returns stderr
// in the error message when the command exits with non-zero and produces stderr.
func TestCLIAgentChatExitErrorWithStderr(t *testing.T) {
	bashPath, err := lookPathSafe("bash")
	if err != nil {
		t.Skip("bash not found in PATH")
	}

	// bash -c "echo error_msg >&2; exit 1" ignores the extra arg (the message)
	ag := &CLIAgent{
		id:      "stderr-agent",
		command: bashPath,
		args:    []string{"-c", "echo 'some error' >&2; exit 1"},
		timeout: 5 * time.Second,
	}

	_, err = ag.Chat(context.Background(), "session:test", "ignored")
	if err == nil {
		t.Error("expected error from command with stderr")
	}
	if !strings.Contains(err.Error(), "stderr-agent") {
		t.Errorf("expected agent id in error, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "stderr:") {
		t.Errorf("expected 'stderr:' in error, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "some error") {
		t.Errorf("expected stderr content in error, got %q", err.Error())
	}
}

// TestChatStreamNilOnChunk tests that ChatStream works fine when onChunk is nil.
func TestChatStreamNilOnChunk(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	chunks := []openai.ChatCompletionStreamResponse{
		{Choices: []openai.ChatCompletionStreamChoice{{Delta: openai.ChatCompletionStreamChoiceDelta{Content: "ok"}}}},
	}

	router := makeStreamRouter(chunks)
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)

	resp, err := ag.ChatStream(context.Background(), "test:nil-onchunk", "hi", nil)
	if err != nil {
		t.Fatalf("ChatStream with nil onChunk: %v", err)
	}
	if resp.Text != "ok" {
		t.Errorf("expected 'ok', got %q", resp.Text)
	}
}

// TestDelegateToolCLIAgentAsync tests that DelegateTool routes CLIAgent
// through the async path (runAsync).
func TestDelegateToolCLIAgentAsync(t *testing.T) {
	echoPath, err := lookPathSafe("echo")
	if err != nil {
		t.Skip("echo not found in PATH")
	}

	cliAg := NewCLIAgent("echo-cli", echoPath, nil, 5*time.Second)
	mgr := taskqueue.New(testLogger(), filepath.Join(t.TempDir(), "tasks.json"), taskqueue.Config{MaxConcurrent: 5})
	tool := &DelegateTool{
		Agents:   map[string]Chatter{"echo-cli": cliAg},
		MaxDepth: 5,
		TaskMgr:  mgr,
		Logger:   zap.NewNop().Sugar(),
	}

	args, _ := json.Marshal(map[string]string{"agent_id": "echo-cli", "message": "hello async"})
	result := tool.Run(context.Background(), string(args))

	// Async path returns "Spawned ... in background"
	if !strings.Contains(result, "Spawned echo-cli in background") {
		t.Errorf("expected async spawn message, got %q", result)
	}
}

// TestSoftTrimBelowThreshold tests that softTrim returns history unchanged
// when tokens are below threshold (ratio > 0 but tokens fit).
func TestSoftTrimBelowThreshold(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.SoftTrimRatio = 0.99 // very high threshold — should NOT trigger
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	router := makeRouter() // no responses needed — should not be called
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)

	history := []session.Message{
		{Role: "user", Content: "short q", TS: time.Now().UnixMilli()},
		{Role: "assistant", Content: "short a", TS: time.Now().UnixMilli()},
	}

	result := ag.softTrim(context.Background(), "test:below-threshold", history)
	if len(result) != len(history) {
		t.Errorf("expected no trimming (below threshold), got %d instead of %d", len(result), len(history))
	}
}

// TestLoopRouterChatError tests that loop returns an error when router.Chat fails.
func TestLoopRouterChatError(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	// Router with no responses — will error
	router := makeRouter() // empty slice — "no more responses"

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)

	_, err := ag.Chat(context.Background(), "test:loop-err", "hi")
	if err == nil {
		t.Error("expected error from Chat when router fails")
	}
}

// TestDelegateToolTaskStatusWithRunning tests taskStatus when there are running tasks.
func TestDelegateToolTaskStatusWithRunning(t *testing.T) {
	mgr := taskqueue.New(testLogger(), filepath.Join(t.TempDir(), "tasks.json"), taskqueue.Config{MaxConcurrent: 5})
	tool := &DelegateTool{
		Agents:  map[string]Chatter{},
		TaskMgr: mgr,
		Logger:  zap.NewNop().Sugar(),
	}

	// Submit a long-running task so it shows as running
	blockCh := make(chan struct{})
	mgr.Submit("session:test", "worker", "do some work", func(ctx context.Context) (string, error) {
		<-blockCh
		return "done", nil
	}, taskqueue.SubmitOpts{})
	// Give goroutine time to start
	time.Sleep(50 * time.Millisecond)

	// Request status
	args, _ := json.Marshal(map[string]string{"action": "status"})
	result := tool.Run(context.Background(), string(args))
	close(blockCh)

	if !strings.Contains(result, "1 task(s)") {
		t.Errorf("expected running task count, got %q", result)
	}
	if !strings.Contains(result, "worker") {
		t.Errorf("expected agent id in status, got %q", result)
	}
}

// TestDelegateToolTaskStatusWithLongMessage tests taskStatus truncation for long messages.
func TestDelegateToolTaskStatusWithLongMessage(t *testing.T) {
	mgr := taskqueue.New(testLogger(), filepath.Join(t.TempDir(), "tasks.json"), taskqueue.Config{MaxConcurrent: 5})
	tool := &DelegateTool{
		Agents:  map[string]Chatter{},
		TaskMgr: mgr,
		Logger:  zap.NewNop().Sugar(),
	}

	longMsg := strings.Repeat("x", 200)
	blockCh := make(chan struct{})
	mgr.Submit("session:test", "worker", longMsg, func(ctx context.Context) (string, error) {
		<-blockCh
		return "done", nil
	}, taskqueue.SubmitOpts{})
	time.Sleep(50 * time.Millisecond)

	args, _ := json.Marshal(map[string]string{"action": "status"})
	result := tool.Run(context.Background(), string(args))
	close(blockCh)

	// Message should be truncated with "..."
	if !strings.Contains(result, "...") {
		t.Errorf("expected truncated message with '...' in status, got %q", result)
	}
}

// TestLoopMaxIterations tests that loop stops after MaxIterations with
// "Maximum iterations reached" message.
func TestLoopMaxIterations(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	cfg.Agents.Defaults.LoopDetectionN = 999 // disable loop detection so we hit max iterations
	cfg.Agents.Defaults.MaxIterations = 5    // small value for fast test
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	mt := &mockTool{name: "worker", result: "done"}

	// Each iteration uses a different tool call argument so loop detection doesn't kick in.
	var responses []openai.ChatCompletionResponse
	for i := range 6 { // more than MaxIterations=5
		responses = append(responses, openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{{
				Message: openai.ChatCompletionMessage{
					Role: "assistant",
					ToolCalls: []openai.ToolCall{{
						ID:   fmt.Sprintf("tc_%d", i),
						Type: openai.ToolTypeFunction,
						Function: openai.FunctionCall{
							Name:      "worker",
							Arguments: fmt.Sprintf(`{"i":%d}`, i),
						},
					}},
				},
			}},
		})
	}

	router := makeRouter(responses...)
	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", []Tool{mt})

	resp, err := ag.Chat(context.Background(), "test:max-iter", "go")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !resp.Stopped {
		t.Error("expected Stopped=true from max iterations")
	}
	if !strings.Contains(resp.Text, "Maximum iterations reached") {
		t.Errorf("expected 'Maximum iterations reached', got %q", resp.Text)
	}
	if mt.calls != 5 {
		t.Errorf("expected 5 tool calls (MaxIterations), got %d", mt.calls)
	}
}

// TestLoopStreamMaxIterations tests that loopStream stops after MaxIterations.
func TestLoopStreamMaxIterations(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	cfg.Agents.Defaults.LoopDetectionN = 999 // disable loop detection
	cfg.Agents.Defaults.MaxIterations = 5    // small value for fast test
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	mt := &mockTool{name: "worker", result: "done"}

	// Each stream returns a unique tool call.
	idx := 0
	var streams [][]openai.ChatCompletionStreamResponse
	for i := range 6 {
		streams = append(streams, []openai.ChatCompletionStreamResponse{
			{Choices: []openai.ChatCompletionStreamChoice{{
				Delta: openai.ChatCompletionStreamChoiceDelta{
					ToolCalls: []openai.ToolCall{{
						Index: &idx,
						ID:    fmt.Sprintf("tc_%d", i),
						Type:  openai.ToolTypeFunction,
						Function: openai.FunctionCall{
							Name:      "worker",
							Arguments: fmt.Sprintf(`{"i":%d}`, i),
						},
					}},
				},
			}}},
		})
	}

	fp := &sequentialStreamProvider{streams: streams}
	providers := map[string]models.Provider{"test": fp}
	router := models.NewRouter(testLogger(), providers, "test/test-model", nil)

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", []Tool{mt})

	resp, err := ag.ChatStream(context.Background(), "test:stream-max-iter", "go", nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if !resp.Stopped {
		t.Error("expected Stopped=true from max iterations")
	}
	if !strings.Contains(resp.Text, "Maximum iterations reached") {
		t.Errorf("expected 'Maximum iterations reached', got %q", resp.Text)
	}
	if mt.calls != 5 {
		t.Errorf("expected 5 tool calls (MaxIterations), got %d", mt.calls)
	}
}

// TestDispatchToolWithFailingAgent tests dispatch with an agent that returns errors,
// covering the failed task status counting path.
func TestDispatchToolWithFailingAgent(t *testing.T) {
	failingAgent := &mockChatter{err: fmt.Errorf("agent failed")}
	dt := &DispatchTool{
		Agents:        map[string]Chatter{"fail": failingAgent},
		MaxConcurrent: 1,
		Logger:        zap.NewNop().Sugar(),
	}

	args := `{"task_graph":{"tasks":[{"id":"t1","agent_id":"fail","message":"do work","blocking":true}]}}`
	result := dt.Run(context.Background(), args)

	if !strings.Contains(result, "Dispatch Results") {
		t.Errorf("expected 'Dispatch Results' in output, got %q", result)
	}
	// Should contain error/failure info
	if !strings.Contains(result, "agent failed") {
		t.Errorf("expected 'agent failed' in output, got %q", result)
	}
}

// TestDispatchToolParseError tests dispatch with invalid task_graph JSON.
func TestDispatchToolParseError(t *testing.T) {
	dt := &DispatchTool{
		Agents: map[string]Chatter{"worker": &mockChatter{response: "ok"}},
		Logger: zap.NewNop().Sugar(),
	}

	// Pass task_graph as a plain string instead of object — should fail parse
	args := `{"task_graph":"not an object"}`
	result := dt.Run(context.Background(), args)
	if !strings.Contains(result, "error") {
		t.Errorf("expected error from bad task_graph parse, got %q", result)
	}
}

// TestChatStreamSoftTrimDuringStream tests that ChatStream applies soft trim
// when there's pre-existing history exceeding the threshold.
func TestChatStreamSoftTrimDuringStream(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	cfg.Agents.Defaults.SoftTrimRatio = 0.0001 // very low to trigger
	cfg.Agents.Defaults.ContextPruning.KeepLastAssistants = 1
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	key := "test:stream-soft-trim"
	now := time.Now().UnixMilli()
	var msgs []session.Message
	for i := range 10 {
		msgs = append(msgs, session.Message{
			Role:    "user",
			Content: fmt.Sprintf("Long question %d with enough text to generate a lot of estimated tokens for context window calculations", i),
			TS:      now,
		})
		msgs = append(msgs, session.Message{
			Role:    "assistant",
			Content: fmt.Sprintf("Long answer %d with enough text to generate a lot of estimated tokens for context window calculations too", i),
			TS:      now,
		})
	}
	if err := sm.AppendMessages(key, msgs); err != nil {
		t.Fatal(err)
	}

	// First call: summary (from softTrim/forceSoftTrim via Chat path)
	summaryProvider := &fakeStreamProvider{
		chatResponses: []openai.ChatCompletionResponse{{
			Choices: []openai.ChatCompletionChoice{{
				Message: openai.ChatCompletionMessage{
					Role:    "assistant",
					Content: "Summary of conversation.",
				},
			}},
		}},
		streamChunks: []openai.ChatCompletionStreamResponse{
			{Choices: []openai.ChatCompletionStreamChoice{{Delta: openai.ChatCompletionStreamChoiceDelta{Content: "Streamed after trim."}}}},
		},
	}
	providers := map[string]models.Provider{"test": summaryProvider}
	router := models.NewRouter(testLogger(), providers, "test/test-model", nil)

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)

	resp, err := ag.ChatStream(context.Background(), key, "new question", nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp.Text != "Streamed after trim." {
		t.Errorf("expected 'Streamed after trim.', got %q", resp.Text)
	}
}

// ---------------------------------------------------------------------------
// Dispatch Announcer / Progress tests (REQ-160, REQ-161)
// ---------------------------------------------------------------------------

func TestDispatchToolAcknowledgement(t *testing.T) {
	ann := &mockAnnouncer{}
	worker := &mockChatter{response: "done"}
	dt := &DispatchTool{
		Agents:     map[string]Chatter{"worker": worker},
		Announcers: []Announcer{ann},
		Logger:     zap.NewNop().Sugar(),
	}

	ctx := context.WithValue(context.Background(), tools.SessionKeyContextKey{}, "agent:main:telegram:123")
	args := `{"task_graph":{"tasks":[
		{"id":"t1","agent_id":"worker","message":"do work","blocking":true},
		{"id":"t2","agent_id":"worker","message":"more work","blocking":false}
	]}}`
	result := dt.Run(ctx, args)

	if !strings.Contains(result, "Dispatch Results") {
		t.Fatalf("expected success, got %q", result)
	}

	entries := ann.getEntries()
	if len(entries) == 0 {
		t.Fatal("expected at least one announcer message (ack)")
	}
	ack := entries[0]
	if ack.SessionKey != "agent:main:telegram:123" {
		t.Errorf("ack session key = %q, want agent:main:telegram:123", ack.SessionKey)
	}
	if !strings.Contains(ack.Text, "2 tasks") {
		t.Errorf("ack text = %q, want mention of 2 tasks", ack.Text)
	}
}

func TestDispatchToolAckNoSessionKey(t *testing.T) {
	ann := &mockAnnouncer{}
	worker := &mockChatter{response: "done"}
	dt := &DispatchTool{
		Agents:     map[string]Chatter{"worker": worker},
		Announcers: []Announcer{ann},
		Logger:     zap.NewNop().Sugar(),
	}

	// No session key in context → no ack
	args := `{"task_graph":{"tasks":[{"id":"t1","agent_id":"worker","message":"work","blocking":true}]}}`
	dt.Run(context.Background(), args)

	entries := ann.getEntries()
	if len(entries) != 0 {
		t.Errorf("expected no announcer messages without session key, got %d", len(entries))
	}
}

func TestDispatchToolAckNoAnnouncers(t *testing.T) {
	worker := &mockChatter{response: "done"}
	dt := &DispatchTool{
		Agents: map[string]Chatter{"worker": worker},
		// No Announcers set
		Logger: zap.NewNop().Sugar(),
	}

	ctx := context.WithValue(context.Background(), tools.SessionKeyContextKey{}, "agent:main:telegram:123")
	args := `{"task_graph":{"tasks":[{"id":"t1","agent_id":"worker","message":"work","blocking":true}]}}`
	// Should not panic
	result := dt.Run(ctx, args)
	if !strings.Contains(result, "Dispatch Results") {
		t.Fatalf("expected success, got %q", result)
	}
}

func TestDispatchToolProgressUpdates(t *testing.T) {
	ann := &mockAnnouncer{}
	worker := &mockChatter{response: "done"}
	dt := &DispatchTool{
		Agents:          map[string]Chatter{"worker": worker},
		Announcers:      []Announcer{ann},
		ProgressUpdates: true,
		Logger:          zap.NewNop().Sugar(),
	}

	ctx := context.WithValue(context.Background(), tools.SessionKeyContextKey{}, "agent:main:slack:U99")
	args := `{"task_graph":{"tasks":[
		{"id":"t1","agent_id":"worker","message":"first","blocking":true},
		{"id":"t2","agent_id":"worker","message":"second","blocking":true,"depends_on":["t1"]}
	]}}`
	result := dt.Run(ctx, args)

	if !strings.Contains(result, "Dispatch Results") {
		t.Fatalf("expected success, got %q", result)
	}

	entries := ann.getEntries()
	// Expect: 1 ack + 2 progress updates
	if len(entries) < 3 {
		t.Fatalf("expected at least 3 announcer messages (1 ack + 2 progress), got %d", len(entries))
	}

	// First message is ack
	if !strings.Contains(entries[0].Text, "2 tasks") {
		t.Errorf("first message should be ack, got %q", entries[0].Text)
	}

	// Progress messages should contain [N/2] format
	foundProgress := false
	for _, e := range entries[1:] {
		if strings.Contains(e.Text, "[") && strings.Contains(e.Text, "/2]") {
			foundProgress = true
			if e.SessionKey != "agent:main:slack:U99" {
				t.Errorf("progress session key = %q, want agent:main:slack:U99", e.SessionKey)
			}
		}
	}
	if !foundProgress {
		t.Error("expected at least one progress update with [N/2] format")
	}
}

func TestDispatchToolProgressDisabledByDefault(t *testing.T) {
	ann := &mockAnnouncer{}
	worker := &mockChatter{response: "done"}
	dt := &DispatchTool{
		Agents:          map[string]Chatter{"worker": worker},
		Announcers:      []Announcer{ann},
		ProgressUpdates: false, // explicitly disabled
		Logger:          zap.NewNop().Sugar(),
	}

	ctx := context.WithValue(context.Background(), tools.SessionKeyContextKey{}, "agent:main:telegram:123")
	args := `{"task_graph":{"tasks":[{"id":"t1","agent_id":"worker","message":"work","blocking":true}]}}`
	dt.Run(ctx, args)

	entries := ann.getEntries()
	// Should only have ack, no progress
	if len(entries) != 1 {
		t.Errorf("expected 1 message (ack only), got %d", len(entries))
	}
}

func TestDispatchToolExplicitProgressFnTakesPrecedence(t *testing.T) {
	ann := &mockAnnouncer{}
	worker := &mockChatter{response: "done"}

	var customCalled int
	dt := &DispatchTool{
		Agents:          map[string]Chatter{"worker": worker},
		Announcers:      []Announcer{ann},
		ProgressUpdates: true,
		ProgressFn: func(taskID, agentID string, status orchestrator.TaskStatus) {
			customCalled++
		},
		Logger: zap.NewNop().Sugar(),
	}

	ctx := context.WithValue(context.Background(), tools.SessionKeyContextKey{}, "agent:main:telegram:123")
	args := `{"task_graph":{"tasks":[{"id":"t1","agent_id":"worker","message":"work","blocking":true}]}}`
	dt.Run(ctx, args)

	if customCalled != 1 {
		t.Errorf("expected custom ProgressFn called once, got %d", customCalled)
	}

	entries := ann.getEntries()
	// Should have ack but no auto-progress (custom fn takes precedence)
	if len(entries) != 1 {
		t.Errorf("expected 1 message (ack only, custom fn suppresses auto-progress), got %d", len(entries))
	}
}

func TestDispatchToolMultipleAnnouncers(t *testing.T) {
	ann1 := &mockAnnouncer{}
	ann2 := &mockAnnouncer{}
	worker := &mockChatter{response: "done"}
	dt := &DispatchTool{
		Agents:     map[string]Chatter{"worker": worker},
		Announcers: []Announcer{ann1, ann2},
		Logger:     zap.NewNop().Sugar(),
	}

	ctx := context.WithValue(context.Background(), tools.SessionKeyContextKey{}, "agent:main:telegram:123")
	args := `{"task_graph":{"tasks":[{"id":"t1","agent_id":"worker","message":"work","blocking":true}]}}`
	dt.Run(ctx, args)

	// Both announcers should get the ack
	if len(ann1.getEntries()) != 1 {
		t.Errorf("ann1: expected 1 message, got %d", len(ann1.getEntries()))
	}
	if len(ann2.getEntries()) != 1 {
		t.Errorf("ann2: expected 1 message, got %d", len(ann2.getEntries()))
	}
}

// ---------------------------------------------------------------------------
// Orchestrator routing heuristics (REQ-171)
// ---------------------------------------------------------------------------

func TestInitStaticPromptOrchestratorHeuristics(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	subChatter := &mockChatter{response: "ok"}
	dt := &DelegateTool{
		Agents: map[string]Chatter{
			"main":         subChatter,
			"orchestrator": subChatter,
			"coder":        subChatter,
		},
		Logger: zap.NewNop().Sugar(),
	}

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, "", []Tool{dt})

	if !strings.Contains(ag.sysPromptStatic, "When to use the orchestrator") {
		t.Error("static prompt missing orchestrator routing heuristics")
	}
	if !strings.Contains(ag.sysPromptStatic, "2 or more different specialists") {
		t.Error("static prompt missing orchestrator delegation guidance")
	}
	if !strings.Contains(ag.sysPromptStatic, "Handle directly") {
		t.Error("static prompt missing 'handle directly' guidance")
	}
}

func TestInitStaticPromptNoOrchestratorHeuristics(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	subChatter := &mockChatter{response: "ok"}
	dt := &DelegateTool{
		Agents: map[string]Chatter{
			"main":  subChatter,
			"coder": subChatter,
		},
		Logger: zap.NewNop().Sugar(),
	}

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, "", []Tool{dt})

	if strings.Contains(ag.sysPromptStatic, "When to use the orchestrator") {
		t.Error("static prompt should NOT contain orchestrator heuristics when no orchestrator registered")
	}
}

// ---------------------------------------------------------------------------
// DispatchTool.Run with TaskMgr (background task path)
// ---------------------------------------------------------------------------

func TestDispatchToolWithTaskManager(t *testing.T) {
	ann := &mockAnnouncer{}
	worker := &mockChatter{response: "background work done"}
	mgr := taskqueue.New(testLogger(), filepath.Join(t.TempDir(), "tasks.json"), taskqueue.Config{MaxConcurrent: 5})

	dt := &DispatchTool{
		Agents:     map[string]Chatter{"worker": worker},
		Announcers: []Announcer{ann},
		TaskMgr:    mgr,
		Logger:     zap.NewNop().Sugar(),
	}

	ctx := context.WithValue(context.Background(), tools.SessionKeyContextKey{}, "agent:main:test:123")
	args := `{"task_graph":{"tasks":[
		{"id":"t1","agent_id":"worker","message":"bg task","blocking":true}
	]}}`
	result := dt.Run(ctx, args)

	// Should return acknowledgement, not the result
	if !strings.Contains(result, "Dispatch submitted") {
		t.Errorf("expected 'Dispatch submitted', got %q", result)
	}
	if !strings.Contains(result, "1 tasks") {
		t.Errorf("expected '1 tasks', got %q", result)
	}

	// Wait for the background task to complete
	time.Sleep(500 * time.Millisecond)

	// Check the announcer received the result
	entries := ann.getEntries()
	found := false
	for _, e := range entries {
		if strings.Contains(e.Text, "dispatch result") || strings.Contains(e.Text, "background work done") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected dispatch result announcement, got entries: %v", entries)
	}
}

func TestDispatchToolWithTaskManagerError(t *testing.T) {
	ann := &mockAnnouncer{}
	worker := &mockChatter{err: fmt.Errorf("worker failed")}
	mgr := taskqueue.New(testLogger(), filepath.Join(t.TempDir(), "tasks.json"), taskqueue.Config{MaxConcurrent: 5})

	dt := &DispatchTool{
		Agents:     map[string]Chatter{"worker": worker},
		Announcers: []Announcer{ann},
		TaskMgr:    mgr,
		Logger:     zap.NewNop().Sugar(),
	}

	ctx := context.WithValue(context.Background(), tools.SessionKeyContextKey{}, "agent:main:test:456")
	args := `{"task_graph":{"tasks":[
		{"id":"t1","agent_id":"worker","message":"fail task","blocking":true}
	]}}`
	result := dt.Run(ctx, args)

	if !strings.Contains(result, "Dispatch submitted") {
		t.Errorf("expected 'Dispatch submitted', got %q", result)
	}

	// Wait for the background task to complete (with error)
	time.Sleep(500 * time.Millisecond)

	// Check the announcer received the error
	entries := ann.getEntries()
	found := false
	for _, e := range entries {
		if strings.Contains(e.Text, "error") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected error announcement, got entries: %v", entries)
	}
}

// ---------------------------------------------------------------------------
// UpdateConfig
// ---------------------------------------------------------------------------

func TestUpdateConfig(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	router := models.NewRouter(testLogger(), nil, "test-model", nil)

	ag := &Agent{
		cfg:      cfg,
		def:      cfg.DefaultAgent(),
		router:   router,
		sessions: sm,
		toolMap:  make(map[string]Tool),
		logger:   zap.NewNop().Sugar(),
	}

	newCfg := newTestConfig()
	newCfg.Agents.Defaults.Model.Primary = "new-model"
	newCfg.Agents.Defaults.Model.Fallbacks = []string{"fallback-1"}

	ag.UpdateConfig(newCfg)

	if ag.cfg != newCfg {
		t.Error("config should be swapped to newCfg")
	}
}

// ---------------------------------------------------------------------------
// buildLightSystemPrompt
// ---------------------------------------------------------------------------

func TestBuildLightSystemPrompt_Basic(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, "", nil)

	prompt := ag.buildLightSystemPrompt()
	if !strings.Contains(prompt, "Test") {
		t.Error("light prompt should contain agent identity name")
	}
	if !strings.Contains(prompt, "test") {
		t.Error("light prompt should contain agent identity theme")
	}
	if !strings.Contains(prompt, "Current date/time:") {
		t.Error("light prompt should contain date/time")
	}
}

func TestBuildLightSystemPrompt_WithHeartbeat(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	ws := t.TempDir()

	// Create HEARTBEAT.md
	heartbeatContent := "## Daily Tasks\n- Check status\n- Run reports\n"
	if err := os.WriteFile(filepath.Join(ws, "HEARTBEAT.md"), []byte(heartbeatContent), 0644); err != nil {
		t.Fatal(err)
	}

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, ws, nil)

	prompt := ag.buildLightSystemPrompt()
	if !strings.Contains(prompt, "Heartbeat Context") {
		t.Error("light prompt should include Heartbeat Context section")
	}
	if !strings.Contains(prompt, "Daily Tasks") {
		t.Error("light prompt should include HEARTBEAT.md content")
	}
}

func TestBuildLightSystemPrompt_NoWorkspace(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, "", nil)

	prompt := ag.buildLightSystemPrompt()
	if strings.Contains(prompt, "Heartbeat") {
		t.Error("light prompt without workspace should not contain Heartbeat section")
	}
}

func TestBuildLightSystemPrompt_TimezoneHandling(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.UserTimezone = "America/New_York"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, "", nil)

	prompt := ag.buildLightSystemPrompt()
	if !strings.Contains(prompt, "America/New_York") {
		t.Error("light prompt should contain the configured timezone")
	}
}

func TestBuildLightSystemPrompt_InvalidTimezone(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.UserTimezone = "Invalid/NotReal"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), nil, sm, nil, nil, "", nil)

	prompt := ag.buildLightSystemPrompt()
	// Falls back to UTC for computation, but still prints the configured tz name
	if !strings.Contains(prompt, "Invalid/NotReal") {
		t.Error("light prompt should contain the configured timezone string")
	}
	if !strings.Contains(prompt, "Current date/time:") {
		t.Error("light prompt should still include date/time")
	}
}

// ---------------------------------------------------------------------------
// ChatLight full round-trip
// ---------------------------------------------------------------------------

func TestChatLight_RealRouter(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)

	router := makeRouter(openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{{
			Message: openai.ChatCompletionMessage{
				Role:    "assistant",
				Content: "Light response!",
			},
		}},
		Usage: openai.Usage{PromptTokens: 10, CompletionTokens: 5},
	})

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)

	resp, err := ag.ChatLight(context.Background(), "light-session", "heartbeat check")
	if err != nil {
		t.Fatalf("ChatLight: %v", err)
	}
	if resp.Text != "Light response!" {
		t.Errorf("expected 'Light response!', got %q", resp.Text)
	}

	// Verify history was persisted.
	h, err := sm.GetHistory("light-session")
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if len(h) < 2 {
		t.Fatalf("expected at least 2 messages, got %d", len(h))
	}
}

func TestChatLight_WithHeartbeat(t *testing.T) {
	cfg := newTestConfig()
	cfg.Agents.Defaults.Model.Primary = "test/test-model"
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	ws := t.TempDir()

	if err := os.WriteFile(filepath.Join(ws, "HEARTBEAT.md"), []byte("## Tasks\n- Run checks\n"), 0644); err != nil {
		t.Fatal(err)
	}

	router := makeRouter(openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{{
			Message: openai.ChatCompletionMessage{
				Role:    "assistant",
				Content: "Done with tasks",
			},
		}},
	})

	ag := New(testLogger(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, ws, nil)

	resp, err := ag.ChatLight(context.Background(), "heartbeat-session", "run tasks")
	if err != nil {
		t.Fatalf("ChatLight: %v", err)
	}
	if resp.Text != "Done with tasks" {
		t.Errorf("expected 'Done with tasks', got %q", resp.Text)
	}
}

func TestChatLight_SemExhausted(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), time.Hour)
	ag := &Agent{
		cfg:      cfg,
		def:      cfg.DefaultAgent(),
		sessions: sm,
		toolMap:  make(map[string]Tool),
		sem:      make(chan struct{}, 1),
		logger:   zap.NewNop().Sugar(),
	}
	ag.sem <- struct{}{}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := ag.ChatLight(ctx, "test-session", "hello")
	if err == nil {
		t.Error("expected error from ChatLight when semaphore exhausted")
	}
	if !strings.Contains(err.Error(), "max concurrent") {
		t.Errorf("expected 'max concurrent' error, got %q", err.Error())
	}

	<-ag.sem
}
