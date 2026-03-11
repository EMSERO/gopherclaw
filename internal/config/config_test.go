package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseRoot(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data := []byte(`{
		"env": {"FOO": "bar"},
		"logging": {"level": "debug"},
		"agents": {
			"defaults": {
				"model": {"primary": "github-copilot/claude-sonnet-4.6", "fallbacks": ["github-copilot/gpt-4.1"]},
				"workspace": "/tmp/ws",
				"userTimezone": "America/New_York",
				"timeoutSeconds": 120,
				"maxConcurrent": 4,
				"contextPruning": {"hardClearRatio": 0.3, "keepLastAssistants": 3}
			},
			"list": [{"id": "main", "default": true, "identity": {"name": "TestBot", "theme": "test"}}]
		},
		"gateway": {"port": 9999, "bind": "loopback", "auth": {"token": "secret"}},
		"channels": {"telegram": {"enabled": false, "botToken": "tok:123"}}
	}`)
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Env["FOO"] != "bar" {
		t.Errorf("expected env FOO=bar, got %q", cfg.Env["FOO"])
	}
	if cfg.Agents.Defaults.Model.Primary != "github-copilot/claude-sonnet-4.6" {
		t.Errorf("unexpected primary model: %s", cfg.Agents.Defaults.Model.Primary)
	}
	if len(cfg.Agents.Defaults.Model.Fallbacks) != 1 {
		t.Errorf("expected 1 fallback, got %d", len(cfg.Agents.Defaults.Model.Fallbacks))
	}
	if cfg.Gateway.Port != 9999 {
		t.Errorf("expected port 9999, got %d", cfg.Gateway.Port)
	}
	if cfg.Agents.Defaults.ContextPruning.HardClearRatio != 0.3 {
		t.Errorf("expected hardClearRatio 0.3, got %f", cfg.Agents.Defaults.ContextPruning.HardClearRatio)
	}
	if cfg.Agents.Defaults.ContextPruning.KeepLastAssistants != 3 {
		t.Errorf("expected keepLastAssistants 3, got %d", cfg.Agents.Defaults.ContextPruning.KeepLastAssistants)
	}
	if cfg.Path != cfgPath {
		t.Errorf("expected Path=%s, got %s", cfgPath, cfg.Path)
	}
}

func TestApplyDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data := []byte(`{"agents": {"list": [{"id": "main", "default": true, "identity": {"name": "Bot"}}]}}`)
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Check defaults
	if cfg.Agents.Defaults.TimeoutSeconds != 300 {
		t.Errorf("expected default timeout 300, got %d", cfg.Agents.Defaults.TimeoutSeconds)
	}
	if cfg.Tools.Exec.TimeoutSec != 300 {
		t.Errorf("expected exec timeout 300, got %d", cfg.Tools.Exec.TimeoutSec)
	}
	if cfg.Tools.Web.Search.MaxResults != 5 {
		t.Errorf("expected search maxResults 5, got %d", cfg.Tools.Web.Search.MaxResults)
	}
	if cfg.Tools.Web.Fetch.MaxChars != 50000 {
		t.Errorf("expected fetch maxChars 50000, got %d", cfg.Tools.Web.Fetch.MaxChars)
	}
	if cfg.Gateway.Port != 18789 {
		t.Errorf("expected default port 18789, got %d", cfg.Gateway.Port)
	}
	if cfg.Agents.Defaults.UserTimezone != "UTC" {
		t.Errorf("expected default timezone UTC, got %s", cfg.Agents.Defaults.UserTimezone)
	}
	if cfg.Session.IdleMinutes != 120 {
		t.Errorf("expected default idle 120, got %d", cfg.Session.IdleMinutes)
	}
	if cfg.Session.MaxConcurrent != 2 {
		t.Errorf("expected default maxConcurrent 2, got %d", cfg.Session.MaxConcurrent)
	}
	if cfg.Agents.Defaults.ContextPruning.HardClearRatio != 0.5 {
		t.Errorf("expected default hardClearRatio 0.5, got %f", cfg.Agents.Defaults.ContextPruning.HardClearRatio)
	}
	if cfg.Agents.Defaults.ContextPruning.KeepLastAssistants != 2 {
		t.Errorf("expected default keepLastAssistants 2, got %d", cfg.Agents.Defaults.ContextPruning.KeepLastAssistants)
	}
	if cfg.Agents.Defaults.LoopDetectionN != 3 {
		t.Errorf("expected default loopDetectionN 3, got %d", cfg.Agents.Defaults.LoopDetectionN)
	}
	if cfg.Channels.Telegram.TimeoutSeconds != 300 {
		t.Errorf("expected default telegram timeout 300, got %d", cfg.Channels.Telegram.TimeoutSeconds)
	}
}

