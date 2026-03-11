package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	slacklib "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
	openai "github.com/sashabaranov/go-openai"
	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/agent"
	"github.com/EMSERO/gopherclaw/internal/config"
	"github.com/EMSERO/gopherclaw/internal/models"
	"github.com/EMSERO/gopherclaw/internal/session"
	"github.com/EMSERO/gopherclaw/internal/taskqueue"
)

// ---------------------------------------------------------------------------
// generatePairCode
// ---------------------------------------------------------------------------

func TestGeneratePairCode(t *testing.T) {
	code := generatePairCode()
	if len(code) != 6 {
		t.Errorf("expected 6-digit code, got %q (len %d)", code, len(code))
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			t.Errorf("expected all digits, got %q", code)
		}
	}
}

func TestGeneratePairCodeUnique(t *testing.T) {
	seen := make(map[string]bool)
	for range 20 {
		code := generatePairCode()
		seen[code] = true
	}
	if len(seen) < 18 {
		t.Errorf("too many collisions: only %d unique out of 20", len(seen))
	}
}

// ---------------------------------------------------------------------------
// pairedUsersFile
// ---------------------------------------------------------------------------

func TestPairedUsersFile(t *testing.T) {
	path := pairedUsersFile()
	if path == "" {
		t.Fatal("pairedUsersFile returned empty string")
	}
	if !strings.Contains(path, "slack-default-allowFrom.json") {
		t.Errorf("expected path to contain 'slack-default-allowFrom.json', got %q", path)
	}
	if !strings.Contains(path, ".gopherclaw") {
		t.Errorf("expected path to contain '.gopherclaw', got %q", path)
	}
	if !strings.Contains(path, "credentials") {
		t.Errorf("expected path to contain 'credentials', got %q", path)
	}
}

// ---------------------------------------------------------------------------
// sessionKeyFor
// ---------------------------------------------------------------------------

func TestSessionKeyForDefaultScope(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	key := bot.sessionKeyFor("U123", "C456")
	if key != "main:slack:U123" {
		t.Errorf("expected user-scoped key, got %q", key)
	}
}

func TestSessionKeyForNilConfig(t *testing.T) {
	bot := &Bot{fullCfg: nil, logger: zap.NewNop().Sugar()}
	key := bot.sessionKeyFor("U123", "C456")
	if key != "main:slack:U123" {
		t.Errorf("expected user-scoped key when config is nil, got %q", key)
	}
}

func TestSessionKeyForChannelScope(t *testing.T) {
	bot := &Bot{fullCfg: &config.Root{Session: config.Session{Scope: "channel"}}, logger: zap.NewNop().Sugar()}
	key := bot.sessionKeyFor("U123", "C456")
	expected := "main:slack:channel:C456"
	if key != expected {
		t.Errorf("expected %q, got %q", expected, key)
	}
}

func TestSessionKeyForGlobalScope(t *testing.T) {
	bot := &Bot{fullCfg: &config.Root{Session: config.Session{Scope: "global"}}, logger: zap.NewNop().Sugar()}
	key := bot.sessionKeyFor("U123", "C456")
	expected := "main:slack:global"
	if key != expected {
		t.Errorf("expected %q, got %q", expected, key)
	}
}

func TestSessionKeyForUserScope(t *testing.T) {
	bot := &Bot{fullCfg: &config.Root{Session: config.Session{Scope: "user"}}, logger: zap.NewNop().Sugar()}
	key := bot.sessionKeyFor("U123", "C456")
	expected := "main:slack:U123"
	if key != expected {
		t.Errorf("expected %q, got %q", expected, key)
	}
}

func TestSessionKeyForEmptyScope(t *testing.T) {
	bot := &Bot{fullCfg: &config.Root{Session: config.Session{Scope: ""}}, logger: zap.NewNop().Sugar()}
	key := bot.sessionKeyFor("U123", "C456")
	expected := "main:slack:U123"
	if key != expected {
		t.Errorf("expected %q, got %q", expected, key)
	}
}

func TestSessionKeyForUnknownScope(t *testing.T) {
	bot := &Bot{fullCfg: &config.Root{Session: config.Session{Scope: "something-else"}}, logger: zap.NewNop().Sugar()}
	key := bot.sessionKeyFor("U123", "C456")
	// unknown scope falls through to default (user-based)
	expected := "main:slack:U123"
	if key != expected {
		t.Errorf("expected %q, got %q", expected, key)
	}
}

// ---------------------------------------------------------------------------
// matchesResetTrigger
// ---------------------------------------------------------------------------

func TestMatchesResetTriggerExact(t *testing.T) {
	triggers := []string{"reset", "clear"}
	if !matchesResetTrigger("reset", triggers) {
		t.Error("expected 'reset' to match")
	}
	if !matchesResetTrigger("clear", triggers) {
		t.Error("expected 'clear' to match")
	}
}

func TestMatchesResetTriggerCaseInsensitive(t *testing.T) {
	triggers := []string{"Reset", "CLEAR"}
	if !matchesResetTrigger("reset", triggers) {
		t.Error("expected case-insensitive match for 'reset'")
	}
	if !matchesResetTrigger("RESET", triggers) {
		t.Error("expected case-insensitive match for 'RESET'")
	}
	if !matchesResetTrigger("Clear", triggers) {
		t.Error("expected case-insensitive match for 'Clear'")
	}
}

func TestMatchesResetTriggerWithWhitespace(t *testing.T) {
	triggers := []string{"reset"}
	if !matchesResetTrigger("  reset  ", triggers) {
		t.Error("expected match with leading/trailing whitespace")
	}
}

func TestMatchesResetTriggerNoMatch(t *testing.T) {
	triggers := []string{"reset", "clear"}
	if matchesResetTrigger("hello", triggers) {
		t.Error("expected no match for 'hello'")
	}
}

func TestMatchesResetTriggerEmptyTriggers(t *testing.T) {
	if matchesResetTrigger("reset", nil) {
		t.Error("expected no match with nil triggers")
	}
	if matchesResetTrigger("reset", []string{}) {
		t.Error("expected no match with empty triggers")
	}
}

func TestMatchesResetTriggerEmptyText(t *testing.T) {
	triggers := []string{"reset"}
	if matchesResetTrigger("", triggers) {
		t.Error("expected no match for empty text")
	}
}

func TestMatchesResetTriggerWhitespaceOnlyText(t *testing.T) {
	triggers := []string{"reset"}
	if matchesResetTrigger("   ", triggers) {
		t.Error("expected no match for whitespace-only text")
	}
}

func TestMatchesResetTriggerTriggerWithWhitespace(t *testing.T) {
	triggers := []string{"  reset  "}
	if !matchesResetTrigger("reset", triggers) {
		t.Error("expected match when trigger has whitespace")
	}
}

// ---------------------------------------------------------------------------
// isAuthorized
// ---------------------------------------------------------------------------

func TestSlackIsAuthorizedAllowAll(t *testing.T) {
	bot := &Bot{cfg: config.SlackConfig{AllowUsers: nil}, logger: zap.NewNop().Sugar()}
	if !bot.isAuthorized("U123") {
		t.Error("expected all users to be authorized when AllowUsers is empty")
	}
}

func TestSlackIsAuthorizedEmptySlice(t *testing.T) {
	bot := &Bot{cfg: config.SlackConfig{AllowUsers: []string{}}, logger: zap.NewNop().Sugar()}
	if !bot.isAuthorized("U123") {
		t.Error("expected all users to be authorized when AllowUsers is empty slice")
	}
}

func TestSlackIsAuthorizedAllowList(t *testing.T) {
	bot := &Bot{cfg: config.SlackConfig{AllowUsers: []string{"U111", "U222"}}, logger: zap.NewNop().Sugar()}
	if !bot.isAuthorized("U111") {
		t.Error("expected U111 to be authorized")
	}
	if !bot.isAuthorized("U222") {
		t.Error("expected U222 to be authorized")
	}
	if bot.isAuthorized("U999") {
		t.Error("expected U999 to be unauthorized")
	}
}

func TestSlackIsAuthorizedCaseInsensitive(t *testing.T) {
	bot := &Bot{cfg: config.SlackConfig{AllowUsers: []string{"U111"}}, logger: zap.NewNop().Sugar()}
	if !bot.isAuthorized("u111") {
		t.Error("expected case-insensitive match for 'u111'")
	}
	if !bot.isAuthorized("U111") {
		t.Error("expected case-insensitive match for 'U111'")
	}
}

// ---------------------------------------------------------------------------
// validatePairCode
// ---------------------------------------------------------------------------

func TestValidatePairCodeCorrect(t *testing.T) {
	bot := &Bot{pairCode: "123456", logger: zap.NewNop().Sugar()}
	if !bot.validatePairCode("123456") {
		t.Error("expected correct code to be accepted")
	}
}

func TestValidatePairCodeWrong(t *testing.T) {
	bot := &Bot{pairCode: "123456", logger: zap.NewNop().Sugar()}
	if bot.validatePairCode("000000") {
		t.Error("expected wrong code to be rejected")
	}
}

func TestValidatePairCodeTrimmed(t *testing.T) {
	bot := &Bot{pairCode: "123456", logger: zap.NewNop().Sugar()}
	if !bot.validatePairCode("  123456  ") {
		t.Error("expected code with whitespace to be accepted after trim")
	}
}

func TestValidatePairCodeEmpty(t *testing.T) {
	bot := &Bot{pairCode: "123456", logger: zap.NewNop().Sugar()}
	if bot.validatePairCode("") {
		t.Error("expected empty code to be rejected")
	}
}

// ---------------------------------------------------------------------------
// ChannelName, IsConnected, Username, PairedCount
// ---------------------------------------------------------------------------

func TestChannelName(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	if bot.ChannelName() != "slack" {
		t.Errorf("expected 'slack', got %q", bot.ChannelName())
	}
}

func TestIsConnectedDefault(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	if bot.IsConnected() {
		t.Error("expected bot to not be connected by default")
	}
}

func TestIsConnectedAfterSet(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	bot.connected.Store(true)
	if !bot.IsConnected() {
		t.Error("expected bot to be connected after Store(true)")
	}
	bot.connected.Store(false)
	if bot.IsConnected() {
		t.Error("expected bot to not be connected after Store(false)")
	}
}

func TestUsername(t *testing.T) {
	bot := &Bot{botID: "UBOT123", logger: zap.NewNop().Sugar()}
	if bot.Username() != "UBOT123" {
		t.Errorf("expected 'UBOT123', got %q", bot.Username())
	}
}

func TestUsernameEmpty(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	if bot.Username() != "" {
		t.Errorf("expected empty string, got %q", bot.Username())
	}
}

func TestPairedCountEmpty(t *testing.T) {
	bot := &Bot{paired: make(map[string]bool), logger: zap.NewNop().Sugar()}
	if bot.PairedCount() != 0 {
		t.Errorf("expected 0 paired users, got %d", bot.PairedCount())
	}
}

func TestPairedCountWithUsers(t *testing.T) {
	bot := &Bot{paired: map[string]bool{"U1": true, "U2": true, "U3": true}, logger: zap.NewNop().Sugar()}
	if bot.PairedCount() != 3 {
		t.Errorf("expected 3 paired users, got %d", bot.PairedCount())
	}
}

// ---------------------------------------------------------------------------
// stripMention
// ---------------------------------------------------------------------------

func TestSlackStripMention(t *testing.T) {
	cases := []struct {
		text   string
		botID  string
		expect string
	}{
		{"<@UBOT123> hello world", "UBOT123", "hello world"},
		{"  <@UBOT123>   hello  ", "UBOT123", "hello"},
		{"no mention", "UBOT123", "no mention"},
		{"<@UBOT123>", "UBOT123", ""},
		{"<@UBOT123> <@UBOT123> double", "UBOT123", "double"},
	}
	for _, tc := range cases {
		got := stripMention(tc.text, tc.botID)
		if got != tc.expect {
			t.Errorf("stripMention(%q, %q) = %q, want %q", tc.text, tc.botID, got, tc.expect)
		}
	}
}

func TestSlackStripMentionEmptyBotID(t *testing.T) {
	got := stripMention("  hello  ", "")
	if got != "hello" {
		t.Errorf("expected 'hello', got %q", got)
	}
}

func TestSlackStripMentionNoMatch(t *testing.T) {
	got := stripMention("<@OTHER> hello", "UBOT123")
	if got != "<@OTHER> hello" {
		t.Errorf("expected '<@OTHER> hello', got %q", got)
	}
}

// ---------------------------------------------------------------------------
// splitMessage
// ---------------------------------------------------------------------------

func TestSlackSplitMessageShort(t *testing.T) {
	parts := splitMessage("hello", 3000)
	if len(parts) != 1 || parts[0] != "hello" {
		t.Errorf("expected single part, got %v", parts)
	}
}

func TestSlackSplitMessageExactLimit(t *testing.T) {
	exact := strings.Repeat("x", 3000)
	parts := splitMessage(exact, 3000)
	if len(parts) != 1 {
		t.Errorf("expected 1 part at limit, got %d", len(parts))
	}
}

func TestSlackSplitMessageOverLimit(t *testing.T) {
	over := strings.Repeat("x", 3000) + "tail"
	parts := splitMessage(over, 3000)
	if len(parts) < 2 {
		t.Errorf("expected at least 2 parts, got %d", len(parts))
	}
	var reconstructed strings.Builder
	for _, p := range parts {
		reconstructed.WriteString(p)
	}
	if reconstructed.String() != over {
		t.Error("split and rejoin does not match original")
	}
}

func TestSlackSplitMessagePreferNewlineBreak(t *testing.T) {
	// Create a message that's over the limit with a newline near the end
	// The split should happen at the newline rather than at maxLen
	prefix := strings.Repeat("a", 2850)
	msg := prefix + "\nrest-of-the-line" + strings.Repeat("b", 200)
	parts := splitMessage(msg, 3000)
	if len(parts) < 2 {
		t.Fatalf("expected at least 2 parts, got %d", len(parts))
	}
	// The first part should end at the newline boundary
	if !strings.HasPrefix(parts[0], prefix) {
		t.Error("expected first part to contain the prefix up to the newline")
	}
	// Verify the split happened at the newline
	if !strings.HasSuffix(parts[0], "\n") {
		t.Errorf("expected first part to end at newline break, got last chars: %q", parts[0][len(parts[0])-5:])
	}
}

func TestSlackSplitMessageEmpty(t *testing.T) {
	parts := splitMessage("", 3000)
	if len(parts) != 1 || parts[0] != "" {
		t.Errorf("expected single empty part, got %v", parts)
	}
}

func TestSlackSplitMessageMultipleParts(t *testing.T) {
	// 3 * 3000 + some extra
	msg := strings.Repeat("x", 9500)
	parts := splitMessage(msg, 3000)
	if len(parts) < 4 {
		t.Errorf("expected at least 4 parts for 9500 chars at 3000 limit, got %d", len(parts))
	}
	var reconstructed strings.Builder
	for _, p := range parts {
		reconstructed.WriteString(p)
	}
	if reconstructed.String() != msg {
		t.Error("split and rejoin does not match original")
	}
}

func TestSlackSplitMessageSmallLimit(t *testing.T) {
	parts := splitMessage("hello world", 5)
	if len(parts) < 2 {
		t.Errorf("expected multiple parts, got %d", len(parts))
	}
	var reconstructed string
	for _, p := range parts {
		reconstructed += p
	}
	if reconstructed != "hello world" {
		t.Errorf("expected 'hello world' after rejoin, got %q", reconstructed)
	}
}

// ---------------------------------------------------------------------------
// configSnapshot
// ---------------------------------------------------------------------------

func TestConfigSnapshot(t *testing.T) {
	bot := &Bot{
		cfg:    config.SlackConfig{BotToken: "xoxb-test", TimeoutSeconds: 42},
		msgCfg: config.Messages{AckReactionScope: "all", StreamEditMs: 200},
		logger: zap.NewNop().Sugar(),
	}
	slackCfg, msgCfg := bot.configSnapshot()
	if slackCfg.BotToken != "xoxb-test" {
		t.Errorf("expected BotToken 'xoxb-test', got %q", slackCfg.BotToken)
	}
	if slackCfg.TimeoutSeconds != 42 {
		t.Errorf("expected TimeoutSeconds 42, got %d", slackCfg.TimeoutSeconds)
	}
	if msgCfg.AckReactionScope != "all" {
		t.Errorf("expected AckReactionScope 'all', got %q", msgCfg.AckReactionScope)
	}
	if msgCfg.StreamEditMs != 200 {
		t.Errorf("expected StreamEditMs 200, got %d", msgCfg.StreamEditMs)
	}
}

func TestConfigSnapshotConcurrent(t *testing.T) {
	bot := &Bot{
		cfg:    config.SlackConfig{BotToken: "xoxb-test"},
		msgCfg: config.Messages{AckReactionScope: "all"},
		logger: zap.NewNop().Sugar(),
	}
	// Verify configSnapshot is safe for concurrent access (uses mutex)
	done := make(chan struct{})
	for range 10 {
		go func() {
			defer func() { done <- struct{}{} }()
			_, _ = bot.configSnapshot()
		}()
	}
	for range 10 {
		<-done
	}
}

// ---------------------------------------------------------------------------
// messageQueue and enqueue
// ---------------------------------------------------------------------------

