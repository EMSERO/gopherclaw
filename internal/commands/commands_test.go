package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// mockProvider is a minimal models.Provider that returns a canned response.
type mockProvider struct{}

func (p *mockProvider) Chat(_ context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	return openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{
			{Message: openai.ChatCompletionMessage{Role: "assistant", Content: "summary"}},
		},
	}, nil
}

func (p *mockProvider) ChatStream(_ context.Context, req openai.ChatCompletionRequest) (models.Stream, error) {
	return nil, fmt.Errorf("streaming not implemented in mock")
}

// newTestAgent creates a minimal agent backed by a mock HTTP model server.
func newTestAgent(t *testing.T, cfg *config.Root, sess *session.Manager) *agent.Agent {
	t.Helper()

	// Create a mock HTTP server that returns a valid OpenAI chat completion response.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := openai.ChatCompletionResponse{
			Choices: []openai.ChatCompletionChoice{
				{Message: openai.ChatCompletionMessage{Role: "assistant", Content: "summary"}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	providers := map[string]models.Provider{
		"test": &mockProvider{},
	}
	router := models.NewRouter(testLogger(), providers, "test/mock-model", nil)

	def := &config.AgentDef{
		ID:      "main",
		Default: true,
		Identity: config.Identity{
			Name:  "TestBot",
			Theme: "a test bot",
		},
	}

	return agent.New(testLogger(), cfg, def, router, sess, nil, nil, "", nil)
}

// newTestConfig returns a minimal config with sensible defaults for testing.
func newTestConfig() *config.Root {
	return &config.Root{
		Agents: config.Agents{
			Defaults: config.AgentDefaults{
				Model: config.ModelConfig{
					Primary: "test/mock-model",
				},
				UserTimezone:   "UTC",
				TimeoutSeconds: 30,
				ContextPruning: config.ContextPruning{
					KeepLastAssistants: 2,
					HardClearRatio:     0.5,
				},
				LoopDetectionN: 3,
			},
		},
		Session: config.Session{
			MaxConcurrent: 2,
		},
	}
}

// newTestCronManager creates a cron.Manager in a temp directory.
func newTestCronManager(t *testing.T) *cron.Manager {
	t.Helper()
	dir := t.TempDir()
	mgr := cron.New(testLogger(), dir, func(ctx context.Context, job *cron.Job) cron.RunResult {
		return cron.RunResult{Text: "ok"}
	})
	return mgr
}

// --- Handle tests ---

func TestHandleHelp(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	cronMgr := newTestCronManager(t)

	cmdCtx := Ctx{
		SessionKey:  "test:session:1",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: cronMgr,
	}

	r := Handle(context.Background(), "/help", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /help to be handled")
	}
	if !strings.Contains(r.Text, "/help") {
		t.Errorf("help text should contain /help, got: %s", r.Text)
	}
	if !strings.Contains(r.Text, "/new") {
		t.Errorf("help text should contain /new, got: %s", r.Text)
	}
	if !strings.Contains(r.Text, "/cron") {
		t.Errorf("help text should contain /cron, got: %s", r.Text)
	}
}

func TestHandleNew(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	key := "test:session:new"

	// Seed history
	_ = sess.AppendMessages(key, []session.Message{
		{Role: "user", Content: "hello", TS: time.Now().UnixMilli()},
	})

	cmdCtx := Ctx{
		SessionKey: key,
		Agent:      ag,
		Sessions:   sess,
		Config:     cfg,
	}

	r := Handle(context.Background(), "/new", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /new to be handled")
	}
	if r.Text != "Session cleared." {
		t.Errorf("expected 'Session cleared.', got %q", r.Text)
	}

	// Verify session was actually cleared
	history, _ := sess.GetHistory(key)
	if len(history) != 0 {
		t.Errorf("expected empty history after /new, got %d messages", len(history))
	}
}

func TestHandleReset(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	key := "test:session:reset"

	_ = sess.AppendMessages(key, []session.Message{
		{Role: "user", Content: "hello", TS: time.Now().UnixMilli()},
	})

	cmdCtx := Ctx{
		SessionKey: key,
		Agent:      ag,
		Sessions:   sess,
		Config:     cfg,
	}

	r := Handle(context.Background(), "/reset", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /reset to be handled")
	}
	if r.Text != "Session cleared." {
		t.Errorf("expected 'Session cleared.', got %q", r.Text)
	}

	history, _ := sess.GetHistory(key)
	if len(history) != 0 {
		t.Errorf("expected empty history after /reset, got %d messages", len(history))
	}
}

func TestHandleCompact(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	key := "test:session:compact"

	cmdCtx := Ctx{
		SessionKey: key,
		Agent:      ag,
		Sessions:   sess,
		Config:     cfg,
	}

	// Compact on empty session should succeed
	r := Handle(context.Background(), "/compact", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /compact to be handled")
	}
	if r.Text != "Session compacted." {
		t.Errorf("expected 'Session compacted.', got %q", r.Text)
	}
}

func TestHandleCompactWithInstructions(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	key := "test:session:compact-instr"

	cmdCtx := Ctx{
		SessionKey: key,
		Agent:      ag,
		Sessions:   sess,
		Config:     cfg,
	}

	r := Handle(context.Background(), "/compact focus on code changes", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /compact to be handled")
	}
	if r.Text != "Session compacted." {
		t.Errorf("expected 'Session compacted.', got %q", r.Text)
	}
}

func TestHandleModelShow(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	key := "test:session:model"

	cmdCtx := Ctx{
		SessionKey: key,
		Agent:      ag,
		Sessions:   sess,
		Config:     cfg,
	}

	r := Handle(context.Background(), "/model", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /model to be handled")
	}
	if !strings.Contains(r.Text, "test/mock-model") {
		t.Errorf("expected model name in output, got %q", r.Text)
	}
	if !strings.Contains(r.Text, "Current model:") {
		t.Errorf("expected 'Current model:' prefix, got %q", r.Text)
	}
}

func TestHandleModelSet(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	key := "test:session:model-set"

	cmdCtx := Ctx{
		SessionKey: key,
		Agent:      ag,
		Sessions:   sess,
		Config:     cfg,
	}

	r := Handle(context.Background(), "/model gpt-4.1", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /model to be handled")
	}
	if !strings.Contains(r.Text, "gpt-4.1") {
		t.Errorf("expected model name in output, got %q", r.Text)
	}
	if !strings.Contains(r.Text, "Model set to:") {
		t.Errorf("expected 'Model set to:' prefix, got %q", r.Text)
	}

	// Verify the model was actually set by querying it
	r2 := Handle(context.Background(), "/model", cmdCtx)
	if !strings.Contains(r2.Text, "gpt-4.1") {
		t.Errorf("expected model to be gpt-4.1 after set, got %q", r2.Text)
	}
}

func TestHandleContext(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	key := "test:session:context"

	// Seed some messages
	_ = sess.AppendMessages(key, []session.Message{
		{Role: "user", Content: "hello world", TS: time.Now().UnixMilli()},
		{Role: "assistant", Content: "hi there!", TS: time.Now().UnixMilli()},
	})

	cmdCtx := Ctx{
		SessionKey: key,
		Agent:      ag,
		Sessions:   sess,
		Config:     cfg,
	}

	r := Handle(context.Background(), "/context", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /context to be handled")
	}
	if !strings.Contains(r.Text, "Messages: 2") {
		t.Errorf("expected 'Messages: 2', got %q", r.Text)
	}
	if !strings.Contains(r.Text, "Estimated tokens:") {
		t.Errorf("expected token estimate, got %q", r.Text)
	}
	if !strings.Contains(r.Text, key) {
		t.Errorf("expected session key in output, got %q", r.Text)
	}
}

func TestHandleContextEmpty(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	key := "test:session:context-empty"

	cmdCtx := Ctx{
		SessionKey: key,
		Agent:      ag,
		Sessions:   sess,
		Config:     cfg,
	}

	r := Handle(context.Background(), "/context", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /context to be handled")
	}
	if !strings.Contains(r.Text, "Messages: 0") {
		t.Errorf("expected 'Messages: 0', got %q", r.Text)
	}
}

func TestHandleExport(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	key := "test:session:export"

	_ = sess.AppendMessages(key, []session.Message{
		{Role: "user", Content: "hello", TS: time.Now().UnixMilli()},
		{Role: "assistant", Content: "world", TS: time.Now().UnixMilli()},
	})

	cmdCtx := Ctx{
		SessionKey: key,
		Agent:      ag,
		Sessions:   sess,
		Config:     cfg,
	}

	r := Handle(context.Background(), "/export", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /export to be handled")
	}
	if !strings.Contains(r.Text, "[user]: hello") {
		t.Errorf("expected user message in export, got %q", r.Text)
	}
	if !strings.Contains(r.Text, "[assistant]: world") {
		t.Errorf("expected assistant message in export, got %q", r.Text)
	}
}

func TestHandleExportEmpty(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	key := "test:session:export-empty"

	cmdCtx := Ctx{
		SessionKey: key,
		Agent:      ag,
		Sessions:   sess,
		Config:     cfg,
	}

	r := Handle(context.Background(), "/export", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /export to be handled")
	}
	if r.Text != "Session is empty." {
		t.Errorf("expected 'Session is empty.', got %q", r.Text)
	}
}

func TestHandleCronDisabled(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)

	cmdCtx := Ctx{
		SessionKey:  "test:session:cron",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: nil, // cron not enabled
	}

	r := Handle(context.Background(), "/cron list", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron to be handled")
	}
	if r.Text != "Cron is not enabled." {
		t.Errorf("expected 'Cron is not enabled.', got %q", r.Text)
	}
}

// --- Non-command and path tests ---

func TestHandleNonCommand(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)

	cmdCtx := Ctx{
		SessionKey: "test:session:1",
		Agent:      ag,
		Sessions:   sess,
		Config:     cfg,
	}

	tests := []struct {
		name  string
		input string
	}{
		{"plain text", "hello world"},
		{"question", "what is the weather?"},
		{"empty", ""},
		{"spaces only", "   "},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := Handle(context.Background(), tc.input, cmdCtx)
			if r.Handled {
				t.Errorf("expected non-command %q to not be handled", tc.input)
			}
			if r.Text != "" {
				t.Errorf("expected empty text for non-command, got %q", r.Text)
			}
		})
	}
}

