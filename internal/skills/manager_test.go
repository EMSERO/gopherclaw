package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
)

func testLogger() *zap.SugaredLogger { return zap.NewNop().Sugar() }

func TestNewManager_LoadsSkills(t *testing.T) {
	workspace := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "state", "skill-states.json")

	createSkillFile(t, workspace, "alpha", "---\nname: alpha\ndescription: First skill\n---\nAlpha content\n")
	createSkillFile(t, workspace, "beta", "---\nname: beta\ndescription: Second skill\n---\nBeta content\n")

	m, err := NewManager(testLogger(), workspace, statePath)
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	skills := m.Skills()
	if len(skills) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(skills))
	}

	byName := make(map[string]Skill, len(skills))
	for _, s := range skills {
		byName[s.Name] = s
	}

	if s, ok := byName["alpha"]; !ok {
		t.Error("missing skill 'alpha'")
	} else {
		if s.Description != "First skill" {
			t.Errorf("alpha description: got %q, want %q", s.Description, "First skill")
		}
		if s.Content != "Alpha content" {
			t.Errorf("alpha content: got %q, want %q", s.Content, "Alpha content")
		}
		if !s.Enabled {
			t.Error("alpha should be enabled by default")
		}
	}

	if s, ok := byName["beta"]; !ok {
		t.Error("missing skill 'beta'")
	} else {
		if s.Description != "Second skill" {
			t.Errorf("beta description: got %q, want %q", s.Description, "Second skill")
		}
		if !s.Enabled {
			t.Error("beta should be enabled by default")
		}
	}
}

func TestNewManager_AppliesPersistedState(t *testing.T) {
	workspace := t.TempDir()
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "skill-states.json")

	createSkillFile(t, workspace, "enabled-skill", "---\nname: enabled-skill\ndescription: stays enabled\n---\nContent\n")
	createSkillFile(t, workspace, "disabled-skill", "---\nname: disabled-skill\ndescription: should be disabled\n---\nContent\n")

	// Write a persisted state file that disables "disabled-skill".
	states := skillStates{
		"enabled-skill":  true,
		"disabled-skill": false,
	}
	data, err := json.Marshal(states)
	if err != nil {
		t.Fatalf("failed to marshal states: %v", err)
	}
	if err := os.WriteFile(statePath, data, 0600); err != nil {
		t.Fatalf("failed to write state file: %v", err)
	}

	m, err := NewManager(testLogger(), workspace, statePath)
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	skills := m.Skills()
	byName := make(map[string]Skill, len(skills))
	for _, s := range skills {
		byName[s.Name] = s
	}

	if s, ok := byName["enabled-skill"]; !ok {
		t.Error("missing skill 'enabled-skill'")
	} else if !s.Enabled {
		t.Error("'enabled-skill' should be enabled per persisted state")
	}

	if s, ok := byName["disabled-skill"]; !ok {
		t.Error("missing skill 'disabled-skill'")
	} else if s.Enabled {
		t.Error("'disabled-skill' should be disabled per persisted state")
	}
}

func TestEnabledSkills(t *testing.T) {
	workspace := t.TempDir()
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "skill-states.json")

	createSkillFile(t, workspace, "on1", "---\nname: on1\n---\nContent\n")
	createSkillFile(t, workspace, "on2", "---\nname: on2\n---\nContent\n")
	createSkillFile(t, workspace, "off1", "---\nname: off1\n---\nContent\n")

	// Pre-persist state: disable off1.
	states := skillStates{
		"on1":  true,
		"on2":  true,
		"off1": false,
	}
	data, err := json.Marshal(states)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(statePath, data, 0600); err != nil {
		t.Fatalf("write state: %v", err)
	}

	m, err := NewManager(testLogger(), workspace, statePath)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	enabled := m.EnabledSkills()
	if len(enabled) != 2 {
		t.Fatalf("expected 2 enabled skills, got %d", len(enabled))
	}

	names := make(map[string]bool)
	for _, s := range enabled {
		names[s.Name] = true
		if !s.Enabled {
			t.Errorf("skill %q in EnabledSkills() has Enabled=false", s.Name)
		}
	}

	if !names["on1"] {
		t.Error("expected 'on1' in enabled skills")
	}
	if !names["on2"] {
		t.Error("expected 'on2' in enabled skills")
	}
	if names["off1"] {
		t.Error("'off1' should not be in enabled skills")
	}
}

