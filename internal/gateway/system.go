package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/EMSERO/gopherclaw/internal/taskqueue"
	"github.com/EMSERO/gopherclaw/internal/tools"
)

// handleSystemEvent handles POST /gopherclaw/system/event.
// Payload: {"text": "...", "mode": "now"} — triggers an agent turn
// with the text as input and delivers the response to the Telegram channel.
func (s *Server) handleSystemEvent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
		Mode string `json:"mode"` // "now" or "next-heartbeat" (treated as "now")
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 10<<20)).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid JSON"})
		return
	}
	if req.Text == "" {
		s.writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "text is required"})
		return
	}

	sessionKey := "agent:main:gateway"

	// Fire the agent turn asynchronously via taskMgr for lifecycle tracking.
	timeout := time.Duration(s.cfg.Agents.Defaults.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	taskFn := func(ctx context.Context) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		resp, err := s.ag.Chat(ctx, sessionKey, req.Text)
		if err != nil {
			s.logger.Errorf("system event: agent error: %v", err)
			return "", err
		}

		s.logger.Infof("system event: agent response: %s", truncate(resp.Text, 200))

		// Deliver to all registered channel bots
		if resp.Text != "" {
			for _, d := range s.deliverers {
				d.SendToAllPaired(resp.Text)
			}
		}
		return resp.Text, nil
	}
	if s.taskMgr != nil {
		s.taskMgr.Submit(sessionKey, "system", req.Text, taskFn, taskqueue.SubmitOpts{})
	} else {
		go func() {
			if _, err := taskFn(context.Background()); err != nil {
				s.logger.Warnf("system event: untracked task failed: %v", err)
			}
		}()
	}

	s.writeJSON(w, http.StatusAccepted, AcceptedResponse{Status: "accepted"})
}

// handleToolInvoke handles POST /tools/invoke — executes a single tool.
// Payload: {"tool": "exec", "args": {"command": "ls"}, "sessionKey": "..."}
func (s *Server) handleToolInvoke(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tool       string          `json:"tool"`
		Args       json.RawMessage `json:"args"`
		SessionKey string          `json:"sessionKey"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 10<<20)).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid JSON"})
		return
	}
	if req.Tool == "" {
		s.writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "tool name is required"})
		return
	}

	// Find the tool
	var found interface {
		Run(ctx context.Context, argsJSON string) string
	}
	for _, t := range s.tools {
		if t.Name() == req.Tool {
			found = t
			break
		}
	}
	if found == nil {
		s.writeJSON(w, http.StatusNotFound, ErrorResponse{Error: "tool not found: " + req.Tool})
		return
	}

	ctx := r.Context()
	if req.SessionKey != "" {
		ctx = context.WithValue(ctx, tools.SessionKeyContextKey{}, req.SessionKey)
	}

	result := found.Run(ctx, string(req.Args))
	s.writeJSON(w, http.StatusOK, ToolInvokeResponse{OK: true, Result: result})
}

// Cron REST API handlers

func (s *Server) handleCronList(w http.ResponseWriter, _ *http.Request) {
	if s.cronMgr == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{Error: "cron not initialized"})
		return
	}
	jobs := s.cronMgr.List()
	s.writeJSON(w, http.StatusOK, CronListResponse{Jobs: jobs})
}

func (s *Server) handleCronAdd(w http.ResponseWriter, r *http.Request) {
	if s.cronMgr == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{Error: "cron not initialized"})
		return
	}
	var req struct {
		Spec        string `json:"spec"`
		Instruction string `json:"instruction"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 10<<20)).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid JSON"})
		return
	}
	if req.Spec == "" || req.Instruction == "" {
		s.writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "spec and instruction are required"})
		return
	}
	job, err := s.cronMgr.Add(req.Spec, req.Instruction)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}
	s.writeJSON(w, http.StatusCreated, CronJobResponse{Job: job})
}

func (s *Server) handleCronRemove(w http.ResponseWriter, r *http.Request) {
	if s.cronMgr == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{Error: "cron not initialized"})
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.cronMgr.Remove(id); err != nil {
		s.writeJSON(w, http.StatusNotFound, ErrorResponse{Error: err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, OKResponse{OK: true})
}

func (s *Server) handleCronRun(w http.ResponseWriter, r *http.Request) {
	if s.cronMgr == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{Error: "cron not initialized"})
		return
	}
	id := chi.URLParam(r, "id")
	ctx := context.Background()
	if err := s.cronMgr.RunNow(ctx, id); err != nil {
		s.writeJSON(w, http.StatusNotFound, ErrorResponse{Error: err.Error()})
		return
	}
	s.writeJSON(w, http.StatusAccepted, AcceptedResponse{Status: "accepted"})
}

func (s *Server) handleCronSetEnabled(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cronMgr == nil {
			s.writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{Error: "cron not initialized"})
			return
		}
		id := chi.URLParam(r, "id")
		if err := s.cronMgr.SetEnabled(id, enabled); err != nil {
			s.writeJSON(w, http.StatusNotFound, ErrorResponse{Error: err.Error()})
			return
		}
		e := enabled
		s.writeJSON(w, http.StatusOK, OKResponse{OK: true, Enabled: &e})
	}
}

// Task queue API handlers

func (s *Server) handleTaskList(w http.ResponseWriter, r *http.Request) {
	if s.taskMgr == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{Error: "task queue not initialized"})
		return
	}
	session := r.URL.Query().Get("session")
	var tasks any
	if session != "" {
		tasks = s.taskMgr.ListForSession(session)
	} else {
		tasks = s.taskMgr.List()
	}
	s.writeJSON(w, http.StatusOK, TaskListResponse{Tasks: tasks})
}

func (s *Server) handleTaskCancel(w http.ResponseWriter, r *http.Request) {
	if s.taskMgr == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, ErrorResponse{Error: "task queue not initialized"})
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.taskMgr.Cancel(id); err != nil {
		s.writeJSON(w, http.StatusNotFound, ErrorResponse{Error: err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, OKResponse{OK: true})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
