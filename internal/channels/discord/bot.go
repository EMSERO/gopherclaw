package discord

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"

	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/agent"
	"github.com/EMSERO/gopherclaw/internal/channels/common"
	"github.com/EMSERO/gopherclaw/internal/commands"
	"github.com/EMSERO/gopherclaw/internal/config"
	"github.com/EMSERO/gopherclaw/internal/cron"
	"github.com/EMSERO/gopherclaw/internal/session"
	"github.com/EMSERO/gopherclaw/internal/taskqueue"
)

// Bot is the Discord channel handler.
type Bot struct {
	logger   *zap.SugaredLogger
	dg       *discordgo.Session
	cfg      config.DiscordConfig
	msgCfg   config.Messages
	fullCfg  *config.Root
	ag       agent.PrimaryAgent
	sessions *session.Manager
	cronMgr  *cron.Manager
	taskMgr  *taskqueue.Manager
	pairCode string
	botID    string // bot's own user ID (set after Open)

	mu              sync.Mutex
	paired          map[string]bool          // Discord user ID (snowflake string) → paired
	queues          map[string]*messageQueue // user ID → pending messages
	pendingConfirms map[string]chan bool     // confirmID → result channel (REQ-061)
	connected       atomic.Bool
}

// SetTaskManager sets the task queue manager for /tasks and /cancel commands.
func (b *Bot) SetTaskManager(m *taskqueue.Manager) { b.taskMgr = m }

// generatePairCode returns a cryptographically random 6-digit decimal string.
func generatePairCode() string { return common.GeneratePairCode() }

// validatePairCode checks the user-supplied code against the generated one.
func (b *Bot) validatePairCode(input string) bool {
	return common.ValidatePairCode(input, b.pairCode)
}

// messageQueue collects rapid-fire messages from one user before processing.
type messageQueue struct {
	messages []queuedMessage
	timer    *time.Timer
	firstMsg *discordgo.MessageCreate // for replyToMode: "first"
}

type queuedMessage struct {
	text string
	m    *discordgo.MessageCreate
}

// New creates a Discord bot (does not connect yet).
func New(logger *zap.SugaredLogger, cfg config.DiscordConfig, msgCfg config.Messages, fullCfg *config.Root, ag agent.PrimaryAgent, sm *session.Manager, cronMgr *cron.Manager) (*Bot, error) {
	dg, err := discordgo.New("Bot " + cfg.BotToken)
	if err != nil {
		return nil, fmt.Errorf("create discord session: %w", err)
	}
	// Request intent for DMs and guild messages
	dg.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentsGuildMessageReactions

	code := generatePairCode()
	bot := &Bot{
		logger:          logger,
		dg:              dg,
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

	dg.AddHandler(bot.handleMessage)
	dg.AddHandler(bot.handleInteraction)
	return bot, nil
}

// handleInteraction processes Discord button interactions (confirmations).
func (b *Bot) handleInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionMessageComponent {
		return
	}
	customID := i.MessageComponentData().CustomID
	if b.handleConfirmCallback(customID) {
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content:    i.Message.Content + "\n\n✅ Response recorded.",
				Components: []discordgo.MessageComponent{}, // remove buttons
			},
		})
	}
}

// Start connects to Discord and blocks until ctx is cancelled.
func (b *Bot) Start(ctx context.Context) error {
	if err := b.dg.Open(); err != nil {
		return fmt.Errorf("discord: open connection: %w", err)
	}
	if b.dg.State == nil || b.dg.State.User == nil {
		_ = b.dg.Close()
		return fmt.Errorf("discord: connected but bot user not available (ready event not received)")
	}
	b.botID = b.dg.State.User.ID
	b.connected.Store(true)
	b.logger.Infof("discord: bot started (%s#%s)", b.dg.State.User.Username, b.dg.State.User.Discriminator)
	b.logger.Infof("discord: pairing code: %s  (DM the bot: /pair %s)", b.pairCode, b.pairCode)

	<-ctx.Done()
	b.connected.Store(false)
	if err := b.dg.Close(); err != nil {
		b.logger.Errorf("discord: close: %v", err)
	}
	return nil
}

