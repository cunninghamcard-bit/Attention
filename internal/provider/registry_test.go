package provider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/auth"
)

func TestResolveBuiltIn(t *testing.T) {
	r := New(ai.BuiltinModels(), nil)
	model, ok := r.Resolve("gpt-5")
	if !ok {
		t.Fatal("Resolve gpt-5 = false")
	}
	if model.Provider != "openai" || model.API != ai.APIOpenAIResponses {
		t.Fatalf("model = %+v", model)
	}
}

func TestResolveDuplicateIDRequiresProvider(t *testing.T) {
	models := []ai.Model{
		{ID: "shared-model", Name: "A", Provider: "provider-a"},
		{ID: "shared-model", Name: "B", Provider: "provider-b"},
	}
	r := New(models, nil)

	if _, ok := r.Resolve("shared-model"); ok {
		t.Fatal("Resolve shared-model = true, want false for duplicate ID")
	}
	model, ok := r.ResolveByProvider("provider-b", "shared-model")
	if !ok {
		t.Fatal("ResolveByProvider provider-b/shared-model = false")
	}
	if model.Provider != "provider-b" || model.Name != "B" {
		t.Fatalf("model = %+v", model)
	}
}

func TestOverrideOnlyRedirectsBuiltInProvider(t *testing.T) {
	r := New(ai.BuiltinModels(), nil)
	err := r.ApplyConfig(ModelsConfig{Providers: map[string]ProviderConfig{
		"openai": {
			BaseURL: new("https://proxy.example/v1"),
			Headers: map[string]string{
				"X-Proxy": "yes",
			},
		},
	}})
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}

	model, ok := r.Resolve("gpt-5")
	if !ok {
		t.Fatal("Resolve gpt-5 = false")
	}
	if model.BaseURL != "https://proxy.example/v1" {
		t.Fatalf("BaseURL = %q", model.BaseURL)
	}
	if model.Headers["X-Proxy"] != "yes" {
		t.Fatalf("headers = %+v", model.Headers)
	}
	if len(r.All()) != len(ai.BuiltinModels()) {
		t.Fatalf("All len = %d, want built-ins kept", len(r.All()))
	}
}

func TestCustomProviderModelsReplaceProvider(t *testing.T) {
	r := New(ai.BuiltinModels(), nil)
	err := r.ApplyConfig(ModelsConfig{Providers: map[string]ProviderConfig{
		"openai": {
			BaseURL: new("https://custom.example/v1"),
			Models: []ModelDefinition{{
				ID:   "custom-gpt",
				Name: new("Custom GPT"),
			}},
		},
	}})
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}

	if _, ok := r.Resolve("gpt-5"); ok {
		t.Fatal("built-in gpt-5 remained after provider model replacement")
	}
	model, ok := r.Resolve("custom-gpt")
	if !ok {
		t.Fatal("Resolve custom-gpt = false")
	}
	if model.Provider != "openai" || model.API != ai.APIOpenAIResponses {
		t.Fatalf("custom model = %+v", model)
	}
	if model.BaseURL != "https://custom.example/v1" {
		t.Fatalf("BaseURL = %q", model.BaseURL)
	}
}

func TestCustomModelMissingBaseURLErrors(t *testing.T) {
	r := New(ai.BuiltinModels(), nil)
	err := r.ApplyConfig(ModelsConfig{Providers: map[string]ProviderConfig{
		"local": {
			API: new(string(ai.APIOpenAIResponses)),
			Models: []ModelDefinition{{
				ID: "local-model",
			}},
		},
	}})
	if err == nil {
		t.Fatal("ApplyConfig error = nil")
	}
	if !strings.Contains(err.Error(), "baseUrl") {
		t.Fatalf("error = %q, want baseUrl", err)
	}
	if r.LoadError() == nil {
		t.Fatal("LoadError = nil")
	}
}

