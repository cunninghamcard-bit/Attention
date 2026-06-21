package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/config"
	"github.com/cunninghamcard-bit/Attention/internal/mode/compat"
)

func TestBuildProviderResolvesCustomModelFromModelsJSON(t *testing.T) {
	ctx := context.Background()
	agentDir := t.TempDir()
	t.Setenv(config.EnvAgentDir, agentDir)
	t.Setenv(config.EnvSessionDir, "")

	modelsJSON := `{
  "providers": {
    "local-openai": {
      "name": "Local OpenAI",
      "baseUrl": "http://localhost:8317/v1",
      "api": "openai-responses",
      "apiKey": "local-key",
      "authHeader": true,
      "models": [
        {
          "id": "local-gpt-5.5",
          "name": "Local GPT-5.5",
          "contextWindow": 400000,
          "maxTokens": 128000
        }
      ]
    }
  }
}`
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(modelsJSON), 0o600); err != nil {
		t.Fatalf("write models.json: %v", err)
	}

	cfg, err := config.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	prov, err := buildProvider(ctx, cfg)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}

	model, ok := prov.Resolve("local-gpt-5.5")
	if !ok {
		t.Fatal("Resolve local-gpt-5.5 = false")
	}
	if model.Provider != "local-openai" || model.API != ai.APIOpenAIResponses {
		t.Fatalf("model = %+v, want local-openai/openai-responses", model)
	}
	if model.BaseURL != "http://localhost:8317/v1" {
		t.Fatalf("BaseURL = %q, want local endpoint", model.BaseURL)
	}

	auth, err := prov.ResolveAuth(ctx, model)
	if err != nil {
		t.Fatalf("ResolveAuth: %v", err)
	}
	if auth.APIKey != "local-key" || auth.Headers["Authorization"] != "Bearer local-key" {
		t.Fatalf("auth = %#v, want literal key plus Authorization header", auth)
	}
}

func TestResolveModelUnknownListsAvailableModels(t *testing.T) {
	ctx := context.Background()
	agentDir := t.TempDir()
	t.Setenv(config.EnvAgentDir, agentDir)
	t.Setenv(config.EnvSessionDir, "")

	modelsJSON := `{
  "providers": {
    "local-openai": {
      "baseUrl": "http://localhost:8317/v1",
      "api": "openai-responses",
      "apiKey": "local-key",
      "models": [{"id": "local-gpt-5.5"}]
    }
  }
}`
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(modelsJSON), 0o600); err != nil {
		t.Fatalf("write models.json: %v", err)
	}

	cfg, err := config.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	prov, err := buildProvider(ctx, cfg)
	if err != nil {
		t.Fatalf("buildProvider: %v", err)
	}

	err = resolveModel(prov, "missing-model")
	if err == nil {
		t.Fatal("resolveModel error = nil, want unknown model error")
	}
	if !strings.Contains(err.Error(), "missing-model") || !strings.Contains(err.Error(), "local-gpt-5.5") {
		t.Fatalf("error = %q, want missing and available model IDs", err)
	}
}

func TestRunPromptModeJSONSelectsRPCPath(t *testing.T) {
	originalPrint := runPrintMode
	originalJSON := runJSONMode
	defer func() {
		runPrintMode = originalPrint
		runJSONMode = originalJSON
	}()

	var called string
	runPrintMode = func(context.Context, printPromptRunner, []string) error {
		called = "print"
		return nil
	}
	runJSONMode = func(_ context.Context, _ jsonPromptRunner, prompts []string) error {
		called = "json"
		if len(prompts) != 1 || prompts[0] != "prompt" {
			t.Fatalf("prompts = %q, want [prompt]", prompts)
		}
		return nil
	}

	if err := runPromptMode(context.Background(), "json", nil, []string{"prompt"}); err != nil {
		t.Fatalf("runPromptMode: %v", err)
	}
	if called != "json" {
		t.Fatalf("called mode = %q, want json", called)
	}
}

func TestRunPromptModeRPCSelectsBidirectionalRPCPath(t *testing.T) {
	originalPrint := runPrintMode
	originalJSON := runJSONMode
	originalRPC := runRPCMode
	defer func() {
		runPrintMode = originalPrint
		runJSONMode = originalJSON
		runRPCMode = originalRPC
	}()

	var called string
	runPrintMode = func(context.Context, printPromptRunner, []string) error {
		called = "print"
		return nil
	}
	runJSONMode = func(context.Context, jsonPromptRunner, []string) error {
		called = "json"
		return nil
	}
	runRPCMode = func(context.Context, compat.Target) error {
		called = "rpc"
		return nil
	}

	if err := runPromptMode(context.Background(), "rpc", nil, []string{"ignored"}); err != nil {
		t.Fatalf("runPromptMode: %v", err)
	}
	if called != "rpc" {
		t.Fatalf("called mode = %q, want rpc", called)
	}
}

