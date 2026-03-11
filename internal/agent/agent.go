package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/agentapi"
	"github.com/EMSERO/gopherclaw/internal/config"
	"github.com/EMSERO/gopherclaw/internal/eidetic"
	"github.com/EMSERO/gopherclaw/internal/embeddings"
	"github.com/EMSERO/gopherclaw/internal/hooks"
	"github.com/EMSERO/gopherclaw/internal/models"
	"github.com/EMSERO/gopherclaw/internal/session"
	"github.com/EMSERO/gopherclaw/internal/skills"
	"github.com/EMSERO/gopherclaw/internal/tools"
)

// Tool is an alias for agentapi.Tool.
type Tool = agentapi.Tool

// Agent handles the conversation loop for one agent definition.
type Agent struct {
	cfgMu     sync.RWMutex // protects cfg
	cfg       *config.Root
	def       *config.AgentDef
	router    *models.Router
	sessions  *session.Manager
	skills    []skills.Skill
	wsMDs     map[string]string
	toolMap   map[string]Tool
	toolDefs  []openai.Tool
	workspace string // filesystem path for memory, skills, etc.
	logger    *zap.SugaredLogger

	// Per-session model overrides (sessionKey → model string)
	sessionModels sync.Map

	// Concurrency limiter (nil if maxConcurrent <= 0)
	sem chan struct{}

	// System prompt caching
	sysPromptStatic string     // identity + skills + workspace + subagents (built once)
	memoryMu        sync.Mutex // protects memoryCache and memoryMtime
	memoryCache     string     // cached MEMORY.md content
	memoryMtime     time.Time  // last mtime of MEMORY.md file

	// Eidetic memory integration (nil = disabled)
	eideticMu sync.RWMutex
	eideticC  eidetic.Client

	// Embeddings client for hybrid search (nil = keyword-only)
	embedMu sync.RWMutex
	embedC  *embeddings.Client

	// Bounded goroutine pool for background eidetic writes
	eideticSem chan struct{}

	// Lifecycle hook bus (nil = no-op)
	Hooks *hooks.Bus

	// Usage tracks normalized token usage per session (REQ-420)
	Usage *UsageTracker
}

// New creates an Agent.
func New(
	logger *zap.SugaredLogger,
	cfg *config.Root,
	def *config.AgentDef,
	router *models.Router,
	sessions *session.Manager,
	skillList []skills.Skill,
	wsMDs map[string]string,
	workspace string,
	toolList []Tool,
) *Agent {
	a := &Agent{
		cfg:        cfg,
		def:        def,
		router:     router,
		sessions:   sessions,
		skills:     skillList,
		wsMDs:      wsMDs,
		workspace:  workspace,
		logger:     logger,
		toolMap:    make(map[string]Tool),
		Usage:      NewUsageTracker(),
		eideticSem: make(chan struct{}, 8), // limit concurrent background eidetic writes
	}
	for _, t := range toolList {
		a.toolMap[t.Name()] = t
		fd := &openai.FunctionDefinition{
			Name:       t.Name(),
			Parameters: t.Schema(),
		}
		if d, ok := t.(agentapi.Describer); ok {
			fd.Description = d.Description()
		}
		a.toolDefs = append(a.toolDefs, openai.Tool{
			Type:     openai.ToolTypeFunction,
			Function: fd,
		})
	}

	// Set up concurrency limiter (prefer session-level, fall back to agent defaults)
	maxC := cfg.Session.MaxConcurrent
	if maxC <= 0 {
		maxC = cfg.Agents.Defaults.MaxConcurrent
	}
	if maxC > 0 {
		a.sem = make(chan struct{}, maxC)
	}

	a.initStaticPrompt()
	return a
}

// emitHook fires a lifecycle event if the hook bus is set.
func (a *Agent) emitHook(ctx context.Context, e hooks.Event) {
	if a.Hooks != nil {
		if e.AgentID == "" {
			e.AgentID = a.def.ID
		}
		a.Hooks.Emit(ctx, e)
	}
}

// Response is an alias for agentapi.Response.
type Response = agentapi.Response

// ResponseUsage is an alias for agentapi.ResponseUsage.
type ResponseUsage = agentapi.ResponseUsage

// ModelClient is the interface for making model API calls (used for soft trim summarization).
type ModelClient interface {
	Chat(ctx context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error)
}

// loopDetector tracks consecutive identical tool calls.
type loopDetector struct {
	lastKey string
	count   int
	limit   int
}

func newLoopDetector(limit int) *loopDetector {
	if limit <= 0 {
		limit = 3
	}
	return &loopDetector{limit: limit}
}

// check returns true if a loop is detected (N consecutive identical tool calls).
func (d *loopDetector) check(calls []openai.ToolCall) bool {
	key := toolCallsFingerprint(calls)
	if key == d.lastKey {
		d.count++
	} else {
		d.lastKey = key
		d.count = 1
	}
	return d.count >= d.limit
}

// toolCallsFingerprint produces a string key for a set of tool calls (name+args).
func toolCallsFingerprint(calls []openai.ToolCall) string {
	var sb strings.Builder
	for _, tc := range calls {
		sb.WriteString(tc.Function.Name)
		sb.WriteByte(':')
		sb.WriteString(tc.Function.Arguments)
		sb.WriteByte(';')
	}
	return sb.String()
}

