package ai

import (
	"maps"
	"slices"
	"sort"
)

//go:generate go run ./generate-models

type InputCapability string

const (
	InputText  InputCapability = "text"
	InputImage InputCapability = "image"
)

var extendedThinkingLevels = []string{
	"off",
	"minimal",
	"low",
	"medium",
	"high",
	"xhigh",
}

// Model field tags mirror pi's Model interface (packages/ai/src/types.ts:538)
// so the rpc/json wire shape matches pi's camelCase contract.
type Model struct {
	ID               string             `json:"id"`
	Name             string             `json:"name"`
	API              API                `json:"api"`
	Provider         string             `json:"provider"`
	BaseURL          string             `json:"baseUrl"`
	Reasoning        bool               `json:"reasoning"`
	Input            []InputCapability  `json:"input"`
	Cost             Cost               `json:"cost"`
	ContextWindow    int                `json:"contextWindow"`
	MaxTokens        int                `json:"maxTokens"`
	Headers          map[string]string  `json:"headers,omitempty"`
	Compat           *Compat            `json:"compat,omitempty"`
	ThinkingLevelMap map[string]*string `json:"thinkingLevelMap,omitempty"`
	AuthHeader       bool               `json:"authHeader,omitempty"`
}

// modelRegistry is a two-level map: provider → model ID → Model,
// mirroring pi's Map<provider, Map<id, Model>>. Populated by
// models_generated.go init().
var modelRegistry = map[string]map[string]Model{}

func GetModel(provider, id string) (Model, bool) {
	if provider != "" {
		if providerModels, ok := modelRegistry[provider]; ok {
			model, ok := providerModels[id]
			if !ok {
				return Model{}, false
			}
			return copyModel(model), true
		}
		return Model{}, false
	}
	var found Model
	foundOK := false
	for _, providerModels := range modelRegistry {
		if model, ok := providerModels[id]; ok {
			if foundOK {
				return Model{}, false
			}
			found = model
			foundOK = true
		}
	}
	if !foundOK {
		return Model{}, false
	}
	return copyModel(found), true
}

func BuiltinModels() []Model {
	var models []Model
	for _, providerModels := range modelRegistry {
		for _, model := range providerModels {
			models = append(models, copyModel(model))
		}
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].Provider != models[j].Provider {
			return models[i].Provider < models[j].Provider
		}
		return models[i].ID < models[j].ID
	})
	return models
}

func ModelSupportsInput(model Model, input InputCapability) bool {
	return slices.Contains(model.Input, input)
}

func supportedThinkingLevels(model Model) []string {
	if !model.Reasoning {
		return []string{"off"}
	}

	levels := make([]string, 0, len(extendedThinkingLevels))
	for _, level := range extendedThinkingLevels {
		mapped, ok := model.ThinkingLevelMap[level]
		if ok && mapped == nil {
			continue
		}
		if level == "xhigh" && !ok {
			continue
		}
		levels = append(levels, level)
	}
	return levels
}

func clampThinkingLevel(model Model, level string) string {
	levels := supportedThinkingLevels(model)
	if slices.Contains(levels, level) {
		return level
	}

	requestedIndex := indexThinkingLevel(level)
	if requestedIndex == -1 {
		if len(levels) == 0 {
			return "off"
		}
		return levels[0]
	}
	for i := requestedIndex; i < len(extendedThinkingLevels); i++ {
		candidate := extendedThinkingLevels[i]
		if containsThinkingLevel(levels, candidate) {
			return candidate
		}
	}
	for i := requestedIndex - 1; i >= 0; i-- {
		candidate := extendedThinkingLevels[i]
		if containsThinkingLevel(levels, candidate) {
			return candidate
		}
	}
	if len(levels) == 0 {
		return "off"
	}
	return levels[0]
}

func reasoningEffort(model Model, reasoning string) string {
	if reasoning == "" {
		return ""
	}
	level := clampThinkingLevel(model, reasoning)
	if level == "off" {
		return ""
	}
	return level
}

func indexThinkingLevel(level string) int {
	for i, candidate := range extendedThinkingLevels {
		if candidate == level {
			return i
		}
	}
	return -1
}

func containsThinkingLevel(levels []string, level string) bool {
	return slices.Contains(levels, level)
}

func CalculateCost(model Model, usage *Usage) *Cost {
	if usage == nil {
		return nil
	}
	usage.Cost = &Cost{
		Input:      model.Cost.Input * float64(usage.Input) / 1_000_000,
		Output:     model.Cost.Output * float64(usage.Output) / 1_000_000,
		CacheRead:  model.Cost.CacheRead * float64(usage.CacheRead) / 1_000_000,
		CacheWrite: model.Cost.CacheWrite * float64(usage.CacheWrite) / 1_000_000,
	}
	usage.Cost.Total = usage.Cost.Input + usage.Cost.Output + usage.Cost.CacheRead + usage.Cost.CacheWrite
	return usage.Cost
}

