package initialize

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// discoverSkills
// ---------------------------------------------------------------------------

func TestDiscoverSkillsWithSKILLMD(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "weather")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# Weather"), 0644); err != nil {
		t.Fatal(err)
	}

	names := discoverSkills(dir)
	if len(names) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(names))
	}
	if names[0] != "weather" {
		t.Errorf("expected 'weather', got %q", names[0])
	}
}

func TestDiscoverSkillsWithoutSKILLMD(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "my-tool")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	// No SKILL.md — REQ-023 manual drop-in

	names := discoverSkills(dir)
	if len(names) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(names))
	}
	if !strings.Contains(names[0], "no SKILL.md") {
		t.Errorf("expected '(no SKILL.md)' suffix, got %q", names[0])
	}
	if !strings.Contains(names[0], "my-tool") {
		t.Errorf("expected skill name 'my-tool', got %q", names[0])
	}
}

func TestDiscoverSkillsMixed(t *testing.T) {
	dir := t.TempDir()

	// Skill with SKILL.md
	withMD := filepath.Join(dir, "alpha")
	os.MkdirAll(withMD, 0755)
	os.WriteFile(filepath.Join(withMD, "SKILL.md"), []byte("---\nname: alpha\n---"), 0644)

	// Skill without SKILL.md
	withoutMD := filepath.Join(dir, "beta")
	os.MkdirAll(withoutMD, 0755)

	// Regular file (not a directory) — should be ignored
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("readme"), 0644)

	names := discoverSkills(dir)
	if len(names) != 2 {
		t.Fatalf("expected 2 skills, got %d: %v", len(names), names)
	}

	foundAlpha, foundBeta := false, false
	for _, n := range names {
		if n == "alpha" {
			foundAlpha = true
		}
		if strings.HasPrefix(n, "beta") && strings.Contains(n, "no SKILL.md") {
			foundBeta = true
		}
	}
	if !foundAlpha {
		t.Error("expected 'alpha' skill")
	}
	if !foundBeta {
		t.Error("expected 'beta (no SKILL.md)' skill")
	}
}

func TestDiscoverSkillsEmptyDir(t *testing.T) {
	dir := t.TempDir()

	names := discoverSkills(dir)
	if len(names) != 0 {
		t.Errorf("expected 0 skills from empty dir, got %d", len(names))
	}
}

func TestDiscoverSkillsNonexistentDir(t *testing.T) {
	names := discoverSkills("/nonexistent/path/that/does/not/exist")
	if names != nil {
		t.Errorf("expected nil from nonexistent dir, got %v", names)
	}
}

// ---------------------------------------------------------------------------
// setupWorkspace
// ---------------------------------------------------------------------------

func TestSetupWorkspace_CreatesDirectories(t *testing.T) {
	gcDir := t.TempDir()

	if err := setupWorkspace(gcDir); err != nil {
		t.Fatalf("setupWorkspace: %v", err)
	}

	expectedDirs := []string{
		filepath.Join("workspace", "skills"),
		filepath.Join("workspace", "agents", "orchestrator"),
		filepath.Join("logs"),
		filepath.Join("state"),
		filepath.Join("agents", "main", "sessions"),
		filepath.Join("credentials"),
	}
	for _, rel := range expectedDirs {
		full := filepath.Join(gcDir, rel)
		info, err := os.Stat(full)
		if err != nil {
			t.Errorf("expected directory %s to exist: %v", rel, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("expected %s to be a directory", rel)
		}
	}
}

func TestSetupWorkspace_CreatesOrchestratorIdentity(t *testing.T) {
	gcDir := t.TempDir()

	if err := setupWorkspace(gcDir); err != nil {
		t.Fatalf("setupWorkspace: %v", err)
	}

	identityPath := filepath.Join(gcDir, "workspace", "agents", "orchestrator", "IDENTITY.md")
	data, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatalf("expected IDENTITY.md to exist: %v", err)
	}
	if len(data) == 0 {
		t.Error("expected IDENTITY.md to have content")
	}
	if string(data) != defaultOrchestratorIdentity {
		t.Error("IDENTITY.md content does not match defaultOrchestratorIdentity")
	}
}

