// Package commands provides a unified slash-command handler shared by all channels.
package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/EMSERO/gopherclaw/internal/agent"
	"github.com/EMSERO/gopherclaw/internal/config"
	"github.com/EMSERO/gopherclaw/internal/cron"
	"github.com/EMSERO/gopherclaw/internal/session"
	"github.com/EMSERO/gopherclaw/internal/taskqueue"
)

// Ctx holds all dependencies needed to execute commands.
type Ctx struct {
	SessionKey  string
	Agent       *agent.Agent
	Sessions    *session.Manager
	Config      *config.Root
	CronManager *cron.Manager          // optional; enables /cron commands
	TaskManager *taskqueue.Manager     // optional; enables /tasks and /cancel commands
	Version     string                  // binary version (for /status)
	StartTime   time.Time              // process start time (for /status uptime)
	SkillCount  int                    // loaded skill count (for /status)
	Fallbacks   []string               // model fallback chain (for /status)
}

// Result is the return value from Handle.
type Result struct {
	Text    string
	Handled bool
}

// Handle processes a slash command if text starts with '/'.
// Returns Result.Handled=true if the command was recognised (even if it erred).
// Unknown slash commands also return Handled=true so they are not forwarded to the agent.
func Handle(ctx context.Context, text string, cmdCtx Ctx) Result {
	if !strings.HasPrefix(text, "/") {
		return Result{}
	}
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return Result{}
	}

	cmd := parts[0]

	// Don't treat file paths (e.g. /home/user/...) as slash commands.
	if strings.Contains(cmd, "/") && len(cmd) > 1 && cmd[1:] != "" && strings.ContainsRune(cmd[1:], '/') {
		return Result{} // contains multiple slashes — it's a path, not a command
	}
	// args = everything after the command
	args := strings.TrimSpace(strings.TrimPrefix(text, cmd))

	switch cmd {
	case "/help":
		return Result{Text: helpText, Handled: true}

	case "/new", "/reset":
		cmdCtx.Sessions.Reset(cmdCtx.SessionKey)
		return Result{Text: "Session cleared.", Handled: true}

	case "/compact":
		if err := cmdCtx.Agent.Compact(ctx, cmdCtx.SessionKey, args); err != nil {
			return Result{Text: fmt.Sprintf("Compact failed: %v", err), Handled: true}
		}
		return Result{Text: "Session compacted.", Handled: true}

	case "/model":
		if args == "" {
			m := cmdCtx.Agent.ResolveModel(cmdCtx.SessionKey)
			return Result{Text: fmt.Sprintf("Current model: `%s`", m), Handled: true}
		}
		cmdCtx.Agent.SetSessionModel(cmdCtx.SessionKey, args)
		return Result{Text: fmt.Sprintf("Model set to: `%s`", args), Handled: true}

	case "/context":
		history, _ := cmdCtx.Sessions.GetHistory(cmdCtx.SessionKey)
		tokens := session.EstimateTokens(history)
		return Result{
			Text:    fmt.Sprintf("Messages: %d\nEstimated tokens: ~%d\nSession key: %s", len(history), tokens, cmdCtx.SessionKey),
			Handled: true,
		}

	case "/export":
		history, _ := cmdCtx.Sessions.GetHistory(cmdCtx.SessionKey)
		if len(history) == 0 {
			return Result{Text: "Session is empty.", Handled: true}
		}
		var sb strings.Builder
		for _, m := range history {
			fmt.Fprintf(&sb, "[%s]: %s\n\n", m.Role, m.Content)
		}
		return Result{Text: sb.String(), Handled: true}

	case "/cron":
		if cmdCtx.CronManager == nil {
			return Result{Text: "Cron is not enabled.", Handled: true}
		}
		return handleCron(args, cmdCtx.CronManager)

	case "/tasks":
		if cmdCtx.TaskManager == nil {
			return Result{Text: "Task queue is not enabled.", Handled: true}
		}
		return handleTasks(args, cmdCtx.SessionKey, cmdCtx.TaskManager)

	case "/cancel":
		if cmdCtx.TaskManager == nil {
			return Result{Text: "Task queue is not enabled.", Handled: true}
		}
		return handleCancel(args, cmdCtx.SessionKey, cmdCtx.TaskManager)

	case "/status":
		return handleStatus(cmdCtx)
	}

	// Unknown slash command
	return Result{Text: fmt.Sprintf("Unknown command: %s\n\nType /help for available commands.", cmd), Handled: true}
}

