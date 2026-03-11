package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/EMSERO/gopherclaw/internal/agentapi"
)

// Tool is an alias for agentapi.Tool.
type Tool = agentapi.Tool

// resolveSymlinks resolves symlinks in path, handling the case where the path
// (or a suffix of it) does not yet exist on disk. It walks up to find the
// deepest existing ancestor, resolves that via EvalSymlinks, then appends the
// remaining unresolved tail. This prevents symlink escape while still allowing
// boundary checks on paths that have not been created yet (e.g. write_file).
func resolveSymlinks(path string) string {
	clean := filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(clean)
	if err == nil {
		return resolved
	}
	// Walk upward until we find an existing ancestor.
	dir := clean
	var tail []string
	for {
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached filesystem root without finding existing path.
			return clean
		}
		tail = append([]string{filepath.Base(dir)}, tail...)
		dir = parent
		resolved, err = filepath.EvalSymlinks(dir)
		if err == nil {
			return filepath.Join(append([]string{resolved}, tail...)...)
		}
	}
}

// checkPathAllowed verifies that path is within one of the allowed directories.
// If allowPaths is empty, all paths are permitted (backward-compatible default).
// Symlinks are resolved via filepath.EvalSymlinks so that a symlink pointing
// outside the allowed boundaries cannot bypass the check.
func checkPathAllowed(path string, allowPaths []string) error {
	if len(allowPaths) == 0 {
		return nil
	}
	clean := resolveSymlinks(path)
	for _, allowed := range allowPaths {
		cleanAllowed := resolveSymlinks(allowed)
		if clean == cleanAllowed || strings.HasPrefix(clean, cleanAllowed+string(filepath.Separator)) {
			return nil
		}
	}
	return fmt.Errorf("path %q is not in the configured allowed paths", path)
}

// ReadFileTool reads a file and returns its contents.
type ReadFileTool struct {
	AllowPaths []string // empty = no restriction
}

type readFileInput struct {
	Path string `json:"path"`
}

func (t *ReadFileTool) Name() string { return "read_file" }

func (t *ReadFileTool) Description() string {
	return "Read the contents of a file at the given path, optionally reading a specific line range."
}

func (t *ReadFileTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Absolute or relative file path"}
		},
		"required": ["path"]
	}`)
}

func (t *ReadFileTool) Run(_ context.Context, argsJSON string) string {
	var in readFileInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	if err := checkPathAllowed(in.Path, t.AllowPaths); err != nil {
		return fmt.Sprintf("error: %v", err)
	}

	const maxRead = 200_000

	f, err := os.Open(in.Path)
	if err != nil {
		return fmt.Sprintf("error reading %s: %v", in.Path, err)
	}
	defer func() { _ = f.Close() }()

	// Read up to maxRead+1 bytes to detect truncation without loading the whole file.
	buf := make([]byte, maxRead+1)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return fmt.Sprintf("error reading %s: %v", in.Path, err)
	}

	if n <= maxRead {
		return string(buf[:n])
	}

	// File is larger than maxRead — get actual size for the message.
	info, statErr := f.Stat()
	totalSize := int64(n) // fallback if stat fails
	if statErr == nil {
		totalSize = info.Size()
	}
	dropped := totalSize - maxRead
	return string(buf[:maxRead]) + fmt.Sprintf(
		"\n[... truncated: showing 200,000 of %d chars (%d dropped). "+
			"For large files, use exec with head/tail/sed to process specific sections.]", totalSize, dropped)
}

// WriteFileTool writes content to a file.
type WriteFileTool struct {
	AllowPaths []string // empty = no restriction
}

type writeFileInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (t *WriteFileTool) Name() string { return "write_file" }

func (t *WriteFileTool) Description() string {
	return "Create or overwrite a file at the given path with the specified content."
}

func (t *WriteFileTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path":    {"type": "string", "description": "File path to write"},
			"content": {"type": "string", "description": "Content to write"}
		},
		"required": ["path", "content"]
	}`)
}

func (t *WriteFileTool) Run(_ context.Context, argsJSON string) string {
	var in writeFileInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	if err := checkPathAllowed(in.Path, t.AllowPaths); err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(in.Path), 0755); err != nil {
		return fmt.Sprintf("error creating dirs: %v", err)
	}
	if err := os.WriteFile(in.Path, []byte(in.Content), 0644); err != nil {
		return fmt.Sprintf("error writing %s: %v", in.Path, err)
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path)
}

// ListDirTool lists a directory's contents.
type ListDirTool struct {
	AllowPaths []string // empty = no restriction
}

type listDirInput struct {
	Path string `json:"path"`
}

func (t *ListDirTool) Name() string { return "list_dir" }

func (t *ListDirTool) Description() string {
	return "List the contents of a directory, showing files and subdirectories."
}

func (t *ListDirTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Directory path to list"}
		},
		"required": ["path"]
	}`)
}

func (t *ListDirTool) Run(_ context.Context, argsJSON string) string {
	var in listDirInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	if err := checkPathAllowed(in.Path, t.AllowPaths); err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	entries, err := os.ReadDir(in.Path)
	if err != nil {
		return fmt.Sprintf("error listing %s: %v", in.Path, err)
	}
	if len(entries) == 0 {
		return "(empty directory)"
	}

	var result strings.Builder
	fmt.Fprintf(&result, "%s:\n", in.Path)
	for _, e := range entries {
		info, _ := e.Info()
		size := int64(0)
		if info != nil {
			size = info.Size()
		}
		kind := "file"
		if e.IsDir() {
			kind = "dir "
		}
		fmt.Fprintf(&result, "  %s  %-8d  %s\n", kind, size, e.Name())
	}
	return result.String()
}