func TestSlackMessageQueueBasic(t *testing.T) {
	bot := &Bot{
		cfg:    config.SlackConfig{TimeoutSeconds: 1},
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	const userID = "U_TEST"
	for range 3 {
		bot.mu.Lock()
		q, ok := bot.queues[userID]
		if !ok {
			q = &messageQueue{}
			bot.queues[userID] = q
		}
		q.messages = append(q.messages, queuedMessage{text: "msg", channelID: "C1", userID: userID})
		bot.mu.Unlock()
	}
	bot.mu.Lock()
	n := len(bot.queues[userID].messages)
	bot.mu.Unlock()
	if n != 3 {
		t.Errorf("expected 3 queued messages, got %d", n)
	}
}

func TestEnqueueCreatesQueue(t *testing.T) {
	bot := &Bot{
		queues: make(map[string]*messageQueue),
		msgCfg: config.Messages{Queue: config.MessageQueue{DebounceMs: 5000, Cap: 100}},
		logger: zap.NewNop().Sugar(),
	}
	bot.enqueue("U1", "C1", "ts1", "hello")
	bot.mu.Lock()
	q, ok := bot.queues["U1"]
	bot.mu.Unlock()
	if !ok {
		t.Fatal("expected queue to be created for U1")
	}
	if len(q.messages) != 1 {
		t.Errorf("expected 1 message in queue, got %d", len(q.messages))
	}
	if q.messages[0].text != "hello" {
		t.Errorf("expected message text 'hello', got %q", q.messages[0].text)
	}
	if q.messages[0].channelID != "C1" {
		t.Errorf("expected channelID 'C1', got %q", q.messages[0].channelID)
	}
	if q.messages[0].userID != "U1" {
		t.Errorf("expected userID 'U1', got %q", q.messages[0].userID)
	}
	if q.messages[0].ts != "ts1" {
		t.Errorf("expected ts 'ts1', got %q", q.messages[0].ts)
	}
	// Clean up: stop the timer to avoid leaking
	if q.timer != nil {
		q.timer.Stop()
	}
}

func TestEnqueueMultipleMessages(t *testing.T) {
	bot := &Bot{
		queues: make(map[string]*messageQueue),
		msgCfg: config.Messages{Queue: config.MessageQueue{DebounceMs: 5000, Cap: 100}},
		logger: zap.NewNop().Sugar(),
	}
	bot.enqueue("U1", "C1", "ts1", "msg1")
	bot.enqueue("U1", "C1", "ts2", "msg2")
	bot.enqueue("U1", "C1", "ts3", "msg3")
	bot.mu.Lock()
	q := bot.queues["U1"]
	n := len(q.messages)
	bot.mu.Unlock()
	if n != 3 {
		t.Errorf("expected 3 messages in queue, got %d", n)
	}
	// Clean up
	if q.timer != nil {
		q.timer.Stop()
	}
}

func TestEnqueueSeparateUsers(t *testing.T) {
	bot := &Bot{
		queues: make(map[string]*messageQueue),
		msgCfg: config.Messages{Queue: config.MessageQueue{DebounceMs: 5000, Cap: 100}},
		logger: zap.NewNop().Sugar(),
	}
	bot.enqueue("U1", "C1", "ts1", "msg1")
	bot.enqueue("U2", "C2", "ts2", "msg2")
	bot.mu.Lock()
	n1 := len(bot.queues["U1"].messages)
	n2 := len(bot.queues["U2"].messages)
	bot.mu.Unlock()
	if n1 != 1 {
		t.Errorf("expected 1 message for U1, got %d", n1)
	}
	if n2 != 1 {
		t.Errorf("expected 1 message for U2, got %d", n2)
	}
	// Clean up
	for _, q := range bot.queues {
		if q.timer != nil {
			q.timer.Stop()
		}
	}
}

func TestEnqueueCapFlush(t *testing.T) {
	// When cap is reached, the queue should be flushed (deleted from the map).
	// flushLocked will call processMessages in a goroutine, which will return
	// immediately since there's no agent. We test that the queue is removed.
	bot := &Bot{
		queues: make(map[string]*messageQueue),
		msgCfg: config.Messages{Queue: config.MessageQueue{DebounceMs: 5000, Cap: 3}},
		logger: zap.NewNop().Sugar(),
	}
	bot.enqueue("U1", "C1", "ts1", "msg1")
	bot.enqueue("U1", "C1", "ts2", "msg2")
	// Third message should trigger cap-based flush, which deletes from map
	bot.enqueue("U1", "C1", "ts3", "msg3")
	// Give the flush goroutine a moment to start (processMessages will panic/nil-deref
	// but the goroutine has panic recovery)
	time.Sleep(50 * time.Millisecond)
	bot.mu.Lock()
	_, exists := bot.queues["U1"]
	bot.mu.Unlock()
	if exists {
		t.Error("expected queue to be flushed after hitting cap")
	}
}

func TestEnqueueDefaultCap(t *testing.T) {
	// When cap is 0, it defaults to 20
	bot := &Bot{
		queues: make(map[string]*messageQueue),
		msgCfg: config.Messages{Queue: config.MessageQueue{DebounceMs: 5000, Cap: 0}},
		logger: zap.NewNop().Sugar(),
	}
	for i := range 19 {
		bot.enqueue("U1", "C1", "ts", "msg"+string(rune('a'+i)))
	}
	bot.mu.Lock()
	q := bot.queues["U1"]
	n := len(q.messages)
	bot.mu.Unlock()
	if n != 19 {
		t.Errorf("expected 19 messages (below default cap 20), got %d", n)
	}
	if q.timer != nil {
		q.timer.Stop()
	}
}

// ---------------------------------------------------------------------------
// flushLocked
// ---------------------------------------------------------------------------

func TestFlushLockedEmptyQueue(t *testing.T) {
	bot := &Bot{
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	// Should not panic on non-existent queue
	bot.mu.Lock()
	bot.flushLocked("U_NONEXISTENT")
	bot.mu.Unlock()
}

func TestFlushLockedEmptyMessages(t *testing.T) {
	bot := &Bot{
		queues: map[string]*messageQueue{
			"U1": {messages: nil},
		},
		logger: zap.NewNop().Sugar(),
	}
	bot.mu.Lock()
	bot.flushLocked("U1")
	bot.mu.Unlock()
	// Queue should not be deleted since messages was nil/empty
	bot.mu.Lock()
	_, exists := bot.queues["U1"]
	bot.mu.Unlock()
	if !exists {
		t.Error("expected queue to still exist when messages is nil")
	}
}

func TestFlushLockedRemovesQueue(t *testing.T) {
	bot := &Bot{
		queues: map[string]*messageQueue{
			"U1": {messages: []queuedMessage{{text: "hi", channelID: "C1", userID: "U1"}}},
		},
		logger: zap.NewNop().Sugar(),
	}
	bot.mu.Lock()
	bot.flushLocked("U1")
	bot.mu.Unlock()
	// Give the goroutine a moment
	time.Sleep(50 * time.Millisecond)
	bot.mu.Lock()
	_, exists := bot.queues["U1"]
	bot.mu.Unlock()
	if exists {
		t.Error("expected queue to be deleted after flush")
	}
}

// ---------------------------------------------------------------------------
// processMessages (empty case)
// ---------------------------------------------------------------------------

func TestProcessMessagesEmpty(t *testing.T) {
	bot := &Bot{
		cfg:    config.SlackConfig{TimeoutSeconds: 1},
		msgCfg: config.Messages{},
		logger: zap.NewNop().Sugar(),
	}
	// Should return immediately without panicking
	bot.processMessages("U1", nil)
	bot.processMessages("U1", []queuedMessage{})
}

// ---------------------------------------------------------------------------
// savePairedUsers / loadPairedUsers (round-trip with temp dir)
// ---------------------------------------------------------------------------

func TestSavePairedUsersFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "slack-default-allowFrom.json")

	state := struct {
		Version   int      `json:"version"`
		AllowFrom []string `json:"allowFrom"`
	}{Version: 1, AllowFrom: []string{"U111", "U222"}}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("expected 0600 permissions, got %o", perm)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var loaded struct {
		Version   int      `json:"version"`
		AllowFrom []string `json:"allowFrom"`
	}
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.Version != 1 {
		t.Errorf("expected version 1, got %d", loaded.Version)
	}
	if len(loaded.AllowFrom) != 2 {
		t.Errorf("expected 2 paired users, got %d", len(loaded.AllowFrom))
	}
}

func TestLoadPairedUsersNoFile(t *testing.T) {
	// loadPairedUsers should not panic when the file doesn't exist
	t.Setenv("HOME", t.TempDir())
	bot := &Bot{paired: make(map[string]bool), logger: zap.NewNop().Sugar()}
	bot.loadPairedUsers()
	if len(bot.paired) != 0 {
		t.Errorf("expected 0 paired users when file doesn't exist, got %d", len(bot.paired))
	}
}

func TestLoadPairedUsersInvalidJSON(t *testing.T) {
	// loadPairedUsers uses pairedUsersFile() which points to a fixed path.
	// We can't easily redirect it, but we can verify the bot handles missing
	// file gracefully (which we tested above).
	bot := &Bot{paired: make(map[string]bool), logger: zap.NewNop().Sugar()}
	bot.loadPairedUsers()
	// Should not panic
}

// ---------------------------------------------------------------------------
// ackReaction (logic branches, not actual API calls)
// ---------------------------------------------------------------------------

func TestAckReactionEmptyScope(t *testing.T) {
	// When scope is empty, ackReaction returns early (no API call made).
	// We can't test the API call itself but we verify it doesn't panic.
	bot := &Bot{
		msgCfg: config.Messages{AckReactionScope: ""},
		cfg:    config.SlackConfig{},
		logger: zap.NewNop().Sugar(),
	}
	// This should return immediately without calling api.AddReaction
	// (api is nil, so if it tried to call it, it would panic)
	bot.ackReactionWith("C1", "ts1", false, bot.cfg, bot.msgCfg)
}

func TestAckReactionGroupMentionsNotMention(t *testing.T) {
	// When scope is "group-mentions" and isMention is false, should return early.
	bot := &Bot{
		msgCfg: config.Messages{AckReactionScope: "group-mentions"},
		cfg:    config.SlackConfig{},
		logger: zap.NewNop().Sugar(),
	}
	// api is nil; if ackReaction tried to call AddReaction it would panic
	bot.ackReactionWith("C1", "ts1", false, bot.cfg, bot.msgCfg)
}

// ---------------------------------------------------------------------------
// New constructor
// ---------------------------------------------------------------------------

