package common

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// SmartChunk — basic splitting
// ---------------------------------------------------------------------------

func TestSmartChunk_ShortText(t *testing.T) {
	parts := SmartChunk("hello", 100)
	if len(parts) != 1 || parts[0] != "hello" {
		t.Fatalf("expected single part, got %v", parts)
	}
}

func TestSmartChunk_ExactLength(t *testing.T) {
	msg := strings.Repeat("x", 100)
	parts := SmartChunk(msg, 100)
	if len(parts) != 1 || parts[0] != msg {
		t.Fatalf("expected single part for exact-length, got %d parts", len(parts))
	}
}

func TestSmartChunk_EmptyInput(t *testing.T) {
	parts := SmartChunk("", 100)
	if len(parts) != 1 || parts[0] != "" {
		t.Fatalf("expected single empty part, got %v", parts)
	}
}

func TestSmartChunk_DefaultMaxLen(t *testing.T) {
	msg := strings.Repeat("a", 5000)
	parts := SmartChunk(msg, 0) // should default to 4096
	if len(parts) < 2 {
		t.Fatal("expected multiple parts with default maxLen")
	}
}

// ---------------------------------------------------------------------------
// SmartChunk — break-point priorities
// ---------------------------------------------------------------------------

func TestSmartChunk_PrefersParagraphBreak(t *testing.T) {
	para1 := strings.Repeat("a", 40)
	para2 := strings.Repeat("b", 40)
	text := para1 + "\n\n" + para2
	parts := SmartChunk(text, 50)
	if len(parts) < 2 {
		t.Fatalf("expected split at paragraph boundary, got %d parts", len(parts))
	}
	if parts[0] != para1+"\n\n" {
		t.Fatalf("first part should end with paragraph break, got %q", parts[0])
	}
}

func TestSmartChunk_PrefersNewlineOverHardCut(t *testing.T) {
	line1 := strings.Repeat("a", 80)
	line2 := strings.Repeat("b", 80)
	text := line1 + "\n" + line2
	parts := SmartChunk(text, 100)
	if len(parts) < 2 {
		t.Fatalf("expected split at newline, got %d parts", len(parts))
	}
	if parts[0] != line1+"\n" {
		t.Fatalf("expected first part to be line1+newline, got %q", parts[0])
	}
}

func TestSmartChunk_SentenceBoundary(t *testing.T) {
	// No newlines — should split at sentence boundary.
	s1 := strings.Repeat("a", 45) + "."
	s2 := strings.Repeat("b", 45)
	text := s1 + " " + s2
	parts := SmartChunk(text, 50)
	if len(parts) < 2 {
		t.Fatalf("expected split at sentence boundary, got %d parts", len(parts))
	}
	if parts[0] != s1 {
		t.Fatalf("expected first part to end at sentence, got %q", parts[0])
	}
}

func TestSmartChunk_SpaceFallback(t *testing.T) {
	// No newlines, no sentence boundaries — should split at word boundary.
	text := strings.Repeat("word ", 25) // 125 chars
	parts := SmartChunk(text, 50)
	if len(parts) < 2 {
		t.Fatalf("expected multiple parts, got %d", len(parts))
	}
	for i, p := range parts {
		if len(p) > 50 {
			t.Fatalf("part[%d] exceeds maxLen: %d bytes", i, len(p))
		}
	}
}

func TestSmartChunk_HardCutNoBreaks(t *testing.T) {
	text := strings.Repeat("x", 200)
	parts := SmartChunk(text, 50)
	// All parts should be exactly maxLen except possibly the last.
	for i, p := range parts[:len(parts)-1] {
		if len(p) != 50 {
			t.Fatalf("part[%d] should be exactly maxLen (50), got %d", i, len(p))
		}
	}
}

func TestSmartChunk_ReassemblyWithoutFences(t *testing.T) {
	text := strings.Repeat("abc\n", 100) // 400 chars
	parts := SmartChunk(text, 50)
	reassembled := strings.Join(parts, "")
	if reassembled != text {
		t.Fatal("reassembled text diverges from original")
	}
}

// ---------------------------------------------------------------------------
// SmartChunk — code fence awareness
// ---------------------------------------------------------------------------

func TestSmartChunk_ClosesOpenFence(t *testing.T) {
	code := "```go\nfunc main() {\n" + strings.Repeat("  // line\n", 20) + "}\n```"
	text := "Before code:\n" + code + "\nAfter code."
	parts := SmartChunk(text, 80)
	if len(parts) < 2 {
		t.Fatalf("expected multiple parts, got %d", len(parts))
	}
	// If the first part ends inside a fence, it must end with ``` (closing).
	if fenceOpen := trackFence(parts[0], ""); fenceOpen != "" {
		// The healing should have added a close — re-check.
		t.Fatalf("first part should not end with open fence after healing, got open=%q", fenceOpen)
	}
}

func TestSmartChunk_ReopensClosedFence(t *testing.T) {
	// Create a code block that must span two chunks.
	code := "```python\n" + strings.Repeat("x = 1\n", 30) + "```"
	parts := SmartChunk(code, 80)
	if len(parts) < 2 {
		t.Fatalf("expected multi-part, got %d", len(parts))
	}
	// The second part should start with the reopened fence header.
	if !strings.HasPrefix(parts[1], "```python\n") {
		t.Fatalf("second part should reopen the fence, got %q", parts[1][:min(40, len(parts[1]))])
	}
}

