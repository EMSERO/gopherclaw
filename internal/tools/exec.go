package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ExecInput is the argument schema for the exec tool.
type ExecInput struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"` // seconds, 0 = use default
}

// SandboxConfig holds Docker sandbox settings (mirrors config.SandboxConfig).
type SandboxConfig struct {
	Enabled      bool
	Image        string
	Mounts       []string
	SetupCommand string
}

// builtinDangerousPatterns are hardcoded patterns that always require
// interactive confirmation before execution. These are checked via substring
// match on the raw command string. Like DenyCommands, this is defense-in-depth
// and trivially bypassable; the real security boundary is the Docker sandbox.
var builtinDangerousPatterns = []string{
	"rm -rf /",
	"rm -rf /*",
	"mkfs.",
	"dd if=",
	":(){", // fork bomb
	"chmod -R 777 /",
	"chmod 777 /",
	"> /dev/sda",
	"mv / ",
	"shutdown",
	"reboot",
	"init 0",
	"init 6",
	"halt",
	"poweroff",
	"userdel",
	"kill -9",
	"iptables -F",
	"| bash", // curl ... | bash and similar pipe-to-shell patterns
	"| sh",   // curl ... | sh
}

// ConfirmFunc is a callback that asks the user to confirm a dangerous command.
// It receives the command string and a timeout duration, and returns true if
// the user confirms execution. If nil, dangerous commands are hard-blocked.
type ConfirmFunc func(command string, timeout time.Duration) (bool, error)

// ExecTool executes a shell command and returns combined output.
type ExecTool struct {
	DefaultTimeout      time.Duration
	BackgroundWait      time.Duration     // if >0: return partial output after this delay if cmd still running
	BackgroundHardLimit time.Duration     // hard kill deadline for bg processes (default 30m)
	MaxOutputChars      int               // output truncation limit (default 100000)
	Env                 map[string]string // extra env vars
	DenyCommands        []string          // deny patterns (empty = no restriction)
	DangerousPatterns   []string          // extra patterns that trigger confirmation (merged with builtins)
	Sandbox             *SandboxConfig    // nil = run on host
	ConfirmTimeout      time.Duration     // timeout for destructive command confirmation (default 60s)
	Confirm             ConfirmFunc       // nil = hard-block dangerous commands
	Confirmer           ExecConfirmer     // session-aware confirmer (REQ-061); takes priority over Confirm

	mu          sync.Mutex
	containerID string // "" = not started
}

// ExecConfirmer presents a confirmation prompt via channel bots.
type ExecConfirmer interface {
	// RequestExecConfirm sends a confirmation prompt to the user for a dangerous
	// command. sessionKey identifies which user/chat to prompt. Returns true if
	// the user confirms, false otherwise.
	RequestExecConfirm(ctx context.Context, sessionKey, command, pattern string) (bool, error)
}

func (t *ExecTool) maxOutput() int {
	if t.MaxOutputChars > 0 {
		return t.MaxOutputChars
	}
	return 100_000
}

func (t *ExecTool) bgHardTimeout() time.Duration {
	if t.BackgroundHardLimit > 0 {
		return t.BackgroundHardLimit
	}
	return 30 * time.Minute
}

func (t *ExecTool) Name() string { return "exec" }

func (t *ExecTool) Description() string {
	return "Execute a shell command and return its output. Supports timeouts and background execution."
}

