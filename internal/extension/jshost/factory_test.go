package jshost

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	aioauth "github.com/cunninghamcard-bit/Attention/internal/ai/oauth"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
)

func TestExtensionFactoryRegistersLoadedProvider(t *testing.T) {
	host := &Host{
		writer:             newJSONLineWriter(io.Discard),
		pendingInvocations: map[string]*invocationWaiter{},
	}
	t.Cleanup(func() {
		host.closeWithError(errHostClosed)
	})

	const path = "/test/provider-extension.ts"
	preserveOAuthProvider(t, "js-openai")
	resultCh := make(chan loadedExtensionResult, 1)
	go func() {
		ext, err := extension.Load(
			path,
			hook.NewRegistry(),
			func(context.Context) extension.ExtensionContext {
				return extension.ExtensionContext{}
			},
			ExtensionFactory(host, path),
		)
		resultCh <- loadedExtensionResult{ext: ext, err: err}
	}()

	waitForPendingLoad(t, host)

	displayName := "JS OpenAI"
	baseURL := "https://provider.test/v1"
	apiKey := "JS_OPENAI_KEY"
	apiName := string(ai.APIOpenAIResponses)
	authHeader := true
	reasoning := true
	contextWindow := 123
	maxTokens := 45
	cacheRead := 0.5
	overrideName := "GPT override"
	oauthName := "Batch 3b"

	host.deliverLoaded(loadedMessage{
		Type: "loaded",
		Providers: []ProviderInfo{
			{
				Name: "js-openai",
				Config: ProviderConfigInfo{
					Name:       &displayName,
					BaseURL:    &baseURL,
					APIKey:     &apiKey,
					API:        &apiName,
					Headers:    map[string]string{"X-Provider": "js"},
					AuthHeader: &authHeader,
					Compat: &ai.Compat{
						SendSessionIdHeader: &authHeader,
					},
					Models: []ProviderModelInfo{
						{
							ID:            "js-gpt",
							Reasoning:     &reasoning,
							Input:         []ai.InputCapability{ai.InputText, ai.InputImage},
							Cost:          &ProviderModelCostInfo{CacheRead: &cacheRead},
							ContextWindow: &contextWindow,
							MaxTokens:     &maxTokens,
							Headers:       map[string]string{"X-Model": "js-model"},
						},
					},
					ModelOverrides: map[string]ProviderModelOverrideInfo{
						"gpt-5": {
							Name:    &overrideName,
							Headers: map[string]string{"X-Override": "js-override"},
						},
					},
					OAuth: &ProviderOAuthInfo{Name: oauthName},
				},
			},
		},
	})

	result := receiveLoadedExtensionResult(t, resultCh)
	if result.err != nil {
		t.Fatalf("Load error: %v", result.err)
	}

	provider, ok := result.ext.Providers["js-openai"]
	if !ok {
		t.Fatalf("providers = %#v, want js-openai", result.ext.Providers)
	}
	if provider.OAuth == nil || provider.OAuth.Name != oauthName {
		t.Fatalf("provider OAuth = %#v, want %q", provider.OAuth, oauthName)
	}
	registered, ok := aioauth.GetProvider("js-openai")
	if !ok {
		t.Fatal("oauth provider js-openai was not registered")
	}
	jsProvider, ok := registered.(jsOAuthProvider)
	if !ok {
		t.Fatalf("oauth provider type = %T, want jsOAuthProvider", registered)
	}
	if jsProvider.host != host || jsProvider.Name() != oauthName {
		t.Fatalf("oauth provider = %#v, want host and name %q", jsProvider, oauthName)
	}
	if provider.Name == nil || *provider.Name != displayName {
		t.Fatalf("provider name = %v, want %q", provider.Name, displayName)
	}
	if provider.BaseURL == nil || *provider.BaseURL != baseURL {
		t.Fatalf("provider baseURL = %v, want %q", provider.BaseURL, baseURL)
	}
	if provider.API == nil || *provider.API != apiName {
		t.Fatalf("provider API = %v, want %q", provider.API, apiName)
	}
	if provider.AuthHeader == nil || !*provider.AuthHeader {
		t.Fatalf("provider AuthHeader = %v, want true", provider.AuthHeader)
	}
	if provider.Headers["X-Provider"] != "js" {
		t.Fatalf("provider headers = %#v, want X-Provider", provider.Headers)
	}
	if provider.Compat == nil || provider.Compat.SendSessionIdHeader == nil || !*provider.Compat.SendSessionIdHeader {
		t.Fatalf("provider compat = %#v, want sendSessionIdHeader", provider.Compat)
	}
	if len(provider.Models) != 1 || provider.Models[0].ID != "js-gpt" {
		t.Fatalf("provider models = %#v, want js-gpt", provider.Models)
	}
	if provider.Models[0].ContextWindow == nil || *provider.Models[0].ContextWindow != contextWindow {
		t.Fatalf("model ContextWindow = %v, want %d", provider.Models[0].ContextWindow, contextWindow)
	}
	if provider.Models[0].Cost == nil || provider.Models[0].Cost.CacheRead == nil ||
		*provider.Models[0].Cost.CacheRead != cacheRead {
		t.Fatalf("model cost = %#v, want cacheRead %v", provider.Models[0].Cost, cacheRead)
	}
	override, ok := provider.ModelOverrides["gpt-5"]
	if !ok || override.Name == nil || *override.Name != overrideName {
		t.Fatalf("model overrides = %#v, want gpt-5 override", provider.ModelOverrides)
	}
}

func preserveOAuthProvider(t *testing.T, id string) {
	t.Helper()

	original, ok := aioauth.GetProvider(id)
	t.Cleanup(func() {
		aioauth.UnregisterProvider(id)
		if ok {
			aioauth.RegisterProvider(original)
		}
	})
}

type loadedExtensionResult struct {
	ext extension.Extension
	err error
}

func waitForPendingLoad(t *testing.T, host *Host) {
	t.Helper()

	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()

	for {
		host.mu.Lock()
		ready := host.pendingLoad != nil
		host.mu.Unlock()
		if ready {
			return
		}

		select {
		case <-deadline:
			t.Fatal("host pending load was not registered")
		case <-tick.C:
		}
	}
}

func receiveLoadedExtensionResult(
	t *testing.T,
	resultCh <-chan loadedExtensionResult,
) loadedExtensionResult {
	t.Helper()

	select {
	case result := <-resultCh:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("extension factory did not finish")
	}
	return loadedExtensionResult{}
}
