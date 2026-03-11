package main

// startup.go – focused helpers extracted from the monolithic main() function.
// Each function handles one phase of startup initialisation.

import (
	"context"
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/EMSERO/gopherclaw/internal/agent"
	"github.com/EMSERO/gopherclaw/internal/channels/discord"
	slackch "github.com/EMSERO/gopherclaw/internal/channels/slack"
	"github.com/EMSERO/gopherclaw/internal/channels/telegram"
	"github.com/EMSERO/gopherclaw/internal/config"
	"github.com/EMSERO/gopherclaw/internal/cron"
	"github.com/EMSERO/gopherclaw/internal/gateway"
	"github.com/EMSERO/gopherclaw/internal/log"
	"github.com/EMSERO/gopherclaw/internal/session"
	"github.com/EMSERO/gopherclaw/internal/tools"
)

// ---------------------------------------------------------------------------
// Logger
// ---------------------------------------------------------------------------

// initLoggerAndBroadcaster sets up the structured logger and attaches an SSE
// log broadcaster for the control UI.  It returns the logger and broadcaster.
func initLoggerAndBroadcaster(cfg *config.Root) (*zap.SugaredLogger, *gateway.LogBroadcaster) {
	logger, err := log.Init(cfg.Logging.Level, cfg.Logging.ConsoleLevel, cfg.Logging.File)
	if err != nil {
		fatalf("failed to init logger: %v", err)
	}

	lb := gateway.NewLogBroadcaster()
	logger = log.AddCore(logger, zapcore.NewCore(
		zapcore.NewJSONEncoder(zapcore.EncoderConfig{
			TimeKey:      "ts",
			LevelKey:     "level",
			MessageKey:   "msg",
			CallerKey:    "caller",
			EncodeTime:   zapcore.ISO8601TimeEncoder,
			EncodeLevel:  zapcore.LowercaseLevelEncoder,
			EncodeCaller: zapcore.ShortCallerEncoder,
		}),
		lb,
		zapcore.DebugLevel,
	))

	if cfg.Logging.RedactSensitive != "" {
		logger.Infof("logging: redactSensitive=%q applied (tool output suppressed from logs)", cfg.Logging.RedactSensitive)
	}
	return logger, lb
}

// fatalf prints to stderr and exits — used before the logger is initialised.
func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// ---------------------------------------------------------------------------
// Cron runner
// ---------------------------------------------------------------------------

// makeCronRunner returns a cron.RunFunc that drives agent.Chat/ChatLight for
// scheduled jobs.  Extracted to avoid a 30-line closure body inside main().
func makeCronRunner(logger *zap.SugaredLogger, ag *agent.Agent, cfg *config.Root) cron.RunFunc {
	return func(cronCtx context.Context, job *cron.Job) cron.RunResult {
		sessionKey := job.EffectiveSessionKey()
		instruction := job.EffectiveInstruction()

		// Per-job model override; resolve short aliases (e.g. "sonnet" → full model ID).
		if model := job.EffectiveModel(); model != "" {
			ag.SetSessionModel(sessionKey, cfg.ResolveModelAlias(model))
			defer ag.ClearSessionModel(sessionKey)
		}

		// Per-job timeout.
		if timeout := job.EffectiveTimeout(); timeout > 0 {
			var cancel context.CancelFunc
			cronCtx, cancel = context.WithTimeout(cronCtx, timeout)
			defer cancel()
		}

		// Light context: use minimal bootstrap (HEARTBEAT.md only).
		var resp agent.Response
		var err error
		if job.LightContext || cfg.Agents.Defaults.Heartbeat.LightContext {
			resp, err = ag.ChatLight(cronCtx, sessionKey, instruction)
		} else {
			resp, err = ag.Chat(cronCtx, sessionKey, instruction)
		}
		if err != nil {
			logger.Errorf("cron job %s (%s): %v", job.DisplayName(), sessionKey, err)
			return cron.RunResult{Err: err}
		}
		logger.Infof("cron job %s (%s): %s", job.DisplayName(), sessionKey, resp.Text)
		return cron.RunResult{Text: resp.Text}
	}
}

// ---------------------------------------------------------------------------
// Channel bots
// ---------------------------------------------------------------------------

// channelBots groups the three optional channel bot handles so callers don't
// need to juggle three independent nil-checked variables.
type channelBots struct {
	tg *telegram.Bot
	dc *discord.Bot
	sl *slackch.Bot
}

