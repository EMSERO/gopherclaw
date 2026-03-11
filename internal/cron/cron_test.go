package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

func testLogger() *zap.SugaredLogger { return zap.NewNop().Sugar() }

// ──────────────────────────────────────────────────────────────────────
// intervalFromSpec
// ──────────────────────────────────────────────────────────────────────

func TestIntervalFromSpec_Hourly(t *testing.T) {
	d, err := intervalFromSpec("@hourly")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != time.Hour {
		t.Errorf("expected 1h, got %v", d)
	}
}

func TestIntervalFromSpec_Daily(t *testing.T) {
	d, err := intervalFromSpec("@daily")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != 24*time.Hour {
		t.Errorf("expected 24h, got %v", d)
	}
}

func TestIntervalFromSpec_Weekly(t *testing.T) {
	d, err := intervalFromSpec("@weekly")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != 7*24*time.Hour {
		t.Errorf("expected 168h, got %v", d)
	}
}

func TestIntervalFromSpec_Every1h(t *testing.T) {
	d, err := intervalFromSpec("@every 1h")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != time.Hour {
		t.Errorf("expected 1h, got %v", d)
	}
}

func TestIntervalFromSpec_Every30m(t *testing.T) {
	d, err := intervalFromSpec("@every 30m")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != 30*time.Minute {
		t.Errorf("expected 30m, got %v", d)
	}
}

func TestIntervalFromSpec_Every90s(t *testing.T) {
	d, err := intervalFromSpec("@every 90s")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != 90*time.Second {
		t.Errorf("expected 90s, got %v", d)
	}
}

func TestIntervalFromSpec_HHMM(t *testing.T) {
	d, err := intervalFromSpec("14:30")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != 24*time.Hour {
		t.Errorf("HH:MM should map to daily (24h), got %v", d)
	}
}

func TestIntervalFromSpec_HHMM_Midnight(t *testing.T) {
	d, err := intervalFromSpec("00:00")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != 24*time.Hour {
		t.Errorf("00:00 should map to daily (24h), got %v", d)
	}
}

func TestIntervalFromSpec_HHMM_EndOfDay(t *testing.T) {
	d, err := intervalFromSpec("23:59")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != 24*time.Hour {
		t.Errorf("23:59 should map to daily (24h), got %v", d)
	}
}

func TestIntervalFromSpec_Invalid(t *testing.T) {
	cases := []string{
		"",
		"bogus",
		"@minutely",
		"@every",
		"@every -1h",
		"@every 0s",
		"@every notaduration",
		"25:00",
		"12:60",
		"abc",
		"123",
		"1:30",  // too short — not 5 chars
		"12345", // 5 chars but no colon at [2]
	}
	for _, spec := range cases {
		_, err := intervalFromSpec(spec)
		if err == nil {
			t.Errorf("expected error for spec %q, got nil", spec)
		}
	}
}

func TestIntervalFromSpec_WhitespaceIsTrimmed(t *testing.T) {
	d, err := intervalFromSpec("  @hourly  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != time.Hour {
		t.Errorf("expected 1h, got %v", d)
	}
}

// ──────────────────────────────────────────────────────────────────────
// nextRun
// ──────────────────────────────────────────────────────────────────────

func TestNextRun_Hourly(t *testing.T) {
	now := time.Now()
	next, err := nextRun("@hourly")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !next.After(now) {
		t.Errorf("expected future time, got %v (now=%v)", next, now)
	}
	// Should be the start of the next hour
	expected := now.Truncate(time.Hour).Add(time.Hour)
	if !next.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, next)
	}
}

func TestNextRun_Daily(t *testing.T) {
	now := time.Now()
	next, err := nextRun("@daily")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !next.After(now) {
		t.Errorf("expected future time, got %v (now=%v)", next, now)
	}
	// Should be midnight tomorrow
	expected := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Add(24 * time.Hour)
	if !next.Equal(expected) {
		t.Errorf("expected %v, got %v", expected, next)
	}
}

func TestNextRun_Weekly(t *testing.T) {
	now := time.Now()
	next, err := nextRun("@weekly")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !next.After(now) {
		t.Errorf("expected future time, got %v (now=%v)", next, now)
	}
	if next.Weekday() != time.Sunday {
		t.Errorf("expected Sunday, got %v", next.Weekday())
	}
}

func TestNextRun_Every(t *testing.T) {
	before := time.Now()
	next, err := nextRun("@every 2h30m")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	after := time.Now()
	// next should be ~2h30m from now
	expected := 2*time.Hour + 30*time.Minute
	lo := before.Add(expected)
	hi := after.Add(expected)
	if next.Before(lo) || next.After(hi) {
		t.Errorf("expected ~%v from now, got %v", expected, next)
	}
}

func TestNextRun_HHMM_Future(t *testing.T) {
	now := time.Now()
	// Pick a time 1 hour from now (wraps around if needed)
	futureH := (now.Hour() + 1) % 24
	spec := fmt.Sprintf("%02d:%02d", futureH, now.Minute())
	next, err := nextRun(spec)
	if err != nil {
		t.Fatalf("unexpected error for spec %q: %v", spec, err)
	}
	if !next.After(now) {
		t.Errorf("expected future time for spec %q, got %v (now=%v)", spec, next, now)
	}
}

func TestNextRun_HHMM_Past(t *testing.T) {
	now := time.Now()
	// Pick a time that has already passed today (1 hour ago, wraps around)
	pastH := (now.Hour() + 23) % 24
	spec := fmt.Sprintf("%02d:00", pastH)
	next, err := nextRun(spec)
	if err != nil {
		t.Fatalf("unexpected error for spec %q: %v", spec, err)
	}
	if !next.After(now) {
		t.Errorf("expected future time (tomorrow) for spec %q, got %v (now=%v)", spec, next, now)
	}
}

func TestNextRun_Invalid(t *testing.T) {
	_, err := nextRun("garbage")
	if err == nil {
		t.Error("expected error for invalid spec")
	}
}

func TestNextRun_EveryNegative(t *testing.T) {
	_, err := nextRun("@every -5m")
	if err == nil {
		t.Error("expected error for negative duration")
	}
}

func TestNextRun_EveryZero(t *testing.T) {
	_, err := nextRun("@every 0s")
	if err == nil {
		t.Error("expected error for zero duration")
	}
}

func TestNextRun_EveryBadDuration(t *testing.T) {
	_, err := nextRun("@every notaduration")
	if err == nil {
		t.Error("expected error for bad duration")
	}
}

// ──────────────────────────────────────────────────────────────────────
// Job helpers
// ──────────────────────────────────────────────────────────────────────

func TestEffectiveInstruction_PayloadPriority(t *testing.T) {
	j := &Job{
		Instruction: "simple instruction",
		Payload:     &Payload{Message: "full instruction"},
	}
	if got := j.EffectiveInstruction(); got != "full instruction" {
		t.Errorf("expected payload message, got %q", got)
	}
}

func TestEffectiveInstruction_FallbackToInstruction(t *testing.T) {
	j := &Job{Instruction: "simple instruction"}
	if got := j.EffectiveInstruction(); got != "simple instruction" {
		t.Errorf("expected instruction field, got %q", got)
	}
}