func TestHandleFilePathNotCommand(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)

	cmdCtx := Ctx{
		SessionKey: "test:session:path",
		Agent:      ag,
		Sessions:   sess,
		Config:     cfg,
	}

	paths := []string{
		"/home/user/file.txt",
		"/usr/local/bin/go",
		"/etc/nginx/nginx.conf",
		"/var/log/syslog",
		"/tmp/test/output",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			r := Handle(context.Background(), path, cmdCtx)
			if r.Handled {
				t.Errorf("file path %q should not be treated as a command", path)
			}
		})
	}
}

func TestHandleUnknownCommand(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)

	cmdCtx := Ctx{
		SessionKey: "test:session:unknown",
		Agent:      ag,
		Sessions:   sess,
		Config:     cfg,
	}

	tests := []string{"/foo", "/bar", "/unknown", "/ping"}

	for _, cmd := range tests {
		t.Run(cmd, func(t *testing.T) {
			r := Handle(context.Background(), cmd, cmdCtx)
			if !r.Handled {
				t.Errorf("unknown command %q should still be handled", cmd)
			}
			if !strings.Contains(r.Text, "Unknown command:") {
				t.Errorf("expected 'Unknown command:' in response, got %q", r.Text)
			}
			if !strings.Contains(r.Text, "/help") {
				t.Errorf("expected /help hint in response, got %q", r.Text)
			}
		})
	}
}

