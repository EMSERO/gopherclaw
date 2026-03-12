# Gateway API Reference

GopherClaw exposes an HTTP API on the configured gateway port (default `18789`). All endpoints except `/health` and the control UI require authentication.

Base URL: `http://127.0.0.1:18789`

---

## Authentication

Most endpoints require a bearer token. Provide it via:

- **Header:** `Authorization: Bearer <token>`
- **Query parameter:** `?token=<token>`

The token is configured in `gateway.auth.token`. If not set, one is auto-generated on first run and saved to config.

Auth modes (`gateway.auth.mode`):
- `"token"` (default) — bearer token required
- `"none"` — no authentication
- `"trusted-proxy"` — assumes upstream proxy handles auth

---

## Endpoints

### Health Check

```
GET /health
GET /healthz
GET /ready
GET /readyz
```

No authentication required. `/healthz`, `/ready`, and `/readyz` are aliases for Docker/Kubernetes probes.

**Response:**
```json
{
  "status": "ok",
  "version": "0.4.0",
  "checks": {
    "sessions": "ok",
    "cron": "ok",
    "channels": [
      {"name": "telegram", "connected": true},
      {"name": "discord", "connected": false}
    ]
  }
}
```

`status` is `"ok"` when all channels are connected, `"degraded"` if any channel is disconnected. `version` reflects the build version injected via `-ldflags` at compile time.
```

---

### Chat Completions

```
POST /v1/chat/completions
```

OpenAI-compatible chat completions endpoint.

**Headers:**
- `Authorization: Bearer <token>` (required)
- `X-Session-Key: <key>` (optional, defaults to `agent:main:gateway`)

**Request Body:**
```json
{
  "messages": [
    {"role": "user", "content": "Hello"}
  ],
  "model": "github-copilot/claude-sonnet-4.6",
  "stream": false
}
```

**Non-streaming response:**
```json
{
  "id": "chatcmpl-abc123",
  "object": "chat.completion",
  "created": 1709123456,
  "model": "github-copilot/claude-sonnet-4.6",
  "choices": [{
    "index": 0,
    "message": {"role": "assistant", "content": "Hello! How can I help?"},
    "finish_reason": "stop"
  }],
  "usage": {
    "prompt_tokens": 10,
    "completion_tokens": 8,
    "total_tokens": 18
  }
}
```

**Streaming response** (`"stream": true`):

Content-Type: `text/event-stream`

```
data: {"id":"chatcmpl-abc123","object":"chat.completion.chunk","choices":[{"delta":{"content":"Hello"}}]}

data: {"id":"chatcmpl-abc123","object":"chat.completion.chunk","choices":[{"delta":{"content":"!"}}]}

data: {"id":"chatcmpl-abc123","object":"chat.completion.chunk","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":8,"total_tokens":18}}

data: [DONE]
```

**Example:**
```bash
curl -X POST http://127.0.0.1:18789/v1/chat/completions \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"messages":[{"role":"user","content":"What time is it?"}]}'
```

---

### List Models

```
GET /v1/models
```

**Response:**
```json
{
  "object": "list",
  "data": [
    {"id": "github-copilot/claude-sonnet-4.6", "object": "model", "created": 1677610602, "owned_by": "github-copilot"},
    {"id": "github-copilot/gpt-4.1", "object": "model", "created": 1677610602, "owned_by": "github-copilot"}
  ]
}
```

**Example:**
```bash
curl http://127.0.0.1:18789/v1/models -H "Authorization: Bearer $TOKEN"
```

---

### Get Model

```
GET /v1/models/{model}
```

Returns a single model object. Returns 404 if the model is not configured as primary or fallback.

---

### Webhooks

```
POST /webhooks/{session}
```

Send a message to a named session and get a response.

**Request Body:**
```json
{
  "message": "Run the daily report",
  "stream": false
}
```

**Non-streaming response:**
```json
{
  "text": "Here is the daily report...",
  "stopped": false
}
```

**Streaming response** (`"stream": true`):

Content-Type: `text/event-stream`

```
data: {"text":"Here "}

data: {"text":"is the "}

data: {"text":"daily report..."}

