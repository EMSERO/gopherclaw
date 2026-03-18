package gateway

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/EMSERO/gopherclaw/internal/surfaces"
	"github.com/EMSERO/gopherclaw/internal/taskqueue"
)

func (s *Server) surfacesDisabled(w http.ResponseWriter) bool {
	if s.surfaceHandler == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "surfaces not enabled"})
		return true
	}
	return false
}

func (s *Server) handleSurfaceList(w http.ResponseWriter, r *http.Request) {
	if s.surfacesDisabled(w) {
		return
	}
	s.surfaceHandler.List(w, r)
}

func (s *Server) handleSurfaceGet(w http.ResponseWriter, r *http.Request) {
	if s.surfacesDisabled(w) {
		return
	}
	s.surfaceHandler.GetOne(w, r)
}

func (s *Server) handleSurfaceUpdate(w http.ResponseWriter, r *http.Request) {
	if s.surfacesDisabled(w) {
		return
	}
	s.surfaceHandler.Update(w, r)
}

func (s *Server) handleSurfaceRespond(w http.ResponseWriter, r *http.Request) {
	if s.surfacesDisabled(w) {
		return
	}
	s.surfaceHandler.Respond(w, r)
}

func (s *Server) handleSurfaceChat(w http.ResponseWriter, r *http.Request) {
	if s.surfacesDisabled(w) {
		return
	}
	s.surfaceHandler.Chat(w, r)
}

func (s *Server) handleSurfaceMessages(w http.ResponseWriter, r *http.Request) {
	if s.surfacesDisabled(w) {
		return
	}
	s.surfaceHandler.ListMessages(w, r)
}

// handleSurfaceExecute submits the surface content as a task to the agent,
// then marks the surface as "acted".
func (s *Server) handleSurfaceExecute(w http.ResponseWriter, r *http.Request) {
	if s.surfacesDisabled(w) {
		return
	}
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid id"})
		return
	}

	ctx := r.Context()
	surf, err := s.surfaceStore.Get(ctx, id)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to get surface"})
		return
	}
	if surf == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "surface not found"})
		return
	}

	// Build task prompt from the surface.
	prompt := fmt.Sprintf("Please act on this %s surface: %s", surf.SurfaceType, surf.Content)
	sessionKey := fmt.Sprintf("surface-exec:%s", id)

	// Submit to task queue.
	taskID := s.taskMgr.Submit(sessionKey, "main", prompt, func(taskCtx context.Context) (string, error) {
		resp, err := s.ag.Chat(taskCtx, sessionKey, prompt)
		if err != nil {
			return "", err
		}
		return resp.Text, nil
	}, taskqueue.SubmitOpts{})

	// Mark surface as acted.
	acted := surfaces.StatusActed
	_, _ = s.surfaceStore.Update(ctx, id, surfaces.UpdateRequest{Status: &acted})

	s.writeJSON(w, http.StatusOK, map[string]any{"task_id": taskID, "surface_id": id})
}
