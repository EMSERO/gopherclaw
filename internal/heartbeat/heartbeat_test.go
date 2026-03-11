package heartbeat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/agentapi"
	"github.com/EMSERO/gopherclaw/internal/config"
)

func TestStripHeartbeatToken(t *testing.T) {
	tests := []struct {
		name          string
		raw           string
		maxAckChars   int
		wantText      string
		wantDeliver   bool
	}{
		{"empty", "", 300, "", false},
		{"just token", "HEARTBEAT_OK", 300, "", false},
		{"token with whitespace", "  HEARTBEAT_OK  ", 300, "", false},
		{"token with markdown bold", "**HEARTBEAT_OK**", 300, "", false},
		{"token with trailing punct", "HEARTBEAT_OK!!!", 300, "", false},
		{"token + short note", "HEARTBEAT_OK All clear.", 300, "", false},
		{"token + long text", "HEARTBEAT_OK " + string(make([]byte, 400)), 300, "", true},
		{"no token", "Something happened that needs attention.", 300, "Something happened that needs attention.", true},
		{"token in backticks", "`HEARTBEAT_OK`", 300, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, deliver := StripHeartbeatToken(tt.raw, tt.maxAckChars)
			if deliver != tt.wantDeliver {
				t.Errorf("deliver = %v, want %v", deliver, tt.wantDeliver)
			}
			if tt.wantDeliver && tt.wantText != "" && text != tt.wantText {
				t.Errorf("text = %q, want %q", text, tt.wantText)
			}
		})
	}
}

