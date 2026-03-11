# Architecture

## Component Overview

```
┌─────────────────────────────────────────────────────────┐
│                    GitHub Actions CI                     │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌────────┐  │
│  │ go test  │  │golangci  │  │  Trivy   │  │gorele- │  │
│  │ coverage │  │  -lint   │  │  scan    │  │aser    │  │
│  └──────────┘  └──────────┘  └──────────┘  └────────┘  │
└─────────────────────────────────────────────────────────┘
                              │
                              ▼
                    GitHub Releases
          ┌─────────────────────────────────┐
          │  linux-amd64  linux-arm64       │
          │  darwin-amd64 darwin-arm64      │
          │  .deb  .rpm  checksums.txt      │
          │  launchd.plist (macOS archive)  │
          └─────────────────────────────────┘

┌─────────────────────────────────────────────────────────────┐
│                     gopherclaw binary                        │
│                                                              │
│  ┌─────────────────────┐  ┌──────────────────────────────┐  │
│  │   cmd/gopherclaw    │  │     internal/updater         │  │
│  │   - init subcommand │  │  - startup version check     │  │
│  │   - update subcmd   │  │  - cache management          │  │
│  │   - rollback subcmd │  │  - self-replace binary       │  │
│  │   - --version flag  │  │  - backup + rollback         │  │
│  └─────────────────────┘  │  - syscall.Exec re-exec      │  │
│                            └──────────────────────────────┘  │
│  ┌──────────────────────────────────────────────────────┐   │
│  │               internal/tools/exec.go                 │   │
│  │  - command blocklist (built-in substring patterns)    │   │
│  │  - deny-list patterns via config (DenyCommands)      │   │
│  │  - confirmation prompt dispatch per channel          │   │
│  │  - Telegram: inline Yes/No keyboard buttons          │   │
│  │  - Terminal: stdin prompt; Dashboard: modal dialog   │   │
│  │  - auto-block on timeout (configurable, default 60s) │   │
│  │  - block + log when no confirmation channel          │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
│  ┌──────────────────────────────────────────────────────┐   │
│  │           internal/models/router.go                  │   │
│  │  - primary model + ordered fallback chain            │   │
│  │  - automatic fallback on failure (429, 5xx, timeout) │   │
│  │  - per-attempt logging (WARN on fallback)            │   │
│  │  - per-model cooldowns with exponential backoff      │   │
│  │  - per-provider rate limiting                        │   │
│  │  - retry with exponential backoff + jitter per model │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
│  NOTE: capability-based task-type routing is PROPOSED but     │
│  NOT YET IMPLEMENTED. The router currently uses a simple     │
│  primary + fallback chain without task-type awareness.       │
│                                                              │
│  ┌──────────────────────────────────────────────────────┐   │
│  │           internal/orchestrator/                     │   │
│  │  - semaphore: maxConcurrent (default 5)              │   │
│  │  - dependency-aware task graph execution             │   │
│  │  - topological sort with cycle detection             │   │
│  │  - blocking/non-blocking dependency semantics        │   │
│  └──────────────────────────────────────────────────────┘   │
│  ┌──────────────────────────────────────────────────────┐   │
│  │           internal/taskqueue/                        │   │
│  │  - semaphore: maxConcurrent (default 5)              │   │
│  │  - persistent task state: tasks.json (survives restart)│  │
│  │  - task cancellation (single + per-session)          │   │
│  │  - progress announcements to channel bots            │   │
│  │  - completed task pruning loop                       │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
│  ┌──────────────────────────────────────────────────────┐   │
│  │           internal/skills/ (loader + watcher)        │   │
│  │  - fsnotify hot-reload of SKILL.md metadata          │   │
│  │  - enable/disable state per skill                    │   │
│  │  - unverified skill flagging (non-CrawHub origin)    │   │
│  │  - skill update check via CrawHub (once/day)         │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
│  ┌──────────────────────────────────────────────────────┐   │
│  │           internal/gateway/ui.go (dashboard)         │   │
│  │  - status panel (version, uptime, model, sessions)   │   │
│  │  - skills panel (list, verified, enable/disable)     │   │
│  │  - orchestrator panel (queue depth, task history)    │   │
│  │  - logs tail (live, filterable by level)             │   │
│  │  - version history + rollback button                 │   │
│  │  NOTE: model panel with recommendations/costs is     │   │
│  │  PROPOSED (not yet implemented)                      │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
│  ┌──────────────────────────────────────────────────────┐   │
│  │           internal/config/ (migration)               │   │
│  │  - versioned migration table                         │   │
│  │  - auto-run on startup when version changes          │   │
│  │  - backup config.json → config.json.bak before run   │   │
│  │  - auto-restore config.json.bak on migration failure │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
│  ┌──────────────────────────────────────────────────────┐   │
│  │               internal/init                          │   │
│  │  - detect ~/.openclaw/ → prompt migrate              │   │
│  │  - interactive credential wizard                     │   │
│  │  - CrawHub skill picker                              │   │
│  │  - write ~/.gopherclaw/config.json                   │   │
│  └──────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────┐
│               ~/.gopherclaw/ (runtime data)              │
│                                                          │
│  config.json                                             │
│  config.json.bak              ← pre-migration backup     │
│  state/update-check.json      ← version check cache      │
│  state/skill-states.json      ← enabled/disabled skills  │
│  tasks.json                   ← background task state      │
│  workspace/skills/            ← installed skills          │
│  workspace/.clawhub/lock.json ← installed skill versions │
│  agents/main/sessions/        ← session history           │
│  credentials/                 ← channel tokens            │
│  gopherclaw.bak               ← previous binary (rollback)│
└─────────────────────────────────────────────────────────┘

External Dependencies:
  CrawHub API         ←→  skill install/list + update checks (proposed)
  GitHub Releases API ←→  version check + binary download (proposed)
```

