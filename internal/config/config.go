package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Root is the top-level configuration structure.
type Root struct {
	Env       map[string]string          `json:"env"`
	Providers map[string]*ProviderConfig `json:"providers"`
	Logging   Logging                    `json:"logging"`
	Agents    Agents                     `json:"agents"`
	Tools     Tools                      `json:"tools"`
	Session   Session                    `json:"session"`
	Channels  Channels                   `json:"channels"`
	Gateway   Gateway                    `json:"gateway"`
	Messages  Messages                   `json:"messages"`
	TaskQueue TaskQueueConfig            `json:"taskQueue"`
	Update    UpdateConfig               `json:"update"`
	Eidetic   EideticConfig              `json:"eidetic"`

	// Path is the filesystem path this config was loaded from (not serialized).
	Path string `json:"-"`
}

// EideticConfig controls the optional Eidetic memory sidecar integration.
// When Enabled is false (the default) the integration is fully disabled and
// GopherClaw behaves as if this block were absent.
type EideticConfig struct {
	Enabled         bool    `json:"enabled"`
	BaseURL         string  `json:"baseURL"`              // default "http://localhost:7700"
	APIKey          string  `json:"apiKey"`               // Bearer token
	AgentID         string  `json:"agentID"`              // override agent_id; defaults to agent def ID
	RecentLimit     int     `json:"recentLimit"`          // entries injected into system prompt (default 20)
	SearchLimit     int     `json:"searchLimit"`          // max results for eidetic_search tool (default 10)
	SearchThreshold float64 `json:"searchThreshold"`      // cosine similarity threshold (default 0.5)
	TimeoutSeconds  int     `json:"timeoutSeconds"`       // per-request timeout (default 5)
	RecallEnabled   *bool   `json:"recallEnabled"`        // semantic recall per turn (default true when eidetic enabled)
	RecallLimit     int     `json:"recallLimit"`          // max recalled entries per turn (default 5)
	RecallThreshold float64 `json:"recallThreshold"`      // min relevance for recalled entries (default 0.4)
	RecallTimeoutS  int     `json:"recallTimeoutSeconds"` // per-recall timeout; 0 = use timeoutSeconds (default 5)

	// Embeddings enables client-side vector embedding generation.
	// When configured, GopherClaw generates embeddings before storing memories
	// and uses hybrid (keyword + vector) search for retrieval.
	Embeddings EmbeddingsConfig `json:"embeddings"`
}

// EmbeddingsConfig controls client-side vector embedding generation for
// hybrid (keyword + vector) memory search.
type EmbeddingsConfig struct {
	Enabled    bool   `json:"enabled"`              // generate embeddings on store, use hybrid search
	Provider   string `json:"provider"`             // provider name for base URL lookup (e.g. "ollama", "openai")
	Model      string `json:"model"`                // embedding model ID (e.g. "nomic-embed-text", "text-embedding-3-small")
	BaseURL    string `json:"baseURL"`              // override provider default endpoint
	APIKey     string `json:"apiKey"`               // API key (empty = "no-key" for local providers)
	Dimensions int    `json:"dimensions,omitempty"` // optional truncated dimensions (0 = model default)
}

// ProviderConfig holds connection settings for a model provider.
type ProviderConfig struct {
	APIKey  string `json:"apiKey"`
	BaseURL string `json:"baseURL"` // empty = use provider default
}

type Messages struct {
	Queue            MessageQueue `json:"queue"`
	AckReactionScope string       `json:"ackReactionScope"` // "group-mentions", "all", ""
	Usage            string       `json:"usage"`            // "off" (default), "tokens"
	StreamEditMs     int          `json:"streamEditMs"`     // streaming message edit interval in ms (default 400)
}

type MessageQueue struct {
	Mode       string `json:"mode"`       // "collect" or ""
	DebounceMs int    `json:"debounceMs"` // e.g. 300
	Cap        int    `json:"cap"`
}

type Logging struct {
	Level           string `json:"level"`
	File            string `json:"file"`
	ConsoleLevel    string `json:"consoleLevel"`
	RedactSensitive string `json:"redactSensitive"`
}

type Agents struct {
	Defaults AgentDefaults `json:"defaults"`
	List     []AgentDef    `json:"list"`
}

