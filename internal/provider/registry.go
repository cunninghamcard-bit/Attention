package provider

import (
	"context"
	"fmt"
	"maps"
	"sort"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/config"
)

const (
	defaultContextWindow = 128_000
	defaultMaxTokens     = 16_384
)

var supportedAPIs = map[string]ai.API{
	string(ai.APIAnthropicMessages):    ai.APIAnthropicMessages,
	string(ai.APIOpenAICompletions):    ai.APIOpenAICompletions,
	string(ai.APIOpenAIResponses):      ai.APIOpenAIResponses,
	string(ai.APIOpenAICodexResponses): ai.APIOpenAICodexResponses,
}

type Registry struct {
	builtin    []ai.Model
	auth       AuthResolver
	config     ModelsConfig
	registered map[string]ProviderConfig

	models         []ai.Model
	requestConfigs map[string]requestConfig
	loadErr        error
}

type requestConfig struct {
	apiKey     *string
	headers    map[string]string
	authHeader *bool
}

type buildState struct {
	models         []ai.Model
	requestConfigs map[string]requestConfig
	defaults       map[string]providerDefaults
}

type providerDefaults struct {
	api     ai.API
	baseURL string
}

func New(builtin []ai.Model, authResolver AuthResolver) *Registry {
	r := &Registry{
		builtin:    copyModels(builtin),
		auth:       authResolver,
		config:     ModelsConfig{Providers: map[string]ProviderConfig{}},
		registered: map[string]ProviderConfig{},
	}
	r.models, r.requestConfigs = r.baseState()
	return r
}

// CloneBase returns a registry with the same builtin models, auth resolver, and
// static models config, excluding runtime-registered extension providers.
func (r *Registry) CloneBase() *Registry {
	cloned := New(r.builtin, r.auth)
	if err := cloned.ApplyConfig(copyModelsConfig(r.config)); err != nil {
		cloned.loadErr = err
	}
	return cloned
}

func (r *Registry) ApplyConfig(cfg ModelsConfig) error {
	if cfg.Providers == nil {
		cfg.Providers = map[string]ProviderConfig{}
	}
	r.config = cfg
	if err := r.rebuild(); err != nil {
		r.loadErr = err
		r.models, r.requestConfigs = r.baseState()
		return err
	}
	r.loadErr = nil
	return nil
}

func (r *Registry) RegisterProvider(name string, cfg ProviderConfig) error {
	if name == "" {
		return fmt.Errorf("provider name is required")
	}

	nextRegistered := copyProviderConfigMap(r.registered)
	if existing, ok := nextRegistered[name]; ok {
		nextRegistered[name] = mergeProviderConfig(existing, cfg)
	} else {
		nextRegistered[name] = copyProviderConfig(cfg)
	}

	models, requests, err := r.build(r.config, nextRegistered)
	if err != nil {
		return err
	}
	r.registered = nextRegistered
	r.models = models
	r.requestConfigs = requests
	return nil
}

func (r *Registry) UnregisterProvider(name string) error {
	if _, ok := r.registered[name]; !ok {
		return nil
	}

	nextRegistered := copyProviderConfigMap(r.registered)
	delete(nextRegistered, name)
	models, requests, err := r.build(r.config, nextRegistered)
	if err != nil {
		return err
	}
	r.registered = nextRegistered
	r.models = models
	r.requestConfigs = requests
	return nil
}

func (r *Registry) Resolve(modelID string) (ai.Model, bool) {
	var found ai.Model
	foundOK := false
	for _, model := range r.models {
		if model.ID == modelID {
			if foundOK {
				return ai.Model{}, false
			}
			found = model
			foundOK = true
		}
	}
	if !foundOK {
		return ai.Model{}, false
	}
	return copyModel(found), true
}

func (r *Registry) ResolveByProvider(providerName, modelID string) (ai.Model, bool) {
	if providerName == "" {
		return r.Resolve(modelID)
	}
	for _, model := range r.models {
		if model.Provider == providerName && model.ID == modelID {
			return copyModel(model), true
		}
	}
	return ai.Model{}, false
}

