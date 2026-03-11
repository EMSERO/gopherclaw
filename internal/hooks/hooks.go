// Package hooks provides an event-driven lifecycle hook system for GopherClaw.
//
// Hooks allow external plugins and internal subsystems to react to key events
// in the agent lifecycle (model resolution, prompt building, tool calls,
// message delivery, session management, gateway events).
//
// Usage:
//
//	bus := hooks.New()
//	bus.On(hooks.BeforeModelResolve, func(ctx context.Context, e hooks.Event) error {
//	    log.Infof("resolving model for session %s", e.SessionKey)
//	    return nil
//	})
//	bus.Emit(ctx, hooks.Event{Type: hooks.BeforeModelResolve, SessionKey: key})
package hooks

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// EventType identifies the lifecycle event.
type EventType string

const (
	// Model lifecycle
	BeforeModelResolve EventType = "before_model_resolve"
	AfterModelResolve  EventType = "after_model_resolve"

	// Prompt lifecycle
	BeforePromptBuild EventType = "before_prompt_build"
	AfterPromptBuild  EventType = "after_prompt_build"

	// Tool lifecycle
	BeforeToolCall EventType = "before_tool_call"
	AfterToolCall  EventType = "after_tool_call"

	// Message lifecycle
	BeforeMessageSend  EventType = "before_message_send"
	AfterMessageSend   EventType = "after_message_send"
	BeforeMessageStore EventType = "before_message_store"

	// Session lifecycle
	SessionCreated EventType = "session_created"
	SessionReset   EventType = "session_reset"
	SessionPruned  EventType = "session_pruned"

	// Gateway lifecycle
	GatewayStarted EventType = "gateway_started"
	GatewayStopped EventType = "gateway_stopped"
)

// Event carries data for a lifecycle hook invocation.
type Event struct {
	Type       EventType
	SessionKey string
	AgentID    string
	Model      string
	ToolName   string
	Channel    string         // "telegram", "discord", "slack", "gateway"
	Data       map[string]any // extensible payload
	Timestamp  time.Time
}

// Handler is a function called when an event fires.
// Returning an error logs a warning but does not abort the operation.
type Handler func(ctx context.Context, e Event) error

// Bus is the central event dispatcher. Thread-safe.
type Bus struct {
	mu       sync.RWMutex
	handlers map[EventType][]namedHandler
	logger   *zap.SugaredLogger
}

type namedHandler struct {
	name string
	fn   Handler
}

// New creates an empty hook bus.
func New(logger *zap.SugaredLogger) *Bus {
	return &Bus{handlers: make(map[EventType][]namedHandler), logger: logger}
}

// On registers a handler for a specific event type.
// name is used for logging and debugging; it need not be unique.
func (b *Bus) On(eventType EventType, name string, fn Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[eventType] = append(b.handlers[eventType], namedHandler{name: name, fn: fn})
}

// Off removes all handlers with the given name from a specific event type.
func (b *Bus) Off(eventType EventType, name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	handlers := b.handlers[eventType]
	filtered := make([]namedHandler, 0, len(handlers))
	for _, h := range handlers {
		if h.name != name {
			filtered = append(filtered, h)
		}
	}
	b.handlers[eventType] = filtered
}

// Emit dispatches an event to all registered handlers synchronously.
// Each handler is called in registration order. Errors are logged but
// do not prevent subsequent handlers from running.
func (b *Bus) Emit(ctx context.Context, e Event) {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}

	b.mu.RLock()
	handlers := make([]namedHandler, len(b.handlers[e.Type]))
	copy(handlers, b.handlers[e.Type])
	b.mu.RUnlock()

	for _, h := range handlers {
		if err := h.fn(ctx, e); err != nil {
			b.logger.Warnf("hook handler error: event=%s handler=%s err=%v", e.Type, h.name, err)
		}
	}
}

// EmitAsync dispatches an event to all registered handlers in parallel.
// Errors are logged. The method blocks until all handlers complete or
// the context is cancelled.
func (b *Bus) EmitAsync(ctx context.Context, e Event) {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}

	b.mu.RLock()
	handlers := make([]namedHandler, len(b.handlers[e.Type]))
	copy(handlers, b.handlers[e.Type])
	b.mu.RUnlock()

	if len(handlers) == 0 {
		return
	}

	var wg sync.WaitGroup
	wg.Add(len(handlers))
	for _, h := range handlers {
		go func(h namedHandler) {
			defer wg.Done()
			if err := h.fn(ctx, e); err != nil {
				b.logger.Warnf("hook handler error (async): event=%s handler=%s err=%v", e.Type, h.name, err)
			}
		}(h)
	}
	wg.Wait()
}

// HasHandlers returns true if at least one handler is registered for the event.
func (b *Bus) HasHandlers(eventType EventType) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.handlers[eventType]) > 0
}

// HandlerCount returns the number of handlers registered for an event type.
func (b *Bus) HandlerCount(eventType EventType) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.handlers[eventType])
}
