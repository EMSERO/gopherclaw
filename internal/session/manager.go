package session

import (
	"bufio"
	"context"
	cryptoRand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/atomicfile"
)

// ErrSessionNotFound is returned when a session key does not match any active session.
var ErrSessionNotFound = errors.New("session not found")

const sessionsFile = "sessions.json"

// Entry tracks metadata for a session.
type Entry struct {
	SessionID string `json:"sessionId"`
	UpdatedAt int64  `json:"updatedAt"` // unix ms
}

// Message is a single conversation entry stored in JSONL.
type Message struct {
	Role       string            `json:"role"`
	Content    string            `json:"content,omitempty"`
	ImageURLs  []string          `json:"image_urls,omitempty"` // base64 data URLs or HTTP URLs for vision
	ToolCalls  []openai.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
	Name       string            `json:"name,omitempty"`
	TS         int64             `json:"ts"`
}

// PruningPolicy holds token-based context pruning parameters.
type PruningPolicy struct {
	HardClearRatio     float64       // fraction of model max tokens to trigger clear (default 0.5)
	ModelMaxTokens     int           // model context window size (default 16384)
	KeepLastAssistants int           // assistant messages to keep after hard clear (default 2)
	SoftTrimRatio      float64       // fraction of model max tokens to trigger soft trim (default 0.0 = disabled)
	SurgicalPruning    bool          // when true, prefer trimming tool results before hard clear
	CacheTTL           time.Duration // prompt-cache lifetime to respect before pruning (default 0 = prune immediately)
}

// EstimateTokens exposes the token estimator for external callers (e.g. soft trim).
func EstimateTokens(msgs []Message) int {
	return estimateTokens(msgs)
}

// Manager handles session persistence.
type Manager struct {
	logger    *zap.SugaredLogger
	mu        sync.Mutex
	dir       string
	sessions  map[string]*Entry
	ttl       time.Duration
	pruning   PruningPolicy
	saveDirty bool         // sessions.json metadata needs flush
	saveTimer *time.Timer  // coalescing timer for metadata saves
	locks     *LockManager // file-based write locks (REQ-460)

	// lastAPICall tracks the last API call timestamp per session key.
	// When CacheTTL > 0, pruning is deferred until this TTL expires,
	// preserving the provider's prompt cache for cost savings.
	lastAPICall map[string]time.Time
}

// New creates a session Manager.
// dir is the sessions directory (e.g. ~/.gopherclaw/agents/main/sessions).
func New(logger *zap.SugaredLogger, dir string, ttl time.Duration) (*Manager, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	m := &Manager{
		logger:      logger,
		dir:         dir,
		sessions:    make(map[string]*Entry),
		ttl:         ttl,
		lastAPICall: make(map[string]time.Time),
		locks:       NewLockManager(logger),
	}
	if err := m.load(); err != nil {
		m.logger.Warnf("session: failed to load sessions.json (starting fresh): %v", err)
	}
	return m, nil
}

// SetPruning configures token-based context pruning.
func (m *Manager) SetPruning(p PruningPolicy) {
	m.mu.Lock()
	m.pruning = p
	m.mu.Unlock()
}

// TouchAPICall records the current time as the last API call for a session.
// When CacheTTL > 0 in the pruning policy, pruning is deferred until the
// TTL after this timestamp expires, preserving the provider's prompt cache.
func (m *Manager) TouchAPICall(key string) {
	m.mu.Lock()
	m.lastAPICall[key] = time.Now()
	m.mu.Unlock()
}

