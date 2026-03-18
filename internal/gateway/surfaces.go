package gateway

import "net/http"

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
