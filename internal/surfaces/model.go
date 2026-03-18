package surfaces

import (
	"time"

	"github.com/google/uuid"
)

// SurfaceType identifies what kind of surface the agent produced.
type SurfaceType string

const (
	TypeInsight    SurfaceType = "insight"
	TypeQuestion   SurfaceType = "question"
	TypeWarning    SurfaceType = "warning"
	TypeReminder   SurfaceType = "reminder"
	TypeConnection SurfaceType = "connection"
)

// SurfaceStatus tracks the lifecycle of a surface.
type SurfaceStatus string

const (
	StatusActive    SurfaceStatus = "active"
	StatusDismissed SurfaceStatus = "dismissed"
	StatusAnswered  SurfaceStatus = "answered"
	StatusExpired   SurfaceStatus = "expired"
	StatusActed     SurfaceStatus = "acted"
)

// Surface is an agent-produced item surfaced to the user.
type Surface struct {
	ID              uuid.UUID     `json:"id"`
	Content         string        `json:"content"`
	SurfaceType     SurfaceType   `json:"surface_type"`
	Priority        int           `json:"priority"`
	Status          SurfaceStatus `json:"status"`
	RelatedEntryIDs []uuid.UUID   `json:"related_entry_ids"`
	Tags            []string      `json:"tags"`
	UserResponse    *string       `json:"user_response,omitempty"`
	RespondedAt     *time.Time    `json:"responded_at,omitempty"`
	ReasoningCycle  *uuid.UUID    `json:"reasoning_cycle,omitempty"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
	ExpiredAt       *time.Time    `json:"expired_at,omitempty"`
}

// UpdateRequest is the payload for PATCH /api/surfaces/:id.
type UpdateRequest struct {
	Status *SurfaceStatus `json:"status,omitempty"`
}

// RespondRequest is the payload for POST /api/surfaces/:id/respond.
type RespondRequest struct {
	Response string `json:"response"`
}

// CreateRequest holds the fields needed to insert a new surface.
type CreateRequest struct {
	Content         string      `json:"content"`
	SurfaceType     SurfaceType `json:"surface_type"`
	Priority        int         `json:"priority"`
	RelatedEntryIDs []uuid.UUID `json:"related_entry_ids"`
	Tags            []string    `json:"tags"`
	ReasoningCycle  uuid.UUID   `json:"reasoning_cycle"`
}

// ChatMessage is one message in a surface conversation thread.
type ChatMessage struct {
	ID        uuid.UUID `json:"id"`
	SurfaceID uuid.UUID `json:"surface_id"`
	Role      string    `json:"role"` // "user" or "assistant"
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// ChatRequest is the payload for POST /api/surfaces/:id/chat.
type ChatRequest struct {
	Message string `json:"message"`
}

// ChatResponse is the response from the chat endpoint.
type ChatResponse struct {
	UserMessage      ChatMessage `json:"user_message"`
	AssistantMessage ChatMessage `json:"assistant_message"`
}

// ListFilter holds query-string filters for GET /api/surfaces.
type ListFilter struct {
	Status      string
	SurfaceType string
	Limit       int
}
