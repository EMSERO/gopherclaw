// Package common provides shared utilities used by all channel bot
// implementations (Telegram, Discord, Slack).
package common

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/agent"
	"github.com/EMSERO/gopherclaw/internal/commands"
	"github.com/EMSERO/gopherclaw/internal/config"
	"github.com/EMSERO/gopherclaw/internal/cron"
	"github.com/EMSERO/gopherclaw/internal/session"
	"github.com/EMSERO/gopherclaw/internal/taskqueue"
)

// GeneratePairCode returns a random 6-digit pairing code.
func GeneratePairCode() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "000000"
	}
	n := (int(b[0])<<16 | int(b[1])<<8 | int(b[2])) % 1_000_000
	return fmt.Sprintf("%06d", n)
}

// MatchesResetTrigger returns true if text (case-insensitive, trimmed)
// matches any of the configured reset trigger phrases.
func MatchesResetTrigger(text string, triggers []string) bool {
	lower := strings.TrimSpace(strings.ToLower(text))
	for _, t := range triggers {
		if strings.TrimSpace(strings.ToLower(t)) == lower {
			return true
		}
	}
	return false
}

// SplitMessage splits text into chunks of at most maxLen bytes,
// preferring to break at newline boundaries.
func SplitMessage(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}
	var parts []string
	for len(text) > maxLen {
		// Try to split on a newline near the limit
		cut := maxLen
		for i := maxLen; i > maxLen-200 && i > 0; i-- {
			if text[i] == '\n' {
				cut = i + 1
				break
			}
		}
		parts = append(parts, text[:cut])
		text = text[cut:]
	}
	if text != "" {
		parts = append(parts, text)
	}
	return parts
}

// SessionKey builds a deterministic session key from the agent ID,
// platform name, session scope, and user/channel identifiers.
func SessionKey(agentID, platform, scope, userID, channelID string) string {
	switch scope {
	case "channel":
		return fmt.Sprintf("%s:%s:channel:%s", agentID, platform, channelID)
	case "global":
		return fmt.Sprintf("%s:%s:global", agentID, platform)
	default: // "user" or unset
		return fmt.Sprintf("%s:%s:%s", agentID, platform, userID)
	}
}

// PairedUsersPath returns the filesystem path to the paired-users state
// file for the given platform (e.g. "telegram", "discord", "slack").
func PairedUsersPath(platform string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gopherclaw", "credentials", platform+"-default-allowFrom.json")
}

// pairedState is the JSON schema for the allowFrom file.
type pairedState struct {
	Version   int      `json:"version"`
	AllowFrom []string `json:"allowFrom"`
}

// LoadPairedUsers reads the paired-users state file for the given platform
// and returns a set of user ID strings. Returns an empty map on any error.
func LoadPairedUsers(logger *zap.SugaredLogger, platform string) map[string]bool {
	data, err := os.ReadFile(PairedUsersPath(platform))
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warnf("%s: failed to read paired users file: %v", platform, err)
		}
		return nil
	}
	var state pairedState
	if err := json.Unmarshal(data, &state); err != nil {
		logger.Warnf("%s: failed to parse paired users: %v", platform, err)
		return nil
	}
	m := make(map[string]bool, len(state.AllowFrom))
	for _, id := range state.AllowFrom {
		m[id] = true
	}
	return m
}

// SavePairedUsers writes the paired-users state file for the given platform.
func SavePairedUsers(logger *zap.SugaredLogger, platform string, ids []string) error {
	state := pairedState{Version: 1, AllowFrom: ids}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	path := PairedUsersPath(platform)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	logger.Infof("%s: saved %d paired users", platform, len(ids))
	return nil
}

// ValidatePairCode performs a constant-time comparison of the user-supplied
// pairing code against the expected code.  Using a single shared implementation
// ensures all channel bots use the same timing-safe check.
func ValidatePairCode(input, expected string) bool {
	trimmed := strings.TrimSpace(input)
	return subtle.ConstantTimeCompare([]byte(trimmed), []byte(expected)) == 1
}

// CombineTexts joins a slice of message texts with newline separators.
// All three channel bots use this pattern in processMessages.
func CombineTexts(texts []string) string {
	var sb strings.Builder
	for i, t := range texts {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(t)
	}
	return sb.String()
}

// CmdCtxDeps holds the shared bot dependencies needed to build a commands.Ctx.
// Each channel bot embeds or passes this to BuildCmdCtx.
type CmdCtxDeps struct {
	Agent       *agent.Agent
	Sessions    *session.Manager
	Config      *config.Root
	CronManager *cron.Manager
	TaskManager *taskqueue.Manager
	Version     string
	StartTime   time.Time
	SkillCount  int
}