type AgentDefaults struct {
	Engine            string                     `json:"engine"`    // "router" (default) or "claude-cli"
	CLIEngine         CLIEngineConfig            `json:"cliEngine"` // config when engine="claude-cli"
	Model             ModelConfig                `json:"model"`
	Models            map[string]ModelAliasEntry `json:"models"` // model-id → {alias}; used for reverse alias lookup
	Subagents         SubagentsConfig            `json:"subagents"`
	Workspace         string                     `json:"workspace"`
	UserTimezone      string                     `json:"userTimezone"`
	TimeoutSeconds    int                        `json:"timeoutSeconds"`
	MaxConcurrent     int                        `json:"maxConcurrent"`
	ContextPruning    ContextPruning             `json:"contextPruning"`
	LoopDetectionN    int                        `json:"loopDetectionN"`    // consecutive identical tool calls to trigger break (default 3)
	ToolLoopDetection ToolLoopDetectionConfig    `json:"toolLoopDetection"` // multi-detector loop detection (REQ-410)
	MaxIterations     int                        `json:"maxIterations"`     // max tool-call rounds per turn (default 200)
	SoftTrimRatio     float64                    `json:"softTrimRatio"`     // fraction of model max tokens to trigger soft trim (default 0.0 = disabled)
	Thinking          ThinkingConfig             `json:"thinking"`          // extended thinking (Anthropic models)
	Memory            MemoryConfig               `json:"memory"`            // persistent memory files
	Sandbox           SandboxConfig              `json:"sandbox"`           // Docker exec sandbox
	Heartbeat         HeartbeatConfig            `json:"heartbeat"`         // parsed for config compat; not implemented
}

// SubagentsConfig controls default model and concurrency for subagent calls.
type SubagentsConfig struct {
	Model         string `json:"model"`         // default model for subagent calls (empty = inherit primary)
	MaxConcurrent int    `json:"maxConcurrent"` // max concurrent subagent tasks (default 4)
}

// ToolLoopDetectionConfig controls the multi-detector tool loop detection system (REQ-410).
type ToolLoopDetectionConfig struct {
	Enabled                       bool `json:"enabled"`                       // enable multi-detector loop detection (default true)
	HistorySize                   int  `json:"historySize"`                   // sliding window size (default 30)
	WarningThreshold              int  `json:"warningThreshold"`              // repetitions to trigger warning (default 10)
	CriticalThreshold             int  `json:"criticalThreshold"`             // repetitions to trigger hard break (default 20)
	GlobalCircuitBreakerThreshold int  `json:"globalCircuitBreakerThreshold"` // any single hash repeated this many times = stop (default 30)
}

// ModelAliasEntry maps a model ID to a short alias (e.g. "sonnet").
type ModelAliasEntry struct {
	Alias string `json:"alias"`
}

// ResolveModelAlias resolves a short alias (e.g. "sonnet") to its full model ID
// (e.g. "github-copilot/claude-sonnet-4.6"). If no matching alias is found, the
// input is returned unchanged (allowing full model IDs to pass through directly).
func (c *Root) ResolveModelAlias(nameOrAlias string) string {
	for modelID, entry := range c.Agents.Defaults.Models {
		if entry.Alias == nameOrAlias {
			return modelID
		}
	}
	return nameOrAlias
}

// MemoryConfig controls the persistent memory system.
type MemoryConfig struct {
	Enabled bool `json:"enabled"` // load MEMORY.md into system prompt, enable memory tools
}

// SandboxConfig controls Docker-based exec sandbox for the exec tool.
type SandboxConfig struct {
	Enabled      bool     `json:"enabled"`
	Image        string   `json:"image"`        // Docker image, e.g. "ubuntu:22.04"
	Mounts       []string `json:"mounts"`       // "host:container:mode" bind mounts
	SetupCommand string   `json:"setupCommand"` // run once after container creation
}

// ThinkingConfig controls extended thinking for providers that support it.
type ThinkingConfig struct {
	Enabled      bool   `json:"enabled"`
	BudgetTokens int    `json:"budgetTokens"` // tokens reserved for thinking (default 8192)
	Level        string `json:"level"`        // "off", "enabled", "adaptive"; empty = use Enabled field; Claude 4.6 defaults to "adaptive"
}