// SetRuntimeAPIKey applies an in-memory API-key override for a provider when the
// underlying auth resolver supports it (CLI --api-key). pi sets this on its
// AuthStorage from main: .agents/references/pi/packages/coding-agent/src/main.ts:586.
func (r *Registry) SetRuntimeAPIKey(provider, apiKey string) {
	if setter, ok := r.auth.(interface{ SetRuntimeAPIKey(string, string) }); ok {
		setter.SetRuntimeAPIKey(provider, apiKey)
	}
}

func (r *Registry) ResolveAuth(ctx context.Context, m ai.Model) (ResolvedRequestAuth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ensureSupportedAPI(m.API); err != nil {
		return ResolvedRequestAuth{}, err
	}

	req := r.requestConfigs[m.Provider]
	headers := map[string]string{}
	if err := resolveHeadersInto(headers, req.headers, fmt.Sprintf("provider %q", m.Provider)); err != nil {
		return ResolvedRequestAuth{}, err
	}
	if err := resolveHeadersInto(headers, m.Headers, fmt.Sprintf("model %q", m.ID)); err != nil {
		return ResolvedRequestAuth{}, err
	}

	apiKey, err := r.resolveAPIKey(ctx, m.Provider, req.apiKey)
	if err != nil {
		return ResolvedRequestAuth{}, err
	}

	authHeader := m.AuthHeader || boolPtrValue(req.authHeader)
	if authHeader {
		if apiKey == "" {
			return ResolvedRequestAuth{}, fmt.Errorf(
				"missing credential for provider %q; set apiKey in models.json or run `/login %s`",
				m.Provider,
				m.Provider,
			)
		}
		headers["Authorization"] = "Bearer " + apiKey
	}

	return ResolvedRequestAuth{
		APIKey:  apiKey,
		Headers: headers,
	}, nil
}

func (r *Registry) All() []ai.Model {
	return copyModels(r.models)
}

func (r *Registry) Available(ctx context.Context) []ai.Model {
	if ctx == nil {
		ctx = context.Background()
	}

	available := []ai.Model{}
	for _, model := range r.models {
		if r.hasConfiguredAuth(ctx, model) {
			available = append(available, copyModel(model))
		}
	}
	return available
}

func (r *Registry) LoadError() error {
	return r.loadErr
}

func (r *Registry) rebuild() error {
	models, requests, err := r.build(r.config, r.registered)
	if err != nil {
		return err
	}
	r.models = models
	r.requestConfigs = requests
	return nil
}

func (r *Registry) build(
	cfg ModelsConfig,
	registered map[string]ProviderConfig,
) ([]ai.Model, map[string]requestConfig, error) {
	state := r.newBuildState()
	for _, name := range sortedProviderNames(cfg.Providers) {
		if err := state.applyProviderConfig(name, cfg.Providers[name]); err != nil {
			return nil, nil, err
		}
	}
	for _, name := range sortedProviderNames(registered) {
		if err := state.applyProviderConfig(name, registered[name]); err != nil {
			return nil, nil, err
		}
	}
	return copyModels(state.models), copyRequestConfigs(state.requestConfigs), nil
}

func (r *Registry) newBuildState() *buildState {
	models := copyModels(r.builtin)
	return &buildState{
		models:         models,
		requestConfigs: map[string]requestConfig{},
		defaults:       builtInDefaults(models),
	}
}

func (r *Registry) baseState() ([]ai.Model, map[string]requestConfig) {
	state := r.newBuildState()
	return copyModels(state.models), copyRequestConfigs(state.requestConfigs)
}

func (s *buildState) applyProviderConfig(provider string, cfg ProviderConfig) error {
	s.storeRequestConfig(provider, cfg)

	if len(cfg.Models) > 0 {
		return s.replaceProviderModels(provider, cfg)
	}

	for i, model := range s.models {
		if model.Provider != provider {
			continue
		}
		model = applyProviderOverride(model, cfg)
		if override, ok := cfg.ModelOverrides[model.ID]; ok {
			model = applyModelOverride(model, override)
		}
		s.models[i] = model
	}
	return nil
}

func (s *buildState) replaceProviderModels(provider string, cfg ProviderConfig) error {
	models := make([]ai.Model, 0, len(s.models))
	for _, model := range s.models {
		if model.Provider != provider {
			models = append(models, model)
		}
	}
	s.models = models

	defaults, hasDefaults := s.defaults[provider]
	for _, def := range cfg.Models {
		model, err := buildCustomModel(provider, cfg, def, defaults, hasDefaults)
		if err != nil {
			return err
		}
		s.models = append(s.models, model)
	}
	return nil
}

