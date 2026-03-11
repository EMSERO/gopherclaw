package gateway

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/EMSERO/gopherclaw/internal/cron"
)

// handleUsage handles GET /gopherclaw/api/usage — returns token usage per session (REQ-420).
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if s.ag == nil || s.ag.Usage == nil {
		s.writeJSON(w, http.StatusOK, UsageAllResponse{Sessions: map[string]any{}, Aggregate: nil})
		return
	}

	sessionKey := r.URL.Query().Get("session")
	if sessionKey != "" {
		u, calls := s.ag.Usage.GetSession(sessionKey)
		s.writeJSON(w, http.StatusOK, UsageSessionResponse{
			Session: sessionKey,
			Usage:   u,
			Calls:   calls,
		})
		return
	}

	all := s.ag.Usage.GetAll()
	agg := s.ag.Usage.Aggregate()
	s.writeJSON(w, http.StatusOK, UsageAllResponse{
		Sessions:  all,
		Aggregate: agg,
	})
}

// handleCronHistory handles GET /gopherclaw/api/cron/{name}/history — returns paginated run history (REQ-430).
func (s *Server) handleCronHistory(w http.ResponseWriter, r *http.Request) {
	jobID := chi.URLParam(r, "name")
	if jobID == "" {
		s.writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "job name required"})
		return
	}

	if s.cronMgr == nil {
		s.writeJSON(w, http.StatusOK, CronHistoryResponse{Entries: []any{}, Total: 0})
		return
	}

	q := r.URL.Query()
	opts := cron.RunLogPageOpts{
		Limit:   50,
		Status:  q.Get("status"),
		SortDir: q.Get("sort"),
		Query:   q.Get("q"),
	}
	if l := q.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			opts.Limit = n
		}
	}
	if o := q.Get("offset"); o != "" {
		if n, err := strconv.Atoi(o); err == nil {
			opts.Offset = n
		}
	}

	page, err := cron.ReadRunLogPage(s.cronMgr.Dir(), jobID, opts)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}

	s.writeJSON(w, http.StatusOK, CronHistoryResponse{
		Entries: page.Entries,
		Total:   page.Total,
		HasMore: page.HasMore,
	})
}
