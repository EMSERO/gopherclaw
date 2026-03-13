package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/agentapi"
	"github.com/EMSERO/gopherclaw/internal/config"
	"github.com/EMSERO/gopherclaw/internal/eidetic"
	"github.com/EMSERO/gopherclaw/internal/embeddings"
	"github.com/EMSERO/gopherclaw/internal/memory"
	"github.com/EMSERO/gopherclaw/internal/models"
)

// StreamingCLIAgent implements PrimaryAgent by driving a long-lived
// `claude -p --input-format stream-json --output-format stream-json` subprocess.
// Each session gets its own subprocess; idle subprocesses are reaped after a TTL.
type StreamingCLIAgent struct {
	command string   // resolved path to claude binary
	args    []string // base args (before --session-id)
	logger  *zap.SugaredLogger

	mu       sync.Mutex
	sessions map[string]*cliSession // sessionKey → live subprocess
	usage    *UsageTracker

	// Per-session model overrides.
	modelMu       sync.RWMutex
	sessionModels map[string]string

	idleTTL time.Duration // how long before idle sessions are reaped

	// Memory integration
	cfg       *config.Root
	workspace string

	// Base system prompt (identity, skills, workspace docs — built once)
	baseSystemPrompt string

	// Eidetic memory (nil = disabled)
	eideticMu  sync.RWMutex
	eideticC   eidetic.Client
	embedMu    sync.RWMutex
	embedC     *embeddings.Client
	eideticSem chan struct{} // bounds concurrent background writes

	// MEMORY.md cache (re-read only when mtime changes)
	memoryMu    sync.Mutex
	memoryCache string
	memoryMtime time.Time
}

// cliSession is a running claude subprocess for one session.
type cliSession struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	cancel context.CancelFunc

	mu       sync.Mutex // serialises Chat calls on this session
	idle     *time.Timer
	initDone bool // true after the system init event has been consumed
}

// StreamingCLIConfig configures a StreamingCLIAgent.
type StreamingCLIConfig struct {
	Command      string   // path or name of the claude binary (default "claude")
	ExtraArgs    []string // additional CLI flags (e.g. --mcp-config, --system-prompt)
	Model        string   // model to request (e.g. "sonnet")
	IdleTTL      time.Duration
	MCPConfig    string // path to MCP config JSON for GopherClaw tools
	SystemPrompt string // static base system prompt (identity, skills, workspace docs)

	// Memory integration
	Config    *config.Root // full config for eidetic settings, timezone, etc.
	Workspace string       // filesystem path for MEMORY.md, daily logs
}

// NewStreamingCLIAgent creates a StreamingCLIAgent.
func NewStreamingCLIAgent(logger *zap.SugaredLogger, cfg StreamingCLIConfig) (*StreamingCLIAgent, error) {
	command := cfg.Command
	if command == "" {
		command = "claude"
	}
	resolved, err := exec.LookPath(command)
	if err != nil {
		return nil, fmt.Errorf("streaming-cli: cannot find %q on PATH: %w", command, err)
	}

	// Build base args.  Session-specific flags are added at spawn time.
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--dangerously-skip-permissions",
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.MCPConfig != "" {
		args = append(args, "--mcp-config", cfg.MCPConfig)
	}
	// NOTE: --system-prompt is NOT baked into base args.  It is built
	// dynamically at spawn time so it can include MEMORY.md and eidetic
	// recent entries.  The static portion is stored in baseSystemPrompt.
	args = append(args, cfg.ExtraArgs...)

	ttl := cfg.IdleTTL
	if ttl == 0 {
		ttl = 30 * time.Minute
	}

	return &StreamingCLIAgent{
		command:          resolved,
		args:             args,
		logger:           logger,
		sessions:         make(map[string]*cliSession),
		usage:            NewUsageTracker(),
		sessionModels:    make(map[string]string),
		idleTTL:          ttl,
		cfg:              cfg.Config,
		workspace:        cfg.Workspace,
		baseSystemPrompt: cfg.SystemPrompt,
		eideticSem:       make(chan struct{}, 8),
	}, nil
}

// Compile-time check.
var _ PrimaryAgent = (*StreamingCLIAgent)(nil)

// ── subprocess lifecycle ────────────────────────────────────────────