func buildCustomModel(
	provider string,
	cfg ProviderConfig,
	def ModelDefinition,
	defaults providerDefaults,
	hasDefaults bool,
) (ai.Model, error) {
	if def.ID == "" {
		return ai.Model{}, fmt.Errorf("provider %s: model missing id", provider)
	}

	apiValue := stringPtrValue(def.API)
	if apiValue == "" {
		apiValue = stringPtrValue(cfg.API)
	}
	if apiValue == "" && hasDefaults {
		apiValue = string(defaults.api)
	}
	if apiValue == "" {
		return ai.Model{}, fmt.Errorf(
			"provider %s, model %s: no api specified; set api at provider or model level",
			provider,
			def.ID,
		)
	}
	baseURL := stringPtrValue(def.BaseURL)
	if baseURL == "" {
		baseURL = stringPtrValue(cfg.BaseURL)
	}
	if baseURL == "" && hasDefaults {
		baseURL = defaults.baseURL
	}
	if baseURL == "" {
		return ai.Model{}, fmt.Errorf(
			"provider %s, model %s: baseUrl is required for custom models",
			provider,
			def.ID,
		)
	}

	name := def.ID
	if def.Name != nil {
		name = *def.Name
	}

	return ai.Model{
		ID:               def.ID,
		Name:             name,
		API:              ai.API(apiValue),
		Provider:         provider,
		BaseURL:          baseURL,
		Reasoning:        boolPtrValue(def.Reasoning),
		Input:            modelInput(def.Input),
		Cost:             modelCost(def.Cost, ai.Cost{}),
		ContextWindow:    intPtrValue(def.ContextWindow, defaultContextWindow),
		MaxTokens:        intPtrValue(def.MaxTokens, defaultMaxTokens),
		Headers:          mergeStringMaps(cfg.Headers, def.Headers),
		Compat:           mergeCompat(cfg.Compat, def.Compat),
		ThinkingLevelMap: copyStringPointerMap(def.ThinkingLevelMap),
		AuthHeader:       boolPtrValue(cfg.AuthHeader),
	}, nil
}

func applyProviderOverride(model ai.Model, cfg ProviderConfig) ai.Model {
	model = copyModel(model)
	if cfg.BaseURL != nil {
		model.BaseURL = *cfg.BaseURL
	}
	if cfg.Headers != nil {
		model.Headers = mergeStringMaps(model.Headers, cfg.Headers)
	}
	if cfg.Compat != nil {
		model.Compat = mergeCompat(model.Compat, cfg.Compat)
	}
	if cfg.AuthHeader != nil {
		model.AuthHeader = *cfg.AuthHeader
	}
	return model
}

func applyModelOverride(model ai.Model, override ModelOverride) ai.Model {
	model = copyModel(model)
	if override.Name != nil {
		model.Name = *override.Name
	}
	if override.Reasoning != nil {
		model.Reasoning = *override.Reasoning
	}
	if override.ThinkingLevelMap != nil {
		model.ThinkingLevelMap = mergeStringPointerMaps(model.ThinkingLevelMap, override.ThinkingLevelMap)
	}
	if override.Input != nil {
		model.Input = append([]ai.InputCapability{}, override.Input...)
	}
	if override.Cost != nil {
		model.Cost = modelCost(override.Cost, model.Cost)
	}
	if override.ContextWindow != nil {
		model.ContextWindow = *override.ContextWindow
	}
	if override.MaxTokens != nil {
		model.MaxTokens = *override.MaxTokens
	}
	if override.Headers != nil {
		model.Headers = mergeStringMaps(model.Headers, override.Headers)
	}
	if override.Compat != nil {
		model.Compat = mergeCompat(model.Compat, override.Compat)
	}
	return model
}

