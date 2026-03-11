# Eidetic Integration — GopherClaw

## Problem

GopherClaw's memory system writes flat markdown files (daily logs + `MEMORY.md`) but has no way to search them semantically. Over time these files grow large and the agent can only retrieve recent context — it cannot recall a specific decision made three weeks ago or find all prior mentions of a topic unless they happen to be in the current session.

## Proposed Solution

Integrate [Eidetic](https://github.com/EMSERO/eidetic) as an optional memory sidecar. Eidetic runs locally, exposes an MCP-over-HTTP API, and provides three capabilities:

1. **`append_memory`** — write a structured memory entry (stored in Postgres + flat file)
2. **`search_memory`** — semantic vector search over all stored memories
3. **`get_recent`** — retrieve the N most recent entries

GopherClaw calls Eidetic in three places:
- **System prompt construction** — `get_recent` injects recent memories alongside `MEMORY.md`
- **Post-turn** — `append_memory` records every exchange after it completes (fire-and-forget goroutine)
- **Tool** — `eidetic_search` exposes `search_memory` to the agent for explicit recall

Eidetic is **optional**. If unconfigured or unreachable, GopherClaw behaves exactly as it does today.

## Goals

- Total recall: every exchange is written to Eidetic automatically, no agent action required
- Semantic search: agent can retrieve relevant past context by topic, not just recency
- Zero degradation: Eidetic down or absent = no change in behavior
- Additive: existing flat-file memory system (`MEMORY.md`, daily logs) is unchanged

## Non-Goals

- Replacing `MEMORY.md` or daily log files
- Eidetic hosting, deployment, or provisioning (assumed to run on localhost)
- Cross-agent memory sharing (each agent has its own `agent_id` namespace in Eidetic)
- Encrypting memory content at rest (out of scope; local-only threat model accepted)
- Exposing Eidetic admin endpoints through GopherClaw

## Key Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Eidetic as optional sidecar | Config-gated, graceful no-op | Not all deployments have Postgres + Ollama; forcing a dependency breaks simple setups |
| append_memory timing | Post-turn, goroutine (non-blocking) | Memory write must not add latency to the response path |
| agent_id in Eidetic | GopherClaw agent ID (e.g. `"alfred"`) | Namespaces memories per agent; consistent with existing workspace layout |
| get_recent in system prompt | Injected at `buildSystemPrompt()` | Same pattern as `MEMORY.md` injection; no new machinery needed |
| search_memory as agent tool | `eidetic_search` tool registered alongside memory tools | Lets agent invoke it on demand; consistent with existing tool model |
| No writes in `buildSystemPrompt` | Read-only in prompt construction | Prevents prompt-construction side effects; writes only happen post-turn |

## Future Work

### Stdio Transport Client

Currently GopherClaw connects to Eidetic over HTTP (`localhost:7700`), which requires Eidetic to be running as a separate long-lived process (e.g. `eidetic serve` as a systemd service or daemon).

A cleaner local integration would have GopherClaw spawn `eidetic stdio` as a subprocess and pipe JSON-RPC over stdin/stdout directly:

- No port to manage, no `eidetic serve` to keep running
- No auth token needed — local subprocess is inherently trusted
- Eidetic starts on demand and dies when GopherClaw does
- One less thing that can be "down" when memory is needed

**Recommended approach:** Support both transports, selected by config:
- `eidetic.command` set → spawn `eidetic stdio` as a subprocess (stdio transport)
- `eidetic.url` set → use existing HTTP client (current behavior)

This keeps HTTP for remote/shared deployments while making local single-machine setups zero-config.

**Tradeoffs:**
- Requires GopherClaw to manage subprocess lifecycle (spawn, restart on crash, clean shutdown)
- Multiple concurrent GopherClaw sessions each spin up their own Eidetic subprocess — all write to the same Postgres, which is safe but worth noting

**Prerequisite:** `eidetic stdio` subcommand is already implemented (Eidetic commit `469e0e1`). The work is entirely on the GopherClaw side — a subprocess manager and stdio JSON-RPC client.
