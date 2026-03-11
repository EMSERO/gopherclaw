# Architecture — Eidetic Integration

## Component Overview

```
┌─────────────────────────────────────────────────────────┐
│           Agent.Chat() / ChatStream() / ChatLight()      │
│                                                          │
│  1. buildSystemPrompt()                                  │
│       └── eidetic.GetRecent()  ──────────────────────┐   │
│             (≤2s timeout, read-only)                  │   │
│                                                       │   │
│  2. loop() / loopStream()                             │   │
│       └── model + tools (unchanged)                   │   │
│                                                       │   │
│  3. return response to caller                         │   │
│       └── go eidetic.AppendMemory()  ─────────────┐   │   │
│             (non-blocking goroutine)               │   │   │
└────────────────────────────────────────────────────│───│───┘
                                                     │   │
                    ┌────────────────────────────────┘   │
                    │       ┌───────────────────────────┘
                    ▼       ▼
           ┌─────────────────────┐
           │  internal/eidetic   │
           │  (HTTP MCP client)  │
           │                     │
           │  AppendMemory()     │
           │  SearchMemory()     │
           │  GetRecent()        │
           └──────────┬──────────┘
                      │  POST /mcp (Bearer token)
                      ▼
           ┌─────────────────────┐
           │   Eidetic Server    │
           │   localhost:7700    │
           │                     │
           │  Postgres (entries) │
           │  Ollama (embeddings)│
           │  flat files (notes) │
           └─────────────────────┘

           ┌─────────────────────┐
           │  eidetic_search     │  ← agent tool (registered when enabled)
           │  Tool               │
           │  Run() calls        │
           │  SearchMemory()     │
           └─────────────────────┘
```

---

## Data Flow

### Session Start (System Prompt Construction)

```
Agent.buildSystemPrompt()
  │
  ├── [existing] load sysPromptStatic (identity, skills, workspace docs)
  ├── [existing] append current date/time
  ├── [existing] loadMemoryCached() → ## Memory (MEMORY.md)
  └── [new]  eideticClient.GetRecent(ctx2s, recentLimit)
               → on success: append ## Recent Memory section
               → on error/timeout: skip silently
```

### Per-Turn Append (Post-Response)

Applies to `Chat()`, `ChatStream()`, and `ChatLight()` — all session types.

```
Agent.Chat() / ChatStream() / ChatLight()
  │
  ├── ... loop() completes, resp returned to caller ...
  │
  └── go func() {
        content = "[User]: {userText}\n[Assistant]: {resp.Text}"
        tags    = ["session:{sessionKey}", "agent:{agentID}"]
        err = eideticClient.AppendMemory(ctx5s, content, tags)
        if err != nil { log.Debugf("eidetic: append failed: %v", err) }
      }()
```

### Explicit Search (Agent Tool Call)

```
Agent receives tool call: eidetic_search{"query": "what did we decide about auth?"}
  │
  └── EideticSearchTool.Run(ctx, argsJSON)
        │
        └── eideticClient.SearchMemory(ctx, query, limit, threshold)
              → format results → return string to model
```

---

## Package Structure

```
internal/
  eidetic/
    client.go       — Client struct, Config, AppendMemory, SearchMemory, GetRecent
    types.go        — MemoryEntry, AppendResult
    client_test.go  — tests against httptest.Server mock

  tools/
    memory.go       — [existing] MemoryAppendTool, MemoryGetTool (unchanged)
    eidetic.go      — [new] EideticSearchTool

  agent/
    agent.go        — [modified] buildSystemPrompt, Chat, ChatStream, ChatLight inject Eidetic
    agent_test.go   — [extended] Eidetic-related test cases

  config/
    config.go       — [modified] EideticConfig struct added to Root
```

---

## Config Schema

New field added to `config.json` root:

```json
{
  "eidetic": {
    "enabled": true,
    "baseURL": "http://localhost:7700",
    "apiKey": "eidetic-dev-key",
    "agentID": "alfred",
    "recentLimit": 20,
    "searchLimit": 10,
    "searchThreshold": 0.5,
    "timeoutSeconds": 5
  }
}
```

Go struct added to `internal/config/config.go`:

```go
type EideticConfig struct {
    Enabled         bool    `json:"enabled"`
    BaseURL         string  `json:"baseURL"`
    APIKey          string  `json:"apiKey"`
    AgentID         string  `json:"agentID"`
    RecentLimit     int     `json:"recentLimit"`
    SearchLimit     int     `json:"searchLimit"`
    SearchThreshold float64 `json:"searchThreshold"`
    TimeoutSeconds  int     `json:"timeoutSeconds"`
}
```

Defaults applied in `applyDefaults()`:
- `BaseURL` → `"http://localhost:7700"`
- `RecentLimit` → `20`
- `SearchLimit` → `10`
- `SearchThreshold` → `0.5`
- `TimeoutSeconds` → `5`
- `AgentID` → falls back to `cfg.DefaultAgent().ID` at agent construction time if empty

---

## Eidetic Client Design

```go
// internal/eidetic/client.go

type Config struct {
    BaseURL         string
    APIKey          string
    AgentID         string
    TimeoutSeconds  int
}

type Client struct {
    cfg    Config
    http   *http.Client
}

func New(cfg Config) *Client

func (c *Client) AppendMemory(ctx context.Context, content string, tags []string) (string, error)
func (c *Client) SearchMemory(ctx context.Context, query string, limit int, threshold float64) ([]MemoryEntry, error)
func (c *Client) GetRecent(ctx context.Context, limit int) ([]MemoryEntry, error)
func (c *Client) Health(ctx context.Context) error
```

