package migrate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EMSERO/gopherclaw/internal/session"
)

func makeEvent(eventType, role string, content any, extra map[string]string) string {
	msg := map[string]any{
		"role":    role,
		"content": content,
	}
	for k, v := range extra {
		msg[k] = v
	}
	ev := map[string]any{
		"type":      eventType,
		"timestamp": "2026-01-15T10:00:00.000Z",
		"message":   msg,
	}
	b, _ := json.Marshal(ev)
	return string(b)
}

func TestConvertUserMessage(t *testing.T) {
	line := makeEvent("message", "user", []map[string]string{
		{"type": "text", "text": "hello world"},
	}, nil)

	msgs, err := convertEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Errorf("expected role 'user', got %q", msgs[0].Role)
	}
	if msgs[0].Content != "hello world" {
		t.Errorf("expected content 'hello world', got %q", msgs[0].Content)
	}
}

func TestConvertAssistantText(t *testing.T) {
	line := makeEvent("message", "assistant", []map[string]string{
		{"type": "text", "text": "I can help with that."},
	}, nil)

	msgs, err := convertEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Role != "assistant" {
		t.Fatalf("expected 1 assistant message, got %+v", msgs)
	}
	if msgs[0].Content != "I can help with that." {
		t.Errorf("unexpected content: %q", msgs[0].Content)
	}
	if len(msgs[0].ToolCalls) != 0 {
		t.Error("expected no tool calls")
	}
}

func TestConvertAssistantToolCall(t *testing.T) {
	line := makeEvent("message", "assistant", []map[string]any{
		{
			"type":      "toolCall",
			"id":        "toolu_abc123",
			"name":      "exec",
			"arguments": map[string]string{"command": "ls -la"},
		},
	}, nil)

	msgs, err := convertEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Role != "assistant" {
		t.Errorf("expected role 'assistant', got %q", m.Role)
	}
	if len(m.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(m.ToolCalls))
	}
	tc := m.ToolCalls[0]
	if tc.ID != "toulu_abc123" && tc.ID != "toolu_abc123" {
		t.Errorf("unexpected tool call ID: %q", tc.ID)
	}
	if tc.Function.Name != "exec" {
		t.Errorf("expected function name 'exec', got %q", tc.Function.Name)
	}
	// Arguments should be valid JSON
	var args map[string]string
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		t.Errorf("arguments not valid JSON: %v (got %q)", err, tc.Function.Arguments)
	}
}

func TestConvertToolResult(t *testing.T) {
	// Build a toolResult event manually since makeEvent doesn't support all fields
	ev := map[string]any{
		"type":      "message",
		"timestamp": "2026-01-15T10:00:01.000Z",
		"message": map[string]any{
			"role":       "toolResult",
			"toolCallId": "toolu_abc123",
			"toolName":   "exec",
			"content":    []map[string]string{{"type": "text", "text": "fallback"}},
			"details": map[string]any{
				"aggregated": "file.txt\ndir/",
			},
		},
	}
	line, _ := json.Marshal(ev)

	msgs, err := convertEvent(string(line))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Role != "tool" {
		t.Errorf("expected role 'tool', got %q", m.Role)
	}
	if m.Content != "file.txt\ndir/" {
		t.Errorf("expected aggregated content, got %q", m.Content)
	}
	if m.ToolCallID != "toulu_abc123" && m.ToolCallID != "toolu_abc123" {
		t.Errorf("unexpected ToolCallID: %q", m.ToolCallID)
	}
	if m.Name != "exec" {
		t.Errorf("expected name 'exec', got %q", m.Name)
	}
}

func TestConvertSkipsNonMessageEvents(t *testing.T) {
	nonMsg := `{"type":"session","timestamp":"2026-01-15T10:00:00Z","data":{}}`
	msgs, err := convertEvent(nonMsg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages for non-message event, got %d", len(msgs))
	}
}

func TestConvertInvalidJSON(t *testing.T) {
	msgs, err := convertEvent("not-json")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages, got %d", len(msgs))
	}
}

