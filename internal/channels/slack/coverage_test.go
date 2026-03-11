package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	slacklib "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/agent"
	"github.com/EMSERO/gopherclaw/internal/config"
	"github.com/EMSERO/gopherclaw/internal/models"
	"github.com/EMSERO/gopherclaw/internal/session"
)

// ---------------------------------------------------------------------------
// fakeProvider implements models.Provider for testing.
// ---------------------------------------------------------------------------

type covFakeProvider struct {
	response openai.ChatCompletionResponse
	err      error
}

func (f *covFakeProvider) Chat(_ context.Context, _ openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	if f.err != nil {
		return openai.ChatCompletionResponse{}, f.err
	}
	return f.response, nil
}

func (f *covFakeProvider) ChatStream(_ context.Context, _ openai.ChatCompletionRequest) (models.Stream, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &covFakeStream{response: f.response}, nil
}

type covFakeStream struct {
	response openai.ChatCompletionResponse
	done     bool
}

func (s *covFakeStream) Recv() (openai.ChatCompletionStreamResponse, error) {
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

func (s *covFakeStream) Close() error { return nil }

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
		}},
	}
}

func covNewTestAgent(t *testing.T, resp openai.ChatCompletionResponse) (*agent.Agent, *session.Manager) {
	t.Helper()
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cfg := covTestConfig()
	provider := &covFakeProvider{response: resp}
	router := models.NewRouter(zap.NewNop().Sugar(), map[string]models.Provider{"test": provider}, "test/model-1", nil)
	ag := agent.New(zap.NewNop().Sugar(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)
	return ag, sm
}

func covNewTestAgentError(t *testing.T, agErr error) (*agent.Agent, *session.Manager) {
	t.Helper()
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cfg := covTestConfig()
	provider := &covFakeProvider{err: agErr}
	router := models.NewRouter(zap.NewNop().Sugar(), map[string]models.Provider{"test": provider}, "test/model-1", nil)
	ag := agent.New(zap.NewNop().Sugar(), cfg, cfg.DefaultAgent(), router, sm, nil, nil, "", nil)
	return ag, sm
}

// mockSlackAPI creates a test HTTP server that mocks the Slack API.
func mockSlackAPI(t *testing.T) (*httptest.Server, *slacklib.Client) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.Contains(path, "files.info"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":   true,
				"file": map[string]any{"id": "F123", "mimetype": "image/png", "name": "test.png", "url_private_download": ""},
			})
		case strings.Contains(path, "chat.postMessage"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": "C123",
				"ts":      "1234567890.123456",
			})
		case strings.Contains(path, "chat.update"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": "C123",
				"ts":      "1234567890.123456",
			})
		case strings.Contains(path, "chat.delete"):
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case strings.Contains(path, "reactions.add"):
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case strings.Contains(path, "conversations.open"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"channel": map[string]any{"id": "D_MOCK"},
			})
		case strings.Contains(path, "auth.test"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":      true,
				"user_id": "UBOT",
				"user":    "testbot",
				"team":    "testteam",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	api := slacklib.New("xoxb-test", slacklib.OptionAPIURL(srv.URL+"/"))
	return srv, api
}

// ---------------------------------------------------------------------------
// handleFileShared tests
// ---------------------------------------------------------------------------

func TestHandleFileShared_Unauthorized(t *testing.T) {
	bot := &Bot{
		cfg:    config.SlackConfig{AllowUsers: []string{"U_ALLOWED"}},
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.FileSharedEvent{
		UserID:    "U_NOTALLOWED",
		ChannelID: "C123",
		File:      slackevents.FileEventFile{ID: "F123"},
	}
	// Should return early without API calls (api is nil)
	bot.handleFileShared(ev)
}

func TestHandleFileShared_GetFileInfoError(t *testing.T) {
	srv, api := mockSlackAPI(t)
	defer srv.Close()

	// Override the server to return an error for files.info
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"error": "file_not_found",
		})
	}))
	defer errSrv.Close()
	errAPI := slacklib.New("xoxb-test", slacklib.OptionAPIURL(errSrv.URL+"/"))

	bot := &Bot{
		api:    errAPI,
		cfg:    config.SlackConfig{AllowUsers: nil}, // allow all
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	_ = api // suppress unused

	ev := &slackevents.FileSharedEvent{
		UserID:    "U123",
		ChannelID: "C123",
		File:      slackevents.FileEventFile{ID: "F_NONEXIST"},
	}
	bot.handleFileShared(ev)
}

