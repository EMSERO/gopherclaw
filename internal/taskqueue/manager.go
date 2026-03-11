package taskqueue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/agentapi"
	"github.com/EMSERO/gopherclaw/internal/atomicfile"
)

// TaskStatus represents the lifecycle state of a task.
type TaskStatus string

const (
	StatusPending   TaskStatus = "pending"
	StatusRunning   TaskStatus = "running"
	StatusSuccess   TaskStatus = "success"
	StatusFailed    TaskStatus = "failed"
	StatusCancelled TaskStatus = "cancelled"
)

// TaskRecord is the persisted representation of a task.
type TaskRecord struct {
	ID            string     `json:"id"`
	AgentID       string     `json:"agentId"`
	SessionKey    string     `json:"sessionKey"`
	Message       string     `json:"message"`
	Status        TaskStatus `json:"status"`
	Result        string     `json:"result,omitempty"`
	Error         string     `json:"error,omitempty"`
	CreatedAtMs   int64      `json:"createdAtMs"`
	StartedAtMs   int64      `json:"startedAtMs,omitempty"`
	CompletedAtMs int64      `json:"completedAtMs,omitempty"`
	DurationMs    int64      `json:"durationMs,omitempty"`
}

// taskFile is the on-disk format.
type taskFile struct {
	Version int           `json:"version"`
	Tasks   []*TaskRecord `json:"tasks"`
}

// runningTask holds the in-memory context for a running task.
type runningTask struct {
	record *TaskRecord
	cancel context.CancelFunc
}

// Config holds task queue configuration.
type Config struct {
	MaxConcurrent    int
	ResultRetention  time.Duration
	ProgressThrottle time.Duration
}

func (c Config) withDefaults() Config {
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 5
	}
	if c.ResultRetention <= 0 {
		c.ResultRetention = time.Hour
	}
	if c.ProgressThrottle <= 0 {
		c.ProgressThrottle = 5 * time.Second
	}
	return c
}

// SubmitOpts configures optional behavior for Submit.
type SubmitOpts struct {
	// OnComplete is called after the task finishes (success, failure, or cancellation).
	OnComplete func(result string, err error)
}

// Manager manages background tasks with persistence and cancellation.
type Manager struct {
	mu       sync.Mutex
	tasks    []*TaskRecord
	running  map[string]*runningTask
	sem      chan struct{}
	filePath string
	cfg      Config
	logger   *zap.SugaredLogger

	rootCtx    context.Context    // cancelled on Shutdown; all task contexts derive from this
	rootCancel context.CancelFunc // call from Shutdown to cascade-cancel all tasks

	announcers []agentapi.Announcer

	progressMu   sync.Mutex
	lastProgress map[string]time.Time

	saveDirty bool
	saveTimer *time.Timer
}

// New creates a TaskManager. It loads existing state from filePath,
// marking any previously-running tasks as failed (interrupted by restart).
func New(logger *zap.SugaredLogger, filePath string, cfg Config) *Manager {
	cfg = cfg.withDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		running:      make(map[string]*runningTask),
		sem:          make(chan struct{}, cfg.MaxConcurrent),
		filePath:     filePath,
		cfg:          cfg,
		logger:       logger,
		lastProgress: make(map[string]time.Time),
		rootCtx:      ctx,
		rootCancel:   cancel,
	}
	m.load()
	return m
}

// Submit enqueues and starts a background task. The provided fn runs in a
// goroutine, gated by the global concurrency semaphore. Returns the task ID.
func (m *Manager) Submit(sessionKey, agentID, message string, fn func(ctx context.Context) (string, error), opts SubmitOpts) string {
	id := randomHex(12)
	now := time.Now().UnixMilli()

	msgPreview := message
	if len(msgPreview) > 200 {
		msgPreview = msgPreview[:200]
	}

	record := &TaskRecord{
		ID:          id,
		AgentID:     agentID,
		SessionKey:  sessionKey,
		Message:     msgPreview,
		Status:      StatusPending,
		CreatedAtMs: now,
	}

	m.mu.Lock()
	m.tasks = append(m.tasks, record)
	m.scheduleSave()
	m.mu.Unlock()

	go m.run(record, fn, opts)

	return id
}

