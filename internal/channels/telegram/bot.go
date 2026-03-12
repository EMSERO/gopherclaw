package telegram

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tele "gopkg.in/telebot.v3"

	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/agent"
	"github.com/EMSERO/gopherclaw/internal/channels/common"
	"github.com/EMSERO/gopherclaw/internal/commands"
	"github.com/EMSERO/gopherclaw/internal/config"
	"github.com/EMSERO/gopherclaw/internal/cron"
	"github.com/EMSERO/gopherclaw/internal/session"
	"github.com/EMSERO/gopherclaw/internal/skills"
	"github.com/EMSERO/gopherclaw/internal/taskqueue"
)

// Bot is the Telegram channel handler.
type Bot struct {
	logger   *zap.SugaredLogger
	bot      *tele.Bot
	cfg      config.TelegramConfig
	msgCfg   config.Messages
	fullCfg  *config.Root
	agent    agent.PrimaryAgent
	sessions *session.Manager
	cronMgr  *cron.Manager
	taskMgr  *taskqueue.Manager
	paired   map[int64]bool // senderId → paired
	pairCode string         // one-time code required by /pair

	mu              sync.Mutex
	queues          map[int64]*messageQueue // senderID → pending messages
	pendingConfirms map[string]chan bool    // confirmID → result channel (REQ-061)
	skillMgr        *skills.Manager         // for /status skill count
	version         string                  // for /status
	startTime       time.Time               // for /status uptime
	cancelBot       context.CancelFunc      // cancel function to stop the bot for reconnection
	connected       atomic.Bool
}

// SetTaskManager sets the task queue manager for /tasks and /cancel commands.
func (b *Bot) SetTaskManager(m *taskqueue.Manager) { b.taskMgr = m }

// SetSkillManager wires the skill manager so /status can report skill count.
func (b *Bot) SetSkillManager(m *skills.Manager) { b.skillMgr = m }

// SetVersion sets the binary version string for /status.
func (b *Bot) SetVersion(v string) { b.version = v }

// SetStartTime sets the process start time for /status uptime.
func (b *Bot) SetStartTime(t time.Time) { b.startTime = t }

// cmdCtx builds a commands.Ctx populated from the bot's state.
func (b *Bot) cmdCtx(sessionKey string) commands.Ctx {
	deps := b.cmdCtxDeps()
	return common.BuildCmdCtx(sessionKey, deps)
}

func (b *Bot) cmdCtxDeps() common.CmdCtxDeps {
	d := common.CmdCtxDeps{
		Agent:       b.agent,
		Sessions:    b.sessions,
		Config:      b.fullCfg,
		CronManager: b.cronMgr,
		TaskManager: b.taskMgr,
		Version:     b.version,
		StartTime:   b.startTime,
	}
	if b.skillMgr != nil {
		d.SkillCount = b.skillMgr.Count()
	}
	return d
}

// generatePairCode returns a cryptographically random 6-digit decimal string.
func generatePairCode() string { return common.GeneratePairCode() }

// messageQueue collects rapid-fire messages from one sender before processing.
type messageQueue struct {
	messages []queuedMessage
	timer    *time.Timer
	firstMsg tele.Context // the first message context (for replyToMode: "first")
}

type queuedMessage struct {
	text string
	ctx  tele.Context
}

