package config

import (
	"encoding/json"
	"testing"
)

// FuzzConfigUnmarshal fuzzes JSON deserialization of the full config.Root
// struct followed by applyDefaults(). This tests that arbitrary JSON never
// causes a panic during config parsing or default application.
func FuzzConfigUnmarshal(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"env":{"FOO":"bar"}}`))
	f.Add([]byte(`{"gateway":{"port":8080,"auth":{"mode":"token","token":"abc"}}}`))
	f.Add([]byte(`{"agents":{"defaults":{"model":{"primary":"gpt-4"}}}}`))
	f.Add([]byte(`{"channels":{"telegram":{"enabled":true,"botToken":"123:abc"}}}`))
	f.Add([]byte(`{"tools":{"exec":{"timeoutSec":10},"browser":{"enabled":true}}}`))
	f.Add([]byte(`{"session":{"scope":"user","reset":{"mode":"daily"}}}`))
	f.Add([]byte(`{"logging":{"level":"debug","file":"/tmp/test.log"}}`))
	f.Add([]byte(`{"providers":{"openai":{"apiKey":"sk-test"}}}`))
	f.Add([]byte(``))
	f.Add([]byte(`{`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`"just a string"`))
	f.Add([]byte(`{"gateway":{"port":-1}}`))
	f.Add([]byte(`{"agents":{"defaults":{"timeoutSeconds":999999999}}}`))
	f.Add([]byte(`{"agents":{"defaults":{"contextPruning":{"hardClearRatio":999.9}}}}`))
	f.Add([]byte(`{"eidetic":{"enabled":true,"baseURL":"http://localhost:7700"}}`))
	f.Add([]byte(`{"agents":{"defaults":{"sandbox":{"enabled":true}}}}`))
	f.Add([]byte(`{"agents":{"list":[{"id":"a","default":true},{"id":"b"}]}}`))
	f.Add([]byte(`{"taskQueue":{"maxConcurrent":100}}`))

	// Full realistic config
	f.Add([]byte(`{
		"env": {"HOME": "/home/test"},
		"providers": {"anthropic": {"apiKey": "sk-ant-test"}},
		"logging": {"level": "info", "consoleLevel": "warn"},
		"agents": {
			"defaults": {
				"model": {"primary": "claude-sonnet-4-20250514", "fallbacks": ["gpt-4"]},
				"workspace": "/tmp/workspace",
				"timeoutSeconds": 300,
				"maxConcurrent": 2,
				"memory": {"enabled": true},
				"thinking": {"enabled": true, "budgetTokens": 8192}
			},
			"list": [{"id": "main", "default": true, "identity": {"name": "Test"}}]
		},
		"tools": {
			"web": {"search": {"enabled": true}, "fetch": {"enabled": true}},
			"exec": {"timeoutSec": 60},
			"browser": {"enabled": false}
		},
		"session": {"scope": "user", "reset": {"mode": "daily", "atHour": 4}},
		"channels": {
			"telegram": {"enabled": true, "botToken": "123:abc"},
			"discord": {"enabled": false},
			"slack": {"enabled": false}
		},
		"gateway": {"port": 18789, "auth": {"mode": "token", "token": "secret"}}
	}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var cfg Root
		err := json.Unmarshal(data, &cfg)
		if err != nil {
			return // invalid JSON is expected; just ensure no panic
		}
		// applyDefaults must not panic on any successfully parsed config.
		cfg.applyDefaults()

		// Exercise methods that read config fields to catch nil derefs.
		_ = cfg.DefaultAgent()
		_ = cfg.GatewayListenAddr()
		_ = cfg.ResolveModelAlias("test")
		_ = cfg.Agents.Defaults.ContextPruning.IsSurgicalPruning()
		_ = cfg.Tools.Browser.IsHeadless()
		_ = cfg.Agents.Defaults.Heartbeat.HeartbeatEnabled()
		_ = cfg.Agents.Defaults.Heartbeat.HeartbeatPrompt()
		_ = cfg.Agents.Defaults.Heartbeat.HeartbeatAckMaxChars()
	})
}