---

## CI Workflows

### `ci.yml` — runs on every PR and push to main

```
trigger: push/PR to main
jobs:
  test:
    - go test ./... -coverprofile=coverage.out
    - check coverage ≥ 78%
  lint:
    - golangci-lint run
```

### `release.yml` — runs on version tags (`v*`)

```
trigger: tag push v*
jobs:
  test:        (same as ci.yml — must pass before release)
  security:
    - trivy fs . --severity HIGH,CRITICAL
  release:
    - goreleaser release
```

---

## goreleaser Configuration

**Targets:**
- `linux/amd64`, `linux/arm64`
- `darwin/amd64`, `darwin/arm64`

**Archives:**
- Linux: `.tar.gz` + `.deb` + `.rpm`
- macOS: `.tar.gz` containing binary + `gopherclaw.plist` (launchd)

**Extras:**
- `checksums.txt` (SHA256 for all artifacts)
- GitHub Release notes auto-generated from commit history

---

## Exec Tool Safeguards

```
User/Skill invokes exec("rm -rf /tmp/foo")
      │
      ├─ exec.go: match against built-in blocklist + deny-list patterns
      │     ├─ NO MATCH → execute immediately (unchanged behavior)
      │     └─ MATCH → dangerous command detected
      │
      ├─ is confirmation channel available? (active Telegram/Discord/terminal/UI)
      │     ├─ NO → BLOCK, log WARN, notify on next available channel
      │     └─ YES → send confirmation prompt to user
      │                 Telegram: inline keyboard [✅ Yes] [❌ No]
      │                 Terminal: "Confirm? [y/N]" stdin prompt
      │                 Dashboard: modal dialog with Yes/No buttons
      │
      ├─ wait for response (timeout: 60s configurable)
      │     ├─ YES within timeout → execute
      │     ├─ NO within timeout → block, log
      │     └─ TIMEOUT → auto-block, log WARN
      └─ result returned to caller
```

**Built-in blocklist patterns (initial, not exhaustive):**
- `rm -rf`, `rm -r /`, `rm --no-preserve-root`
- `dd if=`, `mkfs`, `fdisk`, `parted`
- `shutdown`, `reboot`, `halt`, `poweroff`, `init 0`, `init 6`
- `userdel`, `passwd`, `usermod`
- `chmod -R 777 /`, `chown -R /`
- `iptables -F`, `ufw reset`
- `systemctl disable`, `systemctl stop` (system-critical services)
- `kill -9 1`, `pkill -9 -1`
- `curl ... | bash`, `curl ... | sh`, `wget -O- ... | bash`
- `> /dev/sda`, `cat /dev/zero >`