func TestSetupWorkspace_SkipsExistingIdentity(t *testing.T) {
	gcDir := t.TempDir()

	// Pre-create the identity file with custom content.
	orchDir := filepath.Join(gcDir, "workspace", "agents", "orchestrator")
	if err := os.MkdirAll(orchDir, 0755); err != nil {
		t.Fatal(err)
	}
	custom := "custom identity"
	if err := os.WriteFile(filepath.Join(orchDir, "IDENTITY.md"), []byte(custom), 0644); err != nil {
		t.Fatal(err)
	}

	if err := setupWorkspace(gcDir); err != nil {
		t.Fatalf("setupWorkspace: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(orchDir, "IDENTITY.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != custom {
		t.Error("setupWorkspace overwrote existing IDENTITY.md")
	}
}

func TestSetupWorkspace_Idempotent(t *testing.T) {
	gcDir := t.TempDir()

	for i := 0; i < 3; i++ {
		if err := setupWorkspace(gcDir); err != nil {
			t.Fatalf("setupWorkspace call %d: %v", i, err)
		}
	}

	// Still has identity.
	identityPath := filepath.Join(gcDir, "workspace", "agents", "orchestrator", "IDENTITY.md")
	if _, err := os.Stat(identityPath); err != nil {
		t.Errorf("expected IDENTITY.md after repeated calls: %v", err)
	}
}

// ---------------------------------------------------------------------------
// pickSkills
// ---------------------------------------------------------------------------

func TestPickSkills_InstalledSkills(t *testing.T) {
	gcDir := t.TempDir()

	// Create workspace with a skill directory.
	skillsDir := filepath.Join(gcDir, "workspace", "skills", "my-skill")
	if err := os.MkdirAll(skillsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillsDir, "SKILL.md"), []byte("# My Skill"), 0644); err != nil {
		t.Fatal(err)
	}

	reader := bufio.NewReader(strings.NewReader(""))
	if err := pickSkills(gcDir, reader); err != nil {
		t.Fatalf("pickSkills: %v", err)
	}
}

func TestPickSkills_NoSkillsInstalled(t *testing.T) {
	gcDir := t.TempDir()

	// Create empty skills directory.
	if err := os.MkdirAll(filepath.Join(gcDir, "workspace", "skills"), 0755); err != nil {
		t.Fatal(err)
	}

	reader := bufio.NewReader(strings.NewReader(""))
	if err := pickSkills(gcDir, reader); err != nil {
		t.Fatalf("pickSkills: %v", err)
	}
}

func TestPickSkills_SkillsDirMissing(t *testing.T) {
	gcDir := t.TempDir()

	reader := bufio.NewReader(strings.NewReader(""))
	// Should not error even if workspace/skills doesn't exist.
	if err := pickSkills(gcDir, reader); err != nil {
		t.Fatalf("pickSkills: %v", err)
	}
}

// ---------------------------------------------------------------------------
// builtinSkills
// ---------------------------------------------------------------------------

func TestBuiltinSkillsNotEmpty(t *testing.T) {
	if len(builtinSkills) == 0 {
		t.Error("builtinSkills should not be empty")
	}
	for _, s := range builtinSkills {
		if s.Name == "" {
			t.Error("skill name should not be empty")
		}
		if s.Description == "" {
			t.Errorf("skill %q should have a description", s.Name)
		}
	}
}

// ---------------------------------------------------------------------------
// defaultOrchestratorIdentity
// ---------------------------------------------------------------------------

func TestDefaultOrchestratorIdentity(t *testing.T) {
	if defaultOrchestratorIdentity == "" {
		t.Error("defaultOrchestratorIdentity should not be empty")
	}
	if !strings.Contains(defaultOrchestratorIdentity, "Orchestrator") {
		t.Error("defaultOrchestratorIdentity should mention 'Orchestrator'")
	}
}

// ---------------------------------------------------------------------------
// runWithHome (core of Run)
// ---------------------------------------------------------------------------

// helper to read and parse the generated config.json
func readConfig(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".gopherclaw", "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse config.json: %v", err)
	}
	return cfg
}

