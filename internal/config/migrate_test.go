package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAutoMigrate_EmptyPath(t *testing.T) {
	if err := AutoMigrate("", "1.0.0"); err != nil {
		t.Fatalf("expected nil error for empty path, got %v", err)
	}
}

func TestAutoMigrate_MissingFile(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does_not_exist.json")

	if err := AutoMigrate(missing, "1.0.0"); err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
}

func TestAutoMigrate_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	if err := os.WriteFile(cfgPath, []byte(`{not valid json!!!`), 0600); err != nil {
		t.Fatal(err)
	}

	if err := AutoMigrate(cfgPath, "1.0.0"); err != nil {
		t.Fatalf("expected nil error for malformed JSON, got %v", err)
	}
}

func TestAutoMigrate_AlreadyUpToDate(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfg := map[string]any{
		"meta": map[string]any{
			"lastTouchedVersion": "1.0.0",
		},
	}
	writeJSON(t, cfgPath, cfg)

	if err := AutoMigrate(cfgPath, "1.0.0"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	backupPath := cfgPath + ".bak"
	if _, err := os.Stat(backupPath); err == nil {
		t.Fatal("backup file should not exist when config is already up to date")
	}
}

func TestAutoMigrate_AppliesMigration(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfg := map[string]any{
		"meta": map[string]any{
			"lastTouchedVersion": "0.1.0",
		},
		"someKey": "someValue",
	}
	writeJSON(t, cfgPath, cfg)

	originalData, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	const newVersion = "99.0.0"
	if err := AutoMigrate(cfgPath, newVersion); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify backup was created with original content.
	backupPath := cfgPath + ".bak"
	backupData, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("expected backup file at %s: %v", backupPath, err)
	}
	if string(backupData) != string(originalData) {
		t.Fatal("backup content does not match original config")
	}

	// Verify the migrated config has the updated version.
	migrated := readJSON(t, cfgPath)
	metaRaw, ok := migrated["meta"].(map[string]any)
	if !ok {
		t.Fatal("migrated config missing meta object")
	}
	got, _ := metaRaw["lastTouchedVersion"].(string)
	if got != newVersion {
		t.Fatalf("expected lastTouchedVersion=%q, got %q", newVersion, got)
	}
}

func TestAutoMigrate_PreservesExistingFields(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfg := map[string]any{
		"meta": map[string]any{
			"lastTouchedVersion": "0.0.1",
			"customMetaField":    "keep-me",
		},
		"database": map[string]any{
			"host": "localhost",
			"port": float64(5432),
		},
		"featureFlags": []any{"alpha", "beta"},
	}
	writeJSON(t, cfgPath, cfg)

	const newVersion = "99.0.0"
	if err := AutoMigrate(cfgPath, newVersion); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	migrated := readJSON(t, cfgPath)

	// Verify meta was updated but custom meta field preserved.
	metaRaw, ok := migrated["meta"].(map[string]any)
	if !ok {
		t.Fatal("migrated config missing meta object")
	}
	if got, _ := metaRaw["lastTouchedVersion"].(string); got != newVersion {
		t.Fatalf("expected lastTouchedVersion=%q, got %q", newVersion, got)
	}
	if got, _ := metaRaw["customMetaField"].(string); got != "keep-me" {
		t.Fatalf("expected customMetaField=%q, got %q", "keep-me", got)
	}

	// Verify database section preserved.
	db, ok := migrated["database"].(map[string]any)
	if !ok {
		t.Fatal("migrated config missing database object")
	}
	if host, _ := db["host"].(string); host != "localhost" {
		t.Fatalf("expected database.host=%q, got %q", "localhost", host)
	}
	if port, _ := db["port"].(float64); port != 5432 {
		t.Fatalf("expected database.port=%v, got %v", 5432, port)
	}

	// Verify featureFlags array preserved.
	flags, ok := migrated["featureFlags"].([]any)
	if !ok {
		t.Fatal("migrated config missing featureFlags array")
	}
	if len(flags) != 2 {
		t.Fatalf("expected 2 featureFlags, got %d", len(flags))
	}
	if flags[0] != "alpha" || flags[1] != "beta" {
		t.Fatalf("featureFlags content mismatch: got %v", flags)
	}
}

// writeJSON is a test helper that marshals v to cfgPath.
func writeJSON(t *testing.T, cfgPath string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, data, 0600); err != nil {
		t.Fatal(err)
	}
}

// readJSON is a test helper that reads and unmarshals cfgPath.
func readJSON(t *testing.T, cfgPath string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