**Note:** The `ExecTool.DenyCommands` field supports additional deny patterns
set at initialization. A user-extensible `customBlocklist` via `config.json`
is PROPOSED but NOT YET IMPLEMENTED. Currently, deny patterns are set
programmatically, not via config file.

**Confirmation timeout** is configurable via `ExecTool.ConfirmTimeout`
(default 60s). The `ExecConfirmer` interface enables per-channel confirmation
prompts (Telegram inline keyboard, etc.).

---

## Model Router

```
Request arrives at agent
      │
      ├─ router.go: build model list [primary, fallback[0], fallback[1], ...]
      │     - skip models currently in cooldown
      │     - per-provider rate limiting (if configured)
      │
      ├─ attempt primary model (with per-model timeout, capped at 60s when fallbacks exist)
      │     ├─ SUCCESS → return response
      │     └─ FAIL (error/timeout/quota) → retry up to 2x with exponential backoff
      │           └─ still failing → log WARN, try next fallback
      │
      ├─ fallback[0]: (e.g. github-copilot/claude-sonnet-4.6)
      │     ├─ SUCCESS → return response
      │     └─ FAIL → retry up to 2x, then log WARN, try next
      │
      ├─ fallback[1]: (e.g. github-copilot/gpt-4.1)
      │     ├─ SUCCESS → return response
      │     └─ FAIL → retry up to 2x, then log WARN, try next
      │
      └─ fallback[N]: last resort
            ├─ SUCCESS → return response
            └─ FAIL → return error to caller (all models exhausted)

Note: Capability-based task-type routing (matching task type to best model)
is PROPOSED but NOT YET IMPLEMENTED. The router currently uses a simple
primary + ordered fallback chain regardless of task type.
```

---

## Orchestrator (internal/orchestrator/)

The orchestrator executes task graphs with dependency tracking:

```
Dispatcher receives task graph (N tasks)
      │
      ├─ validate: required fields, duplicate IDs, agent references, dependencies
      │
      ├─ topological sort via Kahn's algorithm (detect cycles → error)
      │
      ├─ for each ready task (dependencies met):
      │     ├─ acquire semaphore slot (maxConcurrent, default 5)
      │     │     ├─ slot available → goroutine: run task via agent.Chat()
      │     │     └─ all slots busy → block until slot frees (or context cancelled)
      │     │
      │     └─ as goroutines complete → release semaphore
      │           → downstream tasks with dependencies met become ready
      │           → blocking failures cascade-cancel dependents
      │
      └─ all tasks complete (or failed/cancelled) → return ResultSet
```

## Task Queue (internal/taskqueue/)

The task queue manages background tasks with persistence:

```
Submit(sessionKey, agentID, message, fn) → task ID
      │
      ├─ record task as "pending" in memory + schedule disk save
      │
      ├─ goroutine: acquire semaphore (maxConcurrent, default 5)
      │     ├─ slot available → mark "running", execute fn(ctx)
      │     └─ blocked → wait for slot
      │
      ├─ on completion: mark success/failed/cancelled, schedule save
      │
      └─ persistence: debounced writes to tasks.json (2s coalesce)
            - on restart, previously-running tasks marked as "failed (interrupted)"
```

**Task state file (tasks.json):**
```json
{
  "version": 1,
  "tasks": [
    {
      "id": "hex-id",
      "agentId": "main",
      "sessionKey": "telegram:12345",
      "message": "...",
      "status": "success",
      "createdAtMs": 1709308800000
    }
  ]
}
```

---

## Skill Lifecycle

```
Startup:
  skills/loader.go → scan workspace/skills/ → parse SKILL.md frontmatter
  → check state/skill-states.json for enabled/disabled state
  → flag non-CrawHub skills as unverified
  → register enabled skills in agent tool registry

Runtime (hot-reload):
  skills/loader.go → fsnotify watch on workspace/skills/
  → SKILL.md change detected → reload metadata for affected skill
  → re-register in tool registry (no restart needed)
  → log INFO: "skill reloaded: <name>"

Enable/Disable:
  /skills enable <name> OR dashboard toggle
  → update state/skill-states.json
  → add/remove from active tool registry immediately
  → log INFO: "skill enabled/disabled: <name>"

Unverified warning:
  → on first invocation of unverified skill
  → log WARN + send one-time warning to active channel
  → does NOT block execution
```