func TestEffectiveInstruction_PayloadEmptyMessage(t *testing.T) {
	j := &Job{
		Instruction: "fallback",
		Payload:     &Payload{Message: ""},
	}
	if got := j.EffectiveInstruction(); got != "fallback" {
		t.Errorf("expected fallback to instruction, got %q", got)
	}
}

func TestEffectiveInstruction_BothEmpty(t *testing.T) {
	j := &Job{}
	if got := j.EffectiveInstruction(); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestEffectiveModel_WithPayload(t *testing.T) {
	j := &Job{Payload: &Payload{Model: "claude-3"}}
	if got := j.EffectiveModel(); got != "claude-3" {
		t.Errorf("expected claude-3, got %q", got)
	}
}

func TestEffectiveModel_NoPayload(t *testing.T) {
	j := &Job{}
	if got := j.EffectiveModel(); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestEffectiveModel_PayloadNoModel(t *testing.T) {
	j := &Job{Payload: &Payload{Message: "hello"}}
	if got := j.EffectiveModel(); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestEffectiveTimeout_WithPayload(t *testing.T) {
	j := &Job{Payload: &Payload{TimeoutSeconds: 60}}
	if got := j.EffectiveTimeout(); got != 60*time.Second {
		t.Errorf("expected 60s, got %v", got)
	}
}

func TestEffectiveTimeout_NoPayload(t *testing.T) {
	j := &Job{}
	if got := j.EffectiveTimeout(); got != 0 {
		t.Errorf("expected 0, got %v", got)
	}
}

func TestEffectiveTimeout_PayloadZero(t *testing.T) {
	j := &Job{Payload: &Payload{TimeoutSeconds: 0}}
	if got := j.EffectiveTimeout(); got != 0 {
		t.Errorf("expected 0, got %v", got)
	}
}

func TestEffectiveTimeout_PayloadNegative(t *testing.T) {
	j := &Job{Payload: &Payload{TimeoutSeconds: -10}}
	if got := j.EffectiveTimeout(); got != 0 {
		t.Errorf("expected 0 for negative timeout, got %v", got)
	}
}

func TestEffectiveSessionKey_Persistent(t *testing.T) {
	j := &Job{ID: "abc123"}
	got := j.EffectiveSessionKey()
	if got != "cron:abc123" {
		t.Errorf("expected cron:abc123, got %q", got)
	}
}

func TestEffectiveSessionKey_CustomKey(t *testing.T) {
	j := &Job{ID: "abc123", SessionKey: "custom-key"}
	got := j.EffectiveSessionKey()
	if got != "custom-key" {
		t.Errorf("expected custom-key, got %q", got)
	}
}

func TestEffectiveSessionKey_Isolated(t *testing.T) {
	j := &Job{ID: "abc123", SessionTarget: "isolated"}
	got := j.EffectiveSessionKey()
	if !strings.HasPrefix(got, "cron:abc123:") {
		t.Errorf("expected cron:abc123:<timestamp>, got %q", got)
	}
	// Should contain a timestamp portion
	parts := strings.SplitN(got, ":", 3)
	if len(parts) != 3 || parts[2] == "" {
		t.Errorf("expected 3 colon-separated parts with timestamp, got %q", got)
	}
}

func TestEffectiveSessionKey_IsolatedOverridesCustomKey(t *testing.T) {
	j := &Job{ID: "abc123", SessionTarget: "isolated", SessionKey: "custom"}
	got := j.EffectiveSessionKey()
	// isolated takes priority
	if !strings.HasPrefix(got, "cron:abc123:") {
		t.Errorf("isolated should override custom key, got %q", got)
	}
}

func TestEffectiveInterval_Schedule(t *testing.T) {
	j := &Job{
		Schedule: &Schedule{Kind: "every", EveryMs: 3600000},
	}
	d, err := j.EffectiveInterval()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != time.Hour {
		t.Errorf("expected 1h, got %v", d)
	}
}

func TestEffectiveInterval_Spec(t *testing.T) {
	j := &Job{Spec: "@daily"}
	d, err := j.EffectiveInterval()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != 24*time.Hour {
		t.Errorf("expected 24h, got %v", d)
	}
}

func TestEffectiveInterval_Neither(t *testing.T) {
	j := &Job{ID: "test"}
	_, err := j.EffectiveInterval()
	if err == nil {
		t.Error("expected error when no schedule defined")
	}
}

func TestEffectiveInterval_ScheduleTakesPriority(t *testing.T) {
	j := &Job{
		Spec:     "@hourly",
		Schedule: &Schedule{Kind: "every", EveryMs: 1800000}, // 30 min
	}
	d, err := j.EffectiveInterval()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != 30*time.Minute {
		t.Errorf("schedule should take priority, expected 30m, got %v", d)
	}
}

func TestEffectiveInterval_ScheduleWrongKind(t *testing.T) {
	j := &Job{
		Spec:     "@hourly",
		Schedule: &Schedule{Kind: "cron", EveryMs: 1800000},
	}
	d, err := j.EffectiveInterval()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Kind is not "every", so should fall back to spec
	if d != time.Hour {
		t.Errorf("expected fallback to spec (1h), got %v", d)
	}
}

func TestDisplaySchedule_FullFormat(t *testing.T) {
	j := &Job{Schedule: &Schedule{Kind: "every", EveryMs: 3600000}}
	got := j.DisplaySchedule()
	if got != "every 1h0m0s" {
		t.Errorf("expected 'every 1h0m0s', got %q", got)
	}
}

func TestDisplaySchedule_Spec(t *testing.T) {
	j := &Job{Spec: "@daily"}
	got := j.DisplaySchedule()
	if got != "@daily" {
		t.Errorf("expected '@daily', got %q", got)
	}
}

func TestDisplaySchedule_Unknown(t *testing.T) {
	j := &Job{}
	got := j.DisplaySchedule()
	if got != "unknown" {
		t.Errorf("expected 'unknown', got %q", got)
	}
}

func TestDisplayName_WithName(t *testing.T) {
	j := &Job{ID: "abc", Name: "My Job"}
	if got := j.DisplayName(); got != "My Job" {
		t.Errorf("expected 'My Job', got %q", got)
	}
}

func TestDisplayName_FallbackToID(t *testing.T) {
	j := &Job{ID: "abc"}
	if got := j.DisplayName(); got != "abc" {
		t.Errorf("expected 'abc', got %q", got)
	}
}

func TestWantsDelivery_True(t *testing.T) {
	j := &Job{Delivery: &Delivery{Mode: "announce"}}
	if !j.WantsDelivery() {
		t.Error("expected WantsDelivery() = true")
	}
}

func TestWantsDelivery_False_NoDelivery(t *testing.T) {
	j := &Job{}
	if j.WantsDelivery() {
		t.Error("expected WantsDelivery() = false with no Delivery")
	}
}

func TestWantsDelivery_False_WrongMode(t *testing.T) {
	j := &Job{Delivery: &Delivery{Mode: "silent"}}
	if j.WantsDelivery() {
		t.Error("expected WantsDelivery() = false with mode 'silent'")
	}
}

func TestWantsDelivery_False_EmptyMode(t *testing.T) {
	j := &Job{Delivery: &Delivery{}}
	if j.WantsDelivery() {
		t.Error("expected WantsDelivery() = false with empty mode")
	}
}

// ──────────────────────────────────────────────────────────────────────
// Manager lifecycle
// ──────────────────────────────────────────────────────────────────────

func noopRunFunc(_ context.Context, _ *Job) RunResult {
	return RunResult{}
}

func TestNew_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)
	if m == nil {
		t.Fatal("expected non-nil manager")
	}
	if len(m.List()) != 0 {
		t.Errorf("expected 0 jobs, got %d", len(m.List()))
	}
}

func TestNew_LoadsExistingCrons(t *testing.T) {
	dir := t.TempDir()
	jobs := []*Job{
		{ID: "j1", Spec: "@daily", Instruction: "do stuff", Enabled: true},
		{ID: "j2", Spec: "@hourly", Instruction: "more stuff", Enabled: false},
	}
	data, _ := json.MarshalIndent(jobs, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "crons.json"), data, 0600)

	m := New(testLogger(), dir, noopRunFunc)
	list := m.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(list))
	}
	if list[0].ID != "j1" || list[1].ID != "j2" {
		t.Errorf("unexpected job IDs: %s, %s", list[0].ID, list[1].ID)
	}
}

