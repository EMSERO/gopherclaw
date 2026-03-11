package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckPathAllowedNoRestriction(t *testing.T) {
	// Empty allowPaths means no restriction
	if err := checkPathAllowed("/any/path/at/all", nil); err != nil {
		t.Errorf("expected no error with empty allowPaths, got: %v", err)
	}
	if err := checkPathAllowed("/etc/passwd", []string{}); err != nil {
		t.Errorf("expected no error with empty allowPaths slice, got: %v", err)
	}
}

func TestCheckPathAllowedAllowed(t *testing.T) {
	allowed := []string{"/home/user/workspace", "/tmp"}

	cases := []string{
		"/home/user/workspace/file.txt",
		"/home/user/workspace/sub/dir/file.go",
		"/home/user/workspace", // exact match
		"/tmp/scratch.txt",
	}
	for _, path := range cases {
		if err := checkPathAllowed(path, allowed); err != nil {
			t.Errorf("expected %q to be allowed, got: %v", path, err)
		}
	}
}

func TestCheckPathAllowedDenied(t *testing.T) {
	allowed := []string{"/home/user/workspace"}

	cases := []string{
		"/etc/passwd",
		"/home/user/other",
		"/home/user/workspaceExtra", // not a subdir, just a similarly named path
	}
	for _, path := range cases {
		if err := checkPathAllowed(path, allowed); err == nil {
			t.Errorf("expected %q to be denied, but was allowed", path)
		}
	}
}

func TestCheckPathAllowedTraversal(t *testing.T) {
	allowed := []string{"/tmp/safe"}

	// Traversal attempts that should be blocked after filepath.Clean
	cases := []string{
		"/tmp/safe/../../etc/passwd",
		"/tmp/safe/../../../root/.ssh/id_rsa",
	}
	for _, path := range cases {
		if err := checkPathAllowed(path, allowed); err == nil {
			t.Errorf("expected path traversal %q to be denied", path)
		}
	}
}

func TestReadFileToolAllowPath(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "allowed.txt")
	if err := os.WriteFile(f, []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}

	tool := &ReadFileTool{AllowPaths: []string{dir}}
	args, _ := json.Marshal(map[string]string{"path": f})
	result := tool.Run(context.Background(), string(args))
	if result != "content" {
		t.Errorf("expected 'content', got %q", result)
	}

	// Outside allowed path
	tool2 := &ReadFileTool{AllowPaths: []string{"/nonexistent/allowed"}}
	result2 := tool2.Run(context.Background(), string(args))
	if !strings.Contains(result2, "not in the configured allowed paths") {
		t.Errorf("expected denial error, got %q", result2)
	}
}

func TestWriteFileToolAllowPath(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "out.txt")

	tool := &WriteFileTool{AllowPaths: []string{dir}}
	args, _ := json.Marshal(map[string]string{"path": f, "content": "hello"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "wrote") {
		t.Errorf("expected write success, got %q", result)
	}

	// Outside allowed path
	tool2 := &WriteFileTool{AllowPaths: []string{"/nonexistent/allowed"}}
	result2 := tool2.Run(context.Background(), string(args))
	if !strings.Contains(result2, "not in the configured allowed paths") {
		t.Errorf("expected denial error, got %q", result2)
	}
}

func TestListDirToolAllowPath(t *testing.T) {
	dir := t.TempDir()

	tool := &ListDirTool{AllowPaths: []string{dir}}
	args, _ := json.Marshal(map[string]string{"path": dir})
	result := tool.Run(context.Background(), string(args))
	// Empty dir is fine — no denial error
	if strings.Contains(result, "not in the configured allowed paths") {
		t.Errorf("expected list success, got denial: %q", result)
	}

	// Outside allowed path
	tool2 := &ListDirTool{AllowPaths: []string{"/nonexistent/allowed"}}
	result2 := tool2.Run(context.Background(), string(args))
	if !strings.Contains(result2, "not in the configured allowed paths") {
		t.Errorf("expected denial error, got %q", result2)
	}
}

func TestExecDenyCommand(t *testing.T) {
	tool := &ExecTool{
		DefaultTimeout: 5e9, // 5 seconds
		DenyCommands:   []string{"rm -rf", "dd if="},
	}

	cases := []struct {
		cmd   string
		deny  bool
	}{
		{"echo hello", false},
		{"rm -rf /tmp/test", true},
		{"dd if=/dev/zero of=/tmp/x", true},
		{"ls -la", false},
	}

	for _, tc := range cases {
		args, _ := json.Marshal(map[string]string{"command": tc.cmd})
		result := tool.Run(context.Background(), string(args))
		denied := strings.Contains(result, "denied pattern")
		if denied != tc.deny {
			t.Errorf("command %q: expected deny=%v, got result %q", tc.cmd, tc.deny, result)
		}
	}
}
