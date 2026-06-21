package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const enabledStateFile = "plugins-enabled.json"

var ErrPluginNotFound = errors.New("plugin not found")

type PluginInfo struct {
	ID        string   `json:"id"`
	Version   string   `json:"version,omitempty"`
	Enabled   bool     `json:"enabled"`
	HasApp    bool     `json:"hasApp"`
	Owners    []string `json:"owners,omitempty"`
	Dir       string   `json:"dir"`
	LoadError string   `json:"loadError,omitempty"`
}

type Registry struct {
	mu         sync.RWMutex
	bundledDir string
	agentDir   string
	plugins    map[string]PluginInfo
	enabled    map[string]bool
}

func NewRegistry(bundledDir string, agentDir string) (*Registry, error) {
	registry := &Registry{
		bundledDir: bundledDir,
		agentDir:   agentDir,
		plugins:    map[string]PluginInfo{},
		enabled:    map[string]bool{},
	}
	if err := registry.Reload(); err != nil {
		return nil, err
	}
	return registry, nil
}

func (r *Registry) Reload() error {
	enabled, err := r.loadEnabledState()
	if err != nil {
		return err
	}

	plugins := map[string]PluginInfo{}
	if err := r.scanSource(r.bundledDir, enabled, plugins); err != nil {
		return err
	}
	if err := r.scanSource(filepath.Join(r.agentDir, "plugins"), enabled, plugins); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.enabled = enabled
	r.plugins = plugins
	return nil
}

func (r *Registry) List() []PluginInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	infos := make([]PluginInfo, 0, len(r.plugins))
	for _, info := range r.plugins {
		infos = append(infos, clonePluginInfo(info))
	}
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].ID < infos[j].ID
	})
	return infos
}

func (r *Registry) Get(id string) (PluginInfo, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	info, ok := r.plugins[id]
	if !ok {
		return PluginInfo{}, false
	}
	return clonePluginInfo(info), true
}

func (r *Registry) ContributionPath(id, owner string) (string, bool) {
	info, ok := r.Get(id)
	if !ok || !info.Enabled || info.LoadError != "" {
		return "", false
	}

	manifest, err := readManifest(info.Dir)
	if err != nil {
		return "", false
	}
	contribution, ok := manifest.contribution(owner)
	if !ok {
		return "", false
	}
	path, err := filepath.Abs(filepath.Join(info.Dir, contribution))
	if err != nil {
		return "", false
	}
	return path, true
}

func (r *Registry) EnabledWithContribution(owner string) []PluginInfo {
	infos := r.List()
	out := make([]PluginInfo, 0, len(infos))
	for _, info := range infos {
		if _, ok := r.ContributionPath(info.ID, owner); ok {
			out = append(out, info)
		}
	}
	return out
}

func (r *Registry) SetEnabled(id string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	info, ok := r.plugins[id]
	if !ok {
		return ErrPluginNotFound
	}

	nextEnabled := cloneEnabledState(r.enabled)
	nextEnabled[id] = enabled
	if err := r.persistEnabledState(nextEnabled); err != nil {
		return err
	}

	r.enabled = nextEnabled
	info.Enabled = enabled
	r.plugins[id] = info
	return nil
}

func (r *Registry) scanSource(
	sourceDir string,
	enabled map[string]bool,
	plugins map[string]PluginInfo,
) error {
	if sourceDir == "" {
		return nil
	}

	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("plugin registry source %s: %w", sourceDir, err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		dir := filepath.Join(sourceDir, entry.Name())
		manifestPath := filepath.Join(dir, "manifest.json")
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			plugins[entry.Name()] = PluginInfo{
				ID:        entry.Name(),
				Enabled:   enabledValue(enabled, entry.Name()),
				Dir:       dir,
				LoadError: fmt.Sprintf("manifest.json: %v", err),
			}
			continue
		}

		manifest, err := ParseManifest(data)
		if err != nil {
			plugins[entry.Name()] = PluginInfo{
				ID:        entry.Name(),
				Enabled:   enabledValue(enabled, entry.Name()),
				Dir:       dir,
				LoadError: err.Error(),
			}
			continue
		}

		info := PluginInfo{
			ID:      manifest.ID,
			Version: manifest.Version,
			Enabled: enabledValue(enabled, manifest.ID),
			HasApp:  manifest.hasApp(),
			Owners:  manifest.owners(),
			Dir:     dir,
		}
		plugins[manifest.ID] = info
	}
	return nil
}

func (r *Registry) loadEnabledState() (map[string]bool, error) {
	path := r.enabledPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]bool{}, nil
		}
		return nil, fmt.Errorf("plugin enabled state %s: %w", path, err)
	}

	var enabled map[string]bool
	if err := json.Unmarshal(data, &enabled); err != nil {
		return nil, fmt.Errorf("plugin enabled state %s: %w", path, err)
	}
	if enabled == nil {
		enabled = map[string]bool{}
	}
	return enabled, nil
}

func (r *Registry) persistEnabledState(enabled map[string]bool) error {
	if err := os.MkdirAll(r.agentDir, 0o700); err != nil {
		return fmt.Errorf("plugin enabled state dir %s: %w", r.agentDir, err)
	}

	data, err := json.MarshalIndent(enabled, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	path := r.enabledPath()
	tmp, err := os.CreateTemp(r.agentDir, enabledStateFile+".*.tmp")
	if err != nil {
		return fmt.Errorf("plugin enabled state temp: %w", err)
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("plugin enabled state write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("plugin enabled state close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("plugin enabled state rename: %w", err)
	}
	removeTmp = false
	return nil
}

func (r *Registry) enabledPath() string {
	return filepath.Join(r.agentDir, enabledStateFile)
}

func readManifest(dir string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return Manifest{}, err
	}
	return ParseManifest(data)
}

func clonePluginInfo(info PluginInfo) PluginInfo {
	info.Owners = append([]string(nil), info.Owners...)
	return info
}

func cloneEnabledState(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for id, enabled := range in {
		out[id] = enabled
	}
	return out
}

func enabledValue(enabled map[string]bool, id string) bool {
	value, ok := enabled[id]
	if !ok {
		return true
	}
	return value
}

func (m Manifest) contribution(owner string) (string, bool) {
	if m.Contributions == nil {
		return "", false
	}
	var contribution string
	switch owner {
	case "session":
		contribution = m.Contributions.Session
	case "engine":
		contribution = m.Contributions.Engine
	case "environment":
		contribution = m.Contributions.Environment
	default:
		return "", false
	}
	contribution = strings.TrimSpace(contribution)
	if contribution == "" {
		return "", false
	}
	return contribution, true
}

func (m Manifest) hasApp() bool {
	return strings.TrimSpace(m.Main) != ""
}

func (m Manifest) owners() []string {
	if m.Contributions == nil {
		return nil
	}

	owners := []string{}
	if strings.TrimSpace(m.Contributions.Session) != "" {
		owners = append(owners, "session")
	}
	if strings.TrimSpace(m.Contributions.Engine) != "" {
		owners = append(owners, "engine")
	}
	if strings.TrimSpace(m.Contributions.Environment) != "" {
		owners = append(owners, "environment")
	}
	return owners
}
