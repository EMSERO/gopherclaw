package eidetic_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/EMSERO/gopherclaw/internal/eidetic"
)

// mcpResponse builds a successful JSON-RPC 2.0 response wrapping v as the
// MCP content[0].text value.
func mcpResponse(id int64, v interface{}) interface{} {
	text, _ := json.Marshal(v)
	return map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]interface{}{
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": string(text)},
			},
		},
	}
}

func newTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, eidetic.Client) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := eidetic.New(eidetic.Config{
		BaseURL:        srv.URL,
		APIKey:         "test-key",
		TimeoutSeconds: 5,
	})
	return srv, client
}

func TestHealth(t *testing.T) {
	srv, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing/wrong auth header: %s", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusOK)
	})
	_ = srv

	if err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health() error: %v", err)
	}
}

func TestHealth_ServerError(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	if err := client.Health(context.Background()); err == nil {
		t.Fatal("expected error from 503, got nil")
	}
}

func TestAppendMemory(t *testing.T) {
	var gotBody map[string]interface{}

	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		resp := mcpResponse(1, map[string]interface{}{
			"id":          "abc-123",
			"source_file": "2026-01-01.md",
			"embedded":    false,
		})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	err := client.AppendMemory(context.Background(), eidetic.AppendRequest{
		Content: "[User]: hello\n[Assistant]: hi",
		AgentID: "main",
		Tags:    []string{"session:abc"},
	})
	if err != nil {
		t.Fatalf("AppendMemory() error: %v", err)
	}

	params, _ := gotBody["params"].(map[string]interface{})
	if params["name"] != "append_memory" {
		t.Errorf("expected tool name append_memory, got %v", params["name"])
	}
	args, _ := params["arguments"].(map[string]interface{})
	if args["content"] != "[User]: hello\n[Assistant]: hi" {
		t.Errorf("unexpected content: %v", args["content"])
	}
	if args["agent_id"] != "main" {
		t.Errorf("unexpected agent_id: %v", args["agent_id"])
	}
}

func TestGetRecent(t *testing.T) {
	ts := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	serverResp := map[string]interface{}{
		"entries": []interface{}{
			map[string]interface{}{
				"id":          "entry-1",
				"content":     "some memory",
				"agent_id":    "main",
				"timestamp":   ts.Format(time.RFC3339),
				"source_file": "2026-01-15.md",
				"tags":        []interface{}{"session:x"},
				"word_count":  2,
			},
		},
		"total": 1,
	}

	var gotArgs map[string]interface{}
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		params, _ := body["params"].(map[string]interface{})
		gotArgs, _ = params["arguments"].(map[string]interface{})

		resp := mcpResponse(1, serverResp)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	entries, err := client.GetRecent(context.Background(), "main", 5)
	if err != nil {
		t.Fatalf("GetRecent() error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.ID != "entry-1" {
		t.Errorf("unexpected ID: %s", e.ID)
	}
	if e.Content != "some memory" {
		t.Errorf("unexpected Content: %s", e.Content)
	}
	if !e.Timestamp.Equal(ts) {
		t.Errorf("unexpected Timestamp: %v", e.Timestamp)
	}

	if gotArgs["agent_id"] != "main" {
		t.Errorf("expected agent_id=main in request, got %v", gotArgs["agent_id"])
	}
	if limit, _ := gotArgs["limit"].(float64); int(limit) != 5 {
		t.Errorf("expected limit=5, got %v", gotArgs["limit"])
	}
}

func TestSearchMemory(t *testing.T) {
	ts := time.Date(2026, 2, 1, 9, 0, 0, 0, time.UTC)
	serverResp := map[string]interface{}{
		"results": []interface{}{
			map[string]interface{}{
				"id":          "res-1",
				"content":     "relevant content",
				"agent_id":    "main",
				"timestamp":   ts.Format(time.RFC3339),
				"source_file": "2026-02-01.md",
				"tags":        []interface{}{},
				"word_count":  2,
				"relevance":   0.87,
			},
		},
		"total":              1,
		"query_embedding_ms": 12,
		"search_ms":          5,
	}

	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		resp := mcpResponse(1, serverResp)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	entries, err := client.SearchMemory(context.Background(), eidetic.SearchRequest{
		Query:     "some query",
		Limit:     10,
		Threshold: 0.7,
	})
	if err != nil {
		t.Fatalf("SearchMemory() error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Relevance != 0.87 {
		t.Errorf("unexpected Relevance: %f", entries[0].Relevance)
	}
}

func TestToolError(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "agent_id must not be empty"},
				},
				"isError": true,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	err := client.AppendMemory(context.Background(), eidetic.AppendRequest{
		Content: "test",
		AgentID: "",
	})
	if err == nil {
		t.Fatal("expected error for isError:true, got nil")
	}
}

func TestRPCError(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"error": map[string]interface{}{
				"code":    -32602,
				"message": "invalid params",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	_, err := client.GetRecent(context.Background(), "", 10)
	if err == nil {
		t.Fatal("expected error for rpc error, got nil")
	}
}

func TestNew_DefaultTimeout(t *testing.T) {
	// TimeoutSeconds <= 0 should default to 5 seconds.
	client := eidetic.New(eidetic.Config{
		BaseURL:        "http://localhost:0",
		TimeoutSeconds: 0,
	})
	if client == nil {
		t.Fatal("expected non-nil client with zero TimeoutSeconds")
	}
}