event: done
data: {"text":"Here is the daily report...","stopped":false}
```

Session key is `webhook:<session>`.

**Example:**
```bash
curl -X POST http://127.0.0.1:18789/webhooks/daily-report \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"message":"Run the daily report"}'
```

---

### Tool Invocation

```
POST /tools/invoke
```

Invoke a tool directly without going through the agent conversation loop.

**Request Body:**
```json
{
  "tool": "exec",
  "args": {"command": "uptime"},
  "sessionKey": "optional-session-key"
}
```

**Response:**
```json
{
  "ok": true,
  "result": " 14:32:01 up 7 days, 3:45, 1 user, load average: 0.12, 0.08, 0.05"
}
```

Returns 404 if the tool name is not found.

**Example:**
```bash
curl -X POST http://127.0.0.1:18789/tools/invoke \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"tool":"exec","args":{"command":"date"}}'
```

---

### System Event

```
POST /gopherclaw/system/event
```

Trigger a system event — the agent processes the text and delivers the response to all paired channels.

**Request Body:**
```json
{
  "text": "The backup job completed successfully",
  "mode": "now"
}
```

**Response:** `202 Accepted`
```json
{
  "status": "accepted"
}
```

Processing happens asynchronously. The response is delivered to all paired Telegram/Discord/Slack users.

### Notify User

```
POST /gopherclaw/api/notify
```

Send a notification message to the user's active channel (Telegram/Discord/Slack). Used by the MCP server and external integrations.

**Request Body:**
```json
{
  "message": "Deployment complete!",
  "session": "optional-session-key"
}
```

**Response:** `200 OK`
```json
{
  "status": "delivered"
}
```

---

## Control UI Endpoints

These require `gateway.controlUi.enabled: true` and bearer token auth (except the HTML page and log stream).

### Control UI Page

```
GET /gopherclaw
```

No authentication required. Returns the HTML control UI. The gateway auth token is injected into the page for API calls.

---

### WebSocket Status

```
GET /gopherclaw/ws
```

WebSocket connection for real-time status updates. No bearer token required (same-origin browser check via Origin header).

**Status message (sent every 5 seconds):**
```json
{
  "status": "running",
  "model": "github-copilot/claude-sonnet-4.6",
  "fallbacks": ["github-copilot/gpt-4.1"],
  "sessions": [
    {"key": "agent:main:telegram:12345", "updatedAt": 1709123456, "ago": "5s", "messageCount": 10}
  ],
  "channels": [
    {"name": "telegram", "connected": true, "username": "MyBot", "pairedCount": 3}
  ],
  "cron": {"enabled": 2, "total": 5},
  "timestamp": 1709123456
}
```

Send any message to the WebSocket to request an immediate status refresh.

---

### View Config

```
GET /gopherclaw/api/config
```

Returns the full config with sensitive values redacted. Any key containing "token", "key", "secret", or "password" is replaced with `"****"`.

---

### Session History

```
GET /gopherclaw/api/sessions/{key}/history
```

Returns the message history for a session as a JSON array.

---

### Clear Session

```
POST /gopherclaw/sessions/clear
```

**Request Body:**
```json
{"key": "agent:main:telegram:12345"}
```

**Response:**
```json
{"cleared": "agent:main:telegram:12345"}
```

---

### Clear All Sessions

```
POST /gopherclaw/sessions/clear-all
```

Clears all session histories.

**Response:**
```json
{"cleared": "all"}
```

---

### Log Stream (SSE)

```
GET /gopherclaw/api/log
```

No authentication required. Server-Sent Events stream of real-time log output in JSON format.

Returns 503 if the log broadcaster is not configured.

**Example:**
```bash
curl -N http://127.0.0.1:18789/gopherclaw/api/log
```

---

## Cron Management

All cron endpoints require bearer token auth and `gateway.controlUi.enabled: true`.

### List Jobs

```
GET /gopherclaw/api/cron
```

**Response:**
```json
{
  "jobs": [
    {
      "id": "abc123",
      "spec": "@daily",
      "instruction": "Generate the daily summary",
      "enabled": true,
      "nextRun": 1709200000,
      "lastRun": 1709113600,
      "lastError": null
    }
  ]
}
```

---

### Add Job

```
POST /gopherclaw/api/cron
```

**Request Body:**
```json
{
  "spec": "@daily",
  "instruction": "Generate the daily summary"
}
```

Supported schedule formats: `@daily`, `@hourly`, `@every 1h`, `HH:MM`.

**Response:** `201 Created`
```json
{
  "job": {"id": "abc123", "spec": "@daily", "instruction": "...", "enabled": true}
}
```

---

### Remove Job

```
DELETE /gopherclaw/api/cron/{id}
```

**Response:**
```json
{"ok": true}
```

---

### Run Job Now

```
POST /gopherclaw/api/cron/{id}/run
```

Triggers immediate execution. Returns `202 Accepted`.

---

### Enable / Disable Job

```
POST /gopherclaw/api/cron/{id}/enable
POST /gopherclaw/api/cron/{id}/disable
```

**Response:**
```json
{"ok": true, "enabled": true}
```

---

### Cron Run History

```
GET /gopherclaw/api/cron/{name}/history
```

Returns paginated run history for a cron job.

**Query Parameters:**

| Param | Type | Default | Description |
|-------|------|---------|-------------|
| `limit` | int | `20` | Max entries to return |
| `offset` | int | `0` | Offset for pagination |
| `status` | string | `""` | Filter by status (`"ok"`, `"error"`) |
| `sort` | string | `"desc"` | Sort order (`"asc"`, `"desc"`) |
| `q` | string | `""` | Text search in instruction/output |

**Response:**
```json
{
  "runs": [
    {
      "jobId": "abc123",
      "startedAt": 1709123456,
      "finishedAt": 1709123466,
      "status": "ok",
      "output": "Daily summary generated.",
      "deliveryStatus": "delivered"
    }
  ],
  "total": 42
}
```

**Example:**
```bash
curl "http://127.0.0.1:18789/gopherclaw/api/cron/daily-summary/history?limit=10&status=error" \
  -H "Authorization: Bearer $TOKEN"
