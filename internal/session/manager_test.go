package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"go.uber.org/zap"
)

func testLogger() *zap.SugaredLogger { return zap.NewNop().Sugar() }

func TestSessionCreateAndRead(t *testing.T) {
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	key := "test:session:1"

	// No history initially
	h, err := m.GetHistory(key)
	if err != nil {
		t.Fatal(err)
	}
	if h != nil {
		t.Errorf("expected nil history for new session, got %d messages", len(h))
	}

	// Append messages
	msgs := []Message{
		{Role: "user", Content: "Hello", TS: time.Now().UnixMilli()},
		{Role: "assistant", Content: "Hi there!", TS: time.Now().UnixMilli()},
	}
	if err := m.AppendMessages(key, msgs); err != nil {
		t.Fatal(err)
	}

	// Read back
	h, err = m.GetHistory(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(h))
	}
	if h[0].Role != "user" || h[0].Content != "Hello" {
		t.Errorf("unexpected first message: %+v", h[0])
	}
	if h[1].Role != "assistant" || h[1].Content != "Hi there!" {
		t.Errorf("unexpected second message: %+v", h[1])
	}
}

func TestSessionTTLExpiry(t *testing.T) {
	dir := t.TempDir()
	// Create manager with very short TTL
	m, err := New(testLogger(), dir, 1*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	key := "test:ttl:1"
	msgs := []Message{
		{Role: "user", Content: "test", TS: time.Now().UnixMilli()},
	}
	if err := m.AppendMessages(key, msgs); err != nil {
		t.Fatal(err)
	}

	// Wait for TTL to expire
	time.Sleep(10 * time.Millisecond)

	h, err := m.GetHistory(key)
	if err != nil {
		t.Fatal(err)
	}
	if h != nil {
		t.Errorf("expected nil history after TTL expiry, got %d messages", len(h))
	}
}

func TestSessionAppend(t *testing.T) {
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	key := "test:append:1"

	// Append first batch
	batch1 := []Message{
		{Role: "user", Content: "first", TS: time.Now().UnixMilli()},
		{Role: "assistant", Content: "response1", TS: time.Now().UnixMilli()},
	}
	if err := m.AppendMessages(key, batch1); err != nil {
		t.Fatal(err)
	}

	// Append second batch
	batch2 := []Message{
		{Role: "user", Content: "second", TS: time.Now().UnixMilli()},
		{Role: "assistant", Content: "response2", TS: time.Now().UnixMilli()},
	}
	if err := m.AppendMessages(key, batch2); err != nil {
		t.Fatal(err)
	}

	// All four messages should be present
	h, err := m.GetHistory(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 4 {
		t.Errorf("expected 4 messages, got %d", len(h))
	}
}

func TestSessionReset(t *testing.T) {
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	key := "test:reset:1"
	if err := m.AppendMessages(key, []Message{{Role: "user", Content: "hello", TS: time.Now().UnixMilli()}}); err != nil {
		t.Fatal(err)
	}

	m.Reset(key)

	h, err := m.GetHistory(key)
	if err != nil {
		t.Fatal(err)
	}
	if h != nil {
		t.Errorf("expected nil after reset, got %d messages", len(h))
	}
}

func TestSessionResetAll(t *testing.T) {
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	for i := range 3 {
		key := "test:resetall:" + string(rune('A'+i))
		if err := m.AppendMessages(key, []Message{{Role: "user", Content: "x", TS: time.Now().UnixMilli()}}); err != nil {
			t.Fatal(err)
		}
	}

	m.ResetAll()

	active := m.ActiveSessions()
	if len(active) != 0 {
		t.Errorf("expected 0 active sessions after ResetAll, got %d", len(active))
	}
}

func TestSessionResetIdle(t *testing.T) {
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	key := "test:idle:1"
	if err := m.AppendMessages(key, []Message{{Role: "user", Content: "x", TS: time.Now().UnixMilli()}}); err != nil {
		t.Fatal(err)
	}

	// Sleep a tiny bit then reset with a very short idle threshold
	time.Sleep(5 * time.Millisecond)
	n := m.ResetIdle(1 * time.Millisecond)
	if n != 1 {
		t.Errorf("expected 1 idle session reset, got %d", n)
	}
}

func TestTokenPruning(t *testing.T) {
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// Configure pruning with low threshold
	m.SetPruning(PruningPolicy{
		HardClearRatio:     0.001, // very low threshold to trigger pruning
		ModelMaxTokens:     100,
		KeepLastAssistants: 1,
	})

	key := "test:prune:1"
	now := time.Now().UnixMilli()
	msgs := []Message{
		{Role: "user", Content: "first question with enough text to push tokens over the very low threshold we set", TS: now},
		{Role: "assistant", Content: "first answer with plenty of content here to generate tokens", TS: now},
		{Role: "user", Content: "second question also with significant content for token estimation", TS: now},
		{Role: "assistant", Content: "second answer that is also quite long for testing purposes", TS: now},
		{Role: "user", Content: "third question that is verbose enough to exceed our tiny token limit", TS: now},
		{Role: "assistant", Content: "third answer that should definitely push us over the edge", TS: now},
	}
	if err := m.AppendMessages(key, msgs); err != nil {
		t.Fatal(err)
	}

	h, err := m.GetHistory(key)
	if err != nil {
		t.Fatal(err)
	}

	// After pruning, should have fewer messages
	if len(h) >= len(msgs) {
		t.Errorf("expected pruning to reduce messages from %d, got %d", len(msgs), len(h))
	}
	// Last message should still be an assistant
	if len(h) > 0 && h[len(h)-1].Role != "assistant" {
		t.Errorf("last message after pruning should be assistant, got %s", h[len(h)-1].Role)
	}
}

func TestEstimateTokens(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "hello world"}, // 11 chars / 4 + 4 = ~6
	}
	tokens := EstimateTokens(msgs)
	if tokens <= 0 {
		t.Errorf("expected positive token count, got %d", tokens)
	}
}

