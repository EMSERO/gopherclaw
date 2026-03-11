package orchestrator

import (
	"strings"
)

// DefaultMaxInterpolateLen is the maximum number of characters substituted
// into a {{task-id.output}} placeholder (DEC-003).
const DefaultMaxInterpolateLen = 4000

// Interpolate replaces {{task-id.output}} placeholders in msg with the
// corresponding task output from results. Unknown references are left as-is.
// Output values are truncated to maxLen characters (0 = DefaultMaxInterpolateLen).
func Interpolate(msg string, results map[string]string, maxLen int) string {
	if maxLen <= 0 {
		maxLen = DefaultMaxInterpolateLen
	}
	// Fast path: no placeholders
	if !strings.Contains(msg, "{{") {
		return msg
	}
	var b strings.Builder
	b.Grow(len(msg))
	for {
		start := strings.Index(msg, "{{")
		if start == -1 {
			b.WriteString(msg)
			break
		}
		end := strings.Index(msg[start:], "}}")
		if end == -1 {
			b.WriteString(msg)
			break
		}
		end += start + 2 // past the "}}"

		b.WriteString(msg[:start])
		placeholder := msg[start+2 : end-2] // strip {{ and }}

		// Expected format: "task-id.output"
		if taskID, ok := strings.CutSuffix(placeholder, ".output"); ok {
			if output, found := results[taskID]; found {
				if len(output) > maxLen {
					output = output[:maxLen] + "\n... (truncated)"
				}
				b.WriteString(output)
			} else {
				// Unknown reference — leave as-is
				b.WriteString(msg[start:end])
			}
		} else {
			// Not a recognized pattern — leave as-is
			b.WriteString(msg[start:end])
		}
		msg = msg[end:]
	}
	return b.String()
}
