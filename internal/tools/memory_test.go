package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemoryAppendToolName(t *testing.T) {
	tool := &MemoryAppendTool{}
	if tool.Name() != "memory_append" {
		t.Errorf("expected name memory_append, got %s", tool.Name())
	}
	var m map[string]any
	if err := json.Unmarshal(tool.Schema(), &m); err != nil {
		t.Errorf("invalid schema: %v", err)
	}
}

func TestMemoryGetToolName(t *testing.T) {
	tool := &MemoryGetTool{}
	if tool.Name() != "memory_get" {
		t.Errorf("expected name memory_get, got %s", tool.Name())
	}
	var m map[string]any
	if err := json.Unmarshal(tool.Schema(), &m); err != nil {
		t.Errorf("invalid schema: %v", err)
	}
}

func TestMemoryAppendNoWorkspace(t *testing.T) {
	tool := &MemoryAppendTool{Workspace: ""}
	args, _ := json.Marshal(memoryAppendInput{Content: "test"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "no workspace") {
		t.Errorf("expected no workspace error, got %q", result)
	}
}

func TestMemoryAppendEmptyContent(t *testing.T) {
	tool := &MemoryAppendTool{Workspace: t.TempDir()}
	args, _ := json.Marshal(memoryAppendInput{Content: ""})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "content is required") {
		t.Errorf("expected content required error, got %q", result)
	}
}

func TestMemoryAppendInvalidJSON(t *testing.T) {
	tool := &MemoryAppendTool{Workspace: t.TempDir()}
	result := tool.Run(context.Background(), "not json")
	if !strings.Contains(result, "invalid arguments") {
		t.Errorf("expected invalid arguments error, got %q", result)
	}
}

func TestMemoryAppendMemoryMD(t *testing.T) {
	ws := t.TempDir()
	tool := &MemoryAppendTool{Workspace: ws}
	args, _ := json.Marshal(memoryAppendInput{Content: "remember this", File: "memory.md"})
	result := tool.Run(context.Background(), string(args))
	if result != "ok" {
		t.Errorf("expected ok, got %q", result)
	}
	data, err := os.ReadFile(filepath.Join(ws, "MEMORY.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "remember this") {
		t.Errorf("expected content in MEMORY.md, got %q", string(data))
	}
}

func TestMemoryAppendDaily(t *testing.T) {
	ws := t.TempDir()
	tool := &MemoryAppendTool{Workspace: ws}
	args, _ := json.Marshal(memoryAppendInput{Content: "daily note", File: "daily"})
	result := tool.Run(context.Background(), string(args))
	if result != "ok" {
		t.Errorf("expected ok, got %q", result)
	}
	today := time.Now().Format("2006-01-02")
	path := filepath.Join(ws, "memory", today+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "daily note") {
		t.Errorf("expected daily note, got %q", string(data))
	}
}

func TestMemoryAppendDefaultDaily(t *testing.T) {
	ws := t.TempDir()
	tool := &MemoryAppendTool{Workspace: ws}
	args, _ := json.Marshal(memoryAppendInput{Content: "default file"})
	result := tool.Run(context.Background(), string(args))
	if result != "ok" {
		t.Errorf("expected ok, got %q", result)
	}
	today := time.Now().Format("2006-01-02")
	path := filepath.Join(ws, "memory", today+".md")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("daily file not created: %v", err)
	}
}

func TestMemoryGetNoWorkspace(t *testing.T) {
	tool := &MemoryGetTool{Workspace: ""}
	args, _ := json.Marshal(memoryGetInput{File: "memory.md"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "no workspace") {
		t.Errorf("expected no workspace error, got %q", result)
	}
}

func TestMemoryGetInvalidJSON(t *testing.T) {
	tool := &MemoryGetTool{Workspace: t.TempDir()}
	result := tool.Run(context.Background(), "bad json")
	if !strings.Contains(result, "invalid arguments") {
		t.Errorf("expected invalid arguments error, got %q", result)
	}
}

func TestMemoryGetMemoryMD(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "MEMORY.md"), []byte("my memory"), 0644); err != nil {
		t.Fatal(err)
	}
	tool := &MemoryGetTool{Workspace: ws}
	args, _ := json.Marshal(memoryGetInput{File: "memory.md"})
	result := tool.Run(context.Background(), string(args))
	if result != "my memory" {
		t.Errorf("expected 'my memory', got %q", result)
	}
}

