// Package initialize implements the `gopherclaw init` first-run wizard.
package initialize

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/EMSERO/gopherclaw/internal/migrate"
)

// Run executes the interactive init wizard.
func Run() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("get home dir: %w", err)
	}
	return runWithHome(home, bufio.NewReader(os.Stdin))
}

// runWithHome is the testable core of Run. It accepts a home directory and
// a reader instead of using os.UserHomeDir() and os.Stdin directly.
func runWithHome(home string, reader *bufio.Reader) error {
	gcDir := filepath.Join(home, ".gopherclaw")
	configPath := filepath.Join(gcDir, "config.json")

	fmt.Println("=== GopherClaw Init ===")
	fmt.Println()

	// Step 1: Detect OpenClaw and offer migration
	ocDir := filepath.Join(home, ".openclaw")
	if _, err := os.Stat(filepath.Join(ocDir, "openclaw.json")); err == nil {
		fmt.Println("Found existing OpenClaw installation at ~/.openclaw/")
		fmt.Print("Migrate config and sessions to GopherClaw? [Y/n] ")
		answer, _ := reader.ReadString('\n')
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer == "" || answer == "y" || answer == "yes" {
			fmt.Println("Migrating config...")
			cfgOut, err := migrate.MigrateConfig("", "")
			if err != nil {
				fmt.Fprintf(os.Stderr, "  config migration failed: %v\n", err)
			} else {
				fmt.Printf("  config: %s\n", cfgOut)
			}

			fmt.Println("Migrating sessions...")
			n, err := migrate.Run("", "")
			if err != nil {
				fmt.Fprintf(os.Stderr, "  session migration failed: %v\n", err)
			} else {
				fmt.Printf("  migrated %d session(s)\n", n)
			}

			// If config was migrated, we're done with setup
			if _, err := os.Stat(configPath); err == nil {
				fmt.Println()
				fmt.Println("Migration complete. Config is at ~/.gopherclaw/config.json")
				return setupWorkspace(gcDir)
			}
		}
	}

	// Step 2: Fresh install wizard
	if _, err := os.Stat(configPath); err == nil {
		fmt.Printf("Config already exists at %s\n", configPath)
		fmt.Print("Overwrite? [y/N] ")
		answer, _ := reader.ReadString('\n')
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer != "y" && answer != "yes" {
			fmt.Println("Keeping existing config.")
			return setupWorkspace(gcDir)
		}
	}

	cfg := make(map[string]any)

	// Model provider
	fmt.Println()
	fmt.Println("=== Model Provider ===")
	fmt.Println("1) GitHub Copilot (requires VS Code Copilot extension)")
	fmt.Println("2) OpenRouter (API key required)")
	fmt.Println("3) Anthropic (API key required)")
	fmt.Println("4) OpenAI (API key required)")
	fmt.Print("Choose [1]: ")
	choice, _ := reader.ReadString('\n')
	choice = strings.TrimSpace(choice)
	if choice == "" {
		choice = "1"
	}

	agents := map[string]any{
		"defaults": map[string]any{
			"userTimezone":   "UTC",
			"timeoutSeconds": 300,
			"workspace":      filepath.Join(gcDir, "workspace"),
		},
		"list": []any{
			map[string]any{
				"id":      "main",
				"default": true,
				"identity": map[string]any{
					"name":  "GopherClaw",
					"theme": "helpful AI assistant",
				},
			},
		},
	}

	env := make(map[string]any)

	switch choice {
	case "1":
		agents["defaults"].(map[string]any)["model"] = map[string]any{
			"primary":   "github-copilot/claude-sonnet-4.6",
			"fallbacks": []string{"github-copilot/gpt-4.1"},
		}
		fmt.Println("Using GitHub Copilot. Make sure VS Code Copilot extension is installed.")
	case "2":
		fmt.Print("OpenRouter API key: ")
		key, _ := reader.ReadString('\n')
		key = strings.TrimSpace(key)
		if key != "" {
			env["OPENROUTER_API_KEY"] = key
		}
		agents["defaults"].(map[string]any)["model"] = map[string]any{
			"primary": "openrouter/anthropic/claude-sonnet-4.6",
		}
		cfg["providers"] = map[string]any{
			"openrouter": map[string]any{"apiKey": key},
		}
	case "3":
		fmt.Print("Anthropic API key: ")
		key, _ := reader.ReadString('\n')
		key = strings.TrimSpace(key)
		if key != "" {
			env["ANTHROPIC_API_KEY"] = key
		}
		agents["defaults"].(map[string]any)["model"] = map[string]any{
			"primary": "anthropic/claude-sonnet-4-20250514",
		}
		cfg["providers"] = map[string]any{
			"anthropic": map[string]any{"apiKey": key},
		}
	case "4":
		fmt.Print("OpenAI API key: ")
		key, _ := reader.ReadString('\n')
		key = strings.TrimSpace(key)
		if key != "" {
			env["OPENAI_API_KEY"] = key
		}
		agents["defaults"].(map[string]any)["model"] = map[string]any{
			"primary": "openai/gpt-4.1",
		}
		cfg["providers"] = map[string]any{
			"openai": map[string]any{"apiKey": key},
		}
	}

	cfg["agents"] = agents
	if len(env) > 0 {
		cfg["env"] = env
	}

	// Channels
	fmt.Println()
	fmt.Println("=== Channels (optional) ===")

	channels := make(map[string]any)

	fmt.Print("Telegram bot token (enter to skip): ")
	tgToken, _ := reader.ReadString('\n')
	tgToken = strings.TrimSpace(tgToken)
	if tgToken != "" {
		channels["telegram"] = map[string]any{
			"enabled":    true,
			"botToken":   tgToken,
			"dmPolicy":   "pairing",
			"streamMode": "partial",
		}
	}

	fmt.Print("Discord bot token (enter to skip): ")
	dcToken, _ := reader.ReadString('\n')
	dcToken = strings.TrimSpace(dcToken)
	if dcToken != "" {
		channels["discord"] = map[string]any{
			"enabled":  true,
			"botToken": dcToken,
		}
	}

	fmt.Print("Slack bot token (enter to skip): ")
	slToken, _ := reader.ReadString('\n')
	slToken = strings.TrimSpace(slToken)
	if slToken != "" {
		fmt.Print("Slack app token: ")
		slApp, _ := reader.ReadString('\n')
		slApp = strings.TrimSpace(slApp)
		channels["slack"] = map[string]any{
			"enabled":  true,
			"botToken": slToken,
			"appToken": slApp,
		}
	}

	if len(channels) > 0 {
		cfg["channels"] = channels
	}

	// Gateway
	cfg["gateway"] = map[string]any{
		"port": 18789,
		"bind": "loopback",
		"controlUi": map[string]any{
			"enabled":  true,
			"basePath": "/gopherclaw",
		},
		"reload": map[string]any{
			"mode":       "hybrid",
			"debounceMs": 300,
		},
	}

	// Logging
	cfg["logging"] = map[string]any{
		"level":        "info",
		"consoleLevel": "info",
		"file":         filepath.Join(gcDir, "logs", "gopherclaw.log"),
	}

	// Session
	cfg["session"] = map[string]any{
		"scope": "per-sender",
		"reset": map[string]any{
			"mode":        "daily",
			"atHour":      4,
			"idleMinutes": 120,
		},
	}

	// Write config
	if err := os.MkdirAll(gcDir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	out = append(out, '\n')

	if err := os.WriteFile(configPath, out, 0600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	fmt.Printf("\nConfig written to %s\n", configPath)

	if err := setupWorkspace(gcDir); err != nil {
		return err
	}

	// Skill picker (REQ-022)
	return pickSkills(gcDir, reader)
}

// setupWorkspace creates the workspace directory structure.
func setupWorkspace(gcDir string) error {
	dirs := []string{
		filepath.Join(gcDir, "workspace", "skills"),
		filepath.Join(gcDir, "workspace", "agents", "orchestrator"),
		filepath.Join(gcDir, "logs"),
		filepath.Join(gcDir, "state"),
		filepath.Join(gcDir, "agents", "main", "sessions"),
		filepath.Join(gcDir, "credentials"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}

	// Write default orchestrator identity if not present
	orchIdentity := filepath.Join(gcDir, "workspace", "agents", "orchestrator", "IDENTITY.md")
	if _, err := os.Stat(orchIdentity); os.IsNotExist(err) {
		if err := os.WriteFile(orchIdentity, []byte(defaultOrchestratorIdentity), 0644); err != nil {
			return fmt.Errorf("write orchestrator identity: %w", err)
		}
		fmt.Println("  created orchestrator identity template")
	}

	fmt.Println()
	fmt.Println("Init complete. Run: gopherclaw")
	return nil
}

// skillEntry describes a skill available for installation.
type skillEntry struct {
	Name        string
	Description string
}

// builtinSkills is a curated list of known skills bundled with GopherClaw.
// These serve as defaults when a skill registry (CrawHub) is unavailable.
var builtinSkills = []skillEntry{
	{Name: "spec-engineer", Description: "Interactive requirements gathering and specification generation"},
	{Name: "calendar-manager", Description: "Google Calendar event management"},
	{Name: "project-tracker", Description: "Project and task tracking"},
	{Name: "thunderbird-email", Description: "Email management via Thunderbird"},
}

// pickSkills presents available skills and lets the user select which to install.
func pickSkills(gcDir string, reader *bufio.Reader) error {
	skillsDir := filepath.Join(gcDir, "workspace", "skills")

	// Discover already-installed skills
	installed := discoverSkills(skillsDir)

	if len(installed) > 0 {
		fmt.Println()
		fmt.Println("=== Installed Skills ===")
		for _, s := range installed {
			fmt.Printf("  ✓ %s\n", s)
		}
		fmt.Printf("  %d skill(s) found in %s\n", len(installed), skillsDir)
		fmt.Println()
		fmt.Println("You can add more skills by placing directories in the skills folder.")
		return nil
	}

	// No skills installed — offer built-in defaults
	fmt.Println()
	fmt.Println("=== Skills ===")
	fmt.Println("No skills installed. Available built-in skills:")
	fmt.Println()
	for i, s := range builtinSkills {
		fmt.Printf("  %d) %s — %s\n", i+1, s.Name, s.Description)
	}
	fmt.Println()
	fmt.Println("Skills can be installed later by placing directories under:")
	fmt.Printf("  %s\n", skillsDir)
	fmt.Println()
	fmt.Println("(Skill installation from CrawHub registry is not yet available.)")

	return nil
}

// discoverSkills returns names of skill directories that contain a SKILL.md.
func discoverSkills(skillsDir string) []string {
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		skillFile := filepath.Join(skillsDir, e.Name(), "SKILL.md")
		if _, err := os.Stat(skillFile); err == nil {
			names = append(names, e.Name())
		} else {
			// REQ-023: skills without SKILL.md are still valid (manual drop-in)
			names = append(names, e.Name()+" (no SKILL.md)")
		}
	}
	return names
}

const defaultOrchestratorIdentity = `You are the Orchestrator — a planner and synthesizer. You do NOT execute work yourself. Your role is:

1. Receive a complex task from the main agent
2. Break it into subtasks and produce a JSON task graph
3. Execute the graph using the ` + "`dispatch`" + ` tool
4. Receive results from all subtasks
5. Synthesize results into a single coherent response to return to the main agent

## Available Agents

Your available agents are listed via the delegate tool's agent registry. Each agent_id in your task graph must match a registered agent. If you are unsure which agents are available, check the delegate tool description or the Subagents section of your system prompt.

## Task Graph Production

Call the ` + "`dispatch`" + ` tool with a task_graph containing a tasks array. Each task has:
- **id** (string) — unique task identifier
- **agent_id** (string) — which agent handles this task (must be a registered agent)
- **message** (string) — the instruction for the agent
- **depends_on** ([]string) — task IDs that must complete first; empty if independent
- **blocking** (bool) — if true, failure cancels all dependents; if false, failure is recorded but other tasks continue
- **timeout_seconds** (int, optional) — per-task timeout

Use {{task-id.output}} in message fields to interpolate upstream task output.

## Dependency and Blocking Guidance

- Set **blocking: true** for tasks whose output is required by downstream tasks or whose failure makes the overall result meaningless.
- Set **blocking: false** for best-effort enrichment tasks (e.g., fetching supplementary data, optional formatting passes).
- Keep dependency chains short. Prefer wide, parallel graphs over deep sequential chains.
- Tasks with no depends_on run in parallel automatically — maximize parallelism by only adding dependencies where the output is genuinely needed.

## Synthesis

After dispatch completes, you receive a structured result set with per-task status and output. Your job:

1. **Deliver the actual output** — if the task was "research X and write a report," the synthesis IS the report. Never respond with "I've completed the research, let me know if you want to see it."
2. **Include per-task details as an appendix** — after the main synthesis, include a brief appendix with individual task outputs so the main agent can reference specifics if the user asks.
3. **Handle partial failures gracefully** — if some tasks failed, synthesize what succeeded and clearly explain what failed and why. Never return an empty response.
`