const helpText = `/help — show this message
/new — clear session and start fresh
/reset — same as /new
/compact — compress session history to save context
/model — show current model
/model <name> — switch model (e.g. /model sonnet)
/context — show session size and token estimate
/export — dump full conversation history
/status — show version, uptime, model, skills, and queue depth
/cron — manage scheduled jobs (list, add, remove, enable, disable)
/tasks — show running and recent background tasks
/cancel — cancel all tasks for this session
/cancel <id> — cancel a specific task`

func handleStatus(cmdCtx Ctx) Result {
	model := "unknown"
	if cmdCtx.Agent != nil {
		model = cmdCtx.Agent.ResolveModel(cmdCtx.SessionKey)
	}

	uptime := "unknown"
	if !cmdCtx.StartTime.IsZero() {
		uptime = time.Since(cmdCtx.StartTime).Truncate(time.Second).String()
	}

	queueDepth := 0
	if cmdCtx.TaskManager != nil {
		for _, t := range cmdCtx.TaskManager.List() {
			if t.Status == taskqueue.StatusRunning || t.Status == taskqueue.StatusPending {
				queueDepth++
			}
		}
	}

	sessionCount := 0
	if cmdCtx.Sessions != nil {
		sessionCount = len(cmdCtx.Sessions.ActiveSessions())
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Version: %s\n", cmdCtx.Version)
	fmt.Fprintf(&sb, "Uptime: %s\n", uptime)
	fmt.Fprintf(&sb, "Model: %s\n", model)
	if len(cmdCtx.Fallbacks) > 0 {
		fmt.Fprintf(&sb, "Fallbacks: %s\n", strings.Join(cmdCtx.Fallbacks, " → "))
	}
	if cmdCtx.Config != nil {
		if sub := cmdCtx.Config.Agents.Defaults.Subagents.Model; sub != "" {
			fmt.Fprintf(&sb, "Subagent model: %s\n", sub)
		}
		for _, a := range cmdCtx.Config.Agents.List {
			if a.Default {
				continue
			}
			agentModel := ""
			if a.CLICommand != "" {
				agentModel = a.CLICommand
				for i, arg := range a.CLIArgs {
					if arg == "--model" && i+1 < len(a.CLIArgs) {
						agentModel = a.CLIArgs[i+1] + " (CLI)"
						break
					}
				}
			}
			if agentModel != "" {
				fmt.Fprintf(&sb, "Agent %s: %s\n", a.ID, agentModel)
			} else {
				fmt.Fprintf(&sb, "Agent %s: (inherits primary)\n", a.ID)
			}
		}
	}
	fmt.Fprintf(&sb, "Sessions: %d\n", sessionCount)
	fmt.Fprintf(&sb, "Skills: %d\n", cmdCtx.SkillCount)
	fmt.Fprintf(&sb, "Queue depth: %d", queueDepth)

	return Result{Text: sb.String(), Handled: true}
}

func handleCron(args string, mgr *cron.Manager) Result {
	parts := strings.Fields(args)
	if len(parts) == 0 {
		return cronHelp()
	}

	sub := parts[0]
	rest := strings.TrimSpace(strings.TrimPrefix(args, sub))

	switch sub {
	case "list":
		jobs := mgr.List()
		if len(jobs) == 0 {
			return Result{Text: "No cron jobs scheduled.", Handled: true}
		}
		var sb strings.Builder
		sb.WriteString("Cron jobs:\n")
		for _, j := range jobs {
			status := "enabled"
			if !j.Enabled {
				status = "disabled"
			}
			fmt.Fprintf(&sb, "• %s  %s  [%s]", j.DisplayName(), j.DisplaySchedule(), status)
			if j.State != nil && j.State.LastRunAtMs > 0 {
				ago := time.Since(time.UnixMilli(j.State.LastRunAtMs)).Truncate(time.Second)
				fmt.Fprintf(&sb, "  last=%s (%s ago)", j.State.LastRunStatus, ago)
			}
			sb.WriteByte('\n')
		}
		return Result{Text: sb.String(), Handled: true}

	case "add":
		// Format: /cron add <spec> <instruction>
		// spec is the first token of rest
		specParts := strings.SplitN(rest, " ", 2)
		if len(specParts) < 2 {
			return Result{Text: "Usage: /cron add <spec> <instruction>\nExample: /cron add @daily Send daily summary", Handled: true}
		}
		spec := specParts[0]
		instruction := strings.TrimSpace(specParts[1])
		// Handle @every separately (two tokens: @every 1h)
		if spec == "@every" && len(strings.Fields(rest)) >= 2 {
			f := strings.Fields(rest)
			spec = "@every " + f[1]
			instruction = strings.TrimSpace(strings.Join(f[2:], " "))
		}
		if instruction == "" {
			return Result{Text: "Instruction cannot be empty.", Handled: true}
		}
		job, err := mgr.Add(spec, instruction)
		if err != nil {
			return Result{Text: fmt.Sprintf("Error: %v", err), Handled: true}
		}
		return Result{Text: fmt.Sprintf("Cron job added: %s", job.ID), Handled: true}

	case "remove", "rm":
		if rest == "" {
			return Result{Text: "Usage: /cron remove <id>", Handled: true}
		}
		if err := mgr.Remove(rest); err != nil {
			return Result{Text: fmt.Sprintf("Error: %v", err), Handled: true}
		}
		return Result{Text: "Cron job removed.", Handled: true}

	case "enable":
		if rest == "" {
			return Result{Text: "Usage: /cron enable <id>", Handled: true}
		}
		if err := mgr.SetEnabled(rest, true); err != nil {
			return Result{Text: fmt.Sprintf("Error: %v", err), Handled: true}
		}
		return Result{Text: "Cron job enabled.", Handled: true}

	case "disable":
		if rest == "" {
			return Result{Text: "Usage: /cron disable <id>", Handled: true}
		}
		if err := mgr.SetEnabled(rest, false); err != nil {
			return Result{Text: fmt.Sprintf("Error: %v", err), Handled: true}
		}
		return Result{Text: "Cron job disabled.", Handled: true}

	default:
		return cronHelp()
	}
}

func cronHelp() Result {
	return Result{
		Text: "Usage:\n" +
			"  /cron list\n" +
			"  /cron add <spec> <instruction>\n" +
			"  /cron remove <id>\n" +
			"  /cron enable <id>\n" +
			"  /cron disable <id>\n\n" +
			"Spec examples: @daily, @hourly, @weekly, @every 1h, @every 30m, 09:00",
		Handled: true,
	}
}

func handleTasks(args, sessionKey string, mgr *taskqueue.Manager) Result {
	var tasks []*taskqueue.TaskRecord
	if args == "all" {
		tasks = mgr.List()
	} else {
		tasks = mgr.ListForSession(sessionKey)
	}

	if len(tasks) == 0 {
		return Result{Text: "No tasks.", Handled: true}
	}

	var sb strings.Builder
	for _, t := range tasks {
		icon := "⏳"
		switch t.Status {
		case taskqueue.StatusRunning:
			icon = "▶"
		case taskqueue.StatusSuccess:
			icon = "✓"
		case taskqueue.StatusFailed:
			icon = "✗"
		case taskqueue.StatusCancelled:
			icon = "⊘"
		}
		fmt.Fprintf(&sb, "%s %s  %s  [%s]", icon, t.ID[:8], t.Message, t.Status)
		if t.DurationMs > 0 {
			fmt.Fprintf(&sb, "  %dms", t.DurationMs)
		}
		sb.WriteByte('\n')
	}
	return Result{Text: sb.String(), Handled: true}
}

func handleCancel(args, sessionKey string, mgr *taskqueue.Manager) Result {
	if args != "" {
		if err := mgr.Cancel(args); err != nil {
			return Result{Text: fmt.Sprintf("Cancel failed: %v", err), Handled: true}
		}
		return Result{Text: fmt.Sprintf("Task %s cancelled.", args), Handled: true}
	}

	n := mgr.CancelAll(sessionKey)
	if n == 0 {
		return Result{Text: "No running tasks to cancel.", Handled: true}
	}
	return Result{Text: fmt.Sprintf("Cancelled %d task(s).", n), Handled: true}
}