// dgRef returns the current *discordgo.Session under the mutex, safe for
// concurrent use with Reconnect() which swaps b.dg.
func (b *Bot) dgRef() *discordgo.Session { b.mu.Lock(); defer b.mu.Unlock(); return b.dg }

// ChannelName returns the channel type name.
func (b *Bot) ChannelName() string { return "discord" }

// IsConnected reports whether the bot is currently connected.
func (b *Bot) IsConnected() bool { return b.connected.Load() }

// Username returns the bot's Discord username.
func (b *Bot) Username() string {
	dg := b.dgRef()
	if dg.State != nil && dg.State.User != nil {
		return dg.State.User.Username
	}
	return ""
}

// PairedCount returns the number of paired users.
func (b *Bot) PairedCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.paired)
}

// Reconnect closes the current session and reconnects with a new token.
func (b *Bot) Reconnect(ctx context.Context, newCfg config.DiscordConfig) error {
	_ = b.dgRef().Close()
	dg, err := discordgo.New("Bot " + newCfg.BotToken)
	if err != nil {
		return fmt.Errorf("create discord session: %w", err)
	}
	dg.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentsGuildMessageReactions

	b.mu.Lock()
	b.dg = dg
	b.cfg = newCfg
	b.mu.Unlock()

	dg.AddHandler(b.handleMessage)
	dg.AddHandler(b.handleInteraction)
	go func() {
		if err := b.Start(ctx); err != nil {
			b.logger.Errorf("discord: reconnect start: %v", err)
		}
	}()
	b.logger.Infof("discord: reconnected with new token")
	return nil
}

func pairedUsersFile() string { return common.PairedUsersPath("discord") }

// loadPairedUsers reads the paired users from disk.
func (b *Bot) loadPairedUsers() {
	for id := range common.LoadPairedUsers(b.logger, "discord") {
		b.paired[id] = true
		b.logger.Infof("discord: loaded paired user %s", id)
	}
}

// savePairedUsers writes the paired set to disk. Must be called with b.mu held.
func (b *Bot) savePairedUsers() {
	ids := make([]string, 0, len(b.paired))
	for id := range b.paired {
		ids = append(ids, id)
	}
	if err := common.SavePairedUsers(b.logger, "discord", ids); err != nil {
		b.logger.Warnf("discord: failed to save paired users: %v", err)
	}
}

// sessionKeyFor returns the session key for a user+channel, respecting session.scope.
func (b *Bot) sessionKeyFor(userID, channelID string) string {
	scope := ""
	agentID := "main"
	if b.fullCfg != nil {
		scope = b.fullCfg.Session.Scope
		agentID = b.fullCfg.DefaultAgent().ID
	}
	return common.SessionKey(agentID, "discord", scope, userID, channelID)
}

// matchesResetTrigger reports whether text exactly matches any configured reset trigger (case-insensitive).
func matchesResetTrigger(text string, triggers []string) bool {
	return common.MatchesResetTrigger(text, triggers)
}