func TestRunPromptModeRejectsUnknownModeBeforeDispatch(t *testing.T) {
	originalPrint := runPrintMode
	originalJSON := runJSONMode
	originalRPC := runRPCMode
	defer func() {
		runPrintMode = originalPrint
		runJSONMode = originalJSON
		runRPCMode = originalRPC
	}()

	wantErr := errors.New("should not be called")
	runPrintMode = func(context.Context, printPromptRunner, []string) error {
		return wantErr
	}
	runJSONMode = func(context.Context, jsonPromptRunner, []string) error {
		return wantErr
	}
	runRPCMode = func(context.Context, compat.Target) error {
		return wantErr
	}

	err := runPromptMode(context.Background(), "xml", nil, []string{"prompt"})
	if err == nil {
		t.Fatal("runPromptMode error = nil, want unknown mode")
	}
	if errors.Is(err, wantErr) {
		t.Fatal("mode runner was called for unknown mode")
	}
	if !strings.Contains(err.Error(), "unknown mode") {
		t.Fatalf("error = %q, want unknown mode", err)
	}
}

func TestSettingsStringSliceCoercesJSONArray(t *testing.T) {
	settings := config.Settings{
		"paths": []any{"a", 1, "b", false, "c"},
	}

	got := settingsStringSlice(settings, "paths")
	want := []string{"a", "b", "c"}
	if !slices.Equal(got, want) {
		t.Fatalf("settingsStringSlice = %#v, want %#v", got, want)
	}
}

func TestSettingsStringSliceHandlesMissingAndNonArray(t *testing.T) {
	tests := []struct {
		name     string
		settings config.Settings
		key      string
	}{
		{
			name:     "missing key",
			settings: config.Settings{},
			key:      "paths",
		},
		{
			name: "non array",
			settings: config.Settings{
				"paths": "not-an-array",
			},
			key: "paths",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := settingsStringSlice(tt.settings, tt.key)
			if len(got) != 0 {
				t.Fatalf("settingsStringSlice = %#v, want empty", got)
			}
		})
	}
}

func TestSettingsStringSliceCopiesStringSlice(t *testing.T) {
	settings := config.Settings{
		"paths": []string{"a", "b"},
	}

	got := settingsStringSlice(settings, "paths")
	want := []string{"a", "b"}
	if !slices.Equal(got, want) {
		t.Fatalf("settingsStringSlice = %#v, want %#v", got, want)
	}
	got[0] = "changed"
	if settings["paths"].([]string)[0] != "a" {
		t.Fatal("settingsStringSlice returned original backing array")
	}
}

func TestJSExtensionCandidatesLoadsGlobalAndProjectTypeScript(t *testing.T) {
	agentDir := t.TempDir()
	cwd := t.TempDir()
	extraDir := t.TempDir()

	globalDir := filepath.Join(agentDir, "extensions")
	projectDir := filepath.Join(cwd, config.ConfigDirName, "extensions")
	if err := os.MkdirAll(globalDir, 0o700); err != nil {
		t.Fatalf("mkdir global extensions: %v", err)
	}
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatalf("mkdir project extensions: %v", err)
	}
	globalExt := filepath.Join(globalDir, "global.ts")
	projectExt := filepath.Join(projectDir, "project.ts")
	extraExt := filepath.Join(extraDir, "extra.ts")
	explicitExt := filepath.Join(cwd, "standalone.ts")
	if err := os.WriteFile(globalExt, []byte("export default function () {}"), 0o600); err != nil {
		t.Fatalf("write global extension: %v", err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "ignored.js"), []byte(""), 0o600); err != nil {
		t.Fatalf("write ignored extension: %v", err)
	}
	if err := os.WriteFile(projectExt, []byte("export default function () {}"), 0o600); err != nil {
		t.Fatalf("write project extension: %v", err)
	}
	if err := os.WriteFile(extraExt, []byte("export default function () {}"), 0o600); err != nil {
		t.Fatalf("write extra extension: %v", err)
	}
	if err := os.WriteFile(explicitExt, []byte("export default function () {}"), 0o600); err != nil {
		t.Fatalf("write explicit extension: %v", err)
	}

	got := jsExtensionCandidates(agentDir, cwd, []string{extraDir, "standalone.ts"})
	want := []string{globalExt, projectExt, extraExt, explicitExt}
	if len(got) != len(want) {
		t.Fatalf("jsExtensionCandidates len = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("jsExtensionCandidates[%d] = %q, want %q; all=%v", i, got[i], want[i], got)
		}
	}
}

func TestDiscoverJSExtensionsSkipsWithWarningWhenBunMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	agentDir := t.TempDir()
	extensionDir := filepath.Join(agentDir, "extensions")
	if err := os.MkdirAll(extensionDir, 0o700); err != nil {
		t.Fatalf("mkdir extensions: %v", err)
	}
	if err := os.WriteFile(filepath.Join(extensionDir, "ext.ts"), []byte("export default function () {}"), 0o600); err != nil {
		t.Fatalf("write extension: %v", err)
	}

	var stderr bytes.Buffer
	got := discoverJSExtensions(agentDir, t.TempDir(), nil, &stderr)
	if got != nil {
		t.Fatalf("discoverJSExtensions = %v, want nil without bun", got)
	}
	if !strings.Contains(stderr.String(), "bun not found") || !strings.Contains(stderr.String(), "skipping 1") {
		t.Fatalf("stderr = %q, want bun skip warning", stderr.String())
	}
}