// HeartbeatConfig controls periodic heartbeat agent turns.
type HeartbeatConfig struct {
	Every        string             `json:"every"`        // interval duration string, e.g. "30m", "1h" (default "30m"; empty or "0" = disabled)
	ActiveHours  *ActiveHoursConfig `json:"activeHours"`  // optional time-of-day window
	Target       string             `json:"target"`       // delivery target: "last", "none", channel name (default "none")
	Model        string             `json:"model"`        // optional model override for heartbeat turns
	Prompt       string             `json:"prompt"`       // custom prompt (default: read HEARTBEAT.md)
	AckMaxChars  int                `json:"ackMaxChars"`  // max trailing chars after HEARTBEAT_OK before forcing delivery (default 300)
	LightContext bool               `json:"lightContext"` // minimal bootstrap context (identity + HEARTBEAT.md only)
	DirectPolicy string             `json:"directPolicy"` // "allow" (default) or "block"
}

// ActiveHoursConfig restricts heartbeat execution to a time-of-day window.
type ActiveHoursConfig struct {
	Start    string `json:"start"`    // HH:MM (24h), inclusive (e.g. "09:00")
	End      string `json:"end"`      // HH:MM (24h), exclusive (e.g. "22:00"); "24:00" allowed
	Timezone string `json:"timezone"` // IANA timezone or "local" (default: agents.defaults.userTimezone)
}

// HeartbeatEnabled returns true if heartbeat is configured with a non-zero interval.
func (hb HeartbeatConfig) HeartbeatEnabled() bool {
	return hb.Every != "" && hb.Every != "0"
}

// HeartbeatPrompt returns the configured prompt or the default.
func (hb HeartbeatConfig) HeartbeatPrompt() string {
	if hb.Prompt != "" {
		return hb.Prompt
	}
	return "Read HEARTBEAT.md if it exists (workspace context). Follow it strictly. Do not infer or repeat old tasks from prior chats. If nothing needs attention, reply HEARTBEAT_OK."
}

// HeartbeatAckMaxChars returns the configured ack max chars or the default (300).
func (hb HeartbeatConfig) HeartbeatAckMaxChars() int {
	if hb.AckMaxChars > 0 {
		return hb.AckMaxChars
	}
	return 300
}

type ModelConfig struct {
	Primary   string   `json:"primary"`
	Fallbacks []string `json:"fallbacks"`
}

// CLIEngineConfig holds settings for the "claude-cli" engine mode.
type CLIEngineConfig struct {
	Command      string   `json:"command"`      // path/name of claude binary (default "claude")
	Model        string   `json:"model"`        // model to request (e.g. "sonnet", "opus")
	MCPConfig    string   `json:"mcpConfig"`    // path to MCP config JSON for GopherClaw tools
	SystemPrompt string   `json:"systemPrompt"` // custom system prompt override
	ExtraArgs    []string `json:"extraArgs"`    // additional CLI flags
	IdleTTLSec   int      `json:"idleTTLSec"`   // idle subprocess reap timeout in seconds (default 1800)
}

type ContextPruning struct {
	Mode               string  `json:"mode"`
	TTL                string  `json:"ttl"`
	KeepLastAssistants int     `json:"keepLastAssistants"`
	HardClearRatio     float64 `json:"hardClearRatio"`
	SoftTrimRatio      float64 `json:"softTrimRatio"`   // OpenClaw nests this here
	ModelMaxTokens     int     `json:"modelMaxTokens"`  // max context tokens for the model (default 128000)
	SurgicalPruning    *bool   `json:"surgicalPruning"` // prefer trimming tool results over hard clear (default true)
	CacheTTLSeconds    int     `json:"cacheTtlSeconds"` // seconds to defer pruning after last API call (default 0 = immediate)
}

// IsSurgicalPruning returns the resolved value of SurgicalPruning (default true).
func (cp ContextPruning) IsSurgicalPruning() bool {
	if cp.SurgicalPruning != nil {
		return *cp.SurgicalPruning
	}
	return true
}

type AgentDef struct {
	ID              string   `json:"id"`
	Default         bool     `json:"default"`
	Identity        Identity `json:"identity"`
	CLICommand      string   `json:"cliCommand"`      // if set, this agent delegates to a CLI subprocess
	CLIArgs         []string `json:"cliArgs"`         // args prepended before the message, e.g. ["-p"] for claude -p
	MaxConcurrent   int      `json:"maxConcurrent"`   // orchestrator: max parallel subtasks (default 5)
	ProgressUpdates bool     `json:"progressUpdates"` // orchestrator: send per-task completion updates
	Async           bool     `json:"async"`           // if true, runs via TaskManager (non-blocking)
}

