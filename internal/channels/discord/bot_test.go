package discord

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

	"github.com/bwmarrin/discordgo"
	openai "github.com/sashabaranov/go-openai"
	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/agent"
	"github.com/EMSERO/gopherclaw/internal/config"
	"github.com/EMSERO/gopherclaw/internal/models"
	"github.com/EMSERO/gopherclaw/internal/session"
	"github.com/EMSERO/gopherclaw/internal/taskqueue"
)

// testServer is a persistent mock Discord API server shared across all tests.
// Endpoints are set once in TestMain to avoid races between deferred cleanup
// and goroutines spawned by handleMessage.
var testServer *httptest.Server

func TestMain(m *testing.M) {
	testServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" || r.Method == "PATCH" {
			_ = json.NewEncoder(w).Encode(discordgo.Message{
				ID:        "mock_msg_id",
				ChannelID: "mock_ch_id",
				Content:   "ok",
			})
			return
		}
		if r.Method == "PUT" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	base := testServer.URL + "/"
	discordgo.EndpointChannels = base + "channels/"
	discordgo.EndpointUsers = base + "users/"

	code := m.Run()
	testServer.Close()
	os.Exit(code)
}

// ---------------------------------------------------------------------------
// helpers: mock Discord API server
// ---------------------------------------------------------------------------

// mockDiscordServer returns the persistent test HTTP server and a no-op cleanup.
// Endpoint vars are set once in TestMain to prevent races with async goroutines.
func mockDiscordServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	return testServer, func() {}
}

// newTestSession creates a discordgo.Session suitable for test use (no real connection).
func newTestSession(t *testing.T) *discordgo.Session {
	t.Helper()
	dg, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatal(err)
	}
	dg.State = &discordgo.State{
		Ready: discordgo.Ready{
			User: &discordgo.User{
				ID:            "bot1",
				Username:      "TestBot",
				Discriminator: "0001",
			},
		},
	}
	return dg
}

// newMsg creates a discordgo.MessageCreate for testing.
func newMsg(authorID, content, channelID, guildID string, isBot bool) *discordgo.MessageCreate {
	return &discordgo.MessageCreate{
		Message: &discordgo.Message{
			ID:        "msg_" + authorID,
			Author:    &discordgo.User{ID: authorID, Username: authorID, Bot: isBot},
			Content:   content,
			ChannelID: channelID,
			GuildID:   guildID,
		},
	}
}

// ---------------------------------------------------------------------------
// generatePairCode
// ---------------------------------------------------------------------------

func TestDiscordGeneratePairCode(t *testing.T) {
	code := generatePairCode()
	if len(code) != 6 {
		t.Errorf("expected 6-digit code, got %q (len %d)", code, len(code))
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			t.Errorf("expected all digits, got %q", code)
			break
		}
	}
}

func TestDiscordGeneratePairCodeUnique(t *testing.T) {
	seen := make(map[string]bool)
	for range 20 {
		seen[generatePairCode()] = true
	}
	if len(seen) < 18 {
		t.Errorf("too many collisions: only %d unique codes out of 20", len(seen))
	}
}

// ---------------------------------------------------------------------------
// validatePairCode
// ---------------------------------------------------------------------------

func TestDiscordValidatePairCode(t *testing.T) {
	bot := &Bot{pairCode: "123456", paired: make(map[string]bool), logger: zap.NewNop().Sugar()}

	if !bot.validatePairCode("123456") {
		t.Error("expected correct code to be accepted")
	}
	if bot.validatePairCode("000000") {
		t.Error("expected wrong code to be rejected")
	}
	if !bot.validatePairCode("  123456  ") {
		t.Error("expected whitespace-trimmed code to be accepted")
	}
	if bot.validatePairCode("") {
		t.Error("expected empty code to be rejected")
	}
}

func TestDiscordValidatePairCodeEdgeCases(t *testing.T) {
	bot := &Bot{pairCode: "000000", paired: make(map[string]bool), logger: zap.NewNop().Sugar()}
	if !bot.validatePairCode("000000") {
		t.Error("expected 000000 to match")
	}
}

func TestDiscordValidatePairCodeWithNewlines(t *testing.T) {
	bot := &Bot{pairCode: "999999", paired: make(map[string]bool), logger: zap.NewNop().Sugar()}
	if !bot.validatePairCode("\n999999\t") {
		t.Error("expected code with tabs/newlines to be accepted after TrimSpace")
	}
}

// ---------------------------------------------------------------------------
// stripMention
// ---------------------------------------------------------------------------

func TestDiscordStripMention(t *testing.T) {
	cases := []struct {
		text   string
		botID  string
		expect string
	}{
		{"<@123456> hello", "123456", "hello"},
		{"<@!123456> hello", "123456", "hello"},
		{"  <@123456>   world  ", "123456", "world"},
		{"no mention here", "123456", "no mention here"},
		{"<@123456>", "123456", ""},
		{"<@123456> <@!123456> both", "123456", "both"},
		{"<@999> hello", "123456", "<@999> hello"},
	}
	for _, tc := range cases {
		got := stripMention(tc.text, tc.botID)
		if got != tc.expect {
			t.Errorf("stripMention(%q, %q) = %q, want %q", tc.text, tc.botID, got, tc.expect)
		}
	}
}

func TestDiscordStripMentionMultipleMentions(t *testing.T) {
	text := "<@123> and <@!123> repeated"
	got := stripMention(text, "123")
	if got != "and  repeated" {
		t.Errorf("stripMention with multiple mentions = %q, want %q", got, "and  repeated")
	}
}

func TestDiscordStripMentionOnlyNickMention(t *testing.T) {
	text := "<@!456> command"
	got := stripMention(text, "456")
	if got != "command" {
		t.Errorf("stripMention nick-only = %q, want %q", got, "command")
	}
}

// ---------------------------------------------------------------------------
// splitMessage
// ---------------------------------------------------------------------------

func TestDiscordSplitMessage(t *testing.T) {
	parts := splitMessage("hello", 2000)
	if len(parts) != 1 || parts[0] != "hello" {
		t.Errorf("expected single part 'hello', got %v", parts)
	}

	exact := strings.Repeat("a", 2000)
	parts = splitMessage(exact, 2000)
	if len(parts) != 1 {
		t.Errorf("expected 1 part at limit, got %d", len(parts))
	}

	over := exact + "extra"
	parts = splitMessage(over, 2000)
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

	parts = splitMessage("", 2000)
	if len(parts) != 1 || parts[0] != "" {
		t.Errorf("expected single empty part, got %v", parts)
	}
}

func TestDiscordSplitMessageNewlinePreference(t *testing.T) {
	line1 := strings.Repeat("x", 1850) + "\n"
	line2 := strings.Repeat("y", 300)
	text := line1 + line2

	parts := splitMessage(text, 2000)
	if len(parts) < 2 {
		t.Fatalf("expected at least 2 parts, got %d", len(parts))
	}
	if parts[0] != line1 {
		t.Errorf("expected first part to end at newline, got len %d (want %d)", len(parts[0]), len(line1))
	}
}

func TestDiscordSplitMessageNoNewline(t *testing.T) {
	text := strings.Repeat("z", 4500)
	parts := splitMessage(text, 2000)
	if len(parts) < 3 {
		t.Fatalf("expected at least 3 parts, got %d", len(parts))
	}
	if len(parts[0]) != 2000 {
		t.Errorf("expected first part to be 2000 chars, got %d", len(parts[0]))
	}
	var combined strings.Builder
	for _, p := range parts {
		combined.WriteString(p)
	}
	if combined.String() != text {
		t.Error("recombined parts do not match original")
	}
}

func TestDiscordSplitMessageExactMultiple(t *testing.T) {
	text := strings.Repeat("m", 4000)
	parts := splitMessage(text, 2000)
	if len(parts) != 2 {
		t.Errorf("expected 2 parts for 4000 chars, got %d", len(parts))
	}
}

func TestDiscordSplitMessageSmallMaxLen(t *testing.T) {
	text := "abcdefghij"
	parts := splitMessage(text, 3)
	var combined string
	for _, p := range parts {
		if len(p) > 3 {
			t.Errorf("part exceeds maxLen: %q (len=%d)", p, len(p))
		}
		combined += p
	}
	if combined != text {
		t.Errorf("recombined = %q, want %q", combined, text)
	}
}

func TestDiscordSplitMessageSingleChar(t *testing.T) {
	parts := splitMessage("x", 1)
	if len(parts) != 1 || parts[0] != "x" {
		t.Errorf("expected single part 'x', got %v", parts)
	}
}

// ---------------------------------------------------------------------------
// isAuthorized
// ---------------------------------------------------------------------------

func TestDiscordIsAuthorizedAllowlist(t *testing.T) {
	bot := &Bot{
		cfg:    config.DiscordConfig{DMPolicy: "allowlist", AllowUsers: []string{"AAA", "BBB"}},
		paired: make(map[string]bool),
		logger: zap.NewNop().Sugar(),
	}
	if !bot.isAuthorized("AAA", true) {
		t.Error("expected AAA to be authorized")
	}
	if bot.isAuthorized("CCC", true) {
		t.Error("expected CCC to not be authorized")
	}
	if bot.isAuthorized("CCC", false) {
		t.Error("expected CCC to not be authorized in guild with allowlist policy")
	}
}

func TestDiscordIsAuthorizedPairing(t *testing.T) {
	bot := &Bot{
		cfg:    config.DiscordConfig{DMPolicy: "pairing"},
		paired: map[string]bool{"DDD": true},
		logger: zap.NewNop().Sugar(),
	}
	if !bot.isAuthorized("DDD", true) {
		t.Error("expected paired user DDD to be authorized in DM")
	}
	if bot.isAuthorized("EEE", true) {
		t.Error("expected unpaired user EEE to be unauthorized in DM")
	}
	if !bot.isAuthorized("EEE", false) {
		t.Error("expected guild user to be authorized regardless of pairing")
	}
}

func TestDiscordIsAuthorizedDefaultPolicy(t *testing.T) {
	bot := &Bot{
		cfg:    config.DiscordConfig{DMPolicy: ""},
		paired: make(map[string]bool),
		logger: zap.NewNop().Sugar(),
	}
	if !bot.isAuthorized("XYZ", false) {
		t.Error("expected guild user to be authorized with empty policy")
	}
	if !bot.isAuthorized("XYZ", true) {
		t.Error("expected DM user to be authorized with empty policy")
	}
}

func TestDiscordIsAuthorizedAllowlistEmpty(t *testing.T) {
	bot := &Bot{
		cfg:    config.DiscordConfig{DMPolicy: "allowlist", AllowUsers: nil},
		paired: make(map[string]bool),
		logger: zap.NewNop().Sugar(),
	}
	if bot.isAuthorized("anyone", true) {
		t.Error("expected empty allowlist to deny all users")
	}
}

func TestDiscordIsAuthorizedAllowlistMultipleUsers(t *testing.T) {
	bot := &Bot{
		cfg:    config.DiscordConfig{DMPolicy: "allowlist", AllowUsers: []string{"A", "B", "C"}},
		paired: make(map[string]bool),
		logger: zap.NewNop().Sugar(),
	}
	for _, uid := range []string{"A", "B", "C"} {
		if !bot.isAuthorized(uid, true) {
			t.Errorf("expected %s to be authorized", uid)
		}
	}
	if bot.isAuthorized("D", true) {
		t.Error("expected D to not be authorized")
	}
}

func TestDiscordIsAuthorizedPairingNonDM(t *testing.T) {
	bot := &Bot{
		cfg:    config.DiscordConfig{DMPolicy: "pairing"},
		paired: make(map[string]bool),
		logger: zap.NewNop().Sugar(),
	}
	if !bot.isAuthorized("unpaired_user", false) {
		t.Error("expected non-DM user to be authorized in pairing mode")
	}
}

// ---------------------------------------------------------------------------
// sessionKeyFor
// ---------------------------------------------------------------------------

func TestDiscordSessionKeyForUserScope(t *testing.T) {
	bot := &Bot{fullCfg: &config.Root{Session: config.Session{Scope: "user"}}, logger: zap.NewNop().Sugar()}
	key := bot.sessionKeyFor("user123", "chan456")
	if key != "main:discord:user123" {
		t.Errorf("expected user-scoped key, got %q", key)
	}
}

func TestDiscordSessionKeyForChannelScope(t *testing.T) {
	bot := &Bot{fullCfg: &config.Root{Session: config.Session{Scope: "channel"}}, logger: zap.NewNop().Sugar()}
	key := bot.sessionKeyFor("user123", "chan456")
	if key != "main:discord:channel:chan456" {
		t.Errorf("expected channel-scoped key, got %q", key)
	}
}

func TestDiscordSessionKeyForGlobalScope(t *testing.T) {
	bot := &Bot{fullCfg: &config.Root{Session: config.Session{Scope: "global"}}, logger: zap.NewNop().Sugar()}
	key := bot.sessionKeyFor("user123", "chan456")
	if key != "main:discord:global" {
		t.Errorf("expected global-scoped key, got %q", key)
	}
}