func (s *buildState) storeRequestConfig(provider string, cfg ProviderConfig) {
	if cfg.APIKey == nil && cfg.Headers == nil && cfg.AuthHeader == nil {
		return
	}

	current := s.requestConfigs[provider]
	if cfg.APIKey != nil {
		current.apiKey = copyStringPtr(cfg.APIKey)
	}
	if cfg.Headers != nil {
		current.headers = mergeStringMaps(current.headers, cfg.Headers)
	}
	if cfg.AuthHeader != nil {
		current.authHeader = copyBoolPtr(cfg.AuthHeader)
	}
	s.requestConfigs[provider] = current
}

func (r *Registry) resolveAPIKey(ctx context.Context, provider string, configured *string) (string, error) {
	if configured != nil {
		if value := config.ResolveValue(*configured); value != "" {
			return value, nil
		}
	}

	if r.auth == nil {
		return "", fmt.Errorf(
			"missing credential for provider %q; set apiKey in models.json or run `/login %s`",
			provider,
			provider,
		)
	}

	cred, err := r.auth.Resolve(ctx, provider)
	if err != nil {
		return "", fmt.Errorf(
			"resolve credential for provider %q; set apiKey in models.json or configure auth/login: %w",
			provider,
			err,
		)
	}
	if cred.Key == "" {
		return "", fmt.Errorf("credential for provider %q resolved to empty value", provider)
	}
	return cred.Key, nil
}

func (r *Registry) hasConfiguredAuth(ctx context.Context, model ai.Model) bool {
	req := r.requestConfigs[model.Provider]
	if req.apiKey != nil {
		return config.ResolveValue(*req.apiKey) != ""
	}
	if r.auth == nil {
		return false
	}
	cred, err := r.auth.Resolve(ctx, model.Provider)
	return err == nil && cred.Key != ""
}

func resolveHeadersInto(dst map[string]string, headers map[string]string, description string) error {
	for key, value := range headers {
		resolved := config.ResolveValue(value)
		if resolved == "" {
			return fmt.Errorf("%s header %q resolved to empty value", description, key)
		}
		dst[key] = resolved
	}
	return nil
}

func ensureSupportedAPI(api ai.API) error {
	if _, ok := supportedAPIs[string(api)]; ok {
		return nil
	}
	// TODO: pi's model-registry stores configured API strings and resolves the
	// provider later; full transport parity needs along to add the remaining
	// provider wires.
	return fmt.Errorf("unsupported api %q", api)
}

func builtInDefaults(models []ai.Model) map[string]providerDefaults {
	defaults := map[string]providerDefaults{}
	for _, model := range models {
		if _, ok := defaults[model.Provider]; ok {
			continue
		}
		defaults[model.Provider] = providerDefaults{
			api:     model.API,
			baseURL: model.BaseURL,
		}
	}
	return defaults
}