func TestToOpenAIAndFromOpenAI(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "hello", TS: 1234},
		{Role: "assistant", Content: "hi", TS: 1235},
	}

	oai := ToOpenAI(msgs)
	if len(oai) != 2 {
		t.Fatalf("expected 2 openai messages, got %d", len(oai))
	}
	if oai[0].Role != "user" || oai[0].Content != "hello" {
		t.Errorf("unexpected openai msg: %+v", oai[0])
	}

	back := FromOpenAI(oai)
	if len(back) != 2 {
		t.Fatalf("expected 2 messages back, got %d", len(back))
	}
	if back[0].Role != "user" || back[0].Content != "hello" {
		t.Errorf("unexpected converted msg: %+v", back[0])
	}
}

func TestToOpenAIDropsOrphanedToolMessages(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "hello", TS: 1},
		// Orphaned tool result — no preceding assistant with matching tool_calls
		{Role: "tool", Content: "result", ToolCallID: "call_orphan", TS: 2},
		{Role: "assistant", Content: "reply", TS: 3},
	}

	oai := ToOpenAI(msgs)
	if len(oai) != 2 {
		t.Fatalf("expected 2 messages (orphan dropped), got %d", len(oai))
	}
	if oai[0].Role != "user" {
		t.Errorf("expected first msg to be user, got %s", oai[0].Role)
	}
	if oai[1].Role != "assistant" {
		t.Errorf("expected second msg to be assistant, got %s", oai[1].Role)
	}
}

func TestToOpenAIKeepsValidToolMessages(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "hello", TS: 1},
		{Role: "assistant", Content: "", ToolCalls: []openai.ToolCall{
			{ID: "call_1", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "test", Arguments: "{}"}},
		}, TS: 2},
		{Role: "tool", Content: "result", ToolCallID: "call_1", TS: 3},
		{Role: "assistant", Content: "done", TS: 4},
	}

	oai := ToOpenAI(msgs)
	if len(oai) != 4 {
		t.Fatalf("expected 4 messages (all valid), got %d", len(oai))
	}
	if oai[2].Role != "tool" || oai[2].ToolCallID != "call_1" {
		t.Errorf("expected valid tool message preserved, got %+v", oai[2])
	}
}

func TestPruneKeepLastPreservesToolSequence(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "q1", TS: 1},
		{Role: "assistant", Content: "", ToolCalls: []openai.ToolCall{
			{ID: "call_1", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "test", Arguments: "{}"}},
		}, TS: 2},
		{Role: "tool", Content: "result1", ToolCallID: "call_1", TS: 3},
		{Role: "assistant", Content: "a1", TS: 4},
		{Role: "user", Content: "q2", TS: 5},
		{Role: "assistant", Content: "a2", TS: 6},
	}

	result := pruneKeepLast(msgs, 1)

	// Should keep the last assistant and its preceding user, but also
	// the tool sequence should not be orphaned if included.
	// With keepAssistants=1, we want at least "user:q2" + "assistant:a2".
	for _, m := range result {
		if m.Role == "tool" {
			// If a tool message is included, its assistant with tool_calls must also be present.
			found := false
			for _, m2 := range result {
				if m2.Role == "assistant" {
					for _, tc := range m2.ToolCalls {
						if tc.ID == m.ToolCallID {
							found = true
						}
					}
				}
			}
			if !found {
				t.Errorf("tool message with ToolCallID=%s has no matching assistant tool_calls", m.ToolCallID)
			}
		}
	}
}

func TestActiveSessions(t *testing.T) {
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if err := m.AppendMessages("s1", []Message{{Role: "user", Content: "x", TS: time.Now().UnixMilli()}}); err != nil {
		t.Fatal(err)
	}
	if err := m.AppendMessages("s2", []Message{{Role: "user", Content: "y", TS: time.Now().UnixMilli()}}); err != nil {
		t.Fatal(err)
	}

	active := m.ActiveSessions()
	if len(active) != 2 {
		t.Errorf("expected 2 active sessions, got %d", len(active))
	}
	if _, ok := active["s1"]; !ok {
		t.Error("expected s1 in active sessions")
	}
	if _, ok := active["s2"]; !ok {
		t.Error("expected s2 in active sessions")
	}
}

func TestSessionFilePerm(t *testing.T) {
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	key := "test:perm:1"
	if err := m.AppendMessages(key, []Message{{Role: "user", Content: "hello", TS: time.Now().UnixMilli()}}); err != nil {
		t.Fatal(err)
	}

	// Check that JSONL file is created with 0600 permissions
	m.mu.Lock()
	e := m.sessions[key]
	jsonlPath := m.jsonlPath(e.SessionID)
	m.mu.Unlock()

	info, err := os.Stat(jsonlPath)
	if err != nil {
		t.Fatal(err)
	}
	perm := info.Mode().Perm()
	if perm != 0600 {
		t.Errorf("expected file permission 0600, got %o", perm)
	}
}

