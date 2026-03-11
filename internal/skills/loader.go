package skills

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Skill holds a parsed SKILL.md.
type Skill struct {
	Name        string
	Description string
	Content     string // full SKILL.md text (after frontmatter)
	Path        string // directory of the SKILL.md
	Origin      string // "local", "marketplace", or custom origin string
	Enabled     bool   // whether the skill is active (default true)
	Verified    bool   // true if installed via CrawHub; false for manual drop-ins (REQ-102)
}

type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Origin      string `yaml:"origin"` // optional origin tracking
}

// Load discovers all SKILL.md files under the given workspace directory.
func Load(workspace string) ([]Skill, error) {
	pattern := filepath.Join(workspace, "skills", "*", "SKILL.md")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}

	var skills []Skill
	for _, path := range matches {
		s, err := parseSkill(path)
		if err != nil {
			continue
		}
		skills = append(skills, s)
	}
	return skills, nil
}

func parseSkill(path string) (Skill, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, err
	}

	content := string(data)
	var fm frontmatter

	// Parse YAML frontmatter between --- delimiters
	if strings.HasPrefix(content, "---\n") {
		if yamlPart, body, ok := strings.Cut(content[4:], "\n---\n"); ok {
			_ = yaml.Unmarshal([]byte(yamlPart), &fm)
			content = strings.TrimSpace(body)
		}
	}

	// Fallback: infer name from directory
	if fm.Name == "" {
		fm.Name = filepath.Base(filepath.Dir(path))
	}

	origin := fm.Origin
	if origin == "" {
		origin = "local"
	}

	// Skills installed via CrawHub have a _meta.json or origin != "local"
	verified := origin != "local"
	if !verified {
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), "_meta.json")); err == nil {
			verified = true
		}
	}

	return Skill{
		Name:        fm.Name,
		Description: fm.Description,
		Content:     content,
		Path:        filepath.Dir(path),
		Origin:      origin,
		Enabled:     true, // default enabled; Manager applies persisted state
		Verified:    verified,
	}, nil
}

// LoadWorkspaceMDs reads all *.md files directly in the workspace directory.
func LoadWorkspaceMDs(workspace string) map[string]string {
	pattern := filepath.Join(workspace, "*.md")
	matches, _ := filepath.Glob(pattern)
	out := make(map[string]string, len(matches))
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err == nil {
			out[filepath.Base(path)] = string(data)
		}
	}
	return out
}
