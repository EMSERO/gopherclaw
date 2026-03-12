# Configuration Reference

GopherClaw reads its configuration from `~/.gopherclaw/config.json`. All fields are optional unless noted — sensible defaults are applied for anything omitted.

Generate a starter config with `gopherclaw init`.

---

## Top-Level Structure

```json
{
  "env": {},
  "providers": {},
  "logging": {},
  "agents": {},
  "tools": {},
  "session": {},
  "channels": {},
  "gateway": {},
  "messages": {},
  "update": {},
  "eidetic": {}
}
```

---

## `env`

**Type:** `map[string]string`

Environment variables injected into the process on startup. Useful for API keys and secrets that shouldn't be hardcoded in provider configs.

```json
{
  "env": {
    "ANTHROPIC_API_KEY": "sk-ant-...",
    "OPENROUTER_API_KEY": "sk-or-..."
  }
}
```

---

## `providers`

**Type:** `map[string]object`

Connection settings for model providers. Each key is a provider name.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `apiKey` | string | `""` | API key for the provider |
| `baseURL` | string | `""` | Custom base URL (empty = provider default) |

```json
{
  "providers": {
    "anthropic": { "apiKey": "sk-ant-..." },
    "openrouter": { "apiKey": "sk-or-...", "baseURL": "https://openrouter.ai/api/v1" }
  }
}
```

Supported provider prefixes in model IDs: `github-copilot/`, `anthropic/`, `openai/`, `openrouter/`. Built-in base URLs are also available for `groq`, `mistral`, `together`, `fireworks`, `perplexity`, `gemini`, `ollama`, and `lmstudio`.

---

## `logging`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `level` | string | `""` | Log level for file output: `debug`, `info`, `warn`, `error` |
| `consoleLevel` | string | `""` | Log level for console output (independent of file level) |
| `file` | string | `""` | Path to log file (e.g. `~/.gopherclaw/logs/gopherclaw.log`) |
| `redactSensitive` | string | `""` | If non-empty, suppresses tool output from logs |

```json
{
  "logging": {
    "level": "info",
    "consoleLevel": "warn",
    "file": "/home/user/.gopherclaw/logs/gopherclaw.log"
  }
}
```

---

## `agents`

### `agents.defaults`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `model.primary` | string | `""` | Primary model ID (e.g. `github-copilot/claude-sonnet-4.6`) |
| `model.fallbacks` | []string | `[]` | Fallback model IDs tried in order if primary fails |
| `models` | map | `{}` | Model alias map. Keys are model IDs, values have `alias` field for reverse lookup |
| `workspace` | string | `""` | Path to workspace directory (skills, memory, docs) |
| `userTimezone` | string | `"UTC"` | Timezone for date/time in system prompt |
| `timeoutSeconds` | int | `300` | Agent call timeout |
| `maxConcurrent` | int | `2` | Max concurrent requests per agent (also used as session.maxConcurrent fallback) |
| `maxIterations` | int | `50` | Max tool-call rounds per agent turn |
| `loopDetectionN` | int | `3` | Consecutive identical tool calls before breaking the loop |
| `softTrimRatio` | float | `0.0` | Fraction of model max tokens to trigger context soft trim (0 = disabled) |
| `engine` | string | `"router"` | Agent engine: `"router"` (default multi-provider) or `"claude-cli"` (Claude Code subprocess) |

### `agents.defaults.cliEngine`

Configuration for the `claude-cli` engine mode (only used when `engine: "claude-cli"`).

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `command` | string | `"claude"` | CLI command to invoke |
| `model` | string | `""` | Model to pass to the CLI |
| `mcpConfig` | string | `""` | Path to MCP config file for the CLI |
| `systemPrompt` | string | `""` | Additional system prompt text |
| `extraArgs` | []string | `[]` | Extra CLI arguments |
| `idleTTLSec` | int | `0` | Idle timeout in seconds before killing subprocess (0 = no timeout) |

### `agents.defaults.contextPruning`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `mode` | string | `""` | Pruning mode |
| `ttl` | string | `"1h"` | Session TTL (Go duration format) |
| `keepLastAssistants` | int | `2` | Number of recent assistant messages to preserve during hard clear |
| `hardClearRatio` | float | `0.5` | Fraction of context to clear on hard prune |
| `softTrimRatio` | float | `0.0` | Alternate location for softTrimRatio (inherited if top-level is unset) |
| `modelMaxTokens` | int | `128000` | Max context tokens for the model (used for pruning thresholds) |
| `surgicalPruning` | bool | `true` | When true, compress old tool-result messages to short placeholders before falling back to hard clear. Preserves user/assistant messages and protects the last `keepLastAssistants` turns. |
| `cacheTTLSeconds` | int | `0` | Defer context pruning until this many seconds have elapsed since the last model API call. Useful when providers cache prompt prefixes — pruning during the cache window would invalidate the cache. 0 = prune immediately. |

