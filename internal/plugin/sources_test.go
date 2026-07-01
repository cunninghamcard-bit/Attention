package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/config"
	internalextension "github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
)

func TestLoadFilePluginSourcesHooksBinAndResources(t *testing.T) {
	rootDir := t.TempDir()
	agentDir := filepath.Join(rootDir, config.AgentDirName)
	cwd := t.TempDir()
	root := filepath.Join(globalPluginDir(agentDir), "rtk-optimizer")
	writePluginFixture(t, root)

	result := Load(config.Settings{
		settingsPluginsKey: []any{"rtk-optimizer"},
	}, agentDir, cwd)
	if len(result.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v, want none", result.Diagnostics)
	}
	if len(result.Sources) != 1 {
		t.Fatalf("sources = %d, want 1", len(result.Sources))
	}
	if result.Sources[0].Path != sourcePathPrefix+"rtk-optimizer" || result.Sources[0].Factory == nil {
		t.Fatalf("source = %#v, want plugin source", result.Sources[0])
	}
	if len(result.BinDirs) != 1 || result.BinDirs[0] != filepath.Join(root, binDirName) {
		t.Fatalf("bin dirs = %#v, want plugin bin", result.BinDirs)
	}

	reg := hook.NewRegistry()
	_, err := internalextension.Load(result.Sources[0].Path, reg, func(context.Context) internalextension.ExtensionContext {
		return internalextension.ExtensionContext{SessionID: "sess-1"}
	}, result.Sources[0].Factory)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	patch, err := reg.Emit(context.Background(), hook.ToolCallEvent{
		Type:     hook.EventToolCall,
		ToolName: "bash",
		Input:    map[string]any{"command": "npm test"},
	})
	if err != nil {
		t.Fatalf("Emit tool_call: %v", err)
	}
	toolPatch := patch.(hook.ToolCallResult)
	if toolPatch.Input["command"] != "rtk npm test" {
		t.Fatalf("command = %#v, want rewritten command", toolPatch.Input["command"])
	}

	resources, err := reg.Emit(context.Background(), hook.ResourcesDiscoverEvent{
		Type:   hook.EventResourcesDiscover,
		CWD:    cwd,
		Reason: "startup",
	})
	if err != nil {
		t.Fatalf("Emit resources_discover: %v", err)
	}
	discovered := resources.(hook.ResourcesDiscoverResult)
	if len(discovered.SkillPaths) != 1 || discovered.SkillPaths[0] != filepath.Join(root, skillsDirName) {
		t.Fatalf("SkillPaths = %#v, want plugin skills", discovered.SkillPaths)
	}
	if len(discovered.PromptPaths) != 1 || discovered.PromptPaths[0] != filepath.Join(root, commandsDirName) {
		t.Fatalf("PromptPaths = %#v, want plugin commands", discovered.PromptPaths)
	}
}

func TestLoadProjectPluginOverridesGlobalPlugin(t *testing.T) {
	rootDir := t.TempDir()
	agentDir := filepath.Join(rootDir, config.AgentDirName)
	cwd := t.TempDir()
	globalRoot := filepath.Join(globalPluginDir(agentDir), "rtk-optimizer")
	projectRoot := filepath.Join(cwd, config.ConfigDirName, userPluginDirName, "rtk-optimizer")
	writePluginFixture(t, globalRoot)
	writePluginFixture(t, projectRoot)

	result := Load(config.Settings{settingsPluginsKey: []string{"rtk-optimizer"}}, agentDir, cwd)
	if len(result.Diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v, want none", result.Diagnostics)
	}
	if len(result.BinDirs) != 1 || result.BinDirs[0] != filepath.Join(projectRoot, binDirName) {
		t.Fatalf("bin dirs = %#v, want project plugin bin", result.BinDirs)
	}
}

func TestLoadMissingFilePluginReportsDiagnostic(t *testing.T) {
	result := Load(config.Settings{settingsPluginsKey: []string{"missing"}}, t.TempDir(), t.TempDir())
	if len(result.Sources) != 0 {
		t.Fatalf("sources = %#v, want none", result.Sources)
	}
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Type != "error" {
		t.Fatalf("diagnostics = %#v, want one error", result.Diagnostics)
	}
}

func TestFilePluginSystemDoesNotAddTypeScriptRuntime(t *testing.T) {
	rootDir := t.TempDir()
	agentDir := filepath.Join(rootDir, config.AgentDirName)
	cwd := t.TempDir()
	root := filepath.Join(globalPluginDir(agentDir), "ts-plugin")
	mustMkdir(t, filepath.Join(root, manifestDir))
	mustWrite(t, filepath.Join(root, manifestDir, manifestFileName), `{"name":"ts-plugin"}`)
	mustWrite(t, filepath.Join(root, "package.json"), `{"scripts":{"postinstall":"touch should-not-run"}}`)
	mustWrite(t, filepath.Join(root, "index.ts"), `throw new Error("should not run")`)

	result := Load(config.Settings{settingsPluginsKey: []string{"ts-plugin"}}, agentDir, cwd)
	if len(result.Sources) != 1 {
		t.Fatalf("sources = %#v, want plugin source", result.Sources)
	}
	if _, err := os.Stat(filepath.Join(root, "should-not-run")); !os.IsNotExist(err) {
		t.Fatalf("package script marker err = %v, want not created", err)
	}
}

func TestLoadRejectsPluginPathSetting(t *testing.T) {
	result := Load(config.Settings{settingsPluginsKey: []string{"./plugin"}}, t.TempDir(), t.TempDir())
	if len(result.Sources) != 0 {
		t.Fatalf("sources = %#v, want none", result.Sources)
	}
	if len(result.Diagnostics) != 1 || !strings.Contains(result.Diagnostics[0].Message, "must be a name") {
		t.Fatalf("diagnostics = %#v, want plugin name error", result.Diagnostics)
	}
}

func writePluginFixture(t *testing.T, root string) {
	t.Helper()
	mustMkdir(t, filepath.Join(root, manifestDir))
	mustMkdir(t, filepath.Join(root, hooksDirName))
	mustMkdir(t, filepath.Join(root, binDirName))
	mustMkdir(t, filepath.Join(root, skillsDirName))
	mustMkdir(t, filepath.Join(root, commandsDirName))
	mustWrite(t, filepath.Join(root, manifestDir, manifestFileName), `{"name":"rtk-optimizer","version":"1.0.0"}`)
	mustWrite(t, filepath.Join(root, hooksDirName, hooksFileName), `{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [{"type": "command", "command": "rewrite-hook"}]
      }
    ]
  }
}`)
	mustWrite(t, filepath.Join(root, binDirName, "rewrite-hook"), `#!/bin/sh
cat >/dev/null
echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","updatedInput":{"command":"rtk npm test"}}}'
`)
	if err := os.Chmod(filepath.Join(root, binDirName, "rewrite-hook"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
