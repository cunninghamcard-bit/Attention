package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cunninghamcard-bit/Attention/internal/config"
	internalextension "github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
	"github.com/cunninghamcard-bit/Attention/internal/resource"
)

type Result struct {
	Sources     []internalextension.Source
	BinDirs     []string
	Diagnostics []resource.ResourceDiagnostic
}

type pluginManifest struct {
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
}

const (
	settingsPluginsKey = "plugins"
	userPluginDirName  = "plugins"
	manifestDir        = ".attention-plugin"
	manifestFileName   = "plugin.json"
	sourcePathPrefix   = "plugin:"
	binDirName         = "bin"
	hooksDirName       = "hooks"
	hooksFileName      = "hooks.json"
	skillsDirName      = "skills"
	commandsDirName    = "commands"

	pluginRootEnv = "ATTENTION_PLUGIN_ROOT"
	projectDirEnv = "ATTENTION_PROJECT_DIR"
	pathEnv       = "PATH"
)

func Load(settings config.Settings, agentDir string, cwd string) Result {
	names := settingsStringSlice(settings, settingsPluginsKey)
	result := Result{
		Sources:     []internalextension.Source{},
		BinDirs:     []string{},
		Diagnostics: []resource.ResourceDiagnostic{},
	}
	for _, name := range names {
		loaded := loadOne(name, agentDir, cwd)
		result.Sources = append(result.Sources, loaded.Sources...)
		result.BinDirs = append(result.BinDirs, loaded.BinDirs...)
		result.Diagnostics = append(result.Diagnostics, loaded.Diagnostics...)
	}
	return result
}

func loadOne(name string, agentDir string, cwd string) Result {
	root, err := pluginRoot(name, agentDir, cwd)
	if err != nil {
		return pluginError(name, err)
	}
	manifest, err := readManifest(root)
	if err != nil {
		return pluginError(root, err)
	}
	if manifest.Name == "" {
		manifest.Name = filepath.Base(root)
	}

	binDirs := existingDirs(filepath.Join(root, binDirName))
	env := pluginEnv(root, cwd, binDirs)
	hooks, hookDiagnostics := loadPluginHooks(root, env)
	skillDirs := existingDirs(filepath.Join(root, skillsDirName))
	commandDirs := existingDirs(filepath.Join(root, commandsDirName))
	diagnostics := hookDiagnostics

	source := internalextension.Source{
		Path: sourcePathPrefix + manifest.Name,
		Factory: func(api internalextension.ExtensionAPI) error {
			if hooks != nil {
				for _, handler := range hooks.Handlers() {
					handler := handler
					api.On(handler.EventType, func(ctx context.Context, event any, extCtx internalextension.ExtensionContext) (any, error) {
						return handler.Handle(ctx, event, extCtx.SessionID)
					})
				}
			}
			if len(skillDirs) > 0 || len(commandDirs) > 0 {
				api.On(hook.EventResourcesDiscover, func(context.Context, any, internalextension.ExtensionContext) (any, error) {
					return hook.ResourcesDiscoverResult{
						SkillPaths:  append([]string(nil), skillDirs...),
						PromptPaths: append([]string(nil), commandDirs...),
					}, nil
				})
			}
			return nil
		},
	}
	return Result{
		Sources:     []internalextension.Source{source},
		BinDirs:     binDirs,
		Diagnostics: diagnostics,
	}
}

func pluginRoot(name string, agentDir string, cwd string) (string, error) {
	if err := validatePluginName(name); err != nil {
		return "", err
	}
	if agentDir == "" {
		return "", fmt.Errorf("agent dir is empty")
	}
	for _, dir := range pluginSearchDirs(agentDir, cwd) {
		root := filepath.Join(dir, name)
		if manifestExists(root) {
			return root, nil
		}
	}
	return filepath.Join(globalPluginDir(agentDir), name), nil
}

func validatePluginName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("plugin name is empty")
	}
	if strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "~") {
		return fmt.Errorf("plugin name %q must be a name under a plugins directory", name)
	}
	return nil
}

func pluginSearchDirs(agentDir string, cwd string) []string {
	dirs := []string{}
	if strings.TrimSpace(cwd) != "" {
		dirs = append(dirs, filepath.Join(cwd, config.ConfigDirName, userPluginDirName))
	}
	dirs = append(dirs, globalPluginDir(agentDir))
	return uniqueCleanDirs(dirs)
}

func globalPluginDir(agentDir string) string {
	if filepath.Base(agentDir) == config.AgentDirName {
		return filepath.Join(filepath.Dir(agentDir), userPluginDirName)
	}
	return filepath.Join(agentDir, userPluginDirName)
}

func uniqueCleanDirs(dirs []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, dir := range dirs {
		clean := filepath.Clean(dir)
		if !seen[clean] {
			out = append(out, clean)
			seen[clean] = true
		}
	}
	return out
}

func readManifest(root string) (pluginManifest, error) {
	path := manifestPath(root, manifestDir)
	if _, err := os.Stat(path); err != nil {
		return pluginManifest{}, fmt.Errorf("read plugin manifest: no %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return pluginManifest{}, fmt.Errorf("read plugin manifest: %w", err)
	}
	var manifest pluginManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return pluginManifest{}, fmt.Errorf("parse plugin manifest: %w", err)
	}
	return manifest, nil
}

func manifestExists(root string) bool {
	path := manifestPath(root, manifestDir)
	_, err := os.Stat(path)
	return err == nil
}

func manifestPath(root string, dir string) string {
	return filepath.Join(root, dir, manifestFileName)
}

func loadPluginHooks(root string, env map[string]string) (*hook.ShellHooksRunner, []resource.ResourceDiagnostic) {
	path := filepath.Join(root, hooksDirName, hooksFileName)
	runner, err := hook.LoadShellHooksWithOptions(hook.ShellHooksOptions{
		Path:        path,
		CWD:         root,
		Env:         env,
		InputFormat: hook.ShellHookInputPlugin,
	})
	if err == nil {
		return runner, nil
	}
	return nil, []resource.ResourceDiagnostic{{
		Type:    resource.DiagnosticWarning,
		Message: err.Error(),
		Path:    path,
	}}
}

func pluginEnv(root string, cwd string, binDirs []string) map[string]string {
	env := map[string]string{
		pluginRootEnv: root,
		projectDirEnv: cwd,
	}
	if len(binDirs) > 0 {
		pathParts := append([]string(nil), binDirs...)
		if current := os.Getenv(pathEnv); current != "" {
			pathParts = append(pathParts, current)
		}
		env[pathEnv] = strings.Join(pathParts, string(os.PathListSeparator))
	}
	return env
}

func existingDirs(paths ...string) []string {
	out := []string{}
	for _, path := range paths {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			out = append(out, path)
		}
	}
	return out
}

func pluginError(path string, err error) Result {
	return Result{
		Diagnostics: []resource.ResourceDiagnostic{{
			Type:    resource.DiagnosticError,
			Message: err.Error(),
			Path:    path,
		}},
	}
}

func settingsStringSlice(settings config.Settings, key string) []string {
	if settings == nil {
		return []string{}
	}
	value, ok := settings[key]
	if !ok {
		return []string{}
	}
	switch items := value.(type) {
	case []string:
		return append([]string(nil), items...)
	case []any:
		out := make([]string, 0, len(items))
		for _, item := range items {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	default:
		return []string{}
	}
}
