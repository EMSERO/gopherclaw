package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	openai "github.com/sashabaranov/go-openai"

	"github.com/EMSERO/gopherclaw/internal/agent"
	"github.com/EMSERO/gopherclaw/internal/models"
)

// handleChatCompletions proxies POST /v1/chat/completions to the Copilot API.
// This allows VS Code, coding-agent, etc. to use GopherClaw as an OpenAI endpoint.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req openai.ChatCompletionRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 10<<20)).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}

	streaming := req.Stream || r.Header.Get("Accept") == "text/event-stream"

	sessionKey := "agent:main:gateway"
	if xSender := r.Header.Get("X-Session-Key"); xSender != "" {
		sessionKey = sanitizeSessionKey(xSender)
	}

	if streaming {
		s.streamChatResponse(w, r, sessionKey, req)
	} else {
		s.fullChatResponse(w, r, sessionKey, req)
	}
}

func (s *Server) fullChatResponse(w http.ResponseWriter, r *http.Request, sessionKey string, req openai.ChatCompletionRequest) {
	userText := extractLastUser(req.Messages)
	if userText == "" {
		s.writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "no user message"})
		return
	}

	resp, err := s.ag.Chat(r.Context(), sessionKey, userText)
	if err != nil {
		s.logger.Errorf("gateway chat: %v", err)
		s.writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}

	finishReason := openai.FinishReasonStop
	if resp.Stopped {
		finishReason = openai.FinishReasonLength
	}

	promptTok := estimateTokensFromMessages(req.Messages)
	completionTok := estimateTokensFromText(resp.Text)

	s.writeJSON(w, http.StatusOK, openai.ChatCompletionResponse{
		ID:      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   models.ModelID(s.cfg.Agents.Defaults.Model.Primary),
		Choices: []openai.ChatCompletionChoice{
			{
				Index: 0,
				Message: openai.ChatCompletionMessage{
					Role:    "assistant",
					Content: resp.Text,
				},
				FinishReason: finishReason,
			},
		},
		Usage: openai.Usage{
			PromptTokens:     promptTok,
			CompletionTokens: completionTok,
			TotalTokens:      promptTok + completionTok,
		},
	})
}

func (s *Server) streamChatResponse(w http.ResponseWriter, r *http.Request, sessionKey string, req openai.ChatCompletionRequest) {
	userText := extractLastUser(req.Messages)
	if userText == "" {
		s.writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "no user message"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)

	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	modelID := models.ModelID(s.cfg.Agents.Defaults.Model.Primary)
	created := time.Now().Unix()

	var accumulated strings.Builder

	onChunk := func(chunk string) {
		accumulated.WriteString(chunk)
		delta := openai.ChatCompletionStreamResponse{
			ID:      id,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   modelID,
			Choices: []openai.ChatCompletionStreamChoice{
				{
					Index: 0,
					Delta: openai.ChatCompletionStreamChoiceDelta{
						Role:    "assistant",
						Content: chunk,
					},
				},
			},
		}
		data, _ := json.Marshal(delta)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		if ok {
			flusher.Flush()
		}
	}

	resp, err := s.ag.ChatStream(r.Context(), sessionKey, userText, &agent.StreamCallbacks{
		OnChunk: onChunk,
		OnThinking: func(text string) {
			evt := map[string]string{"thinking": text}
			data, _ := json.Marshal(evt)
			_, _ = fmt.Fprintf(w, "event: thinking\ndata: %s\n\n", data)
			if ok {
				flusher.Flush()
			}
		},
		OnToolStart: func(name, args string) {
			evt := map[string]string{"tool": name, "arguments": args}
			data, _ := json.Marshal(evt)
			_, _ = fmt.Fprintf(w, "event: tool_start\ndata: %s\n\n", data)
			if ok {
				flusher.Flush()
			}
		},
		OnToolDone: func(name, result string, toolErr error) {
			evt := map[string]interface{}{"tool": name, "result": result}
			if toolErr != nil {
				evt["error"] = toolErr.Error()
			}
			data, _ := json.Marshal(evt)
			_, _ = fmt.Fprintf(w, "event: tool_done\ndata: %s\n\n", data)
			if ok {
				flusher.Flush()
			}
		},
	})
	if err != nil {
		s.logger.Errorf("gateway stream: %v", err)
		// Send error event and close; don't emit a misleading final chunk.
		errEvt := map[string]string{"error": err.Error()}
		data, _ := json.Marshal(errEvt)
		_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", data)
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
		if ok {
			flusher.Flush()
		}
		return
	}

	// Final chunk: finish_reason + usage
	finishReason := openai.FinishReasonStop
	if resp.Stopped {
		finishReason = openai.FinishReasonLength
	}
	promptTok := estimateTokensFromMessages(req.Messages)
	completionTok := estimateTokensFromText(accumulated.String())
	finalChunk := openai.ChatCompletionStreamResponse{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   modelID,
		Choices: []openai.ChatCompletionStreamChoice{
			{
				Index:        0,
				Delta:        openai.ChatCompletionStreamChoiceDelta{},
				FinishReason: finishReason,
			},
		},
		Usage: &openai.Usage{
			PromptTokens:     promptTok,
			CompletionTokens: completionTok,
			TotalTokens:      promptTok + completionTok,
		},
	}
	data, _ := json.Marshal(finalChunk)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)

	_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	if ok {
		flusher.Flush()
	}
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	primary := s.cfg.Agents.Defaults.Model.Primary
	fallbacks := s.cfg.Agents.Defaults.Model.Fallbacks

	type modelObj struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}

	modelList := []modelObj{{
		ID:      models.ModelID(primary),
		Object:  "model",
		Created: 1677610602,
		OwnedBy: "github-copilot",
	}}
	for _, fb := range fallbacks {
		modelList = append(modelList, modelObj{
			ID:      models.ModelID(fb),
			Object:  "model",
			Created: 1677610602,
			OwnedBy: "github-copilot",
		})
	}

	s.writeJSON(w, http.StatusOK, ModelListResponse{Object: "list", Data: modelList})
}

func (s *Server) handleModelDetail(w http.ResponseWriter, r *http.Request) {
	modelID := chi.URLParam(r, "model")
	primary := models.ModelID(s.cfg.Agents.Defaults.Model.Primary)

	// Check if the requested ID matches primary or any fallback
	known := modelID == primary
	for _, fb := range s.cfg.Agents.Defaults.Model.Fallbacks {
		if modelID == models.ModelID(fb) {
			known = true
			break
		}
	}
	if !known {
		s.writeJSON(w, http.StatusNotFound, ErrorResponse{Error: APIError{
			Message: fmt.Sprintf("The model '%s' does not exist", modelID),
			Type:    "invalid_request_error",
			Code:    "model_not_found",
		}})
		return
	}

	s.writeJSON(w, http.StatusOK, ModelResponse{ID: modelID, Object: "model", Created: 1677610602, OwnedBy: "github-copilot"})
}

func extractLastUser(msgs []openai.ChatCompletionMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	return ""
}

// estimateTokensFromText approximates token count as len(text)/4.
// This is a rough heuristic; accurate counting requires a tokenizer.
func estimateTokensFromText(text string) int {
	n := len(text) / 4
	if n == 0 && len(text) > 0 {
		return 1
	}
	return n
}

// estimateTokensFromMessages sums token estimates across all message contents.
func estimateTokensFromMessages(msgs []openai.ChatCompletionMessage) int {
	var total int
	for _, m := range msgs {
		total += estimateTokensFromText(m.Content)
		// ~4 tokens overhead per message (role + formatting)
		total += 4
	}
	return total
}