---

## Config Auto-Migration

```
Startup:
  config/config.go → load config.json
  → compare meta.lastTouchedVersion with current binary version
  → if equal → no migration needed, continue
  → if older → identify pending migrations from migration table
      → back up config.json → config.json.bak
      → run each migration function in order (additive only)
      → if ANY migration fails:
          → restore config.json from config.json.bak
          → log ERROR: "config migration failed, restored backup"
          → exit with error (do not start with broken config)
      → update meta.lastTouchedVersion
      → write migrated config.json
      → log INFO: "config migrated from <old> to <new>"
  → continue startup with migrated config
```

**Migration table structure (internal/config/migrate.go):**
```go
var migrations = []Migration{
    {FromVersion: "2026.2.19", Apply: migrateAddOrchestratorQueue},
    {FromVersion: "2026.3.1",  Apply: migrateAddExecConfirmTimeout},
    // ... add new migrations here
}
```

---

## Self-Update and Rollback Flow

```
gopherclaw update  (or autoUpdate: true on startup)
      │
      ├─ download new binary to temp file
      ├─ verify SHA256 against checksums.txt
      │     └─ FAIL → abort, delete temp, log ERROR
      │
      ├─ copy current binary → gopherclaw.bak
      ├─ rename temp → current binary path (atomic)
      │
      └─ re-exec via syscall.Exec (load new binary in-place)
            → new version running

gopherclaw rollback  (or dashboard [Rollback] button)
      │
      ├─ check gopherclaw.bak exists
      │     └─ NOT FOUND → exit with error "no backup available"
      │
      ├─ copy gopherclaw.bak → current binary path (atomic)
      │
      └─ re-exec via syscall.Exec (load old binary in-place)
            → previous version running
            → log INFO: "rolled back to previous version"
```

---

## Dashboard Layout

```
/gopherclaw (gateway port, auth-gated)
┌─────────────────────────────────────────────────────┐
│  GopherClaw Dashboard                               │
│  ┌─────────────────┐  ┌────────────────────────┐   │
│  │  STATUS          │  │  VERSION               │   │
│  │  v2026.3.1 ✅    │  │  Current: 2026.3.1     │   │
│  │  Uptime: 4h 23m  │  │  Backup:  2026.2.28    │   │
│  │  Model: gemini   │  │  Latest:  2026.3.1 ✅  │   │
│  │  Sessions: 3     │  │  [Rollback to backup]  │   │
│  │  Skills: 6/6 ✅  │  └────────────────────────┘   │
│  └─────────────────┘                                │
│  ┌─────────────────────────────────────────────┐   │
│  │  SKILLS                                      │   │
│  │  ✅ calendar-manager     [CrawHub]  [ON ●]   │   │
│  │  ✅ coding-agent         [CrawHub]  [ON ●]   │   │
│  │  ⚠️  my-custom-skill     [Manual]   [ON ●]   │   │
│  │  ✅ printful-pod         [CrawHub]  [OFF ○]  │   │
│  └─────────────────────────────────────────────┘   │
│  ┌──────────────┐  ┌──────────────────────────┐    │
│  │  ORCHESTRATOR │  │  LOGS                    │    │
│  │  Active: 2/5  │  │  [INFO] skill reloaded   │    │
│  │  Queued: 3    │  │  [WARN] fallback model   │    │
│  │  Done: 47     │  │  [INFO] task completed   │    │
│  └──────────────┘  └──────────────────────────┘    │
└─────────────────────────────────────────────────────┘
```

---

## `gopherclaw init` Flow

