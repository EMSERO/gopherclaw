package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMatchesDangerousPattern(t *testing.T) {
	cases := []struct {
		command string
		want    string // expected matching pattern, or "" for no match
	}{
		// Exact dangerous patterns
		{"rm -rf /", "rm -rf /"},
		// "rm -rf /*" matches "rm -rf /" first (it's a substring), so the
		// returned pattern is the first match in builtinDangerousPatterns.
		{"rm -rf /*", "rm -rf /"},
		{"mkfs.ext4 /dev/sda1", "mkfs."},
		{"dd if=/dev/zero of=/dev/sda", "dd if="},
		{":(){:|:&};:", ":(){"},
		{"chmod 777 /", "chmod 777 /"},
		{"echo data > /dev/sda", "> /dev/sda"},
		{"mv / /backup", "mv / "},
		{"shutdown -h now", "shutdown"},
		{"reboot", "reboot"},
		{"init 0", "init 0"},
		{"init 6", "init 6"},
		{"halt", "halt"},
		{"poweroff", "poweroff"},

		// Patterns embedded in longer commands
		{"sudo rm -rf / --no-preserve-root", "rm -rf /"},
		{"echo foo && dd if=/dev/urandom of=disk.img", "dd if="},

		// Word boundary: standalone "halt" still matches
		{"sudo halt -f", "halt"},
		{"halt now", "halt"},
		{" halt", "halt"},

		// Word boundary: "halt" inside other words must NOT match
		{"install asphalt driveway", ""},
		{"cat << 'EOF'\nasphalt racing\nEOF", ""},
		{"echo halting is not halt", "halt"}, // "halt" at end IS standalone

		// Word boundary: multiple occurrences, second is standalone
		{"asphalt && halt", "halt"},

		// Safe commands — no match
		{"echo hello", ""},
		{"ls -la", ""},
		{"cat /etc/hosts", ""},
		{"rm file.txt", ""},          // plain rm without -rf /
		{"chmod 644 myfile.txt", ""}, // not 777 /
		{"mv foo bar", ""},           // not mv /
		{"grep -r pattern .", ""},
		{"git status", ""},
		{"go test ./...", ""},
	}

	for _, tc := range cases {
		got := (&ExecTool{}).matchesDangerousPattern(tc.command)
		if got != tc.want {
			t.Errorf("(&ExecTool{}).matchesDangerousPattern(%q) = %q, want %q", tc.command, got, tc.want)
		}
	}
}

func TestMatchesDangerousPattern_CaseInsensitive(t *testing.T) {
	// matchesDangerousPattern lowercases the command before matching against
	// the built-in patterns. Patterns that are all-lowercase in
	// builtinDangerousPatterns will match regardless of input casing.
	cases := []struct {
		command string
		want    string
	}{
		{"RM -RF /", "rm -rf /"},
		{"Rm -Rf /", "rm -rf /"},
		{"MKFS.ext4 /dev/sda", "mkfs."},
		{"DD IF=/dev/zero of=/dev/sda", "dd if="},
		{"SHUTDOWN -h now", "shutdown"},
		{"REBOOT", "reboot"},
		{"Init 0", "init 0"},
		{"Halt", "halt"},
		{"PowerOff", "poweroff"},
		{"MV / /somewhere", "mv / "},
		// Mixed-case safe commands remain safe
		{"ECHO hello", ""},
		{"Git Status", ""},
	}

	for _, tc := range cases {
		got := (&ExecTool{}).matchesDangerousPattern(tc.command)
		if got != tc.want {
			t.Errorf("(&ExecTool{}).matchesDangerousPattern(%q) = %q, want %q", tc.command, got, tc.want)
		}
	}
}

