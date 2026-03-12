package gateway

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"github.com/EMSERO/gopherclaw/internal/updater"
)

//go:embed ui.html
var controlUIHTML string

// checkWSOrigin allows same-origin browser connections and all non-browser
// clients (no Origin header). Cross-origin browser requests are rejected.
func checkWSOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // non-browser client (curl, CLI tools)
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: checkWSOrigin,
}

// handleUI serves the embedded control UI with the auth token injected.
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	html := strings.Replace(controlUIHTML, `"__GATEWAY_TOKEN__"`, `"`+s.cfg.Gateway.Auth.Token+`"`, 1)
	_, _ = fmt.Fprint(w, html)
}

// handleManifest serves the PWA web app manifest.
func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	base := s.cfg.Gateway.ControlUI.BasePath
	if base == "" {
		base = "/gopherclaw"
	}
	manifest := fmt.Sprintf(`{
  "name": "GopherClaw",
  "short_name": "GopherClaw",
  "start_url": "%s",
  "display": "standalone",
  "background_color": "#0d1117",
  "theme_color": "#0d1117",
  "description": "GopherClaw AI agent control panel",
  "icons": [
    {"src": "data:image/svg+xml,%%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'%%3E%%3Ctext y='.9em' font-size='90'%%3E🦫%%3C/text%%3E%%3C/svg%%3E","sizes": "any","type": "image/svg+xml"}
  ]
}`, base)
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = fmt.Fprint(w, manifest)
}

// handleServiceWorker serves a minimal service worker for PWA installability.
func (s *Server) handleServiceWorker(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = fmt.Fprint(w, `self.addEventListener('fetch', function(e) { e.respondWith(fetch(e.request)); });`)
}

// handleWS handles the WebSocket connection for real-time status.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Errorf("ws upgrade: %v", err)
		return
	}
	defer func() { _ = conn.Close() }()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// refresh receives a signal when the client requests an immediate update
	// (e.g. after clearing a session, so the UI reflects the change instantly).
	// done is closed when the handler returns, unblocking the reader goroutine
	// (conn.Close triggers ReadMessage to return an error).
	refresh := make(chan struct{}, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				// Signal the main loop to exit if it hasn't already.
				select {
				case refresh <- struct{}{}:
				default:
				}
				return
			}
			select {
			case refresh <- struct{}{}:
			default:
			}
		}
	}()

	sendStatus := func() bool {
		var sessionList []map[string]any
		if s.sessions != nil {
			active := s.sessions.ActiveSessions()
			for key, updatedAt := range active {
				entry := map[string]any{
					"key":       key,
					"updatedAt": updatedAt.Unix(),
					"ago":       time.Since(updatedAt).Truncate(time.Second).String(),
				}
				// Include message count for manageable session counts
				if len(active) <= 30 {
					if msgs, err := s.sessions.GetHistory(key); err == nil {
						entry["messageCount"] = len(msgs)
					}
				}
				sessionList = append(sessionList, entry)
			}
		}

		// Channel status
		var channelList []map[string]any
		for _, ch := range s.channels {
			channelList = append(channelList, map[string]any{
				"name":        ch.ChannelName(),
				"connected":   ch.IsConnected(),
				"username":    ch.Username(),
				"pairedCount": ch.PairedCount(),
			})
		}

		// Cron summary
		var cronSummary map[string]any
		if s.cronMgr != nil {
			jobs := s.cronMgr.List()
			enabled := 0
			for _, j := range jobs {
				if j.Enabled {
					enabled++
				}
			}
			cronSummary = map[string]any{"enabled": enabled, "total": len(jobs)}
		}

		// Task queue summary
		var taskSummary map[string]any
		if s.taskMgr != nil {
			tasks := s.taskMgr.List()
			pending, running := 0, 0
			for _, t := range tasks {
				switch t.Status {
				case "pending":
					pending++
				case "running":
					running++
				}
			}
			taskSummary = map[string]any{"pending": pending, "running": running, "total": len(tasks)}
		}

		// Skills summary
		var skillsSummary []map[string]any
		if s.skillLister != nil {
			for _, sk := range s.skillLister() {
				skillsSummary = append(skillsSummary, map[string]any{
					"name": sk.Name, "origin": sk.Origin, "enabled": sk.Enabled, "verified": sk.Verified,
				})
			}
		}

		// Version info for dashboard panel (REQ-035)
		backupVer := ""
		if exe, err := os.Executable(); err == nil {
			if resolved, err := filepath.EvalSymlinks(exe); err == nil {
				if _, err := os.Stat(resolved + ".bak"); err == nil {
					backupVer = "(backup available)"
				}
			}
		}
		latestVer := ""
		if state, err := updater.LoadState(); err == nil && state.LastAvailableVersion != "" {
			latestVer = state.LastAvailableVersion
		}

		status := map[string]any{
			"status":    "running",
			"version":   s.version,
			"backupVer": backupVer,
			"latestVer": latestVer,
			"model":         func() string { if s.cfg.Agents.Defaults.Engine == "claude-cli" { return "claude-cli" }; return s.cfg.Agents.Defaults.Model.Primary }(),
			"fallbacks":     s.cfg.Agents.Defaults.Model.Fallbacks,
			"subagentModel": s.cfg.Agents.Defaults.Subagents.Model,
			"agents":        s.cfg.Agents.List,
			"sessions":  sessionList,
			"channels":  channelList,
			"cron":      cronSummary,
			"tasks":     taskSummary,
			"skills":    skillsSummary,
			"timestamp": time.Now().Unix(),
		}
		data, _ := json.Marshal(status)
		return conn.WriteMessage(websocket.TextMessage, data) == nil
	}

	// Send status immediately so the UI doesn't show placeholders for 5s.
	if !sendStatus() {
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !sendStatus() {
				return
			}
		case <-refresh:
			if !sendStatus() {
				return
			}
		}
	}
}