type Identity struct {
	Name  string `json:"name"`
	Theme string `json:"theme"`
	Emoji string `json:"emoji"`
}

type Tools struct {
	Web     Web           `json:"web"`
	Exec    ExecConfig    `json:"exec"`
	Files   FilesConfig   `json:"files"`
	Browser BrowserConfig `json:"browser"`
}

// BrowserConfig controls the browser automation tool (requires Chrome/Chromium).
type BrowserConfig struct {
	Enabled    bool   `json:"enabled"`
	Headless   *bool  `json:"headless"`   // nil = default true when Enabled
	ChromePath string `json:"chromePath"` // empty = auto-detect
	NoSandbox  bool   `json:"noSandbox"`  // launch Chrome with --no-sandbox (required in some containers)
	Width      int    `json:"width"`      // viewport width (default 1280)
	Height     int    `json:"height"`     // viewport height (default 900)
}

// IsHeadless returns true if headless mode is enabled (default: true).
func (bc BrowserConfig) IsHeadless() bool {
	if bc.Headless == nil {
		return true
	}
	return *bc.Headless
}

type FilesConfig struct {
	AllowPaths []string `json:"allowPaths"` // empty = no restriction (backward-compatible)
}

type Web struct {
	Search WebSearch `json:"search"`
	Fetch  WebFetch  `json:"fetch"`
}

type WebSearch struct {
	Enabled        bool `json:"enabled"`
	MaxResults     int  `json:"maxResults"`
	TimeoutSeconds int  `json:"timeoutSeconds"`
}

type WebFetch struct {
	Enabled        bool `json:"enabled"`
	MaxChars       int  `json:"maxChars"`
	TimeoutSeconds int  `json:"timeoutSeconds"`
}

type ExecConfig struct {
	TimeoutSec          int      `json:"timeoutSec"`
	BackgroundMs        int      `json:"backgroundMs"`
	BackgroundHardTimeM int      `json:"backgroundHardTimeM"` // hard kill for bg processes in minutes (default 30)
	MaxOutputChars      int      `json:"maxOutputChars"`      // output truncation limit (default 100000)
	DenyCommands        []string `json:"denyCommands"`        // empty = no restriction (backward-compatible)
	DangerousPatterns   []string `json:"dangerousPatterns"`   // extra patterns that trigger confirmation (merged with builtins)
	ConfirmTimeoutSec   int      `json:"confirmTimeoutSec"`   // timeout for destructive command confirmation (default 60)
}

type Session struct {
	Scope               string       `json:"scope"`
	ResetTriggers       []string     `json:"resetTriggers"`
	Reset               SessionReset `json:"reset"`
	IdleMinutes         int          `json:"idleMinutes"`         // top-level shorthand (default 120)
	MaxConcurrent       int          `json:"maxConcurrent"`       // max concurrent requests per agent (default 2)
	ParentForkMaxTokens int          `json:"parentForkMaxTokens"` // parsed for config compat; not implemented (default 100000)
}

type SessionReset struct {
	Mode        string `json:"mode"`
	AtHour      *int   `json:"atHour"` // nil = default 4 for daily mode
	IdleMinutes int    `json:"idleMinutes"`
}

type Channels struct {
	Telegram TelegramConfig `json:"telegram"`
	Discord  DiscordConfig  `json:"discord"`
	Slack    SlackConfig    `json:"slack"`
}

type DiscordConfig struct {
	Enabled        bool     `json:"enabled"`
	BotToken       string   `json:"botToken"`
	DMPolicy       string   `json:"dmPolicy"`       // "pairing" or "allowlist"
	AllowUsers     []string `json:"allowUsers"`     // Discord user IDs (snowflakes, for allowlist mode)
	StreamMode     string   `json:"streamMode"`     // "partial" or ""
	ReplyToMode    string   `json:"replyToMode"`    // "first" or ""
	TimeoutSeconds int      `json:"timeoutSeconds"` // agent call timeout (default 300)
	AckEmoji       string   `json:"ackEmoji"`       // default "eyes"
}

