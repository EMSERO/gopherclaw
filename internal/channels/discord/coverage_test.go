package discord

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	openai "github.com/sashabaranov/go-openai"
	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/agent"
	"github.com/EMSERO/gopherclaw/internal/config"
	"github.com/EMSERO/gopherclaw/internal/models"
	"github.com/EMSERO/gopherclaw/internal/session"
)

// ---------------------------------------------------------------------------
// fakeProvider / fakeStream — lightweight mocks (distinct from stubProvider
// in bot_test.go so the two files don't conflict).
// ---------------------------------------------------------------------------

type fakeProviderCov struct {
	response openai.ChatCompletionResponse
	err      error
}

func (f *fakeProviderCov) Chat(_ context.Context, _ openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	if f.err != nil {
		return openai.ChatCompletionResponse{}, f.err
	}
	return f.response, nil
}

func (f *fakeProviderCov) ChatStream(_ context.Context, _ openai.ChatCompletionRequest) (models.Stream, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &fakeStreamCov{response: f.response}, nil
}

type fakeStreamCov struct {
	response openai.ChatCompletionResponse
	done     bool
}

func (s *fakeStreamCov) Recv() (openai.ChatCompletionStreamResponse, error) {
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

func (s *fakeStreamCov) Close() error { return nil }

func covTestConfig() *config.Root {
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

func covSimpleResponse(text string) openai.ChatCompletionResponse {
	return openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{{
			Message: openai.ChatCompletionMessage{
				Role:    "assistant",
				Content: text,
			},
			FinishReason: openai.FinishReasonStop,
		}},
		Usage: openai.Usage{PromptTokens: 100, CompletionTokens: 50},
	}
}

func covNewTestAgent(t *testing.T, resp openai.ChatCompletionResponse) (*agent.Agent, *session.Manager) {
	t.Helper()
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cfg := covTestConfig()
	provider := &fakeProviderCov{response: resp}
	router := models.NewRouter(zap.NewNop().Sugar(), map[string]models.Provider{"test": provider}, "test/model-1", nil)
	ag := agent.New(zap.NewNop().Sugar(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)
	return ag, sm
}

func covNewTestAgentError(t *testing.T, err error) (*agent.Agent, *session.Manager) {
	t.Helper()
	sm, errSm := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if errSm != nil {
		t.Fatal(errSm)
	}
	cfg := covTestConfig()
	provider := &fakeProviderCov{err: err}
	router := models.NewRouter(zap.NewNop().Sugar(), map[string]models.Provider{"test": provider}, "test/model-1", nil)
	ag := agent.New(zap.NewNop().Sugar(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)
	return ag, sm
}

// ---------------------------------------------------------------------------
// extractImageURLs — additional coverage
// ---------------------------------------------------------------------------

func TestExtractImageURLsNilSlice(t *testing.T) {
	urls := extractImageURLs(nil)
	if len(urls) != 0 {
		t.Errorf("expected empty slice for nil attachments, got %v", urls)
	}
}

func TestExtractImageURLsEmpty(t *testing.T) {
	urls := extractImageURLs([]*discordgo.MessageAttachment{})
	if len(urls) != 0 {
		t.Errorf("expected empty slice for empty attachments, got %v", urls)
	}
}

func TestExtractImageURLsNilAttachment(t *testing.T) {
	urls := extractImageURLs([]*discordgo.MessageAttachment{nil})
	if len(urls) != 0 {
		t.Errorf("expected empty slice for nil attachment entry, got %v", urls)
	}
}

func TestExtractImageURLsEmptyURL(t *testing.T) {
	urls := extractImageURLs([]*discordgo.MessageAttachment{
		{URL: "", ContentType: "image/png"},
	})
	if len(urls) != 0 {
		t.Errorf("expected empty slice for attachment with empty URL, got %v", urls)
	}
}

func TestExtractImageURLsNonImage(t *testing.T) {
	urls := extractImageURLs([]*discordgo.MessageAttachment{
		{URL: "https://cdn.discord.com/file.pdf", ContentType: "application/pdf"},
		{URL: "https://cdn.discord.com/file.txt", ContentType: "text/plain"},
		{URL: "https://cdn.discord.com/file.zip", ContentType: "application/zip"},
	})
	if len(urls) != 0 {
		t.Errorf("expected no image URLs from non-image attachments, got %v", urls)
	}
}

func TestExtractImageURLsImageTypes(t *testing.T) {
	urls := extractImageURLs([]*discordgo.MessageAttachment{
		{URL: "https://cdn.discord.com/img.png", ContentType: "image/png"},
		{URL: "https://cdn.discord.com/img.jpg", ContentType: "image/jpeg"},
		{URL: "https://cdn.discord.com/img.gif", ContentType: "image/gif"},
		{URL: "https://cdn.discord.com/img.webp", ContentType: "image/webp"},
	})
	if len(urls) != 4 {
		t.Errorf("expected 4 image URLs, got %d", len(urls))
	}
}

func TestExtractImageURLsCaseInsensitive(t *testing.T) {
	urls := extractImageURLs([]*discordgo.MessageAttachment{
		{URL: "https://cdn.discord.com/img.png", ContentType: "Image/PNG"},
		{URL: "https://cdn.discord.com/img.jpg", ContentType: "IMAGE/JPEG"},
	})
	if len(urls) != 2 {
		t.Errorf("expected 2 image URLs (case insensitive), got %d", len(urls))
	}
}

func TestExtractImageURLsMixed(t *testing.T) {
	urls := extractImageURLs([]*discordgo.MessageAttachment{
		{URL: "https://cdn.discord.com/img.png", ContentType: "image/png"},
		{URL: "https://cdn.discord.com/file.pdf", ContentType: "application/pdf"},
		nil,
		{URL: "", ContentType: "image/png"},
		{URL: "https://cdn.discord.com/img2.jpg", ContentType: "image/jpeg"},
	})
	if len(urls) != 2 {
		t.Errorf("expected 2 image URLs from mixed attachments, got %d", len(urls))
	}
	if urls[0] != "https://cdn.discord.com/img.png" {
		t.Errorf("unexpected first URL: %s", urls[0])
	}
	if urls[1] != "https://cdn.discord.com/img2.jpg" {
		t.Errorf("unexpected second URL: %s", urls[1])
	}
}

// ---------------------------------------------------------------------------
// processImageMessage — tests with real agents
// ---------------------------------------------------------------------------

func TestProcessImageMessageSuccess(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	ag, sm := covNewTestAgent(t, covSimpleResponse("I see a cat in the image"))

	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		ag:       ag,
		fullCfg:  covTestConfig(),
		cfg:      config.DiscordConfig{TimeoutSeconds: 5},
		msgCfg:   config.Messages{},
		sessions: sm,
		logger:   zap.NewNop().Sugar(),
	}

	m := newMsg("user1", "what is this?", "ch1", "", false)
	imageURLs := []string{"https://cdn.discord.com/img.png"}

	bot.processImageMessage(dg, m, "user1", "ch1", "what is this?", imageURLs, bot.cfg)
}

func TestProcessImageMessageEmptyText(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	ag, sm := covNewTestAgent(t, covSimpleResponse("I see a landscape"))

	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		ag:       ag,
		fullCfg:  covTestConfig(),
		cfg:      config.DiscordConfig{TimeoutSeconds: 5},
		msgCfg:   config.Messages{},
		sessions: sm,
		logger:   zap.NewNop().Sugar(),
	}

	m := newMsg("user1", "", "ch1", "", false)
	imageURLs := []string{"https://cdn.discord.com/img.png"}

	// Empty text should default to "What's in this image?"
	bot.processImageMessage(dg, m, "user1", "ch1", "", imageURLs, bot.cfg)
}

func TestProcessImageMessageError(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	ag, sm := covNewTestAgentError(t, fmt.Errorf("vision model unavailable"))

	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		ag:       ag,
		fullCfg:  covTestConfig(),
		cfg:      config.DiscordConfig{TimeoutSeconds: 5},
		msgCfg:   config.Messages{},
		sessions: sm,
		logger:   zap.NewNop().Sugar(),
	}

	m := newMsg("user1", "describe", "ch1", "", false)
	imageURLs := []string{"https://cdn.discord.com/img.png"}

	// Should send error message, not panic
	bot.processImageMessage(dg, m, "user1", "ch1", "describe", imageURLs, bot.cfg)
}

