// Package migrate converts OpenClaw session history to GopherClaw format.
//
// OpenClaw stores sessions as event-sourced JSONL files (one event per line)
// in ~/.openclaw/agents/main/sessions/<sessionId>.jsonl with a sessions.json
// index mapping session keys to session IDs.
//
// GopherClaw stores sessions as flat JSONL files (one Message per line) in
// its own sessions directory.
package migrate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/EMSERO/gopherclaw/internal/session"
)

// Run migrates OpenClaw sessions to GopherClaw format.
// openclawDir defaults to ~/.openclaw if empty.
// targetDir defaults to ~/.gopherclaw/agents/main/sessions if empty.
// Returns the number of sessions successfully migrated.
func Run(openclawDir, targetDir string) (int, error) {
	if openclawDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return 0, fmt.Errorf("get home dir: %w", err)
		}
		openclawDir = filepath.Join(home, ".openclaw")
	}
	if targetDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return 0, fmt.Errorf("get home dir: %w", err)
		}
		targetDir = filepath.Join(home, ".gopherclaw", "agents", "main", "sessions")
	}

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return 0, fmt.Errorf("create target dir: %w", err)
	}

	sessionsJSONPath := filepath.Join(openclawDir, "agents", "main", "sessions", "sessions.json")
	data, err := os.ReadFile(sessionsJSONPath)
	if err != nil {
		return 0, fmt.Errorf("read sessions.json: %w", err)
	}

	// sessions.json maps sessionKey → {sessionId, ...}
	var index map[string]struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(data, &index); err != nil {
		return 0, fmt.Errorf("parse sessions.json: %w", err)
	}

	srcDir := filepath.Join(openclawDir, "agents", "main", "sessions")
	count := 0
	for sessionKey, meta := range index {
		if meta.SessionID == "" {
			continue
		}
		srcPath := filepath.Join(srcDir, meta.SessionID+".jsonl")
		msgs, err := convertSessionFile(srcPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  warning: skipping %q (%s): %v\n", sessionKey, meta.SessionID, err)
			continue
		}
		if len(msgs) == 0 {
			continue
		}

		// Write GopherClaw JSONL — one message per line, safe filename
		outName := sanitizeFilename(meta.SessionID) + ".jsonl"
		outPath := filepath.Join(targetDir, outName)
		if err := writeMessages(outPath, msgs); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: failed writing %q: %v\n", outPath, err)
			continue
		}
		fmt.Printf("  migrated %q → %s (%d messages)\n", sessionKey, outName, len(msgs))
		count++
	}
	return count, nil
}

// convertSessionFile reads an OpenClaw JSONL file and returns GopherClaw messages.
func convertSessionFile(path string) ([]session.Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var msgs []session.Message
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20) // 1MB per line
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		converted, err := convertEvent(line)
		if err != nil {
			// Skip unrecognised events silently
			continue
		}
		msgs = append(msgs, converted...)
	}
	return msgs, scanner.Err()
}

// ocEvent is the outer wrapper for all OpenClaw JSONL events.
type ocEvent struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

// ocMessage is the OpenClaw message payload.
type ocMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Details    struct {
		Aggregated string `json:"aggregated"`
	} `json:"details"`
}

// ocContentBlock represents one item in an OpenClaw content array.
type ocContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// convertEvent parses one OpenClaw JSONL line and returns zero or more GopherClaw messages.
func convertEvent(line string) ([]session.Message, error) {
	var ev ocEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return nil, err
	}
	if ev.Type != "message" {
		return nil, nil // skip non-message events
	}

	var msg ocMessage
	if err := json.Unmarshal(ev.Message, &msg); err != nil {
		return nil, err
	}

	ts := parseTimestamp(ev.Timestamp)

	switch msg.Role {
	case "user":
		text := extractTextContent(msg.Content)
		if text == "" {
			return nil, nil
		}
		return []session.Message{{Role: "user", Content: text, TS: ts}}, nil

	case "assistant":
		// May be text content, tool calls, or mixed
		var blocks []ocContentBlock
		if err := json.Unmarshal(msg.Content, &blocks); err != nil {
			return nil, err
		}
		return convertAssistantBlocks(blocks, ts), nil

	case "toolResult":
		// Prefer details.aggregated; fall back to content[0].text
		content := msg.Details.Aggregated
		if content == "" {
			content = extractTextContent(msg.Content)
		}
		return []session.Message{{
			Role:       "tool",
			Content:    content,
			ToolCallID: msg.ToolCallID,
			Name:       msg.ToolName,
			TS:         ts,
		}}, nil
	}

	return nil, nil
}

// convertAssistantBlocks splits an assistant content block list into one
// GopherClaw message (text + tool_calls are combined into one, as the model
// may emit both in a single turn).
func convertAssistantBlocks(blocks []ocContentBlock, ts int64) []session.Message {
	var textParts []string
	var toolCalls []openai.ToolCall

	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				textParts = append(textParts, b.Text)
			}
		case "toolCall":
			// Marshal arguments back to JSON string (OpenAI format)
			argsJSON := "{}"
			if len(b.Arguments) > 0 {
				argsJSON = string(b.Arguments)
			}
			toolCalls = append(toolCalls, openai.ToolCall{
				ID:   b.ID,
				Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{
					Name:      b.Name,
					Arguments: argsJSON,
				},
			})
		}
	}

	if len(toolCalls) == 0 && len(textParts) == 0 {
		return nil
	}

	m := session.Message{
		Role:      "assistant",
		Content:   strings.Join(textParts, "\n"),
		ToolCalls: toolCalls,
		TS:        ts,
	}
	return []session.Message{m}
}

