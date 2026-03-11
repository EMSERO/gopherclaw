package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/EMSERO/gopherclaw/internal/config"
)

func TestAudit_Clean(t *testing.T) {
	cfg := cleanConfig(t)
	r := RunAudit(cfg, AuditOpts{})
	for _, f := range r.Findings {
		if f.Severity == SeverityCritical {
			t.Errorf("unexpected critical finding: %s - %s", f.CheckID, f.Title)
		}
	}
}

func TestAudit_PublicGateway_NoAuth(t *testing.T) {
	cfg := cleanConfig(t)
	cfg.Gateway.Bind = "0.0.0.0"
	cfg.Gateway.Auth.Mode = "none"
	r := RunAudit(cfg, AuditOpts{})
	found := findFinding(r, "GW-001")
	if found == nil {
		t.Fatal("expected GW-001 critical finding for public gateway without auth")
	}
	if found.Severity != SeverityCritical {
		t.Errorf("GW-001 severity = %s, want critical", found.Severity)
	}
}

func TestAudit_LoopbackNoAuth_Info(t *testing.T) {
	cfg := cleanConfig(t)
	cfg.Gateway.Bind = "loopback"
	cfg.Gateway.Auth.Mode = "none"
	r := RunAudit(cfg, AuditOpts{})
	found := findFinding(r, "GW-002")
	if found == nil {
		t.Fatal("expected GW-002 info finding for loopback without auth")
	}
	if found.Severity != SeverityInfo {
		t.Errorf("GW-002 severity = %s, want info", found.Severity)
	}
}

func TestAudit_NoRateLimit(t *testing.T) {
	cfg := cleanConfig(t)
	cfg.Gateway.RateLimit.RPS = 0
	r := RunAudit(cfg, AuditOpts{})
	found := findFinding(r, "GW-003")
	if found == nil {
		t.Fatal("expected GW-003 warning for missing rate limit")
	}
	if found.Severity != SeverityWarn {
		t.Errorf("GW-003 severity = %s, want warn", found.Severity)
	}
}

func TestAudit_ShortAuthToken(t *testing.T) {
	cfg := cleanConfig(t)
	cfg.Gateway.Auth.Token = "abc123"
	r := RunAudit(cfg, AuditOpts{})
	found := findFinding(r, "GW-004")
	if found == nil {
		t.Fatal("expected GW-004 warning for short token")
	}
}

func TestAudit_WorldReadableConfig(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgFile, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := cleanConfig(t)
	cfg.Path = cfgFile
	r := RunAudit(cfg, AuditOpts{})
	found := findFinding(r, "FS-001")
	if found == nil {
		t.Fatal("expected FS-001 critical finding for world-readable config")
	}
	if found.Severity != SeverityCritical {
		t.Errorf("FS-001 severity = %s, want critical", found.Severity)
	}
}

func TestAudit_GroupWritableConfig(t *testing.T) {
	// Root ignores file permission bits, so the FS-002 check won't trigger.
	if os.Getuid() == 0 {
		t.Skip("skipping: running as root (permission bits not enforced)")
	}
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgFile, []byte("{}"), 0660); err != nil {
		t.Fatal(err)
	}
	// Verify the file actually has group-write bit set (some filesystems strip it).
	info, err := os.Stat(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0020 == 0 {
		t.Skip("skipping: filesystem did not preserve group-write bit")
	}
	cfg := cleanConfig(t)
	cfg.Path = cfgFile
	r := RunAudit(cfg, AuditOpts{})
	found := findFinding(r, "FS-002")
	if found == nil {
		t.Fatal("expected FS-002 warning for group-writable config")
	}
}

func TestAudit_NoDenyCommands(t *testing.T) {
	cfg := cleanConfig(t)
	cfg.Tools.Exec.DenyCommands = nil
	r := RunAudit(cfg, AuditOpts{})
	found := findFinding(r, "EXEC-002")
	if found == nil {
		t.Fatal("expected EXEC-002 warning for empty deny list")
	}
}