func TestProcessImageMessageSuppressible(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	ag, sm := covNewTestAgent(t, covSimpleResponse("NO_REPLY"))

	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		ag:       ag,
		fullCfg:  covTestConfig(),
		cfg:      config.DiscordConfig{TimeoutSeconds: 5},
		msgCfg:   config.Messages{},
		sessions: sm,
		logger:   zap.NewNop().Sugar(),
	}

	m := newMsg("user1", "check this", "ch1", "", false)
	imageURLs := []string{"https://cdn.discord.com/img.png"}

	// Suppressible response should be silently dropped
	bot.processImageMessage(dg, m, "user1", "ch1", "check this", imageURLs, bot.cfg)
}

func TestProcessImageMessageMultipleImages(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	ag, sm := covNewTestAgent(t, covSimpleResponse("Multiple images analyzed"))

	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		ag:       ag,
		fullCfg:  covTestConfig(),
		cfg:      config.DiscordConfig{TimeoutSeconds: 5},
		msgCfg:   config.Messages{},
		sessions: sm,
		logger:   zap.NewNop().Sugar(),
	}

	m := newMsg("user1", "compare these", "ch1", "", false)
	imageURLs := []string{
		"https://cdn.discord.com/img1.png",
		"https://cdn.discord.com/img2.jpg",
		"https://cdn.discord.com/img3.gif",
	}

	bot.processImageMessage(dg, m, "user1", "ch1", "compare these", imageURLs, bot.cfg)
}

