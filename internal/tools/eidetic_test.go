package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/EMSERO/gopherclaw/internal/eidetic"
)

// mockEideticClient implements eidetic.Client for testing.
type mockEideticClient struct {
	searchResults []eidetic.MemoryEntry
	searchErr     error
	appendErr     error
	appendCalled  bool
	appendReq     eidetic.AppendRequest
}

func (m *mockEideticClient) AppendMemory(_ context.Context, req eidetic.AppendRequest) error {
	m.appendCalled = true
	m.appendReq = req
	return m.appendErr
}

func (m *mockEideticClient) SearchMemory(_ context.Context, _ eidetic.SearchRequest) ([]eidetic.MemoryEntry, error) {
	return m.searchResults, m.searchErr
}

func (m *mockEideticClient) GetRecent(_ context.Context, _ string, _ int) ([]eidetic.MemoryEntry, error) {
	return nil, nil
}

func (m *mockEideticClient) Health(_ context.Context) error {
	return nil
}

func TestEideticSearchTool_Metadata(t *testing.T) {
	tool := &EideticSearchTool{Client: &mockEideticClient{}, Limit: 10, Threshold: 0.5}

	if tool.Name() != "eidetic_search" {
		t.Errorf("expected name 'eidetic_search', got %q", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("Description should not be empty")
	}

	var schema map[string]any
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatalf("Schema() is not valid JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Error("schema type should be 'object'")
	}
}

func TestEideticSearchTool_Run_InvalidJSON(t *testing.T) {
	tool := &EideticSearchTool{Client: &mockEideticClient{}, Limit: 10}
	result := tool.Run(context.Background(), `{invalid`)
	if !strings.Contains(result, "error") {
		t.Errorf("expected error for invalid JSON, got %q", result)
	}
}

func TestEideticSearchTool_Run_EmptyQuery(t *testing.T) {
	tool := &EideticSearchTool{Client: &mockEideticClient{}, Limit: 10}
	args, _ := json.Marshal(eideticSearchInput{Query: ""})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "query is required") {
		t.Errorf("expected 'query is required' error, got %q", result)
	}
}