// ---------------------------------------------------------------------------
// ReplaceHistory
// ---------------------------------------------------------------------------

func TestReplaceHistory(t *testing.T) {
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	key := "test:replace:1"
	now := time.Now().UnixMilli()

	// Populate initial history.
	original := []Message{
		{Role: "user", Content: "old question", TS: now},
		{Role: "assistant", Content: "old answer", TS: now},
	}
	if err := m.AppendMessages(key, original); err != nil {
		t.Fatal(err)
	}

	// Replace with new history.
	replacement := []Message{
		{Role: "user", Content: "new question", TS: now},
		{Role: "assistant", Content: "new answer", TS: now},
		{Role: "user", Content: "follow up", TS: now},
		{Role: "assistant", Content: "follow up answer", TS: now},
	}
	if err := m.ReplaceHistory(key, replacement); err != nil {
		t.Fatal(err)
	}

	h, err := m.GetHistory(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 4 {
		t.Fatalf("expected 4 messages after replace, got %d", len(h))
	}
	if h[0].Content != "new question" {
		t.Errorf("expected first message 'new question', got %q", h[0].Content)
	}
	if h[3].Content != "follow up answer" {
		t.Errorf("expected last message 'follow up answer', got %q", h[3].Content)
	}
}

func TestReplaceHistoryNonexistentSession(t *testing.T) {
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// ReplaceHistory on a session that does not exist should return nil (no-op).
	err = m.ReplaceHistory("nonexistent:key", []Message{
		{Role: "user", Content: "hello", TS: time.Now().UnixMilli()},
	})
	if err != nil {
		t.Errorf("expected nil error for nonexistent session, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// TrimMessages
// ---------------------------------------------------------------------------

func TestTrimMessages(t *testing.T) {
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	key := "test:trim:1"
	now := time.Now().UnixMilli()

	msgs := []Message{
		{Role: "user", Content: "q1", TS: now},
		{Role: "assistant", Content: "a1", TS: now},
		{Role: "user", Content: "q2", TS: now},
		{Role: "assistant", Content: "a2", TS: now},
		{Role: "user", Content: "q3", TS: now},
		{Role: "assistant", Content: "a3", TS: now},
	}
	if err := m.AppendMessages(key, msgs); err != nil {
		t.Fatal(err)
	}

	// Trim to last 2 messages.
	if err := m.TrimMessages(key, 2); err != nil {
		t.Fatal(err)
	}

	h, err := m.GetHistory(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 2 {
		t.Fatalf("expected 2 messages after trim, got %d", len(h))
	}
	if h[0].Content != "q3" {
		t.Errorf("expected first trimmed message 'q3', got %q", h[0].Content)
	}
	if h[1].Content != "a3" {
		t.Errorf("expected second trimmed message 'a3', got %q", h[1].Content)
	}
}

func TestTrimMessagesZeroNoop(t *testing.T) {
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	key := "test:trim:zero"
	if err := m.AppendMessages(key, []Message{{Role: "user", Content: "x", TS: time.Now().UnixMilli()}}); err != nil {
		t.Fatal(err)
	}

	// maxMessages <= 0 should be a no-op.
	if err := m.TrimMessages(key, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.TrimMessages(key, -1); err != nil {
		t.Fatal(err)
	}

	h, err := m.GetHistory(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 1 {
		t.Errorf("expected 1 message (no-op trim), got %d", len(h))
	}
}

func TestTrimMessagesNonexistentSession(t *testing.T) {
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// Trimming a session that doesn't exist should be a silent no-op.
	if err := m.TrimMessages("nonexistent:key", 5); err != nil {
		t.Errorf("expected nil error for nonexistent session, got %v", err)
	}
}

func TestTrimMessagesAlreadyWithinLimit(t *testing.T) {
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	key := "test:trim:within"
	if err := m.AppendMessages(key, []Message{
		{Role: "user", Content: "q", TS: time.Now().UnixMilli()},
		{Role: "assistant", Content: "a", TS: time.Now().UnixMilli()},
	}); err != nil {
		t.Fatal(err)
	}

	// Limit larger than current count should be a no-op.
	if err := m.TrimMessages(key, 10); err != nil {
		t.Fatal(err)
	}

	h, err := m.GetHistory(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 2 {
		t.Errorf("expected 2 messages unchanged, got %d", len(h))
	}
}

// ---------------------------------------------------------------------------
// FlushSave
// ---------------------------------------------------------------------------

func TestFlushSave(t *testing.T) {
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// Append triggers scheduleSave with a 2s timer; FlushSave should force immediate write.
	if err := m.AppendMessages("flush:key", []Message{
		{Role: "user", Content: "test", TS: time.Now().UnixMilli()},
	}); err != nil {
		t.Fatal(err)
	}

	m.FlushSave()

	// sessions.json should now exist on disk.
	data, err := os.ReadFile(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatalf("sessions.json not written after FlushSave: %v", err)
	}

	var sessions map[string]*Entry
	if err := json.Unmarshal(data, &sessions); err != nil {
		t.Fatalf("invalid sessions.json: %v", err)
	}
	if _, ok := sessions["flush:key"]; !ok {
		t.Error("expected 'flush:key' in sessions.json after FlushSave")
	}
}

func TestFlushSaveWhenNotDirty(t *testing.T) {
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// FlushSave on a brand new manager (not dirty) should not panic or error.
	m.FlushSave()

	// sessions.json may or may not exist, but there should be no panic.
	// Check it doesn't exist since nothing was dirty.
	_, err = os.Stat(filepath.Join(dir, "sessions.json"))
	if err == nil {
		t.Log("sessions.json exists even when not dirty (acceptable if load wrote it)")
	}
}

// ---------------------------------------------------------------------------
// EstimateTokens with tool calls
// ---------------------------------------------------------------------------

func TestEstimateTokensWithToolCalls(t *testing.T) {
	msgsNoTools := []Message{
		{Role: "assistant", Content: "thinking"},
	}
	msgsWithTools := []Message{
		{Role: "assistant", Content: "thinking", ToolCalls: []openai.ToolCall{
			{ID: "call_1", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{
				Name:      "web_search",
				Arguments: `{"query":"golang testing best practices"}`,
			}},
		}},
	}

	tokensNoTools := EstimateTokens(msgsNoTools)
	tokensWithTools := EstimateTokens(msgsWithTools)

	if tokensWithTools <= tokensNoTools {
		t.Errorf("expected tool calls to increase token count: without=%d, with=%d", tokensNoTools, tokensWithTools)
	}
}

func TestEstimateTokensEmpty(t *testing.T) {
	tokens := EstimateTokens(nil)
	if tokens != 0 {
		t.Errorf("expected 0 tokens for empty slice, got %d", tokens)
	}
}

// ---------------------------------------------------------------------------
// pruneKeepLast edge cases
// ---------------------------------------------------------------------------

func TestPruneKeepLastDefaultsToTwo(t *testing.T) {
	// When keepAssistants <= 0, it should default to 2.
	msgs := []Message{
		{Role: "user", Content: "q1", TS: 1},
		{Role: "assistant", Content: "a1", TS: 2},
		{Role: "user", Content: "q2", TS: 3},
		{Role: "assistant", Content: "a2", TS: 4},
		{Role: "user", Content: "q3", TS: 5},
		{Role: "assistant", Content: "a3", TS: 6},
	}

	result := pruneKeepLast(msgs, 0)

	// Default keepAssistants=2 means we keep last 2 assistants (a2, a3) plus their users.
	assistantCount := 0
	for _, m := range result {
		if m.Role == "assistant" {
			assistantCount++
		}
	}
	if assistantCount != 2 {
		t.Errorf("expected 2 assistants with default keepAssistants, got %d", assistantCount)
	}

	// Verify that a3 and a2 are present.
	if result[len(result)-1].Content != "a3" {
		t.Errorf("expected last message to be a3, got %q", result[len(result)-1].Content)
	}
}

func TestPruneKeepLastNegativeDefaults(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "q1", TS: 1},
		{Role: "assistant", Content: "a1", TS: 2},
		{Role: "user", Content: "q2", TS: 3},
		{Role: "assistant", Content: "a2", TS: 4},
		{Role: "user", Content: "q3", TS: 5},
		{Role: "assistant", Content: "a3", TS: 6},
	}

	result := pruneKeepLast(msgs, -5)

	// Negative keepAssistants should also default to 2.
	assistantCount := 0
	for _, m := range result {
		if m.Role == "assistant" {
			assistantCount++
		}
	}
	if assistantCount != 2 {
		t.Errorf("expected 2 assistants with negative keepAssistants, got %d", assistantCount)
	}
}

func TestPruneKeepLastFewerAssistantsThanKeep(t *testing.T) {
	// If there are fewer assistants than keepAssistants, return all messages.
	msgs := []Message{
		{Role: "user", Content: "q1", TS: 1},
		{Role: "assistant", Content: "a1", TS: 2},
	}

	result := pruneKeepLast(msgs, 5)
	if len(result) != len(msgs) {
		t.Errorf("expected all %d messages returned when fewer assistants than keep, got %d", len(msgs), len(result))
	}
}

func TestPruneKeepLastToolSequenceAtCutPoint(t *testing.T) {
	// The cut point falls right at a tool_calls -> tool sequence boundary.
	// pruneKeepLast must walk back to include the assistant with tool_calls.
	msgs := []Message{
		{Role: "user", Content: "q0", TS: 1},
		{Role: "assistant", Content: "a0", TS: 2},
		{Role: "user", Content: "q1", TS: 3},
		{Role: "assistant", Content: "", ToolCalls: []openai.ToolCall{
			{ID: "call_tc1", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "exec", Arguments: `{"cmd":"ls"}`}},
		}, TS: 4},
		{Role: "tool", Content: "file1\nfile2", ToolCallID: "call_tc1", TS: 5},
		{Role: "assistant", Content: "here are the files", TS: 6},
		{Role: "user", Content: "q2", TS: 7},
		{Role: "assistant", Content: "a2", TS: 8},
	}

	// keepAssistants=2: last 2 assistants are at index 5 and 7.
	// cutFrom initially = index 5 (assistant "here are the files").
	// Preceding user at index 3? No — index 4 is tool, index 3 is assistant with tool_calls.
	// The walkback should include the tool and its assistant.
	result := pruneKeepLast(msgs, 2)

	// Verify no orphaned tool messages.
	toolCallIDs := make(map[string]bool)
	for _, m := range result {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				toolCallIDs[tc.ID] = true
			}
		}
	}
	for _, m := range result {
		if m.Role == "tool" && !toolCallIDs[m.ToolCallID] {
			t.Errorf("orphaned tool message with ToolCallID=%s in pruned result", m.ToolCallID)
		}
	}

	// The tool sequence assistant should be present.
	foundToolCallAssistant := false
	for _, m := range result {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 && m.ToolCalls[0].ID == "call_tc1" {
			foundToolCallAssistant = true
		}
	}
	if !foundToolCallAssistant {
		t.Error("expected assistant with tool_calls to be preserved at cut boundary")
	}
}

// ---------------------------------------------------------------------------
// FromOpenAI round-trip with tool calls
// ---------------------------------------------------------------------------

func TestFromOpenAIRoundTripWithToolCalls(t *testing.T) {
	original := []Message{
		{Role: "user", Content: "search for Go docs", TS: 100},
		{Role: "assistant", Content: "", ToolCalls: []openai.ToolCall{
			{ID: "call_abc", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{
				Name:      "web_search",
				Arguments: `{"query":"Go documentation"}`,
			}},
		}, TS: 200},
		{Role: "tool", Content: "Go is a programming language...", ToolCallID: "call_abc", Name: "web_search", TS: 300},
		{Role: "assistant", Content: "Here is what I found", TS: 400},
	}

	oai := ToOpenAI(original)
	if len(oai) != 4 {
		t.Fatalf("expected 4 openai messages, got %d", len(oai))
	}

	// Verify tool_calls survived conversion.
	if len(oai[1].ToolCalls) != 1 || oai[1].ToolCalls[0].ID != "call_abc" {
		t.Errorf("tool calls not preserved in ToOpenAI: %+v", oai[1])
	}
	if oai[2].ToolCallID != "call_abc" || oai[2].Name != "web_search" {
		t.Errorf("tool message fields not preserved: %+v", oai[2])
	}

	// Convert back.
	back := FromOpenAI(oai)
	if len(back) != 4 {
		t.Fatalf("expected 4 messages from FromOpenAI, got %d", len(back))
	}

	// Check fields round-tripped (TS will differ since FromOpenAI uses nowMs()).
	if back[1].Role != "assistant" || len(back[1].ToolCalls) != 1 {
		t.Errorf("assistant tool_calls not round-tripped: %+v", back[1])
	}
	if back[2].Role != "tool" || back[2].ToolCallID != "call_abc" || back[2].Name != "web_search" {
		t.Errorf("tool message not round-tripped: %+v", back[2])
	}
}

// ---------------------------------------------------------------------------
// ResetIdle — active sessions not cleared
// ---------------------------------------------------------------------------

func TestResetIdleKeepsActive(t *testing.T) {
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// Add two sessions.
	if err := m.AppendMessages("old", []Message{{Role: "user", Content: "x", TS: time.Now().UnixMilli()}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(15 * time.Millisecond)

	// Add a fresh session after the sleep.
	if err := m.AppendMessages("new", []Message{{Role: "user", Content: "y", TS: time.Now().UnixMilli()}}); err != nil {
		t.Fatal(err)
	}

	// Reset only sessions idle > 10ms. "old" should be reset, "new" should survive.
	n := m.ResetIdle(10 * time.Millisecond)
	if n != 1 {
		t.Errorf("expected 1 idle session reset, got %d", n)
	}

	active := m.ActiveSessions()
	if _, ok := active["new"]; !ok {
		t.Error("expected 'new' session to survive idle reset")
	}
	if _, ok := active["old"]; ok {
		t.Error("expected 'old' session to be cleared by idle reset")
	}
}

// ---------------------------------------------------------------------------
// ResetAll — verifies JSONL files removed
// ---------------------------------------------------------------------------

func TestResetAllRemovesFiles(t *testing.T) {
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if err := m.AppendMessages("s1", []Message{{Role: "user", Content: "x", TS: time.Now().UnixMilli()}}); err != nil {
		t.Fatal(err)
	}

	// Capture the JSONL path before reset.
	m.mu.Lock()
	jsonlPath := m.jsonlPath(m.sessions["s1"].SessionID)
	m.mu.Unlock()

	// Verify file exists.
	if _, err := os.Stat(jsonlPath); err != nil {
		t.Fatalf("expected JSONL file to exist before reset: %v", err)
	}

	m.ResetAll()

	// File should be removed.
	if _, err := os.Stat(jsonlPath); !os.IsNotExist(err) {
		t.Errorf("expected JSONL file removed after ResetAll, err=%v", err)
	}
}

// ---------------------------------------------------------------------------
// Concurrent access
// ---------------------------------------------------------------------------

func TestConcurrentAppend(t *testing.T) {
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	key := "test:concurrent"
	const goroutines = 10
	const msgsPerGoroutine = 5

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(id int) {
			defer wg.Done()
			for j := range msgsPerGoroutine {
				msg := Message{
					Role:    "user",
					Content: "msg from goroutine",
					TS:      time.Now().UnixMilli() + int64(id*100+j),
				}
				if err := m.AppendMessages(key, []Message{msg}); err != nil {
					t.Errorf("concurrent append error: %v", err)
				}
			}
		}(i)
	}
	wg.Wait()

	h, err := m.GetHistory(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != goroutines*msgsPerGoroutine {
		t.Errorf("expected %d messages after concurrent appends, got %d", goroutines*msgsPerGoroutine, len(h))
	}
}

func TestConcurrentAppendDifferentSessions(t *testing.T) {
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	const goroutines = 8

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(id int) {
			defer wg.Done()
			key := "session:" + string(rune('A'+id))
			for range 3 {
				if err := m.AppendMessages(key, []Message{
					{Role: "user", Content: "hi", TS: time.Now().UnixMilli()},
				}); err != nil {
					t.Errorf("concurrent append to %s: %v", key, err)
				}
			}
		}(i)
	}
	wg.Wait()

	active := m.ActiveSessions()
	if len(active) != goroutines {
		t.Errorf("expected %d active sessions, got %d", goroutines, len(active))
	}
}

// ---------------------------------------------------------------------------
// StartResetLoop (cancellation)
// ---------------------------------------------------------------------------

func TestStartResetLoopCancellation(t *testing.T) {
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Start both daily and idle reset loops. Use high thresholds so they don't
	// actually reset anything — we're just testing that they stop on cancel.
	m.StartResetLoop(ctx, "daily", 3, 60, func(f string, a ...any) {})

	// Give the goroutines a moment to start.
	time.Sleep(5 * time.Millisecond)

	// Cancel should make the goroutines exit without panic.
	cancel()
	time.Sleep(10 * time.Millisecond)
}

func TestStartResetLoopNoDaily(t *testing.T) {
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()

	// dailyMode != "daily" should skip the daily goroutine; idleMinutes <= 0 skips idle.
	m.StartResetLoop(ctx, "off", 0, 0, func(f string, a ...any) {})

	// No goroutines started, just verify no panic.
	time.Sleep(5 * time.Millisecond)
}

// ---------------------------------------------------------------------------
// Load existing sessions.json on startup
// ---------------------------------------------------------------------------

func TestNewLoadsExistingSessions(t *testing.T) {
	dir := t.TempDir()

	// Create a manager, populate a session, and flush.
	m1, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := m1.AppendMessages("persist:key", []Message{
		{Role: "user", Content: "persisted message", TS: time.Now().UnixMilli()},
	}); err != nil {
		t.Fatal(err)
	}
	m1.FlushSave()

	// Create a second manager from the same directory.
	m2, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	h, err := m2.GetHistory("persist:key")
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 1 {
		t.Fatalf("expected 1 message from persisted session, got %d", len(h))
	}
	if h[0].Content != "persisted message" {
		t.Errorf("expected 'persisted message', got %q", h[0].Content)
	}
}

func TestNewWithCorruptSessionsJSON(t *testing.T) {
	dir := t.TempDir()

	// Write invalid JSON to sessions.json.
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), []byte("{corrupt"), 0600); err != nil {
		t.Fatal(err)
	}

	// New should still succeed (warns and starts fresh).
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	active := m.ActiveSessions()
	if len(active) != 0 {
		t.Errorf("expected 0 sessions with corrupt sessions.json, got %d", len(active))
	}
}

// ---------------------------------------------------------------------------
// ToOpenAI: tool message with empty ToolCallID kept
// ---------------------------------------------------------------------------

func TestToOpenAIToolMessageWithEmptyToolCallID(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "hello", TS: 1},
		// A tool message with empty ToolCallID — the orphan check requires
		// ToolCallID != "" to trigger, so this should pass through.
		{Role: "tool", Content: "result", ToolCallID: "", TS: 2},
		{Role: "assistant", Content: "ok", TS: 3},
	}

	oai := ToOpenAI(msgs)
	if len(oai) != 3 {
		t.Fatalf("expected 3 messages (empty ToolCallID not filtered), got %d", len(oai))
	}
}