func TestDefaultAgent(t *testing.T) {
	// With explicit default
	cfg := &Root{
		Agents: Agents{
			List: []AgentDef{
				{ID: "a1", Identity: Identity{Name: "First"}},
				{ID: "a2", Default: true, Identity: Identity{Name: "Second"}},
			},
		},
	}
	if got := cfg.DefaultAgent(); got.ID != "a2" {
		t.Errorf("expected a2, got %s", got.ID)
	}

	// Without explicit default — returns first
	cfg.Agents.List[1].Default = false
	if got := cfg.DefaultAgent(); got.ID != "a1" {
		t.Errorf("expected a1, got %s", got.ID)
	}

	// Empty list — returns synthetic
	cfg.Agents.List = nil
	if got := cfg.DefaultAgent(); got.ID != "main" {
		t.Errorf("expected main, got %s", got.ID)
	}
}

func TestGatewayListenAddr(t *testing.T) {
	cfg := &Root{Gateway: Gateway{Port: 8080, Bind: "loopback"}}
	if addr := cfg.GatewayListenAddr(); addr != "127.0.0.1:8080" {
		t.Errorf("expected 127.0.0.1:8080, got %s", addr)
	}

	cfg.Gateway.Bind = "0.0.0.0"
	if addr := cfg.GatewayListenAddr(); addr != "0.0.0.0:8080" {
		t.Errorf("expected 0.0.0.0:8080, got %s", addr)
	}
}

func TestReload(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data := []byte(`{"gateway": {"port": 5555}}`)
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gateway.Port != 5555 {
		t.Fatalf("expected port 5555, got %d", cfg.Gateway.Port)
	}

	// Modify and reload
	data2 := []byte(`{"gateway": {"port": 6666}}`)
	if err := os.WriteFile(cfgPath, data2, 0600); err != nil {
		t.Fatal(err)
	}

	cfg2, err := cfg.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Gateway.Port != 6666 {
		t.Errorf("expected port 6666 after reload, got %d", cfg2.Gateway.Port)
	}
}

// ---------------------------------------------------------------------------
// EnsureAuth
// ---------------------------------------------------------------------------

func TestEnsureAuth_ModeNone(t *testing.T) {
	cfg := &Root{Gateway: Gateway{Auth: GatewayAuth{Mode: "none"}}}
	tok, gen, err := cfg.EnsureAuth()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "" || gen {
		t.Errorf("mode=none should return empty token and generated=false, got %q / %v", tok, gen)
	}
}

func TestEnsureAuth_ModeTrustedProxy(t *testing.T) {
	cfg := &Root{Gateway: Gateway{Auth: GatewayAuth{Mode: "trusted-proxy"}}}
	tok, gen, err := cfg.EnsureAuth()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "" || gen {
		t.Errorf("mode=trusted-proxy should return empty token and generated=false, got %q / %v", tok, gen)
	}
}

func TestEnsureAuth_ExistingToken(t *testing.T) {
	cfg := &Root{Gateway: Gateway{Auth: GatewayAuth{Token: "my-secret"}}}
	tok, gen, err := cfg.EnsureAuth()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "my-secret" {
		t.Errorf("expected existing token, got %q", tok)
	}
	if gen {
		t.Error("expected generated=false for existing token")
	}
}

