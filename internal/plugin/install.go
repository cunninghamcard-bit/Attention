package plugin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cunninghamcard-bit/Attention/internal/config"
)

type InstallOptions struct {
	Source   string
	AgentDir string
	CWD      string
}

type InstallResult struct {
	Name      string
	Dir       string
	Installed bool
}

func Install(ctx context.Context, opts InstallOptions) (InstallResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	source := strings.TrimSpace(opts.Source)
	if source == "" {
		return InstallResult{}, fmt.Errorf("plugin source is empty")
	}
	if opts.AgentDir == "" {
		return InstallResult{}, fmt.Errorf("agent dir is empty")
	}

	pluginsDir := globalPluginDir(opts.AgentDir)
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		return InstallResult{}, fmt.Errorf("create plugin directory %s: %w", pluginsDir, err)
	}

	tmp, err := os.MkdirTemp(pluginsDir, ".install-")
	if err != nil {
		return InstallResult{}, fmt.Errorf("create temporary plugin directory: %w", err)
	}
	defer func() {
		if tmp != "" {
			_ = os.RemoveAll(tmp)
		}
	}()

	if local, ok := existingLocalPluginSource(source); ok {
		if err := copyDir(local, tmp); err != nil {
			return InstallResult{}, fmt.Errorf("copy plugin from %s: %w", local, err)
		}
	} else if err := clonePlugin(ctx, source, tmp); err != nil {
		return InstallResult{}, err
	}

	manifest, err := readManifest(tmp)
	if err != nil {
		return InstallResult{}, err
	}
	if err := validatePluginName(manifest.Name); err != nil {
		return InstallResult{}, err
	}

	dest := filepath.Join(pluginsDir, manifest.Name)
	if _, err := os.Stat(dest); err == nil {
		if _, err := readManifest(dest); err != nil {
			return InstallResult{}, fmt.Errorf("plugin destination %s already exists but is invalid: %w", dest, err)
		}
		if err := enablePlugin(opts.AgentDir, opts.CWD, manifest.Name); err != nil {
			return InstallResult{}, err
		}
		return InstallResult{Name: manifest.Name, Dir: dest}, nil
	} else if !os.IsNotExist(err) {
		return InstallResult{}, fmt.Errorf("check plugin destination %s: %w", dest, err)
	}

	if err := os.Rename(tmp, dest); err != nil {
		return InstallResult{}, fmt.Errorf("install plugin to %s: %w", dest, err)
	}
	tmp = ""

	if err := enablePlugin(opts.AgentDir, opts.CWD, manifest.Name); err != nil {
		return InstallResult{}, err
	}
	return InstallResult{Name: manifest.Name, Dir: dest, Installed: true}, nil
}

func existingLocalPluginSource(source string) (string, bool) {
	if strings.Contains(source, "://") || strings.HasPrefix(source, "git@") {
		return "", false
	}
	candidate := source
	if source == "~" || strings.HasPrefix(source, "~/") || strings.HasPrefix(source, `~\`) {
		expanded, err := config.ExpandTildePath(source)
		if err != nil {
			return "", false
		}
		candidate = expanded
	}
	if abs, err := filepath.Abs(candidate); err == nil {
		candidate = abs
	}
	info, err := os.Stat(candidate)
	return candidate, err == nil && info.IsDir()
}

func clonePlugin(ctx context.Context, source string, dest string) error {
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth=1", source, dest)
	cmd.Stdout = io.Discard
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return fmt.Errorf("git clone %s: %w: %s", source, err, detail)
		}
		return fmt.Errorf("git clone %s: %w", source, err)
	}
	return nil
}

func enablePlugin(agentDir string, cwd string, name string) error {
	manager, err := config.NewManager(agentDir, cwd)
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	for _, settingsErr := range manager.DrainErrors() {
		if settingsErr.Scope == config.ScopeGlobal {
			return fmt.Errorf("load global settings: %w", settingsErr.Err)
		}
	}

	global, err := manager.ScopeSettings(config.ScopeGlobal)
	if err != nil {
		return err
	}
	plugins := settingsStringSlice(global, settingsPluginsKey)
	if slices.Contains(plugins, name) {
		return nil
	}
	plugins = append(plugins, name)
	if err := manager.Set(config.ScopeGlobal, []string{settingsPluginsKey}, plugins); err != nil {
		return err
	}
	for _, settingsErr := range manager.DrainErrors() {
		if settingsErr.Scope == config.ScopeGlobal {
			return fmt.Errorf("write global settings: %w", settingsErr.Err)
		}
	}
	return nil
}

func copyDir(src string, dest string) error {
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return err
		}
		target := filepath.Join(dest, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if entry.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func copyFile(src string, dest string, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