func (t *ExecTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {"type": "string", "description": "Shell command to execute"},
			"timeout": {"type": "integer", "description": "Timeout in seconds (optional)"}
		},
		"required": ["command"]
	}`)
}

func (t *ExecTool) Run(ctx context.Context, argsJSON string) string {
	var in ExecInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err)
	}

	timeout := t.DefaultTimeout
	if in.Timeout > 0 {
		timeout = time.Duration(in.Timeout) * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Deny-list check — defense-in-depth only. This substring check is trivially
	// bypassable via shell quoting, variable expansion, or encoding. The real
	// security boundary for untrusted commands is the Docker sandbox.
	for _, deny := range t.DenyCommands {
		if strings.Contains(in.Command, deny) {
			return fmt.Sprintf("error: command contains denied pattern %q", deny)
		}
	}

	// Destructive command confirmation — check built-in dangerous patterns (REQ-060–063).
	if pattern := t.matchesDangerousPattern(in.Command); pattern != "" {
		confirmTimeout := t.ConfirmTimeout
		if confirmTimeout == 0 {
			confirmTimeout = 60 * time.Second
		}
		// Try session-aware confirmer first (channel bots), then legacy ConfirmFunc.
		if t.Confirmer != nil {
			sessionKey, _ := ctx.Value(SessionKeyContextKey{}).(string)
			if sessionKey == "" {
				return fmt.Sprintf("error: command matches dangerous pattern %q — blocked (no confirmation channel available)", pattern)
			}
			confirmCtx, confirmCancel := context.WithTimeout(ctx, confirmTimeout)
			defer confirmCancel()
			confirmed, err := t.Confirmer.RequestExecConfirm(confirmCtx, sessionKey, in.Command, pattern)
			if err != nil {
				return fmt.Sprintf("error: confirmation failed: %v", err)
			}
			if !confirmed {
				return "error: command execution cancelled by user"
			}
		} else if t.Confirm != nil {
			confirmed, err := t.Confirm(in.Command, confirmTimeout)
			if err != nil {
				return fmt.Sprintf("error: confirmation failed: %v", err)
			}
			if !confirmed {
				return "error: command execution cancelled by user"
			}
		} else {
			return fmt.Sprintf("error: command matches dangerous pattern %q — blocked (no confirmation channel available)", pattern)
		}
	}

	var cmd *exec.Cmd

	if t.Sandbox != nil && t.Sandbox.Enabled {
		if err := t.ensureContainer(); err != nil {
			return fmt.Sprintf("error: sandbox init: %v", err)
		}
		t.mu.Lock()
		cid := t.containerID
		t.mu.Unlock()
		cmd = exec.CommandContext(ctx, "docker", "exec", cid, "bash", "-c", in.Command)
	} else {
		cmd = exec.CommandContext(ctx, "bash", "-c", in.Command)
		// Inherit environment, inject extras
		cmd.Env = os.Environ()
		for k, v := range t.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		// Set GOPHERCLAW_SHELL (+ OPENCLAW_SHELL for backward compat) so shell config can detect exec context
		cmd.Env = append(cmd.Env, "GOPHERCLAW_SHELL=1", "OPENCLAW_SHELL=1")
	}

	// Use pipe for concurrent output reading (required for background mode).
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return fmt.Sprintf("error: %v", err)
	}

	var buf bytes.Buffer
	var bufMu sync.Mutex // protects buf from concurrent read (background mode) and write (io.Copy)
	readDone := make(chan struct{})
	go func() {
		// io.Copy reads from pr into buf; bufMu serialises with background partial reads.
		var tmp [32 * 1024]byte
		for {
			n, err := pr.Read(tmp[:])
			if n > 0 {
				bufMu.Lock()
				buf.Write(tmp[:n])
				bufMu.Unlock()
			}
			if err != nil {
				break
			}
		}
		close(readDone)
	}()

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
		_ = pw.Close() // signal EOF to reader goroutine
	}()

	maxOut := t.maxOutput()
	finish := func(runErr error) string {
		<-readDone
		bufMu.Lock()
		output := buf.String()
		bufMu.Unlock()
		if len(output) > maxOut {
			dropped := len(output) - maxOut
			output = output[:maxOut] + fmt.Sprintf(
				"\n[... output truncated: showing %d of %d chars (%d dropped). "+
					"For large files, use head/tail/sed to process in chunks.]", maxOut, len(output), dropped)
		}
		if runErr != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return fmt.Sprintf("[timeout after %v]\n%s", timeout, output)
			}
			if output == "" {
				return fmt.Sprintf("error: %v", runErr)
			}
			return output
		}
		if output == "" {
			return "(no output)"
		}
		return output
	}

	// Background mode: if the command hasn't finished within BackgroundWait,
	// return partial output and let the process continue running. The reader
	// and waiter goroutines also continue until the process exits naturally.
	// This is by design — "background mode" is fire-and-forget.
	if t.BackgroundWait > 0 {
		select {
		case runErr := <-waitDone:
			return finish(runErr)
		case <-time.After(t.BackgroundWait):
			// Command still running — return partial output collected so far.
			// The process continues in the background with a hard kill deadline
			// to prevent indefinite goroutine/process leaks.
			go func() {
				select {
				case <-waitDone:
					// Process exited on its own — clean exit.
				case <-time.After(t.bgHardTimeout()):
					if cmd.Process != nil {
						_ = cmd.Process.Kill()
					}
				}
			}()
			bufMu.Lock()
			partial := buf.String()
			bufMu.Unlock()
			if len(partial) > maxOut {
				dropped := len(partial) - maxOut
				partial = partial[:maxOut] + fmt.Sprintf(
					"\n[... output truncated: showing %d of %d chars (%d dropped). "+
						"For large files, use head/tail/sed to process in chunks.]", maxOut, len(partial), dropped)
			}
			if partial == "" {
				return "[process still running in background — no output yet]"
			}
			return partial + "\n[...still running in background]"
		case <-ctx.Done():
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			return finish(ctx.Err())
		}
	}

	select {
	case runErr := <-waitDone:
		return finish(runErr)
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return finish(ctx.Err())
	}
}

// ensureContainer starts the sandbox container if not already running.
func (t *ExecTool) ensureContainer() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.containerID != "" {
		// Verify container is still running
		out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", t.containerID).Output()
		if err == nil && strings.TrimSpace(string(out)) == "true" {
			return nil
		}
		t.containerID = ""
	}

	args := []string{"run", "-d", "--rm"}
	for _, mount := range t.Sandbox.Mounts {
		args = append(args, "-v", mount)
	}
	args = append(args, t.Sandbox.Image, "sleep", "infinity")

	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		return fmt.Errorf("docker run: %w", err)
	}
	t.containerID = strings.TrimSpace(string(out))

	if t.Sandbox.SetupCommand != "" {
		setupOut, err := exec.Command("docker", "exec", t.containerID, "bash", "-c", t.Sandbox.SetupCommand).CombinedOutput()
		if err != nil {
			return fmt.Errorf("setup command failed: %v\n%s", err, setupOut)
		}
	}

	return nil
}

// matchesDangerousPattern returns the first matching dangerous pattern (builtin
// or custom), or "" if no pattern matches. Alphabetic pattern edges require word
// boundaries to avoid false positives (e.g. "asphalt" should not match "halt").
func (t *ExecTool) matchesDangerousPattern(command string) string {
	lower := strings.ToLower(command)
	// Merge builtin patterns with any user-configured custom patterns.
	patterns := builtinDangerousPatterns
	if len(t.DangerousPatterns) > 0 {
		patterns = make([]string, 0, len(builtinDangerousPatterns)+len(t.DangerousPatterns))
		patterns = append(patterns, builtinDangerousPatterns...)
		patterns = append(patterns, t.DangerousPatterns...)
	}
	for _, p := range patterns {
		off := 0
		for {
			idx := strings.Index(lower[off:], p)
			if idx < 0 {
				break
			}
			abs := off + idx
			// For patterns starting with a letter, require no preceding letter.
			if isAlpha(p[0]) && abs > 0 && isAlpha(lower[abs-1]) {
				off = abs + 1
				continue
			}
			// For patterns ending with a letter, require no following letter.
			end := abs + len(p)
			if isAlpha(p[len(p)-1]) && end < len(lower) && isAlpha(lower[end]) {
				off = abs + 1
				continue
			}
			return p
		}
	}
	return ""
}

func isAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// Cleanup stops and removes the sandbox container.
func (t *ExecTool) Cleanup() {
	t.mu.Lock()
	cid := t.containerID
	t.containerID = ""
	t.mu.Unlock()

	if cid != "" {
		_ = exec.Command("docker", "rm", "-f", cid).Run()
	}
}
