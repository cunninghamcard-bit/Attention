package resource

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ContextFile is a project instruction file included in the system prompt.
type ContextFile struct {
	Path    string
	Content string
}

var contextFileNames = []string{"AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD"}

// LoadContextFiles returns global context first, then ancestor context files
// from outermost directory to cwd. Missing or unreadable files are skipped, with
// diagnostics for candidates that exist but cannot be read.
//
// pi: .agents/references/pi/packages/coding-agent/src/core/resource-loader.ts:57-112.
func LoadContextFiles(cwd string, agentDir string) ([]ContextFile, []ResourceDiagnostic) {
	contextFiles := []ContextFile{}
	diagnostics := []ResourceDiagnostic{}
	seenPaths := map[string]struct{}{}

	if strings.TrimSpace(agentDir) != "" {
		resolvedAgentDir, ok := resolveContextDir(agentDir, "")
		if !ok {
			resolvedAgentDir = ""
		}
		globalContext, loadDiagnostics, ok := loadContextFileFromDir(resolvedAgentDir)
		diagnostics = append(diagnostics, loadDiagnostics...)
		if ok {
			contextFiles = append(contextFiles, globalContext)
			seenPaths[globalContext.Path] = struct{}{}
		}
	}

	resolvedCWD, ok := resolveContextDir(cwd, "")
	if !ok {
		return contextFiles, diagnostics
	}

	ancestorContextFiles := []ContextFile{}
	for currentDir := resolvedCWD; ; currentDir = filepath.Dir(currentDir) {
		contextFile, loadDiagnostics, ok := loadContextFileFromDir(currentDir)
		diagnostics = append(diagnostics, loadDiagnostics...)
		if ok {
			if _, seen := seenPaths[contextFile.Path]; !seen {
				ancestorContextFiles = append(ancestorContextFiles, contextFile)
				seenPaths[contextFile.Path] = struct{}{}
			}
		}

		parentDir := filepath.Dir(currentDir)
		if parentDir == currentDir {
			break
		}
	}
	slices.Reverse(ancestorContextFiles)
	contextFiles = append(contextFiles, ancestorContextFiles...)
	return contextFiles, diagnostics
}

func loadContextFileFromDir(dir string) (ContextFile, []ResourceDiagnostic, bool) {
	diagnostics := []ResourceDiagnostic{}
	if dir == "" {
		return ContextFile{}, diagnostics, false
	}
	for _, name := range contextFileNames {
		path := filepath.Join(dir, name)
		content, err := os.ReadFile(path)
		if err != nil {
			if candidateExists(path) || !errors.Is(err, os.ErrNotExist) {
				diagnostics = append(diagnostics, ResourceDiagnostic{
					Type:    DiagnosticWarning,
					Message: "read context file: " + err.Error(),
					Path:    path,
				})
			}
			continue
		}
		absPath, err := filepath.Abs(path)
		if err != nil {
			absPath = filepath.Clean(path)
		}
		return ContextFile{
			Path:    filepath.Clean(absPath),
			Content: string(content),
		}, diagnostics, true
	}
	return ContextFile{}, diagnostics, false
}

func candidateExists(path string) bool {
	if _, err := os.Stat(path); err == nil {
		return true
	}
	if _, err := os.Lstat(path); err == nil {
		return true
	}
	return false
}

// resolveContextDir resolves a directory used for context-file lookup. Unlike the
// shared resolvePath, an empty path defaults to the working directory and failures
// are reported as a boolean rather than an error.
func resolveContextDir(path string, baseDir string) (string, bool) {
	if path == "" {
		if baseDir == "" {
			cwd, err := os.Getwd()
			if err != nil {
				return "", false
			}
			path = cwd
		} else {
			path = baseDir
		}
	}

	expanded := expandTilde(path)
	if baseDir != "" && !filepath.IsAbs(expanded) {
		expanded = filepath.Join(baseDir, expanded)
	}
	resolved, err := filepath.Abs(expanded)
	if err != nil {
		return "", false
	}
	return filepath.Clean(resolved), true
}