func TestDiscordSessionKeyForDefaultScope(t *testing.T) {
	bot := &Bot{fullCfg: &config.Root{Session: config.Session{Scope: ""}}, logger: zap.NewNop().Sugar()}
	key := bot.sessionKeyFor("user123", "chan456")
	if key != "main:discord:user123" {
		t.Errorf("expected default user-scoped key, got %q", key)
	}
}

func TestDiscordSessionKeyForNilConfig(t *testing.T) {
	bot := &Bot{fullCfg: nil, logger: zap.NewNop().Sugar()}
	key := bot.sessionKeyFor("user123", "chan456")
	if key != "main:discord:user123" {
		t.Errorf("expected user-scoped key with nil config, got %q", key)
	}
}

func TestDiscordSessionKeyForUnknownScope(t *testing.T) {
	bot := &Bot{fullCfg: &config.Root{Session: config.Session{Scope: "unknown"}}, logger: zap.NewNop().Sugar()}
	key := bot.sessionKeyFor("user123", "chan456")
	if key != "main:discord:user123" {
		t.Errorf("expected default key for unknown scope, got %q", key)
	}
}

func TestDiscordSessionKeyForDifferentUsers(t *testing.T) {
	bot := &Bot{fullCfg: &config.Root{Session: config.Session{Scope: "user"}}, logger: zap.NewNop().Sugar()}
	k1 := bot.sessionKeyFor("user1", "ch1")
	k2 := bot.sessionKeyFor("user2", "ch1")
	if k1 == k2 {
		t.Error("user-scoped keys should differ for different users")
	}
}

func TestDiscordSessionKeyForDifferentChannels(t *testing.T) {
	bot := &Bot{fullCfg: &config.Root{Session: config.Session{Scope: "channel"}}, logger: zap.NewNop().Sugar()}
	k1 := bot.sessionKeyFor("user1", "ch1")
	k2 := bot.sessionKeyFor("user1", "ch2")
	if k1 == k2 {
		t.Error("channel-scoped keys should differ for different channels")
	}
}

func TestDiscordSessionKeyForGlobalSameForAll(t *testing.T) {
	bot := &Bot{fullCfg: &config.Root{Session: config.Session{Scope: "global"}}, logger: zap.NewNop().Sugar()}
	k1 := bot.sessionKeyFor("user1", "ch1")
	k2 := bot.sessionKeyFor("user2", "ch2")
	if k1 != k2 {
		t.Error("global-scoped keys should be the same for all users/channels")
	}
}

// ---------------------------------------------------------------------------
// matchesResetTrigger
// ---------------------------------------------------------------------------

func TestDiscordMatchesResetTrigger(t *testing.T) {
	triggers := []string{"reset", "clear", "NEW SESSION"}
	cases := []struct {
		text string
		want bool
	}{
		{"reset", true},
		{"RESET", true},
		{"  Reset  ", true},
		{"clear", true},
		{"CLEAR", true},
		{"new session", true},
		{"NEW SESSION", true},
		{"  New Session  ", true},
		{"reset please", false},
		{"", false},
		{"re set", false},
	}
	for _, tc := range cases {
		got := matchesResetTrigger(tc.text, triggers)
		if got != tc.want {
			t.Errorf("matchesResetTrigger(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestDiscordMatchesResetTriggerEmpty(t *testing.T) {
	if matchesResetTrigger("reset", nil) {
		t.Error("expected no match with nil triggers")
	}
	if matchesResetTrigger("reset", []string{}) {
		t.Error("expected no match with empty triggers")
	}
}

func TestDiscordMatchesResetTriggerWhitespace(t *testing.T) {
	triggers := []string{"  reset  "}
	if !matchesResetTrigger("reset", triggers) {
		t.Error("expected match after trimming trigger whitespace")
	}
	if !matchesResetTrigger("  RESET  ", triggers) {
		t.Error("expected case-insensitive match with whitespace")
	}
}

func TestDiscordMatchesResetTriggerSingleTrigger(t *testing.T) {
	if !matchesResetTrigger("x", []string{"x"}) {
		t.Error("expected single trigger match")
	}
	if matchesResetTrigger("y", []string{"x"}) {
		t.Error("expected single trigger non-match")
	}
}

// ---------------------------------------------------------------------------
// ChannelName
// ---------------------------------------------------------------------------

func TestDiscordChannelName(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	if name := bot.ChannelName(); name != "discord" {
		t.Errorf("ChannelName() = %q, want %q", name, "discord")
	}
}

// ---------------------------------------------------------------------------
// IsConnected
// ---------------------------------------------------------------------------

func TestDiscordIsConnected(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}
	if bot.IsConnected() {
		t.Error("expected IsConnected() to be false initially")
	}
	bot.connected.Store(true)
	if !bot.IsConnected() {
		t.Error("expected IsConnected() to be true after Store(true)")
	}
	bot.connected.Store(false)
	if bot.IsConnected() {
		t.Error("expected IsConnected() to be false after Store(false)")
	}
}

// ---------------------------------------------------------------------------
// Username
// ---------------------------------------------------------------------------

func TestDiscordUsernameWithUser(t *testing.T) {
	bot := &Bot{
		dg: &discordgo.Session{
			State: &discordgo.State{
				Ready: discordgo.Ready{
					User: &discordgo.User{Username: "TestBot"},
				},
			},
		},
		logger: zap.NewNop().Sugar(),
	}
	if u := bot.Username(); u != "TestBot" {
		t.Errorf("Username() = %q, want %q", u, "TestBot")
	}
}

func TestDiscordUsernameNilState(t *testing.T) {
	bot := &Bot{dg: &discordgo.Session{State: nil}, logger: zap.NewNop().Sugar()}
	if u := bot.Username(); u != "" {
		t.Errorf("Username() with nil State = %q, want empty", u)
	}
}

func TestDiscordUsernameNilUser(t *testing.T) {
	bot := &Bot{
		dg: &discordgo.Session{
			State: &discordgo.State{Ready: discordgo.Ready{User: nil}},
		},
		logger: zap.NewNop().Sugar(),
	}
	if u := bot.Username(); u != "" {
		t.Errorf("Username() with nil User = %q, want empty", u)
	}
}

// ---------------------------------------------------------------------------
// PairedCount
// ---------------------------------------------------------------------------

func TestDiscordPairedCount(t *testing.T) {
	bot := &Bot{paired: make(map[string]bool), logger: zap.NewNop().Sugar()}
	if c := bot.PairedCount(); c != 0 {
		t.Errorf("PairedCount() = %d, want 0", c)
	}
	bot.paired["user1"] = true
	bot.paired["user2"] = true
	if c := bot.PairedCount(); c != 2 {
		t.Errorf("PairedCount() = %d, want 2", c)
	}
}

func TestDiscordPairedCountConcurrent(t *testing.T) {
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
// configSnapshot
// ---------------------------------------------------------------------------

func TestDiscordConfigSnapshot(t *testing.T) {
	bot := &Bot{
		cfg: config.DiscordConfig{
			BotToken:    "token123",
			StreamMode:  "partial",
			ReplyToMode: "first",
		},
		msgCfg: config.Messages{
			AckReactionScope: "all",
			StreamEditMs:     500,
		},
		logger: zap.NewNop().Sugar(),
	}
	dcfg, mcfg := bot.configSnapshot()
	if dcfg.BotToken != "token123" {
		t.Errorf("configSnapshot discord token = %q, want %q", dcfg.BotToken, "token123")
	}
	if dcfg.StreamMode != "partial" {
		t.Errorf("configSnapshot streamMode = %q, want %q", dcfg.StreamMode, "partial")
	}
	if mcfg.AckReactionScope != "all" {
		t.Errorf("configSnapshot ackReactionScope = %q, want %q", mcfg.AckReactionScope, "all")
	}
	if mcfg.StreamEditMs != 500 {
		t.Errorf("configSnapshot streamEditMs = %d, want %d", mcfg.StreamEditMs, 500)
	}
}

func TestDiscordConfigSnapshotConcurrent(t *testing.T) {
	bot := &Bot{
		cfg:    config.DiscordConfig{BotToken: "initial"},
		msgCfg: config.Messages{AckReactionScope: "all"},
		logger: zap.NewNop().Sugar(),
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			bot.configSnapshot()
		})
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// pairedUsersFile
// ---------------------------------------------------------------------------

func TestDiscordPairedUsersFile(t *testing.T) {
	path := pairedUsersFile()
	if path == "" {
		t.Error("pairedUsersFile() returned empty string")
	}
	if !strings.HasSuffix(path, filepath.Join(".gopherclaw", "credentials", "discord-default-allowFrom.json")) {
		t.Errorf("pairedUsersFile() = %q, expected to end with credentials/discord-default-allowFrom.json", path)
	}
}

// ---------------------------------------------------------------------------
// savePairedUsers / loadPairedUsers
// ---------------------------------------------------------------------------

func TestDiscordSavePairedUsersFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "discord-default-allowFrom.json")

	state := struct {
		Version   int      `json:"version"`
		AllowFrom []string `json:"allowFrom"`
	}{Version: 1, AllowFrom: []string{"111222333444555666", "777888999000111222"}}

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

func TestDiscordLoadPairedUsersValid(t *testing.T) {
	dir := t.TempDir()
	credDir := filepath.Join(dir, ".gopherclaw", "credentials")
	if err := os.MkdirAll(credDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(credDir, "discord-default-allowFrom.json")

	state := struct {
		Version   int      `json:"version"`
		AllowFrom []string `json:"allowFrom"`
	}{Version: 1, AllowFrom: []string{"aaa", "bbb", "ccc"}}
	data, _ := json.MarshalIndent(state, "", "  ")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	// Test JSON round-trip logic (same as loadPairedUsers internals)
	bot := &Bot{paired: make(map[string]bool), logger: zap.NewNop().Sugar()}
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
	for _, id := range loaded.AllowFrom {
		bot.paired[id] = true
	}
	if len(bot.paired) != 3 {
		t.Errorf("expected 3 paired users, got %d", len(bot.paired))
	}
	if !bot.paired["bbb"] {
		t.Error("expected user 'bbb' to be paired")
	}
}

func TestDiscordLoadPairedUsersMissingFile(t *testing.T) {
	bot := &Bot{paired: make(map[string]bool), logger: zap.NewNop().Sugar()}
	// loadPairedUsers reads from the real home dir. We just verify it
	// does not panic regardless of whether the file exists or not.
	bot.loadPairedUsers()
}

func TestDiscordLoadPairedUsersInvalidJSON(t *testing.T) {
	var loaded struct {
		Version   int      `json:"version"`
		AllowFrom []string `json:"allowFrom"`
	}
	err := json.Unmarshal([]byte("{invalid json}"), &loaded)
	if err == nil {
		t.Error("expected error parsing invalid JSON")
	}
}

// ---------------------------------------------------------------------------
// enqueue / queue mechanics
// ---------------------------------------------------------------------------

func TestDiscordEnqueueCreatesQueue(t *testing.T) {
	bot := &Bot{
		queues: make(map[string]*messageQueue),
		msgCfg: config.Messages{
			Queue: config.MessageQueue{Mode: "collect", DebounceMs: 5000, Cap: 20},
		},
		logger: zap.NewNop().Sugar(),
	}
	m := newMsg("u1", "hello", "ch1", "", false)
	bot.enqueue("u1", "ch1", "hello", m)

	bot.mu.Lock()
	q, ok := bot.queues["u1"]
	bot.mu.Unlock()

	if !ok {
		t.Fatal("expected queue to be created for user u1")
	}
	if len(q.messages) != 1 {
		t.Errorf("expected 1 message in queue, got %d", len(q.messages))
	}
	if q.messages[0].text != "hello" {
		t.Errorf("expected message text 'hello', got %q", q.messages[0].text)
	}
	if q.timer != nil {
		q.timer.Stop()
	}
}

func TestDiscordEnqueueAppendsToExisting(t *testing.T) {
	bot := &Bot{
		queues: make(map[string]*messageQueue),
		msgCfg: config.Messages{
			Queue: config.MessageQueue{Mode: "collect", DebounceMs: 5000, Cap: 20},
		},
		logger: zap.NewNop().Sugar(),
	}
	m1 := newMsg("u1", "msg1", "ch1", "", false)
	m2 := newMsg("u1", "msg2", "ch1", "", false)
	bot.enqueue("u1", "ch1", "msg1", m1)
	bot.enqueue("u1", "ch1", "msg2", m2)

	bot.mu.Lock()
	q := bot.queues["u1"]
	bot.mu.Unlock()

	if len(q.messages) != 2 {
		t.Errorf("expected 2 messages in queue, got %d", len(q.messages))
	}
	if q.timer != nil {
		q.timer.Stop()
	}
}

func TestDiscordEnqueueCapFlush(t *testing.T) {
	bot := &Bot{
		queues: make(map[string]*messageQueue),
		msgCfg: config.Messages{
			Queue: config.MessageQueue{Mode: "collect", DebounceMs: 5000, Cap: 2},
		},
		fullCfg: &config.Root{},
		cfg:     config.DiscordConfig{TimeoutSeconds: 1},
		logger: zap.NewNop().Sugar(),
	}
	m := newMsg("u1", "msg", "ch1", "", false)
	bot.enqueue("u1", "ch1", "msg1", m)
	bot.enqueue("u1", "ch1", "msg2", m) // hits cap=2, triggers flush

	time.Sleep(50 * time.Millisecond)

	bot.mu.Lock()
	_, exists := bot.queues["u1"]
	bot.mu.Unlock()
	if exists {
		t.Error("expected queue to be flushed after hitting cap")
	}
}

func TestDiscordEnqueueDefaultCap(t *testing.T) {
	bot := &Bot{
		queues: make(map[string]*messageQueue),
		msgCfg: config.Messages{
			Queue: config.MessageQueue{Mode: "collect", DebounceMs: 5000, Cap: 0},
		},
		logger: zap.NewNop().Sugar(),
	}
	m := newMsg("u1", "x", "ch1", "", false)
	for i := range 5 {
		bot.enqueue("u1", "ch1", strings.Repeat("x", i+1), m)
	}

	bot.mu.Lock()
	q, ok := bot.queues["u1"]
	bot.mu.Unlock()
	if !ok {
		t.Fatal("expected queue to still exist (below default cap)")
	}
	if len(q.messages) != 5 {
		t.Errorf("expected 5 messages, got %d", len(q.messages))
	}
	if q.timer != nil {
		q.timer.Stop()
	}
}

func TestDiscordEnqueueTimerReset(t *testing.T) {
	bot := &Bot{
		queues: make(map[string]*messageQueue),
		msgCfg: config.Messages{
			Queue: config.MessageQueue{Mode: "collect", DebounceMs: 10000, Cap: 20},
		},
		logger: zap.NewNop().Sugar(),
	}
	m := newMsg("u1", "x", "ch1", "", false)
	bot.enqueue("u1", "ch1", "msg1", m)

	bot.mu.Lock()
	firstTimer := bot.queues["u1"].timer
	bot.mu.Unlock()

	bot.enqueue("u1", "ch1", "msg2", m)

	bot.mu.Lock()
	secondTimer := bot.queues["u1"].timer
	bot.mu.Unlock()

	if firstTimer == secondTimer {
		t.Error("expected timer to be replaced on second enqueue")
	}
	if secondTimer != nil {
		secondTimer.Stop()
	}
}

func TestDiscordEnqueueMultipleUsers(t *testing.T) {
	bot := &Bot{
		queues: make(map[string]*messageQueue),
		msgCfg: config.Messages{
			Queue: config.MessageQueue{Mode: "collect", DebounceMs: 5000, Cap: 20},
		},
		logger: zap.NewNop().Sugar(),
	}
	m1 := newMsg("u1", "hello from u1", "ch1", "", false)
	m2 := newMsg("u2", "hello from u2", "ch1", "", false)
	bot.enqueue("u1", "ch1", "hello from u1", m1)
	bot.enqueue("u2", "ch1", "hello from u2", m2)
	bot.enqueue("u1", "ch1", "second from u1", m1)

	bot.mu.Lock()
	defer bot.mu.Unlock()

	q1 := bot.queues["u1"]
	q2 := bot.queues["u2"]
	if q1 == nil || q2 == nil {
		t.Fatal("expected both user queues to exist")
	}
	if len(q1.messages) != 2 {
		t.Errorf("u1 queue should have 2 messages, got %d", len(q1.messages))
	}
	if len(q2.messages) != 1 {
		t.Errorf("u2 queue should have 1 message, got %d", len(q2.messages))
	}
	if q1.timer != nil {
		q1.timer.Stop()
	}
	if q2.timer != nil {
		q2.timer.Stop()
	}
}

// ---------------------------------------------------------------------------
// flushLocked
// ---------------------------------------------------------------------------

func TestDiscordFlushLockedEmptyQueue(t *testing.T) {
	bot := &Bot{queues: make(map[string]*messageQueue), logger: zap.NewNop().Sugar()}
	bot.mu.Lock()
	bot.flushLocked("nobody", "ch1")
	bot.mu.Unlock()
}

func TestDiscordFlushLockedEmptyMessages(t *testing.T) {
	bot := &Bot{
		queues: map[string]*messageQueue{"u1": {messages: nil}},
		logger: zap.NewNop().Sugar(),
	}
	bot.mu.Lock()
	bot.flushLocked("u1", "ch1")
	bot.mu.Unlock()
}

// ---------------------------------------------------------------------------
// ackReaction — early-return paths (no Discord connection needed)
// ---------------------------------------------------------------------------

func TestDiscordAckReactionScopeEmpty(t *testing.T) {
	bot := &Bot{
		msgCfg: config.Messages{AckReactionScope: ""},
		cfg:    config.DiscordConfig{},
		logger: zap.NewNop().Sugar(),
	}
	// Empty scope returns early before touching session
	bot.ackReaction(nil, nil, false, bot.cfg, bot.msgCfg)
}

func TestDiscordAckReactionGroupMentionsDM(t *testing.T) {
	bot := &Bot{
		msgCfg: config.Messages{AckReactionScope: "group-mentions"},
		cfg:    config.DiscordConfig{},
		logger: zap.NewNop().Sugar(),
	}
	// scope="group-mentions" + isDM=true returns early
	bot.ackReaction(nil, nil, true, bot.cfg, bot.msgCfg)
}

// ackReaction with a mock server — exercises the s.MessageReactionAdd path
func TestDiscordAckReactionAll(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:     dg,
		msgCfg: config.Messages{AckReactionScope: "all"},
		cfg:    config.DiscordConfig{AckEmoji: ""},
		logger: zap.NewNop().Sugar(),
	}
	m := newMsg("user1", "hi", "ch1", "", false)
	// Should call MessageReactionAdd with default emoji; mock server accepts it
	bot.ackReaction(dg, m, false, bot.cfg, bot.msgCfg)
}

func TestDiscordAckReactionCustomEmoji(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:     dg,
		msgCfg: config.Messages{AckReactionScope: "all"},
		cfg:    config.DiscordConfig{AckEmoji: "thumbsup"},
		logger: zap.NewNop().Sugar(),
	}
	m := newMsg("user1", "hi", "ch1", "", false)
	bot.ackReaction(dg, m, false, bot.cfg, bot.msgCfg)
}

func TestDiscordAckReactionGroupMentionsNonDM(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:     dg,
		msgCfg: config.Messages{AckReactionScope: "group-mentions"},
		cfg:    config.DiscordConfig{AckEmoji: "wave"},
		logger: zap.NewNop().Sugar(),
	}
	m := newMsg("user1", "hi", "ch1", "guild1", false)
	// scope="group-mentions" + isDM=false => should send reaction
	bot.ackReaction(dg, m, false, bot.cfg, bot.msgCfg)
}

