# Decisions, Assumptions & Risks — Eidetic Integration

## Decisions

### DEC-001 — Eidetic is Optional, Not Required
Eidetic integration is config-gated. If `eidetic.enabled` is absent or false, GopherClaw behaves exactly as it does today. This was an explicit design choice — not all deployments have Postgres + Ollama, and a memory sidecar should never be a hard dependency for a chat assistant.

### DEC-002 — Append Every Exchange, Not Just "Significant" Events
Every completed agent turn (user message + assistant response) is appended to Eidetic. No filtering, no classification, no agent judgment required. The name "Eidetic" implies total recall — selective writing would undermine the premise and add complexity with no clear benefit.

### DEC-003 — All Session Types Recorded (including ChatLight)
All session types — `Chat`, `ChatStream`, and `ChatLight` (heartbeat/cron) — fire post-turn appends to Eidetic. Heartbeat chatter is part of the agent's operational history and should be recorded. The project explicitly chose total recall over a filtered view.

### DEC-004 — No Append in buildSystemPrompt
`buildSystemPrompt()` is read-only. It calls `GetRecent` but never `AppendMemory`. This prevents side effects during prompt construction and keeps the write path confined to one location (post-turn in `Chat`/`ChatStream`/`ChatLight`).

### DEC-005 — 2-Second Hard Cap on GetRecent
The `GetRecent` call in `buildSystemPrompt()` is capped at 2 seconds regardless of `timeoutSeconds`. This is the only Eidetic call in the synchronous response path. A slow Eidetic instance (e.g. Ollama cold start, Postgres contention) must not add meaningful latency to the first model call.

### DEC-006 — nil Client Pattern
When Eidetic is disabled or the startup health check fails, `eideticClient` is set to `nil`. All integration code checks for `nil` before calling any method. This is idiomatic Go and avoids a separate "enabled" flag that could get out of sync with the actual client state.

### DEC-007 — agent_id Namespacing
Memory entries are tagged with the GopherClaw agent's ID (e.g. `"alfred"`). This matches the existing workspace layout where memory files are per-agent, and allows future multi-agent setups to have separate memory namespaces in a shared Eidetic instance.

### DEC-008 — Flat Files as Source of Truth
Eidetic writes to Postgres (for vector search) and reads from the flat `memory/` directory (for note indexing). GopherClaw continues writing to the same flat files via `memory_append`. Eidetic is an index over those files, not a replacement. If Eidetic's DB is lost, the flat files remain and can be re-indexed.

---

## Assumptions

1. **Eidetic runs on localhost** — The integration assumes `localhost:7700` (configurable). Remote Eidetic instances are supported via config but not tested or documented as a supported topology.

2. **Eidetic's flat-file `notes_dir` points to GopherClaw's `memory/` directory** — This must be configured manually in `~/.eidetic/config.yaml`. GopherClaw does not configure Eidetic; it only calls it.

3. **Ollama is running with `nomic-embed-text` pulled** — Required for Eidetic to embed entries. If Ollama is down, Eidetic's health check will reflect this, and GopherClaw will disable integration.

4. **The `append_memory` operation is eventually consistent** — Eidetic queues embeddings asynchronously. An entry may appear in `get_recent` before it's searchable via `search_memory`. This is acceptable; semantic search over brand-new entries is not a use case.

5. **No PII boundary enforcement** — The local-only threat model is accepted. Memory content is stored in plaintext in Postgres and flat files. No encryption at rest is implemented.

6. **One Eidetic instance per machine** — No multi-tenant or multi-instance scenarios are considered.

---

## Risks

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| Eidetic down causes startup warning but normal operation | High (any restart) | Low | nil client pattern; health check disables gracefully |
| GetRecent latency spikes delay system prompt construction | Medium | Medium | 2-second hard cap; timeout returns empty silently |
| Memory store grows faster due to ChatLight sessions being recorded | Medium | Low | Eidetic has `archive_after_days` config; disk warn threshold in health check |
| Appended content contains sensitive data (tokens, passwords) | Low-Medium | Medium | Local-only deployment; no encryption added; accepted per threat model |
| Ollama embedding failures cause entries without vectors | Medium | Low | Entries still written to flat files and appear in `get_recent`; only `search_memory` is affected |
| Concurrent appends from multiple sessions cause Eidetic write contention | Low-Medium | Low | Eidetic handles concurrent writes; GopherClaw fires goroutines; no shared state |
| goroutine leak from append goroutines on shutdown | Low | Low | Goroutines use bounded context timeout (5s); will complete or cancel within timeout on shutdown |
| `nomic-embed-text` model not pulled causes Eidetic health fail | Medium (first run) | Low | `gopherclaw init` docs should include Eidetic setup checklist |