// handleMessage is the discordgo MessageCreate event handler.
func (b *Bot) handleMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	// Ignore the bot's own messages
	if m.Author == nil || m.Author.Bot {
		return
	}

	text := strings.TrimSpace(m.Content)
	userID := m.Author.ID
	channelID := m.ChannelID

	// Detect if this is a DM or a guild channel
	isDM := m.GuildID == ""

	if !isDM {
		// In guild channels, only respond when mentioned
		if !strings.Contains(text, "<@"+b.botID+">") && !strings.Contains(text, "<@!"+b.botID+">") {
			return
		}
		// Strip the mention from the text
		text = stripMention(text, b.botID)
	}

	// Auth check
	if !b.isAuthorized(userID, isDM) {
		if isDM {
			_, _ = s.ChannelMessageSend(channelID, "Send `/pair <code>` to start. Check the gopherclaw log for the code.")
		}
		return
	}

	// Pairing command (handled before general command routing)
	if after, ok := strings.CutPrefix(text, "/pair "); ok {
		code := after
		if !b.validatePairCode(code) {
			_, _ = s.ChannelMessageSend(channelID, "Invalid pairing code. Check the gopherclaw log.")
			return
		}
		b.mu.Lock()
		b.paired[userID] = true
		b.savePairedUsers()
		b.mu.Unlock()
		b.logger.Infof("discord: paired user %s (%s#%s)", userID, m.Author.Username, m.Author.Discriminator)
		_, _ = s.ChannelMessageSend(channelID, fmt.Sprintf("Paired! Your ID is %s.", userID))
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
		discordCmdCtx, discordCmdCancel := context.WithTimeout(context.Background(), 30*time.Second)
		result := commands.Handle(discordCmdCtx, text, cmdCtx)
		discordCmdCancel()
		if result.Handled {
			sendLong(s, channelID, m, result.Text)
		}
		return
	}

	// Snapshot config under lock to avoid races with Reconnect().
	cfg, msgCfg := b.configSnapshot()

	// Reaction ack
	b.ackReaction(s, m, isDM, cfg, msgCfg)

	// Check for image attachments — handle as vision input.
	imageURLs := extractImageURLs(m.Attachments)
	if len(imageURLs) > 0 {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					b.logger.Errorf("discord: panic in image handler: %v", r)
				}
			}()
			b.processImageMessage(s, m, userID, channelID, text, imageURLs, cfg)
		}()
		return
	}

	// Debounce or process immediately
	if msgCfg.Queue.Mode == "collect" && msgCfg.Queue.DebounceMs > 0 {
		b.enqueue(userID, channelID, text, m)
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				b.logger.Errorf("discord: panic in processMessages: %v", r)
			}
		}()
		b.processMessages(userID, channelID, []queuedMessage{{text: text, m: m}}, m)
	}()
}

// isAuthorized checks whether a user is allowed to interact.
func (b *Bot) isAuthorized(userID string, isDM bool) bool {
	b.mu.Lock()
	dmPolicy := b.cfg.DMPolicy
	allowUsers := b.cfg.AllowUsers
	isPaired := b.paired[userID]
	b.mu.Unlock()

	if dmPolicy == "allowlist" {
		return slices.Contains(allowUsers, userID)
	}
	// Default: "pairing" mode
	if isDM && dmPolicy != "" {
		return isPaired
	}
	return true
}

// ackReaction adds a reaction emoji to the message.
func (b *Bot) ackReaction(s *discordgo.Session, m *discordgo.MessageCreate, isDM bool, cfg config.DiscordConfig, msgCfg config.Messages) {
	scope := msgCfg.AckReactionScope
	if scope == "" {
		return
	}
	emoji := cfg.AckEmoji
	if emoji == "" {
		emoji = "👀"
	}
	if scope == "group-mentions" && isDM {
		return
	}
	if err := s.MessageReactionAdd(m.ChannelID, m.ID, emoji); err != nil {
		b.logger.Infof("discord: reaction add failed: %v", err)
	}
}

// enqueue adds a message to the debounce queue.
func (b *Bot) enqueue(userID, channelID, text string, m *discordgo.MessageCreate) {
	b.mu.Lock()
	defer b.mu.Unlock()

	q, ok := b.queues[userID]
	if !ok {
		q = &messageQueue{firstMsg: m}
		b.queues[userID] = q
	}
	q.messages = append(q.messages, queuedMessage{text: text, m: m})

	cap := b.msgCfg.Queue.Cap
	if cap <= 0 {
		cap = 20
	}
	if len(q.messages) >= cap {
		if q.timer != nil {
			q.timer.Stop()
		}
		b.flushLocked(userID, channelID)
		return
	}

	debounce := time.Duration(b.msgCfg.Queue.DebounceMs) * time.Millisecond
	if q.timer != nil {
		q.timer.Stop()
	}
	q.timer = time.AfterFunc(debounce, func() {
		b.mu.Lock()
		b.flushLocked(userID, channelID)
		b.mu.Unlock()
	})
}