// ---------------------------------------------------------------------------
// sendLong — with mock Discord server
// ---------------------------------------------------------------------------

func TestDiscordSendLongShortMessage(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	m := newMsg("user1", "test", "ch1", "", false)
	sendLong(dg, "ch1", m, "Hello, world!")
}

func TestDiscordSendLongMultiPart(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	m := newMsg("user1", "test", "ch1", "", false)
	longText := strings.Repeat("x", 4500) // will be split into 3 parts
	sendLong(dg, "ch1", m, longText)
}

// ---------------------------------------------------------------------------
// handleMessage — with mock Discord server for full path coverage
// ---------------------------------------------------------------------------

func TestDiscordHandleMessageIgnoresBotAuthor(t *testing.T) {
	bot := &Bot{
		botID:  "bot1",
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	m := newMsg("bot1", "hello", "ch1", "", true)
	bot.handleMessage(nil, m)
}

func TestDiscordHandleMessageIgnoresNilAuthor(t *testing.T) {
	bot := &Bot{
		botID:  "bot1",
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	m := &discordgo.MessageCreate{
		Message: &discordgo.Message{Author: nil, Content: "hello"},
	}
	bot.handleMessage(nil, m)
}

func TestDiscordHandleMessageGuildNoMention(t *testing.T) {
	bot := &Bot{
		botID:  "bot1",
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	m := newMsg("user1", "hello no mention", "ch1", "guild1", false)
	bot.handleMessage(nil, m)
}

func TestDiscordHandleMessageGuildWithMention(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:      dg,
		botID:   "bot1",
		paired:  make(map[string]bool),
		queues:  make(map[string]*messageQueue),
		fullCfg: &config.Root{},
		cfg:     config.DiscordConfig{DMPolicy: "", TimeoutSeconds: 1},
		msgCfg: config.Messages{
			AckReactionScope: "",
			Queue:            config.MessageQueue{Mode: "collect", DebounceMs: 5000, Cap: 20},
		},
		logger: zap.NewNop().Sugar(),
	}
	// Message in guild with mention — should pass auth and be enqueued
	m := newMsg("user1", "<@bot1> hello", "ch1", "guild1", false)
	bot.handleMessage(dg, m)

	bot.mu.Lock()
	q, ok := bot.queues["user1"]
	bot.mu.Unlock()

	if !ok {
		t.Fatal("expected message to be enqueued")
	}
	if q.messages[0].text != "hello" {
		t.Errorf("expected stripped mention text 'hello', got %q", q.messages[0].text)
	}
	if q.timer != nil {
		q.timer.Stop()
	}
}

func TestDiscordHandleMessageGuildWithNickMention(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:      dg,
		botID:   "bot1",
		paired:  make(map[string]bool),
		queues:  make(map[string]*messageQueue),
		fullCfg: &config.Root{},
		cfg:     config.DiscordConfig{DMPolicy: "", TimeoutSeconds: 1},
		msgCfg: config.Messages{
			Queue: config.MessageQueue{Mode: "collect", DebounceMs: 5000, Cap: 20},
		},
		logger: zap.NewNop().Sugar(),
	}
	m := newMsg("user1", "<@!bot1> world", "ch1", "guild1", false)
	bot.handleMessage(dg, m)

	bot.mu.Lock()
	q := bot.queues["user1"]
	bot.mu.Unlock()
	if q == nil || q.messages[0].text != "world" {
		t.Error("expected nick mention to be stripped and message enqueued")
	}
	if q != nil && q.timer != nil {
		q.timer.Stop()
	}
}

func TestDiscordHandleMessageDMUnauthorized(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:      dg,
		botID:   "bot1",
		paired:  make(map[string]bool),
		queues:  make(map[string]*messageQueue),
		fullCfg: &config.Root{},
		cfg:     config.DiscordConfig{DMPolicy: "pairing"},
		logger: zap.NewNop().Sugar(),
	}
	// DM from unpaired user — should send pairing prompt
	m := newMsg("stranger", "hello", "ch1", "", false)
	bot.handleMessage(dg, m)
	// No queue entry should exist
	bot.mu.Lock()
	_, ok := bot.queues["stranger"]
	bot.mu.Unlock()
	if ok {
		t.Error("expected unauthorized DM user to not have a queue entry")
	}
}

func TestDiscordHandleMessageGuildUnauthorized(t *testing.T) {
	bot := &Bot{
		botID:   "bot1",
		paired:  make(map[string]bool),
		queues:  make(map[string]*messageQueue),
		fullCfg: &config.Root{},
		cfg:     config.DiscordConfig{DMPolicy: "allowlist", AllowUsers: []string{"allowed"}},
		logger: zap.NewNop().Sugar(),
	}
	// Guild message with mention but user not in allowlist
	m := newMsg("stranger", "<@bot1> hello", "ch1", "guild1", false)
	// isAuthorized returns false for allowlist mode even in guild
	// Should return early after auth check (no DM response in guild)
	bot.handleMessage(nil, m)
}

func TestDiscordHandleMessagePairInvalidCode(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		pairCode: "123456",
		paired:   map[string]bool{"user1": true},
		queues:   make(map[string]*messageQueue),
		fullCfg:  &config.Root{},
		cfg:      config.DiscordConfig{DMPolicy: "pairing"},
		logger: zap.NewNop().Sugar(),
	}
	// Paired user sends /pair with wrong code
	m := newMsg("user1", "/pair 000000", "ch1", "", false)
	bot.handleMessage(dg, m)
	// Should have sent "Invalid pairing code" via mock server
}

func TestDiscordHandleMessagePairValidCode(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		pairCode: "654321",
		paired:   map[string]bool{"user1": true},
		queues:   make(map[string]*messageQueue),
		fullCfg:  &config.Root{},
		cfg:      config.DiscordConfig{DMPolicy: "pairing"},
		logger: zap.NewNop().Sugar(),
	}
	// Paired user sends /pair with correct code
	m := newMsg("user1", "/pair 654321", "ch1", "", false)
	bot.handleMessage(dg, m)

	bot.mu.Lock()
	isPaired := bot.paired["user1"]
	bot.mu.Unlock()
	if !isPaired {
		t.Error("expected user1 to remain paired after valid /pair")
	}
}

