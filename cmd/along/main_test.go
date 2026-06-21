package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/orchestrator"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/config"
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
	runPrintMode = func(context.Context, *orchestrator.Orchestrator, []string) error {
		called = "print"
		return nil
	}
	runJSONMode = func(_ context.Context, _ *orchestrator.Orchestrator, prompts []string) error {
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

func TestRunPromptModeRejectsUnknownModeBeforeDispatch(t *testing.T) {
	originalPrint := runPrintMode
	originalJSON := runJSONMode
	defer func() {
		runPrintMode = originalPrint
		runJSONMode = originalJSON
	}()

	wantErr := errors.New("should not be called")
	runPrintMode = func(context.Context, *orchestrator.Orchestrator, []string) error {
		return wantErr
	}
	runJSONMode = func(context.Context, *orchestrator.Orchestrator, []string) error {
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

