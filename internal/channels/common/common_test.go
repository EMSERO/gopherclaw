package common

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// testLogger returns a no-op sugared logger suitable for unit tests.
func testLogger() *zap.SugaredLogger { return zap.NewNop().Sugar() }

// ---------------------------------------------------------------------------
// GeneratePairCode
// ---------------------------------------------------------------------------

func TestGeneratePairCode_Length(t *testing.T) {
	code := GeneratePairCode()
	if len(code) != 6 {
		t.Fatalf("expected 6-character code, got %q (len %d)", code, len(code))
	}
}

func TestGeneratePairCode_Numeric(t *testing.T) {
	code := GeneratePairCode()
	if _, err := strconv.Atoi(code); err != nil {
		t.Fatalf("expected numeric code, got %q: %v", code, err)
	}
}

func TestGeneratePairCode_Unique(t *testing.T) {
	// Generate many codes and verify we get at least some unique values.
	// Statistically, collisions among 20 random 6-digit codes are very rare.
	seen := make(map[string]bool)
	for i := 0; i < 20; i++ {
		seen[GeneratePairCode()] = true
	}
	if len(seen) < 2 {
		t.Fatalf("expected multiple unique codes, got %d distinct values", len(seen))
	}
}

// ---------------------------------------------------------------------------
// MatchesResetTrigger
// ---------------------------------------------------------------------------

func TestMatchesResetTrigger_ExactMatch(t *testing.T) {
	triggers := []string{"reset", "clear", "start over"}
	if !MatchesResetTrigger("reset", triggers) {
		t.Fatal("expected match for exact trigger")
	}
}

func TestMatchesResetTrigger_CaseInsensitive(t *testing.T) {
	triggers := []string{"reset", "clear"}
	if !MatchesResetTrigger("RESET", triggers) {
		t.Fatal("expected case-insensitive match")
	}
	if !MatchesResetTrigger("Clear", triggers) {
		t.Fatal("expected case-insensitive match for 'Clear'")
	}
}

func TestMatchesResetTrigger_TrimWhitespace(t *testing.T) {
	triggers := []string{"reset"}
	if !MatchesResetTrigger("  reset  ", triggers) {
		t.Fatal("expected match with leading/trailing whitespace")
	}
}

func TestMatchesResetTrigger_TriggerWhitespace(t *testing.T) {
	triggers := []string{"  reset  "}
	if !MatchesResetTrigger("reset", triggers) {
		t.Fatal("expected match when trigger has whitespace")
	}
}

func TestMatchesResetTrigger_NoMatch(t *testing.T) {
	triggers := []string{"reset", "clear"}
	if MatchesResetTrigger("hello", triggers) {
		t.Fatal("expected no match for unrelated text")
	}
}

func TestMatchesResetTrigger_EmptyTriggers(t *testing.T) {
	if MatchesResetTrigger("reset", nil) {
		t.Fatal("expected no match with nil triggers")
	}
	if MatchesResetTrigger("reset", []string{}) {
		t.Fatal("expected no match with empty triggers slice")
	}
}

func TestMatchesResetTrigger_EmptyText(t *testing.T) {
	triggers := []string{"reset"}
	if MatchesResetTrigger("", triggers) {
		t.Fatal("expected no match for empty text")
	}
}

func TestMatchesResetTrigger_MultiWordTrigger(t *testing.T) {
	triggers := []string{"start over"}
	if !MatchesResetTrigger("Start Over", triggers) {
		t.Fatal("expected match for multi-word trigger")
	}
}

// ---------------------------------------------------------------------------
// SplitMessage
// ---------------------------------------------------------------------------

func TestSplitMessage_ShortMessage(t *testing.T) {
	parts := SplitMessage("hello", 100)
	if len(parts) != 1 || parts[0] != "hello" {
		t.Fatalf("expected single part 'hello', got %v", parts)
	}
}

func TestSplitMessage_ExactLength(t *testing.T) {
	msg := "abcde"
	parts := SplitMessage(msg, 5)
	if len(parts) != 1 || parts[0] != msg {
		t.Fatalf("expected single part for exact-length message, got %v", parts)
	}
}

