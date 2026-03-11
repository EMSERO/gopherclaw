package skills

import (
	"os"
	"path/filepath"
	"testing"
)

// helper creates a SKILL.md file inside workspace/skills/<dir>/SKILL.md.
func createSkillFile(t *testing.T, workspace, dir, content string) {
	t.Helper()
	skillDir := filepath.Join(workspace, "skills", dir)
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatalf("failed to create skill dir %s: %v", skillDir, err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0644); err != nil {
		t.Fatalf("failed to write SKILL.md: %v", err)
	}
}

func TestLoad_EmptyWorkspace(t *testing.T) {
	skills, err := Load("")
	if err != nil {
		t.Fatalf("Load('') returned error: %v", err)
	}
	if len(skills) != 0 {
		t.Errorf("expected 0 skills, got %d", len(skills))
	}
}

func TestLoad_NonexistentWorkspace(t *testing.T) {
	skills, err := Load("/nonexistent/workspace/path")
	if err != nil {
		t.Fatalf("Load(nonexistent) returned error: %v", err)
	}
	if len(skills) != 0 {
		t.Errorf("expected 0 skills, got %d", len(skills))
	}
}

func TestLoad_WorkspaceWithNoSkillsDir(t *testing.T) {
	dir := t.TempDir()
	skills, err := Load(dir)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(skills) != 0 {
		t.Errorf("expected 0 skills, got %d", len(skills))
	}
}

func TestLoad_SingleSkillWithFrontmatter(t *testing.T) {
	dir := t.TempDir()
	createSkillFile(t, dir, "greeter", "---\nname: greeter\ndescription: Greets people\n---\nHello, world!\n")

	skills, err := Load(dir)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}

	s := skills[0]
	if s.Name != "greeter" {
		t.Errorf("expected name 'greeter', got %q", s.Name)
	}
	if s.Description != "Greets people" {
		t.Errorf("expected description 'Greets people', got %q", s.Description)
	}
	if s.Content != "Hello, world!" {
		t.Errorf("expected content 'Hello, world!', got %q", s.Content)
	}
	expectedPath := filepath.Join(dir, "skills", "greeter")
	if s.Path != expectedPath {
		t.Errorf("expected path %q, got %q", expectedPath, s.Path)
	}
}

func TestLoad_SkillWithoutFrontmatter(t *testing.T) {
	dir := t.TempDir()
	createSkillFile(t, dir, "my-tool", "This skill has no frontmatter.\nJust plain content.\n")

	skills, err := Load(dir)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}

	s := skills[0]
	// Name should be inferred from directory name.
	if s.Name != "my-tool" {
		t.Errorf("expected name inferred as 'my-tool', got %q", s.Name)
	}
	if s.Description != "" {
		t.Errorf("expected empty description, got %q", s.Description)
	}
}

func TestLoad_SkillFrontmatterWithoutName(t *testing.T) {
	dir := t.TempDir()
	createSkillFile(t, dir, "auto-named", "---\ndescription: Has description but no name\n---\nBody text here.\n")

	skills, err := Load(dir)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}

	s := skills[0]
	// Name should fall back to directory name since frontmatter has no name.
	if s.Name != "auto-named" {
		t.Errorf("expected name 'auto-named', got %q", s.Name)
	}
	if s.Description != "Has description but no name" {
		t.Errorf("expected description 'Has description but no name', got %q", s.Description)
	}
	if s.Content != "Body text here." {
		t.Errorf("expected content 'Body text here.', got %q", s.Content)
	}
}

func TestLoad_MultipleSkills(t *testing.T) {
	dir := t.TempDir()
	createSkillFile(t, dir, "alpha", "---\nname: alpha\ndescription: First skill\n---\nAlpha content\n")
	createSkillFile(t, dir, "beta", "---\nname: beta\ndescription: Second skill\n---\nBeta content\n")
	createSkillFile(t, dir, "gamma", "No frontmatter, just content.\n")

	skills, err := Load(dir)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(skills) != 3 {
		t.Fatalf("expected 3 skills, got %d", len(skills))
	}

	// Build a map for easier assertions (order from Glob is not guaranteed
	// to be stable across platforms, though it typically is).
	byName := make(map[string]Skill, len(skills))
	for _, s := range skills {
		byName[s.Name] = s
	}

	if s, ok := byName["alpha"]; !ok {
		t.Error("missing skill 'alpha'")
	} else if s.Description != "First skill" {
		t.Errorf("alpha description: got %q", s.Description)
	}

	if s, ok := byName["beta"]; !ok {
		t.Error("missing skill 'beta'")
	} else if s.Description != "Second skill" {
		t.Errorf("beta description: got %q", s.Description)
	}

	if _, ok := byName["gamma"]; !ok {
		t.Error("missing skill 'gamma' (inferred name)")
	}
}

func TestLoad_SkillWithIncompleteFrontmatter(t *testing.T) {
	dir := t.TempDir()
	// Has opening --- but no closing ---, so no frontmatter is parsed.
	createSkillFile(t, dir, "incomplete", "---\nname: incomplete\nThis has no closing delimiter.\n")

	skills, err := Load(dir)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}

	s := skills[0]
	// Frontmatter is not closed, so name should be inferred from dir.
	if s.Name != "incomplete" {
		t.Errorf("expected name 'incomplete', got %q", s.Name)
	}
}

