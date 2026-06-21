package jshost

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"

	"github.com/cunninghamcard-bit/Attention/internal/ai"
	"github.com/cunninghamcard-bit/Attention/internal/ai/oauth"
	"github.com/cunninghamcard-bit/Attention/internal/render"
)

type loadRequest struct {
	Type string `json:"type"`
	Path string `json:"path"`
}

type hookRequest struct {
	Type  string         `json:"type"`
	ID    string         `json:"id"`
	Event map[string]any `json:"event"`
}

type commandInvokeRequest struct {
	Type string   `json:"type"`
	ID   string   `json:"id"`
	Name string   `json:"name"`
	Args []string `json:"args"`
}

type toolInvokeRequest struct {
	Type       string         `json:"type"`
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	ToolCallID string         `json:"toolCallId"`
	Args       map[string]any `json:"args"`
}

type oauthInvokeRequest struct {
	Type     string         `json:"type"`
	ID       string         `json:"id"`
	Provider string         `json:"provider"`
	Method   string         `json:"method"`
	Args     map[string]any `json:"args,omitempty"`
}

type toolRenderRequest struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
	// Phase is "call" or "result", selecting the JS renderCall/renderResult fn.
	Phase            string                 `json:"phase"`
	Args             map[string]any         `json:"args,omitempty"`
	Result           *toolRenderResultInput `json:"result,omitempty"`
	Expanded         bool                   `json:"expanded,omitempty"`
	Partial          bool                   `json:"partial,omitempty"`
	ToolCallID       string                 `json:"toolCallId,omitempty"`
	CWD              string                 `json:"cwd,omitempty"`
	ExecutionStarted bool                   `json:"executionStarted,omitempty"`
	ArgsComplete     bool                   `json:"argsComplete,omitempty"`
	ShowImages       bool                   `json:"showImages,omitempty"`
	IsError          bool                   `json:"isError,omitempty"`
	State            any                    `json:"state,omitempty"`
	LastBlocks       []render.Block         `json:"lastBlocks,omitempty"`
}

type toolRenderResultInput struct {
	Content []ai.ContentBlock `json:"content,omitempty"`
	Details any               `json:"details,omitempty"`
	IsError bool              `json:"isError,omitempty"`
}

// LoadedExtension is the serializable capability set returned by the JS host
// after an extension factory runs.
type LoadedExtension struct {
	Events    []string
	Commands  []CommandInfo
	Tools     []ToolInfo
	Providers []ProviderInfo
}

// CommandInfo is a JS slash command advertised by the loaded extension.
type CommandInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// ToolInfo is a JS tool advertised by the loaded extension.
type ToolInfo struct {
	Name            string         `json:"name"`
	Label           string         `json:"label,omitempty"`
	Description     string         `json:"description,omitempty"`
	Parameters      map[string]any `json:"parameters,omitempty"`
	HasRenderCall   bool           `json:"hasRenderCall,omitempty"`
	HasRenderResult bool           `json:"hasRenderResult,omitempty"`
	RenderShell     string         `json:"renderShell,omitempty"`
}

// ProviderInfo is a JS provider advertised by the loaded extension. It mirrors
// pi's registerProvider config and model-registry applyProviderConfig fields
// without OAuth callbacks for batch 3a:
// .agents/references/pi/packages/coding-agent/src/core/extensions/types.ts:1317-1347
// .agents/references/pi/packages/coding-agent/src/core/model-registry.ts:860-928.
type ProviderInfo struct {
	Name   string             `json:"name"`
	Config ProviderConfigInfo `json:"config"`
}

type ProviderConfigInfo struct {
	Name           *string                              `json:"name,omitempty"`
	BaseURL        *string                              `json:"baseUrl,omitempty"`
	APIKey         *string                              `json:"apiKey,omitempty"`
	API            *string                              `json:"api,omitempty"`
	Headers        map[string]string                    `json:"headers,omitempty"`
	AuthHeader     *bool                                `json:"authHeader,omitempty"`
	Compat         *ai.Compat                           `json:"compat,omitempty"`
	Models         []ProviderModelInfo                  `json:"models,omitempty"`
	ModelOverrides map[string]ProviderModelOverrideInfo `json:"modelOverrides,omitempty"`
	OAuth          *ProviderOAuthInfo                   `json:"oauth,omitempty"`
}