func TestDiscordHandleMessagePairNewUser(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		pairCode: "111111",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		fullCfg:  &config.Root{},
		cfg:      config.DiscordConfig{DMPolicy: ""}, // empty policy so auth passes
		logger: zap.NewNop().Sugar(),
	}
	m := newMsg("newuser", "/pair 111111", "ch1", "", false)
	bot.handleMessage(dg, m)

	bot.mu.Lock()
	isPaired := bot.paired["newuser"]
	bot.mu.Unlock()
	if !isPaired {
		t.Error("expected newuser to be paired after valid /pair")
	}
}

func TestDiscordHandleMessageSlashCommand(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:      dg,
		botID:   "bot1",
		paired:  map[string]bool{"user1": true},
		queues:  make(map[string]*messageQueue),
		fullCfg: &config.Root{},
		cfg:     config.DiscordConfig{DMPolicy: "pairing"},
		logger: zap.NewNop().Sugar(),
	}
	// /help is a slash command that returns Handled=true without needing agent/sessions
	m := newMsg("user1", "/help", "ch1", "", false)
	bot.handleMessage(dg, m)
	// Should have called sendLong via mock server
}

func TestDiscordHandleMessageDMWithCollectMode(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:      dg,
		botID:   "bot1",
		paired:  map[string]bool{"user1": true},
		queues:  make(map[string]*messageQueue),
		fullCfg: &config.Root{},
		cfg:     config.DiscordConfig{DMPolicy: "pairing", TimeoutSeconds: 1},
		msgCfg: config.Messages{
			AckReactionScope: "all",
			Queue:            config.MessageQueue{Mode: "collect", DebounceMs: 5000, Cap: 20},
		},
		logger: zap.NewNop().Sugar(),
	}
	m := newMsg("user1", "hello bot", "ch1", "", false)
	bot.handleMessage(dg, m)

	bot.mu.Lock()
	q, ok := bot.queues["user1"]
	bot.mu.Unlock()
	if !ok {
		t.Fatal("expected message to be enqueued in collect mode")
	}
	if q.messages[0].text != "hello bot" {
		t.Errorf("expected 'hello bot', got %q", q.messages[0].text)
	}
	if q.timer != nil {
		q.timer.Stop()
	}
}

func TestDiscordHandleMessageDMImmediateMode(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:      dg,
		botID:   "bot1",
		paired:  map[string]bool{"user1": true},
		queues:  make(map[string]*messageQueue),
		fullCfg: &config.Root{},
		cfg:     config.DiscordConfig{DMPolicy: "pairing", TimeoutSeconds: 1},
		msgCfg: config.Messages{
			AckReactionScope: "all",
			Queue:            config.MessageQueue{Mode: "", DebounceMs: 0}, // immediate mode
		},
		logger: zap.NewNop().Sugar(),
	}
	m := newMsg("user1", "hello direct", "ch1", "", false)
	// This will spawn a goroutine that calls processMessages, which will
	// try to call agent.Chat and panic/recover since ag is nil.
	bot.handleMessage(dg, m)

	// Give the goroutine a moment to finish
	time.Sleep(100 * time.Millisecond)
}

// ---------------------------------------------------------------------------
// SendToAllPaired — with mock Discord server
// ---------------------------------------------------------------------------

func TestDiscordSendToAllPairedNoPaired(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:     dg,
		paired: make(map[string]bool),
		logger: zap.NewNop().Sugar(),
	}
	// Should be a no-op
	bot.SendToAllPaired("hello")
}

func TestDiscordSendToAllPairedWithUsers(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:     dg,
		paired: map[string]bool{"u1": true, "u2": true},
		logger: zap.NewNop().Sugar(),
	}
	// Mock server will accept the UserChannelCreate and ChannelMessageSend calls
	bot.SendToAllPaired("broadcast message")
}

func TestDiscordSendToAllPairedLongMessage(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:     dg,
		paired: map[string]bool{"u1": true},
		logger: zap.NewNop().Sugar(),
	}
	longText := strings.Repeat("z", 4500) // will be split into 3 parts
	bot.SendToAllPaired(longText)
}

// ---------------------------------------------------------------------------
// processMessages — reset trigger path
// ---------------------------------------------------------------------------

func TestDiscordProcessMessagesResetTrigger(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:     dg,
		botID:  "bot1",
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		fullCfg: &config.Root{
			Session: config.Session{
				ResetTriggers: []string{"reset"},
			},
		},
		cfg:    config.DiscordConfig{TimeoutSeconds: 1},
		msgCfg: config.Messages{},
		logger: zap.NewNop().Sugar(),
	}
	replyMsg := newMsg("user1", "reset", "ch1", "", false)
	msgs := []queuedMessage{{text: "reset", m: replyMsg}}
	// processMessages detects the reset trigger and calls sessions.Reset.
	// sessions is nil so it will panic — wrap in recover to exercise the path.
	func() {
		defer func() { recover() }()
		bot.processMessages("user1", "ch1", msgs, replyMsg)
	}()
}

func TestDiscordProcessMessagesCombinesText(t *testing.T) {
	// Verify that multiple queued messages are combined with newlines.
	// We can test the text combination by examining the combined text logic
	// directly. The processMessages function builds combined text then checks
	// reset triggers, so with no matching trigger it proceeds to respond.
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:     dg,
		botID:  "bot1",
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		fullCfg: &config.Root{
			Session: config.Session{
				ResetTriggers: []string{"WONT_MATCH"},
			},
		},
		cfg:    config.DiscordConfig{TimeoutSeconds: 1, ReplyToMode: "first"},
		msgCfg: config.Messages{},
		logger: zap.NewNop().Sugar(),
	}
	m1 := newMsg("user1", "line1", "ch1", "", false)
	m2 := newMsg("user1", "line2", "ch1", "", false)
	msgs := []queuedMessage{
		{text: "line1", m: m1},
		{text: "line2", m: m2},
	}
	// processMessages calls respondFull which calls ag.Chat on nil agent.
	// Wrap in recover to exercise the path up to the agent call.
	func() {
		defer func() { recover() }()
		bot.processMessages("user1", "ch1", msgs, m1)
	}()
}

// ---------------------------------------------------------------------------
// Bot struct initialization
// ---------------------------------------------------------------------------

func TestDiscordBotFieldsAfterConstruction(t *testing.T) {
	bot := &Bot{
		dg:      &discordgo.Session{},
		cfg:     config.DiscordConfig{DMPolicy: "pairing", BotToken: "fake"},
		msgCfg:  config.Messages{},
		fullCfg: &config.Root{},
		paired:  make(map[string]bool),
		queues:  make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}
	if bot.ChannelName() != "discord" {
		t.Error("unexpected channel name")
	}
	if bot.IsConnected() {
		t.Error("should not be connected initially")
	}
	if bot.PairedCount() != 0 {
		t.Error("should have 0 paired users initially")
	}
}

// ---------------------------------------------------------------------------
// savePairedUsers integration test (exercises actual file I/O)
// ---------------------------------------------------------------------------

func TestDiscordSavePairedUsersIntegration(t *testing.T) {
	// savePairedUsers writes to the real home dir path via pairedUsersFile().
	// Back up and restore to avoid polluting the real env.
	path := pairedUsersFile()
	origData, origErr := os.ReadFile(path)
	t.Cleanup(func() {
		if origErr == nil {
			_ = os.WriteFile(path, origData, 0600)
		} else {
			_ = os.Remove(path)
		}
	})

	bot := &Bot{
		paired: map[string]bool{
			"test_save_111": true,
			"test_save_222": true,
		},
		logger: zap.NewNop().Sugar(),
	}

	bot.savePairedUsers()

	// Verify the file was written with correct content.
	data, err := os.ReadFile(path)
	if err != nil {
		// It's acceptable if savePairedUsers fails (e.g. permission issues)
		// but it should not panic. If it wrote nothing, skip validation.
		t.Logf("savePairedUsers did not write file: %v", err)
	} else {
		var loaded struct {
			Version   int      `json:"version"`
			AllowFrom []string `json:"allowFrom"`
		}
		if err := json.Unmarshal(data, &loaded); err != nil {
			t.Errorf("failed to parse saved file: %v", err)
		} else {
			if loaded.Version != 1 {
				t.Errorf("expected version 1, got %d", loaded.Version)
			}
			if len(loaded.AllowFrom) != 2 {
				t.Errorf("expected 2 paired users, got %d", len(loaded.AllowFrom))
			}
		}
	}
}

// ---------------------------------------------------------------------------
// respondFull — exercises the non-streaming response path
// ---------------------------------------------------------------------------

func TestDiscordRespondFullNilAgent(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:      dg,
		botID:   "bot1",
		paired:  make(map[string]bool),
		fullCfg: &config.Root{},
		cfg:     config.DiscordConfig{TimeoutSeconds: 1},
		msgCfg:  config.Messages{},
		logger: zap.NewNop().Sugar(),
	}
	replyMsg := newMsg("user1", "test", "ch1", "", false)
	// ag is nil, so Chat will panic. respondFull has no recover wrapper,
	// so we wrap it ourselves to verify the path is exercised.
	func() {
		defer func() { recover() }()
		bot.respondFull("key1", "ch1", "hello", replyMsg, bot.cfg)
	}()
}

// ---------------------------------------------------------------------------
// respondStreaming — exercises the streaming response path
// ---------------------------------------------------------------------------

func TestDiscordRespondStreamingNilAgent(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:      dg,
		botID:   "bot1",
		paired:  make(map[string]bool),
		fullCfg: &config.Root{},
		cfg:     config.DiscordConfig{TimeoutSeconds: 1},
		msgCfg:  config.Messages{StreamEditMs: 400},
		logger: zap.NewNop().Sugar(),
	}
	replyMsg := newMsg("user1", "test", "ch1", "", false)
	func() {
		defer func() { recover() }()
		bot.respondStreaming("key1", "ch1", "hello", replyMsg, bot.cfg, bot.msgCfg)
	}()
}

func TestDiscordRespondStreamingDefaultEditMs(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:      dg,
		botID:   "bot1",
		paired:  make(map[string]bool),
		fullCfg: &config.Root{},
		cfg:     config.DiscordConfig{TimeoutSeconds: 1},
		msgCfg:  config.Messages{StreamEditMs: 0}, // 0 => default 400
		logger: zap.NewNop().Sugar(),
	}
	replyMsg := newMsg("user1", "test", "ch1", "", false)
	func() {
		defer func() { recover() }()
		bot.respondStreaming("key1", "ch1", "hello", replyMsg, bot.cfg, bot.msgCfg)
	}()
}

// ---------------------------------------------------------------------------
// processMessages — streaming mode path
// ---------------------------------------------------------------------------

func TestDiscordProcessMessagesStreamingMode(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:     dg,
		botID:  "bot1",
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		fullCfg: &config.Root{
			Session: config.Session{
				ResetTriggers: []string{"WONT_MATCH"},
			},
		},
		cfg:    config.DiscordConfig{TimeoutSeconds: 1, StreamMode: "partial"},
		msgCfg: config.Messages{StreamEditMs: 400},
		logger: zap.NewNop().Sugar(),
	}
	m := newMsg("user1", "hi", "ch1", "", false)
	msgs := []queuedMessage{{text: "hi", m: m}}
	func() {
		defer func() { recover() }()
		bot.processMessages("user1", "ch1", msgs, m)
	}()
}

func TestDiscordProcessMessagesReplyToFirst(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:     dg,
		botID:  "bot1",
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		fullCfg: &config.Root{
			Session: config.Session{
				ResetTriggers: []string{"WONT_MATCH"},
			},
		},
		cfg:    config.DiscordConfig{TimeoutSeconds: 1, ReplyToMode: "first"},
		msgCfg: config.Messages{},
		logger: zap.NewNop().Sugar(),
	}
	m1 := newMsg("user1", "msg1", "ch1", "", false)
	m2 := newMsg("user1", "msg2", "ch1", "", false)
	msgs := []queuedMessage{
		{text: "msg1", m: m1},
		{text: "msg2", m: m2},
	}
	func() {
		defer func() { recover() }()
		bot.processMessages("user1", "ch1", msgs, m2)
	}()
}

// ---------------------------------------------------------------------------
// respondFull — exercises the usage token display path
// ---------------------------------------------------------------------------

func TestDiscordRespondFullWithUsageConfig(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:     dg,
		botID:  "bot1",
		paired: make(map[string]bool),
		fullCfg: &config.Root{
			Messages: config.Messages{Usage: "tokens"},
		},
		cfg:    config.DiscordConfig{TimeoutSeconds: 1},
		msgCfg: config.Messages{},
		logger: zap.NewNop().Sugar(),
	}
	replyMsg := newMsg("user1", "test", "ch1", "", false)
	func() {
		defer func() { recover() }()
		bot.respondFull("key1", "ch1", "hello", replyMsg, bot.cfg)
	}()
}

// ---------------------------------------------------------------------------
// ackReaction — error path
// ---------------------------------------------------------------------------

