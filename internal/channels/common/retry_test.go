package common

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// RetrySend
// ---------------------------------------------------------------------------

func TestRetrySend_SuccessOnFirstAttempt(t *testing.T) {
	calls := 0
	err := RetrySend(RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond}, "hi", func(s string) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
}

func TestRetrySend_SuccessOnSecondAttempt(t *testing.T) {
	calls := 0
	err := RetrySend(RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond}, "hi", func(s string) error {
		calls++
		if calls == 1 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls, got %d", calls)
	}
}

func TestRetrySend_AllAttemptsFail(t *testing.T) {
	calls := 0
	err := RetrySend(RetryConfig{MaxAttempts: 3, BaseDelay: time.Millisecond}, "hi", func(s string) error {
		calls++
		return errors.New("fail")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
}

func TestRetrySend_PlainTextFallback(t *testing.T) {
	calls := 0
	var lastText string
	err := RetrySend(RetryConfig{
		MaxAttempts:      2,
		BaseDelay:        time.Millisecond,
		PlainTextOnFinal: true,
	}, "**bold** `code`", func(s string) error {
		calls++
		lastText = s
		if calls == 1 {
			return errors.New("Bad Request: can't parse entities")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls, got %d", calls)
	}
	if strings.Contains(lastText, "**") || strings.Contains(lastText, "`") {
		t.Fatalf("expected stripped markdown, got %q", lastText)
	}
}

func TestRetrySend_DefaultConfig(t *testing.T) {
	rc := RetryConfig{}
	if rc.maxAttempts() != 3 {
		t.Fatalf("expected default maxAttempts=3, got %d", rc.maxAttempts())
	}
	if rc.baseDelay() != 500*time.Millisecond {
		t.Fatalf("expected default baseDelay=500ms, got %v", rc.baseDelay())
	}
	if rc.maxDelay() != 5*time.Second {
		t.Fatalf("expected default maxDelay=5s, got %v", rc.maxDelay())
	}
}

// ---------------------------------------------------------------------------
// parseRetryAfter
// ---------------------------------------------------------------------------

func TestParseRetryAfter_Found(t *testing.T) {
	tests := []struct {
		msg  string
		want time.Duration
	}{
		{"flood control: retry_after: 5", 5 * time.Second},
		{"Too Many Requests: retry after 10", 10 * time.Second},
		{"retry-after: 2.5 seconds", 2500 * time.Millisecond},
	}
	for _, tc := range tests {
		got := parseRetryAfter(tc.msg)
		if got != tc.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

func TestParseRetryAfter_NotFound(t *testing.T) {
	if d := parseRetryAfter("some random error"); d != 0 {
		t.Fatalf("expected 0, got %v", d)
	}
}

func TestParseRetryAfter_TooLarge(t *testing.T) {
	// Values >= 120s are ignored.
	if d := parseRetryAfter("retry_after: 300"); d != 0 {
		t.Fatalf("expected 0 for large value, got %v", d)
	}
}

// ---------------------------------------------------------------------------
// StripMarkdown
// ---------------------------------------------------------------------------

func TestStripMarkdown_Bold(t *testing.T) {
	got := StripMarkdown("**bold** text")
	if got != "bold text" {
		t.Fatalf("expected 'bold text', got %q", got)
	}
}

func TestStripMarkdown_CodeBlock(t *testing.T) {
	got := StripMarkdown("```go\nfmt.Println()\n```")
	if strings.Contains(got, "```") {
		t.Fatalf("code fences should be removed, got %q", got)
	}
	if !strings.Contains(got, "fmt.Println()") {
		t.Fatalf("code content should remain, got %q", got)
	}
}

func TestStripMarkdown_InlineCode(t *testing.T) {
	got := StripMarkdown("use `foo()` here")
	expected := "use foo() here"
	if got != expected {
		t.Fatalf("expected %q, got %q", expected, got)
	}
}

func TestStripMarkdown_Empty(t *testing.T) {
	if StripMarkdown("") != "" {
		t.Fatal("expected empty output")
	}
}

// ---------------------------------------------------------------------------
// parseLeadingFloat
// ---------------------------------------------------------------------------

func TestParseLeadingFloat(t *testing.T) {
	tests := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"5", 5, true},
		{"2.5 seconds", 2.5, true},
		{"abc", 0, false},
		{"10.0", 10, true},
	}
	for _, tc := range tests {
		got, ok := parseLeadingFloat(tc.in)
		if ok != tc.ok {
			t.Errorf("parseLeadingFloat(%q) ok=%v, want %v", tc.in, ok, tc.ok)
		}
		if ok && got != tc.want {
			t.Errorf("parseLeadingFloat(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