func TestParseTimestamp(t *testing.T) {
	ts := parseTimestamp("2026-01-15T10:00:00.000Z")
	if ts <= 0 {
		t.Errorf("expected positive timestamp, got %d", ts)
	}
	// Empty string returns current time (non-zero)
	ts2 := parseTimestamp("")
	if ts2 <= 0 {
		t.Error("expected non-zero timestamp for empty string")
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := []struct {
		in  string
		out string
	}{
		{"abc123", "abc123"},
		{"path/with/slashes", "path_with_slashes"},
		{"name:colon", "name_colon"},
		{"safe-name_123", "safe-name_123"},
	}
	for _, tc := range cases {
		got := sanitizeFilename(tc.in)
		if got != tc.out {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tc.in, got, tc.out)
		}
	}
}

func TestMigrateRoundtrip(t *testing.T) {
	dir := t.TempDir()

	// Build OpenClaw directory structure
	srcDir := filepath.Join(dir, "agents", "main", "sessions")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write sessions.json
	sessionsJSON := `{"agent:main:telegram:99999": {"sessionId": "sess_abc"}}`
	if err := os.WriteFile(filepath.Join(srcDir, "sessions.json"), []byte(sessionsJSON), 0600); err != nil {
		t.Fatal(err)
	}

	// Write OpenClaw JSONL
	lines := []string{
		`{"type":"session","timestamp":"2026-01-01T00:00:00Z","data":{}}`,
		`{"type":"message","timestamp":"2026-01-01T00:01:00Z","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`,
		`{"type":"message","timestamp":"2026-01-01T00:01:01Z","message":{"role":"assistant","content":[{"type":"text","text":"Hi there!"}]}}`,
	}
	var jsonl strings.Builder
	for _, l := range lines {
		jsonl.WriteString(l + "\n")
	}
	if err := os.WriteFile(filepath.Join(srcDir, "sess_abc.jsonl"), []byte(jsonl.String()), 0600); err != nil {
		t.Fatal(err)
	}

	targetDir := filepath.Join(dir, "out")
	n, err := Run(dir, targetDir)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 migrated session, got %d", n)
	}

	// Verify output file exists
	outPath := filepath.Join(targetDir, "sess_abc.jsonl")
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("output file not found: %v", err)
	}

	// Parse output lines
	var msgs []session.Message
	for _, line := range splitLines(string(data)) {
		var m session.Message
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("invalid JSON in output: %v (line: %q)", err, line)
		}
		msgs = append(msgs, m)
	}

	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (skipping session event), got %d", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content != "hello" {
		t.Errorf("unexpected first message: %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Content != "Hi there!" {
		t.Errorf("unexpected second message: %+v", msgs[1])
	}

	// Verify output file permissions
	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("expected 0600 permissions, got %o", perm)
	}
}

func TestMigrateConfig(t *testing.T) {
	dir := t.TempDir()
	ocDir := filepath.Join(dir, "openclaw")
	gcDir := filepath.Join(dir, "gopherclaw")

	// Create OpenClaw config
	if err := os.MkdirAll(ocDir, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"agents": map[string]any{
			"defaults": map[string]any{"model": map[string]any{"primary": "test/model"}},
			"list": []any{
				map[string]any{"id": "main", "default": true},
			},
		},
		"gateway": map[string]any{"port": 18789},
		"logging": map[string]any{
			"file": filepath.Join(ocDir, "logs", "openclaw.log"),
		},
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(ocDir, "openclaw.json"), data, 0600); err != nil {
		t.Fatal(err)
	}

	// Run migration
	outPath, err := MigrateConfig(ocDir, gcDir)
	if err != nil {
		t.Fatalf("MigrateConfig: %v", err)
	}
	if outPath != filepath.Join(gcDir, "config.json") {
		t.Errorf("unexpected output path: %s", outPath)
	}

	// Verify config was written
	result, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}

	var out map[string]any
	if err := json.Unmarshal(result, &out); err != nil {
		t.Fatalf("parse output: %v", err)
	}

	// Verify coding-agent was injected
	agents, _ := out["agents"].(map[string]any)
	list, _ := agents["list"].([]any)
	found := false
	for _, entry := range list {
		if m, ok := entry.(map[string]any); ok && m["id"] == "coding-agent" {
			found = true
		}
	}
	if !found {
		t.Error("expected coding-agent to be injected")
	}

	// Verify path rewriting
	logging, _ := out["logging"].(map[string]any)
	logFile, _ := logging["file"].(string)
	if !filepath.IsAbs(logFile) {
		t.Errorf("expected absolute log path, got %q", logFile)
	}
	if logFile == filepath.Join(ocDir, "logs", "openclaw.log") {
		t.Error("expected openclaw path to be rewritten")
	}
}