// New creates and starts a Telegram bot.
func New(logger *zap.SugaredLogger, cfg config.TelegramConfig, msgCfg config.Messages, fullCfg *config.Root, ag agent.PrimaryAgent, sm *session.Manager, cronMgr *cron.Manager) (*Bot, error) {
	pref := tele.Settings{
		Token:  cfg.BotToken,
		Poller: &tele.LongPoller{Timeout: 30 * time.Second},
	}
	b, err := tele.NewBot(pref)
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}

	code := generatePairCode()
	logger.Infof("telegram: pairing code: %s  (DM the bot: /pair %s)", code, code)

	bot := &Bot{
		logger:          logger,
		bot:             b,
		cfg:             cfg,
		msgCfg:          msgCfg,
		fullCfg:         fullCfg,
		agent:           ag,
		sessions:        sm,
		cronMgr:         cronMgr,
		paired:          make(map[int64]bool),
		queues:          make(map[int64]*messageQueue),
		pairCode:        code,
		pendingConfirms: make(map[string]chan bool),
	}

	// Load existing paired users from state file
	bot.loadPairedUsers()

	// /new and /reset reset the session
	b.Handle("/new", bot.handleReset)
	b.Handle("/reset", bot.handleReset)
	b.Handle("/pair", bot.handlePair)
	b.Handle(tele.OnText, bot.handleText)
	b.Handle(tele.OnCallback, bot.handleCallback)
	b.Handle(tele.OnVoice, bot.handleIgnore)
	b.Handle(tele.OnPhoto, bot.handlePhoto)
	b.Handle(tele.OnDocument, bot.handleDocument)

	return bot, nil
}

// Start begins long-polling (blocks until context is cancelled).
func (b *Bot) Start(ctx context.Context) error {
	b.cancelExistingPoller()
	b.connected.Store(true)
	b.logger.Infof("telegram: bot started (@%s)", b.bot.Me.Username)
	childCtx, cancel := context.WithCancel(ctx)
	b.mu.Lock()
	b.cancelBot = cancel
	b.mu.Unlock()
	go func() {
		<-childCtx.Done()
		b.connected.Store(false)
		b.bot.Stop()
	}()
	b.bot.Start()
	return nil
}

// botRef returns the current *tele.Bot under the mutex, safe for concurrent use
// with Reconnect() which swaps b.bot.
func (b *Bot) botRef() *tele.Bot { b.mu.Lock(); defer b.mu.Unlock(); return b.bot }

// ChannelName returns the channel type name.
func (b *Bot) ChannelName() string { return "telegram" }

// IsConnected reports whether the bot is currently polling.
func (b *Bot) IsConnected() bool { return b.connected.Load() }

// Username returns the bot's Telegram username.
func (b *Bot) Username() string { return b.botRef().Me.Username }

// PairedCount returns the number of paired users.
func (b *Bot) PairedCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.paired)
}

// Reconnect stops the current bot and starts a new one with the updated token.
func (b *Bot) Reconnect(ctx context.Context, newCfg config.TelegramConfig) error {
	// Stop the current bot
	b.mu.Lock()
	if b.cancelBot != nil {
		b.cancelBot()
	}
	b.mu.Unlock()
	// Allow the old poller to drain
	time.Sleep(500 * time.Millisecond)

	pref := tele.Settings{
		Token:  newCfg.BotToken,
		Poller: &tele.LongPoller{Timeout: 30 * time.Second},
	}
	newBot, err := tele.NewBot(pref)
	if err != nil {
		return fmt.Errorf("create telegram bot: %w", err)
	}
	b.mu.Lock()
	b.bot = newBot
	b.cfg = newCfg
	b.mu.Unlock()

	// Re-register handlers
	newBot.Handle("/new", b.handleReset)
	newBot.Handle("/reset", b.handleReset)
	newBot.Handle("/pair", b.handlePair)
	newBot.Handle(tele.OnText, b.handleText)
	newBot.Handle(tele.OnCallback, b.handleCallback)
	newBot.Handle(tele.OnVoice, b.handleIgnore)
	newBot.Handle(tele.OnPhoto, b.handlePhoto)
	newBot.Handle(tele.OnDocument, b.handleDocument)

	go func() {
		if err := b.Start(ctx); err != nil {
			b.logger.Errorf("telegram: reconnect start: %v", err)
		}
	}()
	b.logger.Infof("telegram: reconnected with new token")
	return nil
}

// cancelExistingPoller makes a zero-timeout getUpdates call to Telegram so any
// competing long-poller on the same token receives a 409 Conflict and stops.
// This prevents the "Telegram bot token conflict" error when restarting while
// another instance is still running.
func (b *Bot) cancelExistingPoller() {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?timeout=0&offset=-1", b.cfg.BotToken)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		b.logger.Infof("telegram: cancel existing poller: %v", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	b.logger.Infof("telegram: sent getUpdates to evict any existing poller")
	time.Sleep(500 * time.Millisecond)
}

