package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// Meta holds bookkeeping fields for config versioning.
type Meta struct {
	LastTouchedVersion string `json:"lastTouchedVersion"`
}

// migration is a single versioned config transformation.
type migration struct {
	Version string                        // target version (e.g. "0.1.0")
	Apply   func(raw map[string]any) bool // returns true if any change was made
}

// migrations is the ordered table of versioned config transformations.
// Append new migrations at the end with increasing version strings.
var migrations = []migration{
	{
		Version: "0.1.0",
		Apply: func(raw map[string]any) bool {
			// Ensure meta.lastTouchedVersion exists.
			// Future migrations would add structural transformations here.
			return true
		},
	},
}

// AutoMigrate checks the config's meta.lastTouchedVersion and applies any
// newer migrations. If changes are made, the original config.json is backed
// up to config.json.bak before the migrated version is written.
func AutoMigrate(cfgPath, currentVersion string) error {
	if cfgPath == "" {
		return nil
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil // file doesn't exist yet; nothing to migrate
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil // malformed config; let Load() report the error
	}

	// Read current meta.lastTouchedVersion
	metaRaw, _ := raw["meta"].(map[string]any)
	var lastVersion string
	if metaRaw != nil {
		lastVersion, _ = metaRaw["lastTouchedVersion"].(string)
	}

	if lastVersion == currentVersion {
		return nil // already up to date
	}

	// Apply all migrations newer than lastVersion
	for _, m := range migrations {
		if lastVersion != "" && m.Version <= lastVersion {
			continue
		}
		m.Apply(raw)
	}

	// Always update the meta version stamp
	if metaRaw == nil {
		metaRaw = map[string]any{}
	}
	metaRaw["lastTouchedVersion"] = currentVersion
	raw["meta"] = metaRaw

	// Backup original config
	backupPath := cfgPath + ".bak"
	if err := os.WriteFile(backupPath, data, 0600); err != nil {
		return fmt.Errorf("backup config to %s: %w", backupPath, err)
	}

	// Write migrated config
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal migrated config: %w", err)
	}
	out = append(out, '\n')

	tmp := cfgPath + ".tmp"
	if err := os.WriteFile(tmp, out, 0600); err != nil {
		return fmt.Errorf("write migrated config: %w", err)
	}
	return os.Rename(tmp, cfgPath)
}