// ---------------------------------------------------------------------------
// ToOpenAI: multiple tool calls from one assistant
// ---------------------------------------------------------------------------

func TestToOpenAIMultipleToolCalls(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "do both tasks", TS: 1},
		{Role: "assistant", Content: "", ToolCalls: []openai.ToolCall{
			{ID: "call_a", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "taskA", Arguments: "{}"}},
			{ID: "call_b", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "taskB", Arguments: "{}"}},
		}, TS: 2},
		{Role: "tool", Content: "result A", ToolCallID: "call_a", TS: 3},
		{Role: "tool", Content: "result B", ToolCallID: "call_b", TS: 4},
		{Role: "assistant", Content: "both done", TS: 5},
	}

	oai := ToOpenAI(msgs)
	if len(oai) != 5 {
		t.Fatalf("expected 5 messages (all valid), got %d", len(oai))
	}
}

// ---------------------------------------------------------------------------
// ToOpenAI: mix of valid and orphaned tool messages
// ---------------------------------------------------------------------------

func TestToOpenAIMixedOrphanedAndValid(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "q", TS: 1},
		{Role: "assistant", Content: "", ToolCalls: []openai.ToolCall{
			{ID: "call_valid", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "f", Arguments: "{}"}},
		}, TS: 2},
		{Role: "tool", Content: "valid result", ToolCallID: "call_valid", TS: 3},
		{Role: "tool", Content: "orphan result", ToolCallID: "call_nonexistent", TS: 4},
		{Role: "assistant", Content: "done", TS: 5},
	}

	oai := ToOpenAI(msgs)
	// The orphaned tool message should be dropped, leaving 4 messages.
	if len(oai) != 4 {
		t.Fatalf("expected 4 messages (1 orphan dropped), got %d", len(oai))
	}
	// Verify the orphan is gone.
	for _, m := range oai {
		if m.ToolCallID == "call_nonexistent" {
			t.Error("orphaned tool message should have been dropped")
		}
	}
}

