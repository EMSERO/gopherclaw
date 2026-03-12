package agent

// prompt.go – system-prompt construction extracted from agent.go to reduce its
// size and isolate the prompt-assembly concern.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/EMSERO/gopherclaw/internal/config"
	"github.com/EMSERO/gopherclaw/internal/eidetic"
	"github.com/EMSERO/gopherclaw/internal/memory"
	"github.com/EMSERO/gopherclaw/internal/skills"
)

// BuildCLISystemPrompt constructs a system prompt for the claude-cli engine,
// combining identity, core rules, skills, workspace docs, and an optional
// config-level system prompt override.  This mirrors initStaticPrompt but is
// a standalone function usable without an Agent instance.
func BuildCLISystemPrompt(def *config.AgentDef, skillList []skills.Skill, wsMDs map[string]string, configPrompt string) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "You are %s, %s.\n", def.Identity.Name, def.Identity.Theme)

	sb.WriteString(`
## Core Rules

- Never say "I'll let you know" or "I'll check back" unless you immediately call notify_user to schedule the follow-up. If you cannot follow through, do not promise to.
- When a task finishes, report the concrete result — not your intention to do it.
- You have a notify_user tool. Use it whenever you complete a background action or discover something important the user should know about right away.
- You have a periodic heartbeat system. If a user asks you to monitor, poll, or check something regularly, tell them you can add it to HEARTBEAT.md and it will be checked automatically on each heartbeat cycle. Use the write_file tool to update HEARTBEAT.md in your workspace with the check item. When nothing needs attention during a heartbeat, respond with HEARTBEAT_OK.

`)

	if len(skillList) > 0 {
		sb.WriteString("\n## Skills\n\n")
		for _, s := range skillList {
			fmt.Fprintf(&sb, "### %s\n", s.Name)
			if s.Description != "" {
				fmt.Fprintf(&sb, "%s\n\n", s.Description)
			}
			if s.Content != "" {
				sb.WriteString(s.Content)
				sb.WriteString("\n\n")
			}
		}
	}

	if len(wsMDs) > 0 {
		sb.WriteString("## Workspace\n\n")
		for name, content := range wsMDs {
			fmt.Fprintf(&sb, "### %s\n\n", name)
			sb.WriteString(content)
			sb.WriteString("\n\n")
		}
	}

	if configPrompt != "" {
		sb.WriteString(configPrompt)
		sb.WriteString("\n")
	}

	return sb.String()
}

// initStaticPrompt builds the immutable portion of the system prompt from the
// agent definition, enabled skills, workspace docs, and available subagents.
// It is called once at construction time.
func (a *Agent) initStaticPrompt() {
	var sb strings.Builder

	fmt.Fprintf(&sb, "You are %s, %s.\n", a.def.Identity.Name, a.def.Identity.Theme)

	sb.WriteString(`
## Core Rules

- Never say "I'll let you know" or "I'll check back" unless you immediately call notify_user to schedule the follow-up. If you cannot follow through, do not promise to.
- When a task finishes, report the concrete result — not your intention to do it.
- You have a notify_user tool. Use it whenever you complete a background action or discover something important the user should know about right away.
- You have a periodic heartbeat system. If a user asks you to monitor, poll, or check something regularly, tell them you can add it to HEARTBEAT.md and it will be checked automatically on each heartbeat cycle. Use the write_file tool to update HEARTBEAT.md in your workspace with the check item. When nothing needs attention during a heartbeat, respond with HEARTBEAT_OK.

`)

	if len(a.skills) > 0 {
		sb.WriteString("\n## Skills\n\n")
		for _, s := range a.skills {
			fmt.Fprintf(&sb, "### %s\n", s.Name)
			if s.Description != "" {
				fmt.Fprintf(&sb, "%s\n\n", s.Description)
			}
			if s.Content != "" {
				sb.WriteString(s.Content)
				sb.WriteString("\n\n")
			}
		}
	}

	if len(a.wsMDs) > 0 {
		sb.WriteString("## Workspace\n\n")
		for name, content := range a.wsMDs {
			fmt.Fprintf(&sb, "### %s\n\n", name)
			sb.WriteString(content)
			sb.WriteString("\n\n")
		}
	}

	// List available subagents so the model knows what it can delegate to
	if dt, ok := a.toolMap["delegate"]; ok {
		if d, ok := dt.(*DelegateTool); ok && len(d.Agents) > 0 {
			sb.WriteString("## Subagents\n\n")
			sb.WriteString("Use the `delegate` tool to call these subagents:\n")
			hasOrchestrator := false
			for id := range d.Agents {
				if id == a.def.ID {
					continue // don't list self
				}
				fmt.Fprintf(&sb, "- **%s**\n", id)
				if id == "orchestrator" {
					hasOrchestrator = true
				}
			}
			sb.WriteString("\n")

			// REQ-171: routing heuristics when orchestrator is available
			if hasOrchestrator {
				sb.WriteString("### When to use the orchestrator\n\n")
				sb.WriteString("Delegate to the **orchestrator** when:\n")
				sb.WriteString("- The task requires work from 2 or more different specialists\n")
				sb.WriteString("- Steps have explicit sequential dependencies (output of one feeds into another)\n")
				sb.WriteString("- The request involves parallel research, analysis, or generation across multiple domains\n\n")
				sb.WriteString("Handle directly (do NOT use the orchestrator) when:\n")
				sb.WriteString("- The task is a single-agent request that one specialist can handle alone\n")
				sb.WriteString("- The user is asking a question, making conversation, or requesting a simple lookup\n")
				sb.WriteString("- The task is a follow-up to a previous response already in context\n\n")
			}
		}
	}

	a.sysPromptStatic = sb.String()
}