func TestCustomProviderSupportsOpenAICompletionsAPI(t *testing.T) {
	r := New(ai.BuiltinModels(), nil)
	err := r.RegisterProvider("openai-compatible", ProviderConfig{
		API:     new(string(ai.APIOpenAICompletions)),
		BaseURL: new("https://local.example/v1"),
		APIKey:  new("local-key"),
		Models: []ModelDefinition{{
			ID:   "local-chat",
			Name: new("Local Chat"),
		}},
	})
	if err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}

	model, ok := r.Resolve("local-chat")
	if !ok {
		t.Fatal("Resolve local-chat = false")
	}
	if model.Provider != "openai-compatible" || model.API != ai.APIOpenAICompletions {
		t.Fatalf("model = %+v", model)
	}
	if model.BaseURL != "https://local.example/v1" {
		t.Fatalf("BaseURL = %q", model.BaseURL)
	}
}

func TestUnsupportedAPILoadsAndErrorsWhenUsed(t *testing.T) {
	cfg, err := ParseModelsConfig([]byte(`{
		"providers": {
			"anthropic-local": {
				"api": "anthropic-messages",
				"baseUrl": "https://anthropic.example/v1",
				"apiKey": "anthropic-key",
				"models": [
					{"id": "local-claude"}
				]
			},
			"google-local": {
				"api": "google",
				"baseUrl": "https://google.example/v1",
				"apiKey": "google-key",
				"models": [
					{"id": "local-gemini"}
				]
			}
		}
	}`))
	if err != nil {
		t.Fatalf("ParseModelsConfig: %v", err)
	}

	r := New(ai.BuiltinModels(), nil)
	if err := r.ApplyConfig(cfg); err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if r.LoadError() != nil {
		t.Fatalf("LoadError = %v", r.LoadError())
	}

	supported, ok := r.Resolve("local-claude")
	if !ok {
		t.Fatal("Resolve local-claude = false")
	}
	if supported.API != ai.APIAnthropicMessages {
		t.Fatalf("supported API = %q", supported.API)
	}
	if _, err := r.ResolveAuth(context.Background(), supported); err != nil {
		t.Fatalf("ResolveAuth supported: %v", err)
	}

	unsupported, ok := r.Resolve("local-gemini")
	if !ok {
		t.Fatal("Resolve local-gemini = false")
	}
	if unsupported.API != ai.API("google") {
		t.Fatalf("unsupported API = %q", unsupported.API)
	}
	_, err = r.ResolveAuth(context.Background(), unsupported)
	if err == nil {
		t.Fatal("ResolveAuth unsupported error = nil")
	}
	if !strings.Contains(err.Error(), `unsupported api "google"`) {
		t.Fatalf("error = %q", err)
	}
}

func TestPrecedenceRegisterOverridesConfigAndUnregisterRestores(t *testing.T) {
	r := New(ai.BuiltinModels(), nil)
	err := r.ApplyConfig(ModelsConfig{Providers: map[string]ProviderConfig{
		"openai": {BaseURL: new("https://config.example/v1")},
	}})
	if err != nil {
		t.Fatalf("ApplyConfig: %v", err)
	}
	if err := r.RegisterProvider("openai", ProviderConfig{
		BaseURL: new("https://extension.example/v1"),
	}); err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}

	model, _ := r.Resolve("gpt-5")
	if model.BaseURL != "https://extension.example/v1" {
		t.Fatalf("registered BaseURL = %q", model.BaseURL)
	}

	if err := r.UnregisterProvider("openai"); err != nil {
		t.Fatalf("UnregisterProvider: %v", err)
	}
	model, _ = r.Resolve("gpt-5")
	if model.BaseURL != "https://config.example/v1" {
		t.Fatalf("restored BaseURL = %q", model.BaseURL)
	}

	if err := r.RegisterProvider("local", ProviderConfig{
		API:     new(string(ai.APIOpenAIResponses)),
		BaseURL: new("https://local.example/v1"),
		APIKey:  new("local-key"),
		Models: []ModelDefinition{{
			ID: "local-model",
		}},
	}); err != nil {
		t.Fatalf("RegisterProvider local: %v", err)
	}
	if _, ok := r.Resolve("local-model"); !ok {
		t.Fatal("dynamic model missing before unregister")
	}
	if err := r.UnregisterProvider("local"); err != nil {
		t.Fatalf("UnregisterProvider local: %v", err)
	}
	if _, ok := r.Resolve("local-model"); ok {
		t.Fatal("dynamic model remained after unregister")
	}
}