func TestSanitizeOrphansRemovesOrphanedToolResults(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "hello", TS: 1},
		{Role: "tool", Content: "orphan1", ToolCallID: "call_dead1", TS: 2},
		{Role: "assistant", Content: "", ToolCalls: []openai.ToolCall{
			{ID: "call_good", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "f", Arguments: "{}"}},
		}, TS: 3},
		{Role: "tool", Content: "valid", ToolCallID: "call_good", TS: 4},
		{Role: "tool", Content: "orphan2", ToolCallID: "call_dead2", TS: 5},
		{Role: "assistant", Content: "done", TS: 6},
	}

	cleaned, n := SanitizeOrphans(msgs)
	if n != 2 {
		t.Fatalf("expected 2 orphans removed, got %d", n)
	}
	if len(cleaned) != 4 {
		t.Fatalf("expected 4 messages after cleanup, got %d", len(cleaned))
	}
	for _, m := range cleaned {
		if m.ToolCallID == "call_dead1" || m.ToolCallID == "call_dead2" {
			t.Errorf("orphan tool_call_id %s should have been removed", m.ToolCallID)
		}
	}
}

func TestSanitizeOrphansNoOpWhenClean(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "hello", TS: 1},
		{Role: "assistant", Content: "", ToolCalls: []openai.ToolCall{
			{ID: "call_1", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "f", Arguments: "{}"}},
		}, TS: 2},
		{Role: "tool", Content: "result", ToolCallID: "call_1", TS: 3},
		{Role: "assistant", Content: "done", TS: 4},
	}

	cleaned, n := SanitizeOrphans(msgs)
	if n != 0 {
		t.Fatalf("expected 0 orphans, got %d", n)
	}
	if len(cleaned) != 4 {
		t.Fatalf("expected 4 messages unchanged, got %d", len(cleaned))
	}
}