func TestSplitMessage_SplitsLongMessage(t *testing.T) {
	msg := strings.Repeat("a", 300)
	parts := SplitMessage(msg, 100)
	if len(parts) < 2 {
		t.Fatalf("expected multiple parts, got %d", len(parts))
	}
	// Verify reassembly
	joined := strings.Join(parts, "")
	if joined != msg {
		t.Fatal("reassembled parts do not match original")
	}
}

func TestSplitMessage_PrefersNewlineBreak(t *testing.T) {
	// Build a message with a newline inside the maxLen window.
	line1 := strings.Repeat("a", 80)
	line2 := strings.Repeat("b", 80)
	msg := line1 + "\n" + line2
	parts := SplitMessage(msg, 100)
	if len(parts) < 2 {
		t.Fatalf("expected split at newline, got %d parts", len(parts))
	}
	// First part should end at the newline (line1 + "\n").
	if parts[0] != line1+"\n" {
		t.Fatalf("expected first part to be line1+newline, got %q", parts[0])
	}
}

func TestSplitMessage_NoTrailingEmpty(t *testing.T) {
	msg := strings.Repeat("x", 200)
	parts := SplitMessage(msg, 100)
	for i, p := range parts {
		if p == "" {
			t.Fatalf("part[%d] is empty", i)
		}
	}
}

func TestSplitMessage_EmptyInput(t *testing.T) {
	parts := SplitMessage("", 100)
	if len(parts) != 1 || parts[0] != "" {
		t.Fatalf("expected single empty part, got %v", parts)
	}
}

func TestSplitMessage_AllPartsWithinLimit(t *testing.T) {
	msg := strings.Repeat("a", 50) + "\n" + strings.Repeat("b", 50) + "\n" + strings.Repeat("c", 50)
	parts := SplitMessage(msg, 60)
	for i, p := range parts {
		if len(p) > 60 {
			t.Fatalf("part[%d] length %d exceeds maxLen 60", i, len(p))
		}
	}
}

// ---------------------------------------------------------------------------
// SessionKey
// ---------------------------------------------------------------------------

func TestSessionKey_UserScope(t *testing.T) {
	key := SessionKey("agent1", "telegram", "user", "u123", "c456")
	expected := "agent1:telegram:u123"
	if key != expected {
		t.Fatalf("expected %q, got %q", expected, key)
	}
}

func TestSessionKey_ChannelScope(t *testing.T) {
	key := SessionKey("agent1", "discord", "channel", "u123", "c456")
	expected := "agent1:discord:channel:c456"
	if key != expected {
		t.Fatalf("expected %q, got %q", expected, key)
	}
}

func TestSessionKey_GlobalScope(t *testing.T) {
	key := SessionKey("agent1", "slack", "global", "u123", "c456")
	expected := "agent1:slack:global"
	if key != expected {
		t.Fatalf("expected %q, got %q", expected, key)
	}
}

func TestSessionKey_DefaultScope(t *testing.T) {
	// Empty scope should fall through to the user default.
	key := SessionKey("agent1", "telegram", "", "u123", "c456")
	expected := "agent1:telegram:u123"
	if key != expected {
		t.Fatalf("expected %q, got %q", expected, key)
	}
}

func TestSessionKey_UnknownScope(t *testing.T) {
	// An unrecognised scope should also fall through to user default.
	key := SessionKey("agent1", "telegram", "custom", "u123", "c456")
	expected := "agent1:telegram:u123"
	if key != expected {
		t.Fatalf("expected %q, got %q", expected, key)
	}
}

// ---------------------------------------------------------------------------
// PairedUsersPath
// ---------------------------------------------------------------------------

func TestPairedUsersPath_ContainsPlatform(t *testing.T) {
	path := PairedUsersPath("telegram")
	if !strings.Contains(path, "telegram-default-allowFrom.json") {
		t.Fatalf("expected path to contain platform filename, got %q", path)
	}
}

func TestPairedUsersPath_ContainsGopherclaw(t *testing.T) {
	path := PairedUsersPath("discord")
	if !strings.Contains(path, ".gopherclaw") {
		t.Fatalf("expected path to contain .gopherclaw, got %q", path)
	}
}

