package slack

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	slacklib "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/agent"
	"github.com/EMSERO/gopherclaw/internal/channels/common"
	"github.com/EMSERO/gopherclaw/internal/commands"
	"github.com/EMSERO/gopherclaw/internal/config"
	"github.com/EMSERO/gopherclaw/internal/cron"
	"github.com/EMSERO/gopherclaw/internal/session"
	"github.com/EMSERO/gopherclaw/internal/taskqueue"
)

// Bot is the Slack Socket Mode channel handler.
type Bot struct {
	logger   *zap.SugaredLogger
	api      *slacklib.Client
	client   *socketmode.Client
	cfg      config.SlackConfig
	msgCfg   config.Messages
	fullCfg  *config.Root
	ag       *agent.Agent
	sessions *session.Manager
	cronMgr  *cron.Manager
	taskMgr  *taskqueue.Manager
	botID    string // bot's own user ID (set after auth check)
	pairCode string // one-time code required by /pair

	mu              sync.Mutex
	paired          map[string]bool          // Slack user ID → paired
	queues          map[string]*messageQueue // user ID → pending messages
	pendingConfirms map[string]chan bool     // confirmID → result channel (REQ-061)
	cancelBot       context.CancelFunc       // cancel function to stop the bot for reconnection
	connected       atomic.Bool
}

// SetTaskManager sets the task queue manager for /tasks and /cancel commands.
func (b *Bot) SetTaskManager(m *taskqueue.Manager) { b.taskMgr = m }

// messageQueue collects rapid-fire messages from one user before processing.
type messageQueue struct {
	messages []queuedMessage
	timer    *time.Timer
}

type queuedMessage struct {
	text      string
	channelID string
	userID    string
	ts        string // message timestamp (for reactions)
}

// generatePairCode returns a cryptographically random 6-digit decimal string.
func generatePairCode() string { return common.GeneratePairCode() }

func pairedUsersFile() string { return common.PairedUsersPath("slack") }

// New creates a Slack Socket Mode bot.
func New(logger *zap.SugaredLogger, cfg config.SlackConfig, msgCfg config.Messages, fullCfg *config.Root, ag *agent.Agent, sm *session.Manager, cronMgr *cron.Manager) (*Bot, error) {
	api := slacklib.New(
		cfg.BotToken,
		slacklib.OptionAppLevelToken(cfg.AppToken),
	)
	client := socketmode.New(api)

	code := generatePairCode()
	bot := &Bot{
		logger:          logger,
		api:             api,
		client:          client,
		cfg:             cfg,
		msgCfg:          msgCfg,
		fullCfg:         fullCfg,
		ag:              ag,
		sessions:        sm,
		cronMgr:         cronMgr,
		pairCode:        code,
		paired:          make(map[string]bool),
		queues:          make(map[string]*messageQueue),
		pendingConfirms: make(map[string]chan bool),
	}
	bot.loadPairedUsers()
	// Pre-populate paired set from static allowUsers list so system events
	// reach them even if they never /pair.
	for _, id := range cfg.AllowUsers {
		bot.paired[id] = true
	}
	return bot, nil
}

// sessionKeyFor returns the session key for a user+channel, respecting session.scope.
func (b *Bot) sessionKeyFor(userID, channelID string) string {
	scope := ""
	agentID := "main"
	if b.fullCfg != nil {
		scope = b.fullCfg.Session.Scope
		agentID = b.fullCfg.DefaultAgent().ID
	}
	return common.SessionKey(agentID, "slack", scope, userID, channelID)
}

// matchesResetTrigger reports whether text exactly matches any configured reset trigger (case-insensitive).
func matchesResetTrigger(text string, triggers []string) bool {
	return common.MatchesResetTrigger(text, triggers)
}

// loadPairedUsers reads the paired users from disk.
func (b *Bot) loadPairedUsers() {
	for id := range common.LoadPairedUsers(b.logger, "slack") {
		b.paired[id] = true
		b.logger.Infof("slack: loaded paired user %s", id)
	}
}

// savePairedUsers writes the paired set to disk. Must be called with b.mu held.
func (b *Bot) savePairedUsers() {
	ids := make([]string, 0, len(b.paired))
	for id := range b.paired {
		ids = append(ids, id)
	}
	if err := common.SavePairedUsers(b.logger, "slack", ids); err != nil {
		b.logger.Warnf("slack: failed to save paired users: %v", err)
	}
}