func TestIsEffectivelyEmpty(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"empty", "", true},
		{"only headers", "# Heartbeat\n## Checks", true},
		{"header + empty list", "# Heartbeat\n- [ ]", true},
		{"has content", "# Heartbeat\n- Check email inbox", false},
		{"comment only", "# Heartbeat\n<!-- nothing -->", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isEffectivelyEmpty(tt.content); got != tt.want {
				t.Errorf("isEffectivelyEmpty = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- Mocks ---

type mockChatter struct {
	mu           sync.Mutex
	chatCalls    []string
	chatLightCalls []string
	chatResp     agentapi.Response
	chatErr      error
	modelSet     string
	modelCleared bool
}

func (m *mockChatter) Chat(ctx context.Context, sessionKey, message string) (agentapi.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chatCalls = append(m.chatCalls, message)
	return m.chatResp, m.chatErr
}

func (m *mockChatter) ChatLight(ctx context.Context, sessionKey, message string) (agentapi.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chatLightCalls = append(m.chatLightCalls, message)
	return m.chatResp, m.chatErr
}

func (m *mockChatter) SetSessionModel(key, model string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.modelSet = model
}

func (m *mockChatter) ClearSessionModel(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.modelCleared = true
}

type mockDeliverer struct {
	mu       sync.Mutex
	messages []string
}

func (d *mockDeliverer) SendToAllPaired(text string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.messages = append(d.messages, text)
}

func testLogger() *zap.SugaredLogger {
	l, _ := zap.NewDevelopment()
	return l.Sugar()
}

// --- NewRunner tests ---

func TestNewRunner_DefaultSessionKey(t *testing.T) {
	r := NewRunner(RunnerOpts{
		Logger: testLogger(),
		Agent:  &mockChatter{},
		CfgFn:  func() config.HeartbeatConfig { return config.HeartbeatConfig{} },
	})
	if r.sessionKey != "heartbeat:main" {
		t.Errorf("sessionKey = %q, want %q", r.sessionKey, "heartbeat:main")
	}
}

func TestNewRunner_CustomSessionKey(t *testing.T) {
	r := NewRunner(RunnerOpts{
		Logger:     testLogger(),
		Agent:      &mockChatter{},
		CfgFn:      func() config.HeartbeatConfig { return config.HeartbeatConfig{} },
		SessionKey: "custom:key",
	})
	if r.sessionKey != "custom:key" {
		t.Errorf("sessionKey = %q, want %q", r.sessionKey, "custom:key")
	}
}

func TestNewRunner_Fields(t *testing.T) {
	agent := &mockChatter{}
	deliverers := []Deliverer{&mockDeliverer{}}
	r := NewRunner(RunnerOpts{
		Logger:     testLogger(),
		Agent:      agent,
		CfgFn:      func() config.HeartbeatConfig { return config.HeartbeatConfig{} },
		UserTZFn:   func() string { return "US/Eastern" },
		Workspace:  "/tmp/ws",
		Deliverers: deliverers,
	})
	if r.agent != agent {
		t.Error("agent not set")
	}
	if r.workspace != "/tmp/ws" {
		t.Errorf("workspace = %q", r.workspace)
	}
	if len(r.deliverers) != 1 {
		t.Errorf("deliverers len = %d", len(r.deliverers))
	}
}

// --- Start tests ---

func TestStart_DisabledConfig_ExitsOnCancel(t *testing.T) {
	r := NewRunner(RunnerOpts{
		Logger: testLogger(),
		Agent:  &mockChatter{},
		CfgFn:  func() config.HeartbeatConfig { return config.HeartbeatConfig{Every: ""} },
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	err := r.Start(ctx)
	if err != nil {
		t.Errorf("Start returned error: %v", err)
	}
}

func TestStart_EnabledConfig_ExitsOnCancel(t *testing.T) {
	r := NewRunner(RunnerOpts{
		Logger:   testLogger(),
		Agent:    &mockChatter{chatResp: agentapi.Response{Text: "HEARTBEAT_OK"}},
		CfgFn:    func() config.HeartbeatConfig { return config.HeartbeatConfig{Every: "1ms"} },
		UserTZFn: func() string { return "UTC" },
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := r.Start(ctx)
	if err != nil {
		t.Errorf("Start returned error: %v", err)
	}
}

func TestStart_InvalidInterval_DefaultsTo30m(t *testing.T) {
	// We can't easily test the 30m default wait, but we can verify it doesn't
	// crash and exits on cancel. The real validation is in tick tests.
	r := NewRunner(RunnerOpts{
		Logger:   testLogger(),
		Agent:    &mockChatter{},
		CfgFn:    func() config.HeartbeatConfig { return config.HeartbeatConfig{Every: "invalid"} },
		UserTZFn: func() string { return "UTC" },
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := r.Start(ctx)
	if err != nil {
		t.Errorf("Start returned error: %v", err)
	}
}

// --- tick tests ---

func TestTick_OutsideActiveHours_Skips(t *testing.T) {
	agent := &mockChatter{}
	r := NewRunner(RunnerOpts{
		Logger:   testLogger(),
		Agent:    agent,
		CfgFn:    func() config.HeartbeatConfig { return config.HeartbeatConfig{} },
		UserTZFn: func() string { return "UTC" },
	})
	// Use a 1-hour window 12 hours from now so we're always outside it.
	outside := time.Now().UTC().Add(12 * time.Hour)
	cfg := config.HeartbeatConfig{
		Every: "1m",
		ActiveHours: &config.ActiveHoursConfig{
			Start: outside.Format("15:04"),
			End:   outside.Add(1 * time.Hour).Format("15:04"),
		},
	}
	r.tick(context.Background(), cfg)
	if len(agent.chatCalls) > 0 || len(agent.chatLightCalls) > 0 {
		t.Error("expected no agent calls when outside active hours")
	}
}

func TestTick_ActiveHoursUserTZ(t *testing.T) {
	agent := &mockChatter{}
	r := NewRunner(RunnerOpts{
		Logger:   testLogger(),
		Agent:    agent,
		CfgFn:    func() config.HeartbeatConfig { return config.HeartbeatConfig{} },
		UserTZFn: func() string { return "UTC" },
	})
	// Use a 1-hour window 12 hours from now so we're always outside it.
	outside := time.Now().UTC().Add(12 * time.Hour)
	cfg := config.HeartbeatConfig{
		Every: "1m",
		ActiveHours: &config.ActiveHoursConfig{
			Start:    outside.Format("15:04"),
			End:      outside.Add(1 * time.Hour).Format("15:04"),
			Timezone: "user",
		},
	}
	r.tick(context.Background(), cfg)
	if len(agent.chatCalls) > 0 || len(agent.chatLightCalls) > 0 {
		t.Error("expected no agent calls when outside active hours with user tz")
	}
}

func TestTick_EmptyHeartbeatMD_Skips(t *testing.T) {
	// Create a temp workspace with an empty HEARTBEAT.md
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "HEARTBEAT.md"), []byte("# Heartbeat\n"), 0644)

	agent := &mockChatter{}
	r := NewRunner(RunnerOpts{
		Logger:    testLogger(),
		Agent:     agent,
		CfgFn:     func() config.HeartbeatConfig { return config.HeartbeatConfig{} },
		UserTZFn:  func() string { return "UTC" },
		Workspace: dir,
	})
	cfg := config.HeartbeatConfig{Every: "1m"}
	r.tick(context.Background(), cfg)
	if len(agent.chatCalls) > 0 || len(agent.chatLightCalls) > 0 {
		t.Error("expected no agent calls when HEARTBEAT.md is effectively empty")
	}
}

func TestTick_NonEmptyHeartbeatMD_CallsChat(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "HEARTBEAT.md"), []byte("# Heartbeat\n- Check something"), 0644)

	agent := &mockChatter{chatResp: agentapi.Response{Text: "HEARTBEAT_OK"}}
	r := NewRunner(RunnerOpts{
		Logger:    testLogger(),
		Agent:     agent,
		CfgFn:     func() config.HeartbeatConfig { return config.HeartbeatConfig{} },
		UserTZFn:  func() string { return "UTC" },
		Workspace: dir,
	})
	cfg := config.HeartbeatConfig{Every: "1m"}
	r.tick(context.Background(), cfg)
	if len(agent.chatCalls) != 1 {
		t.Errorf("expected 1 chat call, got %d", len(agent.chatCalls))
	}
}

func TestTick_LightContext_UsesChatLight(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "HEARTBEAT.md"), []byte("- Do stuff"), 0644)

	agent := &mockChatter{chatResp: agentapi.Response{Text: "HEARTBEAT_OK"}}
	r := NewRunner(RunnerOpts{
		Logger:    testLogger(),
		Agent:     agent,
		CfgFn:     func() config.HeartbeatConfig { return config.HeartbeatConfig{} },
		UserTZFn:  func() string { return "UTC" },
		Workspace: dir,
	})
	cfg := config.HeartbeatConfig{Every: "1m", LightContext: true}
	r.tick(context.Background(), cfg)
	if len(agent.chatLightCalls) != 1 {
		t.Errorf("expected 1 ChatLight call, got %d", len(agent.chatLightCalls))
	}
	if len(agent.chatCalls) != 0 {
		t.Errorf("expected 0 Chat calls, got %d", len(agent.chatCalls))
	}
}

func TestTick_ModelOverride_SetsAndClears(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "HEARTBEAT.md"), []byte("- task"), 0644)

	agent := &mockChatter{chatResp: agentapi.Response{Text: "HEARTBEAT_OK"}}
	r := NewRunner(RunnerOpts{
		Logger:       testLogger(),
		Agent:        agent,
		CfgFn:        func() config.HeartbeatConfig { return config.HeartbeatConfig{} },
		UserTZFn:     func() string { return "UTC" },
		ResolveAlias: func(s string) string { return "resolved:" + s },
		Workspace:    dir,
	})
	cfg := config.HeartbeatConfig{Every: "1m", Model: "fast"}
	r.tick(context.Background(), cfg)

	agent.mu.Lock()
	defer agent.mu.Unlock()
	if agent.modelSet != "resolved:fast" {
		t.Errorf("modelSet = %q, want %q", agent.modelSet, "resolved:fast")
	}
	if !agent.modelCleared {
		t.Error("expected model to be cleared after tick")
	}
}

func TestTick_ModelOverride_NoResolveAlias(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "HEARTBEAT.md"), []byte("- task"), 0644)

	agent := &mockChatter{chatResp: agentapi.Response{Text: "HEARTBEAT_OK"}}
	r := NewRunner(RunnerOpts{
		Logger:    testLogger(),
		Agent:     agent,
		CfgFn:     func() config.HeartbeatConfig { return config.HeartbeatConfig{} },
		UserTZFn:  func() string { return "UTC" },
		Workspace: dir,
		// resolveAlias is nil
	})
	cfg := config.HeartbeatConfig{Every: "1m", Model: "exact-model"}
	r.tick(context.Background(), cfg)

	agent.mu.Lock()
	defer agent.mu.Unlock()
	if agent.modelSet != "exact-model" {
		t.Errorf("modelSet = %q, want %q", agent.modelSet, "exact-model")
	}
}

