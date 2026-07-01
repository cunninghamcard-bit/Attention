package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type Scope string

const (
	ScopeGlobal  Scope = "global"
	ScopeProject Scope = "project"
)

// SettingsError records a settings load or persist failure for one scope.
//
// pi: .agents/references/pi/packages/coding-agent/src/core/settings-manager.ts:236-239.
type SettingsError struct {
	Scope Scope
	Err   error
}

type Manager struct {
	mu sync.Mutex

	globalPath  string
	projectPath string

	global  Settings
	project Settings
	merged  Settings

	// Per-scope load failures; a failed scope keeps empty settings and is
	// never persisted over (pi settings-manager.ts:531,548).
	globalLoadErr  error
	projectLoadErr error
	errors         []SettingsError
}

// NewManager loads both settings scopes. A corrupt or unreadable file
// degrades that scope to empty settings with a recorded error instead of
// failing, matching pi's tryLoadFromStorage (settings-manager.ts:326-335).
func NewManager(agentDir string, cwd string) (*Manager, error) {
	globalPath := filepath.Join(agentDir, settingsFile)
	projectPath := filepath.Join(cwd, ConfigDirName, settingsFile)

	m := &Manager{
		globalPath:  globalPath,
		projectPath: projectPath,
	}
	m.global, m.globalLoadErr = readSettings(globalPath)
	m.project, m.projectLoadErr = readSettings(projectPath)
	if m.globalLoadErr != nil {
		m.errors = append(m.errors, SettingsError{Scope: ScopeGlobal, Err: m.globalLoadErr})
	}
	if m.projectLoadErr != nil {
		m.errors = append(m.errors, SettingsError{Scope: ScopeProject, Err: m.projectLoadErr})
	}
	m.merged = mergeSettings(m.global, m.project)
	return m, nil
}

func (m *Manager) Settings() Settings {
	m.mu.Lock()
	defer m.mu.Unlock()

	return cloneSettings(m.merged)
}

func (m *Manager) ScopeSettings(scope Scope) (Settings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch scope {
	case ScopeGlobal:
		return cloneSettings(m.global), nil
	case ScopeProject:
		return cloneSettings(m.project), nil
	default:
		return nil, fmt.Errorf("unknown settings scope %q", scope)
	}
}

// Reload re-reads both scopes. A scope that fails to parse keeps its previous
// in-memory settings and records the error, matching pi's reload
// (settings-manager.ts:407-432).
func (m *Manager) Reload() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if global, err := readSettings(m.globalPath); err != nil {
		m.globalLoadErr = err
		m.errors = append(m.errors, SettingsError{Scope: ScopeGlobal, Err: err})
	} else {
		m.global = global
		m.globalLoadErr = nil
	}
	if project, err := readSettings(m.projectPath); err != nil {
		m.projectLoadErr = err
		m.errors = append(m.errors, SettingsError{Scope: ScopeProject, Err: err})
	} else {
		m.project = project
		m.projectLoadErr = nil
	}

	m.rebuildMerged()
	return nil
}

// DrainErrors returns and clears recorded settings errors.
//
// pi: .agents/references/pi/packages/coding-agent/src/core/settings-manager.ts:564-568.
func (m *Manager) DrainErrors() []SettingsError {
	m.mu.Lock()
	defer m.mu.Unlock()

	drained := m.errors
	m.errors = nil
	return drained
}

func (m *Manager) Set(scope Scope, path []string, value any) error {
	if len(path) == 0 {
		return errors.New("settings path is empty")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	settingsPath, err := m.pathForScope(scope)
	if err != nil {
		return err
	}

	// pi's setValue applies the change in memory immediately; persistence
	// runs separately and is skipped while the scope's file failed to load,
	// so a corrupt file is never clobbered (settings-manager.ts:531,548).
	// Persist failures are recorded, not returned (settings-manager.ts:483-486).
	setNestedSetting(m.scopeSettingsLocked(scope), path, cloneValue(value))
	m.rebuildMerged()

	loadErr := m.globalLoadErr
	if scope == ScopeProject {
		loadErr = m.projectLoadErr
	}
	if loadErr != nil {
		return nil
	}

	settings, err := readSettings(settingsPath)
	if err != nil {
		m.errors = append(m.errors, SettingsError{Scope: scope, Err: err})
		return nil
	}
	setNestedSetting(settings, path, cloneValue(value))
	if err := writeSettings(settingsPath, settings); err != nil {
		m.errors = append(m.errors, SettingsError{Scope: scope, Err: err})
		return nil
	}

	switch scope {
	case ScopeGlobal:
		m.global = settings
	case ScopeProject:
		m.project = settings
	}
	m.rebuildMerged()
	return nil
}

func (m *Manager) scopeSettingsLocked(scope Scope) Settings {
	if scope == ScopeProject {
		if m.project == nil {
			m.project = Settings{}
		}
		return m.project
	}
	if m.global == nil {
		m.global = Settings{}
	}
	return m.global
}

func (m *Manager) pathForScope(scope Scope) (string, error) {
	switch scope {
	case ScopeGlobal:
		return m.globalPath, nil
	case ScopeProject:
		return m.projectPath, nil
	default:
		return "", fmt.Errorf("unknown settings scope %q", scope)
	}
}

func (m *Manager) rebuildMerged() {
	m.merged = mergeSettings(m.global, m.project)
}

func writeSettings(path string, settings Settings) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create settings directory %s: %w", filepath.Dir(path), err)
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings %s: %w", path, err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write settings %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod settings %s: %w", path, err)
	}
	return nil
}

func setNestedSetting(settings Settings, path []string, value any) {
	current := map[string]any(settings)
	for _, key := range path[:len(path)-1] {
		next, ok := asObject(current[key])
		if !ok {
			next = map[string]any{}
			current[key] = next
		}
		current = next
	}
	current[path[len(path)-1]] = value
}

func cloneSettings(settings Settings) Settings {
	if settings == nil {
		return Settings{}
	}

	cloned := make(Settings, len(settings))
	for key, value := range settings {
		cloned[key] = cloneValue(value)
	}
	return cloned
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case Settings:
		return cloneSettings(typed)
	case map[string]any:
		cloned := make(map[string]any, len(typed))
		for key, value := range typed {
			cloned[key] = cloneValue(value)
		}
		return cloned
	case []any:
		cloned := make([]any, len(typed))
		for i, value := range typed {
			cloned[i] = cloneValue(value)
		}
		return cloned
	default:
		return typed
	}
}