type SlackConfig struct {
	Enabled        bool     `json:"enabled"`
	BotToken       string   `json:"botToken"`       // xoxb-...
	AppToken       string   `json:"appToken"`       // xapp-... (Socket Mode)
	AllowUsers     []string `json:"allowUsers"`     // Slack user IDs; empty = all workspace members
	StreamMode     string   `json:"streamMode"`     // "partial" or ""
	TimeoutSeconds int      `json:"timeoutSeconds"` // agent call timeout (default 300)
	AckEmoji       string   `json:"ackEmoji"`       // default "eyes"
}

type TelegramConfig struct {
	Enabled        bool                   `json:"enabled"`
	BotToken       string                 `json:"botToken"`
	DMPolicy       string                 `json:"dmPolicy"`
	StreamMode     string                 `json:"streamMode"`
	HistoryLimit   int                    `json:"historyLimit"`
	Groups         map[string]GroupConfig `json:"groups"`
	GroupPolicy    string                 `json:"groupPolicy"`
	ReplyToMode    string                 `json:"replyToMode"`    // "first" or ""
	TimeoutSeconds int                    `json:"timeoutSeconds"` // agent call timeout (default 300)
	AckEmoji       string                 `json:"ackEmoji"`       // default "👀"
}

type GroupConfig struct {
	RequireMention bool `json:"requireMention"`
}

type Gateway struct {
	Port            int             `json:"port"`
	Bind            string          `json:"bind"`
	ControlUI       ControlUI       `json:"controlUi"`
	Auth            GatewayAuth     `json:"auth"`
	Reload          GatewayReload   `json:"reload"`
	RateLimit       RateLimitConfig `json:"rateLimit"`
	WebhookSecret   string          `json:"webhookSecret"`   // HMAC-SHA256 secret for webhook signature validation
	ReadTimeoutSec  int             `json:"readTimeoutSec"`  // HTTP read timeout in seconds (default 300)
	WriteTimeoutSec int             `json:"writeTimeoutSec"` // HTTP write timeout in seconds (default 600)
}

// RateLimitConfig controls per-IP request rate limiting.
type RateLimitConfig struct {
	RPS   float64 `json:"rps"`   // max requests per second per IP (0 = disabled)
	Burst int     `json:"burst"` // burst capacity (default max(1, rps))
}

// UpdateConfig controls self-update behavior.
type UpdateConfig struct {
	AutoUpdate bool `json:"autoUpdate"` // if true, binary self-updates on startup when new version available (default: false)
}

// TaskQueueConfig holds settings for the background task queue.
type TaskQueueConfig struct {
	MaxConcurrent     int `json:"maxConcurrent"`     // max parallel tasks (default 5)
	ResultRetentionM  int `json:"resultRetentionM"`  // minutes to keep completed task results (default 60)
	ProgressThrottleS int `json:"progressThrottleS"` // seconds between progress updates (default 5)
}

type GatewayReload struct {
	Mode       string `json:"mode"`       // "hybrid", "poll", ""
	DebounceMs int    `json:"debounceMs"` // e.g. 300
}

type ControlUI struct {
	Enabled  bool   `json:"enabled"`
	BasePath string `json:"basePath"`
}

type GatewayAuth struct {
	Mode  string `json:"mode"`
	Token string `json:"token"`
}

// Load reads and parses the config file.
// When path is empty it uses ~/.gopherclaw/config.json.
func Load(path string) (*Root, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, ".gopherclaw", "config.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg Root
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.Path = path
	cfg.applyDefaults()
	return &cfg, nil
}

