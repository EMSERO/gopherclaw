package gateway

import (
	"encoding/json"
	"io"
	"net/http"
)

// handleNotify handles POST /gopherclaw/api/notify.
// Payload: {"message": "...", "session": "..."}.
// If session is empty, the message is delivered to all paired users.
// This endpoint is used by the gopherclaw-mcp server and external integrations.
func (s *Server) handleNotify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Message string `json:"message"`
		Session string `json:"session"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid JSON"})
		return
	}
	if req.Message == "" {
		s.writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "message is required"})
		return
	}

	delivered := 0
	for _, d := range s.deliverers {
		d.SendToAllPaired(req.Message)
		delivered++
	}

	if delivered == 0 {
		s.writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{Error: "no delivery channels available"})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"delivered": delivered,
		"message":   "ok",
	})
}