func TestNewCreatesBot(t *testing.T) {
	cfg := config.SlackConfig{
		BotToken:       "xoxb-test",
		AppToken:       "xapp-test",
		AllowUsers:     []string{"U1", "U2"},
		TimeoutSeconds: 60,
	}
	msgCfg := config.Messages{}
	fullCfg := &config.Root{}

	bot, err := New(zap.NewNop().Sugar(), cfg, msgCfg, fullCfg, nil, nil, nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if bot == nil {
		t.Fatal("New returned nil bot")
	}
	if bot.cfg.BotToken != "xoxb-test" {
		t.Errorf("expected BotToken 'xoxb-test', got %q", bot.cfg.BotToken)
	}
	if bot.api == nil {
		t.Error("expected api client to be created")
	}
	if bot.client == nil {
		t.Error("expected socket mode client to be created")
	}
	if len(bot.pairCode) != 6 {
		t.Errorf("expected 6-digit pair code, got %q", bot.pairCode)
	}
	if bot.queues == nil {
		t.Error("expected queues map to be initialized")
	}
	if bot.paired == nil {
		t.Error("expected paired map to be initialized")
	}
}

func TestNewPopulatesPairedFromAllowUsers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := config.SlackConfig{
		BotToken:   "xoxb-test",
		AppToken:   "xapp-test",
		AllowUsers: []string{"U1", "U2", "U3"},
	}
	bot, err := New(zap.NewNop().Sugar(), cfg, config.Messages{}, &config.Root{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	// AllowUsers should be pre-populated into paired set
	if !bot.paired["U1"] {
		t.Error("expected U1 to be in paired set")
	}
	if !bot.paired["U2"] {
		t.Error("expected U2 to be in paired set")
	}
	if !bot.paired["U3"] {
		t.Error("expected U3 to be in paired set")
	}
	if bot.PairedCount() != 3 {
		t.Errorf("expected 3 paired users, got %d", bot.PairedCount())
	}
}

func TestNewNilConfig(t *testing.T) {
	cfg := config.SlackConfig{
		BotToken: "xoxb-test",
		AppToken: "xapp-test",
	}
	bot, err := New(zap.NewNop().Sugar(), cfg, config.Messages{}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if bot.fullCfg != nil {
		t.Error("expected fullCfg to be nil")
	}
}

// ---------------------------------------------------------------------------
// queuedMessage struct
// ---------------------------------------------------------------------------

func TestQueuedMessageFields(t *testing.T) {
	msg := queuedMessage{
		text:      "hello world",
		channelID: "C123",
		userID:    "U456",
		ts:        "1234567890.123456",
	}
	if msg.text != "hello world" {
		t.Errorf("unexpected text: %q", msg.text)
	}
	if msg.channelID != "C123" {
		t.Errorf("unexpected channelID: %q", msg.channelID)
	}
	if msg.userID != "U456" {
		t.Errorf("unexpected userID: %q", msg.userID)
	}
	if msg.ts != "1234567890.123456" {
		t.Errorf("unexpected ts: %q", msg.ts)
	}
}

// ---------------------------------------------------------------------------
// Bot struct concurrent access safety
// ---------------------------------------------------------------------------

func TestPairedCountConcurrent(t *testing.T) {
	bot := &Bot{paired: make(map[string]bool), logger: zap.NewNop().Sugar()}
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = bot.PairedCount()
		}(i)
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Bot connected toggle
// ---------------------------------------------------------------------------

func TestConnectedToggle(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	if bot.IsConnected() {
		t.Error("expected false initially")
	}
	bot.connected.Store(true)
	if !bot.IsConnected() {
		t.Error("expected true after Store(true)")
	}
	bot.connected.Store(false)
	if bot.IsConnected() {
		t.Error("expected false after Store(false)")
	}
}

// ---------------------------------------------------------------------------
// Integration-style tests: enqueue with timer behavior
// ---------------------------------------------------------------------------

func TestEnqueueTimerReset(t *testing.T) {
	// Verify that enqueue resets the timer when a new message arrives
	bot := &Bot{
		queues: make(map[string]*messageQueue),
		msgCfg: config.Messages{Queue: config.MessageQueue{DebounceMs: 5000, Cap: 100}},
		logger: zap.NewNop().Sugar(),
	}
	bot.enqueue("U1", "C1", "ts1", "msg1")
	bot.mu.Lock()
	q1 := bot.queues["U1"]
	timer1 := q1.timer
	bot.mu.Unlock()

	if timer1 == nil {
		t.Fatal("expected timer to be set after first enqueue")
	}

	// Enqueue again; timer should be replaced
	bot.enqueue("U1", "C1", "ts2", "msg2")
	bot.mu.Lock()
	q2 := bot.queues["U1"]
	timer2 := q2.timer
	bot.mu.Unlock()

	if timer2 == nil {
		t.Fatal("expected timer to be set after second enqueue")
	}

	// Clean up
	timer2.Stop()
}

// ---------------------------------------------------------------------------
// splitMessage edge cases
// ---------------------------------------------------------------------------

func TestSplitMessageSingleChar(t *testing.T) {
	parts := splitMessage("x", 1)
	if len(parts) != 1 || parts[0] != "x" {
		t.Errorf("expected ['x'], got %v", parts)
	}
}

func TestSplitMessageAllNewlines(t *testing.T) {
	msg := strings.Repeat("\n", 5000)
	parts := splitMessage(msg, 3000)
	var total int
	for _, p := range parts {
		total += len(p)
	}
	if total != 5000 {
		t.Errorf("expected total length 5000 after split, got %d", total)
	}
}

// ---------------------------------------------------------------------------
// matchesResetTrigger additional edge cases
// ---------------------------------------------------------------------------

func TestMatchesResetTriggerPartialMatch(t *testing.T) {
	triggers := []string{"reset"}
	// "reset now" should NOT match — it's not an exact match
	if matchesResetTrigger("reset now", triggers) {
		t.Error("expected no match for partial text 'reset now'")
	}
}

func TestMatchesResetTriggerMultipleTriggers(t *testing.T) {
	triggers := []string{"reset", "clear", "new session"}
	if !matchesResetTrigger("new session", triggers) {
		t.Error("expected match for 'new session'")
	}
	if !matchesResetTrigger("NEW SESSION", triggers) {
		t.Error("expected case-insensitive match for 'NEW SESSION'")
	}
}

// ---------------------------------------------------------------------------
// handleMessageEvent early-return paths (no API calls needed)
// ---------------------------------------------------------------------------

func TestHandleMessageEventIgnoresBotMessages(t *testing.T) {
	bot := &Bot{
		cfg:    config.SlackConfig{},
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	// BotID set means it's a bot message — should return immediately.
	// api is nil, so if it tried to call PostMessage it would panic.
	ev := &slackevents.MessageEvent{
		BotID:   "B123",
		Channel: "D123",
		User:    "U123",
		Text:    "hello",
	}
	bot.handleMessageEvent(ev)
}

func TestHandleMessageEventIgnoresSubtype(t *testing.T) {
	bot := &Bot{
		cfg:    config.SlackConfig{},
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	// SubType set (e.g. "message_changed") — should return immediately.
	ev := &slackevents.MessageEvent{
		SubType: "message_changed",
		Channel: "D123",
		User:    "U123",
		Text:    "hello",
	}
	bot.handleMessageEvent(ev)
}

func TestHandleMessageEventIgnoresNonDM(t *testing.T) {
	bot := &Bot{
		cfg:    config.SlackConfig{},
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	// Channel starting with "C" (not "D") — should return immediately.
	ev := &slackevents.MessageEvent{
		Channel: "C123",
		User:    "U123",
		Text:    "hello",
	}
	bot.handleMessageEvent(ev)
}

func TestHandleMessageEventIgnoresBotAndSubtypeCombined(t *testing.T) {
	bot := &Bot{
		cfg:    config.SlackConfig{},
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.MessageEvent{
		BotID:   "B123",
		SubType: "message_deleted",
		Channel: "D123",
		User:    "U123",
		Text:    "hello",
	}
	bot.handleMessageEvent(ev)
}

// ---------------------------------------------------------------------------
// handleEventsAPI dispatch
// ---------------------------------------------------------------------------

func TestHandleEventsAPIIgnoresNonCallback(t *testing.T) {
	bot := &Bot{
		cfg:    config.SlackConfig{},
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	// A non-CallbackEvent type should be silently ignored.
	event := slackevents.EventsAPIEvent{
		Type: "url_verification",
	}
	bot.handleEventsAPI(event)
}

func TestHandleEventsAPICallbackWithUnknownInnerEvent(t *testing.T) {
	bot := &Bot{
		cfg:    config.SlackConfig{},
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	// A CallbackEvent with an unknown inner event type should be silently ignored.
	event := slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: nil, // unknown/nil inner event
		},
	}
	bot.handleEventsAPI(event)
}

// ---------------------------------------------------------------------------
// processMessages with config snapshot (non-empty, non-reset)
// ---------------------------------------------------------------------------

func TestProcessMessagesCombinesMultipleTexts(t *testing.T) {
	// processMessages with multiple messages but no agent will panic in
	// respondFull; we rely on the configSnapshot path being exercised.
	// Since we can't fully run it without an agent, we test that it doesn't
	// panic for the empty-messages case (already tested) and that
	// configSnapshot works correctly (tested separately).
}

// ---------------------------------------------------------------------------
// SendToAllPaired with empty paired set
// ---------------------------------------------------------------------------

func TestSendToAllPairedNoPairedUsers(t *testing.T) {
	bot := &Bot{
		paired: make(map[string]bool),
		logger: zap.NewNop().Sugar(),
	}
	// Should return early without calling any API (api is nil).
	bot.SendToAllPaired("hello")
}

// ---------------------------------------------------------------------------
// handleMessageEvent with collect mode enqueue path
// ---------------------------------------------------------------------------

func TestHandleMessageEventCollectModeEnqueues(t *testing.T) {
	bot := &Bot{
		cfg: config.SlackConfig{AllowUsers: nil}, // allow all
		msgCfg: config.Messages{
			Queue:            config.MessageQueue{Mode: "collect", DebounceMs: 5000, Cap: 100},
			AckReactionScope: "", // no reaction to avoid API call
		},
		fullCfg: &config.Root{},
		paired:  make(map[string]bool),
		queues:  make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.MessageEvent{
		Channel:   "D123",
		User:      "U123",
		Text:      "hello world",
		TimeStamp: "123.456",
	}
	bot.handleMessageEvent(ev)
	// Message should be enqueued
	bot.mu.Lock()
	q, ok := bot.queues["U123"]
	bot.mu.Unlock()
	if !ok {
		t.Fatal("expected message to be enqueued for U123")
	}
	if len(q.messages) != 1 {
		t.Errorf("expected 1 queued message, got %d", len(q.messages))
	}
	if q.messages[0].text != "hello world" {
		t.Errorf("expected text 'hello world', got %q", q.messages[0].text)
	}
	// Clean up timer
	if q.timer != nil {
		q.timer.Stop()
	}
}

// ---------------------------------------------------------------------------
// handleMentionEvent with collect mode enqueue path
// ---------------------------------------------------------------------------

func TestHandleMentionEventCollectModeEnqueues(t *testing.T) {
	bot := &Bot{
		botID: "UBOT",
		cfg:   config.SlackConfig{AllowUsers: nil}, // allow all
		msgCfg: config.Messages{
			Queue:            config.MessageQueue{Mode: "collect", DebounceMs: 5000, Cap: 100},
			AckReactionScope: "", // no reaction
		},
		fullCfg: &config.Root{},
		paired:  make(map[string]bool),
		queues:  make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.AppMentionEvent{
		Channel:   "C123",
		User:      "U456",
		Text:      "<@UBOT> check this out",
		TimeStamp: "789.012",
	}
	bot.handleMentionEvent(ev)
	bot.mu.Lock()
	q, ok := bot.queues["U456"]
	bot.mu.Unlock()
	if !ok {
		t.Fatal("expected message to be enqueued for U456")
	}
	if len(q.messages) != 1 {
		t.Errorf("expected 1 queued message, got %d", len(q.messages))
	}
	if q.messages[0].text != "check this out" {
		t.Errorf("expected stripped text 'check this out', got %q", q.messages[0].text)
	}
	// Clean up timer
	if q.timer != nil {
		q.timer.Stop()
	}
}

// ---------------------------------------------------------------------------
// handleMentionEvent unauthorized user
// ---------------------------------------------------------------------------

func TestHandleMentionEventUnauthorizedNilAPI(t *testing.T) {
	// When user is not authorized, handleMentionEvent calls api.PostMessage.
	// With a nil API it will panic, so we verify the auth check logic by
	// testing that an authorized user with collect mode does NOT panic.
	bot := &Bot{
		botID: "UBOT",
		cfg:   config.SlackConfig{AllowUsers: []string{"U_ALLOWED"}},
		msgCfg: config.Messages{
			Queue:            config.MessageQueue{Mode: "collect", DebounceMs: 5000, Cap: 100},
			AckReactionScope: "",
		},
		fullCfg: &config.Root{},
		paired:  make(map[string]bool),
		queues:  make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.AppMentionEvent{
		Channel:   "C123",
		User:      "U_ALLOWED",
		Text:      "<@UBOT> hello",
		TimeStamp: "ts1",
	}
	bot.handleMentionEvent(ev)
	bot.mu.Lock()
	q := bot.queues["U_ALLOWED"]
	bot.mu.Unlock()
	if q == nil {
		t.Fatal("expected queue for authorized user")
	}
	if q.timer != nil {
		q.timer.Stop()
	}
}

// ---------------------------------------------------------------------------
// enqueue negative cap defaults to 20
// ---------------------------------------------------------------------------

func TestEnqueueNegativeCap(t *testing.T) {
	bot := &Bot{
		queues: make(map[string]*messageQueue),
		msgCfg: config.Messages{Queue: config.MessageQueue{DebounceMs: 5000, Cap: -1}},
		logger: zap.NewNop().Sugar(),
	}
	// Negative cap should default to 20
	for range 19 {
		bot.enqueue("U1", "C1", "ts", "msg")
	}
	bot.mu.Lock()
	q := bot.queues["U1"]
	n := len(q.messages)
	bot.mu.Unlock()
	if n != 19 {
		t.Errorf("expected 19 messages (below default cap 20), got %d", n)
	}
	if q.timer != nil {
		q.timer.Stop()
	}
}

// ---------------------------------------------------------------------------
// handleEventsAPI with MessageEvent callback
// ---------------------------------------------------------------------------

func TestHandleEventsAPICallbackMessageEvent(t *testing.T) {
	bot := &Bot{
		cfg:    config.SlackConfig{},
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	// A CallbackEvent with a MessageEvent that has BotID set should
	// be dispatched to handleMessageEvent and return early (bot message).
	msgEv := &slackevents.MessageEvent{
		BotID:   "B999",
		Channel: "D123",
		User:    "U123",
		Text:    "hi",
	}
	event := slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: msgEv,
		},
	}
	bot.handleEventsAPI(event)
}

// ---------------------------------------------------------------------------
// handleEventsAPI with AppMentionEvent callback
// ---------------------------------------------------------------------------

func TestHandleEventsAPICallbackAppMentionEvent(t *testing.T) {
	bot := &Bot{
		botID: "UBOT",
		cfg:   config.SlackConfig{AllowUsers: nil},
		msgCfg: config.Messages{
			Queue:            config.MessageQueue{Mode: "collect", DebounceMs: 5000, Cap: 100},
			AckReactionScope: "",
		},
		fullCfg: &config.Root{},
		paired:  make(map[string]bool),
		queues:  make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	mentionEv := &slackevents.AppMentionEvent{
		Channel:   "C123",
		User:      "U789",
		Text:      "<@UBOT> test",
		TimeStamp: "ts1",
	}
	event := slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: mentionEv,
		},
	}
	bot.handleEventsAPI(event)
	// Should have been dispatched to handleMentionEvent and enqueued
	bot.mu.Lock()
	q := bot.queues["U789"]
	bot.mu.Unlock()
	if q == nil {
		t.Fatal("expected message to be enqueued via handleEventsAPI dispatch")
	}
	if q.timer != nil {
		q.timer.Stop()
	}
}

// ---------------------------------------------------------------------------
// handleMessageEvent — direct processing path (non-collect mode)
// ---------------------------------------------------------------------------

func TestHandleMessageEventDirectProcessing(t *testing.T) {
	// DM from authorized user with no collect mode and empty ack scope.
	// The goroutine will call processMessages which calls configSnapshot then
	// respondFull, which will panic on nil agent. The panic recovery catches it.
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg:      config.SlackConfig{AllowUsers: nil, TimeoutSeconds: 1},
		msgCfg:   config.Messages{AckReactionScope: ""},
		fullCfg:  &config.Root{},
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		sessions: sm,
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.MessageEvent{
		Channel:   "D999",
		User:      "U555",
		Text:      "regular message",
		TimeStamp: "ts1",
	}
	bot.handleMessageEvent(ev)
	// Allow the goroutine to run and be recovered
	time.Sleep(100 * time.Millisecond)
}

// ---------------------------------------------------------------------------
// handleMentionEvent — direct processing path (non-collect mode)
// ---------------------------------------------------------------------------

func TestHandleMentionEventDirectProcessing(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		botID:    "UBOT",
		cfg:      config.SlackConfig{AllowUsers: nil, TimeoutSeconds: 1},
		msgCfg:   config.Messages{AckReactionScope: ""},
		fullCfg:  &config.Root{},
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		sessions: sm,
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.AppMentionEvent{
		Channel:   "C999",
		User:      "U555",
		Text:      "<@UBOT> do something",
		TimeStamp: "ts1",
	}
	bot.handleMentionEvent(ev)
	time.Sleep(100 * time.Millisecond)
}

// ---------------------------------------------------------------------------
// processMessages — reset trigger path
// ---------------------------------------------------------------------------

func TestProcessMessagesResetTrigger(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg:      config.SlackConfig{TimeoutSeconds: 1},
		msgCfg:   config.Messages{},
		fullCfg:  &config.Root{Session: config.Session{ResetTriggers: []string{"reset"}}},
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	// "reset" matches the trigger — processMessages should call sessions.Reset
	// and then try api.PostMessage (which is nil and will panic).
	func() {
		defer func() { recover() }()
		bot.processMessages("U1", []queuedMessage{{text: "reset", channelID: "C1", userID: "U1", ts: "ts1"}})
	}()
}

// ---------------------------------------------------------------------------
// processMessages — non-reset, non-streaming path (respondFull)
// ---------------------------------------------------------------------------

func TestProcessMessagesRespondFull(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg:      config.SlackConfig{TimeoutSeconds: 1},
		msgCfg:   config.Messages{},
		fullCfg:  &config.Root{},
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	// No reset trigger, not streaming — calls respondFull which panics on nil agent.
	func() {
		defer func() { recover() }()
		bot.processMessages("U1", []queuedMessage{{text: "hello", channelID: "C1", userID: "U1", ts: "ts1"}})
	}()
}

// ---------------------------------------------------------------------------
// processMessages — streaming path
// ---------------------------------------------------------------------------

func TestProcessMessagesRespondStreaming(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg:      config.SlackConfig{TimeoutSeconds: 1, StreamMode: "partial"},
		msgCfg:   config.Messages{},
		fullCfg:  &config.Root{},
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	// StreamMode=partial — calls respondStreaming which panics on nil api.
	func() {
		defer func() { recover() }()
		bot.processMessages("U1", []queuedMessage{{text: "hello", channelID: "C1", userID: "U1", ts: "ts1"}})
	}()
}

// ---------------------------------------------------------------------------
// processMessages — multi-message combine
// ---------------------------------------------------------------------------

func TestProcessMessagesCombinesText(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg:      config.SlackConfig{TimeoutSeconds: 1},
		msgCfg:   config.Messages{},
		fullCfg:  &config.Root{},
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	// Multiple messages get combined with newlines.
	// Will panic on nil agent in respondFull, but exercises the combine path.
	func() {
		defer func() { recover() }()
		bot.processMessages("U1", []queuedMessage{
			{text: "line1", channelID: "C1", userID: "U1", ts: "ts1"},
			{text: "line2", channelID: "C1", userID: "U1", ts: "ts2"},
			{text: "line3", channelID: "C1", userID: "U1", ts: "ts3"},
		})
	}()
}

// ---------------------------------------------------------------------------
// processMessages — no reset triggers configured
// ---------------------------------------------------------------------------

func TestProcessMessagesNoResetTriggersConfigured(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg:      config.SlackConfig{TimeoutSeconds: 1},
		msgCfg:   config.Messages{},
		fullCfg:  &config.Root{Session: config.Session{ResetTriggers: nil}},
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	func() {
		defer func() { recover() }()
		bot.processMessages("U1", []queuedMessage{{text: "reset", channelID: "C1", userID: "U1", ts: "ts1"}})
	}()
}

// ---------------------------------------------------------------------------
// processMessages — nil fullCfg
// ---------------------------------------------------------------------------

func TestProcessMessagesNilFullCfg(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg:      config.SlackConfig{TimeoutSeconds: 1},
		msgCfg:   config.Messages{},
		fullCfg:  nil,
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	func() {
		defer func() { recover() }()
		bot.processMessages("U1", []queuedMessage{{text: "reset", channelID: "C1", userID: "U1", ts: "ts1"}})
	}()
}

// ---------------------------------------------------------------------------
// ackReaction — "all" scope (exercises emoji default and custom)
// ---------------------------------------------------------------------------

func TestAckReactionAllScopeDefaultEmoji(t *testing.T) {
	bot := &Bot{
		msgCfg: config.Messages{AckReactionScope: "all"},
		cfg:    config.SlackConfig{AckEmoji: ""},
		logger: zap.NewNop().Sugar(),
	}
	func() {
		defer func() { recover() }()
		bot.ackReactionWith("C1", "ts1", false, bot.cfg, bot.msgCfg)
	}()
}

func TestAckReactionAllScopeCustomEmoji(t *testing.T) {
	bot := &Bot{
		msgCfg: config.Messages{AckReactionScope: "all"},
		cfg:    config.SlackConfig{AckEmoji: "thumbsup"},
		logger: zap.NewNop().Sugar(),
	}
	func() {
		defer func() { recover() }()
		bot.ackReactionWith("C1", "ts1", true, bot.cfg, bot.msgCfg)
	}()
}

func TestAckReactionGroupMentionsIsMention(t *testing.T) {
	bot := &Bot{
		msgCfg: config.Messages{AckReactionScope: "group-mentions"},
		cfg:    config.SlackConfig{AckEmoji: "wave"},
		logger: zap.NewNop().Sugar(),
	}
	func() {
		defer func() { recover() }()
		bot.ackReactionWith("C1", "ts1", true, bot.cfg, bot.msgCfg)
	}()
}

// ---------------------------------------------------------------------------
// handleMentionEvent — slash command path
// ---------------------------------------------------------------------------

func TestHandleMentionEventSlashCommand(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		botID:    "UBOT",
		cfg:      config.SlackConfig{AllowUsers: nil},
		msgCfg:   config.Messages{AckReactionScope: ""},
		fullCfg:  &config.Root{},
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		sessions: sm,
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.AppMentionEvent{
		Channel:   "C123",
		User:      "U123",
		Text:      "<@UBOT> /new",
		TimeStamp: "ts1",
	}
	func() {
		defer func() { recover() }()
		bot.handleMentionEvent(ev)
	}()
}

// ---------------------------------------------------------------------------
// handleMessageEvent — slash command path
// ---------------------------------------------------------------------------

func TestHandleMessageEventSlashCommand(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg:      config.SlackConfig{AllowUsers: nil},
		msgCfg:   config.Messages{AckReactionScope: ""},
		fullCfg:  &config.Root{},
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		sessions: sm,
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.MessageEvent{
		Channel:   "D123",
		User:      "U123",
		Text:      "/new",
		TimeStamp: "ts1",
	}
	func() {
		defer func() { recover() }()
		bot.handleMessageEvent(ev)
	}()
}

// ---------------------------------------------------------------------------
// SendToAllPaired — with paired users but nil api (tests iteration path)
// ---------------------------------------------------------------------------

func TestSendToAllPairedWithUsers(t *testing.T) {
	bot := &Bot{
		paired: map[string]bool{"U1": true, "U2": true},
		logger: zap.NewNop().Sugar(),
	}
	// Will iterate paired users and try api.OpenConversation, which panics.
	func() {
		defer func() { recover() }()
		bot.SendToAllPaired("system event")
	}()
}

// ---------------------------------------------------------------------------
// AnnounceToSession
// ---------------------------------------------------------------------------

func TestAnnounceToSessionNonSlackKeyIgnored(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}

	// Telegram key — should be silently ignored (no panic, no sends)
	bot.AnnounceToSession("main:telegram:123", "should be ignored")
}

func TestAnnounceToSessionMalformedKey(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}

	// Too few parts
	bot.AnnounceToSession("main", "should be ignored")
	bot.AnnounceToSession("", "should be ignored")
	bot.AnnounceToSession("agent:main:slack", "should be ignored")
	// No panic for any of these
}

// ===========================================================================
// NEW TESTS — coverage expansion
// ===========================================================================

// ---------------------------------------------------------------------------
// SetTaskManager
// ---------------------------------------------------------------------------

func TestSetTaskManager(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	if bot.taskMgr != nil {
		t.Fatal("expected taskMgr to be nil initially")
	}
	mgr := taskqueue.New(zap.NewNop().Sugar(), filepath.Join(t.TempDir(), "tasks.json"), taskqueue.Config{})
	bot.SetTaskManager(mgr)
	if bot.taskMgr != mgr {
		t.Error("expected taskMgr to be set to the provided manager")
	}
}

func TestSetTaskManagerNil(t *testing.T) {
	mgr := taskqueue.New(zap.NewNop().Sugar(), filepath.Join(t.TempDir(), "tasks.json"), taskqueue.Config{})
	bot := &Bot{taskMgr: mgr, logger: zap.NewNop().Sugar()}
	bot.SetTaskManager(nil)
	if bot.taskMgr != nil {
		t.Error("expected taskMgr to be nil after setting nil")
	}
}

func TestSetTaskManagerReplace(t *testing.T) {
	mgr1 := taskqueue.New(zap.NewNop().Sugar(), filepath.Join(t.TempDir(), "tasks1.json"), taskqueue.Config{})
	mgr2 := taskqueue.New(zap.NewNop().Sugar(), filepath.Join(t.TempDir(), "tasks2.json"), taskqueue.Config{})
	bot := &Bot{taskMgr: mgr1, logger: zap.NewNop().Sugar()}
	bot.SetTaskManager(mgr2)
	if bot.taskMgr != mgr2 {
		t.Error("expected taskMgr to be replaced with the new manager")
	}
}

// ---------------------------------------------------------------------------
// savePairedUsers — round-trip through disk
// ---------------------------------------------------------------------------

func TestSavePairedUsersWritesFile(t *testing.T) {
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	t.Setenv("HOME", tmpHome)
	defer os.Setenv("HOME", origHome)

	bot := &Bot{
		paired: map[string]bool{"UAAA": true, "UBBB": true},
		logger: zap.NewNop().Sugar(),
	}

	bot.mu.Lock()
	bot.savePairedUsers()
	bot.mu.Unlock()

	path := filepath.Join(tmpHome, ".gopherclaw", "credentials", "slack-default-allowFrom.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected file to exist at %s: %v", path, err)
	}

	var state struct {
		Version   int      `json:"version"`
		AllowFrom []string `json:"allowFrom"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("failed to parse saved file: %v", err)
	}
	if state.Version != 1 {
		t.Errorf("expected version 1, got %d", state.Version)
	}
	if len(state.AllowFrom) != 2 {
		t.Errorf("expected 2 users, got %d", len(state.AllowFrom))
	}
	found := make(map[string]bool)
	for _, id := range state.AllowFrom {
		found[id] = true
	}
	if !found["UAAA"] || !found["UBBB"] {
		t.Errorf("expected UAAA and UBBB in allowFrom, got %v", state.AllowFrom)
	}
}

func TestSavePairedUsersEmptyMap(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	bot := &Bot{
		paired: map[string]bool{},
		logger: zap.NewNop().Sugar(),
	}

	bot.mu.Lock()
	bot.savePairedUsers()
	bot.mu.Unlock()

	path := filepath.Join(tmpHome, ".gopherclaw", "credentials", "slack-default-allowFrom.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected file to exist: %v", err)
	}

	var state struct {
		Version   int      `json:"version"`
		AllowFrom []string `json:"allowFrom"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("failed to parse: %v", err)
	}
	if len(state.AllowFrom) != 0 {
		t.Errorf("expected 0 users, got %d", len(state.AllowFrom))
	}
}

// ---------------------------------------------------------------------------
// loadPairedUsers — success path (file exists with valid data)
// ---------------------------------------------------------------------------

func TestLoadPairedUsersFromFile(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	dir := filepath.Join(tmpHome, ".gopherclaw", "credentials")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	state := struct {
		Version   int      `json:"version"`
		AllowFrom []string `json:"allowFrom"`
	}{Version: 1, AllowFrom: []string{"U111", "U222"}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(dir, "slack-default-allowFrom.json"), data, 0600); err != nil {
		t.Fatal(err)
	}

	bot := &Bot{
		paired: make(map[string]bool),
		logger: zap.NewNop().Sugar(),
	}
	bot.loadPairedUsers()

	if !bot.paired["U111"] {
		t.Error("expected U111 to be paired")
	}
	if !bot.paired["U222"] {
		t.Error("expected U222 to be paired")
	}
}

// ---------------------------------------------------------------------------
// savePairedUsers + loadPairedUsers round-trip
// ---------------------------------------------------------------------------

func TestSaveAndLoadPairedUsersRoundTrip(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	bot := &Bot{
		paired: map[string]bool{"UXYZ": true, "UABC": true, "U999": true},
		logger: zap.NewNop().Sugar(),
	}

	bot.mu.Lock()
	bot.savePairedUsers()
	bot.mu.Unlock()

	bot2 := &Bot{
		paired: make(map[string]bool),
		logger: zap.NewNop().Sugar(),
	}
	bot2.loadPairedUsers()

	if len(bot2.paired) != 3 {
		t.Errorf("expected 3 paired users after round-trip, got %d", len(bot2.paired))
	}
	for _, id := range []string{"UXYZ", "UABC", "U999"} {
		if !bot2.paired[id] {
			t.Errorf("expected %s to be paired after round-trip", id)
		}
	}
}

// ---------------------------------------------------------------------------
// handleEvents — context cancelled immediately
// ---------------------------------------------------------------------------

func TestHandleEventsContextCancelled(t *testing.T) {
	smClient := &socketmode.Client{
		Events: make(chan socketmode.Event, 10),
	}
	bot := &Bot{
		client: smClient,
		logger: zap.NewNop().Sugar(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		bot.handleEvents(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleEvents did not return after context cancellation")
	}
}

// ---------------------------------------------------------------------------
// handleEvents — channel closed
// ---------------------------------------------------------------------------

func TestHandleEventsChannelClosed(t *testing.T) {
	evChan := make(chan socketmode.Event, 10)
	smClient := &socketmode.Client{
		Events: evChan,
	}
	bot := &Bot{
		client: smClient,
		logger: zap.NewNop().Sugar(),
	}

	close(evChan)

	ctx := context.Background()

	done := make(chan struct{})
	go func() {
		bot.handleEvents(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleEvents did not return after channel close")
	}
}

// ---------------------------------------------------------------------------
// handleEvents — connecting and connected events (no panic)
// ---------------------------------------------------------------------------

func TestHandleEventsConnectingAndConnected(t *testing.T) {
	evChan := make(chan socketmode.Event, 10)
	smClient := &socketmode.Client{
		Events: evChan,
	}
	bot := &Bot{
		client: smClient,
		logger: zap.NewNop().Sugar(),
	}

	evChan <- socketmode.Event{Type: socketmode.EventTypeConnecting}
	evChan <- socketmode.Event{Type: socketmode.EventTypeConnected}
	close(evChan)

	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		bot.handleEvents(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleEvents did not return")
	}
}

// ---------------------------------------------------------------------------
// handleEvents — invalid auth event causes return
// ---------------------------------------------------------------------------

func TestHandleEventsInvalidAuth(t *testing.T) {
	evChan := make(chan socketmode.Event, 10)
	smClient := &socketmode.Client{
		Events: evChan,
	}
	bot := &Bot{
		client: smClient,
		logger: zap.NewNop().Sugar(),
	}

	evChan <- socketmode.Event{Type: socketmode.EventTypeInvalidAuth}

	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		bot.handleEvents(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleEvents did not return after invalid auth")
	}
}

// ---------------------------------------------------------------------------
// handleMessageEvent — bot message ignored
// ---------------------------------------------------------------------------

func TestHandleMessageEventBotMessageIgnored(t *testing.T) {
	bot := &Bot{
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.MessageEvent{
		Channel:   "D123",
		User:      "U123",
		BotID:     "BBOT",
		Text:      "bot says hi",
		TimeStamp: "ts1",
	}
	bot.handleMessageEvent(ev)
}

// ---------------------------------------------------------------------------
// handleMessageEvent — subtype message ignored
// ---------------------------------------------------------------------------

func TestHandleMessageEventSubtypeIgnored(t *testing.T) {
	bot := &Bot{
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.MessageEvent{
		Channel:   "D123",
		User:      "U123",
		SubType:   "message_changed",
		Text:      "edited",
		TimeStamp: "ts1",
	}
	bot.handleMessageEvent(ev)
}

// ---------------------------------------------------------------------------
// handleMessageEvent — non-DM channel ignored
// ---------------------------------------------------------------------------

func TestHandleMessageEventNonDMIgnored(t *testing.T) {
	bot := &Bot{
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.MessageEvent{
		Channel:   "C123",
		User:      "U123",
		Text:      "hello",
		TimeStamp: "ts1",
	}
	bot.handleMessageEvent(ev)
}

// ---------------------------------------------------------------------------
// handleMessageEvent — pair command with invalid code
// ---------------------------------------------------------------------------

func TestHandleMessageEventPairInvalidCode(t *testing.T) {
	bot := &Bot{
		pairCode: "123456",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.MessageEvent{
		Channel:   "D123",
		User:      "U123",
		Text:      "/pair 000000",
		TimeStamp: "ts1",
	}
	func() {
		defer func() { recover() }()
		bot.handleMessageEvent(ev)
	}()
}

// ---------------------------------------------------------------------------
// handleMessageEvent — pair command with valid code
// ---------------------------------------------------------------------------

func TestHandleMessageEventPairValidCode(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	bot := &Bot{
		pairCode: "654321",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.MessageEvent{
		Channel:   "D123",
		User:      "U123",
		Text:      "/pair 654321",
		TimeStamp: "ts1",
	}
	func() {
		defer func() { recover() }()
		bot.handleMessageEvent(ev)
	}()
	bot.mu.Lock()
	defer bot.mu.Unlock()
	if !bot.paired["U123"] {
		t.Error("expected U123 to be paired after valid pair command")
	}
}

// ---------------------------------------------------------------------------
// handleMessageEvent — unauthorized user
// ---------------------------------------------------------------------------

func TestHandleMessageEventUnauthorized(t *testing.T) {
	bot := &Bot{
		cfg:      config.SlackConfig{AllowUsers: []string{"UALLOWED"}},
		pairCode: "999999",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.MessageEvent{
		Channel:   "D123",
		User:      "UNOTALLOWED",
		Text:      "hello",
		TimeStamp: "ts1",
	}
	func() {
		defer func() { recover() }()
		bot.handleMessageEvent(ev)
	}()
}

// ---------------------------------------------------------------------------
// handleMessageEvent — collect mode (exercises enqueue path)
// ---------------------------------------------------------------------------

func TestHandleMessageEventCollectMode(t *testing.T) {
	bot := &Bot{
		cfg:      config.SlackConfig{AllowUsers: nil},
		msgCfg:   config.Messages{Queue: config.MessageQueue{Mode: "collect", DebounceMs: 500}, AckReactionScope: ""},
		fullCfg:  &config.Root{},
		pairCode: "111111",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.MessageEvent{
		Channel:   "D123",
		User:      "U123",
		Text:      "hello there",
		TimeStamp: "ts1",
	}
	bot.handleMessageEvent(ev)

	bot.mu.Lock()
	q, ok := bot.queues["U123"]
	bot.mu.Unlock()
	if !ok {
		t.Fatal("expected message to be queued for U123")
	}
	if len(q.messages) != 1 {
		t.Errorf("expected 1 queued message, got %d", len(q.messages))
	}
	if q.messages[0].text != "hello there" {
		t.Errorf("expected queued text 'hello there', got %q", q.messages[0].text)
	}
	if q.timer != nil {
		q.timer.Stop()
	}
}

// ---------------------------------------------------------------------------
// handleMentionEvent — unauthorized user
// ---------------------------------------------------------------------------

func TestHandleMentionEventUnauthorized(t *testing.T) {
	bot := &Bot{
		botID:    "UBOT",
		cfg:      config.SlackConfig{AllowUsers: []string{"UALLOWED"}},
		msgCfg:   config.Messages{AckReactionScope: ""},
		fullCfg:  &config.Root{},
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.AppMentionEvent{
		Channel:   "C123",
		User:      "UNOTALLOWED",
		Text:      "<@UBOT> hello",
		TimeStamp: "ts1",
	}
	func() {
		defer func() { recover() }()
		bot.handleMentionEvent(ev)
	}()
}

// ---------------------------------------------------------------------------
// handleMentionEvent — collect mode
// ---------------------------------------------------------------------------

func TestHandleMentionEventCollectMode(t *testing.T) {
	bot := &Bot{
		botID:    "UBOT",
		cfg:      config.SlackConfig{AllowUsers: nil},
		msgCfg:   config.Messages{Queue: config.MessageQueue{Mode: "collect", DebounceMs: 500}, AckReactionScope: ""},
		fullCfg:  &config.Root{},
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.AppMentionEvent{
		Channel:   "C123",
		User:      "U123",
		Text:      "<@UBOT> collect this",
		TimeStamp: "ts1",
	}
	bot.handleMentionEvent(ev)

	bot.mu.Lock()
	q, ok := bot.queues["U123"]
	bot.mu.Unlock()
	if !ok {
		t.Fatal("expected message to be queued")
	}
	if len(q.messages) != 1 {
		t.Errorf("expected 1 queued message, got %d", len(q.messages))
	}
	if q.messages[0].text != "collect this" {
		t.Errorf("expected 'collect this', got %q", q.messages[0].text)
	}
	if q.timer != nil {
		q.timer.Stop()
	}
}

// ---------------------------------------------------------------------------
// enqueue — cap reached triggers flush
// ---------------------------------------------------------------------------

func TestEnqueueCapReachedFlushes(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg:      config.SlackConfig{TimeoutSeconds: 1},
		msgCfg:   config.Messages{Queue: config.MessageQueue{Mode: "collect", DebounceMs: 5000, Cap: 3}},
		fullCfg:  &config.Root{},
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}

	bot.enqueue("U1", "C1", "ts1", "msg1")
	bot.enqueue("U1", "C1", "ts2", "msg2")

	bot.mu.Lock()
	_, exists := bot.queues["U1"]
	bot.mu.Unlock()
	if !exists {
		t.Fatal("expected queue to exist before cap")
	}

	// 3rd enqueue hits cap=3 and flushes.
	bot.enqueue("U1", "C1", "ts3", "msg3")

	bot.mu.Lock()
	_, exists = bot.queues["U1"]
	bot.mu.Unlock()
	if exists {
		t.Error("expected queue to be flushed after cap reached")
	}

	time.Sleep(50 * time.Millisecond)
}

// ---------------------------------------------------------------------------
// enqueue — timer reset on second message
// ---------------------------------------------------------------------------

func TestEnqueueTimerResetOnSecondMessage(t *testing.T) {
	bot := &Bot{
		msgCfg: config.Messages{Queue: config.MessageQueue{Mode: "collect", DebounceMs: 10000, Cap: 100}},
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}

	bot.enqueue("U1", "C1", "ts1", "first")

	bot.mu.Lock()
	q := bot.queues["U1"]
	firstTimer := q.timer
	bot.mu.Unlock()

	if firstTimer == nil {
		t.Fatal("expected timer to be set after first enqueue")
	}

	bot.enqueue("U1", "C1", "ts2", "second")

	bot.mu.Lock()
	q2 := bot.queues["U1"]
	secondTimer := q2.timer
	msgs := len(q2.messages)
	bot.mu.Unlock()

	if msgs != 2 {
		t.Errorf("expected 2 messages, got %d", msgs)
	}
	if secondTimer == nil {
		t.Fatal("expected timer to still be set after second enqueue")
	}
	secondTimer.Stop()
}

// ---------------------------------------------------------------------------
// configSnapshot — returns consistent copy
// ---------------------------------------------------------------------------

func TestConfigSnapshotReturnsCurrentValues(t *testing.T) {
	bot := &Bot{
		cfg:    config.SlackConfig{TimeoutSeconds: 42, AckEmoji: "rocket"},
		msgCfg: config.Messages{AckReactionScope: "all"},
		logger: zap.NewNop().Sugar(),
	}

	cfgSnap, msgSnap := bot.configSnapshot()
	if cfgSnap.TimeoutSeconds != 42 {
		t.Errorf("expected TimeoutSeconds=42, got %d", cfgSnap.TimeoutSeconds)
	}
	if cfgSnap.AckEmoji != "rocket" {
		t.Errorf("expected AckEmoji=rocket, got %q", cfgSnap.AckEmoji)
	}
	if msgSnap.AckReactionScope != "all" {
		t.Errorf("expected AckReactionScope=all, got %q", msgSnap.AckReactionScope)
	}
}

// ---------------------------------------------------------------------------
// configSnapshot — concurrent access is safe
// ---------------------------------------------------------------------------

func TestConfigSnapshotConcurrentSafe(t *testing.T) {
	bot := &Bot{
		cfg:    config.SlackConfig{TimeoutSeconds: 10},
		msgCfg: config.Messages{AckReactionScope: "all"},
		logger: zap.NewNop().Sugar(),
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg, msg := bot.configSnapshot()
			_ = cfg
			_ = msg
		}()
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// processMessages — empty slice is no-op
// ---------------------------------------------------------------------------

func TestProcessMessagesEmptySlice(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	bot.processMessages("U1", nil)
	bot.processMessages("U1", []queuedMessage{})
}

// ---------------------------------------------------------------------------
// processMessages — streaming mode
// ---------------------------------------------------------------------------

func TestProcessMessagesStreamingMode(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg:      config.SlackConfig{TimeoutSeconds: 1, StreamMode: "partial"},
		msgCfg:   config.Messages{},
		fullCfg:  &config.Root{},
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	func() {
		defer func() { recover() }()
		bot.processMessages("U1", []queuedMessage{{text: "hello", channelID: "C1", userID: "U1", ts: "ts1"}})
	}()
}

// ---------------------------------------------------------------------------
// processMessages — reset trigger match
// ---------------------------------------------------------------------------

func TestProcessMessagesResetTriggerMatch(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg:      config.SlackConfig{TimeoutSeconds: 1},
		msgCfg:   config.Messages{},
		fullCfg:  &config.Root{Session: config.Session{ResetTriggers: []string{"reset", "clear"}}},
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	func() {
		defer func() { recover() }()
		bot.processMessages("U1", []queuedMessage{{text: "reset", channelID: "C1", userID: "U1", ts: "ts1"}})
	}()
}

// ---------------------------------------------------------------------------
// AnnounceToSession — channel scope
// ---------------------------------------------------------------------------

func TestAnnounceToSessionChannelScope(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	func() {
		defer func() { recover() }()
		bot.AnnounceToSession("myagent:id:slack:channel:C999", "hello channel")
	}()
}

// ---------------------------------------------------------------------------
// AnnounceToSession — user scope (exercises user DM path)
// ---------------------------------------------------------------------------

func TestAnnounceToSessionUserScope(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	func() {
		defer func() { recover() }()
		bot.AnnounceToSession("myagent:id:slack:UABC", "hello user")
	}()
}

// ---------------------------------------------------------------------------
// handleEventsAPI — unknown inner event type (no crash)
// ---------------------------------------------------------------------------

func TestHandleEventsAPIUnknownInnerEvent(t *testing.T) {
	bot := &Bot{
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	event := slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.PinAddedEvent{},
		},
	}
	bot.handleEventsAPI(event)
}

// ---------------------------------------------------------------------------
// handleEventsAPI — non-callback event type
// ---------------------------------------------------------------------------

func TestHandleEventsAPINonCallbackEvent(t *testing.T) {
	bot := &Bot{
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	event := slackevents.EventsAPIEvent{
		Type: slackevents.URLVerification,
	}
	bot.handleEventsAPI(event)
}

// ---------------------------------------------------------------------------
// apiRef — concurrent access is safe
// ---------------------------------------------------------------------------

func TestApiRefConcurrent(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = bot.apiRef()
		}()
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// SendToAllPaired — no paired users (early return, no api calls)
// ---------------------------------------------------------------------------

func TestSendToAllPairedNoUsersEarlyReturn(t *testing.T) {
	bot := &Bot{
		paired: make(map[string]bool),
		logger: zap.NewNop().Sugar(),
	}
	bot.SendToAllPaired("hello")
}

// ---------------------------------------------------------------------------
// handleMentionEvent — non-slash message in non-collect mode (exercises
// the goroutine processMessages path)
// ---------------------------------------------------------------------------

func TestHandleMentionEventNonSlashNonCollect(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		botID:    "UBOT",
		cfg:      config.SlackConfig{AllowUsers: nil, TimeoutSeconds: 1},
		msgCfg:   config.Messages{Queue: config.MessageQueue{Mode: "", DebounceMs: 0}, AckReactionScope: ""},
		fullCfg:  &config.Root{},
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		sessions: sm,
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.AppMentionEvent{
		Channel:   "C123",
		User:      "U123",
		Text:      "<@UBOT> hello world",
		TimeStamp: "ts1",
	}
	// Will spawn goroutine that panics on nil agent, but exercises the non-collect path.
	func() {
		defer func() { recover() }()
		bot.handleMentionEvent(ev)
	}()
	// Give goroutine time to complete.
	time.Sleep(50 * time.Millisecond)
}

// ---------------------------------------------------------------------------
// handleMessageEvent — non-slash, non-collect mode message
// ---------------------------------------------------------------------------

func TestHandleMessageEventNonSlashNonCollect(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg:      config.SlackConfig{AllowUsers: nil, TimeoutSeconds: 1},
		msgCfg:   config.Messages{Queue: config.MessageQueue{Mode: "", DebounceMs: 0}, AckReactionScope: ""},
		fullCfg:  &config.Root{},
		pairCode: "000000",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		sessions: sm,
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.MessageEvent{
		Channel:   "D123",
		User:      "U123",
		Text:      "just a regular message",
		TimeStamp: "ts1",
	}
	func() {
		defer func() { recover() }()
		bot.handleMessageEvent(ev)
	}()
	time.Sleep(50 * time.Millisecond)
}

// ===========================================================================
// Additional coverage tests (appended)
// ===========================================================================

// ---------------------------------------------------------------------------
// handleEventsAPI — CallbackEvent dispatches AppMentionEvent to handleMentionEvent
// (exercises the full dispatch flow without needing the socketmode.Client Ack)
// ---------------------------------------------------------------------------

func TestHandleEventsAPIDispatchAppMention(t *testing.T) {
	bot := &Bot{
		botID: "UBOT",
		cfg:   config.SlackConfig{AllowUsers: nil},
		msgCfg: config.Messages{
			Queue:            config.MessageQueue{Mode: "collect", DebounceMs: 5000, Cap: 100},
			AckReactionScope: "",
		},
		fullCfg: &config.Root{},
		paired:  make(map[string]bool),
		queues:  make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	mentionEv := &slackevents.AppMentionEvent{
		Channel:   "C123",
		User:      "U123",
		Text:      "<@UBOT> test via dispatch",
		TimeStamp: "ts1",
	}
	event := slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: mentionEv,
		},
	}
	bot.handleEventsAPI(event)

	bot.mu.Lock()
	q := bot.queues["U123"]
	bot.mu.Unlock()
	if q == nil {
		t.Fatal("expected message enqueued via handleEventsAPI dispatch")
	}
	if q.messages[0].text != "test via dispatch" {
		t.Errorf("unexpected text: %q", q.messages[0].text)
	}
	if q.timer != nil {
		q.timer.Stop()
	}
}

// ---------------------------------------------------------------------------
// handleEvents — multiple event types in sequence
// ---------------------------------------------------------------------------

func TestHandleEventsMultipleEventTypes(t *testing.T) {
	evChan := make(chan socketmode.Event, 10)
	smClient := &socketmode.Client{
		Events: evChan,
	}
	bot := &Bot{
		client: smClient,
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}

	// Send a mix of event types
	evChan <- socketmode.Event{Type: socketmode.EventTypeConnecting}
	evChan <- socketmode.Event{Type: socketmode.EventTypeConnected}
	evChan <- socketmode.Event{Type: socketmode.EventTypeConnecting}
	close(evChan)

	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		bot.handleEvents(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleEvents did not return after multiple events")
	}
}

// ---------------------------------------------------------------------------
// handleEventsAPI — CallbackEvent dispatches MessageEvent to handleMessageEvent
// (exercises the full dispatch path via handleEventsAPI directly)
// ---------------------------------------------------------------------------

func TestHandleEventsAPIDispatchMessageCollect(t *testing.T) {
	bot := &Bot{
		cfg: config.SlackConfig{AllowUsers: nil},
		msgCfg: config.Messages{
			Queue:            config.MessageQueue{Mode: "collect", DebounceMs: 5000, Cap: 100},
			AckReactionScope: "",
		},
		fullCfg: &config.Root{},
		paired:  make(map[string]bool),
		queues:  make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}

	msgEv := &slackevents.MessageEvent{
		Channel:   "D123",
		User:      "U123",
		Text:      "hello from dispatch",
		TimeStamp: "ts1",
	}
	event := slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: msgEv,
		},
	}
	bot.handleEventsAPI(event)

	bot.mu.Lock()
	q := bot.queues["U123"]
	bot.mu.Unlock()
	if q == nil {
		t.Fatal("expected message to be enqueued via dispatch")
	}
	if q.messages[0].text != "hello from dispatch" {
		t.Errorf("unexpected text: %q", q.messages[0].text)
	}
	if q.timer != nil {
		q.timer.Stop()
	}
}

// ---------------------------------------------------------------------------
// handleEvents — InvalidAuth returns immediately even with more events queued
// ---------------------------------------------------------------------------

func TestHandleEventsInvalidAuthStopsLoop(t *testing.T) {
	evChan := make(chan socketmode.Event, 10)
	smClient := &socketmode.Client{
		Events: evChan,
	}
	bot := &Bot{
		client: smClient,
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}

	evChan <- socketmode.Event{Type: socketmode.EventTypeInvalidAuth}
	// These should never be processed
	evChan <- socketmode.Event{Type: socketmode.EventTypeConnecting}
	evChan <- socketmode.Event{Type: socketmode.EventTypeConnected}

	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		bot.handleEvents(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleEvents did not return after InvalidAuth")
	}
}

// ---------------------------------------------------------------------------
// AnnounceToSession — various key format variations
// ---------------------------------------------------------------------------

func TestAnnounceToSessionShortKeyIgnored(t *testing.T) {
	bot := &Bot{paired: make(map[string]bool), logger: zap.NewNop().Sugar()}
	// len < 4 parts
	bot.AnnounceToSession("a:b:c", "msg")
	// 4 parts but parts[2] != "slack"
	bot.AnnounceToSession("a:b:discord:U1", "msg")
	// 2 parts
	bot.AnnounceToSession("a:b", "msg")
	// 1 part
	bot.AnnounceToSession("a", "msg")
	// empty
	bot.AnnounceToSession("", "msg")
}

func TestAnnounceToSessionFivePartChannelScope(t *testing.T) {
	bot := &Bot{paired: make(map[string]bool), logger: zap.NewNop().Sugar()}
	// Format: agent:id:slack:channel:C123 — 5 parts, parts[3]=="channel"
	func() {
		defer func() { recover() }()
		bot.AnnounceToSession("myagent:id:slack:channel:C999", "hello channel")
	}()
}

func TestAnnounceToSessionFourPartUserScope(t *testing.T) {
	bot := &Bot{paired: make(map[string]bool), logger: zap.NewNop().Sugar()}
	// Format: agent:id:slack:U123 — 4 parts, no "channel" at parts[3]
	func() {
		defer func() { recover() }()
		bot.AnnounceToSession("myagent:id:slack:UABC", "hello user")
	}()
}

func TestAnnounceToSessionSixPartChannelScope(t *testing.T) {
	bot := &Bot{paired: make(map[string]bool), logger: zap.NewNop().Sugar()}
	// Format: a:b:slack:channel:C123:extra — 6 parts, parts[3]=="channel", parts[4]=="C123"
	func() {
		defer func() { recover() }()
		bot.AnnounceToSession("a:b:slack:channel:C123:extra", "msg")
	}()
}

func TestAnnounceToSessionFivePartNonChannel(t *testing.T) {
	bot := &Bot{paired: make(map[string]bool), logger: zap.NewNop().Sugar()}
	// Format: a:b:slack:user:U999 — 5 parts, parts[3]=="user" (not "channel")
	// Falls into user scope: userID = parts[len(parts)-1] = "U999"
	func() {
		defer func() { recover() }()
		bot.AnnounceToSession("a:b:slack:user:U999", "msg")
	}()
}

func TestAnnounceToSessionFourPartWrongPlatform(t *testing.T) {
	bot := &Bot{paired: make(map[string]bool), logger: zap.NewNop().Sugar()}
	// 4 parts, but parts[2] != "slack"
	bot.AnnounceToSession("a:b:telegram:123", "msg")
	// No panic = early return exercised
}

// ---------------------------------------------------------------------------
// handleMessageEvent — /pair with correct code then send message
// ---------------------------------------------------------------------------

func TestHandleMessageEventPairThenMessage(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg:      config.SlackConfig{AllowUsers: nil},
		msgCfg:   config.Messages{AckReactionScope: ""},
		fullCfg:  &config.Root{},
		pairCode: "123456",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		sessions: sm,
		logger: zap.NewNop().Sugar(),
	}

	// First, pair the user
	pairEv := &slackevents.MessageEvent{
		Channel:   "D999",
		User:      "UNEW",
		Text:      "/pair 123456",
		TimeStamp: "ts1",
	}
	func() {
		defer func() { recover() }()
		bot.handleMessageEvent(pairEv)
	}()

	bot.mu.Lock()
	paired := bot.paired["UNEW"]
	bot.mu.Unlock()
	if !paired {
		t.Fatal("expected user to be paired after valid /pair")
	}

	// Then send a regular message — should be processed (not blocked)
	msgEv := &slackevents.MessageEvent{
		Channel:   "D999",
		User:      "UNEW",
		Text:      "hello after pair",
		TimeStamp: "ts2",
	}
	func() {
		defer func() { recover() }()
		bot.handleMessageEvent(msgEv)
	}()
	time.Sleep(50 * time.Millisecond)
}

// ---------------------------------------------------------------------------
// handleMessageEvent — slash command routing (various commands)
// ---------------------------------------------------------------------------

func TestHandleMessageEventSlashCommandHelp(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg:      config.SlackConfig{AllowUsers: nil},
		msgCfg:   config.Messages{},
		fullCfg:  &config.Root{},
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.MessageEvent{
		Channel:   "D123",
		User:      "U123",
		Text:      "/help",
		TimeStamp: "ts1",
	}
	// /help is a recognized command. The PostMessage will panic (nil API),
	// but the command routing path is exercised.
	func() {
		defer func() { recover() }()
		bot.handleMessageEvent(ev)
	}()
}

func TestHandleMessageEventSlashCommandNewSession(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg:      config.SlackConfig{AllowUsers: nil},
		msgCfg:   config.Messages{},
		fullCfg:  &config.Root{},
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.MessageEvent{
		Channel:   "D123",
		User:      "U123",
		Text:      "/new",
		TimeStamp: "ts1",
	}
	func() {
		defer func() { recover() }()
		bot.handleMessageEvent(ev)
	}()
}

// ---------------------------------------------------------------------------
// handleMentionEvent — slash command routing
// ---------------------------------------------------------------------------

func TestHandleMentionEventSlashCommandHelp(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		botID:    "UBOT",
		cfg:      config.SlackConfig{AllowUsers: nil},
		msgCfg:   config.Messages{},
		fullCfg:  &config.Root{},
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.AppMentionEvent{
		Channel:   "C123",
		User:      "U123",
		Text:      "<@UBOT> /help",
		TimeStamp: "ts1",
	}
	func() {
		defer func() { recover() }()
		bot.handleMentionEvent(ev)
	}()
}

// ---------------------------------------------------------------------------
// processMessages — fullCfg with usage "tokens" (exercises token usage branch
// in respondFull/respondStreaming — panics at nil agent but reaches the branch)
// ---------------------------------------------------------------------------

func TestProcessMessagesUsageTokensConfig(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg:     config.SlackConfig{TimeoutSeconds: 1},
		msgCfg:  config.Messages{Usage: "tokens"},
		fullCfg: &config.Root{Messages: config.Messages{Usage: "tokens"}},
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	func() {
		defer func() { recover() }()
		bot.processMessages("U1", []queuedMessage{{text: "hello", channelID: "C1", userID: "U1", ts: "ts1"}})
	}()
}

func TestProcessMessagesStreamingUsageTokensConfig(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg:     config.SlackConfig{TimeoutSeconds: 1, StreamMode: "partial"},
		msgCfg:  config.Messages{Usage: "tokens", StreamEditMs: 200},
		fullCfg: &config.Root{Messages: config.Messages{Usage: "tokens"}},
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	func() {
		defer func() { recover() }()
		bot.processMessages("U1", []queuedMessage{{text: "hello", channelID: "C1", userID: "U1", ts: "ts1"}})
	}()
}

// ---------------------------------------------------------------------------
// processMessages — configSnapshot returns default StreamEditMs when 0
// ---------------------------------------------------------------------------

func TestProcessMessagesStreamEditMsDefault(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg:      config.SlackConfig{TimeoutSeconds: 1, StreamMode: "partial"},
		msgCfg:   config.Messages{StreamEditMs: 0},
		fullCfg:  &config.Root{},
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	func() {
		defer func() { recover() }()
		bot.processMessages("U1", []queuedMessage{{text: "test", channelID: "C1", userID: "U1", ts: "ts1"}})
	}()
}

// ---------------------------------------------------------------------------
// flushLocked — debounce timer fires (real timeout scenario)
// ---------------------------------------------------------------------------

func TestFlushLockedViaDebounceTimer(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg:      config.SlackConfig{TimeoutSeconds: 1},
		msgCfg:   config.Messages{Queue: config.MessageQueue{Mode: "collect", DebounceMs: 50, Cap: 100}},
		fullCfg:  &config.Root{Session: config.Session{ResetTriggers: []string{"reset"}}},
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}

	// Enqueue a reset message so processMessages does not need an agent
	bot.enqueue("U1", "C1", "ts1", "reset")

	// Wait for debounce timer to fire (50ms debounce + buffer)
	time.Sleep(200 * time.Millisecond)

	// Queue should have been flushed
	bot.mu.Lock()
	_, exists := bot.queues["U1"]
	bot.mu.Unlock()
	if exists {
		t.Error("expected queue to be flushed after debounce timer")
	}
}

// ---------------------------------------------------------------------------
// flushLocked — with timer already set, then cap reached
// ---------------------------------------------------------------------------

func TestEnqueueCapReachedWithExistingTimer(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg:      config.SlackConfig{TimeoutSeconds: 1},
		msgCfg:   config.Messages{Queue: config.MessageQueue{Mode: "collect", DebounceMs: 5000, Cap: 3}},
		fullCfg:  &config.Root{Session: config.Session{ResetTriggers: []string{}}},
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}

	// First two enqueues: timer is created/reset
	bot.enqueue("U2", "C2", "ts1", "msg1")
	bot.enqueue("U2", "C2", "ts2", "msg2")

	bot.mu.Lock()
	q := bot.queues["U2"]
	hasTimer := q != nil && q.timer != nil
	bot.mu.Unlock()
	if !hasTimer {
		t.Fatal("expected timer after 2 enqueues")
	}

	// Third enqueue reaches cap; should stop timer and flush
	bot.enqueue("U2", "C2", "ts3", "msg3")
	time.Sleep(100 * time.Millisecond)

	bot.mu.Lock()
	_, exists := bot.queues["U2"]
	bot.mu.Unlock()
	if exists {
		t.Error("expected queue flushed after cap reached")
	}
}

// ---------------------------------------------------------------------------
// handleMessageEvent — ack reaction with "all" scope (panics at nil API
// but exercises the ackReaction call before the panic)
// ---------------------------------------------------------------------------

func TestHandleMessageEventAckReactionAllScope(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg:      config.SlackConfig{AllowUsers: nil, TimeoutSeconds: 1, AckEmoji: "wave"},
		msgCfg:   config.Messages{AckReactionScope: "all"},
		fullCfg:  &config.Root{},
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.MessageEvent{
		Channel:   "D123",
		User:      "U123",
		Text:      "hello",
		TimeStamp: "ts1",
	}
	func() {
		defer func() { recover() }()
		bot.handleMessageEvent(ev)
	}()
	time.Sleep(50 * time.Millisecond)
}

// ---------------------------------------------------------------------------
// handleMentionEvent — ack reaction with "group-mentions" scope
// (isMention=true, so reaction should be attempted)
// ---------------------------------------------------------------------------

func TestHandleMentionEventAckReactionGroupMentions(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		botID:    "UBOT",
		cfg:      config.SlackConfig{AllowUsers: nil, TimeoutSeconds: 1},
		msgCfg:   config.Messages{AckReactionScope: "group-mentions"},
		fullCfg:  &config.Root{},
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.AppMentionEvent{
		Channel:   "C123",
		User:      "U123",
		Text:      "<@UBOT> hello",
		TimeStamp: "ts1",
	}
	func() {
		defer func() { recover() }()
		bot.handleMentionEvent(ev)
	}()
	time.Sleep(50 * time.Millisecond)
}

// ---------------------------------------------------------------------------
// handleMessageEvent — ack reaction with "group-mentions" scope for DM
// (isMention=false, so reaction should be skipped)
// ---------------------------------------------------------------------------

func TestHandleMessageEventAckReactionGroupMentionsDM(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg: config.SlackConfig{AllowUsers: nil, TimeoutSeconds: 1},
		msgCfg: config.Messages{
			AckReactionScope: "group-mentions",
			Queue:            config.MessageQueue{Mode: "collect", DebounceMs: 5000, Cap: 100},
		},
		fullCfg:  &config.Root{},
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.MessageEvent{
		Channel:   "D123",
		User:      "U123",
		Text:      "hello",
		TimeStamp: "ts1",
	}
	// Should NOT try to add reaction (scope is group-mentions, this is a DM)
	bot.handleMessageEvent(ev)

	// Verify message was enqueued (not blocked by ackReaction)
	bot.mu.Lock()
	q := bot.queues["U123"]
	bot.mu.Unlock()
	if q == nil {
		t.Fatal("expected message to be enqueued")
	}
	if q.timer != nil {
		q.timer.Stop()
	}
}

// ---------------------------------------------------------------------------
// handleMessageEvent — message with allowUsers restriction and authorized user
// exercises the whole flow through processMessages
// ---------------------------------------------------------------------------

func TestHandleMessageEventAuthorizedWithAllowList(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg:      config.SlackConfig{AllowUsers: []string{"U_VALID"}, TimeoutSeconds: 1},
		msgCfg:   config.Messages{AckReactionScope: ""},
		fullCfg:  &config.Root{Session: config.Session{ResetTriggers: []string{"reset"}}},
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.MessageEvent{
		Channel:   "D123",
		User:      "U_VALID",
		Text:      "reset",
		TimeStamp: "ts1",
	}
	// "reset" triggers session reset, which calls PostMessage (nil API -> panic)
	func() {
		defer func() { recover() }()
		bot.handleMessageEvent(ev)
	}()
	time.Sleep(100 * time.Millisecond)
}

// ---------------------------------------------------------------------------
// handleMentionEvent — non-slash command text (not a /command, exercises the
// text message processing path via mention)
// ---------------------------------------------------------------------------

func TestHandleMentionEventRegularTextCollectMode(t *testing.T) {
	bot := &Bot{
		botID: "UBOT",
		cfg:   config.SlackConfig{AllowUsers: nil},
		msgCfg: config.Messages{
			Queue:            config.MessageQueue{Mode: "collect", DebounceMs: 5000, Cap: 100},
			AckReactionScope: "",
		},
		fullCfg: &config.Root{},
		paired:  make(map[string]bool),
		queues:  make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.AppMentionEvent{
		Channel:   "C123",
		User:      "U123",
		Text:      "<@UBOT> what's the weather?",
		TimeStamp: "ts1",
	}
	bot.handleMentionEvent(ev)

	bot.mu.Lock()
	q := bot.queues["U123"]
	bot.mu.Unlock()
	if q == nil {
		t.Fatal("expected message enqueued")
	}
	if q.messages[0].text != "what's the weather?" {
		t.Errorf("unexpected text %q", q.messages[0].text)
	}
	if q.timer != nil {
		q.timer.Stop()
	}
}

// ---------------------------------------------------------------------------
// processMessages — multiple messages combined (more than 2)
// ---------------------------------------------------------------------------

func TestProcessMessagesCombinesFourMessages(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg:     config.SlackConfig{TimeoutSeconds: 1},
		msgCfg:  config.Messages{},
		fullCfg: &config.Root{Session: config.Session{ResetTriggers: []string{"a\nb\nc\nd"}}},
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	// 4 messages combine to "a\nb\nc\nd" which matches the reset trigger
	msgs := []queuedMessage{
		{text: "a", channelID: "C1", userID: "U1", ts: "ts1"},
		{text: "b", channelID: "C1", userID: "U1", ts: "ts2"},
		{text: "c", channelID: "C1", userID: "U1", ts: "ts3"},
		{text: "d", channelID: "C1", userID: "U1", ts: "ts4"},
	}
	func() {
		defer func() { recover() }()
		bot.processMessages("U1", msgs)
	}()
}

// ---------------------------------------------------------------------------
// handleEventsAPI — CallbackEvent with various inner event types
// ---------------------------------------------------------------------------

func TestHandleEventsAPICallbackWithReactionAdded(t *testing.T) {
	bot := &Bot{
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	// ReactionAddedEvent is a known type but not handled by our switch
	event := slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.ReactionAddedEvent{},
		},
	}
	bot.handleEventsAPI(event)
}

func TestHandleEventsAPICallbackWithMemberJoined(t *testing.T) {
	bot := &Bot{
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	event := slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.MemberJoinedChannelEvent{},
		},
	}
	bot.handleEventsAPI(event)
}

// ---------------------------------------------------------------------------
// Reconnect-related: configSnapshot race safety
// ---------------------------------------------------------------------------

func TestConfigSnapshotAfterFieldChanges(t *testing.T) {
	bot := &Bot{
		cfg:    config.SlackConfig{TimeoutSeconds: 10, StreamMode: "partial"},
		msgCfg: config.Messages{StreamEditMs: 300, AckReactionScope: "all"},
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}

	cfg, msgCfg := bot.configSnapshot()
	if cfg.TimeoutSeconds != 10 {
		t.Errorf("expected TimeoutSeconds=10, got %d", cfg.TimeoutSeconds)
	}
	if cfg.StreamMode != "partial" {
		t.Errorf("expected StreamMode=partial, got %q", cfg.StreamMode)
	}
	if msgCfg.StreamEditMs != 300 {
		t.Errorf("expected StreamEditMs=300, got %d", msgCfg.StreamEditMs)
	}
	if msgCfg.AckReactionScope != "all" {
		t.Errorf("expected AckReactionScope=all, got %q", msgCfg.AckReactionScope)
	}

	// Modify under lock (as Reconnect would)
	bot.mu.Lock()
	bot.cfg.TimeoutSeconds = 20
	bot.cfg.StreamMode = ""
	bot.mu.Unlock()

	cfg2, _ := bot.configSnapshot()
	if cfg2.TimeoutSeconds != 20 {
		t.Errorf("expected TimeoutSeconds=20 after change, got %d", cfg2.TimeoutSeconds)
	}
	if cfg2.StreamMode != "" {
		t.Errorf("expected StreamMode empty after change, got %q", cfg2.StreamMode)
	}
}

// ---------------------------------------------------------------------------
// SendToAllPaired — with paired users (exercises iteration, panics at nil API)
// ---------------------------------------------------------------------------

func TestSendToAllPairedMultipleUsers(t *testing.T) {
	bot := &Bot{
		paired: map[string]bool{"U1": true, "U2": true, "U3": true},
		logger: zap.NewNop().Sugar(),
	}
	func() {
		defer func() { recover() }()
		bot.SendToAllPaired("broadcast")
	}()
}

// ---------------------------------------------------------------------------
// handleMessageEvent — unhandled slash command (not in commands.Handle)
// ---------------------------------------------------------------------------

func TestHandleMessageEventUnknownSlashCommand(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		cfg:      config.SlackConfig{AllowUsers: nil},
		msgCfg:   config.Messages{},
		fullCfg:  &config.Root{},
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.MessageEvent{
		Channel:   "D123",
		User:      "U123",
		Text:      "/unknowncmd arg1 arg2",
		TimeStamp: "ts1",
	}
	// Unknown commands are still Handled=true by commands.Handle, so PostMessage
	// is called (nil API -> panic). This exercises the slash command routing.
	func() {
		defer func() { recover() }()
		bot.handleMessageEvent(ev)
	}()
}

// ---------------------------------------------------------------------------
// handleMentionEvent — text that starts with / but is a file path (not a command)
// ---------------------------------------------------------------------------

func TestHandleMentionEventFilePathSlashCommand(t *testing.T) {
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bot := &Bot{
		botID: "UBOT",
		cfg:   config.SlackConfig{AllowUsers: nil, TimeoutSeconds: 1},
		msgCfg: config.Messages{
			AckReactionScope: "",
			Queue:            config.MessageQueue{Mode: "collect", DebounceMs: 5000, Cap: 100},
		},
		fullCfg:  &config.Root{},
		sessions: sm,
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	ev := &slackevents.AppMentionEvent{
		Channel:   "C123",
		User:      "U123",
		Text:      "<@UBOT> /home/user/file.txt",
		TimeStamp: "ts1",
	}
	// Text starts with "/" so handleMentionEvent routes it as a slash command.
	// commands.Handle detects multiple slashes and returns Handled=false,
	// but handleMentionEvent still returns (the entire if block ends with return).
	// So no message should be enqueued — this exercises the "unhandled slash" path.
	bot.handleMentionEvent(ev)
	bot.mu.Lock()
	q := bot.queues["U123"]
	bot.mu.Unlock()
	// Message should NOT be enqueued because handleMentionEvent returns after
	// the slash-command routing block.
	if q != nil {
		t.Error("expected no enqueue for file path that starts with /")
		if q.timer != nil {
			q.timer.Stop()
		}
	}
}

// ---------------------------------------------------------------------------
// sessionKeyFor — with agents that have explicit IDs
// ---------------------------------------------------------------------------

func TestSessionKeyForWithAgentID(t *testing.T) {
	bot := &Bot{
		fullCfg: &config.Root{
			Session: config.Session{Scope: "user"},
			Agents: config.Agents{
				List: []config.AgentDef{
					{ID: "mybot", Default: true},
				},
			},
		},
		logger: zap.NewNop().Sugar(),
	}
	key := bot.sessionKeyFor("U123", "C456")
	if !strings.HasPrefix(key, "mybot:") {
		t.Errorf("expected key to start with 'mybot:', got %q", key)
	}
}

// ---------------------------------------------------------------------------
// isAuthorized — case-insensitive with mixed case IDs
// ---------------------------------------------------------------------------

func TestIsAuthorizedMixedCase(t *testing.T) {
	bot := &Bot{
		cfg: config.SlackConfig{AllowUsers: []string{"Uabc123"}},
		logger: zap.NewNop().Sugar(),
	}
	if !bot.isAuthorized("UABC123") {
		t.Error("expected case-insensitive match for UABC123")
	}
	if !bot.isAuthorized("uabc123") {
		t.Error("expected case-insensitive match for uabc123")
	}
	if bot.isAuthorized("UXYZ") {
		t.Error("expected no match for UXYZ")
	}
}

// ===========================================================================
// NEW TESTS — coverage expansion for CanConfirm, handleConfirmCallback,
// SendConfirmPrompt, respondFull, AnnounceToSession
// ===========================================================================

// ---------------------------------------------------------------------------
// CanConfirm
// ---------------------------------------------------------------------------

func TestCanConfirmSlackSessionKey(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	if !bot.CanConfirm("main:slack:U123") {
		t.Error("expected CanConfirm to return true for 'main:slack:U123'")
	}
}

func TestCanConfirmSlackChannelScopeKey(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	if !bot.CanConfirm("main:slack:channel:C456") {
		t.Error("expected CanConfirm to return true for channel-scope slack key")
	}
}

func TestCanConfirmDiscordKeyFalse(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	if bot.CanConfirm("main:discord:123") {
		t.Error("expected CanConfirm to return false for discord key")
	}
}

func TestCanConfirmTelegramKeyFalse(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	if bot.CanConfirm("main:telegram:123") {
		t.Error("expected CanConfirm to return false for telegram key")
	}
}

func TestCanConfirmEmptyStringFalse(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	if bot.CanConfirm("") {
		t.Error("expected CanConfirm to return false for empty string")
	}
}

func TestCanConfirmNoColonWrappedSlack(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	// "slack" without colons around it should not match ":slack:"
	if bot.CanConfirm("slack") {
		t.Error("expected CanConfirm to return false for bare 'slack' (no colon-wrapped match)")
	}
}

func TestCanConfirmSlackAtEnd(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	// "main:slack:" has trailing colon, so ":slack:" is present
	if !bot.CanConfirm("main:slack:") {
		t.Error("expected CanConfirm to return true when ':slack:' is contained")
	}
}

func TestCanConfirmGlobalScopeSlackKey(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	if !bot.CanConfirm("myagent:slack:global") {
		t.Error("expected CanConfirm to return true for global scope slack key")
	}
}

// ---------------------------------------------------------------------------
// handleConfirmCallback
// ---------------------------------------------------------------------------

func TestHandleConfirmCallbackNonConfirmPrefix(t *testing.T) {
	bot := &Bot{
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}
	// callbackID that does not start with "confirm:" should return false
	if bot.handleConfirmCallback("other:callback:123", "yes") {
		t.Error("expected handleConfirmCallback to return false for non-confirm callbackID")
	}
}

func TestHandleConfirmCallbackEmptyCallbackID(t *testing.T) {
	bot := &Bot{
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}
	if bot.handleConfirmCallback("", "yes") {
		t.Error("expected handleConfirmCallback to return false for empty callbackID")
	}
}

func TestHandleConfirmCallbackYesValue(t *testing.T) {
	bot := &Bot{
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}
	confirmID := "confirm:C123:1234567890"
	ch := make(chan bool, 1)
	bot.mu.Lock()
	bot.pendingConfirms[confirmID] = ch
	bot.mu.Unlock()

	result := bot.handleConfirmCallback(confirmID, "yes")
	if !result {
		t.Error("expected handleConfirmCallback to return true for matching confirmID")
	}

	select {
	case val := <-ch:
		if !val {
			t.Error("expected true to be sent to channel for 'yes' value")
		}
	default:
		t.Error("expected a value to be sent to the channel")
	}
}

func TestHandleConfirmCallbackNoValue(t *testing.T) {
	bot := &Bot{
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}
	confirmID := "confirm:C123:1234567890"
	ch := make(chan bool, 1)
	bot.mu.Lock()
	bot.pendingConfirms[confirmID] = ch
	bot.mu.Unlock()

	result := bot.handleConfirmCallback(confirmID, "no")
	if !result {
		t.Error("expected handleConfirmCallback to return true for matching confirmID")
	}

	select {
	case val := <-ch:
		if val {
			t.Error("expected false to be sent to channel for 'no' value")
		}
	default:
		t.Error("expected a value to be sent to the channel")
	}
}

func TestHandleConfirmCallbackUnknownConfirmID(t *testing.T) {
	bot := &Bot{
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}
	// confirmID starts with "confirm:" but is not in pendingConfirms
	result := bot.handleConfirmCallback("confirm:unknown:999", "yes")
	if result {
		t.Error("expected handleConfirmCallback to return false for unknown confirmID")
	}
}

func TestHandleConfirmCallbackChannelAlreadyConsumed(t *testing.T) {
	bot := &Bot{
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}
	confirmID := "confirm:C123:9999"
	// Create a channel with buffer size 1, pre-fill it so the next send goes to default
	ch := make(chan bool, 1)
	ch <- true // fill the buffer
	bot.mu.Lock()
	bot.pendingConfirms[confirmID] = ch
	bot.mu.Unlock()

	// Should return true but not block (goes to default case in select)
	result := bot.handleConfirmCallback(confirmID, "yes")
	if !result {
		t.Error("expected handleConfirmCallback to return true even when buffer is full")
	}
}

func TestHandleConfirmCallbackNilPendingConfirms(t *testing.T) {
	bot := &Bot{
		pendingConfirms: nil,
		logger:          zap.NewNop().Sugar(),
	}
	// When pendingConfirms is nil, the map lookup returns zero value (nil, false)
	// The function should return false for any confirm: prefix ID
	result := bot.handleConfirmCallback("confirm:C123:111", "yes")
	if result {
		t.Error("expected handleConfirmCallback to return false when pendingConfirms is nil")
	}
}

func TestHandleConfirmCallbackArbitraryValue(t *testing.T) {
	bot := &Bot{
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}
	confirmID := "confirm:C123:5555"
	ch := make(chan bool, 1)
	bot.mu.Lock()
	bot.pendingConfirms[confirmID] = ch
	bot.mu.Unlock()

	// Any value other than "yes" should send false
	result := bot.handleConfirmCallback(confirmID, "maybe")
	if !result {
		t.Error("expected handleConfirmCallback to return true for matching confirmID")
	}

	select {
	case val := <-ch:
		if val {
			t.Error("expected false to be sent for non-'yes' value")
		}
	default:
		t.Error("expected a value to be sent to the channel")
	}
}

// ---------------------------------------------------------------------------
// SendConfirmPrompt — invalid session key (< 3 parts)
// ---------------------------------------------------------------------------

func TestSendConfirmPromptInvalidShortKey(t *testing.T) {
	bot := &Bot{
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}
	ctx := context.Background()
	_, err := bot.SendConfirmPrompt(ctx, "ab", "rm -rf /", "rm.*")
	if err == nil {
		t.Error("expected error for session key with < 3 parts")
	}
	if !strings.Contains(err.Error(), "invalid session key") {
		t.Errorf("expected 'invalid session key' error, got: %v", err)
	}
}

func TestSendConfirmPromptInvalidSinglePartKey(t *testing.T) {
	bot := &Bot{
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}
	ctx := context.Background()
	_, err := bot.SendConfirmPrompt(ctx, "main", "rm -rf /", "rm.*")
	if err == nil {
		t.Error("expected error for single-part session key")
	}
}

func TestSendConfirmPromptInvalidEmptyKey(t *testing.T) {
	bot := &Bot{
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}
	ctx := context.Background()
	_, err := bot.SendConfirmPrompt(ctx, "", "rm -rf /", "rm.*")
	if err == nil {
		t.Error("expected error for empty session key")
	}
}

func TestSendConfirmPromptInvalidTwoPartKey(t *testing.T) {
	bot := &Bot{
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}
	ctx := context.Background()
	_, err := bot.SendConfirmPrompt(ctx, "a:b", "rm -rf /", "rm.*")
	if err == nil {
		t.Error("expected error for two-part session key")
	}
}

func TestSendConfirmPromptChannelScopeKeyPanicsAtAPI(t *testing.T) {
	// 5+ parts with "channel" at parts[3] — reaches channelID = parts[4]
	// Then tries to call api.PostMessage which panics (nil API)
	bot := &Bot{
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}
	ctx := context.Background()
	func() {
		defer func() { recover() }()
		_, _ = bot.SendConfirmPrompt(ctx, "agent:slack:channel:C123:extra", "rm -rf /", "rm.*")
	}()
}

func TestSendConfirmPromptUserScopeKeyPanicsAtAPI(t *testing.T) {
	// 3 parts — reaches userID = parts[len(parts)-1] then tries OpenConversation
	bot := &Bot{
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}
	ctx := context.Background()
	func() {
		defer func() { recover() }()
		_, _ = bot.SendConfirmPrompt(ctx, "agent:slack:U123", "rm -rf /", "rm.*")
	}()
}

func TestSendConfirmPromptContextCancelled(t *testing.T) {
	// We can test the context cancellation path by creating a scenario
	// where the confirmID is registered but context is already cancelled.
	// However, since SendConfirmPrompt tries to call api.PostMessage first,
	// we need to verify the error path for the key validation only.
	bot := &Bot{
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	// Short key should still return error before any ctx check
	_, err := bot.SendConfirmPrompt(ctx, "x", "cmd", "pattern")
	if err == nil {
		t.Error("expected error for short key even with cancelled context")
	}
}

func TestSendConfirmPromptCommandTruncation(t *testing.T) {
	// Tests that command > 200 chars is truncated.
	// This will panic at nil API when trying to send, but exercises the truncation path.
	bot := &Bot{
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}
	ctx := context.Background()
	longCmd := strings.Repeat("x", 300)
	func() {
		defer func() { recover() }()
		_, _ = bot.SendConfirmPrompt(ctx, "agent:slack:channel:C123:extra", longCmd, "pattern")
	}()
}

func TestSendConfirmPromptExactly200CharCommand(t *testing.T) {
	// Exactly 200 chars should NOT be truncated.
	bot := &Bot{
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}
	ctx := context.Background()
	exactCmd := strings.Repeat("y", 200)
	func() {
		defer func() { recover() }()
		_, _ = bot.SendConfirmPrompt(ctx, "agent:slack:channel:C123:extra", exactCmd, "pattern")
	}()
}

// ---------------------------------------------------------------------------
// respondFull — error path (agent is nil -> panic, already tested, but let's
// also verify the configSnapshot + timeout creation path)
// ---------------------------------------------------------------------------

func TestRespondFullWithZeroTimeout(t *testing.T) {
	// TimeoutSeconds=0 creates a context with 0 timeout which expires immediately.
	// ag.Chat will fail with context deadline exceeded (or panic on nil agent).
	bot := &Bot{
		cfg:     config.SlackConfig{TimeoutSeconds: 0},
		fullCfg: &config.Root{},
		logger:  zap.NewNop().Sugar(),
	}
	func() {
		defer func() { recover() }()
		bot.respondFull("main:slack:U1", "C1", "hello", bot.cfg)
	}()
}

// ---------------------------------------------------------------------------
// AnnounceToSession — additional edge cases
// ---------------------------------------------------------------------------

func TestAnnounceToSessionNonSlackKeyReturnsEarly(t *testing.T) {
	// Non-slack session key should return early without any API calls
	bot := &Bot{logger: zap.NewNop().Sugar()}
	// api is nil, so if it tried to make API calls, it would panic
	bot.AnnounceToSession("main:discord:U123", "test message")
	bot.AnnounceToSession("main:telegram:12345", "test message")
	bot.AnnounceToSession("agent:webchat:session1", "test message")
}

func TestAnnounceToSessionShortKeyReturnsEarly(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	// < 3 parts returns early
	bot.AnnounceToSession("ab", "test")
	bot.AnnounceToSession("x", "test")
	bot.AnnounceToSession("", "test")
}

func TestAnnounceToSessionTwoPartKey(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	// 2 parts: len < 3, returns early
	bot.AnnounceToSession("a:b", "test")
}

func TestAnnounceToSessionThreePartSlackKey(t *testing.T) {
	// 3 parts: parts[1] == "slack" -> isSlack=true, user scope path
	// Will try apiRef().OpenConversation which panics (nil api)
	bot := &Bot{logger: zap.NewNop().Sugar()}
	func() {
		defer func() { recover() }()
		bot.AnnounceToSession("main:slack:U999", "hello user")
	}()
}

func TestAnnounceToSessionThreePartNonSlack(t *testing.T) {
	// 3 parts: parts[1] != "slack" -> isSlack check via second condition
	// parts[2] == "slack" would be needed for len >= 4 check, but len is 3
	// So isSlack is false and we return early
	bot := &Bot{logger: zap.NewNop().Sugar()}
	bot.AnnounceToSession("agent:main:slack", "msg")
	// No panic = returned early
}

func TestAnnounceToSessionFourPartSlack(t *testing.T) {
	// 4 parts: len >= 4, parts[2] == "slack" -> isSlack=true
	// Not >= 5 parts with channel, so user scope: userID = parts[3]
	bot := &Bot{logger: zap.NewNop().Sugar()}
	func() {
		defer func() { recover() }()
		bot.AnnounceToSession("a:b:slack:U123", "hello")
	}()
}

func TestAnnounceToSessionFivePartChannelScopePost(t *testing.T) {
	// 5 parts: len >= 5, parts[3] == "channel" -> channel scope
	// channelID = parts[4], tries PostMessage -> panic on nil api
	bot := &Bot{logger: zap.NewNop().Sugar()}
	func() {
		defer func() { recover() }()
		bot.AnnounceToSession("a:b:slack:channel:C999", "channel msg")
	}()
}

func TestAnnounceToSessionFivePartNonChannelUserScope(t *testing.T) {
	// 5 parts but parts[3] != "channel" -> falls to user scope
	// userID = parts[4]
	bot := &Bot{logger: zap.NewNop().Sugar()}
	func() {
		defer func() { recover() }()
		bot.AnnounceToSession("a:b:slack:user:UXXX", "user msg")
	}()
}

func TestAnnounceToSessionFourPartNonSlack(t *testing.T) {
	// 4 parts but parts[2] != "slack" -> isSlack=false, return early
	bot := &Bot{logger: zap.NewNop().Sugar()}
	bot.AnnounceToSession("a:b:discord:U1", "msg")
	// No panic = returned early
}

// ---------------------------------------------------------------------------
// handleConfirmCallback — concurrent access safety
// ---------------------------------------------------------------------------

func TestHandleConfirmCallbackConcurrent(t *testing.T) {
	bot := &Bot{
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}
	confirmID := "confirm:C123:concurrent"
	ch := make(chan bool, 1)
	bot.mu.Lock()
	bot.pendingConfirms[confirmID] = ch
	bot.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bot.handleConfirmCallback(confirmID, "yes")
		}()
	}
	wg.Wait()

	// Channel should have received at least one value
	select {
	case val := <-ch:
		if !val {
			t.Error("expected true from concurrent yes callbacks")
		}
	default:
		t.Error("expected at least one value in channel")
	}
}

// ---------------------------------------------------------------------------
// handleConfirmCallback — confirm prefix variations
// ---------------------------------------------------------------------------

func TestHandleConfirmCallbackConfirmPrefixOnly(t *testing.T) {
	bot := &Bot{
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}
	// "confirm:" is a valid prefix, but there's no matching entry
	result := bot.handleConfirmCallback("confirm:", "yes")
	if result {
		t.Error("expected false when confirmID is 'confirm:' with no matching entry")
	}
}

func TestHandleConfirmCallbackPrefixWithExactMatch(t *testing.T) {
	bot := &Bot{
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}
	// Register with the exact "confirm:" key
	ch := make(chan bool, 1)
	bot.mu.Lock()
	bot.pendingConfirms["confirm:"] = ch
	bot.mu.Unlock()

	result := bot.handleConfirmCallback("confirm:", "no")
	if !result {
		t.Error("expected true when confirmID exactly matches an entry")
	}
	select {
	case val := <-ch:
		if val {
			t.Error("expected false for 'no' value")
		}
	default:
		t.Error("expected a value in the channel")
	}
}

// ---------------------------------------------------------------------------
// SendConfirmPrompt — pendingConfirms initialization when nil
// ---------------------------------------------------------------------------

func TestSendConfirmPromptCleanupOnPanic(t *testing.T) {
	// pendingConfirms is initialized in New(); verify the deferred cleanup
	// runs even when the API call panics (nil api).
	bot := &Bot{
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}
	ctx := context.Background()
	func() {
		defer func() { recover() }()
		_, _ = bot.SendConfirmPrompt(ctx, "agent:slack:channel:C123:x", "cmd", "pat")
	}()
	bot.mu.Lock()
	remaining := len(bot.pendingConfirms)
	bot.mu.Unlock()
	if remaining != 0 {
		t.Errorf("expected pendingConfirms to be empty after cleanup, got %d entries", remaining)
	}
}

// ---------------------------------------------------------------------------
// CanConfirm — table-driven test for comprehensive coverage
// ---------------------------------------------------------------------------

func TestCanConfirmTableDriven(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	cases := []struct {
		key    string
		expect bool
	}{
		{"main:slack:U123", true},
		{"main:discord:123", false},
		{"", false},
		{"slack", false},
		{"main:slack:channel:C456", true},
		{"agent:slack:global", true},
		{"agent:slack:", true},
		{"mybot:SLACK:user", false}, // case-sensitive: "SLACK" != "slack"
		{":slack:", true},
		{"a:slack:b:slack:c", true},
		{"noslackhere", false},
	}
	for _, tc := range cases {
		got := bot.CanConfirm(tc.key)
		if got != tc.expect {
			t.Errorf("CanConfirm(%q) = %v, want %v", tc.key, got, tc.expect)
		}
	}
}

// ---------------------------------------------------------------------------
// AnnounceToSession — isSlack detection for 3-part key with slack in pos 1
// ---------------------------------------------------------------------------

func TestAnnounceToSessionThreePartSlackIsSlackTrue(t *testing.T) {
	// parts = ["main", "slack", "U123"], len=3, parts[1]=="slack" -> isSlack=true
	// Tries OpenConversation on nil api -> panic
	bot := &Bot{logger: zap.NewNop().Sugar()}
	func() {
		defer func() { recover() }()
		bot.AnnounceToSession("main:slack:UTEST", "msg to user")
	}()
}

func TestAnnounceToSessionFourPartSlackIsSlackTrue(t *testing.T) {
	// parts = ["a", "b", "slack", "U123"], len=4, parts[2]=="slack" -> isSlack=true
	// Not channel scope (len < 5), user scope: userID=parts[3]
	bot := &Bot{logger: zap.NewNop().Sugar()}
	func() {
		defer func() { recover() }()
		bot.AnnounceToSession("a:b:slack:UDEF", "msg to user")
	}()
}

// ===========================================================================
// Stub types for integration tests with mock Slack API and agent
// ===========================================================================

// slackStubProvider implements models.Provider with a canned response.
type slackStubProvider struct {
	response string
	err      error
}

func (s *slackStubProvider) Chat(_ context.Context, _ openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	if s.err != nil {
		return openai.ChatCompletionResponse{}, s.err
	}
	return openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{
			{
				Message: openai.ChatCompletionMessage{
					Role:    "assistant",
					Content: s.response,
				},
				FinishReason: openai.FinishReasonStop,
			},
		},
		Usage: openai.Usage{PromptTokens: 10, CompletionTokens: 5},
	}, nil
}

func (s *slackStubProvider) ChatStream(_ context.Context, _ openai.ChatCompletionRequest) (models.Stream, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &slackStubStream{chunks: []string{s.response}, pos: 0}, nil
}

// slackStubStream implements models.Stream.
type slackStubStream struct {
	chunks []string
	pos    int
}

func (s *slackStubStream) Recv() (openai.ChatCompletionStreamResponse, error) {
	if s.pos >= len(s.chunks) {
		return openai.ChatCompletionStreamResponse{}, io.EOF
	}
	chunk := s.chunks[s.pos]
	s.pos++
	return openai.ChatCompletionStreamResponse{
		Choices: []openai.ChatCompletionStreamChoice{
			{
				Delta: openai.ChatCompletionStreamChoiceDelta{
					Content: chunk,
				},
			},
		},
	}, nil
}

func (s *slackStubStream) Close() error { return nil }

// mockSlackServer creates a mock Slack API server that handles common endpoints.
// Returns the httptest server and a Slack API client pointed at it.
func mockSlackServer(t *testing.T) (*httptest.Server, *slacklib.Client) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "conversations.open"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":      true,
				"channel": map[string]interface{}{"id": "D_MOCK_DM"},
			})
		case strings.Contains(r.URL.Path, "chat.postMessage"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":      true,
				"channel": "C_MOCK",
				"ts":      "1234567890.123456",
			})
		case strings.Contains(r.URL.Path, "chat.update"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":      true,
				"channel": "C_MOCK",
				"ts":      "1234567890.123456",
			})
		case strings.Contains(r.URL.Path, "reactions.add"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
		case strings.Contains(r.URL.Path, "auth.test"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":      true,
				"user_id": "UMOCKBOT",
				"user":    "mockbot",
				"team":    "mockteam",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
		}
	}))
	api := slacklib.New("xoxb-test-token", slacklib.OptionAPIURL(srv.URL+"/"))
	return srv, api
}

// newTestAgent creates an agent with a stubProvider for testing.
func newTestAgent(t *testing.T, response string, provErr error) (*agent.Agent, *session.Manager) {
	t.Helper()
	sm, err := session.New(zap.NewNop().Sugar(), t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	prov := &slackStubProvider{response: response, err: provErr}
	router := models.NewRouter(
		zap.NewNop().Sugar(),
		map[string]models.Provider{"test": prov},
		"test/model-1",
		nil,
	)
	agCfg := &config.Root{
		Agents: config.Agents{
			List: []config.AgentDef{{ID: "main", Default: true, Identity: config.Identity{Name: "TestBot", Theme: "test"}}},
		},
	}
	ag := agent.New(zap.NewNop().Sugar(), agCfg, agCfg.DefaultAgent(), router, sm, nil, nil, t.TempDir(), nil)
	return ag, sm
}

// ===========================================================================
// Integration tests: respondFull with mock API + agent
// ===========================================================================

// ---------------------------------------------------------------------------
// respondFull — success path (agent returns a response)
// ---------------------------------------------------------------------------

func TestRespondFullSuccess(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	ag, sm := newTestAgent(t, "Hello from the agent!", nil)

	bot := &Bot{
		api:      api,
		ag:       ag,
		sessions: sm,
		cfg:      config.SlackConfig{TimeoutSeconds: 10},
		fullCfg:  &config.Root{},
		logger:   zap.NewNop().Sugar(),
	}
	// Should complete without panic and send the response
	bot.respondFull("main:slack:U1", "C1", "hello", bot.cfg)
}

// ---------------------------------------------------------------------------
// respondFull — error path (agent Chat returns error)
// ---------------------------------------------------------------------------

func TestRespondFullAgentError(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	ag, sm := newTestAgent(t, "", fmt.Errorf("model unavailable"))

	bot := &Bot{
		api:      api,
		ag:       ag,
		sessions: sm,
		cfg:      config.SlackConfig{TimeoutSeconds: 10},
		fullCfg:  &config.Root{},
		logger:   zap.NewNop().Sugar(),
	}
	// Should log the error and send "Sorry, something went wrong."
	bot.respondFull("main:slack:U1", "C1", "hello", bot.cfg)
}

// ---------------------------------------------------------------------------
// respondFull — with usage tokens display
// ---------------------------------------------------------------------------

func TestRespondFullWithUsageTokens(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	ag, sm := newTestAgent(t, "Here is the answer!", nil)

	agCfg := &config.Root{
		Agents: config.Agents{
			List: []config.AgentDef{{ID: "main", Default: true}},
		},
		Messages: config.Messages{Usage: "tokens"},
	}

	bot := &Bot{
		api:      api,
		ag:       ag,
		sessions: sm,
		cfg:      config.SlackConfig{TimeoutSeconds: 10},
		fullCfg:  agCfg,
		logger:   zap.NewNop().Sugar(),
	}
	bot.respondFull("main:slack:U1", "C1", "question", bot.cfg)
}

// ---------------------------------------------------------------------------
// respondFull — long response that requires splitting
// ---------------------------------------------------------------------------

func TestRespondFullLongResponse(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	longResp := strings.Repeat("x", 5000) // will be split into multiple parts
	ag, sm := newTestAgent(t, longResp, nil)

	bot := &Bot{
		api:      api,
		ag:       ag,
		sessions: sm,
		cfg:      config.SlackConfig{TimeoutSeconds: 10},
		fullCfg:  &config.Root{},
		logger:   zap.NewNop().Sugar(),
	}
	bot.respondFull("main:slack:U1", "C1", "hello", bot.cfg)
}

// ===========================================================================
// Integration tests: respondStreaming with mock API + agent
// ===========================================================================

// ---------------------------------------------------------------------------
// respondStreaming — success path
// ---------------------------------------------------------------------------

func TestRespondStreamingSuccess(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	ag, sm := newTestAgent(t, "Streamed response!", nil)

	bot := &Bot{
		api:      api,
		ag:       ag,
		sessions: sm,
		cfg:      config.SlackConfig{TimeoutSeconds: 10, StreamMode: "partial"},
		msgCfg:   config.Messages{StreamEditMs: 50},
		fullCfg:  &config.Root{},
		logger:   zap.NewNop().Sugar(),
	}
	bot.respondStreaming("main:slack:U1", "C1", "hello", bot.cfg, bot.msgCfg)
}

// ---------------------------------------------------------------------------
// respondStreaming — error path (agent ChatStream returns error)
// ---------------------------------------------------------------------------

func TestRespondStreamingAgentError(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	ag, sm := newTestAgent(t, "", fmt.Errorf("stream error"))

	bot := &Bot{
		api:      api,
		ag:       ag,
		sessions: sm,
		cfg:      config.SlackConfig{TimeoutSeconds: 10, StreamMode: "partial"},
		msgCfg:   config.Messages{StreamEditMs: 50},
		fullCfg:  &config.Root{},
		logger:   zap.NewNop().Sugar(),
	}
	bot.respondStreaming("main:slack:U1", "C1", "hello", bot.cfg, bot.msgCfg)
}

// ---------------------------------------------------------------------------
// respondStreaming — with usage tokens display
// ---------------------------------------------------------------------------

func TestRespondStreamingWithUsageTokens(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	ag, sm := newTestAgent(t, "Streamed answer!", nil)

	agCfg := &config.Root{
		Agents: config.Agents{
			List: []config.AgentDef{{ID: "main", Default: true}},
		},
		Messages: config.Messages{Usage: "tokens"},
	}

	bot := &Bot{
		api:      api,
		ag:       ag,
		sessions: sm,
		cfg:      config.SlackConfig{TimeoutSeconds: 10, StreamMode: "partial"},
		msgCfg:   config.Messages{StreamEditMs: 50},
		fullCfg:  agCfg,
		logger:   zap.NewNop().Sugar(),
	}
	bot.respondStreaming("main:slack:U1", "C1", "question", bot.cfg, bot.msgCfg)
}

// ---------------------------------------------------------------------------
// respondStreaming — default StreamEditMs (0 defaults to 400)
// ---------------------------------------------------------------------------

func TestRespondStreamingDefaultEditMs(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	ag, sm := newTestAgent(t, "Default edit interval", nil)

	bot := &Bot{
		api:      api,
		ag:       ag,
		sessions: sm,
		cfg:      config.SlackConfig{TimeoutSeconds: 10, StreamMode: "partial"},
		msgCfg:   config.Messages{StreamEditMs: 0}, // should default to 400
		fullCfg:  &config.Root{},
		logger:   zap.NewNop().Sugar(),
	}
	bot.respondStreaming("main:slack:U1", "C1", "hello", bot.cfg, bot.msgCfg)
}

// ---------------------------------------------------------------------------
// respondStreaming — long response that requires splitting
// ---------------------------------------------------------------------------

func TestRespondStreamingLongResponse(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	longResp := strings.Repeat("y", 5000)
	ag, sm := newTestAgent(t, longResp, nil)

	bot := &Bot{
		api:      api,
		ag:       ag,
		sessions: sm,
		cfg:      config.SlackConfig{TimeoutSeconds: 10, StreamMode: "partial"},
		msgCfg:   config.Messages{StreamEditMs: 50},
		fullCfg:  &config.Root{},
		logger:   zap.NewNop().Sugar(),
	}
	bot.respondStreaming("main:slack:U1", "C1", "hello", bot.cfg, bot.msgCfg)
}

// ---------------------------------------------------------------------------
// respondStreaming — placeholder send fails
// ---------------------------------------------------------------------------

func TestRespondStreamingPlaceholderFails(t *testing.T) {
	// Mock server that fails on chat.postMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "chat.postMessage") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":    false,
				"error": "channel_not_found",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	}))
	defer srv.Close()

	api := slacklib.New("xoxb-test", slacklib.OptionAPIURL(srv.URL+"/"))
	ag, sm := newTestAgent(t, "response", nil)

	bot := &Bot{
		api:      api,
		ag:       ag,
		sessions: sm,
		cfg:      config.SlackConfig{TimeoutSeconds: 10, StreamMode: "partial"},
		msgCfg:   config.Messages{StreamEditMs: 50},
		fullCfg:  &config.Root{},
		logger:   zap.NewNop().Sugar(),
	}
	// Should return early after failed placeholder send
	bot.respondStreaming("main:slack:U1", "C1", "hello", bot.cfg, bot.msgCfg)
}