func TestNew_InvalidCronsJson(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "crons.json"), []byte("not json"), 0600)
	m := New(testLogger(), dir, noopRunFunc)
	// Should start fresh when json is invalid
	if len(m.List()) != 0 {
		t.Errorf("expected 0 jobs after invalid json, got %d", len(m.List()))
	}
}

func TestAdd_Success(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)

	j, err := m.Add("@hourly", "test instruction")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if j.ID == "" {
		t.Error("expected non-empty ID")
	}
	if j.Spec != "@hourly" {
		t.Errorf("expected @hourly, got %q", j.Spec)
	}
	if j.Instruction != "test instruction" {
		t.Errorf("expected 'test instruction', got %q", j.Instruction)
	}
	if !j.Enabled {
		t.Error("expected job to be enabled")
	}

	list := m.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 job, got %d", len(list))
	}
}

func TestAdd_InvalidSpec(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)
	_, err := m.Add("bogus", "instruction")
	if err == nil {
		t.Error("expected error for invalid spec")
	}
	if len(m.List()) != 0 {
		t.Errorf("expected 0 jobs after failed add, got %d", len(m.List()))
	}
}

func TestAdd_PersistsToDisk(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)
	_, _ = m.Add("@daily", "persistent instruction")

	// Read back from disk
	data, err := os.ReadFile(filepath.Join(dir, "crons.json"))
	if err != nil {
		t.Fatalf("failed to read crons.json: %v", err)
	}
	var loaded []*Job
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("failed to parse crons.json: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("expected 1 job on disk, got %d", len(loaded))
	}
	if loaded[0].Instruction != "persistent instruction" {
		t.Errorf("expected 'persistent instruction', got %q", loaded[0].Instruction)
	}
}

func TestAdd_MultipleJobs(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)
	_, _ = m.Add("@hourly", "first")
	_, _ = m.Add("@daily", "second")
	_, _ = m.Add("@weekly", "third")

	list := m.List()
	if len(list) != 3 {
		t.Fatalf("expected 3 jobs, got %d", len(list))
	}
}

func TestRemove_Success(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)
	j, _ := m.Add("@hourly", "to remove")

	err := m.Remove(j.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(m.List()) != 0 {
		t.Errorf("expected 0 jobs after remove, got %d", len(m.List()))
	}
}

func TestRemove_NonExistent(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)
	err := m.Remove("nonexistent")
	if err == nil {
		t.Error("expected error for non-existent job")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got %q", err.Error())
	}
}

func TestRemove_PersistsToDisk(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)
	j, _ := m.Add("@hourly", "temp")
	_ = m.Remove(j.ID)

	// Verify crons.json on disk has no jobs
	data, _ := os.ReadFile(filepath.Join(dir, "crons.json"))
	var loaded []*Job
	_ = json.Unmarshal(data, &loaded)
	if len(loaded) != 0 {
		t.Errorf("expected 0 jobs on disk after remove, got %d", len(loaded))
	}
}

func TestRemove_MiddleJob(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)
	_, _ = m.Add("@hourly", "first")
	j2, _ := m.Add("@daily", "second")
	_, _ = m.Add("@weekly", "third")

	_ = m.Remove(j2.ID)
	list := m.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(list))
	}
	if list[0].Instruction != "first" || list[1].Instruction != "third" {
		t.Errorf("unexpected remaining jobs: %q, %q", list[0].Instruction, list[1].Instruction)
	}
}

func TestSetEnabled_Disable(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)
	j, _ := m.Add("@hourly", "toggle me")

	err := m.SetEnabled(j.ID, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	list := m.List()
	if list[0].Enabled {
		t.Error("expected job to be disabled")
	}
}

func TestSetEnabled_Enable(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)
	j, _ := m.Add("@hourly", "toggle me")
	_ = m.SetEnabled(j.ID, false)
	err := m.SetEnabled(j.ID, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	list := m.List()
	if !list[0].Enabled {
		t.Error("expected job to be re-enabled")
	}
}

func TestSetEnabled_NonExistent(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)
	err := m.SetEnabled("nonexistent", true)
	if err == nil {
		t.Error("expected error for non-existent job")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got %q", err.Error())
	}
}

func TestList_ReturnsDeepCopy(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)
	j, _ := m.Add("@hourly", "original")

	list := m.List()
	list[0].Instruction = "mutated"

	// The original should not be affected
	list2 := m.List()
	if list2[0].Instruction != "original" {
		t.Errorf("List() should return deep copy; got %q", list2[0].Instruction)
	}
	_ = j
}

func TestList_CopiesSubStructs(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)

	// Manually inject a full-format job
	m.mu.Lock()
	m.jobs = append(m.jobs, &Job{
		ID:       "full1",
		Enabled:  true,
		Schedule: &Schedule{Kind: "every", EveryMs: 60000},
		Payload:  &Payload{Kind: "agentTurn", Message: "hello"},
		Delivery: &Delivery{Mode: "announce"},
		State:    &JobState{LastRunStatus: "ok"},
	})
	m.mu.Unlock()

	list := m.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 job, got %d", len(list))
	}

	// Mutate the copy's sub-structs
	list[0].Schedule.EveryMs = 999
	list[0].Payload.Message = "mutated"
	list[0].Delivery.Mode = "mutated"
	list[0].State.LastRunStatus = "mutated"

	// Originals should be unchanged
	list2 := m.List()
	if list2[0].Schedule.EveryMs != 60000 {
		t.Error("Schedule should be a deep copy")
	}
	if list2[0].Payload.Message != "hello" {
		t.Error("Payload should be a deep copy")
	}
	if list2[0].Delivery.Mode != "announce" {
		t.Error("Delivery should be a deep copy")
	}
	if list2[0].State.LastRunStatus != "ok" {
		t.Error("State should be a deep copy")
	}
}

// ──────────────────────────────────────────────────────────────────────
// LoadJobsFile
// ──────────────────────────────────────────────────────────────────────