func pairedUsersFile() string { return common.PairedUsersPath("telegram") }

// loadPairedUsers reads existing paired users from the state file.
func (b *Bot) loadPairedUsers() {
	m := common.LoadPairedUsers(b.logger, "telegram")
	for idStr := range m {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			continue
		}
		b.paired[id] = true
		b.logger.Infof("telegram: loaded paired user %d", id)
	}
}

// savePairedUsers writes the current paired set to disk. Must be called with b.mu held.
func (b *Bot) savePairedUsers() {
	ids := make([]string, 0, len(b.paired))
	for id := range b.paired {
		ids = append(ids, strconv.FormatInt(id, 10))
	}
	if err := common.SavePairedUsers(b.logger, "telegram", ids); err != nil {
		b.logger.Warnf("telegram: failed to save paired users: %v", err)
	}
}

// sessionKeyFor returns the session key for a sender+chat, respecting session.scope.
func (b *Bot) sessionKeyFor(senderID, chatID int64) string {
	scope := ""
	agentID := "main"
	if b.fullCfg != nil {
		scope = b.fullCfg.Session.Scope
		agentID = b.fullCfg.DefaultAgent().ID
	}
	return common.SessionKey(agentID, "telegram", scope,
		strconv.FormatInt(senderID, 10), strconv.FormatInt(chatID, 10))
}

// matchesResetTrigger reports whether text exactly matches any configured reset trigger (case-insensitive).
func matchesResetTrigger(text string, triggers []string) bool {
	return common.MatchesResetTrigger(text, triggers)
}

func (b *Bot) handleText(c tele.Context) error {
	if !b.shouldRespond(c) {
		return nil
	}

	sender := c.Sender()
	text := c.Text()

	// Slash command routing (handles /model, /compact, /context, /export, /cron, etc.)
	// Note: /new, /reset, /pair are registered as dedicated handlers and won't reach here.
	if strings.HasPrefix(text, "/") {
		sessionKey := b.sessionKeyFor(sender.ID, c.Chat().ID)
		cmdCtx, cmdCancel := context.WithTimeout(context.Background(), 30*time.Second)
		result := commands.Handle(cmdCtx, text, b.cmdCtx(sessionKey))
		cmdCancel()
		if result.Handled {
			return sendLong(c, result.Text)
		}
	}

	// Reaction acknowledgment: react before processing
	b.ackReaction(c)

	// If debouncing is enabled, queue the message
	if b.msgCfg.Queue.Mode == "collect" && b.msgCfg.Queue.DebounceMs > 0 {
		b.enqueue(sender.ID, text, c)
		return nil
	}

	// No debouncing — process immediately
	return b.processMessages(sender.ID, []queuedMessage{{text: text, ctx: c}}, c)
}

// ackReaction reacts to a message with an emoji to acknowledge receipt.
func (b *Bot) ackReaction(c tele.Context) {
	scope := b.msgCfg.AckReactionScope
	if scope == "" {
		return
	}

	chat := c.Chat()
	isGroup := chat.Type == tele.ChatGroup || chat.Type == tele.ChatSuperGroup || chat.Type == tele.ChatChannel

	// "group-mentions" means only react in groups when mentioned
	if scope == "group-mentions" && !isGroup {
		return
	}
	// "all" means react everywhere

	// Use configured ack emoji (default: 👀)
	ackEmoji := b.cfg.AckEmoji
	if ackEmoji == "" {
		ackEmoji = "👀"
	}
	opts := tele.ReactionOptions{
		Reactions: []tele.Reaction{
			{Type: "emoji", Emoji: ackEmoji},
		},
	}
	if err := b.botRef().React(c.Chat(), c.Message(), opts); err != nil {
		b.logger.Infof("telegram: failed to set ack reaction: %v", err)
	}
}