// isContextOverflow returns true if an error indicates the model's context
// window was exceeded. Detects patterns from Anthropic, OpenAI, and others.
func isContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "context_length_exceeded") ||
		strings.Contains(msg, "maximum context length") ||
		strings.Contains(msg, "token limit") ||
		strings.Contains(msg, "too many tokens") ||
		strings.Contains(msg, "context window") ||
		strings.Contains(msg, "prompt is too long") ||
		strings.Contains(msg, "request too large")
}

// randomToolCallID generates a short random ID for a tool call (fallback when the model omits one).
func randomToolCallID() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return "call_" + hex.EncodeToString(buf)
}

// acquireSem blocks until a concurrency slot is available, or returns immediately if no limit.
func (a *Agent) acquireSem(ctx context.Context) error {
	if a.sem == nil {
		return nil
	}
	select {
	case a.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Agent) releaseSem() {
	if a.sem == nil {
		return
	}
	<-a.sem
}

const defaultMaxIterations = 50
const maxOverflowCompactions = 3 // auto-compact attempts before giving up

// getCfg returns the current config under the read lock.
func (a *Agent) getCfg() *config.Root {
	a.cfgMu.RLock()
	cfg := a.cfg
	a.cfgMu.RUnlock()
	return cfg
}

// GetConfig returns the current config. Exported for use by heartbeat runner etc.
func (a *Agent) GetConfig() *config.Root { return a.getCfg() }

// maxIter returns the configured max iterations, defaulting to 200 if unset.
func (a *Agent) maxIter() int {
	if n := a.getCfg().Agents.Defaults.MaxIterations; n > 0 {
		return n
	}
	return defaultMaxIterations
}

// SetSessionModel sets a per-session model override.
func (a *Agent) SetSessionModel(key, model string) { a.sessionModels.Store(key, model) }

// ClearSessionModel removes a per-session model override.
func (a *Agent) ClearSessionModel(key string) { a.sessionModels.Delete(key) }

// ResolveModel returns the model for a session (override if set, else config default).
func (a *Agent) ResolveModel(key string) string {
	if v, ok := a.sessionModels.Load(key); ok {
		return v.(string)
	}
	a.cfgMu.RLock()
	primary := a.cfg.Agents.Defaults.Model.Primary
	a.cfgMu.RUnlock()
	return primary
}

// ModelHealth returns the health status of configured models (registration + cooldown).
func (a *Agent) ModelHealth() []models.ModelHealthStatus {
	return a.router.ModelHealth()
}

// UpdateConfig swaps the agent's config and updates the router's model list.
// Called by the hot-reload callback when the config file changes.
func (a *Agent) UpdateConfig(newCfg *config.Root) {
	a.cfgMu.Lock()
	a.cfg = newCfg
	a.cfgMu.Unlock()
	a.router.SetModels(newCfg.Agents.Defaults.Model.Primary, newCfg.Agents.Defaults.Model.Fallbacks)
}

// SetEidetic wires an Eidetic client into the agent.  Pass nil to disable.
// Safe to call after New() and concurrently with requests.
func (a *Agent) SetEidetic(client eidetic.Client) {
	a.eideticMu.Lock()
	a.eideticC = client
	a.eideticMu.Unlock()
}

// getEidetic returns the current Eidetic client (nil if disabled).
func (a *Agent) getEidetic() eidetic.Client {
	a.eideticMu.RLock()
	c := a.eideticC
	a.eideticMu.RUnlock()
	return c
}

// SetEmbeddings wires an embeddings client into the agent. Pass nil to disable.
// Safe to call after New() and concurrently with requests.
func (a *Agent) SetEmbeddings(client *embeddings.Client) {
	a.embedMu.Lock()
	a.embedC = client
	a.embedMu.Unlock()
}

// getEmbeddings returns the current embeddings client (nil if disabled).
func (a *Agent) getEmbeddings() *embeddings.Client {
	a.embedMu.RLock()
	c := a.embedC
	a.embedMu.RUnlock()
	return c
}

// embed generates an embedding vector for text, returning nil on error or
// when no embeddings client is configured.
func (a *Agent) embed(ctx context.Context, text string) []float32 {
	ec := a.getEmbeddings()
	if ec == nil {
		return nil
	}
	vec, err := ec.Embed(ctx, text)
	if err != nil {
		a.logger.Debugf("embeddings: generation failed (non-fatal): %v", err)
		return nil
	}
	return vec
}

// eideticAgentID returns the agent_id to use when writing to Eidetic.
// Uses the configured override if set, otherwise falls back to the agent def ID.
func (a *Agent) eideticAgentID() string {
	if id := a.getCfg().Eidetic.AgentID; id != "" {
		return id
	}
	return a.def.ID
}

// appendToEidetic fires a background goroutine that writes a conversation turn
// to the Eidetic memory store.  When an embeddings client is configured, the
// vector is generated inline (in the goroutine) so it doesn't block the caller.
// Failures are logged at debug level and never surfaced to the caller.
func (a *Agent) appendToEidetic(sessionKey, userText, assistantText string) {
	c := a.getEidetic()
	if c == nil {
		return
	}
	content := fmt.Sprintf("[User]: %s\n[Assistant]: %s", userText, assistantText)
	agentID := a.eideticAgentID()
	tags := []string{
		"session:" + sessionKey,
		"agent:" + agentID,
	}
	// Use semaphore to bound concurrent background writes.
	select {
	case a.eideticSem <- struct{}{}:
	default:
		a.logger.Debugf("eidetic: append_memory skipped (semaphore full)")
		return
	}
	go func() {
		defer func() { <-a.eideticSem }()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Generate embedding vector if configured (non-blocking to caller).
		vec := a.embed(ctx, content)

		if err := c.AppendMemory(ctx, eidetic.AppendRequest{
			Content: content,
			AgentID: agentID,
			Tags:    tags,
			Vector:  vec,
		}); err != nil {
			a.logger.Warnf("eidetic: append_memory failed (non-fatal): %v", err)
		}
	}()
}

// Compact forces a soft trim of the session and persists it to disk.
func (a *Agent) Compact(ctx context.Context, sessionKey, instructions string) error {
	history, err := a.sessions.GetHistory(sessionKey)
	if err != nil {
		return err
	}
	if len(history) == 0 {
		return nil
	}
	result := a.forceSoftTrim(ctx, sessionKey, history, instructions)
	return a.sessions.ReplaceHistory(sessionKey, result)
}

// forceSoftTrim always summarizes the older portion of history, regardless of token count.
func (a *Agent) forceSoftTrim(ctx context.Context, sessionKey string, history []session.Message, instructions string) []session.Message {
	keepN := a.getCfg().Agents.Defaults.ContextPruning.KeepLastAssistants
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

	// Don't split in the middle of a tool_calls→tool sequence.
	for splitAt > 0 && history[splitAt-1].Role == "tool" {
		splitAt--
	}
	if splitAt > 0 && history[splitAt-1].Role == "assistant" && len(history[splitAt-1].ToolCalls) > 0 {
		splitAt--
	}

	oldMessages := history[:splitAt]
	recentMessages := history[splitAt:]

	var sb strings.Builder
	for _, m := range oldMessages {
		fmt.Fprintf(&sb, "[%s]: %s\n", m.Role, m.Content)
	}

	systemPrompt := "Summarize the following conversation concisely, preserving key facts, decisions, and context needed to continue the conversation. Be brief."
	if instructions != "" {
		systemPrompt += "\n\nAdditional instructions: " + instructions
	}

	model := a.ResolveModel(sessionKey)
	summaryReq := openai.ChatCompletionRequest{
		Model: model,
		Messages: []openai.ChatCompletionMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: sb.String()},
		},
	}

	resp, err := a.router.Chat(ctx, summaryReq)
	if err != nil || len(resp.Choices) == 0 {
		a.logger.Warnf("soft trim: summarization failed: %v", err)
		return history
	}

	summary := resp.Choices[0].Message.Content
	a.logger.Infof("soft trim: summarized %d messages into %d chars", len(oldMessages), len(summary))

	summaryMsg := session.Message{
		Role:    "assistant",
		Content: fmt.Sprintf("[Conversation summary]: %s", summary),
		TS:      time.Now().UnixMilli(),
	}

	result := []session.Message{summaryMsg}
	result = append(result, recentMessages...)
	return result
}