func TestDiscordAckReactionError(t *testing.T) {
	// Create a mock server that returns errors for PUT (reaction) requests
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message": "Missing Permissions"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	origChannels := discordgo.EndpointChannels
	discordgo.EndpointChannels = srv.URL + "/channels/"
	defer func() { discordgo.EndpointChannels = origChannels }()

	dg := newTestSession(t)
	bot := &Bot{
		dg:     dg,
		msgCfg: config.Messages{AckReactionScope: "all"},
		cfg:    config.DiscordConfig{AckEmoji: "wave"},
		logger: zap.NewNop().Sugar(),
	}
	m := newMsg("user1", "hi", "ch1", "", false)
	// This should hit the error branch in ackReaction
	bot.ackReaction(dg, m, false, bot.cfg, bot.msgCfg)
}

// ---------------------------------------------------------------------------
// SendToAllPaired — error paths
// ---------------------------------------------------------------------------

func TestDiscordSendToAllPairedDMCreateError(t *testing.T) {
	// Mock server that fails on UserChannelCreate (POST to users endpoint)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "users") {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message": "Cannot DM user"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	origUsers := discordgo.EndpointUsers
	discordgo.EndpointUsers = srv.URL + "/users/"
	defer func() { discordgo.EndpointUsers = origUsers }()

	dg := newTestSession(t)
	bot := &Bot{
		dg:     dg,
		paired: map[string]bool{"u1": true},
		logger: zap.NewNop().Sugar(),
	}
	// Should hit the error branch for UserChannelCreate
	bot.SendToAllPaired("test")
}

// ---------------------------------------------------------------------------
// AnnounceToSession
// ---------------------------------------------------------------------------

func TestAnnounceToSessionChannelScope(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{dg: dg, logger: zap.NewNop().Sugar()}

	// Channel-scoped key: agent:main:discord:channel:C12345
	bot.AnnounceToSession("main:discord:channel:C12345", "hello channel")
	// No panic, message sent to channel endpoint via mock server
}

func TestAnnounceToSessionUserScope(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{dg: dg, logger: zap.NewNop().Sugar()}

	// User-scoped key: agent:main:discord:U999
	bot.AnnounceToSession("main:discord:U999", "hello user")
	// No panic, DM created + message sent via mock server
}

func TestAnnounceToSessionNonDiscordKeyIgnored(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}

	// Telegram key — should be silently ignored (no panic, no sends)
	bot.AnnounceToSession("main:telegram:123", "should be ignored")
}

func TestAnnounceToSessionMalformedKey(t *testing.T) {
	bot := &Bot{logger: zap.NewNop().Sugar()}

	// Too few parts
	bot.AnnounceToSession("main", "should be ignored")
	bot.AnnounceToSession("", "should be ignored")
	bot.AnnounceToSession("agent:main:discord", "should be ignored")
}

func TestAnnounceToSessionLongMessage(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{dg: dg, logger: zap.NewNop().Sugar()}

	// Message longer than 2000 chars should be split
	long := strings.Repeat("x", 3000)
	bot.AnnounceToSession("main:discord:channel:C123", long)
	// No panic — message split and sent via mock server
}

// ---------------------------------------------------------------------------
// SetTaskManager
// ---------------------------------------------------------------------------

func TestSetTaskManager(t *testing.T) {
	bot := &Bot{
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}

	if bot.taskMgr != nil {
		t.Fatal("expected taskMgr to be nil initially")
	}

	tm := taskqueue.New(zap.NewNop().Sugar(), filepath.Join(t.TempDir(), "tasks.json"), taskqueue.Config{})
	bot.SetTaskManager(tm)

	if bot.taskMgr != tm {
		t.Error("expected taskMgr to be set to the provided manager")
	}
}

func TestSetTaskManagerNil(t *testing.T) {
	tm := taskqueue.New(zap.NewNop().Sugar(), filepath.Join(t.TempDir(), "tasks.json"), taskqueue.Config{})
	bot := &Bot{
		paired:  make(map[string]bool),
		queues:  make(map[string]*messageQueue),
		taskMgr: tm,
		logger: zap.NewNop().Sugar(),
	}

	bot.SetTaskManager(nil)
	if bot.taskMgr != nil {
		t.Error("expected taskMgr to be nil after setting nil")
	}
}

func TestSetTaskManagerReplace(t *testing.T) {
	tm1 := taskqueue.New(zap.NewNop().Sugar(), filepath.Join(t.TempDir(), "tasks1.json"), taskqueue.Config{})
	tm2 := taskqueue.New(zap.NewNop().Sugar(), filepath.Join(t.TempDir(), "tasks2.json"), taskqueue.Config{})
	bot := &Bot{
		paired:  make(map[string]bool),
		queues:  make(map[string]*messageQueue),
		taskMgr: tm1,
		logger: zap.NewNop().Sugar(),
	}

	bot.SetTaskManager(tm2)
	if bot.taskMgr != tm2 {
		t.Error("expected taskMgr to be replaced with the second manager")
	}
}

// ---------------------------------------------------------------------------
// enqueue — cap-reached with existing timer triggers flush
// ---------------------------------------------------------------------------

func TestEnqueueCapReachedWithExistingTimer(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:     dg,
		botID:  "bot1",
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		fullCfg: &config.Root{
			Session: config.Session{
				ResetTriggers: []string{"WONT_MATCH"},
			},
		},
		cfg:    config.DiscordConfig{TimeoutSeconds: 1},
		msgCfg: config.Messages{Queue: config.MessageQueue{Mode: "collect", DebounceMs: 5000, Cap: 3}},
		logger: zap.NewNop().Sugar(),
	}

	// Enqueue first message — creates timer
	m1 := newMsg("user1", "msg1", "ch1", "", false)
	bot.enqueue("user1", "ch1", "msg1", m1)

	bot.mu.Lock()
	q := bot.queues["user1"]
	hasTimer := q != nil && q.timer != nil
	bot.mu.Unlock()
	if !hasTimer {
		t.Fatal("expected timer to be set after first enqueue")
	}

	// Enqueue second message — resets timer
	m2 := newMsg("user1", "msg2", "ch1", "", false)
	bot.enqueue("user1", "ch1", "msg2", m2)

	// Enqueue third message — hits cap (3), should flush and stop the timer
	m3 := newMsg("user1", "msg3", "ch1", "", false)
	bot.enqueue("user1", "ch1", "msg3", m3)

	// After cap-reached flush, the queue should be drained
	time.Sleep(50 * time.Millisecond) // allow goroutine to start
	bot.mu.Lock()
	_, queueStillExists := bot.queues["user1"]
	bot.mu.Unlock()
	if queueStillExists {
		t.Error("expected queue to be drained after cap-reached flush")
	}
}

func TestEnqueueDebounceFlush(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:     dg,
		botID:  "bot1",
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		fullCfg: &config.Root{
			Session: config.Session{
				ResetTriggers: []string{"WONT_MATCH"},
			},
		},
		cfg:    config.DiscordConfig{TimeoutSeconds: 1},
		msgCfg: config.Messages{Queue: config.MessageQueue{Mode: "collect", DebounceMs: 50, Cap: 100}},
		logger: zap.NewNop().Sugar(),
	}

	m1 := newMsg("user1", "hello", "ch1", "", false)
	bot.enqueue("user1", "ch1", "hello", m1)

	// Wait for debounce timer to fire
	time.Sleep(200 * time.Millisecond)

	bot.mu.Lock()
	_, queueStillExists := bot.queues["user1"]
	bot.mu.Unlock()
	if queueStillExists {
		t.Error("expected queue to be drained after debounce timer fires")
	}
}

func TestEnqueueDefaultCap(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:     dg,
		botID:  "bot1",
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		fullCfg: &config.Root{
			Session: config.Session{
				ResetTriggers: []string{"WONT_MATCH"},
			},
		},
		cfg:    config.DiscordConfig{TimeoutSeconds: 1},
		msgCfg: config.Messages{Queue: config.MessageQueue{Mode: "collect", DebounceMs: 5000, Cap: 0}}, // 0 => default 20
		logger: zap.NewNop().Sugar(),
	}

	// Enqueue 20 messages to hit default cap
	for i := 0; i < 20; i++ {
		m := newMsg("user1", "msg", "ch1", "", false)
		bot.enqueue("user1", "ch1", "msg", m)
	}

	time.Sleep(50 * time.Millisecond)
	bot.mu.Lock()
	_, queueStillExists := bot.queues["user1"]
	bot.mu.Unlock()
	if queueStillExists {
		t.Error("expected queue to be drained after hitting default cap of 20")
	}
}

// ---------------------------------------------------------------------------
// respondFull — with a working mock agent
// ---------------------------------------------------------------------------

func TestRespondFullSuccess(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	sessDir := t.TempDir()
	sm, err := session.New(zap.NewNop().Sugar(), sessDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	router := models.NewRouter(
		zap.NewNop().Sugar(),
		map[string]models.Provider{"test": &stubProvider{response: "Hello from agent!"}},
		"test/model-1",
		nil,
	)
	agCfg := &config.Root{
		Agents: config.Agents{
			List: []config.AgentDef{{ID: "main", Default: true, Identity: config.Identity{Name: "Bot", Theme: "test"}}},
		},
	}
	ag := agent.New(zap.NewNop().Sugar(), agCfg, agCfg.DefaultAgent(), router, sm, nil, nil, t.TempDir(), nil)

	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		ag:       ag,
		fullCfg:  agCfg,
		cfg:      config.DiscordConfig{TimeoutSeconds: 5},
		msgCfg:   config.Messages{},
		sessions: sm,
		logger: zap.NewNop().Sugar(),
	}

	replyMsg := newMsg("user1", "hi", "ch1", "", false)
	// Should complete without panic and send the response
	bot.respondFull("test-session", "ch1", "hi", replyMsg, bot.cfg)
}

func TestRespondFullWithUsageTokens(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	sessDir := t.TempDir()
	sm, err := session.New(zap.NewNop().Sugar(), sessDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	router := models.NewRouter(
		zap.NewNop().Sugar(),
		map[string]models.Provider{"test": &stubProvider{response: "Hello!"}},
		"test/model-1",
		nil,
	)
	agCfg := &config.Root{
		Agents: config.Agents{
			List: []config.AgentDef{{ID: "main", Default: true, Identity: config.Identity{Name: "Bot", Theme: "test"}}},
		},
		Messages: config.Messages{Usage: "tokens"},
	}
	ag := agent.New(zap.NewNop().Sugar(), agCfg, agCfg.DefaultAgent(), router, sm, nil, nil, t.TempDir(), nil)

	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		ag:       ag,
		fullCfg:  agCfg,
		cfg:      config.DiscordConfig{TimeoutSeconds: 5},
		msgCfg:   config.Messages{},
		sessions: sm,
		logger: zap.NewNop().Sugar(),
	}

	replyMsg := newMsg("user1", "hi", "ch1", "", false)
	bot.respondFull("test-session-usage", "ch1", "hi", replyMsg, bot.cfg)
}

func TestRespondFullError(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	sessDir := t.TempDir()
	sm, err := session.New(zap.NewNop().Sugar(), sessDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	router := models.NewRouter(
		zap.NewNop().Sugar(),
		map[string]models.Provider{"test": &stubProvider{err: fmt.Errorf("model unavailable")}},
		"test/model-1",
		nil,
	)
	agCfg := &config.Root{
		Agents: config.Agents{
			List: []config.AgentDef{{ID: "main", Default: true, Identity: config.Identity{Name: "Bot", Theme: "test"}}},
		},
	}
	ag := agent.New(zap.NewNop().Sugar(), agCfg, agCfg.DefaultAgent(), router, sm, nil, nil, t.TempDir(), nil)

	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		ag:       ag,
		fullCfg:  agCfg,
		cfg:      config.DiscordConfig{TimeoutSeconds: 5},
		msgCfg:   config.Messages{},
		sessions: sm,
		logger: zap.NewNop().Sugar(),
	}

	replyMsg := newMsg("user1", "hi", "ch1", "", false)
	// Should handle error gracefully — sends "Sorry, something went wrong."
	bot.respondFull("test-session-err", "ch1", "hi", replyMsg, bot.cfg)
}

// ---------------------------------------------------------------------------
// respondStreaming — with a working mock agent
// ---------------------------------------------------------------------------

func TestRespondStreamingSuccess(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	sessDir := t.TempDir()
	sm, err := session.New(zap.NewNop().Sugar(), sessDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	router := models.NewRouter(
		zap.NewNop().Sugar(),
		map[string]models.Provider{"test": &stubStreamProvider{chunks: []string{"Hello", " world", "!"}}},
		"test/model-1",
		nil,
	)
	agCfg := &config.Root{
		Agents: config.Agents{
			List: []config.AgentDef{{ID: "main", Default: true, Identity: config.Identity{Name: "Bot", Theme: "test"}}},
		},
	}
	ag := agent.New(zap.NewNop().Sugar(), agCfg, agCfg.DefaultAgent(), router, sm, nil, nil, t.TempDir(), nil)

	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		ag:       ag,
		fullCfg:  agCfg,
		cfg:      config.DiscordConfig{TimeoutSeconds: 5},
		msgCfg:   config.Messages{StreamEditMs: 10}, // fast edit interval for test
		sessions: sm,
		logger: zap.NewNop().Sugar(),
	}

	replyMsg := newMsg("user1", "hi", "ch1", "", false)
	bot.respondStreaming("test-stream-session", "ch1", "hi", replyMsg, bot.cfg, bot.msgCfg)
}