// validatePairCode checks the user-supplied code against the generated one.
func (b *Bot) validatePairCode(input string) bool {
	return common.ValidatePairCode(input, b.pairCode)
}

// Start connects to Slack via Socket Mode and blocks until ctx is cancelled.
func (b *Bot) Start(ctx context.Context) error {
	childCtx, cancel := context.WithCancel(ctx)
	b.mu.Lock()
	b.cancelBot = cancel
	b.mu.Unlock()

	// Identify the bot
	if info, err := b.apiRef().AuthTest(); err == nil {
		b.botID = info.UserID
		b.connected.Store(true)
		b.logger.Infof("slack: bot started (user: %s, team: %s)", info.User, info.Team)
	} else {
		b.logger.Warnf("slack: auth test failed: %v", err)
	}
	b.logger.Infof("slack: pairing code: %s  (DM the bot: /pair %s)", b.pairCode, b.pairCode)

	go b.handleEvents(childCtx)

	if err := b.client.RunContext(childCtx); err != nil && childCtx.Err() == nil {
		b.connected.Store(false)
		return fmt.Errorf("slack: socket mode: %w", err)
	}
	b.connected.Store(false)
	return nil
}

// ChannelName returns the channel type name.
// apiRef returns the current *slacklib.Client under the mutex, safe for
// concurrent use with Reconnect() which swaps b.apiRef().
func (b *Bot) apiRef() *slacklib.Client { b.mu.Lock(); defer b.mu.Unlock(); return b.api }

func (b *Bot) ChannelName() string { return "slack" }

// IsConnected reports whether the bot is currently connected.
func (b *Bot) IsConnected() bool { return b.connected.Load() }

// Username returns the bot's Slack username.
func (b *Bot) Username() string { return b.botID }

// PairedCount returns the number of paired users.
func (b *Bot) PairedCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.paired)
}

// Reconnect stops the current socket mode connection and reconnects with new tokens.
func (b *Bot) Reconnect(ctx context.Context, newCfg config.SlackConfig) error {
	b.mu.Lock()
	if b.cancelBot != nil {
		b.cancelBot()
	}
	b.mu.Unlock()
	time.Sleep(500 * time.Millisecond)

	api := slacklib.New(
		newCfg.BotToken,
		slacklib.OptionAppLevelToken(newCfg.AppToken),
	)
	client := socketmode.New(api)

	b.mu.Lock()
	b.api = api
	b.client = client
	b.cfg = newCfg
	b.mu.Unlock()

	go func() {
		if err := b.Start(ctx); err != nil {
			b.logger.Errorf("slack: reconnect start: %v", err)
		}
	}()
	b.logger.Infof("slack: reconnected with new tokens")
	return nil
}

// handleEvents processes incoming Socket Mode events.
func (b *Bot) handleEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-b.client.Events:
			if !ok {
				return
			}
			switch evt.Type {
			case socketmode.EventTypeEventsAPI:
				b.client.Ack(*evt.Request)
				eventsAPI, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					continue
				}
				b.handleEventsAPI(eventsAPI)
			case socketmode.EventTypeInteractive:
				b.client.Ack(*evt.Request)
				callback, ok := evt.Data.(slacklib.InteractionCallback)
				if !ok {
					continue
				}
				if callback.Type == slacklib.InteractionTypeInteractionMessage {
					for _, action := range callback.ActionCallback.AttachmentActions {
						b.handleConfirmCallback(callback.CallbackID, action.Value)
					}
				}
			case socketmode.EventTypeConnecting:
				b.logger.Infof("slack: connecting...")
			case socketmode.EventTypeConnected:
				b.logger.Infof("slack: connected")
			case socketmode.EventTypeInvalidAuth:
				b.logger.Errorf("slack: invalid auth credentials")
				return
			}
		}
	}
}

// handleEventsAPI dispatches Slack API events.
func (b *Bot) handleEventsAPI(event slackevents.EventsAPIEvent) {
	switch event.Type {
	case slackevents.CallbackEvent:
		switch ev := event.InnerEvent.Data.(type) {
		case *slackevents.MessageEvent:
			b.handleMessageEvent(ev)
		case *slackevents.AppMentionEvent:
			b.handleMentionEvent(ev)
		case *slackevents.FileSharedEvent:
			b.handleFileShared(ev)
		}
	}
}

