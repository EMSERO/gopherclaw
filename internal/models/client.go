package models

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

// TokenProvider can fetch a fresh Copilot API token and report the API base URL.
type TokenProvider interface {
	GetToken(ctx context.Context) (string, error)
	APIURL() string
}

// copilotTransport injects the Copilot auth headers on every request
// and rewrites the host to match the API URL from the token.
type copilotTransport struct {
	base     http.RoundTripper
	provider TokenProvider
}

func (t *copilotTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := t.provider.GetToken(req.Context())
	if err != nil {
		return nil, fmt.Errorf("get copilot token: %w", err)
	}

	// Clone the request to avoid mutating the original
	r := req.Clone(req.Context())

	// Rewrite host to match the API URL from the current token.
	// This handles the case where the API URL changes between sessions.
	apiURL := t.provider.APIURL()
	if apiURL != "" {
		parsed, _ := url.Parse(apiURL)
		if parsed != nil && parsed.Host != "" {
			r.URL.Host = parsed.Host
			r.URL.Scheme = parsed.Scheme
			r.Host = parsed.Host
		}
	}

	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("Editor-Version", "vscode/1.85.1")
	r.Header.Set("Editor-Plugin-Version", "copilot-chat/0.12.2023120701")
	r.Header.Set("Openai-Intent", "conversation-edits")
	r.Header.Set("Copilot-Integration-Id", "vscode-chat")

	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

// newHTTP1Transport returns an http.Transport that forces HTTP/1.1 (no ALPN upgrade).
// The GitHub Copilot API times out on HTTP/2 connections in some environments.
func newHTTP1Transport() *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 60 * time.Second,
		}).DialContext,
		TLSClientConfig:     &tls.Config{},
		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   false,
		DisableKeepAlives:   false,
	}
}

// NewCopilotProvider creates a Provider backed by the GitHub Copilot API.
func NewCopilotProvider(provider TokenProvider) Provider {
	return &openaiProvider{client: NewCopilotClient(provider)}
}

// NewCopilotClient creates an OpenAI-compatible client pointed at GitHub Copilot.
// The Copilot API does NOT use a /v1 prefix — paths go directly on the domain.
// The transport rewrites the host dynamically from the token's endpoints.api field.
// HTTP/2 is disabled because the Copilot API may time out on HTTP/2 handshakes.
func NewCopilotClient(provider TokenProvider) *openai.Client {
	apiURL := provider.APIURL() // e.g. "https://api.enterprise.githubcopilot.com"

	httpClient := &http.Client{
		Transport: &copilotTransport{
			base:     newHTTP1Transport(),
			provider: provider,
		},
		Timeout: 5 * time.Minute,
	}
	cfg := openai.DefaultConfig("copilot") // token injected by transport
	cfg.BaseURL = apiURL                   // go-openai appends /chat/completions directly
	cfg.HTTPClient = httpClient
	return openai.NewClientWithConfig(cfg)
}

// ModelID strips the provider prefix from a model string.
// e.g. "github-copilot/claude-sonnet-4.6" → "claude-sonnet-4.6"
func ModelID(full string) string {
	if _, after, ok := strings.Cut(full, "/"); ok {
		return after
	}
	return full
}
