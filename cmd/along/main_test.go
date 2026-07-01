package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/config"
	"github.com/cunninghamcard-bit/Attention/internal/orchestrator"
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

// fakeShutdownTarget records the one-shot exit-path calls so the print/json
// shutdown sequence can be asserted without a real orchestrator.
type fakeShutdownTarget struct {
	shutdownCalls    int
	shutdownReason   string
	shutdownCtxAlive bool
	closeCalls       int
	shutdownErr      error
	closeErr         error
}

func (f *fakeShutdownTarget) NotifySessionShutdown(ctx context.Context, reason string) error {
	f.shutdownCalls++
	f.shutdownReason = reason
	// The run ctx may already be cancelled by a signal; shutdownOrchestrator
	// detaches it with context.WithoutCancel so handlers still run.
	f.shutdownCtxAlive = ctx.Err() == nil
	return f.shutdownErr
}

func (f *fakeShutdownTarget) Close() error {
	f.closeCalls++
	return f.closeErr
}

func TestShutdownOrchestratorEmitsSessionShutdownOnPrintExit(t *testing.T) {
	// FIX B: the print/json one-shot path must deliver session_shutdown before
	// Close so extension/hook shutdown handlers run for non-rpc runs. The ctx is
	// already cancelled (as a signal would leave it); shutdown must still fire.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	target := &fakeShutdownTarget{}
	if err := shutdownOrchestrator(ctx, "print", target); err != nil {
		t.Fatalf("shutdownOrchestrator: %v", err)
	}
	if target.shutdownCalls != 1 {
		t.Fatalf("shutdown calls = %d, want 1", target.shutdownCalls)
	}
	if target.shutdownReason != "quit" {
		t.Fatalf("shutdown reason = %q, want quit", target.shutdownReason)
	}
	if !target.shutdownCtxAlive {
		t.Fatal("shutdown ctx was cancelled, want detached via context.WithoutCancel")
	}
	if target.closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", target.closeCalls)
	}
}

func TestShutdownOrchestratorSkipsSessionShutdownForRPC(t *testing.T) {
	// rpc mode already emits session_shutdown in serve()'s defer, so the
	// one-shot path must NOT emit it again (avoid a double emit) but must still
	// Close the orchestrator.
	target := &fakeShutdownTarget{}
	if err := shutdownOrchestrator(context.Background(), "rpc", target); err != nil {
		t.Fatalf("shutdownOrchestrator: %v", err)
	}
	if target.shutdownCalls != 0 {
		t.Fatalf("shutdown calls = %d, want 0 (rpc emits in serve defer)", target.shutdownCalls)
	}
	if target.closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", target.closeCalls)
	}
}

func TestShutdownOrchestratorJoinsShutdownAndCloseErrors(t *testing.T) {
	shutdownErr := errors.New("shutdown boom")
	closeErr := errors.New("close boom")
	target := &fakeShutdownTarget{shutdownErr: shutdownErr, closeErr: closeErr}

	err := shutdownOrchestrator(context.Background(), "print", target)
	if err == nil {
		t.Fatal("shutdownOrchestrator error = nil, want joined errors")
	}
	if !errors.Is(err, shutdownErr) || !errors.Is(err, closeErr) {
		t.Fatalf("error = %v, want both shutdown and close errors", err)
	}
}

func TestRunPluginInstallCommandInstallsAndEnablesLocalPlugin(t *testing.T) {
	root := t.TempDir()
	agentDir := filepath.Join(root, config.AgentDirName)
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(filepath.Join(source, ".attention-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".attention-plugin", "plugin.json"), []byte(`{"name":"demo"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvAgentDir, agentDir)

	handled, err := runPluginCommand(context.Background(), []string{"plugin", "install", source})
	if err != nil {
		t.Fatalf("runPluginCommand: %v", err)
	}
	if !handled {
		t.Fatal("runPluginCommand handled = false, want true")
	}
	if _, err := os.Stat(filepath.Join(root, "plugins", "demo", ".attention-plugin", "plugin.json")); err != nil {
		t.Fatalf("installed plugin manifest: %v", err)
	}

	manager, err := config.NewManager(agentDir, t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	global, err := manager.ScopeSettings(config.ScopeGlobal)
	if err != nil {
		t.Fatalf("ScopeSettings: %v", err)
	}
	if got := settingsStringSlice(global, "plugins"); !slices.Equal(got, []string{"demo"}) {
		t.Fatalf("plugins = %#v, want [demo]", got)
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