func TestTick_AgentError_NoDelivery(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "HEARTBEAT.md"), []byte("- task"), 0644)

	agent := &mockChatter{chatErr: errors.New("agent fail")}
	deliverer := &mockDeliverer{}
	r := NewRunner(RunnerOpts{
		Logger:     testLogger(),
		Agent:      agent,
		CfgFn:      func() config.HeartbeatConfig { return config.HeartbeatConfig{} },
		UserTZFn:   func() string { return "UTC" },
		Workspace:  dir,
		Deliverers: []Deliverer{deliverer},
	})
	cfg := config.HeartbeatConfig{Every: "1m", Target: "last"}
	r.tick(context.Background(), cfg)

	deliverer.mu.Lock()
	defer deliverer.mu.Unlock()
	if len(deliverer.messages) > 0 {
		t.Error("expected no delivery on agent error")
	}
}

func TestTick_HeartbeatOK_NoDelivery(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "HEARTBEAT.md"), []byte("- task"), 0644)

	agent := &mockChatter{chatResp: agentapi.Response{Text: "HEARTBEAT_OK"}}
	deliverer := &mockDeliverer{}
	r := NewRunner(RunnerOpts{
		Logger:     testLogger(),
		Agent:      agent,
		CfgFn:      func() config.HeartbeatConfig { return config.HeartbeatConfig{} },
		UserTZFn:   func() string { return "UTC" },
		Workspace:  dir,
		Deliverers: []Deliverer{deliverer},
	})
	cfg := config.HeartbeatConfig{Every: "1m", Target: "last"}
	r.tick(context.Background(), cfg)

	deliverer.mu.Lock()
	defer deliverer.mu.Unlock()
	if len(deliverer.messages) > 0 {
		t.Error("expected no delivery for HEARTBEAT_OK response")
	}
}

