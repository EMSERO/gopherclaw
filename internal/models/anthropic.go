package models

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

const (
	anthropicAPIBase        = "https://api.anthropic.com/v1"
	anthropicVersion        = "2023-06-01"
	anthropicDefaultMaxToks = 16384
)

// anthropicProvider implements Provider using Anthropic's native messages API.
type anthropicProvider struct {
	apiKey   string
	thinking ThinkingConfig
	client   *http.Client
}

func newAnthropicProvider(apiKey string, thinking ThinkingConfig) *anthropicProvider {
	return &anthropicProvider{
		apiKey:   apiKey,
		thinking: thinking,
		client:   &http.Client{Timeout: 10 * time.Minute},
	}
}

// ── internal Anthropic request/response types ──────────────────────────────

type anthropicRequest struct {
	Model     string            `json:"model"`
	MaxTokens int               `json:"max_tokens"`
	System    json.RawMessage   `json:"system,omitempty"`
	Messages  []anthropicMsg    `json:"messages"`
	Tools     []anthropicTool   `json:"tools,omitempty"`
	Stream    bool              `json:"stream,omitempty"`
	Thinking  *antThinking      `json:"thinking,omitempty"`
}

// anthropicMsg.Content is json.RawMessage to hold either a string or []block.
type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      string          `json:"content,omitempty"`
	CacheControl *cacheControl   `json:"cache_control,omitempty"`
}

type cacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type antThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type anthropicResponse struct {
	ID         string           `json:"id"`
	Type       string           `json:"type"`
	Role       string           `json:"role"`
	Content    []anthropicBlock `json:"content"`
	Model      string           `json:"model"`
	StopReason string           `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type anthropicAPIError struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// newAnthropicAPIError builds a structured APIError from an HTTP response,
// parsing the Retry-After header and the JSON error body.
func newAnthropicAPIError(resp *http.Response, body []byte) *APIError {
	msg := fmt.Sprintf("anthropic %d: %s", resp.StatusCode, string(body))
	var parsed anthropicAPIError
	if json.Unmarshal(body, &parsed) == nil && parsed.Error.Message != "" {
		msg = fmt.Sprintf("anthropic %d: %s", resp.StatusCode, parsed.Error.Message)
	}
	return &APIError{
		StatusCode: resp.StatusCode,
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		Message:    msg,
	}
}

// parseRetryAfter parses a Retry-After header value (seconds or HTTP-date).
// Returns 0 if absent or unparseable.
func parseRetryAfter(val string) time.Duration {
	if val == "" {
		return 0
	}
	// Try seconds first.
	if secs, err := strconv.Atoi(val); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	// Try HTTP-date (RFC1123).
	if t, err := time.Parse(time.RFC1123, val); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}

// ── Chat (non-streaming) ───────────────────────────────────────────────────

func (p *anthropicProvider) Chat(ctx context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	ar, err := p.buildRequest(req, false)
	if err != nil {
		return openai.ChatCompletionResponse{}, err
	}
	return p.doChat(ctx, ar)
}

func (p *anthropicProvider) doChat(ctx context.Context, ar anthropicRequest) (openai.ChatCompletionResponse, error) {
	body, err := json.Marshal(ar)
	if err != nil {
		return openai.ChatCompletionResponse{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", anthropicAPIBase+"/messages", bytes.NewReader(body))
	if err != nil {
		return openai.ChatCompletionResponse{}, err
	}
	p.setHeaders(httpReq, false)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return openai.ChatCompletionResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10 MB limit
	if err != nil {
		return openai.ChatCompletionResponse{}, err
	}
	if resp.StatusCode != http.StatusOK {
		// Retry once with thinking disabled if the provider rejected the thinking level.
		// Guard: ar.Thinking is set to nil before retry, so recursion depth is at most 1.
		if ar.Thinking != nil && isThinkingRejection(resp.StatusCode, data) {
			ar.Thinking = nil
			return p.doChat(ctx, ar)
		}
		return openai.ChatCompletionResponse{}, newAnthropicAPIError(resp, data)
	}

	var ar2 anthropicResponse
	if err := json.Unmarshal(data, &ar2); err != nil {
		return openai.ChatCompletionResponse{}, fmt.Errorf("parse anthropic response: %w", err)
	}
	return p.convertResponse(ar2), nil
}

// ── ChatStream ─────────────────────────────────────────────────────────────

func (p *anthropicProvider) ChatStream(ctx context.Context, req openai.ChatCompletionRequest) (Stream, error) {
	ar, err := p.buildRequest(req, true)
	if err != nil {
		return nil, err
	}
	return p.doChatStream(ctx, ar)
}

func (p *anthropicProvider) doChatStream(ctx context.Context, ar anthropicRequest) (Stream, error) {
	body, err := json.Marshal(ar)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", anthropicAPIBase+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	p.setHeaders(httpReq, true)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB limit
		_ = resp.Body.Close()
		// Retry once with thinking disabled if the provider rejected the thinking level.
		// Guard: ar.Thinking is set to nil before retry, so recursion depth is at most 1.
		if ar.Thinking != nil && isThinkingRejection(resp.StatusCode, data) {
			ar.Thinking = nil
			return p.doChatStream(ctx, ar)
		}
		return nil, newAnthropicAPIError(resp, data)
	}

	return &anthropicStream{
		reader:       bufio.NewReader(resp.Body),
		body:         resp.Body,
		blockToolIdx: make(map[int]int),
		blockType:    make(map[int]string),
	}, nil
}

// ── helpers ────────────────────────────────────────────────────────────────

func (p *anthropicProvider) setHeaders(req *http.Request, stream bool) {
	req.Header.Set("x-api-key", p.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("anthropic-beta", "prompt-caching-2024-07-31")
	req.Header.Set("content-type", "application/json")
	if stream {
		req.Header.Set("accept", "text/event-stream")
	}
}

// shouldThink returns true if thinking should be enabled for the given model.
// Priority: explicit Level > legacy Enabled field > per-model defaults (Claude 4.6 → adaptive).
func (p *anthropicProvider) shouldThink(model string) bool {
	level := p.thinking.Level
	if level != "" {
		return level == "enabled" || level == "adaptive"
	}
	// Legacy: use Enabled field
	if p.thinking.Enabled {
		return true
	}
	// Per-model defaults: Claude 4.6 models default to adaptive thinking
	if strings.Contains(model, "claude") && strings.Contains(model, "4-6") {
		return true
	}
	return false
}

func (p *anthropicProvider) buildRequest(req openai.ChatCompletionRequest, stream bool) (anthropicRequest, error) {
	system, msgs, err := convertToAnthropicMessages(req.Messages)
	if err != nil {
		return anthropicRequest{}, err
	}

	maxToks := anthropicDefaultMaxToks
	if req.MaxTokens > 0 {
		maxToks = req.MaxTokens
	}

	// Encode system prompt as a block array with cache_control for prompt caching.
	var systemJSON json.RawMessage
	if system != "" {
		sysBlocks := []anthropicBlock{{
			Type:         "text",
			Text:         system,
			CacheControl: &cacheControl{Type: "ephemeral"},
		}}
		systemJSON, _ = json.Marshal(sysBlocks)
	}

	ar := anthropicRequest{
		Model:     req.Model,
		MaxTokens: maxToks,
		System:    systemJSON,
		Messages:  msgs,
		Stream:    stream,
	}
	if len(req.Tools) > 0 {
		ar.Tools = convertToAnthropicTools(req.Tools)
	}
	if p.shouldThink(req.Model) {
		budget := p.thinking.BudgetTokens
		if budget <= 0 {
			budget = 8192
		}
		if maxToks <= budget {
			maxToks = budget + 4096
			ar.MaxTokens = maxToks
		}
		ar.Thinking = &antThinking{Type: "enabled", BudgetTokens: budget}
	}
	return ar, nil
}

// isThinkingRejection returns true if the API error indicates the model does
// not support the requested thinking level.
func isThinkingRejection(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	var apiErr anthropicAPIError
	if json.Unmarshal(body, &apiErr) != nil {
		return false
	}
	msg := strings.ToLower(apiErr.Error.Message)
	return strings.Contains(msg, "thinking") || strings.Contains(msg, "think")
}

// convertToAnthropicMessages converts the OpenAI message list to Anthropic format.
// System messages are extracted into the returned string.
// Consecutive "tool" role messages are batched into a single user message.
func convertToAnthropicMessages(msgs []openai.ChatCompletionMessage) (system string, result []anthropicMsg, err error) {
	var pending []anthropicBlock // pending tool_result blocks

	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		content, e := json.Marshal(pending)
		if e != nil {
			return e
		}
		result = append(result, anthropicMsg{Role: "user", Content: content})
		pending = nil
		return nil
	}

	for _, m := range msgs {
		switch m.Role {
		case "system":
			system = m.Content

		case "user":
			if err = flush(); err != nil {
				return
			}
			content, e := json.Marshal([]anthropicBlock{{Type: "text", Text: m.Content}})
			if e != nil {
				err = e
				return
			}
			result = append(result, anthropicMsg{Role: "user", Content: content})

		case "assistant":
			if err = flush(); err != nil {
				return
			}
			if len(m.ToolCalls) > 0 {
				var blocks []anthropicBlock
				if m.Content != "" {
					blocks = append(blocks, anthropicBlock{Type: "text", Text: m.Content})
				}
				for _, tc := range m.ToolCalls {
					input := json.RawMessage(tc.Function.Arguments)
					if len(input) == 0 {
						input = json.RawMessage("{}")
					}
					blocks = append(blocks, anthropicBlock{
						Type:  "tool_use",
						ID:    tc.ID,
						Name:  tc.Function.Name,
						Input: input,
					})
				}
				content, e := json.Marshal(blocks)
				if e != nil {
					err = e
					return
				}
				result = append(result, anthropicMsg{Role: "assistant", Content: content})
			} else {
				content, e := json.Marshal([]anthropicBlock{{Type: "text", Text: m.Content}})
				if e != nil {
					err = e
					return
				}
				result = append(result, anthropicMsg{Role: "assistant", Content: content})
			}

		case "tool":
			pending = append(pending, anthropicBlock{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   m.Content,
			})
		}
	}
	err = flush()
	return
}

func convertToAnthropicTools(tools []openai.Tool) []anthropicTool {
	result := make([]anthropicTool, 0, len(tools))
	for _, t := range tools {
		if t.Function == nil {
			continue
		}
		// FunctionDefinition.Parameters is typed as any in go-openai.
		var schema json.RawMessage
		switch p := t.Function.Parameters.(type) {
		case json.RawMessage:
			schema = p
		case []byte:
			schema = p
		default:
			if t.Function.Parameters != nil {
				schema, _ = json.Marshal(t.Function.Parameters)
			}
		}
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		result = append(result, anthropicTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
		})
	}
	return result
}

func (p *anthropicProvider) convertResponse(ar anthropicResponse) openai.ChatCompletionResponse {
	var text strings.Builder
	var toolCalls []openai.ToolCall
	for _, block := range ar.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			args := "{}"
			if len(block.Input) > 0 {
				args = string(block.Input)
			}
			toolCalls = append(toolCalls, openai.ToolCall{
				ID:   block.ID,
				Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{
					Name:      block.Name,
					Arguments: args,
				},
			})
			// "thinking" blocks: not surfaced to the agent loop
		}
	}

	finishReason := openai.FinishReasonStop
	switch ar.StopReason {
	case "tool_use":
		finishReason = openai.FinishReasonToolCalls
	case "max_tokens":
		finishReason = openai.FinishReasonLength
	}

	msg := openai.ChatCompletionMessage{Role: "assistant", Content: text.String()}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}
	return openai.ChatCompletionResponse{
		ID:    ar.ID,
		Model: ar.Model,
		Choices: []openai.ChatCompletionChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: finishReason,
		}},
		Usage: openai.Usage{
			PromptTokens:     ar.Usage.InputTokens,
			CompletionTokens: ar.Usage.OutputTokens,
			TotalTokens:      ar.Usage.InputTokens + ar.Usage.OutputTokens,
		},
	}
}

// ── anthropicStream: SSE streaming ────────────────────────────────────────

type anthropicStream struct {
	reader       *bufio.Reader
	body         io.ReadCloser
	done         bool
	blockType    map[int]string // block index → "text" | "tool_use" | "thinking"
	blockToolIdx map[int]int    // block index → tool call index (among tool_use blocks only)
	toolCount    int
	inputTokens  int // captured from message_start
	outputTokens int // captured from message_delta
	thinkingCb   func(string)
}

func (s *anthropicStream) Close() error {
	return s.body.Close()
}

// SetThinkingCallback registers a callback for thinking_delta events.
func (s *anthropicStream) SetThinkingCallback(cb func(string)) {
	s.thinkingCb = cb
}

// Usage returns the token usage captured from message_start and message_delta events.
func (s *anthropicStream) Usage() (inputTokens, outputTokens int) {
	return s.inputTokens, s.outputTokens
}

func (s *anthropicStream) Recv() (openai.ChatCompletionStreamResponse, error) {
	if s.done {
		return openai.ChatCompletionStreamResponse{}, io.EOF
	}
	for {
		chunk, hasChunk, isDone, err := s.readNext()
		if err != nil {
			return openai.ChatCompletionStreamResponse{}, err
		}
		if isDone {
			s.done = true
			return openai.ChatCompletionStreamResponse{}, io.EOF
		}
		if hasChunk {
			return chunk, nil
		}
		// no output from this event — read the next one
	}
}

// readNext reads one complete SSE event and delegates to processEvent.
func (s *anthropicStream) readNext() (openai.ChatCompletionStreamResponse, bool, bool, error) {
	var eventType, dataLine string
	for {
		line, err := s.reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")

		if after, ok := strings.CutPrefix(line, "event: "); ok {
			eventType = after
		} else if after, ok := strings.CutPrefix(line, "data: "); ok {
			dataLine = after
		} else if line == "" {
			// Blank line signals end of SSE event block.
			if dataLine != "" {
				break
			}
		}

		if err != nil {
			if err == io.EOF {
				if dataLine != "" {
					break
				}
				return openai.ChatCompletionStreamResponse{}, false, true, nil
			}
			return openai.ChatCompletionStreamResponse{}, false, false, err
		}
	}
	if dataLine == "" {
		return openai.ChatCompletionStreamResponse{}, false, false, nil
	}
	return s.processEvent(eventType, dataLine)
}

func (s *anthropicStream) processEvent(eventType, dataJSON string) (openai.ChatCompletionStreamResponse, bool, bool, error) {
	switch eventType {
	case "message_stop":
		return openai.ChatCompletionStreamResponse{}, false, true, nil

	case "content_block_start":
		var ev struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal([]byte(dataJSON), &ev); err != nil {
			return openai.ChatCompletionStreamResponse{}, false, false, nil
		}
		s.blockType[ev.Index] = ev.ContentBlock.Type
		if ev.ContentBlock.Type == "tool_use" {
			toolIdx := s.toolCount
			s.blockToolIdx[ev.Index] = toolIdx
			s.toolCount++
			chunk := openai.ChatCompletionStreamResponse{
				Choices: []openai.ChatCompletionStreamChoice{{
					Index: 0,
					Delta: openai.ChatCompletionStreamChoiceDelta{
						ToolCalls: []openai.ToolCall{{
							Index: &toolIdx,
							ID:    ev.ContentBlock.ID,
							Type:  openai.ToolTypeFunction,
							Function: openai.FunctionCall{
								Name: ev.ContentBlock.Name,
							},
						}},
					},
				}},
			}
			return chunk, true, false, nil
		}
		return openai.ChatCompletionStreamResponse{}, false, false, nil

	case "content_block_delta":
		var ev struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(dataJSON), &ev); err != nil {
			return openai.ChatCompletionStreamResponse{}, false, false, nil
		}
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text == "" {
				return openai.ChatCompletionStreamResponse{}, false, false, nil
			}
			chunk := openai.ChatCompletionStreamResponse{
				Choices: []openai.ChatCompletionStreamChoice{{
					Index: 0,
					Delta: openai.ChatCompletionStreamChoiceDelta{
						Content: ev.Delta.Text,
					},
				}},
			}
			return chunk, true, false, nil

		case "input_json_delta":
			toolIdx, ok := s.blockToolIdx[ev.Index]
			if !ok {
				return openai.ChatCompletionStreamResponse{}, false, false, nil
			}
			idx := toolIdx
			chunk := openai.ChatCompletionStreamResponse{
				Choices: []openai.ChatCompletionStreamChoice{{
					Index: 0,
					Delta: openai.ChatCompletionStreamChoiceDelta{
						ToolCalls: []openai.ToolCall{{
							Index: &idx,
							Function: openai.FunctionCall{
								Arguments: ev.Delta.PartialJSON,
							},
						}},
					},
				}},
			}
			return chunk, true, false, nil

		case "thinking_delta":
			if s.thinkingCb != nil && ev.Delta.Text != "" {
				s.thinkingCb(ev.Delta.Text)
			}
			return openai.ChatCompletionStreamResponse{}, false, false, nil
		}
		return openai.ChatCompletionStreamResponse{}, false, false, nil

	case "message_start":
		// Capture input token usage from the initial message event.
		var ev struct {
			Message struct {
				Usage struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(dataJSON), &ev) == nil {
			s.inputTokens = ev.Message.Usage.InputTokens
		}
		return openai.ChatCompletionStreamResponse{}, false, false, nil

	case "message_delta":
		// Capture output token usage from the final delta event.
		var ev struct {
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(dataJSON), &ev) == nil {
			s.outputTokens = ev.Usage.OutputTokens
		}
		return openai.ChatCompletionStreamResponse{}, false, false, nil

	case "error":
		// Anthropic sends an "error" event for mid-stream failures (rate limit, overload, etc.).
		var ev struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(dataJSON), &ev) == nil && ev.Error.Message != "" {
			return openai.ChatCompletionStreamResponse{}, false, false, fmt.Errorf("anthropic stream error: %s: %s", ev.Error.Type, ev.Error.Message)
		}
		return openai.ChatCompletionStreamResponse{}, false, false, fmt.Errorf("anthropic stream error: %s", dataJSON)

	default:
		// content_block_stop, ping, etc. — no output
		return openai.ChatCompletionStreamResponse{}, false, false, nil
	}
}
