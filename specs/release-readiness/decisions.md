# Decisions

This document tracks all significant architectural and product decisions made during the GopherClaw release-readiness specification process.

## DEC-010: Exec tool requires confirmation for destructive commands
**Status:** Confirmed  
**Context:** GopherClaw runs as a highly privileged local agent. An LLM hallucination or bad skill instruction could execute `rm -rf /` or similar destructive commands.  
**Decision:** The `exec` tool will intercept commands matching a predefined blocklist. If a match occurs, it will pause execution and send a confirmation prompt to the user via the active channel (e.g., Telegram, CLI).  
**Rationale:** Provides a non-bypassable safety net for the most dangerous operations without breaking the agent's ability to run safe commands autonomously.

## DEC-011: Exec tool blocks entirely if no interactive channel is available
**Status:** Confirmed  
**Context:** If a destructive command runs during a background task (e.g., cron job) where the user isn't present to confirm it, what should happen?  
**Decision:** The command is hard-blocked. A `WARN` log is emitted, and the failure is returned to the agent. The agent can decide how to recover, but the command will not run.  
**Rationale:** Silent execution bypasses the safety net. Hanging indefinitely leaks goroutines. Hard blocking is the only safe default.

## DEC-012: Blocklist is built-in, substring-based, with programmatic deny-list
**Status:** Confirmed (partially implemented)
**Context:** How do we define "destructive"?
**Decision:** GopherClaw ships with a built-in list of substring patterns covering obvious destructive commands (`rm -rf`, `dd`, system shutdown, etc.). Additional deny patterns can be set via the `ExecTool.DenyCommands` field at initialization. A user-extensible `customBlocklist` in `config.json` is proposed but not yet implemented.
**Rationale:** Catches 99% of mistakes without needing a complex shell parser. Programmatic extensibility allows environment-specific hardening.

## DEC-013: Orchestrator caps concurrent tasks
**Status:** Confirmed  
**Context:** Agents can spawn many parallel tasks (e.g., summarizing 50 emails). Unbounded concurrency crashes the local machine or hits API rate limits.  
**Decision:** The orchestrator will use a semaphore to limit concurrent agent tasks (default: 5, configurable).  
**Rationale:** Protects the host machine (CPU/memory) and prevents catastrophic rate-limit exhaustion at the API level.

## DEC-014: Orchestrator uses semaphore-gated concurrency; task queue provides persistence
**Status:** Confirmed
**Context:** If the concurrency cap is reached, what happens to task #6?
**Decision:** The orchestrator (`internal/orchestrator/`) uses a semaphore to gate concurrent task execution; tasks block until a slot is available. A separate task queue (`internal/taskqueue/`) provides persistent task state via `tasks.json` with debounced writes.
**Rationale:** Semaphore-gated execution is simpler and sufficient for current workloads. Persistent task state via taskqueue covers restart resilience.

## DEC-015: Model Router uses primary + fallback chain
**Status:** Confirmed (partially implemented)
**Context:** A typical environment has multiple models: Claude Sonnet 4.6 (Copilot), GPT-4.1 (Copilot), Gemini 3.1 Pro (OpenRouter), and Gemini 2.5 Flash.
**Decision:** The router tries the primary model first, then falls back through an ordered list on failure. Each model attempt includes retry with exponential backoff for transient errors. Per-model cooldowns with exponential backoff prevent repeated failures. Note: capability-based task-type routing (matching task type to best model) is PROPOSED but NOT YET IMPLEMENTED. The current router does not evaluate task types.
**Rationale:** Simple fallback chain provides reliability. Capability-based routing can be layered on later.

## DEC-017: GopherClaw handles config auto-migration on update
**Status:** Confirmed  
**Context:** When a user updates the binary, new features might require new fields in `config.json`.  
**Decision:** The startup sequence will compare the binary version against the config version (`meta.lastTouchedVersion`). If the binary is newer, it backs up `config.json` to `config.json.bak` and runs versioned migration functions sequentially. If any migration fails, it aborts and automatically restores `config.json.bak`.  
**Rationale:** Zero-friction updates. Versioned migrations with automatic failure rollback ensure the agent never gets stranded in a broken config state.

## DEC-018: Rollback is a first-class citizen (binary + UI)
**Status:** Confirmed  
**Context:** If an update breaks the agent, the user needs an escape hatch.  
**Decision:** The updater will always save the currently running binary as `gopherclaw.bak` before replacing it. A `gopherclaw rollback` command (and a UI button in the dashboard) will instantly restore the backup, followed by an automatic self-restart to load the old binary.  
**Rationale:** Lowers the risk of updating. Automatic restart after rollback means zero command-line intervention is required if done via dashboard.

## DEC-019: Skills support metadata hot-reload
**Status:** Confirmed  
**Context:** Currently, editing a `SKILL.md` requires restarting the entire OpenClaw/GopherClaw service to pick up the new instructions.  
**Decision:** GopherClaw will use `fsnotify` to watch the `workspace/skills/` directory. When a `SKILL.md` file changes, only that skill's metadata is reloaded into the active tool registry without restarting the service (stateful reinit deferred to v2).  
**Rationale:** Massively speeds up skill development and tuning iterations without overcomplicating v1 with stateful teardown logic.

## DEC-020: Web Dashboard provides observability
**Status:** Confirmed  
**Context:** It's hard to know what the agent is doing in the background (queue depth, loaded skills, current model).  
**Decision:** The existing embedded HTML/JS `ui.go` will be extended into a full dashboard showing: System Status, Active Skills, Orchestrator Queue, Live Logs, and Version History. No React/bundler will be introduced.  
**Rationale:** Makes the invisible visible while preserving the single-binary, zero-dependency deployment model.

## DEC-021: Interactive confirmation uses UI buttons where available
**Status:** Confirmed  
**Context:** How does the user confirm a destructive command?  
**Decision:** On platforms that support it (like Telegram), GopherClaw will send an interactive prompt with clickable Yes/No inline keyboard buttons, rather than requiring the user to type out a reply.  
**Rationale:** Reduces friction for security checks. A single tap is better than typing.

## DEC-022: Release via goreleaser to GitHub
**Status:** Confirmed  
**Context:** Need a standardized way to build and distribute the Go binary.  
**Decision:** `goreleaser` will build cross-platform binaries (Linux/macOS, amd64/arm64), generate `.deb`/`.rpm` packages, and publish them to GitHub Releases with SHA256 checksums automatically on version tags.  
**Rationale:** Industry standard for Go tools. Eliminates manual build steps and ensures artifact integrity.

## DEC-023: `gopherclaw init` handles OpenClaw migration
**Status:** Confirmed  
**Context:** Existing users already have `~/.openclaw` configured with active sessions and skills.  
**Decision:** The `init` command will detect `~/.openclaw/config.json`. If found, it will offer to automatically migrate the config, sessions, and skills to `~/.gopherclaw/` instead of asking the user to re-enter API keys.  
**Rationale:** Painless cutover from the old Node.js runtime to the new Go runtime.