func (b *Bot) handleMessageEvent(ev *slackevents.MessageEvent) {
	// Ignore bot messages and subtypes (edits, deletes, etc.)
	if ev.BotID != "" || ev.SubType != "" {
		return
	}
	// Only handle DMs — Slack DM channels start with 'D'
	if !strings.HasPrefix(ev.Channel, "D") {
		return
	}

	text := strings.TrimSpace(ev.Text)
	userID := ev.User
	channelID := ev.Channel
	ts := ev.TimeStamp

	// Pairing command (handled before auth check so unpaired users can pair)
	if after, ok := strings.CutPrefix(text, "/pair "); ok {
		code := after
		if !b.validatePairCode(code) {
			_, _, _ = b.apiRef().PostMessage(channelID, slacklib.MsgOptionText("Invalid pairing code. Check the gopherclaw log.", false))
			return
		}
		b.mu.Lock()
		b.paired[userID] = true
		b.savePairedUsers()
		b.mu.Unlock()
		b.logger.Infof("slack: paired user %s", userID)
		_, _, _ = b.apiRef().PostMessage(channelID, slacklib.MsgOptionText(fmt.Sprintf("Paired! Your ID is %s.", userID), false))
		return
	}

	if !b.isAuthorized(userID) {
		_, _, _ = b.apiRef().PostMessage(channelID, slacklib.MsgOptionText("Send `/pair <code>` to start. Check the gopherclaw log for the code.", false))
		return
	}

	// Slash command routing
	if strings.HasPrefix(text, "/") {
		sessionKey := b.sessionKeyFor(userID, channelID)
		cmdCtx := common.BuildCmdCtx(sessionKey, common.CmdCtxDeps{
			Agent:       b.ag,
			Sessions:    b.sessions,
			Config:      b.fullCfg,
			CronManager: b.cronMgr,
			TaskManager: b.taskMgr,
		})
		slackCmdCtx, slackCmdCancel := context.WithTimeout(context.Background(), 30*time.Second)
		result := commands.Handle(slackCmdCtx, text, cmdCtx)
		slackCmdCancel()
		if result.Handled {
			_, _, _ = b.apiRef().PostMessage(channelID, slacklib.MsgOptionText(result.Text, false))
		}
		return
	}

	// Snapshot config under lock to avoid races with Reconnect().
	cfg, msgCfg := b.configSnapshot()

	b.ackReactionWith(channelID, ts, false, cfg, msgCfg)

	if msgCfg.Queue.Mode == "collect" && msgCfg.Queue.DebounceMs > 0 {
		b.enqueue(userID, channelID, ts, text)
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				b.logger.Errorf("slack: panic in processMessages: %v", r)
			}
		}()
		b.processMessages(userID, []queuedMessage{{text: text, channelID: channelID, userID: userID, ts: ts}})
	}()
}

func (b *Bot) handleMentionEvent(ev *slackevents.AppMentionEvent) {
	text := stripMention(ev.Text, b.botID)
	userID := ev.User
	channelID := ev.Channel
	ts := ev.TimeStamp

	if !b.isAuthorized(userID) {
		_, _, _ = b.apiRef().PostMessage(channelID, slacklib.MsgOptionText("You are not authorized to use this bot.", false))
		return
	}

	// Slash command routing
	if strings.HasPrefix(text, "/") {
		sessionKey := b.sessionKeyFor(userID, channelID)
		cmdCtx := common.BuildCmdCtx(sessionKey, common.CmdCtxDeps{
			Agent:       b.ag,
			Sessions:    b.sessions,
			Config:      b.fullCfg,
			CronManager: b.cronMgr,
			TaskManager: b.taskMgr,
		})
		slackCmdCtx, slackCmdCancel := context.WithTimeout(context.Background(), 30*time.Second)
		result := commands.Handle(slackCmdCtx, text, cmdCtx)
		slackCmdCancel()
		if result.Handled {
			_, _, _ = b.apiRef().PostMessage(channelID, slacklib.MsgOptionText(result.Text, false))
		}
		return
	}

	// Snapshot config under lock to avoid races with Reconnect().
	cfg, msgCfg := b.configSnapshot()

	b.ackReactionWith(channelID, ts, true, cfg, msgCfg)

	if msgCfg.Queue.Mode == "collect" && msgCfg.Queue.DebounceMs > 0 {
		b.enqueue(userID, channelID, ts, text)
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				b.logger.Errorf("slack: panic in processMessages: %v", r)
			}
		}()
		b.processMessages(userID, []queuedMessage{{text: text, channelID: channelID, userID: userID, ts: ts}})
	}()
}