func TestRunWithHome_FreshInit_DefaultProvider(t *testing.T) {
	home := t.TempDir()

	// Simulate: choose default provider (1), skip all channels, no skills
	input := "1\n\n\n\n"
	reader := bufio.NewReader(strings.NewReader(input))

	if err := runWithHome(home, reader); err != nil {
		t.Fatalf("runWithHome: %v", err)
	}

	cfg := readConfig(t, home)

	// Verify agents section exists
	agents, ok := cfg["agents"].(map[string]any)
	if !ok {
		t.Fatal("expected 'agents' key in config")
	}
	defaults, ok := agents["defaults"].(map[string]any)
	if !ok {
		t.Fatal("expected 'agents.defaults' in config")
	}
	model, ok := defaults["model"].(map[string]any)
	if !ok {
		t.Fatal("expected 'agents.defaults.model' in config")
	}
	primary, _ := model["primary"].(string)
	if !strings.Contains(primary, "github-copilot") {
		t.Errorf("expected github-copilot primary model, got %q", primary)
	}

	// Verify gateway section
	gw, ok := cfg["gateway"].(map[string]any)
	if !ok {
		t.Fatal("expected 'gateway' key in config")
	}
	port, _ := gw["port"].(float64)
	if port != 18789 {
		t.Errorf("expected gateway port 18789, got %v", port)
	}

	// Verify logging section
	logging, ok := cfg["logging"].(map[string]any)
	if !ok {
		t.Fatal("expected 'logging' key in config")
	}
	level, _ := logging["level"].(string)
	if level != "info" {
		t.Errorf("expected logging level 'info', got %q", level)
	}

	// Verify session section
	session, ok := cfg["session"].(map[string]any)
	if !ok {
		t.Fatal("expected 'session' key in config")
	}
	scope, _ := session["scope"].(string)
	if scope != "per-sender" {
		t.Errorf("expected session scope 'per-sender', got %q", scope)
	}

	// No channels should be set (all skipped)
	if _, ok := cfg["channels"]; ok {
		t.Error("expected no 'channels' key when all channels skipped")
	}

	// No env should be set (copilot doesn't need API key)
	if _, ok := cfg["env"]; ok {
		t.Error("expected no 'env' key for github-copilot provider")
	}

	// Workspace directories should exist
	gcDir := filepath.Join(home, ".gopherclaw")
	for _, rel := range []string{
		filepath.Join("workspace", "skills"),
		filepath.Join("logs"),
		filepath.Join("state"),
		filepath.Join("credentials"),
	} {
		if _, err := os.Stat(filepath.Join(gcDir, rel)); err != nil {
			t.Errorf("expected directory %s to exist: %v", rel, err)
		}
	}
}

func TestRunWithHome_FreshInit_OpenRouterProvider(t *testing.T) {
	home := t.TempDir()

	// Choose OpenRouter (2) with API key, skip channels
	input := "2\ntest-openrouter-key\n\n\n\n"
	reader := bufio.NewReader(strings.NewReader(input))

	if err := runWithHome(home, reader); err != nil {
		t.Fatalf("runWithHome: %v", err)
	}

	cfg := readConfig(t, home)

	// Check model
	agents := cfg["agents"].(map[string]any)
	defaults := agents["defaults"].(map[string]any)
	model := defaults["model"].(map[string]any)
	primary, _ := model["primary"].(string)
	if !strings.Contains(primary, "openrouter") {
		t.Errorf("expected openrouter primary model, got %q", primary)
	}

	// Check env
	env, ok := cfg["env"].(map[string]any)
	if !ok {
		t.Fatal("expected 'env' key for openrouter")
	}
	if env["OPENROUTER_API_KEY"] != "test-openrouter-key" {
		t.Errorf("expected OPENROUTER_API_KEY, got %v", env["OPENROUTER_API_KEY"])
	}

	// Check providers
	providers, ok := cfg["providers"].(map[string]any)
	if !ok {
		t.Fatal("expected 'providers' key for openrouter")
	}
	if _, ok := providers["openrouter"]; !ok {
		t.Error("expected 'openrouter' in providers")
	}
}

func TestRunWithHome_FreshInit_AnthropicProvider(t *testing.T) {
	home := t.TempDir()

	// Choose Anthropic (3) with API key, skip channels
	input := "3\ntest-anthropic-key\n\n\n\n"
	reader := bufio.NewReader(strings.NewReader(input))

	if err := runWithHome(home, reader); err != nil {
		t.Fatalf("runWithHome: %v", err)
	}

	cfg := readConfig(t, home)

	agents := cfg["agents"].(map[string]any)
	defaults := agents["defaults"].(map[string]any)
	model := defaults["model"].(map[string]any)
	primary, _ := model["primary"].(string)
	if !strings.Contains(primary, "anthropic") {
		t.Errorf("expected anthropic primary model, got %q", primary)
	}

	env := cfg["env"].(map[string]any)
	if env["ANTHROPIC_API_KEY"] != "test-anthropic-key" {
		t.Errorf("expected ANTHROPIC_API_KEY, got %v", env["ANTHROPIC_API_KEY"])
	}

	providers := cfg["providers"].(map[string]any)
	if _, ok := providers["anthropic"]; !ok {
		t.Error("expected 'anthropic' in providers")
	}
}

