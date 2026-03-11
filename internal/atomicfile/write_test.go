package atomicfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFile_Basic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	data := []byte(`{"hello":"world"}`)
	if err := WriteFile(path, data, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("got %q, want %q", got, data)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0644 {
		t.Errorf("perm = %v, want 0644", info.Mode().Perm())
	}
}

func TestWriteFile_OverwritePreservesOnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	original := []byte("original content")
	if err := os.WriteFile(path, original, 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Writing to a read-only directory should fail at CreateTemp
	roDir := filepath.Join(dir, "readonly")
	if err := os.MkdirAll(roDir, 0555); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	defer os.Chmod(roDir, 0755) // cleanup

	roPath := filepath.Join(roDir, "test.json")
	if err := os.WriteFile(roPath, original, 0644); err != nil {
		// If we can't even write the original (e.g. root), skip
		t.Skipf("cannot write to dir with 0555 perms (likely root): %v", err)
	}
	// Make dir read-only after writing original
	os.Chmod(roDir, 0555)

	err := WriteFile(roPath, []byte("new content"), 0644)
	if err == nil {
		t.Fatal("expected error writing to read-only dir, got nil")
	}

	// Original should be intact
	os.Chmod(roDir, 0755)
	got, err := os.ReadFile(roPath)
	if err != nil {
		t.Fatalf("ReadFile after failed write: %v", err)
	}
	if string(got) != string(original) {
		t.Errorf("original corrupted: got %q, want %q", got, original)
	}
}

func TestWriteFile_TempFileCleanedUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	// Successful write should leave no temp files
	if err := WriteFile(path, []byte("data"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}