type ProviderOAuthInfo struct {
	Name string `json:"name"`
}

type ProviderModelInfo struct {
	ID               string                 `json:"id"`
	Name             *string                `json:"name,omitempty"`
	API              *string                `json:"api,omitempty"`
	BaseURL          *string                `json:"baseUrl,omitempty"`
	Reasoning        *bool                  `json:"reasoning,omitempty"`
	ThinkingLevelMap map[string]*string     `json:"thinkingLevelMap,omitempty"`
	Input            []ai.InputCapability   `json:"input,omitempty"`
	Cost             *ProviderModelCostInfo `json:"cost,omitempty"`
	ContextWindow    *int                   `json:"contextWindow,omitempty"`
	MaxTokens        *int                   `json:"maxTokens,omitempty"`
	Headers          map[string]string      `json:"headers,omitempty"`
	Compat           *ai.Compat             `json:"compat,omitempty"`
}

type ProviderModelOverrideInfo struct {
	Name             *string                `json:"name,omitempty"`
	Reasoning        *bool                  `json:"reasoning,omitempty"`
	ThinkingLevelMap map[string]*string     `json:"thinkingLevelMap,omitempty"`
	Input            []ai.InputCapability   `json:"input,omitempty"`
	Cost             *ProviderModelCostInfo `json:"cost,omitempty"`
	ContextWindow    *int                   `json:"contextWindow,omitempty"`
	MaxTokens        *int                   `json:"maxTokens,omitempty"`
	Headers          map[string]string      `json:"headers,omitempty"`
	Compat           *ai.Compat             `json:"compat,omitempty"`
}

type ProviderModelCostInfo struct {
	Input      *float64 `json:"input,omitempty"`
	Output     *float64 `json:"output,omitempty"`
	CacheRead  *float64 `json:"cacheRead,omitempty"`
	CacheWrite *float64 `json:"cacheWrite,omitempty"`
}

type loadedMessage struct {
	Type      string         `json:"type"`
	Events    []string       `json:"events"`
	Commands  []CommandInfo  `json:"commands,omitempty"`
	Tools     []ToolInfo     `json:"tools,omitempty"`
	Providers []ProviderInfo `json:"providers,omitempty"`
}

type hookResultMessage struct {
	Type   string     `json:"type"`
	ID     string     `json:"id"`
	Result HookResult `json:"result"`
}

type commandResultMessage struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Error string `json:"error,omitempty"`
}

type toolResultPayload struct {
	Content   any   `json:"content,omitempty"`
	Details   any   `json:"details,omitempty"`
	IsError   *bool `json:"isError,omitempty"`
	Terminate *bool `json:"terminate,omitempty"`
}

type toolResultMessage struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	toolResultPayload
}

type oauthResult struct {
	Credentials *oauth.Credentials `json:"credentials,omitempty"`
	APIKey      string             `json:"apiKey,omitempty"`
	Error       string             `json:"error,omitempty"`
}

type oauthResultMessage struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	oauthResult
}

type toolRenderMessage struct {
	Type   string         `json:"type"`
	ID     string         `json:"id"`
	Blocks []render.Block `json:"blocks,omitempty"`
	Error  string         `json:"error,omitempty"`
}

type toolUpdateMessage struct {
	Type          string            `json:"type"`
	ID            string            `json:"id"`
	PartialResult toolResultPayload `json:"partialResult"`
}

type errorMessage struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func (m loadedMessage) loadedExtension() LoadedExtension {
	out := LoadedExtension{
		Events:   append([]string(nil), m.Events...),
		Commands: append([]CommandInfo(nil), m.Commands...),
		Tools:    make([]ToolInfo, 0, len(m.Tools)),
		Providers: make(
			[]ProviderInfo,
			0,
			len(m.Providers),
		),
	}
	for _, tool := range m.Tools {
		out.Tools = append(out.Tools, ToolInfo{
			Name:            tool.Name,
			Label:           tool.Label,
			Description:     tool.Description,
			Parameters:      maps.Clone(tool.Parameters),
			HasRenderCall:   tool.HasRenderCall,
			HasRenderResult: tool.HasRenderResult,
			RenderShell:     tool.RenderShell,
		})
	}
	for _, provider := range m.Providers {
		out.Providers = append(out.Providers, copyProviderInfo(provider))
	}
	return out
}