func (c *Root) applyDefaults() {
	if c.Agents.Defaults.TimeoutSeconds == 0 {
		c.Agents.Defaults.TimeoutSeconds = 300
	}
	if c.Tools.Exec.TimeoutSec == 0 {
		c.Tools.Exec.TimeoutSec = 300
	}
	if c.Tools.Exec.ConfirmTimeoutSec == 0 {
		c.Tools.Exec.ConfirmTimeoutSec = 60
	}
	if c.Tools.Exec.BackgroundHardTimeM == 0 {
		c.Tools.Exec.BackgroundHardTimeM = 30
	}
	if c.Tools.Exec.MaxOutputChars == 0 {
		c.Tools.Exec.MaxOutputChars = 100_000
	}
	if c.Tools.Web.Search.MaxResults == 0 {
		c.Tools.Web.Search.MaxResults = 5
	}
	if c.Tools.Web.Fetch.MaxChars == 0 {
		c.Tools.Web.Fetch.MaxChars = 50000
	}
	if c.Tools.Web.Search.TimeoutSeconds == 0 {
		c.Tools.Web.Search.TimeoutSeconds = 30
	}
	if c.Tools.Web.Fetch.TimeoutSeconds == 0 {
		c.Tools.Web.Fetch.TimeoutSeconds = 30
	}
	if c.Gateway.Port == 0 {
		c.Gateway.Port = 18789
	}
	if c.Gateway.ReadTimeoutSec == 0 {
		c.Gateway.ReadTimeoutSec = 300
	}
	if c.Gateway.WriteTimeoutSec == 0 {
		c.Gateway.WriteTimeoutSec = 600
	}
	if c.Agents.Defaults.UserTimezone == "" {
		c.Agents.Defaults.UserTimezone = "UTC"
	}
	// Session defaults
	if c.Session.IdleMinutes == 0 {
		// Also check nested Reset.IdleMinutes for backward compat
		if c.Session.Reset.IdleMinutes > 0 {
			c.Session.IdleMinutes = c.Session.Reset.IdleMinutes
		} else {
			c.Session.IdleMinutes = 120
		}
	}
	if c.Session.MaxConcurrent == 0 {
		if c.Agents.Defaults.MaxConcurrent > 0 {
			c.Session.MaxConcurrent = c.Agents.Defaults.MaxConcurrent
		} else {
			c.Session.MaxConcurrent = 2
		}
	}
	if c.Session.Reset.AtHour == nil && c.Session.Reset.Mode == "daily" {
		h := 4
		c.Session.Reset.AtHour = &h
	}
	if c.Agents.Defaults.ContextPruning.HardClearRatio == 0 {
		c.Agents.Defaults.ContextPruning.HardClearRatio = 0.5
	}
	if c.Agents.Defaults.ContextPruning.KeepLastAssistants == 0 {
		c.Agents.Defaults.ContextPruning.KeepLastAssistants = 2
	}
	if c.Agents.Defaults.ContextPruning.ModelMaxTokens == 0 {
		c.Agents.Defaults.ContextPruning.ModelMaxTokens = 128_000
	}
	// Inherit softTrimRatio from contextPruning if the top-level field is unset
	// (OpenClaw nests it under contextPruning; GopherClaw supports both paths)
	if c.Agents.Defaults.SoftTrimRatio == 0 && c.Agents.Defaults.ContextPruning.SoftTrimRatio > 0 {
		c.Agents.Defaults.SoftTrimRatio = c.Agents.Defaults.ContextPruning.SoftTrimRatio
	}
	if c.Agents.Defaults.LoopDetectionN == 0 {
		c.Agents.Defaults.LoopDetectionN = 3
	}
	if c.Agents.Defaults.MaxIterations == 0 {
		c.Agents.Defaults.MaxIterations = 50
	}
	if c.Agents.Defaults.Subagents.MaxConcurrent == 0 {
		c.Agents.Defaults.Subagents.MaxConcurrent = 4
	}
	// Multi-detector tool loop detection defaults (REQ-410)
	tld := &c.Agents.Defaults.ToolLoopDetection
	if !tld.Enabled && tld.HistorySize == 0 && tld.WarningThreshold == 0 {
		// Not explicitly configured — apply defaults (enabled by default)
		tld.Enabled = true
	}
	if tld.HistorySize == 0 {
		tld.HistorySize = 30
	}
	if tld.WarningThreshold == 0 {
		tld.WarningThreshold = 10
	}
	if tld.CriticalThreshold == 0 {
		tld.CriticalThreshold = 20
	}
	if tld.GlobalCircuitBreakerThreshold == 0 {
		tld.GlobalCircuitBreakerThreshold = 30
	}
	if c.Channels.Telegram.TimeoutSeconds == 0 {
		c.Channels.Telegram.TimeoutSeconds = 300
	}
	if c.Channels.Discord.TimeoutSeconds == 0 {
		c.Channels.Discord.TimeoutSeconds = 300
	}
	if c.Channels.Slack.TimeoutSeconds == 0 {
		c.Channels.Slack.TimeoutSeconds = 300
	}
	// Browser: default headless=true when enabled and not explicitly set
	if c.Tools.Browser.Enabled && c.Tools.Browser.Headless == nil {
		t := true
		c.Tools.Browser.Headless = &t
	}
	// Sandbox defaults
	if c.Agents.Defaults.Sandbox.Enabled && c.Agents.Defaults.Sandbox.Image == "" {
		c.Agents.Defaults.Sandbox.Image = "ubuntu:22.04"
	}
	// Eidetic defaults (only meaningful when enabled, but always set so callers
	// don't have to guard against zero values)
	if c.Eidetic.BaseURL == "" {
		c.Eidetic.BaseURL = "http://localhost:7700"
	}
	if c.Eidetic.RecentLimit == 0 {
		c.Eidetic.RecentLimit = 20
	}
	if c.Eidetic.SearchLimit == 0 {
		c.Eidetic.SearchLimit = 10
	}
	if c.Eidetic.SearchThreshold == 0 {
		c.Eidetic.SearchThreshold = 0.5
	}
	if c.Eidetic.TimeoutSeconds == 0 {
		c.Eidetic.TimeoutSeconds = 5
	}
	if c.Eidetic.RecallEnabled == nil {
		t := true
		c.Eidetic.RecallEnabled = &t
	}
	if c.Eidetic.RecallLimit == 0 {
		c.Eidetic.RecallLimit = 5
	}
	if c.Eidetic.RecallThreshold == 0 {
		c.Eidetic.RecallThreshold = 0.4
	}
}