// run acquires the semaphore, executes the task function, and updates state.
func (m *Manager) run(record *TaskRecord, fn func(ctx context.Context) (string, error), opts SubmitOpts) {
	// Acquire semaphore
	m.sem <- struct{}{}
	defer func() { <-m.sem }()

	// Check if cancelled while waiting for semaphore
	m.mu.Lock()
	if record.Status == StatusCancelled {
		m.mu.Unlock()
		return
	}

	ctx, cancel := context.WithCancel(m.rootCtx)
	record.Status = StatusRunning
	record.StartedAtMs = time.Now().UnixMilli()
	m.running[record.ID] = &runningTask{record: record, cancel: cancel}
	m.scheduleSave()
	m.mu.Unlock()

	// Execute
	result, err := func() (res string, fnErr error) {
		defer func() {
			if r := recover(); r != nil {
				fnErr = fmt.Errorf("panic: %v", r)
			}
		}()
		return fn(ctx)
	}()

	cancel() // release context resources
	now := time.Now()

	m.mu.Lock()
	delete(m.running, record.ID)
	record.CompletedAtMs = now.UnixMilli()
	if record.StartedAtMs > 0 {
		record.DurationMs = now.UnixMilli() - record.StartedAtMs
	}

	if err != nil {
		if err == context.Canceled {
			record.Status = StatusCancelled
		} else {
			record.Status = StatusFailed
			record.Error = truncate(err.Error(), 2000)
		}
	} else {
		record.Status = StatusSuccess
		record.Result = truncate(result, 2000)
	}
	m.scheduleSave()
	m.mu.Unlock()

	// Callback (wrapped in panic recovery to prevent goroutine crashes).
	if opts.OnComplete != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					m.logger.Errorf("taskqueue: OnComplete panic for task %s: %v", record.ID, r)
				}
			}()
			opts.OnComplete(result, err)
		}()
	}
}

// Cancel cancels a running task by ID.
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check pending tasks first
	for _, t := range m.tasks {
		if t.ID == id && t.Status == StatusPending {
			t.Status = StatusCancelled
			t.CompletedAtMs = time.Now().UnixMilli()
			m.scheduleSave()
			return nil
		}
	}

	rt, ok := m.running[id]
	if !ok {
		return fmt.Errorf("task %s not found or not running", id)
	}
	rt.cancel()
	rt.record.Status = StatusCancelled
	rt.record.CompletedAtMs = time.Now().UnixMilli()
	if rt.record.StartedAtMs > 0 {
		rt.record.DurationMs = rt.record.CompletedAtMs - rt.record.StartedAtMs
	}
	delete(m.running, id)
	m.scheduleSave()
	return nil
}

// CancelAll cancels all running tasks for a session key. Returns count cancelled.
func (m *Manager) CancelAll(sessionKey string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	n := 0
	now := time.Now().UnixMilli()
	for id, rt := range m.running {
		if rt.record.SessionKey == sessionKey {
			rt.cancel()
			rt.record.Status = StatusCancelled
			rt.record.CompletedAtMs = now
			if rt.record.StartedAtMs > 0 {
				rt.record.DurationMs = now - rt.record.StartedAtMs
			}
			delete(m.running, id)
			n++
		}
	}
	// Also cancel pending tasks
	for _, t := range m.tasks {
		if t.SessionKey == sessionKey && t.Status == StatusPending {
			t.Status = StatusCancelled
			t.CompletedAtMs = now
			n++
		}
	}
	if n > 0 {
		m.scheduleSave()
	}
	return n
}

// List returns a snapshot of all tasks (running + recently completed).
func (m *Manager) List() []*TaskRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

// ListForSession returns tasks associated with a specific session.
func (m *Manager) ListForSession(sessionKey string) []*TaskRecord {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []*TaskRecord
	for _, t := range m.tasks {
		if t.SessionKey == sessionKey {
			cp := *t
			out = append(out, &cp)
		}
	}
	return out
}

// RunningCount returns the number of currently running tasks.
func (m *Manager) RunningCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.running)
}

// AddAnnouncer registers a channel bot for progress updates.
func (m *Manager) AddAnnouncer(a agentapi.Announcer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.announcers = append(m.announcers, a)
}

