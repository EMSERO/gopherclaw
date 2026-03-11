// Package session — writelock.go implements file-based session write locks (REQ-460).
//
// Before writing to a session JSONL file, an exclusive lock is acquired using
// atomic file creation (O_CREATE|O_EXCL). The lock file contains a JSON payload
// with PID, creation time, and process start time for stale detection.
//
// Stale lock detection:
//   - Dead PID (process not alive)
//   - Recycled PID (process start time mismatch)
//   - Age > maxLockAge (30 minutes)
//
// A watchdog goroutine periodically reclaims over-held locks.
// Signal handlers release all locks on SIGINT/SIGTERM.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
)

const (
	lockSuffix     = ".lock"
	lockTimeout    = 10 * time.Second
	lockBaseDelay  = 50 * time.Millisecond
	maxLockAge     = 30 * time.Minute
	maxLockHold    = 5 * time.Minute
	watchdogPeriod = 60 * time.Second
)

// LockPayload is stored in the lock file for stale detection.
type LockPayload struct {
	PID       int    `json:"pid"`
	CreatedAt string `json:"createdAt"` // ISO8601
	StartTime int64  `json:"starttime"` // process start time (jiffies or boot-relative)
}

// WriteLock represents an acquired file lock.
type WriteLock struct {
	path string
}

// Release removes the lock file.
func (l *WriteLock) Release() error {
	if l == nil || l.path == "" {
		return nil
	}
	return os.Remove(l.path)
}

// LockManager manages file-based write locks with watchdog and cleanup.
type LockManager struct {
	logger *zap.SugaredLogger
	mu     sync.Mutex
	locks  map[string]*WriteLock // path → lock
	done   chan struct{}
}

// NewLockManager creates a lock manager and starts the watchdog goroutine.
func NewLockManager(logger *zap.SugaredLogger) *LockManager {
	lm := &LockManager{
		logger: logger,
		locks:  make(map[string]*WriteLock),
		done:   make(chan struct{}),
	}
	go lm.watchdog()
	return lm
}

// Stop shuts down the watchdog and releases all held locks.
func (lm *LockManager) Stop() {
	close(lm.done)
	lm.ReleaseAll()
}

// ReleaseAll releases all currently held locks (used on signal shutdown).
func (lm *LockManager) ReleaseAll() {
	lm.mu.Lock()
	defer lm.mu.Unlock()
	for path, lock := range lm.locks {
		if err := lock.Release(); err != nil && !os.IsNotExist(err) {
			lm.logger.Warnf("session: release lock %s: %v", path, err)
		}
		delete(lm.locks, path)
	}
}

// Acquire attempts to acquire a write lock for a session JSONL file.
// Returns a WriteLock that must be Released when done.
func (lm *LockManager) Acquire(sessionPath string) (*WriteLock, error) {
	lockPath := sessionPath + lockSuffix
	deadline := time.Now().Add(lockTimeout)

	for attempt := 0; ; attempt++ {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("session write lock timeout after %s for %s", lockTimeout, filepath.Base(sessionPath))
		}

		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			// Successfully created lock file — write payload
			payload := LockPayload{
				PID:       os.Getpid(),
				CreatedAt: time.Now().UTC().Format(time.RFC3339),
				StartTime: getProcessStartTime(os.Getpid()),
			}
			data, _ := json.Marshal(payload)
			_, _ = f.Write(data)
			_ = f.Close()

			lock := &WriteLock{path: lockPath}
			lm.mu.Lock()
			lm.locks[lockPath] = lock
			lm.mu.Unlock()
			return lock, nil
		}

		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create lock file: %w", err)
		}

		// Lock file exists — check if stale. Use rename-then-remove to avoid
		// TOCTOU race where another goroutine acquires the lock between our
		// stale check and remove.
		if isLockStale(lockPath) {
			stalePath := lockPath + ".stale"
			if err := os.Rename(lockPath, stalePath); err == nil {
				lm.logger.Warnf("session: reclaiming stale lock %s", filepath.Base(lockPath))
				_ = os.Remove(stalePath)
			}
			continue // retry immediately after reclaim
		}

		// Wait with progressive backoff
		delay := lockBaseDelay * time.Duration(1<<min(attempt, 8))
		if delay > time.Second {
			delay = time.Second
		}
		select {
		case <-time.After(delay):
		case <-lm.done:
			return nil, fmt.Errorf("lock manager stopped")
		}
	}
}

// Release releases a specific lock and removes it from tracking.
func (lm *LockManager) Release(lock *WriteLock) {
	if lock == nil {
		return
	}
	_ = lock.Release()
	lm.mu.Lock()
	delete(lm.locks, lock.path)
	lm.mu.Unlock()
}

// watchdog periodically checks for locks held longer than maxLockHold.
func (lm *LockManager) watchdog() {
	ticker := time.NewTicker(watchdogPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			lm.reclaimOverheld()
		case <-lm.done:
			return
		}
	}
}

// reclaimOverheld releases locks that have been held longer than maxLockHold.
func (lm *LockManager) reclaimOverheld() {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	for path, lock := range lm.locks {
		payload, err := readLockPayload(path)
		if err != nil {
			continue
		}
		created, err := time.Parse(time.RFC3339, payload.CreatedAt)
		if err != nil {
			continue
		}
		if time.Since(created) > maxLockHold {
			lm.logger.Warnf("session: watchdog releasing over-held lock %s (held for %s)", filepath.Base(path), time.Since(created))
			_ = lock.Release()
			delete(lm.locks, path)
		}
	}
}

// isLockStale checks if an existing lock file is stale.
func isLockStale(lockPath string) bool {
	payload, err := readLockPayload(lockPath)
	if err != nil {
		// Can't read payload — check mtime fallback
		info, statErr := os.Stat(lockPath)
		if statErr != nil {
			return true // can't stat → stale
		}
		return time.Since(info.ModTime()) > maxLockAge
	}

	// Check age
	created, err := time.Parse(time.RFC3339, payload.CreatedAt)
	if err == nil && time.Since(created) > maxLockAge {
		return true
	}

	// Check if PID is alive
	if !isProcessAlive(payload.PID) {
		return true
	}

	// Check for PID recycling (process start time mismatch)
	if payload.StartTime > 0 {
		currentStartTime := getProcessStartTime(payload.PID)
		if currentStartTime > 0 && currentStartTime != payload.StartTime {
			return true // PID was recycled
		}
	}

	return false
}

// readLockPayload reads and parses a lock file's JSON payload.
func readLockPayload(path string) (LockPayload, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return LockPayload{}, err
	}
	var payload LockPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return LockPayload{}, err
	}
	return payload, nil
}

// isProcessAlive checks if a process with the given PID exists.
func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds. Use signal 0 to probe.
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

// getProcessStartTime reads the process start time from /proc/<pid>/stat.
// Returns 0 if unavailable (non-Linux or error).
func getProcessStartTime(pid int) int64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// Format: pid (comm) state ... starttime is field 22 (0-indexed from the end of comm)
	// Find the closing paren of comm to avoid issues with spaces in process names
	s := string(data)
	idx := strings.LastIndex(s, ")")
	if idx < 0 || idx+2 >= len(s) {
		return 0
	}
	fields := strings.Fields(s[idx+2:])
	// starttime is at position 19 (field 22 overall, minus 3 for pid+comm+state parsed)
	if len(fields) < 20 {
		return 0
	}
	st, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0
	}
	return st
}
