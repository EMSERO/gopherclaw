// Package heartbeat implements periodic agent turns that check a HEARTBEAT.md
// checklist and optionally deliver results to channel bots.
//
// The runner triggers an agent turn at a configurable interval, using a light
// system prompt (identity + HEARTBEAT.md only). If the agent replies with
// HEARTBEAT_OK (optionally followed by ≤ackMaxChars of text), the response is
// suppressed. Otherwise the response is delivered to the configured target.
package heartbeat

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/agentapi"
	"github.com/EMSERO/gopherclaw/internal/config"
	"github.com/EMSERO/gopherclaw/internal/memory"
)

// HeartbeatOK is the token that agents emit to indicate nothing needs attention.
const HeartbeatOK = "HEARTBEAT_OK"

// Chatter is the minimal agent interface needed by the heartbeat runner.
type Chatter interface {
	Chat(ctx context.Context, sessionKey, message string) (agentapi.Response, error)
	ChatLight(ctx context.Context, sessionKey, message string) (agentapi.Response, error)
	SetSessionModel(key, model string)
	ClearSessionModel(key string)
}

// Deliverer can broadcast a message to all paired users on a channel.
type Deliverer interface {
	SendToAllPaired(text string)
}

// Runner manages the periodic heartbeat loop.
type Runner struct {
	logger     *zap.SugaredLogger
	agent      Chatter
	cfg        func() config.HeartbeatConfig // returns current config (supports hot-reload)
	userTZ     func() string                 // returns current user timezone
	resolveAlias func(string) string         // resolves model alias to full ID
	workspace  string
	deliverers []Deliverer
	sessionKey string
}

// RunnerOpts configures a new Runner.
type RunnerOpts struct {
	Logger       *zap.SugaredLogger
	Agent        Chatter
	CfgFn        func() config.HeartbeatConfig // dynamic config getter
	UserTZFn     func() string                 // dynamic timezone getter
	ResolveAlias func(string) string           // model alias resolver
	Workspace    string
	Deliverers   []Deliverer
	SessionKey   string // session key for heartbeat turns (default "heartbeat:main")
}

// NewRunner creates a heartbeat runner.
func NewRunner(opts RunnerOpts) *Runner {
	sk := opts.SessionKey
	if sk == "" {
		sk = "heartbeat:main"
	}
	return &Runner{
		logger:       opts.Logger,
		agent:        opts.Agent,
		cfg:          opts.CfgFn,
		userTZ:       opts.UserTZFn,
		resolveAlias: opts.ResolveAlias,
		workspace:    opts.Workspace,
		deliverers:   opts.Deliverers,
		sessionKey:   sk,
	}
}

// Start runs the heartbeat loop until ctx is cancelled. It respects config
// changes (interval, active hours) on each tick.
func (r *Runner) Start(ctx context.Context) error {
	r.logger.Infof("heartbeat: runner started")
	defer r.logger.Infof("heartbeat: runner stopped")

	for {
		cfg := r.cfg()
		if !cfg.HeartbeatEnabled() {
			// Heartbeat disabled — sleep a bit and re-check config.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(1 * time.Minute):
				continue
			}
		}

		interval, err := time.ParseDuration(cfg.Every)
		if err != nil || interval <= 0 {
			r.logger.Warnf("heartbeat: invalid interval %q, defaulting to 30m", cfg.Every)
			interval = 30 * time.Minute
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
			r.tick(ctx, cfg)
		}
	}
}