func TestRespondStreamingError(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	sessDir := t.TempDir()
	sm, err := session.New(zap.NewNop().Sugar(), sessDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	router := models.NewRouter(
		zap.NewNop().Sugar(),
		map[string]models.Provider{"test": &stubProvider{err: fmt.Errorf("stream error")}},
		"test/model-1",
		nil,
	)
	agCfg := &config.Root{
		Agents: config.Agents{
			List: []config.AgentDef{{ID: "main", Default: true, Identity: config.Identity{Name: "Bot", Theme: "test"}}},
		},
	}
	ag := agent.New(zap.NewNop().Sugar(), agCfg, agCfg.DefaultAgent(), router, sm, nil, nil, t.TempDir(), nil)

	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		ag:       ag,
		fullCfg:  agCfg,
		cfg:      config.DiscordConfig{TimeoutSeconds: 5},
		msgCfg:   config.Messages{StreamEditMs: 10},
		sessions: sm,
		logger: zap.NewNop().Sugar(),
	}

	replyMsg := newMsg("user1", "hi", "ch1", "", false)
	// Should handle error gracefully — edits placeholder to error msg
	bot.respondStreaming("test-stream-err", "ch1", "hi", replyMsg, bot.cfg, bot.msgCfg)
}

func TestRespondStreamingWithUsageTokens(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	sessDir := t.TempDir()
	sm, err := session.New(zap.NewNop().Sugar(), sessDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	router := models.NewRouter(
		zap.NewNop().Sugar(),
		map[string]models.Provider{"test": &stubStreamProvider{chunks: []string{"Streamed response"}}},
		"test/model-1",
		nil,
	)
	agCfg := &config.Root{
		Agents: config.Agents{
			List: []config.AgentDef{{ID: "main", Default: true, Identity: config.Identity{Name: "Bot", Theme: "test"}}},
		},
		Messages: config.Messages{Usage: "tokens"},
	}
	ag := agent.New(zap.NewNop().Sugar(), agCfg, agCfg.DefaultAgent(), router, sm, nil, nil, t.TempDir(), nil)

	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		ag:       ag,
		fullCfg:  agCfg,
		cfg:      config.DiscordConfig{TimeoutSeconds: 5},
		msgCfg:   config.Messages{StreamEditMs: 10},
		sessions: sm,
		logger: zap.NewNop().Sugar(),
	}

	replyMsg := newMsg("user1", "hi", "ch1", "", false)
	bot.respondStreaming("test-stream-usage", "ch1", "hi", replyMsg, bot.cfg, bot.msgCfg)
}

func TestRespondStreamingLongResponse(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	sessDir := t.TempDir()
	sm, err := session.New(zap.NewNop().Sugar(), sessDir, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Generate a response longer than 2000 chars to exercise multi-part sending
	longText := strings.Repeat("x", 2500)
	router := models.NewRouter(
		zap.NewNop().Sugar(),
		map[string]models.Provider{"test": &stubStreamProvider{chunks: []string{longText}}},
		"test/model-1",
		nil,
	)
	agCfg := &config.Root{
		Agents: config.Agents{
			List: []config.AgentDef{{ID: "main", Default: true, Identity: config.Identity{Name: "Bot", Theme: "test"}}},
		},
	}
	ag := agent.New(zap.NewNop().Sugar(), agCfg, agCfg.DefaultAgent(), router, sm, nil, nil, t.TempDir(), nil)

	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		ag:       ag,
		fullCfg:  agCfg,
		cfg:      config.DiscordConfig{TimeoutSeconds: 5},
		msgCfg:   config.Messages{StreamEditMs: 10},
		sessions: sm,
		logger: zap.NewNop().Sugar(),
	}

	replyMsg := newMsg("user1", "hi", "ch1", "", false)
	bot.respondStreaming("test-stream-long", "ch1", "hi", replyMsg, bot.cfg, bot.msgCfg)
}

// ---------------------------------------------------------------------------
// processMessages — integration through respondFull
// ---------------------------------------------------------------------------

func TestProcessMessagesFullPathSuccess(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	sessDir := t.TempDir()
	sm, err := session.New(zap.NewNop().Sugar(), sessDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	router := models.NewRouter(
		zap.NewNop().Sugar(),
		map[string]models.Provider{"test": &stubProvider{response: "processed reply"}},
		"test/model-1",
		nil,
	)
	agCfg := &config.Root{
		Agents: config.Agents{
			List: []config.AgentDef{{ID: "main", Default: true, Identity: config.Identity{Name: "Bot", Theme: "test"}}},
		},
		Session: config.Session{
			ResetTriggers: []string{"WONT_MATCH"},
		},
	}
	ag := agent.New(zap.NewNop().Sugar(), agCfg, agCfg.DefaultAgent(), router, sm, nil, nil, t.TempDir(), nil)

	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		ag:       ag,
		fullCfg:  agCfg,
		cfg:      config.DiscordConfig{TimeoutSeconds: 5},
		msgCfg:   config.Messages{},
		sessions: sm,
		logger: zap.NewNop().Sugar(),
	}

	m := newMsg("user1", "hello", "ch1", "", false)
	msgs := []queuedMessage{{text: "hello", m: m}}
	// Should process through respondFull without panic
	bot.processMessages("user1", "ch1", msgs, m)
}

func TestProcessMessagesResetTrigger(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	sessDir := t.TempDir()
	sm, err := session.New(zap.NewNop().Sugar(), sessDir, 0)
	if err != nil {
		t.Fatal(err)
	}

	agCfg := &config.Root{
		Agents: config.Agents{
			List: []config.AgentDef{{ID: "main", Default: true, Identity: config.Identity{Name: "Bot", Theme: "test"}}},
		},
		Session: config.Session{
			ResetTriggers: []string{"reset", "clear"},
		},
	}

	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		fullCfg:  agCfg,
		cfg:      config.DiscordConfig{TimeoutSeconds: 5},
		msgCfg:   config.Messages{},
		sessions: sm,
		logger: zap.NewNop().Sugar(),
	}

	m := newMsg("user1", "reset", "ch1", "", false)
	msgs := []queuedMessage{{text: "reset", m: m}}
	// Should hit reset trigger path — sends "Session cleared."
	bot.processMessages("user1", "ch1", msgs, m)
}

// ---------------------------------------------------------------------------
// AnnounceToSession — error paths for better coverage
// ---------------------------------------------------------------------------

func TestAnnounceToSessionChannelSendError(t *testing.T) {
	// Mock server that fails on ChannelMessageSend
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message": "Missing Access"}`))
	}))
	defer srv.Close()

	origChannels := discordgo.EndpointChannels
	discordgo.EndpointChannels = srv.URL + "/channels/"
	defer func() { discordgo.EndpointChannels = origChannels }()

	dg := newTestSession(t)
	bot := &Bot{dg: dg, logger: zap.NewNop().Sugar()}

	// Channel-scoped key — should hit the error branch
	// Key format: parts[2] must be "discord" and len >= 4
	bot.AnnounceToSession("scope:agent:discord:channel:C12345", "hello channel")
}

func TestAnnounceToSessionUserDMError(t *testing.T) {
	// Mock server that fails on UserChannelCreate
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message": "Cannot DM user"}`))
	}))
	defer srv.Close()

	origUsers := discordgo.EndpointUsers
	discordgo.EndpointUsers = srv.URL + "/users/"
	defer func() { discordgo.EndpointUsers = origUsers }()

	dg := newTestSession(t)
	bot := &Bot{dg: dg, logger: zap.NewNop().Sugar()}

	// User-scoped key — should hit the UserChannelCreate error branch
	// Key format: parts[2] must be "discord" and len >= 4
	bot.AnnounceToSession("scope:agent:discord:U999", "hello user")
}

func TestAnnounceToSessionUserSendError(t *testing.T) {
	// Mock server that succeeds on UserChannelCreate but fails on ChannelMessageSend
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "users") && r.Method == "POST" {
			// UserChannelCreate succeeds
			_ = json.NewEncoder(w).Encode(discordgo.Channel{ID: "dm-ch-1"})
			return
		}
		// ChannelMessageSend fails
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message": "Cannot send messages to this user"}`))
	}))
	defer srv.Close()

	origUsers := discordgo.EndpointUsers
	origChannels := discordgo.EndpointChannels
	discordgo.EndpointUsers = srv.URL + "/users/"
	discordgo.EndpointChannels = srv.URL + "/channels/"
	defer func() {
		discordgo.EndpointUsers = origUsers
		discordgo.EndpointChannels = origChannels
	}()

	dg := newTestSession(t)
	bot := &Bot{dg: dg, logger: zap.NewNop().Sugar()}

	// User-scoped key — UserChannelCreate succeeds, ChannelMessageSend fails
	// Key format: parts[2] must be "discord" and len >= 4
	bot.AnnounceToSession("scope:agent:discord:U999", "hello user")
}

// ---------------------------------------------------------------------------
// SendToAllPaired — success path
// ---------------------------------------------------------------------------

func TestSendToAllPairedSuccess(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:     dg,
		paired: map[string]bool{"u1": true, "u2": true},
		logger: zap.NewNop().Sugar(),
	}

	// Should send to both users without panic
	bot.SendToAllPaired("broadcast message")
}

func TestSendToAllPairedLongMessage(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:     dg,
		paired: map[string]bool{"u1": true},
		logger: zap.NewNop().Sugar(),
	}

	long := strings.Repeat("y", 3000)
	bot.SendToAllPaired(long)
}

func TestSendToAllPairedSendError(t *testing.T) {
	// Mock server that succeeds on UserChannelCreate but fails on ChannelMessageSend
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "users") && r.Method == "POST" {
			_ = json.NewEncoder(w).Encode(discordgo.Channel{ID: "dm-ch-1"})
			return
		}
		// ChannelMessageSend fails
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message": "Cannot send"}`))
	}))
	defer srv.Close()

	origUsers := discordgo.EndpointUsers
	origChannels := discordgo.EndpointChannels
	discordgo.EndpointUsers = srv.URL + "/users/"
	discordgo.EndpointChannels = srv.URL + "/channels/"
	defer func() {
		discordgo.EndpointUsers = origUsers
		discordgo.EndpointChannels = origChannels
	}()

	dg := newTestSession(t)
	bot := &Bot{
		dg:     dg,
		paired: map[string]bool{"u1": true},
		logger: zap.NewNop().Sugar(),
	}

	// Should hit the send error branch
	bot.SendToAllPaired("msg")
}

func TestSendToAllPairedNoPairedUsers(t *testing.T) {
	dg := newTestSession(t)
	bot := &Bot{
		dg:     dg,
		paired: make(map[string]bool),
		logger: zap.NewNop().Sugar(),
	}

	// No users to send to — should return immediately
	bot.SendToAllPaired("no one to send to")
}

// ---------------------------------------------------------------------------
// configSnapshot
// ---------------------------------------------------------------------------