// ---------------------------------------------------------------------------
// Dynamic prompt builders
// ---------------------------------------------------------------------------

// buildSystemPrompt constructs the full system prompt by combining the static
// portion with dynamic content (current time, MEMORY.md, Eidetic recent memory).
// It also returns the fetched recent entries so callers (e.g. recallMemories)
// can reuse them without a redundant GetRecent call.
func (a *Agent) buildSystemPrompt() (string, []eidetic.MemoryEntry) {
	var sb strings.Builder
	sb.WriteString(a.sysPromptStatic)

	// Dynamic: current date/time
	tz := a.getCfg().Agents.Defaults.UserTimezone
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	fmt.Fprintf(&sb, "Current date/time: %s (%s)\n\n", now.Format("2006-01-02 15:04:05"), tz)

	// Dynamic: memory (re-read only when mtime changes)
	if content := a.loadMemoryCached(); content != "" {
		sb.WriteString("## Memory\n\n")
		sb.WriteString(content)
		sb.WriteString("\n\n")
	}

	// Dynamic: eidetic-specific rules (only when integration is active)
	if a.getEidetic() != nil {
		sb.WriteString("- Before asking the user a question, use eidetic_search to check if you already know the answer from a previous conversation. Only ask if memory has no relevant result.\n\n")
	}

	// Dynamic: recent Eidetic memory (bounded 2s cap; non-fatal on error)
	var recentEntries []eidetic.MemoryEntry
	if c := a.getEidetic(); c != nil {
		cfg := a.getCfg()
		limit := cfg.Eidetic.RecentLimit
		if limit <= 0 {
			limit = 20
		}
		rCtx, rCancel := context.WithTimeout(context.Background(), 2*time.Second)
		entries, rErr := c.GetRecent(rCtx, a.eideticAgentID(), limit)
		rCancel()
		if rErr != nil {
			a.logger.Debugf("eidetic: get_recent failed (non-fatal): %v", rErr)
		} else if len(entries) > 0 {
			recentEntries = entries
			sb.WriteString("## Recent Memory\n\n")
			for _, e := range entries {
				fmt.Fprintf(&sb, "- [%s] %s\n",
					e.Timestamp.Format("2006-01-02 15:04"),
					e.Content,
				)
			}
			sb.WriteString("\n")
		}
	}

	return sb.String(), recentEntries
}

// buildLightSystemPrompt constructs a minimal system prompt used by ChatLight,
// containing only the identity line, current time, and HEARTBEAT.md context.
func (a *Agent) buildLightSystemPrompt() string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "You are %s, %s.\n", a.def.Identity.Name, a.def.Identity.Theme)

	tz := a.getCfg().Agents.Defaults.UserTimezone
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	fmt.Fprintf(&sb, "Current date/time: %s (%s)\n\n", now.Format("2006-01-02 15:04:05"), tz)

	if a.workspace != "" {
		if content := memory.LoadHeartbeatMD(a.workspace); content != "" {
			sb.WriteString("## Heartbeat Context\n\n")
			sb.WriteString(content)
			sb.WriteString("\n\n")
		}
	}

	return sb.String()
}

// ---------------------------------------------------------------------------
// Semantic recall — search eidetic memory using the user's message
// ---------------------------------------------------------------------------

