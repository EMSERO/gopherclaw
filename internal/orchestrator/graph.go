package orchestrator

import (
	"encoding/json"
	"fmt"
	"time"
)

// TaskStatus represents the execution status of a task.
type TaskStatus string

const (
	TaskStatusPending   TaskStatus = "pending"
	TaskStatusRunning   TaskStatus = "running"
	TaskStatusSuccess   TaskStatus = "success"
	TaskStatusFailed    TaskStatus = "failed"
	TaskStatusCancelled TaskStatus = "cancelled"
	TaskStatusTimeout   TaskStatus = "timeout"
)

// Task is a single unit of work in a task graph.
type Task struct {
	ID             string   `json:"id"`
	AgentID        string   `json:"agent_id"`
	Message        string   `json:"message"`
	DependsOn      []string `json:"depends_on"`
	Blocking       bool     `json:"blocking"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

// TaskGraph is the structured plan produced by the orchestrator LLM.
type TaskGraph struct {
	Tasks []Task `json:"tasks"`
}

// ParseTaskGraph unmarshals a JSON task graph, returning a validation error if
// the JSON is malformed.
func ParseTaskGraph(data []byte) (TaskGraph, error) {
	var g TaskGraph
	if err := json.Unmarshal(data, &g); err != nil {
		return TaskGraph{}, fmt.Errorf("parse task graph: %w", err)
	}
	return g, nil
}

// TaskResult holds the outcome of a single dispatched task.
type TaskResult struct {
	ID         string        `json:"id"`
	AgentID    string        `json:"agent_id"`
	Status     TaskStatus    `json:"status"`
	Output     string        `json:"output"`
	Error      string        `json:"error,omitempty"`
	DurationMs int64         `json:"duration_ms"`
	Duration   time.Duration `json:"-"`
}

// ResultSet is the complete set of results returned by the dispatcher.
type ResultSet struct {
	Tasks []TaskResult `json:"tasks"`
}

// ByID returns the result for a given task ID, or nil if not found.
func (rs *ResultSet) ByID(id string) *TaskResult {
	for i := range rs.Tasks {
		if rs.Tasks[i].ID == id {
			return &rs.Tasks[i]
		}
	}
	return nil
}

// FormatAppendix renders the result set in the <task-appendix> format
// expected by the orchestrator response.
func (rs *ResultSet) FormatAppendix() string {
	var b []byte
	for _, t := range rs.Tasks {
		header := fmt.Sprintf("[task: %s | agent: %s | status: %s]\n", t.ID, t.AgentID, t.Status)
		b = append(b, header...)
		switch t.Status {
		case TaskStatusSuccess:
			output := t.Output
			if len(output) > 2000 {
				output = output[:2000] + "\n... (truncated)"
			}
			b = append(b, output...)
		case TaskStatusFailed, TaskStatusTimeout:
			b = append(b, ("ERROR: " + t.Error)...)
		case TaskStatusCancelled:
			b = append(b, ("CANCELLED: " + t.Error)...)
		default:
			b = append(b, ("STATUS: " + string(t.Status))...)
		}
		b = append(b, "\n\n"...)
	}
	return string(b)
}