// softTrim checks if the history exceeds the soft trim threshold and, if so,
// uses LLM-powered compaction (REQ-400) to summarize old messages and persists the result.
// Falls back to forceSoftTrim if compaction fails.
func (a *Agent) softTrim(ctx context.Context, sessionKey string, history []session.Message) []session.Message {
	cfg := a.getCfg()
	ratio := cfg.Agents.Defaults.SoftTrimRatio
	if ratio <= 0 {
		return history
	}
	maxTokens := cfg.Agents.Defaults.ContextPruning.ModelMaxTokens
	threshold := int(ratio * float64(maxTokens))
	tokens := session.EstimateTokens(history)
	if tokens <= threshold {
		return history
	}

	// Try LLM-powered compaction first (REQ-400)
	keepN := cfg.Agents.Defaults.ContextPruning.KeepLastAssistants
	if keepN <= 0 {
		keepN = 2
	}
	compacted, err := CompactHistory(ctx, a.logger, a.router, a.ResolveModel(sessionKey), history, maxTokens, keepN)
	if err != nil {
		a.logger.Warnf("soft trim: compaction failed, falling back to basic summarization: %v", err)
		compacted = a.forceSoftTrim(ctx, sessionKey, history, "")
	}

	// Persist to disk
	if err := a.sessions.ReplaceHistory(sessionKey, compacted); err != nil {
		a.logger.Warnf("soft trim: persist failed: %v", err)
	}
	return compacted
}

