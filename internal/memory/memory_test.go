package memory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMemoryMD_EmptyWorkspace(t *testing.T) {
	result := LoadMemoryMD("")
	if result != "" {
		t.Errorf("expected empty string for empty workspace, got %q", result)
	}
}

func TestLoadMemoryMD_NonexistentWorkspace(t *testing.T) {
	result := LoadMemoryMD("/nonexistent/path/that/does/not/exist")
	if result != "" {
		t.Errorf("expected empty string for nonexistent workspace, got %q", result)
	}
}

func TestLoadMemoryMD_WorkspaceWithoutMemoryFile(t *testing.T) {
	dir := t.TempDir()
	result := LoadMemoryMD(dir)
	if result != "" {
		t.Errorf("expected empty string when MEMORY.md does not exist, got %q", result)
	}
}

func TestLoadMemoryMD_ValidFile(t *testing.T) {
	dir := t.TempDir()
	content := "# Memory\n\nThis is a test memory file.\n\n- Item 1\n- Item 2\n"
	err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(content), 0644)
	if err != nil {
		t.Fatalf("failed to write MEMORY.md: %v", err)
	}

	result := LoadMemoryMD(dir)
	if result != content {
		t.Errorf("expected %q, got %q", content, result)
	}
}

func TestLoadMemoryMD_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(""), 0644)
	if err != nil {
		t.Fatalf("failed to write empty MEMORY.md: %v", err)
	}

	result := LoadMemoryMD(dir)
	if result != "" {
		t.Errorf("expected empty string for empty file, got %q", result)
	}
}

func TestLoadMemoryMD_LargeContent(t *testing.T) {
	dir := t.TempDir()
	// Build a reasonably large memory file.
	var content string
	for range 100 {
		content += "- This is line number something in the memory file.\n"
	}
	err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(content), 0644)
	if err != nil {
		t.Fatalf("failed to write MEMORY.md: %v", err)
	}

	result := LoadMemoryMD(dir)
	if result != content {
		t.Errorf("content mismatch: got %d bytes, want %d bytes", len(result), len(content))
	}
}

func TestLoadMemoryMD_UnreadableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "MEMORY.md")
	err := os.WriteFile(path, []byte("secret"), 0644)
	if err != nil {
		t.Fatalf("failed to write MEMORY.md: %v", err)
	}
	// Remove read permission.
	if err := os.Chmod(path, 0000); err != nil {
		t.Skipf("cannot change file permissions: %v", err)
	}
	defer os.Chmod(path, 0644) // restore for cleanup

	result := LoadMemoryMD(dir)
	if result != "" {
		t.Errorf("expected empty string for unreadable file, got %q", result)
	}
}

// ---------------------------------------------------------------------------
// LoadHeartbeatMD
// ---------------------------------------------------------------------------

func TestLoadHeartbeatMD_EmptyWorkspace(t *testing.T) {
	result := LoadHeartbeatMD("")
	if result != "" {
		t.Errorf("expected empty string for empty workspace, got %q", result)
	}
}

func TestLoadHeartbeatMD_NonexistentWorkspace(t *testing.T) {
	result := LoadHeartbeatMD("/nonexistent/path/that/does/not/exist")
	if result != "" {
		t.Errorf("expected empty string for nonexistent workspace, got %q", result)
	}
}

func TestLoadHeartbeatMD_WorkspaceWithoutFile(t *testing.T) {
	dir := t.TempDir()
	result := LoadHeartbeatMD(dir)
	if result != "" {
		t.Errorf("expected empty string when HEARTBEAT.md missing, got %q", result)
	}
}

func TestLoadHeartbeatMD_ValidFile(t *testing.T) {
	dir := t.TempDir()
	content := "# Heartbeat\n\nCron context for scheduled tasks.\n"
	if err := os.WriteFile(filepath.Join(dir, "HEARTBEAT.md"), []byte(content), 0644); err != nil {
		t.Fatalf("failed to write HEARTBEAT.md: %v", err)
	}
	result := LoadHeartbeatMD(dir)
	if result != content {
		t.Errorf("expected %q, got %q", content, result)
	}
}

func TestLoadHeartbeatMD_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "HEARTBEAT.md"), []byte(""), 0644); err != nil {
		t.Fatalf("failed to write empty HEARTBEAT.md: %v", err)
	}
	result := LoadHeartbeatMD(dir)
	if result != "" {
		t.Errorf("expected empty string for empty file, got %q", result)
	}
}

func TestLoadHeartbeatMD_UnreadableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "HEARTBEAT.md")
	if err := os.WriteFile(path, []byte("secret"), 0644); err != nil {
		t.Fatalf("failed to write HEARTBEAT.md: %v", err)
	}
	if err := os.Chmod(path, 0000); err != nil {
		t.Skipf("cannot change file permissions: %v", err)
	}
	defer os.Chmod(path, 0644)

	result := LoadHeartbeatMD(dir)
	if result != "" {
		t.Errorf("expected empty string for unreadable file, got %q", result)
	}
}