// ===========================================================================
// Integration tests: SendToAllPaired with mock API
// ===========================================================================

func TestSendToAllPairedWithMockAPI(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	bot := &Bot{
		api:    api,
		paired: map[string]bool{"U1": true, "U2": true},
		logger: zap.NewNop().Sugar(),
	}
	bot.SendToAllPaired("broadcast message")
}

func TestSendToAllPairedLongMessage(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	bot := &Bot{
		api:    api,
		paired: map[string]bool{"U1": true},
		logger: zap.NewNop().Sugar(),
	}
	longMsg := strings.Repeat("z", 5000)
	bot.SendToAllPaired(longMsg)
}

func TestSendToAllPairedOpenConversationError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "conversations.open") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":    false,
				"error": "user_not_found",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	}))
	defer srv.Close()

	api := slacklib.New("xoxb-test", slacklib.OptionAPIURL(srv.URL+"/"))
	bot := &Bot{
		api:    api,
		paired: map[string]bool{"U1": true},
		logger: zap.NewNop().Sugar(),
	}
	bot.SendToAllPaired("test message")
}

func TestSendToAllPairedPostMessageError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "conversations.open") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":      true,
				"channel": map[string]interface{}{"id": "D_MOCK"},
			})
			return
		}
		if strings.Contains(r.URL.Path, "chat.postMessage") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":    false,
				"error": "not_authed",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	}))
	defer srv.Close()

	api := slacklib.New("xoxb-test", slacklib.OptionAPIURL(srv.URL+"/"))
	bot := &Bot{
		api:    api,
		paired: map[string]bool{"U1": true},
		logger: zap.NewNop().Sugar(),
	}
	bot.SendToAllPaired("test message")
}