// AnnounceProgress sends a throttled progress update to the parent session.
func (m *Manager) AnnounceProgress(sessionKey, text string) {
	m.progressMu.Lock()
	last := m.lastProgress[sessionKey]
	if time.Since(last) < m.cfg.ProgressThrottle {
		m.progressMu.Unlock()
		return
	}
	m.lastProgress[sessionKey] = time.Now()
	m.progressMu.Unlock()

	m.mu.Lock()
	announcers := make([]agentapi.Announcer, len(m.announcers))
	copy(announcers, m.announcers)
	m.mu.Unlock()

	for _, a := range announcers {
		a.AnnounceToSession(sessionKey, text)
	}
}

// Announce sends an unthrottled message to a session (for final results).
func (m *Manager) Announce(sessionKey, text string) {
	m.mu.Lock()
	announcers := make([]agentapi.Announcer, len(m.announcers))
	copy(announcers, m.announcers)
	m.mu.Unlock()

	for _, a := range announcers {
		a.AnnounceToSession(sessionKey, text)
	}
}

// Acquire implements the Limiter interface using the global semaphore.
func (m *Manager) Acquire(ctx context.Context) error {
	select {
	case m.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release implements the Limiter interface.
func (m *Manager) Release() {
	<-m.sem
}

// Shutdown cancels all running tasks and flushes state to disk.
func (m *Manager) Shutdown() {
	// Cancel root context first — cascades to all task contexts.
	m.rootCancel()

	m.mu.Lock()
	now := time.Now().UnixMilli()
	for id, rt := range m.running {
		rt.cancel()
		rt.record.Status = StatusCancelled
		rt.record.Error = "shutdown"
		rt.record.CompletedAtMs = now
		if rt.record.StartedAtMs > 0 {
			rt.record.DurationMs = now - rt.record.StartedAtMs
		}
		delete(m.running, id)
	}
	if m.saveTimer != nil {
		m.saveTimer.Stop()
	}
	m.mu.Unlock()

	m.flushSave()
}

// StartPruneLoop prunes completed tasks older than retention. Blocks until ctx done.
func (m *Manager) StartPruneLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.prune()
		}
	}
}

func (m *Manager) prune() {
	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := time.Now().Add(-m.cfg.ResultRetention).UnixMilli()
	pruned := false
	filtered := m.tasks[:0]
	for _, t := range m.tasks {
		if t.CompletedAtMs > 0 && t.CompletedAtMs < cutoff {
			pruned = true
			continue
		}
		filtered = append(filtered, t)
	}
	m.tasks = filtered
	if pruned {
		m.scheduleSave()
	}
}

// --- persistence ---

func (m *Manager) load() {
	data, err := os.ReadFile(m.filePath)
	if err != nil {
		return
	}
	var f taskFile
	if err := json.Unmarshal(data, &f); err != nil {
		m.logger.Warnf("taskqueue: failed to parse %s: %v", m.filePath, err)
		return
	}

	now := time.Now().UnixMilli()
	for _, t := range f.Tasks {
		if t.Status == StatusRunning || t.Status == StatusPending {
			t.Status = StatusFailed
			t.Error = "interrupted by restart"
			t.CompletedAtMs = now
		}
	}
	m.tasks = f.Tasks
	m.saveDirty = true
}

func (m *Manager) scheduleSave() {
	m.saveDirty = true
	if m.saveTimer != nil {
		return
	}
	m.saveTimer = time.AfterFunc(2*time.Second, func() {
		m.flushSave()
	})
}

func (m *Manager) flushSave() {
	m.mu.Lock()
	if !m.saveDirty {
		m.mu.Unlock()
		return
	}
	m.saveDirty = false
	m.saveTimer = nil

	f := taskFile{Version: 1, Tasks: make([]*TaskRecord, len(m.tasks))}
	for i, t := range m.tasks {
		cp := *t
		f.Tasks[i] = &cp
	}
	m.mu.Unlock()

	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		m.logger.Warnf("taskqueue: marshal error: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.filePath), 0700); err != nil {
		m.logger.Warnf("taskqueue: mkdir error: %v", err)
		return
	}
	if err := atomicfile.WriteFile(m.filePath, data, 0600); err != nil {
		m.logger.Warnf("taskqueue: write error: %v", err)
	}
}

func (m *Manager) snapshotLocked() []*TaskRecord {
	out := make([]*TaskRecord, len(m.tasks))
	for i, t := range m.tasks {
		cp := *t
		out[i] = &cp
	}
	return out
}

// --- helpers ---

func randomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}
