# Skill Authoring Guide

Skills extend GopherClaw's capabilities by injecting domain-specific instructions and context into the agent's system prompt. Skills are markdown files — no Go code required.

---

## Quick Start

1. Create a directory under your workspace skills folder:
   ```bash
   mkdir -p ~/.gopherclaw/workspace/skills/my-skill
   ```

2. Create a `SKILL.md` file:
   ```bash
   cat > ~/.gopherclaw/workspace/skills/my-skill/SKILL.md << 'EOF'
   ---
   name: my-skill
   description: A short description of what this skill does
   ---

   ## When to Use

   Use this skill when the user asks about [topic].

   ## How It Works

   1. Step one
   2. Step two
   3. Step three

   ## Output

   Always produce output in [format].
   EOF
   ```

3. Restart GopherClaw. The skill is loaded automatically.

---

## SKILL.md Format

Each skill is a directory containing a `SKILL.md` file. The file has two parts:

### YAML Frontmatter

```yaml
---
name: my-skill
description: One-line description shown in skill listings
---
```

| Field | Required | Description |
|-------|----------|-------------|
| `name` | No | Skill name. Defaults to the directory name if omitted |
| `description` | No | Short description for display purposes |

The frontmatter must be delimited by `---` on its own line.

### Markdown Body

Everything after the closing `---` is the skill content. This is injected verbatim into the agent's system prompt under a `## Skills` section.

The body should contain:
- **When to activate** — trigger phrases or situations
- **Instructions** — step-by-step behavior
- **Output format** — what the agent should produce
- **Constraints** — what the agent should avoid

---

## Available Tools

Skills can instruct the agent to use any of its built-in tools:

| Tool | Description | Example Usage |
|------|-------------|---------------|
| `exec` | Run shell commands via `bash -c` | `exec({"command": "curl https://..."})` |
| `read_file` | Read a file from disk | `read_file({"path": "/home/user/data.json"})` |
| `write_file` | Write content to a file | `write_file({"path": "output.md", "content": "..."})` |
| `list_dir` | List directory contents | `list_dir({"path": "/home/user/project"})` |
| `web_search` | Search the web (DuckDuckGo) | `web_search({"query": "..."})` |
| `web_fetch` | Fetch and extract text from a URL | `web_fetch({"url": "https://..."})` |
| `memory_append` | Append to MEMORY.md | `memory_append({"content": "..."})` |
| `memory_get` | Read current MEMORY.md | `memory_get({})` |
| `browser` | Browser automation (Chrome) | `browser({"action": "navigate", "url": "..."})` |
| `delegate` | Call another agent | `delegate({"agent_id": "coding-agent", "message": "..."})` |
| `notify_user` | Send a proactive message to the user | `notify_user({"message": "Task completed!"})` |
| `eidetic_search` | Search semantic memory for past conversations | `eidetic_search({"query": "deployment decisions"})` |
| `eidetic_append` | Store a memory entry in semantic memory | `eidetic_append({"content": "User prefers dark mode"})` |

Tools must be enabled in `config.json` — `web_search` and `web_fetch` require `tools.web.search.enabled` and `tools.web.fetch.enabled` respectively. The `browser` tool requires `tools.browser.enabled` and Chrome/Chromium installed. The `eidetic_search` and `eidetic_append` tools require `eidetic.enabled: true` and a running Eidetic server.

---

## Directory Layout

```
~/.gopherclaw/workspace/skills/my-skill/
├── SKILL.md              # Required — skill definition
├── references/           # Optional — reference docs loaded by the skill
│   ├── templates/        # Template files
│   └── events.md         # Domain data
├── scripts/              # Optional — helper scripts called via exec tool
│   ├── check_data.sh
│   └── process.py
└── designs/              # Optional — artifacts produced by the skill
```

The `references/` directory is a convention — the agent can read files from it using `read_file`. Skills often instruct the agent to check reference files for context before acting.

---

## Workspace Docs

Files placed directly in the workspace root (`~/.gopherclaw/workspace/*.md`) are loaded into every agent's system prompt under a `## Workspace` section. Common workspace docs:

| File | Purpose |
|------|---------|
| `MEMORY.md` | Persistent memory (updated by memory tools, re-read on mtime change) |
| `IDENTITY.md` | Custom identity/personality instructions |
| `USER.md` | User preferences and context |

These are loaded by `LoadWorkspaceMDs()` — only `.md` files directly in the workspace root are included (not subdirectories).

---

## Worked Example: Weather Skill

```markdown
---
name: weather-reporter
description: Check weather and provide forecasts
---

## Trigger

Activate when the user asks about weather, forecasts, or temperature for any location.

## Process

1. Use the `exec` tool to call: `curl -s "wttr.in/{location}?format=j1"`
2. Parse the JSON response for current conditions and forecast
3. Present a concise summary

## Output Format

- Current: temperature, conditions, humidity, wind
- Forecast: next 3 days, high/low, conditions
- Use °F for US locations, °C otherwise

## Constraints

- Do not make up weather data
- If the API is unreachable, say so honestly
- Default to the user's timezone for "today" and "tomorrow"
```

---

## Manual Drop-In (REQ-023)

Skills can be installed by simply placing a directory under `~/.gopherclaw/workspace/skills/`. No registry, CLI command, or metadata file is required beyond `SKILL.md`.

If `SKILL.md` is missing, the directory is still recognized (shown as "no SKILL.md" in `gopherclaw init`), but no content is injected into the system prompt.

---

## OpenClaw Compatibility (REQ-024)

GopherClaw uses the same `SKILL.md` format as OpenClaw. Skills from `~/.openclaw/workspace/skills/` work without modification after migration. The frontmatter fields (`name`, `description`) and markdown body format are identical.