// isAuthorized checks the allow-list (empty = allow all workspace members).
// Uses case-insensitive comparison for user IDs to match OpenClaw behavior.
func (b *Bot) isAuthorized(userID string) bool {
	if len(b.cfg.AllowUsers) == 0 {
		return true
	}
	for _, id := range b.cfg.AllowUsers {
		if strings.EqualFold(id, userID) {
			return true
		}
	}
	return false
}

// ackReactionWith adds a reaction emoji to a message using pre-snapshotted config.
func (b *Bot) ackReactionWith(channelID, ts string, isMention bool, cfg config.SlackConfig, msgCfg config.Messages) {
	scope := msgCfg.AckReactionScope
	if scope == "" {
		return
	}
	if scope == "group-mentions" && !isMention {
		return
	}
	emoji := cfg.AckEmoji
	if emoji == "" {
		emoji = "eyes"
	}
	ref := slacklib.NewRefToMessage(channelID, ts)
	if err := b.apiRef().AddReaction(emoji, ref); err != nil {
		b.logger.Infof("slack: reaction add failed: %v", err)
	}
}

// enqueue adds a message to the debounce queue.
func (b *Bot) enqueue(userID, channelID, ts, text string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	q, ok := b.queues[userID]
	if !ok {
		q = &messageQueue{}
		b.queues[userID] = q
	}
	q.messages = append(q.messages, queuedMessage{text: text, channelID: channelID, userID: userID, ts: ts})

	cap := b.msgCfg.Queue.Cap
	if cap <= 0 {
		cap = 20
	}
	if len(q.messages) >= cap {
		if q.timer != nil {
			q.timer.Stop()
		}
		b.flushLocked(userID)
		return
	}

	debounce := time.Duration(b.msgCfg.Queue.DebounceMs) * time.Millisecond
	if q.timer != nil {
		q.timer.Stop()
	}
	q.timer = time.AfterFunc(debounce, func() {
		b.mu.Lock()
		b.flushLocked(userID)
		b.mu.Unlock()
	})
}

// flushLocked drains the queue and spawns a goroutine to process. Must hold b.mu.
func (b *Bot) flushLocked(userID string) {
	q, ok := b.queues[userID]
	if !ok || len(q.messages) == 0 {
		return
	}
	msgs := q.messages
	delete(b.queues, userID)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				b.logger.Errorf("slack: panic in processMessages: %v", r)
			}
		}()
		b.processMessages(userID, msgs)
	}()
}

// configSnapshot returns a consistent copy of bot config fields. Must be called
// at the start of any goroutine that reads config to avoid races with Reconnect().
func (b *Bot) configSnapshot() (config.SlackConfig, config.Messages) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg, b.msgCfg
}

// processMessages combines queued messages and calls the agent.
func (b *Bot) processMessages(userID string, msgs []queuedMessage) {
	if len(msgs) == 0 {
		return
	}
	cfg, msgCfg := b.configSnapshot()

	var combined strings.Builder
	for i, m := range msgs {
		if i > 0 {
			combined.WriteString("\n")
		}
		combined.WriteString(m.text)
	}
	text := combined.String()
	channelID := msgs[0].channelID
	sessionKey := b.sessionKeyFor(userID, channelID)

	// Reset trigger check
	if b.fullCfg != nil && matchesResetTrigger(text, b.fullCfg.Session.ResetTriggers) {
		b.sessions.Reset(sessionKey)
		_, _, _ = b.apiRef().PostMessage(channelID, slacklib.MsgOptionText("Session cleared.", false))
		return
	}

	if cfg.StreamMode == "partial" {
		b.respondStreaming(sessionKey, channelID, text, cfg, msgCfg)
	} else {
		b.respondFull(sessionKey, channelID, text, cfg)
	}
}

