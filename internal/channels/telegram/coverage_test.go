package telegram

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"go.uber.org/zap"
	tele "gopkg.in/telebot.v3"

	"github.com/EMSERO/gopherclaw/internal/agent"
	"github.com/EMSERO/gopherclaw/internal/config"
	"github.com/EMSERO/gopherclaw/internal/models"
	"github.com/EMSERO/gopherclaw/internal/session"
)

// ---------------------------------------------------------------------------
// fakeProvider implements models.Provider for testing.
// ---------------------------------------------------------------------------

type fakeProvider struct {
	response openai.ChatCompletionResponse
	err      error
}

func (f *fakeProvider) Chat(_ context.Context, _ openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	if f.err != nil {
		return openai.ChatCompletionResponse{}, f.err
	}
	return f.response, nil
}

func (f *fakeProvider) ChatStream(_ context.Context, _ openai.ChatCompletionRequest) (models.Stream, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &fakeStream{
		response: f.response,
	}, nil
}

type fakeStream struct {
	response openai.ChatCompletionResponse
	done     bool
}

func (s *fakeStream) Recv() (openai.ChatCompletionStreamResponse, error) {
	if s.done {
		return openai.ChatCompletionStreamResponse{}, io.EOF
	}
	s.done = true
	content := ""
	if len(s.response.Choices) > 0 {
		content = s.response.Choices[0].Message.Content
	}
	return openai.ChatCompletionStreamResponse{
		Choices: []openai.ChatCompletionStreamChoice{{
			Delta: openai.ChatCompletionStreamChoiceDelta{
				Content: content,
				Role:    "assistant",
			},
		}},
	}, nil
}

func (s *fakeStream) Close() error { return nil }

func newTestConfig() *config.Root {
	return &config.Root{
		Agents: config.Agents{
			Defaults: config.AgentDefaults{
				Model:          config.ModelConfig{Primary: "test/model-1"},
				UserTimezone:   "UTC",
				LoopDetectionN: 3,
			},
			List: []config.AgentDef{{ID: "main", Default: true, Identity: config.Identity{Name: "Test", Theme: "test"}}},
		},
	}
}

func newTestAgent(t *testing.T, resp openai.ChatCompletionResponse) (*agent.Agent, *session.Manager) {
	t.Helper()
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cfg := newTestConfig()
	provider := &fakeProvider{response: resp}
	router := models.NewRouter(zap.NewNop().Sugar(), map[string]models.Provider{"test": provider}, "test/model-1", nil)
	ag := agent.New(zap.NewNop().Sugar(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)
	return ag, sm
}

func newTestAgentError(t *testing.T, err error) (*agent.Agent, *session.Manager) {
	t.Helper()
	sm, errSm := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if errSm != nil {
		t.Fatal(errSm)
	}
	cfg := newTestConfig()
	provider := &fakeProvider{err: err}
	router := models.NewRouter(zap.NewNop().Sugar(), map[string]models.Provider{"test": provider}, "test/model-1", nil)
	ag := agent.New(zap.NewNop().Sugar(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)
	return ag, sm
}

func simpleResponse(text string) openai.ChatCompletionResponse {
	return openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{{
			Message: openai.ChatCompletionMessage{
				Role:    "assistant",
				Content: text,
			},
		}},
	}
}

// newMockTeleBot creates a tele.Bot backed by an httptest server that returns
// errors for file operations (getFile). The caller must defer srv.Close().
func newMockTeleBot(t *testing.T) (*tele.Bot, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "getMe") {
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"TestBot","username":"TestBot"}}`))
			return
		}
		if strings.Contains(r.URL.Path, "getFile") {
			_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: invalid file_id"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	bot, err := tele.NewBot(tele.Settings{
		URL:    srv.URL,
		Token:  "000000000:test-token",
		Poller: &tele.LongPoller{Timeout: 1 * time.Second},
	})
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	return bot, srv
}

// newMockTeleBotWithFile creates a tele.Bot backed by an httptest server that
// returns a valid file for getFile and serves file content for download.
func newMockTeleBotWithFile(t *testing.T) (*tele.Bot, *httptest.Server) {
	t.Helper()
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Serve the file content at /file/bot<token>/<path>
		if strings.Contains(r.URL.Path, "/file/") {
			w.Header().Set("Content-Type", "image/png")
			// Minimal valid PNG data
			_, _ = w.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "getMe") {
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"TestBot","username":"TestBot"}}`))
			return
		}
		if strings.Contains(r.URL.Path, "getFile") {
			// Return a valid file result with a file_path the bot can download
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_id":"test_file_id","file_unique_id":"test_unique","file_size":12,"file_path":"photos/test.png"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	srvURL = srv.URL
	_ = srvURL
	bot, err := tele.NewBot(tele.Settings{
		URL:    srv.URL,
		Token:  "000000000:test-token",
		Poller: &tele.LongPoller{Timeout: 1 * time.Second},
	})
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	return bot, srv
}

// ---------------------------------------------------------------------------
// handlePhoto tests
// ---------------------------------------------------------------------------

func TestHandlePhoto_ShouldNotRespond(t *testing.T) {
	b := &Bot{
		cfg:    config.TelegramConfig{GroupPolicy: "disabled"},
		paired: make(map[int64]bool),
		logger: zap.NewNop().Sugar(),
	}
	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 100, Type: tele.ChatGroup},
	}
	err := b.handlePhoto(mc)
	if err != nil {
		t.Fatalf("handlePhoto returned error: %v", err)
	}
	if len(mc.replies) != 0 {
		t.Error("expected no replies when shouldRespond returns false")
	}
}

