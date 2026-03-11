# Architecture — Multi-Agent Orchestration

## Component Overview

```
User / Channel
      │
      ▼
┌─────────────────────────────────────────┐
│              Main Agent                 │
│  (entry point, LLM-native routing)      │
│                                         │
│  Simple request? → handle directly      │
│  Complex request? → delegate(orchestrator) │
└────────────────────┬────────────────────┘
                     │ delegate("orchestrator", task)
                     ▼
┌─────────────────────────────────────────┐
│           Orchestrator Agent            │
│  (planner + synthesizer, no execution)  │
│                                         │
│  1. Receive task from main agent        │
│  2. LLM produces JSON task graph        │
│  3. Call Dispatcher.Execute(graph)      │
│  4. Receive ResultSet                   │
│  5. LLM synthesizes final response      │
└────────────────────┬────────────────────┘
                     │ Execute(TaskGraph, ProgressFunc)
                     ▼
┌─────────────────────────────────────────┐
│              Dispatcher                 │
│  (in-process Go, internal/orchestrator) │
│                                         │
│  - Validates + topologically sorts graph│
│  - Runs independent tasks in goroutines │
│  - Enforces dependencies                │
│  - Handles partial failure              │
│  - Calls ProgressFunc on task complete  │
│  - Returns structured ResultSet         │
└──────┬──────────┬──────────┬────────────┘
       │          │          │
       ▼          ▼          ▼
 ┌──────────┐ ┌──────────┐ ┌──────────┐
 │ coding-  │ │ email-   │ │research- │
 │  agent   │ │  agent   │ │(Chatter) │
 │(Chatter) │ │(Chatter) │ │          │
 └──────────┘ └──────────┘ └──────────┘
    Specialist subagents (unchanged)
```

---

## Data Flow

### Happy Path

```
1. User → Main Agent: "Research X, write code for Y, and email Z the summary"
2. Main Agent → Orchestrator: delegates full task
3. Orchestrator (LLM turn 1): produces TaskGraph JSON
4. Orchestrator → Dispatcher: Execute(graph, progressFn)
5. Dispatcher: topological sort → [research-agent ∥ email-draft-agent] → [coding-agent]
6. Dispatcher: goroutine A calls research-agent.Chat()
              goroutine B calls email-draft-agent.Chat()  ← parallel
7. goroutine A completes → progressFn("research-1", success) fires (if enabled)
8. Both complete → coding-agent.Chat() starts (depends on research result)
9. All tasks done → Dispatcher returns ResultSet to Orchestrator
10. Orchestrator (LLM turn 2): synthesizes ResultSet → structured response
11. Orchestrator → Main Agent: final response (synthesis + appendix)
12. Main Agent → User: delivers synthesis
```

### Blocking Failure Path

```
5. Dispatcher: goroutine A (research-agent) fails, blocking=true
6. Dispatcher: cancels context → goroutine B receives cancellation
7. Dispatcher: coding-agent task never starts (dependency on research failed)
8. Dispatcher: returns ResultSet with A=failed, B=cancelled, coding=cancelled
9. Orchestrator: synthesizes partial results + failure explanation
10. Main Agent → User: "Research failed due to X. Email draft was cancelled. Here's what I have..."
```

---

## Task Graph Schema

```json
{
  "tasks": [
    {
      "id": "research-1",
      "agent_id": "research-agent",
      "message": "Research the top 3 competitors of product X",
      "depends_on": [],
      "blocking": true,
      "timeout_seconds": 60
    },
    {
      "id": "code-1",
      "agent_id": "coding-agent",
      "message": "Write a comparison table based on: {{research-1.output}}",
      "depends_on": ["research-1"],
      "blocking": true,
      "timeout_seconds": 120
    },
    {
      "id": "email-1",
      "agent_id": "email-agent",
      "message": "Draft a summary email using: {{code-1.output}}",
      "depends_on": ["code-1"],
      "blocking": false,
      "timeout_seconds": 30
    }
  ]
}
```

