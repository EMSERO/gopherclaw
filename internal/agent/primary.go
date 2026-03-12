package agent

import (
	"context"

	"github.com/EMSERO/gopherclaw/internal/models"
)

// PrimaryAgent is the interface satisfied by both *Agent (router-backed) and
// *StreamingCLIAgent (claude-cli-backed).  All channel bots, the gateway, and
// the command handler accept this interface instead of a concrete *Agent.
type PrimaryAgent interface {
	Chat(ctx context.Context, sessionKey, message string) (Response, error)
	ChatStream(ctx context.Context, sessionKey, message string, cb *StreamCallbacks) (Response, error)
	ChatWithImages(ctx context.Context, sessionKey, caption string, imageURLs []string) (Response, error)
	ChatLight(ctx context.Context, sessionKey, message string) (Response, error)

	Compact(ctx context.Context, sessionKey, instructions string) error

	SetSessionModel(key, model string)
	ClearSessionModel(key string)
	ResolveModel(key string) string

	ModelHealth() []models.ModelHealthStatus
	GetUsage() *UsageTracker
}

// Compile-time check: *Agent satisfies PrimaryAgent.
var _ PrimaryAgent = (*Agent)(nil)