```

---

### Token Usage

```
GET /gopherclaw/api/usage
```

Returns token usage statistics. Query `?session=<key>` for a single session, or omit for aggregate usage across all sessions.

**Response:**
```json
{
  "sessions": {
    "agent:main:telegram:12345": {
      "input": 15200,
      "output": 8400,
      "cacheRead": 3200,
      "cacheWrite": 1100,
      "total": 27900
    }
  },
  "aggregate": {
    "input": 45000,
    "output": 22000,
    "total": 67000
  }
}
```

**Example:**
```bash
curl "http://127.0.0.1:18789/gopherclaw/api/usage" \
  -H "Authorization: Bearer $TOKEN"
```

---

## Task Queue API

All task endpoints require bearer token auth and `gateway.controlUi.enabled: true`.

### List Tasks

```
GET /gopherclaw/api/tasks
```

Returns all background tasks (running, completed, and cancelled).

**Response:**
```json
{
  "tasks": [
    {
      "id": "task-abc123",
      "agentID": "coding-agent",
      "status": "running",
      "message": "Refactor the auth module...",
      "createdAt": 1709123456
    }
  ]
}
```

---

### Cancel Task

```
POST /gopherclaw/api/tasks/{id}/cancel
```

Cancels a running background task.

**Response:**
```json
{"ok": true}
```

Returns `404` if the task ID is not found.

---

## Skills API

All skills endpoints require bearer token auth and `gateway.controlUi.enabled: true`.

### List Skills

```
GET /gopherclaw/api/skills
```

Returns all loaded skills with their enabled/verified status.

**Response:**
```json
{
  "skills": [
    {
      "name": "calendar-manager",
      "description": "Manage calendar events",
      "origin": "CrawHub",
      "enabled": true,
      "verified": true
    }
  ]
}
```

---

### Enable / Disable Skill

```
POST /gopherclaw/api/skills/{name}/enable
POST /gopherclaw/api/skills/{name}/disable
```

Toggles a skill's enabled state at runtime. Takes effect immediately without restart.

**Response:**
```json
{"ok": true}
```

Returns `404` if the skill name is not found.

---

## Version & Rollback API

All version endpoints require bearer token auth and `gateway.controlUi.enabled: true`.

### Version Info

```
GET /gopherclaw/api/version
```

Returns the current version, backup version (if available), and latest available version.

**Response:**
```json
{
  "current": "1.1.0",
  "backup": "1.0.0",
  "latest": "1.1.0"
}
```

---

### Rollback

```
POST /gopherclaw/api/rollback
```

Rolls back to the previously backed-up binary (`gopherclaw.bak`). The service re-execs automatically.

**Response:** `202 Accepted`
```json
{"status": "rolling back"}
```

Returns an error if no backup binary exists.

---

## Error Format

All endpoints return errors as:

```json
{
  "error": "description of what went wrong"
}
```

Common HTTP status codes:
- `400` — Invalid request body or parameters
- `401` — Missing or invalid auth token
- `404` — Resource not found (tool, job, model, skill, task)
- `429` — Rate limited (when `gateway.rateLimit.rps > 0`). Retry after the rate limit window resets.
- `500` — Internal server error
- `503` — Service not available (log broadcaster or cron not initialized)
