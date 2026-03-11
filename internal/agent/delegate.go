package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/agentapi"
	"github.com/EMSERO/gopherclaw/internal/taskqueue"
)

// delegateDepthKey is the context key for tracking subagent call depth.
type delegateDepthKey struct{}

const defaultMaxDelegateDepth = 1

const (
	// defaultAnnounceMaxRetries is the number of times to retry main-agent
	// summarization before falling back to raw announce.
	defaultAnnounceMaxRetries = 3
	// defaultAnnounceBaseBackoffMs is the initial backoff in milliseconds between retries.
	defaultAnnounceBaseBackoffMs = 2000
)

// Chatter is an alias for agentapi.Chatter.
type Chatter = agentapi.Chatter

// Announcer is an alias for agentapi.Announcer.
type Announcer = agentapi.Announcer

// DelegateTool is a Tool that lets the main agent call a named subagent.
// It lives in the agent package to avoid a circular import with internal/tools.
type DelegateTool struct {
	// Agents maps agent ID → Chatter.  The main agent itself may be included.
	Agents map[string]Chatter
	// AsyncAgents is the set of agent IDs that should run asynchronously via TaskMgr.
	AsyncAgents map[string]bool
	// MainAgentID is the ID of the main agent in the Agents map.
	// When set, async results are routed through the main agent for
	// summarization before being announced to the channel.
	MainAgentID string
	// DefaultModel is applied to subagent calls when the caller does not
	// specify an explicit model override.  Sourced from config
	// agents.defaults.subagents.model.  Empty = inherit the subagent's own default.
	DefaultModel string
	// MaxDepth is the maximum recursion depth (default 5).
	MaxDepth int
	// Announcers deliver async results back to the originating session.
	Announcers []Announcer
	// TaskMgr is the task queue manager for async tasks.
	TaskMgr *taskqueue.Manager
	// Logger is the injected structured logger.
	Logger *zap.SugaredLogger
	// AnnounceMaxRetries overrides the default retry count for async result
	// announcement. Zero (default) uses defaultAnnounceMaxRetries.
	AnnounceMaxRetries int
	// AnnounceBaseBackoffMs overrides the default backoff for async result
	// announcement retries. Zero (default) uses defaultAnnounceBaseBackoffMs.
	AnnounceBaseBackoffMs int
}

type delegateInput struct {
	Action    string `json:"action,omitempty"` // "status", "cancel", "steer", or "run" (default)
	AgentID   string `json:"agent_id"`
	Message   string `json:"message"`
	SessionID string `json:"session_id,omitempty"`
	TaskID    string `json:"task_id,omitempty"` // for action=cancel or action=steer
	Model     string `json:"model,omitempty"`   // optional model override for spawn
	Mode      string `json:"mode,omitempty"`    // "ephemeral" (default) or "persistent"
}

func (t *DelegateTool) Name() string { return "delegate" }

func (t *DelegateTool) Description() string {
	return "Delegate a task to a subagent by ID, check the status of background tasks, cancel a running task, or steer (redirect) a running task with a new message."
}

