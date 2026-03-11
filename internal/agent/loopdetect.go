// Package agent — loopdetect.go implements multi-detector tool loop detection (REQ-410, REQ-411).
//
// Four detectors run on each tool call:
//  1. Generic repeat — same tool+args N times (warn@10, critical@20)
//  2. Known poll no-progress — poll-like calls with identical outcomes (warn@10, critical@20)
//  3. Ping-pong — alternating between two patterns with no progress (warn@10, critical@20)
//  4. Global circuit breaker — any single tool+args repeated 30 times = hard stop
//
// Tool calls are hashed via name + SHA-256(stableJSON(params)).
// A sliding window of the last historySize calls is maintained per session.
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// LoopDetectionLevel indicates the severity of a loop detection finding.
type LoopDetectionLevel string

const (
	LoopLevelNone     LoopDetectionLevel = ""
	LoopLevelWarning  LoopDetectionLevel = "warning"
	LoopLevelCritical LoopDetectionLevel = "critical"
)

// LoopDetectorKind identifies which detector triggered.
type LoopDetectorKind string

const (
	DetectorGenericRepeat        LoopDetectorKind = "generic_repeat"
	DetectorKnownPollNoProgress  LoopDetectorKind = "known_poll_no_progress"
	DetectorPingPong             LoopDetectorKind = "ping_pong"
	DetectorGlobalCircuitBreaker LoopDetectorKind = "global_circuit_breaker"
)

// LoopDetectionResult is returned by DetectLoop with the detection outcome.
type LoopDetectionResult struct {
	Stuck    bool
	Level    LoopDetectionLevel
	Detector LoopDetectorKind
	Message  string
}

// ToolCallRecord tracks a single tool call in the sliding window.
type ToolCallRecord struct {
	ToolName   string
	ArgsHash   string // SHA-256 of stableJSON(params)
	ResultHash string // SHA-256 of outcome (set after execution)
	CallHash   string // name:argsHash composite key
	TS         time.Time
}

// ToolLoopDetectionConfig holds thresholds for the multi-detector system.
type ToolLoopDetectionConfig struct {
	Enabled                       bool `json:"enabled"`
	HistorySize                   int  `json:"historySize"`
	WarningThreshold              int  `json:"warningThreshold"`
	CriticalThreshold             int  `json:"criticalThreshold"`
	GlobalCircuitBreakerThreshold int  `json:"globalCircuitBreakerThreshold"`
	GenericRepeat                 bool `json:"genericRepeat"`
	KnownPollNoProgress           bool `json:"knownPollNoProgress"`
	PingPong                      bool `json:"pingPong"`
}

// DefaultToolLoopDetectionConfig returns a config with sensible defaults.
func DefaultToolLoopDetectionConfig() ToolLoopDetectionConfig {
	return ToolLoopDetectionConfig{
		Enabled:                       true,
		HistorySize:                   30,
		WarningThreshold:              10,
		CriticalThreshold:             20,
		GlobalCircuitBreakerThreshold: 30,
		GenericRepeat:                 true,
		KnownPollNoProgress:           true,
		PingPong:                      true,
	}
}

// ToolLoopDetector tracks tool call history and runs multi-detector analysis.
type ToolLoopDetector struct {
	history []ToolCallRecord
	cfg     ToolLoopDetectionConfig
}

// NewToolLoopDetector creates a detector with the given config.
func NewToolLoopDetector(cfg ToolLoopDetectionConfig) *ToolLoopDetector {
	if cfg.HistorySize <= 0 {
		cfg.HistorySize = 30
	}
	if cfg.WarningThreshold <= 0 {
		cfg.WarningThreshold = 10
	}
	if cfg.CriticalThreshold <= 0 {
		cfg.CriticalThreshold = 20
	}
	if cfg.GlobalCircuitBreakerThreshold <= 0 {
		cfg.GlobalCircuitBreakerThreshold = 30
	}
	return &ToolLoopDetector{cfg: cfg}
}

// RecordCall adds a tool call to the sliding window. Returns the record index.
func (d *ToolLoopDetector) RecordCall(toolName, argsJSON string) int {
	argsHash := hashArgs(argsJSON)
	callHash := toolName + ":" + argsHash
	rec := ToolCallRecord{
		ToolName: toolName,
		ArgsHash: argsHash,
		CallHash: callHash,
		TS:       time.Now(),
	}
	d.history = append(d.history, rec)
	// Trim sliding window
	if len(d.history) > d.cfg.HistorySize {
		d.history = d.history[len(d.history)-d.cfg.HistorySize:]
	}
	return len(d.history) - 1
}