func TestLoadJobsFile_Success(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)

	jobsPath := filepath.Join(dir, "jobs.json")
	file := struct {
		Version int    `json:"version"`
		Jobs    []*Job `json:"jobs"`
	}{
		Version: 1,
		Jobs: []*Job{
			{
				ID:       "full1",
				Name:     "Full Job",
				Enabled:  true,
				Schedule: &Schedule{Kind: "every", EveryMs: 3600000},
				Payload:  &Payload{Kind: "agentTurn", Message: "do full stuff"},
			},
		},
	}
	data, _ := json.MarshalIndent(file, "", "  ")
	_ = os.WriteFile(jobsPath, data, 0600)

	err := m.LoadJobsFile(jobsPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	list := m.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 job, got %d", len(list))
	}
	if list[0].ID != "full1" {
		t.Errorf("expected ID full1, got %q", list[0].ID)
	}
	if list[0].Name != "Full Job" {
		t.Errorf("expected name 'Full Job', got %q", list[0].Name)
	}
}

func TestLoadJobsFile_NonExistent(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)
	err := m.LoadJobsFile(filepath.Join(dir, "nonexistent.json"))
	if err != nil {
		t.Errorf("expected no error for non-existent file, got %v", err)
	}
}

func TestLoadJobsFile_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)
	jobsPath := filepath.Join(dir, "jobs.json")
	_ = os.WriteFile(jobsPath, []byte("not json"), 0600)

	err := m.LoadJobsFile(jobsPath)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestLoadJobsFile_Deduplication(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)

	// Add a simple job first
	m.mu.Lock()
	m.jobs = append(m.jobs, &Job{ID: "shared-id", Spec: "@daily", Instruction: "simple"})
	m.mu.Unlock()

	// Create jobs.json with a job that has the same ID
	jobsPath := filepath.Join(dir, "jobs.json")
	file := struct {
		Version int    `json:"version"`
		Jobs    []*Job `json:"jobs"`
	}{
		Version: 1,
		Jobs: []*Job{
			{ID: "shared-id", Schedule: &Schedule{Kind: "every", EveryMs: 60000}},
			{ID: "unique-id", Schedule: &Schedule{Kind: "every", EveryMs: 120000}},
		},
	}
	data, _ := json.MarshalIndent(file, "", "  ")
	_ = os.WriteFile(jobsPath, data, 0600)

	err := m.LoadJobsFile(jobsPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	list := m.List()
	if len(list) != 2 {
		t.Fatalf("expected 2 jobs (deduplicated), got %d", len(list))
	}

	// The first one should be the simple format (pre-existing)
	if list[0].ID != "shared-id" || list[0].Spec != "@daily" {
		t.Errorf("first job should be original simple job, got ID=%q Spec=%q", list[0].ID, list[0].Spec)
	}
	// The second one should be the unique full-format job
	if list[1].ID != "unique-id" {
		t.Errorf("second job should be unique-id, got %q", list[1].ID)
	}
}

func TestLoadJobsFile_MultipleJobs(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)

	jobsPath := filepath.Join(dir, "jobs.json")
	file := struct {
		Version int    `json:"version"`
		Jobs    []*Job `json:"jobs"`
	}{
		Version: 1,
		Jobs: []*Job{
			{ID: "a", Enabled: true, Schedule: &Schedule{Kind: "every", EveryMs: 60000}},
			{ID: "b", Enabled: false, Schedule: &Schedule{Kind: "every", EveryMs: 120000}},
			{ID: "c", Enabled: true, Schedule: &Schedule{Kind: "every", EveryMs: 300000}},
		},
	}
	data, _ := json.MarshalIndent(file, "", "  ")
	_ = os.WriteFile(jobsPath, data, 0600)

	err := m.LoadJobsFile(jobsPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	list := m.List()
	if len(list) != 3 {
		t.Fatalf("expected 3 jobs, got %d", len(list))
	}
}

// ──────────────────────────────────────────────────────────────────────
// Persistence round-trip
// ──────────────────────────────────────────────────────────────────────

func TestPersistence_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	m1 := New(testLogger(), dir, noopRunFunc)
	_, _ = m1.Add("@hourly", "hourly task")
	_, _ = m1.Add("@daily", "daily task")
	_, _ = m1.Add("09:30", "morning check")

	// Create a new manager from the same directory; it should load saved jobs
	m2 := New(testLogger(), dir, noopRunFunc)
	list := m2.List()
	if len(list) != 3 {
		t.Fatalf("expected 3 jobs after round-trip, got %d", len(list))
	}

	specs := map[string]string{}
	for _, j := range list {
		specs[j.Spec] = j.Instruction
	}
	if specs["@hourly"] != "hourly task" {
		t.Error("missing or wrong @hourly job")
	}
	if specs["@daily"] != "daily task" {
		t.Error("missing or wrong @daily job")
	}
	if specs["09:30"] != "morning check" {
		t.Error("missing or wrong 09:30 job")
	}
}

func TestPersistence_RemoveIsPersistedOnRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m1 := New(testLogger(), dir, noopRunFunc)
	j1, _ := m1.Add("@hourly", "first")
	_, _ = m1.Add("@daily", "second")
	_ = m1.Remove(j1.ID)

	m2 := New(testLogger(), dir, noopRunFunc)
	list := m2.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 job after round-trip, got %d", len(list))
	}
	if list[0].Instruction != "second" {
		t.Errorf("expected 'second', got %q", list[0].Instruction)
	}
}

func TestPersistence_EnabledStateIsPreserved(t *testing.T) {
	dir := t.TempDir()
	m1 := New(testLogger(), dir, noopRunFunc)
	j, _ := m1.Add("@hourly", "toggle me")
	_ = m1.SetEnabled(j.ID, false)

	m2 := New(testLogger(), dir, noopRunFunc)
	list := m2.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 job, got %d", len(list))
	}
	if list[0].Enabled {
		t.Error("expected job to remain disabled after round-trip")
	}
}

func TestPersistence_FullFormatJobsSavedToJobsFile(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)

	jobsPath := filepath.Join(dir, "jobs.json")

	// Load a full-format job
	file := struct {
		Version int    `json:"version"`
		Jobs    []*Job `json:"jobs"`
	}{
		Version: 1,
		Jobs: []*Job{
			{
				ID:       "full1",
				Enabled:  true,
				Schedule: &Schedule{Kind: "every", EveryMs: 60000},
				Payload:  &Payload{Kind: "agentTurn", Message: "hello"},
			},
		},
	}
	data, _ := json.MarshalIndent(file, "", "  ")
	_ = os.WriteFile(jobsPath, data, 0600)

	_ = m.LoadJobsFile(jobsPath)

	// Also add a simple job
	_, _ = m.Add("@hourly", "simple job")

	// The simple job should be in crons.json, not in jobs.json
	cronsData, _ := os.ReadFile(filepath.Join(dir, "crons.json"))
	var simpleJobs []*Job
	_ = json.Unmarshal(cronsData, &simpleJobs)

	hasSimple := false
	for _, j := range simpleJobs {
		if j.Instruction == "simple job" {
			hasSimple = true
		}
		if j.ID == "full1" {
			t.Error("full-format job should not appear in crons.json")
		}
	}
	if !hasSimple {
		t.Error("simple job not found in crons.json")
	}
}

