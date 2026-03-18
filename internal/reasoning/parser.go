package reasoning

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// CycleResponse is the top-level JSON shape Claude returns.
type CycleResponse struct {
	Expire []string     `json:"expire"`
	Create []RawSurface `json:"create"`
}

// RawSurface is the JSON shape for a new surface to create.
type RawSurface struct {
	Content     string   `json:"content"`
	SurfaceType string   `json:"surface_type"`
	Priority    int      `json:"priority"`
	Tags        []string `json:"tags"`
	TriggerAt   string   `json:"trigger_at,omitempty"` // RFC3339 timestamp for reminders
}

// ParseTriggerAt parses the trigger_at field as RFC3339. Returns nil if empty or invalid.
func (r *RawSurface) ParseTriggerAt() *time.Time {
	if r.TriggerAt == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, r.TriggerAt)
	if err != nil {
		return nil
	}
	return &t
}

var validTypes = map[string]bool{
	"insight":    true,
	"question":   true,
	"warning":    true,
	"reminder":   true,
	"connection": true,
}

// ParseResponse extracts the cycle response from Claude's text output.
func ParseResponse(raw string) (*CycleResponse, error) {
	raw = strings.TrimSpace(raw)

	// Strip markdown code fences if present.
	if strings.HasPrefix(raw, "```") {
		lines := strings.SplitN(raw, "\n", 2)
		if len(lines) == 2 {
			raw = lines[1]
		}
		if idx := strings.LastIndex(raw, "```"); idx >= 0 {
			raw = raw[:idx]
		}
		raw = strings.TrimSpace(raw)
	}

	// Find the JSON object boundaries.
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object found in response")
	}
	raw = raw[start : end+1]

	var resp CycleResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}

	if resp.Expire == nil {
		resp.Expire = []string{}
	}

	// Validate and clamp surfaces.
	var clean []RawSurface
	for _, s := range resp.Create {
		if s.Content == "" {
			continue
		}
		if !validTypes[s.SurfaceType] {
			s.SurfaceType = "insight"
		}
		if s.Priority < 1 {
			s.Priority = 3
		}
		if s.Priority > 5 {
			s.Priority = 5
		}
		if s.Tags == nil {
			s.Tags = []string{}
		}
		clean = append(clean, s)
	}

	// Cap at 5 surfaces per cycle.
	if len(clean) > 5 {
		clean = clean[:5]
	}
	resp.Create = clean

	return &resp, nil
}
