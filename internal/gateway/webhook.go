package gateway

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/EMSERO/gopherclaw/internal/agent"
)

type webhookRequest struct {
	Message string `json:"message"`
	Stream  bool   `json:"stream"`
}

type webhookResponse struct {
	Text    string `json:"text"`
	Stopped bool   `json:"stopped,omitempty"`
}

// handleWebhook handles POST /webhooks/{session}.
// The session path parameter is used as the session key suffix.
// Each webhook session is isolated: sessionKey = "webhook:<session>".
// When gateway.webhookSecret is configured, the request body must be signed
// with HMAC-SHA256 and the hex digest provided in the X-Signature header.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	sessionParam := chi.URLParam(r, "session")
	if sessionParam == "" {
		s.writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "session parameter required"})
		return
	}

	// Read body for both HMAC validation and JSON decoding.
	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "read body: " + err.Error()})
		return
	}

	// HMAC signature validation when webhookSecret is configured.
	if secret := s.cfg.Gateway.WebhookSecret; secret != "" {
		sig := r.Header.Get("X-Signature")
		if sig == "" {
			s.writeJSON(w, http.StatusUnauthorized, ErrorResponse{Error: "missing X-Signature header"})
			return
		}
		if !verifyHMAC(body, sig, secret) {
			s.writeJSON(w, http.StatusForbidden, ErrorResponse{Error: "invalid signature"})
			return
		}
	}

	var req webhookRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	if req.Message == "" {
		s.writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "message is required"})
		return
	}

	sessionKey := "webhook:" + sanitizeSessionKey(sessionParam)

	if req.Stream {
		s.webhookStream(w, r, sessionKey, req.Message)
		return
	}

	resp, err := s.ag.Chat(r.Context(), sessionKey, req.Message)
	if err != nil {
		s.logger.Errorf("webhook chat: %v", err)
		s.writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}

	s.writeJSON(w, http.StatusOK, webhookResponse{Text: resp.Text, Stopped: resp.Stopped})
}

func (s *Server) webhookStream(w http.ResponseWriter, r *http.Request, sessionKey, message string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, hasFlusher := w.(http.Flusher)

	onChunk := func(chunk string) {
		data, _ := json.Marshal(map[string]string{"text": chunk})
		_, _ = w.Write([]byte("data: " + string(data) + "\n\n"))
		if hasFlusher {
			flusher.Flush()
		}
	}

	resp, err := s.ag.ChatStream(r.Context(), sessionKey, message, &agent.StreamCallbacks{OnChunk: onChunk})
	if err != nil {
		s.logger.Errorf("webhook stream: %v", err)
		return
	}

	// Send final event with full text and stopped flag
	finalData, _ := json.Marshal(webhookResponse{Text: resp.Text, Stopped: resp.Stopped})
	_, _ = w.Write([]byte("event: done\ndata: " + string(finalData) + "\n\n"))
	if hasFlusher {
		flusher.Flush()
	}
}

// verifyHMAC validates that sig (hex-encoded) matches the HMAC-SHA256 of body
// using the given secret. Comparison is constant-time.
func verifyHMAC(body []byte, sig, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))

	// Support both raw hex and "sha256=hex" formats.
	sig = bytes.NewBufferString(sig).String()
	if len(sig) > 7 && sig[:7] == "sha256=" {
		sig = sig[7:]
	}
	return hmac.Equal([]byte(expected), []byte(sig))
}