func TestTick_NonOK_TargetNone_NoDelivery(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "HEARTBEAT.md"), []byte("- task"), 0644)

	agent := &mockChatter{chatResp: agentapi.Response{Text: "Something needs attention!"}}
	deliverer := &mockDeliverer{}
	r := NewRunner(RunnerOpts{
		Logger:     testLogger(),
		Agent:      agent,
		CfgFn:      func() config.HeartbeatConfig { return config.HeartbeatConfig{} },
		UserTZFn:   func() string { return "UTC" },
		Workspace:  dir,
		Deliverers: []Deliverer{deliverer},
	})
	cfg := config.HeartbeatConfig{Every: "1m", Target: "none"}
	r.tick(context.Background(), cfg)

	deliverer.mu.Lock()
	defer deliverer.mu.Unlock()
	if len(deliverer.messages) > 0 {
		t.Error("expected no delivery when target=none")
	}
}

func TestTick_NonOK_EmptyTarget_NoDelivery(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "HEARTBEAT.md"), []byte("- task"), 0644)

	agent := &mockChatter{chatResp: agentapi.Response{Text: "Alert!"}}
	deliverer := &mockDeliverer{}
	r := NewRunner(RunnerOpts{
		Logger:     testLogger(),
		Agent:      agent,
		CfgFn:      func() config.HeartbeatConfig { return config.HeartbeatConfig{} },
		UserTZFn:   func() string { return "UTC" },
		Workspace:  dir,
		Deliverers: []Deliverer{deliverer},
	})
	cfg := config.HeartbeatConfig{Every: "1m", Target: ""}
	r.tick(context.Background(), cfg)

	deliverer.mu.Lock()
	defer deliverer.mu.Unlock()
	if len(deliverer.messages) > 0 {
		t.Error("expected no delivery when target is empty")
	}
}