func TestAudit_NoDefaultModel(t *testing.T) {
	cfg := cleanConfig(t)
	cfg.Agents.Defaults.Model.Primary = ""
	r := RunAudit(cfg, AuditOpts{})
	found := findFinding(r, "MODEL-001")
	if found == nil {
		t.Fatal("expected MODEL-001 warning for missing default model")
	}
}

func TestAudit_SmallContextWindow(t *testing.T) {
	cfg := cleanConfig(t)
	cfg.Agents.Defaults.ContextPruning.ModelMaxTokens = 8000
	r := RunAudit(cfg, AuditOpts{})
	found := findFinding(r, "MODEL-003")
	if found == nil {
		t.Fatal("expected MODEL-003 warning for small context window")
	}
}

func TestAudit_ProviderMissingKey(t *testing.T) {
	cfg := cleanConfig(t)
	cfg.Providers = map[string]*config.ProviderConfig{
		"openai": {APIKey: ""},
	}
	cfg.Agents.Defaults.Model.Primary = "openai/gpt-4"
	r := RunAudit(cfg, AuditOpts{})
	found := findFinding(r, "MODEL-004")
	if found == nil {
		t.Fatal("expected MODEL-004 warning for missing provider key")
	}
	if !strings.Contains(found.Detail, "openai") {
		t.Errorf("expected detail to mention openai, got: %s", found.Detail)
	}
}

func TestAudit_Summary(t *testing.T) {
	cfg := cleanConfig(t)
	cfg.Gateway.Bind = "0.0.0.0"
	cfg.Gateway.Auth.Mode = "none"
	cfg.Agents.Defaults.Model.Primary = ""
	r := RunAudit(cfg, AuditOpts{})
	if r.Summary.Critical < 1 {
		t.Errorf("expected at least 1 critical, got %d", r.Summary.Critical)
	}
	if r.Summary.Warn < 1 {
		t.Errorf("expected at least 1 warning, got %d", r.Summary.Warn)
	}
}

func TestFormatReport_Empty(t *testing.T) {
	r := Report{}
	out := FormatReport(r)
	if !strings.Contains(out, "No findings") {
		t.Errorf("expected 'No findings' in empty report, got: %s", out)
	}
}

func TestFormatReport_WithFindings(t *testing.T) {
	r := Report{
		Summary: Summary{Critical: 1},
		Findings: []Finding{{
			CheckID:     "TEST-001",
			Severity:    SeverityCritical,
			Title:       "Test finding",
			Detail:      "detail",
			Remediation: "fix it",
		}},
	}
	out := FormatReport(r)
	if !strings.Contains(out, "TEST-001") {
		t.Errorf("expected TEST-001 in output, got: %s", out)
	}
	if !strings.Contains(out, "1 critical") {
		t.Errorf("expected '1 critical' in summary, got: %s", out)
	}
}

func cleanConfig(t *testing.T) *config.Root {
	t.Helper()
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgFile, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	return &config.Root{
		Path: cfgFile,
		Providers: map[string]*config.ProviderConfig{
			"openai": {APIKey: "sk-test-key-long-enough-to-not-warn"},
		},
		Agents: config.Agents{
			Defaults: config.AgentDefaults{
				Model: config.ModelConfig{
					Primary:   "openai/gpt-4",
					Fallbacks: []string{"openai/gpt-3.5-turbo"},
				},
				Sandbox: config.SandboxConfig{Enabled: true},
				ContextPruning: config.ContextPruning{
					ModelMaxTokens: 128000,
				},
			},
		},
		Tools: config.Tools{
			Exec: config.ExecConfig{
				DenyCommands: []string{"rm -rf /"},
			},
		},
		Gateway: config.Gateway{
			Port: 18789,
			Bind: "loopback",
			Auth: config.GatewayAuth{
				Mode:  "token",
				Token: "a-very-long-token-that-is-at-least-32-chars-long",
			},
			RateLimit: config.RateLimitConfig{
				RPS:   10,
				Burst: 20,
			},
		},
	}
}

func findFinding(r Report, checkID string) *Finding {
	for i := range r.Findings {
		if r.Findings[i].CheckID == checkID {
			return &r.Findings[i]
		}
	}
	return nil
}