// GetHistory returns the message history for a session key.
// Returns nil if the session is new or expired.
func (m *Manager) GetHistory(key string) ([]Message, error) {
	m.mu.Lock()
	e, ok := m.sessions[key]
	pruning := m.pruning
	// Copy fields while holding the lock to avoid racing with AppendMessages.
	var sessionID string
	var updatedAt int64
	if ok {
		sessionID = e.SessionID
		updatedAt = e.UpdatedAt
	}
	m.mu.Unlock()

	if !ok {
		return nil, nil
	}

	// Check TTL
	if m.ttl > 0 && time.Since(time.UnixMilli(updatedAt)) > m.ttl {
		m.Reset(key)
		return nil, nil
	}

	msgs, err := m.loadJSONL(sessionID)
	if err != nil {
		return nil, err
	}

	// Strip orphaned tool results and persist the cleanup so it only happens once.
	if cleaned, n := SanitizeOrphans(msgs); n > 0 {
		m.logger.Warnf("session: removed %d orphaned tool result message(s) from %s", n, key)
		msgs = cleaned
		if err := m.rewriteJSONL(sessionID, msgs); err != nil {
			m.logger.Warnf("session: orphan cleanup rewrite failed for %s: %v", key, err)
		}
	}

	// Token-based pruning.
	// When CacheTTL > 0, defer pruning until the cache window expires
	// so we don't invalidate the provider's prompt cache mid-session.
	canPrune := true
	if pruning.CacheTTL > 0 {
		m.mu.Lock()
		lastCall, ok := m.lastAPICall[key]
		m.mu.Unlock()
		if ok && time.Since(lastCall) < pruning.CacheTTL {
			canPrune = false
		}
	}

	if canPrune && pruning.HardClearRatio > 0 && pruning.ModelMaxTokens > 0 {
		threshold := int(pruning.HardClearRatio * float64(pruning.ModelMaxTokens))
		if estimateTokens(msgs) > threshold {
			// Surgical pruning: compress tool results first, preserve conversation.
			if pruning.SurgicalPruning {
				msgs = PruneToolResults(msgs, pruning.KeepLastAssistants)
			}
			// If still over threshold after surgical pruning (or surgical disabled), hard clear.
			if estimateTokens(msgs) > threshold {
				msgs = pruneKeepLast(msgs, pruning.KeepLastAssistants)
			}
			if err := m.rewriteJSONL(sessionID, msgs); err != nil {
				m.logger.Warnf("session: prune rewrite failed for %s: %v", key, err)
			}
		}
	}

	return msgs, nil
}

// AppendMessages adds messages to a session's history.
func (m *Manager) AppendMessages(key string, msgs []Message) error {
	m.mu.Lock()
	e, ok := m.sessions[key]
	if !ok {
		e = &Entry{SessionID: newID(), UpdatedAt: nowMs()}
		m.sessions[key] = e
	}
	e.UpdatedAt = nowMs()
	id := e.SessionID
	m.mu.Unlock()

	if err := m.appendJSONL(id, msgs); err != nil {
		return err
	}
	m.scheduleSave()
	return nil
}

// ReplaceHistory replaces the entire session JSONL with msgs.
// Returns nil if the session doesn't exist.
func (m *Manager) ReplaceHistory(key string, msgs []Message) error {
	m.mu.Lock()
	e, ok := m.sessions[key]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	id := e.SessionID
	m.mu.Unlock()
	return m.rewriteJSONL(id, msgs)
}

// TrimMessages caps a session's history to the last maxMessages entries.
// No-op if maxMessages <= 0, session doesn't exist, or history is already within the limit.
func (m *Manager) TrimMessages(key string, maxMessages int) error {
	if maxMessages <= 0 {
		return nil
	}
	m.mu.Lock()
	e, ok := m.sessions[key]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	msgs, err := m.loadJSONL(e.SessionID)
	if err != nil || len(msgs) <= maxMessages {
		return err
	}
	return m.rewriteJSONL(e.SessionID, msgs[len(msgs)-maxMessages:])
}

// Reset clears a session's history.
func (m *Manager) Reset(key string) {
	m.mu.Lock()
	if e, ok := m.sessions[key]; ok {
		if err := os.Remove(m.jsonlPath(e.SessionID)); err != nil && !os.IsNotExist(err) {
			m.logger.Warnf("session: remove %s: %v", key, err)
		}
	}
	delete(m.sessions, key)
	delete(m.lastAPICall, key)
	m.mu.Unlock()
	if err := m.save(); err != nil {
		m.logger.Warnf("session: save after reset %s: %v", key, err)
	}
}

// ResetAll clears all sessions.
func (m *Manager) ResetAll() {
	m.mu.Lock()
	for key, e := range m.sessions {
		if err := os.Remove(m.jsonlPath(e.SessionID)); err != nil && !os.IsNotExist(err) {
			m.logger.Warnf("session: remove %s: %v", key, err)
		}
		delete(m.sessions, key)
	}
	m.lastAPICall = make(map[string]time.Time)
	m.mu.Unlock()
	if err := m.save(); err != nil {
		m.logger.Warnf("session: save after reset-all: %v", err)
	}
}