// initChannelBots creates channel bots for every enabled channel, registers
// each with the gateway as a deliverer/channel, and wires the confirm manager.
func initChannelBots(
	logger *zap.SugaredLogger,
	cfg *config.Root,
	ag *agent.Agent,
	sessionMgr *session.Manager,
	cronMgr *cron.Manager,
	confirmMgr *tools.ConfirmManager,
	gw *gateway.Server,
) channelBots {
	var bots channelBots

	if cfg.Channels.Telegram.Enabled {
		b, err := telegram.New(logger.Named("telegram"), cfg.Channels.Telegram, cfg.Messages, cfg, ag, sessionMgr, cronMgr)
		if err != nil {
			logger.Fatalf("telegram init: %v", err)
		}
		gw.AddDeliverer(b)
		gw.AddChannel(b)
		confirmMgr.AddChannel(b)
		bots.tg = b
	}

	if cfg.Channels.Discord.Enabled {
		b, err := discord.New(logger.Named("discord"), cfg.Channels.Discord, cfg.Messages, cfg, ag, sessionMgr, cronMgr)
		if err != nil {
			logger.Fatalf("discord init: %v", err)
		}
		gw.AddDeliverer(b)
		gw.AddChannel(b)
		confirmMgr.AddChannel(b)
		bots.dc = b
	}

	if cfg.Channels.Slack.Enabled {
		b, err := slackch.New(logger.Named("slack"), cfg.Channels.Slack, cfg.Messages, cfg, ag, sessionMgr, cronMgr)
		if err != nil {
			logger.Fatalf("slack init: %v", err)
		}
		gw.AddDeliverer(b)
		gw.AddChannel(b)
		confirmMgr.AddChannel(b)
		bots.sl = b
	}

	return bots
}

// ---------------------------------------------------------------------------
// Hot-reload callback
// ---------------------------------------------------------------------------

// reloadDeps bundles the mutable state that the hot-reload callback needs to
// read and update.  Grouping it here avoids a 10-parameter closure.
type reloadDeps struct {
	logger       *zap.SugaredLogger
	envMap       map[string]string
	prevChannels config.Channels
	sessionMgr   *session.Manager
	ag           *agent.Agent
	bots         channelBots
}

// onConfigReloaded is the handler invoked by the fsnotify watcher whenever
// config.json changes.  egCtx is the errgroup's context.
func (d *reloadDeps) onConfigReloaded(egCtx context.Context, newCfg *config.Root) {
	// Unset env vars removed from config.
	for k := range d.envMap {
		if _, ok := newCfg.Env[k]; !ok {
			_ = os.Unsetenv(k)
			d.logger.Infof("env: unset %s (removed from config)", k)
		}
	}
	// Set new/updated env vars.
	for k, v := range newCfg.Env {
		_ = os.Setenv(k, v)
	}
	d.envMap = newCfg.Env

	// Update session pruning policy.
	d.sessionMgr.SetPruning(session.PruningPolicy{
		HardClearRatio:     newCfg.Agents.Defaults.ContextPruning.HardClearRatio,
		ModelMaxTokens:     newCfg.Agents.Defaults.ContextPruning.ModelMaxTokens,
		KeepLastAssistants: newCfg.Agents.Defaults.ContextPruning.KeepLastAssistants,
		SoftTrimRatio:      newCfg.Agents.Defaults.SoftTrimRatio,
		SurgicalPruning:    newCfg.Agents.Defaults.ContextPruning.IsSurgicalPruning(),
		CacheTTL:           time.Duration(newCfg.Agents.Defaults.ContextPruning.CacheTTLSeconds) * time.Second,
	})

	// Reconnect channel bots if tokens changed.
	if d.bots.tg != nil && newCfg.Channels.Telegram.BotToken != d.prevChannels.Telegram.BotToken {
		d.logger.Infof("hot-reload: telegram bot token changed, reconnecting...")
		if err := d.bots.tg.Reconnect(egCtx, newCfg.Channels.Telegram); err != nil {
			d.logger.Errorf("hot-reload: telegram reconnect: %v", err)
		}
	}
	if d.bots.dc != nil && newCfg.Channels.Discord.BotToken != d.prevChannels.Discord.BotToken {
		d.logger.Infof("hot-reload: discord bot token changed, reconnecting...")
		if err := d.bots.dc.Reconnect(egCtx, newCfg.Channels.Discord); err != nil {
			d.logger.Errorf("hot-reload: discord reconnect: %v", err)
		}
	}
	if d.bots.sl != nil && (newCfg.Channels.Slack.BotToken != d.prevChannels.Slack.BotToken ||
		newCfg.Channels.Slack.AppToken != d.prevChannels.Slack.AppToken) {
		d.logger.Infof("hot-reload: slack tokens changed, reconnecting...")
		if err := d.bots.sl.Reconnect(egCtx, newCfg.Channels.Slack); err != nil {
			d.logger.Errorf("hot-reload: slack reconnect: %v", err)
		}
	}
	d.prevChannels = newCfg.Channels

	// Update agent config and model routing.
	d.ag.UpdateConfig(newCfg)
	d.logger.Infof("hot-reload: updated config (model=%s)", newCfg.Agents.Defaults.Model.Primary)
}
