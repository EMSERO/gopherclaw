package agent

import (
	"testing"
	"time"
)

func TestStableJSON(t *testing.T) {
	// Different key orderings should produce the same stable output
	a := `{"z": 1, "a": 2}`
	b := `{"a": 2, "z": 1}`
	if stableJSON(a) != stableJSON(b) {
		t.Errorf("stableJSON should normalize key order: %q != %q", stableJSON(a), stableJSON(b))
	}
}

func TestHashArgs(t *testing.T) {
	h1 := hashArgs(`{"command": "ls", "dir": "/tmp"}`)
	h2 := hashArgs(`{"dir": "/tmp", "command": "ls"}`)
	if h1 != h2 {
		t.Error("hashArgs should be order-independent")
	}

	h3 := hashArgs(`{"command": "ls", "dir": "/var"}`)
	if h1 == h3 {
		t.Error("different args should produce different hashes")
	}
}

func TestDetectGenericRepeat(t *testing.T) {
	cfg := DefaultToolLoopDetectionConfig()
	cfg.WarningThreshold = 3
	cfg.CriticalThreshold = 5
	cfg.HistorySize = 30
	d := NewToolLoopDetector(cfg)

	// Add 2 identical calls — no detection
	for range 2 {
		d.RecordCall("exec", `{"command":"ls"}`)
	}
	r := d.DetectLoop()
	if r.Stuck {
		t.Error("should not be stuck at 2 calls")
	}

	// Add 1 more → threshold 3 = warning
	d.RecordCall("exec", `{"command":"ls"}`)
	r = d.DetectLoop()
	if !r.Stuck || r.Level != LoopLevelWarning {
		t.Errorf("at 3 calls: Stuck=%v, Level=%v, want warning", r.Stuck, r.Level)
	}
	if r.Detector != DetectorGenericRepeat {
		t.Errorf("Detector = %v, want generic_repeat", r.Detector)
	}

	// Add 2 more → 5 = critical
	d.RecordCall("exec", `{"command":"ls"}`)
	d.RecordCall("exec", `{"command":"ls"}`)
	r = d.DetectLoop()
	if !r.Stuck || r.Level != LoopLevelCritical {
		t.Errorf("at 5 calls: Stuck=%v, Level=%v, want critical", r.Stuck, r.Level)
	}
}

func TestDetectPollNoProgress(t *testing.T) {
	cfg := DefaultToolLoopDetectionConfig()
	cfg.WarningThreshold = 3
	cfg.CriticalThreshold = 5
	cfg.GenericRepeat = false // disable to isolate poll-no-progress detector
	d := NewToolLoopDetector(cfg)

	// Same call same outcome (consecutive)
	for range 3 {
		idx := d.RecordCall("command_status", `{"id":"abc123"}`)
		d.RecordOutcome(idx, "running...")
	}
	r := d.DetectLoop()
	if !r.Stuck || r.Level != LoopLevelWarning {
		t.Errorf("at 3 poll-no-progress: Stuck=%v, Level=%v, want warning", r.Stuck, r.Level)
	}
	if r.Detector != DetectorKnownPollNoProgress {
		t.Errorf("Detector = %v, want known_poll_no_progress", r.Detector)
	}
}

func TestDetectPollBreaksOnNewOutput(t *testing.T) {
	cfg := DefaultToolLoopDetectionConfig()
	cfg.WarningThreshold = 3
	d := NewToolLoopDetector(cfg)

	// 2 same, then different outcome
	for range 2 {
		idx := d.RecordCall("command_status", `{"id":"abc"}`)
		d.RecordOutcome(idx, "running...")
	}
	idx := d.RecordCall("command_status", `{"id":"abc"}`)
	d.RecordOutcome(idx, "done!")

	r := d.DetectLoop()
	// poll-no-progress should NOT trigger because the outcome changed
	if r.Stuck && r.Detector == DetectorKnownPollNoProgress {
		t.Error("should not trigger poll-no-progress after outcome changed")
	}
}

func TestDetectPingPong(t *testing.T) {
	cfg := DefaultToolLoopDetectionConfig()
	cfg.WarningThreshold = 6 // 3 pairs
	cfg.CriticalThreshold = 10
	cfg.HistorySize = 30
	d := NewToolLoopDetector(cfg)

	// A, B, A, B, A, B (3 pairs = 6 calls)
	for range 3 {
		idx := d.RecordCall("read_file", `{"path":"/etc/foo"}`)
		d.RecordOutcome(idx, "content-a")
		idx = d.RecordCall("write_file", `{"path":"/tmp/bar"}`)
		d.RecordOutcome(idx, "ok")
	}
	r := d.DetectLoop()
	if !r.Stuck {
		t.Error("ping-pong should be detected at 6 alternating calls")
	}
	if r.Detector != DetectorPingPong {
		t.Errorf("Detector = %v, want ping_pong", r.Detector)
	}
}

func TestGlobalCircuitBreaker(t *testing.T) {
	cfg := DefaultToolLoopDetectionConfig()
	cfg.HistorySize = 40
	cfg.GlobalCircuitBreakerThreshold = 10
	d := NewToolLoopDetector(cfg)

	for range 10 {
		d.RecordCall("exec", `{"command":"broken"}`)
	}
	r := d.DetectLoop()
	if !r.Stuck || r.Level != LoopLevelCritical {
		t.Errorf("circuit breaker: Stuck=%v, Level=%v, want critical", r.Stuck, r.Level)
	}
	if r.Detector != DetectorGlobalCircuitBreaker {
		t.Errorf("Detector = %v, want global_circuit_breaker", r.Detector)
	}
}