func copyModel(model Model) Model {
	if model.Input != nil {
		model.Input = append([]InputCapability{}, model.Input...)
	}
	if model.Headers != nil {
		model.Headers = copyStringMap(model.Headers)
	}
	if model.ThinkingLevelMap != nil {
		model.ThinkingLevelMap = copyStringPointerMap(model.ThinkingLevelMap)
	}
	if model.Compat != nil {
		model.Compat = copyCompat(model.Compat)
	}
	return model
}

func copyStringMap(values map[string]string) map[string]string {
	copied := make(map[string]string, len(values))
	maps.Copy(copied, values)
	return copied
}

func copyStringPointerMap(values map[string]*string) map[string]*string {
	copied := make(map[string]*string, len(values))
	for key, value := range values {
		if value == nil {
			copied[key] = nil
			continue
		}
		copyValue := *value
		copied[key] = &copyValue
	}
	return copied
}

func copyCompat(compat *Compat) *Compat {
	if compat == nil {
		return nil
	}
	return &Compat{
		SupportsStore:                    copyBoolPtr(compat.SupportsStore),
		SupportsDeveloperRole:            copyBoolPtr(compat.SupportsDeveloperRole),
		SupportsReasoningEffort:          copyBoolPtr(compat.SupportsReasoningEffort),
		SupportsUsageInStreaming:         copyBoolPtr(compat.SupportsUsageInStreaming),
		RequiresToolResultName:           copyBoolPtr(compat.RequiresToolResultName),
		RequiresAssistantAfterToolResult: copyBoolPtr(compat.RequiresAssistantAfterToolResult),
		RequiresThinkingAsText:           copyBoolPtr(compat.RequiresThinkingAsText),
		RequiresReasoningContentOnAssistantMessages: copyBoolPtr(
			compat.RequiresReasoningContentOnAssistantMessages,
		),
		ZaiToolStream:                   copyBoolPtr(compat.ZaiToolStream),
		SupportsStrictMode:              copyBoolPtr(compat.SupportsStrictMode),
		SendSessionAffinityHeaders:      copyBoolPtr(compat.SendSessionAffinityHeaders),
		SupportsLongCacheRetention:      copyBoolPtr(compat.SupportsLongCacheRetention),
		MaxTokensField:                  copyStringPtr(compat.MaxTokensField),
		ThinkingFormat:                  copyStringPtr(compat.ThinkingFormat),
		CacheControlFormat:              copyStringPtr(compat.CacheControlFormat),
		OpenRouterRouting:               copyOpenRouterRouting(compat.OpenRouterRouting),
		VercelGatewayRouting:            copyVercelGatewayRouting(compat.VercelGatewayRouting),
		SendSessionIdHeader:             copyBoolPtr(compat.SendSessionIdHeader),
		SupportsEagerToolInputStreaming: copyBoolPtr(compat.SupportsEagerToolInputStreaming),
		SupportsCacheControlOnTools:     copyBoolPtr(compat.SupportsCacheControlOnTools),
		ForceAdaptiveThinking:           copyBoolPtr(compat.ForceAdaptiveThinking),
	}
}

func copyStringPtr(value *string) *string {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func copyBoolPtr(value *bool) *bool {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func copyOpenRouterRouting(routing *OpenRouterRouting) *OpenRouterRouting {
	if routing == nil {
		return nil
	}
	copied := *routing
	copied.AllowFallbacks = copyBoolPtr(routing.AllowFallbacks)
	copied.RequireParameters = copyBoolPtr(routing.RequireParameters)
	copied.DataCollection = copyStringPtr(routing.DataCollection)
	copied.ZDR = copyBoolPtr(routing.ZDR)
	copied.EnforceDistillableText = copyBoolPtr(routing.EnforceDistillableText)
	copied.Order = append([]string{}, routing.Order...)
	copied.Only = append([]string{}, routing.Only...)
	copied.Ignore = append([]string{}, routing.Ignore...)
	copied.Quantizations = append([]string{}, routing.Quantizations...)
	if routing.MaxPrice != nil {
		copied.MaxPrice = make(map[string]any, len(routing.MaxPrice))
		maps.Copy(copied.MaxPrice, routing.MaxPrice)
	}
	return &copied
}

func copyVercelGatewayRouting(routing *VercelGatewayRouting) *VercelGatewayRouting {
	if routing == nil {
		return nil
	}
	copied := *routing
	copied.Order = append([]string{}, routing.Order...)
	copied.Only = append([]string{}, routing.Only...)
	return &copied
}