func TestPairedUsersPath_DifferentPlatforms(t *testing.T) {
	p1 := PairedUsersPath("telegram")
	p2 := PairedUsersPath("discord")
	if p1 == p2 {
		t.Fatal("expected different paths for different platforms")
	}
}

// ---------------------------------------------------------------------------
// LoadPairedUsers / SavePairedUsers (round-trip via temp directory)
// ---------------------------------------------------------------------------

// withTempHome overrides HOME so PairedUsersPath writes to a temp dir,
// then restores HOME after the test.
func withTempHome(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	t.Setenv("HOME", tmpDir)
	t.Cleanup(func() { os.Setenv("HOME", origHome) })
	return tmpDir
}

func TestSavePairedUsers_CreatesFile(t *testing.T) {
	withTempHome(t)

	ids := []string{"user1", "user2"}
	if err := SavePairedUsers(testLogger(), "testplat", ids); err != nil {
		t.Fatalf("SavePairedUsers failed: %v", err)
	}

	path := PairedUsersPath("testplat")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatalf("expected file to exist at %s", path)
	}
}

func TestSavePairedUsers_JSONFormat(t *testing.T) {
	withTempHome(t)

	ids := []string{"aaa", "bbb"}
	if err := SavePairedUsers(testLogger(), "testplat", ids); err != nil {
		t.Fatalf("SavePairedUsers failed: %v", err)
	}

	data, err := os.ReadFile(PairedUsersPath("testplat"))
	if err != nil {
		t.Fatalf("failed to read file: %v", err)
	}

	var state struct {
		Version   int      `json:"version"`
		AllowFrom []string `json:"allowFrom"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if state.Version != 1 {
		t.Fatalf("expected version 1, got %d", state.Version)
	}
	if len(state.AllowFrom) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(state.AllowFrom))
	}
}

func TestLoadPairedUsers_RoundTrip(t *testing.T) {
	withTempHome(t)

	ids := []string{"u1", "u2", "u3"}
	if err := SavePairedUsers(testLogger(), "testplat", ids); err != nil {
		t.Fatalf("SavePairedUsers failed: %v", err)
	}

	users := LoadPairedUsers(testLogger(), "testplat")
	if users == nil {
		t.Fatal("LoadPairedUsers returned nil")
	}
	for _, id := range ids {
		if !users[id] {
			t.Fatalf("expected user %q in loaded set", id)
		}
	}
	if len(users) != len(ids) {
		t.Fatalf("expected %d users, got %d", len(ids), len(users))
	}
}

func TestLoadPairedUsers_MissingFile(t *testing.T) {
	withTempHome(t)

	users := LoadPairedUsers(testLogger(), "nonexistent")
	if users != nil {
		t.Fatalf("expected nil for missing file, got %v", users)
	}
}

func TestLoadPairedUsers_InvalidJSON(t *testing.T) {
	withTempHome(t)

	path := PairedUsersPath("badplat")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{invalid json"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	users := LoadPairedUsers(testLogger(), "badplat")
	if users != nil {
		t.Fatalf("expected nil for invalid JSON, got %v", users)
	}
}

func TestLoadPairedUsers_EmptyAllowFrom(t *testing.T) {
	withTempHome(t)

	if err := SavePairedUsers(testLogger(), "emptyplat", []string{}); err != nil {
		t.Fatalf("SavePairedUsers failed: %v", err)
	}
	users := LoadPairedUsers(testLogger(), "emptyplat")
	if users == nil {
		t.Fatal("expected non-nil empty map")
	}
	if len(users) != 0 {
		t.Fatalf("expected 0 users, got %d", len(users))
	}
}

func TestSavePairedUsers_OverwritesExisting(t *testing.T) {
	withTempHome(t)

	if err := SavePairedUsers(testLogger(), "overwrite", []string{"a", "b"}); err != nil {
		t.Fatalf("first save failed: %v", err)
	}
	if err := SavePairedUsers(testLogger(), "overwrite", []string{"c"}); err != nil {
		t.Fatalf("second save failed: %v", err)
	}

	users := LoadPairedUsers(testLogger(), "overwrite")
	if len(users) != 1 || !users["c"] {
		t.Fatalf("expected only {c}, got %v", users)
	}
}

// ---------------------------------------------------------------------------
// IsSuppressible
// ---------------------------------------------------------------------------

func TestIsSuppressible(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"", true},
		{"   ", true},
		{"\n\t", true},
		{"NO_REPLY", true},
		{"no_reply", true},
		{"No_Reply", true},
		{" NO_REPLY ", true},
		{"...", true},
		{" ... ", true},
		{"\u2026", true},
		{"Hello", false},
		{"NO_REPLY extra text", false},
		{"Here is your answer", false},
	}
	for _, tt := range tests {
		got := IsSuppressible(tt.input)
		if got != tt.want {
			t.Errorf("IsSuppressible(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// UserFacingError
// ---------------------------------------------------------------------------

func TestUserFacingError(t *testing.T) {
	tests := []struct {
		err      error
		contains string
	}{
		{nil, "Sorry, something went wrong."},
		{fmt.Errorf("context deadline exceeded"), "timed out"},
		{fmt.Errorf("context canceled"), "timed out"},
		{fmt.Errorf("status 429: rate limit exceeded"), "rate-limited"},
		{fmt.Errorf("status 401: Unauthorized"), "authentication"},
		{fmt.Errorf("maximum context length exceeded"), "too long"},
		{fmt.Errorf("status 502: bad gateway"), "server error"},
		{fmt.Errorf("some random error"), "Sorry, something went wrong."},
	}
	for _, tt := range tests {
		got := UserFacingError(tt.err)
		if !strings.Contains(got, tt.contains) {
			t.Errorf("UserFacingError(%v) = %q, want to contain %q", tt.err, got, tt.contains)
		}
	}
}

// ---------------------------------------------------------------------------
// ToolNotifier
// ---------------------------------------------------------------------------

func TestToolNotifier_FirstThreeIndividual(t *testing.T) {
	var sent []string
	tn := NewToolNotifier(func(s string) { sent = append(sent, s) })

	tn.OnToolStart("exec", "")
	tn.OnToolStart("browser", "")
	tn.OnToolStart("read_file", "")

	if len(sent) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(sent))
	}
	if sent[0] != "🔧 exec…" {
		t.Errorf("sent[0] = %q, want %q", sent[0], "🔧 exec…")
	}
	if sent[1] != "🔧 browser…" {
		t.Errorf("sent[1] = %q, want %q", sent[1], "🔧 browser…")
	}
	if sent[2] != "🔧 read_file…" {
		t.Errorf("sent[2] = %q, want %q", sent[2], "🔧 read_file…")
	}
}

func TestToolNotifier_ThrottlesAfterThree(t *testing.T) {
	var sent []string
	tn := NewToolNotifier(func(s string) { sent = append(sent, s) })

	// Fire 10 tool calls rapidly; only the first 3 should produce messages.
	for i := 0; i < 10; i++ {
		tn.OnToolStart("exec", "")
	}

	if len(sent) != 3 {
		t.Fatalf("expected 3 messages (throttled), got %d: %v", len(sent), sent)
	}
}

func TestToolNotifier_BatchAfterInterval(t *testing.T) {
	var sent []string
	tn := NewToolNotifier(func(s string) { sent = append(sent, s) })

	// Send 3 individual
	for i := 0; i < 3; i++ {
		tn.OnToolStart("exec", "")
	}

	// Simulate elapsed time by resetting lastNotify
	tn.mu.Lock()
	tn.lastNotify = tn.lastNotify.Add(-ToolNotifyInterval - 1)
	tn.mu.Unlock()

	tn.OnToolStart("browser", "")
	if len(sent) != 4 {
		t.Fatalf("expected 4 messages (3 individual + 1 batch), got %d", len(sent))
	}
	if !strings.Contains(sent[3], "still working") {
		t.Errorf("batch message should contain 'still working', got %q", sent[3])
	}
	if !strings.Contains(sent[3], "4") {
		t.Errorf("batch message should contain tool count '4', got %q", sent[3])
	}
}