All three tool methods share a private `callMCP(ctx, toolName, args)` helper that:
1. Builds the JSON-RPC 2.0 envelope
2. POSTs to `{baseURL}/mcp` with `Authorization: Bearer {apiKey}`
3. Parses the response envelope
4. Extracts `result.content[0].text` and unmarshals the inner JSON

```go
// MCP request envelope
{
  "jsonrpc": "2.0",
  "method": "tools/call",
  "id": 1,
  "params": {
    "name": "{toolName}",
    "arguments": { ... }
  }
}

// MCP response envelope
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "content": [{"type": "text", "text": "{inner JSON string}"}]
  }
}
```

---

## Agent Modifications

### `agent.go` — New field

```go
type Agent struct {
    // ... existing fields ...
    eidetic *eidetic.Client // nil if disabled
}
```

Set in `New()` from config:

```go
if cfg.Eidetic.Enabled {
    agentID := cfg.Eidetic.AgentID
    if agentID == "" {
        agentID = def.ID
    }
    a.eidetic = eidetic.New(eidetic.Config{
        BaseURL:        cfg.Eidetic.BaseURL,
        APIKey:         cfg.Eidetic.APIKey,
        AgentID:        agentID,
        TimeoutSeconds: cfg.Eidetic.TimeoutSeconds,
    })
}
```

### `buildSystemPrompt()` — Eidetic injection point

```go
func (a *Agent) buildSystemPrompt() string {
    var sb strings.Builder
    sb.WriteString(a.sysPromptStatic)

    // ... [existing] date/time ...
    // ... [existing] MEMORY.md ...

    // [new] Eidetic recent memory
    if a.eidetic != nil {
        ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
        defer cancel()
        entries, err := a.eidetic.GetRecent(ctx, a.getCfg().Eidetic.RecentLimit)
        if err == nil && len(entries) > 0 {
            sb.WriteString("## Recent Memory\n\n")
            for _, e := range entries {
                fmt.Fprintf(&sb, "- [%s] %s\n", e.Timestamp.Format("2006-01-02 15:04"), e.Content)
            }
            sb.WriteString("\n")
        }
    }

    return sb.String()
}
```

### `Chat()` / `ChatStream()` / `ChatLight()` — Post-turn append

The same post-turn append block is applied in all three methods:

```go
// After: if err := a.sessions.AppendMessages(...); ...
// Before: return resp, nil

if a.eidetic != nil && resp.Text != "" {
    content := fmt.Sprintf("[User]: %s\n[Assistant]: %s", userText, resp.Text)
    tags := []string{
        "session:" + sessionKey,
        "agent:" + a.def.ID,
    }
    go func() {
        ctx, cancel := context.WithTimeout(context.Background(),
            time.Duration(a.getCfg().Eidetic.TimeoutSeconds)*time.Second)
        defer cancel()
        if _, err := a.eidetic.AppendMemory(ctx, content, tags); err != nil {
            log.Debugf("eidetic: append failed: %v", err)
        }
    }()
}
```

---

## EideticSearchTool

```go
// internal/tools/eidetic.go

type EideticSearchTool struct {
    Client           *eidetic.Client
    DefaultLimit     int
    DefaultThreshold float64
}

func (t *EideticSearchTool) Name() string        { return "eidetic_search" }
func (t *EideticSearchTool) Description() string { ... }
func (t *EideticSearchTool) Schema() json.RawMessage { ... }
func (t *EideticSearchTool) Run(ctx context.Context, argsJSON string) string { ... }
```

Output format per result:
```
[2026-03-03 14:22] (relevance: 0.73) [User]: what model are we using...
```

---

## Tool Registration

In `agent.go DefaultTools()`:

```go
// After memory tools block:
if cfg.Eidetic.Enabled && eideticClient != nil {
    toolList = append(toolList, &tools.EideticSearchTool{
        Client:           eideticClient,
        DefaultLimit:     cfg.Eidetic.SearchLimit,
        DefaultThreshold: cfg.Eidetic.SearchThreshold,
    })
}
```

`DefaultTools()` signature gains an `eideticClient *eidetic.Client` parameter (nil if disabled).

---

## Startup Health Check

In `initialize/init.go` or `gateway/server.go` startup sequence:

```go
if cfg.Eidetic.Enabled {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    if err := eideticClient.Health(ctx); err != nil {
        log.Warnf("eidetic: health check failed (%v), disabling", err)
        eideticClient = nil  // disables all integration
    } else {
        log.Infof("eidetic: connected at %s", cfg.Eidetic.BaseURL)
    }
}
```

---

## Key Technical Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Client package isolation | `internal/eidetic` with no agent/config imports | Keeps client independently testable; avoids import cycles |
| Append timing | Post-turn goroutine | Zero added latency to response path |
| GetRecent timeout cap | 2 seconds hard cap | Prompt construction must not significantly delay first model call |
| Eidetic disabled = nil client | nil pointer check at every call site | Simple, idiomatic Go; no separate "disabled" flag to track |
| agentID fallback | `def.ID` at agent construction | Consistent with existing workspace layout; no extra config required for default case |
| All session types appended | Chat, ChatStream, ChatLight all write to Eidetic | Total recall — heartbeat and cron activity is part of the agent's operational history |
| Error level for append failures | DEBUG | Append failures are expected when Eidetic is down; INFO would spam logs |