func TestEnsureAuth_GeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"gateway":{}}`), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Clear any token that defaults might have set (there are none, but be safe).
	cfg.Gateway.Auth.Token = ""
	cfg.Gateway.Auth.Mode = ""

	tok, gen, err := cfg.EnsureAuth()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !gen {
		t.Error("expected generated=true")
	}
	if len(tok) != 64 {
		t.Errorf("expected 64-char hex token, got len=%d (%q)", len(tok), tok)
	}

	// Verify the token was persisted to disk.
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	gw, _ := raw["gateway"].(map[string]any)
	auth, _ := gw["auth"].(map[string]any)
	if auth["token"] != tok {
		t.Errorf("persisted token mismatch: got %v, want %s", auth["token"], tok)
	}
	if auth["mode"] != "token" {
		t.Errorf("persisted mode should be 'token', got %v", auth["mode"])
	}
}

func TestEnsureAuth_EmptyPath(t *testing.T) {
	// Root with no Path — persistToken should silently skip, no error.
	cfg := &Root{
		Gateway: Gateway{Auth: GatewayAuth{Mode: "", Token: ""}},
	}
	tok, gen, err := cfg.EnsureAuth()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !gen {
		t.Error("expected generated=true even with empty Path")
	}
	if len(tok) != 64 {
		t.Errorf("expected 64-char hex token, got len=%d", len(tok))
	}
}

// ---------------------------------------------------------------------------
// ResolveModelAlias
// ---------------------------------------------------------------------------

func TestResolveModelAlias_Match(t *testing.T) {
	cfg := &Root{
		Agents: Agents{
			Defaults: AgentDefaults{
				Models: map[string]ModelAliasEntry{
					"github-copilot/claude-sonnet-4.6": {Alias: "sonnet"},
					"github-copilot/gpt-4.1":           {Alias: "gpt"},
				},
			},
		},
	}
	got := cfg.ResolveModelAlias("sonnet")
	if got != "github-copilot/claude-sonnet-4.6" {
		t.Errorf("expected resolved model ID, got %s", got)
	}
	got = cfg.ResolveModelAlias("gpt")
	if got != "github-copilot/gpt-4.1" {
		t.Errorf("expected resolved model ID for gpt, got %s", got)
	}
}

func TestResolveModelAlias_NoMatch(t *testing.T) {
	cfg := &Root{
		Agents: Agents{
			Defaults: AgentDefaults{
				Models: map[string]ModelAliasEntry{
					"github-copilot/claude-sonnet-4.6": {Alias: "sonnet"},
				},
			},
		},
	}
	got := cfg.ResolveModelAlias("unknown-alias")
	if got != "unknown-alias" {
		t.Errorf("expected passthrough for unknown alias, got %s", got)
	}
	// Full model ID should also pass through.
	got = cfg.ResolveModelAlias("github-copilot/gpt-4.1")
	if got != "github-copilot/gpt-4.1" {
		t.Errorf("expected passthrough for full model ID, got %s", got)
	}
}

func TestResolveModelAlias_EmptyModels(t *testing.T) {
	cfg := &Root{}
	got := cfg.ResolveModelAlias("sonnet")
	if got != "sonnet" {
		t.Errorf("expected passthrough with nil models map, got %s", got)
	}
}

// ---------------------------------------------------------------------------
// BrowserConfig.IsHeadless
// ---------------------------------------------------------------------------

func TestBrowserConfigIsHeadless_NilDefault(t *testing.T) {
	bc := BrowserConfig{Headless: nil}
	if !bc.IsHeadless() {
		t.Error("nil Headless should default to true")
	}
}

func TestBrowserConfigIsHeadless_ExplicitTrue(t *testing.T) {
	v := true
	bc := BrowserConfig{Headless: &v}
	if !bc.IsHeadless() {
		t.Error("explicit true should return true")
	}
}

func TestBrowserConfigIsHeadless_ExplicitFalse(t *testing.T) {
	v := false
	bc := BrowserConfig{Headless: &v}
	if bc.IsHeadless() {
		t.Error("explicit false should return false")
	}
}

// ---------------------------------------------------------------------------
// Load — error cases
// ---------------------------------------------------------------------------

func TestLoad_EmptyPathUsesDefault(t *testing.T) {
	// Calling Load("") constructs ~/.gopherclaw/config.json, which almost
	// certainly doesn't exist in CI. We just verify it returns an error
	// containing the expected path fragment rather than panicking.
	_, err := Load("")
	if err == nil {
		// This is OK if the file happens to exist. But on most systems it won't.
		t.Skip("default config exists, cannot test missing-file path")
	}
	// The error should reference the constructed path.
	if got := err.Error(); !containsAny(got, "config.json", "gopherclaw") {
		t.Errorf("expected error to reference default path, got: %s", got)
	}
}

func TestLoad_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{not valid json`), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(cfgPath)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if got := err.Error(); !containsAny(got, "parse config") {
		t.Errorf("expected 'parse config' in error, got: %s", got)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load("/tmp/nonexistent-gopherclaw-test-config-xyz.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if got := err.Error(); !containsAny(got, "read config") {
		t.Errorf("expected 'read config' in error, got: %s", got)
	}
}

