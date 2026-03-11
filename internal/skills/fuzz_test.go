package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func FuzzParseSkill(f *testing.F) {
	// Valid SKILL.md with full frontmatter.
	f.Add([]byte("---\nname: greeter\ndescription: Greets people\n---\nHello, world!\n"))

	// Valid frontmatter with origin field.
	f.Add([]byte("---\nname: marketplace-skill\ndescription: From marketplace\norigin: marketplace\n---\nContent here.\n"))

	// Frontmatter with empty name (falls back to dir name).
	f.Add([]byte("---\ndescription: Has description but no name\n---\nBody text here.\n"))

	// Empty frontmatter block.
	f.Add([]byte("---\n\n---\nContent after empty frontmatter.\n"))

	// Missing closing delimiter — frontmatter not parsed.
	f.Add([]byte("---\nname: incomplete\nThis has no closing delimiter.\n"))

	// No frontmatter at all.
	f.Add([]byte("This skill has no frontmatter.\nJust plain content.\n"))

	// Empty content.
	f.Add([]byte(""))

	// Only the opening delimiter.
	f.Add([]byte("---\n"))

	// Only delimiters, no body.
	f.Add([]byte("---\nname: x\n---\n"))

	// Binary garbage.
	f.Add([]byte{0x00, 0x01, 0xFF, 0xFE, 0x80, 0x7F})

	// Invalid YAML in frontmatter.
	f.Add([]byte("---\n: : :\n[[[invalid yaml\n---\nstill has body\n"))

	// Frontmatter with extra fields.
	f.Add([]byte("---\nname: test\ndescription: desc\norigin: custom\nextra_field: value\nnested:\n  key: val\n---\nbody\n"))

	// Unicode content.
	f.Add([]byte("---\nname: unicode-test\ndescription: \xe4\xb8\xad\xe6\x96\x87\xe6\x8f\x8f\xe8\xbf\xb0\n---\n\xf0\x9f\x91\x8b Hello\n"))

	// Very long single line.
	long := make([]byte, 0, 10000)
	long = append(long, []byte("---\nname: ")...)
	for i := 0; i < 9900; i++ {
		long = append(long, 'A')
	}
	long = append(long, []byte("\n---\nbody\n")...)
	f.Add(long)

	f.Fuzz(func(t *testing.T, data []byte) {
		// Write the fuzzed data to a temp file and parse it.
		dir := t.TempDir()
		skillDir := filepath.Join(dir, "skills", "fuzz-skill")
		if err := os.MkdirAll(skillDir, 0755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(skillDir, "SKILL.md")
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}

		s, err := parseSkill(path)
		if err != nil {
			// parseSkill only errors on file-read failure, which shouldn't
			// happen here. Any other error is unexpected.
			t.Fatalf("parseSkill returned unexpected error: %v", err)
		}

		// Invariants that must always hold:
		if s.Name == "" {
			t.Fatal("Name must never be empty (should fall back to dir name)")
		}
		if s.Origin == "" {
			t.Fatal("Origin must never be empty (should default to 'local')")
		}
		if !s.Enabled {
			t.Fatal("Enabled must be true by default")
		}
		if s.Path != skillDir {
			t.Fatalf("Path = %q, want %q", s.Path, skillDir)
		}
	})
}
