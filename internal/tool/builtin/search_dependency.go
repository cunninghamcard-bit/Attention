package builtin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/config"
	"github.com/cunninghamcard-bit/Attention/internal/execenv"
	"github.com/cunninghamcard-bit/Attention/internal/tool"
)

const searchToolResolveTimeout = 5 * time.Second

var (
	searchToolBinDir            = defaultSearchToolBinDir
	managedSearchToolDownloader = downloadManagedSearchTool
)

type searchToolDependency struct {
	key         string
	displayName string
	binaryName  string
	candidates  []string
	repo        string
	tagPrefix   string
	assetName   func(version, goos, goarch string) string
	termuxPkg   string
}

// resolveSearchTool returns the command to invoke for a search dependency,
// mirroring pi's ensureTool which yields a path or fails. Presentation of the
// failure as a tool.Result is the caller's job (grep/find), so dependency
// resolution stays decoupled from tool-result formatting.
func resolveSearchTool(ctx context.Context, env execenv.ExecutionEnv, dependency searchToolDependency) (string, error) {
	if command := managedSearchToolPath(dependency); command != "" {
		return command, nil
	}

	for _, candidate := range dependency.candidates {
		lookupCommand, err := shellJoin([]string{"command", "-v", candidate})
		if err != nil {
			return "", err
		}
		result, err := env.Exec(ctx, lookupCommand, execenv.ExecOptions{
			Timeout: searchToolResolveTimeout,
		})
		if err != nil {
			return "", fmt.Errorf("Failed to locate %s: %v", dependency.displayName, err)
		}
		if result.ExitCode != 0 {
			continue
		}
		resolvedCommand := firstCommandLookupLine(result.Stdout)
		if resolvedCommand == "" {
			resolvedCommand = candidate
		}
		return resolvedCommand, nil
	}

	if offlineSearchToolDownloads() {
		return "", fmt.Errorf("%s is not available. Offline mode enabled, skipping download.", dependency.displayName)
	}
	if runtime.GOOS == "android" {
		pkgName := dependency.termuxPkg
		if pkgName == "" {
			pkgName = dependency.binaryName
		}
		return "", fmt.Errorf("%s is not available. Install with: pkg install %s", dependency.displayName, pkgName)
	}

	command, err := managedSearchToolDownloader(ctx, dependency)
	if err != nil {
		if errors.Is(err, errUnsupportedManagedSearchToolPlatform) {
			return "", fmt.Errorf("%s is not available and could not be downloaded", dependency.displayName)
		}
		return "", fmt.Errorf("%s is not available and could not be downloaded: %v", dependency.displayName, err)
	}
	return command, nil
}

func firstCommandLookupLine(stdout string) string {
	for rawLine := range strings.SplitSeq(stdout, "\n") {
		line := strings.TrimSpace(rawLine)
		if line != "" {
			return line
		}
	}
	return ""
}

// missingSearchToolResult is the tool.Result for the defensive post-exec path
// in grep/find, where the resolved command reports "not found" at run time.
func missingSearchToolResult(dependency searchToolDependency) tool.Result {
	return errorResult(
		"%s is not available and could not be downloaded",
		dependency.displayName,
	)
}

func commandNotFoundForAny(exitCode int, stderr string, commands ...string) bool {
	if exitCode == 127 {
		return true
	}
	for _, command := range commands {
		if isShellCommandNotFound(exitCode, stderr, command) {
			return true
		}
	}
	return false
}

func managedSearchToolPath(dependency searchToolDependency) string {
	binDir, err := searchToolBinDir()
	if err != nil {
		return ""
	}
	candidate := filepath.Join(binDir, binaryFileName(dependency.binaryName))
	info, err := os.Stat(candidate)
	if err != nil || info.IsDir() {
		return ""
	}
	return candidate
}

func defaultSearchToolBinDir() (string, error) {
	agentDir, err := config.AgentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(agentDir, "bin"), nil
}

func offlineSearchToolDownloads() bool {
	return truthyEnv("ALONG_OFFLINE") || truthyEnv("PI_OFFLINE")
}

func truthyEnv(key string) bool {
	value := os.Getenv(key)
	if value == "" {
		return false
	}
	value = strings.ToLower(value)
	return value == "1" || value == "true" || value == "yes"
}