// chatOpts holds the strategy options for a chat turn, enabling Chat/ChatLight/ChatStream
// to share a single implementation.
type chatOpts struct {
	promptFn func(ctx context.Context, userText string) string                                                                                    // builds the system prompt (receives user text for semantic recall)
	loopFn   func(ctx context.Context, key, prompt string, msgs []openai.ChatCompletionMessage) (Response, []openai.ChatCompletionMessage, error) // runs the agent loop
}

// StreamCallbacks groups optional callbacks for real-time visibility into the
// agent loop.  All fields are optional; nil callbacks are silently skipped.
type StreamCallbacks struct {
	OnChunk         func(text string)                           // streamed text delta
	OnThinking      func(text string)                           // extended thinking delta
	OnToolStart     func(name string, args string)              // tool about to execute
	OnToolDone      func(name string, result string, err error) // tool finished
	OnIterationText func(text string)                           // intermediate text block emitted alongside tool calls (between iterations)
}

// chatImpl is the shared implementation for Chat, ChatLight, and ChatStream.
func (a *Agent) chatImpl(ctx context.Context, sessionKey, userText string, opts chatOpts) (Response, error) {
	if err := a.acquireSem(ctx); err != nil {
		return Response{}, fmt.Errorf("max concurrent requests exceeded: %w", err)
	}
	defer a.releaseSem()

	ctx = context.WithValue(ctx, agentapi.SessionKeyContextKey{}, sessionKey)

	history, err := a.sessions.GetHistory(sessionKey)
	if err != nil {
		return Response{}, fmt.Errorf("load history: %w", err)
	}

	history = a.softTrim(ctx, sessionKey, history)

	userMsg := session.Message{Role: "user", Content: userText, TS: time.Now().UnixMilli()}
	history = append(history, userMsg)

	a.emitHook(ctx, hooks.Event{Type: hooks.BeforePromptBuild, SessionKey: sessionKey})
	sysPrompt := opts.promptFn(ctx, userText)
	a.emitHook(ctx, hooks.Event{Type: hooks.AfterPromptBuild, SessionKey: sessionKey})

	var newMsgs []session.Message
	newMsgs = append(newMsgs, userMsg)

	resp, addMsgs, err := opts.loopFn(ctx, sessionKey, sysPrompt, session.ToOpenAI(history))
	if err != nil && isContextOverflow(err) {
		// Auto-recovery: clear history and retry with only the current user message.
		a.logger.Warnf("agent: context overflow for %s (%d history msgs), clearing and retrying", sessionKey, len(history))
		cleared := []session.Message{userMsg}
		if replErr := a.sessions.ReplaceHistory(sessionKey, cleared); replErr != nil {
			a.logger.Warnf("agent: auto-clear persist failed: %v", replErr)
		}
		resp, addMsgs, err = opts.loopFn(ctx, sessionKey, sysPrompt, session.ToOpenAI(cleared))
	}
	if err != nil {
		return Response{}, err
	}
	newMsgs = append(newMsgs, session.FromOpenAI(addMsgs)...)

	if err := a.sessions.AppendMessages(sessionKey, newMsgs); err != nil {
		a.logger.Warnf("agent: AppendMessages failed (non-fatal): %v", err)
	}

	if resp.Text != "" {
		a.appendToEidetic(sessionKey, userText, resp.Text)
	}

	return resp, nil
}

// Chat processes a single user message and returns the assistant response.
// sessionKey identifies the conversation (e.g. "agent:main:telegram:12345").
func (a *Agent) Chat(ctx context.Context, sessionKey, userText string) (Response, error) {
	return a.chatImpl(ctx, sessionKey, userText, chatOpts{
		promptFn: a.buildSystemPromptWithRecall,
		loopFn:   a.loop,
	})
}

// ChatWithImages processes a user message with attached images (base64 data URLs).
// The images are included as multi-content parts in the user message for vision models.
func (a *Agent) ChatWithImages(ctx context.Context, sessionKey, userText string, imageURLs []string) (Response, error) {
	if err := a.acquireSem(ctx); err != nil {
		return Response{}, fmt.Errorf("max concurrent requests exceeded: %w", err)
	}
	defer a.releaseSem()

	ctx = context.WithValue(ctx, agentapi.SessionKeyContextKey{}, sessionKey)

	history, err := a.sessions.GetHistory(sessionKey)
	if err != nil {
		return Response{}, fmt.Errorf("load history: %w", err)
	}

	history = a.softTrim(ctx, sessionKey, history)

	userMsg := session.Message{
		Role:      "user",
		Content:   userText,
		ImageURLs: imageURLs,
		TS:        time.Now().UnixMilli(),
	}
	history = append(history, userMsg)

	a.emitHook(ctx, hooks.Event{Type: hooks.BeforePromptBuild, SessionKey: sessionKey})
	sysPrompt := a.buildSystemPromptWithRecall(ctx, userText)
	a.emitHook(ctx, hooks.Event{Type: hooks.AfterPromptBuild, SessionKey: sessionKey})

	var newMsgs []session.Message
	newMsgs = append(newMsgs, userMsg)

	resp, addMsgs, err := a.loop(ctx, sessionKey, sysPrompt, session.ToOpenAI(history))
	if err != nil && isContextOverflow(err) {
		a.logger.Warnf("agent: context overflow (images) for %s (%d history msgs), clearing and retrying", sessionKey, len(history))
		cleared := []session.Message{userMsg}
		if replErr := a.sessions.ReplaceHistory(sessionKey, cleared); replErr != nil {
			a.logger.Warnf("agent: auto-clear persist failed: %v", replErr)
		}
		resp, addMsgs, err = a.loop(ctx, sessionKey, sysPrompt, session.ToOpenAI(cleared))
	}
	if err != nil {
		return Response{}, err
	}
	newMsgs = append(newMsgs, session.FromOpenAI(addMsgs)...)

	if err := a.sessions.AppendMessages(sessionKey, newMsgs); err != nil {
		a.logger.Warnf("agent: AppendMessages failed (non-fatal): %v", err)
	}

	if resp.Text != "" {
		a.appendToEidetic(sessionKey, userText, resp.Text)
	}

	return resp, nil
}

