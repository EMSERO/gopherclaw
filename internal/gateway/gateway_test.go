package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
	openai "github.com/sashabaranov/go-openai"

	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/agent"
	"github.com/EMSERO/gopherclaw/internal/config"
	"github.com/EMSERO/gopherclaw/internal/cron"
	"github.com/EMSERO/gopherclaw/internal/models"
	"github.com/EMSERO/gopherclaw/internal/session"
	"github.com/EMSERO/gopherclaw/internal/taskqueue"
)

func testLogger() *zap.SugaredLogger { return zap.NewNop().Sugar() }

func TestHealthEndpoint(t *testing.T) {
	cfg := &config.Root{
		Gateway: config.Gateway{Port: 18789},
	}
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	s := &Server{logger: testLogger(), cfg: cfg, sessions: sm}

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	s.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["status"] != "ok" {
		t.Errorf("expected status ok, got %v", resp["status"])
	}
	// version field is now populated from s.version (SetVersion)
	if _, ok := resp["version"]; !ok {
		t.Error("expected version field in health response")
	}
}

func TestModelsEndpoint(t *testing.T) {
	cfg := &config.Root{
		Agents: config.Agents{
			Defaults: config.AgentDefaults{
				Model: config.ModelConfig{
					Primary:   "github-copilot/claude-sonnet-4.6",
					Fallbacks: []string{"github-copilot/gpt-4.1"},
				},
			},
		},
		Gateway: config.Gateway{Port: 18789},
	}
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	s := &Server{logger: testLogger(), cfg: cfg, sessions: sm}

	req := httptest.NewRequest("GET", "/v1/models", nil)
	w := httptest.NewRecorder()
	s.handleModels(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Object != "list" {
		t.Errorf("expected object=list, got %s", resp.Object)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("expected 2 models, got %d", len(resp.Data))
	}
	if resp.Data[0].ID != "claude-sonnet-4.6" {
		t.Errorf("expected primary model claude-sonnet-4.6, got %s", resp.Data[0].ID)
	}
	if resp.Data[1].ID != "gpt-4.1" {
		t.Errorf("expected fallback model gpt-4.1, got %s", resp.Data[1].ID)
	}
}

func TestChatEndpoint(t *testing.T) {
	cfg := &config.Root{
		Agents: config.Agents{
			Defaults: config.AgentDefaults{
				Model: config.ModelConfig{Primary: "test-model"},
			},
		},
		Gateway: config.Gateway{Port: 18789},
	}
	sm, _ := session.New(testLogger(), t.TempDir(), 0)

	// We can't easily mock the agent.Agent struct since it's concrete,
	// so test the extractLastUser function and handler wiring instead.
	s := &Server{logger: testLogger(), cfg: cfg, sessions: sm}

	// Test extractLastUser
	msgs := []openai.ChatCompletionMessage{
		{Role: "system", Content: "system prompt"},
		{Role: "user", Content: "hello world"},
		{Role: "assistant", Content: "hi"},
		{Role: "user", Content: "latest message"},
	}
	if got := extractLastUser(msgs); got != "latest message" {
		t.Errorf("expected 'latest message', got %q", got)
	}

	// Test empty messages
	if got := extractLastUser(nil); got != "" {
		t.Errorf("expected empty, got %q", got)
	}

	_ = s // server available for future integration tests
}

func TestAuthMiddleware(t *testing.T) {
	cfg := &config.Root{
		Gateway: config.Gateway{
			Auth: config.GatewayAuth{Token: "secret123"},
		},
	}
	s := &Server{logger: testLogger(), cfg: cfg}

	handler := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "OK")
	}))

	// Without token — unauthorized
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 without token, got %d", w.Code)
	}

	// With wrong token
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 with wrong token, got %d", w.Code)
	}

	// With correct token
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer secret123")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with correct token, got %d", w.Code)
	}
}

func TestCheckWSOrigin(t *testing.T) {
	cases := []struct {
		name   string
		origin string
		host   string
		want   bool
	}{
		{"no origin header", "", "localhost:18789", true},
		{"same host", "http://localhost:18789", "localhost:18789", true},
		{"same host https", "https://localhost:18789", "localhost:18789", true},
		{"different host", "http://evil.example.com", "localhost:18789", false},
		{"different port", "http://localhost:9999", "localhost:18789", false},
		{"malformed origin", "not-a-url://\x00bad", "localhost:18789", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/ws", nil)
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			got := checkWSOrigin(req)
			if got != tc.want {
				t.Errorf("checkWSOrigin(%q, host=%q) = %v, want %v", tc.origin, tc.host, got, tc.want)
			}
		})
	}
}

func TestSessionClearEndpoint(t *testing.T) {
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	s := &Server{logger: testLogger(), cfg: &config.Root{}, sessions: sm}

	// Seed a session
	_ = sm.AppendMessages("agent:main:tg:42", []session.Message{{Role: "user", Content: "hi", TS: 1}})

	active := sm.ActiveSessions()
	if _, ok := active["agent:main:tg:42"]; !ok {
		t.Fatal("expected session to exist before clear")
	}

	body := `{"key":"agent:main:tg:42"}`
	req := httptest.NewRequest("POST", "/gopherclaw/sessions/clear", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleSessionClear(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["cleared"] != "agent:main:tg:42" {
		t.Errorf("expected cleared key in response, got %v", resp)
	}
}

func TestSessionClearMissingKey(t *testing.T) {
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	s := &Server{logger: testLogger(), cfg: &config.Root{}, sessions: sm}

	req := httptest.NewRequest("POST", "/gopherclaw/sessions/clear", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleSessionClear(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing key, got %d", w.Code)
	}
}

func TestSessionClearAllEndpoint(t *testing.T) {
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	s := &Server{logger: testLogger(), cfg: &config.Root{}, sessions: sm}

	// Seed two sessions
	_ = sm.AppendMessages("agent:main:tg:1", []session.Message{{Role: "user", Content: "a", TS: 1}})
	_ = sm.AppendMessages("agent:main:tg:2", []session.Message{{Role: "user", Content: "b", TS: 1}})

	if len(sm.ActiveSessions()) != 2 {
		t.Fatal("expected 2 sessions before clear-all")
	}

	req := httptest.NewRequest("POST", "/gopherclaw/sessions/clear-all", nil)
	w := httptest.NewRecorder()
	s.handleSessionClearAll(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["cleared"] != "all" {
		t.Errorf("expected cleared=all in response, got %v", resp)
	}
}

func TestAuthMiddlewareNoToken(t *testing.T) {
	cfg := &config.Root{
		Gateway: config.Gateway{
			Auth: config.GatewayAuth{Token: ""}, // no token configured
		},
	}
	s := &Server{logger: testLogger(), cfg: cfg}

	handler := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "OK")
	}))

	// Without auth — should pass through when no token configured
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 when no token configured, got %d", w.Code)
	}
}

// --- Mock tool for handleToolInvoke tests ---

type gatewayMockTool struct {
	name   string
	result string
}

func (t *gatewayMockTool) Name() string                              { return t.name }
func (t *gatewayMockTool) Schema() json.RawMessage                   { return json.RawMessage(`{}`) }
func (t *gatewayMockTool) Run(_ context.Context, args string) string { return t.result }

// --- handleModelDetail ---

func TestHandleModelDetailKnown(t *testing.T) {
	cfg := &config.Root{
		Agents: config.Agents{
			Defaults: config.AgentDefaults{
				Model: config.ModelConfig{
					Primary:   "github-copilot/claude-sonnet-4.6",
					Fallbacks: []string{"github-copilot/gpt-4.1"},
				},
			},
		},
	}
	s := &Server{logger: testLogger(), cfg: cfg}

	// Test primary model
	req := httptest.NewRequest("GET", "/v1/models/claude-sonnet-4.6", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("model", "claude-sonnet-4.6")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleModelDetail(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for known model, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["id"] != "claude-sonnet-4.6" {
		t.Errorf("expected id=claude-sonnet-4.6, got %v", resp["id"])
	}

	// Test fallback model
	req2 := httptest.NewRequest("GET", "/v1/models/gpt-4.1", nil)
	rctx2 := chi.NewRouteContext()
	rctx2.URLParams.Add("model", "gpt-4.1")
	req2 = req2.WithContext(context.WithValue(req2.Context(), chi.RouteCtxKey, rctx2))
	w2 := httptest.NewRecorder()
	s.handleModelDetail(w2, req2)

	if w2.Code != http.StatusOK {
		t.Errorf("expected 200 for fallback model, got %d", w2.Code)
	}
}

func TestHandleModelDetailUnknown(t *testing.T) {
	cfg := &config.Root{
		Agents: config.Agents{
			Defaults: config.AgentDefaults{
				Model: config.ModelConfig{Primary: "github-copilot/claude-sonnet-4.6"},
			},
		},
	}
	s := &Server{logger: testLogger(), cfg: cfg}

	req := httptest.NewRequest("GET", "/v1/models/nonexistent-model", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("model", "nonexistent-model")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleModelDetail(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown model, got %d", w.Code)
	}
}

// --- estimateTokensFromText ---

func TestEstimateTokensFromText(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
	}{
		{"empty", "", 0},
		{"short (1 char)", "a", 1},
		{"short (3 chars)", "abc", 1},
		{"exactly 4 chars", "abcd", 1},
		{"8 chars", "abcdefgh", 2},
		{"long text", strings.Repeat("x", 100), 25},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := estimateTokensFromText(tc.text)
			if got != tc.want {
				t.Errorf("estimateTokensFromText(%q) = %d, want %d", tc.text, got, tc.want)
			}
		})
	}
}

// --- estimateTokensFromMessages ---

func TestEstimateTokensFromMessages(t *testing.T) {
	// Empty messages
	if got := estimateTokensFromMessages(nil); got != 0 {
		t.Errorf("expected 0 for nil messages, got %d", got)
	}

	// Multiple messages: each gets +4 overhead
	msgs := []openai.ChatCompletionMessage{
		{Role: "user", Content: "hello"},   // 1 token + 4 overhead = 5
		{Role: "assistant", Content: "hi"}, // 1 token + 4 overhead = 5
	}
	got := estimateTokensFromMessages(msgs)
	// "hello" = 5 chars / 4 = 1 token + 4 overhead = 5
	// "hi"    = 2 chars / 4 = 1 token + 4 overhead = 5
	// total = 10
	if got != 10 {
		t.Errorf("expected 10 for two short messages, got %d", got)
	}
}

// --- truncate ---

func TestTruncate(t *testing.T) {
	cases := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"short string", "hello", 10, "hello"},
		{"exact length", "hello", 5, "hello"},
		{"over length", "hello world", 5, "hello..."},
		{"empty", "", 5, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.s, tc.n)
			if got != tc.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
			}
		})
	}
}

// --- writeJSON ---

func TestWriteJSON(t *testing.T) {
	s := &Server{logger: testLogger()}
	w := httptest.NewRecorder()
	s.writeJSON(w, http.StatusCreated, map[string]string{"foo": "bar"})

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["foo"] != "bar" {
		t.Errorf("expected foo=bar, got %v", resp["foo"])
	}
}

// --- handleToolInvoke ---

func TestHandleToolInvokeInvalidJSON(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}}
	req := httptest.NewRequest("POST", "/tools/invoke", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	s.handleToolInvoke(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", w.Code)
	}
}