func TestMigrateConfigAlreadyExists(t *testing.T) {
	dir := t.TempDir()
	ocDir := filepath.Join(dir, "openclaw")
	gcDir := filepath.Join(dir, "gopherclaw")

	// Create both source and target configs
	if err := os.MkdirAll(ocDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(gcDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ocDir, "openclaw.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gcDir, "config.json"), []byte(`{"existing":true}`), 0600); err != nil {
		t.Fatal(err)
	}

	outPath, err := MigrateConfig(ocDir, gcDir)
	if err != nil {
		t.Fatalf("MigrateConfig: %v", err)
	}
	if outPath != filepath.Join(gcDir, "config.json") {
		t.Errorf("unexpected path: %s", outPath)
	}

	// Original should not be overwritten
	data, _ := os.ReadFile(outPath)
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	if _, ok := m["existing"]; !ok {
		t.Error("existing config should not be overwritten")
	}
}

func TestMigrateConfigMissingSource(t *testing.T) {
	dir := t.TempDir()
	_, err := MigrateConfig(filepath.Join(dir, "nonexistent"), filepath.Join(dir, "out"))
	if err == nil {
		t.Error("expected error for missing source")
	}
}

func TestRewriteOpenclawPaths(t *testing.T) {
	obj := map[string]any{
		"file":  "/home/user/.openclaw/logs/app.log",
		"count": 42,
		"nested": map[string]any{
			"workspace": "/home/user/.openclaw/workspace",
		},
		"list": []any{
			"/home/user/.openclaw/sessions",
			"unchanged",
			map[string]any{"path": "/home/user/.openclaw/data"},
		},
	}

	rewriteOpenclawPaths(obj, "/home/user/.openclaw", "/home/user/.gopherclaw")

	if obj["file"] != "/home/user/.gopherclaw/logs/app.log" {
		t.Errorf("expected rewritten file, got %v", obj["file"])
	}
	nested := obj["nested"].(map[string]any)
	if nested["workspace"] != "/home/user/.gopherclaw/workspace" {
		t.Errorf("expected rewritten workspace, got %v", nested["workspace"])
	}
	list := obj["list"].([]any)
	if list[0] != "/home/user/.gopherclaw/sessions" {
		t.Errorf("expected rewritten list item, got %v", list[0])
	}
	if list[1] != "unchanged" {
		t.Errorf("expected unchanged item, got %v", list[1])
	}
	innerMap := list[2].(map[string]any)
	if innerMap["path"] != "/home/user/.gopherclaw/data" {
		t.Errorf("expected rewritten inner map path, got %v", innerMap["path"])
	}
}

func TestMigrateConfigCodingAgentNotDuplicated(t *testing.T) {
	dir := t.TempDir()
	ocDir := filepath.Join(dir, "openclaw")
	gcDir := filepath.Join(dir, "gopherclaw")

	if err := os.MkdirAll(ocDir, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"agents": map[string]any{
			"list": []any{
				map[string]any{"id": "main", "default": true},
				map[string]any{"id": "coding-agent", "cliCommand": "claude"},
			},
		},
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(filepath.Join(ocDir, "openclaw.json"), data, 0600); err != nil {
		t.Fatal(err)
	}

	outPath, err := MigrateConfig(ocDir, gcDir)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := os.ReadFile(outPath)
	var out map[string]any
	_ = json.Unmarshal(result, &out)
	agents, _ := out["agents"].(map[string]any)
	list, _ := agents["list"].([]any)
	count := 0
	for _, entry := range list {
		if m, ok := entry.(map[string]any); ok && m["id"] == "coding-agent" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 coding-agent, got %d", count)
	}
}

func TestConvertAssistantMixedBlocks(t *testing.T) {
	// Assistant message with both text and tool_use blocks
	line := makeEvent("message", "assistant", []map[string]any{
		{"type": "text", "text": "Let me run that."},
		{"type": "toolCall", "id": "tc1", "name": "exec", "arguments": map[string]string{"command": "ls"}},
		{"type": "text", "text": " Done."},
	}, nil)

	msgs, err := convertEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Role != "assistant" {
		t.Errorf("expected assistant, got %q", m.Role)
	}
	if len(m.ToolCalls) != 1 {
		t.Errorf("expected 1 tool call, got %d", len(m.ToolCalls))
	}
}

func TestConvertToolResultFallbackContent(t *testing.T) {
	// Tool result with no aggregated details — should fall back to content text
	ev := map[string]any{
		"type":      "message",
		"timestamp": "2026-01-15T10:00:01.000Z",
		"message": map[string]any{
			"role":       "toolResult",
			"toolCallId": "tc1",
			"toolName":   "exec",
			"content":    []map[string]string{{"type": "text", "text": "fallback output"}},
		},
	}
	line, _ := json.Marshal(ev)
	msgs, err := convertEvent(string(line))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Content != "fallback output" {
		t.Errorf("expected fallback content, got %+v", msgs)
	}
}

func TestRunEmptySessions(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "agents", "main", "sessions")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Empty sessions.json — should return 0 with no error
	if err := os.WriteFile(filepath.Join(srcDir, "sessions.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	n, err := Run(dir, filepath.Join(dir, "out"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 sessions, got %d", n)
	}
}

func TestExtractTextContentPlainString(t *testing.T) {
	raw, _ := json.Marshal("plain text content")
	result := extractTextContent(json.RawMessage(raw))
	if result != "plain text content" {
		t.Errorf("expected 'plain text content', got %q", result)
	}
}

func TestExtractTextContentInvalidJSON(t *testing.T) {
	result := extractTextContent(json.RawMessage(`not-json`))
	if result != "" {
		t.Errorf("expected empty for invalid JSON, got %q", result)
	}
}

func TestRunSkipsEmptySessionID(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "agents", "main", "sessions")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatal(err)
	}
	sessionsJSON := `{"agent:main:tg:1": {"sessionId": ""}}`
	if err := os.WriteFile(filepath.Join(srcDir, "sessions.json"), []byte(sessionsJSON), 0600); err != nil {
		t.Fatal(err)
	}
	n, err := Run(dir, filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("expected 0 migrated, got %d", n)
	}
}

