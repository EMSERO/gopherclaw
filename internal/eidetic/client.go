// Package eidetic implements a thin HTTP client for the Eidetic memory service.
// The Eidetic server exposes a JSON-RPC 2.0 endpoint at /mcp and a health
// check at /health.  All exported types used across packages are defined in
// types.go; this file contains only the client and its request/response shapes.
package eidetic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// Client is the interface for interacting with the Eidetic memory service.
// The nil value is not valid; use a nil *Client variable tested against the
// interface to represent "disabled" — or keep a separate bool flag.
// All methods must be safe for concurrent use.
type Client interface {
	AppendMemory(ctx context.Context, req AppendRequest) error
	SearchMemory(ctx context.Context, req SearchRequest) ([]MemoryEntry, error)
	GetRecent(ctx context.Context, agentID string, limit int) ([]MemoryEntry, error)
	Health(ctx context.Context) error
}

// HTTPClient implements Client over the Eidetic HTTP MCP API.
type HTTPClient struct {
	cfg    Config
	http   *http.Client
	nextID atomic.Int64
}

// New creates a new HTTPClient from cfg.  It does not perform a health check;
// call Health() if you want to verify connectivity at startup.
func New(cfg Config) *HTTPClient {
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &HTTPClient{
		cfg:  cfg,
		http: &http.Client{Timeout: timeout},
	}
}

// ---- internal JSON-RPC shapes -----------------------------------------------

type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int64       `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

type toolCallParams struct {
	Name      string      `json:"name"`
	Arguments interface{} `json:"arguments"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type contentResult struct {
	Content []contentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type contentItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ---- Eidetic tool argument shapes -------------------------------------------

type appendArgs struct {
	Content string    `json:"content"`
	AgentID string    `json:"agent_id"`
	Tags    []string  `json:"tags,omitempty"`
	Vector  []float32 `json:"_vector,omitempty"` // embedding vector for Meilisearch hybrid search
}

type searchArgs struct {
	Query     string    `json:"query"`
	Limit     int       `json:"limit,omitempty"`
	Threshold float64   `json:"threshold,omitempty"`
	Vector    []float32 `json:"_vector,omitempty"` // query embedding for hybrid search
	Hybrid    bool      `json:"hybrid,omitempty"`  // request hybrid ranking
}

type recentArgs struct {
	Limit   int    `json:"limit,omitempty"`
	AgentID string `json:"agent_id,omitempty"`
}

// ---- Eidetic tool result shapes ---------------------------------------------

type searchResultItem struct {
	ID         string   `json:"id"`
	Content    string   `json:"content"`
	AgentID    string   `json:"agent_id"`
	Timestamp  string   `json:"timestamp"`
	SourceFile string   `json:"source_file"`
	Tags       []string `json:"tags"`
	WordCount  int      `json:"word_count"`
	Relevance  float64  `json:"relevance"`
}

type searchMemoryResult struct {
	Results []searchResultItem `json:"results"`
	Total   int                `json:"total"`
}

type memoryEntryItem struct {
	ID         string   `json:"id"`
	Content    string   `json:"content"`
	AgentID    string   `json:"agent_id"`
	Timestamp  string   `json:"timestamp"`
	SourceFile string   `json:"source_file"`
	Tags       []string `json:"tags"`
	WordCount  int      `json:"word_count"`
}

type getRecentResult struct {
	Entries []memoryEntryItem `json:"entries"`
	Total   int               `json:"total"`
}

// ---- Core transport ---------------------------------------------------------

// call sends a JSON-RPC 2.0 tools/call and returns the text payload from the
// MCP content[0] item.
func (c *HTTPClient) call(ctx context.Context, toolName string, args interface{}) (string, error) {
	id := c.nextID.Add(1)
	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  "tools/call",
		Params:  toolCallParams{Name: toolName, Arguments: args},
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/mcp", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}

	var rpcResp rpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return "", fmt.Errorf("decode rpc: %w", err)
	}
	if rpcResp.Error != nil {
		return "", fmt.Errorf("rpc %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	var cr contentResult
	if err := json.Unmarshal(rpcResp.Result, &cr); err != nil {
		return "", fmt.Errorf("decode content: %w", err)
	}
	if cr.IsError {
		text := ""
		if len(cr.Content) > 0 {
			text = cr.Content[0].Text
		}
		return "", fmt.Errorf("tool error: %s", text)
	}
	if len(cr.Content) == 0 {
		return "", fmt.Errorf("empty content")
	}
	return cr.Content[0].Text, nil
}

// ---- Client methods ---------------------------------------------------------

// AppendMemory writes a memory entry to Eidetic.
func (c *HTTPClient) AppendMemory(ctx context.Context, req AppendRequest) error {
	tags := req.Tags
	if tags == nil {
		tags = []string{}
	}
	_, err := c.call(ctx, "append_memory", appendArgs{
		Content: req.Content,
		AgentID: req.AgentID,
		Tags:    tags,
		Vector:  req.Vector,
	})
	return err
}

// SearchMemory performs a semantic search over stored memories.
func (c *HTTPClient) SearchMemory(ctx context.Context, req SearchRequest) ([]MemoryEntry, error) {
	text, err := c.call(ctx, "search_memory", searchArgs(req))
	if err != nil {
		return nil, err
	}

	var sr searchMemoryResult
	if err := json.Unmarshal([]byte(text), &sr); err != nil {
		return nil, fmt.Errorf("decode search result: %w", err)
	}

	entries := make([]MemoryEntry, len(sr.Results))
	for i, r := range sr.Results {
		ts, _ := time.Parse(time.RFC3339, r.Timestamp)
		entries[i] = MemoryEntry{
			ID:         r.ID,
			Content:    r.Content,
			AgentID:    r.AgentID,
			Timestamp:  ts,
			SourceFile: r.SourceFile,
			Tags:       r.Tags,
			WordCount:  r.WordCount,
			Relevance:  r.Relevance,
		}
	}
	return entries, nil
}

// GetRecent retrieves the N most recent memory entries, optionally filtered by
// agentID (empty = all agents).
func (c *HTTPClient) GetRecent(ctx context.Context, agentID string, limit int) ([]MemoryEntry, error) {
	text, err := c.call(ctx, "get_recent", recentArgs{Limit: limit, AgentID: agentID})
	if err != nil {
		return nil, err
	}

	var rr getRecentResult
	if err := json.Unmarshal([]byte(text), &rr); err != nil {
		return nil, fmt.Errorf("decode recent result: %w", err)
	}

	entries := make([]MemoryEntry, len(rr.Entries))
	for i, e := range rr.Entries {
		ts, _ := time.Parse(time.RFC3339, e.Timestamp)
		entries[i] = MemoryEntry{
			ID:         e.ID,
			Content:    e.Content,
			AgentID:    e.AgentID,
			Timestamp:  ts,
			SourceFile: e.SourceFile,
			Tags:       e.Tags,
			WordCount:  e.WordCount,
		}
	}
	return entries, nil
}

// Health calls GET /health to verify the service is reachable.
func (c *HTTPClient) Health(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.BaseURL+"/health", nil)
	if err != nil {
		return err
	}
	if c.cfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	return nil
}