// enqueue adds a message to the debounce queue for a sender.
func (b *Bot) enqueue(senderID int64, text string, c tele.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()

	q, ok := b.queues[senderID]
	if !ok {
		q = &messageQueue{
			firstMsg: c,
		}
		b.queues[senderID] = q
	}

	q.messages = append(q.messages, queuedMessage{text: text, ctx: c})

	// Cap check
	cap := b.msgCfg.Queue.Cap
	if cap <= 0 {
		cap = 20
	}
	if len(q.messages) >= cap {
		// Flush immediately at cap
		if q.timer != nil {
			q.timer.Stop()
		}
		b.flushLocked(senderID)
		return
	}

	// Reset debounce timer
	debounce := time.Duration(b.msgCfg.Queue.DebounceMs) * time.Millisecond
	if q.timer != nil {
		q.timer.Stop()
	}
	q.timer = time.AfterFunc(debounce, func() {
		b.mu.Lock()
		b.flushLocked(senderID)
		b.mu.Unlock()
	})
}

// flushLocked drains the queue for a sender and processes. Must hold b.mu.
func (b *Bot) flushLocked(senderID int64) {
	q, ok := b.queues[senderID]
	if !ok || len(q.messages) == 0 {
		return
	}

	msgs := q.messages
	firstCtx := q.firstMsg
	delete(b.queues, senderID)

	// Process asynchronously to release the lock
	go func() {
		defer func() {
			if r := recover(); r != nil {
				b.logger.Errorf("telegram: panic in processMessages: %v", r)
			}
		}()
		if err := b.processMessages(senderID, msgs, firstCtx); err != nil {
			b.logger.Errorf("telegram: process queued messages: %v", err)
		}
	}()
}

// configSnapshot returns a consistent copy of bot config fields. Must be called
// at the start of any goroutine that reads config to avoid races with Reconnect().
func (b *Bot) configSnapshot() (config.TelegramConfig, config.Messages) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg, b.msgCfg
}

// processMessages handles one or more collected messages for a sender.
func (b *Bot) processMessages(senderID int64, msgs []queuedMessage, replyCtx tele.Context) error {
	cfg, msgCfg := b.configSnapshot()

	// Combine all queued messages into one user text
	var combined strings.Builder
	for i, m := range msgs {
		if i > 0 {
			combined.WriteString("\n")
		}
		combined.WriteString(m.text)
	}
	text := combined.String()

	// Determine which message to reply to
	replyTo := replyCtx // default: reply to the trigger context
	if cfg.ReplyToMode == "first" && len(msgs) > 0 {
		replyTo = msgs[0].ctx
	}

	sessionKey := b.sessionKeyFor(senderID, replyCtx.Chat().ID)

	// Reset trigger check: if text matches a trigger, reset session and return
	if b.fullCfg != nil && matchesResetTrigger(text, b.fullCfg.Session.ResetTriggers) {
		b.sessions.Reset(sessionKey)
		return sendLong(replyTo, "Session cleared.")
	}

	if cfg.StreamMode == "partial" {
		return b.respondStreaming(replyTo, sessionKey, text, cfg, msgCfg)
	}
	return b.respondFull(replyTo, sessionKey, text, cfg, msgCfg)
}