func TestEideticSearchTool_Run_NoResults(t *testing.T) {
	tool := &EideticSearchTool{
		Client:    &mockEideticClient{searchResults: nil},
		Limit:     10,
		Threshold: 0.5,
	}
	args, _ := json.Marshal(eideticSearchInput{Query: "test query"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "No matching memories") {
		t.Errorf("expected 'No matching memories', got %q", result)
	}
}

func TestEideticSearchTool_Run_SearchError(t *testing.T) {
	tool := &EideticSearchTool{
		Client:    &mockEideticClient{searchErr: fmt.Errorf("connection refused")},
		Limit:     10,
		Threshold: 0.5,
	}
	args, _ := json.Marshal(eideticSearchInput{Query: "test query"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "error") {
		t.Errorf("expected error from search failure, got %q", result)
	}
}

func TestEideticSearchTool_Run_WithResults(t *testing.T) {
	ts := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	tool := &EideticSearchTool{
		Client: &mockEideticClient{
			searchResults: []eidetic.MemoryEntry{
				{Content: "user asked about foo", Relevance: 0.92, Timestamp: ts},
				{Content: "assistant replied bar", Relevance: 0.85, Timestamp: ts},
			},
		},
		Limit:     10,
		Threshold: 0.5,
	}
	args, _ := json.Marshal(eideticSearchInput{Query: "foo"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "user asked about foo") {
		t.Errorf("expected memory content in result, got %q", result)
	}
	if !strings.Contains(result, "0.92") {
		t.Errorf("expected relevance score in result, got %q", result)
	}
}

func TestEideticSearchTool_Run_UsesToolDefaults(t *testing.T) {
	// When input has zero limit/threshold, tool defaults should be used.
	tool := &EideticSearchTool{
		Client:    &mockEideticClient{searchResults: []eidetic.MemoryEntry{}},
		Limit:     5,
		Threshold: 0.7,
	}
	// limit=0 and threshold=0 in input → should fall back to tool defaults
	args, _ := json.Marshal(eideticSearchInput{Query: "anything", Limit: 0, Threshold: 0})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "No matching memories") {
		t.Errorf("expected 'No matching memories', got %q", result)
	}
}

// ---------------------------------------------------------------------------
// EideticAppendTool
// ---------------------------------------------------------------------------

func TestEideticAppendTool_Metadata(t *testing.T) {
	tool := &EideticAppendTool{Client: &mockEideticClient{}, AgentID: "test"}
	if tool.Name() != "eidetic_append" {
		t.Errorf("expected name 'eidetic_append', got %q", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("Description should not be empty")
	}
	var schema map[string]any
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatalf("Schema() is not valid JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Error("schema type should be 'object'")
	}
}

func TestEideticAppendTool_Run_InvalidJSON(t *testing.T) {
	tool := &EideticAppendTool{Client: &mockEideticClient{}}
	result := tool.Run(context.Background(), `{invalid`)
	if !strings.Contains(result, "error") {
		t.Errorf("expected error for invalid JSON, got %q", result)
	}
}

func TestEideticAppendTool_Run_EmptyContent(t *testing.T) {
	tool := &EideticAppendTool{Client: &mockEideticClient{}}
	args, _ := json.Marshal(eideticAppendInput{Content: ""})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "content is required") {
		t.Errorf("expected 'content is required', got %q", result)
	}
}

func TestEideticAppendTool_Run_ContentTooLong(t *testing.T) {
	tool := &EideticAppendTool{Client: &mockEideticClient{}}
	long := strings.Repeat("x", 4001)
	args, _ := json.Marshal(eideticAppendInput{Content: long})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "4000 character limit") {
		t.Errorf("expected character limit error, got %q", result)
	}
}

func TestEideticAppendTool_Run_Success(t *testing.T) {
	mock := &mockEideticClient{}
	tool := &EideticAppendTool{Client: mock, AgentID: "gopher"}
	args, _ := json.Marshal(eideticAppendInput{
		Content: "Brian's brothers are named X and Y",
		Tags:    []string{"personal", "family"},
	})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "stored successfully") {
		t.Errorf("expected success, got %q", result)
	}
	if !mock.appendCalled {
		t.Error("expected AppendMemory to be called")
	}
	if mock.appendReq.AgentID != "gopher" {
		t.Errorf("expected agentID 'gopher', got %q", mock.appendReq.AgentID)
	}
	if mock.appendReq.Content != "Brian's brothers are named X and Y" {
		t.Errorf("content mismatch: %q", mock.appendReq.Content)
	}
	if len(mock.appendReq.Tags) != 2 || mock.appendReq.Tags[0] != "personal" {
		t.Errorf("tags mismatch: %v", mock.appendReq.Tags)
	}
}

func TestEideticAppendTool_Run_AppendError(t *testing.T) {
	mock := &mockEideticClient{appendErr: fmt.Errorf("connection refused")}
	tool := &EideticAppendTool{Client: mock, AgentID: "test"}
	args, _ := json.Marshal(eideticAppendInput{Content: "some memory"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "failed to store") {
		t.Errorf("expected error message, got %q", result)
	}
}

func TestEideticAppendTool_Run_NoTags(t *testing.T) {
	mock := &mockEideticClient{}
	tool := &EideticAppendTool{Client: mock, AgentID: "test"}
	args, _ := json.Marshal(eideticAppendInput{Content: "test entry"})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "stored successfully") {
		t.Errorf("expected success, got %q", result)
	}
	if mock.appendReq.Tags != nil {
		t.Errorf("expected nil tags, got %v", mock.appendReq.Tags)
	}
}

func TestEideticAppendTool_Run_ContentAtLimit(t *testing.T) {
	mock := &mockEideticClient{}
	tool := &EideticAppendTool{Client: mock, AgentID: "test"}
	exactly4000 := strings.Repeat("a", 4000)
	args, _ := json.Marshal(eideticAppendInput{Content: exactly4000})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "stored successfully") {
		t.Errorf("expected success at exactly 4000 chars, got %q", result)
	}
}
