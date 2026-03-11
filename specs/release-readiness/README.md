# GopherClaw Release Readiness & Hardening

## Problem Statement

GopherClaw is a production-capable Go rewrite of OpenClaw but needs release infrastructure, security safeguards, runtime observability, and quality-of-life improvements to be the best it can be: secure, highly concurrent, easy to use, and smart about model selection.

## Proposed Solution

Build on the existing GopherClaw binary across five areas:
1. **Release infrastructure** — CI/CD, goreleaser, self-update, rollback, config auto-migration
2. **Security hardening** — exec tool blocklist with real-time confirmation, block-without-channel policy
3. **Orchestration reliability** — concurrency cap + persistent task state to handle burst loads
4. **Model selection** — quality-first routing with automatic fallback chain
5. **Observability & UX** — web dashboard, health checks, skill hot-reload and management

## Goals

- CI pipeline that gates merges on tests (80%+ coverage), lint, and security scans
- Automated releases to GitHub Releases (Linux, macOS) via goreleaser
- `gopherclaw init` wizard + OpenClaw migration
- Self-update with explicit rollback (`gopherclaw rollback`) and dashboard version history
- Automatic config migration on upgrade (non-destructive, backed up)
- Exec tool: global destructive command confirmation, hard block without confirmation channel
- Orchestrator: configurable concurrency cap with semaphore-gated execution
- Model router: quality-first primary model, automatic fallback chain (never drop a task due to single provider failure)
- Web dashboard: status, skills, orchestrator queue, logs, version history + rollback
- Skill hot-reload, enable/disable at runtime, unverified skill warnings
- `/status` slash command for quick health check on any channel

## Non-Goals

- Windows support (deferred)
- Homebrew tap (nice-to-have, not blocking)
- Per-skill or per-user exec permission overrides (global policy only)
- Bounded FIFO queue with rejection (semaphore blocking is sufficient for v1)
- Task-type-aware model routing (quality-first only for v1)
- Stateful skill hot-reload teardown/reinit (metadata-only for v1)
- Multi-tenant or adversarial hardening (personal assistant, trusted users only)

## Key Decisions

| Decision | Rationale |
|---|---|
| Exec: global confirmation for destructive commands | Non-bypassable safety layer; skills cannot override |
| Exec: hard block without confirmation channel | Silent skip gives false confidence; hard block is observable and safe |
| Orchestrator: semaphore-gated concurrency + persistent task state | Cap prevents resource exhaustion; task state survives restarts |
| Model selection: quality-first | Project preference — correct > fast |
| Model fallback: automatic chain | Tasks never fail due to single provider outage |
| Dashboard: extends existing ui.go | Keeps single-binary architecture; no bundler/React |
| Skill hot-reload: metadata-only | Simple, reliable; stateful reinit deferred |
| Rollback: one backup (previous version only) | Sufficient for "bad update" recovery; full version history in dashboard |
| Config migration: versioned table | Maintainable, isolated, testable migrations |
| Use CrawHub for skill registry | Already exists, no maintenance burden |
| `~/.gopherclaw/` config dir | Already implemented in binary |
| goreleaser for releases | Standard Go tooling; handles cross-compilation, .deb/.rpm, checksums |
| 80% coverage threshold | Meaningful bar without being punishing |
| Mock AI server for CI | No real API calls; fast, free, deterministic tests |
| `autoUpdate` off by default | Service should not self-modify silently |

## Cross-References

- Requirements: [requirements.md](requirements.md)
- Architecture: [architecture.md](architecture.md)
- Decisions: [decisions.md](decisions.md)
- Open questions: [open-questions.md](open-questions.md)
