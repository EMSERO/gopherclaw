# Requirements

## CI/CD Pipeline

### REQ-001 — Test Gate
**Priority:** Must  
**Description:** All PRs and merges to main must pass `go test ./...` before merge.  
**Acceptance criteria:** GitHub Actions workflow runs tests; merge blocked if tests fail.

### REQ-002 — Coverage Threshold
**Priority:** Must  
**Description:** Test coverage must be ≥ 78% across the codebase.  
**Acceptance criteria:** Coverage report generated in CI; build fails if below threshold.

### REQ-003 — Lint Gate
**Priority:** Must  
**Description:** `golangci-lint` must pass on every PR.  
**Acceptance criteria:** Lint step in CI workflow; merge blocked on lint failures.

### REQ-004 — Security Scanning
**Priority:** Must  
**Description:** Trivy scans the binary and dependencies for known CVEs on every release.  
**Acceptance criteria:** Trivy scan step in release workflow; HIGH/CRITICAL findings block release.

### REQ-005 — Mock AI Server
**Priority:** Must  
**Description:** Integration tests use a mock AI server — no real API calls in CI.  
**Acceptance criteria:** Tests pass without any API keys configured in CI environment.

### REQ-006 — CI Badge
**Priority:** Should  
**Description:** README shows live CI status badge.  
**Acceptance criteria:** Badge links to GitHub Actions workflow, reflects current main branch status.

---

## Release Artifacts

### REQ-010 — Linux Binaries
**Priority:** Must  
**Description:** goreleaser produces `gopherclaw-linux-amd64` and `gopherclaw-linux-arm64` on every tagged release.  
**Acceptance criteria:** Artifacts attached to GitHub Release; checksums file included.

### REQ-011 — Linux Packages
**Priority:** Must  
**Description:** goreleaser produces `.deb` and `.rpm` packages for Linux.  
**Acceptance criteria:** Packages installable on Ubuntu 22.04+ and Fedora 38+.

### REQ-012 — macOS Binaries
**Priority:** Must  
**Description:** goreleaser produces `gopherclaw-darwin-amd64` and `gopherclaw-darwin-arm64` (Apple Silicon).  
**Acceptance criteria:** Artifacts attached to GitHub Release.

### REQ-013 — macOS launchd Plist
**Priority:** Must  
**Description:** A launchd plist is shipped with the macOS release so GopherClaw can run as a background service.  
**Acceptance criteria:** Plist file in release archive; install instructions in docs.

### REQ-014 — Release Signing / Checksums
**Priority:** Must  
**Description:** All release artifacts include a `checksums.txt` (SHA256).  
**Acceptance criteria:** goreleaser generates checksums automatically.

### REQ-015 — Homebrew Tap
**Priority:** Could  
**Description:** A Homebrew tap (`brew install yourusername/tap/gopherclaw`) for Mac users.  
**Acceptance criteria:** `brew install` succeeds on macOS; tap auto-updates on new release.

---

## First-Run Experience (`gopherclaw init`)

### REQ-020 — OpenClaw Migration Detection
**Priority:** Must  
**Description:** `gopherclaw init` detects `~/.openclaw/` and prompts the user to migrate.  
**Acceptance criteria:** If user confirms, runs existing migrate logic (config + sessions); if declined, proceeds to fresh setup.

### REQ-021 — Interactive Setup Wizard
**Priority:** Must  
**Description:** For fresh installs, `gopherclaw init` interactively collects required credentials and writes a starter `~/.gopherclaw/config.json`.  
**Acceptance criteria:** Config file valid and GopherClaw starts successfully after init completes.

### REQ-022 — Skill Picker
**Priority:** Must  
**Description:** During init, user is presented with a list of skills from CrawHub and can select which to install.  
**Acceptance criteria:** Selected skills installed to `~/.gopherclaw/workspace/skills/`; lock file updated.

### REQ-023 — Manual Skill Drop-in
**Priority:** Must  
**Description:** Skills can be installed by manually placing a directory under `~/.gopherclaw/workspace/skills/` without using the CLI or CrawHub.  
**Acceptance criteria:** Manually dropped skill is loaded and functional on next start; no error if `_meta.json` is absent.

### REQ-024 — Skill Format Compatibility
**Priority:** Must  
**Description:** OpenClaw skills (same `SKILL.md` format) work in GopherClaw without modification.  
**Acceptance criteria:** All 6 existing skills load and function correctly after migration.

---

## Self-Update & Rollback