func (b *Bot) respondFull(sessionKey, channelID, text string, cfg config.SlackConfig) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()

	resp, err := b.ag.Chat(ctx, sessionKey, text)
	if err != nil {
		b.logger.Errorf("slack: chat error: %v", err)
		_, _, _ = b.apiRef().PostMessage(channelID, slacklib.MsgOptionText(common.UserFacingError(err), false))
		return
	}

	outText := resp.Text
	// Suppress NO_REPLY / empty responses (REQ-503)
	if common.IsSuppressible(outText) {
		return
	}
	if b.fullCfg != nil && b.fullCfg.Messages.Usage == "tokens" && resp.Usage.OutputTokens > 0 {
		outText += fmt.Sprintf("\n\n📊 ~%d in / ~%d out tokens", resp.Usage.InputTokens, resp.Usage.OutputTokens)
	}
	rc := common.RetryConfig{PlainTextOnFinal: true, Logger: b.logger}
	parts := splitMessage(outText, 3000)
	for _, part := range parts {
		_ = common.RetrySend(rc, part, func(t string) error {
			_, _, err := b.apiRef().PostMessage(channelID, slacklib.MsgOptionText(t, false))
			return err
		})
	}
}

func (b *Bot) respondStreaming(sessionKey, channelID, text string, cfg config.SlackConfig, msgCfg config.Messages) {
	// Send placeholder
	_, ts, err := b.apiRef().PostMessage(channelID, slacklib.MsgOptionText("…", false))
	if err != nil {
		b.logger.Errorf("slack: send placeholder: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()

	var mu sync.Mutex
	var current strings.Builder
	lastEdit := time.Now()
	editMs := msgCfg.StreamEditMs
	if editMs <= 0 {
		editMs = 400
	}
	editInterval := time.Duration(editMs) * time.Millisecond

	// ts is protected by mu — both onChunk and OnIterationText access it.
	onChunk := func(chunk string) {
		mu.Lock()
		current.WriteString(chunk)
		elapsed := time.Since(lastEdit)
		if elapsed < editInterval {
			mu.Unlock()
			return
		}
		preview := current.String()
		if len(preview) > 2900 {
			preview = preview[:2900]
		}
		lastEdit = time.Now()
		curTS := ts
		mu.Unlock()
		_, _, _, _ = b.apiRef().UpdateMessage(channelID, curTS, slacklib.MsgOptionText(preview+"…", false))
	}

	var iterationTexts []string
	resp, err := b.ag.ChatStream(ctx, sessionKey, text, &agent.StreamCallbacks{
		OnChunk: onChunk,
		OnToolStart: common.NewToolNotifier(func(s string) {
			_, _, _ = b.apiRef().PostMessage(channelID, slacklib.MsgOptionText(s, false))
		}).OnToolStart,
		OnIterationText: func(block string) {
			if block != "" {
				iterationTexts = append(iterationTexts, block)
				// Send intermediate text by updating the placeholder, then
				// create a new placeholder for the next iteration.
				mu.Lock()
				current.Reset()
				curTS := ts
				mu.Unlock()
				_, _, _, _ = b.apiRef().UpdateMessage(channelID, curTS, slacklib.MsgOptionText(block, false))
				_, newTS, err := b.apiRef().PostMessage(channelID, slacklib.MsgOptionText("…", false))
				if err == nil {
					mu.Lock()
					ts = newTS
					mu.Unlock()
				}
			}
		},
	})
	// Snapshot ts under lock for all post-stream operations.
	mu.Lock()
	finalTS := ts
	mu.Unlock()

	if err != nil {
		b.logger.Errorf("slack: stream error: %v", err)
		_, _, _, _ = b.apiRef().UpdateMessage(channelID, finalTS, slacklib.MsgOptionText(common.UserFacingError(err), false))
		return
	}

	finalText := resp.Text
	// Strip already-sent iteration text blocks from the final message.
	for _, block := range iterationTexts {
		finalText = strings.Replace(finalText, block, "", 1)
	}
	finalText = strings.TrimSpace(finalText)
	if finalText == "" {
		finalText = current.String()
	}
	// Suppress NO_REPLY / empty responses (REQ-503)
	if common.IsSuppressible(finalText) {
		_, _, _ = b.apiRef().DeleteMessage(channelID, finalTS)
		return
	}
	if b.fullCfg != nil && b.fullCfg.Messages.Usage == "tokens" && resp.Usage.OutputTokens > 0 {
		finalText += fmt.Sprintf("\n\n📊 ~%d in / ~%d out tokens", resp.Usage.InputTokens, resp.Usage.OutputTokens)
	}

	parts := splitMessage(finalText, 3000)
	if len(parts) == 0 {
		return
	}
	_, _, _, _ = b.apiRef().UpdateMessage(channelID, finalTS, slacklib.MsgOptionText(parts[0], false))
	for _, part := range parts[1:] {
		_, _, _ = b.apiRef().PostMessage(channelID, slacklib.MsgOptionText(part, false))
	}
}

// handleFileShared processes a file_shared event. If the file is an image,
// it downloads it, encodes as base64, and sends to the agent as a vision message.
func (b *Bot) handleFileShared(ev *slackevents.FileSharedEvent) {
	userID := ev.UserID
	channelID := ev.ChannelID
	fileID := ev.File.ID

	if !b.isAuthorized(userID) {
		return
	}

	// Fetch file metadata.
	info, _, _, err := b.apiRef().GetFileInfo(fileID, 0, 0)
	if err != nil {
		b.logger.Errorf("slack: get file info %s: %v", fileID, err)
		return
	}

	// Only handle image files as vision inputs.
	if !strings.HasPrefix(info.Mimetype, "image/") {
		return
	}

	cfg, _ := b.configSnapshot()

	dlCtx, dlCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer dlCancel()
	dataURL, err := b.downloadFileAsDataURL(dlCtx, info)
	if err != nil {
		b.logger.Errorf("slack: download file %s: %v", fileID, err)
		_, _, _ = b.apiRef().PostMessage(channelID, slacklib.MsgOptionText("Failed to download image.", false))
		return
	}

	caption := info.Title
	if caption == "" || caption == info.Name {
		caption = "What's in this image?"
	}

	sessionKey := b.sessionKeyFor(userID, channelID)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()

	resp, err := b.ag.ChatWithImages(ctx, sessionKey, caption, []string{dataURL})
	if err != nil {
		b.logger.Errorf("slack: chat with image: %v", err)
		_, _, _ = b.apiRef().PostMessage(channelID, slacklib.MsgOptionText(common.UserFacingError(err), false))
		return
	}
	if common.IsSuppressible(resp.Text) {
		return
	}
	parts := splitMessage(resp.Text, 3000)
	for _, part := range parts {
		_, _, _ = b.apiRef().PostMessage(channelID, slacklib.MsgOptionText(part, false))
	}
}

// downloadFileAsDataURL downloads a Slack file and returns a base64-encoded data URL.
func (b *Bot) downloadFileAsDataURL(ctx context.Context, f *slacklib.File) (string, error) {
	url := f.URLPrivateDownload
	if url == "" {
		url = f.URLPrivate
	}
	if url == "" {
		return "", fmt.Errorf("no download URL for file %s", f.ID)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+b.cfg.BotToken)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20)) // 20 MB limit
	if err != nil {
		return "", err
	}

	mime := f.Mimetype
	if mime == "" {
		mime = http.DetectContentType(data)
	}

	var buf bytes.Buffer
	buf.WriteString("data:")
	buf.WriteString(mime)
	buf.WriteString(";base64,")
	buf.WriteString(base64.StdEncoding.EncodeToString(data))
	return buf.String(), nil
}