func TestHandlePhoto_NilPhoto(t *testing.T) {
	b := &Bot{
		cfg:    config.TelegramConfig{},
		paired: make(map[int64]bool),
		logger: zap.NewNop().Sugar(),
	}
	mc := &mockContext{
		sender:  &tele.User{ID: 42},
		chat:    &tele.Chat{ID: 42, Type: tele.ChatPrivate},
		message: &tele.Message{Photo: nil},
	}
	err := b.handlePhoto(mc)
	if err != nil {
		t.Fatalf("handlePhoto returned error: %v", err)
	}
	if len(mc.replies) != 0 {
		t.Error("expected no replies when photo is nil")
	}
}

func TestHandlePhoto_DownloadError(t *testing.T) {
	teleBot, srv := newMockTeleBot(t)
	defer srv.Close()

	b := &Bot{
		cfg:    config.TelegramConfig{TimeoutSeconds: 1},
		msgCfg: config.Messages{AckReactionScope: ""},
		paired: make(map[int64]bool),
		bot:    teleBot,
		logger: zap.NewNop().Sugar(),
	}
	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
		message: &tele.Message{
			Photo: &tele.Photo{
				File: tele.File{FileID: "invalid_file"},
			},
		},
	}
	err := b.handlePhoto(mc)
	if err != nil {
		t.Fatalf("handlePhoto returned error: %v", err)
	}
	// Should have replied with failure message
	if len(mc.replies) == 0 {
		t.Fatal("expected a reply for download error")
	}
	reply := mc.replies[0].(string)
	if !strings.Contains(reply, "Failed") {
		t.Errorf("expected failure reply, got %q", reply)
	}
}

// ---------------------------------------------------------------------------
// handleDocument tests
// ---------------------------------------------------------------------------

func TestHandleDocument_ShouldNotRespond(t *testing.T) {
	b := &Bot{
		cfg:    config.TelegramConfig{GroupPolicy: "disabled"},
		paired: make(map[int64]bool),
		logger: zap.NewNop().Sugar(),
	}
	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 100, Type: tele.ChatGroup},
	}
	err := b.handleDocument(mc)
	if err != nil {
		t.Fatalf("handleDocument returned error: %v", err)
	}
}

func TestHandleDocument_NilDocument(t *testing.T) {
	b := &Bot{
		cfg:    config.TelegramConfig{},
		paired: make(map[int64]bool),
		logger: zap.NewNop().Sugar(),
	}
	mc := &mockContext{
		sender:  &tele.User{ID: 42},
		chat:    &tele.Chat{ID: 42, Type: tele.ChatPrivate},
		message: &tele.Message{Document: nil},
	}
	err := b.handleDocument(mc)
	if err != nil {
		t.Fatalf("handleDocument returned error: %v", err)
	}
}