func TestTick_NonOK_Delivers(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "HEARTBEAT.md"), []byte("- task"), 0644)

	agent := &mockChatter{chatResp: agentapi.Response{Text: "Something needs attention!"}}
	deliverer := &mockDeliverer{}
	r := NewRunner(RunnerOpts{
		Logger:     testLogger(),
		Agent:      agent,
		CfgFn:      func() config.HeartbeatConfig { return config.HeartbeatConfig{} },
		UserTZFn:   func() string { return "UTC" },
		Workspace:  dir,
		Deliverers: []Deliverer{deliverer},
	})
	cfg := config.HeartbeatConfig{Every: "1m", Target: "last"}
	r.tick(context.Background(), cfg)

	deliverer.mu.Lock()
	defer deliverer.mu.Unlock()
	if len(deliverer.messages) != 1 {
		t.Fatalf("expected 1 delivery, got %d", len(deliverer.messages))
	}
	if deliverer.messages[0] != "Something needs attention!" {
		t.Errorf("delivered = %q", deliverer.messages[0])
	}
}

func TestTick_MultipleDeliverers(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "HEARTBEAT.md"), []byte("- task"), 0644)

	agent := &mockChatter{chatResp: agentapi.Response{Text: "Alert!"}}
	d1 := &mockDeliverer{}
	d2 := &mockDeliverer{}
	r := NewRunner(RunnerOpts{
		Logger:     testLogger(),
		Agent:      agent,
		CfgFn:      func() config.HeartbeatConfig { return config.HeartbeatConfig{} },
		UserTZFn:   func() string { return "UTC" },
		Workspace:  dir,
		Deliverers: []Deliverer{d1, d2},
	})
	cfg := config.HeartbeatConfig{Every: "1m", Target: "last"}
	r.tick(context.Background(), cfg)

	if len(d1.messages) != 1 || len(d2.messages) != 1 {
		t.Errorf("expected both deliverers to get 1 message, got %d and %d", len(d1.messages), len(d2.messages))
	}
}

func TestTick_NoWorkspace_CallsChat(t *testing.T) {
	// No workspace set — should skip the HEARTBEAT.md check
	agent := &mockChatter{chatResp: agentapi.Response{Text: "HEARTBEAT_OK"}}
	r := NewRunner(RunnerOpts{
		Logger:   testLogger(),
		Agent:    agent,
		CfgFn:    func() config.HeartbeatConfig { return config.HeartbeatConfig{} },
		UserTZFn: func() string { return "UTC" },
	})
	cfg := config.HeartbeatConfig{Every: "1m"}
	r.tick(context.Background(), cfg)
	if len(agent.chatCalls) != 1 {
		t.Errorf("expected 1 chat call, got %d", len(agent.chatCalls))
	}
}

func TestTick_PromptContainsCurrentTime(t *testing.T) {
	agent := &mockChatter{chatResp: agentapi.Response{Text: "HEARTBEAT_OK"}}
	r := NewRunner(RunnerOpts{
		Logger:   testLogger(),
		Agent:    agent,
		CfgFn:    func() config.HeartbeatConfig { return config.HeartbeatConfig{} },
		UserTZFn: func() string { return "UTC" },
	})
	cfg := config.HeartbeatConfig{Every: "1m"}
	r.tick(context.Background(), cfg)

	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.chatCalls) != 1 {
		t.Fatal("expected 1 chat call")
	}
	if !strings.Contains(agent.chatCalls[0], "Current time:") {
		t.Errorf("prompt should contain 'Current time:', got: %s", agent.chatCalls[0])
	}
	if !strings.Contains(agent.chatCalls[0], "(UTC)") {
		t.Errorf("prompt should contain timezone, got: %s", agent.chatCalls[0])
	}
}

