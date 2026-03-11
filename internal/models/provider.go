package models

import (
	"context"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

// Stream is a streaming chat completion iterator.
// *openai.ChatCompletionStream satisfies this interface.
type Stream interface {
	Recv() (openai.ChatCompletionStreamResponse, error)
	Close() error
}

// StreamUsage is an optional interface that streaming providers can implement
// to report token usage captured from the stream (e.g. Anthropic message_start
// and message_delta events).
type StreamUsage interface {
	Usage() (inputTokens, outputTokens int)
}

// ThinkingStream is an optional interface that streaming providers can implement
// to surface extended thinking deltas (e.g. Anthropic thinking_delta events).
type ThinkingStream interface {
	SetThinkingCallback(func(text string))
}

// cancelOnCloseStream wraps a Stream and calls a context cancel function when
// Close is called, preventing the stream's context from leaking.
type cancelOnCloseStream struct {
	Stream
	cancel context.CancelFunc
}

func (s *cancelOnCloseStream) Close() error {
	err := s.Stream.Close()
	s.cancel()
	return err
}

// Usage delegates to the underlying stream if it implements StreamUsage.
func (s *cancelOnCloseStream) Usage() (int, int) {
	if su, ok := s.Stream.(StreamUsage); ok {
		return su.Usage()
	}
	return 0, 0
}

// SetThinkingCallback delegates to the underlying stream if it implements ThinkingStream.
func (s *cancelOnCloseStream) SetThinkingCallback(cb func(string)) {
	if ts, ok := s.Stream.(ThinkingStream); ok {
		ts.SetThinkingCallback(cb)
	}
}

// Provider can make chat completion requests to a model API backend.
type Provider interface {
	Chat(ctx context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error)
	ChatStream(ctx context.Context, req openai.ChatCompletionRequest) (Stream, error)
}

// ThinkingConfig controls extended thinking for providers that support it (Anthropic).
type ThinkingConfig struct {
	Enabled      bool
	BudgetTokens int
	Level        string // "off", "enabled", "adaptive"; empty = use Enabled field
}

// splitModel splits "provider/model-id" into ("provider", "model-id").
// If there is no "/" separator, returns ("github-copilot", fullModel) for backward compatibility.
func splitModel(fullModel string) (providerID, modelID string) {
	providerID, modelID, ok := strings.Cut(fullModel, "/")
	if !ok {
		return "github-copilot", fullModel
	}
	return providerID, modelID
}

// knownBaseURLs maps provider names to their default OpenAI-compatible base URLs.
var knownBaseURLs = map[string]string{
	"openai":     "https://api.openai.com/v1",
	"groq":       "https://api.groq.com/openai/v1",
	"openrouter": "https://openrouter.ai/api/v1",
	"mistral":    "https://api.mistral.ai/v1",
	"together":   "https://api.together.xyz/v1",
	"fireworks":  "https://api.fireworks.ai/inference/v1",
	"perplexity": "https://api.perplexity.ai",
	"gemini":     "https://generativelanguage.googleapis.com/v1beta/openai",
	"ollama":     "http://localhost:11434/v1",
	"lmstudio":   "http://localhost:1234/v1",
}

// DefaultBaseURL returns the known base URL for a provider name, or empty string if unknown.
func DefaultBaseURL(name string) string {
	return knownBaseURLs[name]
}