// extractTextContent joins all text blocks in an OpenClaw content array.
func extractTextContent(raw json.RawMessage) string {
	var blocks []ocContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		// Content may be a plain string in some older events
		var s string
		if err2 := json.Unmarshal(raw, &s); err2 == nil {
			return s
		}
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// parseTimestamp converts an ISO 8601 string to unix milliseconds.
func parseTimestamp(s string) int64 {
	if s == "" {
		return time.Now().UnixMilli()
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Now().UnixMilli()
		}
	}
	return t.UnixMilli()
}

// writeMessages writes GopherClaw messages as JSONL to path.
func writeMessages(path string, msgs []session.Message) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)
	for _, m := range msgs {
		if err := enc.Encode(m); err != nil {
			return err
		}
	}
	return nil
}

// MigrateConfig reads ~/.openclaw/openclaw.json, injects GopherClaw-specific
// fields (e.g. coding-agent CLI subagent), and writes ~/.gopherclaw/config.json.
// Returns the output path. If the target file already exists it is not
// overwritten (returns path, nil).
func MigrateConfig(openclawDir, gopherclawDir string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	if openclawDir == "" {
		openclawDir = filepath.Join(home, ".openclaw")
	}
	if gopherclawDir == "" {
		gopherclawDir = filepath.Join(home, ".gopherclaw")
	}

	srcPath := filepath.Join(openclawDir, "openclaw.json")
	dstPath := filepath.Join(gopherclawDir, "config.json")

	if _, err := os.Stat(dstPath); err == nil {
		return dstPath, nil // already migrated
	}

	data, err := os.ReadFile(srcPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", srcPath, err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", fmt.Errorf("parse %s: %w", srcPath, err)
	}

	// Inject coding-agent CLI subagent if not already defined
	agents, _ := raw["agents"].(map[string]any)
	if agents != nil {
		list, _ := agents["list"].([]any)
		hasCodingAgent := false
		for _, entry := range list {
			if m, ok := entry.(map[string]any); ok && m["id"] == "coding-agent" {
				hasCodingAgent = true
				break
			}
		}
		if !hasCodingAgent {
			list = append(list, map[string]any{
				"id":         "coding-agent",
				"cliCommand": "claude",
				"cliArgs":    []string{"-p", "--dangerously-skip-permissions"},
			})
			agents["list"] = list
		}
	}

	// Rewrite paths from ~/.openclaw to ~/.gopherclaw
	rewriteOpenclawPaths(raw, openclawDir, gopherclawDir)

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal config: %w", err)
	}
	out = append(out, '\n')

	if err := os.MkdirAll(gopherclawDir, 0755); err != nil {
		return "", fmt.Errorf("create %s: %w", gopherclawDir, err)
	}
	if err := os.WriteFile(dstPath, out, 0600); err != nil {
		return "", fmt.Errorf("write %s: %w", dstPath, err)
	}
	return dstPath, nil
}

// rewriteOpenclawPaths walks a parsed JSON config and replaces any string
// values containing the openclawDir path with the gopherclawDir equivalent.
func rewriteOpenclawPaths(obj map[string]any, openclawDir, gopherclawDir string) {
	for k, v := range obj {
		switch val := v.(type) {
		case string:
			if strings.Contains(val, openclawDir) {
				obj[k] = strings.ReplaceAll(val, openclawDir, gopherclawDir)
			}
		case map[string]any:
			rewriteOpenclawPaths(val, openclawDir, gopherclawDir)
		case []any:
			for i, item := range val {
				if s, ok := item.(string); ok && strings.Contains(s, openclawDir) {
					val[i] = strings.ReplaceAll(s, openclawDir, gopherclawDir)
				} else if m, ok := item.(map[string]any); ok {
					rewriteOpenclawPaths(m, openclawDir, gopherclawDir)
				}
			}
		}
	}
}

// MigrateJobsFile copies ~/.openclaw/cron/jobs.json to ~/.gopherclaw/cron/jobs.json
// if the target doesn't exist. Returns the output path and whether a copy was
// made. Returns ("", false, nil) if the source doesn't exist or target already exists.
func MigrateJobsFile(openclawDir, gopherclawDir string) (string, bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, fmt.Errorf("get home dir: %w", err)
	}
	if openclawDir == "" {
		openclawDir = filepath.Join(home, ".openclaw")
	}
	if gopherclawDir == "" {
		gopherclawDir = filepath.Join(home, ".gopherclaw")
	}

	srcPath := filepath.Join(openclawDir, "cron", "jobs.json")
	dstPath := filepath.Join(gopherclawDir, "cron", "jobs.json")

	// Already migrated — nothing to do
	if _, err := os.Stat(dstPath); err == nil {
		return dstPath, false, nil
	}

	data, err := os.ReadFile(srcPath)
	if err != nil {
		return "", false, nil // source doesn't exist, nothing to migrate
	}

	if err := os.MkdirAll(filepath.Dir(dstPath), 0700); err != nil {
		return "", false, fmt.Errorf("create %s: %w", filepath.Dir(dstPath), err)
	}
	if err := os.WriteFile(dstPath, data, 0600); err != nil {
		return "", false, fmt.Errorf("write %s: %w", dstPath, err)
	}
	return dstPath, true, nil
}

// sanitizeFilename replaces characters unsafe for filenames with underscores.
func sanitizeFilename(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			sb.WriteRune('_')
		} else {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