// getOrSpawn returns the live subprocess for a session, spawning one if needed.
func (s *StreamingCLIAgent) getOrSpawn(sessionKey string) (*cliSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sess, ok := s.sessions[sessionKey]; ok {
		sess.resetIdle(s.idleTTL)
		return sess, nil
	}

	sess, err := s.spawn(sessionKey)
	if err != nil {
		return nil, err
	}
	s.sessions[sessionKey] = sess
	return sess, nil
}

// spawn starts a new claude subprocess for the given session.
func (s *StreamingCLIAgent) spawn(sessionKey string) (*cliSession, error) {
	ctx, cancel := context.WithCancel(context.Background())

	args := make([]string, len(s.args))
	copy(args, s.args)

	// Check for per-session model override.
	s.modelMu.RLock()
	if m, ok := s.sessionModels[sessionKey]; ok {
		// Remove any existing --model flag and add the override.
		filtered := make([]string, 0, len(args))
		for i := 0; i < len(args); i++ {
			if args[i] == "--model" && i+1 < len(args) {
				i++ // skip the value
				continue
			}
			filtered = append(filtered, args[i])
		}
		args = append(filtered, "--model", m)
	}
	s.modelMu.RUnlock()

	// Build dynamic system prompt with MEMORY.md + eidetic recent entries.
	sysPrompt := s.buildDynamicSystemPrompt()
	if sysPrompt != "" {
		args = append(args, "--system-prompt", sysPrompt)
	}

	cmd := exec.CommandContext(ctx, s.command, args...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("streaming-cli: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("streaming-cli: stdout pipe: %w", err)
	}
	// Discard stderr to avoid blocking.
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("streaming-cli: start %q: %w", s.command, err)
	}

	s.logger.Infof("streaming-cli: spawned subprocess pid=%d for session %q", cmd.Process.Pid, sessionKey)

	sess := &cliSession{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewScanner(stdout),
		cancel: cancel,
	}

	// Set a generous max line size for JSON events.
	sess.stdout.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	sess.idle = time.AfterFunc(s.idleTTL, func() {
		s.reap(sessionKey)
	})

	// With --input-format stream-json, the init event only arrives after
	// the first message is sent, so we don't wait for it here.  Instead,
	// readResponse skips past it.
	sess.initDone = false

	return sess, nil
}

// reap kills and removes an idle session.
func (s *StreamingCLIAgent) reap(sessionKey string) {
	s.mu.Lock()
	sess, ok := s.sessions[sessionKey]
	if ok {
		delete(s.sessions, sessionKey)
	}
	s.mu.Unlock()
	if ok {
		s.logger.Infof("streaming-cli: reaping idle session %q", sessionKey)
		sess.kill()
	}
}

// Close kills all subprocesses.
func (s *StreamingCLIAgent) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, sess := range s.sessions {
		s.logger.Infof("streaming-cli: closing session %q", key)
		sess.kill()
	}
	s.sessions = make(map[string]*cliSession)
}

// ── memory integration ──────────────────────────────────────────────

// SetEidetic wires an Eidetic client into the agent. Pass nil to disable.
func (s *StreamingCLIAgent) SetEidetic(client eidetic.Client) {
	s.eideticMu.Lock()
	s.eideticC = client
	s.eideticMu.Unlock()
}

func (s *StreamingCLIAgent) getEidetic() eidetic.Client {
	s.eideticMu.RLock()
	c := s.eideticC
	s.eideticMu.RUnlock()
	return c
}

// SetEmbeddings wires an embeddings client for hybrid search. Pass nil to disable.
func (s *StreamingCLIAgent) SetEmbeddings(client *embeddings.Client) {
	s.embedMu.Lock()
	s.embedC = client
	s.embedMu.Unlock()
}

func (s *StreamingCLIAgent) getEmbeddings() *embeddings.Client {
	s.embedMu.RLock()
	c := s.embedC
	s.embedMu.RUnlock()
	return c
}

func (s *StreamingCLIAgent) embed(ctx context.Context, text string) []float32 {
	ec := s.getEmbeddings()
	if ec == nil {
		return nil
	}
	vec, err := ec.Embed(ctx, text)
	if err != nil {
		s.logger.Debugf("embeddings: generation failed (non-fatal): %v", err)
		return nil
	}
	return vec
}

func (s *StreamingCLIAgent) eideticAgentID() string {
	if s.cfg != nil {
		if id := s.cfg.Eidetic.AgentID; id != "" {
			return id
		}
	}
	return "main"
}

