package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAgentDirDefaultAndEnvOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(EnvAgentDir, "")

	got, err := AgentDir()
	if err != nil {
		t.Fatalf("AgentDir: %v", err)
	}
	want := filepath.Join(home, ConfigDirName, "agent")
	if got != want {
		t.Fatalf("AgentDir = %q, want %q", got, want)
	}

	t.Setenv(EnvAgentDir, "~/custom-agent")
	got, err = AgentDir()
	if err != nil {
		t.Fatalf("AgentDir env: %v", err)
	}
	want = filepath.Join(home, "custom-agent")
	if got != want {
		t.Fatalf("AgentDir env = %q, want %q", got, want)
	}
}

func TestSessionDirEnvOverrideAndDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(EnvAgentDir, "")
	t.Setenv(EnvSessionDir, "")

	got, err := SessionDir()
	if err != nil {
		t.Fatalf("SessionDir: %v", err)
	}
	want := filepath.Join(home, ConfigDirName, "agent", "sessions")
	if got != want {
		t.Fatalf("SessionDir = %q, want %q", got, want)
	}

	t.Setenv(EnvSessionDir, "~/custom-sessions")
	got, err = SessionDir()
	if err != nil {
		t.Fatalf("SessionDir env: %v", err)
	}
	want = filepath.Join(home, "custom-sessions")
	if got != want {
		t.Fatalf("SessionDir env = %q, want %q", got, want)
	}
}

func TestResolveValueEnvLiteralCommandAndCache(t *testing.T) {
	resetResolveValueCache()

	t.Setenv("ALONG_TEST_CONFIG_VALUE", "from-env")
	if got := ResolveValue("ALONG_TEST_CONFIG_VALUE"); got != "from-env" {
		t.Fatalf("ResolveValue env = %q, want from-env", got)
	}
	if got := ResolveValue("literal-value"); got != "literal-value" {
		t.Fatalf("ResolveValue literal = %q, want literal-value", got)
	}
	if got := ResolveValue("!printf hi"); got != "hi" {
		t.Fatalf("ResolveValue command = %q, want hi", got)
	}

	if runtime.GOOS == "windows" {
		t.Skip("cache assertion uses POSIX shell redirection")
	}
	counter := filepath.Join(t.TempDir(), "counter")
	command := "!printf x >> " + shellQuote(counter) + "; wc -c < " + shellQuote(counter)
	if got := ResolveValue(command); got != "1" {
		t.Fatalf("ResolveValue first cached command = %q, want 1", got)
	}
	if got := ResolveValue(command); got != "1" {
		t.Fatalf("ResolveValue second cached command = %q, want cached 1", got)
	}
}

func TestLoadSettingsDeepMerge(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "agent")
	projectDir := filepath.Join(root, "project")
	writeFile(t, filepath.Join(agentDir, settingsFile), `{
		"transport": "sse",
		"retry": {"enabled": true, "maxRetries": 3},
		"nested": {"inner": {"keep": 1, "drop": 2}},
		"tools": ["global"],
		"globalOnly": 1
	}`)
	writeFile(t, filepath.Join(projectDir, ConfigDirName, settingsFile), `{
		"retry": {"maxRetries": 5},
		"nested": {"inner": {"keep": 9}},
		"tools": ["project"],
		"projectOnly": true
	}`)
	t.Chdir(projectDir)
	t.Setenv(EnvAgentDir, agentDir)

	cfg, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AgentDir != agentDir {
		t.Fatalf("AgentDir = %q, want %q", cfg.AgentDir, agentDir)
	}
	if cfg.Settings["transport"] != "sse" || cfg.Settings["projectOnly"] != true {
		t.Fatalf("settings = %+v", cfg.Settings)
	}
	retry, ok := cfg.Settings["retry"].(map[string]any)
	if !ok {
		t.Fatalf("retry = %T, want map", cfg.Settings["retry"])
	}
	if retry["enabled"] != true || retry["maxRetries"] != float64(5) {
		t.Fatalf("retry = %+v, want merged enabled=true maxRetries=5", retry)
	}
	tools, ok := cfg.Settings["tools"].([]any)
	if !ok || len(tools) != 1 || tools[0] != "project" {
		t.Fatalf("tools = %+v, want project array replacement", cfg.Settings["tools"])
	}
	// pi merges one level deep only: a depth-2 object from the project file
	// replaces the global one wholesale (settings-manager.ts:138).
	nested, ok := cfg.Settings["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested = %T, want map", cfg.Settings["nested"])
	}
	inner, ok := nested["inner"].(map[string]any)
	if !ok {
		t.Fatalf("inner = %T, want map", nested["inner"])
	}
	if inner["keep"] != float64(9) {
		t.Fatalf("inner.keep = %v, want 9", inner["keep"])
	}
	if _, exists := inner["drop"]; exists {
		t.Fatalf("inner = %+v, want depth-2 object replaced wholesale (drop removed)", inner)
	}
}