func TestRunWithHome_FreshInit_OpenAIProvider(t *testing.T) {
	home := t.TempDir()

	// Choose OpenAI (4) with API key, skip channels
	input := "4\ntest-openai-key\n\n\n\n"
	reader := bufio.NewReader(strings.NewReader(input))

	if err := runWithHome(home, reader); err != nil {
		t.Fatalf("runWithHome: %v", err)
	}

	cfg := readConfig(t, home)

	agents := cfg["agents"].(map[string]any)
	defaults := agents["defaults"].(map[string]any)
	model := defaults["model"].(map[string]any)
	primary, _ := model["primary"].(string)
	if !strings.Contains(primary, "openai") {
		t.Errorf("expected openai primary model, got %q", primary)
	}

	env := cfg["env"].(map[string]any)
	if env["OPENAI_API_KEY"] != "test-openai-key" {
		t.Errorf("expected OPENAI_API_KEY, got %v", env["OPENAI_API_KEY"])
	}

	providers := cfg["providers"].(map[string]any)
	if _, ok := providers["openai"]; !ok {
		t.Error("expected 'openai' in providers")
	}
}

func TestRunWithHome_FreshInit_WithTelegramChannel(t *testing.T) {
	home := t.TempDir()

	// Choose default provider, add telegram token, skip discord and slack
	input := "1\nmy-tg-token\n\n\n"
	reader := bufio.NewReader(strings.NewReader(input))

	if err := runWithHome(home, reader); err != nil {
		t.Fatalf("runWithHome: %v", err)
	}

	cfg := readConfig(t, home)
	channels, ok := cfg["channels"].(map[string]any)
	if !ok {
		t.Fatal("expected 'channels' key when telegram token provided")
	}
	tg, ok := channels["telegram"].(map[string]any)
	if !ok {
		t.Fatal("expected 'channels.telegram'")
	}
	if tg["botToken"] != "my-tg-token" {
		t.Errorf("expected telegram botToken 'my-tg-token', got %v", tg["botToken"])
	}
	if tg["enabled"] != true {
		t.Error("expected telegram enabled=true")
	}
}

func TestRunWithHome_FreshInit_WithDiscordChannel(t *testing.T) {
	home := t.TempDir()

	// Choose default provider, skip telegram, add discord, skip slack
	input := "1\n\nmy-dc-token\n\n"
	reader := bufio.NewReader(strings.NewReader(input))

	if err := runWithHome(home, reader); err != nil {
		t.Fatalf("runWithHome: %v", err)
	}

	cfg := readConfig(t, home)
	channels := cfg["channels"].(map[string]any)
	dc, ok := channels["discord"].(map[string]any)
	if !ok {
		t.Fatal("expected 'channels.discord'")
	}
	if dc["botToken"] != "my-dc-token" {
		t.Errorf("expected discord botToken 'my-dc-token', got %v", dc["botToken"])
	}
}

func TestRunWithHome_FreshInit_WithSlackChannel(t *testing.T) {
	home := t.TempDir()

	// Choose default provider, skip telegram, skip discord, add slack bot+app tokens
	input := "1\n\n\nmy-slack-bot\nmy-slack-app\n"
	reader := bufio.NewReader(strings.NewReader(input))

	if err := runWithHome(home, reader); err != nil {
		t.Fatalf("runWithHome: %v", err)
	}

	cfg := readConfig(t, home)
	channels := cfg["channels"].(map[string]any)
	sl, ok := channels["slack"].(map[string]any)
	if !ok {
		t.Fatal("expected 'channels.slack'")
	}
	if sl["botToken"] != "my-slack-bot" {
		t.Errorf("expected slack botToken 'my-slack-bot', got %v", sl["botToken"])
	}
	if sl["appToken"] != "my-slack-app" {
		t.Errorf("expected slack appToken 'my-slack-app', got %v", sl["appToken"])
	}
}

