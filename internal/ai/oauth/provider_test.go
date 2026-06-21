package oauth

import (
	"context"
	"reflect"
	"testing"
)

func TestProviderRegistryRegisterGetProvidersAndUnregister(t *testing.T) {
	restoreProviderRegistry(t)

	a := fakeProvider{id: "z-provider", name: "Z Provider"}
	b := fakeProvider{id: "a-provider", name: "A Provider"}
	RegisterProvider(a)
	RegisterProvider(b)

	got, ok := GetProvider("z-provider")
	if !ok {
		t.Fatal("GetProvider z-provider = false")
	}
	if got.ID() != "z-provider" || got.Name() != "Z Provider" {
		t.Fatalf("provider = %s/%s", got.ID(), got.Name())
	}

	providers := Providers()
	ids := make([]string, 0, len(providers))
	for _, provider := range providers {
		ids = append(ids, provider.ID())
	}
	want := []string{"a-provider", "z-provider"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("providers = %v, want %v", ids, want)
	}

	UnregisterProvider("z-provider")
	if _, ok := GetProvider("z-provider"); ok {
		t.Fatal("GetProvider z-provider = true after unregister")
	}
}

func TestBuiltInProvidersAreRegistered(t *testing.T) {
	RegisterBuiltins()

	tests := []struct {
		id   string
		name string
	}{
		{id: "anthropic", name: "Anthropic"},
		{id: "openai-codex", name: "OpenAI Codex"},
	}

	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			provider, ok := GetProvider(tt.id)
			if !ok {
				t.Fatalf("GetProvider %q = false", tt.id)
			}
			if provider.ID() != tt.id || provider.Name() != tt.name {
				t.Fatalf("provider = %s/%s, want %s/%s", provider.ID(), provider.Name(), tt.id, tt.name)
			}
			apiKey := provider.GetAPIKey(Credentials{Access: "access-token"})
			if apiKey != "access-token" {
				t.Fatalf("GetAPIKey = %q, want access-token", apiKey)
			}
		})
	}
}

func TestLoginRoutesToRegisteredProvider(t *testing.T) {
	restoreProviderRegistry(t)

	RegisterProvider(loginProvider{
		fakeProvider: fakeProvider{id: "demo", name: "Demo"},
		creds:        Credentials{Access: "access-9", Refresh: "refresh-9"},
	})

	creds, err := Login(context.Background(), "demo", LoginCallbacks{})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if creds.Access != "access-9" || creds.Refresh != "refresh-9" {
		t.Fatalf("creds = %+v", creds)
	}

	if _, err := Login(context.Background(), "missing", LoginCallbacks{}); err == nil {
		t.Fatal("Login missing = nil error, want failure")
	}
}

type loginProvider struct {
	fakeProvider
	creds Credentials
}

func (p loginProvider) Login(context.Context, LoginCallbacks) (Credentials, error) {
	return p.creds, nil
}

func restoreProviderRegistry(t *testing.T) {
	t.Helper()

	original := Providers()
	for _, provider := range original {
		UnregisterProvider(provider.ID())
	}
	t.Cleanup(func() {
		for _, provider := range Providers() {
			UnregisterProvider(provider.ID())
		}
		for _, provider := range original {
			RegisterProvider(provider)
		}
	})
}

type fakeProvider struct {
	id   string
	name string
}

func (p fakeProvider) ID() string {
	return p.id
}

func (p fakeProvider) Name() string {
	return p.name
}

func (p fakeProvider) Login(context.Context, LoginCallbacks) (Credentials, error) {
	return Credentials{}, nil
}

func (p fakeProvider) RefreshToken(context.Context, string) (Credentials, error) {
	return Credentials{}, nil
}

func (p fakeProvider) GetAPIKey(creds Credentials) string {
	return creds.Access
}
