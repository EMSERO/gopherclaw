# Multi-Agent Orchestration

## Problem

GopherClaw's current `delegate` tool is sequential — one subagent at a time, no parallelism, no dependency awareness. Complex multi-step tasks are bottlenecked by serial execution and the main agent has no way to fan out work efficiently.

## Proposed Solution

Introduce a three-tier agent hierarchy. The **main agent** handles simple requests directly and routes complex multi-step tasks to a dedicated **orchestrator agent**. The orchestrator plans the work as an explicit task graph, then a **Go dispatcher** executes it — running independent tasks concurrently via goroutines, respecting dependencies, handling partial failures based on task criticality. Results are synthesized by the orchestrator and returned to the main agent.

## Goals

- Main agent remains the single entry point; routing to orchestrator is LLM-native (no config rules)
- Orchestrator produces an inspectable JSON task graph before any execution begins
- Dispatcher executes the task graph with true Go concurrency (goroutines + errgroup)
- Blocking task failures cancel dependent tasks and surface clearly; best-effort failures enrich the result if they succeed but don't block
- Specialist subagents are unchanged — they just receive work and return results
- Task graph and execution trace are logged and visible in the dashboard

## Non-Goals

- The orchestrator does not execute work itself — it plans and synthesizes only
- No distributed dispatcher (everything in-process in the gateway)
- No dynamic subagent spawning at runtime — specialists are registered at startup
- No user-facing task graph editor — planning is fully LLM-driven
- No cross-process or networked subagents in this spec (CLI agents work in the dispatcher via `CLIAgent.Chat()` — see DEC-005 — but no distributed/networked dispatch)

## Key Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Routing mechanism | LLM-native (main agent decides) | Explicit rules are brittle; LLM judgment is flexible enough for routing |
| Planning | Structured JSON task graph | Enables parallelism, dependency tracking, partial failure handling, and dashboard visibility |
| Dispatcher location | In-process Go component | Goroutines + errgroup are the natural Go fit; no serialization overhead; direct subagent registry access |
| Partial failure | Orchestrator decides per task criticality | Declared in task graph (`blocking: true/false`); dispatcher acts accordingly |
| Orchestrator identity | Separate named agent in registry | Clean separation; main agent delegates via existing `delegate`/`dispatch` mechanism |
