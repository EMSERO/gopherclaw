package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/eidetic"
	"github.com/EMSERO/gopherclaw/internal/session"
)

func TestDispatchTool_Metadata(t *testing.T) {
	tool := &DispatchTool{Logger: zap.NewNop().Sugar()}

	if tool.Name() != "dispatch" {
		t.Errorf("expected name 'dispatch', got %q", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("Description should not be empty")
	}
	if !strings.Contains(tool.Description(), "task graph") {
		t.Errorf("Description should mention 'task graph', got %q", tool.Description())
	}

	var schema map[string]any
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatalf("Schema() is not valid JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Error("schema type should be 'object'")
	}
}

func TestDispatchTool_Run_InvalidJSON(t *testing.T) {
	tool := &DispatchTool{Agents: map[string]Chatter{}, Logger: zap.NewNop().Sugar()}
	result := tool.Run(context.Background(), `{bad json`)
	if !strings.Contains(result, "error") {
		t.Errorf("expected error for invalid JSON, got %q", result)
	}
}

func TestDispatchTool_Run_InvalidTaskGraph(t *testing.T) {
	tool := &DispatchTool{Agents: map[string]Chatter{}, Logger: zap.NewNop().Sugar()}
	// task_graph with unknown agent should fail parsing or execution
	args := `{"task_graph": {"tasks": [{"id":"t1","agent_id":"missing","message":"hi","blocking":true}]}}`
	result := tool.Run(context.Background(), args)
	// Should return an error (unknown agent)
	if !strings.Contains(result, "error") && !strings.Contains(result, "Dispatch Results") {
		t.Errorf("unexpected result for unknown agent: %q", result)
	}
}

// mockEideticClientForAgent implements eidetic.Client for agent tests.
type mockEideticClientForAgent struct{}

func (m *mockEideticClientForAgent) AppendMemory(_ context.Context, _ eidetic.AppendRequest) error {
	return nil
}
func (m *mockEideticClientForAgent) SearchMemory(_ context.Context, _ eidetic.SearchRequest) ([]eidetic.MemoryEntry, error) {
	return nil, nil
}
func (m *mockEideticClientForAgent) GetRecent(_ context.Context, _ string, _ int) ([]eidetic.MemoryEntry, error) {
	return nil, nil
}
func (m *mockEideticClientForAgent) Health(_ context.Context) error { return nil }

func TestAgent_SetEidetic(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := &Agent{
		cfg:      cfg,
		def:      cfg.DefaultAgent(),
		sessions: sm,
		toolMap:  make(map[string]Tool),
		logger: zap.NewNop().Sugar(),
	}

	// Initially nil.
	if ag.getEidetic() != nil {
		t.Error("expected nil eidetic client initially")
	}

	// Set a client.
	client := &mockEideticClientForAgent{}
	ag.SetEidetic(client)
	if ag.getEidetic() == nil {
		t.Error("expected non-nil eidetic client after SetEidetic")
	}

	// Clear it.
	ag.SetEidetic(nil)
	if ag.getEidetic() != nil {
		t.Error("expected nil after SetEidetic(nil)")
	}
}

func TestAgent_EideticAgentID_Default(t *testing.T) {
	cfg := newTestConfig()
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := &Agent{
		cfg:      cfg,
		def:      cfg.DefaultAgent(),
		sessions: sm,
		toolMap:  make(map[string]Tool),
		logger: zap.NewNop().Sugar(),
	}

	// No override configured — should fall back to def.ID.
	id := ag.eideticAgentID()
	if id != cfg.DefaultAgent().ID {
		t.Errorf("expected agent def ID %q, got %q", cfg.DefaultAgent().ID, id)
	}
}

func TestAgent_EideticAgentID_Override(t *testing.T) {
	cfg := newTestConfig()
	cfg.Eidetic.AgentID = "custom-agent-id"
	sm, _ := session.New(testLogger(), t.TempDir(), 0)
	ag := &Agent{
		cfg:      cfg,
		def:      cfg.DefaultAgent(),
		sessions: sm,
		toolMap:  make(map[string]Tool),
		logger: zap.NewNop().Sugar(),
	}

	id := ag.eideticAgentID()
	if id != "custom-agent-id" {
		t.Errorf("expected 'custom-agent-id', got %q", id)
	}
}
