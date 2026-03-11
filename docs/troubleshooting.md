# Troubleshooting

Common issues and how to fix them.

---

## 1. "failed to load config" on startup

**Symptoms:** GopherClaw exits immediately with `failed to load config: read config ~/.gopherclaw/config.json: no such file or directory`.

**Cause:** No config file exists.

**Fix:** Run the init wizard:
```bash
gopherclaw init
```

Or if migrating from OpenClaw:
```bash
gopherclaw --migrate
```

---

## 2. GitHub Copilot token errors

**Symptoms:** `auth check failed`, `copilot token exchange: 401`, or `no providers available` in logs.

**Cause:** The Copilot token exchange requires a valid VS Code Copilot session. Tokens expire and need periodic refresh.

**Fix:**

1. Ensure VS Code with the Copilot extension is installed and signed in
2. Check that `~/.config/github-copilot/hosts.json` (Linux) or `~/Library/Application Support/github-copilot/hosts.json` (macOS) exists and contains a valid `oauth_token`
3. Verify auth works:
   ```bash
   gopherclaw --check
   ```
4. If the token is stale, open VS Code, use Copilot Chat once to refresh the token, then restart GopherClaw

GopherClaw automatically refreshes tokens in the background, but the initial token must be valid.

---

## 3. Telegram bot not responding

**Symptoms:** Bot is online in Telegram but ignores messages.

**Cause:** User is not paired.

**Fix:**

1. Check the GopherClaw log for the pairing code:
   ```
   telegram: pairing code: 482916
   ```
2. Send `/pair 482916` to the bot in a DM
3. The bot should respond with a confirmation

If using `dmPolicy: "allowlist"` instead of `"pairing"`, add the user's Telegram ID to `channels.telegram.allowUsers` in config.

**Group messages:** By default, the bot only responds when mentioned (`@BotName`). Change this with `channels.telegram.groupPolicy`:
- `"mention"` (default) — respond only when mentioned
- `"open"` — respond to all messages
- `"allowlist"` — respond only to paired users
- `"disabled"` — ignore all group messages

---

## 4. "no providers available" or model not found

**Symptoms:** Agent calls fail with provider errors. Logs show `no providers available` or `model not found`.

**Cause:** The configured model ID doesn't match any registered provider.

**Fix:**

1. Check your model config:
   ```bash
   gopherclaw --check
   ```
2. Verify the model ID format is `provider/model-name`:
   - `github-copilot/claude-sonnet-4.6`
   - `anthropic/claude-sonnet-4-20250514`
   - `openai/gpt-4.1`
   - `openrouter/anthropic/claude-sonnet-4.6`
3. Ensure the provider has valid credentials — either in `providers` section or `env` map
4. If using fallbacks, ensure at least one fallback model is reachable

---

## 5. Gateway auth token lost or unknown

**Symptoms:** Can't authenticate to the HTTP API. Token was auto-generated but not saved or forgotten.

**Cause:** Token was generated on first run and logged, but the log has scrolled past.

**Fix:**

Option A — Check the config file:
```bash
cat ~/.gopherclaw/config.json | grep -A2 '"auth"'
```

Option B — Check the log file:
```bash
grep "auth token" ~/.gopherclaw/logs/gopherclaw.log
```

Option C — Set a known token in config:
```json
{
  "gateway": {
    "auth": {
      "mode": "token",
      "token": "your-chosen-token-here"
    }
  }
}
```

Restart GopherClaw after changing the token.

---

## 6. Browser tool fails: "Chrome not found"

**Symptoms:** The browser tool returns errors about Chrome/Chromium not being found.

**Cause:** The browser tool uses chromedp which requires Chrome or Chromium to be installed.

**Fix:**

Install Chrome or Chromium:
```bash
# Ubuntu/Debian
sudo apt install chromium-browser

# macOS
brew install --cask google-chrome

# Fedora
sudo dnf install chromium
```

If Chrome is installed at a non-standard path, set it in config:
```json
{
  "tools": {
    "browser": {
      "enabled": true,
      "chromePath": "/usr/bin/chromium-browser"
    }
  }
}
```

---

## 7. systemd service won't start

**Symptoms:** `systemctl --user start gopherclaw-gateway` fails or the service exits immediately.

**Cause:** Usually a config error, missing binary, or path issue.

**Fix:**

1. Check the service status:
   ```bash
   systemctl --user status gopherclaw-gateway
   journalctl --user -u gopherclaw-gateway -n 50
   ```

2. Verify the binary path in the service file:
   ```bash
   cat ~/.config/systemd/user/gopherclaw-gateway.service
   ```
   The `ExecStart` path must point to the actual binary location.

3. Test the binary directly:
   ```bash
   ~/.local/bin/gopherclaw --check
   ```

4. If using environment variables, ensure they're set in the service file:
   ```ini
   [Service]
   Environment=HOME=/home/user
   ExecStart=/home/user/.local/bin/gopherclaw
   ```

5. Reload after changes:
   ```bash
   systemctl --user daemon-reload
   systemctl --user restart gopherclaw-gateway
   ```

---

## 8. macOS launchd service setup

**Symptoms:** Want to run GopherClaw as a background service on macOS.

**Fix:**

1. Copy the plist to LaunchAgents:
   ```bash
   cp gopherclaw.plist ~/Library/LaunchAgents/com.emsero.gopherclaw.plist
   ```

2. Edit the plist to set the correct binary path:
   ```bash
   nano ~/Library/LaunchAgents/com.emsero.gopherclaw.plist
   ```