// --- Cron subcommand tests ---

func TestHandleCronHelp(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	cronMgr := newTestCronManager(t)

	cmdCtx := Ctx{
		SessionKey:  "test:session:cron",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: cronMgr,
	}

	// /cron with no subcommand should show help
	r := Handle(context.Background(), "/cron", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron to be handled")
	}
	if !strings.Contains(r.Text, "Usage:") {
		t.Errorf("expected cron help text, got %q", r.Text)
	}
	if !strings.Contains(r.Text, "/cron list") {
		t.Errorf("expected '/cron list' in help, got %q", r.Text)
	}
	if !strings.Contains(r.Text, "/cron add") {
		t.Errorf("expected '/cron add' in help, got %q", r.Text)
	}
}

func TestHandleCronUnknownSubcommand(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	cronMgr := newTestCronManager(t)

	cmdCtx := Ctx{
		SessionKey:  "test:session:cron",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: cronMgr,
	}

	r := Handle(context.Background(), "/cron blah", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron blah to be handled")
	}
	if !strings.Contains(r.Text, "Usage:") {
		t.Errorf("expected cron help for unknown subcommand, got %q", r.Text)
	}
}

func TestHandleCronListEmpty(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	cronMgr := newTestCronManager(t)

	cmdCtx := Ctx{
		SessionKey:  "test:session:cron",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: cronMgr,
	}

	r := Handle(context.Background(), "/cron list", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron list to be handled")
	}
	if r.Text != "No cron jobs scheduled." {
		t.Errorf("expected 'No cron jobs scheduled.', got %q", r.Text)
	}
}

func TestHandleCronAddAndList(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	cronMgr := newTestCronManager(t)

	cmdCtx := Ctx{
		SessionKey:  "test:session:cron",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: cronMgr,
	}

	// Add a daily job
	r := Handle(context.Background(), "/cron add @daily Send daily summary", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron add to be handled")
	}
	if !strings.Contains(r.Text, "Cron job added:") {
		t.Errorf("expected 'Cron job added:', got %q", r.Text)
	}

	// List should show the job
	r = Handle(context.Background(), "/cron list", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron list to be handled")
	}
	if !strings.Contains(r.Text, "Cron jobs:") {
		t.Errorf("expected 'Cron jobs:' header, got %q", r.Text)
	}
	if !strings.Contains(r.Text, "@daily") {
		t.Errorf("expected '@daily' schedule in list, got %q", r.Text)
	}
	if !strings.Contains(r.Text, "enabled") {
		t.Errorf("expected 'enabled' status in list, got %q", r.Text)
	}
}

func TestHandleCronAddHourly(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	cronMgr := newTestCronManager(t)

	cmdCtx := Ctx{
		SessionKey:  "test:session:cron",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: cronMgr,
	}

	r := Handle(context.Background(), "/cron add @hourly Check for updates", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron add to be handled")
	}
	if !strings.Contains(r.Text, "Cron job added:") {
		t.Errorf("expected 'Cron job added:', got %q", r.Text)
	}
}

