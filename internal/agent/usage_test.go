package agent

import (
	"testing"
)

func TestNormalizeUsage_OpenAI(t *testing.T) {
	raw := map[string]any{
		"prompt_tokens":     float64(100),
		"completion_tokens": float64(50),
		"total_tokens":      float64(150),
	}
	u := NormalizeUsage(raw)
	if u.Input != 100 {
		t.Errorf("Input = %d, want 100", u.Input)
	}
	if u.Output != 50 {
		t.Errorf("Output = %d, want 50", u.Output)
	}
	if u.Total != 150 {
		t.Errorf("Total = %d, want 150", u.Total)
	}
}

func TestNormalizeUsage_Anthropic(t *testing.T) {
	raw := map[string]any{
		"input_tokens":                float64(200),
		"output_tokens":               float64(80),
		"cache_read_input_tokens":     float64(50),
		"cache_creation_input_tokens": float64(10),
	}
	u := NormalizeUsage(raw)
	if u.Input != 200 {
		t.Errorf("Input = %d, want 200", u.Input)
	}
	if u.Output != 80 {
		t.Errorf("Output = %d, want 80", u.Output)
	}
	if u.CacheRead != 50 {
		t.Errorf("CacheRead = %d, want 50", u.CacheRead)
	}
	if u.CacheWrite != 10 {
		t.Errorf("CacheWrite = %d, want 10", u.CacheWrite)
	}
	if u.Total != 280 { // 200+80
		t.Errorf("Total = %d, want 280", u.Total)
	}
}

func TestNormalizeUsage_NestedCachedTokens(t *testing.T) {
	raw := map[string]any{
		"prompt_tokens":     float64(100),
		"completion_tokens": float64(30),
		"prompt_tokens_details": map[string]any{
			"cached_tokens": float64(40),
		},
	}
	u := NormalizeUsage(raw)
	if u.CacheRead != 40 {
		t.Errorf("CacheRead = %d, want 40", u.CacheRead)
	}
}

func TestNormalizeUsage_NegativeClamp(t *testing.T) {
	// Some providers (Kimi/pi-ai) pre-subtract cache from prompt, yielding negative
	raw := map[string]any{
		"input_tokens":  float64(-50),
		"output_tokens": float64(100),
	}
	u := NormalizeUsage(raw)
	if u.Input != 0 {
		t.Errorf("Input = %d, want 0 (clamped)", u.Input)
	}
	if u.Output != 100 {
		t.Errorf("Output = %d, want 100", u.Output)
	}
	if u.Total != 100 {
		t.Errorf("Total = %d, want 100", u.Total)
	}
}

func TestNormalizeUsage_Google(t *testing.T) {
	raw := map[string]any{
		"promptTokenCount":     float64(300),
		"candidatesTokenCount": float64(120),
		"totalTokenCount":      float64(420),
	}
	u := NormalizeUsage(raw)
	if u.Input != 300 {
		t.Errorf("Input = %d, want 300", u.Input)
	}
	if u.Output != 120 {
		t.Errorf("Output = %d, want 120", u.Output)
	}
	if u.Total != 420 {
		t.Errorf("Total = %d, want 420", u.Total)
	}
}

func TestNormalizeUsage_Nil(t *testing.T) {
	u := NormalizeUsage(nil)
	if u.Total != 0 {
		t.Errorf("Total = %d, want 0", u.Total)
	}
}

func TestUsageTracker_Accumulate(t *testing.T) {
	ut := NewUsageTracker()
	ut.Accumulate("s1", NormalizedUsage{Input: 100, Output: 50, Total: 150})
	ut.Accumulate("s1", NormalizedUsage{Input: 200, Output: 80, Total: 280})
	ut.Accumulate("s2", NormalizedUsage{Input: 10, Output: 5, Total: 15})

	u1, c1 := ut.GetSession("s1")
	if u1.Input != 300 {
		t.Errorf("s1 Input = %d, want 300", u1.Input)
	}
	if u1.Output != 130 {
		t.Errorf("s1 Output = %d, want 130", u1.Output)
	}
	if u1.Total != 430 {
		t.Errorf("s1 Total = %d, want 430", u1.Total)
	}
	if c1 != 2 {
		t.Errorf("s1 Calls = %d, want 2", c1)
	}

	u2, c2 := ut.GetSession("s2")
	if u2.Input != 10 || c2 != 1 {
		t.Errorf("s2: Input=%d Calls=%d, want 10/1", u2.Input, c2)
	}
}

func TestUsageTracker_GetAll(t *testing.T) {
	ut := NewUsageTracker()
	ut.Accumulate("s1", NormalizedUsage{Input: 100, Output: 50, Total: 150})
	ut.Accumulate("s2", NormalizedUsage{Input: 200, Output: 80, Total: 280})

	all := ut.GetAll()
	if len(all) != 2 {
		t.Fatalf("len(GetAll) = %d, want 2", len(all))
	}
}

func TestUsageTracker_Aggregate(t *testing.T) {
	ut := NewUsageTracker()
	ut.Accumulate("s1", NormalizedUsage{Input: 100, Output: 50, CacheRead: 10, Total: 150})
	ut.Accumulate("s2", NormalizedUsage{Input: 200, Output: 80, CacheRead: 20, Total: 280})

	agg := ut.Aggregate()
	if agg.Input != 300 {
		t.Errorf("Aggregate Input = %d, want 300", agg.Input)
	}
	if agg.CacheRead != 30 {
		t.Errorf("Aggregate CacheRead = %d, want 30", agg.CacheRead)
	}
	if agg.Total != 430 {
		t.Errorf("Aggregate Total = %d, want 430", agg.Total)
	}
}

func TestUsageTracker_UnknownSession(t *testing.T) {
	ut := NewUsageTracker()
	u, c := ut.GetSession("nonexistent")
	if u.Total != 0 || c != 0 {
		t.Errorf("unknown session: Total=%d Calls=%d, want 0/0", u.Total, c)
	}
}
