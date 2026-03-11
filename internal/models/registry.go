package models

import (
	"errors"
	"fmt"
	"os"

	openai "github.com/sashabaranov/go-openai"

	"go.uber.org/zap"

	"github.com/EMSERO/gopherclaw/internal/config"
)

// ErrNoProvider is returned when no provider is configured for a model.
var ErrNoProvider = errors.New("no provider configured")

// BuildProviders creates the full provider map from configuration.
//
// GitHub Copilot is always registered using the supplied TokenProvider for auth.
// Additional providers are built from cfg.Providers:
//   - "anthropic" → native Anthropic messages API client
//   - all others  → OpenAI-compatible client (go-openai with custom base URL + API key)
//
// Unknown provider names with neither a configured baseURL nor a known default
// fall back to the OpenAI base URL; an operator warning is logged.
func BuildProviders(logger *zap.SugaredLogger, cfg *config.Root, copilotAuth TokenProvider) map[string]Provider {
	providers := make(map[string]Provider)

	// GitHub Copilot is always available via the existing auth system.
	providers["github-copilot"] = &openaiProvider{client: NewCopilotClient(copilotAuth)}

	tc := ThinkingConfig{
		Enabled:      cfg.Agents.Defaults.Thinking.Enabled,
		BudgetTokens: cfg.Agents.Defaults.Thinking.BudgetTokens,
		Level:        cfg.Agents.Defaults.Thinking.Level,
	}

	// Auto-register Anthropic from ANTHROPIC_API_KEY if not explicitly configured.
	if _, ok := cfg.Providers["anthropic"]; !ok {
		key := cfg.Env["ANTHROPIC_API_KEY"]
		if key == "" {
			key = os.Getenv("ANTHROPIC_API_KEY")
		}
		if key != "" {
			providers["anthropic"] = newAnthropicProvider(key, tc)
			logger.Infof("providers: registered anthropic from ANTHROPIC_API_KEY env (thinking=%v)", tc.Enabled)
		}
	}

	for name, pcfg := range cfg.Providers {
		if pcfg == nil {
			continue
		}
		switch name {
		case "anthropic":
			apiKey := pcfg.APIKey
			if apiKey == "" {
				apiKey = cfg.Env["ANTHROPIC_API_KEY"]
			}
			if apiKey == "" {
				apiKey = os.Getenv("ANTHROPIC_API_KEY")
			}
			if apiKey == "" {
				logger.Warnf("providers: anthropic configured but no API key found")
				continue
			}
			providers[name] = newAnthropicProvider(apiKey, tc)
			logger.Infof("providers: registered anthropic (thinking=%v)", tc.Enabled)

		default:
			// OpenAI-compatible provider.
			baseURL := pcfg.BaseURL
			if baseURL == "" {
				baseURL = DefaultBaseURL(name)
			}
			if baseURL == "" {
				logger.Warnf("providers: %q has no baseURL and is not a known provider — defaulting to OpenAI API", name)
				baseURL = "https://api.openai.com/v1"
			}
			apiKey := pcfg.APIKey
			if apiKey == "" {
				apiKey = "no-key"
			}
			oaiCfg := openai.DefaultConfig(apiKey)
			oaiCfg.BaseURL = baseURL
			providers[name] = &openaiProvider{client: openai.NewClientWithConfig(oaiCfg)}
			logger.Infof("providers: registered %q → %s", name, baseURL)
		}
	}

	return providers
}

// ProviderNames returns the sorted list of registered provider names for logging.
func ProviderNames(providers map[string]Provider) []string {
	names := make([]string, 0, len(providers))
	for n := range providers {
		names = append(names, n)
	}
	return names
}

// ProviderFor returns the provider and stripped model ID for a full model string.
// Returns an error if the provider is not registered.
func ProviderFor(providers map[string]Provider, fullModel string) (Provider, string, error) {
	providerID, modelID := splitModel(fullModel)
	p, ok := providers[providerID]
	if !ok {
		return nil, "", fmt.Errorf("%w for %q (model: %q) — add it to the providers config section", ErrNoProvider, providerID, fullModel)
	}
	return p, modelID, nil
}