func TestSetEnabled_PersistsState(t *testing.T) {
	workspace := t.TempDir()
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "skill-states.json")

	createSkillFile(t, workspace, "toggle-me", "---\nname: toggle-me\ndescription: A toggleable skill\n---\nContent\n")
	createSkillFile(t, workspace, "other", "---\nname: other\n---\nContent\n")

	m, err := NewManager(testLogger(), workspace, statePath)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Verify default state: all enabled.
	if len(m.EnabledSkills()) != 2 {
		t.Fatalf("expected 2 enabled skills initially, got %d", len(m.EnabledSkills()))
	}

	// Disable toggle-me.
	found := m.SetEnabled("toggle-me", false)
	if !found {
		t.Fatal("SetEnabled('toggle-me', false) returned false, expected true")
	}

	// Verify in-memory state changed.
	enabled := m.EnabledSkills()
	if len(enabled) != 1 {
		t.Fatalf("expected 1 enabled skill after disable, got %d", len(enabled))
	}
	if enabled[0].Name != "other" {
		t.Errorf("expected remaining enabled skill to be 'other', got %q", enabled[0].Name)
	}

	// Verify state file was written.
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("failed to read persisted state file: %v", err)
	}

	var persisted skillStates
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("failed to unmarshal persisted state: %v", err)
	}

	if val, ok := persisted["toggle-me"]; !ok {
		t.Error("'toggle-me' missing from persisted state")
	} else if val {
		t.Error("'toggle-me' should be false in persisted state")
	}

	if val, ok := persisted["other"]; !ok {
		t.Error("'other' missing from persisted state")
	} else if !val {
		t.Error("'other' should be true in persisted state")
	}

	// Re-enable toggle-me and verify persistence again.
	found = m.SetEnabled("toggle-me", true)
	if !found {
		t.Fatal("SetEnabled('toggle-me', true) returned false")
	}

	data, err = os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("failed to re-read state file: %v", err)
	}
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("unmarshal re-read: %v", err)
	}
	if !persisted["toggle-me"] {
		t.Error("'toggle-me' should be true after re-enable")
	}
}

func TestSetEnabled_NotFound(t *testing.T) {
	workspace := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "skill-states.json")

	createSkillFile(t, workspace, "existing", "---\nname: existing\n---\nContent\n")

	m, err := NewManager(testLogger(), workspace, statePath)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	found := m.SetEnabled("nonexistent-skill", false)
	if found {
		t.Error("SetEnabled should return false for a nonexistent skill")
	}

	// Verify no state file was written (since no skill was found, persistStates is not called).
	if _, err := os.Stat(statePath); err == nil {
		t.Error("state file should not have been created when SetEnabled returns false")
	}
}

func TestReload(t *testing.T) {
	workspace := t.TempDir()
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "skill-states.json")

	createSkillFile(t, workspace, "original", "---\nname: original\ndescription: Original skill\n---\nOriginal content\n")

	m, err := NewManager(testLogger(), workspace, statePath)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if m.Count() != 1 {
		t.Fatalf("expected 1 skill initially, got %d", m.Count())
	}

	// Add a new skill to the workspace.
	createSkillFile(t, workspace, "added", "---\nname: added\ndescription: New skill\n---\nNew content\n")

	// Reload should pick up the new skill.
	if err := m.Reload(); err != nil {
		t.Fatalf("Reload returned error: %v", err)
	}

	if m.Count() != 2 {
		t.Fatalf("expected 2 skills after Reload, got %d", m.Count())
	}

	skills := m.Skills()
	byName := make(map[string]Skill, len(skills))
	for _, s := range skills {
		byName[s.Name] = s
	}

	if _, ok := byName["original"]; !ok {
		t.Error("missing 'original' after Reload")
	}
	if s, ok := byName["added"]; !ok {
		t.Error("missing 'added' after Reload")
	} else if s.Description != "New skill" {
		t.Errorf("added description: got %q, want %q", s.Description, "New skill")
	}

	// Remove the original skill and modify the added skill.
	if err := os.RemoveAll(filepath.Join(workspace, "skills", "original")); err != nil {
		t.Fatalf("failed to remove 'original' skill dir: %v", err)
	}
	// Overwrite the added skill content.
	if err := os.WriteFile(
		filepath.Join(workspace, "skills", "added", "SKILL.md"),
		[]byte("---\nname: added\ndescription: Updated description\n---\nUpdated content\n"),
		0644,
	); err != nil {
		t.Fatalf("failed to update 'added' skill: %v", err)
	}

	if err := m.Reload(); err != nil {
		t.Fatalf("Reload (second) returned error: %v", err)
	}

	if m.Count() != 1 {
		t.Fatalf("expected 1 skill after removing 'original', got %d", m.Count())
	}

	skills = m.Skills()
	if skills[0].Name != "added" {
		t.Errorf("expected remaining skill to be 'added', got %q", skills[0].Name)
	}
	if skills[0].Description != "Updated description" {
		t.Errorf("expected updated description, got %q", skills[0].Description)
	}
	if skills[0].Content != "Updated content" {
		t.Errorf("expected updated content, got %q", skills[0].Content)
	}
}

