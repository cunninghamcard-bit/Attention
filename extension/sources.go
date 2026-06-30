package extension

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

func Load(settings config.Settings, agentDir string, cwd string) Result {
	names := settingsStringSlice(settings, "plugins")
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
	root, err := pluginRoot(name, agentDir)
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

	binDirs := existingDirs(filepath.Join(root, "bin"))
	env := pluginEnv(root, cwd, binDirs)
	hooks, hookDiagnostics := loadPluginHooks(root, env)
	skillDirs := existingDirs(filepath.Join(root, "skills"))
	commandDirs := existingDirs(filepath.Join(root, "commands"))
	diagnostics := hookDiagnostics

	source := internalextension.Source{
		Path: "plugin:" + manifest.Name,
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

func pluginRoot(name string, agentDir string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("plugin name is empty")
	}
	if strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "~") {
		return "", fmt.Errorf("plugin name %q must be a name under the plugins directory", name)
	}
	if agentDir == "" {
		return "", fmt.Errorf("agent dir is empty")
	}
	return filepath.Join(agentDir, "plugins", name), nil
}

func readManifest(root string) (pluginManifest, error) {
	path := filepath.Join(root, ".claude-plugin", "plugin.json")
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

func loadPluginHooks(root string, env map[string]string) (*hook.ShellHooksRunner, []resource.ResourceDiagnostic) {
	path := filepath.Join(root, "hooks", "hooks.json")
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
		"ATTENTION_PLUGIN_ROOT": root,
		"ATTENTION_PROJECT_DIR": cwd,
		"CLAUDE_PLUGIN_ROOT":    root,
		"CLAUDE_PROJECT_DIR":    cwd,
	}
	if len(binDirs) > 0 {
		pathParts := append([]string(nil), binDirs...)
		if current := os.Getenv("PATH"); current != "" {
			pathParts = append(pathParts, current)
		}
		env["PATH"] = strings.Join(pathParts, string(os.PathListSeparator))
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
