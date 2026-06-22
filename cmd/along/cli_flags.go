package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/cunninghamcard-bit/Attention/internal/agentloop"
	"github.com/cunninghamcard-bit/Attention/internal/execenv"
	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/tool/builtin"
)

// VERSION is the along kernel version string reported by --version. It falls
// back to the module version embedded by `go build` when available.
const VERSION = "0.1.0-dev"

// repeatableFlag is a flag.Value that accumulates one entry per occurrence,
// mirroring pi's repeatable flags (--append-system-prompt, --skill,
// --prompt-template). The zero value is an empty, ready-to-use slice.
type repeatableFlag []string

func (r *repeatableFlag) String() string {
	if r == nil {
		return ""
	}
	return strings.Join(*r, ",")
}

func (r *repeatableFlag) Set(value string) error {
	*r = append(*r, value)
	return nil
}

// toolSelection holds the parsed tool-related flags. Precedence (highest first):
// noTools > noBuiltinTools > (tools allowlist | default base) then excludeTools
// is applied last to whatever base survived.
type toolSelection struct {
	tools         []string // --tools / -t allowlist (nil => no allowlist)
	excludeTools  []string // --exclude-tools / -xt denylist
	noTools       bool     // --no-tools / -nt
	noBuiltinTool bool     // --no-builtin-tools / -nbt
}

// selectTools resolves the active tool set from the selection flags.
//
//   - base is the default tool set used when no --tools allowlist is given
//     (NewCodingTools in main).
//   - all is the full built-in set (NewAllTools) that --tools selects from, so
//     read-only tools like grep/find/ls are reachable via the allowlist.
//
// Precedence: --no-tools clears everything; --no-builtin-tools drops the
// built-in base (no extension tools exist today, so this is also empty);
// otherwise the base is either the --tools allowlist filtered from `all` or the
// default base; finally --exclude-tools removes named tools from that result.
// An unknown tool name in --tools is an error.
func selectTools(
	sel toolSelection,
	base []extension.ToolDefinition,
	all []extension.ToolDefinition,
) ([]extension.ToolDefinition, error) {
	if sel.noTools {
		return []extension.ToolDefinition{}, nil
	}
	if sel.noBuiltinTool {
		// All tools today are built-in; dropping built-ins leaves nothing.
		return []extension.ToolDefinition{}, nil
	}

	result := base
	if len(sel.tools) > 0 {
		byName := make(map[string]extension.ToolDefinition, len(all))
		for _, def := range all {
			byName[def.Name] = def
		}
		allowed := make([]extension.ToolDefinition, 0, len(sel.tools))
		for _, name := range sel.tools {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			def, ok := byName[name]
			if !ok {
				return nil, fmt.Errorf("unknown tool %q (available: %s)", name, strings.Join(toolNames(all), ", "))
			}
			allowed = append(allowed, def)
		}
		result = allowed
	}

	if len(sel.excludeTools) > 0 {
		excluded := make(map[string]struct{}, len(sel.excludeTools))
		for _, name := range sel.excludeTools {
			name = strings.TrimSpace(name)
			if name != "" {
				excluded[name] = struct{}{}
			}
		}
		filtered := make([]extension.ToolDefinition, 0, len(result))
		for _, def := range result {
			if _, drop := excluded[def.Name]; drop {
				continue
			}
			filtered = append(filtered, def)
		}
		result = filtered
	}

	return result, nil
}

func toolNames(defs []extension.ToolDefinition) []string {
	names := make([]string, 0, len(defs))
	for _, def := range defs {
		names = append(names, def.Name)
	}
	sort.Strings(names)
	return names
}

// splitCommaList parses a comma-separated flag value into trimmed, non-empty
// entries, mirroring pi's args.ts handling of --tools/--exclude-tools.
func splitCommaList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// validThinkingLevels are the levels the along kernel supports. pi accepts a
// wider set (off/minimal/xhigh) but along's agentloop only defines low/medium/
// high, so the kernel validates against what it can honor.
var validThinkingLevels = []string{"low", "medium", "high"}

// validateThinkingLevel checks a --thinking value against the supported levels.
func validateThinkingLevel(level string) (agentloop.ThinkingLevel, error) {
	for _, valid := range validThinkingLevels {
		if level == valid {
			return agentloop.ThinkingLevel(level), nil
		}
	}
	return "", fmt.Errorf("invalid thinking level %q (want %s)", level, strings.Join(validThinkingLevels, ", "))
}

// resolveThinkingLevel maps the --thinking flag value to a ThinkingLevel. An
// empty value means "not set" and resolves to the empty level, which lets the
// orchestrator fall back to its settings/default. A non-empty invalid value is
// an error.
func resolveThinkingLevel(value string) (agentloop.ThinkingLevel, error) {
	if value == "" {
		return "", nil
	}
	return validateThinkingLevel(value)
}

// validateName rejects an explicitly-set but empty --name value, mirroring pi's
// "--name requires a value" diagnostic. It uses fs.Visit so that simply omitting
// --name is fine; only `--name ""` is an error.
func validateName(value string, fs flagVisiter) error {
	if strings.TrimSpace(value) != "" {
		return nil
	}
	var wasSet bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "name" || f.Name == "n" {
			wasSet = true
		}
	})
	if wasSet {
		return fmt.Errorf("--name requires a non-empty value")
	}
	return nil
}

// flagVisiter is the subset of *flag.FlagSet used by validateName, kept narrow
// so the check is unit-testable without a full FlagSet.
type flagVisiter interface {
	Visit(func(*flag.Flag))
}

// resolveSessionDir applies the --session-dir precedence: an explicit flag value
// (with ~ expansion) wins; otherwise the default root is used. The default
// already honors the ALONG_CODING_AGENT_SESSION_DIR env var via config.SessionDir.
func resolveSessionDir(flagValue, defaultRoot string) (string, error) {
	if strings.TrimSpace(flagValue) == "" {
		return defaultRoot, nil
	}
	return expandTilde(flagValue)
}

// expandTilde expands a leading "~" / "~/" to the user's home directory.
func expandTilde(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, "~\\") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

// versionString returns the kernel version, preferring the module version
// embedded by the Go toolchain when the binary was built from a tagged module.
func versionString() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return VERSION
}

// baseToolSet is the default tool set when no tool flags constrain it.
func baseToolSet(env execenv.ExecutionEnv, shellCommandPrefix string) []extension.ToolDefinition {
	return builtin.NewCodingTools(env, shellCommandPrefix)
}

// allToolSet is the full tool set used as the --tools allowlist source.
func allToolSet(env execenv.ExecutionEnv, shellCommandPrefix string) []extension.ToolDefinition {
	return builtin.NewAllTools(env, shellCommandPrefix)
}
