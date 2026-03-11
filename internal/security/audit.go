// Package security implements the security audit system (REQ-440).
//
// The audit checks configuration, file permissions, exec safety, and model
// hygiene, producing severity-graded findings with remediation text.
// Run via `gopherclaw security audit` CLI subcommand.
package security

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/EMSERO/gopherclaw/internal/config"
)

// Severity levels for audit findings.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarn     Severity = "warn"
	SeverityCritical Severity = "critical"
)

// Finding is a single audit check result.
type Finding struct {
	CheckID     string   `json:"checkId"`
	Severity    Severity `json:"severity"`
	Title       string   `json:"title"`
	Detail      string   `json:"detail"`
	Remediation string   `json:"remediation"`
}

// Report is the output of a security audit.
type Report struct {
	Timestamp string    `json:"timestamp"`
	Summary   Summary   `json:"summary"`
	Findings  []Finding `json:"findings"`
}

// Summary counts findings by severity.
type Summary struct {
	Critical int `json:"critical"`
	Warn     int `json:"warn"`
	Info     int `json:"info"`
}

// AuditOpts controls audit behavior.
type AuditOpts struct {
	Deep bool // if true, probe the running gateway
}

// RunAudit performs all security checks and returns a report.
func RunAudit(cfg *config.Root, opts AuditOpts) Report {
	var findings []Finding

	findings = append(findings, collectGatewayFindings(cfg)...)
	findings = append(findings, collectFilesystemFindings(cfg)...)
	findings = append(findings, collectExecFindings(cfg)...)
	findings = append(findings, collectModelFindings(cfg)...)

	// Build summary
	var sum Summary
	for _, f := range findings {
		switch f.Severity {
		case SeverityCritical:
			sum.Critical++
		case SeverityWarn:
			sum.Warn++
		case SeverityInfo:
			sum.Info++
		}
	}

	return Report{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Summary:   sum,
		Findings:  findings,
	}
}

// collectGatewayFindings checks gateway bind, auth, and rate limit configuration.
func collectGatewayFindings(cfg *config.Root) []Finding {
	var findings []Finding

	bind := cfg.Gateway.Bind
	authMode := cfg.Gateway.Auth.Mode

	isPublic := bind != "loopback" && bind != ""
	noAuth := authMode == "none" || authMode == "trusted-proxy"

	if isPublic && noAuth {
		findings = append(findings, Finding{
			CheckID:     "GW-001",
			Severity:    SeverityCritical,
			Title:       "Public gateway without authentication",
			Detail:      fmt.Sprintf("Gateway binds to %s with auth.mode=%q — API is unauthenticated on a public address.", cfg.GatewayListenAddr(), authMode),
			Remediation: "Set gateway.auth.mode to \"token\" or restrict gateway.bind to \"loopback\".",
		})
	} else if noAuth {
		findings = append(findings, Finding{
			CheckID:     "GW-002",
			Severity:    SeverityInfo,
			Title:       "Authentication disabled (loopback only)",
			Detail:      fmt.Sprintf("Gateway auth.mode=%q on loopback bind — acceptable for local development.", authMode),
			Remediation: "Enable token auth if the gateway may be exposed to a network.",
		})
	}

	if cfg.Gateway.RateLimit.RPS == 0 {
		findings = append(findings, Finding{
			CheckID:     "GW-003",
			Severity:    SeverityWarn,
			Title:       "No rate limiting configured",
			Detail:      "gateway.rateLimit.rps is 0 — the API accepts unlimited requests per IP.",
			Remediation: "Set gateway.rateLimit.rps to a reasonable value (e.g., 10).",
		})
	}

	if cfg.Gateway.Auth.Token != "" && len(cfg.Gateway.Auth.Token) < 32 {
		findings = append(findings, Finding{
			CheckID:     "GW-004",
			Severity:    SeverityWarn,
			Title:       "Short auth token",
			Detail:      fmt.Sprintf("Auth token is only %d characters — should be at least 32.", len(cfg.Gateway.Auth.Token)),
			Remediation: "Regenerate a longer token or delete the token to let GopherClaw auto-generate one.",
		})
	}

	return findings
}

// collectFilesystemFindings checks config and state directory permissions.
func collectFilesystemFindings(cfg *config.Root) []Finding {
	var findings []Finding

	if cfg.Path != "" {
		info, err := os.Stat(cfg.Path)
		if err == nil {
			perm := info.Mode().Perm()

			if perm&0004 != 0 {
				findings = append(findings, Finding{
					CheckID:     "FS-001",
					Severity:    SeverityCritical,
					Title:       "Config file is world-readable",
					Detail:      fmt.Sprintf("%s has permissions %o — secrets (API keys, tokens) are exposed to all users.", cfg.Path, perm),
					Remediation: fmt.Sprintf("chmod 600 %s", cfg.Path),
				})
			}

			if perm&0020 != 0 {
				findings = append(findings, Finding{
					CheckID:     "FS-002",
					Severity:    SeverityWarn,
					Title:       "Config file is group-writable",
					Detail:      fmt.Sprintf("%s has permissions %o — group members can modify the config.", cfg.Path, perm),
					Remediation: fmt.Sprintf("chmod 600 %s", cfg.Path),
				})
			}
		}
	}

	stateDir := filepath.Dir(cfg.Path)
	if stateDir != "" {
		info, err := os.Stat(stateDir)
		if err == nil {
			perm := info.Mode().Perm()
			if perm&0002 != 0 {
				findings = append(findings, Finding{
					CheckID:     "FS-003",
					Severity:    SeverityCritical,
					Title:       "State directory is world-writable",
					Detail:      fmt.Sprintf("%s has permissions %o — any user can tamper with session data.", stateDir, perm),
					Remediation: fmt.Sprintf("chmod 700 %s", stateDir),
				})
			}
		}
	}

	return findings
}