// DefaultAgent returns the first agent with default:true, or the first agent.
func (c *Root) DefaultAgent() *AgentDef {
	for i := range c.Agents.List {
		if c.Agents.List[i].Default {
			return &c.Agents.List[i]
		}
	}
	if len(c.Agents.List) > 0 {
		return &c.Agents.List[0]
	}
	return &AgentDef{ID: "main", Identity: Identity{Name: "GopherClaw"}}
}

// Reload re-reads the config file and applies defaults.
func (c *Root) Reload() (*Root, error) {
	return Load(c.Path)
}

// EnsureAuth ensures a gateway auth token is set. If mode is "token" (the
// default) and no token is configured, a cryptographically random token is
// generated, written into the in-memory config, and persisted to config.json.
// Returns the token in use and whether one was generated.
//
// mode "none" and "trusted-proxy" are explicit opt-outs; EnsureAuth is a no-op.
func (c *Root) EnsureAuth() (token string, generated bool, err error) {
	mode := c.Gateway.Auth.Mode
	if mode == "none" || mode == "trusted-proxy" {
		return "", false, nil
	}
	// Default mode is "token". If token is already set, nothing to do.
	if t := c.Gateway.Auth.Token; t != "" {
		return t, false, nil
	}
	// Generate a 32-byte (64 hex char) random token.
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", false, fmt.Errorf("generate auth token: %w", err)
	}
	token = hex.EncodeToString(buf)
	c.Gateway.Auth.Token = token
	c.Gateway.Auth.Mode = "token"
	if err := c.persistToken(token); err != nil {
		// Non-fatal: token is set in memory; log caller should warn to save it.
		return token, true, fmt.Errorf("persist auth token: %w", err)
	}
	return token, true, nil
}

// persistToken writes the generated token back into config.json, preserving
// all other fields. Writes to a temp file then renames for atomicity.
func (c *Root) persistToken(token string) error {
	if c.Path == "" {
		return nil
	}
	data, err := os.ReadFile(c.Path)
	if err != nil {
		return err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	gw, _ := raw["gateway"].(map[string]any)
	if gw == nil {
		gw = map[string]any{}
	}
	authRaw, _ := gw["auth"].(map[string]any)
	if authRaw == nil {
		authRaw = map[string]any{}
	}
	authRaw["mode"] = "token"
	authRaw["token"] = token
	gw["auth"] = authRaw
	raw["gateway"] = gw

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')

	// Write atomically via temp file + rename.
	tmp := c.Path + ".tmp"
	if err := os.WriteFile(tmp, out, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, c.Path)
}

// GatewayListenAddr returns the listen address for the gateway.
func (c *Root) GatewayListenAddr() string {
	host := "127.0.0.1"
	if c.Gateway.Bind != "loopback" && c.Gateway.Bind != "" {
		host = "0.0.0.0"
	}
	return fmt.Sprintf("%s:%d", host, c.Gateway.Port)
}
