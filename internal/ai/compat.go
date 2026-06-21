package ai

type Compat struct {
	SupportsStore                               *bool                 `json:"supportsStore,omitempty"`
	SupportsDeveloperRole                       *bool                 `json:"supportsDeveloperRole,omitempty"`
	SupportsReasoningEffort                     *bool                 `json:"supportsReasoningEffort,omitempty"`
	SupportsUsageInStreaming                    *bool                 `json:"supportsUsageInStreaming,omitempty"`
	RequiresToolResultName                      *bool                 `json:"requiresToolResultName,omitempty"`
	RequiresAssistantAfterToolResult            *bool                 `json:"requiresAssistantAfterToolResult,omitempty"`
	RequiresThinkingAsText                      *bool                 `json:"requiresThinkingAsText,omitempty"`
	RequiresReasoningContentOnAssistantMessages *bool                 `json:"requiresReasoningContentOnAssistantMessages,omitempty"`
	ZaiToolStream                               *bool                 `json:"zaiToolStream,omitempty"`
	SupportsStrictMode                          *bool                 `json:"supportsStrictMode,omitempty"`
	SendSessionAffinityHeaders                  *bool                 `json:"sendSessionAffinityHeaders,omitempty"`
	SupportsLongCacheRetention                  *bool                 `json:"supportsLongCacheRetention,omitempty"`
	MaxTokensField                              *string               `json:"maxTokensField,omitempty"`
	ThinkingFormat                              *string               `json:"thinkingFormat,omitempty"`
	CacheControlFormat                          *string               `json:"cacheControlFormat,omitempty"`
	OpenRouterRouting                           *OpenRouterRouting    `json:"openRouterRouting,omitempty"`
	VercelGatewayRouting                        *VercelGatewayRouting `json:"vercelGatewayRouting,omitempty"`

	SendSessionIdHeader             *bool `json:"sendSessionIdHeader,omitempty"`
	SupportsEagerToolInputStreaming *bool `json:"supportsEagerToolInputStreaming,omitempty"`
	SupportsCacheControlOnTools     *bool `json:"supportsCacheControlOnTools,omitempty"`
	ForceAdaptiveThinking           *bool `json:"forceAdaptiveThinking,omitempty"`
}

type OpenRouterRouting struct {
	AllowFallbacks         *bool          `json:"allow_fallbacks,omitempty"`
	RequireParameters      *bool          `json:"require_parameters,omitempty"`
	DataCollection         *string        `json:"data_collection,omitempty"`
	ZDR                    *bool          `json:"zdr,omitempty"`
	EnforceDistillableText *bool          `json:"enforce_distillable_text,omitempty"`
	Order                  []string       `json:"order,omitempty"`
	Only                   []string       `json:"only,omitempty"`
	Ignore                 []string       `json:"ignore,omitempty"`
	Quantizations          []string       `json:"quantizations,omitempty"`
	Sort                   any            `json:"sort,omitempty"`
	MaxPrice               map[string]any `json:"max_price,omitempty"`
	PreferredMinThroughput any            `json:"preferred_min_throughput,omitempty"`
	PreferredMaxLatency    any            `json:"preferred_max_latency,omitempty"`
}

type VercelGatewayRouting struct {
	Only  []string `json:"only,omitempty"`
	Order []string `json:"order,omitempty"`
}