func TestReload_PreservesPersistedState(t *testing.T) {
	workspace := t.TempDir()
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "skill-states.json")

	createSkillFile(t, workspace, "keeper", "---\nname: keeper\n---\nContent\n")
	createSkillFile(t, workspace, "toggled", "---\nname: toggled\n---\nContent\n")

	m, err := NewManager(testLogger(), workspace, statePath)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Disable "toggled" to persist state.
	m.SetEnabled("toggled", false)

	// Add a new skill and reload.
	createSkillFile(t, workspace, "newcomer", "---\nname: newcomer\n---\nContent\n")

	if err := m.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	// Verify "toggled" is still disabled after reload (state was persisted and reapplied).
	skills := m.Skills()
	byName := make(map[string]Skill, len(skills))
	for _, s := range skills {
		byName[s.Name] = s
	}

	if s, ok := byName["toggled"]; !ok {
		t.Error("missing 'toggled' after Reload")
	} else if s.Enabled {
		t.Error("'toggled' should remain disabled after Reload")
	}

	if s, ok := byName["keeper"]; !ok {
		t.Error("missing 'keeper' after Reload")
	} else if !s.Enabled {
		t.Error("'keeper' should be enabled after Reload")
	}

	// The newcomer has no persisted state, so it should default to enabled.
	if s, ok := byName["newcomer"]; !ok {
		t.Error("missing 'newcomer' after Reload")
	} else if !s.Enabled {
		t.Error("'newcomer' should be enabled by default after Reload")
	}
}

func TestCount(t *testing.T) {
	workspace := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "skill-states.json")

	// Empty workspace: 0 skills.
	m, err := NewManager(testLogger(), workspace, statePath)
	if err != nil {
		t.Fatalf("NewManager (empty): %v", err)
	}
	if m.Count() != 0 {
		t.Errorf("expected 0 skills for empty workspace, got %d", m.Count())
	}

	// Add skills one at a time and verify count after reload.
	createSkillFile(t, workspace, "one", "---\nname: one\n---\nContent\n")
	if err := m.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if m.Count() != 1 {
		t.Errorf("expected 1 skill, got %d", m.Count())
	}

	createSkillFile(t, workspace, "two", "---\nname: two\n---\nContent\n")
	createSkillFile(t, workspace, "three", "---\nname: three\n---\nContent\n")
	if err := m.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if m.Count() != 3 {
		t.Errorf("expected 3 skills, got %d", m.Count())
	}
}

func TestNewManager_NoStatePath(t *testing.T) {
	workspace := t.TempDir()

	createSkillFile(t, workspace, "skill-a", "---\nname: skill-a\ndescription: Skill A\n---\nContent A\n")
	createSkillFile(t, workspace, "skill-b", "---\nname: skill-b\ndescription: Skill B\n---\nContent B\n")

	// Pass empty statePath -- no persistence.
	m, err := NewManager(testLogger(), workspace, "")
	if err != nil {
		t.Fatalf("NewManager with empty statePath returned error: %v", err)
	}

	// All skills should be loaded and enabled.
	if m.Count() != 2 {
		t.Fatalf("expected 2 skills, got %d", m.Count())
	}
	enabled := m.EnabledSkills()
	if len(enabled) != 2 {
		t.Fatalf("expected 2 enabled skills, got %d", len(enabled))
	}

	// SetEnabled should still work in-memory and return true.
	found := m.SetEnabled("skill-a", false)
	if !found {
		t.Error("SetEnabled should return true even without persistence")
	}

	enabled = m.EnabledSkills()
	if len(enabled) != 1 {
		t.Fatalf("expected 1 enabled skill after disable, got %d", len(enabled))
	}
	if enabled[0].Name != "skill-b" {
		t.Errorf("expected 'skill-b' to be the remaining enabled skill, got %q", enabled[0].Name)
	}
}

