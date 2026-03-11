package tools

import (
	"context"
	"fmt"
	"sync"
)

// ChannelConfirmer can send a confirmation prompt on a specific channel bot.
type ChannelConfirmer interface {
	// CanConfirm returns true if this channel can reach the given session key.
	CanConfirm(sessionKey string) bool
	// SendConfirmPrompt sends an inline-button confirmation prompt and blocks
	// until the user responds or ctx expires.
	SendConfirmPrompt(ctx context.Context, sessionKey, command, pattern string) (bool, error)
}

// ConfirmManager routes confirmation requests to the appropriate channel bot.
// It satisfies the ExecConfirmer interface.
type ConfirmManager struct {
	mu        sync.RWMutex
	channels  []ChannelConfirmer
}

// NewConfirmManager creates a ConfirmManager.
func NewConfirmManager() *ConfirmManager {
	return &ConfirmManager{}
}

// AddChannel registers a channel bot as a confirmation handler.
func (m *ConfirmManager) AddChannel(c ChannelConfirmer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.channels = append(m.channels, c)
}

// RequestExecConfirm implements ExecConfirmer. It finds the first channel
// that can reach the session and sends a confirmation prompt.
func (m *ConfirmManager) RequestExecConfirm(ctx context.Context, sessionKey, command, pattern string) (bool, error) {
	m.mu.RLock()
	channels := make([]ChannelConfirmer, len(m.channels))
	copy(channels, m.channels)
	m.mu.RUnlock()

	for _, ch := range channels {
		if ch.CanConfirm(sessionKey) {
			return ch.SendConfirmPrompt(ctx, sessionKey, command, pattern)
		}
	}
	return false, fmt.Errorf("no channel can reach session %q for confirmation", sessionKey)
}
