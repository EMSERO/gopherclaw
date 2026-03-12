package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/agent"
	"github.com/EMSERO/gopherclaw/internal/auth"
	"github.com/EMSERO/gopherclaw/internal/channels/discord"
	slackch "github.com/EMSERO/gopherclaw/internal/channels/slack"
	"github.com/EMSERO/gopherclaw/internal/channels/telegram"
	"github.com/EMSERO/gopherclaw/internal/config"
	"github.com/EMSERO/gopherclaw/internal/cron"
	"github.com/EMSERO/gopherclaw/internal/eidetic"
	"github.com/EMSERO/gopherclaw/internal/embeddings"
	"github.com/EMSERO/gopherclaw/internal/gateway"
	"github.com/EMSERO/gopherclaw/internal/heartbeat"
	"github.com/EMSERO/gopherclaw/internal/hooks"
	"github.com/EMSERO/gopherclaw/internal/initialize"
	"github.com/EMSERO/gopherclaw/internal/migrate"
	"github.com/EMSERO/gopherclaw/internal/models"
	"github.com/EMSERO/gopherclaw/internal/reload"
	"github.com/EMSERO/gopherclaw/internal/security"
	"github.com/EMSERO/gopherclaw/internal/session"
	"github.com/EMSERO/gopherclaw/internal/skills"
	"github.com/EMSERO/gopherclaw/internal/taskqueue"
	"github.com/EMSERO/gopherclaw/internal/tools"
	"github.com/EMSERO/gopherclaw/internal/updater"
)