// containsAny returns true if s contains any of the substrings.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// applyDefaults — edge cases
// ---------------------------------------------------------------------------

func TestApplyDefaults_IdleMinutesBackwardCompat(t *testing.T) {
	// When session.idleMinutes is 0 but session.reset.idleMinutes is set,
	// the nested value should be promoted.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data := []byte(`{"session": {"reset": {"idleMinutes": 60}}}`)
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Session.IdleMinutes != 60 {
		t.Errorf("expected idleMinutes=60 from reset.idleMinutes, got %d", cfg.Session.IdleMinutes)
	}
}

func TestApplyDefaults_MaxConcurrentFromAgents(t *testing.T) {
	// When session.maxConcurrent is 0 but agents.defaults.maxConcurrent is
	// set, it should be inherited.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data := []byte(`{"agents": {"defaults": {"maxConcurrent": 8}}}`)
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Session.MaxConcurrent != 8 {
		t.Errorf("expected maxConcurrent=8 from agents.defaults, got %d", cfg.Session.MaxConcurrent)
	}
}

func TestApplyDefaults_SoftTrimRatioInheritance(t *testing.T) {
	// When softTrimRatio is in contextPruning (OpenClaw style), it should
	// be promoted to the top-level AgentDefaults.SoftTrimRatio.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data := []byte(`{"agents": {"defaults": {"contextPruning": {"softTrimRatio": 0.75}}}}`)
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agents.Defaults.SoftTrimRatio != 0.75 {
		t.Errorf("expected softTrimRatio=0.75 inherited from contextPruning, got %f", cfg.Agents.Defaults.SoftTrimRatio)
	}
}

func TestApplyDefaults_SoftTrimRatioTopLevelTakesPrecedence(t *testing.T) {
	// When both top-level and nested softTrimRatio are set, top-level wins
	// because applyDefaults only copies when top-level is 0.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data := []byte(`{"agents": {"defaults": {"softTrimRatio": 0.6, "contextPruning": {"softTrimRatio": 0.9}}}}`)
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agents.Defaults.SoftTrimRatio != 0.6 {
		t.Errorf("expected top-level softTrimRatio=0.6 to take precedence, got %f", cfg.Agents.Defaults.SoftTrimRatio)
	}
}

func TestApplyDefaults_BrowserHeadlessDefault(t *testing.T) {
	// When browser is enabled but headless is not explicitly set, it
	// defaults to true.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data := []byte(`{"tools": {"browser": {"enabled": true}}}`)
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tools.Browser.Headless == nil {
		t.Fatal("expected Headless to be set when browser is enabled")
	}
	if !*cfg.Tools.Browser.Headless {
		t.Error("expected Headless=true by default when browser is enabled")
	}
}

func TestApplyDefaults_BrowserDisabledHeadlessUntouched(t *testing.T) {
	// When browser is NOT enabled, Headless should remain nil (not set).
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data := []byte(`{"tools": {"browser": {"enabled": false}}}`)
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tools.Browser.Headless != nil {
		t.Error("expected Headless to remain nil when browser is disabled")
	}
}