// handleSessionClear clears a single session by key.
func (s *Server) handleSessionClear(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.Key == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "key required"})
		return
	}
	s.sessions.Reset(req.Key)
	s.logger.Infof("ui: cleared session %s", req.Key)
	s.writeJSON(w, http.StatusOK, map[string]any{"cleared": req.Key})
}

// handleSessionClearAll clears all sessions.
func (s *Server) handleSessionClearAll(w http.ResponseWriter, _ *http.Request) {
	s.sessions.ResetAll()
	s.logger.Infof("ui: cleared all sessions")
	s.writeJSON(w, http.StatusOK, map[string]any{"cleared": "all"})
}

// handleConfigView returns a redacted copy of the running config.
func (s *Server) handleConfigView(w http.ResponseWriter, _ *http.Request) {
	// Round-trip through JSON to get a generic map we can redact.
	raw, err := json.Marshal(s.cfg)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	redactSensitive(m)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(m)
}

// redactSensitive walks a JSON structure and replaces values whose keys
// contain "token", "key", "secret", or "password" with "****".
func redactSensitive(v any) {
	switch val := v.(type) {
	case map[string]any:
		for k, child := range val {
			low := strings.ToLower(k)
			if strings.Contains(low, "token") || strings.Contains(low, "key") ||
				strings.Contains(low, "secret") || strings.Contains(low, "password") {
				if s, ok := child.(string); ok && s != "" {
					val[k] = "****"
				}
			} else {
				redactSensitive(child)
			}
		}
	case []any:
		for _, item := range val {
			redactSensitive(item)
		}
	}
}

// handleSessionHistory returns the message history for a single session.
func (s *Server) handleSessionHistory(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if key == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": "key required"})
		return
	}
	msgs, err := s.sessions.GetHistory(key)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if msgs == nil {
		s.writeJSON(w, http.StatusOK, []any{})
		return
	}
	s.writeJSON(w, http.StatusOK, msgs)
}

// handleSkillList returns the list of loaded skills.
func (s *Server) handleSkillList(w http.ResponseWriter, _ *http.Request) {
	if s.skillLister == nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"skills": []any{}})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"skills": s.skillLister()})
}

// handleSkillSetEnabled returns a handler that enables or disables a skill.
func (s *Server) handleSkillSetEnabled(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.skillToggler == nil {
			s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "skill manager not available"})
			return
		}
		name := chi.URLParam(r, "name")
		if !s.skillToggler(name, enabled) {
			s.writeJSON(w, http.StatusNotFound, map[string]any{"error": "skill not found"})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name, "enabled": enabled})
	}
}

// handleLogStream serves an SSE stream of log lines from the LogBroadcaster.
func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	if s.logBroadcaster == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "log streaming not configured"})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming not supported"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := s.logBroadcaster.Subscribe()
	defer s.logBroadcaster.Unsubscribe(ch)

	for {
		select {
		case <-r.Context().Done():
			return
		case line, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", line)
			flusher.Flush()
		}
	}
}

// handleVersionInfo returns current, backup, and latest version info (REQ-035).
func (s *Server) handleVersionInfo(w http.ResponseWriter, _ *http.Request) {
	info := map[string]any{
		"current": s.version,
	}

	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			if _, err := os.Stat(resolved + ".bak"); err == nil {
				info["backupAvailable"] = true
			}
		}
	}

	if state, err := updater.LoadState(); err == nil && state.LastAvailableVersion != "" {
		info["latest"] = state.LastAvailableVersion
		info["updateAvailable"] = updater.IsNewer(s.version, state.LastAvailableVersion)
	}

	s.writeJSON(w, http.StatusOK, info)
}

// handleRollback triggers a rollback to the previous version (REQ-035).
func (s *Server) handleRollback(w http.ResponseWriter, _ *http.Request) {
	if err := updater.Rollback(); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.logger.Infof("dashboard: rollback triggered — restart service to use previous version")
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "Rolled back. Restart the service to use the previous version."})
}