func (b *Bot) respondFull(c tele.Context, sessionKey string, text string, cfg config.TelegramConfig, msgCfg config.Messages) error {
	_ = c.Notify(tele.Typing)

	if cfg.HistoryLimit > 0 {
		_ = b.sessions.TrimMessages(sessionKey, cfg.HistoryLimit)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	resp, err := b.agent.Chat(ctx, sessionKey, text)
	if err != nil {
		b.logger.Errorf("telegram: chat error: %v", err)
		return c.Reply(common.UserFacingError(err))
	}

	outText := resp.Text
	// Suppress NO_REPLY / empty responses (REQ-503)
	if common.IsSuppressible(outText) {
		return nil
	}
	if b.fullCfg != nil && b.fullCfg.Messages.Usage == "tokens" && resp.Usage.OutputTokens > 0 {
		outText += fmt.Sprintf("\n\n📊 ~%d in / ~%d out tokens", resp.Usage.InputTokens, resp.Usage.OutputTokens)
	}
	return sendLong(c, outText)
}

func (b *Bot) respondStreaming(c tele.Context, sessionKey string, text string, cfg config.TelegramConfig, msgCfg config.Messages) error {
	_ = c.Notify(tele.Typing)

	if cfg.HistoryLimit > 0 {
		_ = b.sessions.TrimMessages(sessionKey, cfg.HistoryLimit)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()

	// Keep the typing indicator alive throughout processing.
	// Telegram's typing status expires after ~5s, so refresh it periodically.
	typingDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-typingDone:
				return
			case <-ticker.C:
				_ = c.Notify(tele.Typing)
			}
		}
	}()

	// Track intermediate text blocks so we don't re-send them in the final message.
	var iterationTexts []string
	sendFn := func(s string) { _ = c.Send(s) }
	resp, err := b.agent.ChatStream(ctx, sessionKey, text, &agent.StreamCallbacks{
		// No OnChunk — accumulate silently; the full response is sent at the end.
		OnToolStart: common.NewToolNotifier(sendFn).OnToolStart,
		OnIterationText: func(block string) {
			// Send intermediate text blocks between tool iterations so the
			// user sees the agent's thinking/progress in real time.
			if block != "" {
				iterationTexts = append(iterationTexts, block)
				_ = sendLong(c, block)
			}
		},
	})
	close(typingDone)

	if err != nil {
		b.logger.Errorf("telegram: stream error: %v", err)
		return c.Reply(common.UserFacingError(err))
	}

	// Strip already-sent iteration text blocks from the final message to avoid duplication.
	finalText := resp.Text
	for _, block := range iterationTexts {
		finalText = strings.Replace(finalText, block, "", 1)
	}
	finalText = strings.TrimSpace(finalText)

	// Suppress NO_REPLY / empty responses (REQ-503)
	if common.IsSuppressible(finalText) {
		return nil
	}
	if finalText == "" {
		return nil
	}
	if b.fullCfg != nil && b.fullCfg.Messages.Usage == "tokens" && resp.Usage.OutputTokens > 0 {
		finalText += fmt.Sprintf("\n\n📊 ~%d in / ~%d out tokens", resp.Usage.InputTokens, resp.Usage.OutputTokens)
	}

	return sendLong(c, finalText)
}

func (b *Bot) handleReset(c tele.Context) error {
	if !b.shouldRespond(c) {
		return nil
	}
	sessionKey := b.sessionKeyFor(c.Sender().ID, c.Chat().ID)
	b.sessions.Reset(sessionKey)
	return c.Reply("Session cleared.")
}

func (b *Bot) validatePairCode(input string) bool {
	return common.ValidatePairCode(input, b.pairCode)
}

func (b *Bot) handlePair(c tele.Context) error {
	if !b.validatePairCode(c.Message().Payload) {
		return c.Reply("Invalid pairing code. Check the gopherclaw log for the code.")
	}
	b.mu.Lock()
	b.paired[c.Sender().ID] = true
	b.savePairedUsers()
	b.mu.Unlock()
	b.logger.Infof("telegram: paired user %d (%s)", c.Sender().ID, c.Sender().Username)
	return c.Reply(fmt.Sprintf("Paired! Your ID is %d.", c.Sender().ID))
}

