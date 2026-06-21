package resource

import (
	"os"
	"path/filepath"
	"strings"
)

// resolvePath makes path absolute, expanding a leading "~" and resolving a
// relative path against baseDir (when baseDir is set). Shared by the skill and
// prompt-template loaders, which previously each carried an identical copy.
func resolvePath(path string, baseDir string) (string, error) {
	expanded := expandTilde(path)
	if baseDir == "" {
		return filepath.Abs(expanded)
	}
	if filepath.IsAbs(expanded) {
		return filepath.Abs(expanded)
	}
	return filepath.Abs(filepath.Join(baseDir, expanded))
}

// expandTilde expands a leading "~" or "~/" to the user's home directory.
func expandTilde(path string) string {
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
