# Open Questions

## All Questions Resolved ✅

All open questions from the discovery interview have been answered. See `decisions.md` for the full decision log.

---

## Summary of Resolved Questions

| OQ | Question | Resolution |
|---|---|---|
| OQ-001 | CrawHub API shape | Reverse-engineer from OpenClaw source or network traffic before building; manual drop-in always works as fallback |
| OQ-002 | GitHub repo name/owner | Confirmed: `EMSERO/gopherclaw` — goreleaser, updater, and README all updated |
| OQ-003 | Version scheme | Calendar versioning (`2026.x.y`) — matches OpenClaw, already used in current `main.go` |
| OQ-004 | `gopherclaw init` wizard scope | All channel types (Telegram, Discord, Slack) + primary model key; OpenClaw migration skips wizard |
| OQ-005 | Auto-update on service restart | Post-rollback: automatic self-restart via `syscall.Exec` re-exec (DEC-018) |
| OQ-010 | Blocklist user-extensible? | Partially — `DenyCommands` field supports additional deny patterns at initialization; config.json `customBlocklist` is proposed but not yet implemented (DEC-012) |
| OQ-011 | Confirmation UX per channel | Telegram: inline Yes/No keyboard buttons; Terminal: stdin prompt; Dashboard: modal dialog; timeout: 60s configurable (DEC-021) |
| OQ-012 | Task persistence across restarts | Task state persisted to `tasks.json` via `internal/taskqueue/`; previously-running tasks marked failed on restart (DEC-014) |
| OQ-013 | Task-type-aware model routing | Currently: primary + fallback chain only. Capability-based routing is PROPOSED but NOT IMPLEMENTED (DEC-015) |
| OQ-014 | Dashboard tech stack | Extend existing plain HTML/JS (`ui.html` + `ui.go`); no React/bundler (DEC-020) |
| OQ-015 | Skill hot-reload scope | Metadata-only (`SKILL.md` frontmatter) for v1; stateful reinit deferred to v2 (DEC-019) |
| OQ-016 | Config migration versioning | Versioned migration table (`FromVersion` + `Apply` func); automatic rollback to `config.json.bak` on failure (DEC-017) |
| OQ-017 | Dashboard rollback UX | Automatic self-restart after rollback via `syscall.Exec` re-exec (DEC-018) |

---

## Resolved (previously unknown)

### OQ-002 — GitHub Repo Name / Owner
**Status:** Resolved ✅
**Question:** What is the GitHub org/username and repo name for GopherClaw?
**Resolution:** Confirmed: **EMSERO/gopherclaw**. All references updated in `.goreleaser.yml`, `internal/updater/updater.go`, `README.md`, and spec docs.

---

## Deferred / Out of Scope

- **Skill converter skill** — converting non-GopherClaw skills to compatible format (future)
- **Homebrew tap** — nice-to-have Mac install experience, not blocking v1
- **Windows support** — deferred indefinitely
- **Auto-update of skills** — `gopherclaw update` updates the binary only; skill updates are separate (CrawHub handles this)
- **Task-type-aware model routing** — proposed but not implemented; capability-tag system deferred
- **Stateful skill hot-reload** — metadata-only reload for v1; full teardown/reinit deferred (OQ-015)

---

## Risks

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| CrawHub API changes or goes offline | Medium | High — skill install broken | Manual drop-in always works as fallback; could bundle default skill list |
| Self-update corrupts binary | Low | High — service goes down | Atomic replace + SHA256 verify + keep backup of old binary (DEC-018) |
| 80% coverage threshold too aggressive for current codebase | Medium | Blocks CI green | Audit actual coverage first; adjust threshold if needed |
| CrawHub API is private/undocumented | Medium | Blocks skill picker | Need to inspect OpenClaw source or network traffic before building |
| Destructive blocklist bypassed via script indirection | Medium | High — false security confidence | Document as defense-in-depth; not a sandbox replacement |
| Confirmation timeout holds goroutine too long | Medium | Medium — resource leak under denial/timeout | Configurable short default (60s); goroutine cancelled on timeout |
| Dashboard exposes sensitive session data | Low | High — privacy/security | Dashboard behind existing auth token; no session content in default view |
| `syscall.Exec` re-exec fails in containerized/restricted environments | Low | Medium — rollback succeeds but no auto-restart | Fall back to logging restart instructions to user |
| Task state deserialization fails after schema change | Low | Medium — task records lost on upgrade | Version the task file format; log and discard on schema mismatch |