func TestProcessImageMessageLongResponse(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	longText := strings.Repeat("This is a detailed analysis. ", 100)
	ag, sm := covNewTestAgent(t, covSimpleResponse(longText))

	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		ag:       ag,
		fullCfg:  covTestConfig(),
		cfg:      config.DiscordConfig{TimeoutSeconds: 5},
		msgCfg:   config.Messages{},
		sessions: sm,
		logger:   zap.NewNop().Sugar(),
	}

	m := newMsg("user1", "analyze", "ch1", "", false)
	imageURLs := []string{"https://cdn.discord.com/img.png"}

	// Long response should be split by sendLong
	bot.processImageMessage(dg, m, "user1", "ch1", "analyze", imageURLs, bot.cfg)
}

// ---------------------------------------------------------------------------
// respondFull — suppressible response path
// ---------------------------------------------------------------------------

func TestRespondFullSuppressible(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	ag, sm := covNewTestAgent(t, covSimpleResponse("NO_REPLY"))

	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		ag:       ag,
		fullCfg:  covTestConfig(),
		cfg:      config.DiscordConfig{TimeoutSeconds: 5},
		msgCfg:   config.Messages{},
		sessions: sm,
		logger:   zap.NewNop().Sugar(),
	}

	replyMsg := newMsg("user1", "hi", "ch1", "", false)
	// Should silently suppress the response
	bot.respondFull("test-suppress", "ch1", "hi", replyMsg, bot.cfg)
}

func TestRespondFullWithTokenUsage(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	ag, sm := covNewTestAgent(t, covSimpleResponse("Here's my answer"))

	cfg := covTestConfig()
	cfg.Messages = config.Messages{Usage: "tokens"}
	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		ag:       ag,
		fullCfg:  cfg,
		cfg:      config.DiscordConfig{TimeoutSeconds: 5},
		msgCfg:   config.Messages{},
		sessions: sm,
		logger:   zap.NewNop().Sugar(),
	}

	replyMsg := newMsg("user1", "hi", "ch1", "", false)
	bot.respondFull("test-usage", "ch1", "hi", replyMsg, bot.cfg)
}