func (t *DelegateTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"action": {
				"type": "string",
				"enum": ["run", "status", "cancel", "steer"],
				"description": "Action: 'run' (default) delegates to a subagent, 'status' returns ALL background tasks (including orchestrator dispatches), 'cancel' cancels any task by task_id, 'steer' cancels then re-spawns with a new message"
			},
			"agent_id": {
				"type": "string",
				"description": "ID of the subagent to call (required for action=run)"
			},
			"message": {
				"type": "string",
				"description": "Message or task to send to the subagent (required for action=run or action=steer)"
			},
			"session_id": {
				"type": "string",
				"description": "Optional: reuse a specific subagent session (omit for a fresh ephemeral session)"
			},
			"task_id": {
				"type": "string",
				"description": "Task ID to cancel or steer (required for action=cancel and action=steer; use action=status to find IDs)"
			},
			"model": {
				"type": "string",
				"description": "Optional model override for the subagent (e.g. 'github-copilot/gpt-4o'). Only for action=run."
			},
			"mode": {
				"type": "string",
				"enum": ["ephemeral", "persistent"],
				"description": "Session mode: 'ephemeral' (default) creates a fresh session per call, 'persistent' reuses a stable session key for the agent allowing multi-turn conversations"
			}
		}
	}`)
}

func (t *DelegateTool) Run(ctx context.Context, argsJSON string) string {
	var in delegateInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err)
	}

	if in.Action == "status" {
		return t.taskStatus()
	}
	if in.Action == "cancel" {
		if in.TaskID == "" {
			return "error: task_id is required for action=cancel"
		}
		if err := t.TaskMgr.Cancel(in.TaskID); err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		return fmt.Sprintf("Task %s cancelled.", in.TaskID)
	}
	if in.Action == "steer" {
		return t.steer(ctx, in)
	}

	// Recursion depth check
	depth, _ := ctx.Value(delegateDepthKey{}).(int)
	maxDepth := t.MaxDepth
	if maxDepth <= 0 {
		maxDepth = defaultMaxDelegateDepth
	}
	if depth >= maxDepth {
		return fmt.Sprintf("error: subagent recursion limit (%d) reached", maxDepth)
	}

	if in.AgentID == "" {
		return "error: agent_id is required"
	}
	if in.Message == "" {
		return "error: message is required"
	}

	ag, ok := t.Agents[in.AgentID]
	if !ok {
		return fmt.Sprintf("error: subagent %q not found; available: %v", in.AgentID, agentIDs(t.Agents))
	}

	sessionKey := in.SessionID
	if sessionKey == "" {
		if in.Mode == "persistent" {
			// Deterministic key so the session persists across delegate calls.
			parentKey, _ := ctx.Value(agentapi.SessionKeyContextKey{}).(string)
			sessionKey = fmt.Sprintf("subagent:%s:persistent:%s", in.AgentID, parentKey)
		} else {
			sessionKey = fmt.Sprintf("subagent:%s:%s", in.AgentID, randomHex(8))
		}
	}

	// Apply model override: explicit request wins, then DefaultModel from config.
	model := in.Model
	if model == "" {
		model = t.DefaultModel
	}
	if model != "" {
		if a, ok := ag.(*Agent); ok {
			a.SetSessionModel(sessionKey, model)
			defer a.ClearSessionModel(sessionKey)
		}
	}

	// CLI agents and agents marked async run via TaskManager.
	_, isCLI := ag.(*CLIAgent)
	isAsync := t.AsyncAgents[in.AgentID]
	if isCLI || isAsync {
		return t.runAsync(ctx, ag, in.AgentID, sessionKey, in.Message, model)
	}

	// Synchronous delegation for in-process agents
	ctx = context.WithValue(ctx, delegateDepthKey{}, depth+1)
	resp, err := ag.Chat(ctx, sessionKey, in.Message)
	if err != nil {
		return fmt.Sprintf("error calling subagent %q: %v", in.AgentID, err)
	}
	if resp.Stopped {
		return fmt.Sprintf("[subagent stopped] %s", resp.Text)
	}
	return resp.Text
}

// runAsync submits the subagent call to TaskManager and announces the
// result back to the originating session when done.
func (t *DelegateTool) runAsync(ctx context.Context, ag Chatter, agentID, sessionKey, message, model string) string {
	parentSessionKey, _ := ctx.Value(agentapi.SessionKeyContextKey{}).(string)

	announcers := t.Announcers

	mainAgentID := t.MainAgentID
	agents := t.Agents

	// Resolve announce retry config (zero = use defaults).
	maxRetries := t.AnnounceMaxRetries
	if maxRetries == 0 {
		maxRetries = defaultAnnounceMaxRetries
	}
	baseBackoffMs := t.AnnounceBaseBackoffMs
	if baseBackoffMs == 0 {
		baseBackoffMs = defaultAnnounceBaseBackoffMs
	}

	taskID := t.TaskMgr.Submit(parentSessionKey, agentID, message, func(taskCtx context.Context) (string, error) {
		// Use a background context for depth tracking, but honour TaskManager's cancellation
		chatCtx := context.WithValue(taskCtx, delegateDepthKey{}, 1)

		// Apply model override for async tasks.
		if model != "" {
			if a, ok := ag.(*Agent); ok {
				a.SetSessionModel(sessionKey, model)
				defer a.ClearSessionModel(sessionKey)
			}
		}

		resp, err := ag.Chat(chatCtx, sessionKey, message)
		if err != nil {
			return "", err
		}
		return resp.Text, nil
	}, taskqueue.SubmitOpts{
		OnComplete: func(result string, err error) {
			announceAsyncResult(announceParams{
				agentID:          agentID,
				parentSessionKey: parentSessionKey,
				result:           result,
				err:              err,
				mainAgentID:      mainAgentID,
				agents:           agents,
				announcers:       announcers,
				logger:           t.Logger,
				notifPrefix:      "Background task completed",
				maxRetries:       maxRetries,
				baseBackoffMs:    baseBackoffMs,
			})
		},
	})

	t.Logger.Infof("delegate: submitted async %s (task %s) for session %s", agentID, taskID, parentSessionKey)
	idShort := taskID
	if len(idShort) > 16 {
		idShort = idShort[:16]
	}
	return fmt.Sprintf("Spawned %s in background (task %s). Results will be announced when complete.", agentID, idShort)
}

// steer cancels a running task and re-spawns it with a new message.
func (t *DelegateTool) steer(ctx context.Context, in delegateInput) string {
	if in.TaskID == "" {
		return "error: task_id is required for action=steer"
	}
	if in.Message == "" {
		return "error: message is required for action=steer (the new instruction)"
	}

	// Find the task to get its agent ID and session info.
	tasks := t.TaskMgr.List()
	var target *taskqueue.TaskRecord
	for _, task := range tasks {
		if task.ID == in.TaskID || strings.HasPrefix(task.ID, in.TaskID) {
			target = task
			break
		}
	}
	if target == nil {
		return fmt.Sprintf("error: task %q not found", in.TaskID)
	}

	agentID := in.AgentID
	if agentID == "" {
		agentID = target.AgentID
	}
	ag, ok := t.Agents[agentID]
	if !ok {
		return fmt.Sprintf("error: subagent %q not found", agentID)
	}

	// Cancel the old task.
	_ = t.TaskMgr.Cancel(in.TaskID)

	// Re-spawn with the new message on a fresh session.
	model := in.Model
	if model == "" {
		model = t.DefaultModel
	}
	sessionKey := fmt.Sprintf("subagent:%s:%s", agentID, randomHex(8))
	return t.runAsync(ctx, ag, agentID, sessionKey, in.Message, model)
}

// taskStatus returns a summary of all tasks from TaskManager.
func (t *DelegateTool) taskStatus() string {
	tasks := t.TaskMgr.List()
	if len(tasks) == 0 {
		return "No tasks."
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d task(s):\n", len(tasks))
	for _, task := range tasks {
		preview := task.Message
		if len(preview) > 80 {
			preview = preview[:80] + "..."
		}
		idShort := task.ID
		if len(idShort) > 8 {
			idShort = idShort[:8]
		}
		fmt.Fprintf(&b, "- [%s] agent=%s, status=%s, message: %q\n", idShort, task.AgentID, task.Status, preview)
	}
	return b.String()
}

// announceParams holds parameters for the announce-with-retry helper.
type announceParams struct {
	agentID          string
	parentSessionKey string
	result           string
	err              error
	mainAgentID      string
	agents           map[string]Chatter
	announcers       []Announcer
	logger           *zap.SugaredLogger
	notifPrefix      string // e.g. "Background task completed" or "Background dispatch completed"
	maxRetries       int    // max retry attempts (used as-is)
	baseBackoffMs    int    // initial backoff in milliseconds (used as-is)
}

// maxAsyncResultChars caps the size of async delegate results injected into the
// parent session to prevent context-window explosion. ~4K tokens.
const maxAsyncResultChars = 16384

// announceAsyncResult routes an async result through the main agent with
// retry + exponential backoff, falling back to raw announce on exhaustion.
func announceAsyncResult(p announceParams) {
	p.logger.Debugf("announce: OnComplete for %s session=%s err=%v resultLen=%d",
		p.agentID, p.parentSessionKey, p.err, len(p.result))

	// Cap result size to prevent token explosion in the parent session.
	if len(p.result) > maxAsyncResultChars {
		p.logger.Infof("announce: truncating %s result from %d to %d chars", p.agentID, len(p.result), maxAsyncResultChars)
		p.result = p.result[:maxAsyncResultChars] + "\n\n[... result truncated to fit context window ...]"
	}

	// Build notification for the main agent.
	var notification string
	if p.err != nil {
		notification = fmt.Sprintf(
			"[%s] Agent %q failed with error: %v\n\nPlease inform the user about this failure.",
			p.notifPrefix, p.agentID, p.err)
	} else if strings.TrimSpace(p.result) == "" {
		notification = fmt.Sprintf(
			"[%s] Agent %q finished but produced no output.\n\nPlease let the user know the task completed.",
			p.notifPrefix, p.agentID)
	} else {
		notification = fmt.Sprintf(
			"[%s] Agent %q finished:\n\n%s\n\nPlease acknowledge this result to the user. Summarize the key outcome concisely.",
			p.notifPrefix, p.agentID, p.result)
	}

	maxRetries := p.maxRetries
	baseBackoffMs := p.baseBackoffMs

	// Route through the main agent with retry.
	if mainAg, ok := p.agents[p.mainAgentID]; ok && p.parentSessionKey != "" {
		for attempt := 0; attempt <= maxRetries; attempt++ {
			if attempt > 0 {
				backoff := time.Duration(baseBackoffMs*(1<<(attempt-1))) * time.Millisecond
				p.logger.Infof("announce: retry %d/%d for %s (backoff %v)", attempt, maxRetries, p.agentID, backoff)
				time.Sleep(backoff)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			resp, chatErr := mainAg.Chat(ctx, p.parentSessionKey, notification)
			cancel()
			if chatErr == nil {
				for _, a := range p.announcers {
					a.AnnounceToSession(p.parentSessionKey, resp.Text)
				}
				return
			}
			p.logger.Warnf("announce: main agent Chat failed (attempt %d/%d): %v", attempt+1, maxRetries+1, chatErr)
		}
		p.logger.Warnf("announce: all %d retries exhausted for %s; falling back to raw announce", maxRetries+1, p.agentID)
	}

	// Fallback: announce raw result directly.
	var rawText string
	if p.err != nil {
		rawText = fmt.Sprintf("**[%s]** error: %v", p.agentID, p.err)
	} else if strings.TrimSpace(p.result) == "" {
		rawText = fmt.Sprintf("**[%s]** completed (no output)", p.agentID)
	} else {
		rawText = fmt.Sprintf("**[%s result]**\n\n%s", p.agentID, p.result)
	}
	for _, a := range p.announcers {
		a.AnnounceToSession(p.parentSessionKey, rawText)
	}
}

// agentIDs returns a sorted list of agent IDs for error messages.
func agentIDs(m map[string]Chatter) []string {
	ids := make([]string, 0, len(m))
	for k := range m {
		ids = append(ids, k)
	}
	return ids
}

// randomHex generates n random bytes as a hex string.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}
