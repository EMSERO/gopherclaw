package reasoning

import (
	"fmt"
	"strings"
	"time"

	"github.com/EMSERO/gopherclaw/internal/eidetic"
	"github.com/EMSERO/gopherclaw/internal/surfaces"
)

// BuildPrompt assembles the prompt sent to Claude for a reasoning cycle.
func BuildPrompt(entries []eidetic.MemoryEntry, activeSurfaces []surfaces.Surface, recentlyResolved []surfaces.Surface, answeredSurfaces []surfaces.Surface) string {
	var b strings.Builder

	b.WriteString(`You are the reasoning engine for GopherClaw's ambient surfaces system.
Your job is to review the user's semantic memory and current surfaces, then decide:
1. Which active surfaces are still relevant (expire any that are stale or no longer useful)
2. What NEW surfaces to create (insights, questions, warnings, reminders, connections)

The current time is: `)
	b.WriteString(time.Now().Format(time.RFC3339))
	b.WriteString("\n\n")

	// Recent entries
	b.WriteString("## Recent Eidetic Memory Entries\n\n")
	if len(entries) == 0 {
		b.WriteString("(none)\n\n")
	} else {
		for _, e := range entries {
			fmt.Fprintf(&b, "- [%s] (agent: %s, %dw) %s\n",
				e.Timestamp.Format("2006-01-02 15:04"), e.AgentID, e.WordCount,
				truncate(e.Content, 500))
			if len(e.Tags) > 0 {
				fmt.Fprintf(&b, "  tags: %s\n", strings.Join(e.Tags, ", "))
			}
		}
		b.WriteString("\n")
	}

	// Currently active surfaces — Claude should review these
	if len(activeSurfaces) > 0 {
		b.WriteString("## Currently Active Surfaces (review for continued relevance)\n\n")
		for _, s := range activeSurfaces {
			age := time.Since(s.CreatedAt).Round(time.Minute)
			fmt.Fprintf(&b, "- id=%s [%s] p%d (age: %s) %s\n",
				s.ID, s.SurfaceType, s.Priority, age, truncate(s.Content, 200))
		}
		b.WriteString("\n")
	}

	// Recently resolved surfaces — don't recreate these
	if len(recentlyResolved) > 0 {
		b.WriteString("## Recently Resolved Surfaces (DO NOT recreate these or similar ones)\n\n")
		for _, s := range recentlyResolved {
			fmt.Fprintf(&b, "- [%s] (%s) %s\n", s.SurfaceType, s.Status, truncate(s.Content, 200))
		}
		b.WriteString("\n")
	}

	// Recent user answers
	if len(answeredSurfaces) > 0 {
		b.WriteString("## Recent User Answers (incorporate this new information)\n\n")
		for _, s := range answeredSurfaces {
			resp := ""
			if s.UserResponse != nil {
				resp = *s.UserResponse
			}
			fmt.Fprintf(&b, "- Q: %s\n  A: %s\n", truncate(s.Content, 200), truncate(resp, 200))
		}
		b.WriteString("\n")
	}

	b.WriteString(`## Instructions

Output a single JSON object with two fields:

{
  "expire": ["<surface-id>", ...],
  "create": [
    {"content": "...", "surface_type": "...", "priority": 1-5, "tags": [...]}
  ]
}

### expire
List the IDs of any active surfaces that are no longer relevant, have been superseded by newer information, or are stale. If all active surfaces are still relevant, use an empty array.

### create
New surfaces to show the user. Surface types:
- "insight" — an observation or pattern you noticed
- "question" — something you need the user to answer (they can respond inline)
- "warning" — something that needs attention
- "reminder" — a time-based nudge
- "connection" — a link between two pieces of information

Rules:
- Output ONLY the JSON object. No markdown, no explanation, no code fences.
- Priority 1 = most urgent, 5 = least
- Maximum 5 new surfaces per cycle
- Do NOT create surfaces that duplicate or closely overlap active surfaces
- Do NOT recreate surfaces that the user recently dismissed, answered, or acted on
- Be concise — each surface content should be 1-3 sentences
- Ask questions when you notice gaps or ambiguity in the data
- Only create surfaces that provide genuine value right now
- If nothing new is worth surfacing, use an empty create array
`)

	return b.String()
}

func truncate(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}