// flushLocked drains the queue and spawns a goroutine to process. Must hold b.mu.
func (b *Bot) flushLocked(userID, channelID string) {
	q, ok := b.queues[userID]
	if !ok || len(q.messages) == 0 {
		return
	}
	msgs := q.messages
	firstMsg := q.firstMsg
	delete(b.queues, userID)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				b.logger.Errorf("discord: panic in processMessages: %v", r)
			}
		}()
		b.processMessages(userID, channelID, msgs, firstMsg)
	}()
}

// configSnapshot returns a consistent copy of bot config fields. Must be called
// at the start of any goroutine that reads config to avoid races with Reconnect().
func (b *Bot) configSnapshot() (config.DiscordConfig, config.Messages) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg, b.msgCfg
}

// processMessages combines queued messages and invokes the agent.
func (b *Bot) processMessages(userID, channelID string, msgs []queuedMessage, replyMsg *discordgo.MessageCreate) {
	cfg, msgCfg := b.configSnapshot()

	var combined strings.Builder
	for i, m := range msgs {
		if i > 0 {
			combined.WriteString("\n")
		}
		combined.WriteString(m.text)
	}
	text := combined.String()

	replyTo := replyMsg
	if cfg.ReplyToMode == "first" && len(msgs) > 0 {
		replyTo = msgs[0].m
	}

	sessionKey := b.sessionKeyFor(userID, channelID)

	// Reset trigger check
	if b.fullCfg != nil && matchesResetTrigger(text, b.fullCfg.Session.ResetTriggers) {
		b.sessions.Reset(sessionKey)
		sendLong(b.dgRef(), channelID, replyTo, "Session cleared.")
		return
	}

	if cfg.StreamMode == "partial" {
		b.respondStreaming(sessionKey, channelID, text, replyTo, cfg, msgCfg)
	} else {
		b.respondFull(sessionKey, channelID, text, replyTo, cfg)
	}
}