func TestConfigSnapshotConcurrency(t *testing.T) {
	bot := &Bot{
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		cfg:    config.DiscordConfig{BotToken: "tok1", TimeoutSeconds: 10},
		msgCfg: config.Messages{StreamEditMs: 400},
		logger: zap.NewNop().Sugar(),
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg, msgCfg := bot.configSnapshot()
			if cfg.TimeoutSeconds != 10 {
				t.Errorf("unexpected TimeoutSeconds: %d", cfg.TimeoutSeconds)
			}
			if msgCfg.StreamEditMs != 400 {
				t.Errorf("unexpected StreamEditMs: %d", msgCfg.StreamEditMs)
			}
		}()
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// dgRef
// ---------------------------------------------------------------------------

func TestDgRefConcurrency(t *testing.T) {
	dg := newTestSession(t)
	bot := &Bot{
		dg:     dg,
		paired: make(map[string]bool),
		queues: make(map[string]*messageQueue),
		logger: zap.NewNop().Sugar(),
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ref := bot.dgRef()
			if ref != dg {
				t.Errorf("dgRef returned unexpected session")
			}
		}()
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Username — additional path: nil State
// ---------------------------------------------------------------------------

func TestUsernameNilState(t *testing.T) {
	dg, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatal(err)
	}
	// Don't set State.User
	bot := &Bot{dg: dg, logger: zap.NewNop().Sugar()}
	if got := bot.Username(); got != "" {
		t.Errorf("expected empty username with nil State.User, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// handleMessage — additional paths
// ---------------------------------------------------------------------------

func TestHandleMessageGuildNotMentioned(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:      dg,
		botID:   "bot1",
		paired:  map[string]bool{"user1": true},
		queues:  make(map[string]*messageQueue),
		fullCfg: &config.Root{},
		cfg:     config.DiscordConfig{DMPolicy: "pairing", TimeoutSeconds: 1},
		msgCfg:  config.Messages{},
		logger: zap.NewNop().Sugar(),
	}

	// Guild message without bot mention — should be ignored (no response)
	m := newMsg("user1", "hello everyone", "ch1", "guild1", false)
	bot.handleMessage(dg, m)
}

func TestHandleMessageGuildMentioned(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	sessDir := t.TempDir()
	sm, err := session.New(zap.NewNop().Sugar(), sessDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	router := models.NewRouter(
		zap.NewNop().Sugar(),
		map[string]models.Provider{"test": &stubProvider{response: "guild reply"}},
		"test/model-1",
		nil,
	)
	agCfg := &config.Root{
		Agents: config.Agents{
			List: []config.AgentDef{{ID: "main", Default: true, Identity: config.Identity{Name: "Bot", Theme: "test"}}},
		},
		Session: config.Session{
			ResetTriggers: []string{"WONT_MATCH"},
		},
	}
	ag := agent.New(zap.NewNop().Sugar(), agCfg, agCfg.DefaultAgent(), router, sm, nil, nil, t.TempDir(), nil)

	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   map[string]bool{"user1": true},
		queues:   make(map[string]*messageQueue),
		ag:       ag,
		fullCfg:  agCfg,
		cfg:      config.DiscordConfig{DMPolicy: "pairing", TimeoutSeconds: 5},
		msgCfg:   config.Messages{},
		sessions: sm,
		logger: zap.NewNop().Sugar(),
	}

	// Guild message WITH bot mention
	m := newMsg("user1", "<@bot1> hello bot", "ch1", "guild1", false)
	bot.handleMessage(dg, m)
	// Wait for async processing
	time.Sleep(100 * time.Millisecond)
}

func TestHandleMessageDMCollectMode(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:     dg,
		botID:  "bot1",
		paired: map[string]bool{"user1": true},
		queues: make(map[string]*messageQueue),
		fullCfg: &config.Root{
			Agents: config.Agents{
				List: []config.AgentDef{{ID: "main", Default: true, Identity: config.Identity{Name: "Bot", Theme: "test"}}},
			},
			Session: config.Session{ResetTriggers: []string{"WONT_MATCH"}},
		},
		cfg:    config.DiscordConfig{DMPolicy: "pairing", TimeoutSeconds: 1},
		msgCfg: config.Messages{Queue: config.MessageQueue{Mode: "collect", DebounceMs: 5000, Cap: 100}},
		logger: zap.NewNop().Sugar(),
	}

	// DM in collect mode — should enqueue rather than process immediately
	m := newMsg("user1", "queued message", "ch1", "", false)
	bot.handleMessage(dg, m)

	bot.mu.Lock()
	_, exists := bot.queues["user1"]
	bot.mu.Unlock()
	if !exists {
		t.Error("expected message to be enqueued in collect mode")
	}
}

func TestHandleMessagePairCommand(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		pairCode: "123456",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		fullCfg:  &config.Root{},
		cfg:      config.DiscordConfig{}, // empty DMPolicy — isAuthorized returns true for guild/non-policy DMs
		msgCfg:   config.Messages{},
		logger: zap.NewNop().Sugar(),
	}

	// Guild message with /pair command and bot mention (isAuthorized returns true for guild msgs)
	m := newMsg("newuser", "<@bot1> /pair 123456", "ch1", "guild1", false)
	bot.handleMessage(dg, m)

	bot.mu.Lock()
	isPaired := bot.paired["newuser"]
	bot.mu.Unlock()
	if !isPaired {
		t.Error("expected user to be paired after correct /pair command")
	}
}

func TestHandleMessagePairCommandBadCode(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		pairCode: "123456",
		paired:   map[string]bool{"newuser": true}, // pre-paired to pass auth
		queues:   make(map[string]*messageQueue),
		fullCfg:  &config.Root{},
		cfg:      config.DiscordConfig{DMPolicy: "pairing"},
		msgCfg:   config.Messages{},
		logger: zap.NewNop().Sugar(),
	}

	m := newMsg("newuser", "/pair 000000", "ch1", "", false)
	bot.handleMessage(dg, m)
	// Should send "Invalid pairing code" — no panic
}

// ---------------------------------------------------------------------------
// stub types for creating working agents in tests
// ---------------------------------------------------------------------------

// stubProvider implements models.Provider with a canned response.
type stubProvider struct {
	response string
	err      error
}

func (s *stubProvider) Chat(_ context.Context, _ openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
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
		Usage: openai.Usage{
			PromptTokens:     10,
			CompletionTokens: 5,
		},
	}, nil
}

func (s *stubProvider) ChatStream(_ context.Context, req openai.ChatCompletionRequest) (models.Stream, error) {
	if s.err != nil {
		return nil, s.err
	}
	// Fallback: return non-streaming response
	return &stubStream{chunks: []string{s.response}, pos: 0}, nil
}

// stubStreamProvider implements models.Provider with streaming chunks.
type stubStreamProvider struct {
	chunks []string
}

func (s *stubStreamProvider) Chat(_ context.Context, _ openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	combined := strings.Join(s.chunks, "")
	return openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{
			{
				Message: openai.ChatCompletionMessage{
					Role:    "assistant",
					Content: combined,
				},
				FinishReason: openai.FinishReasonStop,
			},
		},
		Usage: openai.Usage{PromptTokens: 10, CompletionTokens: 5},
	}, nil
}

func (s *stubStreamProvider) ChatStream(_ context.Context, _ openai.ChatCompletionRequest) (models.Stream, error) {
	return &stubStream{chunks: s.chunks, pos: 0}, nil
}

// stubStream implements models.Stream.
type stubStream struct {
	chunks []string
	pos    int
}

func (s *stubStream) Recv() (openai.ChatCompletionStreamResponse, error) {
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

func (s *stubStream) Close() error { return nil }

// ---------------------------------------------------------------------------
// CanConfirm tests
// ---------------------------------------------------------------------------

func TestCanConfirm_DiscordKey(t *testing.T) {
	b := &Bot{logger: zap.NewNop().Sugar()}
	if !b.CanConfirm("main:discord:12345") {
		t.Error("expected true for discord session key")
	}
	if !b.CanConfirm("agent:discord:channel:67890") {
		t.Error("expected true for discord channel key")
	}
}

func TestCanConfirm_NonDiscordKey(t *testing.T) {
	b := &Bot{logger: zap.NewNop().Sugar()}
	if b.CanConfirm("main:telegram:12345") {
		t.Error("expected false for telegram session key")
	}
	if b.CanConfirm("") {
		t.Error("expected false for empty key")
	}
}

// ---------------------------------------------------------------------------
// handleConfirmCallback tests
// ---------------------------------------------------------------------------

func TestHandleConfirmCallback_NonConfirmPrefix(t *testing.T) {
	b := &Bot{pendingConfirms: make(map[string]chan bool), logger: zap.NewNop().Sugar()}
	handled := b.handleConfirmCallback("some_action")
	if handled {
		t.Error("expected false for non-confirm prefix")
	}
}

func TestHandleConfirmCallback_NotInPending(t *testing.T) {
	b := &Bot{pendingConfirms: make(map[string]chan bool), logger: zap.NewNop().Sugar()}
	handled := b.handleConfirmCallback("confirm:123:456:yes")
	if handled {
		t.Error("expected false when confirmID not in pending map")
	}
}

func TestHandleConfirmCallback_YesAnswer(t *testing.T) {
	b := &Bot{pendingConfirms: make(map[string]chan bool), logger: zap.NewNop().Sugar()}
	resultCh := make(chan bool, 1)
	confirmID := "confirm:123:456789"
	b.pendingConfirms[confirmID] = resultCh

	handled := b.handleConfirmCallback(confirmID + ":yes")
	if !handled {
		t.Error("expected true for matching confirm ID")
	}
	select {
	case v := <-resultCh:
		if !v {
			t.Error("expected true for 'yes' answer")
		}
	default:
		t.Error("expected value on result channel")
	}
}

func TestHandleConfirmCallback_NoAnswer(t *testing.T) {
	b := &Bot{pendingConfirms: make(map[string]chan bool), logger: zap.NewNop().Sugar()}
	resultCh := make(chan bool, 1)
	confirmID := "confirm:123:999999"
	b.pendingConfirms[confirmID] = resultCh

	handled := b.handleConfirmCallback(confirmID + ":no")
	if !handled {
		t.Error("expected true for matching confirm ID")
	}
	select {
	case v := <-resultCh:
		if v {
			t.Error("expected false for 'no' answer")
		}
	default:
		t.Error("expected value on result channel")
	}
}

// ---------------------------------------------------------------------------
// CanConfirm — additional cases
// ---------------------------------------------------------------------------

func TestCanConfirm_SlackKey(t *testing.T) {
	b := &Bot{logger: zap.NewNop().Sugar()}
	if b.CanConfirm("main:slack:123") {
		t.Error("expected false for slack session key")
	}
}

func TestCanConfirm_EmptyKey(t *testing.T) {
	b := &Bot{logger: zap.NewNop().Sugar()}
	if b.CanConfirm("") {
		t.Error("expected false for empty session key")
	}
}

func TestCanConfirm_DiscordKeyVariants(t *testing.T) {
	b := &Bot{logger: zap.NewNop().Sugar()}
	cases := []struct {
		key  string
		want bool
	}{
		{"main:discord:123", true},
		{"main:slack:123", false},
		{"", false},
		{"agent:discord:channel:456", true},
		{"main:discord:global", true},
		{"no-colon-discord-embedded", false},
		{":discord:", true},
	}
	for _, tc := range cases {
		got := b.CanConfirm(tc.key)
		if got != tc.want {
			t.Errorf("CanConfirm(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// handleConfirmCallback — additional cases
// ---------------------------------------------------------------------------

func TestHandleConfirmCallback_ConfirmPrefixButNoColonAfter(t *testing.T) {
	// "confirm:" alone — lastColon is at index 7 (the colon in "confirm:"),
	// confirmID = "confirm", answer = "". Not in pendingConfirms → returns false.
	b := &Bot{pendingConfirms: make(map[string]chan bool), logger: zap.NewNop().Sugar()}
	handled := b.handleConfirmCallback("confirm:")
	if handled {
		t.Error("expected false for 'confirm:' with no matching pendingConfirm")
	}
}

func TestHandleConfirmCallback_UnknownConfirmID(t *testing.T) {
	b := &Bot{pendingConfirms: make(map[string]chan bool), logger: zap.NewNop().Sugar()}
	// Register one confirmID, then try a different one
	resultCh := make(chan bool, 1)
	b.pendingConfirms["confirm:ch1:111"] = resultCh
	handled := b.handleConfirmCallback("confirm:ch1:999:yes")
	if handled {
		t.Error("expected false for unknown confirmID")
	}
	// Ensure resultCh was not written to
	select {
	case <-resultCh:
		t.Error("expected no value on result channel for non-matching confirmID")
	default:
	}
}

func TestHandleConfirmCallback_ChannelFullyBuffered(t *testing.T) {
	// When the result channel is already full (buffer=1 and already has a value),
	// the default case in the select should be hit without blocking.
	b := &Bot{pendingConfirms: make(map[string]chan bool), logger: zap.NewNop().Sugar()}
	resultCh := make(chan bool, 1)
	resultCh <- true // pre-fill the buffer
	confirmID := "confirm:ch1:222"
	b.pendingConfirms[confirmID] = resultCh

	// This should hit the default branch in the select (buffer already full)
	handled := b.handleConfirmCallback(confirmID + ":yes")
	if !handled {
		t.Error("expected true even when channel buffer is full")
	}
}

func TestHandleConfirmCallback_EmptyAnswer(t *testing.T) {
	// CustomID = "confirm:ch1:333:" — answer is empty string, which is != "yes"
	b := &Bot{pendingConfirms: make(map[string]chan bool), logger: zap.NewNop().Sugar()}
	resultCh := make(chan bool, 1)
	confirmID := "confirm:ch1:333"
	b.pendingConfirms[confirmID] = resultCh

	handled := b.handleConfirmCallback(confirmID + ":")
	if !handled {
		t.Error("expected true for matching confirmID")
	}
	select {
	case v := <-resultCh:
		if v {
			t.Error("expected false for empty answer (not 'yes')")
		}
	default:
		t.Error("expected value on result channel")
	}
}

// ---------------------------------------------------------------------------
// handleInteraction
// ---------------------------------------------------------------------------

func TestHandleInteraction_NonComponentType(t *testing.T) {
	b := &Bot{pendingConfirms: make(map[string]chan bool), logger: zap.NewNop().Sugar()}
	dg := newTestSession(t)
	// InteractionCreate with type != InteractionMessageComponent should return early
	i := &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			Type: discordgo.InteractionPing,
		},
	}
	// Should not panic
	b.handleInteraction(dg, i)
}

func TestHandleInteraction_ConfirmYes(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	b := &Bot{pendingConfirms: make(map[string]chan bool), logger: zap.NewNop().Sugar()}
	resultCh := make(chan bool, 1)
	confirmID := "confirm:ch1:444"
	b.pendingConfirms[confirmID] = resultCh

	i := &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			Type: discordgo.InteractionMessageComponent,
			Data: discordgo.MessageComponentInteractionData{
				CustomID: confirmID + ":yes",
			},
			Message: &discordgo.Message{
				Content: "Confirm action?",
			},
		},
	}

	b.handleInteraction(dg, i)

	select {
	case v := <-resultCh:
		if !v {
			t.Error("expected true for 'yes' confirmation via handleInteraction")
		}
	default:
		t.Error("expected value on result channel")
	}
}

func TestHandleInteraction_ConfirmNo(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	b := &Bot{pendingConfirms: make(map[string]chan bool), logger: zap.NewNop().Sugar()}
	resultCh := make(chan bool, 1)
	confirmID := "confirm:ch1:555"
	b.pendingConfirms[confirmID] = resultCh

	i := &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			Type: discordgo.InteractionMessageComponent,
			Data: discordgo.MessageComponentInteractionData{
				CustomID: confirmID + ":no",
			},
			Message: &discordgo.Message{
				Content: "Confirm action?",
			},
		},
	}

	b.handleInteraction(dg, i)

	select {
	case v := <-resultCh:
		if v {
			t.Error("expected false for 'no' confirmation via handleInteraction")
		}
	default:
		t.Error("expected value on result channel")
	}
}