func TestHandleCronAddEvery(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	cronMgr := newTestCronManager(t)

	cmdCtx := Ctx{
		SessionKey:  "test:session:cron",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: cronMgr,
	}

	// @every is special — two tokens for the spec
	r := Handle(context.Background(), "/cron add @every 30m Run health check", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron add @every to be handled")
	}
	if !strings.Contains(r.Text, "Cron job added:") {
		t.Errorf("expected 'Cron job added:', got %q", r.Text)
	}

	// Verify the schedule
	r = Handle(context.Background(), "/cron list", cmdCtx)
	if !strings.Contains(r.Text, "@every 30m") {
		t.Errorf("expected '@every 30m' schedule in list, got %q", r.Text)
	}
}

func TestHandleCronAddTimeSpec(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	cronMgr := newTestCronManager(t)

	cmdCtx := Ctx{
		SessionKey:  "test:session:cron",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: cronMgr,
	}

	r := Handle(context.Background(), "/cron add 09:00 Morning standup reminder", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron add to be handled")
	}
	if !strings.Contains(r.Text, "Cron job added:") {
		t.Errorf("expected 'Cron job added:', got %q", r.Text)
	}
}

func TestHandleCronAddInvalidSpec(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	cronMgr := newTestCronManager(t)

	cmdCtx := Ctx{
		SessionKey:  "test:session:cron",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: cronMgr,
	}

	r := Handle(context.Background(), "/cron add badspec Do something", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron add to be handled")
	}
	if !strings.Contains(r.Text, "Error:") {
		t.Errorf("expected error for invalid spec, got %q", r.Text)
	}
}

func TestHandleCronAddMissingInstruction(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	cronMgr := newTestCronManager(t)

	cmdCtx := Ctx{
		SessionKey:  "test:session:cron",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: cronMgr,
	}

	r := Handle(context.Background(), "/cron add @daily", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron add to be handled")
	}
	if !strings.Contains(r.Text, "Usage:") {
		t.Errorf("expected usage hint for missing instruction, got %q", r.Text)
	}
}

func TestHandleCronAddMissingAll(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	cronMgr := newTestCronManager(t)

	cmdCtx := Ctx{
		SessionKey:  "test:session:cron",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: cronMgr,
	}

	r := Handle(context.Background(), "/cron add", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron add to be handled")
	}
	if !strings.Contains(r.Text, "Usage:") {
		t.Errorf("expected usage hint, got %q", r.Text)
	}
}

func TestHandleCronRemove(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	cronMgr := newTestCronManager(t)

	cmdCtx := Ctx{
		SessionKey:  "test:session:cron",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: cronMgr,
	}

	// First add a job
	r := Handle(context.Background(), "/cron add @daily Test job", cmdCtx)
	// Extract the job ID from "Cron job added: <id>"
	jobID := strings.TrimPrefix(r.Text, "Cron job added: ")

	// Remove it
	r = Handle(context.Background(), "/cron remove "+jobID, cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron remove to be handled")
	}
	if r.Text != "Cron job removed." {
		t.Errorf("expected 'Cron job removed.', got %q", r.Text)
	}

	// Verify it's gone
	r = Handle(context.Background(), "/cron list", cmdCtx)
	if r.Text != "No cron jobs scheduled." {
		t.Errorf("expected no jobs after remove, got %q", r.Text)
	}
}

func TestHandleCronRemoveRm(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	cronMgr := newTestCronManager(t)

	cmdCtx := Ctx{
		SessionKey:  "test:session:cron",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: cronMgr,
	}

	// Add then remove using "rm" alias
	r := Handle(context.Background(), "/cron add @hourly Some job", cmdCtx)
	jobID := strings.TrimPrefix(r.Text, "Cron job added: ")

	r = Handle(context.Background(), "/cron rm "+jobID, cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron rm to be handled")
	}
	if r.Text != "Cron job removed." {
		t.Errorf("expected 'Cron job removed.', got %q", r.Text)
	}
}

func TestHandleCronRemoveMissingID(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	cronMgr := newTestCronManager(t)

	cmdCtx := Ctx{
		SessionKey:  "test:session:cron",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: cronMgr,
	}

	r := Handle(context.Background(), "/cron remove", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron remove to be handled")
	}
	if !strings.Contains(r.Text, "Usage:") {
		t.Errorf("expected usage hint for missing ID, got %q", r.Text)
	}
}

func TestHandleCronRemoveNotFound(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	cronMgr := newTestCronManager(t)

	cmdCtx := Ctx{
		SessionKey:  "test:session:cron",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: cronMgr,
	}

	r := Handle(context.Background(), "/cron remove nonexistent", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron remove to be handled")
	}
	if !strings.Contains(r.Text, "Error:") {
		t.Errorf("expected error for not-found job, got %q", r.Text)
	}
}