**Notes:**
- `depends_on` is a list of task IDs that must complete before this task starts
- `blocking: true` — failure cancels all downstream dependents and surfaces to orchestrator
- `blocking: false` — failure recorded in ResultSet, does not cancel other tasks
- `timeout_seconds` is optional; no timeout if omitted
- `{{task-id.output}}` interpolation: the dispatcher substitutes completed task output into downstream messages before dispatch

---

## Result Set Schema

```json
{
  "tasks": [
    {
      "id": "research-1",
      "agent_id": "research-agent",
      "status": "success",
      "output": "Competitor A does X, Competitor B does Y...",
      "error": null,
      "duration_ms": 4210
    },
    {
      "id": "code-1",
      "agent_id": "coding-agent",
      "status": "failed",
      "output": "",
      "error": "context deadline exceeded",
      "duration_ms": 120000
    },
    {
      "id": "email-1",
      "agent_id": "email-agent",
      "status": "cancelled",
      "output": "",
      "error": "upstream task code-1 failed",
      "duration_ms": 0
    }
  ]
}
```

**Status values:** `success` | `failed` | `cancelled` | `timeout`

---

## Orchestrator Response Format

The orchestrator's response to the main agent is a structured text document with two sections. The main agent surfaces the synthesis to the user; the appendix is retained in context for follow-up questions but not surfaced unless asked.

```
<synthesis>
[The actual deliverable — report, answer, code, summary, etc.
This is what the user asked for. Not a description of what was done.]
</synthesis>

<task-appendix>
[task: research-1 | agent: research-agent | status: success]
Competitor A does X, Competitor B does Y...

[task: code-1 | agent: coding-agent | status: failed]
ERROR: context deadline exceeded

[task: email-1 | agent: email-agent | status: cancelled]
CANCELLED: upstream task code-1 failed
</task-appendix>
```

**Rules for the synthesis section:**
- Must contain the actual requested output, not a summary of what agents did
- Must be self-contained — user should not need to ask a follow-up to get the real answer
- On partial failure: synthesis includes what was completed plus a clear explanation of what failed

**Rules for the appendix section:**
- Always present, even on full success
- One entry per task in the graph
- Raw output truncated to 2000 chars per task if necessary (same truncation limit as interpolation)
- Main agent uses this section to answer "what did agent X actually say?" questions without re-running dispatch

---

## Progress Update Design (REQ-161)

When the orchestrator agent's `progressUpdates` field is `true` (set in `agents.list[]`), the dispatcher accepts an optional callback:

```go
type ProgressFunc func(taskID string, agentID string, status TaskStatus)

func (d *Dispatcher) Execute(ctx context.Context, graph TaskGraph, progress ProgressFunc) (ResultSet, error)
```

The orchestrator passes a `ProgressFunc` that writes a brief message to the user's session channel. When `progressUpdates: false` (default), a no-op function is passed — no structural change to the dispatcher.

```
// Example progress messages surfaced to user:
"[1/3] Research done ✓"
"[2/3] Code generation done ✓"
"[3/3] Email draft done ✓"
```

**Implementation notes:**
- `ProgressFunc` is called synchronously inside the goroutine, immediately after a task completes and its result is stored
- The function must be safe to call from multiple goroutines concurrently
- Progress messages are best-effort — a slow channel write does not block task execution (use non-blocking send with drop on full)
- Progress count format is `[completed/total]` where total is the number of tasks in the graph

---

## Dispatcher Internal Design