func TestRunWithHome_FreshInit_AllChannels(t *testing.T) {
	home := t.TempDir()

	// All channels provided
	input := "1\ntg-tok\ndc-tok\nsl-bot\nsl-app\n"
	reader := bufio.NewReader(strings.NewReader(input))

	if err := runWithHome(home, reader); err != nil {
		t.Fatalf("runWithHome: %v", err)
	}

	cfg := readConfig(t, home)
	channels := cfg["channels"].(map[string]any)

	if _, ok := channels["telegram"]; !ok {
		t.Error("expected telegram channel")
	}
	if _, ok := channels["discord"]; !ok {
		t.Error("expected discord channel")
	}
	if _, ok := channels["slack"]; !ok {
		t.Error("expected slack channel")
	}
}

func TestRunWithHome_ExistingConfig_DeclineOverwrite(t *testing.T) {
	home := t.TempDir()
	gcDir := filepath.Join(home, ".gopherclaw")
	if err := os.MkdirAll(gcDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write an existing config
	original := `{"existing": true}`
	configPath := filepath.Join(gcDir, "config.json")
	if err := os.WriteFile(configPath, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}

	// Decline overwrite (default is N)
	input := "\n"
	reader := bufio.NewReader(strings.NewReader(input))

	if err := runWithHome(home, reader); err != nil {
		t.Fatalf("runWithHome: %v", err)
	}

	// Config should be unchanged
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Error("existing config was overwritten despite declining")
	}

	// Workspace dirs should still be created
	if _, err := os.Stat(filepath.Join(gcDir, "workspace", "skills")); err != nil {
		t.Error("expected workspace dirs to be created even when keeping config")
	}
}

func TestRunWithHome_ExistingConfig_AcceptOverwrite(t *testing.T) {
	home := t.TempDir()
	gcDir := filepath.Join(home, ".gopherclaw")
	if err := os.MkdirAll(gcDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write an existing config
	original := `{"existing": true}`
	configPath := filepath.Join(gcDir, "config.json")
	if err := os.WriteFile(configPath, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}

	// Accept overwrite, choose default provider, skip channels
	input := "y\n1\n\n\n\n"
	reader := bufio.NewReader(strings.NewReader(input))

	if err := runWithHome(home, reader); err != nil {
		t.Fatalf("runWithHome: %v", err)
	}

	// Config should be changed
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == original {
		t.Error("existing config was NOT overwritten despite accepting")
	}

	// Should be valid JSON with agents key
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("overwritten config is not valid JSON: %v", err)
	}
	if _, ok := cfg["agents"]; !ok {
		t.Error("overwritten config should have 'agents' key")
	}
}

func TestRunWithHome_ConfigFilePermissions(t *testing.T) {
	home := t.TempDir()

	input := "1\n\n\n\n"
	reader := bufio.NewReader(strings.NewReader(input))

	if err := runWithHome(home, reader); err != nil {
		t.Fatalf("runWithHome: %v", err)
	}

	configPath := filepath.Join(home, ".gopherclaw", "config.json")
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}

	// Config should be written with 0600 permissions (may have umask applied)
	perm := info.Mode().Perm()
	if perm&0077 != 0 {
		t.Errorf("config.json should not be group/world readable, got %o", perm)
	}
}