func TestReregisterPreservesUnsetFields(t *testing.T) {
	r := New(ai.BuiltinModels(), nil)
	err := r.RegisterProvider("local", ProviderConfig{
		API:     new(string(ai.APIOpenAIResponses)),
		BaseURL: new("https://local.example/v1"),
		APIKey:  new("local-key"),
		Models: []ModelDefinition{{
			ID: "local-model",
		}},
	})
	if err != nil {
		t.Fatalf("RegisterProvider: %v", err)
	}

	err = r.RegisterProvider("local", ProviderConfig{
		Headers: map[string]string{"X-Trace": "trace-1"},
	})
	if err != nil {
		t.Fatalf("RegisterProvider second: %v", err)
	}

	model, ok := r.Resolve("local-model")
	if !ok {
		t.Fatal("Resolve local-model = false")
	}
	if model.BaseURL != "https://local.example/v1" {
		t.Fatalf("BaseURL = %q", model.BaseURL)
	}
	resolved, err := r.ResolveAuth(context.Background(), model)
	if err != nil {
		t.Fatalf("ResolveAuth: %v", err)
	}
	if resolved.APIKey != "local-key" || resolved.Headers["X-Trace"] != "trace-1" {
		t.Fatalf("auth = %+v", resolved)
	}
}

func TestResolveAuthEnvLiteralMissingHeadersAndAuthHeader(t *testing.T) {
	t.Setenv("LOCAL_API_KEY", "env-key")
	t.Setenv("LOCAL_HEADER", "header-value")

	r := New(ai.BuiltinModels(), missingAuth{})
	err := r.RegisterProvider("env-provider", ProviderConfig{
		API:        new(string(ai.APIOpenAIResponses)),
		BaseURL:    new("https://env.example/v1"),
		APIKey:     new("LOCAL_API_KEY"),
		AuthHeader: new(true),
		Headers:    map[string]string{"X-Env": "LOCAL_HEADER"},
		Models: []ModelDefinition{{
			ID: "env-model",
		}},
	})
	if err != nil {
		t.Fatalf("RegisterProvider env: %v", err)
	}
	model, _ := r.Resolve("env-model")
	resolved, err := r.ResolveAuth(context.Background(), model)
	if err != nil {
		t.Fatalf("ResolveAuth env: %v", err)
	}
	if resolved.APIKey != "env-key" {
		t.Fatalf("APIKey = %q", resolved.APIKey)
	}
	if resolved.Headers["X-Env"] != "header-value" {
		t.Fatalf("headers = %+v", resolved.Headers)
	}
	if resolved.Headers["Authorization"] != "Bearer env-key" {
		t.Fatalf("Authorization = %q", resolved.Headers["Authorization"])
	}

	err = r.RegisterProvider("literal-provider", ProviderConfig{
		API:     new(string(ai.APIAnthropicMessages)),
		BaseURL: new("https://literal.example/v1"),
		APIKey:  new("literal-key"),
		Models: []ModelDefinition{{
			ID: "literal-model",
		}},
	})
	if err != nil {
		t.Fatalf("RegisterProvider literal: %v", err)
	}
	model, _ = r.Resolve("literal-model")
	resolved, err = r.ResolveAuth(context.Background(), model)
	if err != nil {
		t.Fatalf("ResolveAuth literal: %v", err)
	}
	if resolved.APIKey != "literal-key" {
		t.Fatalf("literal APIKey = %q", resolved.APIKey)
	}

	missingRegistry := New(ai.BuiltinModels(), missingAuth{})
	missingModel, _ := missingRegistry.Resolve("claude-sonnet-4-5")
	_, err = missingRegistry.ResolveAuth(context.Background(), missingModel)
	if err == nil {
		t.Fatal("ResolveAuth missing error = nil")
	}
	if !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") || !strings.Contains(err.Error(), "/login anthropic") {
		t.Fatalf("missing error = %q", err)
	}
}