func TestMemoryGetNotExists(t *testing.T) {
	ws := t.TempDir()
	tool := &MemoryGetTool{Workspace: ws}
	args, _ := json.Marshal(memoryGetInput{File: "memory.md"})
	result := tool.Run(context.Background(), string(args))
	if result != "(no content)" {
		t.Errorf("expected '(no content)', got %q", result)
	}
}

func TestMemoryGetDaily(t *testing.T) {
	ws := t.TempDir()
	today := time.Now().Format("2006-01-02")
	dir := filepath.Join(ws, "memory")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, today+".md"), []byte("today's log"), 0644); err != nil {
		t.Fatal(err)
	}
	tool := &MemoryGetTool{Workspace: ws}
	args, _ := json.Marshal(memoryGetInput{File: "daily"})
	result := tool.Run(context.Background(), string(args))
	if result != "today's log" {
		t.Errorf("expected 'today's log', got %q", result)
	}
}

func TestMemoryGetDateFile(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, "memory")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "2026-01-15.md"), []byte("old log"), 0644); err != nil {
		t.Fatal(err)
	}
	tool := &MemoryGetTool{Workspace: ws}
	args, _ := json.Marshal(memoryGetInput{File: "2026-01-15"})
	result := tool.Run(context.Background(), string(args))
	if result != "old log" {
		t.Errorf("expected 'old log', got %q", result)
	}
}

func TestMemoryGetPathTraversal(t *testing.T) {
	ws := t.TempDir()
	tool := &MemoryGetTool{Workspace: ws}
	args, _ := json.Marshal(memoryGetInput{File: "../../etc/passwd"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "path traversal") {
		t.Errorf("expected path traversal error, got %q", result)
	}
}

func TestMemoryGetLineRange(t *testing.T) {
	ws := t.TempDir()
	content := "line1\nline2\nline3\nline4\nline5"
	if err := os.WriteFile(filepath.Join(ws, "MEMORY.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	tool := &MemoryGetTool{Workspace: ws}

	// Lines 2-4
	args, _ := json.Marshal(memoryGetInput{File: "memory.md", FromLine: 2, ToLine: 4})
	result := tool.Run(context.Background(), string(args))
	if result != "line2\nline3\nline4" {
		t.Errorf("expected lines 2-4, got %q", result)
	}

	// FromLine beyond file length
	args, _ = json.Marshal(memoryGetInput{File: "memory.md", FromLine: 100})
	result = tool.Run(context.Background(), string(args))
	if result != "(no content)" {
		t.Errorf("expected '(no content)' for out of range, got %q", result)
	}
}

func TestMemoryAppendThenGet(t *testing.T) {
	ws := t.TempDir()
	appendTool := &MemoryAppendTool{Workspace: ws}
	getTool := &MemoryGetTool{Workspace: ws}

	// Append twice
	args1, _ := json.Marshal(memoryAppendInput{Content: "first entry", File: "memory.md"})
	appendTool.Run(context.Background(), string(args1))
	args2, _ := json.Marshal(memoryAppendInput{Content: "second entry", File: "memory.md"})
	appendTool.Run(context.Background(), string(args2))

	// Get
	getArgs, _ := json.Marshal(memoryGetInput{File: "memory.md"})
	result := getTool.Run(context.Background(), string(getArgs))
	if !strings.Contains(result, "first entry") || !strings.Contains(result, "second entry") {
		t.Errorf("expected both entries, got %q", result)
	}
}