func TestSmartChunk_MultipleFences(t *testing.T) {
	block1 := "```js\nconsole.log(1);\n```"
	block2 := "```go\nfmt.Println(1)\n```"
	text := block1 + "\n\nSome text in between.\n\n" + block2
	parts := SmartChunk(text, 40)

	// Reassemble and check that all fences are properly paired.
	full := strings.Join(parts, "")
	opens := strings.Count(full, "```js") + strings.Count(full, "```go")
	closes := strings.Count(full, "\n```\n") + strings.Count(full, "\n```")
	// There should be at least as many closes as opens (healing may add extras).
	if closes < 2 {
		t.Fatalf("expected at least 2 fence closings, got %d (opens=%d)", closes, opens)
	}
}

func TestSmartChunk_TildesFence(t *testing.T) {
	text := "~~~bash\necho hello\n" + strings.Repeat("echo line\n", 20) + "~~~"
	parts := SmartChunk(text, 60)
	if len(parts) < 2 {
		t.Fatalf("expected multi-part with tildes, got %d", len(parts))
	}
	// First part should be closed with ~~~
	if !strings.HasSuffix(strings.TrimSpace(parts[0]), "~~~") {
		t.Fatalf("expected first part to be closed with ~~~, got %q", parts[0][max(0, len(parts[0])-20):])
	}
}

func TestSmartChunk_NestedBackticksIgnored(t *testing.T) {
	// Inline backticks inside a code fence should not confuse the tracker.
	text := "```\nhere is `inline` and ``more``\n" + strings.Repeat("line\n", 30) + "```"
	parts := SmartChunk(text, 60)
	// After healing, no part should end with an unmatched fence.
	for i, p := range parts {
		if open := trackFence(p, ""); open != "" {
			t.Fatalf("part[%d] has unmatched fence %q after healing", i, open)
		}
	}
}

// ---------------------------------------------------------------------------
// findBreakPoint unit tests
// ---------------------------------------------------------------------------

func TestFindBreakPoint_ParagraphPreferred(t *testing.T) {
	text := "aaa\n\nbbb" + strings.Repeat("c", 100)
	bp := findBreakPoint(text, 10)
	if bp != 5 { // "aaa\n\n" = 5 bytes
		t.Fatalf("expected break at 5, got %d", bp)
	}
}

func TestFindBreakPoint_NewlineFallback(t *testing.T) {
	text := "aaa\nbbb" + strings.Repeat("c", 100)
	bp := findBreakPoint(text, 10)
	if bp != 4 { // "aaa\n" = 4 bytes
		t.Fatalf("expected break at 4, got %d", bp)
	}
}

// ---------------------------------------------------------------------------
// trackFence / healFences unit tests
// ---------------------------------------------------------------------------

func TestTrackFence_NoFence(t *testing.T) {
	if f := trackFence("hello\nworld", ""); f != "" {
		t.Fatalf("expected no fence, got %q", f)
	}
}

func TestTrackFence_OpenFence(t *testing.T) {
	text := "```go\nfunc main() {"
	if f := trackFence(text, ""); f != "```go" {
		t.Fatalf("expected open fence '```go', got %q", f)
	}
}

func TestTrackFence_ClosedFence(t *testing.T) {
	text := "```go\nfunc main() {\n}\n```"
	if f := trackFence(text, ""); f != "" {
		t.Fatalf("expected closed fence, got %q", f)
	}
}

func TestTrackFence_InitialOpen(t *testing.T) {
	// Starting inside a fence, encountering a close.
	text := "some code\n```"
	if f := trackFence(text, "```go"); f != "" {
		t.Fatalf("expected fence to close, got %q", f)
	}
}

func TestHealFences_NoFence(t *testing.T) {
	parts := []string{"hello", "world"}
	healed := healFences(parts)
	if len(healed) != 2 || healed[0] != "hello" || healed[1] != "world" {
		t.Fatalf("expected no changes, got %v", healed)
	}
}

func TestHealFences_SinglePart(t *testing.T) {
	parts := []string{"```go\ncode\n```"}
	healed := healFences(parts)
	if len(healed) != 1 || healed[0] != parts[0] {
		t.Fatalf("single part should be unchanged, got %v", healed)
	}
}

func TestFenceMark(t *testing.T) {
	tests := []struct{ in, want string }{
		{"```go", "```"},
		{"~~~bash", "~~~"},
		{"````", "````"},
		{"```", "```"},
	}
	for _, tc := range tests {
		got := fenceMark(tc.in)
		if got != tc.want {
			t.Errorf("fenceMark(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsFenceClose(t *testing.T) {
	tests := []struct {
		trimmed, open string
		want          bool
	}{
		{"```", "```go", true},
		{"```", "```", true},
		{"~~~", "~~~bash", true},
		{"````", "```go", true}, // more backticks is OK
		{"``", "```go", false},  // fewer backticks is not
		{"~~~", "```go", false}, // different character
	}
	for _, tc := range tests {
		got := isFenceClose(tc.trimmed, tc.open)
		if got != tc.want {
			t.Errorf("isFenceClose(%q, %q) = %v, want %v", tc.trimmed, tc.open, got, tc.want)
		}
	}
}
