package models

import (
	"log/slog"
	"sync"
	"time"
)

// Cooldown tracks per-model failure cooldowns using exponential backoff.
// When a model fails, it enters a cooldown period that doubles on each
// consecutive failure, up to a configurable maximum.
//
// Thread-safe.
type Cooldown struct {
	mu         sync.Mutex
	state      map[string]*cooldownEntry
	baseDelay  time.Duration // default 1 min
	maxDelay   time.Duration // default 1 hour
	multiplier float64       // default 5 (1m → 5m → 25m → 1h)
}

type cooldownEntry struct {
	until    time.Time
	failures int
}

// CooldownConfig configures cooldown behaviour.
type CooldownConfig struct {
	BaseDelay  time.Duration // initial cooldown after first failure (default 1m)
	MaxDelay   time.Duration // cap on cooldown duration (default 1h)
	Multiplier float64       // backoff multiplier (default 5)
}

// NewCooldown creates a Cooldown tracker with the given config.
func NewCooldown(cfg CooldownConfig) *Cooldown {
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = 1 * time.Minute
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = 1 * time.Hour
	}
	if cfg.Multiplier <= 0 {
		cfg.Multiplier = 5
	}
	return &Cooldown{
		state:      make(map[string]*cooldownEntry),
		baseDelay:  cfg.BaseDelay,
		maxDelay:   cfg.MaxDelay,
		multiplier: cfg.Multiplier,
	}
}

// RecordFailure marks a model as having failed, starting or extending its
// cooldown with exponential backoff.
func (c *Cooldown) RecordFailure(model string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.state[model]
	if !ok {
		e = &cooldownEntry{}
		c.state[model] = e
	}
	e.failures++

	delay := c.baseDelay
	for i := 1; i < e.failures; i++ {
		delay = time.Duration(float64(delay) * c.multiplier)
		if delay > c.maxDelay {
			delay = c.maxDelay
			break
		}
	}
	e.until = time.Now().Add(delay)

	slog.Info("model cooldown started",
		"model", model,
		"failures", e.failures,
		"cooldown", delay.String())
}

// RecordSuccess clears the cooldown state for a model.
func (c *Cooldown) RecordSuccess(model string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.state, model)
}

// GC removes expired cooldown entries to prevent unbounded map growth.
func (c *Cooldown) GC() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for model, e := range c.state {
		if now.After(e.until) {
			delete(c.state, model)
		}
	}
}

// IsAvailable returns true if the model is not in a cooldown period.
func (c *Cooldown) IsAvailable(model string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.state[model]
	if !ok {
		return true
	}
	if time.Now().After(e.until) {
		// Cooldown expired — keep the failure count for escalation if it
		// fails again, but allow the attempt.
		return true
	}
	return false
}

// CooldownRemaining returns the remaining cooldown duration for a model.
// Returns 0 if the model is available.
func (c *Cooldown) CooldownRemaining(model string) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.state[model]
	if !ok {
		return 0
	}
	remaining := time.Until(e.until)
	if remaining <= 0 {
		return 0
	}
	return remaining
}

// Reset clears the cooldown state for a specific model.
func (c *Cooldown) Reset(model string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.state, model)
}

// ResetAll clears all cooldown state.
func (c *Cooldown) ResetAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = make(map[string]*cooldownEntry)
}

// AvailableFrom returns the subset of models that are currently not in cooldown.
func (c *Cooldown) AvailableFrom(models []string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	out := make([]string, 0, len(models))
	for _, m := range models {
		e, ok := c.state[m]
		if !ok || now.After(e.until) {
			out = append(out, m)
		}
	}
	return out
}
