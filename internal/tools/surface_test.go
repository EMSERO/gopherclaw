package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestSurfaceCreateToolName(t *testing.T) {
	tool := &SurfaceCreateTool{}
	if tool.Name() != "surface_create" {
		t.Errorf("Name() = %q, want %q", tool.Name(), "surface_create")
	}
}

func TestSurfaceCreateToolSchema(t *testing.T) {
	tool := &SurfaceCreateTool{}
	var m map[string]any
	if err := json.Unmarshal(tool.Schema(), &m); err != nil {
		t.Fatalf("invalid JSON schema: %v", err)
	}
	props, ok := m["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties in schema")
	}
	for _, required := range []string{"content", "surface_type", "priority"} {
		if _, ok := props[required]; !ok {
			t.Errorf("missing required property %q", required)
		}
	}
}

func TestSurfaceCreateToolNilStore(t *testing.T) {
	tool := &SurfaceCreateTool{Logger: zap.NewNop().Sugar()}
	args, _ := json.Marshal(surfaceCreateInput{
		Content:     "test",
		SurfaceType: "insight",
		Priority:    3,
	})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "not enabled") {
		t.Errorf("expected 'not enabled' error, got %q", result)
	}
}

func TestSurfaceCreateToolInvalidJSON(t *testing.T) {
	tool := &SurfaceCreateTool{Logger: zap.NewNop().Sugar()}
	result := tool.Run(context.Background(), "not json")
	if !strings.Contains(result, "invalid input") {
		t.Errorf("expected 'invalid input' error, got %q", result)
	}
}

func TestSurfaceCreateToolEmptyContent(t *testing.T) {
	tool := &SurfaceCreateTool{Logger: zap.NewNop().Sugar()}
	args, _ := json.Marshal(surfaceCreateInput{
		Content:     "",
		SurfaceType: "insight",
		Priority:    3,
	})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "content is required") {
		t.Errorf("expected 'content is required' error, got %q", result)
	}
}

func TestSurfaceCreateToolInvalidType(t *testing.T) {
	tool := &SurfaceCreateTool{Logger: zap.NewNop().Sugar()}
	args, _ := json.Marshal(surfaceCreateInput{
		Content:     "test",
		SurfaceType: "bogus",
		Priority:    3,
	})
	result := tool.Run(context.Background(), string(args))
	if !strings.Contains(result, "invalid surface_type") {
		t.Errorf("expected 'invalid surface_type' error, got %q", result)
	}
}

func TestSurfaceCreateToolPriorityClamping(t *testing.T) {
	// These test the clamping logic without a store (will hit nil store error after clamping)
	// We're just verifying the validation doesn't reject clamped values
	tool := &SurfaceCreateTool{Logger: zap.NewNop().Sugar()}

	cases := []struct {
		name string
		pri  int
	}{
		{"zero", 0},
		{"negative", -5},
		{"too high", 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, _ := json.Marshal(surfaceCreateInput{
				Content:     "test",
				SurfaceType: "insight",
				Priority:    tc.pri,
			})
			result := tool.Run(context.Background(), string(args))
			// Should fail on nil store, not on validation
			if !strings.Contains(result, "not enabled") {
				t.Errorf("expected nil store error after clamping, got %q", result)
			}
		})
	}
}

func TestSurfaceCreateToolValidTypes(t *testing.T) {
	tool := &SurfaceCreateTool{Logger: zap.NewNop().Sugar()}
	validTypes := []string{"insight", "question", "warning", "reminder", "connection"}
	for _, st := range validTypes {
		t.Run(st, func(t *testing.T) {
			args, _ := json.Marshal(surfaceCreateInput{
				Content:     "test",
				SurfaceType: st,
				Priority:    3,
			})
			result := tool.Run(context.Background(), string(args))
			// Should pass type validation, fail on nil store
			if strings.Contains(result, "invalid surface_type") {
				t.Errorf("type %q should be valid, got rejection: %q", st, result)
			}
		})
	}
}

func TestSurfaceCreateToolTriggerAtParsing(t *testing.T) {
	// Valid trigger_at should not cause an error (will fail on nil store)
	tool := &SurfaceCreateTool{Logger: zap.NewNop().Sugar()}
	args, _ := json.Marshal(surfaceCreateInput{
		Content:     "remind me",
		SurfaceType: "reminder",
		Priority:    2,
		TriggerAt:   "2026-03-20T09:00:00Z",
	})
	result := tool.Run(context.Background(), string(args))
	if strings.Contains(result, "invalid") && !strings.Contains(result, "not enabled") {
		t.Errorf("valid trigger_at should not cause validation error, got %q", result)
	}
}