func (b *Bot) respondFull(sessionKey, channelID, text string, replyMsg *discordgo.MessageCreate, cfg config.DiscordConfig) {
	dg := b.dgRef()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()

	resp, err := b.ag.Chat(ctx, sessionKey, text)
	if err != nil {
		b.logger.Errorf("discord: chat error: %v", err)
		_, _ = dg.ChannelMessageSendReply(channelID, common.UserFacingError(err), &discordgo.MessageReference{
			MessageID: replyMsg.ID,
			ChannelID: channelID,
			GuildID:   replyMsg.GuildID,
		})
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
	sendLong(b.dgRef(), channelID, replyMsg, outText)
}

func (b *Bot) respondStreaming(sessionKey, channelID, text string, replyMsg *discordgo.MessageCreate, cfg config.DiscordConfig, msgCfg config.Messages) {
	dg := b.dgRef()
	// Send placeholder
	placeholder, err := dg.ChannelMessageSendReply(channelID, "…", &discordgo.MessageReference{
		MessageID: replyMsg.ID,
		ChannelID: channelID,
		GuildID:   replyMsg.GuildID,
	})
	if err != nil {
		b.logger.Errorf("discord: send placeholder: %v", err)
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

	onChunk := func(chunk string) {
		mu.Lock()
		current.WriteString(chunk)
		elapsed := time.Since(lastEdit)
		if elapsed < editInterval {
			mu.Unlock()
			return
		}
		preview := current.String()
		if len(preview) > 1900 {
			preview = preview[:1900]
		}
		lastEdit = time.Now()
		mu.Unlock()
		_, _ = dg.ChannelMessageEdit(channelID, placeholder.ID, preview+"▋")
	}

	var iterationTexts []string
	resp, err := b.ag.ChatStream(ctx, sessionKey, text, &agent.StreamCallbacks{
		OnChunk: onChunk,
		OnToolStart: common.NewToolNotifier(func(s string) {
			_, _ = dg.ChannelMessageSend(channelID, s)
		}).OnToolStart,
		OnIterationText: func(block string) {
			if block != "" {
				iterationTexts = append(iterationTexts, block)
				// Send intermediate text as a separate message and reset
				// the streaming placeholder for the next iteration.
				mu.Lock()
				current.Reset()
				mu.Unlock()
				_, _ = dg.ChannelMessageEdit(channelID, placeholder.ID, block)
				// Create a new placeholder for the next iteration.
				newPlaceholder, err := dg.ChannelMessageSend(channelID, "…")
				if err == nil {
					placeholder = newPlaceholder
				}
			}
		},
	})
	if err != nil {
		b.logger.Errorf("discord: stream error: %v", err)
		_, _ = dg.ChannelMessageEdit(channelID, placeholder.ID, common.UserFacingError(err))
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
		_ = dg.ChannelMessageDelete(channelID, placeholder.ID)
		return
	}
	if b.fullCfg != nil && b.fullCfg.Messages.Usage == "tokens" && resp.Usage.OutputTokens > 0 {
		finalText += fmt.Sprintf("\n\n📊 ~%d in / ~%d out tokens", resp.Usage.InputTokens, resp.Usage.OutputTokens)
	}

	parts := splitMessage(finalText, 2000)
	if len(parts) == 0 {
		return
	}
	_, _ = dg.ChannelMessageEdit(channelID, placeholder.ID, parts[0])
	for _, part := range parts[1:] {
		_, _ = dg.ChannelMessageSend(channelID, part)
	}
}

// extractImageURLs returns HTTP URLs for image attachments from a Discord message.
func extractImageURLs(attachments []*discordgo.MessageAttachment) []string {
	var urls []string
	for _, a := range attachments {
		if a == nil || a.URL == "" {
			continue
		}
		ct := strings.ToLower(a.ContentType)
		if strings.HasPrefix(ct, "image/") {
			urls = append(urls, a.URL)
		}
	}
	return urls
}

// processImageMessage sends a user message with image attachments to the agent.
func (b *Bot) processImageMessage(dg *discordgo.Session, m *discordgo.MessageCreate, userID, channelID, text string, imageURLs []string, cfg config.DiscordConfig) {
	if text == "" {
		text = "What's in this image?"
	}
	sessionKey := b.sessionKeyFor(userID, channelID)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()

	_ = dg.ChannelTyping(channelID)
	resp, err := b.ag.ChatWithImages(ctx, sessionKey, text, imageURLs)
	if err != nil {
		b.logger.Errorf("discord: chat with images: %v", err)
		_, _ = dg.ChannelMessageSendReply(channelID, common.UserFacingError(err), m.Reference())
		return
	}
	if common.IsSuppressible(resp.Text) {
		return
	}
	sendLong(dg, channelID, m, resp.Text)
}

// SendToAllPaired sends a message to all paired Discord users via DM.
func (b *Bot) SendToAllPaired(text string) {
	dg := b.dgRef()
	b.mu.Lock()
	ids := make([]string, 0, len(b.paired))
	for id := range b.paired {
		ids = append(ids, id)
	}
	b.mu.Unlock()

	for _, userID := range ids {
		ch, err := dg.UserChannelCreate(userID)
		if err != nil {
			b.logger.Errorf("discord: sendToAllPaired create DM %s: %v", userID, err)
			continue
		}
		parts := splitMessage(text, 2000)
		for _, part := range parts {
			if _, err := dg.ChannelMessageSend(ch.ID, part); err != nil {
				b.logger.Errorf("discord: sendToAllPaired send %s: %v", userID, err)
			}
		}
	}
}

// AnnounceToSession sends a message to the user/channel identified by a session key.
// Session key format: <agentID>:discord:<userID> (user scope) or
// <agentID>:discord:channel:<channelID> (channel scope).
func (b *Bot) AnnounceToSession(sessionKey, text string) {
	dg := b.dgRef()
	parts := strings.Split(sessionKey, ":")
	if len(parts) < 3 {
		return
	}
	isDiscord := (len(parts) == 3 && parts[1] == "discord") ||
		(len(parts) >= 4 && parts[2] == "discord")
	if !isDiscord {
		return
	}
	// Determine target: "channel" scope or user scope
	if len(parts) >= 5 && parts[3] == "channel" {
		channelID := parts[4]
		for _, chunk := range splitMessage(text, 2000) {
			if _, err := dg.ChannelMessageSend(channelID, chunk); err != nil {
				b.logger.Errorf("discord: AnnounceToSession channel %s: %v", channelID, err)
			}
		}
		return
	}
	// User scope — open DM
	userID := parts[len(parts)-1]
	ch, err := dg.UserChannelCreate(userID)
	if err != nil {
		b.logger.Errorf("discord: AnnounceToSession create DM %s: %v", userID, err)
		return
	}
	for _, chunk := range splitMessage(text, 2000) {
		if _, err := dg.ChannelMessageSend(ch.ID, chunk); err != nil {
			b.logger.Errorf("discord: AnnounceToSession send %s: %v", userID, err)
		}
	}
}

// stripMention removes <@botID> and <@!botID> from message text.
func stripMention(text, botID string) string {
	text = strings.ReplaceAll(text, "<@"+botID+">", "")
	text = strings.ReplaceAll(text, "<@!"+botID+">", "")
	return strings.TrimSpace(text)
}

// sendLong sends a message, splitting if over Discord's 2000 char limit.
func sendLong(s *discordgo.Session, channelID string, replyMsg *discordgo.MessageCreate, text string) {
	rc := common.RetryConfig{PlainTextOnFinal: true}
	parts := splitMessage(text, 2000)
	ref := &discordgo.MessageReference{
		MessageID: replyMsg.ID,
		ChannelID: channelID,
		GuildID:   replyMsg.GuildID,
	}
	for i, part := range parts {
		idx := i
		_ = common.RetrySend(rc, part, func(t string) error {
			var err error
			if idx == 0 {
				_, err = s.ChannelMessageSendReply(channelID, t, ref)
			} else {
				_, err = s.ChannelMessageSend(channelID, t)
			}
			return err
		})
	}
}

func splitMessage(text string, maxLen int) []string { return common.SmartChunk(text, maxLen) }

// CanConfirm returns true if this bot can reach the given session key.
func (b *Bot) CanConfirm(sessionKey string) bool {
	return strings.Contains(sessionKey, ":discord:")
}

// SendConfirmPrompt sends a button-based confirmation prompt to the Discord
// channel/DM associated with the session key and blocks until the user responds
// or the context expires.
func (b *Bot) SendConfirmPrompt(ctx context.Context, sessionKey, command, pattern string) (bool, error) {
	parts := strings.Split(sessionKey, ":")
	if len(parts) < 3 {
		return false, fmt.Errorf("invalid session key: %s", sessionKey)
	}

	// Determine channel to message:
	// User scope: <agent>:discord:<userID> — open DM
	// Channel scope: <agent>:discord:channel:<channelID>
	var channelID string
	dg := b.dgRef()
	if len(parts) >= 5 && parts[3] == "channel" {
		channelID = parts[4]
	} else {
		userID := parts[len(parts)-1]
		ch, err := dg.UserChannelCreate(userID)
		if err != nil {
			return false, fmt.Errorf("discord: create DM for confirm: %w", err)
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

	msg := fmt.Sprintf("⚠️ **Dangerous command detected**\n\nMatched pattern: `%s`\n\n```\n%s\n```\n\nAllow execution?", pattern, display)
	_, err := dg.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content: msg,
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					discordgo.Button{
						Label:    "Yes, execute",
						Style:    discordgo.DangerButton,
						CustomID: confirmID + ":yes",
					},
					discordgo.Button{
						Label:    "No, block",
						Style:    discordgo.SecondaryButton,
						CustomID: confirmID + ":no",
					},
				},
			},
		},
	})
	if err != nil {
		return false, fmt.Errorf("discord: send confirm prompt: %w", err)
	}

	select {
	case confirmed := <-resultCh:
		return confirmed, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// handleConfirmCallback checks if an interaction is a confirmation response and resolves it.
// Returns true if the interaction was handled as a confirmation.
func (b *Bot) handleConfirmCallback(customID string) bool {
	if !strings.HasPrefix(customID, "confirm:") {
		return false
	}

	lastColon := strings.LastIndex(customID, ":")
	if lastColon < 0 {
		return false
	}
	answer := customID[lastColon+1:]
	confirmID := customID[:lastColon]

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