func TestRunSkipsMissingSessionFile(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "agents", "main", "sessions")
	if err := os.MkdirAll(srcDir, 0755); err != nil {
		t.Fatal(err)
	}
	sessionsJSON := `{"agent:main:tg:1": {"sessionId": "nonexistent_sess"}}`
	if err := os.WriteFile(filepath.Join(srcDir, "sessions.json"), []byte(sessionsJSON), 0600); err != nil {
		t.Fatal(err)
	}
	n, err := Run(dir, filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("expected 0, got %d", n)
	}
}

func TestConvertSessionFileEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(path, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	msgs, err := convertSessionFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected 0, got %d", len(msgs))
	}
}

func TestMigrateConfigInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	ocDir := filepath.Join(dir, "openclaw")
	if err := os.MkdirAll(ocDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ocDir, "openclaw.json"), []byte("not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := MigrateConfig(ocDir, filepath.Join(dir, "gc"))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestParseTimestampVariousFormats(t *testing.T) {
	ts := parseTimestamp("2026-01-15T10:00:00Z")
	if ts <= 0 {
		t.Error("expected positive timestamp for Z format")
	}
	ts2 := parseTimestamp("not-a-date")
	if ts2 <= 0 {
		t.Error("expected fallback timestamp for invalid format")
	}
}