func TestHandleCronEnable(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	cronMgr := newTestCronManager(t)

	cmdCtx := Ctx{
		SessionKey:  "test:session:cron",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: cronMgr,
	}

	// Add a job, disable it, then enable it
	r := Handle(context.Background(), "/cron add @daily Test enable", cmdCtx)
	jobID := strings.TrimPrefix(r.Text, "Cron job added: ")

	// Disable first
	r = Handle(context.Background(), "/cron disable "+jobID, cmdCtx)
	if r.Text != "Cron job disabled." {
		t.Errorf("expected 'Cron job disabled.', got %q", r.Text)
	}

	// List should show disabled
	r = Handle(context.Background(), "/cron list", cmdCtx)
	if !strings.Contains(r.Text, "disabled") {
		t.Errorf("expected 'disabled' in list, got %q", r.Text)
	}

	// Enable
	r = Handle(context.Background(), "/cron enable "+jobID, cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron enable to be handled")
	}
	if r.Text != "Cron job enabled." {
		t.Errorf("expected 'Cron job enabled.', got %q", r.Text)
	}

	// List should show enabled
	r = Handle(context.Background(), "/cron list", cmdCtx)
	if !strings.Contains(r.Text, "enabled") {
		t.Errorf("expected 'enabled' in list after enable, got %q", r.Text)
	}
}

func TestHandleCronEnableMissingID(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	cronMgr := newTestCronManager(t)

	cmdCtx := Ctx{
		SessionKey:  "test:session:cron",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: cronMgr,
	}

	r := Handle(context.Background(), "/cron enable", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron enable to be handled")
	}
	if !strings.Contains(r.Text, "Usage:") {
		t.Errorf("expected usage hint, got %q", r.Text)
	}
}

func TestHandleCronEnableNotFound(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	cronMgr := newTestCronManager(t)

	cmdCtx := Ctx{
		SessionKey:  "test:session:cron",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: cronMgr,
	}

	r := Handle(context.Background(), "/cron enable nonexistent", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron enable to be handled")
	}
	if !strings.Contains(r.Text, "Error:") {
		t.Errorf("expected error for not-found job, got %q", r.Text)
	}
}

func TestHandleCronDisable(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	cronMgr := newTestCronManager(t)

	cmdCtx := Ctx{
		SessionKey:  "test:session:cron",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: cronMgr,
	}

	r := Handle(context.Background(), "/cron add @daily Test disable", cmdCtx)
	jobID := strings.TrimPrefix(r.Text, "Cron job added: ")

	r = Handle(context.Background(), "/cron disable "+jobID, cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron disable to be handled")
	}
	if r.Text != "Cron job disabled." {
		t.Errorf("expected 'Cron job disabled.', got %q", r.Text)
	}
}

func TestHandleCronDisableMissingID(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	cronMgr := newTestCronManager(t)

	cmdCtx := Ctx{
		SessionKey:  "test:session:cron",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: cronMgr,
	}

	r := Handle(context.Background(), "/cron disable", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron disable to be handled")
	}
	if !strings.Contains(r.Text, "Usage:") {
		t.Errorf("expected usage hint, got %q", r.Text)
	}
}

func TestHandleCronDisableNotFound(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	cronMgr := newTestCronManager(t)

	cmdCtx := Ctx{
		SessionKey:  "test:session:cron",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: cronMgr,
	}

	r := Handle(context.Background(), "/cron disable nonexistent", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron disable to be handled")
	}
	if !strings.Contains(r.Text, "Error:") {
		t.Errorf("expected error for not-found job, got %q", r.Text)
	}
}

func TestHandleCronListWithState(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	cronMgr := newTestCronManager(t)

	cmdCtx := Ctx{
		SessionKey:  "test:session:cron",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: cronMgr,
	}

	// Add a job
	r := Handle(context.Background(), "/cron add @daily Test with state", cmdCtx)
	jobID := strings.TrimPrefix(r.Text, "Cron job added: ")

	// Manually set state on the job by using the list to get reference
	// Since we can't easily set state through the public API without running the job,
	// just verify the list output format with a job that has no state.
	r = Handle(context.Background(), "/cron list", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron list to be handled")
	}
	if !strings.Contains(r.Text, "Cron jobs:") {
		t.Errorf("expected 'Cron jobs:' header, got %q", r.Text)
	}
	_ = jobID // used above
}

func TestHandleCronAddEveryWithoutInstruction(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	cronMgr := newTestCronManager(t)

	cmdCtx := Ctx{
		SessionKey:  "test:session:cron",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: cronMgr,
	}

	// @every 1h but no instruction text (only two tokens after "add")
	r := Handle(context.Background(), "/cron add @every 1h", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron add to be handled")
	}
	if !strings.Contains(r.Text, "Instruction cannot be empty") {
		t.Errorf("expected empty instruction error, got %q", r.Text)
	}
}

// --- Edge cases ---

func TestHandleSlashOnly(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)

	cmdCtx := Ctx{
		SessionKey: "test:session:slash",
		Agent:      ag,
		Sessions:   sess,
		Config:     cfg,
	}

	// A single "/" should be treated as an unknown command
	r := Handle(context.Background(), "/", cmdCtx)
	// "/" is a single-slash command with no second slash, so it passes the path check
	// and falls through to the unknown command handler
	if !r.Handled {
		t.Fatal("expected '/' to be handled as unknown command")
	}
}

