package auth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	aioauth "github.com/cunninghamcard-bit/Attention/internal/ai/oauth"
)

func TestStoreRoundTripAPIKeyAndOAuth(t *testing.T) {
	store := newTempStore(t)
	ctx := context.Background()

	if err := store.Set(ctx, "anthropic", Credential{
		Type: TypeAPIKey,
		Key:  "sk-ant-test",
	}); err != nil {
		t.Fatalf("Set api key: %v", err)
	}
	oauth := OAuthCredential{
		Refresh:   "refresh-1",
		Access:    "access-1",
		Expires:   123456789,
		AccountID: "acct-1",
	}
	if err := store.Set(ctx, "openai-codex", Credential{
		Type:  TypeOAuth,
		OAuth: &oauth,
	}); err != nil {
		t.Fatalf("Set oauth: %v", err)
	}

	reopened, err := NewStore(store.Path())
	if err != nil {
		t.Fatalf("NewStore reopen: %v", err)
	}

	apiKey, ok := reopened.Get("anthropic")
	if !ok {
		t.Fatal("anthropic credential missing")
	}
	if apiKey.Type != TypeAPIKey || apiKey.Key != "sk-ant-test" {
		t.Fatalf("api key credential = %+v", apiKey)
	}

	oauthGot, ok := reopened.Get("openai-codex")
	if !ok {
		t.Fatal("openai-codex credential missing")
	}
	if oauthGot.Type != TypeOAuth ||
		oauthGot.OAuth == nil ||
		oauthGot.OAuth.Refresh != "refresh-1" ||
		oauthGot.OAuth.Access != "access-1" ||
		oauthGot.OAuth.Expires != 123456789 ||
		oauthGot.OAuth.AccountID != "acct-1" {
		t.Fatalf("oauth credential = %+v", oauthGot)
	}

	raw := readAuthJSON(t, store.Path())
	if raw["anthropic"]["type"] != TypeAPIKey || raw["anthropic"]["key"] != "sk-ant-test" {
		t.Fatalf("anthropic wire = %+v", raw["anthropic"])
	}
	if raw["openai-codex"]["type"] != TypeOAuth ||
		raw["openai-codex"]["refresh"] != "refresh-1" ||
		raw["openai-codex"]["access"] != "access-1" ||
		raw["openai-codex"]["expires"] != float64(123456789) ||
		raw["openai-codex"]["accountId"] != "acct-1" {
		t.Fatalf("openai-codex wire = %+v", raw["openai-codex"])
	}
	if _, ok := raw["openai-codex"]["OAuth"]; ok {
		t.Fatalf("oauth credential persisted nested OAuth field: %+v", raw["openai-codex"])
	}
}

