# Requirements — Multi-Agent Orchestration

## Tier Architecture

### REQ-100 — Three-Tier Hierarchy
**Priority:** Must  
**Description:** GopherClaw shall support a three-tier agent hierarchy: main agent → orchestrator agent → specialist subagents. The main agent is the sole entry point for all user requests.  
**Acceptance criteria:** All user messages enter through the main agent. The orchestrator and specialists are never directly addressable by users.

### REQ-101 — Main Agent Routing (LLM-Native)
**Priority:** Must  
**Description:** The main agent decides whether to handle a request directly or delegate to the orchestrator. This decision is LLM-native — governed by the main agent's system prompt, not explicit config rules. The system prompt must encode concrete routing heuristics: use the orchestrator when the task requires 2+ different specialists OR has explicit sequential dependencies between steps. Handle directly when the task is a single-agent request, a question, or a simple lookup.  
**Acceptance criteria:** Simple single-step requests are handled directly by the main agent without invoking the orchestrator. Multi-step requests requiring multiple specialists are routed to the orchestrator. Routing heuristics are documented in the main agent's shipped identity template.

### REQ-102 — Orchestrator as Named Agent
**Priority:** Must  
**Description:** The orchestrator is a named agent registered in the agent registry at startup. The main agent delegates to it via the existing `delegate` mechanism using a reserved ID (e.g. `"orchestrator"`).  
**Acceptance criteria:** `orchestrator` appears in the agent registry. Main agent can invoke it via `delegate("orchestrator", task)`.

### REQ-103 — Orchestrator Is Planner + Synthesizer Only
**Priority:** Must  
**Description:** The orchestrator does not execute work itself. Its role is: (1) receive task from main agent, (2) produce a JSON task graph, (3) hand graph to dispatcher, (4) receive results, (5) synthesize and return to main agent.  
**Acceptance criteria:** Orchestrator makes no direct tool calls to perform work. All execution is delegated via the dispatcher.

---

## Task Graph

### REQ-110 — Structured JSON Task Graph
**Priority:** Must  
**Description:** Before dispatching any work, the orchestrator produces a structured JSON task graph describing all subtasks, their dependencies, and their criticality.  
**Acceptance criteria:** Task graph is produced as a discrete step before any subtask execution begins. Graph is logged in full.

### REQ-111 — Task Graph Schema
**Priority:** Must  
**Description:** Each task in the graph must include: `id` (string), `agent_id` (string), `message` (string), `depends_on` ([]string, task IDs that must complete first), `blocking` (bool — if false, failure does not prevent synthesis).  
**Acceptance criteria:** Dispatcher rejects task graphs with missing required fields. Schema validated before execution begins.

### REQ-112 — Task Graph Logged and Inspectable
**Priority:** Must  
**Description:** The full task graph is written to the GopherClaw structured log at INFO level when execution begins.  
**Acceptance criteria:** Task graph appears in logs with all fields. Visible in the dashboard Logs tab.

### REQ-113 — Task Graph Visible in Dashboard (Future)
**Priority:** Should  
**Description:** A future dashboard view shows the task graph execution state (pending / running / done / failed) in real time.  
**Acceptance criteria:** Deferred — tracked in decisions.md as DEC-002. Not required for initial implementation.

---

## Dispatcher

### REQ-120 — In-Process Go Dispatcher
**Priority:** Must  
**Description:** The dispatcher is an in-process Go component. It takes a validated task graph and executes it using goroutines. No external processes, no network calls, no separate service.  
**Acceptance criteria:** Dispatcher lives in `internal/agent/` or `internal/orchestrator/`. No IPC or HTTP involved in task execution.

### REQ-121 — Parallel Execution of Independent Tasks
**Priority:** Must  
**Description:** Tasks with no unmet dependencies are executed concurrently. The dispatcher must not serialize tasks that could run in parallel.  
**Acceptance criteria:** Two tasks with no `depends_on` relationship start within the same scheduler tick. Verified by test with mock agents and timing assertions.

### REQ-122 — Dependency Ordering
**Priority:** Must  
**Description:** A task whose `depends_on` list is non-empty must not start until all listed tasks have completed successfully (or the dependency failed and was non-blocking).  
**Acceptance criteria:** Task B with `depends_on: ["A"]` never starts before task A completes. Verified by test.