```
Dispatcher.Execute(graph TaskGraph, progress ProgressFunc) (ResultSet, error)
  │
  ├── Validate(graph)           — check required fields, unknown agent_ids
  ├── DetectCycles(graph)       — Kahn's algorithm; error if cycle found
  ├── TopologicalSort(graph)    — compute execution order
  │
  ├── sem = make(chan struct{}, maxConcurrent)
  ├── eg, ctx = errgroup.WithContext(ctx)
  ├── completed = atomic.Int32
  │
  ├── Loop: while tasks remain
  │     ├── Find tasks with all deps satisfied
  │     ├── For each ready task:
  │     │     sem ← acquire
  │     │     eg.Go(func() {
  │     │       defer sem.release
  │     │       result = agent.Chat(ctx, sessionKey, message)
  │     │       store result
  │     │       completed.Add(1)
  │     │       progress(task.id, task.agentID, result.status)  ← no-op if disabled
  │     │       if blocking && failed: cancel(ctx)
  │     │     })
  │     └── Wait for any goroutine to finish before checking again
  │
  └── eg.Wait() → collect ResultSet → return
```

**Key Go primitives:**
- `golang.org/x/sync/errgroup` — goroutine lifecycle + context propagation
- `chan struct{}` semaphore — max concurrency enforcement
- `sync.Map` or mutex-protected map — result collection across goroutines
- `context.WithCancel` — blocking failure cancellation
- `atomic.Int32` — lock-free completed task counter for progress messages

---

## Package Structure

```
internal/
  agent/
    agent.go          — existing Agent type (unchanged)
    delegate.go       — DelegateTool (uses taskqueue.Manager for async tasks)
    dispatch.go       — OrchestratorAgent: wraps Agent, adds dispatch tool
  orchestrator/
    dispatcher.go     — Dispatcher: Execute(), validate, sort, run
    graph.go          — TaskGraph, Task, ResultSet types + JSON schema
    interpolate.go    — {{task-id.output}} substitution
    progress.go       — ProgressFunc type + no-op implementation
    dispatcher_test.go
```

**Why `internal/orchestrator/` separate from `internal/agent/`?**  
The dispatcher has no dependency on the Agent type — it only needs the `Chatter` interface. Keeping it separate avoids import cycles and makes it independently testable with mock Chatters.

---

## Integration Points

### Config

The orchestrator is defined as an entry in `agents.list` (same as any other agent):

```json
{
  "agents": {
    "list": [
      {
        "id": "orchestrator",
        "identity": {"name": "Orchestrator", "theme": "planner and synthesizer"},
        "maxConcurrent": 5,
        "progressUpdates": false
      }
    ]
  }
}
```

`maxConcurrent` defaults to 5 if not set. `progressUpdates` defaults to false. These are per-agent fields on `AgentDef`, not top-level config keys.

### Agent Registry

`main.go` builds the orchestrator agent the same way it builds other subagents — from `cfg.Agents.List`. No special-casing in startup code. The `OrchestratorAgent` wrapper is applied if the agent's ID matches the reserved `"orchestrator"` ID.

### Existing DelegateTool

No changes required. The orchestrator is just another named agent in the registry. The main agent calls `delegate("orchestrator", task)` exactly as it would call any subagent.

### Session Keys

Each dispatcher execution generates a unique dispatch ID (`dispatchID = randomHex(8)`). Subtask session keys follow the pattern `orchestrator:{dispatchID}:{task-id}` for log traceability.

---

## Key Technical Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Dispatcher location | In-process (`internal/orchestrator`) | Go goroutines are the right primitive; no serialization overhead; direct Chatter interface access |
| Concurrency primitive | `errgroup` + semaphore | errgroup handles context cancellation cleanly; semaphore enforces max concurrency without a worker pool |
| Cycle detection | Kahn's algorithm | O(V+E), simple to implement, well-understood |
| Output interpolation | `{{task-id.output}}` in message strings | Lets dependent tasks use upstream results without orchestrator needing a second LLM turn |
| Orchestrator identity | Separate named agent in registry | Clean separation; reuses existing agent machinery; no special-casing in main.go |
| Routing decision | LLM-native in main agent system prompt | Explicit rules are brittle; routing is a judgment call the LLM handles well |
| Progress updates | Optional ProgressFunc callback (no-op by default) | Zero overhead when disabled; no structural change to dispatcher; safe for concurrent use |
| Orchestrator response format | Synthesis + task-appendix sections | Synthesis is user-facing; appendix enables follow-up without re-dispatch; clear separation of concerns |