### REQ-030 — Startup Version Check
**Priority:** Must  
**Description:** On startup, GopherClaw checks GitHub Releases API for a newer version. Check is throttled to once per 24 hours; result cached in `~/.gopherclaw/state/update-check.json`.  
**Acceptance criteria:** Check is non-blocking; startup time unaffected; stale cache used if GitHub API is unreachable.

### REQ-031 — Update Notification
**Priority:** Must  
**Description:** If a newer version is available, user is notified via active channel (Telegram message, terminal log, or gateway UI).  
**Acceptance criteria:** Notification includes current version, new version, and how to update.

### REQ-032 — `gopherclaw update` Command
**Priority:** Must  
**Description:** `gopherclaw update` downloads the latest binary from GitHub Releases and replaces the running binary in-place. The current binary is backed up as `gopherclaw.bak` before replacement.  
**Acceptance criteria:** After running, `gopherclaw --version` reports the new version; original binary exists as `.bak`.

### REQ-033 — Auto-Update Config Option
**Priority:** Should  
**Description:** Config option `autoUpdate: true` enables silent background updates without requiring manual `gopherclaw update`.  
**Acceptance criteria:** When enabled, binary self-updates on startup if new version available; user notified after update. Default: `false`.

### REQ-034 — `gopherclaw rollback` Command
**Priority:** Must  
**Description:** `gopherclaw rollback` restores the previously backed-up binary (`gopherclaw.bak`) in-place, replacing the current binary.  
**Acceptance criteria:** After running, `gopherclaw --version` reports the previous version. If no backup exists, command exits with a clear error message.  
**Decision reference:** DEC-021

### REQ-035 — Dashboard Version History and Rollback
**Priority:** Must  
**Description:** The dashboard displays current version, backed-up version (if any), and latest available version from GitHub Releases. A one-click rollback button triggers `gopherclaw rollback` and shows confirmation before executing.  
**Acceptance criteria:** Version panel visible in dashboard. Rollback button triggers confirmation dialog; on confirm, service rolls back and notifies user.  
**Decision reference:** DEC-022

### REQ-036 — Config Auto-Migration on Upgrade
**Priority:** Must  
**Description:** On startup, if `meta.lastTouchedVersion` differs from the current binary version, GopherClaw runs any pending config migrations automatically. The original config is backed up as `config.json.bak` before migration. Migration is additive only — existing values are preserved; new fields are added with defaults.  
**Acceptance criteria:** After upgrade, config contains all new fields with correct defaults. `config.json.bak` exists with the pre-migration config. Service starts successfully without manual config editing.  
**Decision reference:** DEC-023

---

## Security — Exec Tool Safeguards

### REQ-060 — Destructive Command Blocklist
**Priority:** Must  
**Description:** `internal/tools/exec.go` maintains a built-in blocklist of dangerous command patterns (e.g. `rm -rf`, `dd`, `mkfs`, `shutdown`, `reboot`, `halt`, `poweroff`, `userdel`, `kill -9`, `iptables -F`, `chmod -R 777`, `curl ... | bash`). Before executing any shell command, GopherClaw checks the command against this blocklist.  
**Acceptance criteria:** Commands matching any blocklist pattern are not executed without explicit confirmation. Built-in blocklist is not overridable by skills. Additional deny patterns can be set via `DenyCommands` at initialization.
**Decision reference:** DEC-012

### REQ-061 — Destructive Command Confirmation Prompt
**Priority:** Must  
**Description:** When a command matches the blocklist, GopherClaw sends the user an explicit confirmation prompt on the active channel before executing. The prompt must include: the full command to be run, why it is considered dangerous, and clear Yes/No options (inline keyboard for Telegram/Discord, stdin for terminal, modal for gateway UI).  
**Acceptance criteria:** No dangerous command executes until user explicitly confirms. Confirmation prompt is delivered on the same channel as the original request.  
**Decision reference:** DEC-010

### REQ-062 — Block Without Confirmation Channel
**Priority:** Must  
**Description:** If a dangerous command is triggered in a context where real-time confirmation cannot be collected (e.g. cron job, async webhook, headless mode), the command is blocked entirely. The block is logged at WARN level and the operator is notified on next available channel.  
**Acceptance criteria:** Dangerous commands in non-interactive contexts are never executed. Block event appears in logs and dashboard.  
**Decision reference:** DEC-011

### REQ-063 — Confirmation Timeout
**Priority:** Must  
**Description:** If the user does not respond to a confirmation prompt within a configurable timeout (default: 60 seconds), the command is automatically blocked. The timeout and block are logged.  
**Acceptance criteria:** Unconfirmed dangerous commands auto-block after timeout. Timeout is configurable via `tools.exec.confirmTimeoutSec`.