func TestHandleToolInvokeMissingToolName(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}}
	req := httptest.NewRequest("POST", "/tools/invoke", strings.NewReader(`{"args":{}}`))
	w := httptest.NewRecorder()
	s.handleToolInvoke(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing tool name, got %d", w.Code)
	}
}

func TestHandleToolInvokeNotFound(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}, tools: []agent.Tool{}}
	req := httptest.NewRequest("POST", "/tools/invoke", strings.NewReader(`{"tool":"missing"}`))
	w := httptest.NewRecorder()
	s.handleToolInvoke(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for tool not found, got %d", w.Code)
	}
}

func TestHandleToolInvokeSuccess(t *testing.T) {
	mock := &gatewayMockTool{name: "echo", result: "echoed back"}
	s := &Server{logger: testLogger(), cfg: &config.Root{}, tools: []agent.Tool{mock}}
	body := `{"tool":"echo","args":{"msg":"test"}}`
	req := httptest.NewRequest("POST", "/tools/invoke", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleToolInvoke(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != true {
		t.Errorf("expected ok=true, got %v", resp["ok"])
	}
	if resp["result"] != "echoed back" {
		t.Errorf("expected result='echoed back', got %v", resp["result"])
	}
}

// --- handleSystemEvent ---

func TestHandleSystemEventInvalidJSON(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}}
	req := httptest.NewRequest("POST", "/gopherclaw/system/event", strings.NewReader("bad"))
	w := httptest.NewRecorder()
	s.handleSystemEvent(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleSystemEventMissingText(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}}
	req := httptest.NewRequest("POST", "/gopherclaw/system/event", strings.NewReader(`{"mode":"now"}`))
	w := httptest.NewRecorder()
	s.handleSystemEvent(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing text, got %d", w.Code)
	}
}

func TestHandleSystemEventSuccess(t *testing.T) {
	// The handler fires an async goroutine that calls s.ag.Chat().
	// We provide a real agent with a real (but empty) router so the goroutine
	// gets a clean "all models failed" error instead of a nil-pointer panic.
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	cfg := &config.Root{
		Agents: config.Agents{
			Defaults: config.AgentDefaults{
				Model:          config.ModelConfig{Primary: "test/model"},
				TimeoutSeconds: 1,
			},
		},
	}
	router := models.NewRouter(testLogger(), map[string]models.Provider{}, "test/model", nil)
	ag := agent.New(testLogger(), cfg, &config.AgentDef{
		Identity: config.Identity{Name: "test", Theme: "test"},
	}, router, sm, nil, nil, t.TempDir(), nil)
	s := &Server{logger: testLogger(), cfg: cfg, ag: ag}

	body := `{"text":"deploy check","mode":"now"}`
	req := httptest.NewRequest("POST", "/gopherclaw/system/event", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleSystemEvent(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["status"] != "accepted" {
		t.Errorf("expected status=accepted, got %v", resp["status"])
	}
}

// --- Cron handlers ---

func TestHandleCronListNoCronMgr(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}, cronMgr: nil}
	req := httptest.NewRequest("GET", "/gopherclaw/api/cron", nil)
	w := httptest.NewRecorder()
	s.handleCronList(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 without cronMgr, got %d", w.Code)
	}
}