func copyProviderInfo(in ProviderInfo) ProviderInfo {
	return ProviderInfo{
		Name:   in.Name,
		Config: copyProviderConfigInfo(in.Config),
	}
}

func copyProviderConfigInfo(in ProviderConfigInfo) ProviderConfigInfo {
	return ProviderConfigInfo{
		Name:           copyStringPtr(in.Name),
		BaseURL:        copyStringPtr(in.BaseURL),
		APIKey:         copyStringPtr(in.APIKey),
		API:            copyStringPtr(in.API),
		Headers:        maps.Clone(in.Headers),
		AuthHeader:     copyBoolPtr(in.AuthHeader),
		Compat:         in.Compat,
		Models:         copyProviderModelInfos(in.Models),
		ModelOverrides: copyProviderModelOverrideInfos(in.ModelOverrides),
		OAuth:          copyProviderOAuthInfo(in.OAuth),
	}
}

func copyProviderOAuthInfo(in *ProviderOAuthInfo) *ProviderOAuthInfo {
	if in == nil {
		return nil
	}
	return &ProviderOAuthInfo{Name: in.Name}
}

func copyProviderModelInfos(in []ProviderModelInfo) []ProviderModelInfo {
	if in == nil {
		return nil
	}
	out := make([]ProviderModelInfo, 0, len(in))
	for _, model := range in {
		out = append(out, ProviderModelInfo{
			ID:               model.ID,
			Name:             copyStringPtr(model.Name),
			API:              copyStringPtr(model.API),
			BaseURL:          copyStringPtr(model.BaseURL),
			Reasoning:        copyBoolPtr(model.Reasoning),
			ThinkingLevelMap: copyStringPointerMap(model.ThinkingLevelMap),
			Input:            append([]ai.InputCapability(nil), model.Input...),
			Cost:             copyProviderModelCostInfo(model.Cost),
			ContextWindow:    copyIntPtr(model.ContextWindow),
			MaxTokens:        copyIntPtr(model.MaxTokens),
			Headers:          maps.Clone(model.Headers),
			Compat:           model.Compat,
		})
	}
	return out
}

func copyProviderModelOverrideInfos(
	in map[string]ProviderModelOverrideInfo,
) map[string]ProviderModelOverrideInfo {
	if in == nil {
		return nil
	}
	out := make(map[string]ProviderModelOverrideInfo, len(in))
	for id, override := range in {
		out[id] = ProviderModelOverrideInfo{
			Name:             copyStringPtr(override.Name),
			Reasoning:        copyBoolPtr(override.Reasoning),
			ThinkingLevelMap: copyStringPointerMap(override.ThinkingLevelMap),
			Input:            append([]ai.InputCapability(nil), override.Input...),
			Cost:             copyProviderModelCostInfo(override.Cost),
			ContextWindow:    copyIntPtr(override.ContextWindow),
			MaxTokens:        copyIntPtr(override.MaxTokens),
			Headers:          maps.Clone(override.Headers),
			Compat:           override.Compat,
		}
	}
	return out
}

func copyProviderModelCostInfo(in *ProviderModelCostInfo) *ProviderModelCostInfo {
	if in == nil {
		return nil
	}
	return &ProviderModelCostInfo{
		Input:      copyFloatPtr(in.Input),
		Output:     copyFloatPtr(in.Output),
		CacheRead:  copyFloatPtr(in.CacheRead),
		CacheWrite: copyFloatPtr(in.CacheWrite),
	}
}