func TestHandleCommandWithExtraSpaces(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)

	cmdCtx := Ctx{
		SessionKey: "test:session:spaces",
		Agent:      ag,
		Sessions:   sess,
		Config:     cfg,
	}

	// /model with extra spaces before the model name
	r := Handle(context.Background(), "/model   gpt-4.1", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /model to be handled")
	}
	// The args parsing uses TrimSpace, so this should work
	if !strings.Contains(r.Text, "Model set to:") {
		t.Errorf("expected model set response, got %q", r.Text)
	}
}

func TestHandleCronAddWeekly(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	cronMgr := newTestCronManager(t)

	cmdCtx := Ctx{
		SessionKey:  "test:session:cron",
		Agent:       ag,
		Sessions:    sess,
		Config:      cfg,
		CronManager: cronMgr,
	}

	r := Handle(context.Background(), "/cron add @weekly Weekly report", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cron add @weekly to be handled")
	}
	if !strings.Contains(r.Text, "Cron job added:") {
		t.Errorf("expected 'Cron job added:', got %q", r.Text)
	}
}

// --- Task queue command tests ---

func testTaskManager(t *testing.T) *taskqueue.Manager {
	t.Helper()
	dir := t.TempDir()
	return taskqueue.New(testLogger(), filepath.Join(dir, "tasks.json"), taskqueue.Config{
		MaxConcurrent:   3,
		ResultRetention: time.Hour,
	})
}

func TestHandleTasksEmpty(t *testing.T) {
	mgr := testTaskManager(t)
	defer mgr.Shutdown()

	r := Handle(context.Background(), "/tasks", Ctx{
		SessionKey:  "sess-a",
		TaskManager: mgr,
	})
	if !r.Handled {
		t.Fatal("expected /tasks to be handled")
	}
	if r.Text != "No tasks." {
		t.Errorf("expected 'No tasks.', got %q", r.Text)
	}
}

func TestHandleTasksShowsRunning(t *testing.T) {
	mgr := testTaskManager(t)
	defer mgr.Shutdown()

	started := make(chan struct{})
	mgr.Submit("sess-a", "agent1", "do something", func(ctx context.Context) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}, taskqueue.SubmitOpts{})
	<-started

	r := Handle(context.Background(), "/tasks", Ctx{
		SessionKey:  "sess-a",
		TaskManager: mgr,
	})
	if !r.Handled {
		t.Fatal("expected /tasks to be handled")
	}
	if !strings.Contains(r.Text, "do something") {
		t.Errorf("expected task message in output, got %q", r.Text)
	}
	if !strings.Contains(r.Text, "running") {
		t.Errorf("expected 'running' status, got %q", r.Text)
	}
}

func TestHandleTasksFiltersBySession(t *testing.T) {
	mgr := testTaskManager(t)
	defer mgr.Shutdown()

	started := make(chan struct{}, 2)
	mgr.Submit("sess-a", "agent1", "task-a", func(ctx context.Context) (string, error) {
		started <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	}, taskqueue.SubmitOpts{})
	mgr.Submit("sess-b", "agent1", "task-b", func(ctx context.Context) (string, error) {
		started <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	}, taskqueue.SubmitOpts{})
	<-started
	<-started

	r := Handle(context.Background(), "/tasks", Ctx{
		SessionKey:  "sess-a",
		TaskManager: mgr,
	})
	if !strings.Contains(r.Text, "task-a") {
		t.Errorf("expected task-a, got %q", r.Text)
	}
	if strings.Contains(r.Text, "task-b") {
		t.Errorf("should not contain task-b, got %q", r.Text)
	}
}

func TestHandleTasksAll(t *testing.T) {
	mgr := testTaskManager(t)
	defer mgr.Shutdown()

	started := make(chan struct{}, 2)
	mgr.Submit("sess-a", "agent1", "task-a", func(ctx context.Context) (string, error) {
		started <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	}, taskqueue.SubmitOpts{})
	mgr.Submit("sess-b", "agent1", "task-b", func(ctx context.Context) (string, error) {
		started <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	}, taskqueue.SubmitOpts{})
	<-started
	<-started

	r := Handle(context.Background(), "/tasks all", Ctx{
		SessionKey:  "sess-a",
		TaskManager: mgr,
	})
	if !strings.Contains(r.Text, "task-a") || !strings.Contains(r.Text, "task-b") {
		t.Errorf("expected both tasks, got %q", r.Text)
	}
}

func TestHandleCancelAll(t *testing.T) {
	mgr := testTaskManager(t)
	defer mgr.Shutdown()

	started := make(chan struct{}, 2)
	mgr.Submit("sess-a", "agent1", "task1", func(ctx context.Context) (string, error) {
		started <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	}, taskqueue.SubmitOpts{})
	mgr.Submit("sess-a", "agent1", "task2", func(ctx context.Context) (string, error) {
		started <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	}, taskqueue.SubmitOpts{})
	<-started
	<-started

	r := Handle(context.Background(), "/cancel", Ctx{
		SessionKey:  "sess-a",
		TaskManager: mgr,
	})
	if !r.Handled {
		t.Fatal("expected /cancel to be handled")
	}
	if !strings.Contains(r.Text, "Cancelled 2 task(s).") {
		t.Errorf("expected 'Cancelled 2 task(s).', got %q", r.Text)
	}
}

