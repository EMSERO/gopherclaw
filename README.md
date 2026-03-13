# GopherClaw

[![CI](https://github.com/EMSERO/gopherclaw/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/EMSERO/gopherclaw/actions/workflows/ci.yml)

Go port of [OpenClaw](https://github.com/openclaw/openclaw) — a personal AI assistant gateway that runs as a systemd service, connects Telegram/HTTP to AI models (GitHub Copilot, Anthropic, OpenAI, OpenRouter), manages sessions, and executes skills (tools).

Single 15MB static binary with multi-agent orchestration, multi-channel support, and self-update.

Requires Go 1.26+.

---

## Quick Start

```bash
# Install from GitHub Releases (deb)
curl -LO https://github.com/EMSERO/gopherclaw/releases/latest/download/gopherclaw_0.1.0_linux_amd64.deb
sudo dpkg -i gopherclaw_0.1.0_linux_amd64.deb

# Or download a tarball
# https://github.com/EMSERO/gopherclaw/releases

# First-time setup
gopherclaw init

# Or migrate from OpenClaw
gopherclaw --migrate

# Run
gopherclaw

# As a systemd user service (Linux)
systemctl --user enable --now gopherclaw

# Update
gopherclaw update
```

---

## Vision

OpenClaw works, but it carries framework weight, loose dependency hygiene, and is hard to audit. GopherClaw rebuilds the same product with different priorities:

**Own the stack, minimize the attack surface.**
Every dependency is a liability. GopherClaw uses the smallest set of well-maintained packages that do the job — chi for routing, zap for logging, gorilla/websocket, go-openai, telebot — and nothing else. No ORM, no megaframework, no 40-package transitive tree hiding CVEs.

**Security is not an afterthought.**
- govulncheck on every PR and release gate (reachability-aware Go vulnerability scanning)
- gosec static analysis for security anti-patterns
- golangci-lint with errcheck, staticcheck, and unused — 0 issues
- SBOM (CycloneDX) generated for every release artifact
- Explicit error handling on every Close, Write, and deferred cleanup
- Minimal external attack surface: few dependencies, no inbound connections initiated by the binary, loopback-only gateway by default

**Modern Go, idiomatic and auditable.**
Targeting Go 1.26 means the Green Tea GC, faster stdlib, and access to `go fix` modernizers. The codebase uses stdlib patterns (`errors.Is`, `strings.Cut`, `for range N`, `any`) rather than invented abstractions. Less code, fewer places for bugs to hide.

**Single binary, zero install.**
`go build` produces one self-contained binary. No Python runtime, no Node, no containers required to run your own AI assistant. Copy it to a server, point systemd at it, done.

---

## Repository Layout

```
gopherclaw/
  cmd/gopherclaw/main.go          # entry point — wire and run
  cmd/gopherclaw-mcp/main.go      # MCP server binary (stdio transport)
  internal/
    config/      config.go         # config.json → Go structs + defaults
    log/         log.go            # zap structured logger
    auth/        copilot.go        # GitHub Copilot token exchange + cache
    models/      client.go         # OpenAI-compatible HTTP client (streaming)
                 router.go         # primary/fallback model selection
                 registry.go       # BuildProviders: github-copilot, anthropic, openai-compat
                 anthropic.go      # native Anthropic messages API (extended thinking)
    session/     manager.go        # JSONL history, TTL, pruning, ReplaceHistory
    skills/      loader.go         # discover SKILL.md, parse YAML frontmatter
    memory/      memory.go         # LoadMemoryMD — inject MEMORY.md into system prompt
    agent/       agent.go          # conversation loop, soft-trim, model overrides, DefaultTools
                 delegate.go       # DelegateTool + Chatter interface
                 dispatch.go       # DispatchTool — orchestrator task graph execution
                 cli_agent.go      # CLIAgent — subprocess-backed subagents (e.g. claude -p)
    orchestrator/
                 dispatcher.go     # parallel task execution, dependency ordering, failure handling
                 graph.go          # TaskGraph, TaskResult, ResultSet types
                 interpolate.go    # {{task-id.output}} substitution
                 progress.go       # ProgressFunc callback
    tools/       exec.go           # exec tool (bash -c) + Docker sandbox
                 confirm.go        # destructive command confirmation (blocklist + prompts)
                 web.go            # web_search (DDG) + web_fetch tools + SSRF protection
                 files.go          # read_file, write_file, list_dir
                 memory.go         # memory_append, memory_get tools
                 browser.go        # BrowserTool + BrowserPool (chromedp)
                 notify.go         # notify_user — proactive message delivery
                 eidetic.go        # eidetic_search, eidetic_append — semantic memory tools
    eidetic/     client.go         # Eidetic memory sidecar client (HTTP/MCP)
    hooks/       hooks.go          # lifecycle event bus (15 event types)
    security/    audit.go          # `gopherclaw security` CLI audit command
    channels/
      common/    chunker.go        # SmartChunk — code-fence-aware message splitting
                 retry.go          # RetrySend — exponential backoff with plain-text fallback
    commands/    commands.go        # unified slash command handler (all channels)
    cron/        cron.go            # lightweight job scheduler
    gateway/     server.go          # chi HTTP server + auth middleware
                 ratelimit.go       # per-IP token-bucket rate limiting middleware
                 chat.go            # POST /v1/chat/completions, GET /v1/models
                 webhook.go         # POST /webhooks/{session}
                 logstream.go       # SSE log streaming endpoint
                 ui.go              # control UI + WebSocket
                 system.go          # system event endpoint
    channels/
      telegram/  bot.go             # Telegram bot (telebot v3)
      discord/   bot.go             # Discord bot (discordgo)
      slack/     bot.go             # Slack Socket Mode bot (slack-go)
    migrate/     migrate.go         # convert OpenClaw sessions + config
    initialize/  init.go            # gopherclaw init wizard
    updater/     updater.go         # self-update from GitHub Releases
    reload/      watcher.go         # fsnotify config hot-reload
    taskqueue/   manager.go         # background task queue with concurrency, persistence, pruning
    agentapi/    agentapi.go        # Deliverer interface for cross-channel message delivery
  .github/workflows/
    ci.yml                          # test + lint + govulncheck on PRs
    release.yml                     # test + lint + govulncheck + goreleaser + SBOM on version tags
  .goreleaser.yml                   # multi-platform release config
  specs/                            # feature specifications
  go.mod
  README.md
```

---

## Data Directory

GopherClaw stores all runtime data under `~/.gopherclaw/`:

| Path | Purpose |
|------|---------|
| `~/.gopherclaw/config.json` | Configuration |
| `~/.gopherclaw/agents/main/sessions/` | Session JSONL files |
| `~/.gopherclaw/workspace/skills/*/SKILL.md` | Installed skills |
| `~/.gopherclaw/workspace/agents/orchestrator/` | Orchestrator identity |
| `~/.gopherclaw/workspace/*.md` | Workspace docs (injected into system prompt) |
| `~/.gopherclaw/logs/gopherclaw.log` | Log file |
| `~/.gopherclaw/state/update-check.json` | Version check cache |
| `~/.gopherclaw/credentials/` | Channel tokens |
| `~/.gopherclaw/tasks.json` | Background task queue state |
| `~/.gopherclaw/cron/jobs.json` | Cron job definitions |
| `~/.gopherclaw/cron/runs/` | Per-job JSONL run history |

**OpenClaw compatibility:** `gopherclaw --migrate` copies config and sessions from `~/.openclaw/`. Some backward-compatible state still reads from `~/.openclaw/` paths: pairing credentials (`~/.openclaw/credentials/`), simple cron format (`~/.openclaw/agents/main/crons.json`).

---

## Build & Run

```bash
# Build from source
go build -o gopherclaw ./cmd/gopherclaw

# First-time setup
gopherclaw init

# Verify auth and config
gopherclaw --check

# Run
gopherclaw

# As a systemd user service (Linux)
systemctl --user enable --now gopherclaw

# As a launchd service (macOS)
cp gopherclaw.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/gopherclaw.plist
```

---

## Multi-Agent Orchestration

GopherClaw supports a three-tier agent hierarchy:

1. **Main agent** — handles simple requests directly, routes complex tasks to the orchestrator
2. **Orchestrator** — plans work as a JSON task graph, dispatches to specialists, synthesizes results
3. **Specialists** — subagents (in-process or CLI) that execute individual tasks

The dispatcher runs independent tasks in parallel via goroutines, respects dependencies (topological sort), handles partial failures (blocking vs. best-effort), and enforces max concurrency.

---

## Slash Commands

| Command | Description |
|---------|-------------|
| `/help` | Show available commands |
| `/new`, `/reset` | Clear session and start fresh |
| `/compact` | Compress session history to save context |
| `/model` | Show current model |
| `/model <name>` | Switch model (e.g. `/model sonnet`) |
| `/context` | Show session size and token estimate |
| `/export` | Dump full conversation history |
| `/cron` | Manage scheduled jobs |

---

## MCP Server (`gopherclaw-mcp`)

GopherClaw ships a standalone MCP (Model Context Protocol) server binary that exposes GopherClaw's tools to any MCP-compatible client — Claude Code, Claude Desktop, Cursor, Windsurf, etc.

### Available Tools

| Tool | Description | Requires |
|------|-------------|----------|
| `browser_navigate` | Navigate to a URL, return page text | `tools.browser.enabled` |
| `browser_screenshot` | Capture page as PNG (returned as base64 image) | `tools.browser.enabled` |
| `browser_click` | Click element by CSS selector | `tools.browser.enabled` |
| `browser_type` | Type text into an input element | `tools.browser.enabled` |
| `browser_eval` | Execute JavaScript and return result | `tools.browser.enabled` |
| `browser_scrape` | Scrape elements by CSS selector | `tools.browser.enabled` |
| `browser_snapshot` | Accessibility snapshot (title, URL, interactive elements) | `tools.browser.enabled` |
| `browser_links` | List all links on the page | `tools.browser.enabled` |
| `browser_text` | Get full page text content | `tools.browser.enabled` |
| `browser_cookies` | Get cookies for current page | `tools.browser.enabled` |
| `eidetic_search` | Semantic memory search (hybrid keyword + vector) | `eidetic.enabled` |
| `eidetic_append` | Store a memory entry (max 4000 chars) | `eidetic.enabled` |
| `notify_user` | Send notification to user's active channel | `gateway.port > 0` |
| `web_search` | Search the web (DuckDuckGo) | always |
| `web_fetch` | Fetch a web page as text (with SSRF protection) | always |
| `read_file` | Read file contents (scoped to workspace) | always |
| `write_file` | Write/create a file (scoped to workspace) | always |
| `list_dir` | List directory contents (scoped to workspace) | always |
| `memory_append` | Append to workspace MEMORY.md | `agents.defaults.memory.enabled` |
| `memory_get` | Read workspace MEMORY.md | `agents.defaults.memory.enabled` |

### Setup with Claude Code

Add to `~/.claude/claude_code_config.json`:

```json
{
  "mcpServers": {
    "gopherclaw": {
      "command": "/usr/bin/gopherclaw-mcp",
      "args": ["--config", "~/.gopherclaw/config.json"]
    }
  }
}
```

### Setup with Claude Desktop

Add to `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "gopherclaw": {
      "command": "/usr/bin/gopherclaw-mcp",
      "args": ["--config", "/home/you/.gopherclaw/config.json"]
    }
  }
}
```

### Configuration

`gopherclaw-mcp` reads the same `~/.gopherclaw/config.json` as the main binary. Tools are auto-registered based on what's enabled in config:

- **Browser tools** require `tools.browser.enabled: true` and Chrome/Chromium installed
- **Eidetic tools** require `eidetic.enabled: true` and the eidetic sidecar running
- **Notify tool** requires `gateway.port` > 0 (routes through the gateway HTTP API)
- **File tools** are scoped to `agents.defaults.workspace` if set (otherwise unrestricted)
- **Web tools** and **memory tools** are always available (if memory is enabled in config)

Logs are written to `~/.gopherclaw/logs/gopherclaw-mcp.log`.

---

## Key Technical Notes

### GitHub Copilot API
- Token exchange: `GET https://api.github.com/copilot_internal/v2/token`
- API base URL: `https://api.enterprise.githubcopilot.com` (no `/v1` prefix)
- **Must use HTTP/1.1** — HTTP/2 to this endpoint times out
- Required headers: `Authorization: Bearer <token>`, `Editor-Version: vscode/1.85.1`, `Copilot-Integration-Id: vscode-chat`

### Session Keys

Session key format depends on `session.scope` in config:

| Scope | Telegram | Discord | Slack |
|-------|----------|---------|-------|
| `user` (default) | `main:telegram:<senderID>` | `main:discord:<userID>` | `main:slack:<userID>` |
| `channel` | `main:telegram:channel:<chatID>` | `main:discord:channel:<channelID>` | `main:slack:channel:<channelID>` |
| `global` | `main:telegram:global` | `main:discord:global` | `main:slack:global` |

---

## Security Model

GopherClaw is a **personal assistant** designed to run as the operator and used by a small number of trusted users. It is not hardened for multi-tenant or adversarial environments.

**The agent has full shell access.**
The `exec` tool runs arbitrary commands via `bash -c`. The `read_file` and `write_file` tools have no path restrictions. This is intentional: a capable personal assistant needs real access to the machine.

**Destructive command confirmation.**
A built-in blocklist of dangerous patterns (e.g. `rm -rf`, `dd`, `shutdown`, `curl | bash`) intercepts dangerous commands before execution. Matching commands require explicit user confirmation via the active channel (Telegram inline keyboard, terminal stdin, or dashboard modal). Unconfirmed commands are auto-blocked after a configurable timeout (default 60s). In non-interactive contexts (cron, webhooks), dangerous commands are hard-blocked entirely.

**SSRF protection.**
The `web_fetch` tool performs DNS-based IP preflight checks that block requests to private, loopback, link-local, and multicast addresses. A custom `DialContext` transport prevents DNS rebinding attacks.

**Gateway auth.**
The HTTP gateway requires a bearer token (`gateway.auth.token` in config). If not configured, a random token is auto-generated and saved on first run. Keep the gateway on loopback or ensure a token is always set.

**Rate limiting.**
Optional per-IP rate limiting (`gateway.rateLimit.rps`) protects the gateway from abuse. Requests exceeding the limit receive `429 Too Many Requests`.

**Telegram pairing requires a code.**
At startup, a random 6-digit pairing code is printed to the log. Users must send `/pair <code>` to the bot. Paired users persist across restarts.

---

## Acknowledgements

GopherClaw is a reimplementation inspired by [OpenClaw](https://github.com/openclaw/openclaw), which is MIT-licensed. GopherClaw shares no source code with OpenClaw; it reads the same configuration and session formats for drop-in compatibility.
