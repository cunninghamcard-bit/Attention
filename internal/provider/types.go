package provider

import (
	"context"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/auth"
)

type AuthResolver interface {
	Resolve(context.Context, string) (auth.Credential, error)
}

type ModelsConfig struct {
	Providers map[string]ProviderConfig `json:"providers"`
}

type ProviderConfig struct {
	Name           *string                  `json:"name,omitempty"`
	BaseURL        *string                  `json:"baseUrl,omitempty"`
	APIKey         *string                  `json:"apiKey,omitempty"`
	API            *string                  `json:"api,omitempty"`
	Headers        map[string]string        `json:"headers,omitempty"`
	AuthHeader     *bool                    `json:"authHeader,omitempty"`
	Compat         *ai.Compat               `json:"compat,omitempty"`
	Models         []ModelDefinition        `json:"models,omitempty"`
	ModelOverrides map[string]ModelOverride `json:"modelOverrides,omitempty"`
}

type ModelDefinition struct {
	ID               string               `json:"id"`
	Name             *string              `json:"name,omitempty"`
	API              *string              `json:"api,omitempty"`
	BaseURL          *string              `json:"baseUrl,omitempty"`
	Reasoning        *bool                `json:"reasoning,omitempty"`
	ThinkingLevelMap map[string]*string   `json:"thinkingLevelMap,omitempty"`
	Input            []ai.InputCapability `json:"input,omitempty"`
	Cost             *ModelCost           `json:"cost,omitempty"`
	ContextWindow    *int                 `json:"contextWindow,omitempty"`
	MaxTokens        *int                 `json:"maxTokens,omitempty"`
	Headers          map[string]string    `json:"headers,omitempty"`
	Compat           *ai.Compat           `json:"compat,omitempty"`
}

type ModelOverride struct {
	Name             *string              `json:"name,omitempty"`
	Reasoning        *bool                `json:"reasoning,omitempty"`
	ThinkingLevelMap map[string]*string   `json:"thinkingLevelMap,omitempty"`
	Input            []ai.InputCapability `json:"input,omitempty"`
	Cost             *ModelCost           `json:"cost,omitempty"`
	ContextWindow    *int                 `json:"contextWindow,omitempty"`
	MaxTokens        *int                 `json:"maxTokens,omitempty"`
	Headers          map[string]string    `json:"headers,omitempty"`
	Compat           *ai.Compat           `json:"compat,omitempty"`
}

type ModelCost struct {
	Input      *float64 `json:"input,omitempty"`
	Output     *float64 `json:"output,omitempty"`
	CacheRead  *float64 `json:"cacheRead,omitempty"`
	CacheWrite *float64 `json:"cacheWrite,omitempty"`
}

type ResolvedRequestAuth struct {
	APIKey  string
	Headers map[string]string
}