func TestApplyDefaults_SandboxImageDefault(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data := []byte(`{"agents": {"defaults": {"sandbox": {"enabled": true}}}}`)
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agents.Defaults.Sandbox.Image != "ubuntu:22.04" {
		t.Errorf("expected default sandbox image 'ubuntu:22.04', got %q", cfg.Agents.Defaults.Sandbox.Image)
	}
}

func TestApplyDefaults_SandboxImageCustom(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data := []byte(`{"agents": {"defaults": {"sandbox": {"enabled": true, "image": "alpine:3.18"}}}}`)
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agents.Defaults.Sandbox.Image != "alpine:3.18" {
		t.Errorf("expected custom sandbox image 'alpine:3.18', got %q", cfg.Agents.Defaults.Sandbox.Image)
	}
}

func TestApplyDefaults_SandboxDisabledImageUntouched(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data := []byte(`{"agents": {"defaults": {"sandbox": {"enabled": false}}}}`)
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agents.Defaults.Sandbox.Image != "" {
		t.Errorf("expected empty sandbox image when disabled, got %q", cfg.Agents.Defaults.Sandbox.Image)
	}
}

func TestApplyDefaults_DailyModeAtHour(t *testing.T) {
	// When reset.mode is "daily" and atHour is nil, it defaults to 4.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data := []byte(`{"session": {"reset": {"mode": "daily"}}}`)
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Session.Reset.AtHour == nil {
		t.Fatal("expected atHour to be set for daily mode")
	}
	if *cfg.Session.Reset.AtHour != 4 {
		t.Errorf("expected atHour=4, got %d", *cfg.Session.Reset.AtHour)
	}
}

func TestApplyDefaults_DailyModeAtHourExplicit(t *testing.T) {
	// When reset.mode is "daily" and atHour is explicitly set, it is preserved.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data := []byte(`{"session": {"reset": {"mode": "daily", "atHour": 7}}}`)
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Session.Reset.AtHour == nil {
		t.Fatal("expected atHour to be set")
	}
	if *cfg.Session.Reset.AtHour != 7 {
		t.Errorf("expected atHour=7 (explicit), got %d", *cfg.Session.Reset.AtHour)
	}
}

func TestApplyDefaults_NonDailyModeAtHourNil(t *testing.T) {
	// When mode is not "daily", atHour should remain nil.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data := []byte(`{"session": {"reset": {"mode": "idle"}}}`)
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Session.Reset.AtHour != nil {
		t.Errorf("expected atHour=nil for non-daily mode, got %d", *cfg.Session.Reset.AtHour)
	}
}

func TestApplyDefaults_ChannelTimeouts(t *testing.T) {
	// All channel timeouts should default to 300 when unset.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data := []byte(`{}`)
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Channels.Discord.TimeoutSeconds != 300 {
		t.Errorf("expected discord timeout=300, got %d", cfg.Channels.Discord.TimeoutSeconds)
	}
	if cfg.Channels.Slack.TimeoutSeconds != 300 {
		t.Errorf("expected slack timeout=300, got %d", cfg.Channels.Slack.TimeoutSeconds)
	}
}

func TestApplyDefaults_WebTimeouts(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data := []byte(`{}`)
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tools.Web.Search.TimeoutSeconds != 30 {
		t.Errorf("expected search timeout=30, got %d", cfg.Tools.Web.Search.TimeoutSeconds)
	}
	if cfg.Tools.Web.Fetch.TimeoutSeconds != 30 {
		t.Errorf("expected fetch timeout=30, got %d", cfg.Tools.Web.Fetch.TimeoutSeconds)
	}
}

// ---------------------------------------------------------------------------
// GatewayListenAddr — additional bind values
// ---------------------------------------------------------------------------