// RecordOutcome sets the result hash for the most recent call at the given index.
func (d *ToolLoopDetector) RecordOutcome(idx int, result string) {
	if idx >= 0 && idx < len(d.history) {
		d.history[idx].ResultHash = hashResult(result)
	}
}

// DetectLoop runs all enabled detectors and returns the highest-severity finding.
func (d *ToolLoopDetector) DetectLoop() LoopDetectionResult {
	if !d.cfg.Enabled || len(d.history) == 0 {
		return LoopDetectionResult{}
	}

	best := LoopDetectionResult{}

	// 1. Global circuit breaker (always runs, highest priority)
	if r := d.detectGlobalCircuitBreaker(); r.Stuck && isSeverer(r.Level, best.Level) {
		best = r
	}

	// 2. Generic repeat
	if d.cfg.GenericRepeat {
		if r := d.detectGenericRepeat(); r.Stuck && isSeverer(r.Level, best.Level) {
			best = r
		}
	}

	// 3. Known poll no-progress
	if d.cfg.KnownPollNoProgress {
		if r := d.detectPollNoProgress(); r.Stuck && isSeverer(r.Level, best.Level) {
			best = r
		}
	}

	// 4. Ping-pong
	if d.cfg.PingPong {
		if r := d.detectPingPong(); r.Stuck && isSeverer(r.Level, best.Level) {
			best = r
		}
	}

	return best
}

// detectGenericRepeat finds the same callHash repeated N times in history.
func (d *ToolLoopDetector) detectGenericRepeat() LoopDetectionResult {
	if len(d.history) == 0 {
		return LoopDetectionResult{}
	}
	last := d.history[len(d.history)-1]
	count := 0
	for _, rec := range d.history {
		if rec.CallHash == last.CallHash {
			count++
		}
	}
	return d.thresholdResult(count, DetectorGenericRepeat, last.ToolName,
		"Tool %q with identical arguments repeated %d times")
}

// detectPollNoProgress detects poll-like commands with identical outcomes.
func (d *ToolLoopDetector) detectPollNoProgress() LoopDetectionResult {
	if len(d.history) == 0 {
		return LoopDetectionResult{}
	}
	last := d.history[len(d.history)-1]
	if last.ResultHash == "" {
		return LoopDetectionResult{}
	}

	// Count consecutive identical outcome for the same call
	count := 0
	for i := len(d.history) - 1; i >= 0; i-- {
		rec := d.history[i]
		if rec.CallHash != last.CallHash || rec.ResultHash != last.ResultHash {
			break
		}
		count++
	}

	return d.thresholdResult(count, DetectorKnownPollNoProgress, last.ToolName,
		"Tool %q polling with no progress — identical outcome %d times")
}

// detectPingPong detects alternating between two call patterns with no progress.
func (d *ToolLoopDetector) detectPingPong() LoopDetectionResult {
	n := len(d.history)
	if n < 4 {
		return LoopDetectionResult{}
	}

	// Check if the last 4+ entries alternate between two distinct call hashes
	a := d.history[n-1].CallHash
	b := d.history[n-2].CallHash
	if a == b {
		return LoopDetectionResult{}
	}

	// Count alternating A, B, A, B... backwards
	pairs := 0
	noProgress := true
	for i := n - 1; i >= 1; i -= 2 {
		if d.history[i].CallHash == a && d.history[i-1].CallHash == b {
			pairs++
			// Check if outcomes are monotonous (no progress evidence)
			if d.history[i].ResultHash != "" && i >= 2 {
				// Compare with 2 entries back (same position in pattern)
				if i-2 >= 0 && d.history[i-2].ResultHash != "" &&
					d.history[i].ResultHash != d.history[i-2].ResultHash {
					noProgress = false
				}
			}
		} else {
			break
		}
	}

	if !noProgress {
		return LoopDetectionResult{}
	}

	count := pairs * 2 // total calls in the ping-pong
	names := fmt.Sprintf("%s ↔ %s", d.history[n-1].ToolName, d.history[n-2].ToolName)
	return d.thresholdResult(count, DetectorPingPong, names,
		"Ping-pong between %q — %d alternating calls with no progress")
}

