# Migration Guide: OpenClaw to GopherClaw

GopherClaw is a drop-in replacement for OpenClaw. It reads the same configuration and session formats, supports the same skills, and connects to the same channels. This guide covers how to migrate.

---

## Automatic Migration

If `~/.gopherclaw/config.json` doesn't exist and `~/.openclaw/openclaw.json` does, GopherClaw automatically migrates the config on startup:

```
$ gopherclaw
auto-migrated config → /home/user/.gopherclaw/config.json
gopherclaw 0.4.0-dev starting
```

No manual action required — just run `gopherclaw`.

---

## Manual Migration

### Option 1: `gopherclaw --migrate`

Migrates both config and sessions:

```bash
gopherclaw --migrate
```

Output:
```
Migrating OpenClaw config → ~/.gopherclaw/config.json ...
Config: /home/user/.gopherclaw/config.json
Migrating OpenClaw sessions...
Done. Migrated 12 session(s).
```

### Option 2: `gopherclaw init`

The init wizard detects OpenClaw and offers migration interactively:

```bash
gopherclaw init
```

```
=== GopherClaw Init ===

Found existing OpenClaw installation at ~/.openclaw/
Migrate config and sessions to GopherClaw? [Y/n]
```

If you decline, it proceeds with a fresh setup wizard.

---

## What Gets Migrated

### Config (`openclaw.json` → `config.json`)

The migration:

1. **Copies the full config** as JSON, preserving all fields
2. **Rewrites paths** — any string value containing `~/.openclaw` is replaced with `~/.gopherclaw`
3. **Injects a CLI subagent** — adds a `coding-agent` entry to `agents.list` if not present:
   ```json
   {
     "id": "coding-agent",
     "cliCommand": "claude",
     "cliArgs": ["-p", "--dangerously-skip-permissions"]
   }
   ```
4. **Writes to** `~/.gopherclaw/config.json` with clean indentation
5. **Idempotent** — if the target config already exists, migration is skipped

### Sessions

Session migration converts OpenClaw's event-sourced JSONL format to GopherClaw's flat JSONL format:

| Feature | OpenClaw Format | GopherClaw Format |
|---------|----------------|-------------------|
| Structure | Event-sourced (`{"type":"message","message":{...}}`) | Flat messages (`{"role":"user","content":"..."}`) |
| Timestamps | ISO 8601 strings | Unix milliseconds |
| Tool results | `details.aggregated` field | `content` field |
| File location | `~/.openclaw/agents/main/sessions/` | `~/.gopherclaw/agents/main/sessions/` |

The migration reads `sessions.json` (the session index) and converts each session's JSONL file individually.

---

## What Stays the Same

These work identically in both systems:

- **Skills** — Same `SKILL.md` format with YAML frontmatter. Skills from `~/.openclaw/workspace/skills/` work without modification.
- **Workspace docs** — `MEMORY.md`, `IDENTITY.md`, `USER.md`, etc. Same format.
- **Cron jobs** — `~/.openclaw/cron/jobs.json` format is compatible.
- **Channel tokens** — Telegram, Discord, Slack tokens carry over.
- **Paired users** — Pairing files (e.g., `telegram-default-allowFrom.json`) are compatible.

---

## Config Differences

| Feature | OpenClaw | GopherClaw |
|---------|----------|------------|
| Config file | `~/.openclaw/openclaw.json` | `~/.gopherclaw/config.json` |
| Data directory | `~/.openclaw/` | `~/.gopherclaw/` |
| Auth system | `auth.profiles` + `auth.order` (complex routing) | `providers` section (flat key-value) |
| CLI subagents | Not supported | `cliCommand` + `cliArgs` on agent definitions |
| Auto-update | Not available | `update.autoUpdate` config option |
| `softTrimRatio` | `agents.defaults.contextPruning.softTrimRatio` | Both paths supported (top-level or nested) |

---

## Post-Migration Checklist

After migrating, verify these items:

1. **Provider auth** — Check that API keys are correct in the `providers` section or `env` map
   ```bash
   gopherclaw --check
   ```

2. **Workspace path** — Verify `agents.defaults.workspace` points to the right location. After migration it may still reference `~/.openclaw/workspace` — update it to `~/.gopherclaw/workspace` if you want a clean separation, or leave it pointing to OpenClaw's workspace to share skills between both

3. **Channel connectivity** — Start GopherClaw and verify Telegram/Discord/Slack bots connect:
   ```bash
   gopherclaw
   # Check logs for "telegram: started" / "discord: started" / "slack: started"
   ```

4. **Session history** — Check that migrated sessions are readable:
   ```bash
   ls ~/.gopherclaw/agents/main/sessions/*.jsonl
   ```

5. **Cron jobs** — Review scheduled jobs:
   ```bash
   curl http://127.0.0.1:18789/gopherclaw/api/cron -H "Authorization: Bearer $TOKEN"
   ```

6. **Skills** — Verify skills loaded:
   ```
   # Look for "loaded N skills" in startup log
   gopherclaw 2>&1 | grep skills
   ```

---

## Running Both Side by Side

OpenClaw and GopherClaw can run simultaneously since they use different data directories and different default ports. This is useful for testing before fully switching over.

- OpenClaw: `~/.openclaw/`, port per its config
- GopherClaw: `~/.gopherclaw/`, port `18789` (default)

If both use the same Telegram/Discord/Slack bot tokens, only one can be connected at a time. To test GopherClaw channels, stop OpenClaw first or use different bot tokens.

---

## Known Limitations

1. **OpenClaw plugins are not supported** — Plugin configuration is preserved in the migrated config but has no effect in GopherClaw
2. **Non-message session events are dropped** — Only `"type": "message"` events are migrated; system events and other event types are skipped
3. **Auth profile complexity is simplified** — OpenClaw's `auth.order` and `auth.profiles` routing is copied but GopherClaw uses flat provider-based auth
4. **Tool output aggregation** — OpenClaw's `details.aggregated` field (streaming summary) is used as the tool result content, but the original streaming detail is lost
