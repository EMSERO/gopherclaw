package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MemoryAppendTool appends content to the agent's memory files.
type MemoryAppendTool struct {
	Workspace string
}

type memoryAppendInput struct {
	Content string `json:"content"`
	File    string `json:"file"` // "memory.md" or "daily" (default)
}

func (t *MemoryAppendTool) Name() string { return "memory_append" }

func (t *MemoryAppendTool) Description() string {
	return "Append content to the agent's persistent memory files (MEMORY.md for long-term or daily log)."
}

func (t *MemoryAppendTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"content": {"type": "string", "description": "Content to append"},
			"file": {"type": "string", "enum": ["memory.md", "daily"], "description": "Target file: 'memory.md' for persistent memory or 'daily' for today's log (default: daily)"}
		},
		"required": ["content"]
	}`)
}

func (t *MemoryAppendTool) Run(_ context.Context, argsJSON string) string {
	var in memoryAppendInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err)
	}
	if t.Workspace == "" {
		return "error: no workspace configured"
	}
	if in.Content == "" {
		return "error: content is required"
	}

	var path string
	if in.File == "memory.md" {
		path = filepath.Join(t.Workspace, "MEMORY.md")
	} else {
		today := time.Now().Format("2006-01-02")
		dir := filepath.Join(t.Workspace, "memory")
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Sprintf("error: create memory dir: %v", err)
		}
		path = filepath.Join(dir, today+".md")
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Sprintf("error: open file: %v", err)
	}
	defer func() { _ = f.Close() }()

	content := strings.TrimRight(in.Content, "\n") + "\n"
	if _, err := f.WriteString(content); err != nil {
		return fmt.Sprintf("error: write: %v", err)
	}
	return "ok"
}

// MemoryGetTool reads content from the agent's memory files.
type MemoryGetTool struct {
	Workspace string
}

type memoryGetInput struct {
	File     string `json:"file"`
	FromLine int    `json:"from_line,omitempty"`
	ToLine   int    `json:"to_line,omitempty"`
}

func (t *MemoryGetTool) Name() string { return "memory_get" }

func (t *MemoryGetTool) Description() string {
	return "Read content from the agent's persistent memory files (MEMORY.md or daily logs)."
}

func (t *MemoryGetTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"file": {"type": "string", "description": "File to read: 'memory.md', 'daily' (today), or 'YYYY-MM-DD' (past daily log)"},
			"from_line": {"type": "integer", "description": "Start line 1-indexed (optional)"},
			"to_line": {"type": "integer", "description": "End line 1-indexed (optional)"}
		},
		"required": ["file"]
	}`)
}

func (t *MemoryGetTool) Run(_ context.Context, argsJSON string) string {
	var in memoryGetInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err)
	}
	if t.Workspace == "" {
		return "error: no workspace configured"
	}

	var path string
	switch in.File {
	case "memory.md":
		path = filepath.Join(t.Workspace, "MEMORY.md")
	case "daily":
		today := time.Now().Format("2006-01-02")
		path = filepath.Join(t.Workspace, "memory", today+".md")
	default:
		// Assume YYYY-MM-DD format; guard against path traversal
		path = filepath.Join(t.Workspace, "memory", in.File+".md")
		// Resolve symlinks to prevent escape via symlinked directories
		absPath := resolveSymlinks(path)
		absWS := resolveSymlinks(t.Workspace)
		if !strings.HasPrefix(absPath, absWS+string(filepath.Separator)) {
			return "error: path traversal not allowed"
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "(no content)"
		}
		return fmt.Sprintf("error: read file: %v", err)
	}

	if in.FromLine <= 0 && in.ToLine <= 0 {
		return string(data)
	}

	lines := strings.Split(string(data), "\n")
	from := max(in.FromLine-1, 0)
	to := in.ToLine
	if to <= 0 || to > len(lines) {
		to = len(lines)
	}
	if from >= len(lines) {
		return "(no content)"
	}
	return strings.Join(lines[from:to], "\n")
}
