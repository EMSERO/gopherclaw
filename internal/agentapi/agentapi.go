// Package agentapi defines the shared interfaces and types used across the
// agent, tools, orchestrator, gateway, and cron packages.  Centralising them
// here eliminates interface duplication and the adapter shims that were
// previously needed to bridge identical-but-separate type definitions.
package agentapi

import (
	"context"
	"encoding/json"
)

// Tool is the interface each tool must satisfy.
type Tool interface {
	Name() string
	Schema() json.RawMessage
	Run(ctx context.Context, argsJSON string) string
}

// Describer is an optional interface that tools can implement to provide a
// human-readable description. The description is passed to the LLM in the
// function definition so it can make better tool-selection decisions.
type Describer interface {
	Description() string
}

// ResponseUsage holds token usage for a response.
type ResponseUsage struct {
	InputTokens  int
	OutputTokens int
}

// Response holds the final result of a conversation turn.
type Response struct {
	Text    string
	Stopped bool // true if max iterations reached
	Usage   ResponseUsage
}

// Chatter is the minimal interface required to participate in delegation
// and orchestration.  Both *Agent and *CLIAgent satisfy it.
type Chatter interface {
	Chat(ctx context.Context, sessionKey, message string) (Response, error)
}

// Announcer delivers async subagent results back to a session's channel.
type Announcer interface {
	AnnounceToSession(sessionKey, text string)
}

// Deliverer delivers messages to paired users across channels.
type Deliverer interface {
	SendToAllPaired(text string)
}

// SessionKeyContextKey is the context key for passing the active session key
// to tools at execution time.
type SessionKeyContextKey struct{}

// ConfirmRequest describes a dangerous command that needs user approval.
type ConfirmRequest struct {
	SessionKey string
	Command    string
	Pattern    string // the matched dangerous pattern
	ResultCh   chan bool
}

// Confirmer can present a confirmation prompt to the user and return the result.
type Confirmer interface {
	// RequestConfirm sends a confirmation prompt to the user on the active channel.
	// Returns true if the user confirms, false if denied or timed out.
	RequestConfirm(ctx context.Context, req ConfirmRequest) (bool, error)
}
