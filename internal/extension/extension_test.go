package extension

import (
	"context"
	"errors"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/hook"
)

func TestLoadRegistersHandlerAfterFactorySuccessAndEmitFires(t *testing.T) {
	registry := hook.NewRegistry()
	ctxProvider := func(context.Context) ExtensionContext {
		return ExtensionContext{}
	}

	ext, err := Load("/test/ext", registry, ctxProvider, func(api ExtensionAPI) error {
		api.On(hook.EventToolCall, func(_ context.Context, event any, _ ExtensionContext) (any, error) {
			e := event.(hook.ToolCallEvent)
			if e.ToolName == "bash" {
				return hook.ToolCallResult{
					Block:  true,
					Reason: "blocked",
				}, nil
			}
			return nil, nil
		})
		return nil
	})
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}

	if !registry.HasHandlers(hook.EventToolCall) {
		t.Fatal("handler not registered in registry")
	}
	if len(ext.Handlers[hook.EventToolCall]) != 1 {
		t.Fatalf("ext.Handlers[%s] = %d, want 1", hook.EventToolCall, len(ext.Handlers[hook.EventToolCall]))
	}

	result, err := registry.Emit(context.Background(), hook.ToolCallEvent{
		Type:     hook.EventToolCall,
		ToolName: "bash",
	})
	if err != nil {
		t.Fatalf("Emit error: %v", err)
	}

	got, ok := result.(hook.ToolCallResult)
	if !ok {
		t.Fatalf("result type = %T, want hook.ToolCallResult", result)
	}
	if !got.Block || got.Reason != "blocked" {
		t.Fatalf("result = %#v, want blocked tool call", got)
	}
}

func TestNoopUIContextDoesNotPanicAndReturnsUnavailableErrors(t *testing.T) {
	ui := NoopUIContext{}

	gotSelect, err := ui.Select("pick", []string{"one"})
	if gotSelect != -1 || !errors.Is(err, ErrNoInteractiveUI) {
		t.Fatalf("Select = %d, %v; want -1, ErrNoInteractiveUI", gotSelect, err)
	}

	gotConfirm, err := ui.Confirm("continue")
	if gotConfirm || !errors.Is(err, ErrNoInteractiveUI) {
		t.Fatalf("Confirm = %v, %v; want false, ErrNoInteractiveUI", gotConfirm, err)
	}

	gotInput, err := ui.Input("name")
	if gotInput != "" || !errors.Is(err, ErrNoInteractiveUI) {
		t.Fatalf("Input = %q, %v; want empty, ErrNoInteractiveUI", gotInput, err)
	}

	gotEditor, err := ui.Editor("edit", "draft")
	if gotEditor != "" || !errors.Is(err, ErrNoInteractiveUI) {
		t.Fatalf("Editor = %q, %v; want empty, ErrNoInteractiveUI", gotEditor, err)
	}

	ui.Notify("ignored")
	ui.SetStatus("branch", "main")
	ui.SetWidget("diff", []string{"one", "two"})
	ui.SetTitle("title")
	ui.SetEditorText("text")
}

func TestLoadUsesFreshExtensionContextPerEmit(t *testing.T) {
	registry := hook.NewRegistry()
	var providerCalls int
	ctxProvider := func(context.Context) ExtensionContext {
		providerCalls++
		tokens := providerCalls
		return ExtensionContext{
			GetContextUsage: func() *ContextUsage {
				return &ContextUsage{Tokens: tokens}
			},
		}
	}
	gotTokens := []int{}

	_, err := Load("/test/ext", registry, ctxProvider, func(api ExtensionAPI) error {
		api.On(hook.EventTurnEnd, func(_ context.Context, _ any, extCtx ExtensionContext) (any, error) {
			usage := extCtx.GetContextUsage()
			if usage == nil {
				t.Fatal("GetContextUsage returned nil")
			}
			gotTokens = append(gotTokens, usage.Tokens)
			return nil, nil
		})
		return nil
	})
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}

	for range 2 {
		_, err := registry.Emit(context.Background(), hook.TurnEndEvent{Type: hook.EventTurnEnd})
		if err != nil {
			t.Fatalf("Emit error: %v", err)
		}
	}

	if len(gotTokens) != 2 || gotTokens[0] != 1 || gotTokens[1] != 2 {
		t.Fatalf("gotTokens = %v, want [1 2]", gotTokens)
	}
}

func TestLoadFactoryErrorDoesNotRegisterHandlers(t *testing.T) {
	registry := hook.NewRegistry()
	ctxProvider := func(context.Context) ExtensionContext {
		return ExtensionContext{}
	}
	wantErr := errors.New("factory failed")

	_, err := Load("/test/ext", registry, ctxProvider, func(api ExtensionAPI) error {
		api.On(hook.EventAgentStart, func(context.Context, any, ExtensionContext) (any, error) {
			return nil, nil
		})
		return wantErr
	})

	if !errors.Is(err, wantErr) {
		t.Fatalf("Load error = %v, want %v", err, wantErr)
	}
	if registry.HasHandlers(hook.EventAgentStart) {
		t.Fatal("handler leaked into registry after factory error")
	}
}