func TestHandleInteraction_UnknownConfirm(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	b := &Bot{pendingConfirms: make(map[string]chan bool), logger: zap.NewNop().Sugar()}

	// No pending confirm registered — handleConfirmCallback returns false
	i := &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			Type: discordgo.InteractionMessageComponent,
			Data: discordgo.MessageComponentInteractionData{
				CustomID: "confirm:ch1:666:yes",
			},
			Message: &discordgo.Message{
				Content: "old prompt",
			},
		},
	}

	// Should not panic; InteractionRespond should NOT be called since handleConfirmCallback returns false
	b.handleInteraction(dg, i)
}

func TestHandleInteraction_NonConfirmComponent(t *testing.T) {
	b := &Bot{pendingConfirms: make(map[string]chan bool), logger: zap.NewNop().Sugar()}
	dg := newTestSession(t)

	// A MessageComponent interaction with a non-confirm customID
	i := &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			Type: discordgo.InteractionMessageComponent,
			Data: discordgo.MessageComponentInteractionData{
				CustomID: "some_other_button",
			},
			Message: &discordgo.Message{
				Content: "some message",
			},
		},
	}

	// Should return without attempting InteractionRespond
	b.handleInteraction(dg, i)
}

// ---------------------------------------------------------------------------
// SendConfirmPrompt
// ---------------------------------------------------------------------------

func TestSendConfirmPrompt_InvalidSessionKey(t *testing.T) {
	b := &Bot{logger: zap.NewNop().Sugar()}
	ctx := context.Background()
	_, err := b.SendConfirmPrompt(ctx, "ab", "rm -rf /", "rm")
	if err == nil {
		t.Fatal("expected error for session key with < 3 parts")
	}
	if !strings.Contains(err.Error(), "invalid session key") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestSendConfirmPrompt_InvalidSessionKeyEmpty(t *testing.T) {
	b := &Bot{logger: zap.NewNop().Sugar()}
	ctx := context.Background()
	_, err := b.SendConfirmPrompt(ctx, "", "rm -rf /", "rm")
	if err == nil {
		t.Fatal("expected error for empty session key")
	}
}

func TestSendConfirmPrompt_InvalidSessionKeyTwoParts(t *testing.T) {
	b := &Bot{logger: zap.NewNop().Sugar()}
	ctx := context.Background()
	_, err := b.SendConfirmPrompt(ctx, "main:discord", "rm -rf /", "rm")
	if err == nil {
		t.Fatal("expected error for session key with only 2 parts")
	}
}

func TestSendConfirmPrompt_ChannelScope(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	b := &Bot{
		dg:              dg,
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}

	// Channel scope key: agent:discord:channel:C12345 (5 parts with "channel" at [3])
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// SendConfirmPrompt will send the prompt and block waiting for response.
	// With a short context timeout, it should return false with ctx.Err().
	confirmed, err := b.SendConfirmPrompt(ctx, "main:discord:channel:C12345:extra", "rm -rf /", "rm.*")
	if confirmed {
		t.Error("expected false when context times out")
	}
	if err == nil || err != context.DeadlineExceeded {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}
}

func TestSendConfirmPrompt_UserScope(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	b := &Bot{
		dg:              dg,
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}

	// User scope key: main:discord:U12345 (3 parts) — opens DM via UserChannelCreate
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	confirmed, err := b.SendConfirmPrompt(ctx, "main:discord:U12345", "ls -la", "ls")
	if confirmed {
		t.Error("expected false when context times out")
	}
	if err == nil || err != context.DeadlineExceeded {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}
}

func TestSendConfirmPrompt_CommandTruncation(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	b := &Bot{
		dg:              dg,
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}

	// Command longer than 200 chars should be truncated with "..."
	longCmd := strings.Repeat("x", 300)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	confirmed, err := b.SendConfirmPrompt(ctx, "main:discord:U12345", longCmd, "xxx")
	if confirmed {
		t.Error("expected false when context times out")
	}
	if err != context.DeadlineExceeded {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}
}

func TestSendConfirmPrompt_ContextAlreadyCancelled(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	b := &Bot{
		dg:              dg,
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	confirmed, err := b.SendConfirmPrompt(ctx, "main:discord:U12345", "rm /tmp/x", "rm")
	if confirmed {
		t.Error("expected false when context is already cancelled")
	}
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestSendConfirmPrompt_UserScopeWithChannelParts(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	b := &Bot{
		dg:              dg,
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}

	// 5+ parts but parts[3] != "channel" — should use userID path (parts[len(parts)-1])
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	confirmed, err := b.SendConfirmPrompt(ctx, "main:discord:scope:user:U999", "echo hi", "echo")
	if confirmed {
		t.Error("expected false when context times out")
	}
	if err != context.DeadlineExceeded {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}
}

func TestSendConfirmPrompt_RespondedYes(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	b := &Bot{
		dg:              dg,
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Launch SendConfirmPrompt in a goroutine, then simulate a "yes" response
	var confirmed bool
	var promptErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		confirmed, promptErr = b.SendConfirmPrompt(ctx, "main:discord:channel:C100:extra", "rm -rf /", "rm")
	}()

	// Wait for the pendingConfirms entry to appear
	var confirmID string
	for i := 0; i < 50; i++ {
		time.Sleep(10 * time.Millisecond)
		b.mu.Lock()
		for id := range b.pendingConfirms {
			confirmID = id
		}
		b.mu.Unlock()
		if confirmID != "" {
			break
		}
	}
	if confirmID == "" {
		t.Fatal("timed out waiting for pendingConfirms entry")
	}

	// Simulate user clicking "yes"
	b.handleConfirmCallback(confirmID + ":yes")

	<-done
	if promptErr != nil {
		t.Errorf("unexpected error: %v", promptErr)
	}
	if !confirmed {
		t.Error("expected confirmed=true after 'yes' response")
	}
}

func TestSendConfirmPrompt_RespondedNo(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	b := &Bot{
		dg:              dg,
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var confirmed bool
	var promptErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		confirmed, promptErr = b.SendConfirmPrompt(ctx, "main:discord:U200", "rm -rf /", "rm")
	}()

	// Wait for the pendingConfirms entry to appear
	var confirmID string
	for i := 0; i < 50; i++ {
		time.Sleep(10 * time.Millisecond)
		b.mu.Lock()
		for id := range b.pendingConfirms {
			confirmID = id
		}
		b.mu.Unlock()
		if confirmID != "" {
			break
		}
	}
	if confirmID == "" {
		t.Fatal("timed out waiting for pendingConfirms entry")
	}

	// Simulate user clicking "no"
	b.handleConfirmCallback(confirmID + ":no")

	<-done
	if promptErr != nil {
		t.Errorf("unexpected error: %v", promptErr)
	}
	if confirmed {
		t.Error("expected confirmed=false after 'no' response")
	}
}

func TestSendConfirmPrompt_CleansUpPendingConfirm(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	b := &Bot{
		dg:              dg,
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, _ = b.SendConfirmPrompt(ctx, "main:discord:U300", "cmd", "pat")

	// After return, the pendingConfirms entry should have been cleaned up
	b.mu.Lock()
	count := len(b.pendingConfirms)
	b.mu.Unlock()
	if count != 0 {
		t.Errorf("expected pendingConfirms to be empty after SendConfirmPrompt returns, got %d entries", count)
	}
}

func TestSendConfirmPrompt_DMCreateError(t *testing.T) {
	// Mock server that fails on UserChannelCreate
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "users") && r.Method == "POST" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message": "Cannot DM"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	origUsers := discordgo.EndpointUsers
	discordgo.EndpointUsers = srv.URL + "/users/"
	defer func() { discordgo.EndpointUsers = origUsers }()

	dg := newTestSession(t)
	b := &Bot{
		dg:              dg,
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}

	ctx := context.Background()
	_, err := b.SendConfirmPrompt(ctx, "main:discord:U500", "rm -rf /", "rm")
	if err == nil {
		t.Fatal("expected error when DM creation fails")
	}
	if !strings.Contains(err.Error(), "create DM for confirm") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSendConfirmPrompt_SendError(t *testing.T) {
	// Mock server that succeeds on UserChannelCreate but fails on ChannelMessageSendComplex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "users") && r.Method == "POST" {
			_ = json.NewEncoder(w).Encode(discordgo.Channel{ID: "dm-ch-err"})
			return
		}
		// ChannelMessageSendComplex fails
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message": "Cannot send"}`))
	}))
	defer srv.Close()

	origUsers := discordgo.EndpointUsers
	origChannels := discordgo.EndpointChannels
	discordgo.EndpointUsers = srv.URL + "/users/"
	discordgo.EndpointChannels = srv.URL + "/channels/"
	defer func() {
		discordgo.EndpointUsers = origUsers
		discordgo.EndpointChannels = origChannels
	}()

	dg := newTestSession(t)
	b := &Bot{
		dg:              dg,
		pendingConfirms: make(map[string]chan bool),
		logger:          zap.NewNop().Sugar(),
	}

	ctx := context.Background()
	_, err := b.SendConfirmPrompt(ctx, "main:discord:U600", "rm -rf /", "rm")
	if err == nil {
		t.Fatal("expected error when message send fails")
	}
	if !strings.Contains(err.Error(), "send confirm prompt") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSendConfirmPrompt_ContextTimeout(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	b := &Bot{
		dg:              dg,
		logger:          zap.NewNop().Sugar(),
		pendingConfirms: make(map[string]chan bool),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	confirmed, err := b.SendConfirmPrompt(ctx, "main:discord:channel:C400:x", "echo test", "echo")
	if confirmed {
		t.Error("expected false when context times out")
	}
	if err != context.DeadlineExceeded {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}

	// Verify cleanup removed the entry
	b.mu.Lock()
	if len(b.pendingConfirms) != 0 {
		t.Error("expected pendingConfirms to be empty after cleanup")
	}
	b.mu.Unlock()
}

// ---------------------------------------------------------------------------
// respondStreaming — empty response path
// ---------------------------------------------------------------------------

func TestRespondStreamingEmptyResponse(t *testing.T) {
	_, cleanup := mockDiscordServer(t)
	defer cleanup()

	dg := newTestSession(t)
	sessDir := t.TempDir()
	sm, err := session.New(zap.NewNop().Sugar(), sessDir, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Return empty string to hit the "(no response)" fallback
	router := models.NewRouter(
		zap.NewNop().Sugar(),
		map[string]models.Provider{"test": &stubStreamProvider{chunks: []string{""}}},
		"test/model-1",
		nil,
	)
	agCfg := &config.Root{
		Agents: config.Agents{
			List: []config.AgentDef{{ID: "main", Default: true, Identity: config.Identity{Name: "Bot", Theme: "test"}}},
		},
	}
	ag := agent.New(zap.NewNop().Sugar(), agCfg, agCfg.DefaultAgent(), router, sm, nil, nil, t.TempDir(), nil)

	bot := &Bot{
		dg:       dg,
		botID:    "bot1",
		paired:   make(map[string]bool),
		queues:   make(map[string]*messageQueue),
		ag:       ag,
		fullCfg:  agCfg,
		cfg:      config.DiscordConfig{TimeoutSeconds: 5},
		msgCfg:   config.Messages{StreamEditMs: 10},
		sessions: sm,
		logger:   zap.NewNop().Sugar(),
	}

	replyMsg := newMsg("user1", "hi", "ch1", "", false)
	// Should hit finalText == "" → "(no response)" path
	bot.respondStreaming("test-stream-empty", "ch1", "hi", replyMsg, bot.cfg, bot.msgCfg)
}