```
gopherclaw init
      │
      ├─ detect ~/.openclaw/?
      │     ├─ YES → "Found OpenClaw install. Migrate? [y/N]"
      │     │           ├─ Y → run migrate.MigrateConfig() + migrate.MigrateSessions()
      │     │           │       → skip wizard, go to skill picker
      │     │           └─ N → proceed to fresh wizard
      │     └─ NO → fresh wizard
      │
      ├─ wizard (fresh install only)
      │     - Primary model (OpenRouter key or GitHub Copilot token)
      │     - Telegram bot token? [optional]
      │     - Discord bot token? [optional]
      │     - Slack bot token? [optional]
      │     → write ~/.gopherclaw/config.json
      │
      └─ skill picker
            - fetch skill list from CrawHub API
            - display interactive checklist
            - install selected skills to workspace/skills/
            - update workspace/.clawhub/lock.json
            → "Init complete. Run: gopherclaw start"
```

---

## `internal/updater` Package

**Responsibilities:**
- Check GitHub Releases API: `GET https://api.github.com/repos/EMSERO/gopherclaw/releases/latest`
- Read/write `~/.gopherclaw/state/update-check.json` for throttling (once per 24h)
- Determine platform-appropriate asset name (e.g. `gopherclaw-linux-amd64`)
- Download asset, verify SHA256 against `checksums.txt`
- Replace binary in-place: back up current as `gopherclaw.bak`, write new to temp, rename atomically
- Rollback: copy `gopherclaw.bak` to current binary path atomically
- Re-exec via `syscall.Exec` after update or rollback to load the new/old binary in-place

**Update check state file:**
```json
{
  "lastCheckedAt": "2026-02-28T00:00:00Z",
  "lastAvailableVersion": "2026.3.1",
  "lastNotifiedVersion": "2026.3.1"
}
```

---

## macOS Service (launchd)

Shipped in macOS release archive as `gopherclaw.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" ...>
<plist version="1.0">
<dict>
  <key>Label</key>             <string>com.emsero.gopherclaw</string>
  <key>ProgramArguments</key>  <array><string>/usr/local/bin/gopherclaw</string><string>start</string></array>
  <key>RunAtLoad</key>         <true/>
  <key>KeepAlive</key>         <true/>
  <key>StandardOutPath</key>   <string>/usr/local/var/log/gopherclaw.log</string>
  <key>StandardErrorPath</key> <string>/usr/local/var/log/gopherclaw.err</string>
</dict>
</plist>
```

Install: `cp gopherclaw.plist ~/Library/LaunchAgents/ && launchctl load ~/Library/LaunchAgents/gopherclaw.plist`

---

## Key Technical Decisions

| Decision | Why |
|---|---|
| `internal/updater` as its own package | Isolates update logic; testable independently; reusable by both startup check and `gopherclaw update` command |
| Atomic binary replace (write temp + rename) | Avoids corrupted binary if interrupted mid-download |
| SHA256 verification before replacing binary | Security — don't self-replace with unverified bytes |
| `syscall.Exec` re-exec after update/rollback | New binary takes effect immediately without systemd/launchd intervention |
| CrawHub skill list fetched at init time | No need to bundle skill catalog in binary; always current |
| Throttle update check to 24h | Avoids GitHub API rate limits (60 req/hr unauthenticated) |
| Non-blocking startup check | Check runs in background goroutine; doesn't slow startup |
| Blocklist in exec.go, non-bypassable by skills | Security controls must not be overridable; deny-list for additions |
| DenyCommands field for additional deny patterns | Allows environment-specific hardening at initialization (config-based customBlocklist is proposed, not yet implemented) |
| Telegram inline keyboard for confirmations | Reduces friction to a single tap vs typing "yes" |
| Semaphore-gated orchestrator concurrency | Prevents resource exhaustion; tasks block until slot available |
| Persistent task state (tasks.json via taskqueue) | Background tasks survive restarts; previously-running tasks marked failed on reload |
| Primary + fallback chain model routing | Simple, reliable failover; capability-based routing deferred |
| Per-model cooldowns with exponential backoff | Failed models are temporarily skipped, preventing repeated failures |
| Versioned migration table + rollback on failure | Maintainable, isolated migrations; config never left in broken state |
| Dashboard extends existing ui.html/ui.go | Keeps single-binary architecture; no bundler/React; already has WebSocket + SSE |
| Config migration rollback on failure | Never leave the user with a broken config they can't fix |
