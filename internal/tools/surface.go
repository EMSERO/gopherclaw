package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/agentapi"
	"github.com/EMSERO/gopherclaw/internal/surfaces"
)

// SurfaceCreateTool allows the agent to create surfaces directly during any
// conversation turn. High-priority surfaces (1-2) are broadcast to channel bots.
type SurfaceCreateTool struct {
	Store      *surfaces.Store
	Deliverers []agentapi.Deliverer
	Logger     *zap.SugaredLogger
}

type surfaceCreateInput struct {
	Content     string   `json:"content"`
	SurfaceType string   `json:"surface_type"`
	Priority    int      `json:"priority"`
	Tags        []string `json:"tags,omitempty"`
}

func (t *SurfaceCreateTool) Name() string { return "surface_create" }

func (t *SurfaceCreateTool) Description() string {
	return "Create an ambient surface (insight, question, warning, reminder, or connection) " +
		"that will be displayed on the user's dashboard. Use this when you notice something " +
		"worth surfacing — a pattern, a potential issue, a question for the user, or a " +
		"connection between pieces of information. Priority 1 is most urgent, 5 is least."
}

func (t *SurfaceCreateTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"content": {
				"type": "string",
				"description": "The surface content (1-3 sentences)"
			},
			"surface_type": {
				"type": "string",
				"enum": ["insight", "question", "warning", "reminder", "connection"],
				"description": "Type of surface"
			},
			"priority": {
				"type": "integer",
				"minimum": 1,
				"maximum": 5,
				"description": "Priority level (1=most urgent, 5=least)"
			},
			"tags": {
				"type": "array",
				"items": {"type": "string"},
				"description": "Optional tags for categorization"
			}
		},
		"required": ["content", "surface_type", "priority"]
	}`)
}

func (t *SurfaceCreateTool) Run(ctx context.Context, argsJSON string) string {
	if t.Store == nil {
		return "error: surfaces not enabled"
	}

	var in surfaceCreateInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return fmt.Sprintf("error: invalid input: %v", err)
	}
	if in.Content == "" {
		return "error: content is required"
	}

	// Validate surface type.
	switch surfaces.SurfaceType(in.SurfaceType) {
	case surfaces.TypeInsight, surfaces.TypeQuestion, surfaces.TypeWarning,
		surfaces.TypeReminder, surfaces.TypeConnection:
	default:
		return fmt.Sprintf("error: invalid surface_type %q", in.SurfaceType)
	}

	// Clamp priority.
	if in.Priority < 1 {
		in.Priority = 1
	} else if in.Priority > 5 {
		in.Priority = 5
	}

	surf, err := t.Store.Create(ctx, surfaces.CreateRequest{
		Content:     in.Content,
		SurfaceType: surfaces.SurfaceType(in.SurfaceType),
		Priority:    in.Priority,
		Tags:        in.Tags,
	})
	if err != nil {
		return fmt.Sprintf("error: failed to create surface: %v", err)
	}

	// Broadcast high-priority surfaces to channel bots.
	if in.Priority <= 2 && len(t.Deliverers) > 0 {
		icon := "💡"
		switch in.SurfaceType {
		case "warning":
			icon = "⚠️"
		case "question":
			icon = "❓"
		case "reminder":
			icon = "⏰"
		case "connection":
			icon = "🔗"
		}
		msg := fmt.Sprintf("%s [%s] %s", icon, in.SurfaceType, in.Content)
		for _, d := range t.Deliverers {
			d.SendToAllPaired(msg)
		}
	}

	return fmt.Sprintf("Surface created: %s (type=%s, priority=%d)", surf.ID, surf.SurfaceType, surf.Priority)
}