### `agents.defaults.thinking`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable extended thinking (Anthropic models only) |
| `budgetTokens` | int | `8192` | Tokens reserved for thinking |
| `level` | string | `""` | Thinking level: `"off"`, `"enabled"`, `"adaptive"`. Empty defaults to `"adaptive"` for Claude 4.6 models. Overrides the `enabled` field when set. |

### `agents.defaults.memory`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Load `MEMORY.md` into system prompt and enable memory tools |

### `agents.defaults.subagents`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `model` | string | `""` | Default model for subagent calls (empty = inherit primary model) |
| `maxConcurrent` | int | `4` | Max concurrent subagent tasks |

### `agents.defaults.heartbeat`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `every` | string | `"30m"` | Interval duration string (e.g. `"30m"`, `"1h"`). Empty or `"0"` disables heartbeat. |
| `activeHours` | object | `null` | Optional time-of-day window. Fields: `start` (HH:MM, inclusive), `end` (HH:MM, exclusive, `"24:00"` allowed), `timezone` (IANA or `"local"`, defaults to `agents.defaults.userTimezone`). |
| `target` | string | `"none"` | Delivery target: `"last"` (last active session), `"none"` (no delivery), or a channel name |
| `model` | string | `""` | Optional model override for heartbeat turns |
| `prompt` | string | `""` | Custom prompt. Default: reads `HEARTBEAT.md` if it exists; otherwise uses a built-in prompt. |
| `ackMaxChars` | int | `300` | Max trailing chars after `HEARTBEAT_OK` before forcing delivery |
| `lightContext` | bool | `false` | If true, cron/heartbeat jobs inject only `HEARTBEAT.md` (skipping full workspace/skills) for minimal bootstrap context |
| `directPolicy` | string | `"allow"` | Heartbeat direct policy: `"allow"` or `"block"` (parsed for config compatibility; not fully implemented) |

### `agents.defaults.sandbox`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Run exec tool commands inside a Docker container |
| `image` | string | `"ubuntu:22.04"` | Docker image for sandbox |
| `mounts` | []string | `[]` | Bind mounts in `host:container:mode` format |
| `setupCommand` | string | `""` | Run once after container creation |

### `agents.list`

Array of agent definitions. Each agent:

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `id` | string | **required** | Unique agent ID |
| `default` | bool | `false` | Whether this is the default (main) agent |
| `identity.name` | string | `""` | Agent name shown in system prompt |
| `identity.theme` | string | `""` | Agent description/role |
| `identity.emoji` | string | `""` | Emoji prefix for messages |
| `cliCommand` | string | `""` | If set, agent delegates to a CLI subprocess |
| `cliArgs` | []string | `[]` | Args prepended before the message (e.g. `["-p"]`) |
| `maxConcurrent` | int | `5` | Orchestrator: max parallel subtasks |
| `progressUpdates` | bool | `false` | Orchestrator: send per-task completion updates |
| `async` | bool | `false` | If true, runs via TaskManager (non-blocking); results delivered asynchronously |

```json
{
  "agents": {
    "defaults": {
      "model": {
        "primary": "github-copilot/claude-sonnet-4.6",
        "fallbacks": ["github-copilot/gpt-4.1"]
      },
      "workspace": "/home/user/.gopherclaw/workspace",
      "memory": { "enabled": true }
    },
    "list": [
      { "id": "main", "default": true, "identity": { "name": "GopherClaw", "theme": "helpful AI assistant" } },
      { "id": "orchestrator", "identity": { "name": "Orchestrator", "theme": "planner and synthesizer" }, "maxConcurrent": 5 },
      { "id": "coding-agent", "cliCommand": "claude", "cliArgs": ["-p", "--dangerously-skip-permissions"] }
    ]
  }
}
```

---

## `tools`

### `tools.exec`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `timeoutSec` | int | `300` | Default exec command timeout in seconds |
| `backgroundMs` | int | `0` | If >0, commands run in background and return partial output after this many ms |
| `backgroundHardTimeM` | int | `30` | Hard kill timeout for background processes in minutes |
| `maxOutputChars` | int | `100000` | Output truncation limit in characters |
| `denyCommands` | []string | `[]` | Commands to reject (empty = no restriction) |
| `confirmTimeoutSec` | int | `60` | Timeout in seconds for destructive command confirmation prompts. If the user doesn't respond within this time, the command is auto-blocked. |