func TestLoadDuplicateCommandDoesNotRegisterHandlers(t *testing.T) {
	registry := hook.NewRegistry()
	ctxProvider := func(context.Context) ExtensionContext {
		return ExtensionContext{}
	}

	_, err := Load("/test/ext", registry, ctxProvider, func(api ExtensionAPI) error {
		api.On(hook.EventTurnStart, func(context.Context, any, ExtensionContext) (any, error) {
			return nil, nil
		})
		api.RegisterCommand("compact", CommandDefinition{Description: "first"})
		api.RegisterCommand("compact", CommandDefinition{Description: "second"})
		return nil
	})

	if err == nil {
		t.Fatal("Load error = nil, want duplicate command error")
	}
	if registry.HasHandlers(hook.EventTurnStart) {
		t.Fatal("handler leaked into registry after duplicate command error")
	}
}

func TestLoadReturnsDescriptors(t *testing.T) {
	registry := hook.NewRegistry()
	ctxProvider := func(context.Context) ExtensionContext {
		return ExtensionContext{}
	}
	name := "Local OpenAI"
	baseURL := "http://localhost:8317/v1"
	apiName := "openai-responses"
	apiKey := "LOCAL_OPENAI_API_KEY"
	authHeader := true
	contextWindow := 400_000

	ext, err := Load("/test/ext", registry, ctxProvider, func(api ExtensionAPI) error {
		api.On(hook.EventTurnStart, func(context.Context, any, ExtensionContext) (any, error) {
			return nil, nil
		})
		api.RegisterTool(ToolDefinition{
			Name:        "grep",
			Label:       "Grep",
			Description: "Search files",
			Parameters:  map[string]any{"pattern": "string"},
			Execute: func(context.Context, ToolCall, ToolUpdateCallback, ExtensionContext) (ToolResult, error) {
				return ToolResult{}, nil
			},
		})
		api.RegisterCommand("compact", CommandDefinition{
			Description: "Compact the session",
			Handler: func(context.Context, []string, ExtensionContext) error {
				return nil
			},
		})
		api.RegisterProvider("local-openai", ProviderDefinition{
			Name:       &name,
			BaseURL:    &baseURL,
			API:        &apiName,
			APIKey:     &apiKey,
			AuthHeader: &authHeader,
			Models: []ProviderModel{
				{
					ID:            "local-gpt-5.5",
					ContextWindow: &contextWindow,
				},
			},
		})
		return nil
	})
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}

	if ext.Path != "/test/ext" {
		t.Fatalf("Path = %q, want /test/ext", ext.Path)
	}
	if len(ext.Handlers[hook.EventTurnStart]) != 1 {
		t.Fatalf("handler count = %d, want 1", len(ext.Handlers[hook.EventTurnStart]))
	}
	if len(ext.Tools) != 1 {
		t.Fatalf("tool count = %d, want 1", len(ext.Tools))
	}
	if ext.Tools[0].Name != "grep" {
		t.Fatalf("tool name = %q, want grep", ext.Tools[0].Name)
	}
	cmd, ok := ext.Commands["compact"]
	if !ok {
		t.Fatal("command compact not returned")
	}
	if cmd.Description != "Compact the session" {
		t.Fatalf("command description = %q, want Compact the session", cmd.Description)
	}
	prov, ok := ext.Providers["local-openai"]
	if !ok {
		t.Fatal("provider local-openai not returned")
	}
	if prov.Name == nil || *prov.Name != name {
		t.Fatalf("provider name = %v, want %q", prov.Name, name)
	}
	if prov.BaseURL == nil || *prov.BaseURL != baseURL {
		t.Fatalf("provider baseURL = %v, want %q", prov.BaseURL, baseURL)
	}
	if len(prov.Models) != 1 || prov.Models[0].ID != "local-gpt-5.5" {
		t.Fatalf("provider models = %#v, want local-gpt-5.5", prov.Models)
	}
}

func TestLoadUnregisterProviderRemovesStagedDefinition(t *testing.T) {
	registry := hook.NewRegistry()
	ctxProvider := func(context.Context) ExtensionContext {
		return ExtensionContext{}
	}
	apiValue := "openai-responses"

	ext, err := Load("/test/ext", registry, ctxProvider, func(api ExtensionAPI) error {
		api.RegisterProvider("local-openai", ProviderDefinition{API: &apiValue})
		api.UnregisterProvider("local-openai")
		return nil
	})
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if len(ext.Providers) != 0 {
		t.Fatalf("providers = %#v, want empty map", ext.Providers)
	}
}