// detectGlobalCircuitBreaker fires if any single callHash has been seen N times.
func (d *ToolLoopDetector) detectGlobalCircuitBreaker() LoopDetectionResult {
	counts := make(map[string]int)
	var maxHash string
	var maxCount int
	for _, rec := range d.history {
		counts[rec.CallHash]++
		if counts[rec.CallHash] > maxCount {
			maxCount = counts[rec.CallHash]
			maxHash = rec.CallHash
		}
	}
	if maxCount >= d.cfg.GlobalCircuitBreakerThreshold {
		toolName := maxHash
		// Extract tool name from hash
		if idx := strings.Index(maxHash, ":"); idx > 0 {
			toolName = maxHash[:idx]
		}
		return LoopDetectionResult{
			Stuck:    true,
			Level:    LoopLevelCritical,
			Detector: DetectorGlobalCircuitBreaker,
			Message:  fmt.Sprintf("Global circuit breaker: %q repeated %d times (threshold %d). Hard stop.", toolName, maxCount, d.cfg.GlobalCircuitBreakerThreshold),
		}
	}
	return LoopDetectionResult{}
}

// thresholdResult maps a count to warning/critical levels.
func (d *ToolLoopDetector) thresholdResult(count int, detector LoopDetectorKind, toolName, msgFmt string) LoopDetectionResult {
	if count >= d.cfg.CriticalThreshold {
		return LoopDetectionResult{
			Stuck:    true,
			Level:    LoopLevelCritical,
			Detector: detector,
			Message:  fmt.Sprintf(msgFmt, toolName, count) + " — breaking loop.",
		}
	}
	if count >= d.cfg.WarningThreshold {
		return LoopDetectionResult{
			Stuck:    true,
			Level:    LoopLevelWarning,
			Detector: detector,
			Message:  fmt.Sprintf(msgFmt, toolName, count) + " — consider a different approach.",
		}
	}
	return LoopDetectionResult{}
}

// isSeverer returns true if a is more severe than b.
func isSeverer(a, b LoopDetectionLevel) bool {
	return severity(a) > severity(b)
}

func severity(l LoopDetectionLevel) int {
	switch l {
	case LoopLevelCritical:
		return 2
	case LoopLevelWarning:
		return 1
	default:
		return 0
	}
}

// hashArgs produces a stable hash of tool arguments (SHA-256 of sorted JSON keys).
func hashArgs(argsJSON string) string {
	stable := stableJSON(argsJSON)
	h := sha256.Sum256([]byte(stable))
	return hex.EncodeToString(h[:])
}

// hashResult produces a SHA-256 hash of a tool result string.
func hashResult(result string) string {
	h := sha256.Sum256([]byte(result))
	return hex.EncodeToString(h[:])
}

// stableJSON re-marshals JSON with sorted keys for deterministic hashing.
func stableJSON(raw string) string {
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return raw // fallback: use as-is
	}
	sorted := sortKeys(parsed)
	out, err := json.Marshal(sorted)
	if err != nil {
		return raw
	}
	return string(out)
}

// sortKeys recursively sorts map keys for deterministic JSON.
func sortKeys(v any) any {
	switch val := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		sorted := make(map[string]any, len(val))
		for _, k := range keys {
			sorted[k] = sortKeys(val[k])
		}
		return sorted
	case []any:
		for i, item := range val {
			val[i] = sortKeys(item)
		}
		return val
	default:
		return v
	}
}

// ──────────────────────────────────────────────────────────────────────
// Command Poll Backoff (REQ-411)
// ──────────────────────────────────────────────────────────────────────

// PollBackoff tracks per-command poll counts and suggests backoff delays.
type PollBackoff struct {
	counts map[string]pollState
}

type pollState struct {
	count    int
	lastSeen time.Time
}

// NewPollBackoff creates a new backoff tracker.
func NewPollBackoff() *PollBackoff {
	return &PollBackoff{counts: make(map[string]pollState)}
}

// Record records a poll for a command. Returns the suggested delay.
// If hasNewOutput is true, the counter resets.
func (pb *PollBackoff) Record(commandKey string, hasNewOutput bool) time.Duration {
	pb.prune()

	if hasNewOutput {
		delete(pb.counts, commandKey)
		return 0
	}

	state := pb.counts[commandKey]
	state.count++
	state.lastSeen = time.Now()
	pb.counts[commandKey] = state

	return pb.suggestDelay(state.count)
}

// suggestDelay returns an exponential backoff delay capped at 60s.
func (pb *PollBackoff) suggestDelay(count int) time.Duration {
	delays := []time.Duration{
		5 * time.Second,
		10 * time.Second,
		30 * time.Second,
		60 * time.Second,
	}
	idx := count - 1
	if idx < 0 {
		return 0
	}
	if idx >= len(delays) {
		return delays[len(delays)-1]
	}
	return delays[idx]
}

// prune removes stale entries older than 1 hour.
func (pb *PollBackoff) prune() {
	cutoff := time.Now().Add(-1 * time.Hour)
	for k, v := range pb.counts {
		if v.lastSeen.Before(cutoff) {
			delete(pb.counts, k)
		}
	}
}
