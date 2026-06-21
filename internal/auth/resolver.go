package auth

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	aioauth "github.com/cunninghamcard-bit/Attention/internal/ai/oauth"
	"github.com/cunninghamcard-bit/Attention/internal/config"
)

var ProviderEnv = map[string][]string{
	"anthropic":              {"ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_API_KEY"},
	"openai":                 {"OPENAI_API_KEY"},
	"openai-codex":           {},
	"azure-openai-responses": {"AZURE_OPENAI_API_KEY"},
	"deepseek":               {"DEEPSEEK_API_KEY"},
	"google":                 {"GEMINI_API_KEY"},
	"google-vertex":          {"GOOGLE_CLOUD_API_KEY"},
	"mistral":                {"MISTRAL_API_KEY"},
	"groq":                   {"GROQ_API_KEY"},
	"cerebras":               {"CEREBRAS_API_KEY"},
	"cloudflare-ai-gateway":  {"CLOUDFLARE_API_KEY"},
	"cloudflare-workers-ai":  {"CLOUDFLARE_API_KEY"},
	"xai":                    {"XAI_API_KEY"},
	"openrouter":             {"OPENROUTER_API_KEY"},
	"vercel-ai-gateway":      {"AI_GATEWAY_API_KEY"},
	"zai":                    {"ZAI_API_KEY"},
	"opencode":               {"OPENCODE_API_KEY"},
	"opencode-go":            {"OPENCODE_API_KEY"},
	"huggingface":            {"HF_TOKEN"},
	"fireworks":              {"FIREWORKS_API_KEY"},
	"together":               {"TOGETHER_API_KEY"},
	"kimi-coding":            {"KIMI_API_KEY"},
	"minimax":                {"MINIMAX_API_KEY"},
	"minimax-cn":             {"MINIMAX_CN_API_KEY"},
	"moonshotai":             {"MOONSHOT_API_KEY"},
	"moonshotai-cn":          {"MOONSHOT_API_KEY"},
	"xiaomi":                 {"XIAOMI_API_KEY"},
	"xiaomi-token-plan-cn":   {"XIAOMI_TOKEN_PLAN_CN_API_KEY"},
	"xiaomi-token-plan-ams":  {"XIAOMI_TOKEN_PLAN_AMS_API_KEY"},
	"xiaomi-token-plan-sgp":  {"XIAOMI_TOKEN_PLAN_SGP_API_KEY"},
	"github-copilot":         {"COPILOT_GITHUB_TOKEN"},
}

type Resolver struct {
	Store Store
	Env   map[string][]string
	Now   func() time.Time

	FallbackResolver func(provider string) string

	refreshOAuth refreshOAuthFunc

	mu               sync.Mutex
	runtimeOverrides map[string]string
	refreshLocks     map[string]*sync.Mutex
}

type MissingCredentialError struct {
	Provider string
	Env      []string
}

type refreshOAuthFunc func(context.Context, string, OAuthCredential) (OAuthCredential, error)

func NewResolver(store Store) *Resolver {
	return &Resolver{
		Store:        store,
		Env:          ProviderEnv,
		Now:          time.Now,
		refreshOAuth: refreshOAuthCredential,
	}
}

func EnvVars(provider string) []string {
	return copyEnvVars(ProviderEnv[provider])
}