// ---------------------------------------------------------------------------
// respondStreaming — suppressible response path
// ---------------------------------------------------------------------------

func TestRespondStreamingSuppressible(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	ag, sm := covNewTestAgent(t, covSimpleResponse("NO_REPLY"))

	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		ag:       ag,
		fullCfg:  covTestConfig(),
		cfg:      config.DiscordConfig{TimeoutSeconds: 5},
		msgCfg:   config.Messages{StreamEditMs: 10},
		sessions: sm,
		logger:   zap.NewNop().Sugar(),
	}

	replyMsg := newMsg("user1", "hi", "ch1", "", false)
	// Should suppress — delete the placeholder
	bot.respondStreaming("test-suppress", "ch1", "hi", replyMsg, bot.cfg, bot.msgCfg)
}

func TestRespondStreamingWithTokenUsage(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	ag, sm := covNewTestAgent(t, covSimpleResponse("Streamed answer"))

	cfg := covTestConfig()
	cfg.Messages = config.Messages{Usage: "tokens"}
	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		ag:       ag,
		fullCfg:  cfg,
		cfg:      config.DiscordConfig{TimeoutSeconds: 5},
		msgCfg:   config.Messages{StreamEditMs: 10},
		sessions: sm,
		logger:   zap.NewNop().Sugar(),
	}

	replyMsg := newMsg("user1", "hi", "ch1", "", false)
	bot.respondStreaming("test-stream-usage2", "ch1", "hi", replyMsg, bot.cfg, bot.msgCfg)
}

// ---------------------------------------------------------------------------
// processMessages with real agent — full & stream modes
// ---------------------------------------------------------------------------

func TestProcessMessagesFullWithAgent(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	ag, sm := covNewTestAgent(t, covSimpleResponse("Agent full reply"))

	cfg := covTestConfig()
	cfg.Session.ResetTriggers = []string{"WONT_MATCH"}

	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		ag:       ag,
		fullCfg:  cfg,
		cfg:      config.DiscordConfig{TimeoutSeconds: 5},
		msgCfg:   config.Messages{},
		sessions: sm,
		logger:   zap.NewNop().Sugar(),
	}

	m := newMsg("user1", "hello", "ch1", "", false)
	msgs := []queuedMessage{{text: "hello", m: m}}
	bot.processMessages("user1", "ch1", msgs, m)
}

func TestProcessMessagesStreamWithAgent(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	ag, sm := covNewTestAgent(t, covSimpleResponse("Agent stream reply"))

	cfg := covTestConfig()
	cfg.Session.ResetTriggers = []string{"WONT_MATCH"}

	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		ag:       ag,
		fullCfg:  cfg,
		cfg:      config.DiscordConfig{TimeoutSeconds: 5, StreamMode: "partial"},
		msgCfg:   config.Messages{StreamEditMs: 10},
		sessions: sm,
		logger:   zap.NewNop().Sugar(),
	}

	m := newMsg("user1", "hello", "ch1", "", false)
	msgs := []queuedMessage{{text: "hello", m: m}}
	bot.processMessages("user1", "ch1", msgs, m)
}

// ---------------------------------------------------------------------------
// handleMessage — image attachment path
// ---------------------------------------------------------------------------