func TestExecTool_DangerousCommand_NoConfirm(t *testing.T) {
	tool := &ExecTool{
		DefaultTimeout: 5 * time.Second,
		Confirm:        nil, // no confirm callback — should hard-block
	}

	dangerousCmds := []string{
		"rm -rf /",
		"dd if=/dev/zero of=/dev/sda",
		"mkfs.ext4 /dev/sda1",
		"shutdown -h now",
	}

	for _, cmd := range dangerousCmds {
		args, _ := json.Marshal(ExecInput{Command: cmd})
		result := tool.Run(context.Background(), string(args))

		if !strings.Contains(result, "blocked") {
			t.Errorf("command %q: expected hard-block error, got %q", cmd, result)
		}
		if !strings.Contains(result, "dangerous pattern") {
			t.Errorf("command %q: expected 'dangerous pattern' in error, got %q", cmd, result)
		}
		if !strings.Contains(result, "no confirmation channel") {
			t.Errorf("command %q: expected 'no confirmation channel' in error, got %q", cmd, result)
		}
	}
}

func TestExecTool_DangerousCommand_Confirmed(t *testing.T) {
	confirmCalled := false

	tool := &ExecTool{
		DefaultTimeout: 5 * time.Second,
		ConfirmTimeout: 1 * time.Second,
		Confirm: func(command string, timeout time.Duration) (bool, error) {
			confirmCalled = true
			// Verify the command is passed through
			if !strings.Contains(command, "echo") {
				t.Errorf("expected command containing 'echo', got %q", command)
			}
			// Verify timeout is passed
			if timeout != 1*time.Second {
				t.Errorf("expected confirm timeout 1s, got %v", timeout)
			}
			return true, nil
		},
	}

	// Use a command that matches a dangerous pattern but is actually safe to run.
	// "echo rm -rf /" contains "rm -rf /" as a substring, triggering the pattern,
	// but the actual execution is just echo.
	args, _ := json.Marshal(ExecInput{Command: "echo rm -rf / is dangerous"})
	result := tool.Run(context.Background(), string(args))

	if !confirmCalled {
		t.Error("expected Confirm callback to be called")
	}
	if strings.Contains(result, "error") {
		t.Errorf("expected successful execution after confirmation, got %q", result)
	}
	if !strings.Contains(result, "rm -rf / is dangerous") {
		t.Errorf("expected echo output, got %q", result)
	}
}

func TestExecTool_DangerousCommand_Denied(t *testing.T) {
	confirmCalled := false

	tool := &ExecTool{
		DefaultTimeout: 5 * time.Second,
		Confirm: func(command string, timeout time.Duration) (bool, error) {
			confirmCalled = true
			return false, nil // user denies
		},
	}

	args, _ := json.Marshal(ExecInput{Command: "echo rm -rf / simulated"})
	result := tool.Run(context.Background(), string(args))

	if !confirmCalled {
		t.Error("expected Confirm callback to be called")
	}
	if !strings.Contains(result, "cancelled by user") {
		t.Errorf("expected 'cancelled by user' error, got %q", result)
	}
}

func TestExecTool_DangerousCommand_ConfirmError(t *testing.T) {
	confirmErr := errors.New("terminal disconnected")

	tool := &ExecTool{
		DefaultTimeout: 5 * time.Second,
		Confirm: func(command string, timeout time.Duration) (bool, error) {
			return false, confirmErr
		},
	}

	args, _ := json.Marshal(ExecInput{Command: "echo rm -rf / simulated"})
	result := tool.Run(context.Background(), string(args))

	if !strings.Contains(result, "confirmation failed") {
		t.Errorf("expected 'confirmation failed' in error, got %q", result)
	}
	if !strings.Contains(result, "terminal disconnected") {
		t.Errorf("expected propagated error message, got %q", result)
	}
}

func TestConfirmManager_NewAndAddChannel(t *testing.T) {
	mgr := NewConfirmManager()
	if mgr == nil {
		t.Fatal("expected non-nil ConfirmManager")
	}

	ch := &mockConfirmer{canConfirm: true, result: true}
	mgr.AddChannel(ch)

	confirmed, err := mgr.RequestExecConfirm(context.Background(), "user:telegram:123", "echo hello", "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !confirmed {
		t.Error("expected confirmed=true")
	}
	if !ch.called {
		t.Error("expected SendConfirmPrompt to be called")
	}
}

