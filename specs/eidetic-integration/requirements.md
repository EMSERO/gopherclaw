# Requirements — Eidetic Integration

## Configuration

### REQ-200 — Optional Eidetic Config Block
**Priority:** Must  
**Description:** A new `eidetic` block in `config.json` controls the integration. If the block is absent or `enabled` is false, all Eidetic behavior is skipped silently. No error, no warning, no change in behavior.  
**Acceptance criteria:** GopherClaw starts and operates normally with no `eidetic` key in config. GopherClaw starts and operates normally with `eidetic.enabled: false`.

### REQ-201 — Config Fields
**Priority:** Must  
**Description:** The `eidetic` config block must support the following fields:
- `enabled` (bool) — master switch; default false
- `baseURL` (string) — Eidetic server URL; default `"http://localhost:7700"`
- `apiKey` (string) — Bearer token for Eidetic API auth
- `agentID` (string) — namespace for memory entries; defaults to the GopherClaw agent's ID
- `recentLimit` (int) — number of recent entries injected into system prompt; default 20
- `searchLimit` (int) — max results returned by `eidetic_search` tool; default 10
- `searchThreshold` (float) — minimum relevance score for search results; default 0.5
- `timeoutSeconds` (int) — HTTP client timeout for all Eidetic calls; default 5

**Acceptance criteria:** Each field applies as documented. Omitted fields use their defaults.

### REQ-202 — Health Check on Startup
**Priority:** Should  
**Description:** When Eidetic is enabled, GopherClaw calls `GET /health` at startup. If the call fails or returns a non-ok status, a warning is logged and Eidetic is disabled for the session. No fatal error.  
**Acceptance criteria:** Startup with Eidetic unreachable logs `WARN eidetic: health check failed, disabling` and continues normally.

---

## Client

### REQ-210 — Internal Eidetic Client Package
**Priority:** Must  
**Description:** A new package `internal/eidetic` implements a thin Go HTTP client wrapping the three MCP tool calls. The client must not be coupled to the agent or config packages — it takes a plain `Config` struct.  
**Acceptance criteria:** `internal/eidetic` has no import of `internal/agent`, `internal/config`, or `internal/tools`. It can be tested in isolation with a mock HTTP server.

### REQ-211 — Client Methods
**Priority:** Must  
**Description:** The client exposes three methods matching the Eidetic MCP tools:
- `AppendMemory(ctx, content, tags []string) (entryID string, err error)`
- `SearchMemory(ctx, query string, limit int, threshold float64) ([]MemoryEntry, error)`
- `GetRecent(ctx, limit int) ([]MemoryEntry, error)`

**Acceptance criteria:** Each method constructs the correct MCP JSON-RPC payload, sends it to `POST /mcp`, parses the response, and returns typed results. Errors from the HTTP layer or Eidetic API are propagated as Go errors.

### REQ-212 — MemoryEntry Type
**Priority:** Must  
**Description:** The client defines a `MemoryEntry` struct with fields: `ID`, `Content`, `AgentID`, `Timestamp`, `Tags`, `WordCount`. For search results, `Relevance` (float64) is also populated.  
**Acceptance criteria:** All fields map correctly to the Eidetic API response schema.

### REQ-213 — Graceful Error Handling
**Priority:** Must  
**Description:** All client methods treat network errors, timeouts, and non-2xx responses as non-fatal. The caller receives an error; it is the caller's responsibility to fall back gracefully. The client does not panic.  
**Acceptance criteria:** Client called against a stopped server returns an error within `timeoutSeconds`. No panic.

---

## System Prompt Injection

### REQ-220 — get_recent Injected into System Prompt
**Priority:** Must  
**Description:** When Eidetic is enabled, `buildSystemPrompt()` calls `GetRecent(ctx, recentLimit)` and injects the results as a `## Recent Memory` section, placed after `## Memory` (MEMORY.md) and before the closing of the system prompt. If `GetRecent` fails or returns no results, the section is omitted silently.  
**Acceptance criteria:** System prompt contains `## Recent Memory` section with up to `recentLimit` entries when Eidetic is reachable. Section is absent when Eidetic is unreachable or returns no entries.

### REQ-221 — Recent Memory Format
**Priority:** Must  
**Description:** Each entry in the `## Recent Memory` section is formatted as:
```
- [{timestamp}] {content}
```
Entries are ordered most-recent first. Content is included verbatim (not truncated in the prompt — entries are kept short by the append logic in REQ-230).  
**Acceptance criteria:** Format matches spec. Order is descending by timestamp.

### REQ-222 — Prompt Injection is Read-Only
**Priority:** Must  
**Description:** `buildSystemPrompt()` must not call `AppendMemory`. Reads only.  
**Acceptance criteria:** No Eidetic write calls originate from prompt construction. Verified by code review and test.

### REQ-223 — Prompt Injection Timeout
**Priority:** Must  
**Description:** The `GetRecent` call in `buildSystemPrompt()` uses a short deadline (max `timeoutSeconds`, but capped at 2 seconds so it never significantly delays the first model call). If it exceeds the deadline, it returns empty silently.  
**Acceptance criteria:** With Eidetic configured but slow (simulated 3s delay), prompt construction completes within 2.5 seconds and omits the Recent Memory section.

---

## Post-Turn Append

### REQ-230 — Append Every Exchange, All Session Types
**Priority:** Must  
**Description:** After each completed agent turn in `Chat`, `ChatStream`, and `ChatLight`, GopherClaw fires a goroutine that calls `AppendMemory` with the exchange content. All session types are recorded — including heartbeat/cron sessions (`ChatLight`). This goroutine is non-blocking — the response is already returned to the caller before the write begins.  
**Acceptance criteria:** `AppendMemory` is called after every turn where Eidetic is enabled, regardless of session type. The caller receives the response before the append goroutine starts.

