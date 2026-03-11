package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/agentapi"
	"github.com/EMSERO/gopherclaw/internal/orchestrator"
	"github.com/EMSERO/gopherclaw/internal/taskqueue"
)

// DispatchTool lets an orchestrator agent execute a task graph against
// registered subagents. The orchestrator LLM produces a JSON task graph,
// and this tool runs it through orchestrator.Dispatcher.
type DispatchTool struct {
	// Agents maps agent ID → Chatter (same registry as DelegateTool).
	Agents map[string]Chatter
	// MaxConcurrent limits simultaneous subtasks (default 5).
	MaxConcurrent int
	// ProgressFn is called after each task completes (optional).
	ProgressFn orchestrator.ProgressFunc
	// Announcers deliver dispatch acknowledgements and progress to the user's channel (REQ-160/161).
	Announcers []Announcer
	// ProgressUpdates enables per-task completion announcements (REQ-161).
	ProgressUpdates bool
	// TaskMgr submits the dispatch as a managed background task when set.
	TaskMgr *taskqueue.Manager
	// MainAgentID is the ID of the main agent in the Agents map.
	// When set, async results are routed through the main agent for
	// summarization before being announced to the channel.
	MainAgentID string
	// Logger is the injected structured logger.
	Logger *zap.SugaredLogger
}

type dispatchInput struct {
	TaskGraph json.RawMessage `json:"task_graph"`
}

func (t *DispatchTool) Name() string { return "dispatch" }

func (t *DispatchTool) Description() string {
	return "Dispatch a task graph with multiple parallel or sequential subtasks to different subagents. Supports dependency chains and output interpolation between tasks."
}

func (t *DispatchTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"task_graph": {
				"type": "object",
				"description": "A structured task graph with a 'tasks' array. Each task has: id (string), agent_id (string), message (string), depends_on ([]string, task IDs), blocking (bool), timeout_seconds (int, optional). Use {{task-id.output}} in message to interpolate upstream task output.",
				"properties": {
					"tasks": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {
								"id": {"type": "string"},
								"agent_id": {"type": "string"},
								"message": {"type": "string"},
								"depends_on": {"type": "array", "items": {"type": "string"}},
								"blocking": {"type": "boolean"},
								"timeout_seconds": {"type": "integer"}
							},
							"required": ["id", "agent_id", "message", "blocking"]
						}
					}
				},
				"required": ["tasks"]
			}
		},
		"required": ["task_graph"]
	}`)
}

func (t *DispatchTool) Run(ctx context.Context, argsJSON string) string {
	var in dispatchInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err)
	}

	graph, err := orchestrator.ParseTaskGraph(in.TaskGraph)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}

	graphJSON, _ := json.Marshal(graph)
	t.Logger.Infof("dispatch: executing task graph with %d tasks: %s", len(graph.Tasks), string(graphJSON))

	parentSessionKey, _ := ctx.Value(agentapi.SessionKeyContextKey{}).(string)

	// When TaskMgr is set, submit as a managed background task
	if t.TaskMgr != nil {
		announcers := t.Announcers
		mainAgentID := t.MainAgentID
		agents := t.Agents

		taskID := t.TaskMgr.Submit(parentSessionKey, "orchestrator", fmt.Sprintf("dispatch %d tasks", len(graph.Tasks)), func(taskCtx context.Context) (string, error) {
			return t.executeDispatch(taskCtx, graph, parentSessionKey)
		}, taskqueue.SubmitOpts{
			OnComplete: func(result string, err error) {
				announceAsyncResult(announceParams{
					agentID:          "orchestrator",
					parentSessionKey: parentSessionKey,
					result:           result,
					err:              err,
					mainAgentID:      mainAgentID,
					agents:           agents,
					announcers:       announcers,
					logger:           t.Logger,
					notifPrefix:      "Background dispatch completed",
					maxRetries:       defaultAnnounceMaxRetries,
				baseBackoffMs:    defaultAnnounceBaseBackoffMs,
				})
			},
		})
		idShort := taskID
		if len(idShort) > 16 {
			idShort = idShort[:16]
		}
		return fmt.Sprintf("Dispatch submitted (task %s). %d tasks will run in background. Results will be announced.", idShort, len(graph.Tasks))
	}

	// Synchronous fallback when no TaskMgr
	result, err := t.executeDispatch(ctx, graph, parentSessionKey)
	if err != nil {
		return fmt.Sprintf("error: dispatch failed: %v", err)
	}
	return result
}

// executeDispatch runs the task graph and returns the formatted result.
func (t *DispatchTool) executeDispatch(ctx context.Context, graph orchestrator.TaskGraph, parentSessionKey string) (string, error) {
	// REQ-160: Send dispatch acknowledgement to user before execution starts
	if parentSessionKey != "" && len(t.Announcers) > 0 {
		ack := fmt.Sprintf("Running %d tasks in parallel, back shortly.", len(graph.Tasks))
		for _, a := range t.Announcers {
			a.AnnounceToSession(parentSessionKey, ack)
		}
	}

	d := &orchestrator.Dispatcher{
		Agents:        t.Agents,
		MaxConcurrent: t.MaxConcurrent,
	}
	// Use global limiter from TaskMgr when available
	if t.TaskMgr != nil {
		d.Limiter = t.TaskMgr
	}

	// REQ-161: Wire progress updates to user session when enabled
	progress := t.ProgressFn
	if progress == nil && t.ProgressUpdates && parentSessionKey != "" && len(t.Announcers) > 0 {
		total := len(graph.Tasks)
		var completed atomic.Int64
		progress = func(taskID string, agentID string, status orchestrator.TaskStatus) {
			n := completed.Add(1)
			if t.TaskMgr != nil {
				t.TaskMgr.AnnounceProgress(parentSessionKey, fmt.Sprintf("[%d/%d] %s (%s): %s", n, total, taskID, agentID, status))
			} else {
				msg := fmt.Sprintf("[%d/%d] %s (%s): %s", n, total, taskID, agentID, status)
				for _, a := range t.Announcers {
					a.AnnounceToSession(parentSessionKey, msg)
				}
			}
		}
	}
	if progress == nil {
		progress = orchestrator.NoOpProgress
	}

	rs, err := d.Execute(ctx, graph, progress)
	if err != nil {
		return "", err
	}

	// Log summary
	var succeeded, failed, cancelled int
	for _, r := range rs.Tasks {
		switch r.Status {
		case orchestrator.TaskStatusSuccess:
			succeeded++
		case orchestrator.TaskStatusFailed, orchestrator.TaskStatusTimeout:
			failed++
		case orchestrator.TaskStatusCancelled:
			cancelled++
		}
	}
	t.Logger.Infof("dispatch: complete — %d succeeded, %d failed, %d cancelled", succeeded, failed, cancelled)

	// Return the full result set as JSON + appendix
	rsJSON, _ := json.Marshal(rs)
	appendix := rs.FormatAppendix()
	return fmt.Sprintf("## Dispatch Results\n\n```json\n%s\n```\n\n## Task Outputs\n\n%s", string(rsJSON), appendix), nil
}