func sortedProviderNames(providers map[string]ProviderConfig) []string {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func mergeProviderConfig(existing ProviderConfig, incoming ProviderConfig) ProviderConfig {
	merged := copyProviderConfig(existing)
	if incoming.Name != nil {
		merged.Name = copyStringPtr(incoming.Name)
	}
	if incoming.BaseURL != nil {
		merged.BaseURL = copyStringPtr(incoming.BaseURL)
	}
	if incoming.APIKey != nil {
		merged.APIKey = copyStringPtr(incoming.APIKey)
	}
	if incoming.API != nil {
		merged.API = copyStringPtr(incoming.API)
	}
	if incoming.Headers != nil {
		merged.Headers = copyStringMap(incoming.Headers)
	}
	if incoming.AuthHeader != nil {
		merged.AuthHeader = copyBoolPtr(incoming.AuthHeader)
	}
	if incoming.Compat != nil {
		merged.Compat = copyCompat(incoming.Compat)
	}
	if incoming.Models != nil {
		merged.Models = copyModelDefinitions(incoming.Models)
	}
	if incoming.ModelOverrides != nil {
		merged.ModelOverrides = copyModelOverrides(incoming.ModelOverrides)
	}
	return merged
}

func modelInput(input []ai.InputCapability) []ai.InputCapability {
	if input == nil {
		return []ai.InputCapability{ai.InputText}
	}
	return append([]ai.InputCapability{}, input...)
}

func modelCost(cost *ModelCost, base ai.Cost) ai.Cost {
	if cost == nil {
		return base
	}
	if cost.Input != nil {
		base.Input = *cost.Input
	}
	if cost.Output != nil {
		base.Output = *cost.Output
	}
	if cost.CacheRead != nil {
		base.CacheRead = *cost.CacheRead
	}
	if cost.CacheWrite != nil {
		base.CacheWrite = *cost.CacheWrite
	}
	return base
}

func intPtrValue(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

func stringPtrValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func boolPtrValue(value *bool) bool {
	return value != nil && *value
}

func copyModels(models []ai.Model) []ai.Model {
	copied := make([]ai.Model, 0, len(models))
	for _, model := range models {
		copied = append(copied, copyModel(model))
	}
	return copied
}

func copyModel(model ai.Model) ai.Model {
	if model.Input != nil {
		model.Input = append([]ai.InputCapability{}, model.Input...)
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

func copyProviderConfigMap(configs map[string]ProviderConfig) map[string]ProviderConfig {
	copied := make(map[string]ProviderConfig, len(configs))
	for key, value := range configs {
		copied[key] = copyProviderConfig(value)
	}
	return copied
}

func copyModelsConfig(cfg ModelsConfig) ModelsConfig {
	return ModelsConfig{Providers: copyProviderConfigMap(cfg.Providers)}
}

func copyProviderConfig(cfg ProviderConfig) ProviderConfig {
	return ProviderConfig{
		Name:           copyStringPtr(cfg.Name),
		BaseURL:        copyStringPtr(cfg.BaseURL),
		APIKey:         copyStringPtr(cfg.APIKey),
		API:            copyStringPtr(cfg.API),
		Headers:        copyStringMap(cfg.Headers),
		AuthHeader:     copyBoolPtr(cfg.AuthHeader),
		Compat:         copyCompat(cfg.Compat),
		Models:         copyModelDefinitions(cfg.Models),
		ModelOverrides: copyModelOverrides(cfg.ModelOverrides),
	}
}

func copyModelDefinitions(models []ModelDefinition) []ModelDefinition {
	if models == nil {
		return nil
	}
	copied := make([]ModelDefinition, 0, len(models))
	for _, model := range models {
		copied = append(copied, ModelDefinition{
			ID:               model.ID,
			Name:             copyStringPtr(model.Name),
			API:              copyStringPtr(model.API),
			BaseURL:          copyStringPtr(model.BaseURL),
			Reasoning:        copyBoolPtr(model.Reasoning),
			ThinkingLevelMap: copyStringPointerMap(model.ThinkingLevelMap),
			Input:            append([]ai.InputCapability{}, model.Input...),
			Cost:             copyModelCost(model.Cost),
			ContextWindow:    copyIntPtr(model.ContextWindow),
			MaxTokens:        copyIntPtr(model.MaxTokens),
			Headers:          copyStringMap(model.Headers),
			Compat:           copyCompat(model.Compat),
		})
	}
	return copied
}

func copyModelOverrides(overrides map[string]ModelOverride) map[string]ModelOverride {
	if overrides == nil {
		return nil
	}
	copied := make(map[string]ModelOverride, len(overrides))
	for id, override := range overrides {
		copied[id] = ModelOverride{
			Name:             copyStringPtr(override.Name),
			Reasoning:        copyBoolPtr(override.Reasoning),
			ThinkingLevelMap: copyStringPointerMap(override.ThinkingLevelMap),
			Input:            append([]ai.InputCapability{}, override.Input...),
			Cost:             copyModelCost(override.Cost),
			ContextWindow:    copyIntPtr(override.ContextWindow),
			MaxTokens:        copyIntPtr(override.MaxTokens),
			Headers:          copyStringMap(override.Headers),
			Compat:           copyCompat(override.Compat),
		}
	}
	return copied
}

func copyRequestConfigs(configs map[string]requestConfig) map[string]requestConfig {
	copied := make(map[string]requestConfig, len(configs))
	for provider, cfg := range configs {
		copied[provider] = requestConfig{
			apiKey:     copyStringPtr(cfg.apiKey),
			headers:    copyStringMap(cfg.headers),
			authHeader: copyBoolPtr(cfg.authHeader),
		}
	}
	return copied
}

func copyStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	copied := make(map[string]string, len(values))
	maps.Copy(copied, values)
	return copied
}

func mergeStringMaps(base map[string]string, override map[string]string) map[string]string {
	if base == nil && override == nil {
		return nil
	}
	merged := copyStringMap(base)
	if merged == nil {
		merged = map[string]string{}
	}
	maps.Copy(merged, override)
	return merged
}

func copyStringPointerMap(values map[string]*string) map[string]*string {
	if values == nil {
		return nil
	}
	copied := make(map[string]*string, len(values))
	for key, value := range values {
		copied[key] = copyStringPtr(value)
	}
	return copied
}

func mergeStringPointerMaps(base map[string]*string, override map[string]*string) map[string]*string {
	if base == nil && override == nil {
		return nil
	}
	merged := copyStringPointerMap(base)
	if merged == nil {
		merged = map[string]*string{}
	}
	for key, value := range override {
		merged[key] = copyStringPtr(value)
	}
	return merged
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

func copyIntPtr(value *int) *int {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func copyFloatPtr(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func copyModelCost(cost *ModelCost) *ModelCost {
	if cost == nil {
		return nil
	}
	return &ModelCost{
		Input:      copyFloatPtr(cost.Input),
		Output:     copyFloatPtr(cost.Output),
		CacheRead:  copyFloatPtr(cost.CacheRead),
		CacheWrite: copyFloatPtr(cost.CacheWrite),
	}
}

func mergeCompat(base *ai.Compat, override *ai.Compat) *ai.Compat {
	if base == nil {
		return copyCompat(override)
	}
	if override == nil {
		return copyCompat(base)
	}

	merged := copyCompat(base)
	if override.SupportsStore != nil {
		merged.SupportsStore = copyBoolPtr(override.SupportsStore)
	}
	if override.SupportsDeveloperRole != nil {
		merged.SupportsDeveloperRole = copyBoolPtr(override.SupportsDeveloperRole)
	}
	if override.SupportsReasoningEffort != nil {
		merged.SupportsReasoningEffort = copyBoolPtr(override.SupportsReasoningEffort)
	}
	if override.SupportsUsageInStreaming != nil {
		merged.SupportsUsageInStreaming = copyBoolPtr(override.SupportsUsageInStreaming)
	}
	if override.RequiresToolResultName != nil {
		merged.RequiresToolResultName = copyBoolPtr(override.RequiresToolResultName)
	}
	if override.RequiresAssistantAfterToolResult != nil {
		merged.RequiresAssistantAfterToolResult = copyBoolPtr(override.RequiresAssistantAfterToolResult)
	}
	if override.RequiresThinkingAsText != nil {
		merged.RequiresThinkingAsText = copyBoolPtr(override.RequiresThinkingAsText)
	}
	if override.RequiresReasoningContentOnAssistantMessages != nil {
		merged.RequiresReasoningContentOnAssistantMessages = copyBoolPtr(
			override.RequiresReasoningContentOnAssistantMessages,
		)
	}
	if override.ZaiToolStream != nil {
		merged.ZaiToolStream = copyBoolPtr(override.ZaiToolStream)
	}
	if override.SupportsStrictMode != nil {
		merged.SupportsStrictMode = copyBoolPtr(override.SupportsStrictMode)
	}
	if override.SendSessionAffinityHeaders != nil {
		merged.SendSessionAffinityHeaders = copyBoolPtr(override.SendSessionAffinityHeaders)
	}
	if override.SupportsLongCacheRetention != nil {
		merged.SupportsLongCacheRetention = copyBoolPtr(override.SupportsLongCacheRetention)
	}
	if override.MaxTokensField != nil {
		merged.MaxTokensField = copyStringPtr(override.MaxTokensField)
	}
	if override.ThinkingFormat != nil {
		merged.ThinkingFormat = copyStringPtr(override.ThinkingFormat)
	}
	if override.CacheControlFormat != nil {
		merged.CacheControlFormat = copyStringPtr(override.CacheControlFormat)
	}
	if override.SendSessionIdHeader != nil {
		merged.SendSessionIdHeader = copyBoolPtr(override.SendSessionIdHeader)
	}
	if override.SupportsEagerToolInputStreaming != nil {
		merged.SupportsEagerToolInputStreaming = copyBoolPtr(override.SupportsEagerToolInputStreaming)
	}
	if override.SupportsCacheControlOnTools != nil {
		merged.SupportsCacheControlOnTools = copyBoolPtr(override.SupportsCacheControlOnTools)
	}
	if override.ForceAdaptiveThinking != nil {
		merged.ForceAdaptiveThinking = copyBoolPtr(override.ForceAdaptiveThinking)
	}
	if override.OpenRouterRouting != nil {
		merged.OpenRouterRouting = mergeOpenRouterRouting(merged.OpenRouterRouting, override.OpenRouterRouting)
	}
	if override.VercelGatewayRouting != nil {
		merged.VercelGatewayRouting = mergeVercelGatewayRouting(
			merged.VercelGatewayRouting,
			override.VercelGatewayRouting,
		)
	}
	return merged
}

func copyCompat(compat *ai.Compat) *ai.Compat {
	if compat == nil {
		return nil
	}
	return &ai.Compat{
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

func copyOpenRouterRouting(routing *ai.OpenRouterRouting) *ai.OpenRouterRouting {
	if routing == nil {
		return nil
	}
	copied := *routing
	copied.Order = append([]string{}, routing.Order...)
	copied.Only = append([]string{}, routing.Only...)
	copied.Ignore = append([]string{}, routing.Ignore...)
	copied.Quantizations = append([]string{}, routing.Quantizations...)
	copied.MaxPrice = copyAnyMap(routing.MaxPrice)
	return &copied
}

func mergeOpenRouterRouting(
	base *ai.OpenRouterRouting,
	override *ai.OpenRouterRouting,
) *ai.OpenRouterRouting {
	if base == nil {
		return copyOpenRouterRouting(override)
	}
	if override == nil {
		return copyOpenRouterRouting(base)
	}

	merged := copyOpenRouterRouting(base)
	if override.AllowFallbacks != nil {
		merged.AllowFallbacks = copyBoolPtr(override.AllowFallbacks)
	}
	if override.RequireParameters != nil {
		merged.RequireParameters = copyBoolPtr(override.RequireParameters)
	}
	if override.DataCollection != nil {
		merged.DataCollection = copyStringPtr(override.DataCollection)
	}
	if override.ZDR != nil {
		merged.ZDR = copyBoolPtr(override.ZDR)
	}
	if override.EnforceDistillableText != nil {
		merged.EnforceDistillableText = copyBoolPtr(override.EnforceDistillableText)
	}
	if override.Order != nil {
		merged.Order = append([]string{}, override.Order...)
	}
	if override.Only != nil {
		merged.Only = append([]string{}, override.Only...)
	}
	if override.Ignore != nil {
		merged.Ignore = append([]string{}, override.Ignore...)
	}
	if override.Quantizations != nil {
		merged.Quantizations = append([]string{}, override.Quantizations...)
	}
	if override.Sort != nil {
		merged.Sort = override.Sort
	}
	if override.MaxPrice != nil {
		merged.MaxPrice = copyAnyMap(override.MaxPrice)
	}
	if override.PreferredMinThroughput != nil {
		merged.PreferredMinThroughput = override.PreferredMinThroughput
	}
	if override.PreferredMaxLatency != nil {
		merged.PreferredMaxLatency = override.PreferredMaxLatency
	}
	return merged
}

func copyVercelGatewayRouting(routing *ai.VercelGatewayRouting) *ai.VercelGatewayRouting {
	if routing == nil {
		return nil
	}
	copied := *routing
	copied.Only = append([]string{}, routing.Only...)
	copied.Order = append([]string{}, routing.Order...)
	return &copied
}

func mergeVercelGatewayRouting(
	base *ai.VercelGatewayRouting,
	override *ai.VercelGatewayRouting,
) *ai.VercelGatewayRouting {
	if base == nil {
		return copyVercelGatewayRouting(override)
	}
	if override == nil {
		return copyVercelGatewayRouting(base)
	}

	merged := copyVercelGatewayRouting(base)
	if override.Only != nil {
		merged.Only = append([]string{}, override.Only...)
	}
	if override.Order != nil {
		merged.Order = append([]string{}, override.Order...)
	}
	return merged
}

func copyAnyMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}
	copied := make(map[string]any, len(values))
	maps.Copy(copied, values)
	return copied
}