### `tools.web.search`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable web search tool (DuckDuckGo) |
| `maxResults` | int | `5` | Maximum search results returned |
| `timeoutSeconds` | int | `30` | Search request timeout |

### `tools.web.fetch`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable web fetch tool |
| `maxChars` | int | `50000` | Maximum characters to fetch per page |
| `timeoutSeconds` | int | `30` | Fetch request timeout |

### `tools.files`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `allowPaths` | []string | `[]` | Restrict file access to these paths (empty = no restriction) |

### `tools.browser`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable browser automation tool (requires Chrome/Chromium) |
| `headless` | bool | `true` | Run browser in headless mode |
| `chromePath` | string | `""` | Path to Chrome binary (empty = auto-detect) |
| `noSandbox` | bool | `false` | Launch Chrome with `--no-sandbox` (required in Docker/container environments) |

---

## `session`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `scope` | string | `""` | Session key scope: `"user"`, `"channel"`, `"global"` |
| `resetTriggers` | []string | `[]` | Messages that trigger session reset |
| `idleMinutes` | int | `120` | Idle timeout before session resets |
| `maxConcurrent` | int | `2` | Max concurrent requests per session |

### `session.reset`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `mode` | string | `""` | Reset mode: `"daily"`, `"idle"`, `""` |
| `atHour` | int | `4` | Hour (0-23) for daily reset |
| `idleMinutes` | int | `0` | Idle timeout (fallback for top-level `idleMinutes`) |

---

## `channels`

### `channels.telegram`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable Telegram bot |
| `botToken` | string | `""` | Telegram Bot API token |
| `dmPolicy` | string | `""` | DM auth policy: `"pairing"`, `"allowlist"` |
| `streamMode` | string | `""` | `"partial"` for streaming message edits |
| `historyLimit` | int | `0` | Max messages to keep before trimming (0 = no limit) |
| `groupPolicy` | string | `"mention"` | Group message policy: `"mention"`, `"open"`, `"allowlist"`, `"disabled"` |
| `groups` | map | `{}` | Per-group config. Key = group ID, value has `requireMention` (bool) |
| `replyToMode` | string | `""` | `"first"` to reply to original message |
| `timeoutSeconds` | int | `300` | Agent call timeout for Telegram |
| `ackEmoji` | string | `"👀"` | Reaction emoji sent when processing begins |

### `channels.discord`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable Discord bot |
| `botToken` | string | `""` | Discord bot token |
| `dmPolicy` | string | `""` | `"pairing"` or `"allowlist"` |
| `allowUsers` | []string | `[]` | Discord user IDs (snowflakes) for allowlist mode |
| `streamMode` | string | `""` | `"partial"` for streaming message edits |
| `replyToMode` | string | `""` | `"first"` to reply to original message |
| `timeoutSeconds` | int | `300` | Agent call timeout |
| `ackEmoji` | string | `"eyes"` | Reaction emoji name |

### `channels.slack`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable Slack bot |
| `botToken` | string | `""` | Slack bot token (`xoxb-...`) |
| `appToken` | string | `""` | Slack app token for Socket Mode (`xapp-...`) |
| `allowUsers` | []string | `[]` | Slack user IDs (empty = all workspace members) |
| `streamMode` | string | `""` | `"partial"` for streaming message edits |
| `timeoutSeconds` | int | `300` | Agent call timeout |
| `ackEmoji` | string | `"eyes"` | Reaction emoji name |

---

## `gateway`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `port` | int | `18789` | HTTP server port |
| `bind` | string | `""` | `"loopback"` for 127.0.0.1 only; anything else binds 0.0.0.0 |
| `readTimeoutSec` | int | `300` | HTTP read timeout in seconds |
| `writeTimeoutSec` | int | `600` | HTTP write timeout in seconds |

### `gateway.auth`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `mode` | string | `"token"` | Auth mode: `"token"`, `"none"`, `"trusted-proxy"` |
| `token` | string | `""` | Bearer token. If empty in token mode, auto-generated on first run |

### `gateway.controlUi`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable the web control UI |
| `basePath` | string | `"/gopherclaw"` | URL base path for the control UI |

### `gateway.rateLimit`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `rps` | float | `0` | Max requests per second per IP (0 = disabled) |
| `burst` | int | `max(1, rps)` | Burst capacity for rate limiting |

When `rps > 0`, the gateway applies per-IP token-bucket rate limiting. Requests exceeding the limit receive a `429 Too Many Requests` response.