// ResetIdle resets sessions that have been idle longer than the given duration.
func (m *Manager) ResetIdle(maxIdle time.Duration) int {
	m.mu.Lock()
	var toReset []string
	now := time.Now()
	for key, e := range m.sessions {
		if now.Sub(time.UnixMilli(e.UpdatedAt)) > maxIdle {
			toReset = append(toReset, key)
		}
	}
	for _, key := range toReset {
		if e, ok := m.sessions[key]; ok {
			if err := os.Remove(m.jsonlPath(e.SessionID)); err != nil && !os.IsNotExist(err) {
				m.logger.Warnf("session: remove idle %s: %v", key, err)
			}
		}
		delete(m.sessions, key)
		delete(m.lastAPICall, key)
	}
	m.mu.Unlock()

	if len(toReset) > 0 {
		if err := m.save(); err != nil {
			m.logger.Warnf("session: save after idle reset: %v", err)
		}
	}
	return len(toReset)
}

// ActiveSessions returns a snapshot of all active session keys and their last update time.
func (m *Manager) ActiveSessions() map[string]time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make(map[string]time.Time, len(m.sessions))
	for key, e := range m.sessions {
		result[key] = time.UnixMilli(e.UpdatedAt)
	}
	return result
}

// StartResetLoop starts background goroutines for daily and idle resets.
func (m *Manager) StartResetLoop(ctx context.Context, dailyMode string, atHour int, idleMinutes int, logFn func(string, ...any)) {
	if dailyMode == "daily" {
		go m.dailyResetLoop(ctx, atHour, logFn)
	}
	if idleMinutes > 0 {
		go m.idleResetLoop(ctx, time.Duration(idleMinutes)*time.Minute, logFn)
	}
}

func (m *Manager) dailyResetLoop(ctx context.Context, atHour int, logFn func(string, ...any)) {
	for {
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day(), atHour, 0, 0, 0, now.Location())
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}
		wait := next.Sub(now)

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
			m.ResetAll()
			logFn("session: daily reset at hour %d — all sessions cleared", atHour)
		}
	}
}

func (m *Manager) idleResetLoop(ctx context.Context, maxIdle time.Duration, logFn func(string, ...any)) {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n := m.ResetIdle(maxIdle)
			if n > 0 {
				logFn("session: idle reset — cleared %d sessions (idle > %s)", n, maxIdle)
			}
		}
	}
}

// SanitizeOrphans removes tool result messages whose ToolCallID doesn't match
// any preceding assistant message's ToolCalls. Returns the cleaned slice and
// the number of messages removed.
func SanitizeOrphans(msgs []Message) ([]Message, int) {
	valid := make(map[string]bool)
	out := make([]Message, 0, len(msgs))
	dropped := 0
	for _, m := range msgs {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					valid[tc.ID] = true
				}
			}
		}
		if m.Role == "tool" && m.ToolCallID != "" && !valid[m.ToolCallID] {
			dropped++
			continue
		}
		out = append(out, m)
	}
	return out, dropped
}

// ToOpenAI converts Messages to the openai.ChatCompletionMessage format.
// Orphaned tool result messages (ToolCallID not matching any preceding
// assistant's ToolCalls) are silently dropped as a safety net; the
// primary cleanup happens in GetHistory which persists the fix.
func ToOpenAI(msgs []Message) []openai.ChatCompletionMessage {
	cleaned, _ := SanitizeOrphans(msgs)
	out := make([]openai.ChatCompletionMessage, 0, len(cleaned))
	for _, m := range cleaned {
		msg := openai.ChatCompletionMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		if len(m.ToolCalls) > 0 {
			msg.ToolCalls = m.ToolCalls
		}
		// Build multi-content message when image URLs are present (vision).
		if len(m.ImageURLs) > 0 && m.Role == "user" {
			parts := []openai.ChatMessagePart{
				{Type: openai.ChatMessagePartTypeText, Text: m.Content},
			}
			for _, u := range m.ImageURLs {
				parts = append(parts, openai.ChatMessagePart{
					Type:     openai.ChatMessagePartTypeImageURL,
					ImageURL: &openai.ChatMessageImageURL{URL: u},
				})
			}
			msg.Content = ""
			msg.MultiContent = parts
		}
		out = append(out, msg)
	}
	return out
}

// FromOpenAI converts openai messages to session Messages.
func FromOpenAI(msgs []openai.ChatCompletionMessage) []Message {
	out := make([]Message, 0, len(msgs))
	now := nowMs()
	for _, m := range msgs {
		out = append(out, Message{
			Role:       m.Role,
			Content:    m.Content,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
			TS:         now,
		})
	}
	return out
}