func TestNewManager_EmptyWorkspace(t *testing.T) {
	workspace := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "skill-states.json")

	m, err := NewManager(testLogger(), workspace, statePath)
	if err != nil {
		t.Fatalf("NewManager with empty workspace returned error: %v", err)
	}

	if m.Count() != 0 {
		t.Errorf("expected 0 skills, got %d", m.Count())
	}
	if len(m.Skills()) != 0 {
		t.Errorf("expected empty Skills(), got %d", len(m.Skills()))
	}
	if len(m.EnabledSkills()) != 0 {
		t.Errorf("expected empty EnabledSkills(), got %d", len(m.EnabledSkills()))
	}
}

func TestSkills_ReturnsCopy(t *testing.T) {
	workspace := t.TempDir()
	createSkillFile(t, workspace, "immutable", "---\nname: immutable\n---\nContent\n")

	m, err := NewManager(testLogger(), workspace, "")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Get a copy and mutate it.
	skills := m.Skills()
	skills[0].Name = "mutated"
	skills[0].Enabled = false

	// The manager's internal state should be unchanged.
	original := m.Skills()
	if original[0].Name != "immutable" {
		t.Errorf("mutating Skills() return value should not affect manager; got name %q", original[0].Name)
	}
	if !original[0].Enabled {
		t.Error("mutating Skills() return value should not affect manager; Enabled was changed")
	}
}

func TestNewManager_IgnoresCorruptStateFile(t *testing.T) {
	workspace := t.TempDir()
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, "skill-states.json")

	createSkillFile(t, workspace, "resilient", "---\nname: resilient\n---\nContent\n")

	// Write invalid JSON to the state file.
	if err := os.WriteFile(statePath, []byte("not valid json{{{"), 0600); err != nil {
		t.Fatalf("failed to write corrupt state file: %v", err)
	}

	// NewManager should succeed, treating corrupt state as no state.
	m, err := NewManager(testLogger(), workspace, statePath)
	if err != nil {
		t.Fatalf("NewManager should not fail with corrupt state file: %v", err)
	}

	// Skill should be enabled by default since state could not be parsed.
	skills := m.Skills()
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}
	if !skills[0].Enabled {
		t.Error("skill should be enabled when state file is corrupt")
	}
}

func TestWatch(t *testing.T) {
	workspace := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "skill-states.json")

	// Start with one skill so we can verify Watch detects a new one.
	createSkillFile(t, workspace, "initial", "---\nname: initial\ndescription: Initial skill\n---\nInitial content\n")

	m, err := NewManager(testLogger(), workspace, statePath)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if m.Count() != 1 {
		t.Fatalf("expected 1 skill initially, got %d", m.Count())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Channel to capture the Watch return value.
	watchErr := make(chan error, 1)
	go func() {
		watchErr <- m.Watch(ctx, 50*time.Millisecond)
	}()

	// Give the watcher time to set up.
	time.Sleep(100 * time.Millisecond)

	// Create a new skill on disk; Watch should detect the new directory and file.
	createSkillFile(t, workspace, "hotloaded", "---\nname: hotloaded\ndescription: Hot-loaded skill\n---\nHot content\n")

	// Wait for the debounce (50ms) plus some margin for the reload.
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	detected := false
	for !detected {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for Watch to detect the new skill")
		case <-ticker.C:
			if m.Count() == 2 {
				detected = true
			}
		}
	}

	// Verify the new skill is present with correct metadata.
	skills := m.Skills()
	byName := make(map[string]Skill, len(skills))
	for _, s := range skills {
		byName[s.Name] = s
	}

	if _, ok := byName["initial"]; !ok {
		t.Error("missing 'initial' skill after Watch reload")
	}
	if s, ok := byName["hotloaded"]; !ok {
		t.Error("missing 'hotloaded' skill after Watch reload")
	} else {
		if s.Description != "Hot-loaded skill" {
			t.Errorf("hotloaded description: got %q, want %q", s.Description, "Hot-loaded skill")
		}
		if s.Content != "Hot content" {
			t.Errorf("hotloaded content: got %q, want %q", s.Content, "Hot content")
		}
		if !s.Enabled {
			t.Error("hotloaded should be enabled by default")
		}
	}

	// Cancel the context to stop Watch.
	cancel()

	select {
	case err := <-watchErr:
		if err != nil {
			t.Errorf("Watch returned non-nil error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Watch to return after context cancellation")
	}
}