func TestGatewayListenAddr_EmptyBind(t *testing.T) {
	// Empty bind should behave like loopback (127.0.0.1).
	cfg := &Root{Gateway: Gateway{Port: 9000, Bind: ""}}
	// Per the implementation: empty string falls through to 0.0.0.0 because
	// the condition is `Bind != "loopback" && Bind != ""`.
	// Wait — let me check: if Bind == "" the condition `Bind != "loopback"` is
	// true and `Bind != ""` is false, so the whole AND is false → host stays
	// 127.0.0.1. Good.
	if addr := cfg.GatewayListenAddr(); addr != "127.0.0.1:9000" {
		t.Errorf("expected 127.0.0.1:9000 for empty bind, got %s", addr)
	}
}

// ---------------------------------------------------------------------------
// persistToken — edge cases via EnsureAuth
// ---------------------------------------------------------------------------

func TestEnsureAuth_PersistPreservesExistingFields(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	// Write a config with some existing fields.
	initial := []byte(`{"env":{"KEY":"val"},"gateway":{"port":4444}}`)
	if err := os.WriteFile(cfgPath, initial, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.Gateway.Auth.Token = ""
	cfg.Gateway.Auth.Mode = ""

	tok, _, err := cfg.EnsureAuth()
	if err != nil {
		t.Fatalf("EnsureAuth: %v", err)
	}

	// Re-read the file and verify existing fields survived.
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	env, _ := raw["env"].(map[string]any)
	if env["KEY"] != "val" {
		t.Errorf("expected env.KEY=val preserved, got %v", env["KEY"])
	}
	gw, _ := raw["gateway"].(map[string]any)
	// port might be float64 from JSON
	if p, ok := gw["port"].(float64); !ok || int(p) != 4444 {
		t.Errorf("expected gateway.port=4444 preserved, got %v", gw["port"])
	}
	auth, _ := gw["auth"].(map[string]any)
	if auth["token"] != tok {
		t.Errorf("expected persisted token=%s, got %v", tok, auth["token"])
	}
}

// ---------------------------------------------------------------------------
// UpdateConfig
// ---------------------------------------------------------------------------

func TestUpdateConfigParsing(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data := []byte(`{
		"agents": {"list": [{"id": "main", "default": true, "identity": {"name": "Bot"}}]},
		"update": {"autoUpdate": true}
	}`)
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !cfg.Update.AutoUpdate {
		t.Error("expected autoUpdate=true, got false")
	}
}

func TestUpdateConfigDefaultsFalse(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	data := []byte(`{
		"agents": {"list": [{"id": "main", "default": true, "identity": {"name": "Bot"}}]}
	}`)
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Update.AutoUpdate {
		t.Error("expected autoUpdate to default to false")
	}
}

func TestUpdateConfigRoundTrip(t *testing.T) {
	uc := UpdateConfig{AutoUpdate: true}
	data, err := json.Marshal(uc)
	if err != nil {
		t.Fatal(err)
	}
	var uc2 UpdateConfig
	if err := json.Unmarshal(data, &uc2); err != nil {
		t.Fatal(err)
	}
	if !uc2.AutoUpdate {
		t.Error("expected AutoUpdate=true after round trip")
	}
}

func TestPersistTokenUnreadableFile(t *testing.T) {
	// persistToken returns error when Path points to a nonexistent file
	cfg := &Root{Path: "/nonexistent/config.json"}
	err := cfg.persistToken("test-token")
	if err == nil {
		t.Error("expected error for nonexistent path")
	}
}

func TestPersistTokenBadJSON(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	// Write invalid JSON
	if err := os.WriteFile(cfgPath, []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &Root{Path: cfgPath}
	err := cfg.persistToken("test-token")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestEnsureAuthReadError(t *testing.T) {
	// EnsureAuth with rand.Read error path can't be triggered,
	// but we can test the persist error path.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Gateway.Auth.Token = ""
	cfg.Gateway.Auth.Mode = ""
	// Remove the file after loading to trigger persist error
	os.Remove(cfgPath)
	tok, generated, err := cfg.EnsureAuth()
	// Token should be generated in memory even if persist fails
	if tok == "" {
		t.Error("expected non-empty token")
	}
	if !generated {
		t.Error("expected generated=true")
	}
	if err == nil {
		t.Error("expected persist error")
	}
}