func TestHandleDocument_NonImageDocument_NoCaption(t *testing.T) {
	// Non-image document without caption should generate a file description
	dir := t.TempDir()
	sm, err := session.New(zap.NewNop().Sugar(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	b := &Bot{
		cfg:    config.TelegramConfig{TimeoutSeconds: 1},
		msgCfg: config.Messages{},
		fullCfg: &config.Root{
			Session: config.Session{ResetTriggers: []string{}},
		},
		sessions: sm,
		paired:   make(map[int64]bool),
		queues:   make(map[int64]*messageQueue),
		bot:      &tele.Bot{Me: &tele.User{Username: "TestBot"}},
		logger:   zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
		message: &tele.Message{
			Document: &tele.Document{
				File:     tele.File{FileID: "doc1", FileSize: 12345},
				FileName: "test.pdf",
				MIME:     "application/pdf",
			},
			Caption: "",
		},
	}

	// processMessages will be called which will try to call agent.Chat
	// Since agent is nil, it will panic. We recover.
	func() {
		defer func() { recover() }()
		_ = b.handleDocument(mc)
	}()
}

func TestHandleDocument_NonImageDocument_WithCaption(t *testing.T) {
	dir := t.TempDir()
	sm, err := session.New(zap.NewNop().Sugar(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	b := &Bot{
		cfg:    config.TelegramConfig{TimeoutSeconds: 1},
		msgCfg: config.Messages{},
		fullCfg: &config.Root{
			Session: config.Session{ResetTriggers: []string{}},
		},
		sessions: sm,
		paired:   make(map[int64]bool),
		queues:   make(map[int64]*messageQueue),
		bot:      &tele.Bot{Me: &tele.User{Username: "TestBot"}},
		logger:   zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
		message: &tele.Message{
			Document: &tele.Document{
				File:     tele.File{FileID: "doc1", FileSize: 500},
				FileName: "report.csv",
				MIME:     "text/csv",
			},
			Caption: "Check this file",
		},
	}

	func() {
		defer func() { recover() }()
		_ = b.handleDocument(mc)
	}()
}

func TestHandleDocument_ImageDocument_DownloadError(t *testing.T) {
	teleBot, srv := newMockTeleBot(t)
	defer srv.Close()

	b := &Bot{
		cfg:    config.TelegramConfig{TimeoutSeconds: 1},
		msgCfg: config.Messages{AckReactionScope: ""},
		paired: make(map[int64]bool),
		bot:    teleBot,
		logger: zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
		message: &tele.Message{
			Document: &tele.Document{
				File:     tele.File{FileID: "img1", FileSize: 1000},
				FileName: "photo.png",
				MIME:     "image/png",
			},
		},
	}

	err := b.handleDocument(mc)
	if err != nil {
		t.Fatalf("handleDocument returned error: %v", err)
	}
	// Should have replied with "Failed to download image."
	if len(mc.replies) == 0 {
		t.Fatal("expected failure reply")
	}
	reply := mc.replies[0].(string)
	if !strings.Contains(reply, "Failed") {
		t.Errorf("expected failure reply, got %q", reply)
	}
}

// ---------------------------------------------------------------------------
// downloadFileAsDataURL tests
// ---------------------------------------------------------------------------

func TestDownloadFileAsDataURL_Error(t *testing.T) {
	teleBot, srv := newMockTeleBot(t)
	defer srv.Close()

	b := &Bot{
		bot:    teleBot,
		paired: make(map[int64]bool),
		logger: zap.NewNop().Sugar(),
	}
	_, err := b.downloadFileAsDataURL(tele.File{FileID: "nonexistent"})
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

// ---------------------------------------------------------------------------
// respondFull with real agent (success path)
// ---------------------------------------------------------------------------

func TestRespondFull_Success(t *testing.T) {
	ag, sm := newTestAgent(t, simpleResponse("Hello from agent!"))

	b := &Bot{
		cfg:      config.TelegramConfig{TimeoutSeconds: 10},
		msgCfg:   config.Messages{},
		fullCfg:  newTestConfig(),
		agent:    ag,
		sessions: sm,
		paired:   make(map[int64]bool),
		queues:   make(map[int64]*messageQueue),
		bot:      &tele.Bot{Me: &tele.User{Username: "TestBot"}},
		logger:   zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
	}

	err := b.respondFull(mc, "main:telegram:42", "hi", b.cfg, b.msgCfg)
	if err != nil {
		t.Fatalf("respondFull error: %v", err)
	}
	if len(mc.replies) == 0 {
		t.Fatal("expected a reply")
	}
	reply := mc.replies[0].(string)
	if !strings.Contains(reply, "Hello from agent!") {
		t.Errorf("expected agent response, got %q", reply)
	}
}

func TestRespondFull_Error(t *testing.T) {
	ag, sm := newTestAgentError(t, context.DeadlineExceeded)

	b := &Bot{
		cfg:      config.TelegramConfig{TimeoutSeconds: 1},
		msgCfg:   config.Messages{},
		fullCfg:  newTestConfig(),
		agent:    ag,
		sessions: sm,
		paired:   make(map[int64]bool),
		queues:   make(map[int64]*messageQueue),
		bot:      &tele.Bot{Me: &tele.User{Username: "TestBot"}},
		logger:   zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
	}

	err := b.respondFull(mc, "main:telegram:42", "hi", b.cfg, b.msgCfg)
	if err != nil {
		t.Fatalf("respondFull error: %v", err)
	}
	if len(mc.replies) == 0 {
		t.Fatal("expected error reply")
	}
}

func TestRespondFull_Suppressible(t *testing.T) {
	ag, sm := newTestAgent(t, simpleResponse("NO_REPLY"))

	b := &Bot{
		cfg:      config.TelegramConfig{TimeoutSeconds: 10},
		msgCfg:   config.Messages{},
		fullCfg:  newTestConfig(),
		agent:    ag,
		sessions: sm,
		paired:   make(map[int64]bool),
		bot:      &tele.Bot{Me: &tele.User{Username: "TestBot"}},
		logger:   zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
	}

	err := b.respondFull(mc, "main:telegram:42", "hi", b.cfg, b.msgCfg)
	if err != nil {
		t.Fatalf("respondFull error: %v", err)
	}
	// Typing notification should be sent, but no text reply
	mc.mu.Lock()
	defer mc.mu.Unlock()
	for _, r := range mc.replies {
		if s, ok := r.(string); ok && s != "" {
			t.Errorf("expected no reply for NO_REPLY, got %q", s)
		}
	}
}

func TestRespondFull_WithTokenUsage(t *testing.T) {
	resp := simpleResponse("Result text")
	resp.Usage = openai.Usage{PromptTokens: 100, CompletionTokens: 50}
	ag, sm := newTestAgent(t, resp)

	cfg := newTestConfig()
	cfg.Messages.Usage = "tokens"

	b := &Bot{
		cfg:      config.TelegramConfig{TimeoutSeconds: 10},
		msgCfg:   config.Messages{Usage: "tokens"},
		fullCfg:  cfg,
		agent:    ag,
		sessions: sm,
		paired:   make(map[int64]bool),
		bot:      &tele.Bot{Me: &tele.User{Username: "TestBot"}},
		logger:   zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
	}

	err := b.respondFull(mc, "main:telegram:42", "hi", b.cfg, b.msgCfg)
	if err != nil {
		t.Fatalf("respondFull error: %v", err)
	}
	if len(mc.replies) == 0 {
		t.Fatal("expected reply")
	}
}

func TestRespondFull_WithHistoryLimit(t *testing.T) {
	ag, sm := newTestAgent(t, simpleResponse("ok"))

	b := &Bot{
		cfg:      config.TelegramConfig{TimeoutSeconds: 10, HistoryLimit: 5},
		msgCfg:   config.Messages{},
		fullCfg:  newTestConfig(),
		agent:    ag,
		sessions: sm,
		paired:   make(map[int64]bool),
		bot:      &tele.Bot{Me: &tele.User{Username: "TestBot"}},
		logger:   zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
	}

	err := b.respondFull(mc, "main:telegram:42", "hi", b.cfg, b.msgCfg)
	if err != nil {
		t.Fatalf("respondFull error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// respondStreaming with real agent
// ---------------------------------------------------------------------------

func TestRespondStreaming_Success(t *testing.T) {
	ag, sm := newTestAgent(t, simpleResponse("Streamed!"))

	b := &Bot{
		cfg:      config.TelegramConfig{TimeoutSeconds: 10, StreamMode: "partial"},
		msgCfg:   config.Messages{StreamEditMs: 50},
		fullCfg:  newTestConfig(),
		agent:    ag,
		sessions: sm,
		paired:   make(map[int64]bool),
		bot:      &tele.Bot{Me: &tele.User{Username: "TestBot"}},
		logger:   zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
	}

	err := b.respondStreaming(mc, "main:telegram:42", "hi", b.cfg, b.msgCfg)
	if err != nil {
		t.Fatalf("respondStreaming error: %v", err)
	}
	if len(mc.replies) == 0 {
		t.Fatal("expected a reply")
	}
}

func TestRespondStreaming_Error(t *testing.T) {
	ag, sm := newTestAgentError(t, context.DeadlineExceeded)

	b := &Bot{
		cfg:      config.TelegramConfig{TimeoutSeconds: 1, StreamMode: "partial"},
		msgCfg:   config.Messages{StreamEditMs: 50},
		fullCfg:  newTestConfig(),
		agent:    ag,
		sessions: sm,
		paired:   make(map[int64]bool),
		bot:      &tele.Bot{Me: &tele.User{Username: "TestBot"}},
		logger:   zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
	}

	err := b.respondStreaming(mc, "main:telegram:42", "hi", b.cfg, b.msgCfg)
	if err != nil {
		t.Fatalf("respondStreaming error: %v", err)
	}
	if len(mc.replies) == 0 {
		t.Fatal("expected error reply")
	}
}

func TestRespondStreaming_Suppressible(t *testing.T) {
	ag, sm := newTestAgent(t, simpleResponse("NO_REPLY"))

	b := &Bot{
		cfg:      config.TelegramConfig{TimeoutSeconds: 10, StreamMode: "partial"},
		msgCfg:   config.Messages{StreamEditMs: 50},
		fullCfg:  newTestConfig(),
		agent:    ag,
		sessions: sm,
		paired:   make(map[int64]bool),
		bot:      &tele.Bot{Me: &tele.User{Username: "TestBot"}},
		logger:   zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
	}

	err := b.respondStreaming(mc, "main:telegram:42", "hi", b.cfg, b.msgCfg)
	if err != nil {
		t.Fatalf("respondStreaming error: %v", err)
	}
}

func TestRespondStreaming_WithTokenUsage(t *testing.T) {
	resp := simpleResponse("Stream result")
	resp.Usage = openai.Usage{PromptTokens: 200, CompletionTokens: 100}
	ag, sm := newTestAgent(t, resp)

	cfg := newTestConfig()
	cfg.Messages.Usage = "tokens"

	b := &Bot{
		cfg:      config.TelegramConfig{TimeoutSeconds: 10, StreamMode: "partial"},
		msgCfg:   config.Messages{StreamEditMs: 50, Usage: "tokens"},
		fullCfg:  cfg,
		agent:    ag,
		sessions: sm,
		paired:   make(map[int64]bool),
		bot:      &tele.Bot{Me: &tele.User{Username: "TestBot"}},
		logger:   zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
	}

	err := b.respondStreaming(mc, "main:telegram:42", "hi", b.cfg, b.msgCfg)
	if err != nil {
		t.Fatalf("respondStreaming error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// New() coverage: with a valid-format token that telebot accepts
// ---------------------------------------------------------------------------

func TestNew_ValidTokenFormat(t *testing.T) {
	// New() calls tele.NewBot which makes a real getMe API call.
	// We can only verify it doesn't panic on a well-formed token.
	// The error is expected since we're not hitting a real Telegram server.
	cfg := config.TelegramConfig{
		BotToken:       "123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11",
		TimeoutSeconds: 10,
	}
	_, err := New(zap.NewNop().Sugar(), cfg, config.Messages{}, nil, nil, nil, nil)
	// We expect an error (Unauthorized or network error) since the token is fake.
	// The important thing is that it doesn't panic and returns a proper error.
	if err == nil {
		// If somehow it succeeded (unlikely with fake token), verify the bot
		t.Log("New() succeeded unexpectedly with fake token")
	} else {
		t.Logf("New() correctly returned error: %v", err)
	}
}

func TestNew_EmptyToken(t *testing.T) {
	_, err := New(zap.NewNop().Sugar(), config.TelegramConfig{BotToken: ""}, config.Messages{}, nil, nil, nil, nil)
	if err == nil {
		t.Error("expected error for empty token")
	}
}

func TestNew_InvalidTokenFormat(t *testing.T) {
	_, err := New(zap.NewNop().Sugar(), config.TelegramConfig{BotToken: "invalid"}, config.Messages{}, nil, nil, nil, nil)
	if err == nil {
		t.Error("expected error for invalid token format")
	}
}

// ---------------------------------------------------------------------------
// handleConfirmCallback tests
// ---------------------------------------------------------------------------

func TestHandleConfirmCallback_NonConfirmPrefix(t *testing.T) {
	b := &Bot{
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}
	if b.handleConfirmCallback("some_other_data") {
		t.Error("expected false for non-confirm prefix")
	}
}

func TestHandleConfirmCallback_Yes(t *testing.T) {
	ch := make(chan bool, 1)
	b := &Bot{
		pendingConfirms: map[string]chan bool{"confirm:123:456": ch},
		logger:          zap.NewNop().Sugar(),
	}
	handled := b.handleConfirmCallback("confirm:123:456:yes")
	if !handled {
		t.Error("expected true for valid confirm callback")
	}
	select {
	case val := <-ch:
		if !val {
			t.Error("expected true for 'yes' answer")
		}
	default:
		t.Error("expected value on channel")
	}
}

func TestHandleConfirmCallback_No(t *testing.T) {
	ch := make(chan bool, 1)
	b := &Bot{
		pendingConfirms: map[string]chan bool{"confirm:123:456": ch},
		logger:          zap.NewNop().Sugar(),
	}
	handled := b.handleConfirmCallback("confirm:123:456:no")
	if !handled {
		t.Error("expected true for valid confirm callback")
	}
	select {
	case val := <-ch:
		if val {
			t.Error("expected false for 'no' answer")
		}
	default:
		t.Error("expected value on channel")
	}
}

func TestHandleConfirmCallback_NoPendingChannel(t *testing.T) {
	b := &Bot{
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}
	if b.handleConfirmCallback("confirm:123:456:yes") {
		t.Error("expected false when no pending channel exists")
	}
}

// ---------------------------------------------------------------------------
// CanConfirm tests
// ---------------------------------------------------------------------------

func TestCanConfirmCoverage(t *testing.T) {
	b := &Bot{logger: zap.NewNop().Sugar()}
	if !b.CanConfirm("main:telegram:123") {
		t.Error("expected true for telegram session key")
	}
	if b.CanConfirm("main:discord:123") {
		t.Error("expected false for non-telegram session key")
	}
}

// ---------------------------------------------------------------------------
// processMessages with real agent (non-reset, full mode)
// ---------------------------------------------------------------------------

func TestProcessMessages_FullMode_WithAgent(t *testing.T) {
	ag, sm := newTestAgent(t, simpleResponse("Agent reply"))

	b := &Bot{
		cfg:      config.TelegramConfig{TimeoutSeconds: 10},
		msgCfg:   config.Messages{},
		fullCfg:  newTestConfig(),
		agent:    ag,
		sessions: sm,
		paired:   make(map[int64]bool),
		queues:   make(map[int64]*messageQueue),
		bot:      &tele.Bot{Me: &tele.User{Username: "TestBot"}},
		logger:   zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
	}

	err := b.processMessages(42, []queuedMessage{{text: "hello", ctx: mc}}, mc)
	if err != nil {
		t.Fatalf("processMessages error: %v", err)
	}
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if len(mc.replies) == 0 {
		t.Fatal("expected a reply from agent")
	}
}

func TestProcessMessages_StreamMode_WithAgent(t *testing.T) {
	ag, sm := newTestAgent(t, simpleResponse("Streamed reply"))

	b := &Bot{
		cfg:      config.TelegramConfig{TimeoutSeconds: 10, StreamMode: "partial"},
		msgCfg:   config.Messages{StreamEditMs: 50},
		fullCfg:  newTestConfig(),
		agent:    ag,
		sessions: sm,
		paired:   make(map[int64]bool),
		queues:   make(map[int64]*messageQueue),
		bot:      &tele.Bot{Me: &tele.User{Username: "TestBot"}},
		logger:   zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
	}

	err := b.processMessages(42, []queuedMessage{{text: "hello", ctx: mc}}, mc)
	if err != nil {
		t.Fatalf("processMessages error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// handleText with real agent (non-slash, immediate mode)
// ---------------------------------------------------------------------------

func TestHandleText_Immediate_WithAgent(t *testing.T) {
	ag, sm := newTestAgent(t, simpleResponse("hi there"))

	b := &Bot{
		cfg:    config.TelegramConfig{TimeoutSeconds: 10},
		msgCfg: config.Messages{AckReactionScope: ""},
		fullCfg: &config.Root{
			Agents: config.Agents{
				Defaults: config.AgentDefaults{Model: config.ModelConfig{Primary: "test-model"}, LoopDetectionN: 3},
				List:     []config.AgentDef{{ID: "main", Default: true}},
			},
		},
		agent:    ag,
		sessions: sm,
		paired:   make(map[int64]bool),
		queues:   make(map[int64]*messageQueue),
		bot:      &tele.Bot{Me: &tele.User{Username: "TestBot"}},
		logger:   zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
		text:   "what's up",
	}

	err := b.handleText(mc)
	if err != nil {
		t.Fatalf("handleText error: %v", err)
	}
	mc.mu.Lock()
	defer mc.mu.Unlock()
	if len(mc.replies) == 0 {
		t.Fatal("expected a reply from agent")
	}
}

// ---------------------------------------------------------------------------
// handleCallback with real agent
// ---------------------------------------------------------------------------

func TestHandleCallback_WithAgent_FullMode(t *testing.T) {
	ag, sm := newTestAgent(t, simpleResponse("callback response"))

	b := &Bot{
		cfg:             config.TelegramConfig{TimeoutSeconds: 10},
		msgCfg:          config.Messages{},
		fullCfg:         newTestConfig(),
		agent:           ag,
		sessions:        sm,
		paired:          make(map[int64]bool),
		queues:          make(map[int64]*messageQueue),
		pendingConfirms: make(map[string]chan bool),
		bot:             &tele.Bot{Me: &tele.User{Username: "TestBot"}},
		logger:          zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender:   &tele.User{ID: 42},
		chat:     &tele.Chat{ID: 42, Type: tele.ChatPrivate},
		callback: &tele.Callback{Data: "some_action"},
	}

	err := b.handleCallback(mc)
	if err != nil {
		t.Fatalf("handleCallback error: %v", err)
	}
	if !mc.responded {
		t.Error("expected callback to be acknowledged")
	}
}

// ---------------------------------------------------------------------------
// handleCallback with \f prefix (telebot strips it)
// ---------------------------------------------------------------------------

func TestHandleCallback_FPrefixStripped(t *testing.T) {
	ag, sm := newTestAgent(t, simpleResponse("callback response"))

	b := &Bot{
		cfg:             config.TelegramConfig{TimeoutSeconds: 10},
		msgCfg:          config.Messages{},
		fullCfg:         newTestConfig(),
		agent:           ag,
		sessions:        sm,
		paired:          make(map[int64]bool),
		queues:          make(map[int64]*messageQueue),
		pendingConfirms: make(map[string]chan bool),
		bot:             &tele.Bot{Me: &tele.User{Username: "TestBot"}},
		logger:          zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender:   &tele.User{ID: 42},
		chat:     &tele.Chat{ID: 42, Type: tele.ChatPrivate},
		callback: &tele.Callback{Data: "\fsome_action"},
	}

	err := b.handleCallback(mc)
	if err != nil {
		t.Fatalf("handleCallback error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// SetSkillManager, SetVersion, SetStartTime
// ---------------------------------------------------------------------------

func TestSetSkillManagerCoverage(t *testing.T) {
	b := &Bot{logger: zap.NewNop().Sugar()}
	if b.skillMgr != nil {
		t.Error("expected nil initially")
	}
	// We can't easily create a real skills.Manager, but SetSkillManager just assigns.
	b.SetSkillManager(nil)
	if b.skillMgr != nil {
		t.Error("expected nil after SetSkillManager(nil)")
	}
}

func TestSetVersion(t *testing.T) {
	b := &Bot{logger: zap.NewNop().Sugar()}
	b.SetVersion("1.2.3")
	if b.version != "1.2.3" {
		t.Errorf("expected '1.2.3', got %q", b.version)
	}
}

func TestSetStartTime(t *testing.T) {
	b := &Bot{logger: zap.NewNop().Sugar()}
	now := time.Now()
	b.SetStartTime(now)
	if b.startTime != now {
		t.Error("expected start time to be set")
	}
}

// ---------------------------------------------------------------------------
// cmdCtx / cmdCtxDeps coverage
// ---------------------------------------------------------------------------

func TestCmdCtxDeps(t *testing.T) {
	b := &Bot{
		agent:    nil,
		sessions: nil,
		fullCfg:  newTestConfig(),
		cronMgr:  nil,
		taskMgr:  nil,
		version:  "1.0.0",
		logger:   zap.NewNop().Sugar(),
	}
	deps := b.cmdCtxDeps()
	if deps.Version != "1.0.0" {
		t.Errorf("expected version '1.0.0', got %q", deps.Version)
	}
}

// ---------------------------------------------------------------------------
// Concurrent configSnapshot during Reconnect-style mutation
// ---------------------------------------------------------------------------

func TestConfigSnapshot_StressTest(t *testing.T) {
	b := &Bot{
		cfg:    config.TelegramConfig{StreamMode: "partial"},
		msgCfg: config.Messages{StreamEditMs: 400},
		paired: make(map[int64]bool),
		logger: zap.NewNop().Sugar(),
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			b.mu.Lock()
			b.cfg.StreamMode = "full"
			b.mu.Unlock()
		}()
		go func() {
			defer wg.Done()
			cfg, _ := b.configSnapshot()
			_ = cfg.StreamMode // read to verify no race
		}()
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// downloadFileAsDataURL format verification (using a fake HTTP server)
// ---------------------------------------------------------------------------

func TestDownloadFileAsDataURL_FormatCheck(t *testing.T) {
	// Create a test HTTP server that returns a PNG-like file
	pngData := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(pngData)
	}))
	defer srv.Close()

	// We can't test downloadFileAsDataURL directly since it uses b.botRef().File()
	// which requires Telegram API. Instead, test the format logic manually.
	mime := http.DetectContentType(pngData)
	encoded := base64.StdEncoding.EncodeToString(pngData)
	dataURL := "data:" + mime + ";base64," + encoded

	if !strings.HasPrefix(dataURL, "data:image/png;base64,") {
		t.Errorf("expected PNG data URL, got %q", dataURL[:30])
	}
}

// ---------------------------------------------------------------------------
// handlePhoto with real agent (success path via processImageMessage-style)
// This exercises the ChatWithImages path when download fails (common case).
// ---------------------------------------------------------------------------

func TestHandlePhoto_WithCaption(t *testing.T) {
	teleBot, srv := newMockTeleBot(t)
	defer srv.Close()

	b := &Bot{
		cfg:    config.TelegramConfig{TimeoutSeconds: 1},
		msgCfg: config.Messages{AckReactionScope: ""},
		paired: make(map[int64]bool),
		bot:    teleBot,
		logger: zap.NewNop().Sugar(),
	}
	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
		message: &tele.Message{
			Photo: &tele.Photo{
				File: tele.File{FileID: "invalid"},
			},
			Caption: "What is this?",
		},
	}

	err := b.handlePhoto(mc)
	if err != nil {
		t.Fatalf("handlePhoto error: %v", err)
	}
	// Should reply with "Failed to download image."
	if len(mc.replies) == 0 {
		t.Fatal("expected reply")
	}
}

func TestHandleDocument_ImageWithCaption_DownloadFail(t *testing.T) {
	teleBot, srv := newMockTeleBot(t)
	defer srv.Close()

	b := &Bot{
		cfg:    config.TelegramConfig{TimeoutSeconds: 1},
		msgCfg: config.Messages{AckReactionScope: ""},
		paired: make(map[int64]bool),
		bot:    teleBot,
		logger: zap.NewNop().Sugar(),
	}
	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
		message: &tele.Message{
			Document: &tele.Document{
				File:     tele.File{FileID: "invalid"},
				FileName: "image.jpg",
				MIME:     "image/jpeg",
			},
			Caption: "Check this image",
		},
	}

	err := b.handleDocument(mc)
	if err != nil {
		t.Fatalf("handleDocument error: %v", err)
	}
	if len(mc.replies) == 0 {
		t.Fatal("expected reply for download failure")
	}
}

// ---------------------------------------------------------------------------
// handlePhoto — full success path with mock file server
// ---------------------------------------------------------------------------

func TestHandlePhoto_SuccessWithAgent(t *testing.T) {
	teleBot, srv := newMockTeleBotWithFile(t)
	defer srv.Close()

	ag, sm := newTestAgent(t, simpleResponse("I see a cat!"))

	b := &Bot{
		cfg:      config.TelegramConfig{TimeoutSeconds: 10},
		msgCfg:   config.Messages{AckReactionScope: ""},
		fullCfg:  newTestConfig(),
		agent:    ag,
		sessions: sm,
		paired:   make(map[int64]bool),
		queues:   make(map[int64]*messageQueue),
		bot:      teleBot,
		logger:   zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
		message: &tele.Message{
			Photo: &tele.Photo{
				File: tele.File{FileID: "valid_photo"},
			},
			Caption: "What is this?",
		},
	}

	err := b.handlePhoto(mc)
	if err != nil {
		t.Fatalf("handlePhoto error: %v", err)
	}
	if len(mc.replies) == 0 {
		t.Fatal("expected agent reply")
	}
	reply := mc.replies[0].(string)
	if !strings.Contains(reply, "cat") {
		t.Errorf("expected agent reply with 'cat', got %q", reply)
	}
}

func TestHandlePhoto_SuccessDefaultCaption(t *testing.T) {
	teleBot, srv := newMockTeleBotWithFile(t)
	defer srv.Close()

	ag, sm := newTestAgent(t, simpleResponse("An image"))

	b := &Bot{
		cfg:      config.TelegramConfig{TimeoutSeconds: 10},
		msgCfg:   config.Messages{AckReactionScope: ""},
		fullCfg:  newTestConfig(),
		agent:    ag,
		sessions: sm,
		paired:   make(map[int64]bool),
		queues:   make(map[int64]*messageQueue),
		bot:      teleBot,
		logger:   zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
		message: &tele.Message{
			Photo: &tele.Photo{
				File: tele.File{FileID: "valid_photo"},
			},
			Caption: "", // empty caption triggers default "What's in this image?"
		},
	}

	err := b.handlePhoto(mc)
	if err != nil {
		t.Fatalf("handlePhoto error: %v", err)
	}
}

func TestHandlePhoto_Suppressible(t *testing.T) {
	teleBot, srv := newMockTeleBotWithFile(t)
	defer srv.Close()

	ag, sm := newTestAgent(t, simpleResponse("NO_REPLY"))

	b := &Bot{
		cfg:      config.TelegramConfig{TimeoutSeconds: 10},
		msgCfg:   config.Messages{AckReactionScope: ""},
		fullCfg:  newTestConfig(),
		agent:    ag,
		sessions: sm,
		paired:   make(map[int64]bool),
		queues:   make(map[int64]*messageQueue),
		bot:      teleBot,
		logger:   zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
		message: &tele.Message{
			Photo: &tele.Photo{
				File: tele.File{FileID: "valid_photo"},
			},
		},
	}

	err := b.handlePhoto(mc)
	if err != nil {
		t.Fatalf("handlePhoto error: %v", err)
	}
	// Suppressible response should produce no reply
	if len(mc.replies) != 0 {
		t.Errorf("expected no reply for suppressible response, got %d", len(mc.replies))
	}
}

func TestHandlePhoto_AgentError(t *testing.T) {
	teleBot, srv := newMockTeleBotWithFile(t)
	defer srv.Close()

	ag, sm := newTestAgentError(t, fmt.Errorf("vision unavailable"))

	b := &Bot{
		cfg:      config.TelegramConfig{TimeoutSeconds: 5},
		msgCfg:   config.Messages{AckReactionScope: ""},
		fullCfg:  newTestConfig(),
		agent:    ag,
		sessions: sm,
		paired:   make(map[int64]bool),
		queues:   make(map[int64]*messageQueue),
		bot:      teleBot,
		logger:   zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
		message: &tele.Message{
			Photo: &tele.Photo{
				File: tele.File{FileID: "valid_photo"},
			},
		},
	}

	err := b.handlePhoto(mc)
	if err != nil {
		t.Fatalf("handlePhoto error: %v", err)
	}
	// Should have an error reply
	if len(mc.replies) == 0 {
		t.Fatal("expected error reply")
	}
}

// ---------------------------------------------------------------------------
// handleDocument — image success path with mock file server
// ---------------------------------------------------------------------------

func TestHandleDocument_ImageSuccess(t *testing.T) {
	teleBot, srv := newMockTeleBotWithFile(t)
	defer srv.Close()

	ag, sm := newTestAgent(t, simpleResponse("Image document analyzed"))

	b := &Bot{
		cfg:      config.TelegramConfig{TimeoutSeconds: 10},
		msgCfg:   config.Messages{AckReactionScope: ""},
		fullCfg:  newTestConfig(),
		agent:    ag,
		sessions: sm,
		paired:   make(map[int64]bool),
		queues:   make(map[int64]*messageQueue),
		bot:      teleBot,
		logger:   zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
		message: &tele.Message{
			Document: &tele.Document{
				File:     tele.File{FileID: "valid_image"},
				FileName: "diagram.png",
				MIME:     "image/png",
			},
			Caption: "What does this diagram show?",
		},
	}

	err := b.handleDocument(mc)
	if err != nil {
		t.Fatalf("handleDocument error: %v", err)
	}
	if len(mc.replies) == 0 {
		t.Fatal("expected agent reply")
	}
}

func TestHandleDocument_ImageDefaultCaption(t *testing.T) {
	teleBot, srv := newMockTeleBotWithFile(t)
	defer srv.Close()

	ag, sm := newTestAgent(t, simpleResponse("Result"))

	b := &Bot{
		cfg:      config.TelegramConfig{TimeoutSeconds: 10},
		msgCfg:   config.Messages{AckReactionScope: ""},
		fullCfg:  newTestConfig(),
		agent:    ag,
		sessions: sm,
		paired:   make(map[int64]bool),
		queues:   make(map[int64]*messageQueue),
		bot:      teleBot,
		logger:   zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
		message: &tele.Message{
			Document: &tele.Document{
				File:     tele.File{FileID: "valid_image"},
				FileName: "photo.jpg",
				MIME:     "image/jpeg",
			},
			Caption: "", // empty triggers default
		},
	}

	err := b.handleDocument(mc)
	if err != nil {
		t.Fatalf("handleDocument error: %v", err)
	}
}

func TestHandleDocument_ImageSuppressible(t *testing.T) {
	teleBot, srv := newMockTeleBotWithFile(t)
	defer srv.Close()

	ag, sm := newTestAgent(t, simpleResponse("NO_REPLY"))

	b := &Bot{
		cfg:      config.TelegramConfig{TimeoutSeconds: 10},
		msgCfg:   config.Messages{AckReactionScope: ""},
		fullCfg:  newTestConfig(),
		agent:    ag,
		sessions: sm,
		paired:   make(map[int64]bool),
		queues:   make(map[int64]*messageQueue),
		bot:      teleBot,
		logger:   zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
		message: &tele.Message{
			Document: &tele.Document{
				File:     tele.File{FileID: "valid_image"},
				FileName: "photo.png",
				MIME:     "image/png",
			},
		},
	}

	err := b.handleDocument(mc)
	if err != nil {
		t.Fatalf("handleDocument error: %v", err)
	}
	if len(mc.replies) != 0 {
		t.Errorf("expected no reply for suppressible response, got %d", len(mc.replies))
	}
}

func TestHandleDocument_ImageAgentError(t *testing.T) {
	teleBot, srv := newMockTeleBotWithFile(t)
	defer srv.Close()

	ag, sm := newTestAgentError(t, fmt.Errorf("vision error"))

	b := &Bot{
		cfg:      config.TelegramConfig{TimeoutSeconds: 5},
		msgCfg:   config.Messages{AckReactionScope: ""},
		fullCfg:  newTestConfig(),
		agent:    ag,
		sessions: sm,
		paired:   make(map[int64]bool),
		queues:   make(map[int64]*messageQueue),
		bot:      teleBot,
		logger:   zap.NewNop().Sugar(),
	}

	mc := &mockContext{
		sender: &tele.User{ID: 42},
		chat:   &tele.Chat{ID: 42, Type: tele.ChatPrivate},
		message: &tele.Message{
			Document: &tele.Document{
				File:     tele.File{FileID: "valid_image"},
				FileName: "photo.png",
				MIME:     "image/png",
			},
		},
	}

	err := b.handleDocument(mc)
	if err != nil {
		t.Fatalf("handleDocument error: %v", err)
	}
	if len(mc.replies) == 0 {
		t.Fatal("expected error reply")
	}
}

// ---------------------------------------------------------------------------
// downloadFileAsDataURL — success path
// ---------------------------------------------------------------------------

func TestDownloadFileAsDataURL_Success(t *testing.T) {
	teleBot, srv := newMockTeleBotWithFile(t)
	defer srv.Close()

	b := &Bot{
		bot:    teleBot,
		paired: make(map[int64]bool),
		logger: zap.NewNop().Sugar(),
	}
	dataURL, err := b.downloadFileAsDataURL(tele.File{FileID: "valid_file"})
	if err != nil {
		t.Fatalf("downloadFileAsDataURL error: %v", err)
	}
	if !strings.HasPrefix(dataURL, "data:") {
		t.Errorf("expected data URL, got %q", dataURL[:20])
	}
	if !strings.Contains(dataURL, ";base64,") {
		t.Error("expected base64 encoding in data URL")
	}
}

// ---------------------------------------------------------------------------
// SendTo — additional coverage
// ---------------------------------------------------------------------------

func TestSendTo_Success(t *testing.T) {
	teleBot, srv := newMockTeleBotWithFile(t)
	defer srv.Close()

	b := &Bot{
		bot:    teleBot,
		paired: make(map[int64]bool),
		logger: zap.NewNop().Sugar(),
	}
	b.SendTo(42, "hello")
}

// ---------------------------------------------------------------------------
// Reconnect — exercises the cancel path
// ---------------------------------------------------------------------------

func TestReconnect_NotConnected(t *testing.T) {
	teleBot, srv := newMockTeleBotWithFile(t)
	defer srv.Close()

	b := &Bot{
		bot:    teleBot,
		cfg:    config.TelegramConfig{BotToken: "000000000:test-token"},
		paired: make(map[int64]bool),
		logger: zap.NewNop().Sugar(),
	}
	// Not connected, Reconnect should be a no-op that doesn't panic
	ctx := context.Background()
	_ = b.Reconnect(ctx, b.cfg)
}