func TestLoadModelsJSONStripsCommentsAndTrailingCommas(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "agent")
	projectDir := filepath.Join(root, "project")
	writeFile(t, filepath.Join(agentDir, modelsJSONFile), `{
		// provider comment
		"baseUrl": "https://example.test//v1",
		"models": [
			{"id": "m1", "label": "comma, // literal",},
		],
	}`)
	mkdir(t, projectDir)
	t.Chdir(projectDir)
	t.Setenv(EnvAgentDir, agentDir)

	cfg, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(cfg.ModelsJSON, &parsed); err != nil {
		t.Fatalf("unmarshal stripped models JSON: %v\n%s", err, cfg.ModelsJSON)
	}
	if parsed["baseUrl"] != "https://example.test//v1" {
		t.Fatalf("baseUrl = %q, want string literal preserved", parsed["baseUrl"])
	}
	models := parsed["models"].([]any)
	model := models[0].(map[string]any)
	if model["label"] != "comma, // literal" {
		t.Fatalf("label = %q, want comment-like string preserved", model["label"])
	}
	if strings.Contains(string(cfg.ModelsJSON), "// provider comment") {
		t.Fatalf("ModelsJSON still contains line comment: %s", cfg.ModelsJSON)
	}
}

func TestLoadMissingFilesUsesDefaults(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "agent")
	projectDir := filepath.Join(root, "project")
	mkdir(t, projectDir)
	t.Chdir(projectDir)
	t.Setenv(EnvAgentDir, agentDir)

	cfg, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Settings) != 0 {
		t.Fatalf("Settings = %+v, want empty defaults", cfg.Settings)
	}
	if len(cfg.ModelsJSON) != 0 {
		t.Fatalf("ModelsJSON len = %d, want 0", len(cfg.ModelsJSON))
	}
}

func TestLoadInvalidJSONDegradesWithError(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, "agent")
	projectDir := filepath.Join(root, "project")
	settingsPath := filepath.Join(agentDir, settingsFile)
	writeFile(t, settingsPath, `{"bad":`)
	writeFile(t, filepath.Join(projectDir, ConfigDirName, settingsFile), `{"projectOnly": true}`)
	t.Chdir(projectDir)
	t.Setenv(EnvAgentDir, agentDir)

	// pi degrades a corrupt settings file to empty settings and records the
	// error instead of failing startup (settings-manager.ts:326-335).
	cfg, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Settings["projectOnly"] != true {
		t.Fatalf("Settings = %+v, want project scope still loaded", cfg.Settings)
	}
	if len(cfg.SettingsErrors) != 1 || cfg.SettingsErrors[0].Scope != ScopeGlobal {
		t.Fatalf("SettingsErrors = %+v, want one global error", cfg.SettingsErrors)
	}
	if !strings.Contains(cfg.SettingsErrors[0].Err.Error(), settingsPath) {
		t.Fatalf("error = %q, want path %q", cfg.SettingsErrors[0].Err, settingsPath)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", path, err)
	}
}
