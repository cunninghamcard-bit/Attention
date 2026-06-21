package oauth

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Provider mirrors pi's OAuthProviderInterface.
// See .agents/references/pi/packages/ai/src/utils/oauth/types.ts:54-72.
type Provider interface {
	ID() string
	Name() string
	Login(ctx context.Context, callbacks LoginCallbacks) (Credentials, error)
	RefreshToken(ctx context.Context, refresh string) (Credentials, error)
	GetAPIKey(creds Credentials) string
}

var (
	providersMu sync.Mutex
	providers   = map[string]Provider{}
)

var builtInProviders = []Provider{
	anthropicProvider{},
	openAICodexProvider{},
}

func init() {
	RegisterBuiltins()
}

// RegisterBuiltins registers the OAuth providers shipped with along.
func RegisterBuiltins() {
	for _, provider := range builtInProviders {
		RegisterProvider(provider)
	}
}

// RegisterProvider adds or replaces an OAuth provider in the package registry.
// See .agents/references/pi/packages/ai/src/utils/oauth/index.ts:41-57.
func RegisterProvider(p Provider) {
	providersMu.Lock()
	defer providersMu.Unlock()

	providers[p.ID()] = p
}

// GetProvider returns the OAuth provider registered for id.
func GetProvider(id string) (Provider, bool) {
	providersMu.Lock()
	defer providersMu.Unlock()

	provider, ok := providers[id]
	return provider, ok
}

// Login runs the registered provider's login flow with the given callbacks and
// returns credentials to persist. Mirrors pi's getOAuthProvider(id).login(...).
// See .agents/references/pi/packages/ai/src/utils/oauth/index.ts.
func Login(ctx context.Context, id string, callbacks LoginCallbacks) (Credentials, error) {
	provider, ok := GetProvider(id)
	if !ok {
		return Credentials{}, fmt.Errorf("oauth login is not supported for provider %q", id)
	}
	return provider.Login(ctx, callbacks)
}

// Providers returns all registered OAuth providers sorted by provider id.
// See .agents/references/pi/packages/ai/src/utils/oauth/index.ts:84-89.
func Providers() []Provider {
	providersMu.Lock()
	defer providersMu.Unlock()

	ids := make([]string, 0, len(providers))
	for id := range providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	result := make([]Provider, 0, len(ids))
	for _, id := range ids {
		result = append(result, providers[id])
	}
	return result
}

// UnregisterProvider removes an OAuth provider from the registry.
// Call RegisterBuiltins to restore built-ins after tests that need a clean registry.
func UnregisterProvider(id string) {
	providersMu.Lock()
	defer providersMu.Unlock()

	delete(providers, id)
}

type anthropicProvider struct{}

func (anthropicProvider) ID() string {
	return "anthropic"
}

func (anthropicProvider) Name() string {
	return "Anthropic"
}

func (anthropicProvider) Login(
	ctx context.Context,
	callbacks LoginCallbacks,
) (Credentials, error) {
	return LoginAnthropic(ctx, callbacks)
}

func (anthropicProvider) RefreshToken(ctx context.Context, refresh string) (Credentials, error) {
	return RefreshAnthropicToken(ctx, refresh)
}

func (anthropicProvider) GetAPIKey(creds Credentials) string {
	return creds.Access
}

type openAICodexProvider struct{}

func (openAICodexProvider) ID() string {
	return "openai-codex"
}

func (openAICodexProvider) Name() string {
	return "OpenAI Codex"
}

func (openAICodexProvider) Login(
	ctx context.Context,
	callbacks LoginCallbacks,
) (Credentials, error) {
	return LoginOpenAICodex(ctx, callbacks)
}

func (openAICodexProvider) RefreshToken(ctx context.Context, refresh string) (Credentials, error) {
	return RefreshOpenAICodexToken(ctx, refresh)
}

func (openAICodexProvider) GetAPIKey(creds Credentials) string {
	return creds.Access
}