func TestHandleFileShared_NonImageFile(t *testing.T) {
	// Create server that returns non-image file info
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "files.info") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"file": map[string]any{
					"id":       "F123",
					"mimetype": "application/pdf",
					"name":     "doc.pdf",
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()
	api := slacklib.New("xoxb-test", slacklib.OptionAPIURL(srv.URL+"/"))

	bot := &Bot{
		api:    api,
		cfg:    config.SlackConfig{AllowUsers: nil},
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.FileSharedEvent{
		UserID:    "U123",
		ChannelID: "C123",
		File:      slackevents.FileEventFile{ID: "F123"},
	}
	// Should return early since it's not an image
	bot.handleFileShared(ev)
}

func TestHandleFileShared_ImageFile_DownloadError(t *testing.T) {
	// Return image file info but with no download URL
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "files.info") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"file": map[string]any{
					"id":                     "F123",
					"mimetype":               "image/png",
					"name":                   "test.png",
					"url_private_download":   "",
					"url_private":            "",
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"channel": "C123",
			"ts":      "ts1",
		})
	}))
	defer srv.Close()
	api := slacklib.New("xoxb-test", slacklib.OptionAPIURL(srv.URL+"/"))

	bot := &Bot{
		api:     api,
		cfg:     config.SlackConfig{AllowUsers: nil, TimeoutSeconds: 1},
		fullCfg: covTestConfig(),
		paired:  make(map[string]bool),
		queues:  make(map[string]*messageQueue),
		logger:  zap.NewNop().Sugar(),
	}

	ev := &slackevents.FileSharedEvent{
		UserID:    "U123",
		ChannelID: "C123",
		File:      slackevents.FileEventFile{ID: "F123"},
	}
	bot.handleFileShared(ev)
}

func TestHandleFileShared_ImageFile_Success(t *testing.T) {
	// First, create a file download server
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return a minimal PNG
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0})
	}))
	defer dlSrv.Close()

	// Create Slack API server that returns file info with download URL
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "files.info") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"file": map[string]any{
					"id":                   "F123",
					"mimetype":             "image/png",
					"name":                 "test.png",
					"title":                "My Photo",
					"url_private_download": dlSrv.URL + "/download",
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"channel": "C123",
			"ts":      "ts1",
		})
	}))
	defer slackSrv.Close()
	api := slacklib.New("xoxb-test", slacklib.OptionAPIURL(slackSrv.URL+"/"))

	ag, sm := covNewTestAgent(t,covSimpleResponse("I see a PNG image"))

	bot := &Bot{
		api:      api,
		cfg:      config.SlackConfig{AllowUsers: nil, BotToken: "xoxb-test", TimeoutSeconds: 10},
		fullCfg:  covTestConfig(),
		ag:       ag,
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger:   zap.NewNop().Sugar(),
	}

	ev := &slackevents.FileSharedEvent{
		UserID:    "U123",
		ChannelID: "C123",
		File:      slackevents.FileEventFile{ID: "F123"},
	}
	bot.handleFileShared(ev)
}