// ChatLight is like Chat but uses a minimal system prompt (identity + HEARTBEAT.md only).
// Used for cron/heartbeat jobs with lightContext enabled.
func (a *Agent) ChatLight(ctx context.Context, sessionKey, userText string) (Response, error) {
	return a.chatImpl(ctx, sessionKey, userText, chatOpts{
		promptFn: func(_ context.Context, _ string) string { return a.buildLightSystemPrompt() },
		loopFn:   a.loop,
	})
}

// ChatStream is like Chat but provides real-time visibility via callbacks.
func (a *Agent) ChatStream(ctx context.Context, sessionKey, userText string, cb *StreamCallbacks) (Response, error) {
	var onChunk func(string)
	if cb != nil {
		onChunk = cb.OnChunk
	}
	return a.chatImpl(ctx, sessionKey, userText, chatOpts{
		promptFn: a.buildSystemPromptWithRecall,
		loopFn: func(ctx context.Context, key, prompt string, msgs []openai.ChatCompletionMessage) (Response, []openai.ChatCompletionMessage, error) {
			return a.loopStream(ctx, key, prompt, msgs, onChunk, cb)
		},
	})
}

// modelCallResult is the outcome of a single model API call (full or streaming).
type modelCallResult struct {
	msg    openai.ChatCompletionMessage // assembled assistant message
	inTok  int                          // input tokens (exact or estimated)
	outTok int                          // output tokens (exact or estimated)
}

// modelCallerFn performs one model API call and returns the result.
// stream=true uses ChatStream, stream=false uses Chat.
type modelCallerFn func(ctx context.Context, req openai.ChatCompletionRequest) (modelCallResult, error)

// fullModelCaller returns a modelCallerFn that does a blocking Chat call.
func (a *Agent) fullModelCaller() modelCallerFn {
	return func(ctx context.Context, req openai.ChatCompletionRequest) (modelCallResult, error) {
		resp, err := a.router.Chat(ctx, req)
		if err != nil {
			return modelCallResult{}, err
		}
		if len(resp.Choices) == 0 {
			return modelCallResult{}, fmt.Errorf("empty response from model")
		}
		return modelCallResult{
			msg:    resp.Choices[0].Message,
			inTok:  resp.Usage.PromptTokens,
			outTok: resp.Usage.CompletionTokens,
		}, nil
	}
}

// streamModelCaller returns a modelCallerFn that streams, calling onChunk per text delta
// and onThinking per extended thinking delta.
func (a *Agent) streamModelCaller(onChunk func(string), onThinking func(string)) modelCallerFn {
	return func(ctx context.Context, req openai.ChatCompletionRequest) (modelCallResult, error) {
		stream, err := a.router.ChatStream(ctx, req)
		if err != nil {
			return modelCallResult{}, err
		}

		// Wire thinking callback if the provider supports it.
		if onThinking != nil {
			if ts, ok := stream.(models.ThinkingStream); ok {
				ts.SetThinkingCallback(onThinking)
			}
		}

		var contentBuf strings.Builder
		var toolCalls []openai.ToolCall
		isToolCall := false

		for {
			chunk, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				_ = stream.Close()
				return modelCallResult{}, err
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			delta := chunk.Choices[0].Delta

			if delta.Content != "" {
				contentBuf.WriteString(delta.Content)
				if onChunk != nil {
					onChunk(delta.Content)
				}
			}

			if len(delta.ToolCalls) > 0 {
				isToolCall = true
				for _, tc := range delta.ToolCalls {
					i := 0
					if tc.Index != nil {
						i = *tc.Index
					}
					const maxToolCalls = 128
					if i >= maxToolCalls {
						continue // ignore malformed index
					}
					for len(toolCalls) <= i {
						toolCalls = append(toolCalls, openai.ToolCall{})
					}
					existing := &toolCalls[i]
					if tc.ID != "" {
						existing.ID = tc.ID
					}
					if tc.Type != "" {
						existing.Type = tc.Type
					}
					if tc.Function.Name != "" {
						existing.Function.Name = tc.Function.Name
					}
					existing.Function.Arguments += tc.Function.Arguments
				}
			}
		}
		_ = stream.Close()

		// Capture usage from stream if the provider supports it.
		var inTok, outTok int
		if su, ok := stream.(models.StreamUsage); ok {
			inTok, outTok = su.Usage()
		} else {
			outTok = len(contentBuf.String()) / 4 // rough estimate
		}

		msg := openai.ChatCompletionMessage{Role: "assistant", Content: contentBuf.String()}

		// Filter phantom empty tool-call slots (Copilot/Anthropic index mapping).
		if isToolCall && len(toolCalls) > 0 {
			real := toolCalls[:0]
			for _, tc := range toolCalls {
				if tc.Function.Name == "" && tc.Function.Arguments == "" {
					continue
				}
				if tc.ID == "" {
					tc.ID = randomToolCallID()
				}
				real = append(real, tc)
			}
			if len(real) > 0 {
				msg.ToolCalls = real
			}
		}

		return modelCallResult{msg: msg, inTok: inTok, outTok: outTok}, nil
	}
}