// estimateTokens provides a rough token count (~4 chars per token).
func estimateTokens(msgs []Message) int {
	total := 0
	for _, m := range msgs {
		total += len(m.Content)/4 + 4 // +4 per message overhead
		for _, tc := range m.ToolCalls {
			total += len(tc.Function.Arguments)/4 + len(tc.Function.Name)/4
		}
	}
	return total
}

// PruneToolResults performs surgical pruning: compresses old tool-result
// messages to short placeholders while preserving user and assistant messages.
// The last keepRecent assistant+tool sequences are left untouched so the model
// has recent tool context available.
//
// This keeps the conversation flow intact (user questions and assistant
// reasoning) while reclaiming the majority of tokens — tool results are
// typically 80%+ of a long session's token budget.
func PruneToolResults(msgs []Message, keepRecent int) []Message {
	if keepRecent <= 0 {
		keepRecent = 2
	}

	// Find the keep boundary: the Nth-from-last assistant message index.
	// Then walk backward past any tool_calls→tool sequences to protect the
	// complete tool-use round.
	var assistantIndices []int
	for i, m := range msgs {
		if m.Role == "assistant" {
			assistantIndices = append(assistantIndices, i)
		}
	}
	keepBoundary := 0 // index at or after which we preserve tool results
	if len(assistantIndices) > keepRecent {
		keepBoundary = assistantIndices[len(assistantIndices)-keepRecent]
		// Walk backward past tool results and their parent assistant(tool_calls).
		for keepBoundary > 0 && msgs[keepBoundary-1].Role == "tool" {
			keepBoundary--
		}
		if keepBoundary > 0 && msgs[keepBoundary-1].Role == "assistant" && len(msgs[keepBoundary-1].ToolCalls) > 0 {
			keepBoundary--
		}
	}

	out := make([]Message, 0, len(msgs))
	for i, m := range msgs {
		if m.Role == "tool" && i < keepBoundary && len(m.Content) > 200 {
			// Compress: keep head (100 chars) + tail (80 chars) with ellipsis.
			head := m.Content
			if len(head) > 100 {
				head = head[:100]
			}
			tail := m.Content
			if len(tail) > 80 {
				tail = tail[len(tail)-80:]
			}
			compressed := Message{
				Role:       m.Role,
				ToolCallID: m.ToolCallID,
				Name:       m.Name,
				Content:    head + "\n…[trimmed]…\n" + tail,
				TS:         m.TS,
			}
			out = append(out, compressed)
		} else {
			out = append(out, m)
		}
	}
	return out
}

// pruneKeepLast removes all messages except the last N assistant messages
// and their associated preceding user/tool messages.
// It ensures the cut point never splits an assistant(tool_calls)→tool(result) sequence.
func pruneKeepLast(msgs []Message, keepAssistants int) []Message {
	if keepAssistants <= 0 {
		keepAssistants = 2
	}

	var assistantIndices []int
	for i, m := range msgs {
		if m.Role == "assistant" {
			assistantIndices = append(assistantIndices, i)
		}
	}

	if len(assistantIndices) <= keepAssistants {
		return msgs
	}

	cutFrom := assistantIndices[len(assistantIndices)-keepAssistants]
	// Also keep the user message immediately before the cut point
	if cutFrom > 0 && msgs[cutFrom-1].Role == "user" {
		cutFrom--
	}

	// If the message just before cutFrom is a tool result, walk backwards past
	// the entire tool_calls→tool sequence to avoid orphaned tool messages.
	for cutFrom > 0 && msgs[cutFrom-1].Role == "tool" {
		cutFrom--
	}
	// The tool results belong to the preceding assistant with tool_calls — include it too.
	if cutFrom > 0 && msgs[cutFrom-1].Role == "assistant" && len(msgs[cutFrom-1].ToolCalls) > 0 {
		cutFrom--
	}

	return msgs[cutFrom:]
}