### REQ-231 — Exchange Content Format
**Priority:** Must  
**Description:** The content appended to Eidetic per turn is:
```
[User]: {userText}
[Assistant]: {assistantResponse}
```
No tool call details, no session keys, no metadata — just the human-visible exchange.  
**Acceptance criteria:** Appended content matches format. Tool call intermediate steps are not included.

### REQ-232 — Tags on Append
**Priority:** Should  
**Description:** Each appended entry includes tags: `["session:{sessionKey}", "agent:{agentID}"]`. This enables future filtering by session or agent without a full semantic search.  
**Acceptance criteria:** Tags appear in the Eidetic entry for the appended exchange.

### REQ-233 — Append Errors are Silent
**Priority:** Must  
**Description:** If `AppendMemory` fails (network error, Eidetic down, etc.), the error is logged at DEBUG level and discarded. It must not surface to the user or affect the session.  
**Acceptance criteria:** With Eidetic unreachable after startup, agent turns complete normally with no user-visible error. Log shows DEBUG-level append failure.

### REQ-234 — No Append on Empty Response
**Priority:** Must  
**Description:** If the assistant response is empty (e.g. tool-only turn with no text output, or an error response), `AppendMemory` is not called.  
**Acceptance criteria:** Empty assistant response produces no Eidetic write call.

---

## eidetic_search Tool

### REQ-240 — eidetic_search Tool Registered
**Priority:** Must  
**Description:** When Eidetic is enabled, a new `eidetic_search` tool is added to the agent's tool registry alongside the existing `memory_append` and `memory_get` tools. If Eidetic is disabled, the tool is not registered.  
**Acceptance criteria:** `eidetic_search` appears in tool list when `eidetic.enabled: true`. Tool is absent when disabled.

### REQ-241 — eidetic_search Tool Schema
**Priority:** Must  
**Description:** The tool schema:
```json
{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "What to search for in past memory"},
    "limit": {"type": "integer", "description": "Max results (default: searchLimit from config)"},
    "threshold": {"type": "float", "description": "Min relevance score 0-1 (default: searchThreshold from config)"}
  },
  "required": ["query"]
}
```
**Acceptance criteria:** Schema matches. `limit` and `threshold` are optional and fall back to config defaults.

### REQ-242 — eidetic_search Tool Description
**Priority:** Must  
**Description:** Tool description: `"Search past memory semantically. Use when asked about something that may have been discussed before, or to recall decisions, context, or facts from previous sessions."`  
**Acceptance criteria:** Description appears in tool definition. Guides the model to use it for recall, not for current-session context.

### REQ-243 — eidetic_search Tool Output Format
**Priority:** Must  
**Description:** Results are returned as a newline-separated list:
```
[{timestamp}] (relevance: {score:.2f}) {content}
```
If no results meet the threshold, returns `"(no matching memories found)"`.  
**Acceptance criteria:** Output format matches spec. Empty result returns the no-match string.

---

## Existing Memory System

### REQ-250 — MEMORY.md Unchanged
**Priority:** Must  
**Description:** The existing `MEMORY.md` loading, caching, and injection into the system prompt is unchanged. Eidetic `## Recent Memory` is injected in addition to, not instead of, `## Memory`.  
**Acceptance criteria:** Both `## Memory` and `## Recent Memory` sections appear in the system prompt when both are populated.

### REQ-251 — Daily Log Files Unchanged
**Priority:** Must  
**Description:** Eidetic's `notes_dir` in `~/.eidetic/config.yaml` points to the same `memory/` directory as GopherClaw's workspace. Eidetic reads these files for its own indexing. GopherClaw continues to write daily logs via `memory_append` as before.  
**Acceptance criteria:** Daily log files are written by GopherClaw and read/indexed by Eidetic independently. No double-write or conflict.

### REQ-252 — memory_append and memory_get Tools Unchanged
**Priority:** Must  
**Description:** The existing `memory_append` and `memory_get` tools are not modified. They continue to write and read flat files as before.  
**Acceptance criteria:** Both tools work identically with or without Eidetic enabled.

---

## Non-Functional

### REQ-260 — No Response Latency Added
**Priority:** Must  
**Description:** Eidetic writes (REQ-230) must not add latency to the agent response path. The append goroutine is fire-and-forget; the response is delivered before the write begins.  
**Acceptance criteria:** P99 response time with Eidetic enabled is within 50ms of P99 without Eidetic (measured locally).

### REQ-261 — Prompt Construction Latency Bounded
**Priority:** Must  
**Description:** The `GetRecent` call in prompt construction is bounded by a 2-second cap (REQ-223). This is the only Eidetic call in the synchronous response path.  
**Acceptance criteria:** See REQ-223.

### REQ-262 — Eidetic Client is Goroutine-Safe
**Priority:** Must  
**Description:** The `internal/eidetic` client is safe to call from multiple goroutines concurrently (e.g. concurrent sessions each appending).  
**Acceptance criteria:** No data races detected by `go test -race` on the client package.

### REQ-263 — Unit Testable Without Real Eidetic
**Priority:** Must  
**Description:** All Eidetic integration code is testable using an `httptest.Server` mock. No real Eidetic server or Postgres required in tests.  
**Acceptance criteria:** `go test ./internal/eidetic/...` and Eidetic-related agent tests pass without a running Eidetic instance.