// buildSystemPromptWithRecall builds the full system prompt and injects any
// eidetic memories semantically relevant to the current user message.
// This is the default promptFn for Chat and ChatStream.
func (a *Agent) buildSystemPromptWithRecall(ctx context.Context, userText string) string {
	base, recentEntries := a.buildSystemPrompt()

	recalled := a.recallMemories(ctx, userText, recentEntries)
	if recalled == "" {
		return base
	}

	// Insert recalled memories just before the trailing newline so they appear
	// after recent memory but before the conversation history.
	return base + recalled
}

// recallMemories searches eidetic memory for entries semantically relevant to
// the user's current message, deduplicates against the recent-memory entries
// already in the prompt, and returns a formatted markdown section (or "").
func (a *Agent) recallMemories(ctx context.Context, userText string, recentEntries []eidetic.MemoryEntry) string {
	c := a.getEidetic()
	if c == nil || userText == "" {
		return ""
	}

	cfg := a.getCfg()
	if cfg.Eidetic.RecallEnabled != nil && !*cfg.Eidetic.RecallEnabled {
		return ""
	}

	// Short messages (< 3 words) rarely produce meaningful semantic matches.
	if len(strings.Fields(userText)) < 3 {
		return ""
	}

	limit := cfg.Eidetic.RecallLimit
	if limit <= 0 {
		limit = 5
	}
	threshold := cfg.Eidetic.RecallThreshold
	if threshold <= 0 {
		threshold = 0.4
	}

	timeoutSec := cfg.Eidetic.RecallTimeoutS
	if timeoutSec <= 0 {
		timeoutSec = cfg.Eidetic.TimeoutSeconds // default: 5s
	}

	rCtx, rCancel := context.WithTimeout(ctx, time.Duration(timeoutSec)*time.Second)
	defer rCancel()

	// Generate query embedding for hybrid search (non-fatal if unavailable).
	queryVec := a.embed(rCtx, userText)
	useHybrid := queryVec != nil

	// Request more results than needed so MMR has room to diversify.
	fetchLimit := limit * 2
	if fetchLimit < 10 {
		fetchLimit = 10
	}

	results, err := c.SearchMemory(rCtx, eidetic.SearchRequest{
		Query:     userText,
		Limit:     fetchLimit,
		Threshold: threshold,
		Vector:    queryVec,
		Hybrid:    useHybrid,
	})
	if err != nil {
		a.logger.Debugf("eidetic: recall search failed (non-fatal): %v", err)
		return ""
	}
	if len(results) == 0 {
		return ""
	}

	// Apply MMR for diversity (lambda=0.7 favors relevance with some diversity).
	results = eidetic.MMR(results, 0.7, limit)

	// Build a set of recent entries already in the prompt so we don't duplicate.
	recentSet := make(map[string]struct{}, len(recentEntries))
	for _, e := range recentEntries {
		recentSet[e.Content] = struct{}{}
	}

	var sb strings.Builder
	sb.WriteString("## Recalled Memories\n\n")
	sb.WriteString("The following are relevant memories from previous conversations:\n\n")
	count := 0
	for _, r := range results {
		if r.Relevance < threshold {
			continue
		}
		if _, dup := recentSet[r.Content]; dup {
			continue
		}
		count++
		fmt.Fprintf(&sb, "- [%s] (relevance: %.0f%%) %s\n",
			r.Timestamp.Format("2006-01-02"),
			r.Relevance*100,
			r.Content,
		)
	}
	if count == 0 {
		return ""
	}

	sb.WriteString("\n")
	return sb.String()
}

// ---------------------------------------------------------------------------
// MEMORY.md helpers
// ---------------------------------------------------------------------------

// memoryMDPath returns the filesystem path to MEMORY.md.
func (a *Agent) memoryMDPath() string {
	if a.workspace == "" {
		return ""
	}
	return filepath.Join(a.workspace, "MEMORY.md")
}

// loadMemoryCached returns the MEMORY.md content, re-reading from disk only
// when the file's mtime has changed.
func (a *Agent) loadMemoryCached() string {
	if !a.getCfg().Agents.Defaults.Memory.Enabled {
		return ""
	}
	p := a.memoryMDPath()
	if p == "" {
		return ""
	}
	info, err := os.Stat(p)
	if err != nil {
		return ""
	}

	// Fast path: check if mtime is unchanged under lock (no I/O).
	a.memoryMu.Lock()
	if info.ModTime().Equal(a.memoryMtime) && a.memoryCache != "" {
		cached := a.memoryCache
		a.memoryMu.Unlock()
		return cached
	}
	a.memoryMu.Unlock()

	// Slow path: read file outside lock, then swap cache.
	content := memory.LoadMemoryMD(a.workspace)
	a.memoryMu.Lock()
	a.memoryMtime = info.ModTime()
	a.memoryCache = content
	a.memoryMu.Unlock()
	return content
}
