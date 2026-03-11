package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/fsnotify/fsnotify"
)

// skillStates maps skill name → enabled. Persisted to state/skill-states.json.
type skillStates map[string]bool

// Manager provides hot-reload and enable/disable toggles for skills.
type Manager struct {
	workspace string
	statePath string // e.g. ~/.gopherclaw/state/skill-states.json
	logger    *zap.SugaredLogger

	mu     sync.RWMutex
	skills []Skill
}

// NewManager creates a Manager, loads skills from workspace, and applies
// persisted enable/disable state.
func NewManager(logger *zap.SugaredLogger, workspace, statePath string) (*Manager, error) {
	m := &Manager{
		workspace: workspace,
		statePath: statePath,
		logger:    logger,
	}
	skills, err := Load(workspace)
	if err != nil {
		return nil, err
	}
	m.skills = skills
	m.applyStates()
	return m, nil
}

// Skills returns a copy of the current skill list.
func (m *Manager) Skills() []Skill {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Skill, len(m.skills))
	copy(out, m.skills)
	return out
}

// EnabledSkills returns only skills that are currently enabled.
func (m *Manager) EnabledSkills() []Skill {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Skill
	for _, s := range m.skills {
		if s.Enabled {
			out = append(out, s)
		}
	}
	return out
}

// SetEnabled enables or disables a skill by name and persists the state.
func (m *Manager) SetEnabled(name string, enabled bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	found := false
	for i := range m.skills {
		if m.skills[i].Name == name {
			m.skills[i].Enabled = enabled
			found = true
			break
		}
	}
	if found {
		m.persistStates()
	}
	return found
}

// Count returns the total number of loaded skills.
func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.skills)
}

// Reload re-reads skills from disk and applies persisted state.
func (m *Manager) Reload() error {
	skills, err := Load(m.workspace)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.skills = skills
	m.applyStates()
	m.mu.Unlock()
	return nil
}

// Watch watches the workspace/skills/ directory for changes and hot-reloads.
// Blocks until ctx is cancelled.
func (m *Manager) Watch(ctx context.Context, debounce time.Duration) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer func() { _ = watcher.Close() }()

	skillsDir := filepath.Join(m.workspace, "skills")
	if err := os.MkdirAll(skillsDir, 0755); err != nil {
		return err
	}
	if err := watcher.Add(skillsDir); err != nil {
		return err
	}

	// Also watch subdirectories for SKILL.md changes.
	entries, _ := os.ReadDir(skillsDir)
	for _, e := range entries {
		if e.IsDir() {
			_ = watcher.Add(filepath.Join(skillsDir, e.Name()))
		}
	}

	m.logger.Infof("skills: watching %s for changes", skillsDir)

	var timer *time.Timer

	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return nil

		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			if !event.Has(fsnotify.Write) && !event.Has(fsnotify.Create) && !event.Has(fsnotify.Remove) {
				continue
			}

			// Watch newly created subdirectories.
			if event.Has(fsnotify.Create) {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					_ = watcher.Add(event.Name)
				}
			}

			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(debounce, func() {
				m.logger.Infof("skills: change detected, reloading...")
				if err := m.Reload(); err != nil {
					m.logger.Errorf("skills: reload failed: %v", err)
					return
				}
				m.logger.Infof("skills: reloaded %d skills", m.Count())
			})

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			m.logger.Errorf("skills: watcher error: %v", err)
		}
	}
}

// applyStates reads persisted skill-states.json and applies enable/disable.
// Must be called with m.mu held (or during init before concurrent access).
func (m *Manager) applyStates() {
	states := m.loadStates()
	if len(states) == 0 {
		return
	}
	for i := range m.skills {
		if enabled, ok := states[m.skills[i].Name]; ok {
			m.skills[i].Enabled = enabled
		}
	}
}

// loadStates reads skill-states.json.
func (m *Manager) loadStates() skillStates {
	if m.statePath == "" {
		return nil
	}
	data, err := os.ReadFile(m.statePath)
	if err != nil {
		return nil
	}
	var states skillStates
	if err := json.Unmarshal(data, &states); err != nil {
		return nil
	}
	return states
}

// persistStates writes current enable/disable state to skill-states.json.
// Must be called with m.mu held.
func (m *Manager) persistStates() {
	if m.statePath == "" {
		return
	}
	states := make(skillStates, len(m.skills))
	for _, s := range m.skills {
		states[s.Name] = s.Enabled
	}
	data, err := json.MarshalIndent(states, "", "  ")
	if err != nil {
		m.logger.Errorf("skills: marshal states: %v", err)
		return
	}
	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(m.statePath), 0700); err != nil {
		m.logger.Errorf("skills: create state dir: %v", err)
		return
	}

	tmp := m.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		m.logger.Errorf("skills: write states: %v", err)
		return
	}
	if err := os.Rename(tmp, m.statePath); err != nil {
		m.logger.Errorf("skills: rename states: %v", err)
	}
}