func (r *Resolver) Resolve(ctx context.Context, provider string) (Credential, error) {
	if provider == "" {
		return Credential{}, fmt.Errorf("provider is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Match pi's runtime override priority for CLI --api-key.
	// See .agents/references/pi/packages/coding-agent/src/core/auth-storage.ts:227-235,463-466.
	if key, ok := r.runtimeAPIKey(provider); ok {
		return Credential{
			Type: TypeAPIKey,
			Key:  key,
		}, nil
	}

	envVars := r.envVars(provider)
	store, err := r.store()
	if err != nil {
		return Credential{}, err
	}

	cred, ok, err := getStoreCredential(store, provider)
	if err != nil {
		// pi records the parse error and resolves as if no credential were
		// stored, so env vars and fallback providers keep working even with a
		// corrupt auth.json (auth-storage.ts:261-274,513-519).
		ok = false
	}
	// Match pi auth priority: auth.json API key/OAuth before env fallback.
	// See .agents/references/pi/packages/coding-agent/src/core/auth-storage.ts:453-515.
	if ok {
		return r.resolveStoredCredential(ctx, store, provider, cred)
	}

	for _, envVar := range envVars {
		if value := os.Getenv(envVar); value != "" {
			return Credential{
				Type: TypeAPIKey,
				Key:  value,
			}, nil
		}
	}

	// Match pi's custom provider fallback after auth.json and env vars.
	// See .agents/references/pi/packages/coding-agent/src/core/auth-storage.ts:517-519.
	if key := r.fallbackAPIKey(provider); key != "" {
		return Credential{
			Type: TypeAPIKey,
			Key:  key,
		}, nil
	}

	return Credential{}, &MissingCredentialError{
		Provider: provider,
		Env:      envVars,
	}
}

// SetRuntimeAPIKey sets an in-memory API-key override without persisting it.
// Mirrors pi AuthStorage.setRuntimeApiKey.
// See .agents/references/pi/packages/coding-agent/src/core/auth-storage.ts:223-229.
func (r *Resolver) SetRuntimeAPIKey(provider, apiKey string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.runtimeOverrides == nil {
		r.runtimeOverrides = map[string]string{}
	}
	r.runtimeOverrides[provider] = apiKey
}

// ClearRuntimeAPIKey removes an in-memory API-key override.
// Mirrors pi AuthStorage.removeRuntimeApiKey.
// See .agents/references/pi/packages/coding-agent/src/core/auth-storage.ts:231-235.
func (r *Resolver) ClearRuntimeAPIKey(provider string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.runtimeOverrides, provider)
}

func (e *MissingCredentialError) Error() string {
	login := fmt.Sprintf("/login %s", e.Provider)
	if len(e.Env) == 0 {
		return fmt.Sprintf("missing credential for provider %q; run `%s`", e.Provider, login)
	}
	return fmt.Sprintf(
		"missing credential for provider %q; set %s or run `%s`",
		e.Provider,
		strings.Join(e.Env, " or "),
		login,
	)
}

func (r *Resolver) envVars(provider string) []string {
	if r.Env == nil {
		return copyEnvVars(ProviderEnv[provider])
	}
	return copyEnvVars(r.Env[provider])
}

func (r *Resolver) store() (Store, error) {
	if r.Store != nil {
		return r.Store, nil
	}
	store, err := NewStore("")
	if err != nil {
		return nil, err
	}
	return store, nil
}

func (r *Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Resolver) refresher() refreshOAuthFunc {
	if r.refreshOAuth != nil {
		return r.refreshOAuth
	}
	return refreshOAuthCredential
}

func (r *Resolver) resolveStoredCredential(
	ctx context.Context,
	store Store,
	provider string,
	cred Credential,
) (Credential, error) {
	switch cred.Type {
	case TypeAPIKey:
		return resolveStoredAPIKey(provider, cred)
	case TypeOAuth:
		return r.resolveOAuth(ctx, store, provider, cred)
	default:
		return Credential{}, fmt.Errorf("unsupported credential type %q for %q", cred.Type, provider)
	}
}

func resolveStoredAPIKey(provider string, cred Credential) (Credential, error) {
	key := config.ResolveValue(cred.Key)
	if key == "" {
		return Credential{}, fmt.Errorf("stored api_key credential for %q resolved to empty value", provider)
	}
	return Credential{
		Type: TypeAPIKey,
		Key:  key,
	}, nil
}

func (r *Resolver) runtimeAPIKey(provider string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := r.runtimeOverrides[provider]
	return key, key != ""
}

func (r *Resolver) fallbackAPIKey(provider string) string {
	if r.FallbackResolver == nil {
		return ""
	}
	return r.FallbackResolver(provider)
}

func (r *Resolver) refreshLock(provider string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.refreshLocks == nil {
		r.refreshLocks = map[string]*sync.Mutex{}
	}
	if r.refreshLocks[provider] == nil {
		r.refreshLocks[provider] = &sync.Mutex{}
	}
	return r.refreshLocks[provider]
}

func (r *Resolver) resolveOAuth(
	ctx context.Context,
	store Store,
	provider string,
	cred Credential,
) (Credential, error) {
	if cred.OAuth == nil {
		return Credential{}, fmt.Errorf("stored oauth credential for %q is missing token fields", provider)
	}
	if _, ok := aioauth.GetProvider(provider); !ok {
		// pi: an oauth credential for an unregistered provider yields no API
		// key — the raw access token (e.g. a migrated github token) must
		// never be sent as one (auth-storage.ts:476-480).
		return Credential{}, &MissingCredentialError{Provider: provider, Env: r.envVars(provider)}
	}

	current := *cred.OAuth
	if r.now().UnixMilli() < current.Expires {
		return resolvedOAuthCredential(provider, current), nil
	}

	lock := r.refreshLock(provider)
	lock.Lock()
	defer lock.Unlock()

	return r.refreshExpiredOAuth(ctx, store, provider, current)
}

// lockedUpdater is implemented by stores that can run a read-modify-write
// sequence under their cross-process lock.
type lockedUpdater interface {
	UpdateLocked(ctx context.Context, fn func(data map[string]Credential) (map[string]Credential, error)) error
}

func (r *Resolver) refreshExpiredOAuth(
	ctx context.Context,
	store Store,
	provider string,
	current OAuthCredential,
) (Credential, error) {
	// pi's refreshOAuthTokenWithLock holds the cross-process file lock across
	// re-read, network refresh, and persist, so concurrent instances never
	// double-refresh and lose a rotated refresh token
	// (auth-storage.ts:404-448).
	if updater, ok := store.(lockedUpdater); ok {
		return r.refreshUnderLock(ctx, updater, store, provider, current)
	}

	// In-memory stores (tests, extension-provided): locked re-read before
	// refresh, same sequence without a file lock.
	latest, ok, err := r.rereadOAuth(store, provider)
	if err != nil {
		return Credential{}, fmt.Errorf("read auth credential for %q before oauth refresh: %w", provider, err)
	}
	if ok {
		current = latest
		if r.now().UnixMilli() < current.Expires {
			return resolvedOAuthCredential(provider, current), nil
		}
	}

	refreshed, err := r.refresher()(ctx, provider, current)
	if err != nil {
		return r.refreshFailed(store, provider)
	}

	next := resolvedOAuthCredential(provider, refreshed)
	if err := store.Set(ctx, provider, next); err != nil {
		return Credential{}, fmt.Errorf("persist refreshed oauth credential for %q: %w", provider, err)
	}
	return next, nil
}

func (r *Resolver) refreshUnderLock(
	ctx context.Context,
	updater lockedUpdater,
	store Store,
	provider string,
	current OAuthCredential,
) (Credential, error) {
	var resolved Credential
	var refreshErr error
	err := updater.UpdateLocked(ctx, func(data map[string]Credential) (map[string]Credential, error) {
		if cred, ok := data[provider]; ok && cred.Type == TypeOAuth && cred.OAuth != nil {
			current = *cred.OAuth
		}
		if r.now().UnixMilli() < current.Expires {
			// Another instance already refreshed while we waited on the lock.
			resolved = resolvedOAuthCredential(provider, current)
			return nil, nil
		}
		refreshed, err := r.refresher()(ctx, provider, current)
		if err != nil {
			refreshErr = err
			return nil, nil
		}
		resolved = resolvedOAuthCredential(provider, refreshed)
		data[provider] = resolved
		return data, nil
	})
	if err != nil {
		return Credential{}, fmt.Errorf("refresh oauth credential for %q: %w", provider, err)
	}
	if refreshErr != nil {
		return r.refreshFailed(store, provider)
	}
	return resolved, nil
}

// refreshFailed mirrors pi's refresh-failure path: re-read in case another
// instance refreshed, otherwise treat the provider exactly like an
// unconfigured one — the stored credentials stay for a later /login retry
// (auth-storage.ts:492-506).
func (r *Resolver) refreshFailed(store Store, provider string) (Credential, error) {
	if reread, ok := r.rereadFreshOAuth(store, provider); ok {
		return reread, nil
	}
	return Credential{}, &MissingCredentialError{Provider: provider, Env: r.envVars(provider)}
}

func (r *Resolver) rereadFreshOAuth(store Store, provider string) (Credential, bool) {
	current, ok, err := r.rereadOAuth(store, provider)
	if err != nil || !ok || r.now().UnixMilli() >= current.Expires {
		return Credential{}, false
	}
	return resolvedOAuthCredential(provider, current), true
}

func (r *Resolver) rereadOAuth(store Store, provider string) (OAuthCredential, bool, error) {
	cred, ok, err := getStoreCredential(store, provider)
	if err != nil || !ok || cred.Type != TypeOAuth || cred.OAuth == nil {
		return OAuthCredential{}, false, err
	}
	return *cred.OAuth, true, nil
}

func resolvedOAuthCredential(provider string, current OAuthCredential) Credential {
	return Credential{
		Type:  TypeOAuth,
		Key:   oauthAPIKey(provider, current),
		OAuth: &current,
	}
}

func oauthAPIKey(provider string, current OAuthCredential) string {
	p, ok := aioauth.GetProvider(provider)
	if !ok {
		// pi: unknown OAuth provider, can't get API key (auth-storage.ts:476-480).
		return ""
	}
	return p.GetAPIKey(aioauth.Credentials{
		Refresh:   current.Refresh,
		Access:    current.Access,
		Expires:   current.Expires,
		AccountID: current.AccountID,
	})
}

func getStoreCredential(store Store, provider string) (Credential, bool, error) {
	if storeWithError, ok := store.(interface {
		GetError(string) (Credential, bool, error)
	}); ok {
		return storeWithError.GetError(provider)
	}

	cred, ok := store.Get(provider)
	return cred, ok, nil
}

func refreshOAuthCredential(
	ctx context.Context,
	provider string,
	cred OAuthCredential,
) (OAuthCredential, error) {
	p, ok := aioauth.GetProvider(provider)
	if !ok {
		return OAuthCredential{}, fmt.Errorf("oauth refresh is not supported for provider %q", provider)
	}
	refreshed, err := p.RefreshToken(ctx, cred.Refresh)
	if err != nil {
		return OAuthCredential{}, err
	}
	return OAuthCredential{
		Refresh:   refreshed.Refresh,
		Access:    refreshed.Access,
		Expires:   refreshed.Expires,
		AccountID: refreshed.AccountID,
	}, nil
}

func copyEnvVars(vars []string) []string {
	if len(vars) == 0 {
		return []string{}
	}
	copied := make([]string, len(vars))
	copy(copied, vars)
	return copied
}