---

## Orchestration — Concurrency and Queueing

### REQ-070 — Orchestrator Concurrency Cap
**Priority:** Must  
**Description:** A configurable `orchestrator.maxConcurrent` setting (default: 5) limits simultaneously executing subtasks via a semaphore. No more than `maxConcurrent` tasks run at the same time regardless of graph shape.  
**Acceptance criteria:** With `maxConcurrent: 2`, no more than 2 subtasks run simultaneously. Verified by test with timing assertions.  
**Decision reference:** DEC-013  
**Cross-reference:** REQ-127 (multi-agent-orchestration spec)

---

## Model Selection

### REQ-080 — Primary Model with Fallback Chain
**Priority:** Must
**Description:** The model router tries the configured primary model first, then falls back through an ordered list on failure. Each model attempt retries transient errors with exponential backoff. Per-model cooldowns skip recently-failed models.
**Acceptance criteria:** All agent requests use the configured primary model unless it is unavailable or in cooldown. On failure, fallback models are tried in order.
**Decision reference:** DEC-015

### REQ-081 — Automatic Model Fallback Chain
**Priority:** Must  
**Description:** If the primary model fails (API error, timeout, quota exceeded), the router automatically retries with the next model in the configured fallback chain without user intervention. All fallback attempts are logged at WARN level.  
**Acceptance criteria:** On primary model failure, request is retried with fallback model transparently. User receives a response even if primary is unavailable. Fallback event logged with reason.
**Decision reference:** DEC-015

### REQ-082 — Fallback Chain Configuration
**Priority:** Must  
**Description:** The fallback chain is configurable in `config.json` under `agents.defaults.model.fallbacks` as an ordered list. Current default chain: `github-copilot/claude-sonnet-4.6` → `github-copilot/gpt-4.1` → `gemini/gemini-2.5-flash`.  
**Acceptance criteria:** Changing the fallback chain in config takes effect on next startup (or hot-reload). Router respects the order.

---

## Health Checks and Status

### REQ-090 — `/status` Slash Command
**Priority:** Must  
**Description:** A `/status` slash command returns a concise health summary including: GopherClaw version, uptime, primary model and active fallback status, loaded skills count and any failed-to-load skills, active session count, orchestrator queue depth, and any active alerts.  
**Acceptance criteria:** `/status` returns a response within 2 seconds. All listed fields present. Works on all channels (Telegram, Discord, terminal, gateway UI).  
**Decision reference:** DEC-017

### REQ-091 — Startup Health Log
**Priority:** Must  
**Description:** On startup, GopherClaw logs a structured health summary at INFO level: version, config path, loaded skills, active channels, model configuration, and any initialization warnings.  
**Acceptance criteria:** Startup log contains all listed fields. Warnings (e.g. unverified skill loaded) appear at WARN level.

---

## Web UI / Dashboard

### REQ-095 — Dashboard Status Panel
**Priority:** Must  
**Description:** The dashboard (served at `/gopherclaw`) includes a real-time status panel showing: GopherClaw version and uptime, primary model and fallback status, active sessions, loaded skills with health indicators, orchestrator queue depth, and recent alerts.  
**Acceptance criteria:** Status panel auto-refreshes (polling or WebSocket). All fields present and accurate.  
**Decision reference:** DEC-018

### REQ-096 — Dashboard Skills Panel
**Priority:** Must  
**Description:** The dashboard includes a skills panel listing all loaded skills with: name, source (CrawHub/manual), verified status, enabled/disabled state, and a toggle to enable or disable each skill at runtime.  
**Acceptance criteria:** Skills panel lists all loaded skills. Enable/disable toggle takes effect immediately (hot-reload). Unverified skills show warning indicator.  
**Decision reference:** DEC-018, DEC-020

### REQ-097 — Dashboard Logs Tail
**Priority:** Must  
**Description:** The dashboard includes a live log tail panel showing recent structured log entries. Filterable by level (DEBUG/INFO/WARN/ERROR).  
**Acceptance criteria:** Log tail updates in real time. Level filter works. At minimum last 100 entries visible.

### REQ-098 — Dashboard Orchestrator Queue
**Priority:** Should  
**Description:** The dashboard includes an orchestrator panel showing: current concurrency usage (active/max), queue depth (queued/cap), and recent task history (last 20 tasks with status).  
**Acceptance criteria:** Orchestrator panel shows live data. Task history updates as tasks complete.  
**Decision reference:** DEC-018

---

## Skills — Runtime Management