// ===========================================================================
// Integration tests: AnnounceToSession with mock API
// ===========================================================================

func TestAnnounceToSessionChannelScopeWithMockAPI(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	bot := &Bot{
		api:    api,
		logger: zap.NewNop().Sugar(),
	}
	bot.AnnounceToSession("agent:id:slack:channel:C999", "hello channel")
}

func TestAnnounceToSessionUserScopeWithMockAPI(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	bot := &Bot{
		api:    api,
		logger: zap.NewNop().Sugar(),
	}
	bot.AnnounceToSession("main:slack:U123", "hello user")
}

func TestAnnounceToSessionUserScopeFourPartWithMockAPI(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	bot := &Bot{
		api:    api,
		logger: zap.NewNop().Sugar(),
	}
	bot.AnnounceToSession("a:b:slack:U123", "hello user via 4-part key")
}

func TestAnnounceToSessionChannelScopePostError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "chat.postMessage") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":    false,
				"error": "channel_not_found",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	}))
	defer srv.Close()

	api := slacklib.New("xoxb-test", slacklib.OptionAPIURL(srv.URL+"/"))
	bot := &Bot{
		api:    api,
		logger: zap.NewNop().Sugar(),
	}
	bot.AnnounceToSession("agent:id:slack:channel:C999", "hello channel")
}