func TestHandleCancelByID(t *testing.T) {
	mgr := testTaskManager(t)
	defer mgr.Shutdown()

	started := make(chan struct{})
	id := mgr.Submit("sess-a", "agent1", "cancel-me", func(ctx context.Context) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}, taskqueue.SubmitOpts{})
	<-started

	r := Handle(context.Background(), "/cancel "+id, Ctx{
		SessionKey:  "sess-a",
		TaskManager: mgr,
	})
	if !r.Handled {
		t.Fatal("expected /cancel <id> to be handled")
	}
	if !strings.Contains(r.Text, "cancelled") {
		t.Errorf("expected 'cancelled', got %q", r.Text)
	}
}

func TestHandleCancelNoTasks(t *testing.T) {
	mgr := testTaskManager(t)
	defer mgr.Shutdown()

	r := Handle(context.Background(), "/cancel", Ctx{
		SessionKey:  "sess-a",
		TaskManager: mgr,
	})
	if r.Text != "No running tasks to cancel." {
		t.Errorf("expected 'No running tasks to cancel.', got %q", r.Text)
	}
}

func TestHandleTasksNilManager(t *testing.T) {
	r := Handle(context.Background(), "/tasks", Ctx{SessionKey: "sess-a"})
	if !r.Handled {
		t.Fatal("expected /tasks to be handled")
	}
	if r.Text != "Task queue is not enabled." {
		t.Errorf("expected disabled message, got %q", r.Text)
	}
}

func TestHandleCancelNilManager(t *testing.T) {
	r := Handle(context.Background(), "/cancel", Ctx{SessionKey: "sess-a"})
	if !r.Handled {
		t.Fatal("expected /cancel to be handled")
	}
	if r.Text != "Task queue is not enabled." {
		t.Errorf("expected disabled message, got %q", r.Text)
	}
}

func TestHandle_Status_Basic(t *testing.T) {
	cfg := newTestConfig()
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := newTestAgent(t, cfg, sess)
	key := "test:session:status"

	startTime := time.Now().Add(-2 * time.Hour)

	cmdCtx := Ctx{
		SessionKey: key,
		Agent:      ag,
		Sessions:   sess,
		Config:     cfg,
		Version:    "1.2.3",
		StartTime:  startTime,
		SkillCount: 7,
	}

	r := Handle(context.Background(), "/status", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /status to be handled")
	}
	if !strings.Contains(r.Text, "Version: 1.2.3") {
		t.Errorf("expected version in output, got: %s", r.Text)
	}
	if !strings.Contains(r.Text, "Uptime: ") {
		t.Errorf("expected uptime in output, got: %s", r.Text)
	}
	// Uptime should contain "h" since we set StartTime to 2 hours ago.
	if !strings.Contains(r.Text, "h") {
		t.Errorf("expected uptime to contain 'h' for hours, got: %s", r.Text)
	}
	if !strings.Contains(r.Text, "Model: ") {
		t.Errorf("expected model in output, got: %s", r.Text)
	}
	// The model should be resolved from the agent (test/mock-model).
	if !strings.Contains(r.Text, "test/mock-model") {
		t.Errorf("expected model to be test/mock-model, got: %s", r.Text)
	}
	if !strings.Contains(r.Text, "Skills: 7") {
		t.Errorf("expected skill count of 7, got: %s", r.Text)
	}
	if !strings.Contains(r.Text, "Queue depth: 0") {
		t.Errorf("expected queue depth of 0 (no task manager), got: %s", r.Text)
	}
}

func TestHandle_Status_NilAgent(t *testing.T) {
	sess, _ := session.New(testLogger(), t.TempDir(), 0)

	cmdCtx := Ctx{
		SessionKey: "test:session:nil-agent",
		Agent:      nil,
		Sessions:   sess,
		Version:    "0.0.1",
		StartTime:  time.Now().Add(-5 * time.Minute),
		SkillCount: 3,
	}

	r := Handle(context.Background(), "/status", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /status to be handled")
	}
	if !strings.Contains(r.Text, "Model: unknown") {
		t.Errorf("expected 'Model: unknown' when Agent is nil, got: %s", r.Text)
	}
	// Other fields should still be present.
	if !strings.Contains(r.Text, "Version: 0.0.1") {
		t.Errorf("expected version, got: %s", r.Text)
	}
	if !strings.Contains(r.Text, "Skills: 3") {
		t.Errorf("expected skill count, got: %s", r.Text)
	}
}

