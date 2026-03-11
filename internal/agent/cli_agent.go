package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// CLIAgent implements Chatter by invoking an external CLI command.
// Each Chat call spawns a fresh subprocess: command [args...] message.
// This is the mechanism used for CLI-backed subagents such as Claude Code.
type CLIAgent struct {
	id      string
	command string
	args    []string
	timeout time.Duration
}

// NewCLIAgent creates a CLIAgent. timeout=0 means no additional timeout
// (the caller's context deadline still applies).
// The command is resolved via exec.LookPath at construction time so that
// bare names (e.g. "claude") work even when the subprocess environment
// has a minimal PATH (e.g. under systemd).
func NewCLIAgent(id, command string, args []string, timeout time.Duration) *CLIAgent {
	if resolved, err := exec.LookPath(command); err == nil {
		command = resolved
	}
	return &CLIAgent{id: id, command: command, args: args, timeout: timeout}
}

// Command returns the resolved command path.
func (a *CLIAgent) Command() string { return a.command }

// Chat runs the CLI command with the message appended as the final argument
// and returns its stdout as the response text.
func (a *CLIAgent) Chat(ctx context.Context, _ string, message string) (Response, error) {
	if a.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.timeout)
		defer cancel()
	}

	cmdArgs := make([]string, len(a.args)+1)
	copy(cmdArgs, a.args)
	cmdArgs[len(a.args)] = message

	cmd := exec.CommandContext(ctx, a.command, cmdArgs...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		stderrStr := strings.TrimSpace(stderr.String())
		if stderrStr != "" {
			return Response{}, fmt.Errorf("cli agent %q: %w\nstderr: %s", a.id, err, stderrStr)
		}
		return Response{}, fmt.Errorf("cli agent %q: %w", a.id, err)
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		// Some CLI tools write their output to stderr; use it as fallback.
		text = strings.TrimSpace(stderr.String())
	}
	return Response{Text: text}, nil
}