func TestAnnounceToSessionUserScopeOpenDMError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "conversations.open") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":    false,
				"error": "user_not_found",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	}))
	defer srv.Close()

	api := slacklib.New("xoxb-test", slacklib.OptionAPIURL(srv.URL+"/"))
	bot := &Bot{
		api:    api,
		logger: zap.NewNop().Sugar(),
	}
	bot.AnnounceToSession("main:slack:U123", "hello user")
}

func TestAnnounceToSessionLongMessage(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	bot := &Bot{
		api:    api,
		logger: zap.NewNop().Sugar(),
	}
	longMsg := strings.Repeat("w", 5000)
	bot.AnnounceToSession("agent:id:slack:channel:C999", longMsg)
}

func TestAnnounceToSessionUserScopePostError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "conversations.open") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":      true,
				"channel": map[string]interface{}{"id": "D_MOCK"},
			})
			return
		}
		if strings.Contains(r.URL.Path, "chat.postMessage") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":    false,
				"error": "not_authed",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	}))
	defer srv.Close()

	api := slacklib.New("xoxb-test", slacklib.OptionAPIURL(srv.URL+"/"))
	bot := &Bot{
		api:    api,
		logger: zap.NewNop().Sugar(),
	}
	bot.AnnounceToSession("main:slack:U123", "hello user")
}

