package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/agent"
	"github.com/EMSERO/gopherclaw/internal/agentapi"
	"github.com/EMSERO/gopherclaw/internal/config"
	"github.com/EMSERO/gopherclaw/internal/cron"
	"github.com/EMSERO/gopherclaw/internal/session"
	"github.com/EMSERO/gopherclaw/internal/taskqueue"
)

// Deliverer is an alias for agentapi.Deliverer.
type Deliverer = agentapi.Deliverer

// ChannelStatusProvider reports whether a channel bot is connected.
type ChannelStatusProvider interface {
	ChannelName() string
	IsConnected() bool
	Username() string
	PairedCount() int
}

// SkillInfo is a minimal skill descriptor for the UI.
type SkillInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Origin      string `json:"origin"`
	Enabled     bool   `json:"enabled"`
	Verified    bool   `json:"verified"`
}

// SkillLister returns the current skill list for the dashboard.
type SkillLister func() []SkillInfo

// SkillToggler toggles a skill's enabled state. Returns true if found.
type SkillToggler func(name string, enabled bool) bool

// Server is the HTTP gateway.
type Server struct {
	cfg            *config.Root
	ag             agent.PrimaryAgent
	sessions       *session.Manager
	cronMgr        *cron.Manager
	taskMgr        *taskqueue.Manager
	tools          []agent.Tool
	deliverers     []Deliverer             // channel bots for system event delivery
	channels       []ChannelStatusProvider // channel bots for status reporting
	logBroadcaster *LogBroadcaster         // log fan-out for SSE
	logger         *zap.SugaredLogger      // structured logger
	version        string                  // current binary version
	skillLister    SkillLister             // optional skill list provider
	skillToggler   SkillToggler            // optional skill toggle
	http           *http.Server
}

// New creates the gateway server.
func New(logger *zap.SugaredLogger, cfg *config.Root, ag agent.PrimaryAgent, sessions *session.Manager, cronMgr *cron.Manager, taskMgr *taskqueue.Manager, tools []agent.Tool, logBroadcaster *LogBroadcaster) *Server {
	s := &Server{cfg: cfg, ag: ag, sessions: sessions, cronMgr: cronMgr, taskMgr: taskMgr, tools: tools, logBroadcaster: logBroadcaster, logger: logger}

	// Warn only when the operator has explicitly opted out of auth on a public bind.
	mode := cfg.Gateway.Auth.Mode
	if (mode == "none" || mode == "trusted-proxy") && strings.HasPrefix(cfg.GatewayListenAddr(), "0.0.0.0") {
		s.logger.Warnf("SECURITY: gateway.auth.mode=%q — HTTP API is unauthenticated on %s", mode, cfg.GatewayListenAddr())
	}

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(rateLimitMiddleware(cfg.Gateway.RateLimit.RPS, cfg.Gateway.RateLimit.Burst))

	// Health (no auth) — /healthz, /ready, /readyz are aliases for Docker/K8s probes
	r.Get("/health", s.handleHealth)
	r.Get("/healthz", s.handleHealth)
	r.Get("/ready", s.handleHealth)
	r.Get("/readyz", s.handleHealth)

	// Control UI — HTML page and status WebSocket served without auth
	// (read-only, no sensitive data; WS has same-origin check).
	// Mutation endpoints (clear, system/event) remain behind authMiddleware.
	if cfg.Gateway.ControlUI.Enabled {
		base := cfg.Gateway.ControlUI.BasePath
		if base == "" {
			base = "/gopherclaw"
		}
		r.Get(base, s.handleUI)
		r.Get(base+"/ws", s.handleWS)
		r.Get(base+"/api/log", s.handleLogStream)
		// PWA assets
		r.Get(base+"/manifest.json", s.handleManifest)
		r.Get(base+"/sw.js", s.handleServiceWorker)
	}

	// Auth-protected routes
	r.Group(func(r chi.Router) {
		r.Use(s.authMiddleware)

		// OpenAI-compatible API
		r.Post("/v1/chat/completions", s.handleChatCompletions)
		r.Get("/v1/models", s.handleModels)
		r.Get("/v1/models/{model}", s.handleModelDetail)

		// Inbound webhooks
		r.Post("/webhooks/{session}", s.handleWebhook)

		// Single tool invocation
		r.Post("/tools/invoke", s.handleToolInvoke)

		// Control UI endpoints (auth required)
		if cfg.Gateway.ControlUI.Enabled {
			base := cfg.Gateway.ControlUI.BasePath
			if base == "" {
				base = "/gopherclaw"
			}
			r.Get(base+"/api/config", s.handleConfigView)

			// Cron management API
			r.Get(base+"/api/cron", s.handleCronList)
			r.Post(base+"/api/cron", s.handleCronAdd)
			r.Delete(base+"/api/cron/{id}", s.handleCronRemove)
			r.Post(base+"/api/cron/{id}/run", s.handleCronRun)
			r.Post(base+"/api/cron/{id}/enable", s.handleCronSetEnabled(true))
			r.Post(base+"/api/cron/{id}/disable", s.handleCronSetEnabled(false))
			r.Get(base+"/api/sessions/{key}/history", s.handleSessionHistory)
			r.Post(base+"/sessions/clear", s.handleSessionClear)
			r.Post(base+"/sessions/clear-all", s.handleSessionClearAll)

			// Task queue API
			r.Get(base+"/api/tasks", s.handleTaskList)
			r.Post(base+"/api/tasks/{id}/cancel", s.handleTaskCancel)

			// Skills API
			r.Get(base+"/api/skills", s.handleSkillList)
			r.Post(base+"/api/skills/{name}/enable", s.handleSkillSetEnabled(true))
			r.Post(base+"/api/skills/{name}/disable", s.handleSkillSetEnabled(false))

			// Version / rollback API (REQ-035)
			r.Get(base+"/api/version", s.handleVersionInfo)
			r.Post(base+"/api/rollback", s.handleRollback)

			// Token usage API (REQ-420)
			r.Get(base+"/api/usage", s.handleUsage)

			// Cron run history API (REQ-430)
			r.Get(base+"/api/cron/{name}/history", s.handleCronHistory)

			// Notify API (for MCP server and external integrations)
			r.Post(base+"/api/notify", s.handleNotify)

			// System event endpoint
			r.Post(base+"/system/event", s.handleSystemEvent)
		}
	})

	s.http = &http.Server{
		Addr:         cfg.GatewayListenAddr(),
		Handler:      r,
		ReadTimeout:  time.Duration(cfg.Gateway.ReadTimeoutSec) * time.Second,
		WriteTimeout: time.Duration(cfg.Gateway.WriteTimeoutSec) * time.Second,
	}

	return s
}