func TestGetHistoryCleansOrphanedToolResults(t *testing.T) {
	dir := t.TempDir()
	mgr, err := New(testLogger(), dir, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Seed a session with an orphaned tool result.
	key := "test:orphan"
	if err := mgr.AppendMessages(key, []Message{
		{Role: "user", Content: "hi", TS: 1},
		{Role: "tool", Content: "orphan", ToolCallID: "call_stale", TS: 2},
		{Role: "assistant", Content: "ok", TS: 3},
	}); err != nil {
		t.Fatal(err)
	}

	// First GetHistory should clean the orphan and rewrite.
	msgs, err := mgr.GetHistory(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages after orphan cleanup, got %d", len(msgs))
	}

	// Second GetHistory should return the same clean result with no rewrite needed.
	msgs2, err := mgr.GetHistory(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs2) != 2 {
		t.Fatalf("expected 2 messages on second load, got %d", len(msgs2))
	}
}

// ---------------------------------------------------------------------------
// PruneToolResults
// ---------------------------------------------------------------------------

func TestPruneToolResults_CompressesOldToolResults(t *testing.T) {
	longResult := make([]byte, 500)
	for i := range longResult {
		longResult[i] = 'x'
	}
	msgs := []Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "", ToolCalls: []openai.ToolCall{{ID: "tc1", Function: openai.FunctionCall{Name: "read"}}}},
		{Role: "tool", ToolCallID: "tc1", Name: "read", Content: string(longResult)},
		{Role: "assistant", Content: "answer1"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "", ToolCalls: []openai.ToolCall{{ID: "tc2", Function: openai.FunctionCall{Name: "read"}}}},
		{Role: "tool", ToolCallID: "tc2", Name: "read", Content: string(longResult)},
		{Role: "assistant", Content: "answer2"},
	}

	result := PruneToolResults(msgs, 1)
	if len(result) != len(msgs) {
		t.Fatalf("expected same number of messages, got %d", len(result))
	}
	// First tool result should be compressed.
	if len(result[2].Content) >= 500 {
		t.Fatalf("expected first tool result to be compressed, got %d chars", len(result[2].Content))
	}
	if result[2].ToolCallID != "tc1" {
		t.Fatal("expected ToolCallID to be preserved")
	}
	// Second tool result should be untouched (within last 1 assistant).
	if result[6].Content != string(longResult) {
		t.Fatal("expected last tool result to remain untouched")
	}
}