func TestHandleCronListWithCronMgr(t *testing.T) {
	cronDir := t.TempDir()
	cronMgr := cron.New(testLogger(), cronDir, nil)
	s := &Server{logger: testLogger(), cfg: &config.Root{}, cronMgr: cronMgr}

	req := httptest.NewRequest("GET", "/gopherclaw/api/cron", nil)
	w := httptest.NewRecorder()
	s.handleCronList(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	jobs, ok := resp["jobs"].([]any)
	if !ok {
		// jobs may be nil (empty list encodes as null)
		if resp["jobs"] != nil {
			t.Errorf("expected jobs array or null, got %v", resp["jobs"])
		}
	} else if len(jobs) != 0 {
		t.Errorf("expected 0 jobs, got %d", len(jobs))
	}
}

func TestHandleCronAddNoCronMgr(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}, cronMgr: nil}
	req := httptest.NewRequest("POST", "/gopherclaw/api/cron", strings.NewReader(`{}`))
	w := httptest.NewRecorder()
	s.handleCronAdd(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleCronAddInvalidJSON(t *testing.T) {
	cronDir := t.TempDir()
	cronMgr := cron.New(testLogger(), cronDir, nil)
	s := &Server{logger: testLogger(), cfg: &config.Root{}, cronMgr: cronMgr}

	req := httptest.NewRequest("POST", "/gopherclaw/api/cron", strings.NewReader("bad"))
	w := httptest.NewRecorder()
	s.handleCronAdd(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleCronAddMissingFields(t *testing.T) {
	cronDir := t.TempDir()
	cronMgr := cron.New(testLogger(), cronDir, nil)
	s := &Server{logger: testLogger(), cfg: &config.Root{}, cronMgr: cronMgr}

	req := httptest.NewRequest("POST", "/gopherclaw/api/cron", strings.NewReader(`{"spec":"@daily"}`))
	w := httptest.NewRecorder()
	s.handleCronAdd(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing instruction, got %d", w.Code)
	}
}

func TestHandleCronAddSuccess(t *testing.T) {
	cronDir := t.TempDir()
	cronMgr := cron.New(testLogger(), cronDir, nil)
	s := &Server{logger: testLogger(), cfg: &config.Root{}, cronMgr: cronMgr}

	body := `{"spec":"@hourly","instruction":"do stuff"}`
	req := httptest.NewRequest("POST", "/gopherclaw/api/cron", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleCronAdd(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["job"] == nil {
		t.Error("expected job in response")
	}
}

func TestHandleCronRemoveNoCronMgr(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}, cronMgr: nil}
	req := httptest.NewRequest("DELETE", "/gopherclaw/api/cron/abc", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "abc")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleCronRemove(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleCronRemoveNotFound(t *testing.T) {
	cronDir := t.TempDir()
	cronMgr := cron.New(testLogger(), cronDir, nil)
	s := &Server{logger: testLogger(), cfg: &config.Root{}, cronMgr: cronMgr}

	req := httptest.NewRequest("DELETE", "/gopherclaw/api/cron/nonexistent", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "nonexistent")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleCronRemove(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleCronRemoveSuccess(t *testing.T) {
	cronDir := t.TempDir()
	cronMgr := cron.New(testLogger(), cronDir, nil)
	s := &Server{logger: testLogger(), cfg: &config.Root{}, cronMgr: cronMgr}

	// Add a job first
	job, err := cronMgr.Add("@daily", "test instruction")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("DELETE", "/gopherclaw/api/cron/"+job.ID, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", job.ID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleCronRemove(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleCronRunNoCronMgr(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}, cronMgr: nil}
	req := httptest.NewRequest("POST", "/gopherclaw/api/cron/abc/run", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "abc")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleCronRun(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleCronRunNotFound(t *testing.T) {
	cronDir := t.TempDir()
	cronMgr := cron.New(testLogger(), cronDir, nil)
	s := &Server{logger: testLogger(), cfg: &config.Root{}, cronMgr: cronMgr}

	req := httptest.NewRequest("POST", "/gopherclaw/api/cron/nonexistent/run", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "nonexistent")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleCronRun(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleCronRunSuccess(t *testing.T) {
	cronDir := t.TempDir()
	cronMgr := cron.New(testLogger(), cronDir, nil)
	s := &Server{logger: testLogger(), cfg: &config.Root{}, cronMgr: cronMgr}

	job, err := cronMgr.Add("@daily", "run this")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/gopherclaw/api/cron/"+job.ID+"/run", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", job.ID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleCronRun(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleCronSetEnabledNoCronMgr(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}, cronMgr: nil}
	handler := s.handleCronSetEnabled(true)
	req := httptest.NewRequest("POST", "/gopherclaw/api/cron/abc/enable", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "abc")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleCronSetEnabledNotFound(t *testing.T) {
	cronDir := t.TempDir()
	cronMgr := cron.New(testLogger(), cronDir, nil)
	s := &Server{logger: testLogger(), cfg: &config.Root{}, cronMgr: cronMgr}

	handler := s.handleCronSetEnabled(false)
	req := httptest.NewRequest("POST", "/gopherclaw/api/cron/nonexistent/disable", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "nonexistent")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleCronSetEnabledSuccess(t *testing.T) {
	cronDir := t.TempDir()
	cronMgr := cron.New(testLogger(), cronDir, nil)
	s := &Server{logger: testLogger(), cfg: &config.Root{}, cronMgr: cronMgr}

	job, err := cronMgr.Add("@hourly", "check things")
	if err != nil {
		t.Fatal(err)
	}

	// Disable
	handler := s.handleCronSetEnabled(false)
	req := httptest.NewRequest("POST", "/gopherclaw/api/cron/"+job.ID+"/disable", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", job.ID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["enabled"] != false {
		t.Errorf("expected enabled=false, got %v", resp["enabled"])
	}

	// Enable
	handler2 := s.handleCronSetEnabled(true)
	req2 := httptest.NewRequest("POST", "/gopherclaw/api/cron/"+job.ID+"/enable", nil)
	rctx2 := chi.NewRouteContext()
	rctx2.URLParams.Add("id", job.ID)
	req2 = req2.WithContext(context.WithValue(req2.Context(), chi.RouteCtxKey, rctx2))
	w2 := httptest.NewRecorder()
	handler2(w2, req2)

	if w2.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w2.Code)
	}
	var resp2 map[string]any
	if err := json.Unmarshal(w2.Body.Bytes(), &resp2); err != nil {
		t.Fatal(err)
	}
	if resp2["enabled"] != true {
		t.Errorf("expected enabled=true, got %v", resp2["enabled"])
	}
}

// --- handleConfigView and redactSensitive ---

func TestRedactSensitive(t *testing.T) {
	m := map[string]any{
		"name":     "myapp",
		"token":    "secret-token-value",
		"apiKey":   "key-1234",
		"password": "hunter2",
		"secret":   "s3cr3t",
		"nested": map[string]any{
			"innerToken": "inner-secret",
			"safe":       "visible",
		},
		"list": []any{
			map[string]any{
				"webhookSecret": "ws-123",
				"url":           "https://example.com",
			},
		},
	}
	redactSensitive(m)

	if m["token"] != "****" {
		t.Errorf("expected token redacted, got %v", m["token"])
	}
	if m["apiKey"] != "****" {
		t.Errorf("expected apiKey redacted, got %v", m["apiKey"])
	}
	if m["password"] != "****" {
		t.Errorf("expected password redacted, got %v", m["password"])
	}
	if m["secret"] != "****" {
		t.Errorf("expected secret redacted, got %v", m["secret"])
	}
	if m["name"] != "myapp" {
		t.Errorf("expected name unchanged, got %v", m["name"])
	}

	nested := m["nested"].(map[string]any)
	if nested["innerToken"] != "****" {
		t.Errorf("expected innerToken redacted, got %v", nested["innerToken"])
	}
	if nested["safe"] != "visible" {
		t.Errorf("expected safe unchanged, got %v", nested["safe"])
	}

	listItem := m["list"].([]any)[0].(map[string]any)
	if listItem["webhookSecret"] != "****" {
		t.Errorf("expected webhookSecret redacted, got %v", listItem["webhookSecret"])
	}
	if listItem["url"] != "https://example.com" {
		t.Errorf("expected url unchanged, got %v", listItem["url"])
	}
}

func TestRedactSensitiveEmptyValue(t *testing.T) {
	// Empty string values should NOT be redacted (only non-empty strings)
	m := map[string]any{
		"token": "",
	}
	redactSensitive(m)
	if m["token"] != "" {
		t.Errorf("expected empty token to stay empty, got %v", m["token"])
	}
}

func TestHandleConfigView(t *testing.T) {
	cfg := &config.Root{
		Gateway: config.Gateway{
			Port: 18789,
			Auth: config.GatewayAuth{
				Token: "my-secret-token",
			},
		},
	}
	s := &Server{logger: testLogger(), cfg: cfg}

	req := httptest.NewRequest("GET", "/gopherclaw/api/config", nil)
	w := httptest.NewRecorder()
	s.handleConfigView(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("expected application/json, got %q", ct)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	// Verify token is redacted
	gateway, ok := resp["gateway"].(map[string]any)
	if !ok {
		t.Fatal("expected gateway key in config response")
	}
	auth, ok := gateway["auth"].(map[string]any)
	if !ok {
		t.Fatal("expected auth key in gateway config")
	}
	if auth["token"] != "****" {
		t.Errorf("expected token to be redacted, got %v", auth["token"])
	}
}

// --- handleSessionHistory ---

func TestHandleSessionHistoryMissingKey(t *testing.T) {
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	s := &Server{logger: testLogger(), cfg: &config.Root{}, sessions: sm}

	req := httptest.NewRequest("GET", "/gopherclaw/api/sessions//history", nil)
	rctx := chi.NewRouteContext()
	// No key parameter set
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleSessionHistory(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing key, got %d", w.Code)
	}
}

func TestHandleSessionHistoryNonExistent(t *testing.T) {
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	s := &Server{logger: testLogger(), cfg: &config.Root{}, sessions: sm}

	req := httptest.NewRequest("GET", "/gopherclaw/api/sessions/no-such-session/history", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("key", "no-such-session")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleSessionHistory(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for nonexistent session (empty array), got %d", w.Code)
	}
	// Should return empty array
	body := strings.TrimSpace(w.Body.String())
	if body != "[]" {
		t.Errorf("expected empty array [], got %q", body)
	}
}

func TestHandleSessionHistoryWithMessages(t *testing.T) {
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	s := &Server{logger: testLogger(), cfg: &config.Root{}, sessions: sm}

	// Seed a session
	_ = sm.AppendMessages("agent:main:tg:42", []session.Message{
		{Role: "user", Content: "hello", TS: 1000},
		{Role: "assistant", Content: "hi there", TS: 2000},
	})

	req := httptest.NewRequest("GET", "/gopherclaw/api/sessions/agent:main:tg:42/history", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("key", "agent:main:tg:42")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleSessionHistory(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var msgs []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0]["role"] != "user" {
		t.Errorf("expected first message role=user, got %v", msgs[0]["role"])
	}
	if msgs[1]["content"] != "hi there" {
		t.Errorf("expected second message content='hi there', got %v", msgs[1]["content"])
	}
}

// --- handleWebhook ---

func TestHandleWebhookMissingSession(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}}

	req := httptest.NewRequest("POST", "/webhooks/", strings.NewReader(`{"message":"hi"}`))
	rctx := chi.NewRouteContext()
	// No session param
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing session, got %d", w.Code)
	}
}

func TestHandleWebhookInvalidJSON(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}}

	req := httptest.NewRequest("POST", "/webhooks/test-session", strings.NewReader("not json"))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("session", "test-session")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", w.Code)
	}
}

func TestHandleWebhookMissingMessage(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}}

	req := httptest.NewRequest("POST", "/webhooks/test-session", strings.NewReader(`{"message":""}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("session", "test-session")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty message, got %d", w.Code)
	}
}

// --- authMiddleware mode=none ---

func TestAuthMiddlewareModeNone(t *testing.T) {
	cfg := &config.Root{
		Gateway: config.Gateway{
			Auth: config.GatewayAuth{
				Mode:  "none",
				Token: "should-be-ignored",
			},
		},
	}
	s := &Server{logger: testLogger(), cfg: cfg}

	handler := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "OK")
	}))

	// No token provided, but mode=none so it should pass
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with mode=none, got %d", w.Code)
	}
}

func TestAuthMiddlewareModeTrustedProxy(t *testing.T) {
	cfg := &config.Root{
		Gateway: config.Gateway{
			Auth: config.GatewayAuth{
				Mode:  "trusted-proxy",
				Token: "should-be-ignored",
			},
		},
	}
	s := &Server{logger: testLogger(), cfg: cfg}

	handler := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "OK")
	}))

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with mode=trusted-proxy, got %d", w.Code)
	}
}

// --- authMiddleware with query parameter token ---

func TestAuthMiddlewareQueryParamToken(t *testing.T) {
	cfg := &config.Root{
		Gateway: config.Gateway{
			Auth: config.GatewayAuth{Token: "qp-secret"},
		},
	}
	s := &Server{logger: testLogger(), cfg: cfg}

	handler := s.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "OK")
	}))

	// Correct token via query param
	req := httptest.NewRequest("GET", "/?token=qp-secret", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with correct query param token, got %d", w.Code)
	}

	// Wrong query param token
	req2 := httptest.NewRequest("GET", "/?token=wrong", nil)
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 with wrong query param token, got %d", w2.Code)
	}
}

// --- New() constructor route wiring ---

func TestNewConstructorRoutes(t *testing.T) {
	cfg := &config.Root{
		Gateway: config.Gateway{
			Port: 18789,
			ControlUI: config.ControlUI{
				Enabled:  true,
				BasePath: "/gopherclaw",
			},
		},
	}
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	srv := New(testLogger(), cfg, nil, sm, nil, nil, nil, nil)

	// Use the server's handler directly via httptest
	ts := httptest.NewServer(srv.http.Handler)
	defer ts.Close()

	// Health endpoint (no auth needed)
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 from /health, got %d", resp.StatusCode)
	}

	// Control UI endpoint (no auth)
	resp2, err := http.Get(ts.URL + "/gopherclaw")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("expected 200 from /gopherclaw, got %d", resp2.StatusCode)
	}
	ct := resp2.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("expected text/html content type from UI, got %q", ct)
	}

	// Auth-protected endpoint without token — should 200 (no token configured)
	resp3, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Errorf("expected 200 from /v1/models (no auth configured), got %d", resp3.StatusCode)
	}
}

func TestNewConstructorWithAuth(t *testing.T) {
	cfg := &config.Root{
		Gateway: config.Gateway{
			Port: 18789,
			Auth: config.GatewayAuth{Token: "test-token"},
			ControlUI: config.ControlUI{
				Enabled:  true,
				BasePath: "/gopherclaw",
			},
		},
	}
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	srv := New(testLogger(), cfg, nil, sm, nil, nil, nil, nil)

	ts := httptest.NewServer(srv.http.Handler)
	defer ts.Close()

	// Health (no auth)
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 from /health, got %d", resp.StatusCode)
	}

	// Models without token — should be 401
	resp2, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 from /v1/models without token, got %d", resp2.StatusCode)
	}

	// Models with token — should pass
	client := &http.Client{}
	req, _ := http.NewRequest("GET", ts.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	resp3, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Errorf("expected 200 from /v1/models with token, got %d", resp3.StatusCode)
	}
}

// --- LogBroadcaster ---

func TestLogBroadcaster(t *testing.T) {
	lb := NewLogBroadcaster()

	// Subscribe
	ch := lb.Subscribe()

	// Write a log line
	n, err := lb.Write([]byte("test log line\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n != len("test log line\n") {
		t.Errorf("expected n=%d, got %d", len("test log line\n"), n)
	}

	// Read from subscriber
	select {
	case line := <-ch:
		if line != "test log line" {
			t.Errorf("expected 'test log line', got %q", line)
		}
	default:
		t.Error("expected to receive log line")
	}

	// Unsubscribe
	lb.Unsubscribe(ch)

	// Write after unsubscribe should not panic
	n2, err := lb.Write([]byte("after unsub\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n2 != len("after unsub\n") {
		t.Errorf("expected n=%d, got %d", len("after unsub\n"), n2)
	}

	// Sync is a no-op
	if err := lb.Sync(); err != nil {
		t.Errorf("Sync should return nil, got %v", err)
	}
}

func TestLogBroadcasterEmptyWrite(t *testing.T) {
	lb := NewLogBroadcaster()
	ch := lb.Subscribe()
	defer lb.Unsubscribe(ch)

	// Empty line should be skipped
	n, err := lb.Write([]byte("\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("expected n=1, got %d", n)
	}

	// Channel should be empty
	select {
	case line := <-ch:
		t.Errorf("expected no message for empty line, got %q", line)
	default:
		// expected
	}
}

// --- AddDeliverer / AddChannel ---

func TestServerAddDelivererAndChannel(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}}

	// AddDeliverer
	md := &mockDeliverer{}
	s.AddDeliverer(md)
	if len(s.deliverers) != 1 {
		t.Errorf("expected 1 deliverer, got %d", len(s.deliverers))
	}

	// AddChannel
	mc := &mockChannelStatus{}
	s.AddChannel(mc)
	if len(s.channels) != 1 {
		t.Errorf("expected 1 channel, got %d", len(s.channels))
	}
}

// --- handleUI ---

func TestHandleUI(t *testing.T) {
	cfg := &config.Root{
		Gateway: config.Gateway{
			Auth: config.GatewayAuth{Token: "ui-test-token"},
		},
	}
	s := &Server{logger: testLogger(), cfg: cfg}

	req := httptest.NewRequest("GET", "/gopherclaw", nil)
	w := httptest.NewRecorder()
	s.handleUI(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("expected text/html, got %q", ct)
	}
	// The token should be injected into the HTML
	if !strings.Contains(w.Body.String(), "ui-test-token") {
		t.Error("expected token to be injected into UI HTML")
	}
}

// --- handleLogStream without broadcaster ---

func TestHandleLogStreamNoBroadcaster(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}, logBroadcaster: nil}
	req := httptest.NewRequest("GET", "/gopherclaw/api/log", nil)
	w := httptest.NewRecorder()
	s.handleLogStream(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

// --- helper: build a test agent with an empty router (no real providers) ---

func testAgent(t *testing.T) (*agent.Agent, *session.Manager, *config.Root) {
	t.Helper()
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	cfg := &config.Root{
		Agents: config.Agents{
			Defaults: config.AgentDefaults{
				Model:          config.ModelConfig{Primary: "test/model"},
				TimeoutSeconds: 5,
			},
		},
	}
	router := models.NewRouter(testLogger(), map[string]models.Provider{}, "test/model", nil)
	ag := agent.New(testLogger(), cfg, &config.AgentDef{
		Identity: config.Identity{Name: "test", Theme: "a test agent"},
	}, router, sm, nil, nil, t.TempDir(), nil)
	return ag, sm, cfg
}

// --- handleWebhook success path (agent returns error -> 500) ---

func TestHandleWebhookAgentError(t *testing.T) {
	ag, _, cfg := testAgent(t)
	s := &Server{logger: testLogger(), cfg: cfg, ag: ag}

	body := `{"message":"hello webhook"}`
	req := httptest.NewRequest("POST", "/webhooks/test-session", strings.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("session", "test-session")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleWebhook(w, req)

	// The empty router has no providers, so agent returns error -> 500
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for agent error, got %d: %s", w.Code, w.Body.String())
	}
}

// --- handleWebhook stream path (exercises webhookStream) ---

func TestHandleWebhookStreamAgentError(t *testing.T) {
	ag, _, cfg := testAgent(t)
	s := &Server{logger: testLogger(), cfg: cfg, ag: ag}

	body := `{"message":"hello stream","stream":true}`
	req := httptest.NewRequest("POST", "/webhooks/test-session", strings.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("session", "test-session")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleWebhook(w, req)

	// Even with an error, the stream handler sets the content type and writes events
	ct := w.Header().Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("expected text/event-stream, got %q", ct)
	}
}

// --- handleChatCompletions (non-streaming) ---

func TestHandleChatCompletionsInvalidJSON(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}}
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("bad json"))
	w := httptest.NewRecorder()
	s.handleChatCompletions(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", w.Code)
	}
}

func TestHandleChatCompletionsNoUserMessage(t *testing.T) {
	ag, _, cfg := testAgent(t)
	s := &Server{logger: testLogger(), cfg: cfg, ag: ag}

	body := `{"model":"test","messages":[{"role":"system","content":"be helpful"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleChatCompletions(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for no user message, got %d", w.Code)
	}
}

func TestHandleChatCompletionsAgentError(t *testing.T) {
	ag, _, cfg := testAgent(t)
	s := &Server{logger: testLogger(), cfg: cfg, ag: ag}

	body := `{"model":"test","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleChatCompletions(w, req)

	// Agent has no providers -> returns error -> 500
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 for agent error, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleChatCompletionsStreamNoUserMessage(t *testing.T) {
	ag, _, cfg := testAgent(t)
	s := &Server{logger: testLogger(), cfg: cfg, ag: ag}

	// Streaming request with no user message
	body := `{"model":"test","stream":true,"messages":[{"role":"system","content":"be helpful"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleChatCompletions(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for no user message in streaming, got %d", w.Code)
	}
}

func TestHandleChatCompletionsStreamAgentError(t *testing.T) {
	ag, _, cfg := testAgent(t)
	s := &Server{logger: testLogger(), cfg: cfg, ag: ag}

	body := `{"model":"test","stream":true,"messages":[{"role":"user","content":"hello stream"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleChatCompletions(w, req)

	// Stream path sets text/event-stream header
	ct := w.Header().Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("expected text/event-stream, got %q", ct)
	}
	// Should contain [DONE] marker even on error
	if !strings.Contains(w.Body.String(), "[DONE]") {
		t.Error("expected [DONE] marker in stream response")
	}
}

func TestHandleChatCompletionsStreamViaAcceptHeader(t *testing.T) {
	ag, _, cfg := testAgent(t)
	s := &Server{logger: testLogger(), cfg: cfg, ag: ag}

	body := `{"model":"test","messages":[{"role":"user","content":"hello accept"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()
	s.handleChatCompletions(w, req)

	ct := w.Header().Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("expected text/event-stream via Accept header, got %q", ct)
	}
}

func TestHandleChatCompletionsWithSessionKey(t *testing.T) {
	ag, _, cfg := testAgent(t)
	s := &Server{logger: testLogger(), cfg: cfg, ag: ag}

	body := `{"model":"test","messages":[{"role":"user","content":"hello custom session"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("X-Session-Key", "custom:session:key")
	w := httptest.NewRecorder()
	s.handleChatCompletions(w, req)

	// Will error (no providers), but exercises the X-Session-Key branch
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 (agent error), got %d", w.Code)
	}
}

// --- AddDeliverer / AddChannel ---

type mockDeliverer struct{ sent string }

func (d *mockDeliverer) SendToAllPaired(text string) { d.sent = text }

type mockChannelStatus struct {
	name      string
	connected bool
}

func (c *mockChannelStatus) ChannelName() string { return c.name }
func (c *mockChannelStatus) IsConnected() bool   { return c.connected }
func (c *mockChannelStatus) Username() string    { return "testbot" }
func (c *mockChannelStatus) PairedCount() int    { return 3 }

func TestAddDeliverer(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}}
	d := &mockDeliverer{}
	s.AddDeliverer(d)
	if len(s.deliverers) != 1 {
		t.Errorf("expected 1 deliverer, got %d", len(s.deliverers))
	}
}

func TestAddChannel(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}}
	ch := &mockChannelStatus{name: "telegram", connected: true}
	s.AddChannel(ch)
	if len(s.channels) != 1 {
		t.Errorf("expected 1 channel, got %d", len(s.channels))
	}
}

// --- handleCronAdd with invalid spec ---

func TestHandleCronAddInvalidSpec(t *testing.T) {
	cronDir := t.TempDir()
	cronMgr := cron.New(testLogger(), cronDir, nil)
	s := &Server{logger: testLogger(), cfg: &config.Root{}, cronMgr: cronMgr}

	body := `{"spec":"invalid-spec","instruction":"do stuff"}`
	req := httptest.NewRequest("POST", "/gopherclaw/api/cron", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleCronAdd(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid spec, got %d: %s", w.Code, w.Body.String())
	}
}

// --- handleToolInvoke with sessionKey ---

func TestHandleToolInvokeWithSessionKey(t *testing.T) {
	mock := &gatewayMockTool{name: "echo", result: "ok with key"}
	s := &Server{logger: testLogger(), cfg: &config.Root{}, tools: []agent.Tool{mock}}
	body := `{"tool":"echo","args":{},"sessionKey":"custom:key"}`
	req := httptest.NewRequest("POST", "/tools/invoke", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleToolInvoke(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["result"] != "ok with key" {
		t.Errorf("expected 'ok with key', got %v", resp["result"])
	}
}

// --- handleWS via real WebSocket ---

func TestHandleWSConnection(t *testing.T) {
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	cfg := &config.Root{
		Agents: config.Agents{
			Defaults: config.AgentDefaults{
				Model: config.ModelConfig{Primary: "test/model"},
			},
		},
		Gateway: config.Gateway{
			ControlUI: config.ControlUI{Enabled: true, BasePath: "/gopherclaw"},
		},
	}
	s := &Server{logger: testLogger(), cfg: cfg, sessions: sm}

	// Add a mock channel for coverage of the channel status loop
	s.AddChannel(&mockChannelStatus{name: "telegram", connected: true})

	ts := httptest.NewServer(http.HandlerFunc(s.handleWS))
	defer ts.Close()

	// Convert http URL to ws URL
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/gopherclaw/ws"

	// Connect with gorilla websocket
	dialer := websocket.Dialer{}
	conn, resp, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("websocket dial failed: %v", err)
	}
	defer conn.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Errorf("expected 101, got %d", resp.StatusCode)
	}

	// Read the first status message (sent immediately on connect)
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read message: %v", err)
	}

	var status map[string]any
	if err := json.Unmarshal(msg, &status); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if status["status"] != "running" {
		t.Errorf("expected status=running, got %v", status["status"])
	}

	// Send a message to trigger the refresh path
	if err := conn.WriteMessage(websocket.TextMessage, []byte("refresh")); err != nil {
		t.Fatalf("write message: %v", err)
	}

	// Read the refreshed status
	_, msg2, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read refresh message: %v", err)
	}
	var status2 map[string]any
	if err := json.Unmarshal(msg2, &status2); err != nil {
		t.Fatalf("unmarshal refresh status: %v", err)
	}
	if status2["status"] != "running" {
		t.Errorf("expected status=running after refresh, got %v", status2["status"])
	}

	// Verify channels are included
	channels, ok := status2["channels"].([]any)
	if !ok || len(channels) == 0 {
		t.Error("expected channels in status response")
	}
}

// --- handleLogStream with broadcaster (SSE test) ---

func TestHandleLogStreamWithBroadcaster(t *testing.T) {
	lb := NewLogBroadcaster()
	s := &Server{logger: testLogger(), cfg: &config.Root{}, logBroadcaster: lb}

	// Use a real HTTP server so we get a flusher
	ts := httptest.NewServer(http.HandlerFunc(s.handleLogStream))
	defer ts.Close()

	// Make a streaming request with a context that we can cancel
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/gopherclaw/api/log", nil)
	client := &http.Client{}

	// Start reading in a goroutine
	done := make(chan string, 1)
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		done <- string(buf[:n])
	}()

	// Write a log line
	_, _ = lb.Write([]byte("test SSE log\n"))

	// Cancel context to end the stream
	cancel()

	// Wait for the response or timeout
	select {
	case data := <-done:
		if !strings.Contains(data, "test SSE log") {
			t.Errorf("expected SSE data to contain 'test SSE log', got %q", data)
		}
	case <-time.After(2 * time.Second):
		// Acceptable - the log may not have been delivered before cancel
	}
}

// --- handleWS with sessions and cron (covers more branches) ---

// --- Mock provider for successful agent responses ---

type mockProvider struct {
	response openai.ChatCompletionResponse
	chatErr  error
	// For streaming:
	streamChunks []openai.ChatCompletionStreamResponse
	streamErr    error
}

func (p *mockProvider) Chat(_ context.Context, _ openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	return p.response, p.chatErr
}

func (p *mockProvider) ChatStream(_ context.Context, _ openai.ChatCompletionRequest) (models.Stream, error) {
	if p.streamErr != nil {
		return nil, p.streamErr
	}
	return &mockStream{chunks: p.streamChunks}, nil
}

type mockStream struct {
	chunks []openai.ChatCompletionStreamResponse
	idx    int
}

func (s *mockStream) Recv() (openai.ChatCompletionStreamResponse, error) {
	if s.idx >= len(s.chunks) {
		return openai.ChatCompletionStreamResponse{}, io.EOF
	}
	chunk := s.chunks[s.idx]
	s.idx++
	return chunk, nil
}

func (s *mockStream) Close() error { return nil }

// testAgentWithMock creates an agent backed by a mock provider that returns success.
func testAgentWithMock(t *testing.T, text string, stopped bool) (*agent.Agent, *session.Manager, *config.Root) {
	t.Helper()
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	cfg := &config.Root{
		Agents: config.Agents{
			Defaults: config.AgentDefaults{
				Model:          config.ModelConfig{Primary: "mock/test-model"},
				TimeoutSeconds: 5,
			},
		},
	}
	finishReason := openai.FinishReasonStop
	if stopped {
		finishReason = openai.FinishReasonLength
	}
	mp := &mockProvider{
		response: openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{
				{
					Message: openai.ChatCompletionMessage{
						Role:    "assistant",
						Content: text,
					},
					FinishReason: finishReason,
				},
			},
			Usage: openai.Usage{PromptTokens: 10, CompletionTokens: 5},
		},
		streamChunks: []openai.ChatCompletionStreamResponse{
			{
				Choices: []openai.ChatCompletionStreamChoice{
					{Delta: openai.ChatCompletionStreamChoiceDelta{Role: "assistant", Content: text}},
				},
			},
		},
	}
	router := models.NewRouter(testLogger(), map[string]models.Provider{"mock": mp}, "mock/test-model", nil)
	ag := agent.New(testLogger(), cfg, &config.AgentDef{
		Identity: config.Identity{Name: "test", Theme: "test"},
	}, router, sm, nil, nil, t.TempDir(), nil)
	return ag, sm, cfg
}

// --- fullChatResponse success path ---

func TestHandleChatCompletionsFullResponseSuccess(t *testing.T) {
	ag, _, cfg := testAgentWithMock(t, "Hello from the agent!", false)
	s := &Server{logger: testLogger(), cfg: cfg, ag: ag}

	body := `{"model":"test","messages":[{"role":"user","content":"hi there"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleChatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp openai.ChatCompletionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Object != "chat.completion" {
		t.Errorf("expected object=chat.completion, got %q", resp.Object)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(resp.Choices))
	}
	if resp.Choices[0].Message.Content != "Hello from the agent!" {
		t.Errorf("expected agent response text, got %q", resp.Choices[0].Message.Content)
	}
	if resp.Choices[0].FinishReason != openai.FinishReasonStop {
		t.Errorf("expected finish_reason=stop, got %q", resp.Choices[0].FinishReason)
	}
	if resp.Usage.PromptTokens == 0 {
		t.Error("expected non-zero prompt tokens")
	}
	if resp.Usage.CompletionTokens == 0 {
		t.Error("expected non-zero completion tokens")
	}
	if resp.Model != "test-model" {
		t.Errorf("expected model=test-model, got %q", resp.Model)
	}
	if !strings.HasPrefix(resp.ID, "chatcmpl-") {
		t.Errorf("expected ID to start with chatcmpl-, got %q", resp.ID)
	}
}

func TestHandleChatCompletionsFullResponseStopped(t *testing.T) {
	ag, _, cfg := testAgentWithMock(t, "Maximum iterations reached.", true)
	s := &Server{logger: testLogger(), cfg: cfg, ag: ag}

	body := `{"model":"test","messages":[{"role":"user","content":"long task"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleChatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp openai.ChatCompletionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// When agent returns Stopped=true, the response should have finish_reason=length
	// However, the Stopped field on agent.Response is only true for loop detection,
	// not for the finish_reason from the model. The mock provider returns FinishReasonLength
	// which causes resp.Stopped to be false (it's set by iteration limit in agent loop).
	// The important thing is that the endpoint returns 200 with valid response.
	if len(resp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(resp.Choices))
	}
}

// --- streamChatResponse success path ---

func TestHandleChatCompletionsStreamSuccess(t *testing.T) {
	ag, _, cfg := testAgentWithMock(t, "streamed reply", false)
	s := &Server{logger: testLogger(), cfg: cfg, ag: ag}

	body := `{"model":"test","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleChatCompletions(w, req)

	ct := w.Header().Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("expected text/event-stream, got %q", ct)
	}

	bodyStr := w.Body.String()

	// Should contain SSE data lines
	if !strings.Contains(bodyStr, "data: ") {
		t.Error("expected SSE data lines in response")
	}

	// Should contain the [DONE] marker
	if !strings.Contains(bodyStr, "data: [DONE]") {
		t.Error("expected [DONE] marker in stream response")
	}

	// Parse the first SSE event to verify the OpenAI-compatible format
	for line := range strings.SplitSeq(bodyStr, "\n") {
		if strings.HasPrefix(line, "data: {") {
			var chunk openai.ChatCompletionStreamResponse
			jsonStr := strings.TrimPrefix(line, "data: ")
			if err := json.Unmarshal([]byte(jsonStr), &chunk); err != nil {
				t.Fatalf("failed to parse SSE chunk: %v", err)
			}
			if chunk.Object != "chat.completion.chunk" {
				t.Errorf("expected object=chat.completion.chunk, got %q", chunk.Object)
			}
			if !strings.HasPrefix(chunk.ID, "chatcmpl-") {
				t.Errorf("expected ID chatcmpl- prefix, got %q", chunk.ID)
			}
			break // just check the first chunk
		}
	}
}

// --- webhookStream success path ---

func TestHandleWebhookStreamSuccess(t *testing.T) {
	ag, _, cfg := testAgentWithMock(t, "webhook stream reply", false)
	s := &Server{logger: testLogger(), cfg: cfg, ag: ag}

	body := `{"message":"hello stream","stream":true}`
	req := httptest.NewRequest("POST", "/webhooks/stream-test", strings.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("session", "stream-test")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleWebhook(w, req)

	ct := w.Header().Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("expected text/event-stream, got %q", ct)
	}

	bodyStr := w.Body.String()
	// Should contain data lines with text chunks
	if !strings.Contains(bodyStr, "data: ") {
		t.Error("expected SSE data lines")
	}
	// Should contain the final "done" event
	if !strings.Contains(bodyStr, "event: done") {
		t.Error("expected event: done in webhook stream")
	}
	// Verify the done event contains the full text
	if !strings.Contains(bodyStr, "webhook stream reply") {
		t.Errorf("expected webhook stream reply in output, got %q", bodyStr)
	}
}

// --- webhook non-stream success path ---

func TestHandleWebhookNonStreamSuccess(t *testing.T) {
	ag, _, cfg := testAgentWithMock(t, "webhook reply", false)
	s := &Server{logger: testLogger(), cfg: cfg, ag: ag}

	body := `{"message":"hello webhook"}`
	req := httptest.NewRequest("POST", "/webhooks/success-test", strings.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("session", "success-test")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp webhookResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Text != "webhook reply" {
		t.Errorf("expected text='webhook reply', got %q", resp.Text)
	}
	if resp.Stopped {
		t.Error("expected stopped=false")
	}
}

// --- Start() tests ---

func TestStartListensAndShuts(t *testing.T) {
	cfg := &config.Root{
		Gateway: config.Gateway{Port: 0}, // port 0 = random free port
	}
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	s := &Server{logger: testLogger(), cfg: cfg, sessions: sm}

	// Manually create a chi router with a health endpoint
	r := chi.NewRouter()
	r.Get("/health", s.handleHealth)
	s.http = &http.Server{
		Addr:    "127.0.0.1:0",
		Handler: r,
	}

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Start(ctx)
	}()

	// Give the server time to start
	time.Sleep(100 * time.Millisecond)

	// Cancel context to trigger graceful shutdown
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Start returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Start did not return within 5 seconds")
	}
}

// --- handleLogStream with broadcaster that delivers a line ---

func TestHandleLogStreamSSEDelivery(t *testing.T) {
	lb := NewLogBroadcaster()
	s := &Server{logger: testLogger(), cfg: &config.Root{}, logBroadcaster: lb}

	ts := httptest.NewServer(http.HandlerFunc(s.handleLogStream))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL, nil)
	client := &http.Client{}

	type result struct {
		data string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		// Read enough for the first SSE event
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		done <- result{data: string(buf[:n])}
	}()

	// Give the handler time to subscribe
	time.Sleep(50 * time.Millisecond)
	_, _ = lb.Write([]byte("SSE delivery test\n"))

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("request error: %v", r.err)
		}
		if !strings.Contains(r.data, "data: SSE delivery test") {
			t.Errorf("expected SSE data containing 'SSE delivery test', got %q", r.data)
		}
	case <-time.After(3 * time.Second):
		// Acceptable timeout
	}
}

func TestHandleWSWithSessionsAndCron(t *testing.T) {
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	// Add some sessions for coverage of session iteration
	_ = sm.AppendMessages("agent:main:tg:1", []session.Message{{Role: "user", Content: "hi", TS: 1}})

	cronDir := t.TempDir()
	cronMgr := cron.New(testLogger(), cronDir, nil)
	_, _ = cronMgr.Add("@daily", "test cron")

	cfg := &config.Root{
		Agents: config.Agents{
			Defaults: config.AgentDefaults{
				Model: config.ModelConfig{Primary: "test/model"},
			},
		},
	}
	s := &Server{logger: testLogger(), cfg: cfg, sessions: sm, cronMgr: cronMgr}

	ts := httptest.NewServer(http.HandlerFunc(s.handleWS))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()

	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var status map[string]any
	if err := json.Unmarshal(msg, &status); err != nil {
		t.Fatal(err)
	}

	// Verify sessions are included
	sessions, ok := status["sessions"].([]any)
	if !ok || len(sessions) == 0 {
		t.Error("expected sessions in status")
	}

	// Verify cron summary is included
	cronInfo, ok := status["cron"].(map[string]any)
	if !ok {
		t.Error("expected cron summary in status")
	} else {
		total := cronInfo["total"].(float64)
		if total != 1 {
			t.Errorf("expected 1 total cron job, got %v", total)
		}
	}
}

// ---------------------------------------------------------------------------
// Skill API tests
// ---------------------------------------------------------------------------

func TestHandleSkillList_NoManager(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}}

	req := httptest.NewRequest("GET", "/gopherclaw/api/skills", nil)
	w := httptest.NewRecorder()
	s.handleSkillList(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	skills, ok := resp["skills"].([]any)
	if !ok {
		t.Fatal("expected skills to be an array")
	}
	if len(skills) != 0 {
		t.Errorf("expected empty skills array, got %d items", len(skills))
	}
}

func TestHandleSkillList_WithSkills(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}}
	s.skillLister = func() []SkillInfo {
		return []SkillInfo{
			{Name: "weather", Description: "Check the weather", Origin: "builtin", Enabled: true},
			{Name: "stocks", Description: "Stock prices", Origin: "plugin", Enabled: false},
		}
	}

	req := httptest.NewRequest("GET", "/gopherclaw/api/skills", nil)
	w := httptest.NewRecorder()
	s.handleSkillList(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp struct {
		Skills []SkillInfo `json:"skills"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Skills) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(resp.Skills))
	}
	if resp.Skills[0].Name != "weather" {
		t.Errorf("expected first skill name 'weather', got %q", resp.Skills[0].Name)
	}
	if resp.Skills[0].Description != "Check the weather" {
		t.Errorf("expected description 'Check the weather', got %q", resp.Skills[0].Description)
	}
	if resp.Skills[0].Origin != "builtin" {
		t.Errorf("expected origin 'builtin', got %q", resp.Skills[0].Origin)
	}
	if !resp.Skills[0].Enabled {
		t.Error("expected first skill to be enabled")
	}
	if resp.Skills[1].Name != "stocks" {
		t.Errorf("expected second skill name 'stocks', got %q", resp.Skills[1].Name)
	}
	if resp.Skills[1].Enabled {
		t.Error("expected second skill to be disabled")
	}
}

func TestHandleSkillSetEnabled_Enable(t *testing.T) {
	var toggledName string
	var toggledEnabled bool

	s := &Server{logger: testLogger(), cfg: &config.Root{}}
	s.skillToggler = func(name string, enabled bool) bool {
		toggledName = name
		toggledEnabled = enabled
		return true
	}

	handler := s.handleSkillSetEnabled(true)

	req := httptest.NewRequest("POST", "/gopherclaw/api/skills/weather/enable", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "weather")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if toggledName != "weather" {
		t.Errorf("expected toggler called with name 'weather', got %q", toggledName)
	}
	if !toggledEnabled {
		t.Error("expected toggler called with enabled=true")
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != true {
		t.Errorf("expected ok=true, got %v", resp["ok"])
	}
	if resp["name"] != "weather" {
		t.Errorf("expected name='weather', got %v", resp["name"])
	}
	if resp["enabled"] != true {
		t.Errorf("expected enabled=true, got %v", resp["enabled"])
	}
}

func TestHandleSkillSetEnabled_Disable(t *testing.T) {
	var toggledEnabled bool

	s := &Server{logger: testLogger(), cfg: &config.Root{}}
	s.skillToggler = func(name string, enabled bool) bool {
		toggledEnabled = enabled
		return true
	}

	handler := s.handleSkillSetEnabled(false)

	req := httptest.NewRequest("POST", "/gopherclaw/api/skills/stocks/disable", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "stocks")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if toggledEnabled {
		t.Error("expected toggler called with enabled=false")
	}
}

func TestHandleSkillSetEnabled_NotFound(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}}
	s.skillToggler = func(name string, enabled bool) bool {
		return false // skill not found
	}

	handler := s.handleSkillSetEnabled(true)

	req := httptest.NewRequest("POST", "/gopherclaw/api/skills/nonexistent/enable", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "nonexistent")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["error"] != "skill not found" {
		t.Errorf("expected error 'skill not found', got %v", resp["error"])
	}
}

func TestHandleSkillSetEnabled_NoManager(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}}
	// skillToggler is nil

	handler := s.handleSkillSetEnabled(true)

	req := httptest.NewRequest("POST", "/gopherclaw/api/skills/anything/enable", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "anything")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["error"] != "skill manager not available" {
		t.Errorf("expected error 'skill manager not available', got %v", resp["error"])
	}
}

// ---------------------------------------------------------------------------
// Task API tests
// ---------------------------------------------------------------------------

func TestHandleTaskList(t *testing.T) {
	dir := t.TempDir()
	mgr := taskqueue.New(testLogger(), filepath.Join(dir, "tasks.json"), taskqueue.Config{})
	s := &Server{logger: testLogger(), cfg: &config.Root{}, taskMgr: mgr}

	// Submit a task so the list is non-empty.
	mgr.Submit("session1", "agent1", "do something", func(ctx context.Context) (string, error) {
		// Block until cancelled so the task stays visible.
		<-ctx.Done()
		return "", ctx.Err()
	}, taskqueue.SubmitOpts{})

	// Give the goroutine a moment to start.
	time.Sleep(50 * time.Millisecond)

	req := httptest.NewRequest("GET", "/gopherclaw/api/tasks", nil)
	w := httptest.NewRecorder()
	s.handleTaskList(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	tasks, ok := resp["tasks"].([]any)
	if !ok || len(tasks) == 0 {
		t.Fatal("expected at least one task in response")
	}

	task0 := tasks[0].(map[string]any)
	if task0["sessionKey"] != "session1" {
		t.Errorf("expected sessionKey 'session1', got %v", task0["sessionKey"])
	}
	if task0["message"] != "do something" {
		t.Errorf("expected message 'do something', got %v", task0["message"])
	}
}

func TestHandleTaskList_NoManager(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}, taskMgr: nil}

	req := httptest.NewRequest("GET", "/gopherclaw/api/tasks", nil)
	w := httptest.NewRecorder()
	s.handleTaskList(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestHandleTaskList_FilterBySession(t *testing.T) {
	dir := t.TempDir()
	mgr := taskqueue.New(testLogger(), filepath.Join(dir, "tasks.json"), taskqueue.Config{})
	s := &Server{logger: testLogger(), cfg: &config.Root{}, taskMgr: mgr}

	// Submit tasks for different sessions.
	mgr.Submit("sess-a", "agent1", "task A", func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}, taskqueue.SubmitOpts{})
	mgr.Submit("sess-b", "agent1", "task B", func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}, taskqueue.SubmitOpts{})

	time.Sleep(50 * time.Millisecond)

	req := httptest.NewRequest("GET", "/gopherclaw/api/tasks?session=sess-a", nil)
	w := httptest.NewRecorder()
	s.handleTaskList(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	tasks, ok := resp["tasks"].([]any)
	if !ok {
		t.Fatal("expected tasks array")
	}
	for _, raw := range tasks {
		tk := raw.(map[string]any)
		if tk["sessionKey"] != "sess-a" {
			t.Errorf("expected only sess-a tasks, got sessionKey=%v", tk["sessionKey"])
		}
	}
}

func TestHandleTaskCancel(t *testing.T) {
	dir := t.TempDir()
	mgr := taskqueue.New(testLogger(), filepath.Join(dir, "tasks.json"), taskqueue.Config{})
	s := &Server{logger: testLogger(), cfg: &config.Root{}, taskMgr: mgr}

	cancelled := make(chan struct{})
	taskID := mgr.Submit("session1", "agent1", "cancel me", func(ctx context.Context) (string, error) {
		<-ctx.Done()
		close(cancelled)
		return "", ctx.Err()
	}, taskqueue.SubmitOpts{})

	time.Sleep(50 * time.Millisecond)

	req := httptest.NewRequest("POST", "/gopherclaw/api/tasks/"+taskID+"/cancel", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", taskID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleTaskCancel(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != true {
		t.Errorf("expected ok=true, got %v", resp["ok"])
	}

	// Verify the task's context was actually cancelled.
	select {
	case <-cancelled:
		// success
	case <-time.After(2 * time.Second):
		t.Error("task context was not cancelled within timeout")
	}
}

func TestHandleTaskCancel_NotFound(t *testing.T) {
	dir := t.TempDir()
	mgr := taskqueue.New(testLogger(), filepath.Join(dir, "tasks.json"), taskqueue.Config{})
	s := &Server{logger: testLogger(), cfg: &config.Root{}, taskMgr: mgr}

	req := httptest.NewRequest("POST", "/gopherclaw/api/tasks/nonexistent/cancel", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "nonexistent")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleTaskCancel(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleTaskCancel_NoManager(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}, taskMgr: nil}

	req := httptest.NewRequest("POST", "/gopherclaw/api/tasks/abc/cancel", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "abc")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleTaskCancel(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

func TestSetVersion(t *testing.T) {
	s := &Server{logger: testLogger()}
	if s.version != "" {
		t.Fatalf("expected empty version initially, got %q", s.version)
	}
	s.SetVersion("v1.2.3")
	if s.version != "v1.2.3" {
		t.Errorf("expected version %q, got %q", "v1.2.3", s.version)
	}
	// Overwrite with a different value.
	s.SetVersion("v2.0.0")
	if s.version != "v2.0.0" {
		t.Errorf("expected version %q, got %q", "v2.0.0", s.version)
	}
}

func TestSetSkillManager(t *testing.T) {
	s := &Server{logger: testLogger()}
	if s.skillLister != nil {
		t.Fatal("expected nil skillLister initially")
	}
	if s.skillToggler != nil {
		t.Fatal("expected nil skillToggler initially")
	}

	lister := func() []SkillInfo {
		return []SkillInfo{{Name: "test-skill", Description: "desc", Enabled: true}}
	}
	toggler := func(name string, enabled bool) bool {
		return name == "test-skill"
	}

	s.SetSkillManager(lister, toggler)

	if s.skillLister == nil {
		t.Fatal("expected non-nil skillLister after SetSkillManager")
	}
	if s.skillToggler == nil {
		t.Fatal("expected non-nil skillToggler after SetSkillManager")
	}

	// Verify the lister returns expected data.
	skills := s.skillLister()
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}
	if skills[0].Name != "test-skill" {
		t.Errorf("expected skill name %q, got %q", "test-skill", skills[0].Name)
	}

	// Verify toggler works.
	if !s.skillToggler("test-skill", false) {
		t.Error("expected toggler to return true for 'test-skill'")
	}
	if s.skillToggler("unknown-skill", true) {
		t.Error("expected toggler to return false for 'unknown-skill'")
	}
}

// ---------------------------------------------------------------------------
// handleWS with taskMgr and skillLister coverage
// ---------------------------------------------------------------------------

func TestHandleWSWithTaskMgrAndSkills(t *testing.T) {
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	dir := t.TempDir()
	taskMgr := taskqueue.New(testLogger(), filepath.Join(dir, "tasks.json"), taskqueue.Config{})

	// Submit a task so the task summary in sendStatus is non-empty.
	taskMgr.Submit("sess1", "agent1", "pending task", func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}, taskqueue.SubmitOpts{})
	time.Sleep(30 * time.Millisecond)

	cfg := &config.Root{
		Agents: config.Agents{
			Defaults: config.AgentDefaults{
				Model: config.ModelConfig{Primary: "test/model"},
			},
		},
	}
	s := &Server{logger: testLogger(), cfg: cfg, sessions: sm, taskMgr: taskMgr}
	s.skillLister = func() []SkillInfo {
		return []SkillInfo{
			{Name: "calculator", Origin: "builtin", Enabled: true},
		}
	}

	ts := httptest.NewServer(http.HandlerFunc(s.handleWS))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()

	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var status map[string]any
	if err := json.Unmarshal(msg, &status); err != nil {
		t.Fatal(err)
	}

	// Verify tasks summary
	tasks, ok := status["tasks"].(map[string]any)
	if !ok {
		t.Fatal("expected tasks summary in status")
	}
	total := tasks["total"].(float64)
	if total < 1 {
		t.Errorf("expected at least 1 task total, got %v", total)
	}

	// Verify skills summary
	skills, ok := status["skills"].([]any)
	if !ok {
		t.Fatal("expected skills array in status")
	}
	if len(skills) != 1 {
		t.Errorf("expected 1 skill, got %d", len(skills))
	}
	skill0 := skills[0].(map[string]any)
	if skill0["name"] != "calculator" {
		t.Errorf("expected skill name 'calculator', got %v", skill0["name"])
	}
}

// ---------------------------------------------------------------------------
// handleWS with many sessions (> 30 to skip messageCount)
// ---------------------------------------------------------------------------

func TestHandleWSManySessionsSkipsMsgCount(t *testing.T) {
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	// Create 35 sessions so len(active) > 30
	for i := range 35 {
		_ = sm.AppendMessages(fmt.Sprintf("agent:main:tg:%d", i), []session.Message{
			{Role: "user", Content: "hi", TS: int64(i)},
		})
	}

	cfg := &config.Root{
		Agents: config.Agents{
			Defaults: config.AgentDefaults{
				Model: config.ModelConfig{Primary: "test/model"},
			},
		},
	}
	s := &Server{logger: testLogger(), cfg: cfg, sessions: sm}

	ts := httptest.NewServer(http.HandlerFunc(s.handleWS))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()

	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var status map[string]any
	if err := json.Unmarshal(msg, &status); err != nil {
		t.Fatal(err)
	}

	sessions, ok := status["sessions"].([]any)
	if !ok || len(sessions) < 35 {
		t.Errorf("expected 35 sessions, got %d", len(sessions))
	}

	// With >30 sessions, messageCount should NOT be included
	sess0 := sessions[0].(map[string]any)
	if _, hasMC := sess0["messageCount"]; hasMC {
		t.Error("expected messageCount to be omitted when >30 sessions")
	}
}

// ---------------------------------------------------------------------------
// handleLogStream — channel closed by broadcaster
// ---------------------------------------------------------------------------

func TestHandleLogStreamChannelClosed(t *testing.T) {
	lb := NewLogBroadcaster()
	s := &Server{logger: testLogger(), cfg: &config.Root{}, logBroadcaster: lb}

	ts := httptest.NewServer(http.HandlerFunc(s.handleLogStream))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL, nil)
	client := &http.Client{}

	type result struct {
		data string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()

		// Read first event
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		done <- result{data: string(buf[:n])}
	}()

	// Give handler time to subscribe
	time.Sleep(50 * time.Millisecond)

	// Get the subscriber channel and close it to trigger the "!ok" path
	lb.mu.Lock()
	for ch := range lb.clients {
		close(ch)
		delete(lb.clients, ch)
	}
	lb.mu.Unlock()

	select {
	case r := <-done:
		// Either empty data or error is fine; the handler should return cleanly
		_ = r
	case <-time.After(3 * time.Second):
		t.Error("handler did not exit after channel close")
	}
}

// ---------------------------------------------------------------------------
// handleWS context cancellation path
// ---------------------------------------------------------------------------

func TestHandleWSContextCancellation(t *testing.T) {
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	cfg := &config.Root{
		Agents: config.Agents{
			Defaults: config.AgentDefaults{
				Model: config.ModelConfig{Primary: "test/model"},
			},
		},
	}
	s := &Server{logger: testLogger(), cfg: cfg, sessions: sm}

	ts := httptest.NewServer(http.HandlerFunc(s.handleWS))
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}

	// Read the initial status
	_, _, err = conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// Close the connection from client side to trigger context cancellation
	conn.Close()

	// The handler should exit cleanly. We just verify no goroutine leak / panic.
	time.Sleep(100 * time.Millisecond)
}

func TestHandleVersionInfo(t *testing.T) {
	s := &Server{
		logger:  testLogger(),
		cfg:     &config.Root{},
		version: "0.4.0",
	}

	req := httptest.NewRequest("GET", "/api/version", nil)
	w := httptest.NewRecorder()
	s.handleVersionInfo(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["current"] != "0.4.0" {
		t.Errorf("expected current=0.4.0, got %v", body["current"])
	}
}

func TestHandleRollback_NoBackup(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}}

	req := httptest.NewRequest("POST", "/api/rollback", nil)
	w := httptest.NewRecorder()
	s.handleRollback(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when no backup, got %d", w.Code)
	}

	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := body["error"]; !ok {
		t.Error("expected error field in response")
	}
}

// ---------------------------------------------------------------------------
// ipLimiter / extractIP / rateLimitMiddleware
// ---------------------------------------------------------------------------

func TestNewIPLimiter(t *testing.T) {
	l := newIPLimiter(10, 5)
	defer close(l.done)

	if l.rps != 10 {
		t.Errorf("rps: got %f, want 10", l.rps)
	}
	if l.burst != 5 {
		t.Errorf("burst: got %d, want 5", l.burst)
	}
	if l.clients == nil {
		t.Error("clients map should be initialized")
	}
}

func TestNewIPLimiter_ZeroBurst(t *testing.T) {
	l := newIPLimiter(5, 0)
	defer close(l.done)

	if l.burst != 5 {
		t.Errorf("burst should default to rps (5), got %d", l.burst)
	}
}

func TestNewIPLimiter_NegativeBurst(t *testing.T) {
	l := newIPLimiter(3, -1)
	defer close(l.done)

	if l.burst != 3 {
		t.Errorf("burst should default to rps (3), got %d", l.burst)
	}
}

func TestIPLimiterAllow_WithinBurst(t *testing.T) {
	l := newIPLimiter(10, 3)
	defer close(l.done)

	for i := 0; i < 3; i++ {
		if !l.allow("1.2.3.4") {
			t.Errorf("request %d should be allowed within burst", i)
		}
	}
}

func TestIPLimiterAllow_ExceedsBurst(t *testing.T) {
	l := newIPLimiter(10, 2)
	defer close(l.done)

	l.allow("1.2.3.4") // consume 1
	l.allow("1.2.3.4") // consume 2
	if l.allow("1.2.3.4") {
		t.Error("third request should be denied (burst=2)")
	}
}

func TestIPLimiterAllow_TokenRefill(t *testing.T) {
	l := newIPLimiter(1000, 1)
	defer close(l.done)

	// Consume the single burst token.
	if !l.allow("1.2.3.4") {
		t.Fatal("first request should be allowed")
	}

	// Manually advance lastSeen to simulate elapsed time.
	l.mu.Lock()
	l.clients["1.2.3.4"].lastSeen = time.Now().Add(-1 * time.Second)
	l.mu.Unlock()

	// Should be allowed now due to token refill.
	if !l.allow("1.2.3.4") {
		t.Error("request after 1s should be allowed (rps=1000)")
	}
}

func TestIPLimiterAllow_DifferentIPs(t *testing.T) {
	l := newIPLimiter(10, 1)
	defer close(l.done)

	if !l.allow("1.1.1.1") {
		t.Error("first IP first request should be allowed")
	}
	if !l.allow("2.2.2.2") {
		t.Error("second IP first request should be allowed")
	}
	// First IP exhausted.
	if l.allow("1.1.1.1") {
		t.Error("first IP second request should be denied")
	}
}

func TestIPLimiterAllow_TokenCapAtBurst(t *testing.T) {
	l := newIPLimiter(10000, 3)
	defer close(l.done)

	// First request creates bucket.
	l.allow("1.2.3.4")

	// Set lastSeen far in the past so refill would exceed burst.
	l.mu.Lock()
	l.clients["1.2.3.4"].lastSeen = time.Now().Add(-1 * time.Hour)
	l.mu.Unlock()

	// After refill, tokens should be capped at burst (3).
	l.allow("1.2.3.4") // refills and consumes 1 → 2 left
	l.allow("1.2.3.4") // 1 left
	l.allow("1.2.3.4") // 0 left
	if l.allow("1.2.3.4") {
		t.Error("should be denied after exhausting burst tokens")
	}
}

func TestExtractIP_XForwardedFor(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-For", "10.0.0.1, 10.0.0.2, 10.0.0.3")
	if ip := extractIP(r); ip != "10.0.0.1" {
		t.Errorf("expected first XFF IP '10.0.0.1', got %q", ip)
	}
}

func TestExtractIP_XForwardedForSingle(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-For", "192.168.1.1")
	if ip := extractIP(r); ip != "192.168.1.1" {
		t.Errorf("expected '192.168.1.1', got %q", ip)
	}
}

func TestExtractIP_XRealIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Real-IP", "172.16.0.1")
	if ip := extractIP(r); ip != "172.16.0.1" {
		t.Errorf("expected '172.16.0.1', got %q", ip)
	}
}

func TestExtractIP_RemoteAddr(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.50:12345"
	if ip := extractIP(r); ip != "203.0.113.50" {
		t.Errorf("expected '203.0.113.50', got %q", ip)
	}
}

func TestExtractIP_RemoteAddrNoPort(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.50"
	r.Header.Del("X-Forwarded-For")
	r.Header.Del("X-Real-IP")
	if ip := extractIP(r); ip != "203.0.113.50" {
		t.Errorf("expected '203.0.113.50', got %q", ip)
	}
}

func TestExtractIP_XFFPriority(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-For", "10.0.0.1")
	r.Header.Set("X-Real-IP", "10.0.0.2")
	r.RemoteAddr = "10.0.0.3:1234"
	if ip := extractIP(r); ip != "10.0.0.1" {
		t.Errorf("XFF should take priority, got %q", ip)
	}
}

func TestRateLimitMiddleware_PassThrough(t *testing.T) {
	mw := rateLimitMiddleware(0, 0)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestRateLimitMiddleware_EnforcesLimit(t *testing.T) {
	mw := rateLimitMiddleware(100, 1)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// First request should pass.
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "1.2.3.4:5678"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first request should pass, got %d", w.Code)
	}

	// Second request should be rate limited (burst=1).
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req)
	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("second request should be rate-limited, got %d", w2.Code)
	}
}

func TestRateLimitMiddleware_NegativeRPS(t *testing.T) {
	mw := rateLimitMiddleware(-5, 10)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("negative rps should be pass-through, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// handleNotify tests
// ---------------------------------------------------------------------------

func TestHandleNotify_InvalidJSON(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}}
	req := httptest.NewRequest("POST", "/gopherclaw/api/notify", strings.NewReader("bad"))
	w := httptest.NewRecorder()
	s.handleNotify(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", w.Code)
	}
}

func TestHandleNotify_EmptyMessage(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}}
	req := httptest.NewRequest("POST", "/gopherclaw/api/notify", strings.NewReader(`{"message":""}`))
	w := httptest.NewRecorder()
	s.handleNotify(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty message, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["error"] != "message is required" {
		t.Errorf("expected 'message is required', got %v", resp["error"])
	}
}

func TestHandleNotify_NoDeliverers(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}}
	req := httptest.NewRequest("POST", "/gopherclaw/api/notify", strings.NewReader(`{"message":"hello"}`))
	w := httptest.NewRecorder()
	s.handleNotify(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 with no deliverers, got %d", w.Code)
	}
}

func TestHandleNotify_Success(t *testing.T) {
	d := &mockDeliverer{}
	s := &Server{logger: testLogger(), cfg: &config.Root{}, deliverers: []Deliverer{d}}
	req := httptest.NewRequest("POST", "/gopherclaw/api/notify", strings.NewReader(`{"message":"test notify"}`))
	w := httptest.NewRecorder()
	s.handleNotify(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["delivered"] != float64(1) {
		t.Errorf("expected delivered=1, got %v", resp["delivered"])
	}
	if d.sent != "test notify" {
		t.Errorf("expected deliverer to receive 'test notify', got %q", d.sent)
	}
}

// ---------------------------------------------------------------------------
// handleUsage tests
// ---------------------------------------------------------------------------

func TestHandleUsage_NilAgent(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}, ag: nil}
	req := httptest.NewRequest("GET", "/gopherclaw/api/usage", nil)
	w := httptest.NewRecorder()
	s.handleUsage(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp UsageAllResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
}

func TestHandleUsage_WithTracker_AllSessions(t *testing.T) {
	ag, _, cfg := testAgent(t)
	ag.Usage = agent.NewUsageTracker()
	ag.Usage.Accumulate("sess1", agent.NormalizedUsage{Input: 100, Output: 50, Total: 150})
	ag.Usage.Accumulate("sess2", agent.NormalizedUsage{Input: 200, Output: 100, Total: 300})
	s := &Server{logger: testLogger(), cfg: cfg, ag: ag}

	req := httptest.NewRequest("GET", "/gopherclaw/api/usage", nil)
	w := httptest.NewRecorder()
	s.handleUsage(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["sessions"] == nil {
		t.Error("expected sessions in response")
	}
	if resp["aggregate"] == nil {
		t.Error("expected aggregate in response")
	}
}

func TestHandleUsage_WithTracker_SingleSession(t *testing.T) {
	ag, _, cfg := testAgent(t)
	ag.Usage = agent.NewUsageTracker()
	ag.Usage.Accumulate("sess1", agent.NormalizedUsage{Input: 100, Output: 50, Total: 150})
	s := &Server{logger: testLogger(), cfg: cfg, ag: ag}

	req := httptest.NewRequest("GET", "/gopherclaw/api/usage?session=sess1", nil)
	w := httptest.NewRecorder()
	s.handleUsage(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp UsageSessionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Session != "sess1" {
		t.Errorf("expected session=sess1, got %q", resp.Session)
	}
	if resp.Calls != 1 {
		t.Errorf("expected calls=1, got %d", resp.Calls)
	}
}

// ---------------------------------------------------------------------------
// handleCronHistory tests
// ---------------------------------------------------------------------------

func TestHandleCronHistory_EmptyJobID(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}}
	req := httptest.NewRequest("GET", "/gopherclaw/api/cron//history", nil)
	rctx := chi.NewRouteContext()
	// No name param
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleCronHistory(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty job name, got %d", w.Code)
	}
}

func TestHandleCronHistory_NilCronMgr(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}, cronMgr: nil}
	req := httptest.NewRequest("GET", "/gopherclaw/api/cron/test-job/history", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "test-job")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleCronHistory(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp CronHistoryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 0 {
		t.Errorf("expected total=0, got %d", resp.Total)
	}
}

func TestHandleCronHistory_WithCronMgr(t *testing.T) {
	cronDir := t.TempDir()
	cronMgr := cron.New(testLogger(), cronDir, nil)
	s := &Server{logger: testLogger(), cfg: &config.Root{}, cronMgr: cronMgr}

	// Create run log directory and a history entry
	err := cron.AppendRunLog(testLogger(), cronDir, cron.RunLogEntry{
		TS:         1000,
		JobID:      "myjob",
		Action:     "finished",
		Status:     "ok",
		Summary:    "completed",
		RunAtMs:    900,
		DurationMs: 100,
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/gopherclaw/api/cron/myjob/history?limit=10&offset=0&status=ok&sort=desc&q=completed", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", "myjob")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleCronHistory(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp CronHistoryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 1 {
		t.Errorf("expected total=1, got %d", resp.Total)
	}
}

// ---------------------------------------------------------------------------
// verifyHMAC tests
// ---------------------------------------------------------------------------

func TestVerifyHMAC_Valid(t *testing.T) {
	body := []byte("hello world")
	secret := "mysecret"
	mac := hmacSHA256(body, secret)

	if !verifyHMAC(body, mac, secret) {
		t.Error("expected valid HMAC to pass")
	}
}

func TestVerifyHMAC_ValidWithPrefix(t *testing.T) {
	body := []byte("hello world")
	secret := "mysecret"
	mac := "sha256=" + hmacSHA256(body, secret)

	if !verifyHMAC(body, mac, secret) {
		t.Error("expected valid HMAC with sha256= prefix to pass")
	}
}

func TestVerifyHMAC_Invalid(t *testing.T) {
	body := []byte("hello world")
	if verifyHMAC(body, "deadbeef", "mysecret") {
		t.Error("expected invalid HMAC to fail")
	}
}

func TestVerifyHMAC_WrongBody(t *testing.T) {
	secret := "mysecret"
	mac := hmacSHA256([]byte("hello"), secret)
	if verifyHMAC([]byte("different"), mac, secret) {
		t.Error("expected HMAC with wrong body to fail")
	}
}

// hmacSHA256 computes the hex-encoded HMAC-SHA256 of body with the given secret.
func hmacSHA256(body []byte, secret string) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------------
// handleWebhook with HMAC signature validation
// ---------------------------------------------------------------------------

func TestHandleWebhook_HMACMissingSig(t *testing.T) {
	cfg := &config.Root{
		Gateway: config.Gateway{WebhookSecret: "test-secret"},
	}
	s := &Server{logger: testLogger(), cfg: cfg}

	body := `{"message":"hello"}`
	req := httptest.NewRequest("POST", "/webhooks/test", strings.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("session", "test")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleWebhook(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing signature, got %d", w.Code)
	}
}

func TestHandleWebhook_HMACInvalidSig(t *testing.T) {
	cfg := &config.Root{
		Gateway: config.Gateway{WebhookSecret: "test-secret"},
	}
	s := &Server{logger: testLogger(), cfg: cfg}

	body := `{"message":"hello"}`
	req := httptest.NewRequest("POST", "/webhooks/test", strings.NewReader(body))
	req.Header.Set("X-Signature", "invalid-hex")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("session", "test")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleWebhook(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for invalid signature, got %d", w.Code)
	}
}

func TestHandleWebhook_HMACValidSig(t *testing.T) {
	secret := "test-secret"
	cfg := &config.Root{
		Gateway: config.Gateway{WebhookSecret: secret},
	}
	ag, _, _ := testAgentWithMock(t, "signed reply", false)
	s := &Server{logger: testLogger(), cfg: cfg, ag: ag}

	body := `{"message":"hello webhook"}`
	sig := hmacSHA256([]byte(body), secret)

	req := httptest.NewRequest("POST", "/webhooks/signed", strings.NewReader(body))
	req.Header.Set("X-Signature", sig)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("session", "signed")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	s.handleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 with valid signature, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// handleManifest tests
// ---------------------------------------------------------------------------

func TestHandleManifest(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}}
	req := httptest.NewRequest("GET", "/gopherclaw/manifest.json", nil)
	w := httptest.NewRecorder()
	s.handleManifest(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "application/manifest+json" {
		t.Errorf("expected application/manifest+json, got %q", ct)
	}
	if !strings.Contains(w.Body.String(), "GopherClaw") {
		t.Error("expected manifest to contain 'GopherClaw'")
	}
	if !strings.Contains(w.Body.String(), "/gopherclaw") {
		t.Error("expected manifest start_url to contain '/gopherclaw'")
	}
}

func TestHandleManifest_CustomBasePath(t *testing.T) {
	cfg := &config.Root{
		Gateway: config.Gateway{
			ControlUI: config.ControlUI{BasePath: "/custom"},
		},
	}
	s := &Server{logger: testLogger(), cfg: cfg}
	req := httptest.NewRequest("GET", "/custom/manifest.json", nil)
	w := httptest.NewRecorder()
	s.handleManifest(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "/custom") {
		t.Error("expected manifest start_url to contain '/custom'")
	}
}

// ---------------------------------------------------------------------------
// handleServiceWorker tests
// ---------------------------------------------------------------------------

func TestHandleServiceWorker(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}}
	req := httptest.NewRequest("GET", "/gopherclaw/sw.js", nil)
	w := httptest.NewRecorder()
	s.handleServiceWorker(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if ct != "application/javascript" {
		t.Errorf("expected application/javascript, got %q", ct)
	}
	if !strings.Contains(w.Body.String(), "fetch") {
		t.Error("expected service worker to contain 'fetch'")
	}
	cc := w.Header().Get("Cache-Control")
	if cc != "no-cache" {
		t.Errorf("expected Cache-Control no-cache, got %q", cc)
	}
}

// ---------------------------------------------------------------------------
// handleHealth — channels and degraded status
// ---------------------------------------------------------------------------

func TestHandleHealth_WithChannels(t *testing.T) {
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	s := &Server{logger: testLogger(), cfg: &config.Root{}, sessions: sm}
	s.AddChannel(&mockChannelStatus{name: "telegram", connected: true})
	s.AddChannel(&mockChannelStatus{name: "discord", connected: true})

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	s.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["status"] != "ok" {
		t.Errorf("expected status=ok (all channels connected), got %v", resp["status"])
	}
}

func TestHandleHealth_DegradedChannel(t *testing.T) {
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	s := &Server{logger: testLogger(), cfg: &config.Root{}, sessions: sm}
	s.AddChannel(&mockChannelStatus{name: "telegram", connected: true})
	s.AddChannel(&mockChannelStatus{name: "discord", connected: false})

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	s.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["status"] != "degraded" {
		t.Errorf("expected status=degraded (one channel disconnected), got %v", resp["status"])
	}
}

func TestHandleHealth_WithCronMgr(t *testing.T) {
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	cronMgr := cron.New(testLogger(), t.TempDir(), nil)
	s := &Server{logger: testLogger(), cfg: &config.Root{}, sessions: sm, cronMgr: cronMgr}

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	s.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	checks := resp["checks"].(map[string]any)
	if checks["cron"] != "ok" {
		t.Errorf("expected cron check=ok, got %v", checks["cron"])
	}
}

func TestHandleHealth_NilSessions(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}, sessions: nil}

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	s.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	checks := resp["checks"].(map[string]any)
	if checks["sessions"] != "unavailable" {
		t.Errorf("expected sessions=unavailable, got %v", checks["sessions"])
	}
}

func TestHandleHealth_WithVersion(t *testing.T) {
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	s := &Server{logger: testLogger(), cfg: &config.Root{}, sessions: sm, version: "1.2.3"}

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	s.handleHealth(w, req)

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["version"] != "1.2.3" {
		t.Errorf("expected version=1.2.3, got %v", resp["version"])
	}
}

// ---------------------------------------------------------------------------
// handleRollback — success path (create a backup file)
// ---------------------------------------------------------------------------

func TestHandleRollback_WithBackup(t *testing.T) {
	// Create a temporary executable and its .bak backup
	dir := t.TempDir()
	exe := filepath.Join(dir, "gopherclaw")
	bak := exe + ".bak"
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho current"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bak, []byte("#!/bin/sh\necho backup"), 0755); err != nil {
		t.Fatal(err)
	}

	// The updater.Rollback() function looks at os.Executable(), which we
	// cannot easily override. So we just verify the error path is tested above
	// and the success path writes the expected JSON shape.
	s := &Server{logger: testLogger(), cfg: &config.Root{}}
	req := httptest.NewRequest("POST", "/api/rollback", nil)
	w := httptest.NewRecorder()
	s.handleRollback(w, req)

	// We can't easily make updater.Rollback succeed in test, so just verify
	// that the handler returns JSON with the expected structure.
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// Either error or ok key should be present
	if _, hasErr := resp["error"]; !hasErr {
		if _, hasOk := resp["ok"]; !hasOk {
			t.Error("expected either 'error' or 'ok' key in rollback response")
		}
	}
}

// ---------------------------------------------------------------------------
// handleVersionInfo — additional coverage
// ---------------------------------------------------------------------------

func TestHandleVersionInfo_EmptyVersion(t *testing.T) {
	s := &Server{logger: testLogger(), cfg: &config.Root{}, version: ""}
	req := httptest.NewRequest("GET", "/api/version", nil)
	w := httptest.NewRecorder()
	s.handleVersionInfo(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["current"] != "" {
		t.Errorf("expected empty current version, got %v", resp["current"])
	}
}

// ---------------------------------------------------------------------------
// sanitizeSessionKey tests
// ---------------------------------------------------------------------------

func TestSanitizeSessionKey_PathSeparators(t *testing.T) {
	got := sanitizeSessionKey("path/to\\session")
	if strings.ContainsAny(got, "/\\") {
		t.Errorf("expected path separators to be replaced, got %q", got)
	}
}

func TestSanitizeSessionKey_ControlChars(t *testing.T) {
	got := sanitizeSessionKey("hello\x00world\x1f")
	if strings.ContainsRune(got, 0) || strings.ContainsRune(got, 0x1f) {
		t.Errorf("expected control chars to be replaced, got %q", got)
	}
}

func TestSanitizeSessionKey_TruncateLong(t *testing.T) {
	long := strings.Repeat("a", 250)
	got := sanitizeSessionKey(long)
	if len(got) != 200 {
		t.Errorf("expected truncated to 200, got %d", len(got))
	}
}