func TestHandleMessageWithImageAttachment(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	ag, sm := covNewTestAgent(t, covSimpleResponse("I see an image"))

	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   map[string]bool{"user1": true},
		queues:   make(map[string]*messageQueue),
		ag:       ag,
		fullCfg:  covTestConfig(),
		cfg:      config.DiscordConfig{DMPolicy: "pairing", TimeoutSeconds: 5},
		msgCfg:   config.Messages{},
		sessions: sm,
		logger:   zap.NewNop().Sugar(),
	}

	m := &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        "msg_img1",
			Author:    &discordgo.User{ID: "user1", Username: "user1"},
			Content:   "check this image",
			ChannelID: "ch1",
			GuildID:   "",
			Attachments: []*discordgo.MessageAttachment{
				{URL: "https://cdn.discord.com/img.png", ContentType: "image/png"},
			},
		},
	}
	bot.handleMessage(dg, m)

	// Give async goroutine a moment
	time.Sleep(100 * time.Millisecond)
}

func TestHandleMessageWithImageAttachmentGuild(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	ag, sm := covNewTestAgent(t, covSimpleResponse("Guild image response"))

	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		ag:       ag,
		fullCfg:  covTestConfig(),
		cfg:      config.DiscordConfig{TimeoutSeconds: 5},
		msgCfg:   config.Messages{},
		sessions: sm,
		logger:   zap.NewNop().Sugar(),
	}

	m := &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        "msg_img2",
			Author:    &discordgo.User{ID: "user1", Username: "user1"},
			Content:   "<@bot1> check this",
			ChannelID: "ch1",
			GuildID:   "guild1",
			Attachments: []*discordgo.MessageAttachment{
				{URL: "https://cdn.discord.com/img.png", ContentType: "image/png"},
			},
		},
	}
	bot.handleMessage(dg, m)

	time.Sleep(100 * time.Millisecond)
}

// ---------------------------------------------------------------------------
// New — constructor tests
// ---------------------------------------------------------------------------

func TestNewValidToken(t *testing.T) {
	bot, err := New(
		zap.NewNop().Sugar(),
		config.DiscordConfig{BotToken: "test-token-123"},
		config.Messages{},
		&config.Root{},
		nil, // agent
		nil, // session manager
		nil, // cron manager
	)
	if err != nil {
		t.Fatalf("New() returned error: %v", err)
	}
	if bot == nil {
		t.Fatal("New() returned nil bot")
	}
	if bot.dg == nil {
		t.Error("expected dg session to be initialized")
	}
	if bot.pairCode == "" {
		t.Error("expected pair code to be generated")
	}
	if len(bot.pairCode) != 6 {
		t.Errorf("expected 6-digit pair code, got %q", bot.pairCode)
	}
	if bot.paired == nil {
		t.Error("expected paired map to be initialized")
	}
	if bot.queues == nil {
		t.Error("expected queues map to be initialized")
	}
	if bot.pendingConfirms == nil {
		t.Error("expected pendingConfirms map to be initialized")
	}
}

func TestNewEmptyToken(t *testing.T) {
	// discordgo.New("Bot ") should still succeed — it just won't connect
	bot, err := New(
		zap.NewNop().Sugar(),
		config.DiscordConfig{BotToken: ""},
		config.Messages{},
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("New() with empty token returned error: %v", err)
	}
	if bot == nil {
		t.Fatal("New() with empty token returned nil bot")
	}
}

// ---------------------------------------------------------------------------
// SetSkillManager / SetVersion / SetStartTime
// ---------------------------------------------------------------------------

func TestSetSkillManager(t *testing.T) {
	bot := &Bot{
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	// SetSkillManager is a no-op if the method doesn't exist on Bot;
	// but the channel interface may require it. Just verify no panic.
	// We check if the method exists via the interface.
	type skillSetter interface {
		SetSkillManager(interface{})
	}
	if _, ok := interface{}(bot).(skillSetter); ok {
		t.Log("Bot implements SetSkillManager")
	}
}

// ---------------------------------------------------------------------------
// dgRef concurrency
// ---------------------------------------------------------------------------

func TestDgRefReturnsSession(t *testing.T) {
	dg := newTestSession(t)
	bot := &Bot{
		dg:     dg,
		logger: zap.NewNop().Sugar(),
	}
	ref := bot.dgRef()
	if ref != dg {
		t.Error("dgRef did not return expected session")
	}
}