func TestPruneToolResults_PreservesShortResults(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: "", ToolCalls: []openai.ToolCall{{ID: "tc1"}}},
		{Role: "tool", ToolCallID: "tc1", Content: "short"},
		{Role: "assistant", Content: "a"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "a2"},
	}
	result := PruneToolResults(msgs, 1)
	if result[2].Content != "short" {
		t.Fatal("short tool result should not be compressed")
	}
}

func TestPruneToolResults_NoToolMessages(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}
	result := PruneToolResults(msgs, 2)
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
}

func TestPruneToolResults_DefaultKeepRecent(t *testing.T) {
	longR := make([]byte, 300)
	for i := range longR {
		longR[i] = 'y'
	}
	// 5 assistants so the old tool result has enough distance from keep boundary.
	msgs := []Message{
		{Role: "assistant", Content: "", ToolCalls: []openai.ToolCall{{ID: "old"}}},
		{Role: "tool", ToolCallID: "old", Content: string(longR)},
		{Role: "assistant", Content: "a"},
		{Role: "assistant", Content: "b"},
		{Role: "assistant", Content: "c"},
		{Role: "assistant", Content: "d"},
	}
	// keepRecent=0 should default to 2, protecting last 2 assistants.
	result := PruneToolResults(msgs, 0)
	if len(result[1].Content) >= 300 {
		t.Fatalf("old tool result should be compressed when keepRecent defaults to 2")
	}
}