// ──────────────────────────────────────────────────────────────────────
// Start, scheduling, and runJob
// ──────────────────────────────────────────────────────────────────────

func TestStart_CancelStops(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		_ = m.Start(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatal("Start() did not return after cancel")
	}
}

func TestStart_SchedulesEnabledJobs(t *testing.T) {
	dir := t.TempDir()
	waitForRunLog(t)
	var mu sync.Mutex
	ran := make(map[string]bool)

	runFunc := func(_ context.Context, j *Job) RunResult {
		mu.Lock()
		ran[j.ID] = true
		mu.Unlock()
		return RunResult{Text: "done"}
	}

	m := New(testLogger(), dir, runFunc)

	// Add a job with a very short interval so it fires quickly
	m.mu.Lock()
	m.jobs = append(m.jobs, &Job{
		ID:      "fast",
		Spec:    "@every 50ms",
		Enabled: true,
	})
	m.mu.Unlock()

	ctx := t.Context()

	go func() {
		_ = m.Start(ctx)
	}()

	// Wait for the job to fire
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		fired := ran["fast"]
		mu.Unlock()
		if fired {
			break
		}
		select {
		case <-deadline:
			t.Fatal("job did not fire within timeout")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestStart_DisabledJobsNotScheduled(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	ran := false

	runFunc := func(_ context.Context, _ *Job) RunResult {
		mu.Lock()
		ran = true
		mu.Unlock()
		return RunResult{}
	}

	m := New(testLogger(), dir, runFunc)
	m.mu.Lock()
	m.jobs = append(m.jobs, &Job{
		ID:      "disabled",
		Spec:    "@every 50ms",
		Enabled: false,
	})
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = m.Start(ctx) }()

	time.Sleep(200 * time.Millisecond)
	cancel()

	mu.Lock()
	if ran {
		t.Error("disabled job should not have fired")
	}
	mu.Unlock()
}

// waitForRunLog gives the async run-log goroutine time to finish writing
// before TempDir cleanup. Register via t.Cleanup in tests that call runJob.
func waitForRunLog(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })
}

func TestRunJob_UpdatesState_Success(t *testing.T) {
	dir := t.TempDir()
	waitForRunLog(t)
	runFunc := func(_ context.Context, _ *Job) RunResult {
		return RunResult{Text: "all good"}
	}

	m := New(testLogger(), dir, runFunc)
	j := &Job{ID: "statetest", Spec: "@daily", Enabled: true}
	m.mu.Lock()
	m.jobs = append(m.jobs, j)
	m.mu.Unlock()

	m.runJob(context.Background(), j)

	m.mu.Lock()
	defer m.mu.Unlock()
	if j.State == nil {
		t.Fatal("expected State to be set")
	}
	if j.State.LastRunStatus != "ok" {
		t.Errorf("expected LastRunStatus 'ok', got %q", j.State.LastRunStatus)
	}
	if j.State.LastStatus != "ok" {
		t.Errorf("expected LastStatus 'ok', got %q", j.State.LastStatus)
	}
	if j.State.ConsecutiveErrors != 0 {
		t.Errorf("expected 0 consecutive errors, got %d", j.State.ConsecutiveErrors)
	}
	if j.State.LastRunAtMs == 0 {
		t.Error("expected LastRunAtMs to be set")
	}
	if j.State.LastDurationMs < 0 {
		t.Errorf("expected non-negative duration, got %d", j.State.LastDurationMs)
	}
}

func TestRunJob_UpdatesState_Error(t *testing.T) {
	dir := t.TempDir()
	waitForRunLog(t)
	runFunc := func(_ context.Context, _ *Job) RunResult {
		return RunResult{Err: fmt.Errorf("something broke")}
	}

	m := New(testLogger(), dir, runFunc)
	j := &Job{ID: "errtest", Spec: "@daily", Enabled: true}
	m.mu.Lock()
	m.jobs = append(m.jobs, j)
	m.mu.Unlock()

	m.runJob(context.Background(), j)

	m.mu.Lock()
	defer m.mu.Unlock()
	if j.State == nil {
		t.Fatal("expected State to be set")
	}
	if j.State.LastRunStatus != "error" {
		t.Errorf("expected LastRunStatus 'error', got %q", j.State.LastRunStatus)
	}
	if j.State.ConsecutiveErrors != 1 {
		t.Errorf("expected 1 consecutive error, got %d", j.State.ConsecutiveErrors)
	}
}

func TestRunJob_ConsecutiveErrors(t *testing.T) {
	dir := t.TempDir()
	waitForRunLog(t)
	runFunc := func(_ context.Context, _ *Job) RunResult {
		return RunResult{Err: fmt.Errorf("fail")}
	}

	m := New(testLogger(), dir, runFunc)
	j := &Job{ID: "multi-err", Spec: "@daily", Enabled: true}
	m.mu.Lock()
	m.jobs = append(m.jobs, j)
	m.mu.Unlock()

	m.runJob(context.Background(), j)
	m.runJob(context.Background(), j)
	m.runJob(context.Background(), j)

	m.mu.Lock()
	defer m.mu.Unlock()
	if j.State.ConsecutiveErrors != 3 {
		t.Errorf("expected 3 consecutive errors, got %d", j.State.ConsecutiveErrors)
	}
}

func TestRunJob_ConsecutiveErrorsResetOnSuccess(t *testing.T) {
	dir := t.TempDir()
	waitForRunLog(t)
	callCount := 0
	runFunc := func(_ context.Context, _ *Job) RunResult {
		callCount++
		if callCount <= 2 {
			return RunResult{Err: fmt.Errorf("fail")}
		}
		return RunResult{Text: "ok"}
	}

	m := New(testLogger(), dir, runFunc)
	j := &Job{ID: "reset-err", Spec: "@daily", Enabled: true}
	m.mu.Lock()
	m.jobs = append(m.jobs, j)
	m.mu.Unlock()

	m.runJob(context.Background(), j)
	m.runJob(context.Background(), j)
	m.runJob(context.Background(), j) // success

	m.mu.Lock()
	defer m.mu.Unlock()
	if j.State.ConsecutiveErrors != 0 {
		t.Errorf("expected 0 consecutive errors after success, got %d", j.State.ConsecutiveErrors)
	}
	if j.State.LastRunStatus != "ok" {
		t.Errorf("expected 'ok', got %q", j.State.LastRunStatus)
	}
}

func TestRunJob_NilRunFunc(t *testing.T) {
	dir := t.TempDir()
	waitForRunLog(t)
	m := New(testLogger(), dir, nil)
	j := &Job{ID: "nilrun", Spec: "@daily", Enabled: true}
	m.mu.Lock()
	m.jobs = append(m.jobs, j)
	m.mu.Unlock()

	// Should not panic
	m.runJob(context.Background(), j)
}

// ──────────────────────────────────────────────────────────────────────
// Delivery
// ──────────────────────────────────────────────────────────────────────

type mockDeliverer struct {
	mu       sync.Mutex
	messages []string
}

func (d *mockDeliverer) SendToAllPaired(text string) {
	d.mu.Lock()
	d.messages = append(d.messages, text)
	d.mu.Unlock()
}

