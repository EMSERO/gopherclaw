# GopherClaw — Implementation Status

> **Note:** This is an internal document for project maintainers tracking implementation progress. It is not user-facing documentation — see [README.md](README.md) and [docs/](docs/) instead.

Last updated: 2026-03-08 (Eidetic memory integration, notify_user tool, search-before-ask prompt, subagents config; 66 new tests)

---

## ✅ Working

### Core Infrastructure
- **Config parsing** — full `config.json` → Go structs, with `applyDefaults()`
- **Logger** — zap, dual console+file output, level from config
- **GitHub Copilot auth** — token exchange, disk cache, background proactive refresh
- **Model router + multi-provider** — `Provider` interface dispatched by model-string prefix (`"provider/model-id"`); built-in providers: `github-copilot` (always), `anthropic` (native messages API, configure via `providers.anthropic.apiKey`), and any OpenAI-compatible endpoint via `providers.<name>.{apiKey,baseURL}`; built-in base URLs for `openai`, `groq`, `openrouter`, `mistral`, `together`, `fireworks`, `perplexity`, `gemini`, `ollama`, `lmstudio`; fallback chain can span providers; backward-compatible (no prefix → `github-copilot`)
- **Extended thinking** — `agents.defaults.thinking.enabled: true` + `budgetTokens: 8192` (Anthropic models; `max_tokens` auto-bumped above budget)
- **Env var injection** — `cfg.Env` map entries are set via `os.Setenv()` at startup before any handlers run
- **Config hot-reload** — fsnotify watches `config.json`; on change, debounced reload (500ms default) updates session pruning policy and env vars; **config snapshots** prevent data races — each message handler snapshots `cfg`/`msgCfg` under the lock before processing
- **`errgroup` lifecycle** — all services (gateway, channel bots, cron, config watcher) launch via `errgroup.WithContext`; if any service fails (e.g. gateway can't bind port), the error propagates and the process shuts down cleanly instead of running headless
- **Systemd service file** — `~/.config/systemd/user/gopherclaw-gateway.service` points at `~/.local/bin/gopherclaw`; PATH includes `%h/.local/bin` for CLI subagent resolution
- **`gopherclaw-restart` script** — `~/.local/bin/gopherclaw-restart` stops the service, clears sessions, cancels Telegram long-poll, then restarts

### HTTP Gateway (port 18789)
- `GET /health` — no auth required
- `GET /v1/models` — lists primary + fallback models
- `POST /v1/chat/completions` — full response and SSE streaming, both work
- `POST /webhooks/{session}` — inbound webhooks; auth-protected; session key `webhook:<session>`; optional SSE streaming via `stream: true`
- `POST /tools/invoke` — single tool execution; payload: `{"tool":"exec","args":{...},"sessionKey":"..."}`
- `POST /gopherclaw/system/event` — system event ingress; payload: `{"text":"...","mode":"now"}`; triggers agent turn, delivers response to all paired Telegram users
- `GET /gopherclaw/api/cron` — list all cron jobs
- `POST /gopherclaw/api/cron` — create cron job; payload: `{"spec":"@daily","instruction":"..."}`
- `DELETE /gopherclaw/api/cron/{id}` — remove cron job
- `POST /gopherclaw/api/cron/{id}/run` — manually trigger cron job
- `POST /gopherclaw/api/cron/{id}/enable` — enable cron job
- `POST /gopherclaw/api/cron/{id}/disable` — disable cron job
- `GET /gopherclaw/api/tasks` — list background tasks (running, completed, cancelled)
- `POST /gopherclaw/api/tasks/{id}/cancel` — cancel a running background task
- `GET /gopherclaw/api/skills` — list loaded skills with enabled/verified status
- `POST /gopherclaw/api/skills/{name}/enable` — enable a skill at runtime
- `POST /gopherclaw/api/skills/{name}/disable` — disable a skill at runtime
- `GET /gopherclaw/api/version` — version info (current, backup, latest)
- `POST /gopherclaw/api/rollback` — rollback to previous binary
- `GET /gopherclaw/api/usage` — token usage per session or aggregate; query `?session=key` for single session
- `GET /gopherclaw/api/cron/{name}/history` — paginated cron run history; query params: `limit`, `offset`, `status`, `sort`, `q`
- `GET /gopherclaw` — control UI (`//go:embed ui.html` + WebSocket status; per-session Clear and Clear All buttons; **Usage tab** with aggregate + per-session token usage; **Cron history panel** with run log table)
- `POST /gopherclaw/sessions/clear` — clear a single session by key
- `POST /gopherclaw/sessions/clear-all` — clear all active sessions
- Bearer token auth on all non-health routes
- **Per-IP rate limiting** — configurable via `gateway.rateLimit.rps` and `gateway.rateLimit.burst`; returns `429 Too Many Requests` when exceeded
- Router: [chi v5](https://github.com/go-chi/chi) (replaced gin)

### Agent Engine
- Full conversation loop with tool calling (up to `maxIterations` iterations, default 200)
- System prompt: `You are {name}, {theme}. Date: {now}. Skills: ... Workspace: ...`
- Both blocking (`Chat`) and streaming (`ChatStream`) modes
- Session persistence: JSONL history per session key, TTL-based expiry
- **Token-based context pruning** — `hardClearRatio` (default 0.5) × `modelMaxTokens` (128k) triggers history clear, keeping last `keepLastAssistants` (default 2) assistant messages
- **Soft trim** — when `softTrimRatio > 0`, calls the model to summarize old messages into a single `[Conversation summary]` message before the hard-clear threshold is reached; summary is persisted to disk via `ReplaceHistory`
- **Persistent compaction** — `/compact [instructions]` slash command force-runs soft trim immediately and saves to disk
- **Per-session model override** — `/model <provider/model-id>` sets per-session model; stored in `Agent.sessionModels` sync.Map; `/model` without args shows current
- **Loop detection** — `loopDetectionN` (default 3) consecutive identical tool-call fingerprints breaks the agent loop with an error message
- **Idle reset** — sessions idle for `idleMinutes` (default 120) are cleared by background goroutine
- **Daily reset** — when `reset.mode: "daily"`, all sessions are cleared at `atHour` (default 4)
- **Max concurrent requests** — semaphore enforces `maxConcurrent` (default 2) per agent; excess requests block until a slot opens
- **Smart block chunking** — `common.SmartChunk()` replaces naive `SplitMessage()` with code-fence-aware, paragraph-respecting splitting. Break-point priority: paragraph boundary > newline > sentence > space > hard cut. `healFences()` closes/reopens Markdown fences that span chunk boundaries. (25 tests)
- **Channel send retry with backoff** — `common.RetrySend()` wraps all channel sends with configurable max attempts (default 3), exponential backoff (500ms base, 5s cap), ±10% jitter, `retry_after` header parsing, and optional markdown→plain-text fallback on final attempt. (13 tests)
- **Human-like pacing** — 800–2500ms random delay between consecutive message chunks in Telegram streaming and multi-chunk sends; prevents bot-like rapid-fire delivery
- **Surgical tool-result pruning** — `PruneToolResults()` compresses old tool-result messages to short head+tail placeholders instead of hard-clearing the entire context. Walk-back logic protects the last N assistant turns. Enabled by default (`surgicalPruning: true`). (4 tests)
- **Cache-TTL-aware pruning** — `cacheTTLSeconds` defers context pruning until the provider's prompt-prefix cache has expired. `TouchAPICall()` tracks last API call per session. (2 tests)
- **Auth profile rotation + cooldowns** — `models.Cooldown` tracks per-model failures with exponential backoff (base 1m, 5× multiplier, max 1h). Router skips cooled-down models, records failures/successes. Prevents repeated retries to a failing API endpoint. (11 tests)
- **Lifecycle hook bus** — `hooks.Bus` event system with 15 event types (model, prompt, tool, message, session, gateway lifecycle). Thread-safe `On()`/`Off()`/`Emit()`/`EmitAsync()`. Wired into agent loop (before/after model resolve, tool calls, prompt build) and main.go (gateway start/stop). (13 tests)
- **Usage tracking** — `messages.usage: "tokens"` appends `📊 ~N tokens` to each reply (estimated from API usage)
- **Normalized usage tracking** — `UsageTracker` normalizes 15+ provider naming variants into unified `NormalizedUsage{Input, Output, CacheRead, CacheWrite, Total}`. Per-session and aggregate tracking. Wired into both `loop()` and `loopStream()`. Dashboard and REST API (`/api/usage`) exposed. (10 tests)
- **Multi-detector loop detection** — `ToolLoopDetector` with sliding window and 4 detection strategies: generic repeat, poll-no-progress, ping-pong, and global circuit breaker. Each detector has configurable thresholds and can be individually enabled. Warning/Critical severity levels. Configurable via `agents.defaults.toolLoopDetection.*`. Falls back to simple detector when disabled. (15 tests)
- **LLM-powered context compaction** — `CompactHistory()` splits messages into token-budgeted chunks, strips tool result details, summarizes via the active model with identifier preservation, and merges summaries. Adaptive chunk ratio (0.15–0.4). Progressive fallback: full → partial → note → hard clear. Wired into `softTrim()` as the primary strategy. (8 tests)
- **Context window guard** — `ValidateContextWindow()` hard-blocks models with <16K context and warns about models with <32K context. `ResolveContextWindow()` resolves effective context from per-model config → agent default → 128K fallback. Checked in `buildRequest()` before every model call.
- **Session write locks** — file-based exclusive locks (`O_CREATE|O_EXCL`) with PID + process start time for stale detection. Watchdog goroutine reclaims over-held locks (>5min). Progressive backoff on contention. Integrated into `appendJSONL()` and `rewriteJSONL()`. (9 tests)
- **Memory system** — `agents.defaults.memory.enabled: true` injects `{workspace}/MEMORY.md` into every system prompt; `memory_append` / `memory_get` tools let the agent write and read memory files

### Eidetic Memory Integration
- **Eidetic sidecar client** — `internal/eidetic/` provides `Client` interface with `AppendMemory`, `SearchMemory`, `GetRecent`, and `Health` methods; HTTP/MCP transport with Bearer token auth
- **System prompt injection** — `GetRecent()` injects the N most recent memory entries into the system prompt under `## Recent Memory` (2-second timeout cap)
- **Post-turn append** — after each agent turn (Chat, ChatStream, ChatLight), a non-blocking goroutine records the user/assistant exchange to Eidetic (5-second timeout)
- **Semantic recall** — automatic per-turn recall searches for relevant past context based on the user's message; configurable via `eidetic.recallEnabled`, `recallLimit`, `recallThreshold`
- **Search-before-ask prompt** — agent system prompt includes instructions to search eidetic memory before asking the user for information it may have discussed previously
- **Agent tools** — `eidetic_search` (semantic search over past conversations) and `eidetic_append` (store a memory entry with optional tags); registered only when eidetic is enabled
- **`notify_user` tool** — sends a proactive message to the user on their current channel via `AnnounceToSession` infrastructure; used by cron failure alerts and agent-initiated notifications
- **Graceful degradation** — startup health check silently disables eidetic if the server is unreachable; all eidetic calls use nil-safe interface pattern with `sync.RWMutex` protection

### Tools
- `exec` — `bash -c <command>`, configurable timeout, output truncated at 100KB; optional `tools.exec.denyCommands` list; optional Docker sandbox (`agents.defaults.sandbox.enabled: true`); **destructive command confirmation** — built-in blocklist of dangerous patterns (e.g. `rm -rf`, `dd`, `shutdown`); matching commands require user confirmation via active channel; auto-blocked after `confirmTimeoutSec` (default 60s) if no response
- `web_search` — DuckDuckGo HTML scraping; decodes DDG redirect URLs, detects rate-limit/CAPTCHA pages
- `web_fetch` — HTTP GET, strips HTML tags, truncates at maxChars; **SSRF protection** — DNS-based IP preflight blocks private, loopback, link-local, and multicast addresses; custom `DialContext` transport prevents DNS rebinding
- `read_file` / `write_file` / `list_dir` — filesystem tools; optional `tools.files.allowPaths` list restricts access
- `memory_append` / `memory_get` — persistent memory tools; write to `MEMORY.md` or daily log files, read any memory file
- `browser` — chromedp-backed browser automation (`tools.browser.enabled: true`); actions: `navigate`, `screenshot`, `click`, `type`, `eval`, `close`; per-session browser pool with 10-min idle reaper
- **`delegate`** — calls a named subagent (`agent_id`, `message`, optional `session_id`); depth-limited to 5 recursive calls; supports `action: "status"` to query tasks via centralized `taskqueue.Manager` (task ID, agent ID, status, message preview)

### Subagents
- **In-process subagents** — additional entries in `agents.list` become `*Agent` instances; main agent calls them via `delegate` tool
- **CLI-backed subagents** — add `cliCommand` + `cliArgs` to an `agents.list` entry; agent spawns the CLI as a subprocess per call; implements `Chatter` interface alongside `*Agent`; command resolved via `exec.LookPath` at construction time so bare names (e.g. `claude`) work under systemd's minimal PATH
  ```json
  {"id": "coding-agent", "cliCommand": "claude", "cliArgs": ["-p", "--dangerously-skip-permissions"]}
  ```
- `Chatter` interface: `Chat(ctx, sessionKey, message) (Response, error)` — both `*Agent` and `*CLIAgent` satisfy it; `DelegateTool.Agents` is `map[string]Chatter`
- **Async delegate feedback** — CLI subagents run asynchronously via centralized `taskqueue.Manager`; two-phase result delivery: (1) raw result announced immediately to the originating session, (2) result fed through the main agent via `Chat()` so it can summarize, react, or take follow-up actions; main agent response announced as a second message
- **Startup CLI warnings** — if a CLI subagent's command can't be resolved via `exec.LookPath`, a warning is logged at startup

### Cron
- `internal/cron/` — lightweight scheduler; specs: `@hourly`, `@daily`, `@weekly`, `@every <duration>`, `HH:MM`
- Jobs persisted to `~/.gopherclaw/cron/jobs.json` (full format); also reads simple format from `~/.openclaw/agents/{id}/crons.json` for backward compatibility
- **Live scheduling** — jobs added via REST API (`POST /api/cron`) or re-enabled via `/api/cron/{id}/enable` are scheduled immediately without requiring a restart
- **Persistent run log** — JSONL run log per job (`<dir>/runs/<jobId>.jsonl`) with auto-pruning (2MB / 2000 lines). `ReadRunLogPage()` supports paginated reads with limit/offset/status/delivery/sort/query filters. Exposed via `GET /api/cron/{name}/history`. (10 tests)
- Slash commands: `/cron list`, `/cron add <spec> <instruction>`, `/cron remove <id>`, `/cron enable/disable <id>`

### Slash Commands (unified — all channels)
- `/new` / `/reset` — clear session
- `/compact [instructions]` — force soft trim and persist to disk
- `/model [provider/model-id]` — show or set per-session model override
- `/context` — show message count, estimated tokens, session key
- `/export` — dump full session as plain text
- `/cron list|add|remove|enable|disable` — manage scheduled jobs

### Migration Tool
- `gopherclaw --migrate` — converts OpenClaw JSONL session history to GopherClaw format
- Reads `~/.openclaw/agents/main/sessions/sessions.json` + per-session JSONL files
- Writes converted sessions to `~/.openclaw/agents/main/sessions/gopherclaw/`
- Converts user/assistant/toolResult events; skips non-message events
- Tool call `arguments` objects are re-serialized to JSON strings (OpenAI format)

### Security Audit CLI
- `gopherclaw security` — runs a comprehensive security audit of the current configuration
- `gopherclaw security --deep` — includes filesystem permission checks
- 4 check categories: gateway (GW-001–004), filesystem (FS-001–003), exec tool (EXEC-001–002), model config (MODEL-001–004)
- ANSI-colored severity indicators (CRITICAL/HIGH/MEDIUM/LOW/INFO)
- Exits 2 on critical findings for CI integration
- (14 tests)

### Telegram Bot
- Long-polling (telebot v3)
- **Startup poller eviction** — calls `getUpdates?timeout=0` before starting to force any competing poller to receive 409 Conflict
- `/new` and `/reset` — clear session; `/pair <code>` — code-based pairing
- **Pairing persistence** — paired users written to `~/.openclaw/credentials/telegram-default-allowFrom.json`; compatible with OpenClaw state
- **`groupPolicy`** — `"mention"` (default, require `@botname`), `"open"` (no mention required), `"allowlist"` (require pairing), `"disabled"` (ignore all group messages); falls back to legacy `groups."*".requireMention` setting
- **`ackEmoji`** — configurable reaction emoji (`telegram.ackEmoji`); defaults to `👀`
- **Session scope** (`session.scope`) — `"user"` (default, per-sender), `"channel"` (per chat ID), `"global"` (single session); unrecognized values fall through to user scope
- **Reset triggers** (`session.resetTriggers`) — exact-match (case-insensitive) words/phrases that automatically clear the session and reply "Session cleared."
- Streaming mode: sends placeholder, edits as chunks arrive (1s debounce)
- **Message queue/debouncing** — collects rapid-fire messages before combining and processing; cap-based flush
- **`replyToMode: "first"`** — replies to the first message in a debounced batch
- **Inline button callback handling** — `callback_query` data injected as user message `callback_data: <value>`
- **`SendTo` / `SendToAllPaired`** — programmatic message delivery (used by system events)

### Discord Bot
- Gateway bot (discordgo) with long-polling connection
- DM support: `dmPolicy: "pairing"` or `"allowlist"` (explicit user IDs)
- Guild support: responds only when mentioned (`<@botID>`)
- `/pair <code>` — pair user; **pairing persistence** to `~/.openclaw/credentials/discord-default-allowFrom.json`
- **`ackEmoji`** — configurable (`discord.ackEmoji`); defaults to `👀`
- **Session scope** — same `session.scope` support as Telegram; channel scope keys by Discord channel ID
- **Reset triggers** — same `session.resetTriggers` support
- Streaming mode, message queue/debouncing, message splitting at 2000 chars

### Slack Bot
- Socket Mode bot (slack-go) — no public URL required
- DM handling and `@mention` handling in channels
- Authorization: `allowUsers` list (empty = all workspace members allowed)
- **`ackEmoji`** — configurable (`slack.ackEmoji`); defaults to `eyes` (Slack emoji name format)
- **Session scope** — same `session.scope` support; channel scope keys by Slack channel ID
- **Reset triggers** — same `session.resetTriggers` support
- Streaming mode, message queue/debouncing, message splitting at 3000 chars

### Skills & Workspace
- Walks `workspace/skills/*/SKILL.md`, parses YAML frontmatter
- Injects skill name, description, full content into system prompt
- Loads all `workspace/*.md` files as workspace context

---

## ✅ Code Quality

- **Go 1.26** — module targets go 1.26; Green Tea GC and faster `io.ReadAll` active by default
- **golangci-lint 2.10.1** — 0 issues (errcheck, staticcheck, unused all pass)
- **trivy 0.69.1** — 0 vulnerabilities, 0 secrets, 0 misconfigurations
- **No gin** — HTTP routing via chi v5; dependency surface reduced significantly
- **go fix modernizations** — `any`, `max()` built-in, `strings.Cut`, `strings.Builder`, `errors.Is`, `for range N`

---

## ⚠️ Known Limitations

### Security (from code audit 2026-02-25)

The following items identified in the audit have been resolved:
- ✅ `/pair` code verification — now requires a 6-digit code printed to the log at startup
- ✅ WebSocket CORS — `checkWSOrigin` rejects cross-origin browser requests
- ✅ Session files — written with mode `0600`
- ✅ Telegram context timeout — `Chat`/`ChatStream` use `cfg.TimeoutSeconds`
- ✅ Config hot-reload — removed vars are unset via `os.Unsetenv`
- ✅ `--check` token — shows auth mode and last 4 chars of token (`****xxxx`)
- ✅ **Gateway auth modes** — `gateway.auth.mode` supports `"token"` (default, auto-generates and persists a random 64-char hex token if unset), `"none"` (explicit opt-out), `"trusted-proxy"` (delegate to upstream)
- ✅ File/shell sandboxing — opt-in `tools.files.allowPaths` and `tools.exec.denyCommands`

### Compatibility & Reliability

- **Session format incompatibility** — GopherClaw writes a simpler JSONL format than OpenClaw. Use `gopherclaw --migrate` to convert existing OpenClaw sessions.
- ✅ **OpenClaw session migration** — `gopherclaw --migrate` converts history to GopherClaw format
- ✅ **Telegram 409 conflict** — GopherClaw proactively evicts existing pollers at startup
- ✅ **DuckDuckGo web search** — hardened: decodes redirect URLs, detects rate-limit pages, `result__url` fallback
- ✅ **Control UI** — the `/gopherclaw` page shows status, model info, fallbacks, and active sessions via WebSocket

### OpenClaw Config Compatibility

- **`session.scope: "per-sender"`** — not a recognized value; falls through to "user" scope (same behavior)
- ✅ **`agents.defaults.contextPruning.softTrimRatio`** — GopherClaw now reads `softTrimRatio` from both `contextPruning.softTrimRatio` (OpenClaw path) and `agents.defaults.softTrimRatio` (GopherClaw path); the nested path is preferred if both are set.
- **`providers` section** — GopherClaw reads OpenRouter/OpenAI API keys from `providers.<name>.apiKey`, not from env vars. Add a `providers` block to use non-Copilot providers.
- **CLI-backed subagents** (`coding-agent`, `codex`, `claude-code`) — not auto-discovered from `~/.openclaw/agents/`; must be added to `agents.list` with `cliCommand`/`cliArgs` to use them.
- ✅ **`logging.consoleLevel`** — now applied; console sink uses `consoleLevel`, file sink uses `level`. If `consoleLevel` is empty it defaults to `level`.
- ✅ **`logging.redactSensitive`** — wired; when set, logged at startup as acknowledgment. Tool output is not logged in normal operation, so the field suppresses nothing currently but is active for future logging additions.
- ✅ **`tools.exec.backgroundMs`** — wired to `ExecTool.BackgroundWait`; when a command runs longer than `backgroundMs` ms, the partial output collected so far is returned with a `[...still running in background]` suffix and the process continues in the background.
- ✅ **`telegram.historyLimit`** — enforced via `session.Manager.TrimMessages()` called before each Telegram chat turn; trims JSONL to the last N messages when the session exceeds the limit.
- **`channels.telegram.groups.<id>`** — per-group config entries are ignored; only the `Groups["*"]` wildcard is consulted as a legacy fallback. Use `groupPolicy` instead.
- **`agents.defaults.heartbeat`** — config parsing is fully implemented (`HeartbeatConfig` struct with all fields: `every`, `activeHours`, `target`, `model`, `prompt`, `ackMaxChars`, `lightContext`, `directPolicy`). Helper methods exist: `HeartbeatEnabled()`, `HeartbeatPrompt()`, `HeartbeatAckMaxChars()`. However, the heartbeat scheduler/delivery loop is not implemented — no periodic heartbeat turns are actually fired. The `directPolicy` field is parsed but unused.
- **`session.parentForkMaxTokens`** — parsed but unused; GopherClaw does not have Slack thread-based session forking. The field is accepted for config compatibility with OpenClaw v2026.2.25.

### New Feature Parity Gaps (found 2026-03-02 vs OpenClaw v2026.3.1)

- ✅ **Gateway: `/healthz`, `/ready`, `/readyz` health endpoints** — `/healthz`, `/ready`, `/readyz` registered as aliases for `/health` in the chi router (no auth required).
- ✅ **Cron: `delivery.mode: "none"`** — `delivery.mode: "none"` explicitly suppresses channel delivery of cron output; state tracks `lastDeliveryStatus: "suppressed"`.
- ✅ **Agents/Thinking: `adaptive` default level** — `thinking.level` field added (`"off"`, `"enabled"`, `"adaptive"`); Claude 4.6 models default to adaptive thinking when level is unset. Legacy `thinking.enabled` field still works.
- ✅ **Agents/Thinking: fallback with `think=off`** — Anthropic provider retries with `thinking: null` when the API rejects a thinking level (400 + "thinking" in error message), preventing hard failure in fallback chains.
- ✅ **Cron: lightweight bootstrap context** — `agents.defaults.heartbeat.lightContext` and per-job `lightContext` flag; when set, cron runner uses `ChatLight()` which injects only identity + date/time + `HEARTBEAT.md`, skipping full workspace/skills.
- ✅ **`OPENCLAW_SHELL` env marker** — `OPENCLAW_SHELL=1` set in exec tool environment for host commands (not sandbox), so shell config can detect OpenClaw/GopherClaw contexts.
- ✅ **SSRF: RFC2544 range exemption** — `198.18.0.0/15` (RFC 2544 benchmark range) explicitly exempted in `isPrivateOrReservedIP()` and both `checkSSRF` and `SSRFSafeTransport` dial-time checks.

### Feature Parity Gaps (from audit 2026-02-26)

Gaps resolved in initial audit:
- ✅ **System events reach all channels** — `gateway.AddDeliverer()` wires Telegram, Discord, and Slack. All three bots implement `SendToAllPaired`, so `/gopherclaw/system/event` delivers to all paired users across all channels.
- ✅ **Slack pairing** — Slack now supports `/pair <code>` with persistence to `~/.openclaw/credentials/slack-default-allowFrom.json`. Static `allowUsers` entries are pre-populated into the paired set for system event delivery.
- ✅ **Config hot-reload reconnects channel bots** — when a bot token or Slack socket key changes during hot-reload, the affected bot is stopped and reconnected with the new credentials automatically.
- ✅ **Usage display shows input and output tokens** — the `📊` message now displays `~N in / ~M out tokens` for both input and output token counts.

### New Feature Parity Gaps (found 2026-02-26 vs OpenClaw v2026.2.25)

- ✅ **Security: `checkPathAllowed` symlink/hardlink escape** — `internal/tools/files.go:checkPathAllowed` now resolves symlinks via `filepath.EvalSymlinks` (with graceful fallback for not-yet-created paths) before boundary checking, preventing symlink/hardlink escapes outside allowed paths.
- ✅ **Security: SSRF guard on `web_fetch`** — `internal/tools/web.go:WebFetchTool.Run` now performs a DNS-based IP preflight check that blocks private, loopback, link-local, multicast (including IPv6 `ff00::/8`), and unspecified addresses before making HTTP requests.
- ✅ **Security: Slack `isAuthorized` case-insensitive** — `internal/channels/slack/bot.go:isAuthorized` now uses `strings.EqualFold` for user ID comparisons, matching OpenClaw's case-insensitive behavior.
- ✅ **Config: `agents.defaults.heartbeat.directPolicy`** — parsed into `HeartbeatConfig.DirectPolicy` for config compatibility; not implemented (heartbeat delivery is not yet supported).
- ✅ **Config: `session.parentForkMaxTokens`** — parsed into `Session.ParentForkMaxTokens` for config compatibility; not implemented (GopherClaw does not have thread sessions).

### Streaming tool call accumulation fix (2026-02-26)

- ✅ **GitHub Copilot phantom slot bug** — Copilot maps Anthropic content-block indices directly as OpenAI `index` fields (0=text, 1=tool_use). Expanding the `toolCalls` slice for index=1 created a phantom empty slot at index=0. Fix: filter out slots with empty name AND empty arguments after accumulation; also update `toolCalls` to the filtered slice before calling `executeTools` to prevent orphaned tool result messages.
- ✅ **`agents.defaults.models` alias resolution** — `config.ResolveModelAlias()` resolves short aliases (e.g. `"sonnet"`) to full model IDs (`"github-copilot/claude-sonnet-4.6"`) using the `agents.defaults.models` map. Used in the cron runner so per-job `payload.model: "sonnet"` works correctly.

### Architecture Improvements (2026-02-26)

- ✅ **`errgroup` lifecycle** — all long-lived services (`gateway`, `telegram`, `discord`, `slack`, `cron`, `reload.Watch`) launch via `errgroup.WithContext`; first error cancels all others and propagates to main. Fire-and-forget goroutines (`authMgr.StartRefresher`, `sessionMgr.StartResetLoop`) remain as background tasks.
- ✅ **Hot-reload config race fix** — `processMessages()` and `handleText()` in each channel bot now snapshot `cfg`/`msgCfg` under the mutex before processing, preventing data races when hot-reload writes new config concurrently.
- ✅ **Cron live scheduling** — `Add()` and `SetEnabled(id, true)` now schedule jobs immediately if `Start()` has been called, fixing a bug where API-created or re-enabled jobs wouldn't run until restart.
- ✅ **`//go:embed` for control UI** — 120 lines of inline HTML/CSS/JS extracted to `internal/gateway/ui.html` and embedded at compile time via `//go:embed`.
- ✅ **Async delegate feedback loop** — CLI subagents run via `taskqueue.Manager`; raw result announced immediately to the session, then fed through the main agent's `Chat()` so it can react (summarize, retry, follow up). Main agent response arrives as a second announcement.
- ✅ **Delegate task status** — `delegate` tool supports `action: "status"` to report tasks via centralized `taskqueue.Manager` (task ID, agent ID, status, message preview).
- ✅ **Centralized task queue** — `internal/taskqueue/` provides `Manager` with persistence, cancellation, backpressure (semaphore-gated concurrency), and result retention with automatic pruning. `DelegateTool` and `gateway.Server` both use `TaskMgr` instead of ad-hoc internal tracking.
- ✅ **CLI agent PATH resolution** — `NewCLIAgent` resolves bare command names via `exec.LookPath` at construction time; systemd service PATH updated to include `%h/.local/bin`.

### Code Review Bug Fixes (2026-02-26)

**Race conditions:**
- ✅ **`Agent.loadMemoryCached()` race** — `memoryCache`/`memoryMtime` now protected by `memoryMu sync.Mutex`; concurrent `Chat()` calls no longer race on the memory cache.
- ✅ **Telegram `paired` map race** — `shouldRespond()` DM pairing check now reads `b.paired` under `b.mu` lock, matching the `"allowlist"` case.
- ✅ **Cron timer vs `ctx.Done()` race** — `time.AfterFunc` callbacks now check `ctx.Err()` before running, preventing zombie job executions after shutdown.

**Security:**
- ✅ **SSRF DNS rebinding** — `web_fetch` now uses a custom `http.Transport` with `DialContext` that validates resolved IPs against SSRF rules before connecting, closing the TOCTOU gap between `checkSSRF()` and the actual TCP connection.
- ✅ **Auth token timing attack** — gateway auth middleware uses `crypto/subtle.ConstantTimeCompare` instead of `!=` for token comparison.
- ✅ **`memory_get` path traversal** — the default case in `MemoryGetTool.Run()` now validates the resolved path stays within the workspace via `filepath.Abs` + `strings.HasPrefix`.

**Logic bugs:**
- ✅ **Context key type mismatch** — `handleToolInvoke` was setting session key with `sessionKeyCtx{}` but tools looked it up with `tools.SessionKeyContextKey{}`; now uses the correct exported type, fixing browser tool session isolation.
- ✅ **Streaming cancel leak** — `Router.ChatStream()` now wraps successful streams in `cancelOnCloseStream`, calling the timeout context's `cancel()` when the stream is closed instead of leaking it.
- ✅ **Discord `State.User` nil check** — `Start()` now checks for nil `State`/`State.User` after `Open()` instead of panicking.
- ✅ **CLI agent warning false positive** — replaced `cli.Command() == def.CLICommand` heuristic with `os.Stat` existence check.
- ✅ **`isExitError` wrapped errors** — `cli_agent.go` now uses `errors.As` instead of type assertion, correctly matching wrapped `*exec.ExitError`.

**Config defaults:**
- ✅ **Browser headless default** — `Headless` is now `*bool` with `IsHeadless()` helper; defaults to `true` when enabled, users can explicitly set `false` for headed mode. Previous logic was dead code.
- ✅ **Daily reset at midnight** — `AtHour` is now `*int`; `atHour: 0` (midnight) is no longer silently overridden to 4 AM.

**Error visibility:**
- ✅ **Nil logger panic** — `log.L` initialized to `zap.NewNop().Sugar()` instead of `nil`; any logging before `Init()` silently no-ops instead of panicking.
- ✅ **`AppendMessages` errors logged** — `Chat()` and `ChatStream()` now log warnings instead of silently discarding session persistence failures.
- ✅ **Session/cron load errors logged** — `session.Manager` and `cron.Manager` log warnings on corrupted JSON instead of silently starting with empty state.
- ✅ **`flushSave` errors logged** — session metadata write failures (marshal + I/O) now logged instead of silently dropped.

**Resource leaks:**
- ✅ **Browser pool reaper shutdown** — `BrowserPool.idleReaper()` goroutine now exits via `done` channel when `CloseAll()` is called.
- ✅ **Exec deny-list documented** — added comment noting substring check is defense-in-depth only; real security boundary is Docker sandbox.
- ✅ **Background exec documented** — added comment clarifying fire-and-forget design is intentional.

---

## Cutover Checklist

**Cutover completed 2026-02-26.** GopherClaw is now the active installation.

- [x] Test Telegram end-to-end (send `/pair`, then a message) — verified 2026-02-26
- [x] Verify tools work (exec tools confirmed via webhook stress tests and Telegram)
- [x] Stop OpenClaw: `systemctl --user stop openclaw-gateway`
- [x] Clear sessions: `rm ~/.openclaw/agents/main/sessions/*.jsonl`
- [x] Start GopherClaw: `systemctl --user start gopherclaw-gateway`
- [x] Test Telegram on port 18789 — verified, bot running on 127.0.0.1:18789
- [x] Test HTTP gateway (`curl .../health`) — `{"status":"ok","version":"<build-version>"}`