func TestCacheTTL_DefersPruning(t *testing.T) {
	dir := t.TempDir()
	mgr, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mgr.SetPruning(PruningPolicy{
		HardClearRatio:     0.5,
		ModelMaxTokens:     100, // Very small so any content exceeds threshold.
		KeepLastAssistants: 1,
		CacheTTL:           5 * time.Minute,
	})

	key := "test:cache:1"
	// Create enough content to exceed the pruning threshold.
	bigContent := make([]byte, 200)
	for i := range bigContent {
		bigContent[i] = 'x'
	}
	msgs := []Message{
		{Role: "user", Content: string(bigContent), TS: time.Now().UnixMilli()},
		{Role: "assistant", Content: string(bigContent), TS: time.Now().UnixMilli()},
		{Role: "user", Content: string(bigContent), TS: time.Now().UnixMilli()},
		{Role: "assistant", Content: string(bigContent), TS: time.Now().UnixMilli()},
	}
	if err := mgr.AppendMessages(key, msgs); err != nil {
		t.Fatal(err)
	}

	// Touch API call — cache is "active".
	mgr.TouchAPICall(key)

	// GetHistory should NOT prune because cache is still within TTL.
	h, err := mgr.GetHistory(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 4 {
		t.Fatalf("expected 4 messages (no pruning during cache TTL), got %d", len(h))
	}
}

// ---------------------------------------------------------------------------
// Reap — cleans orphaned JSONL files
// ---------------------------------------------------------------------------

func TestReap(t *testing.T) {
	dir := t.TempDir()
	m, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Create an active session (has entry in m.sessions).
	if err := m.AppendMessages("active:key", []Message{
		{Role: "user", Content: "hello", TS: time.Now().UnixMilli()},
	}); err != nil {
		t.Fatal(err)
	}
	m.FlushSave()

	// Grab the active session's file ID.
	m.mu.Lock()
	activeID := m.sessions["active:key"].SessionID
	m.mu.Unlock()

	// 2. Create an orphaned but recent .jsonl file (no session entry).
	recentOrphan := filepath.Join(dir, "recent-orphan.jsonl")
	if err := os.WriteFile(recentOrphan, []byte(`{"role":"user","content":"hi","ts":1}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// 3. Create an orphaned OLD .jsonl file (modify time 8 days ago).
	oldOrphan := filepath.Join(dir, "old-orphan.jsonl")
	if err := os.WriteFile(oldOrphan, []byte(`{"role":"user","content":"stale","ts":1}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(oldOrphan, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	// 4. Create an orphaned OLD .tmp file too.
	oldTmp := filepath.Join(dir, "old-rewrite.tmp")
	if err := os.WriteFile(oldTmp, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldTmp, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	// Run reaper with maxAge = 7 days.
	count, err := m.Reap(7 * 24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// Should have removed old-orphan.jsonl and old-rewrite.tmp.
	if count != 2 {
		t.Errorf("expected 2 files reaped, got %d", count)
	}

	// Active session file must still exist.
	if _, err := os.Stat(filepath.Join(dir, activeID+".jsonl")); err != nil {
		t.Errorf("active session file should still exist: %v", err)
	}

	// Recent orphan must still exist.
	if _, err := os.Stat(recentOrphan); err != nil {
		t.Errorf("recent orphan should still exist: %v", err)
	}

	// Old orphan should be gone.
	if _, err := os.Stat(oldOrphan); !os.IsNotExist(err) {
		t.Errorf("old orphan should have been removed, err=%v", err)
	}

	// Old tmp should be gone.
	if _, err := os.Stat(oldTmp); !os.IsNotExist(err) {
		t.Errorf("old tmp file should have been removed, err=%v", err)
	}
}

func TestCacheTTL_PrunesAfterExpiry(t *testing.T) {
	dir := t.TempDir()
	mgr, err := New(testLogger(), dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mgr.SetPruning(PruningPolicy{
		HardClearRatio:     0.5,
		ModelMaxTokens:     100,
		KeepLastAssistants: 1,
		CacheTTL:           1 * time.Millisecond, // Effectively expired immediately.
	})

	key := "test:cache:2"
	bigContent := make([]byte, 200)
	for i := range bigContent {
		bigContent[i] = 'y'
	}
	msgs := []Message{
		{Role: "user", Content: string(bigContent), TS: time.Now().UnixMilli()},
		{Role: "assistant", Content: string(bigContent), TS: time.Now().UnixMilli()},
		{Role: "user", Content: string(bigContent), TS: time.Now().UnixMilli()},
		{Role: "assistant", Content: string(bigContent), TS: time.Now().UnixMilli()},
	}
	if err := mgr.AppendMessages(key, msgs); err != nil {
		t.Fatal(err)
	}
	mgr.TouchAPICall(key)

	// Wait for cache TTL to expire.
	time.Sleep(5 * time.Millisecond)

	h, err := mgr.GetHistory(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) >= 4 {
		t.Fatalf("expected pruning after cache TTL expired, still have %d messages", len(h))
	}
}