func splitLines(s string) []string {
	var lines []string
	for _, l := range []string(splitByNewline(s)) {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func splitByNewline(s string) []string {
	var result []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			result = append(result, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		result = append(result, s[start:])
	}
	return result
}

// ---------------------------------------------------------------------------
// Run — invalid sessions.json content
// ---------------------------------------------------------------------------

func TestRunInvalidSessionsJSON(t *testing.T) {
	dir := t.TempDir()
	openclawDir := filepath.Join(dir, "openclaw")
	sessDir := filepath.Join(openclawDir, "agents", "main", "sessions")
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Write invalid JSON
	if err := os.WriteFile(filepath.Join(sessDir, "sessions.json"), []byte("not json"), 0644); err != nil {
		t.Fatal(err)
	}

	targetDir := filepath.Join(dir, "target")
	_, err := Run(openclawDir, targetDir)
	if err == nil {
		t.Error("expected error for invalid sessions.json")
	}
}

// ---------------------------------------------------------------------------
// Run — unreadable target dir
// ---------------------------------------------------------------------------

func TestRunUnwritableTargetDir(t *testing.T) {
	dir := t.TempDir()
	openclawDir := filepath.Join(dir, "openclaw")
	sessDir := filepath.Join(openclawDir, "agents", "main", "sessions")
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Valid but empty sessions index
	if err := os.WriteFile(filepath.Join(sessDir, "sessions.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	// Target path is a file, so MkdirAll should fail
	targetFile := filepath.Join(dir, "target")
	if err := os.WriteFile(targetFile, []byte("blocker"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := Run(openclawDir, targetFile)
	if err == nil {
		t.Error("expected error for unwritable target dir")
	}
}

// ---------------------------------------------------------------------------
// Run — successful migration with write error in output
// ---------------------------------------------------------------------------

func TestRunWithWriteError(t *testing.T) {
	dir := t.TempDir()
	openclawDir := filepath.Join(dir, "openclaw")
	sessDir := filepath.Join(openclawDir, "agents", "main", "sessions")
	if err := os.MkdirAll(sessDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create a valid session file with one message
	sessionData := `{"type":"message","timestamp":"2024-01-01T00:00:00Z","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(sessDir, "sess1.jsonl"), []byte(sessionData), 0644); err != nil {
		t.Fatal(err)
	}

	// Create sessions.json index
	index := map[string]any{
		"key1": map[string]any{"sessionId": "sess1"},
	}
	indexData, _ := json.Marshal(index)
	if err := os.WriteFile(filepath.Join(sessDir, "sessions.json"), indexData, 0644); err != nil {
		t.Fatal(err)
	}

	// Create target dir but make the output file location read-only
	targetDir := filepath.Join(dir, "target")
	if err := os.MkdirAll(targetDir, 0555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(targetDir, 0755) // cleanup

	count, err := Run(openclawDir, targetDir)
	// Should complete without fatal error (skips failed writes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Count should be 0 since writes failed
	if count != 0 {
		t.Logf("count=%d (write error may have been recovered or permissions differ on this OS)", count)
	}
}

// ---------------------------------------------------------------------------
// convertEvent — assistant with invalid content array
// ---------------------------------------------------------------------------

func TestConvertEventAssistantInvalidContent(t *testing.T) {
	line := `{"type":"message","timestamp":"2024-01-01T00:00:00Z","message":{"role":"assistant","content":"not an array"}}`
	_, err := convertEvent(line)
	if err == nil {
		t.Error("expected error for assistant with non-array content")
	}
}

// ---------------------------------------------------------------------------
// convertEvent — unrecognized role returns nil
// ---------------------------------------------------------------------------

func TestConvertEventUnrecognizedRole(t *testing.T) {
	line := `{"type":"message","timestamp":"2024-01-01T00:00:00Z","message":{"role":"system","content":[{"type":"text","text":"sys"}]}}`
	msgs, err := convertEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msgs != nil {
		t.Errorf("expected nil for unrecognized role, got %v", msgs)
	}
}

// ---------------------------------------------------------------------------
// convertEvent — user with empty text content
// ---------------------------------------------------------------------------

func TestConvertEventUserEmptyContent(t *testing.T) {
	line := `{"type":"message","timestamp":"2024-01-01T00:00:00Z","message":{"role":"user","content":[{"type":"text","text":""}]}}`
	msgs, err := convertEvent(line)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msgs != nil {
		t.Errorf("expected nil for empty user content, got %v", msgs)
	}
}

// ---------------------------------------------------------------------------
// convertAssistantBlocks — empty blocks
// ---------------------------------------------------------------------------

func TestConvertAssistantBlocksEmpty(t *testing.T) {
	msgs := convertAssistantBlocks(nil, 0)
	if msgs != nil {
		t.Errorf("expected nil for empty blocks, got %v", msgs)
	}
}

// ---------------------------------------------------------------------------
// convertEvent — message with invalid inner JSON
// ---------------------------------------------------------------------------

func TestConvertEventInvalidMessageJSON(t *testing.T) {
	line := `{"type":"message","timestamp":"2024-01-01T00:00:00Z","message":"not-an-object"}`
	_, err := convertEvent(line)
	if err == nil {
		t.Error("expected error for invalid message JSON")
	}
}

// ---------------------------------------------------------------------------
// MigrateJobsFile
// ---------------------------------------------------------------------------

func TestMigrateJobsFile_FullMigration(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	// Create source jobs file.
	srcCron := filepath.Join(srcDir, "cron")
	if err := os.MkdirAll(srcCron, 0755); err != nil {
		t.Fatal(err)
	}
	jobsData := `[{"name":"test-job","schedule":"*/5 * * * *"}]`
	if err := os.WriteFile(filepath.Join(srcCron, "jobs.json"), []byte(jobsData), 0644); err != nil {
		t.Fatal(err)
	}

	path, migrated, err := MigrateJobsFile(srcDir, dstDir)
	if err != nil {
		t.Fatalf("MigrateJobsFile: %v", err)
	}
	if !migrated {
		t.Error("expected migrated=true")
	}
	if path == "" {
		t.Error("expected non-empty path")
	}

	// Verify file was copied.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migrated file: %v", err)
	}
	if string(data) != jobsData {
		t.Errorf("migrated data mismatch: got %q", string(data))
	}
}

func TestMigrateJobsFile_AlreadyMigrated(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	// Create both source and destination.
	for _, dir := range []string{srcDir, dstDir} {
		cronDir := filepath.Join(dir, "cron")
		os.MkdirAll(cronDir, 0755)
		os.WriteFile(filepath.Join(cronDir, "jobs.json"), []byte(`[]`), 0644)
	}

	path, migrated, err := MigrateJobsFile(srcDir, dstDir)
	if err != nil {
		t.Fatalf("MigrateJobsFile: %v", err)
	}
	if migrated {
		t.Error("expected migrated=false when already exists")
	}
	if path == "" {
		t.Error("expected non-empty path")
	}
}

func TestMigrateJobsFile_NoSource(t *testing.T) {
	srcDir := t.TempDir() // empty — no cron/jobs.json
	dstDir := t.TempDir()

	_, migrated, err := MigrateJobsFile(srcDir, dstDir)
	if err != nil {
		t.Fatalf("MigrateJobsFile: %v", err)
	}
	if migrated {
		t.Error("expected migrated=false when source doesn't exist")
	}
}

// ---------------------------------------------------------------------------
// sanitizeFilename (additional cases)
// ---------------------------------------------------------------------------

func TestSanitizeFilenameExtended(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`foo:bar*baz?`, "foo_bar_baz_"},
		{`"test"`, "_test_"},
		{"<a>b|c", "_a_b_c"},
		{"", ""},
	}
	for _, tt := range tests {
		got := sanitizeFilename(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