### REQ-100 — Skill Hot-Reload
**Priority:** Must  
**Description:** GopherClaw watches `~/.gopherclaw/workspace/skills/` for filesystem changes (new directories, modified `SKILL.md`). When a change is detected, the affected skill's metadata is reloaded without restarting the service. Hot-reload covers SKILL.md metadata only — stateful skill resources are not re-initialized.  
**Acceptance criteria:** Adding or modifying a skill in the skills directory is reflected within 5 seconds without service restart. Hot-reload event logged at INFO level.  
**Decision reference:** DEC-019

### REQ-101 — Skill Enable/Disable at Runtime
**Priority:** Must  
**Description:** Skills can be enabled or disabled at runtime via the dashboard (REQ-096) or a `/skills disable <name>` / `/skills enable <name>` slash command. Disabled skills are not invoked by the agent even if present in the skills directory.  
**Acceptance criteria:** Disabling a skill prevents it from being called. Enabling restores it. State persists across restarts (stored in skill state file).  
**Decision reference:** DEC-019

### REQ-102 — Unverified Skill Warning
**Priority:** Must  
**Description:** Skills not installed via CrawHub (manual drop-in or unknown origin) are flagged as unverified. An unverified skill shows a visible warning in the dashboard (⚠️ indicator) and a one-time log warning on first invocation: "Unverified skill — not installed from CrawHub. Review SKILL.md before use."  
**Acceptance criteria:** All manually installed skills show unverified indicator in dashboard. Warning logged on first use of each unverified skill. Warning does not block skill execution.  
**Decision reference:** DEC-020

### REQ-103 — Skill Update Notification
**Priority:** Should  
**Description:** For CrawHub-installed skills, GopherClaw periodically checks for updates (throttled to once/day, same mechanism as version check). If a newer skill version is available, the dashboard shows a notification and a one-time message is sent to the active channel.  
**Acceptance criteria:** Dashboard shows update available indicator for skills with newer CrawHub versions. Notification delivered to active channel.

---

## Housekeeping

### REQ-040 — Fix README Config Path
**Priority:** Must
**Description:** README and any docs referencing `~/.openclaw/` updated to `~/.gopherclaw/`.
**Acceptance criteria:** No `~/.openclaw/` references remain in user-facing documentation.

---

## Documentation

### REQ-050 — Configuration Reference
**Priority:** Must
**Description:** A `docs/configuration.md` documents every `config.json` field with its type, default value, and behavior. Organized by section (agents, channels, gateway, tools, session, logging, update).
**Acceptance criteria:** Every field in `internal/config/config.go` structs is documented. A new user can configure GopherClaw using only this document — no source code reading required.

### REQ-051 — Skill Authoring Guide
**Priority:** Should
**Description:** A `docs/skills.md` explains how to create a skill: SKILL.md format, YAML frontmatter fields, available tools (`exec`, `files`, `memory_append`, `memory_get`, `web_search`, `web_fetch`, `browser`), workspace directory layout, and a worked example.
**Acceptance criteria:** A developer can create and install a working skill by following the guide alone. Covers both CrawHub-installed and manual drop-in skills (REQ-023).

### REQ-052 — Gateway API Reference
**Priority:** Should
**Description:** A `docs/api.md` documents every HTTP endpoint: `POST /v1/chat/completions`, `GET /v1/models`, `POST /webhooks/{session}`, cron CRUD endpoints (`GET/POST/PUT/DELETE /v1/cron/jobs`), WebSocket `/ws`, SSE `/v1/logs/stream`, and the control UI routes. Each entry includes method, path, auth requirements, request/response schema, and example curl commands.
**Acceptance criteria:** An integrator can build a client against the gateway using only this document.

### REQ-053 — Migration Guide
**Priority:** Should
**Description:** A `docs/migration.md` covers migrating from OpenClaw to GopherClaw: what `gopherclaw --migrate` does (config translation, session JSONL copy), what `gopherclaw init` migration detection does, what changes between OpenClaw and GopherClaw config formats, and known incompatibilities or manual steps.
**Acceptance criteria:** An existing OpenClaw user can migrate to GopherClaw by following this guide without data loss.

### REQ-054 — Troubleshooting / FAQ
**Priority:** Could
**Description:** A `docs/troubleshooting.md` covers common issues: Copilot token expiry and refresh, Telegram pairing flow, systemd/launchd service setup, gateway auth token management, "no providers available" errors, and browser tool Chrome dependency.
**Acceptance criteria:** Covers at least 8 common failure scenarios with symptoms, cause, and fix.