func TestStoreWritesAuthJSONMode0600(t *testing.T) {
	store := newTempStore(t)

	if err := store.Set(context.Background(), "openai", Credential{
		Type: TypeAPIKey,
		Key:  "sk-test",
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("auth.json mode = %o, want 0600", got)
	}
}

func TestResolverUsesStoreBeforeEnvironment(t *testing.T) {
	store := newTempStore(t)
	if err := store.Set(context.Background(), "openai", Credential{
		Type: TypeAPIKey,
		Key:  "store-key",
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	t.Setenv("OPENAI_API_KEY", "env-key")

	cred, err := NewResolver(store).Resolve(context.Background(), "openai")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.Type != TypeAPIKey || cred.Key != "store-key" {
		t.Fatalf("credential = %+v, want stored api key", cred)
	}
}

func TestResolverUsesEnvironmentFallbackWhenStoreMissing(t *testing.T) {
	store := newTempStore(t)
	t.Setenv("OPENAI_API_KEY", "env-key")

	cred, err := NewResolver(store).Resolve(context.Background(), "openai")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.Type != TypeAPIKey || cred.Key != "env-key" {
		t.Fatalf("credential = %+v, want env api key", cred)
	}
}

func TestResolverRuntimeAPIKeyOverrideHasHighestPriority(t *testing.T) {
	t.Run("stored api key and environment", func(t *testing.T) {
		store := newTempStore(t)
		if err := store.Set(context.Background(), "openai", Credential{
			Type: TypeAPIKey,
			Key:  "store-key",
		}); err != nil {
			t.Fatalf("Set: %v", err)
		}
		t.Setenv("OPENAI_API_KEY", "env-key")

		resolver := NewResolver(store)
		resolver.SetRuntimeAPIKey("openai", "runtime-key")

		cred, err := resolver.Resolve(context.Background(), "openai")
		if err != nil {
			t.Fatalf("Resolve with override: %v", err)
		}
		if cred.Type != TypeAPIKey || cred.Key != "runtime-key" {
			t.Fatalf("credential = %+v, want runtime api key", cred)
		}

		resolver.ClearRuntimeAPIKey("openai")
		cred, err = resolver.Resolve(context.Background(), "openai")
		if err != nil {
			t.Fatalf("Resolve after clear: %v", err)
		}
		if cred.Type != TypeAPIKey || cred.Key != "store-key" {
			t.Fatalf("credential after clear = %+v, want stored api key", cred)
		}
	})

	t.Run("stored oauth and environment", func(t *testing.T) {
		store := newTempStore(t)
		now := time.UnixMilli(10_000)
		stored := OAuthCredential{
			Refresh: "refresh-store",
			Access:  "access-store",
			Expires: now.Add(time.Hour).UnixMilli(),
		}
		if err := store.Set(context.Background(), "openai-codex", Credential{
			Type:  TypeOAuth,
			OAuth: &stored,
		}); err != nil {
			t.Fatalf("Set: %v", err)
		}
		t.Setenv("OPENAI_CODEX_API_KEY", "env-key")

		resolver := NewResolver(store)
		resolver.Env = map[string][]string{
			"openai-codex": {"OPENAI_CODEX_API_KEY"},
		}
		resolver.Now = func() time.Time {
			return now
		}
		resolver.SetRuntimeAPIKey("openai-codex", "runtime-key")

		cred, err := resolver.Resolve(context.Background(), "openai-codex")
		if err != nil {
			t.Fatalf("Resolve with override: %v", err)
		}
		if cred.Type != TypeAPIKey || cred.Key != "runtime-key" {
			t.Fatalf("credential = %+v, want runtime api key", cred)
		}

		resolver.ClearRuntimeAPIKey("openai-codex")
		cred, err = resolver.Resolve(context.Background(), "openai-codex")
		if err != nil {
			t.Fatalf("Resolve after clear: %v", err)
		}
		if cred.Type != TypeOAuth || cred.Key != "access-store" {
			t.Fatalf("credential after clear = %+v, want stored oauth", cred)
		}
	})
}

func TestResolverUsesFallbackResolverAfterStoreAndEnvironment(t *testing.T) {
	t.Run("uses fallback when store and environment are missing", func(t *testing.T) {
		store := newTempStore(t)
		var calls int
		resolver := NewResolver(store)
		resolver.FallbackResolver = func(provider string) string {
			calls++
			if provider != "custom-provider" {
				t.Fatalf("fallback provider = %q, want custom-provider", provider)
			}
			return "fallback-key"
		}

		cred, err := resolver.Resolve(context.Background(), "custom-provider")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if cred.Type != TypeAPIKey || cred.Key != "fallback-key" {
			t.Fatalf("credential = %+v, want fallback api key", cred)
		}
		if calls != 1 {
			t.Fatalf("fallback calls = %d, want 1", calls)
		}
	})

	t.Run("does not consult fallback when store credential exists", func(t *testing.T) {
		store := newTempStore(t)
		if err := store.Set(context.Background(), "custom-provider", Credential{
			Type: TypeAPIKey,
			Key:  "store-key",
		}); err != nil {
			t.Fatalf("Set: %v", err)
		}
		var calls int
		resolver := NewResolver(store)
		resolver.FallbackResolver = func(provider string) string {
			calls++
			return "fallback-key"
		}

		cred, err := resolver.Resolve(context.Background(), "custom-provider")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if cred.Type != TypeAPIKey || cred.Key != "store-key" {
			t.Fatalf("credential = %+v, want stored api key", cred)
		}
		if calls != 0 {
			t.Fatalf("fallback calls = %d, want 0", calls)
		}
	})

	t.Run("does not consult fallback when environment credential exists", func(t *testing.T) {
		store := newTempStore(t)
		t.Setenv("CUSTOM_PROVIDER_API_KEY", "env-key")
		var calls int
		resolver := NewResolver(store)
		resolver.Env = map[string][]string{
			"custom-provider": {"CUSTOM_PROVIDER_API_KEY"},
		}
		resolver.FallbackResolver = func(provider string) string {
			calls++
			return "fallback-key"
		}

		cred, err := resolver.Resolve(context.Background(), "custom-provider")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if cred.Type != TypeAPIKey || cred.Key != "env-key" {
			t.Fatalf("credential = %+v, want env api key", cred)
		}
		if calls != 0 {
			t.Fatalf("fallback calls = %d, want 0", calls)
		}
	})
}

func TestResolverUsesAuthJSON(t *testing.T) {
	store := newTempStore(t)
	if err := store.Set(context.Background(), "zai", Credential{
		Type: TypeAPIKey,
		Key:  "store-key",
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	t.Setenv("ZAI_API_KEY", "")

	cred, err := NewResolver(store).Resolve(context.Background(), "zai")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cred.Type != TypeAPIKey || cred.Key != "store-key" {
		t.Fatalf("credential = %+v, want stored api key", cred)
	}
}

func TestResolverMissingCredentialErrorMentionsEnvAndLogin(t *testing.T) {
	store := newTempStore(t)
	t.Setenv("MISTRAL_API_KEY", "")

	_, err := NewResolver(store).Resolve(context.Background(), "mistral")
	if err == nil {
		t.Fatal("Resolve error = nil, want missing credential error")
	}

	var missing *MissingCredentialError
	if !errors.As(err, &missing) {
		t.Fatalf("error = %T %v, want MissingCredentialError", err, err)
	}
	if missing.Provider != "mistral" {
		t.Fatalf("missing provider = %q, want mistral", missing.Provider)
	}
	if !strings.Contains(err.Error(), "MISTRAL_API_KEY") || !strings.Contains(err.Error(), "/login mistral") {
		t.Fatalf("error = %q, want env and login guidance", err)
	}
}

func TestResolverRefreshesExpiredOAuthWithStub(t *testing.T) {
	store := newTempStore(t)
	now := time.UnixMilli(10_000)
	expired := OAuthCredential{
		Refresh: "refresh-old",
		Access:  "access-old",
		Expires: now.Add(-time.Minute).UnixMilli(),
	}
	if err := store.Set(context.Background(), "openai-codex", Credential{
		Type:  TypeOAuth,
		OAuth: &expired,
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	resolver := NewResolver(store)
	resolver.Now = func() time.Time {
		return now
	}
	var refreshed bool
	resolver.refreshOAuth = func(
		ctx context.Context,
		provider string,
		cred OAuthCredential,
	) (OAuthCredential, error) {
		if provider != "openai-codex" {
			t.Fatalf("provider = %q, want openai-codex", provider)
		}
		if cred.Refresh != "refresh-old" || cred.Access != "access-old" {
			t.Fatalf("refresh input = %+v", cred)
		}
		refreshed = true
		return OAuthCredential{
			Refresh: "refresh-new",
			Access:  "access-new",
			Expires: now.Add(time.Hour).UnixMilli(),
		}, nil
	}

	cred, err := resolver.Resolve(context.Background(), "openai-codex")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !refreshed {
		t.Fatal("refresh stub was not called")
	}
	if cred.Type != TypeOAuth || cred.Key != "access-new" || cred.OAuth == nil || cred.OAuth.Refresh != "refresh-new" {
		t.Fatalf("credential = %+v, want refreshed oauth", cred)
	}

	stored, ok := store.Get("openai-codex")
	if !ok || stored.OAuth == nil || stored.OAuth.Access != "access-new" {
		t.Fatalf("stored credential = %+v, want refreshed oauth", stored)
	}
}

func TestRefreshOAuthCredentialUsesProviderRegistry(t *testing.T) {
	tests := []struct {
		name     string
		provider string
	}{
		{name: "anthropic", provider: "anthropic"},
		{name: "openai codex", provider: "openai-codex"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var called bool
			replaceOAuthProvider(t, testOAuthProvider{
				id:   tt.provider,
				name: tt.name,
				refresh: func(ctx context.Context, refresh string) (aioauth.Credentials, error) {
					called = true
					if refresh != "refresh-old" {
						t.Fatalf("refresh token = %q, want refresh-old", refresh)
					}
					return aioauth.Credentials{
						Refresh:   "refresh-new",
						Access:    "access-new",
						Expires:   20_000,
						AccountID: "acct-new",
					}, nil
				},
			})

			credential, err := refreshOAuthCredential(context.Background(), tt.provider, OAuthCredential{
				Refresh: "refresh-old",
				Access:  "access-old",
				Expires: 10_000,
			})
			if err != nil {
				t.Fatalf("refreshOAuthCredential: %v", err)
			}
			if !called {
				t.Fatal("provider refresh was not called")
			}
			if credential.Refresh != "refresh-new" ||
				credential.Access != "access-new" ||
				credential.Expires != 20_000 ||
				credential.AccountID != "acct-new" {
				t.Fatalf("credential = %+v, want registry refresh result", credential)
			}
		})
	}
}

func TestRefreshOAuthCredentialUnknownProviderErrors(t *testing.T) {
	provider := "missing-oauth-provider"
	removeOAuthProvider(t, provider)

	_, err := refreshOAuthCredential(context.Background(), provider, OAuthCredential{Refresh: "refresh-old"})
	if err == nil {
		t.Fatal("refreshOAuthCredential error = nil, want unsupported provider error")
	}
	if !strings.Contains(err.Error(), `oauth refresh is not supported for provider "missing-oauth-provider"`) {
		t.Fatalf("error = %q, want unsupported provider", err)
	}
}

func TestResolverRefreshesRuntimeRegisteredOAuthProvider(t *testing.T) {
	store := newTempStore(t)
	now := time.UnixMilli(10_000)
	expired := OAuthCredential{
		Refresh: "refresh-old",
		Access:  "access-old",
		Expires: now.Add(-time.Minute).UnixMilli(),
	}
	if err := store.Set(context.Background(), "runtime-oauth", Credential{
		Type:  TypeOAuth,
		OAuth: &expired,
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	var refreshCalls int
	replaceOAuthProvider(t, testOAuthProvider{
		id:   "runtime-oauth",
		name: "Runtime OAuth",
		refresh: func(ctx context.Context, refresh string) (aioauth.Credentials, error) {
			refreshCalls++
			if refresh != "refresh-old" {
				t.Fatalf("refresh token = %q, want refresh-old", refresh)
			}
			return aioauth.Credentials{
				Refresh:   "refresh-new",
				Access:    "access-new",
				Expires:   now.Add(time.Hour).UnixMilli(),
				AccountID: "acct-new",
			}, nil
		},
		getAPIKey: func(creds aioauth.Credentials) string {
			if creds.AccountID != "acct-new" {
				t.Fatalf("account id = %q, want acct-new", creds.AccountID)
			}
			return "api:" + creds.Access + ":" + creds.AccountID
		},
	})

	resolver := NewResolver(store)
	resolver.Now = func() time.Time {
		return now
	}

	cred, err := resolver.Resolve(context.Background(), "runtime-oauth")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls)
	}
	if cred.Type != TypeOAuth || cred.Key != "api:access-new:acct-new" || cred.OAuth == nil {
		t.Fatalf("credential = %+v, want refreshed oauth", cred)
	}
	if cred.OAuth.Refresh != "refresh-new" ||
		cred.OAuth.Expires != now.Add(time.Hour).UnixMilli() ||
		cred.OAuth.AccountID != "acct-new" {
		t.Fatalf("oauth credential = %+v, want runtime provider refresh result", cred.OAuth)
	}
}

func TestResolverSerializesConcurrentExpiredOAuthRefresh(t *testing.T) {
	const workers = 8

	store := newTempStore(t)
	now := time.UnixMilli(10_000)
	expired := OAuthCredential{
		Refresh: "refresh-old",
		Access:  "access-old",
		Expires: now.Add(-time.Minute).UnixMilli(),
	}
	if err := store.Set(context.Background(), "openai-codex", Credential{
		Type:  TypeOAuth,
		OAuth: &expired,
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	resolver := NewResolver(store)
	resolver.Now = func() time.Time {
		return now
	}

	// The refresh runs while the cross-process file lock is held (pi
	// auth-storage.ts:404-448); racers re-read under the lock, find the fresh
	// credential, and never refresh again.
	var refreshCalls atomic.Int32
	resolver.refreshOAuth = func(
		_ context.Context,
		provider string,
		cred OAuthCredential,
	) (OAuthCredential, error) {
		if provider != "openai-codex" || cred.Refresh != "refresh-old" {
			return OAuthCredential{}, errors.New("unexpected refresh input")
		}
		refreshCalls.Add(1)
		time.Sleep(50 * time.Millisecond) // widen the race window under the lock
		return OAuthCredential{
			Refresh: "refresh-new",
			Access:  "access-new",
			Expires: now.Add(time.Hour).UnixMilli(),
		}, nil
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan resolveResult, workers)
	for range workers {
		wg.Go(func() {
			<-start
			cred, err := resolver.Resolve(context.Background(), "openai-codex")
			results <- resolveResult{cred: cred, err: err}
		})
	}
	close(start)
	wg.Wait()
	close(results)

	for result := range results {
		if result.err != nil {
			t.Fatalf("Resolve error = %v", result.err)
		}
		if result.cred.Type != TypeOAuth || result.cred.Key != "access-new" {
			t.Fatalf("credential = %+v, want refreshed oauth", result.cred)
		}
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

func TestResolverUsesFreshStoredOAuthWhenRefreshFails(t *testing.T) {
	// An in-memory store without UpdateLocked exercises the fallback path
	// where another writer can land between refresh failure and re-read.
	store := newMapStore()
	now := time.UnixMilli(10_000)
	expired := OAuthCredential{
		Refresh: "refresh-old",
		Access:  "access-old",
		Expires: now.Add(-time.Minute).UnixMilli(),
	}
	if err := store.Set(context.Background(), "openai-codex", Credential{
		Type:  TypeOAuth,
		OAuth: &expired,
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	resolver := NewResolver(store)
	resolver.Now = func() time.Time {
		return now
	}
	refreshErr := errors.New("refresh failed")
	var refreshed bool
	resolver.refreshOAuth = func(
		ctx context.Context,
		provider string,
		cred OAuthCredential,
	) (OAuthCredential, error) {
		refreshed = true
		fresh := OAuthCredential{
			Refresh: "refresh-fresh",
			Access:  "access-fresh",
			Expires: now.Add(time.Hour).UnixMilli(),
		}
		if err := store.Set(ctx, provider, Credential{
			Type:  TypeOAuth,
			OAuth: &fresh,
		}); err != nil {
			t.Fatalf("Set fresh oauth: %v", err)
		}
		return OAuthCredential{}, refreshErr
	}

	cred, err := resolver.Resolve(context.Background(), "openai-codex")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !refreshed {
		t.Fatal("refresh stub was not called")
	}
	if cred.Type != TypeOAuth ||
		cred.Key != "access-fresh" ||
		cred.OAuth == nil ||
		cred.OAuth.Refresh != "refresh-fresh" {
		t.Fatalf("credential = %+v, want freshly stored oauth", cred)
	}
}

func TestResolverFailedRefreshDegradesToMissingCredential(t *testing.T) {
	store := newTempStore(t)
	now := time.UnixMilli(10_000)
	expired := OAuthCredential{
		Refresh: "refresh-old",
		Access:  "access-old",
		Expires: now.Add(-time.Minute).UnixMilli(),
	}
	if err := store.Set(context.Background(), "openai-codex", Credential{
		Type:  TypeOAuth,
		OAuth: &expired,
	}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	resolver := NewResolver(store)
	resolver.Now = func() time.Time {
		return now
	}
	refreshErr := errors.New("refresh failed")
	var refreshed bool
	resolver.refreshOAuth = func(
		ctx context.Context,
		provider string,
		cred OAuthCredential,
	) (OAuthCredential, error) {
		refreshed = true
		return OAuthCredential{}, refreshErr
	}

	_, err := resolver.Resolve(context.Background(), "openai-codex")
	if err == nil {
		t.Fatal("Resolve error = nil, want missing credential")
	}
	if !refreshed {
		t.Fatal("refresh stub was not called")
	}
	_ = refreshErr
	// pi treats a failed refresh exactly like an unconfigured provider and
	// preserves the stored credentials for a /login retry
	// (auth-storage.ts:492-506).
	var missing *MissingCredentialError
	if !errors.As(err, &missing) || missing.Provider != "openai-codex" {
		t.Fatalf("Resolve error = %v, want MissingCredentialError", err)
	}
	cred, ok := store.Get("openai-codex")
	if !ok || cred.OAuth == nil || cred.OAuth.Refresh != "refresh-old" {
		t.Fatalf("stored credential = %+v/%v, want preserved", cred, ok)
	}
}

func newTempStore(t *testing.T) *FileStore {
	t.Helper()
	store, err := NewStore(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

func readAuthJSON(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	raw := map[string]map[string]any{}
	if err := json.Unmarshal(content, &raw); err != nil {
		t.Fatalf("Unmarshal auth JSON: %v", err)
	}
	return raw
}

type mapStore struct {
	mu   sync.Mutex
	data map[string]Credential
}

func newMapStore() *mapStore {
	return &mapStore{data: map[string]Credential{}}
}

func (m *mapStore) Get(provider string) (Credential, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cred, ok := m.data[provider]
	return cred, ok
}

func (m *mapStore) Set(_ context.Context, provider string, cred Credential) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[provider] = cred
	return nil
}

func (m *mapStore) Delete(_ context.Context, provider string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, provider)
	return nil
}

type resolveResult struct {
	cred Credential
	err  error
}

type testOAuthProvider struct {
	id        string
	name      string
	refresh   func(context.Context, string) (aioauth.Credentials, error)
	getAPIKey func(aioauth.Credentials) string
}

func (p testOAuthProvider) ID() string {
	return p.id
}

func (p testOAuthProvider) Name() string {
	return p.name
}

func (p testOAuthProvider) Login(context.Context, aioauth.LoginCallbacks) (aioauth.Credentials, error) {
	return aioauth.Credentials{}, nil
}

func (p testOAuthProvider) RefreshToken(ctx context.Context, refresh string) (aioauth.Credentials, error) {
	if p.refresh == nil {
		return aioauth.Credentials{}, nil
	}
	return p.refresh(ctx, refresh)
}

func (p testOAuthProvider) GetAPIKey(creds aioauth.Credentials) string {
	if p.getAPIKey != nil {
		return p.getAPIKey(creds)
	}
	return creds.Access
}

func replaceOAuthProvider(t *testing.T, provider aioauth.Provider) {
	t.Helper()

	id := provider.ID()
	original, ok := aioauth.GetProvider(id)
	aioauth.RegisterProvider(provider)
	t.Cleanup(func() {
		aioauth.UnregisterProvider(id)
		if ok {
			aioauth.RegisterProvider(original)
		}
	})
}

func removeOAuthProvider(t *testing.T, provider string) {
	t.Helper()

	original, ok := aioauth.GetProvider(provider)
	aioauth.UnregisterProvider(provider)
	t.Cleanup(func() {
		if ok {
			aioauth.RegisterProvider(original)
		}
	})
}