// rewriteJSONL replaces the JSONL file with the given messages.
// Writes to a temp file first, then atomically renames to prevent data loss
// if encoding fails mid-write.
func (m *Manager) rewriteJSONL(id string, msgs []Message) error {
	path := m.jsonlPath(id)
	lock, err := m.locks.Acquire(path)
	if err != nil {
		return fmt.Errorf("session %s: acquire write lock: %w", id, err)
	}
	defer m.locks.Release(lock)

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".rewrite-*.jsonl")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()

	enc := json.NewEncoder(tmp)
	for _, msg := range msgs {
		if err := enc.Encode(msg); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func (m *Manager) load() error {
	data, err := os.ReadFile(filepath.Join(m.dir, sessionsFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &m.sessions)
}

func (m *Manager) save() error {
	m.mu.Lock()
	data, err := json.MarshalIndent(m.sessions, "", "  ")
	m.mu.Unlock()
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(filepath.Join(m.dir, sessionsFile), data, 0600)
}

// scheduleSave coalesces sessions.json writes. The actual write happens after
// 2 seconds of inactivity. Must NOT hold m.mu when called.
func (m *Manager) scheduleSave() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveDirty = true
	if m.saveTimer == nil {
		m.saveTimer = time.AfterFunc(2*time.Second, m.flushSave)
	} else {
		m.saveTimer.Reset(2 * time.Second)
	}
}

// flushSave writes sessions.json if dirty. Safe to call from timer or directly.
func (m *Manager) flushSave() {
	m.mu.Lock()
	if !m.saveDirty {
		m.mu.Unlock()
		return
	}
	m.saveDirty = false
	m.saveTimer = nil
	data, err := json.MarshalIndent(m.sessions, "", "  ")
	m.mu.Unlock()
	if err != nil {
		m.logger.Warnf("session: flushSave marshal error: %v", err)
		return
	}
	if err := atomicfile.WriteFile(filepath.Join(m.dir, sessionsFile), data, 0600); err != nil {
		m.logger.Warnf("session: flushSave write error: %v", err)
	}
}

// FlushSave forces an immediate write of sessions.json if dirty. Call on shutdown.
func (m *Manager) FlushSave() {
	m.mu.Lock()
	if m.saveTimer != nil {
		m.saveTimer.Stop()
		m.saveTimer = nil
	}
	m.mu.Unlock()
	m.flushSave()
}

// Stop shuts down the lock manager and flushes pending saves. Call on shutdown.
func (m *Manager) Stop() {
	m.FlushSave()
	if m.locks != nil {
		m.locks.Stop()
	}
}

// Reap deletes JSONL files in the sessions directory that are older than maxAge
// and have no corresponding active session. Returns count of files removed.
func (m *Manager) Reap(maxAge time.Duration) (int, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return 0, err
	}

	m.mu.Lock()
	activeKeys := make(map[string]bool, len(m.sessions))
	for _, e := range m.sessions {
		activeKeys[e.SessionID] = true
	}
	m.mu.Unlock()

	var count int
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Only process .jsonl and .tmp files
		if !strings.HasSuffix(name, ".jsonl") && !strings.HasSuffix(name, ".tmp") {
			continue
		}
		// Derive session ID from filename
		sessionID := strings.TrimSuffix(name, ".jsonl")
		sessionID = strings.TrimSuffix(sessionID, ".tmp")
		if activeKeys[sessionID] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if time.Since(info.ModTime()) < maxAge {
			continue
		}
		if err := os.Remove(filepath.Join(m.dir, name)); err == nil {
			count++
		}
	}
	return count, nil
}

// StartReapLoop runs Reap periodically in the background until ctx is cancelled.
func (m *Manager) StartReapLoop(ctx context.Context, interval, maxAge time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := m.Reap(maxAge); err != nil {
				m.logger.Warnf("session reaper: %v", err)
			} else if n > 0 {
				m.logger.Infof("session reaper: cleaned %d orphaned files", n)
			}
		}
	}
}

func (m *Manager) jsonlPath(id string) string {
	return filepath.Join(m.dir, id+".jsonl")
}

func (m *Manager) loadJSONL(id string) ([]Message, error) {
	f, err := os.Open(m.jsonlPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var msgs []Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg Message
		if err := json.Unmarshal(line, &msg); err != nil {
			m.logger.Warnf("session %s: skipping corrupt JSONL line: %v", id, err)
			continue
		}
		msgs = append(msgs, msg)
	}
	return msgs, sc.Err()
}

func (m *Manager) appendJSONL(id string, msgs []Message) error {
	path := m.jsonlPath(id)
	lock, err := m.locks.Acquire(path)
	if err != nil {
		return fmt.Errorf("session %s: acquire write lock: %w", id, err)
	}
	defer m.locks.Release(lock)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	enc := json.NewEncoder(f)
	for _, msg := range msgs {
		if err := enc.Encode(msg); err != nil {
			return err
		}
	}
	return nil
}

func newID() string {
	b := make([]byte, 8)
	_, _ = cryptoRand.Read(b)
	return fmt.Sprintf("%x%x", time.Now().UnixNano(), b)
}

func nowMs() int64 {
	return time.Now().UnixMilli()
}