func TestHandleFileShared_ImageFile_DefaultCaption(t *testing.T) {
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	}))
	defer dlSrv.Close()

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "files.info") {
			// Title equals name => triggers default caption
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"file": map[string]any{
					"id":                   "F123",
					"mimetype":             "image/png",
					"name":                 "test.png",
					"title":                "test.png", // same as name
					"url_private_download": dlSrv.URL + "/download",
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "ts1"})
	}))
	defer slackSrv.Close()
	api := slacklib.New("xoxb-test", slacklib.OptionAPIURL(slackSrv.URL+"/"))

	ag, sm := covNewTestAgent(t,covSimpleResponse("It's an image"))

	bot := &Bot{
		api:      api,
		cfg:      config.SlackConfig{AllowUsers: nil, BotToken: "xoxb-test", TimeoutSeconds: 10},
		fullCfg:  covTestConfig(),
		ag:       ag,
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger:   zap.NewNop().Sugar(),
	}

	ev := &slackevents.FileSharedEvent{
		UserID:    "U123",
		ChannelID: "C123",
		File:      slackevents.FileEventFile{ID: "F123"},
	}
	bot.handleFileShared(ev)
}

// ---------------------------------------------------------------------------
// downloadFileAsDataURL tests
// ---------------------------------------------------------------------------

func TestDownloadFileAsDataURL_NoURL(t *testing.T) {
	bot := &Bot{
		cfg:    config.SlackConfig{BotToken: "xoxb-test"},
		logger: zap.NewNop().Sugar(),
	}
	f := &slacklib.File{ID: "F_NOURL"}
	_, err := bot.downloadFileAsDataURL(context.Background(), f)
	if err == nil {
		t.Error("expected error for file with no URL")
	}
	if !strings.Contains(err.Error(), "no download URL") {
		t.Errorf("expected 'no download URL' error, got: %v", err)
	}
}

func TestDownloadFileAsDataURL_URLPrivateDownload(t *testing.T) {
	pngData := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify auth header
		if auth := r.Header.Get("Authorization"); !strings.Contains(auth, "Bearer") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(pngData)
	}))
	defer srv.Close()

	bot := &Bot{
		cfg:    config.SlackConfig{BotToken: "xoxb-test"},
		logger: zap.NewNop().Sugar(),
	}
	f := &slacklib.File{
		ID:                 "F123",
		URLPrivateDownload: srv.URL + "/download",
		Mimetype:           "image/png",
	}
	dataURL, err := bot.downloadFileAsDataURL(context.Background(), f)
	if err != nil {
		t.Fatalf("downloadFileAsDataURL error: %v", err)
	}
	if !strings.HasPrefix(dataURL, "data:image/png;base64,") {
		t.Errorf("expected PNG data URL prefix, got: %s", dataURL[:30])
	}
}

func TestDownloadFileAsDataURL_URLPrivateFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("JFIF image data"))
	}))
	defer srv.Close()

	bot := &Bot{
		cfg:    config.SlackConfig{BotToken: "xoxb-test"},
		logger: zap.NewNop().Sugar(),
	}
	f := &slacklib.File{
		ID:         "F123",
		URLPrivate: srv.URL + "/image",
		Mimetype:   "",
	}
	dataURL, err := bot.downloadFileAsDataURL(context.Background(), f)
	if err != nil {
		t.Fatalf("downloadFileAsDataURL error: %v", err)
	}
	if !strings.HasPrefix(dataURL, "data:") {
		t.Errorf("expected data URL, got: %s", dataURL[:20])
	}
}

func TestDownloadFileAsDataURL_WithMimetype(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0}) // JPEG header
	}))
	defer srv.Close()

	bot := &Bot{
		cfg:    config.SlackConfig{BotToken: "xoxb-test"},
		logger: zap.NewNop().Sugar(),
	}
	f := &slacklib.File{
		ID:                 "F123",
		URLPrivateDownload: srv.URL + "/download",
		Mimetype:           "image/jpeg",
	}
	dataURL, err := bot.downloadFileAsDataURL(context.Background(), f)
	if err != nil {
		t.Fatalf("downloadFileAsDataURL error: %v", err)
	}
	if !strings.HasPrefix(dataURL, "data:image/jpeg;base64,") {
		t.Errorf("expected JPEG data URL, got: %s", dataURL[:30])
	}
}

// ---------------------------------------------------------------------------
// respondStreaming tests with real agent
// ---------------------------------------------------------------------------