// agentLoop is the unified tool-calling iteration loop used by both loop() and loopStream().
// The caller provides a modelCallerFn that abstracts the difference between full and streaming calls.
func (a *Agent) agentLoop(ctx context.Context, sessionKey, sysPrompt string, messages []openai.ChatCompletionMessage, streaming bool, caller modelCallerFn, cb *StreamCallbacks) (Response, []openai.ChatCompletionMessage, error) {
	var added []openai.ChatCompletionMessage
	var fullText strings.Builder // accumulates assistant text across ALL iterations
	cfg := a.getCfg()

	// Configure loop detection (REQ-410)
	tldCfg := cfg.Agents.Defaults.ToolLoopDetection
	useMulti := tldCfg.Enabled
	var tld *ToolLoopDetector
	var ld *loopDetector
	if useMulti {
		tld = NewToolLoopDetector(ToolLoopDetectionConfig{
			Enabled:                       tldCfg.Enabled,
			HistorySize:                   tldCfg.HistorySize,
			WarningThreshold:              tldCfg.WarningThreshold,
			CriticalThreshold:             tldCfg.CriticalThreshold,
			GlobalCircuitBreakerThreshold: tldCfg.GlobalCircuitBreakerThreshold,
			GenericRepeat:                 true,
			KnownPollNoProgress:           true,
			PingPong:                      true,
		})
	} else {
		ld = newLoopDetector(cfg.Agents.Defaults.LoopDetectionN)
	}
	var totalInput, totalOutput int
	overflowCompactions := 0

	maxIt := a.maxIter()
	softWarnAt := maxIt * 4 / 5 // 80% of max → nudge to wrap up / delegate
	softWarned := false

	for iter := range maxIt {
		// Soft warning: inject a system nudge when approaching the cap so the
		// model has a chance to delegate remaining work or summarise progress.
		if !softWarned && iter >= softWarnAt {
			softWarned = true
			remaining := maxIt - iter
			nudge := openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleSystem,
				Content: fmt.Sprintf("[SYSTEM] You have used %d/%d tool-call iterations. Only %d remain. Wrap up NOW: delegate any remaining work to a subagent (coding-agent, researcher, reviewer, sysadmin, or orchestrator), or summarise your progress for the user. Do NOT start new multi-step work.", iter, maxIt, remaining),
			}
			messages = append(messages, nudge)
			added = append(added, nudge)
		}

		a.emitHook(ctx, hooks.Event{Type: hooks.BeforeModelResolve, SessionKey: sessionKey, Model: a.ResolveModel(sessionKey)})
		req, err := a.buildRequest(ctx, sessionKey, sysPrompt, messages, streaming)
		if err != nil {
			return Response{}, added, err
		}

		result, err := caller(ctx, req)
		if err != nil {
			// Context overflow recovery: auto-compact and retry (up to 3 times).
			if isContextOverflow(err) && overflowCompactions < maxOverflowCompactions {
				overflowCompactions++
				a.logger.Warnf("agent: context overflow detected (attempt %d/%d), compacting mid-loop", overflowCompactions, maxOverflowCompactions)

				// Convert messages back to session format for compaction.
				sessionMsgs := session.FromOpenAI(messages)
				compacted := a.forceSoftTrim(ctx, sessionKey, sessionMsgs, "")
				messages = session.ToOpenAI(compacted)
				// Persist the compacted history.
				if err := a.sessions.ReplaceHistory(sessionKey, compacted); err != nil {
					a.logger.Warnf("agent: mid-loop compact persist failed: %v", err)
				}
				continue // retry with compacted context
			}
			return Response{}, added, err
		}
		a.sessions.TouchAPICall(sessionKey)
		a.emitHook(ctx, hooks.Event{Type: hooks.AfterModelResolve, SessionKey: sessionKey, Model: req.Model})

		// Accumulate usage (REQ-420)
		totalInput += result.inTok
		totalOutput += result.outTok
		if a.Usage != nil {
			a.Usage.Accumulate(sessionKey, NormalizedUsage{
				Input:  result.inTok,
				Output: result.outTok,
				Total:  result.inTok + result.outTok,
			})
		}

		assistantMsg := result.msg
		messages = append(messages, assistantMsg)
		added = append(added, assistantMsg)

		// Accumulate text from every iteration so callers see all text.
		if assistantMsg.Content != "" {
			fullText.WriteString(assistantMsg.Content)
			// Block reply: deliver intermediate text between tool iterations.
			if len(assistantMsg.ToolCalls) > 0 && cb != nil && cb.OnIterationText != nil {
				cb.OnIterationText(assistantMsg.Content)
			}
		}

		if len(assistantMsg.ToolCalls) == 0 {
			return Response{
				Text:  fullText.String(),
				Usage: ResponseUsage{InputTokens: totalInput, OutputTokens: totalOutput},
			}, added, nil
		}

		// Loop detection: multi-detector or simple
		var recordIndices []int
		if useMulti && tld != nil {
			for _, tc := range assistantMsg.ToolCalls {
				idx := tld.RecordCall(tc.Function.Name, tc.Function.Arguments)
				recordIndices = append(recordIndices, idx)
			}
			res := tld.DetectLoop()
			if res.Stuck {
				errMsg := fmt.Sprintf("Loop detected (%s): %s", res.Detector, res.Message)
				if res.Level == LoopLevelCritical {
					a.logger.Warnf("agent: %s — breaking out", errMsg)
					return Response{Text: errMsg, Stopped: true, Usage: ResponseUsage{InputTokens: totalInput, OutputTokens: totalOutput}}, added, nil
				}
				a.logger.Infof("agent: %s — warning, continuing", errMsg)
			}
		} else if ld != nil && ld.check(assistantMsg.ToolCalls) {
			errMsg := fmt.Sprintf("Loop detected: the same tool call was repeated %d consecutive times. Breaking out.", ld.limit)
			a.logger.Warnf("agent: %s", errMsg)
			return Response{Text: errMsg, Stopped: true, Usage: ResponseUsage{InputTokens: totalInput, OutputTokens: totalOutput}}, added, nil
		}

		// Execute tool calls
		if cb != nil && cb.OnToolStart != nil {
			for _, tc := range assistantMsg.ToolCalls {
				cb.OnToolStart(tc.Function.Name, tc.Function.Arguments)
			}
		}

		toolMsgs := a.executeTools(ctx, assistantMsg.ToolCalls)
		if cb != nil && cb.OnToolDone != nil {
			for i, tc := range assistantMsg.ToolCalls {
				if i < len(toolMsgs) {
					cb.OnToolDone(tc.Function.Name, toolMsgs[i].Content, nil)
				}
			}
		}
		messages = append(messages, toolMsgs...)
		added = append(added, toolMsgs...)

		// Record tool outcomes for poll-no-progress detection
		if useMulti && tld != nil {
			for i, idx := range recordIndices {
				if i < len(toolMsgs) {
					tld.RecordOutcome(idx, toolMsgs[i].Content)
				}
			}
		}
	}

	// Hard cap reached — attempt to auto-delegate remaining work to a CLI agent.
	capMsg := fmt.Sprintf("Maximum iterations reached (%d). ", maxIt)
	if dt, ok := a.toolMap["delegate"]; ok {
		if d, ok := dt.(*DelegateTool); ok {
			// Pick the best CLI agent to continue the work.
			for _, agentID := range []string{"coding-agent", "researcher", "orchestrator"} {
				if _, exists := d.Agents[agentID]; exists {
					// Build a continuation summary from the last assistant message.
					var summary string
					if txt := fullText.String(); txt != "" {
						// Truncate to avoid sending a huge payload.
						if len(txt) > 2000 {
							summary = txt[len(txt)-2000:]
						} else {
							summary = txt
						}
					}
					contPrompt := fmt.Sprintf("Continue the following task that was cut short after %d tool iterations. Pick up where it left off and complete the remaining work. Context so far:\n\n%s", maxIt, summary)
					a.logger.Infof("agent: max iterations reached — auto-delegating to %q", agentID)
					contResp, contErr := d.Agents[agentID].Chat(ctx, sessionKey, contPrompt)
					if contErr != nil {
						a.logger.Warnf("agent: auto-delegate to %q failed: %v", agentID, contErr)
						capMsg += fmt.Sprintf("Tried to delegate remaining work to %s but it failed: %v", agentID, contErr)
					} else {
						capMsg += fmt.Sprintf("Remaining work was auto-delegated to %s.\n\n%s", agentID, contResp.Text)
					}
					break
				}
			}
		}
	}
	return Response{Text: capMsg, Stopped: true, Usage: ResponseUsage{InputTokens: totalInput, OutputTokens: totalOutput}}, added, nil
}