func TestRunJob_Delivery(t *testing.T) {
	dir := t.TempDir()
	waitForRunLog(t)
	runFunc := func(_ context.Context, _ *Job) RunResult {
		return RunResult{Text: "delivered content"}
	}

	m := New(testLogger(), dir, runFunc)
	md := &mockDeliverer{}
	m.AddDeliverer(md)

	j := &Job{
		ID:       "deliver",
		Spec:     "@daily",
		Enabled:  true,
		Delivery: &Delivery{Mode: "announce"},
	}
	m.mu.Lock()
	m.jobs = append(m.jobs, j)
	m.mu.Unlock()

	m.runJob(context.Background(), j)

	md.mu.Lock()
	defer md.mu.Unlock()
	if len(md.messages) != 1 {
		t.Fatalf("expected 1 delivered message, got %d", len(md.messages))
	}
	if md.messages[0] != "delivered content" {
		t.Errorf("expected 'delivered content', got %q", md.messages[0])
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if !j.State.LastDelivered {
		t.Error("expected LastDelivered = true")
	}
	if j.State.LastDeliveryStatus != "delivered" {
		t.Errorf("expected LastDeliveryStatus 'delivered', got %q", j.State.LastDeliveryStatus)
	}
}

func TestRunJob_NoDeliveryOnError(t *testing.T) {
	dir := t.TempDir()
	waitForRunLog(t)
	runFunc := func(_ context.Context, _ *Job) RunResult {
		return RunResult{Err: fmt.Errorf("fail"), Text: "should not deliver"}
	}

	m := New(testLogger(), dir, runFunc)
	md := &mockDeliverer{}
	m.AddDeliverer(md)

	j := &Job{
		ID:       "no-deliver-err",
		Spec:     "@daily",
		Enabled:  true,
		Delivery: &Delivery{Mode: "announce"},
	}
	m.mu.Lock()
	m.jobs = append(m.jobs, j)
	m.mu.Unlock()

	m.runJob(context.Background(), j)

	md.mu.Lock()
	defer md.mu.Unlock()
	if len(md.messages) != 0 {
		t.Errorf("expected no delivery on error, got %d messages", len(md.messages))
	}
}

func TestRunJob_NoDeliveryWithoutAnnounceMode(t *testing.T) {
	dir := t.TempDir()
	waitForRunLog(t)
	runFunc := func(_ context.Context, _ *Job) RunResult {
		return RunResult{Text: "result"}
	}

	m := New(testLogger(), dir, runFunc)
	md := &mockDeliverer{}
	m.AddDeliverer(md)

	j := &Job{
		ID:      "no-announce",
		Spec:    "@daily",
		Enabled: true,
		// No Delivery field
	}
	m.mu.Lock()
	m.jobs = append(m.jobs, j)
	m.mu.Unlock()

	m.runJob(context.Background(), j)

	md.mu.Lock()
	defer md.mu.Unlock()
	if len(md.messages) != 0 {
		t.Errorf("expected no delivery without announce mode, got %d messages", len(md.messages))
	}
}

func TestRunJob_NoDeliveryEmptyText(t *testing.T) {
	dir := t.TempDir()
	waitForRunLog(t)
	runFunc := func(_ context.Context, _ *Job) RunResult {
		return RunResult{Text: ""}
	}

	m := New(testLogger(), dir, runFunc)
	md := &mockDeliverer{}
	m.AddDeliverer(md)

	j := &Job{
		ID:       "empty-text",
		Spec:     "@daily",
		Enabled:  true,
		Delivery: &Delivery{Mode: "announce"},
	}
	m.mu.Lock()
	m.jobs = append(m.jobs, j)
	m.mu.Unlock()

	m.runJob(context.Background(), j)

	md.mu.Lock()
	defer md.mu.Unlock()
	if len(md.messages) != 0 {
		t.Errorf("expected no delivery for empty text, got %d messages", len(md.messages))
	}
}

func TestRunJob_NoDeliverers(t *testing.T) {
	dir := t.TempDir()
	waitForRunLog(t)
	runFunc := func(_ context.Context, _ *Job) RunResult {
		return RunResult{Text: "result"}
	}

	m := New(testLogger(), dir, runFunc)
	// No deliverers added

	j := &Job{
		ID:       "no-deliverers",
		Spec:     "@daily",
		Enabled:  true,
		Delivery: &Delivery{Mode: "announce"},
	}
	m.mu.Lock()
	m.jobs = append(m.jobs, j)
	m.mu.Unlock()

	m.runJob(context.Background(), j)

	m.mu.Lock()
	defer m.mu.Unlock()
	if j.State.LastDelivered {
		t.Error("expected LastDelivered = false with no deliverers")
	}
	if j.State.LastDeliveryStatus != "no_deliverers" {
		t.Errorf("expected 'no_deliverers', got %q", j.State.LastDeliveryStatus)
	}
}

func TestRunJob_DeliverySuppressed(t *testing.T) {
	dir := t.TempDir()
	waitForRunLog(t)
	runFunc := func(_ context.Context, _ *Job) RunResult {
		return RunResult{Text: "should not deliver"}
	}

	m := New(testLogger(), dir, runFunc)
	md := &mockDeliverer{}
	m.AddDeliverer(md)

	j := &Job{
		ID:       "suppressed",
		Spec:     "@daily",
		Enabled:  true,
		Delivery: &Delivery{Mode: "none"},
	}
	m.mu.Lock()
	m.jobs = append(m.jobs, j)
	m.mu.Unlock()

	m.runJob(context.Background(), j)

	md.mu.Lock()
	if len(md.messages) != 0 {
		t.Errorf("expected no delivery with mode 'none', got %d messages", len(md.messages))
	}
	md.mu.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if j.State.LastDelivered {
		t.Error("expected LastDelivered = false with suppressed delivery")
	}
	if j.State.LastDeliveryStatus != "suppressed" {
		t.Errorf("expected 'suppressed', got %q", j.State.LastDeliveryStatus)
	}
}

func TestDir(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)
	if m.Dir() != dir {
		t.Errorf("expected %q, got %q", dir, m.Dir())
	}
}

func TestRunLogDir(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)
	expected := filepath.Join(dir, "runs")
	if m.RunLogDir() != expected {
		t.Errorf("expected %q, got %q", expected, m.RunLogDir())
	}
}

// ──────────────────────────────────────────────────────────────────────
// RunNow
// ──────────────────────────────────────────────────────────────────────

