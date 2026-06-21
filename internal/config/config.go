package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	ConfigDirName = ".along"

	EnvAgentDir   = "ALONG_CODING_AGENT_DIR"
	EnvSessionDir = "ALONG_CODING_AGENT_SESSION_DIR"

	modelsJSONFile = "models.json"
	authJSONFile   = "auth.json"
	settingsFile   = "settings.json"
	sessionsDir    = "sessions"

	commandTimeout = 10 * time.Second
)

type Config struct {
	AgentDir   string
	Settings   Settings
	ModelsJSON []byte
	// SettingsErrors carries per-scope settings load failures; the affected
	// scope degrades to empty settings instead of failing startup
	// (pi settings-manager.ts:326-335).
	SettingsErrors []SettingsError
}

type Settings map[string]any

var resolveValueCache = struct {
	sync.Mutex
	values map[string]string
}{
	values: map[string]string{},
}

func Load(ctx context.Context) (Config, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return Config{}, err
		}
	}

	agentDir, err := AgentDir()
	if err != nil {
		return Config{}, err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return Config{}, err
	}

	globalSettingsPath := filepath.Join(agentDir, settingsFile)
	projectSettingsPath := filepath.Join(cwd, ConfigDirName, settingsFile)
	settingsErrors := []SettingsError{}
	globalSettings, err := readSettings(globalSettingsPath)
	if err != nil {
		settingsErrors = append(settingsErrors, SettingsError{Scope: ScopeGlobal, Err: err})
	}
	projectSettings, err := readSettings(projectSettingsPath)
	if err != nil {
		settingsErrors = append(settingsErrors, SettingsError{Scope: ScopeProject, Err: err})
	}

	modelsJSON, err := readModelsJSON(filepath.Join(agentDir, modelsJSONFile))
	if err != nil {
		return Config{}, err
	}

	return Config{
		AgentDir:       agentDir,
		Settings:       mergeSettings(globalSettings, projectSettings),
		ModelsJSON:     modelsJSON,
		SettingsErrors: settingsErrors,
	}, nil
}

func AgentDir() (string, error) {
	if envDir := os.Getenv(EnvAgentDir); envDir != "" {
		return expandTildePath(envDir)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, ConfigDirName, "agent"), nil
}

func SessionDir() (string, error) {
	if envDir := os.Getenv(EnvSessionDir); envDir != "" {
		return expandTildePath(envDir)
	}

	agentDir, err := AgentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(agentDir, sessionsDir), nil
}

func ModelsJSONPath() (string, error) {
	return agentPath(modelsJSONFile)
}

func AuthJSONPath() (string, error) {
	return agentPath(authJSONFile)
}

func SettingsPath() (string, error) {
	return agentPath(settingsFile)
}

func ResolveValue(v string) string {
	if strings.HasPrefix(v, "!") {
		return resolveCommandValue(v)
	}
	if envValue := os.Getenv(v); envValue != "" { // pi: `envValue || config` —— 空 env 回退字面
		return envValue
	}
	return v
}

func agentPath(name string) (string, error) {
	agentDir, err := AgentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(agentDir, name), nil
}

func expandTildePath(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, `~\`) {
		return filepath.Clean(path), nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if path == "~" {
		return homeDir, nil
	}
	return filepath.Join(homeDir, path[2:]), nil
}

func resolveCommandValue(commandConfig string) string {
	resolveValueCache.Lock()
	if cached, ok := resolveValueCache.values[commandConfig]; ok {
		resolveValueCache.Unlock()
		return cached
	}
	resolveValueCache.Unlock()

	result := executeCommand(commandConfig[1:])

	resolveValueCache.Lock()
	resolveValueCache.values[commandConfig] = result
	resolveValueCache.Unlock()

	return result
}

func executeCommand(command string) string {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	shell, args := shellCommand(command)
	cmd := exec.CommandContext(ctx, shell, args...)
	cmd.Stdin = nil
	cmd.Stderr = io.Discard

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(stdout.String())
}

func shellCommand(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/C", command}
	}
	return "sh", []string{"-c", command}
}

func readSettings(path string) (Settings, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Settings{}, nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("read settings %s: %w", path, err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return Settings{}, nil
	}

	settings := Settings{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return Settings{}, fmt.Errorf("parse settings %s: %w", path, err)
	}
	if settings == nil {
		return Settings{}, nil
	}
	return settings, nil
}

func readModelsJSON(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return []byte{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read models.json %s: %w", path, err)
	}

	stripped := []byte(stripJSONComments(string(data)))
	var decoded any
	if err := json.Unmarshal(stripped, &decoded); err != nil {
		return nil, fmt.Errorf("parse models.json %s: %w", path, err)
	}
	return stripped, nil
}

func mergeSettings(base Settings, overrides Settings) Settings {
	result := make(Settings, len(base)+len(overrides))
	maps.Copy(result, base)

	for key, overrideValue := range overrides {
		baseObject, baseOK := asObject(result[key])
		overrideObject, overrideOK := asObject(overrideValue)
		if baseOK && overrideOK {
			// pi merges nested objects exactly one level deep —
			// { ...baseValue, ...overrideValue } — deeper keys replace
			// wholesale (settings-manager.ts:118-146).
			merged := make(map[string]any, len(baseObject)+len(overrideObject))
			maps.Copy(merged, baseObject)
			maps.Copy(merged, overrideObject)
			result[key] = merged
			continue
		}
		result[key] = overrideValue
	}

	return result
}

func asObject(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	if ok {
		return object, true
	}

	settings, ok := value.(Settings)
	if ok {
		return map[string]any(settings), true
	}

	return nil, false
}

func stripJSONComments(input string) string {
	withoutComments := stripJSONLineComments(input)
	return stripJSONTrailingCommas(withoutComments)
}

func stripJSONLineComments(input string) string {
	var out strings.Builder
	out.Grow(len(input))

	var inString bool
	var escaped bool
	for i := 0; i < len(input); i++ {
		c := input[i]
		if inString {
			out.WriteByte(c)
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}

		if c == '"' {
			inString = true
			out.WriteByte(c)
			continue
		}
		if c == '/' && i+1 < len(input) && input[i+1] == '/' {
			i += 2
			for i < len(input) && input[i] != '\n' {
				i++
			}
			if i < len(input) {
				out.WriteByte(input[i])
			}
			continue
		}
		out.WriteByte(c)
	}

	return out.String()
}

func stripJSONTrailingCommas(input string) string {
	var out strings.Builder
	out.Grow(len(input))

	var inString bool
	var escaped bool
	for i := 0; i < len(input); i++ {
		c := input[i]
		if inString {
			out.WriteByte(c)
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}

		if c == '"' {
			inString = true
			out.WriteByte(c)
			continue
		}
		if c == ',' && commaIsTrailing(input, i) {
			continue
		}
		out.WriteByte(c)
	}

	return out.String()
}

func commaIsTrailing(input string, index int) bool {
	for i := index + 1; i < len(input); i++ {
		if isJSONWhitespace(input[i]) {
			continue
		}
		return input[i] == '}' || input[i] == ']'
	}
	return false
}

func isJSONWhitespace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func resetResolveValueCache() {
	resolveValueCache.Lock()
	resolveValueCache.values = map[string]string{}
	resolveValueCache.Unlock()
}