// version, commit, and date are injected via ldflags at build time.
// goreleaser sets these automatically; for manual builds use:
//
//	go build -ldflags "-X main.version=0.1.0 -X main.commit=$(git rev-parse --short HEAD) -X main.date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" ./cmd/gopherclaw
var (
	version = "0.1.0-dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	// Subcommand dispatch (before flag parsing)
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "init":
			if err := initialize.Run(); err != nil {
				fmt.Fprintf(os.Stderr, "init failed: %v\n", err)
				os.Exit(1)
			}
			os.Exit(0)
		case "update":
			fmt.Printf("gopherclaw %s — checking for updates...\n", version)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			newVer, err := updater.Update(ctx, version)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Updated to %s. Restart the service to use the new version.\n", newVer)
			os.Exit(0)
		case "rollback":
			fmt.Printf("gopherclaw %s — rolling back to previous version...\n", version)
			if err := updater.Rollback(); err != nil {
				fmt.Fprintf(os.Stderr, "rollback failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("Rolled back to previous version. Restart the service to use it.")
			os.Exit(0)
		case "security":
			runSecurityAudit()
		}
	}

	cfgPath := flag.String("config", "", "Path to config.json (default: ~/.gopherclaw/config.json)")
	check := flag.Bool("check", false, "Check config and auth, then exit")
	ver := flag.Bool("version", false, "Print version and exit")
	migrateFlag := flag.Bool("migrate", false, "Migrate OpenClaw sessions to GopherClaw format, then exit")
	flag.Parse()

	if *ver {
		fmt.Printf("gopherclaw %s (commit: %s, built: %s)\n", version, commit, date)
		os.Exit(0)
	}

	if *migrateFlag {
		fmt.Println("Migrating OpenClaw config → ~/.gopherclaw/config.json ...")
		cfgOut, err := migrate.MigrateConfig("", "")
		if err != nil {
			fmt.Fprintf(os.Stderr, "config migration failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Config: %s\n", cfgOut)

		fmt.Println("Migrating OpenClaw sessions...")
		n, err := migrate.Run("", "")
		if err != nil {
			fmt.Fprintf(os.Stderr, "session migration failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Done. Migrated %d session(s).\n", n)
		os.Exit(0)
	}

	// Auto-migrate config on first run if ~/.gopherclaw/config.json doesn't exist
	if *cfgPath == "" {
		if cfgOut, err := migrate.MigrateConfig("", ""); err == nil {
			fmt.Printf("auto-migrated config → %s\n", cfgOut)
		}
	}

	// Load config
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Auto-migrate config on version change (REQ-036)
	if err := config.AutoMigrate(cfg.Path, version); err != nil {
		fmt.Fprintf(os.Stderr, "config auto-migration warning: %v\n", err)
		// Non-fatal: continue with the existing config
	}

	// Ensure gateway auth token — generates and persists one if not configured.
	token, generated, authErr := cfg.EnsureAuth()

	// Init logger + log broadcaster for SSE streaming
	logger, logBroadcaster := initLoggerAndBroadcaster(cfg)
	defer func() { _ = logger.Sync() }()

	logger.Infof("gopherclaw %s (commit: %s, built: %s) starting", version, commit, date)
	startTime := time.Now()

	// Non-blocking startup version check (REQ-030) + auto-update (REQ-033)
	// Channel notification is deferred until deliverers are wired (REQ-031).
	updateCh := updater.StartupCheck(context.Background(), version)
	var pendingUpdateVer atomic.Value // stores string
	go func() {
		if newVer, ok := <-updateCh; ok && newVer != "" {
			pendingUpdateVer.Store(newVer)
			if cfg.Update.AutoUpdate {
				logger.Infof("auto-update: new version %s available, updating...", newVer)
				updateCtx, updateCancel := context.WithTimeout(context.Background(), 120*time.Second)
				defer updateCancel()
				installed, err := updater.Update(updateCtx, version)
				if err != nil {
					logger.Errorf("auto-update failed: %v", err)
				} else {
					logger.Infof("auto-update: updated to %s — restart the service to use the new version", installed)
				}
			} else {
				logger.Infof("update available: %s → %s (run 'gopherclaw update')", version, newVer)
			}
		}
	}()

	if generated {
		if authErr != nil {
			logger.Warnf("gateway: generated auth token but could not save to %s: %v", cfg.Path, authErr)
			logger.Warnf("gateway: add this to config.json → gateway.auth.token: %q", token)
		} else {
			logger.Infof("gateway: no auth token was configured — generated and saved to %s", cfg.Path)
			logger.Infof("gateway: auth token: %s", token)
		}
	}

	// Auth manager
	authMgr, err := auth.New()
	if err != nil {
		logger.Fatalf("auth init: %v", err)
	}

	if *check {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		tok, err := authMgr.GetToken(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "auth check failed: %v\n", err)
			os.Exit(1)
		}
		redacted := "****"
		if len(tok) >= 4 {
			redacted = "****" + tok[len(tok)-4:]
		}
		fmt.Printf("OK — copilot token: %s\n", redacted)
		fmt.Printf("config: primary model = %s\n", cfg.Agents.Defaults.Model.Primary)
		fmt.Printf("config: gateway port = %d\n", cfg.Gateway.Port)
		authMode := cfg.Gateway.Auth.Mode
		if authMode == "" {
			authMode = "token"
		}
		fmt.Printf("config: gateway auth mode = %s\n", authMode)
		if cfg.Gateway.Auth.Token != "" {
			t := cfg.Gateway.Auth.Token
			fmt.Printf("config: gateway auth token = ****%s\n", t[max(0, len(t)-4):])
		}
		os.Exit(0)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Start proactive token refresher
	authMgr.StartRefresher(ctx)

	// Model router — build provider registry then attach to router
	providers := models.BuildProviders(logger.Named("models"), cfg, authMgr)
	logger.Infof("providers: %v", models.ProviderNames(providers))
	router := models.NewRouter(
		logger.Named("models"),
		providers,
		cfg.Agents.Defaults.Model.Primary,
		cfg.Agents.Defaults.Model.Fallbacks,
	)
	router.Cooldowns = models.NewCooldown(models.CooldownConfig{})

	// Lifecycle hook bus
	hookBus := hooks.New(logger.Named("hooks"))

	// Sessions
	home, _ := os.UserHomeDir()
	agentDef := cfg.DefaultAgent()
	agentDir := filepath.Join(home, ".gopherclaw", "agents", agentDef.ID)
	sessDir := filepath.Join(agentDir, "sessions")
	ttl := parseTTL(cfg.Agents.Defaults.ContextPruning.TTL)
	sessionMgr, err := session.New(logger.Named("session"), sessDir, ttl)
	if err != nil {
		logger.Fatalf("session init: %v", err)
	}

	// Skills — use Manager for hot-reload and runtime enable/disable (REQ-100, REQ-101)
	workspace := cfg.Agents.Defaults.Workspace
	skillStatePath := filepath.Join(home, ".gopherclaw", "state", "skill-states.json")
	skillLogger := logger.Named("skills")
	skillMgr, err := skills.NewManager(skillLogger, workspace, skillStatePath)
	if err != nil {
		logger.Warnf("skills manager init: %v", err)
		// Fallback: create a minimal manager with empty skills
		skillMgr, _ = skills.NewManager(skillLogger, "", "")
	}
	skillList := skillMgr.EnabledSkills()
	logger.Infof("loaded %d skills (%d enabled)", skillMgr.Count(), len(skillList))

	wsMDs := skills.LoadWorkspaceMDs(workspace)
	logger.Infof("loaded %d workspace docs", len(wsMDs))

	// Inject env vars from config into process environment
	for k, v := range cfg.Env {
		if err := os.Setenv(k, v); err != nil {
			logger.Warnf("setenv %s: %v", k, err)
		} else {
			logger.Infof("env: set %s", k)
		}
	}
	envMap := cfg.Env

	// Start session idle/daily reset loops
	atHour := 4
	if cfg.Session.Reset.AtHour != nil {
		atHour = *cfg.Session.Reset.AtHour
	}
	sessionMgr.StartResetLoop(ctx, cfg.Session.Reset.Mode, atHour, cfg.Session.IdleMinutes, logger.Infof)

	// Session reaper — clean orphaned JSONL files hourly (older than 7 days)
	go sessionMgr.StartReapLoop(ctx, 1*time.Hour, 7*24*time.Hour)

	// Configure token-based pruning on session manager
	sessionMgr.SetPruning(session.PruningPolicy{
		HardClearRatio:     cfg.Agents.Defaults.ContextPruning.HardClearRatio,
		ModelMaxTokens:     cfg.Agents.Defaults.ContextPruning.ModelMaxTokens,
		KeepLastAssistants: cfg.Agents.Defaults.ContextPruning.KeepLastAssistants,
		SoftTrimRatio:      cfg.Agents.Defaults.SoftTrimRatio,
		SurgicalPruning:    cfg.Agents.Defaults.ContextPruning.IsSurgicalPruning(),
		CacheTTL:           time.Duration(cfg.Agents.Defaults.ContextPruning.CacheTTLSeconds) * time.Second,
	})

	// Agent — build base tool list, then add delegate tool with multi-agent registry
	confirmMgr := tools.NewConfirmManager()
	toolList := agent.DefaultTools(cfg, workspace, envMap)

	// Wire confirm manager to exec tool for destructive command prompts (REQ-061)
	for _, t := range toolList {
		if et, ok := t.(*tools.ExecTool); ok {
			et.Confirmer = confirmMgr
			et.ConfirmTimeout = time.Duration(cfg.Tools.Exec.ConfirmTimeoutSec) * time.Second
		}
	}

	// Browser pool (optional)
	var browserPool *tools.BrowserPool
	if cfg.Tools.Browser.Enabled {
		browserPool = tools.NewBrowserPool(cfg.Tools.Browser.IsHeadless(), cfg.Tools.Browser.ChromePath, cfg.Tools.Browser.NoSandbox)
		toolList = append(toolList, &tools.BrowserTool{Pool: browserPool})
		logger.Infof("browser: enabled (headless=%v)", cfg.Tools.Browser.IsHeadless())
	}

	// Eidetic memory integration (optional)
	var eideticClient eidetic.Client
	var embedClient *embeddings.Client
	if cfg.Eidetic.Enabled {
		ec := eidetic.New(eidetic.Config{
			BaseURL:         cfg.Eidetic.BaseURL,
			APIKey:          cfg.Eidetic.APIKey,
			AgentID:         cfg.Eidetic.AgentID,
			RecentLimit:     cfg.Eidetic.RecentLimit,
			SearchLimit:     cfg.Eidetic.SearchLimit,
			SearchThreshold: cfg.Eidetic.SearchThreshold,
			TimeoutSeconds:  cfg.Eidetic.TimeoutSeconds,
		})
		retryQueuePath := filepath.Join(home, ".gopherclaw", "eidetic-retry-queue.json")
		rc := eidetic.NewRetryClient(ec, logger.Named("eidetic"), retryQueuePath)
		go rc.StartRetryLoop(ctx)

		healthCtx, healthCancel := context.WithTimeout(ctx, 5*time.Second)
		if err := ec.Health(healthCtx); err != nil {
			logger.Warnf("eidetic: health check failed, disabling integration: %v", err)
			// Still set retry client so queued appends can drain when service recovers.
			eideticClient = rc
		} else {
			eideticClient = rc
			logger.Infof("eidetic: connected to %s (agentID=%q, recentLimit=%d)",
				cfg.Eidetic.BaseURL, cfg.Eidetic.AgentID, cfg.Eidetic.RecentLimit)

			// Embeddings client for hybrid search (optional)
			if ecfg := cfg.Eidetic.Embeddings; ecfg.Enabled && ecfg.Model != "" {
				embedClient = embeddings.New(embeddings.Config{
					Provider:   ecfg.Provider,
					Model:      ecfg.Model,
					BaseURL:    ecfg.BaseURL,
					APIKey:     ecfg.APIKey,
					Dimensions: ecfg.Dimensions,
				})
				logger.Infof("eidetic: embeddings enabled (provider=%q, model=%q)", ecfg.Provider, ecfg.Model)
			}

			toolList = append(toolList, &tools.EideticSearchTool{
				Client:     eideticClient,
				Embeddings: embedClient,
				Limit:      cfg.Eidetic.SearchLimit,
				Threshold:  cfg.Eidetic.SearchThreshold,
			})
			toolList = append(toolList, &tools.EideticAppendTool{
				Client:     eideticClient,
				Embeddings: embedClient,
				AgentID:    cfg.Eidetic.AgentID,
			})
		}
		healthCancel()
	}

	// Build agent registry (subagents, delegate tool, orchestrators)
	ag, delegateTool, dispatchTools := buildAgents(logger, cfg, agentDef, router, sessionMgr, skillList, wsMDs, workspace, toolList, eideticClient, embedClient, hookBus)

	// Select primary agent engine: "claude-cli" uses StreamingCLIAgent,
	// anything else (default "router") uses the standard Agent.
	var primaryAg agent.PrimaryAgent = ag
	if cfg.Agents.Defaults.Engine == "claude-cli" {
		cliCfg := cfg.Agents.Defaults.CLIEngine
		ttl := time.Duration(cliCfg.IdleTTLSec) * time.Second
		sca, err := agent.NewStreamingCLIAgent(logger.Named("claude-cli"), agent.StreamingCLIConfig{
			Command:      cliCfg.Command,
			Model:        cliCfg.Model,
			MCPConfig:    cliCfg.MCPConfig,
			SystemPrompt: cliCfg.SystemPrompt,
			ExtraArgs:    cliCfg.ExtraArgs,
			IdleTTL:      ttl,
		})
		if err != nil {
			logger.Fatalf("claude-cli engine: %v", err)
		}
		defer sca.Close()
		primaryAg = sca
		logger.Infof("engine: claude-cli (command=%s, model=%s)", sca.ResolveModel(""), cliCfg.Command)
	} else {
		logger.Infof("engine: router (primary=%s)", cfg.Agents.Defaults.Model.Primary)
	}

	// Cron manager
	cronMgr := cron.New(logger.Named("cron"), agentDir, makeCronRunner(logger.Named("cron"), primaryAg, cfg))

	// Auto-migrate jobs.json from OpenClaw if missing or stale
	if dst, copied, err := migrate.MigrateJobsFile("", ""); err != nil {
		logger.Warnf("cron: jobs.json migration failed: %v", err)
	} else if copied {
		logger.Infof("cron: migrated jobs.json → %s", dst)
	}

	// Load full-format jobs.json from ~/.gopherclaw/cron/jobs.json
	jobsFile := filepath.Join(home, ".gopherclaw", "cron", "jobs.json")
	if err := cronMgr.LoadJobsFile(jobsFile); err != nil {
		logger.Warnf("cron: failed to load jobs.json: %v", err)
	} else {
		jobs := cronMgr.List()
		logger.Infof("cron: %d job(s) loaded from %s", len(jobs), jobsFile)
		for _, j := range jobs {
			status := "disabled"
			if j.Enabled {
				status = "enabled"
			}
			logger.Infof("cron:   %-40s [%s] %s", j.DisplayName(), j.DisplaySchedule(), status)
		}
	}

	// Task queue manager
	taskFilePath := filepath.Join(home, ".gopherclaw", "tasks.json")
	tqCfg := cfg.TaskQueue
	taskMgr := taskqueue.New(logger.Named("taskqueue"), taskFilePath, taskqueue.Config{
		MaxConcurrent:    tqCfg.MaxConcurrent,
		ResultRetention:  time.Duration(tqCfg.ResultRetentionM) * time.Minute,
		ProgressThrottle: time.Duration(tqCfg.ProgressThrottleS) * time.Second,
	})
	defer taskMgr.Shutdown()

	// Wire task manager into delegate and dispatch tools
	delegateTool.TaskMgr = taskMgr
	asyncAgents := make(map[string]bool)
	for _, def := range cfg.Agents.List {
		if def.Async {
			asyncAgents[def.ID] = true
		}
	}
	delegateTool.AsyncAgents = asyncAgents
	for _, dt := range dispatchTools {
		dt.TaskMgr = taskMgr
	}

	// Flush session metadata on shutdown
	defer sessionMgr.FlushSave()

	// Exec tool cleanup on shutdown
	for _, t := range toolList {
		if et, ok := t.(*tools.ExecTool); ok && et.Sandbox != nil && et.Sandbox.Enabled {
			defer et.Cleanup()
		}
	}

	// Browser cleanup on shutdown
	if browserPool != nil {
		defer browserPool.CloseAll()
	}

	// HTTP gateway — created early so channels can be wired as deliverers
	gw := gateway.New(logger, cfg, primaryAg, sessionMgr, cronMgr, taskMgr, toolList, logBroadcaster)
	gw.SetVersion(version)

	// Wire skill manager to gateway for dashboard enable/disable (REQ-101)
	gw.SetSkillManager(
		func() []gateway.SkillInfo {
			var out []gateway.SkillInfo
			for _, s := range skillMgr.Skills() {
				out = append(out, gateway.SkillInfo{
					Name: s.Name, Description: s.Description,
					Origin: s.Origin, Enabled: s.Enabled,
					Verified: s.Verified,
				})
			}
			return out
		},
		func(name string, enabled bool) bool {
			return skillMgr.SetEnabled(name, enabled)
		},
	)

	// Channel bots — created and registered with gateway/confirmMgr
	bots := initChannelBots(logger, cfg, primaryAg, sessionMgr, cronMgr, confirmMgr, gw)
	tgBot, dcBot, slBot := bots.tg, bots.dc, bots.sl

	// Find NotifyUserTool so we can wire announcers
	var notifyTool *tools.NotifyUserTool
	for _, t := range toolList {
		if nt, ok := t.(*tools.NotifyUserTool); ok {
			notifyTool = nt
			break
		}
	}

	// Wire channel bots as deliverers and announcers
	wireDeliverers(tgBot, dcBot, slBot, cronMgr, taskMgr, skillMgr, version, startTime, delegateTool, dispatchTools, notifyTool)

	// Deliver update notification to channel bots after startup (REQ-031).
	// Uses a short delay to let bots connect first.
	go func() {
		time.Sleep(10 * time.Second)
		if ver, ok := pendingUpdateVer.Load().(string); ok && ver != "" {
			msg := fmt.Sprintf("GopherClaw update available: %s → %s\nRun `gopherclaw update` to upgrade.", version, ver)
			if tgBot != nil {
				tgBot.SendToAllPaired(msg)
			}
			if dcBot != nil {
				dcBot.SendToAllPaired(msg)
			}
			if slBot != nil {
				slBot.SendToAllPaired(msg)
			}
		}
	}()

	// Heartbeat runner (periodic agent turns)
	var hbRunner *heartbeat.Runner
	if cfg.Agents.Defaults.Heartbeat.HeartbeatEnabled() {
		var hbDeliverers []heartbeat.Deliverer
		if tgBot != nil {
			hbDeliverers = append(hbDeliverers, tgBot)
		}
		if dcBot != nil {
			hbDeliverers = append(hbDeliverers, dcBot)
		}
		if slBot != nil {
			hbDeliverers = append(hbDeliverers, slBot)
		}
		hbRunner = heartbeat.NewRunner(heartbeat.RunnerOpts{
			Logger: logger.Named("heartbeat"),
			Agent:  primaryAg,
			CfgFn: func() config.HeartbeatConfig {
				return ag.GetConfig().Agents.Defaults.Heartbeat
			},
			UserTZFn: func() string {
				return ag.GetConfig().Agents.Defaults.UserTimezone
			},
			ResolveAlias: cfg.ResolveModelAlias,
			Workspace:    workspace,
			Deliverers:   hbDeliverers,
		})
		logger.Infof("heartbeat: enabled (every=%s, target=%s, lightContext=%v)",
			cfg.Agents.Defaults.Heartbeat.Every,
			cfg.Agents.Defaults.Heartbeat.Target,
			cfg.Agents.Defaults.Heartbeat.LightContext)
	}

	// Coordinated service lifecycle via errgroup.
	// If any service returns an error, the errgroup context is cancelled,
	// which signals all other services to shut down.
	eg, egCtx := errgroup.WithContext(ctx)

	// HTTP gateway
	eg.Go(func() error { return gw.Start(egCtx) })

	// Channel bots
	if tgBot != nil {
		eg.Go(func() error { return tgBot.Start(egCtx) })
	}
	if dcBot != nil {
		eg.Go(func() error { return dcBot.Start(egCtx) })
	}
	if slBot != nil {
		eg.Go(func() error { return slBot.Start(egCtx) })
	}

	// Heartbeat runner
	if hbRunner != nil {
		eg.Go(func() error { return hbRunner.Start(egCtx) })
	}

	// Cron scheduler (after deliverers are wired)
	eg.Go(func() error { return cronMgr.Start(egCtx) })

	// Task queue prune loop
	eg.Go(func() error { taskMgr.StartPruneLoop(egCtx); return nil })

	// Skills hot-reload via fsnotify (REQ-100)
	eg.Go(func() error { return skillMgr.Watch(egCtx, 2*time.Second) })

	// Config hot-reload via fsnotify
	if cfg.Gateway.Reload.Mode == "hybrid" || cfg.Gateway.Reload.Mode == "fsnotify" {
		debounceMs := cfg.Gateway.Reload.DebounceMs
		if debounceMs <= 0 {
			debounceMs = 500
		}
		rd := &reloadDeps{
			logger:       logger.Named("reload"),
			envMap:       envMap,
			prevChannels: cfg.Channels,
			sessionMgr:   sessionMgr,
			ag:           ag,
			bots:         bots,
		}
		eg.Go(func() error {
			return reload.Watch(logger.Named("reload"), egCtx, cfg.Path, time.Duration(debounceMs)*time.Millisecond, func(newCfg *config.Root) {
				rd.onConfigReloaded(egCtx, newCfg)
			})
		})
	}

	logger.Infof("gopherclaw ready — press Ctrl+C to stop")
	hookBus.Emit(ctx, hooks.Event{Type: hooks.GatewayStarted})
	if err := eg.Wait(); err != nil {
		logger.Errorf("service exited with error: %v", err)
	}
	hookBus.Emit(context.Background(), hooks.Event{Type: hooks.GatewayStopped})
	logger.Infof("shutting down...")
}

func parseTTL(s string) time.Duration {
	if s == "" {
		return time.Hour
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Hour
	}
	return d
}

// buildAgents creates the main agent, delegate tool, and orchestrator dispatch
// tools. It builds the full agent registry (subagents, CLI agents, orchestrators)
// and returns the primary agent, delegate tool, and dispatch tools.
// eideticClient and embedClient may be nil when the integrations are disabled.
func buildAgents(
	logger *zap.SugaredLogger,
	cfg *config.Root,
	agentDef *config.AgentDef,
	router *models.Router,
	sessionMgr *session.Manager,
	skillList []skills.Skill,
	wsMDs map[string]string,
	workspace string,
	toolList []agent.Tool,
	eideticClient eidetic.Client,
	embedClient *embeddings.Client,
	hookBus *hooks.Bus,
) (*agent.Agent, *agent.DelegateTool, []*agent.DispatchTool) {
	agentRegistry := make(map[string]agent.Chatter)
	var orchestratorDefs []*config.AgentDef
	var allAgents []*agent.Agent // collect for eidetic wiring

	for i := range cfg.Agents.List {
		def := &cfg.Agents.List[i]
		if def.ID == agentDef.ID {
			continue
		}
		if def.ID == "orchestrator" {
			orchestratorDefs = append(orchestratorDefs, def)
			continue
		}
		if def.CLICommand != "" {
			timeout := time.Duration(cfg.Agents.Defaults.TimeoutSeconds) * time.Second
			cli := agent.NewCLIAgent(def.ID, def.CLICommand, def.CLIArgs, timeout)
			agentRegistry[def.ID] = cli
			logger.Infof("subagent: registered CLI agent %q → %s %v", def.ID, cli.Command(), def.CLIArgs)
			if _, err := os.Stat(cli.Command()); err != nil {
				logger.Warnf("subagent: %q command %q not found — invocations will fail", def.ID, def.CLICommand)
			}
		} else {
			subAg := agent.New(logger.Named(def.ID), cfg, def, router, sessionMgr, skillList, wsMDs, workspace, toolList)
			agentRegistry[def.ID] = subAg
			allAgents = append(allAgents, subAg)
			logger.Infof("subagent: registered %q", def.ID)
		}
	}

	delegateTool := &agent.DelegateTool{Agents: agentRegistry, MaxDepth: 5, MainAgentID: agentDef.ID, DefaultModel: cfg.Agents.Defaults.Subagents.Model, Logger: logger.Named("delegate")}
	toolList = append(toolList, delegateTool)

	ag := agent.New(logger.Named(agentDef.ID), cfg, agentDef, router, sessionMgr, skillList, wsMDs, workspace, toolList)
	agentRegistry[agentDef.ID] = ag
	allAgents = append(allAgents, ag)

	var dispatchTools []*agent.DispatchTool
	for _, def := range orchestratorDefs {
		maxC := def.MaxConcurrent
		if maxC <= 0 {
			maxC = cfg.Agents.Defaults.Subagents.MaxConcurrent
		}
		dispatchTool := &agent.DispatchTool{
			Agents:          agentRegistry,
			MaxConcurrent:   maxC,
			ProgressUpdates: def.ProgressUpdates,
			MainAgentID:     agentDef.ID,
			Logger:          logger.Named("dispatch"),
		}
		dispatchTools = append(dispatchTools, dispatchTool)
		orchTools := make([]agent.Tool, len(toolList)+1)
		copy(orchTools, toolList)
		orchTools[len(toolList)] = dispatchTool
		orchAg := agent.New(logger.Named(def.ID), cfg, def, router, sessionMgr, skillList, wsMDs, workspace, orchTools)
		agentRegistry[def.ID] = orchAg
		allAgents = append(allAgents, orchAg)
		logger.Infof("subagent: registered orchestrator %q (maxConcurrent=%d, progressUpdates=%v)", def.ID, maxC, def.ProgressUpdates)
	}

	// Wire Eidetic and embeddings into all Go-native agents (CLI agents excluded
	// — they manage their own memory independently).
	if eideticClient != nil {
		for _, a := range allAgents {
			a.SetEidetic(eideticClient)
			if embedClient != nil {
				a.SetEmbeddings(embedClient)
			}
		}
	}

	// Wire lifecycle hook bus into all Go-native agents.
	for _, a := range allAgents {
		a.Hooks = hookBus
	}

	return ag, delegateTool, dispatchTools
}

// wireDeliverers connects channel bots to the cron manager (for result delivery)
// and to delegate/dispatch tools (for async announcements).
func wireDeliverers(
	tgBot *telegram.Bot,
	dcBot *discord.Bot,
	slBot *slackch.Bot,
	cronMgr *cron.Manager,
	taskMgr *taskqueue.Manager,
	skillMgr *skills.Manager,
	version string,
	startTime time.Time,
	delegateTool *agent.DelegateTool,
	dispatchTools []*agent.DispatchTool,
	notifyTool *tools.NotifyUserTool,
) {
	if tgBot != nil {
		cronMgr.AddDeliverer(tgBot)
		taskMgr.AddAnnouncer(tgBot)
		tgBot.SetTaskManager(taskMgr)
		tgBot.SetSkillManager(skillMgr)
		tgBot.SetVersion(version)
		tgBot.SetStartTime(startTime)
		delegateTool.Announcers = append(delegateTool.Announcers, tgBot)
		for _, dt := range dispatchTools {
			dt.Announcers = append(dt.Announcers, tgBot)
		}
		if notifyTool != nil {
			notifyTool.Announcers = append(notifyTool.Announcers, tgBot)
		}
	}
	if dcBot != nil {
		cronMgr.AddDeliverer(dcBot)
		taskMgr.AddAnnouncer(dcBot)
		dcBot.SetTaskManager(taskMgr)
		for _, dt := range dispatchTools {
			dt.Announcers = append(dt.Announcers, dcBot)
		}
		if notifyTool != nil {
			notifyTool.Announcers = append(notifyTool.Announcers, dcBot)
		}
	}
	if slBot != nil {
		cronMgr.AddDeliverer(slBot)
		taskMgr.AddAnnouncer(slBot)
		slBot.SetTaskManager(taskMgr)
		for _, dt := range dispatchTools {
			dt.Announcers = append(dt.Announcers, slBot)
		}
		if notifyTool != nil {
			notifyTool.Announcers = append(notifyTool.Announcers, slBot)
		}
	}
}

// runSecurityAudit runs the security audit CLI subcommand (REQ-440).
func runSecurityAudit() {
	deep := false
	cfgPath := ""
	for _, arg := range os.Args[2:] {
		switch arg {
		case "--deep":
			deep = true
		case "audit":
			// skip subcommand word
		default:
			if cfgPath == "" {
				cfgPath = arg
			}
		}
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	report := security.RunAudit(cfg, security.AuditOpts{Deep: deep})
	fmt.Print(security.FormatReport(report))

	if report.Summary.Critical > 0 {
		os.Exit(2) // non-zero exit for CI integration
	}
}