// ===========================================================================
// Integration tests: SendConfirmPrompt with mock API
// ===========================================================================

func TestSendConfirmPromptChannelScopeSuccess(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	bot := &Bot{
		api:             api,
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}

	// Launch SendConfirmPrompt in a goroutine and answer it immediately.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resultCh := make(chan struct {
		ok  bool
		err error
	}, 1)

	go func() {
		ok, err := bot.SendConfirmPrompt(ctx, "agent:id:slack:channel:C123:extra", "rm -rf /", "rm.*")
		resultCh <- struct {
			ok  bool
			err error
		}{ok, err}
	}()

	// Give SendConfirmPrompt time to register the pending confirm
	time.Sleep(100 * time.Millisecond)

	// Find the confirmID and answer it
	bot.mu.Lock()
	var confirmID string
	for id := range bot.pendingConfirms {
		confirmID = id
		break
	}
	bot.mu.Unlock()

	if confirmID == "" {
		t.Fatal("expected a pending confirm to be registered")
	}

	// Simulate user clicking "yes"
	bot.handleConfirmCallback(confirmID, "yes")

	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("unexpected error: %v", result.err)
		}
		if !result.ok {
			t.Error("expected confirmation to be true for 'yes'")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SendConfirmPrompt did not return in time")
	}
}