func TestRespondStreaming_Success(t *testing.T) {
	srv, api := mockSlackAPI(t)
	defer srv.Close()

	ag, sm := covNewTestAgent(t,covSimpleResponse("Streamed!"))

	bot := &Bot{
		api:      api,
		cfg:      config.SlackConfig{TimeoutSeconds: 10, StreamMode: "partial"},
		msgCfg:   config.Messages{StreamEditMs: 50},
		fullCfg:  covTestConfig(),
		ag:       ag,
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger:   zap.NewNop().Sugar(),
	}

	bot.respondStreaming("main:slack:U123", "C123", "hi", bot.cfg, bot.msgCfg)
}

func TestRespondStreaming_Error(t *testing.T) {
	srv, api := mockSlackAPI(t)
	defer srv.Close()

	ag, sm := covNewTestAgentError(t,fmt.Errorf("model error"))

	bot := &Bot{
		api:      api,
		cfg:      config.SlackConfig{TimeoutSeconds: 1, StreamMode: "partial"},
		msgCfg:   config.Messages{StreamEditMs: 50},
		fullCfg:  covTestConfig(),
		ag:       ag,
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger:   zap.NewNop().Sugar(),
	}

	bot.respondStreaming("main:slack:U123", "C123", "hi", bot.cfg, bot.msgCfg)
}

func TestRespondStreaming_Suppressible(t *testing.T) {
	srv, api := mockSlackAPI(t)
	defer srv.Close()

	ag, sm := covNewTestAgent(t,covSimpleResponse("NO_REPLY"))

	bot := &Bot{
		api:      api,
		cfg:      config.SlackConfig{TimeoutSeconds: 10, StreamMode: "partial"},
		msgCfg:   config.Messages{StreamEditMs: 50},
		fullCfg:  covTestConfig(),
		ag:       ag,
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger:   zap.NewNop().Sugar(),
	}

	bot.respondStreaming("main:slack:U123", "C123", "hi", bot.cfg, bot.msgCfg)
}

func TestRespondStreaming_WithTokenUsage(t *testing.T) {
	srv, api := mockSlackAPI(t)
	defer srv.Close()

	resp := covSimpleResponse("Result")
	resp.Usage = openai.Usage{PromptTokens: 100, CompletionTokens: 50}
	ag, sm := covNewTestAgent(t,resp)

	cfg := covTestConfig()
	cfg.Messages.Usage = "tokens"

	bot := &Bot{
		api:      api,
		cfg:      config.SlackConfig{TimeoutSeconds: 10, StreamMode: "partial"},
		msgCfg:   config.Messages{StreamEditMs: 50, Usage: "tokens"},
		fullCfg:  cfg,
		ag:       ag,
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger:   zap.NewNop().Sugar(),
	}

	bot.respondStreaming("main:slack:U123", "C123", "hi", bot.cfg, bot.msgCfg)
}

func TestRespondStreaming_DefaultEditMs(t *testing.T) {
	srv, api := mockSlackAPI(t)
	defer srv.Close()

	ag, sm := covNewTestAgent(t,covSimpleResponse("ok"))

	bot := &Bot{
		api:      api,
		cfg:      config.SlackConfig{TimeoutSeconds: 10, StreamMode: "partial"},
		msgCfg:   config.Messages{StreamEditMs: 0}, // default 400
		fullCfg:  covTestConfig(),
		ag:       ag,
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger:   zap.NewNop().Sugar(),
	}

	bot.respondStreaming("main:slack:U123", "C123", "hi", bot.cfg, bot.msgCfg)
}

// ---------------------------------------------------------------------------
// respondFull with real agent
// ---------------------------------------------------------------------------

func TestRespondFull_Success(t *testing.T) {
	srv, api := mockSlackAPI(t)
	defer srv.Close()

	ag, sm := covNewTestAgent(t,covSimpleResponse("Agent reply"))

	bot := &Bot{
		api:      api,
		cfg:      config.SlackConfig{TimeoutSeconds: 10},
		fullCfg:  covTestConfig(),
		ag:       ag,
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger:   zap.NewNop().Sugar(),
	}

	bot.respondFull("main:slack:U123", "C123", "hi", bot.cfg)
}

