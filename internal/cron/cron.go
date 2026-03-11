// Package cron provides a lightweight cron-like scheduler for agent jobs.
// It supports both the simple GopherClaw crons.json format and the full
// OpenClaw jobs.json format (schedule.kind:"every", sessionTarget, wakeMode,
// delivery, payload model/timeout overrides, and state persistence).
package cron

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/agentapi"
	"github.com/EMSERO/gopherclaw/internal/atomicfile"
)

// ErrJobNotFound is returned when a job ID does not match any known job.
var ErrJobNotFound = errors.New("job not found")

// ──────────────────────────────────────────────────────────────────────
// Job types — superset of simple (crons.json) and full (jobs.json)
// ──────────────────────────────────────────────────────────────────────

// Job represents a scheduled agent task.
type Job struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`

	// Legacy simple format fields (crons.json)
	Spec        string `json:"spec,omitempty"`        // @daily, @hourly, @every 1h, HH:MM
	Instruction string `json:"instruction,omitempty"` // text sent to agent
	SessionKey  string `json:"sessionKey,omitempty"`  // empty = "cron:<id>"

	// Full OpenClaw jobs.json fields
	CreatedAtMs int64     `json:"createdAtMs,omitempty"`
	UpdatedAtMs int64     `json:"updatedAtMs,omitempty"`
	Schedule    *Schedule `json:"schedule,omitempty"`

	// "isolated" = fresh session per run; "persistent" = reuse same key
	SessionTarget string `json:"sessionTarget,omitempty"`
	// "now" = run missed runs immediately on startup; "skip" = skip missed
	WakeMode string `json:"wakeMode,omitempty"`

	Payload      *Payload  `json:"payload,omitempty"`
	Delivery     *Delivery `json:"delivery,omitempty"`
	LightContext bool      `json:"lightContext,omitempty"` // if true, use minimal bootstrap context (HEARTBEAT.md only)
	State        *JobState `json:"state,omitempty"`
}

// Schedule defines when a job runs (OpenClaw format).
type Schedule struct {
	Kind     string `json:"kind"`               // "every"
	EveryMs  int64  `json:"everyMs,omitempty"`  // interval in milliseconds
	AnchorMs int64  `json:"anchorMs,omitempty"` // epoch ms reference point
}

// Payload defines what runs when the job fires.
type Payload struct {
	Kind           string `json:"kind"`                     // "agentTurn"
	Message        string `json:"message"`                  // instruction text
	Model          string `json:"model,omitempty"`          // per-job model override
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"` // per-job timeout
}

// Delivery defines how results are delivered.
// Mode: "announce" = send to all paired users; "none" = suppress delivery.
type Delivery struct {
	Mode    string `json:"mode,omitempty"`    // "announce" or "none"
	Channel string `json:"channel,omitempty"` // "last" (currently only option)
}

// JobState tracks runtime state; persisted back to jobs.json.
type JobState struct {
	NextRunAtMs        int64  `json:"nextRunAtMs,omitempty"`
	LastRunAtMs        int64  `json:"lastRunAtMs,omitempty"`
	LastRunStatus      string `json:"lastRunStatus,omitempty"`
	LastStatus         string `json:"lastStatus,omitempty"`
	LastDurationMs     int64  `json:"lastDurationMs,omitempty"`
	LastDeliveryStatus string `json:"lastDeliveryStatus,omitempty"`
	ConsecutiveErrors  int    `json:"consecutiveErrors"`
	LastDelivered      bool   `json:"lastDelivered,omitempty"`
}

// ──────────────────────────────────────────────────────────────────────
// Job helpers — unified access across simple and full formats
// ──────────────────────────────────────────────────────────────────────

// EffectiveInstruction returns the message to send (supports both formats).
func (j *Job) EffectiveInstruction() string {
	if j.Payload != nil && j.Payload.Message != "" {
		return j.Payload.Message
	}
	return j.Instruction
}

// EffectiveModel returns the per-job model override, or "" if none.
func (j *Job) EffectiveModel() string {
	if j.Payload != nil {
		return j.Payload.Model
	}
	return ""
}