// loop runs the non-streaming agent iteration loop.
func (a *Agent) loop(ctx context.Context, sessionKey, sysPrompt string, messages []openai.ChatCompletionMessage) (Response, []openai.ChatCompletionMessage, error) {
	return a.agentLoop(ctx, sessionKey, sysPrompt, messages, false, a.fullModelCaller(), nil)
}

// loopStream runs the streaming agent iteration loop, calling onChunk for each text delta.
func (a *Agent) loopStream(ctx context.Context, sessionKey, sysPrompt string, messages []openai.ChatCompletionMessage, onChunk func(string), cb *StreamCallbacks) (Response, []openai.ChatCompletionMessage, error) {
	var onThinking func(string)
	if cb != nil {
		onThinking = cb.OnThinking
	}
	return a.agentLoop(ctx, sessionKey, sysPrompt, messages, true, a.streamModelCaller(onChunk, onThinking), cb)
}

func (a *Agent) executeTools(ctx context.Context, calls []openai.ToolCall) []openai.ChatCompletionMessage {
	msgs := make([]openai.ChatCompletionMessage, len(calls))
	var wg sync.WaitGroup
	for i, tc := range calls {
		wg.Go(func() {
			defer func() {
				if r := recover(); r != nil {
					a.logger.Errorf("tool %s panic: %v", tc.Function.Name, r)
					msgs[i] = openai.ChatCompletionMessage{
						Role:       "tool",
						Content:    fmt.Sprintf("tool panic: %v", r),
						ToolCallID: tc.ID,
					}
				}
			}()
			a.emitHook(ctx, hooks.Event{Type: hooks.BeforeToolCall, ToolName: tc.Function.Name, Data: map[string]any{"args": tc.Function.Arguments}})
			var result string
			t, ok := a.toolMap[tc.Function.Name]
			if !ok {
				result = fmt.Sprintf("unknown tool: %s", tc.Function.Name)
			} else {
				result = t.Run(ctx, tc.Function.Arguments)
			}
			a.emitHook(ctx, hooks.Event{Type: hooks.AfterToolCall, ToolName: tc.Function.Name})
			msgs[i] = openai.ChatCompletionMessage{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
			}
		})
	}
	wg.Wait()
	return msgs
}