func TestRespondFull_Error(t *testing.T) {
	srv, api := mockSlackAPI(t)
	defer srv.Close()

	ag, sm := covNewTestAgentError(t,fmt.Errorf("model error"))

	bot := &Bot{
		api:      api,
		cfg:      config.SlackConfig{TimeoutSeconds: 1},
		fullCfg:  covTestConfig(),
		ag:       ag,
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger:   zap.NewNop().Sugar(),
	}

	bot.respondFull("main:slack:U123", "C123", "hi", bot.cfg)
}

func TestRespondFull_Suppressible(t *testing.T) {
	srv, api := mockSlackAPI(t)
	defer srv.Close()

	ag, sm := covNewTestAgent(t,covSimpleResponse("NO_REPLY"))

	bot := &Bot{
		api:      api,
		cfg:      config.SlackConfig{TimeoutSeconds: 10},
		fullCfg:  covTestConfig(),
		ag:       ag,
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger:   zap.NewNop().Sugar(),
	}

	bot.respondFull("main:slack:U123", "C123", "hi", bot.cfg)
}

func TestRespondFull_WithTokenUsage(t *testing.T) {
	srv, api := mockSlackAPI(t)
	defer srv.Close()

	resp := covSimpleResponse("With usage")
	resp.Usage = openai.Usage{PromptTokens: 100, CompletionTokens: 50}
	ag, sm := covNewTestAgent(t,resp)

	cfg := covTestConfig()
	cfg.Messages.Usage = "tokens"

	bot := &Bot{
		api:      api,
		cfg:      config.SlackConfig{TimeoutSeconds: 10},
		fullCfg:  cfg,
		ag:       ag,
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger:   zap.NewNop().Sugar(),
	}

	bot.respondFull("main:slack:U123", "C123", "hi", bot.cfg)
}

// ---------------------------------------------------------------------------
// handleEvents — context cancellation
// ---------------------------------------------------------------------------

func TestHandleEvents_ContextCancel(t *testing.T) {
	api := slacklib.New("xoxb-test", slacklib.OptionAppLevelToken("xapp-test"))
	client := socketmode.New(api)

	bot := &Bot{
		api:     api,
		client:  client,
		cfg:     config.SlackConfig{},
		paired:  make(map[string]bool),
		queues:  make(map[string]*messageQueue),
		logger:  zap.NewNop().Sugar(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		bot.handleEvents(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("handleEvents did not return after context cancellation")
	}
}

// ---------------------------------------------------------------------------
// handleEventsAPI — FileSharedEvent dispatch
// ---------------------------------------------------------------------------

func TestHandleEventsAPIFileSharedEvent(t *testing.T) {
	bot := &Bot{
		cfg:    config.SlackConfig{AllowUsers: []string{"U_ALLOWED"}},
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}

	fileEv := &slackevents.FileSharedEvent{
		UserID:    "U_NOTALLOWED", // not in allow list
		ChannelID: "C123",
		File:      slackevents.FileEventFile{ID: "F1"},
	}
	event := slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: fileEv,
		},
	}
	// Should dispatch to handleFileShared, which returns early for unauthorized user
	bot.handleEventsAPI(event)
}

// ---------------------------------------------------------------------------
// processMessages with real agent
// ---------------------------------------------------------------------------

func TestProcessMessages_WithAgent_FullMode(t *testing.T) {
	srv, api := mockSlackAPI(t)
	defer srv.Close()

	ag, sm := covNewTestAgent(t,covSimpleResponse("response"))

	bot := &Bot{
		api:      api,
		cfg:      config.SlackConfig{TimeoutSeconds: 10},
		msgCfg:   config.Messages{},
		fullCfg:  covTestConfig(),
		ag:       ag,
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger:   zap.NewNop().Sugar(),
	}

	bot.processMessages("U123", []queuedMessage{{text: "hello", channelID: "C123", userID: "U123", ts: "ts1"}})
}