// EffectiveTimeout returns the per-job timeout, or 0 if none.
func (j *Job) EffectiveTimeout() time.Duration {
	if j.Payload != nil && j.Payload.TimeoutSeconds > 0 {
		return time.Duration(j.Payload.TimeoutSeconds) * time.Second
	}
	return 0
}

// EffectiveSessionKey returns the session key for this run.
// isolated: fresh key per run; persistent: reuse "cron:<id>".
func (j *Job) EffectiveSessionKey() string {
	if j.SessionTarget == "isolated" {
		return fmt.Sprintf("cron:%s:%d", j.ID, time.Now().UnixMilli())
	}
	if j.SessionKey != "" {
		return j.SessionKey
	}
	return "cron:" + j.ID
}

// EffectiveInterval returns the repeat interval (supports both formats).
func (j *Job) EffectiveInterval() (time.Duration, error) {
	if j.Schedule != nil && j.Schedule.Kind == "every" && j.Schedule.EveryMs > 0 {
		return time.Duration(j.Schedule.EveryMs) * time.Millisecond, nil
	}
	if j.Spec != "" {
		return intervalFromSpec(j.Spec)
	}
	return 0, fmt.Errorf("no schedule defined for job %s", j.ID)
}

// DisplaySchedule returns a human-readable schedule string.
func (j *Job) DisplaySchedule() string {
	if j.Schedule != nil && j.Schedule.Kind == "every" && j.Schedule.EveryMs > 0 {
		d := time.Duration(j.Schedule.EveryMs) * time.Millisecond
		return "every " + d.String()
	}
	if j.Spec != "" {
		return j.Spec
	}
	return "unknown"
}

// DisplayName returns the best available name for display.
func (j *Job) DisplayName() string {
	if j.Name != "" {
		return j.Name
	}
	return j.ID
}

// WantsDelivery returns true if the job wants results announced.
func (j *Job) WantsDelivery() bool {
	return j.Delivery != nil && j.Delivery.Mode == "announce"
}

// isSuppressible returns true if text should not be delivered (empty,
// whitespace-only, the "NO_REPLY" sentinel, or a bare ellipsis).
func isSuppressible(text string) bool {
	t := strings.TrimSpace(text)
	return t == "" || strings.EqualFold(t, "NO_REPLY") || t == "..." || t == "\u2026"
}

// ──────────────────────────────────────────────────────────────────────
// RunFunc and Deliverer
// ──────────────────────────────────────────────────────────────────────

// RunResult is returned by RunFunc after a job completes.
type RunResult struct {
	Text string
	Err  error
}

// RunFunc is the function called when a job fires.
// It receives the job so it can use model/timeout overrides.
type RunFunc func(ctx context.Context, job *Job) RunResult

// Deliverer is an alias for agentapi.Deliverer.
type Deliverer = agentapi.Deliverer

// ──────────────────────────────────────────────────────────────────────
// Manager
// ──────────────────────────────────────────────────────────────────────

// Manager schedules and manages cron jobs.
type Manager struct {
	mu          sync.Mutex
	jobs        []*Job
	runningJobs map[string]bool // tracks currently-executing job IDs (REQ-512)
	dir         string          // directory for crons.json (simple format)
	jobsFile    string          // path to jobs.json (full format); empty = not loaded
	runFunc     RunFunc         // called when a job fires
	deliverers  []Deliverer
	timers      map[string]*time.Timer
	started     bool            // true after Start() has been called
	runCtx      context.Context // set by Start(); used for scheduling jobs added later
	logger      *zap.SugaredLogger
}

// New creates a Manager that stores state in dir/crons.json.
// runFunc is called for each job execution.
func New(logger *zap.SugaredLogger, dir string, runFunc RunFunc) *Manager {
	m := &Manager{
		dir:         dir,
		runFunc:     runFunc,
		runningJobs: make(map[string]bool),
		timers:      make(map[string]*time.Timer),
		logger:      logger,
	}
	if err := m.load(); err != nil {
		m.logger.Warnf("cron: failed to load crons.json (starting fresh): %v", err)
	}
	return m
}