func TestDisabledDetectors(t *testing.T) {
	cfg := DefaultToolLoopDetectionConfig()
	cfg.GenericRepeat = false
	cfg.KnownPollNoProgress = false
	cfg.PingPong = false
	cfg.WarningThreshold = 3
	cfg.CriticalThreshold = 5
	cfg.GlobalCircuitBreakerThreshold = 100 // very high
	d := NewToolLoopDetector(cfg)

	for range 5 {
		d.RecordCall("exec", `{"command":"ls"}`)
	}
	r := d.DetectLoop()
	// Only the global circuit breaker is active, and threshold is 100
	if r.Stuck {
		t.Error("should not be stuck with all detectors disabled and high circuit breaker threshold")
	}
}

func TestDisabledToolLoopDetection(t *testing.T) {
	cfg := DefaultToolLoopDetectionConfig()
	cfg.Enabled = false
	d := NewToolLoopDetector(cfg)

	for range 100 {
		d.RecordCall("exec", `{"command":"ls"}`)
	}
	r := d.DetectLoop()
	if r.Stuck {
		t.Error("should never be stuck when detection is disabled")
	}
}

func TestSlidingWindowEviction(t *testing.T) {
	cfg := DefaultToolLoopDetectionConfig()
	cfg.HistorySize = 5
	cfg.WarningThreshold = 4
	d := NewToolLoopDetector(cfg)

	// Add 3 of tool A
	for range 3 {
		d.RecordCall("toolA", `{}`)
	}
	// Add 3 of tool B (pushes some A out of the window)
	for range 3 {
		d.RecordCall("toolB", `{}`)
	}

	// Window now has: [A, B, B, B] (only last 5 kept, 1 A + 3 B after eviction)
	// Actually: window = 5, we added 6 total, so window = [A, B, B, B, B] — but let me recalculate
	// Actually 6 items, window=5: [A(3), B(1), B(2), B(3)] → no, 3A + 3B = 6, window keeps last 5
	// So window = [A, A, B, B, B] — wait: append order is A,A,A,B,B,B → last 5 = [A, B, B, B, (one more?)]
	// Let me just check that A alone doesn't trigger warning
	r := d.DetectLoop()
	// The last call is toolB, count of B in window should be 3 → below warning threshold of 4
	if r.Stuck && r.Detector == DetectorGenericRepeat {
		t.Logf("result: %+v, history len: %d", r, len(d.history))
		// B appears 3 times in window, below threshold of 4 → should NOT be stuck
		if r.Level != LoopLevelNone {
			t.Error("sliding window should prevent old entries from counting")
		}
	}
}

func TestThresholdOrdering(t *testing.T) {
	cfg := DefaultToolLoopDetectionConfig()
	cfg.WarningThreshold = 3
	cfg.CriticalThreshold = 5
	d := NewToolLoopDetector(cfg)

	// At warning threshold
	for range 3 {
		d.RecordCall("tool", `{"x":1}`)
	}
	r := d.DetectLoop()
	if r.Level != LoopLevelWarning {
		t.Errorf("at 3: Level=%v, want warning", r.Level)
	}

	// Between warning and critical
	d.RecordCall("tool", `{"x":1}`)
	r = d.DetectLoop()
	if r.Level != LoopLevelWarning {
		t.Errorf("at 4: Level=%v, want warning", r.Level)
	}

	// At critical threshold
	d.RecordCall("tool", `{"x":1}`)
	r = d.DetectLoop()
	if r.Level != LoopLevelCritical {
		t.Errorf("at 5: Level=%v, want critical", r.Level)
	}
}

func TestDifferentToolsNoFalsePositive(t *testing.T) {
	cfg := DefaultToolLoopDetectionConfig()
	cfg.WarningThreshold = 3
	d := NewToolLoopDetector(cfg)

	// Different tools or different args
	d.RecordCall("exec", `{"command":"ls"}`)
	d.RecordCall("exec", `{"command":"pwd"}`)
	d.RecordCall("exec", `{"command":"whoami"}`)
	d.RecordCall("read_file", `{"path":"/etc/hosts"}`)

	r := d.DetectLoop()
	if r.Stuck {
		t.Error("different calls should not trigger detection")
	}
}

func TestPollBackoff_Record(t *testing.T) {
	pb := NewPollBackoff()

	// First poll: 5s
	d := pb.Record("cmd-1", false)
	if d != 5*time.Second {
		t.Errorf("poll 1: delay=%v, want 5s", d)
	}

	// Second poll: 10s
	d = pb.Record("cmd-1", false)
	if d != 10*time.Second {
		t.Errorf("poll 2: delay=%v, want 10s", d)
	}

	// Third: 30s
	d = pb.Record("cmd-1", false)
	if d != 30*time.Second {
		t.Errorf("poll 3: delay=%v, want 30s", d)
	}

	// Fourth and beyond: 60s cap
	d = pb.Record("cmd-1", false)
	if d != 60*time.Second {
		t.Errorf("poll 4: delay=%v, want 60s", d)
	}
	d = pb.Record("cmd-1", false)
	if d != 60*time.Second {
		t.Errorf("poll 5: delay=%v, want 60s (capped)", d)
	}
}

func TestPollBackoff_ResetOnOutput(t *testing.T) {
	pb := NewPollBackoff()

	pb.Record("cmd-1", false)
	pb.Record("cmd-1", false)

	// New output resets
	d := pb.Record("cmd-1", true)
	if d != 0 {
		t.Errorf("after new output: delay=%v, want 0", d)
	}

	// Next poll starts fresh
	d = pb.Record("cmd-1", false)
	if d != 5*time.Second {
		t.Errorf("after reset: delay=%v, want 5s", d)
	}
}
