package surfaces

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/agentapi"
)

// Handler wires HTTP routes to the surfaces store.
type Handler struct {
	store  *Store
	agent  agentapi.Chatter
	logger *zap.SugaredLogger
}

// NewHandler creates a handler.
func NewHandler(store *Store, agent agentapi.Chatter, logger *zap.SugaredLogger) *Handler {
	return &Handler{store: store, agent: agent, logger: logger}
}

// List returns surfaces matching query filters.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	f := ListFilter{
		Status:      q.Get("status"),
		SurfaceType: q.Get("type"),
		Limit:       limit,
	}
	surfaces, err := h.store.List(r.Context(), f)
	if err != nil {
		h.logger.Warnf("surfaces list: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to list surfaces")
		return
	}
	if surfaces == nil {
		surfaces = []Surface{}
	}
	writeJSON(w, http.StatusOK, surfaces)
}

// GetOne returns a single surface by ID.
func (h *Handler) GetOne(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUID(w, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	surf, err := h.store.Get(r.Context(), id)
	if err != nil {
		h.logger.Warnf("surfaces get: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to get surface")
		return
	}
	if surf == nil {
		writeError(w, http.StatusNotFound, "surface not found")
		return
	}
	writeJSON(w, http.StatusOK, surf)
}

// Update applies a status change (dismiss, mark acted, etc).
func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUID(w, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	var req UpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ctx := r.Context()
	surf, err := h.store.Update(ctx, id, req)
	if err != nil {
		h.logger.Warnf("surfaces update: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to update surface")
		return
	}
	if surf == nil {
		writeError(w, http.StatusNotFound, "surface not found")
		return
	}

	// Write conversation to Eidetic when surface is resolved.
	if surf.Status == StatusDismissed || surf.Status == StatusActed {
		messages, err := h.store.ListMessages(ctx, id)
		if err == nil && len(messages) > 0 {
			if err := h.store.WriteConversationToEidetic(ctx, surf, messages); err != nil {
				h.logger.Warnf("surfaces: conversation write-back failed: %v", err)
			}
		}
	}

	writeJSON(w, http.StatusOK, surf)
}

// Respond records the user's answer to a question surface.
func (h *Handler) Respond(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUID(w, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	var req RespondRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Response == "" {
		writeError(w, http.StatusBadRequest, "response is required")
		return
	}
	surf, err := h.store.Respond(r.Context(), id, req)
	if err != nil {
		h.logger.Warnf("surfaces respond: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to respond to surface")
		return
	}
	if surf == nil {
		writeError(w, http.StatusNotFound, "surface not found")
		return
	}
	writeJSON(w, http.StatusOK, surf)
}

// Chat handles a conversation turn on a surface — stores user message,
// calls the agent, stores response.
func (h *Handler) Chat(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUID(w, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Message == "" {
		writeError(w, http.StatusBadRequest, "message is required")
		return
	}

	ctx := r.Context()

	surf, err := h.store.Get(ctx, id)
	if err != nil {
		h.logger.Warnf("chat get surface: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to get surface")
		return
	}
	if surf == nil {
		writeError(w, http.StatusNotFound, "surface not found")
		return
	}

	// Store user message.
	userMsg, err := h.store.AddMessage(ctx, id, "user", req.Message)
	if err != nil {
		h.logger.Warnf("chat add user message: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to store message")
		return
	}

	// Build context prompt and call the agent.
	messages, err := h.store.ListMessages(ctx, id)
	if err != nil {
		h.logger.Warnf("chat list messages: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to list messages")
		return
	}

	prompt := buildChatPrompt(surf, messages)
	sessionKey := fmt.Sprintf("surface:%s", id)

	resp, err := h.agent.Chat(ctx, sessionKey, prompt)
	if err != nil {
		h.logger.Warnf("chat agent call: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to get response")
		return
	}

	// Store assistant message.
	assistantMsg, err := h.store.AddMessage(ctx, id, "assistant", resp.Text)
	if err != nil {
		h.logger.Warnf("chat add assistant message: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to store response")
		return
	}

	writeJSON(w, http.StatusOK, ChatResponse{
		UserMessage:      *userMsg,
		AssistantMessage: *assistantMsg,
	})
}

// ListMessages returns the conversation thread for a surface.
func (h *Handler) ListMessages(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUID(w, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	messages, err := h.store.ListMessages(r.Context(), id)
	if err != nil {
		h.logger.Warnf("list messages: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to list messages")
		return
	}
	if messages == nil {
		messages = []ChatMessage{}
	}
	writeJSON(w, http.StatusOK, messages)
}

func buildChatPrompt(surf *Surface, messages []ChatMessage) string {
	var b strings.Builder
	fmt.Fprintf(&b, `You are having a conversation with a user about a surface (an agent-produced insight/question/warning).

Surface type: %s
Surface content: %s
`, surf.SurfaceType, surf.Content)

	if len(surf.Tags) > 0 {
		fmt.Fprintf(&b, "Tags: %s\n", strings.Join(surf.Tags, ", "))
	}
	b.WriteString("\nHelp the user think through this surface. Be concise and direct.\n\n")

	for _, m := range messages {
		if m.Role == "user" {
			fmt.Fprintf(&b, "User: %s\n", m.Content)
		} else {
			fmt.Fprintf(&b, "Assistant: %s\n", m.Content)
		}
	}
	b.WriteString("Assistant:")
	return b.String()
}

func parseUUID(w http.ResponseWriter, s string) (uuid.UUID, bool) {
	id, err := uuid.Parse(s)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return uuid.UUID{}, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