3. Load the service:
   ```bash
   launchctl load ~/Library/LaunchAgents/com.emsero.gopherclaw.plist
   ```

4. Check status:
   ```bash
   launchctl list | grep gopherclaw
   ```

5. View logs:
   ```bash
   tail -f ~/.gopherclaw/logs/gopherclaw.log
   ```

---

## 9. Sessions not resetting

**Symptoms:** Conversations carry over indefinitely. Old context pollutes new interactions.

**Cause:** Session reset is not configured or the reset mode is disabled.

**Fix:**

Configure session reset in `config.json`:
```json
{
  "session": {
    "reset": {
      "mode": "daily",
      "atHour": 4
    },
    "idleMinutes": 120
  }
}
```

- `mode: "daily"` — resets all sessions at the specified hour (default 4 AM)
- `idleMinutes: 120` — resets sessions after 2 hours of inactivity

To manually reset a session, use the `/new` or `/reset` slash command in Telegram/Discord/Slack, or clear via the API:
```bash
curl -X POST http://127.0.0.1:18789/gopherclaw/sessions/clear-all \
  -H "Authorization: Bearer $TOKEN"
```

---

## 10. Slack bot not connecting

**Symptoms:** Slack bot doesn't come online. Logs show Socket Mode connection errors.

**Cause:** Slack requires both a bot token (`xoxb-...`) and an app-level token (`xapp-...`) for Socket Mode.

**Fix:**

1. Verify both tokens are set:
   ```json
   {
     "channels": {
       "slack": {
         "enabled": true,
         "botToken": "xoxb-...",
         "appToken": "xapp-..."
       }
     }
   }
   ```

2. In the Slack app settings (api.slack.com):
   - Enable **Socket Mode** under Settings
   - Generate an **App-Level Token** with `connections:write` scope
   - Ensure the bot has the required OAuth scopes: `chat:write`, `channels:read`, `im:read`, `im:history`, `users:read`

3. Invite the bot to channels where it should respond

---

## 11. HTTP 429 "Too Many Requests" from gateway

**Symptoms:** API calls return `429 Too Many Requests`. Log shows rate limit messages.

**Cause:** Per-IP rate limiting is enabled and the client is exceeding the configured rate.

**Fix:**

1. Check your rate limit config:
   ```json
   {
     "gateway": {
       "rateLimit": {
         "rps": 10,
         "burst": 20
       }
     }
   }
   ```

2. Increase `rps` (requests per second) and/or `burst` capacity. Set `rps: 0` to disable rate limiting entirely.

3. If running automated scripts, add delays between requests or use the webhook endpoint with streaming.

---

## 12. web_fetch blocked by SSRF protection

**Symptoms:** `web_fetch` fails with "SSRF: blocked" or "address is private/reserved" errors.

**Cause:** The URL resolves to a private, loopback, link-local, or multicast IP address. GopherClaw blocks these to prevent Server-Side Request Forgery.

**Fix:**

- If you need to fetch from a local service (e.g. `localhost:3000`), note that SSRF protection cannot be disabled — this is a security boundary.
- Use the `exec` tool with `curl` as a workaround for trusted internal services:
  ```
  exec: curl -s http://localhost:3000/api/data
  ```
- RFC 2544 benchmark range (`198.18.0.0/15`) is explicitly exempted.

---

## 13. Exec command blocked — confirmation timeout

**Symptoms:** A command the agent tried to run was blocked. Log shows "destructive command blocked: confirmation timeout" or "no confirmation channel available".

**Cause:** The command matched the built-in destructive command blocklist (e.g. `rm -rf`, `dd`, `shutdown`, `curl | bash`), and either:
- The user didn't confirm within the timeout (default 60 seconds), or
- The command ran in a non-interactive context (cron, webhook) where confirmation can't be collected.

**Fix:**

1. If the command is safe, respond to the confirmation prompt within the timeout when it appears in Telegram/Discord/terminal/dashboard.

2. Adjust the timeout if needed:
   ```json
   {
     "tools": {
       "exec": {
         "confirmTimeoutSec": 120
       }
     }
   }
   ```

3. For cron jobs that need to run commands matching the blocklist, consider wrapping them in a script with a non-matching name.

4. The blocklist is not overridable, but you can add additional patterns via `tools.exec.denyCommands` for environment-specific hardening.

---

## 14. Browser pool errors or stale sessions

**Symptoms:** Browser tool returns errors about "context cancelled", "target closed", or screenshots show stale pages.

**Cause:** Browser sessions have a 10-minute idle timeout. If the session was reaped between actions, subsequent calls fail.

**Fix:**

1. Start new browser sessions with `navigate` before performing actions — don't assume a prior session is still alive.

2. In containers/Docker, ensure Chrome runs with `--no-sandbox`:
   ```json
   {
     "tools": {
       "browser": {
         "enabled": true,
         "noSandbox": true
       }
     }
   }
   ```

3. Check that Chrome/Chromium is installed (see [section 6](#6-browser-tool-fails-chrome-not-found) above).

4. If Chrome crashes frequently, check system memory — chromedp creates a full browser instance per session.

---

## General Debugging

### Enable debug logging

```json
{
  "logging": {
    "level": "debug",
    "consoleLevel": "debug"
  }
}
```

### View real-time logs via SSE

```bash
curl -N http://127.0.0.1:18789/gopherclaw/api/log
```

### Check version

```bash
gopherclaw --version
```

### Verify config and auth

```bash
gopherclaw --check
```