### REQ-123 — Topological Sort
**Priority:** Must  
**Description:** Dispatcher topologically sorts the task graph before execution. Cycles in the dependency graph are detected and returned as an error before any tasks run.  
**Acceptance criteria:** A graph with a cycle (A → B → A) returns an error. No tasks are executed for a cyclic graph.

### REQ-124 — Partial Failure Handling
**Priority:** Must  
**Description:** When a task fails: if `blocking: true`, all tasks that depend on it (transitively) are cancelled and the dispatcher surfaces the failure to the orchestrator. If `blocking: false`, failure is recorded but execution continues; the result set includes the error for that task.  
**Acceptance criteria:** Blocking failure cancels dependents. Non-blocking failure is recorded and included in results without stopping other tasks.

### REQ-125 — Context Cancellation Propagation
**Priority:** Must  
**Description:** The dispatcher uses `golang.org/x/sync/errgroup` with a shared context. If a blocking task fails, the context is cancelled and all in-flight goroutines receive cancellation.  
**Acceptance criteria:** In-flight tasks respect context cancellation. No goroutine leaks after dispatcher returns.

### REQ-126 — Per-Task Timeout
**Priority:** Should  
**Description:** Each task may optionally specify a `timeout_seconds` field. If the subtask does not complete within the timeout, it is treated as a failure with `blocking` semantics applied as declared.  
**Acceptance criteria:** Task with `timeout_seconds: 30` is cancelled after 30 seconds if not complete.

### REQ-127 — Max Concurrency Limit
**Priority:** Should  
**Description:** A configurable `orchestrator.maxConcurrent` setting (default: 5) limits the number of simultaneously executing subtasks. Dispatcher uses a semaphore to enforce this.  
**Acceptance criteria:** With `maxConcurrent: 2`, no more than 2 subtasks run simultaneously regardless of graph shape.

---

## Specialist Subagents

### REQ-130 — Specialists Unchanged
**Priority:** Must  
**Description:** Specialist subagents require no modification to participate in orchestrated execution. They receive a `Chat()` call and return a result — same as direct delegation.  
**Acceptance criteria:** Existing subagents (e.g. `coding-agent`) work under orchestration without code changes.

### REQ-131 — Specialists Registered at Startup
**Priority:** Must  
**Description:** All specialist subagents are defined in `config.agents.list` and registered at startup. The dispatcher cannot spawn agents dynamically at runtime.  
**Acceptance criteria:** An unknown `agent_id` in a task graph returns an error before execution begins.

---

## Results & Synthesis

### REQ-140 — Structured Result Set
**Priority:** Must  
**Description:** After all tasks complete (or fail), the dispatcher returns a structured result set to the orchestrator: each task's ID, agent, status (success/failed/cancelled/timeout), output text, and error if applicable.  
**Acceptance criteria:** Result set includes an entry for every task in the graph regardless of outcome.

### REQ-141 — Orchestrator Synthesizes Results
**Priority:** Must  
**Description:** The orchestrator receives the full result set and produces a single coherent response to return to the main agent. Synthesis is LLM-driven.  
**Acceptance criteria:** Orchestrator's response to the main agent is a single text response, not a raw dump of task outputs.

### REQ-142 — Partial Results on Blocking Failure
**Priority:** Must  
**Description:** When a blocking task fails and cancels dependents, the orchestrator still synthesizes a response using completed task results plus a clear explanation of what failed and why.  
**Acceptance criteria:** Orchestrator never returns an empty response. Even total failure gets a meaningful error summary.

---

## User Experience

### REQ-160 — Dispatch Acknowledgement
**Priority:** Must  
**Description:** Before delegating to the orchestrator, the main agent sends a brief acknowledgement to the user indicating that work is being fanned out. This prevents the user from experiencing a silent pause during potentially long-running dispatch. The acknowledgement should be concise and natural (e.g. "On it — running a few things in parallel, back shortly."). It must not include a detailed breakdown of the task graph.  
**Acceptance criteria:** User receives a message before the orchestrator begins planning. No more than 1-2 sentences. Does not block dispatch start.

### REQ-161 — Incremental Task Completion Updates
**Priority:** Should  
**Description:** As individual subtasks complete during dispatch, a brief progress update is optionally surfaced to the user (e.g. "Research done, waiting on code generation..."). This is opt-in via the agent definition's `progressUpdates` field in `agents.list[]` (e.g. set `progressUpdates: true` on the orchestrator agent entry). Default is off.
**Acceptance criteria:** With `progressUpdates: true` on the orchestrator agent, each completed task triggers an announcement to the user session before synthesis begins. With it off (default), user sees only the final synthesized response.