func TestRunNow_Success(t *testing.T) {
	dir := t.TempDir()
	waitForRunLog(t)
	var mu sync.Mutex
	ran := false
	runFunc := func(_ context.Context, _ *Job) RunResult {
		mu.Lock()
		ran = true
		mu.Unlock()
		return RunResult{Text: "manual run"}
	}

	m := New(testLogger(), dir, runFunc)
	j := &Job{ID: "manual", Spec: "@daily", Enabled: true}
	m.mu.Lock()
	m.jobs = append(m.jobs, j)
	m.mu.Unlock()

	err := m.RunNow(context.Background(), "manual")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Wait for the goroutine to fire
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		fired := ran
		mu.Unlock()
		if fired {
			break
		}
		select {
		case <-deadline:
			t.Fatal("RunNow job did not fire within timeout")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestRunNow_NotFound(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)
	err := m.RunNow(context.Background(), "nonexistent")
	if err == nil {
		t.Error("expected error for non-existent job")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got %q", err.Error())
	}
}

// ──────────────────────────────────────────────────────────────────────
// computeNextWait
// ──────────────────────────────────────────────────────────────────────

func TestComputeNextWait_FullFormat_FutureAnchor(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)

	future := time.Now().Add(5 * time.Hour)
	j := &Job{
		ID: "future-anchor",
		Schedule: &Schedule{
			Kind:     "every",
			EveryMs:  3600000, // 1h
			AnchorMs: future.UnixMilli(),
		},
	}

	wait := m.computeNextWait(j)
	// Should wait until the anchor time
	if wait < 4*time.Hour || wait > 6*time.Hour {
		t.Errorf("expected ~5h wait for future anchor, got %v", wait)
	}
}

func TestComputeNextWait_FullFormat_PastAnchor(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)

	past := time.Now().Add(-3*time.Hour - 30*time.Minute)
	j := &Job{
		ID: "past-anchor",
		Schedule: &Schedule{
			Kind:     "every",
			EveryMs:  3600000, // 1h
			AnchorMs: past.UnixMilli(),
		},
		State: &JobState{LastRunAtMs: time.Now().Add(-10 * time.Minute).UnixMilli()},
	}

	wait := m.computeNextWait(j)
	// 3.5h past with 1h interval = 3 full periods, next at anchor+4h = ~30 min from now
	if wait < 20*time.Minute || wait > 40*time.Minute {
		t.Errorf("expected ~30min wait, got %v", wait)
	}
}

func TestComputeNextWait_WakeMode_Now_NeverRun(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)

	past := time.Now().Add(-2 * time.Hour)
	j := &Job{
		ID:       "wake-never-run",
		WakeMode: "now",
		Schedule: &Schedule{
			Kind:     "every",
			EveryMs:  3600000,
			AnchorMs: past.UnixMilli(),
		},
		// No State — never run
	}

	wait := m.computeNextWait(j)
	if wait != 0 {
		t.Errorf("expected 0 (fire now) for never-run wake:now job, got %v", wait)
	}
}

func TestComputeNextWait_WakeMode_Now_MissedRun(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)

	past := time.Now().Add(-5 * time.Hour)
	j := &Job{
		ID:       "wake-missed",
		WakeMode: "now",
		Schedule: &Schedule{
			Kind:     "every",
			EveryMs:  3600000, // 1h
			AnchorMs: past.UnixMilli(),
		},
		State: &JobState{
			LastRunAtMs: past.Add(1 * time.Hour).UnixMilli(), // ran once, 4h ago
		},
	}

	wait := m.computeNextWait(j)
	if wait != 0 {
		t.Errorf("expected 0 (fire now) for missed run, got %v", wait)
	}
}

func TestComputeNextWait_SimpleSpec(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)

	j := &Job{ID: "simple", Spec: "@hourly"}
	wait := m.computeNextWait(j)
	// Should be 0 < wait <= 1 hour
	if wait <= 0 || wait > time.Hour {
		t.Errorf("expected 0 < wait <= 1h, got %v", wait)
	}
}

func TestComputeNextWait_InvalidSpec_Fallback(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)

	j := &Job{ID: "bad", Spec: "garbage"}
	wait := m.computeNextWait(j)
	if wait != time.Hour {
		t.Errorf("expected 1h fallback, got %v", wait)
	}
}

func TestComputeNextWait_NoSchedule_Fallback(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)

	j := &Job{ID: "nothing"}
	wait := m.computeNextWait(j)
	if wait != time.Hour {
		t.Errorf("expected 1h fallback, got %v", wait)
	}
}

// ──────────────────────────────────────────────────────────────────────
// AddDeliverer
// ──────────────────────────────────────────────────────────────────────

func TestAddDeliverer(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, noopRunFunc)

	d1 := &mockDeliverer{}
	d2 := &mockDeliverer{}
	m.AddDeliverer(d1)
	m.AddDeliverer(d2)

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.deliverers) != 2 {
		t.Errorf("expected 2 deliverers, got %d", len(m.deliverers))
	}
}

// ──────────────────────────────────────────────────────────────────────
// Add with Start (schedules immediately)
// ──────────────────────────────────────────────────────────────────────