// SendToAllPaired sends a message to all paired/allowed Slack users via DM.
// If allowUsers is empty (all workspace members allowed), the bot opens a DM
// to each user it has previously seen. Use /pair to register for delivery.
func (b *Bot) SendToAllPaired(text string) {
	b.mu.Lock()
	ids := make([]string, 0, len(b.paired))
	for id := range b.paired {
		ids = append(ids, id)
	}
	b.mu.Unlock()

	if len(ids) == 0 {
		b.logger.Infof("slack: sendToAllPaired: no paired users to deliver to")
		return
	}

	for _, userID := range ids {
		// Open a DM channel to the user
		ch, _, _, err := b.apiRef().OpenConversation(&slacklib.OpenConversationParameters{Users: []string{userID}})
		if err != nil {
			b.logger.Errorf("slack: sendToAllPaired open DM %s: %v", userID, err)
			continue
		}
		parts := splitMessage(text, 3000)
		for _, part := range parts {
			if _, _, err := b.apiRef().PostMessage(ch.ID, slacklib.MsgOptionText(part, false)); err != nil {
				b.logger.Errorf("slack: sendToAllPaired send %s: %v", userID, err)
			}
		}
	}
}

// AnnounceToSession sends a message to the user/channel identified by a session key.
// Session key format: <agentID>:slack:<userID> (user scope) or
// <agentID>:slack:channel:<channelID> (channel scope).
func (b *Bot) AnnounceToSession(sessionKey, text string) {
	parts := strings.Split(sessionKey, ":")
	if len(parts) < 3 {
		return
	}
	isSlack := (len(parts) == 3 && parts[1] == "slack") ||
		(len(parts) >= 4 && parts[2] == "slack")
	if !isSlack {
		return
	}
	// Determine target: "channel" scope or user scope
	if len(parts) >= 5 && parts[3] == "channel" {
		channelID := parts[4]
		for _, chunk := range splitMessage(text, 3000) {
			if _, _, err := b.apiRef().PostMessage(channelID, slacklib.MsgOptionText(chunk, false)); err != nil {
				b.logger.Errorf("slack: AnnounceToSession channel %s: %v", channelID, err)
			}
		}
		return
	}
	// User scope — open DM
	userID := parts[len(parts)-1]
	ch, _, _, err := b.apiRef().OpenConversation(&slacklib.OpenConversationParameters{Users: []string{userID}})
	if err != nil {
		b.logger.Errorf("slack: AnnounceToSession open DM %s: %v", userID, err)
		return
	}
	for _, chunk := range splitMessage(text, 3000) {
		if _, _, err := b.apiRef().PostMessage(ch.ID, slacklib.MsgOptionText(chunk, false)); err != nil {
			b.logger.Errorf("slack: AnnounceToSession send %s: %v", userID, err)
		}
	}
}