func TestConfirmManager_NoMatchingChannel(t *testing.T) {
	mgr := NewConfirmManager()
	ch := &mockConfirmer{canConfirm: false}
	mgr.AddChannel(ch)

	_, err := mgr.RequestExecConfirm(context.Background(), "user:discord:456", "rm -rf /", "rm -rf /")
	if err == nil {
		t.Fatal("expected error when no channel can confirm")
	}
	if !strings.Contains(err.Error(), "no channel can reach") {
		t.Errorf("expected 'no channel can reach' in error, got %q", err.Error())
	}
}

func TestConfirmManager_RoutesToCorrectChannel(t *testing.T) {
	mgr := NewConfirmManager()
	ch1 := &mockConfirmer{canConfirm: false}
	ch2 := &mockConfirmer{canConfirm: true, result: false}
	mgr.AddChannel(ch1)
	mgr.AddChannel(ch2)

	confirmed, err := mgr.RequestExecConfirm(context.Background(), "session:key", "cmd", "pattern")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if confirmed {
		t.Error("expected confirmed=false from ch2")
	}
	if ch1.called {
		t.Error("ch1 should not have been called")
	}
	if !ch2.called {
		t.Error("ch2 should have been called")
	}
}

func TestConfirmManager_EmptyChannels(t *testing.T) {
	mgr := NewConfirmManager()
	_, err := mgr.RequestExecConfirm(context.Background(), "key", "cmd", "p")
	if err == nil {
		t.Fatal("expected error with no channels")
	}
}

type mockConfirmer struct {
	canConfirm bool
	result     bool
	called     bool
}

func (m *mockConfirmer) CanConfirm(_ string) bool { return m.canConfirm }
func (m *mockConfirmer) SendConfirmPrompt(_ context.Context, _, _, _ string) (bool, error) {
	m.called = true
	return m.result, nil
}

func TestMatchesDangerousPattern_CustomPatterns(t *testing.T) {
	tool := &ExecTool{
		DangerousPatterns: []string{"docker rm", "npm publish"},
	}

	cases := []struct {
		command string
		want    string
	}{
		// Custom patterns trigger
		{"docker rm my-container", "docker rm"},
		{"npm publish --access public", "npm publish"},
		// Builtin patterns still work alongside custom ones
		{"rm -rf /", "rm -rf /"},
		{"shutdown -h now", "shutdown"},
		{"dd if=/dev/zero of=/dev/sda", "dd if="},
		// Safe commands still pass
		{"echo hello", ""},
		{"docker ps", ""},
		{"npm install", ""},
	}

	for _, tc := range cases {
		got := tool.matchesDangerousPattern(tc.command)
		if got != tc.want {
			t.Errorf("matchesDangerousPattern(%q) = %q, want %q", tc.command, got, tc.want)
		}
	}
}

func TestExecTool_CustomDangerousPattern_TriggersConfirm(t *testing.T) {
	confirmCalled := false

	tool := &ExecTool{
		DefaultTimeout:    5 * time.Second,
		ConfirmTimeout:    1 * time.Second,
		DangerousPatterns: []string{"docker rm"},
		Confirm: func(command string, timeout time.Duration) (bool, error) {
			confirmCalled = true
			return true, nil
		},
	}

	args, _ := json.Marshal(ExecInput{Command: "echo docker rm fake"})
	result := tool.Run(context.Background(), string(args))

	if !confirmCalled {
		t.Error("expected Confirm callback to be called for custom dangerous pattern")
	}
	if strings.Contains(result, "error") {
		t.Errorf("expected successful execution after confirmation, got %q", result)
	}
}

func TestExecTool_SafeCommand_NoConfirmNeeded(t *testing.T) {
	confirmCalled := false

	tool := &ExecTool{
		DefaultTimeout: 5 * time.Second,
		Confirm: func(command string, timeout time.Duration) (bool, error) {
			confirmCalled = true
			return true, nil
		},
	}

	args, _ := json.Marshal(ExecInput{Command: "echo test"})
	result := tool.Run(context.Background(), string(args))

	if confirmCalled {
		t.Error("Confirm should NOT be called for safe commands")
	}
	if !strings.Contains(result, "test") {
		t.Errorf("expected echo output 'test', got %q", result)
	}
	if strings.Contains(result, "error") {
		t.Errorf("expected no error for safe command, got %q", result)
	}
}