// LoadJobsFile loads the full-format jobs.json from the given path,
// merging into any already-loaded simple jobs (deduped by ID).
func (m *Manager) LoadJobsFile(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	var file struct {
		Version int    `json:"version"`
		Jobs    []*Job `json:"jobs"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.jobsFile = path

	// Build index of existing IDs
	existing := make(map[string]bool, len(m.jobs))
	for _, j := range m.jobs {
		existing[j.ID] = true
	}

	for _, j := range file.Jobs {
		if !existing[j.ID] {
			m.jobs = append(m.jobs, j)
		}
	}
	return nil
}

// AddDeliverer registers a channel deliverer for job result delivery.
func (m *Manager) AddDeliverer(d Deliverer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deliverers = append(m.deliverers, d)
}

// Start begins the scheduling loop. It blocks until ctx is cancelled.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	m.started = true
	m.runCtx = ctx
	// Collect enabled jobs under the lock to avoid racing with SetEnabled.
	var toSchedule []*Job
	for _, j := range m.jobs {
		if j.Enabled {
			toSchedule = append(toSchedule, j)
		}
	}
	m.mu.Unlock()

	for _, j := range toSchedule {
		m.scheduleJob(ctx, j)
	}

	<-ctx.Done()
	m.mu.Lock()
	for _, t := range m.timers {
		t.Stop()
	}
	m.mu.Unlock()
	return nil
}

// Add creates a new enabled job (simple format).
// If Start() has already been called, the job is scheduled immediately.
func (m *Manager) Add(spec, instruction string) (*Job, error) {
	if _, err := intervalFromSpec(spec); err != nil {
		return nil, fmt.Errorf("invalid spec %q: %w", spec, err)
	}
	j := &Job{
		ID:          newID(),
		Spec:        spec,
		Instruction: instruction,
		Enabled:     true,
	}
	m.mu.Lock()
	m.jobs = append(m.jobs, j)
	started := m.started
	ctx := m.runCtx
	m.mu.Unlock()
	if err := m.save(); err != nil {
		m.logger.Warnf("cron: save error: %v", err)
	}

	// Schedule immediately if Start() has been called
	if started {
		m.scheduleJob(ctx, j)
	}

	return j, nil
}

// Remove deletes a job by ID.
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, j := range m.jobs {
		if j.ID == id {
			if t, ok := m.timers[id]; ok {
				t.Stop()
				delete(m.timers, id)
			}
			m.jobs = append(m.jobs[:i], m.jobs[i+1:]...)
			m.saveAllLocked()
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrJobNotFound, id)
}

// SetEnabled enables or disables a job by ID.
// Re-enabling a job schedules it immediately if Start() has been called.
func (m *Manager) SetEnabled(id string, enabled bool) error {
	m.mu.Lock()
	var found *Job
	for _, j := range m.jobs {
		if j.ID == id {
			j.Enabled = enabled
			if !enabled {
				if t, ok := m.timers[id]; ok {
					t.Stop()
					delete(m.timers, id)
				}
			}
			found = j
			break
		}
	}
	if found == nil {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrJobNotFound, id)
	}
	started := m.started
	ctx := m.runCtx
	m.saveAllLocked()
	m.mu.Unlock()

	// Schedule after releasing the lock (scheduleJob acquires m.mu internally).
	// The job pointer remains valid because m.jobs stores *Job pointers.
	if enabled && started {
		m.scheduleJob(ctx, found)
	}

	return nil
}

// RunNow manually triggers a job by ID. Non-blocking.
func (m *Manager) RunNow(ctx context.Context, id string) error {
	m.mu.Lock()
	var jobCopy Job
	var found bool
	for _, j := range m.jobs {
		if j.ID == id {
			jobCopy = *j
			found = true
			break
		}
	}
	m.mu.Unlock()
	if !found {
		return fmt.Errorf("%w: %s", ErrJobNotFound, id)
	}
	go m.runJob(ctx, &jobCopy)
	return nil
}

// List returns a copy of all jobs.
func (m *Manager) List() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Job, len(m.jobs))
	for i, j := range m.jobs {
		cp := *j
		if j.Schedule != nil {
			s := *j.Schedule
			cp.Schedule = &s
		}
		if j.Payload != nil {
			p := *j.Payload
			cp.Payload = &p
		}
		if j.Delivery != nil {
			d := *j.Delivery
			cp.Delivery = &d
		}
		if j.State != nil {
			st := *j.State
			cp.State = &st
		}
		out[i] = &cp
	}
	return out
}

// Dir returns the state directory used for crons.json and run logs.
func (m *Manager) Dir() string { return m.dir }

// ──────────────────────────────────────────────────────────────────────
// Scheduling
// ──────────────────────────────────────────────────────────────────────

// backoffDelay computes an exponential backoff delay based on consecutive
// errors and the job's base interval. Returns 0 if no errors.
// Formula: baseInterval * 2^(errors-1), capped at 4 hours.
func backoffDelay(consecutiveErrors int, baseInterval time.Duration) time.Duration {
	if consecutiveErrors <= 0 {
		return 0
	}
	exp := consecutiveErrors - 1
	if exp > 10 {
		exp = 10 // cap exponent to prevent overflow
	}
	delay := baseInterval * (1 << exp)
	const maxBackoff = 4 * time.Hour
	if delay > maxBackoff {
		delay = maxBackoff
	}
	return delay
}

func (m *Manager) scheduleJob(ctx context.Context, j *Job) {
	wait := max(m.computeNextWait(j), 0)

	// REQ-501: apply exponential backoff when consecutive errors exist.
	m.mu.Lock()
	var consecutiveErrors int
	if j.State != nil {
		consecutiveErrors = j.State.ConsecutiveErrors
	}
	m.mu.Unlock()

	if consecutiveErrors > 0 {
		baseInterval := time.Hour // default fallback
		if iv, err := j.EffectiveInterval(); err == nil {
			baseInterval = iv
		}
		bo := backoffDelay(consecutiveErrors, baseInterval)
		if bo > 0 {
			m.logger.Infof("cron: job %s (%s) has %d consecutive errors, adding %s backoff",
				j.ID, j.DisplayName(), consecutiveErrors, bo)
			wait += bo
		}
	}

	// Update state.nextRunAtMs
	m.mu.Lock()
	if j.State == nil {
		j.State = &JobState{}
	}
	j.State.NextRunAtMs = time.Now().Add(wait).UnixMilli()
	m.mu.Unlock()

	t := time.AfterFunc(wait, func() {
		if ctx.Err() != nil {
			return
		}
		m.runJob(ctx, j)

		// Reschedule if still enabled
		m.mu.Lock()
		enabled := false
		for _, jj := range m.jobs {
			if jj.ID == j.ID && jj.Enabled {
				enabled = true
				break
			}
		}
		m.mu.Unlock()

		if enabled {
			m.scheduleJob(ctx, j)
		}
	})

	m.mu.Lock()
	if old, ok := m.timers[j.ID]; ok {
		old.Stop()
	}
	m.timers[j.ID] = t
	m.mu.Unlock()
}

// computeNextWait determines how long to wait before the next run.
// For jobs with schedule.kind:"every" and wakeMode:"now", it computes
// how many intervals have elapsed since anchor and fires immediately
// if a run was missed.
func (m *Manager) computeNextWait(j *Job) time.Duration {
	now := time.Now()

	// Full format: schedule.kind:"every"
	if j.Schedule != nil && j.Schedule.Kind == "every" && j.Schedule.EveryMs > 0 {
		interval := time.Duration(j.Schedule.EveryMs) * time.Millisecond
		anchor := time.UnixMilli(j.Schedule.AnchorMs)

		if now.Before(anchor) {
			return time.Until(anchor)
		}

		elapsed := now.Sub(anchor)
		periods := elapsed / interval
		nextRun := anchor.Add((periods + 1) * interval)

		// wakeMode:"now" — if a run was missed (lastRunAtMs < expected), fire immediately
		if j.WakeMode == "now" && j.State != nil && j.State.LastRunAtMs > 0 {
			lastRun := time.UnixMilli(j.State.LastRunAtMs)
			expectedPrev := anchor.Add(periods * interval)
			if lastRun.Before(expectedPrev) {
				return 0 // missed run — fire now
			}
		} else if j.WakeMode == "now" && (j.State == nil || j.State.LastRunAtMs == 0) {
			// Never run before — fire now
			return 0
		}

		return time.Until(nextRun)
	}

	// Simple spec format
	if j.Spec != "" {
		next, err := nextRun(j.Spec)
		if err != nil {
			return time.Hour // fallback
		}
		return time.Until(next)
	}

	return time.Hour // fallback
}

// runJob executes a single job run and updates state.
func (m *Manager) runJob(ctx context.Context, j *Job) {
	if m.runFunc == nil {
		return
	}

	// REQ-512: concurrent guard — skip if this job is already running.
	m.mu.Lock()
	if m.runningJobs[j.ID] {
		m.mu.Unlock()
		m.logger.Infof("cron: skipping job %s (%s) — already running", j.ID, j.DisplayName())
		return
	}
	m.runningJobs[j.ID] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.runningJobs, j.ID)
		m.mu.Unlock()
	}()

	start := time.Now()

	result := m.runFunc(ctx, j)

	duration := time.Since(start)

	// Update state
	m.mu.Lock()
	if j.State == nil {
		j.State = &JobState{}
	}
	j.State.LastRunAtMs = start.UnixMilli()
	j.State.LastDurationMs = duration.Milliseconds()

	if result.Err != nil {
		j.State.LastRunStatus = "error"
		j.State.LastStatus = "error"
		j.State.ConsecutiveErrors++
		j.State.LastDelivered = false
		j.State.LastDeliveryStatus = ""
	} else {
		j.State.LastRunStatus = "ok"
		j.State.LastStatus = "ok"
		j.State.ConsecutiveErrors = 0
	}

	// Copy deliverers while locked
	deliverers := make([]Deliverer, len(m.deliverers))
	copy(deliverers, m.deliverers)
	m.mu.Unlock()

	// Deliver result to paired users if configured (REQ-503: suppress NO_REPLY / empty)
	if result.Err == nil && result.Text != "" && !isSuppressible(result.Text) {
		if j.Delivery != nil && j.Delivery.Mode == "none" {
			// Delivery explicitly suppressed
			m.mu.Lock()
			j.State.LastDelivered = false
			j.State.LastDeliveryStatus = "suppressed"
			m.mu.Unlock()
		} else if j.WantsDelivery() {
			delivered := false
			for _, d := range deliverers {
				d.SendToAllPaired(result.Text)
				delivered = true
			}
			m.mu.Lock()
			j.State.LastDelivered = delivered
			if delivered {
				j.State.LastDeliveryStatus = "delivered"
			} else {
				j.State.LastDeliveryStatus = "no_deliverers"
			}
			m.mu.Unlock()
		}
	}

	// Persist state
	m.mu.Lock()
	m.saveAllLocked()
	m.mu.Unlock()

	// Append run log entry (REQ-430) — best-effort.
	func() {
		errStr := ""
		status := "ok"
		if result.Err != nil {
			errStr = result.Err.Error()
			status = "error"
		}
		m.mu.Lock()
		delivered := j.State != nil && j.State.LastDelivered
		deliveryStatus := ""
		if j.State != nil {
			deliveryStatus = j.State.LastDeliveryStatus
		}
		sessionKey := j.EffectiveSessionKey()
		instruction := j.EffectiveInstruction()
		m.mu.Unlock()

		summary := result.Text
		if len(summary) > 500 {
			summary = summary[:500]
		}

		entry := RunLogEntry{
			TS:             start.UnixMilli(),
			JobID:          j.ID,
			Action:         instruction,
			Status:         status,
			Error:          errStr,
			Summary:        summary,
			Delivered:      delivered,
			DeliveryStatus: deliveryStatus,
			SessionKey:     sessionKey,
			DurationMs:     duration.Milliseconds(),
		}
		if err := AppendRunLog(m.logger, m.dir, entry); err != nil {
			m.logger.Debugf("cron: run log append failed (non-fatal): %v", err)
		}
	}()
}

// ──────────────────────────────────────────────────────────────────────
// Spec parsing (simple format backward compatibility)
// ──────────────────────────────────────────────────────────────────────

// nextRun computes the next time a job should run given a simple spec.
func nextRun(spec string) (time.Time, error) {
	spec = strings.TrimSpace(spec)
	now := time.Now()

	switch spec {
	case "@hourly":
		return now.Truncate(time.Hour).Add(time.Hour), nil
	case "@daily":
		next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Add(24 * time.Hour)
		return next, nil
	case "@weekly":
		daysUntilSunday := (7 - int(now.Weekday())) % 7
		if daysUntilSunday == 0 {
			daysUntilSunday = 7
		}
		next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Add(time.Duration(daysUntilSunday) * 24 * time.Hour)
		return next, nil
	}

	// @every <duration>
	if after, ok := strings.CutPrefix(spec, "@every "); ok {
		durStr := after
		d, err := time.ParseDuration(durStr)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid duration %q: %w", durStr, err)
		}
		if d <= 0 {
			return time.Time{}, fmt.Errorf("duration must be positive")
		}
		return now.Add(d), nil
	}

	// HH:MM — daily at that time
	if len(spec) == 5 && spec[2] == ':' {
		hParts := strings.Split(spec, ":")
		if len(hParts) == 2 {
			h, errH := strconv.Atoi(hParts[0])
			min, errM := strconv.Atoi(hParts[1])
			if errH == nil && errM == nil && h >= 0 && h < 24 && min >= 0 && min < 60 {
				next := time.Date(now.Year(), now.Month(), now.Day(), h, min, 0, 0, now.Location())
				if !next.After(now) {
					next = next.Add(24 * time.Hour)
				}
				return next, nil
			}
		}
	}

	return time.Time{}, fmt.Errorf("unsupported spec %q (use @hourly, @daily, @weekly, @every <duration>, or HH:MM)", spec)
}

// intervalFromSpec returns the repeat interval for a simple spec.
func intervalFromSpec(spec string) (time.Duration, error) {
	spec = strings.TrimSpace(spec)

	switch spec {
	case "@hourly":
		return time.Hour, nil
	case "@daily":
		return 24 * time.Hour, nil
	case "@weekly":
		return 7 * 24 * time.Hour, nil
	}

	if after, ok := strings.CutPrefix(spec, "@every "); ok {
		durStr := after
		d, err := time.ParseDuration(durStr)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", durStr, err)
		}
		if d <= 0 {
			return 0, fmt.Errorf("duration must be positive")
		}
		return d, nil
	}

	// HH:MM = daily
	if len(spec) == 5 && spec[2] == ':' {
		hParts := strings.Split(spec, ":")
		if len(hParts) == 2 {
			h, errH := strconv.Atoi(hParts[0])
			min, errM := strconv.Atoi(hParts[1])
			if errH == nil && errM == nil && h >= 0 && h < 24 && min >= 0 && min < 60 {
				return 24 * time.Hour, nil
			}
		}
	}

	return 0, fmt.Errorf("unsupported spec %q", spec)
}

// ──────────────────────────────────────────────────────────────────────
// Persistence
// ──────────────────────────────────────────────────────────────────────

func (m *Manager) load() error {
	path := filepath.Join(m.dir, "crons.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &m.jobs)
}

// saveAllLocked persists both simple jobs (crons.json) and full jobs (jobs.json).
// Must be called with m.mu held.
func (m *Manager) saveAllLocked() {
	if err := m.saveCronsLocked(); err != nil {
		m.logger.Warnf("cron: save crons.json: %v", err)
	}
	if m.jobsFile != "" {
		if err := m.saveJobsFileLocked(); err != nil {
			m.logger.Warnf("cron: save jobs.json: %v", err)
		}
	}
}

// saveCronsLocked saves simple-format jobs to dir/crons.json.
func (m *Manager) saveCronsLocked() error {
	// Only save jobs that are in simple format (have Spec, no Schedule)
	var simple []*Job
	for _, j := range m.jobs {
		if j.Spec != "" && j.Schedule == nil {
			simple = append(simple, j)
		}
	}
	if err := os.MkdirAll(m.dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(simple, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(filepath.Join(m.dir, "crons.json"), data, 0600)
}

// saveJobsFileLocked saves full-format jobs to the jobs.json path.
func (m *Manager) saveJobsFileLocked() error {
	// Only save jobs that have the full format (have Schedule)
	var full []*Job
	for _, j := range m.jobs {
		if j.Schedule != nil {
			full = append(full, j)
		}
	}
	file := struct {
		Version int    `json:"version"`
		Jobs    []*Job `json:"jobs"`
	}{
		Version: 1,
		Jobs:    full,
	}
	if err := os.MkdirAll(filepath.Dir(m.jobsFile), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(m.jobsFile, data, 0600)
}

func (m *Manager) save() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveAllLocked()
	return nil
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