func TestRunWithHome_ConfigHasNewline(t *testing.T) {
	home := t.TempDir()

	input := "1\n\n\n\n"
	reader := bufio.NewReader(strings.NewReader(input))

	if err := runWithHome(home, reader); err != nil {
		t.Fatalf("runWithHome: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".gopherclaw", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("config.json is empty")
	}
	if data[len(data)-1] != '\n' {
		t.Error("config.json should end with a newline")
	}
}

func TestRunWithHome_AgentsList(t *testing.T) {
	home := t.TempDir()

	input := "1\n\n\n\n"
	reader := bufio.NewReader(strings.NewReader(input))

	if err := runWithHome(home, reader); err != nil {
		t.Fatalf("runWithHome: %v", err)
	}

	cfg := readConfig(t, home)
	agents := cfg["agents"].(map[string]any)

	list, ok := agents["list"].([]any)
	if !ok {
		t.Fatal("expected 'agents.list' array")
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 agent in list, got %d", len(list))
	}

	agent := list[0].(map[string]any)
	if agent["id"] != "main" {
		t.Errorf("expected agent id 'main', got %v", agent["id"])
	}
	if agent["default"] != true {
		t.Error("expected agent to be default")
	}

	identity, ok := agent["identity"].(map[string]any)
	if !ok {
		t.Fatal("expected agent identity")
	}
	if identity["name"] != "GopherClaw" {
		t.Errorf("expected agent name 'GopherClaw', got %v", identity["name"])
	}
}

func TestRunWithHome_Idempotent_WorkspaceSurvives(t *testing.T) {
	home := t.TempDir()

	// First run: fresh init
	input1 := "1\n\n\n\n"
	if err := runWithHome(home, bufio.NewReader(strings.NewReader(input1))); err != nil {
		t.Fatalf("first runWithHome: %v", err)
	}

	// Place a custom file in workspace
	customFile := filepath.Join(home, ".gopherclaw", "workspace", "skills", "test.txt")
	if err := os.WriteFile(customFile, []byte("custom"), 0644); err != nil {
		t.Fatal(err)
	}

	// Second run: existing config, decline overwrite
	input2 := "\n"
	if err := runWithHome(home, bufio.NewReader(strings.NewReader(input2))); err != nil {
		t.Fatalf("second runWithHome: %v", err)
	}

	// Custom file should survive
	data, err := os.ReadFile(customFile)
	if err != nil {
		t.Fatalf("custom file lost after re-init: %v", err)
	}
	if string(data) != "custom" {
		t.Error("custom file content changed")
	}
}

func TestRunWithHome_OpenClawDetected_DeclineMigration(t *testing.T) {
	home := t.TempDir()

	// Create a fake OpenClaw installation
	ocDir := filepath.Join(home, ".openclaw")
	if err := os.MkdirAll(ocDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ocDir, "openclaw.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Decline migration (n), then choose default provider, skip channels
	input := "n\n1\n\n\n\n"
	reader := bufio.NewReader(strings.NewReader(input))

	if err := runWithHome(home, reader); err != nil {
		t.Fatalf("runWithHome: %v", err)
	}

	// Config should be created fresh (not migrated)
	cfg := readConfig(t, home)
	if _, ok := cfg["agents"]; !ok {
		t.Error("expected fresh config with 'agents' key")
	}
}

func TestRunWithHome_SessionResetConfig(t *testing.T) {
	home := t.TempDir()

	input := "1\n\n\n\n"
	reader := bufio.NewReader(strings.NewReader(input))

	if err := runWithHome(home, reader); err != nil {
		t.Fatalf("runWithHome: %v", err)
	}

	cfg := readConfig(t, home)
	session := cfg["session"].(map[string]any)
	reset, ok := session["reset"].(map[string]any)
	if !ok {
		t.Fatal("expected 'session.reset' in config")
	}
	if reset["mode"] != "daily" {
		t.Errorf("expected reset mode 'daily', got %v", reset["mode"])
	}
	atHour, _ := reset["atHour"].(float64)
	if atHour != 4 {
		t.Errorf("expected atHour 4, got %v", atHour)
	}
	idleMin, _ := reset["idleMinutes"].(float64)
	if idleMin != 120 {
		t.Errorf("expected idleMinutes 120, got %v", idleMin)
	}
}

func TestRunWithHome_LoggingFileUsesGcDir(t *testing.T) {
	home := t.TempDir()

	input := "1\n\n\n\n"
	reader := bufio.NewReader(strings.NewReader(input))

	if err := runWithHome(home, reader); err != nil {
		t.Fatalf("runWithHome: %v", err)
	}

	cfg := readConfig(t, home)
	logging := cfg["logging"].(map[string]any)
	logFile, _ := logging["file"].(string)
	expected := filepath.Join(home, ".gopherclaw", "logs", "gopherclaw.log")
	if logFile != expected {
		t.Errorf("expected log file %q, got %q", expected, logFile)
	}
}

func TestRunWithHome_WorkspacePathInConfig(t *testing.T) {
	home := t.TempDir()

	input := "1\n\n\n\n"
	reader := bufio.NewReader(strings.NewReader(input))

	if err := runWithHome(home, reader); err != nil {
		t.Fatalf("runWithHome: %v", err)
	}

	cfg := readConfig(t, home)
	agents := cfg["agents"].(map[string]any)
	defaults := agents["defaults"].(map[string]any)
	workspace, _ := defaults["workspace"].(string)
	expected := filepath.Join(home, ".gopherclaw", "workspace")
	if workspace != expected {
		t.Errorf("expected workspace %q, got %q", expected, workspace)
	}
}