// loadMemoryCached returns MEMORY.md content, re-reading only when mtime changes.
func (s *StreamingCLIAgent) loadMemoryCached() string {
	if s.workspace == "" {
		return ""
	}
	if s.cfg != nil && !s.cfg.Agents.Defaults.Memory.Enabled {
		return ""
	}
	p := filepath.Join(s.workspace, "MEMORY.md")
	info, err := os.Stat(p)
	if err != nil {
		return ""
	}
	s.memoryMu.Lock()
	if info.ModTime().Equal(s.memoryMtime) && s.memoryCache != "" {
		cached := s.memoryCache
		s.memoryMu.Unlock()
		return cached
	}
	s.memoryMu.Unlock()

	content := memory.LoadMemoryMD(s.workspace)
	s.memoryMu.Lock()
	s.memoryMtime = info.ModTime()
	s.memoryCache = content
	s.memoryMu.Unlock()
	return content
}

// buildDynamicSystemPrompt builds the full system prompt for a subprocess,
// combining the static base (identity, skills, workspace docs) with dynamic
// content: current time, MEMORY.md, and recent eidetic entries.
func (s *StreamingCLIAgent) buildDynamicSystemPrompt() string {
	var sb strings.Builder
	sb.WriteString(s.baseSystemPrompt)

	// Current date/time
	tz := "UTC"
	if s.cfg != nil && s.cfg.Agents.Defaults.UserTimezone != "" {
		tz = s.cfg.Agents.Defaults.UserTimezone
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	fmt.Fprintf(&sb, "Current date/time: %s (%s)\n\n", now.Format("2006-01-02 15:04:05"), tz)

	// MEMORY.md
	if content := s.loadMemoryCached(); content != "" {
		sb.WriteString("## Memory\n\n")
		sb.WriteString(content)
		sb.WriteString("\n\n")
	}

	// Eidetic-specific rules
	if s.getEidetic() != nil {
		sb.WriteString("- Before asking the user a question, use eidetic_search to check if you already know the answer from a previous conversation. Only ask if memory has no relevant result.\n\n")
	}

	// Recent eidetic entries (2s timeout, non-fatal)
	if c := s.getEidetic(); c != nil {
		limit := 20
		if s.cfg != nil && s.cfg.Eidetic.RecentLimit > 0 {
			limit = s.cfg.Eidetic.RecentLimit
		}
		rCtx, rCancel := context.WithTimeout(context.Background(), 2*time.Second)
		entries, rErr := c.GetRecent(rCtx, s.eideticAgentID(), limit)
		rCancel()
		if rErr != nil {
			s.logger.Debugf("eidetic: get_recent failed (non-fatal): %v", rErr)
		} else if len(entries) > 0 {
			sb.WriteString("## Recent Memory\n\n")
			for _, e := range entries {
				fmt.Fprintf(&sb, "- [%s] %s\n",
					e.Timestamp.Format("2006-01-02 15:04"),
					e.Content,
				)
			}
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

// enrichWithRecall prepends semantically recalled memories to the user message,
// giving the subprocess context from previous conversations.
func (s *StreamingCLIAgent) enrichWithRecall(ctx context.Context, message string) string {
	c := s.getEidetic()
	if c == nil || message == "" {
		return message
	}
	if s.cfg != nil && s.cfg.Eidetic.RecallEnabled != nil && !*s.cfg.Eidetic.RecallEnabled {
		return message
	}
	if len(strings.Fields(message)) < 3 {
		return message
	}

	limit := 5
	threshold := 0.4
	timeoutSec := 5
	if s.cfg != nil {
		if s.cfg.Eidetic.RecallLimit > 0 {
			limit = s.cfg.Eidetic.RecallLimit
		}
		if s.cfg.Eidetic.RecallThreshold > 0 {
			threshold = s.cfg.Eidetic.RecallThreshold
		}
		if s.cfg.Eidetic.RecallTimeoutS > 0 {
			timeoutSec = s.cfg.Eidetic.RecallTimeoutS
		} else if s.cfg.Eidetic.TimeoutSeconds > 0 {
			timeoutSec = s.cfg.Eidetic.TimeoutSeconds
		}
	}

	rCtx, rCancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer rCancel()

	queryVec := s.embed(rCtx, message)
	fetchLimit := limit * 2
	if fetchLimit < 10 {
		fetchLimit = 10
	}

	results, err := c.SearchMemory(rCtx, eidetic.SearchRequest{
		Query:     message,
		Limit:     fetchLimit,
		Threshold: threshold,
		Vector:    queryVec,
		Hybrid:    queryVec != nil,
	})
	if err != nil {
		s.logger.Debugf("eidetic: recall search failed (non-fatal): %v", err)
		return message
	}
	if len(results) == 0 {
		return message
	}

	results = eidetic.MMR(results, 0.7, limit)

	var sb strings.Builder
	sb.WriteString("[Recalled memories from previous conversations:]\n")
	count := 0
	for _, r := range results {
		if r.Relevance < threshold {
			continue
		}
		count++
		fmt.Fprintf(&sb, "- [%s] (relevance: %.0f%%) %s\n",
			r.Timestamp.Format("2006-01-02"),
			r.Relevance*100,
			r.Content,
		)
	}
	if count == 0 {
		return message
	}
	sb.WriteString("\n---\n")
	sb.WriteString(message)
	return sb.String()
}

// appendToEidetic saves a conversation turn to eidetic memory in the background.
func (s *StreamingCLIAgent) appendToEidetic(sessionKey, userText, assistantText string) {
	c := s.getEidetic()
	if c == nil {
		return
	}
	content := fmt.Sprintf("[User]: %s\n[Assistant]: %s", userText, assistantText)
	agentID := s.eideticAgentID()
	tags := []string{
		"session:" + sessionKey,
		"agent:" + agentID,
	}
	select {
	case s.eideticSem <- struct{}{}:
	default:
		s.logger.Debugf("eidetic: append_memory skipped (semaphore full)")
		return
	}
	go func() {
		defer func() { <-s.eideticSem }()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		vec := s.embed(ctx, content)
		if err := c.AppendMemory(ctx, eidetic.AppendRequest{
			Content: content,
			AgentID: agentID,
			Tags:    tags,
			Vector:  vec,
		}); err != nil {
			s.logger.Warnf("eidetic: append_memory failed (non-fatal): %v", err)
		}
	}()
}

// ── cliSession helpers ──────────────────────────────────────────────

func (cs *cliSession) resetIdle(d time.Duration) {
	if cs.idle != nil {
		cs.idle.Reset(d)
	}
}

func (cs *cliSession) kill() {
	cs.cancel()
	_ = cs.stdin.Close()
	if cs.cmd == nil || cs.cmd.Process == nil {
		return
	}
	// Wait with a short timeout so we don't block forever.
	done := make(chan struct{})
	go func() {
		_ = cs.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

// ── NDJSON event types ──────────────────────────────────────────────

// cliEvent is the envelope for all stream-json output events.
type cliEvent struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Event     json.RawMessage `json:"event,omitempty"`   // for stream_event
	Message   json.RawMessage `json:"message,omitempty"` // for assistant/user
	Result    string          `json:"result,omitempty"`   // for result
	IsError   bool            `json:"is_error,omitempty"`
	Errors    []string        `json:"errors,omitempty"`

	// Result fields
	DurationMs  int            `json:"duration_ms,omitempty"`
	TotalCost   float64        `json:"total_cost_usd,omitempty"`
	Usage       *cliUsage      `json:"usage,omitempty"`
	ModelUsage  map[string]any `json:"modelUsage,omitempty"`
	NumTurns    int            `json:"num_turns,omitempty"`
	StopReason  string         `json:"stop_reason,omitempty"`
	Error       string         `json:"error,omitempty"` // on assistant error
}

type cliUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// streamEvent is the inner event for type=stream_event.
type streamEvent struct {
	Type         string          `json:"type"` // message_start, content_block_start, content_block_delta, content_block_stop, message_delta, message_stop
	Index        int             `json:"index,omitempty"`
	Delta        json.RawMessage `json:"delta,omitempty"`
	ContentBlock json.RawMessage `json:"content_block,omitempty"`
}

type textDelta struct {
	Type string `json:"type"` // text_delta
	Text string `json:"text"`
}

type contentBlock struct {
	Type string `json:"type"` // text, tool_use
	Text string `json:"text,omitempty"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// cliInputMessage is the SDK-compatible format written to stdin.
type cliInputMessage struct {
	Type    string          `json:"type"`
	Message cliInputContent `json:"message"`
}

type cliInputContent struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ── PrimaryAgent implementation ─────────────────────────────────────

func (s *StreamingCLIAgent) Chat(ctx context.Context, sessionKey, message string) (Response, error) {
	return s.chatImpl(ctx, sessionKey, message, nil, nil)
}

func (s *StreamingCLIAgent) ChatStream(ctx context.Context, sessionKey, message string, cb *StreamCallbacks) (Response, error) {
	return s.chatImpl(ctx, sessionKey, message, nil, cb)
}

func (s *StreamingCLIAgent) ChatWithImages(ctx context.Context, sessionKey, caption string, imageURLs []string) (Response, error) {
	// Append image URLs to the message text; claude CLI doesn't support
	// inline base64 easily via stream-json input, so we describe them.
	var sb strings.Builder
	sb.WriteString(caption)
	for _, u := range imageURLs {
		sb.WriteString("\n\n[Image: ")
		sb.WriteString(u)
		sb.WriteString("]")
	}
	return s.chatImpl(ctx, sessionKey, sb.String(), nil, nil)
}

func (s *StreamingCLIAgent) ChatLight(ctx context.Context, sessionKey, message string) (Response, error) {
	return s.Chat(ctx, sessionKey, message)
}

func (s *StreamingCLIAgent) Compact(ctx context.Context, sessionKey, instructions string) error {
	// Not directly supported — we kill the session so the next message starts fresh.
	s.reap(sessionKey)
	return nil
}

func (s *StreamingCLIAgent) SetSessionModel(key, model string) {
	s.modelMu.Lock()
	s.sessionModels[key] = model
	s.modelMu.Unlock()
	// Kill existing session so it respawns with the new model.
	s.reap(key)
}

func (s *StreamingCLIAgent) ClearSessionModel(key string) {
	s.modelMu.Lock()
	delete(s.sessionModels, key)
	s.modelMu.Unlock()
}

func (s *StreamingCLIAgent) ResolveModel(key string) string {
	s.modelMu.RLock()
	defer s.modelMu.RUnlock()
	if m, ok := s.sessionModels[key]; ok {
		return m
	}
	// Return the base model from args.
	for i, a := range s.args {
		if a == "--model" && i+1 < len(s.args) {
			return s.args[i+1]
		}
	}
	return "claude-cli"
}

func (s *StreamingCLIAgent) ModelHealth() []models.ModelHealthStatus {
	return []models.ModelHealthStatus{{
		Model:     s.ResolveModel(""),
		Provider:  "claude-cli",
		Available: true,
	}}
}

func (s *StreamingCLIAgent) GetUsage() *UsageTracker { return s.usage }

// ── core chat implementation ────────────────────────────────────────

func (s *StreamingCLIAgent) chatImpl(ctx context.Context, sessionKey, message string, imageURLs []string, cb *StreamCallbacks) (Response, error) {
	// Enrich the message with semantically recalled memories from previous conversations.
	enrichedMessage := s.enrichWithRecall(ctx, message)

	sess, err := s.getOrSpawn(sessionKey)
	if err != nil {
		return Response{}, err
	}

	// Serialise calls on the same session.
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.resetIdle(s.idleTTL)

	// Send the enriched message.
	input := cliInputMessage{
		Type:    "user",
		Message: cliInputContent{Role: "user", Content: enrichedMessage},
	}
	data, _ := json.Marshal(input)
	data = append(data, '\n')
	if _, err := sess.stdin.Write(data); err != nil {
		// Subprocess died; reap and retry once.
		sess.mu.Unlock()
		s.reap(sessionKey)
		sess2, err2 := s.getOrSpawn(sessionKey)
		if err2 != nil {
			sess.mu.Lock()
			return Response{}, fmt.Errorf("streaming-cli: respawn failed: %w", err2)
		}
		sess2.mu.Lock()
		defer sess2.mu.Unlock()
		if _, err := sess2.stdin.Write(data); err != nil {
			return Response{}, fmt.Errorf("streaming-cli: write after respawn: %w", err)
		}
		resp, err := s.readResponse(ctx, sessionKey, sess2, cb)
		if err == nil && resp.Text != "" {
			s.appendToEidetic(sessionKey, message, resp.Text)
		}
		return resp, err
	}

	resp, err := s.readResponse(ctx, sessionKey, sess, cb)
	if err != nil {
		// On read error, reap the broken session.
		s.reap(sessionKey)
		return resp, err
	}

	// Write conversation turn to eidetic memory (background, non-blocking).
	// Use original message (not enriched) to avoid storing recall prefixes.
	if resp.Text != "" {
		s.appendToEidetic(sessionKey, message, resp.Text)
	}
	return resp, err
}

// readResponse reads NDJSON events until a result event arrives.
// On the first call for a session it also consumes the system init event.
func (s *StreamingCLIAgent) readResponse(ctx context.Context, sessionKey string, sess *cliSession, cb *StreamCallbacks) (Response, error) {
	var fullText strings.Builder
	var lastToolName string
	var usage agentapi.ResponseUsage

	for {
		select {
		case <-ctx.Done():
			return Response{}, ctx.Err()
		default:
		}

		if !sess.stdout.Scan() {
			if err := sess.stdout.Err(); err != nil {
				return Response{}, fmt.Errorf("streaming-cli: scan: %w", err)
			}
			return Response{}, fmt.Errorf("streaming-cli: subprocess closed stdout")
		}

		var ev cliEvent
		if err := json.Unmarshal(sess.stdout.Bytes(), &ev); err != nil {
			s.logger.Debugf("streaming-cli: ignoring unparseable line: %s", sess.stdout.Text())
			continue
		}

		switch ev.Type {
		case "stream_event":
			s.handleStreamEvent(ev.Event, &fullText, &lastToolName, cb)

		case "assistant":
			// Complete assistant message — extract text if we haven't streamed it.
			if fullText.Len() == 0 {
				fullText.WriteString(s.extractAssistantText(ev.Message))
			}
			// Check for error field (auth failures, billing, etc.).
			if ev.Error != "" {
				return Response{}, fmt.Errorf("streaming-cli: assistant error: %s", ev.Error)
			}

		case "result":
			if ev.IsError {
				errMsg := "unknown error"
				if len(ev.Errors) > 0 {
					errMsg = strings.Join(ev.Errors, "; ")
				}
				return Response{}, fmt.Errorf("streaming-cli: %s: %s", ev.Subtype, errMsg)
			}
			// Use the result text as the final response if we didn't accumulate streaming text.
			text := fullText.String()
			if text == "" {
				text = ev.Result
			}
			if ev.Usage != nil {
				usage.InputTokens = ev.Usage.InputTokens
				usage.OutputTokens = ev.Usage.OutputTokens
			}
			s.usage.Accumulate(sessionKey, NormalizedUsage{
				Input:  usage.InputTokens,
				Output: usage.OutputTokens,
				Total:  usage.InputTokens + usage.OutputTokens,
			})
			return Response{
				Text:  strings.TrimSpace(text),
				Usage: usage,
			}, nil

		case "system":
			if ev.Subtype == "init" && !sess.initDone {
				sess.initDone = true
				s.logger.Debugf("streaming-cli: init received, session_id=%s", ev.SessionID)
			} else {
				s.logger.Debugf("streaming-cli: system event subtype=%s", ev.Subtype)
			}

		default:
			// rate_limit_event, tool_progress, etc.
			s.logger.Debugf("streaming-cli: event type=%s", ev.Type)
		}
	}
}

// handleStreamEvent processes a stream_event (partial message chunks).
func (s *StreamingCLIAgent) handleStreamEvent(raw json.RawMessage, fullText *strings.Builder, lastToolName *string, cb *StreamCallbacks) {
	if raw == nil {
		return
	}
	var ev streamEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return
	}

	switch ev.Type {
	case "content_block_start":
		var block contentBlock
		if err := json.Unmarshal(ev.ContentBlock, &block); err == nil {
			if block.Type == "tool_use" {
				*lastToolName = block.Name
				if cb != nil && cb.OnToolStart != nil {
					cb.OnToolStart(block.Name, "")
				}
			}
		}

	case "content_block_delta":
		var delta textDelta
		if err := json.Unmarshal(ev.Delta, &delta); err == nil && delta.Type == "text_delta" {
			fullText.WriteString(delta.Text)
			if cb != nil && cb.OnChunk != nil {
				cb.OnChunk(delta.Text)
			}
		}

	case "content_block_stop":
		if *lastToolName != "" {
			if cb != nil && cb.OnToolDone != nil {
				cb.OnToolDone(*lastToolName, "", nil)
			}
			*lastToolName = ""
		}
	}
}

// extractAssistantText pulls text content from a complete assistant message.
func (s *StreamingCLIAgent) extractAssistantText(raw json.RawMessage) string {
	if raw == nil {
		return ""
	}
	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ""
	}
	var parts []string
	for _, c := range msg.Content {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}