func copyStringPointerMap(in map[string]*string) map[string]*string {
	if in == nil {
		return nil
	}
	out := make(map[string]*string, len(in))
	for key, value := range in {
		out[key] = copyStringPtr(value)
	}
	return out
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

// HookResult is the JSON result returned by pi event handlers. Raw is preserved
// for before_provider_request compatibility because pi lets handlers return the
// replacement payload directly.
//
// Cancel/Block/etc. are pointers where omission differs from explicit false.
type HookResult struct {
	Raw         json.RawMessage `json:"-"`
	MessageSet  bool            `json:"-"`
	MessagesSet bool            `json:"-"`
	PayloadSet  bool            `json:"-"`
	TextSet     bool            `json:"-"`
	ImagesSet   bool            `json:"-"`
	ContentSet  bool            `json:"-"`
	DetailsSet  bool            `json:"-"`

	Cancel                  *bool          `json:"cancel,omitempty"`
	SkipConversationRestore *bool          `json:"skipConversationRestore,omitempty"`
	Block                   *bool          `json:"block,omitempty"`
	Reason                  string         `json:"reason,omitempty"`
	Input                   map[string]any `json:"input,omitempty"`
	Message                 any            `json:"message,omitempty"`
	Messages                []any          `json:"messages,omitempty"`
	Payload                 any            `json:"payload,omitempty"`
	Action                  string         `json:"action,omitempty"`
	Text                    *string        `json:"text,omitempty"`
	SystemPrompt            *string        `json:"systemPrompt,omitempty"`
	Images                  any            `json:"images,omitempty"`
	Content                 any            `json:"content,omitempty"`
	Details                 any            `json:"details,omitempty"`
	IsError                 *bool          `json:"isError,omitempty"`
	Terminate               *bool          `json:"terminate,omitempty"`
}

func (r *HookResult) UnmarshalJSON(data []byte) error {
	r.Raw = append(r.Raw[:0], data...)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil
	}
	if data[0] != '{' {
		return nil
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	_, r.MessageSet = fields["message"]
	_, r.MessagesSet = fields["messages"]
	_, r.PayloadSet = fields["payload"]
	_, r.TextSet = fields["text"]
	_, r.ImagesSet = fields["images"]
	_, r.ContentSet = fields["content"]
	_, r.DetailsSet = fields["details"]

	type hookResultFields HookResult
	var decoded hookResultFields
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	decoded.Raw = r.Raw
	decoded.MessageSet = r.MessageSet
	decoded.MessagesSet = r.MessagesSet
	decoded.PayloadSet = r.PayloadSet
	decoded.TextSet = r.TextSet
	decoded.ImagesSet = r.ImagesSet
	decoded.ContentSet = r.ContentSet
	decoded.DetailsSet = r.DetailsSet
	*r = HookResult(decoded)
	return nil
}

// UIRequest is a host-to-along extension UI request. Phase 1 supports the
// serializable methods confirm/select/input/notify from pi's ExtensionUIContext:
// .agents/references/pi/packages/coding-agent/src/core/extensions/types.ts:124-135.
type UIRequest struct {
	Type       string   `json:"type"`
	ID         string   `json:"id"`
	Method     string   `json:"method"`
	Title      string   `json:"title,omitempty"`
	Message    string   `json:"message,omitempty"`
	NotifyType string   `json:"notifyType,omitempty"`
	Options    []string `json:"options,omitempty"`
}

// UIResponse is an along-to-host response for UIRequest. Notify is fire-and-
// forget in the host, but callers may still return this shape for uniformity.
type UIResponse struct {
	Type      string  `json:"type"`
	ID        string  `json:"id"`
	Value     *string `json:"value,omitempty"`
	Confirmed *bool   `json:"confirmed,omitempty"`
	Cancelled bool    `json:"cancelled"`
}

// UIResponder maps a UI request emitted by the Bun extension host into the
// response line sent back over stdin.
type UIResponder func(ctx context.Context, request UIRequest) (UIResponse, error)

// ContextRequest is a host-to-along request for the runtime ExtensionContext.
type ContextRequest struct {
	Type   string         `json:"type"`
	ID     string         `json:"id"`
	Method string         `json:"method"`
	Args   map[string]any `json:"args,omitempty"`
}

// ContextResponse is the along-to-host response for ContextRequest.
type ContextResponse struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Value any    `json:"value,omitempty"`
	Error string `json:"error,omitempty"`
}

// ContextResponder maps a ctx request emitted by the Bun extension host into
// the response line sent back over stdin.
type ContextResponder func(ctx context.Context, request ContextRequest) (ContextResponse, error)