func TestAdd_SchedulesImmediatelyWhenStarted(t *testing.T) {
	dir := t.TempDir()
	waitForRunLog(t)
	var mu sync.Mutex
	ran := false
	runFunc := func(_ context.Context, _ *Job) RunResult {
		mu.Lock()
		ran = true
		mu.Unlock()
		return RunResult{}
	}

	m := New(testLogger(), dir, runFunc)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})
	go func() { _ = m.Start(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	// Give Start a moment to set ctx
	time.Sleep(50 * time.Millisecond)

	// Add a very fast job
	_, err := m.Add("@every 50ms", "fast job")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		fired := ran
		mu.Unlock()
		if fired {
			break
		}
		select {
		case <-deadline:
			t.Fatal("job added after Start() did not fire within timeout")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────
// SetEnabled re-enables with Start running
// ──────────────────────────────────────────────────────────────────────

func TestSetEnabled_ReenableSchedulesWhenStarted(t *testing.T) {
	dir := t.TempDir()
	waitForRunLog(t)
	var mu sync.Mutex
	ran := false
	runFunc := func(_ context.Context, _ *Job) RunResult {
		mu.Lock()
		ran = true
		mu.Unlock()
		return RunResult{}
	}

	m := New(testLogger(), dir, runFunc)

	// Pre-add a disabled job
	m.mu.Lock()
	m.jobs = append(m.jobs, &Job{
		ID:      "toggle",
		Spec:    "@every 50ms",
		Enabled: false,
	})
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})
	go func() { _ = m.Start(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	time.Sleep(50 * time.Millisecond)

	// Should not have fired yet
	mu.Lock()
	if ran {
		mu.Unlock()
		t.Fatal("disabled job should not have fired")
	}
	mu.Unlock()

	// Re-enable
	_ = m.SetEnabled("toggle", true)

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		fired := ran
		mu.Unlock()
		if fired {
			break
		}
		select {
		case <-deadline:
			t.Fatal("re-enabled job did not fire within timeout")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────
// newID
// ──────────────────────────────────────────────────────────────────────

func TestNewID_UniqueAndCorrectLength(t *testing.T) {
	ids := make(map[string]bool)
	for range 100 {
		id := newID()
		if len(id) != 12 { // 6 bytes = 12 hex chars
			t.Errorf("expected 12 char ID, got %d chars: %q", len(id), id)
		}
		if ids[id] {
			t.Errorf("duplicate ID generated: %q", id)
		}
		ids[id] = true
	}
}

// ──────────────────────────────────────────────────────────────────────
// Edge cases
// ──────────────────────────────────────────────────────────────────────

func TestRemove_StopsTimer(t *testing.T) {
	dir := t.TempDir()
	waitForRunLog(t)
	var mu sync.Mutex
	runCount := 0
	runFunc := func(_ context.Context, _ *Job) RunResult {
		mu.Lock()
		runCount++
		mu.Unlock()
		return RunResult{}
	}

	m := New(testLogger(), dir, runFunc)

	ctx := t.Context()

	go func() { _ = m.Start(ctx) }()
	time.Sleep(100 * time.Millisecond)

	j, _ := m.Add("@every 100ms", "to-remove")

	// Let it fire at least once
	time.Sleep(250 * time.Millisecond)

	mu.Lock()
	countBefore := runCount
	mu.Unlock()

	_ = m.Remove(j.ID)

	// Wait and verify it stopped firing
	time.Sleep(350 * time.Millisecond)

	mu.Lock()
	countAfter := runCount
	mu.Unlock()

	// Allow at most 2 extra fires (race between Remove and timer, especially under -race)
	if countAfter > countBefore+2 {
		t.Errorf("job continued firing after Remove: before=%d after=%d", countBefore, countAfter)
	}
}

func TestSetEnabled_Disable_StopsTimer(t *testing.T) {
	dir := t.TempDir()
	waitForRunLog(t)
	var mu sync.Mutex
	runCount := 0
	runFunc := func(_ context.Context, _ *Job) RunResult {
		mu.Lock()
		runCount++
		mu.Unlock()
		return RunResult{}
	}

	m := New(testLogger(), dir, runFunc)

	ctx := t.Context()

	go func() { _ = m.Start(ctx) }()
	time.Sleep(50 * time.Millisecond)

	j, _ := m.Add("@every 50ms", "to-disable")

	// Let it fire at least once
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	countBefore := runCount
	mu.Unlock()

	_ = m.SetEnabled(j.ID, false)

	// Wait and verify it stopped firing
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	countAfter := runCount
	mu.Unlock()

	// Allow at most 1 extra fire (race between disable and timer)
	if countAfter > countBefore+1 {
		t.Errorf("job continued firing after disable: before=%d after=%d", countBefore, countAfter)
	}
}

// ──────────────────────────────────────────────────────────────────────
// isSuppressible
// ──────────────────────────────────────────────────────────────────────

func TestIsSuppressible(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"", true},
		{"   ", true},
		{"NO_REPLY", true},
		{"no_reply", true},
		{" NO_REPLY ", true},
		{"...", true},
		{" ... ", true},
		{"\u2026", true},
		{"Hello world", false},
		{"NO_REPLY with extra", false},
	}
	for _, tt := range tests {
		if got := isSuppressible(tt.input); got != tt.want {
			t.Errorf("isSuppressible(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────
// Delivery suppression integration
// ──────────────────────────────────────────────────────────────────────

func TestDelivery_SuppressesNoReply(t *testing.T) {
	dir := t.TempDir()
	md := &mockDeliverer{}
	m := New(testLogger(), dir, func(ctx context.Context, job *Job) RunResult {
		return RunResult{Text: "NO_REPLY"}
	})

	m.AddDeliverer(md)

	j := &Job{
		ID:       "suppress-test",
		Enabled:  true,
		Spec:     "@every 1h",
		Delivery: &Delivery{Mode: "announce"},
	}
	m.mu.Lock()
	m.jobs = append(m.jobs, j)
	m.mu.Unlock()

	m.runJob(context.Background(), j)

	md.mu.Lock()
	count := len(md.messages)
	md.mu.Unlock()

	if count != 0 {
		t.Errorf("expected NO_REPLY to be suppressed, but got %d deliveries", count)
	}
}

func TestDelivery_SuppressesEmpty(t *testing.T) {
	dir := t.TempDir()
	md := &mockDeliverer{}
	m := New(testLogger(), dir, func(ctx context.Context, job *Job) RunResult {
		return RunResult{Text: "   "}
	})

	m.AddDeliverer(md)

	j := &Job{
		ID:       "suppress-empty",
		Enabled:  true,
		Spec:     "@every 1h",
		Delivery: &Delivery{Mode: "announce"},
	}
	m.mu.Lock()
	m.jobs = append(m.jobs, j)
	m.mu.Unlock()

	m.runJob(context.Background(), j)

	md.mu.Lock()
	count := len(md.messages)
	md.mu.Unlock()

	if count != 0 {
		t.Errorf("expected whitespace-only to be suppressed, but got %d deliveries", count)
	}
}

// ──────────────────────────────────────────────────────────────────────
// REQ-512: Concurrent guard
// ──────────────────────────────────────────────────────────────────────

func TestConcurrentGuard(t *testing.T) {
	dir := t.TempDir()
	var calls atomic.Int32

	m := New(testLogger(), dir, func(ctx context.Context, job *Job) RunResult {
		calls.Add(1)
		time.Sleep(100 * time.Millisecond)
		return RunResult{Text: "done"}
	})

	j := &Job{ID: "guard-test", Spec: "@every 1h", Enabled: true}
	m.mu.Lock()
	m.jobs = append(m.jobs, j)
	m.mu.Unlock()

	// Trigger two overlapping runs
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		m.runJob(context.Background(), j)
	}()
	// Small delay so the first goroutine acquires the guard
	time.Sleep(10 * time.Millisecond)
	go func() {
		defer wg.Done()
		m.runJob(context.Background(), j)
	}()
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("expected runFunc called exactly once, got %d", got)
	}
}

// ──────────────────────────────────────────────────────────────────────
// REQ-501: Backoff delay
// ──────────────────────────────────────────────────────────────────────

func TestBackoffDelay(t *testing.T) {
	base := time.Hour

	tests := []struct {
		errors   int
		expected time.Duration
	}{
		{0, 0},
		{1, time.Hour},                // 1h * 2^0 = 1h
		{3, 4 * time.Hour},            // 1h * 2^2 = 4h (at cap)
		{11, 4 * time.Hour},           // exponent capped at 10, 1h * 1024 > 4h → capped at 4h
	}

	for _, tt := range tests {
		got := backoffDelay(tt.errors, base)
		if got != tt.expected {
			t.Errorf("backoffDelay(%d, %v) = %v, want %v", tt.errors, base, got, tt.expected)
		}
	}
}

func TestBackoffScheduling(t *testing.T) {
	dir := t.TempDir()
	m := New(testLogger(), dir, func(ctx context.Context, job *Job) RunResult {
		return RunResult{Text: "ok"}
	})

	j := &Job{
		ID:      "backoff-test",
		Spec:    "@every 1h",
		Enabled: true,
		State: &JobState{
			ConsecutiveErrors: 3,
		},
	}
	m.mu.Lock()
	m.jobs = append(m.jobs, j)
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.scheduleJob(ctx, j)

	m.mu.Lock()
	nextRunMs := j.State.NextRunAtMs
	m.mu.Unlock()

	nextRun := time.UnixMilli(nextRunMs)
	// The base wait is ~1h from @every 1h. Backoff for 3 errors = 1h * 2^2 = 4h.
	// Total should be ~5h. Assert it's at least 4h from now (the backoff portion).
	minExpected := time.Now().Add(4 * time.Hour)
	if nextRun.Before(minExpected) {
		t.Errorf("expected next run at least 4h from now (got %v, want >= %v)", nextRun, minExpected)
	}
}