// stripMention removes <@botID> from message text.
func stripMention(text, botID string) string {
	if botID != "" {
		text = strings.ReplaceAll(text, "<@"+botID+">", "")
	}
	return strings.TrimSpace(text)
}

func splitMessage(text string, maxLen int) []string { return common.SmartChunk(text, maxLen) }

// CanConfirm returns true if this bot can reach the given session key.
func (b *Bot) CanConfirm(sessionKey string) bool {
	return strings.Contains(sessionKey, ":slack:")
}

// SendConfirmPrompt sends a Slack interactive message with approve/deny buttons
// and blocks until the user responds or the context expires.
func (b *Bot) SendConfirmPrompt(ctx context.Context, sessionKey, command, pattern string) (bool, error) {
	parts := strings.Split(sessionKey, ":")
	if len(parts) < 3 {
		return false, fmt.Errorf("invalid session key: %s", sessionKey)
	}

	// Determine channel:
	// Channel scope: <agent>:slack:channel:<channelID>
	// User scope: <agent>:slack:<userID> — open DM
	var channelID string
	if len(parts) >= 5 && parts[3] == "channel" {
		channelID = parts[4]
	} else {
		userID := parts[len(parts)-1]
		ch, _, _, err := b.apiRef().OpenConversation(&slacklib.OpenConversationParameters{Users: []string{userID}})
		if err != nil {
			return false, fmt.Errorf("slack: open DM for confirm: %w", err)
		}
		channelID = ch.ID
	}

	display := command
	if len(display) > 200 {
		display = display[:200] + "..."
	}

	confirmID := fmt.Sprintf("confirm:%s:%d", channelID, time.Now().UnixNano())

	resultCh := make(chan bool, 1)
	b.mu.Lock()
	b.pendingConfirms[confirmID] = resultCh
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		delete(b.pendingConfirms, confirmID)
		b.mu.Unlock()
	}()

	msg := fmt.Sprintf("⚠️ *Dangerous command detected*\n\nMatched pattern: `%s`\n\n```\n%s\n```\n\nAllow execution?", pattern, display)
	_, _, err := b.apiRef().PostMessage(channelID,
		slacklib.MsgOptionText(msg, false),
		slacklib.MsgOptionAttachments(slacklib.Attachment{
			CallbackID: confirmID,
			Actions: []slacklib.AttachmentAction{
				{Name: "confirm", Text: "Yes, execute", Type: "button", Value: "yes", Style: "danger"},
				{Name: "confirm", Text: "No, block", Type: "button", Value: "no"},
			},
		}),
	)
	if err != nil {
		return false, fmt.Errorf("slack: send confirm prompt: %w", err)
	}

	select {
	case confirmed := <-resultCh:
		return confirmed, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// handleConfirmCallback checks if an interactive action is a confirmation response.
// Returns true if handled.
func (b *Bot) handleConfirmCallback(callbackID, value string) bool {
	if !strings.HasPrefix(callbackID, "confirm:") {
		return false
	}

	b.mu.Lock()
	ch, ok := b.pendingConfirms[callbackID]
	b.mu.Unlock()

	if !ok {
		return false
	}

	select {
	case ch <- (value == "yes"):
	default:
	}
	return true
}