### `gateway.reload`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `mode` | string | `""` | Hot-reload mode: `"hybrid"`, `"poll"`, `""` (empty = disabled) |
| `debounceMs` | int | `500` | Debounce interval for file change events |

---

## `messages`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `ackReactionScope` | string | `""` | When to add ack reactions: `"group-mentions"`, `"all"`, `""` |
| `usage` | string | `"off"` | Token usage display: `"off"`, `"tokens"` |
| `streamEditMs` | int | `400` | Streaming message edit interval in ms |

### `messages.queue`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `mode` | string | `""` | `"collect"` to batch messages with debounce |
| `debounceMs` | int | `0` | Debounce wait in ms |
| `cap` | int | `0` | Max messages to collect before flushing |

---

## `taskQueue`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `maxConcurrent` | int | `5` | Max parallel background tasks |
| `resultRetentionM` | int | `60` | Minutes to keep completed task results before pruning |
| `progressThrottleS` | int | `5` | Minimum seconds between progress updates per task |

The task queue is used by async delegate calls and background tasks. Results are automatically pruned after the retention period.

---

## `update`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `autoUpdate` | bool | `false` | If true, binary self-updates on startup when a new version is available |

When `autoUpdate` is `false` (default), GopherClaw logs a notification about available updates. When `true`, it downloads and replaces the binary automatically. A service restart is still required to run the new version.

---

## `eidetic`

Optional integration with the [Eidetic](https://github.com/EMSERO/eidetic) semantic memory sidecar. When enabled, GopherClaw records every conversation exchange and can recall past context via semantic search.

When `enabled` is `false` (the default), the integration is fully disabled and GopherClaw behaves as if this block were absent. If Eidetic is unreachable at startup, the integration is silently disabled.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable Eidetic memory integration |
| `baseURL` | string | `"http://localhost:7700"` | Eidetic server URL |
| `apiKey` | string | `""` | Bearer token for Eidetic API |
| `agentID` | string | `""` | Override agent ID namespace (defaults to the agent's `id` from `agents.list`) |
| `recentLimit` | int | `20` | Number of recent memory entries injected into the system prompt |
| `searchLimit` | int | `10` | Max results returned by the `eidetic_search` tool |
| `searchThreshold` | float64 | `0.5` | Minimum cosine similarity for search results |
| `timeoutSeconds` | int | `5` | Per-request timeout for Eidetic API calls |
| `recallEnabled` | bool | `true` | Enable automatic semantic recall per agent turn |
| `recallLimit` | int | `5` | Max recalled entries per turn |
| `recallThreshold` | float64 | `0.4` | Minimum relevance for recalled entries |
| `recallTimeoutSeconds` | int | `5` | Per-recall timeout (0 = use `timeoutSeconds`) |

### `eidetic.embeddings`

Client-side vector embedding generation for hybrid (keyword + vector) memory search.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Generate embeddings on store and use hybrid search for retrieval |
| `provider` | string | `""` | Provider name for base URL lookup (e.g. `"ollama"`, `"openai"`) |
| `model` | string | `""` | Embedding model ID (e.g. `"nomic-embed-text"`, `"text-embedding-3-small"`) |
| `baseURL` | string | `""` | Override the provider's default endpoint |
| `apiKey` | string | `""` | API key (empty = `"no-key"` for local providers like Ollama) |
| `dimensions` | int | `0` | Optional truncated dimensions (0 = model default) |

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
    "timeoutSeconds": 5,
    "embeddings": {
      "enabled": true,
      "provider": "ollama",
      "model": "nomic-embed-text"
    }
  }
}
```

### How it works

1. **Prompt injection** — On each agent turn, the `recentLimit` most recent memory entries are injected into the system prompt under a `## Recent Memory` section (2-second timeout cap).
2. **Post-turn append** — After each agent turn, the user message and assistant response are recorded to Eidetic asynchronously (non-blocking, 5-second timeout).
3. **Semantic recall** — When `recallEnabled` is true, the agent automatically searches for relevant past context based on the user's message before responding.
4. **Agent tool** — The `eidetic_search` tool lets the agent explicitly search past conversations and decisions.

### `agents.defaults.toolLoopDetection`

Multi-detector tool loop detection system. Enabled by default.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `true` | Enable multi-detector loop detection |
| `historySize` | int | `30` | Sliding window size for tool call history |
| `warningThreshold` | int | `10` | Repetitions to trigger a warning message |
| `criticalThreshold` | int | `20` | Repetitions to trigger a hard loop break |
| `globalCircuitBreakerThreshold` | int | `30` | Any single hash repeated this many times stops the loop |