func TestHandle_Status_NoStartTime(t *testing.T) {
	sess, _ := session.New(testLogger(), t.TempDir(), 0)

	cmdCtx := Ctx{
		SessionKey: "test:session:no-start",
		Agent:      nil,
		Sessions:   sess,
		Version:    "dev",
		// StartTime is zero value (not set).
		SkillCount: 0,
	}

	r := Handle(context.Background(), "/status", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /status to be handled")
	}
	if !strings.Contains(r.Text, "Uptime: unknown") {
		t.Errorf("expected 'Uptime: unknown' when StartTime is zero, got: %s", r.Text)
	}
	if !strings.Contains(r.Text, "Version: dev") {
		t.Errorf("expected version 'dev', got: %s", r.Text)
	}
	if !strings.Contains(r.Text, "Skills: 0") {
		t.Errorf("expected skill count 0, got: %s", r.Text)
	}
}

// ---------------------------------------------------------------------------
// /status with TaskManager (queue depth path)
// ---------------------------------------------------------------------------

func TestHandle_Status_WithTaskManager(t *testing.T) {
	sess, _ := session.New(testLogger(), t.TempDir(), 0)
	dir := t.TempDir()
	taskMgr := taskqueue.New(testLogger(), filepath.Join(dir, "tasks.json"), taskqueue.Config{})

	// Submit a task that stays running
	taskMgr.Submit("test:sess", "agent1", "running task", func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}, taskqueue.SubmitOpts{})
	time.Sleep(30 * time.Millisecond)

	cmdCtx := Ctx{
		SessionKey:  "test:sess",
		Agent:       nil,
		Sessions:    sess,
		TaskManager: taskMgr,
		Version:     "1.0.0",
		StartTime:   time.Now(),
		SkillCount:  2,
	}

	r := Handle(context.Background(), "/status", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /status to be handled")
	}
	if !strings.Contains(r.Text, "Queue depth: 1") {
		t.Errorf("expected queue depth of 1, got: %s", r.Text)
	}
}

// ---------------------------------------------------------------------------
// handleTasks — completed/failed/cancelled task status icons
// ---------------------------------------------------------------------------

func TestHandleTasksStatusIcons(t *testing.T) {
	dir := t.TempDir()
	taskMgr := taskqueue.New(testLogger(), filepath.Join(dir, "tasks.json"), taskqueue.Config{})

	// Submit and cancel a task to get the cancelled status
	taskID := taskMgr.Submit("sess1", "agent1", "cancel me", func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}, taskqueue.SubmitOpts{})
	time.Sleep(30 * time.Millisecond)
	_ = taskMgr.Cancel(taskID)
	time.Sleep(30 * time.Millisecond)

	cmdCtx := Ctx{
		SessionKey:  "sess1",
		Sessions:    nil,
		TaskManager: taskMgr,
	}

	r := Handle(context.Background(), "/tasks all", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /tasks to be handled")
	}
	// Should show the cancelled task
	if !strings.Contains(r.Text, "cancel me") {
		t.Errorf("expected task message in output, got: %s", r.Text)
	}
}

// ---------------------------------------------------------------------------
// handleTasks — completed task with duration
// ---------------------------------------------------------------------------

func TestHandleTasksWithDuration(t *testing.T) {
	dir := t.TempDir()
	taskMgr := taskqueue.New(testLogger(), filepath.Join(dir, "tasks.json"), taskqueue.Config{})

	// Submit a task that completes quickly (will have DurationMs > 0)
	taskMgr.Submit("sess1", "agent1", "quick task", func(ctx context.Context) (string, error) {
		time.Sleep(10 * time.Millisecond)
		return "done", nil
	}, taskqueue.SubmitOpts{})
	time.Sleep(200 * time.Millisecond) // wait for completion

	cmdCtx := Ctx{
		SessionKey:  "sess1",
		Sessions:    nil,
		TaskManager: taskMgr,
	}

	r := Handle(context.Background(), "/tasks all", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /tasks to be handled")
	}
	// Should contain duration in ms
	if !strings.Contains(r.Text, "ms") {
		t.Errorf("expected task duration in ms, got: %s", r.Text)
	}
	// Should show success icon
	if !strings.Contains(r.Text, "quick task") {
		t.Errorf("expected task message, got: %s", r.Text)
	}
}

// ---------------------------------------------------------------------------
// handleCancel — cancel all with some tasks
// ---------------------------------------------------------------------------

func TestHandleCancelAllWithTasks(t *testing.T) {
	dir := t.TempDir()
	taskMgr := taskqueue.New(testLogger(), filepath.Join(dir, "tasks.json"), taskqueue.Config{})

	// Submit tasks that stay running
	taskMgr.Submit("sess1", "agent1", "task A", func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}, taskqueue.SubmitOpts{})
	taskMgr.Submit("sess1", "agent1", "task B", func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}, taskqueue.SubmitOpts{})
	time.Sleep(50 * time.Millisecond)

	cmdCtx := Ctx{
		SessionKey:  "sess1",
		Sessions:    nil,
		TaskManager: taskMgr,
	}

	r := Handle(context.Background(), "/cancel", cmdCtx)
	if !r.Handled {
		t.Fatal("expected /cancel to be handled")
	}
	if !strings.Contains(r.Text, "2") || !strings.Contains(r.Text, "Cancelled") {
		t.Errorf("expected '2 Cancelled', got: %s", r.Text)
	}
}