// ──────────────────────────────────────────────────────────────────────
// ToolNotifier — throttles "🔧 tool…" messages across all channels
// ──────────────────────────────────────────────────────────────────────

const (
	// MaxIndividualToolNotifications is the number of tool-call
	// notifications sent individually before collapsing into summaries.
	MaxIndividualToolNotifications = 3
	// ToolNotifyInterval is the minimum time between batched updates
	// after the individual quota is exhausted.
	ToolNotifyInterval = 10 * time.Second
)

// ToolNotifier tracks tool-call count and throttles channel notifications.
// The first MaxIndividualToolNotifications calls are sent individually;
// subsequent calls produce a batched summary at most every ToolNotifyInterval.
type ToolNotifier struct {
	mu         sync.Mutex
	count      int
	lastNotify time.Time
	send       func(string) // channel-specific send function
}

// NewToolNotifier creates a ToolNotifier that emits via send.
func NewToolNotifier(send func(string)) *ToolNotifier {
	return &ToolNotifier{send: send}
}

// OnToolStart is intended to be used as the StreamCallbacks.OnToolStart callback.
func (tn *ToolNotifier) OnToolStart(name, _ string) {
	tn.mu.Lock()
	defer tn.mu.Unlock()
	tn.count++
	if tn.count <= MaxIndividualToolNotifications {
		tn.send("🔧 " + name + "…")
		tn.lastNotify = time.Now()
	} else if time.Since(tn.lastNotify) >= ToolNotifyInterval {
		tn.send(fmt.Sprintf("🔧 still working… (%d tools used)", tn.count))
		tn.lastNotify = time.Now()
	}
}

// ──────────────────────────────────────────────────────────────────────
// Response suppression — detects NO_REPLY / empty / whitespace responses
// ──────────────────────────────────────────────────────────────────────

// IsSuppressible returns true if text should be suppressed from delivery.
// A response is suppressible when it is empty, whitespace-only, exactly
// "NO_REPLY" (case-insensitive), or a bare ellipsis ("..." / "…").
func IsSuppressible(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return true
	}
	if strings.EqualFold(trimmed, "NO_REPLY") {
		return true
	}
	// Bare ellipsis (ASCII or Unicode)
	if trimmed == "..." || trimmed == "\u2026" {
		return true
	}
	return false
}

// UserFacingError returns a brief user-friendly error message that includes
// a hint about what went wrong (timeout, rate limit, API error, etc.).
func UserFacingError(err error) string {
	if err == nil {
		return "Sorry, something went wrong."
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "context deadline exceeded") || strings.Contains(s, "context canceled"):
		return "Sorry, the request timed out. Try again or use /reset to start a shorter conversation."
	case strings.Contains(s, "429") || strings.Contains(s, "rate limit") || strings.Contains(s, "Rate limit"):
		return "Sorry, I'm being rate-limited by the API. Please wait a moment and try again."
	case strings.Contains(s, "401") || strings.Contains(s, "403") || strings.Contains(s, "Unauthorized") || strings.Contains(s, "Forbidden"):
		return "Sorry, there's an authentication issue with the model API."
	case strings.Contains(s, "maximum context length") || strings.Contains(s, "context_length_exceeded") || strings.Contains(s, "too many tokens"):
		return "Sorry, the conversation is too long for the model. Use /reset to start fresh."
	case strings.Contains(s, "500") || strings.Contains(s, "502") || strings.Contains(s, "503"):
		return "Sorry, the model API returned a server error. Try again in a moment."
	default:
		return "Sorry, something went wrong."
	}
}

// BuildCmdCtx constructs a commands.Ctx from the shared deps and a session key.
// This ensures all channel bots populate the same fields consistently.
func BuildCmdCtx(sessionKey string, d CmdCtxDeps) commands.Ctx {
	ctx := commands.Ctx{
		SessionKey:  sessionKey,
		Agent:       d.Agent,
		Sessions:    d.Sessions,
		Config:      d.Config,
		CronManager: d.CronManager,
		TaskManager: d.TaskManager,
		Version:     d.Version,
		StartTime:   d.StartTime,
		SkillCount:  d.SkillCount,
	}
	if d.Config != nil {
		ctx.Fallbacks = d.Config.Agents.Defaults.Model.Fallbacks
	}
	return ctx
}