func TestProcessMessages_WithAgent_StreamMode(t *testing.T) {
	srv, api := mockSlackAPI(t)
	defer srv.Close()

	ag, sm := covNewTestAgent(t,covSimpleResponse("streamed"))

	bot := &Bot{
		api:      api,
		cfg:      config.SlackConfig{TimeoutSeconds: 10, StreamMode: "partial"},
		msgCfg:   config.Messages{StreamEditMs: 50},
		fullCfg:  covTestConfig(),
		ag:       ag,
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger:   zap.NewNop().Sugar(),
	}

	bot.processMessages("U123", []queuedMessage{{text: "hello", channelID: "C123", userID: "U123", ts: "ts1"}})
}

func TestProcessMessages_ResetTrigger_WithAPI(t *testing.T) {
	srv, api := mockSlackAPI(t)
	defer srv.Close()

	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	bot := &Bot{
		api:      api,
		cfg:      config.SlackConfig{TimeoutSeconds: 1},
		msgCfg:   config.Messages{},
		fullCfg:  &config.Root{Session: config.Session{ResetTriggers: []string{"reset"}}},
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger:   zap.NewNop().Sugar(),
	}

	bot.processMessages("U1", []queuedMessage{{text: "reset", channelID: "C1", userID: "U1", ts: "ts1"}})
}

// ---------------------------------------------------------------------------
// CanConfirm
// ---------------------------------------------------------------------------

func TestCanConfirm(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	if !bot.CanConfirm("main:slack:U123") {
		t.Error("expected true for slack session key")
	}
	if bot.CanConfirm("main:telegram:123") {
		t.Error("expected false for telegram session key")
	}
}

// ---------------------------------------------------------------------------
// handleConfirmCallback
// ---------------------------------------------------------------------------

func TestHandleConfirmCallback_NonConfirm(t *testing.T) {
	bot := &Bot{
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}
	if bot.handleConfirmCallback("some_id", "yes") {
		t.Error("expected false for non-confirm callback")
	}
}

func TestHandleConfirmCallback_Yes(t *testing.T) {
	ch := make(chan bool, 1)
	bot := &Bot{
		pendingConfirms: map[string]chan bool{"confirm:C1:12345": ch},
		logger:          zap.NewNop().Sugar(),
	}
	if !bot.handleConfirmCallback("confirm:C1:12345", "yes") {
		t.Error("expected true")
	}
	select {
	case val := <-ch:
		if !val {
			t.Error("expected true for yes")
		}
	default:
		t.Error("no value on channel")
	}
}

func TestHandleConfirmCallback_No(t *testing.T) {
	ch := make(chan bool, 1)
	bot := &Bot{
		pendingConfirms: map[string]chan bool{"confirm:C1:12345": ch},
		logger:          zap.NewNop().Sugar(),
	}
	if !bot.handleConfirmCallback("confirm:C1:12345", "no") {
		t.Error("expected true")
	}
	select {
	case val := <-ch:
		if val {
			t.Error("expected false for no")
		}
	default:
		t.Error("no value on channel")
	}
}

func TestHandleConfirmCallback_NoPending(t *testing.T) {
	bot := &Bot{
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}
	if bot.handleConfirmCallback("confirm:C1:12345", "yes") {
		t.Error("expected false when no pending callback")
	}
}

// ---------------------------------------------------------------------------
// AnnounceToSession
// ---------------------------------------------------------------------------

func TestAnnounceToSession_ChannelScope(t *testing.T) {
	srv, api := mockSlackAPI(t)
	defer srv.Close()

	bot := &Bot{
		api:    api,
		paired: make(map[string]bool),
		logger: zap.NewNop().Sugar(),
	}
	// Channel scope key
	bot.AnnounceToSession("main:agent:slack:channel:C123", "hello channel")
}