// tick runs a single heartbeat cycle.
func (r *Runner) tick(ctx context.Context, cfg config.HeartbeatConfig) {
	// Check active hours.
	if cfg.ActiveHours != nil {
		tz := cfg.ActiveHours.Timezone
		if tz == "" || tz == "user" {
			tz = r.userTZ()
		}
		if !IsWithinActiveHours(cfg.ActiveHours.Start, cfg.ActiveHours.End, tz, time.Now()) {
			r.logger.Debugf("heartbeat: outside active hours, skipping")
			return
		}
	}

	// Check if HEARTBEAT.md has content (skip if effectively empty).
	if r.workspace != "" {
		content := memory.LoadHeartbeatMD(r.workspace)
		if isEffectivelyEmpty(content) {
			r.logger.Debugf("heartbeat: HEARTBEAT.md empty, skipping")
			return
		}
	}

	// Build prompt with current time.
	tz := r.userTZ()
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	prompt := fmt.Sprintf("%s\n\nCurrent time: %s (%s)", cfg.HeartbeatPrompt(), now.Format("2006-01-02 15:04:05"), tz)

	// Apply per-heartbeat model override.
	if cfg.Model != "" {
		model := cfg.Model
		if r.resolveAlias != nil {
			model = r.resolveAlias(model)
		}
		r.agent.SetSessionModel(r.sessionKey, model)
		defer r.agent.ClearSessionModel(r.sessionKey)
	}

	// Run agent turn with timeout.
	turnCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	var resp agentapi.Response
	if cfg.LightContext {
		resp, err = r.agent.ChatLight(turnCtx, r.sessionKey, prompt)
	} else {
		resp, err = r.agent.Chat(turnCtx, r.sessionKey, prompt)
	}
	if err != nil {
		r.logger.Errorf("heartbeat: agent turn failed: %v", err)
		return
	}

	// Process response: check for HEARTBEAT_OK suppression.
	text, shouldDeliver := StripHeartbeatToken(resp.Text, cfg.HeartbeatAckMaxChars())
	if !shouldDeliver {
		r.logger.Debugf("heartbeat: HEARTBEAT_OK — nothing to report")
		return
	}

	// Deliver to configured target.
	if cfg.Target == "none" || cfg.Target == "" {
		r.logger.Infof("heartbeat: response (target=none, not delivered): %s", truncate(text, 200))
		return
	}

	r.logger.Infof("heartbeat: delivering response (%d chars)", len(text))
	for _, d := range r.deliverers {
		d.SendToAllPaired(text)
	}
}

// StripHeartbeatToken checks if the response starts with HEARTBEAT_OK.
// Returns the cleaned text and whether it should be delivered.
func StripHeartbeatToken(raw string, maxAckChars int) (string, bool) {
	cleaned := strings.TrimSpace(raw)
	if cleaned == "" {
		return "", false
	}

	// Strip markdown formatting around the token.
	normalized := cleaned
	normalized = strings.TrimPrefix(normalized, "**")
	normalized = strings.TrimSuffix(normalized, "**")
	normalized = strings.TrimPrefix(normalized, "`")
	normalized = strings.TrimSuffix(normalized, "`")
	normalized = strings.TrimSpace(normalized)

	// Check if starts with HEARTBEAT_OK.
	if !strings.HasPrefix(normalized, HeartbeatOK) {
		return cleaned, true // no token — deliver as-is
	}

	// Strip the token and check remaining text.
	rest := strings.TrimSpace(normalized[len(HeartbeatOK):])

	// Strip trailing punctuation after token (e.g. "HEARTBEAT_OK!!!")
	rest = strings.TrimLeft(rest, "!.,-—–")
	rest = strings.TrimSpace(rest)

	if len(rest) <= maxAckChars {
		return "", false // suppressed
	}

	// Remaining text exceeds maxAckChars — deliver the overflow.
	return rest, true
}

// isEffectivelyEmpty returns true if the content is empty or contains only
// whitespace, markdown headers, and empty list items.
func isEffectivelyEmpty(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// Skip markdown headers
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Skip empty list items
		if trimmed == "-" || trimmed == "- [ ]" || trimmed == "- [x]" || trimmed == "*" {
			continue
		}
		// Skip HTML/markdown comments
		if strings.HasPrefix(trimmed, "<!--") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		// Found substantive content.
		return false
	}
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
