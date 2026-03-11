// Package embeddings provides a thin wrapper around OpenAI-compatible
// /v1/embeddings endpoints for generating vector embeddings.
//
// It reuses the go-openai client library and supports any OpenAI-compatible
// provider (OpenAI, Ollama, GitHub Copilot, etc.).
package embeddings

import (
	"context"
	"fmt"
	"sync"

	openai "github.com/sashabaranov/go-openai"
)

// Config holds connection settings for the embeddings provider.
type Config struct {
	// Provider is the provider name (e.g. "ollama", "openai").
	// Used to look up base URL defaults when BaseURL is empty.
	Provider string

	// Model is the embedding model ID (e.g. "text-embedding-3-small",
	// "nomic-embed-text").
	Model string

	// BaseURL overrides the provider's default endpoint.
	BaseURL string

	// APIKey for authentication (empty = "no-key" for local providers).
	APIKey string

	// Dimensions optionally requests truncated embeddings (0 = model default).
	Dimensions int
}

// Client generates vector embeddings via an OpenAI-compatible API.
type Client struct {
	oai   *openai.Client
	model string
	dims  int
	mu    sync.Mutex // serializes calls to avoid flooding the provider
}

// New creates a new embeddings Client from cfg.
func New(cfg Config) *Client {
	apiKey := cfg.APIKey
	if apiKey == "" {
		apiKey = "no-key"
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL(cfg.Provider)
	}

	oaiCfg := openai.DefaultConfig(apiKey)
	oaiCfg.BaseURL = baseURL

	return &Client{
		oai:   openai.NewClientWithConfig(oaiCfg),
		model: cfg.Model,
		dims:  cfg.Dimensions,
	}
}

// Embed returns the embedding vector for a single text input.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := c.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("embeddings: empty response")
	}
	return vecs[0], nil
}

// EmbedBatch returns embedding vectors for multiple texts in a single API call.
func (c *Client) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	req := openai.EmbeddingRequest{
		Input: texts,
		Model: openai.EmbeddingModel(c.model),
	}
	// Some providers don't support the dimensions parameter at all,
	// so only set it when explicitly configured.
	// Note: go-openai EmbeddingRequest doesn't have a Dimensions field
	// in older versions; we'll handle this via the raw request if needed.

	c.mu.Lock()
	defer c.mu.Unlock()

	resp, err := c.oai.CreateEmbeddings(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("embeddings: %w", err)
	}

	vecs := make([][]float32, len(resp.Data))
	for i, d := range resp.Data {
		vecs[i] = d.Embedding
	}
	return vecs, nil
}

// defaultBaseURL returns the default base URL for known providers.
func defaultBaseURL(provider string) string {
	switch provider {
	case "ollama":
		return "http://localhost:11434/v1"
	case "openai":
		return "https://api.openai.com/v1"
	case "openrouter":
		return "https://openrouter.ai/api/v1"
	default:
		return "https://api.openai.com/v1"
	}
}
