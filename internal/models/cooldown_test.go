package models

import (
	"testing"
	"time"
)

func TestCooldown_InitiallyAvailable(t *testing.T) {
	cd := NewCooldown(CooldownConfig{})
	if !cd.IsAvailable("test-model") {
		t.Fatal("model should be available before any failure")
	}
}

func TestCooldown_UnavailableAfterFailure(t *testing.T) {
	cd := NewCooldown(CooldownConfig{BaseDelay: 1 * time.Hour})
	cd.RecordFailure("test-model")
	if cd.IsAvailable("test-model") {
		t.Fatal("model should be in cooldown after failure")
	}
}

func TestCooldown_AvailableAfterExpiry(t *testing.T) {
	cd := NewCooldown(CooldownConfig{BaseDelay: 1 * time.Millisecond})
	cd.RecordFailure("test-model")
	time.Sleep(5 * time.Millisecond)
	if !cd.IsAvailable("test-model") {
		t.Fatal("model should be available after cooldown expires")
	}
}

func TestCooldown_ExponentialBackoff(t *testing.T) {
	cd := NewCooldown(CooldownConfig{
		BaseDelay:  10 * time.Millisecond,
		MaxDelay:   1 * time.Second,
		Multiplier: 2,
	})
	cd.RecordFailure("m")
	r1 := cd.CooldownRemaining("m")
	cd.RecordFailure("m")
	r2 := cd.CooldownRemaining("m")
	if r2 <= r1 {
		t.Fatalf("expected longer cooldown after 2nd failure: %v <= %v", r2, r1)
	}
}

func TestCooldown_CappedAtMaxDelay(t *testing.T) {
	cd := NewCooldown(CooldownConfig{
		BaseDelay:  100 * time.Millisecond,
		MaxDelay:   200 * time.Millisecond,
		Multiplier: 10,
	})
	for i := 0; i < 10; i++ {
		cd.RecordFailure("m")
	}
	r := cd.CooldownRemaining("m")
	if r > 210*time.Millisecond {
		t.Fatalf("cooldown should be capped at ~200ms, got %v", r)
	}
}

func TestCooldown_RecordSuccessClears(t *testing.T) {
	cd := NewCooldown(CooldownConfig{BaseDelay: 1 * time.Hour})
	cd.RecordFailure("m")
	cd.RecordSuccess("m")
	if !cd.IsAvailable("m") {
		t.Fatal("model should be available after success")
	}
}

func TestCooldown_Reset(t *testing.T) {
	cd := NewCooldown(CooldownConfig{BaseDelay: 1 * time.Hour})
	cd.RecordFailure("m")
	cd.Reset("m")
	if !cd.IsAvailable("m") {
		t.Fatal("model should be available after reset")
	}
}

func TestCooldown_ResetAll(t *testing.T) {
	cd := NewCooldown(CooldownConfig{BaseDelay: 1 * time.Hour})
	cd.RecordFailure("m1")
	cd.RecordFailure("m2")
	cd.ResetAll()
	if !cd.IsAvailable("m1") || !cd.IsAvailable("m2") {
		t.Fatal("all models should be available after reset-all")
	}
}

func TestCooldown_AvailableFrom(t *testing.T) {
	cd := NewCooldown(CooldownConfig{BaseDelay: 1 * time.Hour})
	cd.RecordFailure("m2")
	all := []string{"m1", "m2", "m3"}
	avail := cd.AvailableFrom(all)
	if len(avail) != 2 {
		t.Fatalf("expected 2 available models, got %d", len(avail))
	}
	for _, m := range avail {
		if m == "m2" {
			t.Fatal("m2 should be in cooldown")
		}
	}
}

func TestCooldown_CooldownRemaining_Zero(t *testing.T) {
	cd := NewCooldown(CooldownConfig{})
	if r := cd.CooldownRemaining("nonexistent"); r != 0 {
		t.Fatalf("expected 0 remaining for unknown model, got %v", r)
	}
}

func TestCooldown_DefaultConfig(t *testing.T) {
	cd := NewCooldown(CooldownConfig{})
	if cd.baseDelay != 1*time.Minute {
		t.Fatalf("expected default baseDelay=1m, got %v", cd.baseDelay)
	}
	if cd.maxDelay != 1*time.Hour {
		t.Fatalf("expected default maxDelay=1h, got %v", cd.maxDelay)
	}
	if cd.multiplier != 5 {
		t.Fatalf("expected default multiplier=5, got %v", cd.multiplier)
	}
}
