// Package common – retry.go provides a generic retry-with-backoff helper for
// channel message sends (Telegram, Discord, Slack).
//
// Features:
//   - Configurable max attempts (default 3)
//   - Exponential backoff: baseDelay × 2^attempt, capped at maxDelay
//   - ±10 % jitter to de-synchronise retries across channels
//   - Respects "retry_after" hints in error strings
//   - Optional markdown→plain-text fallback on the last attempt
package common

import (
	"math/rand"
	"strings"
	"time"

	"go.uber.org/zap"
)

// RetryConfig controls the retry behaviour.
type RetryConfig struct {
	MaxAttempts      int                  // total attempts including the first (default 3)
	BaseDelay        time.Duration        // initial backoff (default 500ms)
	MaxDelay         time.Duration        // cap per-attempt backoff (default 5s)
	JitterFraction   float64              // ±fraction of the delay added as jitter (default 0.10)
	PlainTextOnFinal bool                 // strip markdown on last retry (parse-error fallback)
	Logger           *zap.SugaredLogger   // optional logger for retry warnings
}

func (rc RetryConfig) maxAttempts() int {
	if rc.MaxAttempts <= 0 {
		return 3
	}
	return rc.MaxAttempts
}

func (rc RetryConfig) baseDelay() time.Duration {
	if rc.BaseDelay <= 0 {
		return 500 * time.Millisecond
	}
	return rc.BaseDelay
}

func (rc RetryConfig) maxDelay() time.Duration {
	if rc.MaxDelay <= 0 {
		return 5 * time.Second
	}
	return rc.MaxDelay
}

func (rc RetryConfig) jitterFraction() float64 {
	if rc.JitterFraction <= 0 {
		return 0.10
	}
	return rc.JitterFraction
}

// RetrySend calls fn up to rc.MaxAttempts times with exponential backoff and
// jitter. If rc.PlainTextOnFinal is true and the error string contains
// "parse" or "bad request", the text argument is stripped of markdown on the
// final attempt and fn is called with the stripped version.
//
// fn receives the (possibly modified) text and returns an error.
func RetrySend(rc RetryConfig, text string, fn func(string) error) error {
	maxAtt := rc.maxAttempts()
	delay := rc.baseDelay()
	maxDel := rc.maxDelay()
	jf := rc.jitterFraction()

	var lastErr error
	for attempt := 1; attempt <= maxAtt; attempt++ {
		sendText := text
		if attempt == maxAtt && rc.PlainTextOnFinal && lastErr != nil && isParseLikeError(lastErr) {
			sendText = StripMarkdown(sendText)
		}

		err := fn(sendText)
		if err == nil {
			return nil
		}
		lastErr = err

		if attempt == maxAtt {
			break
		}

		// Check for Retry-After hint in the error message.
		if ra := parseRetryAfter(err.Error()); ra > 0 {
			delay = ra
		}

		// Add jitter: ±jf of delay.
		jitter := time.Duration(float64(delay) * jf * (2*rand.Float64() - 1))
		wait := delay + jitter
		if wait < 0 {
			wait = 0
		}

		if rc.Logger != nil {
			rc.Logger.Warnf("channel send failed (attempt %d/%d): %v — retrying in %s", attempt, maxAtt, err, wait)
		}
		time.Sleep(wait)

		// Exponential backoff.
		delay *= 2
		if delay > maxDel {
			delay = maxDel
		}
	}
	return lastErr
}

// isParseLikeError heuristically detects markdown-parse failures.
func isParseLikeError(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "parse") || strings.Contains(s, "bad request") || strings.Contains(s, "entities")
}

// parseRetryAfter extracts a "retry_after: N" or "retry after N" value from an
// error string and returns a duration. Returns 0 if not found.
func parseRetryAfter(errMsg string) time.Duration {
	lower := strings.ToLower(errMsg)
	for _, prefix := range []string{"retry_after:", "retry_after: ", "retry after ", "retry-after: "} {
		idx := strings.Index(lower, prefix)
		if idx < 0 {
			continue
		}
		rest := strings.TrimSpace(errMsg[idx+len(prefix):])
		var secs float64
		n, _ := parseLeadingFloat(rest)
		if n > 0 {
			secs = n
		}
		if secs > 0 && secs < 120 {
			return time.Duration(secs * float64(time.Second))
		}
	}
	return 0
}

// parseLeadingFloat reads the first float number at the start of s.
func parseLeadingFloat(s string) (float64, bool) {
	i := 0
	dotSeen := false
	for i < len(s) {
		ch := s[i]
		if ch >= '0' && ch <= '9' {
			i++
		} else if ch == '.' && !dotSeen {
			dotSeen = true
			i++
		} else {
			break
		}
	}
	if i == 0 {
		return 0, false
	}
	// Simple float parse.
	var val float64
	var frac float64
	var fracDiv float64 = 1
	inFrac := false
	for _, ch := range s[:i] {
		if ch == '.' {
			inFrac = true
			continue
		}
		if inFrac {
			frac = frac*10 + float64(ch-'0')
			fracDiv *= 10
		} else {
			val = val*10 + float64(ch-'0')
		}
	}
	return val + frac/fracDiv, true
}

// StripMarkdown removes common Markdown formatting for plain-text fallback.
func StripMarkdown(text string) string {
	// Remove code blocks
	for {
		start := strings.Index(text, "```")
		if start < 0 {
			break
		}
		end := strings.Index(text[start+3:], "```")
		if end < 0 {
			// Unclosed — just remove the opening fence.
			text = text[:start] + text[start+3:]
			break
		}
		inner := text[start+3 : start+3+end]
		// Drop the language tag from the first line of the code block.
		if nl := strings.IndexByte(inner, '\n'); nl >= 0 {
			inner = inner[nl+1:]
		}
		text = text[:start] + inner + text[start+3+end+3:]
	}
	// Remove bold/italic markers.
	text = strings.ReplaceAll(text, "**", "")
	text = strings.ReplaceAll(text, "__", "")
	text = strings.ReplaceAll(text, "*", "")
	text = strings.ReplaceAll(text, "_", "")
	// Remove inline code.
	text = strings.ReplaceAll(text, "`", "")
	return text
}
