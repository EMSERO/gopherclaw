package models

import (
	"context"

	openai "github.com/sashabaranov/go-openai"
)

// openaiProvider wraps *openai.Client to implement Provider.
// Used for GitHub Copilot, OpenAI, and any OpenAI-compatible API endpoint.
type openaiProvider struct {
	client *openai.Client
}

func (p *openaiProvider) Chat(ctx context.Context, req openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	return p.client.CreateChatCompletion(ctx, req)
}

func (p *openaiProvider) ChatStream(ctx context.Context, req openai.ChatCompletionRequest) (Stream, error) {
	return p.client.CreateChatCompletionStream(ctx, req)
}