// handleCallback handles inline button callback queries.
// The callback_data is injected as a user message: "callback_data: <value>"
func (b *Bot) handleCallback(c tele.Context) error {
	data := c.Callback().Data
	// Strip telebot's \f prefix if present — markup.Data() prepends it for
	// its unique-handler routing, and if that routing fails (e.g. the unique
	// string contains chars outside [-\w]), the raw \f-prefixed data reaches us.
	data = strings.TrimPrefix(data, "\f")
	if data == "" {
		return c.Respond()
	}

	// Acknowledge the callback
	if err := c.Respond(); err != nil {
		b.logger.Infof("telegram: callback respond error: %v", err)
	}

	// Check if this is a dangerous-command confirmation callback (REQ-061).
	if b.handleConfirmCallback(data) {
		return nil
	}

	sender := c.Sender()
	if sender == nil {
		return nil
	}

	// Inject callback data as a user message
	text := "callback_data: " + data
	sessionKey := b.sessionKeyFor(sender.ID, c.Chat().ID)

	cfg, msgCfg := b.configSnapshot()
	if cfg.StreamMode == "partial" {
		return b.respondStreaming(c, sessionKey, text, cfg, msgCfg)
	}
	return b.respondFull(c, sessionKey, text, cfg, msgCfg)
}

// handlePhoto processes photo messages by downloading the image and sending it
// to the agent as a vision (multi-content) message.
func (b *Bot) handlePhoto(c tele.Context) error {
	if !b.shouldRespond(c) {
		return nil
	}
	photo := c.Message().Photo
	if photo == nil {
		return nil
	}
	b.ackReaction(c)

	dataURL, err := b.downloadFileAsDataURL(photo.File)
	if err != nil {
		b.logger.Errorf("telegram: download photo: %v", err)
		return c.Reply("Failed to download image.")
	}

	caption := c.Message().Caption
	if caption == "" {
		caption = "What's in this image?"
	}

	sender := c.Sender()
	sessionKey := b.sessionKeyFor(sender.ID, c.Chat().ID)
	cfg, _ := b.configSnapshot()

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()

	_ = c.Notify(tele.Typing)
	resp, err := b.agent.ChatWithImages(ctx, sessionKey, caption, []string{dataURL})
	if err != nil {
		b.logger.Errorf("telegram: chat with image: %v", err)
		return c.Reply(common.UserFacingError(err))
	}
	if common.IsSuppressible(resp.Text) {
		return nil
	}
	return sendLong(c, resp.Text)
}

// handleDocument processes document messages. Only images are handled as vision
// inputs; other document types get a text description.
func (b *Bot) handleDocument(c tele.Context) error {
	if !b.shouldRespond(c) {
		return nil
	}
	doc := c.Message().Document
	if doc == nil {
		return nil
	}

	// Only handle image documents as vision inputs.
	if !strings.HasPrefix(doc.MIME, "image/") {
		caption := c.Message().Caption
		if caption == "" {
			caption = fmt.Sprintf("User sent a file: %s (%s, %d bytes)", doc.FileName, doc.MIME, doc.FileSize)
		} else {
			caption = fmt.Sprintf("%s\n\n[Attached file: %s (%s, %d bytes)]", caption, doc.FileName, doc.MIME, doc.FileSize)
		}
		// Process as regular text message.
		sender := c.Sender()
		return b.processMessages(sender.ID, []queuedMessage{{text: caption, ctx: c}}, c)
	}

	b.ackReaction(c)

	dataURL, err := b.downloadFileAsDataURL(doc.File)
	if err != nil {
		b.logger.Errorf("telegram: download document image: %v", err)
		return c.Reply("Failed to download image.")
	}

	caption := c.Message().Caption
	if caption == "" {
		caption = "What's in this image?"
	}

	sender := c.Sender()
	sessionKey := b.sessionKeyFor(sender.ID, c.Chat().ID)
	cfg, _ := b.configSnapshot()

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()

	_ = c.Notify(tele.Typing)
	resp, err := b.agent.ChatWithImages(ctx, sessionKey, caption, []string{dataURL})
	if err != nil {
		b.logger.Errorf("telegram: chat with document image: %v", err)
		return c.Reply(common.UserFacingError(err))
	}
	if common.IsSuppressible(resp.Text) {
		return nil
	}
	return sendLong(c, resp.Text)
}