### REQ-162 — Synthesis Includes Result Summary
**Priority:** Must  
**Description:** The orchestrator's synthesized response must be self-contained and directly useful. It must not require the user to ask follow-up questions just to get the actual output. If the task was "research X and write a report," the synthesis IS the report — not "I've completed the research and written the report, let me know if you want to see it."  
**Acceptance criteria:** Synthesized response delivers the actual output, not a meta-description of what was done. Reviewed via orchestrator identity template examples.

---

## Orchestrator Identity

### REQ-170 — Shipped Default Identity Template
**Priority:** Must  
**Description:** GopherClaw ships a default orchestrator identity template written to `~/.gopherclaw/workspace/agents/orchestrator/` by `gopherclaw init`. The template must cover: (1) role description — planner and synthesizer only, never executor; (2) task graph production instructions with JSON schema example; (3) agent registry reference — how to know which agents are available; (4) dependency and blocking decision guidance; (5) synthesis instructions — produce actual output, not summaries of output; (6) failure handling — always synthesize something useful even on partial failure.  
**Acceptance criteria:** `gopherclaw init` writes a non-empty orchestrator identity. Identity contains all six elements listed above. A developer can run the orchestrator against a test task using only the shipped template without modification.

### REQ-171 — Main Agent Routing Heuristics in Identity
**Priority:** Must  
**Description:** The main agent's shipped identity template includes a routing section that explicitly documents when to use the orchestrator vs. handle directly. Minimum heuristics to include: use orchestrator for tasks requiring 2+ specialists; use orchestrator when steps have explicit sequential dependencies; handle directly for single-agent tasks, questions, lookups, and conversational responses.  
**Acceptance criteria:** Main agent identity template contains a routing section. Routing behavior matches heuristics in at least 4 of 5 representative test cases covering the boundary between direct and orchestrated handling.

---

## Session Continuity

### REQ-180 — Post-Dispatch Context in Main Agent
**Priority:** Must  
**Description:** After the orchestrator returns its synthesized response, the main agent's session context must include enough information to handle follow-up questions about the orchestrated task. At minimum: the synthesized response is in context (it always is, as the return value of the delegate call). The main agent must be able to answer "can you expand on the X part?" without re-running dispatch.  
**Acceptance criteria:** User follow-up questions referencing the orchestrated task output are answered correctly by the main agent from context, without triggering a new orchestrator delegation. Verified by manual test.

### REQ-181 — Full Result Set Accessible to Main Agent on Request
**Priority:** Should  
**Description:** If the user asks for detail beyond the synthesis (e.g. "what exactly did the research agent return?"), the main agent can access the raw subtask output. This requires the orchestrator to include per-task outputs in its response to the main agent, not just the synthesis. The orchestrator should format this as an appendix section that the main agent can reference but doesn't surface unless asked.  
**Acceptance criteria:** Orchestrator response includes a structured appendix with per-task outputs. Main agent can quote specific task output when asked. Appendix is not surfaced to the user unless requested.

---

## Non-Functional

### REQ-150 — No Goroutine Leaks
**Priority:** Must  
**Description:** All dispatcher goroutines must complete within the lifetime of a dispatch call. No background goroutines survive after the dispatcher returns.  
**Acceptance criteria:** `goleak` (or equivalent) in tests detects no leaked goroutines after dispatch completes.

### REQ-151 — Recursion Depth Guard
**Priority:** Must  
**Description:** The existing `delegateDepthKey` context mechanism applies to orchestrated calls. The orchestrator counts as one level of depth; each specialist call counts as another. Maximum depth from config is respected.  
**Acceptance criteria:** An orchestrator → specialist chain at max depth returns an error rather than panicking or deadlocking.

### REQ-152 — Unit Testable Dispatcher
**Priority:** Must  
**Description:** The dispatcher is testable with mock `Chatter` implementations. No real LLM calls required in dispatcher tests.  
**Acceptance criteria:** Dispatcher test suite covers: parallel execution, dependency ordering, blocking failure, non-blocking failure, cycle detection, timeout, max concurrency.