// collectExecFindings checks exec tool configuration safety.
func collectExecFindings(cfg *config.Root) []Finding {
	var findings []Finding

	if !cfg.Agents.Defaults.Sandbox.Enabled {
		findings = append(findings, Finding{
			CheckID:     "EXEC-001",
			Severity:    SeverityInfo,
			Title:       "Docker sandbox is disabled",
			Detail:      "Exec tool runs commands directly on the host — no container isolation.",
			Remediation: "Enable sandbox mode (agents.defaults.sandbox.enabled: true) for production use.",
		})
	}

	if len(cfg.Tools.Exec.DenyCommands) == 0 {
		findings = append(findings, Finding{
			CheckID:     "EXEC-002",
			Severity:    SeverityWarn,
			Title:       "No exec deny list configured",
			Detail:      "tools.exec.denyCommands is empty — the agent can run any command.",
			Remediation: "Add dangerous commands to denyCommands (e.g., \"rm -rf /\", \"shutdown\", \"reboot\").",
		})
	}

	return findings
}

// collectModelFindings checks model configuration hygiene.
func collectModelFindings(cfg *config.Root) []Finding {
	var findings []Finding

	if cfg.Agents.Defaults.Model.Primary == "" {
		findings = append(findings, Finding{
			CheckID:     "MODEL-001",
			Severity:    SeverityWarn,
			Title:       "No default model configured",
			Detail:      "agents.defaults.model.primary is empty — the agent may fail on first request.",
			Remediation: "Set agents.defaults.model.primary to a valid model ID.",
		})
	}

	if len(cfg.Agents.Defaults.Model.Fallbacks) == 0 {
		findings = append(findings, Finding{
			CheckID:     "MODEL-002",
			Severity:    SeverityInfo,
			Title:       "No fallback models configured",
			Detail:      "agents.defaults.model.fallbacks is empty — if the primary model fails, there is no backup.",
			Remediation: "Add one or more fallback models for resilience.",
		})
	}

	maxTokens := cfg.Agents.Defaults.ContextPruning.ModelMaxTokens
	if maxTokens > 0 && maxTokens < 16000 {
		findings = append(findings, Finding{
			CheckID:     "MODEL-003",
			Severity:    SeverityWarn,
			Title:       "Very small model context window",
			Detail:      fmt.Sprintf("modelMaxTokens=%d — below the recommended minimum of 16,000.", maxTokens),
			Remediation: "Set contextPruning.modelMaxTokens to at least 16000.",
		})
	}

	for name, prov := range cfg.Providers {
		if prov.APIKey == "" {
			primary := cfg.Agents.Defaults.Model.Primary
			if strings.HasPrefix(primary, name+"/") {
				findings = append(findings, Finding{
					CheckID:     "MODEL-004",
					Severity:    SeverityWarn,
					Title:       fmt.Sprintf("Provider %q has no API key", name),
					Detail:      fmt.Sprintf("Provider %q is used by the primary model but has no apiKey configured.", name),
					Remediation: fmt.Sprintf("Set providers.%s.apiKey in the config.", name),
				})
			}
		}
	}

	return findings
}

// FormatReport formats a report for terminal output with severity colors.
func FormatReport(r Report) string {
	var sb strings.Builder

	sb.WriteString("=== GopherClaw Security Audit ===\n\n")

	if len(r.Findings) == 0 {
		sb.WriteString("No findings. Configuration looks good!\n")
		return sb.String()
	}

	fmt.Fprintf(&sb, "Summary: %d critical, %d warnings, %d info\n\n",
		r.Summary.Critical, r.Summary.Warn, r.Summary.Info)

	for _, sev := range []Severity{SeverityCritical, SeverityWarn, SeverityInfo} {
		for _, f := range r.Findings {
			if f.Severity != sev {
				continue
			}
			icon := severityIcon(f.Severity)
			fmt.Fprintf(&sb, "%s [%s] %s\n", icon, f.CheckID, f.Title)
			fmt.Fprintf(&sb, "   %s\n", f.Detail)
			fmt.Fprintf(&sb, "   -> %s\n\n", f.Remediation)
		}
	}

	return sb.String()
}

func severityIcon(s Severity) string {
	switch s {
	case SeverityCritical:
		return "\033[31m[CRITICAL]\033[0m"
	case SeverityWarn:
		return "\033[33m[WARNING]\033[0m"
	case SeverityInfo:
		return "\033[36m[INFO]\033[0m"
	default:
		return "[?]"
	}
}