// downloadFileAsDataURL downloads a Telegram file and returns a base64 data URL.
func (b *Bot) downloadFileAsDataURL(f tele.File) (string, error) {
	reader, err := b.botRef().File(&f)
	if err != nil {
		return "", fmt.Errorf("get file reader: %w", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(io.LimitReader(reader, 20<<20)) // 20MB limit
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}

	mime := http.DetectContentType(data)
	encoded := base64.StdEncoding.EncodeToString(data)
	return fmt.Sprintf("data:%s;base64,%s", mime, encoded), nil
}

func (b *Bot) handleIgnore(_ tele.Context) error { return nil }

func (b *Bot) shouldRespond(c tele.Context) bool {
	chat := c.Chat()
	sender := c.Sender()
	if sender == nil {
		return false
	}

	isGroup := chat.Type == tele.ChatGroup || chat.Type == tele.ChatSuperGroup || chat.Type == tele.ChatChannel

	if isGroup {
		// Resolve effective group policy.
		// If groupPolicy is set, it takes precedence over the legacy Groups["*"] setting.
		policy := b.cfg.GroupPolicy
		if policy == "" {
			// Legacy fallback: Groups["*"].RequireMention defaults to "mention"
			if gc, ok := b.cfg.Groups["*"]; ok && !gc.RequireMention {
				policy = "open"
			} else {
				policy = "mention"
			}
		}

		switch policy {
		case "disabled":
			return false
		case "open":
			return true
		case "allowlist":
			b.mu.Lock()
			ok := b.paired[sender.ID]
			b.mu.Unlock()
			return ok
		default: // "mention"
			return isMentioned(c.Text(), b.bot.Me.Username)
		}
	}

	// DM
	if b.cfg.DMPolicy == "pairing" {
		b.mu.Lock()
		ok := b.paired[sender.ID]
		b.mu.Unlock()
		if !ok {
			_ = c.Reply("Send /pair to start.")
			return false
		}
	}
	return true
}

func isMentioned(text, username string) bool {
	if username == "" {
		return false
	}
	return strings.Contains(text, "@"+username)
}

// sendLong sends a message, splitting if over Telegram's 4096 char limit.
// Each chunk is sent with retry + exponential backoff via common.RetrySend,
// with human-like pacing (800–2500ms) between consecutive chunks.
func sendLong(c tele.Context, text string) error {
	rc := common.RetryConfig{PlainTextOnFinal: true}
	parts := splitMessage(text, 4096)
	for i, part := range parts {
		// Human-like pacing between chunks.
		if i > 0 {
			time.Sleep(time.Duration(800+rand.Intn(1700)) * time.Millisecond)
		}
		idx := i
		if err := common.RetrySend(rc, part, func(t string) error {
			if idx == 0 {
				return c.Reply(t)
			}
			_, err := c.Bot().Send(c.Chat(), t)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

// SendTo sends a message to a specific Telegram chat by ID.
// Used by the system event endpoint to deliver agent responses.
// AnnounceToSession sends a message to the chat associated with a session key.
// Session keys have the format "<agentID>:telegram:<chatID>" (user scope),
// "<agentID>:telegram:channel:<chatID>" (channel scope), or
// "<agentID>:telegram:global" (global scope — not deliverable).
func (b *Bot) AnnounceToSession(sessionKey, text string) {
	parts := strings.Split(sessionKey, ":")
	// Accept 3-part user-scope keys ("main:telegram:12345") and
	// 4+-part channel/global keys ("main:telegram:channel:12345").
	if len(parts) < 3 {
		b.logger.Warnf("telegram: AnnounceToSession: key %q has <3 parts, skipping", sessionKey)
		return
	}
	// Find "telegram" at index 1 (3-part) or index 2 (4+-part).
	isTelegram := (len(parts) == 3 && parts[1] == "telegram") ||
		(len(parts) >= 4 && parts[2] == "telegram")
	if !isTelegram {
		b.logger.Debugf("telegram: AnnounceToSession: key %q not for telegram, skipping", sessionKey)
		return
	}
	// Last numeric segment is the chat ID
	idStr := parts[len(parts)-1]
	chatID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		b.logger.Errorf("telegram: AnnounceToSession: bad chat ID in key %q", sessionKey)
		return
	}
	b.logger.Debugf("telegram: AnnounceToSession: sending to chatID=%d textLen=%d", chatID, len(text))
	if err := b.SendTo(chatID, text); err != nil {
		b.logger.Errorf("telegram: AnnounceToSession %d: %v", chatID, err)
	}
}

func (b *Bot) SendTo(chatID int64, text string) error {
	bot := b.botRef()
	chat := &tele.Chat{ID: chatID}
	parts := splitMessage(text, 4096)
	for _, part := range parts {
		if _, err := bot.Send(chat, part); err != nil {
			return err
		}
	}
	return nil
}

// SendToAllPaired sends a message to all paired users.
// Used by the system event endpoint when no specific target is given.
func (b *Bot) SendToAllPaired(text string) {
	b.mu.Lock()
	ids := make([]int64, 0, len(b.paired))
	for id := range b.paired {
		ids = append(ids, id)
	}
	b.mu.Unlock()
	for _, id := range ids {
		if err := b.SendTo(id, text); err != nil {
			b.logger.Errorf("telegram: sendToAllPaired %d: %v", id, err)
		}
	}
}

// CanConfirm returns true if this bot can reach the given session key.
func (b *Bot) CanConfirm(sessionKey string) bool {
	return strings.Contains(sessionKey, ":telegram:")
}

// SendConfirmPrompt sends an inline-button confirmation prompt to the chat
// associated with the session key and blocks until the user responds or
// the context expires.
func (b *Bot) SendConfirmPrompt(ctx context.Context, sessionKey, command, pattern string) (bool, error) {
	parts := strings.Split(sessionKey, ":")
	if len(parts) < 3 {
		return false, fmt.Errorf("invalid session key: %s", sessionKey)
	}
	idStr := parts[len(parts)-1]
	chatID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return false, fmt.Errorf("bad chat ID in session key: %s", sessionKey)
	}

	// Truncate command for display
	display := command
	if len(display) > 200 {
		display = display[:200] + "..."
	}

	confirmID := fmt.Sprintf("confirm:%d:%d", chatID, time.Now().UnixNano())

	resultCh := make(chan bool, 1)
	b.mu.Lock()
	b.pendingConfirms[confirmID] = resultCh
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		delete(b.pendingConfirms, confirmID)
		b.mu.Unlock()
	}()

	// Send message with inline keyboard.
	// Use raw Btn with Data (not markup.Data) to avoid telebot's \f prefix
	// in callback_data — \f triggers telebot's unique-handler routing which
	// fails on colons in confirmID, causing callbacks to miss our handler.
	markup := &tele.ReplyMarkup{}
	btnYes := tele.Btn{Text: "Yes, execute", Data: confirmID + ":yes"}
	btnNo := tele.Btn{Text: "No, block", Data: confirmID + ":no"}
	markup.Inline(markup.Row(btnYes, btnNo))

	msg := fmt.Sprintf("⚠️ *Dangerous command detected*\n\nMatched pattern: `%s`\n\n```\n%s\n```\n\nAllow execution?", pattern, display)
	chat := &tele.Chat{ID: chatID}
	if _, err := b.botRef().Send(chat, msg, markup, tele.ModeMarkdown); err != nil {
		return false, fmt.Errorf("send confirm prompt: %w", err)
	}

	select {
	case confirmed := <-resultCh:
		return confirmed, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// handleConfirmCallback checks if a callback is a confirmation response and resolves it.
// Returns true if the callback was handled as a confirmation.
func (b *Bot) handleConfirmCallback(data string) bool {
	if !strings.HasPrefix(data, "confirm:") {
		return false
	}

	lastColon := strings.LastIndex(data, ":")
	if lastColon < 0 {
		return false
	}
	answer := data[lastColon+1:]
	confirmID := data[:lastColon]

	b.mu.Lock()
	ch, ok := b.pendingConfirms[confirmID]
	b.mu.Unlock()

	if !ok {
		return false
	}

	select {
	case ch <- (answer == "yes"):
	default:
	}
	return true
}

func splitMessage(text string, maxLen int) []string { return common.SmartChunk(text, maxLen) }
