package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteLock_AcquireRelease(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "test-session.jsonl")

	lm := NewLockManager(testLogger())
	defer lm.Stop()

	lock, err := lm.Acquire(sessionPath)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Lock file should exist
	lockPath := sessionPath + lockSuffix
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file not created: %v", err)
	}

	// Read payload
	payload, err := readLockPayload(lockPath)
	if err != nil {
		t.Fatalf("readLockPayload: %v", err)
	}
	if payload.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", payload.PID, os.Getpid())
	}

	// Release
	lm.Release(lock)
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Error("lock file should be removed after release")
	}
}

func TestWriteLock_DoubleAcquireBlocks(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "test-session.jsonl")

	lm := NewLockManager(testLogger())
	defer lm.Stop()

	lock1, err := lm.Acquire(sessionPath)
	if err != nil {
		t.Fatalf("Acquire 1: %v", err)
	}

	// Second acquire from a different LockManager should detect our PID as alive
	lm2 := NewLockManager(testLogger())
	defer lm2.Stop()

	// This should timeout because the lock is held by our process
	done := make(chan error, 1)
	go func() {
		_, err := lm2.Acquire(sessionPath)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("second acquire should have failed or timed out")
		}
		// Expected: timeout error
	case <-time.After(15 * time.Second):
		t.Fatal("test timed out waiting for second acquire to fail")
	}

	lm.Release(lock1)
}

func TestWriteLock_StaleLockReclaim(t *testing.T) {
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "test-session.jsonl")
	lockPath := sessionPath + lockSuffix

	// Create a stale lock from a dead PID
	payload := LockPayload{
		PID:       999999, // almost certainly not a real process
		CreatedAt: time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339),
		StartTime: 12345,
	}
	data, _ := json.Marshal(payload)
	os.WriteFile(lockPath, data, 0600)

	lm := NewLockManager(testLogger())
	defer lm.Stop()

	// Should reclaim the stale lock
	lock, err := lm.Acquire(sessionPath)
	if err != nil {
		t.Fatalf("Acquire (stale reclaim): %v", err)
	}
	lm.Release(lock)
}

func TestWriteLock_AgeStaleness(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "test.jsonl.lock")

	// Create a lock that's extremely old
	payload := LockPayload{
		PID:       os.Getpid(),
		CreatedAt: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339),
		StartTime: getProcessStartTime(os.Getpid()),
	}
	data, _ := json.Marshal(payload)
	os.WriteFile(lockPath, data, 0600)

	if !isLockStale(lockPath) {
		t.Error("lock older than maxLockAge should be stale")
	}
}

func TestWriteLock_IsProcessAlive(t *testing.T) {
	// Our own PID should be alive
	if !isProcessAlive(os.Getpid()) {
		t.Error("our own PID should be alive")
	}

	// PID 0 should not be alive (from our perspective)
	if isProcessAlive(0) {
		t.Error("PID 0 should not be alive")
	}

	// Very high PID should not be alive
	if isProcessAlive(999999999) {
		t.Error("very high PID should not be alive")
	}
}

func TestWriteLock_ReleaseAll(t *testing.T) {
	dir := t.TempDir()
	lm := NewLockManager(testLogger())

	// Acquire multiple locks
	paths := make([]string, 3)
	for i := range 3 {
		paths[i] = filepath.Join(dir, "session"+string(rune('a'+i))+".jsonl")
		_, err := lm.Acquire(paths[i])
		if err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
	}

	// Release all
	lm.ReleaseAll()

	// All lock files should be gone
	for _, p := range paths {
		if _, err := os.Stat(p + lockSuffix); !os.IsNotExist(err) {
			t.Errorf("lock file %s should be removed after ReleaseAll", p)
		}
	}

	lm.Stop()
}

func TestWriteLock_NonExistentLockPayload(t *testing.T) {
	_, err := readLockPayload("/nonexistent/path.lock")
	if err == nil {
		t.Error("expected error for nonexistent lock file")
	}
}

func TestWriteLock_CorruptLockFile(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "corrupt.lock")
	os.WriteFile(lockPath, []byte("not json"), 0600)

	if !isLockStale(lockPath) {
		// Corrupt files fall back to mtime check; since it was just created, it's not stale by age.
		// The payload read fails, so we check mtime. Brand new file → not stale.
		t.Log("corrupt lock not stale (expected: falls back to mtime check)")
	}
}

func TestGetProcessStartTime(t *testing.T) {
	st := getProcessStartTime(os.Getpid())
	// On Linux this should return a positive value; on other OSes returns 0
	if st < 0 {
		t.Errorf("start time should be >= 0, got %d", st)
	}
}