func TestTick_InvalidUserTZ_FallsBackToUTC(t *testing.T) {
	agent := &mockChatter{chatResp: agentapi.Response{Text: "HEARTBEAT_OK"}}
	r := NewRunner(RunnerOpts{
		Logger:   testLogger(),
		Agent:    agent,
		CfgFn:    func() config.HeartbeatConfig { return config.HeartbeatConfig{} },
		UserTZFn: func() string { return "Invalid/Timezone" },
	})
	cfg := config.HeartbeatConfig{Every: "1m"}
	r.tick(context.Background(), cfg)
	// Should not panic; time will be formatted in UTC
	if len(agent.chatCalls) != 1 {
		t.Errorf("expected 1 chat call, got %d", len(agent.chatCalls))
	}
}

// --- truncate tests ---

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"short string", "hello", 10, "hello"},
		{"exact length", "hello", 5, "hello"},
		{"needs truncation", "hello world", 5, "hello..."},
		{"empty", "", 5, ""},
		{"zero limit", "hello", 0, "..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := truncate(tt.s, tt.n); got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.s, tt.n, got, tt.want)
			}
		})
	}
}

// --- Additional isEffectivelyEmpty edge cases ---

func TestIsEffectivelyEmpty_AdditionalCases(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"checked items only", "- [x]\n- [x]", true},
		{"bare asterisk", "* \n*", true},
		{"bare dash", "-\n-", true},
		{"line comment", "// this is a comment", true},
		{"real content mixed", "# Title\n- [ ]\n- Buy groceries", false},
		{"whitespace only", "   \n\n  \t  ", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isEffectivelyEmpty(tt.content); got != tt.want {
				t.Errorf("isEffectivelyEmpty = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- Additional active hours edge cases ---

func TestIsWithinActiveHours_InvalidFormats(t *testing.T) {
	now := time.Date(2026, 3, 9, 14, 30, 0, 0, time.UTC)

	// Invalid start → returns true (allow)
	if got := IsWithinActiveHours("bad", "22:00", "UTC", now); !got {
		t.Error("invalid start should return true")
	}
	// Invalid end → returns true (allow)
	if got := IsWithinActiveHours("09:00", "bad", "UTC", now); !got {
		t.Error("invalid end should return true")
	}
	// Invalid timezone → falls back to UTC
	got := IsWithinActiveHours("09:00", "22:00", "Invalid/TZ", now)
	if !got {
		t.Error("invalid timezone should fall back to UTC, 14:30 is within 09:00-22:00")
	}
}

func TestIsWithinActiveHours_WrappedWindow_Inside(t *testing.T) {
	// 23:30 UTC — inside a 22:00–06:00 window
	now := time.Date(2026, 3, 9, 23, 30, 0, 0, time.UTC)
	if got := IsWithinActiveHours("22:00", "06:00", "UTC", now); !got {
		t.Error("23:30 should be inside 22:00-06:00 wrapped window")
	}
}

func TestIsWithinActiveHours_LocalTimezone(t *testing.T) {
	now := time.Date(2026, 3, 9, 14, 30, 0, 0, time.UTC)
	// "local" and "" should resolve to time.Local
	IsWithinActiveHours("00:00", "24:00", "local", now)
	IsWithinActiveHours("00:00", "24:00", "", now)
	// No panic is the test
}

func TestIsWithinActiveHours(t *testing.T) {
	// Use a fixed time for testing: 2026-03-09 14:30 UTC
	now := time.Date(2026, 3, 9, 14, 30, 0, 0, time.UTC)

	tests := []struct {
		name  string
		start string
		end   string
		tz    string
		want  bool
	}{
		{"within normal window", "09:00", "22:00", "UTC", true},
		{"before window", "15:00", "22:00", "UTC", false},
		{"after window", "09:00", "14:00", "UTC", false},
		{"wrapped window, inside", "22:00", "09:00", "UTC", false}, // 14:30 is outside 22:00-09:00
		{"zero width", "14:00", "14:00", "UTC", false},
		{"end at 24:00", "09:00", "24:00", "UTC", true},
		{"exactly at start", "14:30", "22:00", "UTC", true},
		{"exactly at end (exclusive)", "09:00", "14:30", "UTC", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsWithinActiveHours(tt.start, tt.end, tt.tz, now); got != tt.want {
				t.Errorf("IsWithinActiveHours(%q, %q, %q) = %v, want %v", tt.start, tt.end, tt.tz, got, tt.want)
			}
		})
	}
}