func TestLoad_SkillWithEmptyFrontmatter(t *testing.T) {
	dir := t.TempDir()
	createSkillFile(t, dir, "empty-fm", "---\n\n---\nContent after empty frontmatter.\n")

	skills, err := Load(dir)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}

	s := skills[0]
	// Name inferred from dir since frontmatter is empty.
	if s.Name != "empty-fm" {
		t.Errorf("expected name 'empty-fm', got %q", s.Name)
	}
	if s.Content != "Content after empty frontmatter." {
		t.Errorf("expected content 'Content after empty frontmatter.', got %q", s.Content)
	}
}

// --- LoadWorkspaceMDs tests ---

func TestLoadWorkspaceMDs_EmptyWorkspace(t *testing.T) {
	result := LoadWorkspaceMDs("")
	if len(result) != 0 {
		t.Errorf("expected empty map, got %d entries", len(result))
	}
}

func TestLoadWorkspaceMDs_NonexistentWorkspace(t *testing.T) {
	result := LoadWorkspaceMDs("/nonexistent/workspace/path")
	if len(result) != 0 {
		t.Errorf("expected empty map, got %d entries", len(result))
	}
}

func TestLoadWorkspaceMDs_NoMarkdownFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not markdown"), 0644)

	result := LoadWorkspaceMDs(dir)
	if len(result) != 0 {
		t.Errorf("expected empty map, got %d entries", len(result))
	}
}

func TestLoadWorkspaceMDs_ReturnsMarkdownFiles(t *testing.T) {
	dir := t.TempDir()

	files := map[string]string{
		"README.md":  "# README\n\nProject info.\n",
		"MEMORY.md":  "# Memory\n\nStuff to remember.\n",
		"NOTES.md":   "Some notes.\n",
		"ignore.txt": "not a markdown file",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatalf("failed to write %s: %v", name, err)
		}
	}

	result := LoadWorkspaceMDs(dir)

	if len(result) != 3 {
		t.Fatalf("expected 3 md files, got %d: %v", len(result), result)
	}

	for _, name := range []string{"README.md", "MEMORY.md", "NOTES.md"} {
		content, ok := result[name]
		if !ok {
			t.Errorf("missing %s in result", name)
			continue
		}
		if content != files[name] {
			t.Errorf("%s content mismatch: got %q, want %q", name, content, files[name])
		}
	}

	if _, ok := result["ignore.txt"]; ok {
		t.Error("ignore.txt should not be in result")
	}
}

func TestLoadWorkspaceMDs_IgnoresSubdirectoryMDs(t *testing.T) {
	dir := t.TempDir()

	// Only top-level .md files should be returned.
	os.WriteFile(filepath.Join(dir, "TOP.md"), []byte("top level"), 0644)
	subdir := filepath.Join(dir, "subdir")
	os.MkdirAll(subdir, 0755)
	os.WriteFile(filepath.Join(subdir, "NESTED.md"), []byte("nested"), 0644)

	result := LoadWorkspaceMDs(dir)
	if len(result) != 1 {
		t.Fatalf("expected 1 md file, got %d: %v", len(result), result)
	}
	if _, ok := result["TOP.md"]; !ok {
		t.Error("missing TOP.md in result")
	}
	if _, ok := result["NESTED.md"]; ok {
		t.Error("NESTED.md should not be returned (it's in a subdirectory)")
	}
}

// --- Origin field tests ---

func TestLoad_SkillWithOriginInFrontmatter(t *testing.T) {
	dir := t.TempDir()
	createSkillFile(t, dir, "marketplace-skill", "---\nname: marketplace-skill\ndescription: From the marketplace\norigin: marketplace\n---\nMarketplace content here.\n")

	skills, err := Load(dir)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}

	s := skills[0]
	if s.Name != "marketplace-skill" {
		t.Errorf("expected name %q, got %q", "marketplace-skill", s.Name)
	}
	if s.Origin != "marketplace" {
		t.Errorf("expected origin %q, got %q", "marketplace", s.Origin)
	}
	if s.Description != "From the marketplace" {
		t.Errorf("expected description %q, got %q", "From the marketplace", s.Description)
	}
}

func TestLoad_SkillWithCustomOrigin(t *testing.T) {
	dir := t.TempDir()
	createSkillFile(t, dir, "custom-origin", "---\nname: custom-origin\norigin: github.com/example/repo\n---\nCustom origin content.\n")

	skills, err := Load(dir)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}

	s := skills[0]
	if s.Origin != "github.com/example/repo" {
		t.Errorf("expected origin %q, got %q", "github.com/example/repo", s.Origin)
	}
}

func TestLoad_SkillWithoutOriginDefaultsToLocal(t *testing.T) {
	dir := t.TempDir()
	createSkillFile(t, dir, "local-skill", "---\nname: local-skill\ndescription: A local skill\n---\nLocal content.\n")

	skills, err := Load(dir)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}

	s := skills[0]
	if s.Origin != "local" {
		t.Errorf("expected origin %q (default), got %q", "local", s.Origin)
	}
}

func TestLoad_SkillNoFrontmatterDefaultsToLocal(t *testing.T) {
	dir := t.TempDir()
	createSkillFile(t, dir, "bare-skill", "No frontmatter at all.\n")

	skills, err := Load(dir)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}

	s := skills[0]
	if s.Origin != "local" {
		t.Errorf("expected origin %q (default), got %q", "local", s.Origin)
	}
}

func TestLoad_SkillEnabledByDefault(t *testing.T) {
	dir := t.TempDir()
	createSkillFile(t, dir, "enabled-check", "---\nname: enabled-check\norigin: marketplace\n---\nContent.\n")

	skills, err := Load(dir)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(skills))
	}
	if !skills[0].Enabled {
		t.Error("expected Enabled to be true by default")
	}
}