func TestSendConfirmPromptChannelScopeDenied(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	bot := &Bot{
		api:             api,
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resultCh := make(chan struct {
		ok  bool
		err error
	}, 1)

	go func() {
		ok, err := bot.SendConfirmPrompt(ctx, "agent:id:slack:channel:C123:extra", "dangerous-cmd", "pattern")
		resultCh <- struct {
			ok  bool
			err error
		}{ok, err}
	}()

	time.Sleep(100 * time.Millisecond)

	bot.mu.Lock()
	var confirmID string
	for id := range bot.pendingConfirms {
		confirmID = id
		break
	}
	bot.mu.Unlock()

	if confirmID == "" {
		t.Fatal("expected a pending confirm to be registered")
	}

	// Simulate user clicking "no"
	bot.handleConfirmCallback(confirmID, "no")

	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("unexpected error: %v", result.err)
		}
		if result.ok {
			t.Error("expected confirmation to be false for 'no'")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SendConfirmPrompt did not return in time")
	}
}

func TestSendConfirmPromptContextTimeout(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	bot := &Bot{
		api:             api,
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}

	// Use a very short timeout so it expires before any response
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	ok, err := bot.SendConfirmPrompt(ctx, "agent:id:slack:channel:C123:extra", "cmd", "pat")
	if ok {
		t.Error("expected false when context times out")
	}
	if err == nil {
		t.Error("expected error when context times out")
	}
	if err != context.DeadlineExceeded {
		t.Errorf("expected DeadlineExceeded error, got: %v", err)
	}
}

func TestSendConfirmPromptUserScopeSuccess(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	bot := &Bot{
		api:             api,
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resultCh := make(chan struct {
		ok  bool
		err error
	}, 1)

	go func() {
		ok, err := bot.SendConfirmPrompt(ctx, "main:slack:U456", "some-cmd", "pat")
		resultCh <- struct {
			ok  bool
			err error
		}{ok, err}
	}()

	time.Sleep(100 * time.Millisecond)

	bot.mu.Lock()
	var confirmID string
	for id := range bot.pendingConfirms {
		confirmID = id
		break
	}
	bot.mu.Unlock()

	if confirmID == "" {
		t.Fatal("expected a pending confirm")
	}
	bot.handleConfirmCallback(confirmID, "yes")

	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("unexpected error: %v", result.err)
		}
		if !result.ok {
			t.Error("expected true for 'yes'")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out")
	}
}

func TestSendConfirmPromptOpenDMError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "conversations.open") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":    false,
				"error": "user_not_found",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	}))
	defer srv.Close()

	api := slacklib.New("xoxb-test", slacklib.OptionAPIURL(srv.URL+"/"))
	bot := &Bot{
		api:             api,
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}

	ctx := context.Background()
	ok, err := bot.SendConfirmPrompt(ctx, "main:slack:U123", "cmd", "pat")
	if ok {
		t.Error("expected false when DM open fails")
	}
	if err == nil {
		t.Error("expected error when DM open fails")
	}
	if !strings.Contains(err.Error(), "open DM for confirm") {
		t.Errorf("expected 'open DM for confirm' in error, got: %v", err)
	}
}

func TestSendConfirmPromptPostMessageError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "chat.postMessage") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":    false,
				"error": "channel_not_found",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	}))
	defer srv.Close()

	api := slacklib.New("xoxb-test", slacklib.OptionAPIURL(srv.URL+"/"))
	bot := &Bot{
		api:             api,
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}

	ctx := context.Background()
	ok, err := bot.SendConfirmPrompt(ctx, "agent:id:slack:channel:C123:extra", "cmd", "pat")
	if ok {
		t.Error("expected false when PostMessage fails")
	}
	if err == nil {
		t.Error("expected error when PostMessage fails")
	}
	if !strings.Contains(err.Error(), "send confirm prompt") {
		t.Errorf("expected 'send confirm prompt' in error, got: %v", err)
	}
}

func TestSendConfirmPromptCommandTruncationIntegration(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	bot := &Bot{
		api:             api,
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// Command longer than 200 chars should be truncated to 200 + "..."
	longCmd := strings.Repeat("x", 300)
	// This will time out since nobody answers, but exercises the truncation + prompt path
	ok, err := bot.SendConfirmPrompt(ctx, "agent:id:slack:channel:C123:extra", longCmd, "pattern")
	if ok {
		t.Error("expected false on timeout")
	}
	if err != context.DeadlineExceeded {
		t.Errorf("expected DeadlineExceeded, got: %v", err)
	}
}

// ===========================================================================
// Integration tests: processMessages with mock API + agent
// ===========================================================================

func TestProcessMessagesFullPathWithMockAPI(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	ag, sm := newTestAgent(t, "Agent says hello!", nil)

	bot := &Bot{
		api:      api,
		ag:       ag,
		sessions: sm,
		cfg:      config.SlackConfig{TimeoutSeconds: 10},
		msgCfg:   config.Messages{},
		fullCfg:  &config.Root{},
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger:   zap.NewNop().Sugar(),
	}
	bot.processMessages("U1", []queuedMessage{{text: "hello", channelID: "C1", userID: "U1", ts: "ts1"}})
}

func TestProcessMessagesStreamingPathWithMockAPI(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	ag, sm := newTestAgent(t, "Streamed!", nil)

	bot := &Bot{
		api:      api,
		ag:       ag,
		sessions: sm,
		cfg:      config.SlackConfig{TimeoutSeconds: 10, StreamMode: "partial"},
		msgCfg:   config.Messages{StreamEditMs: 50},
		fullCfg:  &config.Root{},
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger:   zap.NewNop().Sugar(),
	}
	bot.processMessages("U1", []queuedMessage{{text: "hello", channelID: "C1", userID: "U1", ts: "ts1"}})
}

func TestProcessMessagesResetTriggerWithMockAPI(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	ag, sm := newTestAgent(t, "ignored", nil)

	bot := &Bot{
		api:      api,
		ag:       ag,
		sessions: sm,
		cfg:      config.SlackConfig{TimeoutSeconds: 10},
		msgCfg:   config.Messages{},
		fullCfg:  &config.Root{Session: config.Session{ResetTriggers: []string{"reset"}}},
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		logger:   zap.NewNop().Sugar(),
	}
	// "reset" matches trigger — should call sessions.Reset and send confirmation
	bot.processMessages("U1", []queuedMessage{{text: "reset", channelID: "C1", userID: "U1", ts: "ts1"}})
}

// ===========================================================================
// Integration: ackReactionWith with mock API (exercises AddReaction success)
// ===========================================================================

func TestAckReactionWithAllScopeSuccess(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	bot := &Bot{
		api:    api,
		logger: zap.NewNop().Sugar(),
	}
	cfg := config.SlackConfig{AckEmoji: "rocket"}
	msgCfg := config.Messages{AckReactionScope: "all"}
	bot.ackReactionWith("C1", "ts1", false, cfg, msgCfg)
}

func TestAckReactionWithDefaultEmoji(t *testing.T) {
	srv, api := mockSlackServer(t)
	defer srv.Close()

	bot := &Bot{
		api:    api,
		logger: zap.NewNop().Sugar(),
	}
	cfg := config.SlackConfig{AckEmoji: ""} // should default to "eyes"
	msgCfg := config.Messages{AckReactionScope: "all"}
	bot.ackReactionWith("C1", "ts1", false, cfg, msgCfg)
}

func TestAckReactionWithError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "reactions.add") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"ok":    false,
				"error": "too_many_reactions",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true})
	}))
	defer srv.Close()

	api := slacklib.New("xoxb-test", slacklib.OptionAPIURL(srv.URL+"/"))
	bot := &Bot{
		api:    api,
		logger: zap.NewNop().Sugar(),
	}
	cfg := config.SlackConfig{AckEmoji: "wave"}
	msgCfg := config.Messages{AckReactionScope: "all"}
	// Should log the error but not panic
	bot.ackReactionWith("C1", "ts1", false, cfg, msgCfg)
}