func TestAnnounceToSession_UserScope(t *testing.T) {
	srv, api := mockSlackAPI(t)
	defer srv.Close()

	bot := &Bot{
		api:    api,
		paired: make(map[string]bool),
		logger: zap.NewNop().Sugar(),
	}
	// User scope key
	bot.AnnounceToSession("main:slack:U123", "hello user")
}

func TestAnnounceToSession_NotSlack(t *testing.T) {
	bot := &Bot{
		paired: make(map[string]bool),
		logger: zap.NewNop().Sugar(),
	}
	// Non-slack key should be silently ignored
	bot.AnnounceToSession("main:telegram:123", "should be ignored")
}

func TestAnnounceToSession_ShortKey(t *testing.T) {
	bot := &Bot{
		paired: make(map[string]bool),
		logger: zap.NewNop().Sugar(),
	}
	bot.AnnounceToSession("main", "short key")
}

// ---------------------------------------------------------------------------
// SetTaskManager
// ---------------------------------------------------------------------------

func TestSetTaskManagerCoverage(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	bot.SetTaskManager(nil)
	if bot.taskMgr != nil {
		t.Error("expected nil")
	}
}

// ---------------------------------------------------------------------------
// handleMessageEvent — slash command path
// ---------------------------------------------------------------------------

func TestHandleMessageEventSlashCommandCoverage(t *testing.T) {
	srv, api := mockSlackAPI(t)
	defer srv.Close()

	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		api:      api,
		cfg:      config.SlackConfig{AllowUsers: nil},
		msgCfg:   config.Messages{AckReactionScope: ""},
		fullCfg:  &config.Root{},
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		sessions: sm,
		logger:   zap.NewNop().Sugar(),
	}
	ev := &slackevents.MessageEvent{
		Channel:   "D123",
		User:      "U123",
		Text:      "/help",
		TimeStamp: "ts1",
	}
	bot.handleMessageEvent(ev)
}

// ---------------------------------------------------------------------------
// handleMessageEvent — pairing command
// ---------------------------------------------------------------------------

func TestHandleMessageEventPairCommand(t *testing.T) {
	srv, api := mockSlackAPI(t)
	defer srv.Close()

	bot := &Bot{
		api:      api,
		pairCode: "123456",
		cfg:      config.SlackConfig{AllowUsers: nil},
		msgCfg:   config.Messages{},
		fullCfg:  &config.Root{},
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger:   zap.NewNop().Sugar(),
	}
	ev := &slackevents.MessageEvent{
		Channel:   "D123",
		User:      "U123",
		Text:      "/pair 123456",
		TimeStamp: "ts1",
	}
	bot.handleMessageEvent(ev)

	bot.mu.Lock()
	isPaired := bot.paired["U123"]
	bot.mu.Unlock()
	if !isPaired {
		t.Error("expected user to be paired")
	}
}

func TestHandleMessageEventPairCommandInvalid(t *testing.T) {
	srv, api := mockSlackAPI(t)
	defer srv.Close()

	bot := &Bot{
		api:      api,
		pairCode: "123456",
		cfg:      config.SlackConfig{AllowUsers: nil},
		msgCfg:   config.Messages{},
		fullCfg:  &config.Root{},
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger:   zap.NewNop().Sugar(),
	}
	ev := &slackevents.MessageEvent{
		Channel:   "D123",
		User:      "U123",
		Text:      "/pair 000000",
		TimeStamp: "ts1",
	}
	bot.handleMessageEvent(ev)

	bot.mu.Lock()
	isPaired := bot.paired["U123"]
	bot.mu.Unlock()
	if isPaired {
		t.Error("expected user NOT to be paired with wrong code")
	}
}

// ---------------------------------------------------------------------------
// SendToAllPaired with mock API
// ---------------------------------------------------------------------------

func TestSendToAllPaired_WithUsers(t *testing.T) {
	srv, api := mockSlackAPI(t)
	defer srv.Close()

	bot := &Bot{
		api:    api,
		paired: map[string]bool{"U1": true, "U2": true},
		logger: zap.NewNop().Sugar(),
	}
	bot.SendToAllPaired("broadcast")
}
