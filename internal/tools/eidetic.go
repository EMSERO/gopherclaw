package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/EMSERO/gopherclaw/internal/eidetic"
	"github.com/EMSERO/gopherclaw/internal/embeddings"
)

// EideticSearchTool exposes semantic memory search as an agent tool.
// It is registered only when the Eidetic integration is enabled.
type EideticSearchTool struct {
	Client     eidetic.Client
	Embeddings *embeddings.Client // optional: enables hybrid search
	Limit      int                // default search limit
	Threshold  float64            // default cosine similarity threshold
}

type eideticSearchInput struct {
	Query     string  `json:"query"`
	Limit     int     `json:"limit,omitempty"`
	Threshold float64 `json:"threshold,omitempty"`
}

func (t *EideticSearchTool) Name() string { return "eidetic_search" }

func (t *EideticSearchTool) Description() string {
	return "Search the semantic memory store for past conversations and decisions. " +
		"Returns the most relevant memory entries matching the query."
}

func (t *EideticSearchTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {
				"type": "string",
				"description": "Natural language query to search memories"
			},
			"limit": {
				"type": "integer",
				"description": "Maximum number of results to return (optional)"
			},
			"threshold": {
				"type": "number",
				"description": "Minimum cosine similarity threshold 0–1 (optional)"
			}
		},
		"required": ["query"]
	}`)
}

func (t *EideticSearchTool) Run(ctx context.Context, argsJSON string) string {
	var in eideticSearchInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err)
	}
	if in.Query == "" {
		return "error: query is required"
	}

	limit := in.Limit
	if limit <= 0 {
		limit = t.Limit
	}
	threshold := in.Threshold
	if threshold <= 0 {
		threshold = t.Threshold
	}

	// Generate query embedding for hybrid search if configured.
	var queryVec []float32
	if t.Embeddings != nil {
		if v, err := t.Embeddings.Embed(ctx, in.Query); err == nil {
			queryVec = v
		}
	}

	// Request extra results for MMR diversity filtering.
	fetchLimit := limit * 2
	if fetchLimit < 10 {
		fetchLimit = 10
	}

	entries, err := t.Client.SearchMemory(ctx, eidetic.SearchRequest{
		Query:     in.Query,
		Limit:     fetchLimit,
		Threshold: threshold,
		Vector:    queryVec,
		Hybrid:    queryVec != nil,
	})
	if err != nil {
		return fmt.Sprintf("error: search failed: %v", err)
	}
	if len(entries) == 0 {
		return "No matching memories found."
	}

	// Apply MMR for diversity, then cap to requested limit.
	entries = eidetic.MMR(entries, 0.7, limit)

	var sb strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&sb, "[%s] (relevance: %.2f) %s\n",
			e.Timestamp.Format("2006-01-02 15:04"),
			e.Relevance,
			e.Content,
		)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// EideticAppendTool exposes semantic memory storage as an agent tool.
// It is registered only when the Eidetic integration is enabled.
type EideticAppendTool struct {
	Client     eidetic.Client
	Embeddings *embeddings.Client // optional: generates embedding vector on store
	AgentID    string             // default agent_id for writes
}

type eideticAppendInput struct {
	Content string   `json:"content"`
	Tags    []string `json:"tags,omitempty"`
}

func (t *EideticAppendTool) Name() string { return "eidetic_append" }

func (t *EideticAppendTool) Description() string {
	return "Store a memory entry in the semantic memory store. " +
		"Use this to remember important facts, decisions, user preferences, and conversation highlights " +
		"so they can be recalled later via eidetic_search. Each call stores one entry; " +
		"for large amounts of information, make multiple calls with focused, self-contained entries."
}

func (t *EideticAppendTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"content": {
				"type": "string",
				"description": "The memory content to store. Should be a self-contained, meaningful entry (e.g. a fact, decision, or summary). Max 4000 characters."
			},
			"tags": {
				"type": "array",
				"items": {"type": "string"},
				"description": "Optional tags for categorizing this memory (e.g. [\"personal\", \"preference\"])"
			}
		},
		"required": ["content"]
	}`)
}

func (t *EideticAppendTool) Run(ctx context.Context, argsJSON string) string {
	var in eideticAppendInput
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return fmt.Sprintf("error: invalid arguments: %v", err)
	}
	if in.Content == "" {
		return "error: content is required"
	}
	if len(in.Content) > 4000 {
		return "error: content exceeds 4000 character limit; split into smaller entries"
	}

	// Generate embedding vector if configured.
	var vec []float32
	if t.Embeddings != nil {
		if v, err := t.Embeddings.Embed(ctx, in.Content); err == nil {
			vec = v
		}
	}

	err := t.Client.AppendMemory(ctx, eidetic.AppendRequest{
		Content: in.Content,
		AgentID: t.AgentID,
		Tags:    in.Tags,
		Vector:  vec,
	})
	if err != nil {
		return fmt.Sprintf("error: failed to store memory: %v", err)
	}
	return "Memory stored successfully."
}