// AddDeliverer registers a channel deliverer for system event routing.
func (s *Server) AddDeliverer(d Deliverer) { s.deliverers = append(s.deliverers, d) }

// AddChannel registers a channel bot for status reporting in the control UI.
func (s *Server) AddChannel(c ChannelStatusProvider) { s.channels = append(s.channels, c) }

// SetVersion sets the version string shown in the dashboard.
func (s *Server) SetVersion(v string) { s.version = v }

// SetSkillManager registers skill list/toggle callbacks for the dashboard.
func (s *Server) SetSkillManager(lister SkillLister, toggler SkillToggler) {
	s.skillLister = lister
	s.skillToggler = toggler
}

// Start begins listening (blocks until context cancelled or error).
func (s *Server) Start(ctx context.Context) error {
	s.logger.Infof("gateway: listening on %s", s.http.Addr)

	errCh := make(chan error, 1)
	go func() {
		if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.http.Shutdown(shutCtx)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	status := "ok"
	code := http.StatusOK

	checks := map[string]any{}

	// Session store check
	if s.sessions != nil {
		checks["sessions"] = "ok"
	} else {
		checks["sessions"] = "unavailable"
	}

	// Channel connectivity
	if len(s.channels) > 0 {
		allConnected := true
		channelChecks := make([]ChannelCheck, 0, len(s.channels))
		for _, ch := range s.channels {
			connected := ch.IsConnected()
			if !connected {
				allConnected = false
			}
			channelChecks = append(channelChecks, ChannelCheck{
				Name:      ch.ChannelName(),
				Connected: connected,
			})
		}
		checks["channels"] = channelChecks
		if !allConnected {
			status = "degraded"
		}
	}

	// Cron manager
	if s.cronMgr != nil {
		checks["cron"] = "ok"
	}

	// Model provider health
	if s.ag != nil {
		modelHealth := s.ag.ModelHealth()
		checks["models"] = modelHealth
		for _, m := range modelHealth {
			if !m.Available {
				status = "degraded"
			}
		}
	}

	s.writeJSON(w, code, HealthResponse{
		Status:  status,
		Version: s.version,
		Checks:  checks,
	})
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	mode := s.cfg.Gateway.Auth.Mode
	token := s.cfg.Gateway.Auth.Token

	// Explicit opt-outs or no token configured: no auth check.
	if mode == "none" || mode == "trusted-proxy" || token == "" {
		return next
	}
	// Default / "token" mode: require Bearer header or ?token= query param.
	// Token is guaranteed non-empty by EnsureAuth at startup.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if provided == "" || provided == r.Header.Get("Authorization") {
			// No Bearer header — fall back to query parameter (used by browser UI).
			provided = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			s.writeJSON(w, http.StatusUnauthorized, ErrorResponse{Error: "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sanitizeSessionKey strips path separators and other characters that could
// be confusing or dangerous in a session key. Session keys are map keys (not
// filesystem paths), but keeping them clean prevents confusion in log output
// and in channel-bot session-key parsing.
func sanitizeSessionKey(key string) string {
	key = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r < 0x20 {
			return '_'
		}
		return r
	}, key)
	if len(key) > 200 {
		key = key[:200]
	}
	return key
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.logger.Warnf("writeJSON: encode error: %v", err)
	}
}