func (a *Agent) buildRequest(ctx context.Context, sessionKey, sysPrompt string, messages []openai.ChatCompletionMessage, stream bool) (openai.ChatCompletionRequest, error) {
	_ = ctx // may be used in future (e.g. streaming options)
	allMessages := []openai.ChatCompletionMessage{
		{Role: "system", Content: sysPrompt},
	}
	allMessages = append(allMessages, messages...)

	model := a.ResolveModel(sessionKey)

	// Context window guard (REQ-401): hard-block when context window is too small
	cfg := a.getCfg()
	contextTokens := ResolveContextWindow(cfg.Agents.Defaults.ContextPruning.ModelMaxTokens, 0)
	if err := ValidateContextWindow(a.logger, contextTokens); err != nil {
		return openai.ChatCompletionRequest{}, fmt.Errorf("context window guard: %w", err)
	}

	return openai.ChatCompletionRequest{
		Model:    model,
		Messages: allMessages,
		Tools:    a.toolDefs,
		Stream:   stream,
	}, nil
}

// DefaultTools creates the standard tool set from config.
func DefaultTools(cfg *config.Root, workspace string, env map[string]string) []Tool {
	var sandboxCfg *tools.SandboxConfig
	if cfg.Agents.Defaults.Sandbox.Enabled {
		sandboxCfg = &tools.SandboxConfig{
			Enabled:      true,
			Image:        cfg.Agents.Defaults.Sandbox.Image,
			Mounts:       cfg.Agents.Defaults.Sandbox.Mounts,
			SetupCommand: cfg.Agents.Defaults.Sandbox.SetupCommand,
		}
	}

	ssrfTransport := tools.SSRFSafeTransport()

	toolList := []Tool{
		&tools.ExecTool{
			DefaultTimeout:      time.Duration(cfg.Tools.Exec.TimeoutSec) * time.Second,
			BackgroundWait:      time.Duration(cfg.Tools.Exec.BackgroundMs) * time.Millisecond,
			BackgroundHardLimit: time.Duration(cfg.Tools.Exec.BackgroundHardTimeM) * time.Minute,
			MaxOutputChars:      cfg.Tools.Exec.MaxOutputChars,
			Env:                 env,
			DenyCommands:        cfg.Tools.Exec.DenyCommands,
		DangerousPatterns:   cfg.Tools.Exec.DangerousPatterns,
			Sandbox:             sandboxCfg,
		},
		&tools.WebSearchTool{
			MaxResults:     cfg.Tools.Web.Search.MaxResults,
			TimeoutSeconds: cfg.Tools.Web.Search.TimeoutSeconds,
		},
		&tools.WebFetchTool{
			MaxChars:       cfg.Tools.Web.Fetch.MaxChars,
			TimeoutSeconds: cfg.Tools.Web.Fetch.TimeoutSeconds,
			Transport:      ssrfTransport,
		},
		&tools.ReadFileTool{AllowPaths: cfg.Tools.Files.AllowPaths},
		&tools.WriteFileTool{AllowPaths: cfg.Tools.Files.AllowPaths},
		&tools.ListDirTool{AllowPaths: cfg.Tools.Files.AllowPaths},
	}

	// Memory tools
	if cfg.Agents.Defaults.Memory.Enabled && workspace != "" {
		toolList = append(toolList,
			&tools.MemoryAppendTool{Workspace: workspace},
			&tools.MemoryGetTool{Workspace: workspace},
		)
	}

	// Notify tool — Announcers are wired later by wireDeliverers in main.go
	toolList = append(toolList, &tools.NotifyUserTool{})

	return toolList
}