func TestAllAndAvailable(t *testing.T) {
	r := New(ai.BuiltinModels(), fakeAuth{keys: map[string]string{
		"anthropic": "anthropic-key",
	}})
	if len(r.All()) != len(ai.BuiltinModels()) {
		t.Fatalf("All len = %d", len(r.All()))
	}

	available := r.Available(context.Background())
	if len(available) == 0 {
		t.Fatal("Available returned 0 models")
	}
	for _, m := range available {
		if m.Provider != "anthropic" {
			t.Fatalf("Available returned non-anthropic model: %+v", m)
		}
	}
}

func TestParseModelsConfigToleratesCommentsAndTrailingCommas(t *testing.T) {
	cfg, err := ParseModelsConfig([]byte(`{
		// local provider
		"providers": {
			"local": {
				"baseUrl": "https://local.example//v1",
				"api": "openai-responses",
				"models": [
					{"id": "local-model",},
				],
			},
		},
	}`))
	if err != nil {
		t.Fatalf("ParseModelsConfig: %v", err)
	}
	provider := cfg.Providers["local"]
	if provider.BaseURL == nil || *provider.BaseURL != "https://local.example//v1" {
		t.Fatalf("provider = %+v", provider)
	}
	if len(provider.Models) != 1 || provider.Models[0].ID != "local-model" {
		t.Fatalf("models = %+v", provider.Models)
	}
}

type fakeAuth struct {
	keys map[string]string
}

func (f fakeAuth) Resolve(_ context.Context, provider string) (auth.Credential, error) {
	key, ok := f.keys[provider]
	if !ok {
		return auth.Credential{}, errors.New("missing")
	}
	return auth.Credential{Type: auth.TypeAPIKey, Key: key}, nil
}

type missingAuth struct{}

func (missingAuth) Resolve(_ context.Context, provider string) (auth.Credential, error) {
	env := []string{}
	if provider == "anthropic" {
		env = []string{"ANTHROPIC_API_KEY"}
	}
	return auth.Credential{}, &auth.MissingCredentialError{Provider: provider, Env: env}
}

type runtimeAuth struct {
	fakeAuth
	overrides map[string]string
}

func (r *runtimeAuth) SetRuntimeAPIKey(provider, apiKey string) {
	if r.overrides == nil {
		r.overrides = map[string]string{}
	}
	r.overrides[provider] = apiKey
}

func TestSetRuntimeAPIKeyPassesThroughToResolver(t *testing.T) {
	rec := &runtimeAuth{fakeAuth: fakeAuth{keys: map[string]string{}}}
	r := New(ai.BuiltinModels(), rec)
	r.SetRuntimeAPIKey("anthropic", "override-key")
	if rec.overrides["anthropic"] != "override-key" {
		t.Fatalf("override = %q, want override-key", rec.overrides["anthropic"])
	}

	// Resolvers that do not support overrides are a no-op (must not panic).
	New(ai.BuiltinModels(), missingAuth{}).SetRuntimeAPIKey("anthropic", "x")
	New(ai.BuiltinModels(), nil).SetRuntimeAPIKey("anthropic", "x")
}

//go:fix inline
func strPtr(value string) *string {
	return new(value)
}

//go:fix inline
func boolPtr(value bool) *bool {
	return new(value)
}
