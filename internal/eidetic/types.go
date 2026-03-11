package eidetic

import "time"

// Config holds connection settings for the Eidetic memory service.
type Config struct {
	BaseURL         string  // e.g. "http://localhost:7700"
	APIKey          string
	AgentID         string  // default agent_id for writes; falls back to agent def ID
	RecentLimit     int     // number of recent entries to inject (default 20)
	SearchLimit     int     // max results for search (default 10)
	SearchThreshold float64 // cosine similarity threshold (default 0.5)
	TimeoutSeconds  int     // per-request timeout (default 5)
}

// MemoryEntry is a unified memory entry returned by GetRecent and SearchMemory.
type MemoryEntry struct {
	ID         string
	Content    string
	AgentID    string
	Timestamp  time.Time
	SourceFile string
	Tags       []string
	WordCount  int
	Relevance  float64 // only populated by SearchMemory
}

// AppendRequest is the payload for AppendMemory.
type AppendRequest struct {
	Content string
	AgentID string
	Tags    []string
	Vector  []float32 // optional embedding vector for hybrid search
}

// SearchRequest is the payload for SearchMemory.
type SearchRequest struct {
	Query     string
	Limit     int
	Threshold float64
	Vector    []float32 // optional query embedding for hybrid search
	Hybrid    bool      // if true, request hybrid (keyword + vector) ranking
}
