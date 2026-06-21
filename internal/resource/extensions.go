package resource

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cunninghamcard-bit/Attention/internal/config"
)

// DiscoverJSExtensions resolves configured and conventional TypeScript
// extension files. If Bun is unavailable, it skips JS extensions like the CLI
// startup path does.
func DiscoverJSExtensions(
	agentDir string,
	cwd string,
	configuredPaths []string,
	stderr io.Writer,
) []string {
	paths := JSExtensionCandidates(agentDir, cwd, configuredPaths)
	if len(paths) == 0 {
		return nil
	}
	if _, err := exec.LookPath("bun"); err != nil {
		if stderr != nil {
			fmt.Fprintf(
				stderr,
				"load JS extensions: bun not found, skipping %d extension(s): %v\n",
				len(paths),
				err,
			)
		}
		return nil
	}
	return paths
}

// JSExtensionCandidates returns extension files from the global/project
// extension directories plus explicitly configured files or directories.
func JSExtensionCandidates(agentDir string, cwd string, configuredPaths []string) []string {
	dirs := []string{}
	if strings.TrimSpace(agentDir) != "" {
		dirs = append(dirs, filepath.Join(agentDir, "extensions"))
	}
	if strings.TrimSpace(cwd) != "" {
		dirs = append(dirs, filepath.Join(cwd, config.ConfigDirName, "extensions"))
	}

	seen := map[string]struct{}{}
	paths := []string{}
	addPath := func(path string) {
		path = filepath.Clean(path)
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	addDir := func(dir string) {
		matches, err := filepath.Glob(filepath.Join(dir, "*.ts"))
		if err != nil {
			return
		}
		sort.Strings(matches)
		for _, match := range matches {
			addPath(match)
		}
	}
	for _, dir := range dirs {
		addDir(dir)
	}
	for _, rawPath := range configuredPaths {
		addConfiguredJSExtensionPath(rawPath, cwd, addPath, addDir)
	}
	return paths
}

func addConfiguredJSExtensionPath(
	rawPath string,
	cwd string,
	addPath func(string),
	addDir func(string),
) {
	resolvedPath, ok := configuredPath(rawPath, cwd)
	if !ok {
		return
	}
	info, err := os.Stat(resolvedPath)
	if err != nil {
		return
	}
	if info.IsDir() {
		addDir(resolvedPath)
		return
	}
	if info.Mode().IsRegular() {
		addPath(resolvedPath)
	}
}

func configuredPath(rawPath string, cwd string) (string, bool) {
	path := strings.TrimSpace(rawPath)
	if path == "" {
		return "", false
	}
	path = expandHome(path)
	if filepath.IsAbs(path) || strings.TrimSpace(cwd) == "" {
		return filepath.Clean(path), true
	}
	return filepath.Clean(filepath.Join(cwd, path)), true
}

func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}
