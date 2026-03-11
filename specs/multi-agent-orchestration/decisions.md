# Decisions — Multi-Agent Orchestration

## DEC-001 — Orchestrator JSON Reliability
**Decision:** Retry loop (max 2 retries) — prompt engineering + validation, no structured output mode.  
**Rationale:** GopherClaw routes across multiple providers; JSON mode / structured outputs are not universally available. Retry loop is model-agnostic and sufficient for a two-field schema.

---

## DEC-002 — Dashboard Task Graph Visualization
**Decision:** Deferred. Not required for initial implementation.  
**Rationale:** Nice to have for debuggability but adds significant frontend complexity. Full task graph is logged at INFO level (REQ-112) — sufficient for v1 observability. Revisit when dashboard parity work begins.

---

## DEC-003 — Output Interpolation Safety
**Decision:** Truncate `{{task-id.output}}` substitutions to a configurable max, default 4000 tokens. Log a warning when truncation occurs.  
**Rationale:** Raw task output (e.g. a full codebase diff) can blow out a downstream agent's context window. Truncation is simple, predictable, and observable. The orchestrator's planning prompt should account for this limit.

---

## DEC-004 — Orchestrator Re-planning on Failure
**Decision:** No re-planning in v1. On blocking failure, fail fast and synthesize with available results.  
**Rationale:** Re-planning requires a dispatch → failure → re-plan → re-dispatch loop with a circuit breaker. Significant complexity for uncertain gain. Revisit in v2 once basic dispatch is proven out.

---

## DEC-005 — CLI Agents in Orchestrated Dispatch
**Decision:** CLI agents (including `coding-agent`) work in the dispatcher with no changes.  
**Rationale:** `CLIAgent.Chat()` is already synchronous — it blocks on `cmd.Output()` until the subprocess completes. The async wrapping in `DelegateTool` is a UX choice (don't block the chat session), not a property of CLI agents. The dispatcher runs tasks in goroutines and wants to block-and-wait, so CLI agents are fully compatible as-is.

---

## DEC-006 — Max Depth with Orchestrator in Chain
**Decision:** Max depth 5 is sufficient. Specialists do not delegate further.  
**Rationale:** Depth with orchestrator in chain is: main (0) → orchestrator (1) → specialist (2). Three levels well within the limit of 5. This constraint is documented in the orchestrator system prompt — specialists are leaf nodes only.

---

## DEC-007 — Orchestrator Identity Files
**Decision:** A default orchestrator identity template is shipped with GopherClaw and written to `~/.gopherclaw/workspace/agents/orchestrator/` by `gopherclaw init`. Users can customize it.  
**Rationale:** The orchestrator's system prompt is critical to reliable task graph production. Shipping a tested default removes a setup burden and ensures the feature works out of the box.

---

## Assumptions

1. The `Chatter` interface (agent.go) remains stable — dispatcher depends on it directly
2. In-process subagents are fast enough that synchronous goroutine-per-task is appropriate (no backpressure beyond the semaphore)
3. The orchestrator will not be used for single-agent tasks — the main agent routes those directly
4. LLM structured output / JSON mode is not universally available across GopherClaw's supported providers

---

## Risks

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Orchestrator produces invalid task graph JSON | Medium | High — dispatch fails entirely | Retry loop (DEC-001); validate before execution |
| Context window overflow from output interpolation | Medium | Medium — downstream agent failure | Truncation limit (DEC-003) |
| Main agent over-routes to orchestrator (uses it for simple tasks) | Medium | Low — just slower | System prompt tuning; observable via logs |
| Goroutine leak in dispatcher | Low | High — memory/CPU degradation over time | `goleak` in tests (REQ-150) |
| Depth limit hit in nested delegation | Low | Medium — silent failure | Document constraint (DEC-006); clear error message |
| Orchestrator synthesis is meta, not substantive (describes work done instead of delivering output) | Medium | High — user has to ask follow-up just to get the actual answer | REQ-162 + identity template (REQ-170) enforce "synthesis IS the output"; `<synthesis>` section format makes it reviewable |
| Main agent re-routes follow-up questions to orchestrator unnecessarily | Medium | Medium — re-runs all tasks, slow and wasteful | REQ-180 requires follow-ups work from context; routing heuristics in identity (REQ-171) distinguish "new multi-step task" from "follow-up on prior result"; observable via dispatch logs |
