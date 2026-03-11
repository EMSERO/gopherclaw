// Package agent — usage.go implements normalized token usage tracking (REQ-420).
//
// After each model API call the raw provider response is normalized into a
// standard 5-field struct (Input, Output, CacheRead, CacheWrite, Total) and
// accumulated per-session in memory.  The tracker exposes per-session and
// aggregate queries for the dashboard (REQ-302) and WS status feed.
package agent

import (
	"sync"
)

// NormalizedUsage is a provider-agnostic token usage record.
type NormalizedUsage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
	Total      int `json:"total"`
}

// SessionUsage tracks cumulative token usage for a single session.
type SessionUsage struct {
	mu         sync.Mutex
	Cumulative NormalizedUsage `json:"cumulative"`
	Calls      int             `json:"calls"`
}

// UsageTracker accumulates normalized token usage per session (in-memory, no persistence).
type UsageTracker struct {
	sessions sync.Map // map[string]*SessionUsage
}

// NewUsageTracker creates a new usage tracker.
func NewUsageTracker() *UsageTracker {
	return &UsageTracker{}
}

// Accumulate adds usage from a single model call to the session's cumulative total.
func (t *UsageTracker) Accumulate(sessionKey string, usage NormalizedUsage) {
	val, _ := t.sessions.LoadOrStore(sessionKey, &SessionUsage{})
	su := val.(*SessionUsage)
	su.mu.Lock()
	su.Cumulative.Input += usage.Input
	su.Cumulative.Output += usage.Output
	su.Cumulative.CacheRead += usage.CacheRead
	su.Cumulative.CacheWrite += usage.CacheWrite
	su.Cumulative.Total += usage.Total
	su.Calls++
	su.mu.Unlock()
}

// GetSession returns cumulative usage for a session. Returns zero if not found.
func (t *UsageTracker) GetSession(sessionKey string) (NormalizedUsage, int) {
	val, ok := t.sessions.Load(sessionKey)
	if !ok {
		return NormalizedUsage{}, 0
	}
	su := val.(*SessionUsage)
	su.mu.Lock()
	u := su.Cumulative
	c := su.Calls
	su.mu.Unlock()
	return u, c
}

// GetAll returns a snapshot of all session usage.
func (t *UsageTracker) GetAll() map[string]SessionUsage {
	result := make(map[string]SessionUsage)
	t.sessions.Range(func(key, val any) bool {
		k, ok := key.(string)
		if !ok {
			return true
		}
		su := val.(*SessionUsage)
		su.mu.Lock()
		result[k] = SessionUsage{
			Cumulative: su.Cumulative,
			Calls:      su.Calls,
		}
		su.mu.Unlock()
		return true
	})
	return result
}

// ClearSession removes usage data for a session (e.g. on session reset).
func (t *UsageTracker) ClearSession(sessionKey string) {
	t.sessions.Delete(sessionKey)
}

// Aggregate returns the total usage across all sessions.
func (t *UsageTracker) Aggregate() NormalizedUsage {
	var agg NormalizedUsage
	t.sessions.Range(func(_, val any) bool {
		su := val.(*SessionUsage)
		su.mu.Lock()
		agg.Input += su.Cumulative.Input
		agg.Output += su.Cumulative.Output
		agg.CacheRead += su.Cumulative.CacheRead
		agg.CacheWrite += su.Cumulative.CacheWrite
		agg.Total += su.Cumulative.Total
		su.mu.Unlock()
		return true
	})
	return agg
}

// clampNeg clamps a value to 0 if negative (Kimi/pi-ai pre-subtract cache from prompt).
func clampNeg(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

// NormalizeUsage maps provider-specific field names to the standard 5-field format.
// It handles 15+ naming variants from Anthropic, OpenAI, Google, Copilot, etc.
func NormalizeUsage(raw map[string]any) NormalizedUsage {
	if raw == nil {
		return NormalizedUsage{}
	}

	getInt := func(keys ...string) int {
		for _, k := range keys {
			if v, ok := raw[k]; ok {
				switch n := v.(type) {
				case float64:
					return int(n)
				case int:
					return n
				case int64:
					return int(n)
				}
			}
		}
		return 0
	}

	// Nested field extraction (e.g., prompt_tokens_details.cached_tokens)
	getNestedInt := func(outer, inner string) int {
		if v, ok := raw[outer]; ok {
			if m, ok := v.(map[string]any); ok {
				if n, ok := m[inner]; ok {
					switch val := n.(type) {
					case float64:
						return int(val)
					case int:
						return val
					case int64:
						return int(val)
					}
				}
			}
		}
		return 0
	}

	input := getInt(
		"input_tokens", "inputTokens",
		"prompt_tokens", "promptTokens",
		"promptTokenCount",
	)

	output := getInt(
		"output_tokens", "outputTokens",
		"completion_tokens", "completionTokens",
		"candidatesTokenCount",
	)

	cacheRead := getInt(
		"cache_read_input_tokens", "cacheReadInputTokens",
		"cached_tokens", "cachedTokens",
	)
	// Also check nested prompt_tokens_details.cached_tokens (OpenAI format)
	if cacheRead == 0 {
		cacheRead = getNestedInt("prompt_tokens_details", "cached_tokens")
	}

	cacheWrite := getInt(
		"cache_creation_input_tokens", "cacheCreationInputTokens",
	)

	total := getInt(
		"total_tokens", "totalTokens",
		"totalTokenCount",
	)

	// Clamp negative values (some providers pre-subtract cache from prompt)
	input = clampNeg(input)
	output = clampNeg(output)
	cacheRead = clampNeg(cacheRead)
	cacheWrite = clampNeg(cacheWrite)

	// Compute total if not provided
	if total == 0 {
		total = input + output
	}
	total = clampNeg(total)

	return NormalizedUsage{
		Input:      input,
		Output:     output,
		CacheRead:  cacheRead,
		CacheWrite: cacheWrite,
		Total:      total,
	}
}
