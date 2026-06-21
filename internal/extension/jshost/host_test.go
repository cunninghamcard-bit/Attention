package jshost

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cunninghamcard-bit/Attention/internal/extension"
	"github.com/cunninghamcard-bit/Attention/internal/ai"
	aioauth "github.com/cunninghamcard-bit/Attention/internal/ai/oauth"
	"github.com/cunninghamcard-bit/Attention/internal/hook"
	"github.com/cunninghamcard-bit/Attention/internal/message"
	"github.com/cunninghamcard-bit/Attention/internal/render"
)

func TestMarshalHookPayloadBeforeAgentStartIncludesSystemPromptOptions(t *testing.T) {
	payload, err := marshalHookPayload(hook.EventBeforeAgentStart, hook.BeforeAgentStartEvent{
		Prompt:       "hello",
		SystemPrompt: "system",
		Images: []hook.ImageContent{
			{MimeType: "image/png", Data: "abc"},
		},
		SystemPromptOptions: &hook.SystemPromptOptions{
			CustomPrompt:       "custom",
			SelectedTools:      []string{"read", "bash"},
			ToolSnippets:       map[string]string{"read": "Read files", "bash": "Run shell"},
			PromptGuidelines:   []string{"Be direct"},
			AppendSystemPrompt: "appendix",
			CWD:                "/repo",
			ContextFiles: []hook.ContextFileInfo{
				{Path: "/repo/AGENTS.md", Content: "instructions"},
			},
			Skills: []hook.SkillInfo{
				{Name: "review", Description: "Review code"},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshalHookPayload: %v", err)
	}
	if payload["prompt"] != "hello" {
		t.Fatalf("prompt = %#v, want hello", payload["prompt"])
	}
	if payload["systemPrompt"] != "system" {
		t.Fatalf("systemPrompt = %#v, want system", payload["systemPrompt"])
	}
	if images, ok := payload["images"].([]jsImageContent); !ok || len(images) != 1 || images[0].Data != "abc" {
		t.Fatalf("images = %#v, want one image", payload["images"])
	}

	options, ok := payload["systemPromptOptions"].(map[string]any)
	if !ok {
		t.Fatalf("systemPromptOptions = %#v, want object", payload["systemPromptOptions"])
	}
	if options["customPrompt"] != "custom" || options["appendSystemPrompt"] != "appendix" || options["cwd"] != "/repo" {
		t.Fatalf("systemPromptOptions scalar fields = %#v", options)
	}
	selectedTools, ok := options["selectedTools"].([]string)
	if !ok || !slices.Equal(selectedTools, []string{"read", "bash"}) {
		t.Fatalf("selectedTools = %#v, want read/bash", options["selectedTools"])
	}
	toolSnippets, ok := options["toolSnippets"].(map[string]string)
	if !ok || toolSnippets["read"] != "Read files" || toolSnippets["bash"] != "Run shell" {
		t.Fatalf("toolSnippets = %#v, want read/bash snippets", options["toolSnippets"])
	}
	guidelines, ok := options["promptGuidelines"].([]string)
	if !ok || !slices.Equal(guidelines, []string{"Be direct"}) {
		t.Fatalf("promptGuidelines = %#v, want Be direct", options["promptGuidelines"])
	}
	contextFiles, ok := options["contextFiles"].([]map[string]string)
	if !ok || len(contextFiles) != 1 || contextFiles[0]["path"] != "/repo/AGENTS.md" ||
		contextFiles[0]["content"] != "instructions" {
		t.Fatalf("contextFiles = %#v, want AGENTS.md instructions", options["contextFiles"])
	}
	skills, ok := options["skills"].([]map[string]string)
	if !ok || len(skills) != 1 || skills[0]["name"] != "review" || skills[0]["description"] != "Review code" {
		t.Fatalf("skills = %#v, want review skill", options["skills"])
	}
}

func TestMarshalHookPayloadBeforeAgentStartAllowsNilSystemPromptOptions(t *testing.T) {
	payload, err := marshalHookPayload(hook.EventBeforeAgentStart, hook.BeforeAgentStartEvent{})
	if err != nil {
		t.Fatalf("marshalHookPayload: %v", err)
	}
	options, ok := payload["systemPromptOptions"].(map[string]any)
	if !ok || len(options) != 0 {
		t.Fatalf("systemPromptOptions = %#v, want empty object", payload["systemPromptOptions"])
	}
}

func TestConvertHookResultBeforeAgentStart(t *testing.T) {
	systemPrompt := "patched"
	got, err := convertHookResult(hook.EventBeforeAgentStart, hook.BeforeAgentStartEvent{}, HookResult{
		SystemPrompt: &systemPrompt,
		MessageSet:   true,
		Message: map[string]any{
			"customType": "notice",
			"content":    "hello",
			"display":    true,
		},
		MessagesSet: true,
		Messages: []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "extra"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("convertHookResult: %v", err)
	}
	result, ok := got.(hook.BeforeAgentStartResult)
	if !ok {
		t.Fatalf("result type = %T, want BeforeAgentStartResult", got)
	}
	if result.SystemPrompt == nil || *result.SystemPrompt != "patched" {
		t.Fatalf("SystemPrompt = %v, want patched", result.SystemPrompt)
	}
	if len(result.Messages) != 2 {
		t.Fatalf("Messages len = %d, want 2", len(result.Messages))
	}
	custom, ok := result.Messages[0].(message.CustomMessage)
	if !ok || custom.CustomType != "notice" || custom.Content != "hello" || !custom.Display {
		t.Fatalf("first message = %#v, want notice custom message", result.Messages[0])
	}
	aiMessage, ok := result.Messages[1].(ai.Message)
	if !ok || aiMessage.Role != ai.RoleUser || len(aiMessage.Content) != 1 || aiMessage.Content[0].Text != "extra" {
		t.Fatalf("second message = %#v, want user extra message", result.Messages[1])
	}
}

func TestConvertHookResultBeforeAgentStartEmptyReturnsNil(t *testing.T) {
	got, err := convertHookResult(hook.EventBeforeAgentStart, hook.BeforeAgentStartEvent{}, HookResult{})
	if err != nil {
		t.Fatalf("convertHookResult: %v", err)
	}
	if got != nil {
		t.Fatalf("result = %#v, want nil", got)
	}
}

func TestHostRunsConfirmDestructiveSessionBeforeFork(t *testing.T) {
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skipf("bun not on PATH: %v", err)
	}

	extensionPath := confirmDestructiveExtensionPath(t)

	tests := []struct {
		name       string
		choice     string
		wantCancel *bool
		wantNotify bool
	}{
		{
			name:       "stays in current session cancels fork",
			choice:     "No, stay in current session",
			wantCancel: new(true),
			wantNotify: true,
		},
		{
			name:       "creates fork proceeds",
			choice:     "Yes, create fork",
			wantCancel: nil,
			wantNotify: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			host := &Host{}
			if err := host.Start(ctx); err != nil {
				t.Fatalf("Start error: %v", err)
			}
			t.Cleanup(func() {
				if err := host.Stop(); err != nil {
					t.Fatalf("Stop error: %v", err)
				}
			})

			events, err := host.LoadContext(ctx, extensionPath)
			if err != nil {
				t.Fatalf("LoadContext error: %v", err)
			}
			if !slices.Contains(events, "session_before_fork") {
				t.Fatalf("loaded events = %v, want session_before_fork", events)
			}

			var requests []UIRequest
			result, err := host.FireHook(
				ctx,
				"session_before_fork",
				map[string]any{
					"entryId":  "abcdef1234567890",
					"position": "before",
				},
				func(_ context.Context, request UIRequest) (UIResponse, error) {
					requests = append(requests, request)

					switch request.Method {
					case "select":
						if request.Title != "Fork from entry abcdef12?" {
							return UIResponse{}, errors.New("unexpected select title")
						}
						wantOptions := []string{"Yes, create fork", "No, stay in current session"}
						if !slices.Equal(request.Options, wantOptions) {
							return UIResponse{}, errors.New("unexpected select options")
						}
						return UIResponse{
							ID:    request.ID,
							Value: &tt.choice,
						}, nil
					case "notify":
						if request.Message != "Fork cancelled" || request.NotifyType != "info" {
							return UIResponse{}, errors.New("unexpected notify request")
						}
						return UIResponse{ID: request.ID}, nil
					default:
						return UIResponse{}, errors.New("unexpected UI method")
					}
				},
				noopContextResponder,
			)
			if err != nil {
				t.Fatalf("FireHook error: %v", err)
			}

			if !sameCancel(result.Cancel, tt.wantCancel) {
				t.Fatalf("result cancel = %v, want %v", boolValue(result.Cancel), boolValue(tt.wantCancel))
			}
			if !sawMethod(requests, "select") {
				t.Fatalf("UI requests = %#v, want select", requests)
			}
			if gotNotify := sawMethod(requests, "notify"); gotNotify != tt.wantNotify {
				t.Fatalf("notify seen = %v, want %v; requests = %#v", gotNotify, tt.wantNotify, requests)
			}
		})
	}
}

func TestHostLoadsAndInvokesPhase2CCommandAndTool(t *testing.T) {
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skipf("bun not on PATH: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	host := &Host{}
	if err := host.Start(ctx); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	t.Cleanup(func() {
		if err := host.Stop(); err != nil {
			t.Fatalf("Stop error: %v", err)
		}
	})

	loaded, err := host.LoadExtensionContext(ctx, phase2CExtensionPath(t))
	if err != nil {
		t.Fatalf("LoadExtensionContext error: %v", err)
	}
	if len(loaded.Commands) != 1 || loaded.Commands[0].Name != "phase2c" {
		t.Fatalf("loaded commands = %#v, want phase2c", loaded.Commands)
	}
	if len(loaded.Tools) != 1 || loaded.Tools[0].Name != "phase2c_tool" {
		t.Fatalf("loaded tools = %#v, want phase2c_tool", loaded.Tools)
	}
	if loaded.Tools[0].Parameters["type"] != "object" {
		t.Fatalf("tool parameters = %#v, want object schema", loaded.Tools[0].Parameters)
	}

	rec := &phase2CInvocationRecorder{}
	if err := host.InvokeCommand(
		ctx,
		"phase2c",
		[]string{"alpha", "beta"},
		rec.uiResponder,
		rec.ctxResponder,
	); err != nil {
		t.Fatalf("InvokeCommand error: %v", err)
	}
	if !slices.Contains(rec.notifications, "command:alpha beta") {
		t.Fatalf("notifications = %v, want command notification", rec.notifications)
	}
	if !slices.Contains(rec.steers, "steer:alpha beta") {
		t.Fatalf("steers = %v, want command steer", rec.steers)
	}

	var updates []extension.ToolResult
	result, err := host.InvokeTool(
		ctx,
		"phase2c_tool",
		extension.ToolCall{
			ID:   "call-1",
			Args: map[string]any{"text": "hello"},
		},
		func(partial extension.ToolResult) {
			updates = append(updates, partial)
		},
		rec.uiResponder,
		rec.ctxResponder,
	)
	if err != nil {
		t.Fatalf("InvokeTool error: %v", err)
	}
	if len(updates) != 1 || len(updates[0].Content) != 1 || updates[0].Content[0].Text != "partial:hello" {
		t.Fatalf("updates = %#v, want partial text", updates)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "done:hello:claude-sonnet-4-5" {
		t.Fatalf("tool result content = %#v, want done text", result.Content)
	}
	if !result.IsError {
		t.Fatal("tool result IsError = false, want true")
	}
	details, ok := result.Details.(map[string]any)
	if !ok || details["toolCallId"] != "call-1" || details["provider"] != "anthropic" {
		t.Fatalf("tool result details = %#v, want call/provider", result.Details)
	}
	if !slices.Contains(rec.notifications, "tool:hello:call-1") {
		t.Fatalf("notifications = %v, want tool notification", rec.notifications)
	}
	if !slices.Contains(rec.followUps, "follow:hello") {
		t.Fatalf("follow ups = %v, want tool followUp", rec.followUps)
	}
}

func TestHostLoadsAndInvokesToolRender(t *testing.T) {
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skipf("bun not on PATH: %v", err)
	}

	extensionPath := filepath.Join(t.TempDir(), "render-extension.ts")
	if err := os.WriteFile(extensionPath, []byte(`
export default function (pi) {
	pi.registerTool({
		name: "render_tool",
		label: "Render Tool",
		description: "Renders with neutral blocks",
		parameters: { type: "object" },
		renderShell: "self",
		renderCall(input) {
			return [{
				kind: "text",
				text: [
					"call",
					String(input.args?.text ?? ""),
					input.expanded ? "expanded" : "collapsed",
					String(input.toolCallId ?? ""),
					String(input.cwd ?? ""),
					input.executionStarted ? "started" : "queued",
					input.argsComplete ? "complete" : "partial",
					input.showImages ? "images" : "no-images",
					input.isError ? "error" : "ok",
					String(input.state?.mode ?? ""),
					"last-" + String(Array.isArray(input.lastBlocks) ? input.lastBlocks.length : 0),
				].join(":"),
			}];
		},
		renderResult(input) {
			const content = Array.isArray(input.result?.content) ? input.result.content : [];
			const resultText = content
				.map((block) => block && typeof block === "object" && "text" in block ? block.text : "")
				.join("|");
			const ctxText = [
				"ctx",
				String(input.toolCallId ?? ""),
				String(input.cwd ?? ""),
				input.executionStarted ? "started" : "queued",
				input.argsComplete ? "complete" : "partial",
				input.showImages ? "images" : "no-images",
				input.isError ? "error" : "ok",
				String(input.state?.mode ?? ""),
				"last-" + String(Array.isArray(input.lastBlocks) ? input.lastBlocks.length : 0),
			].join(":");
			return [{
				kind: "group",
				label: input.expanded ? "expanded" : "collapsed",
				children: [
					{ kind: "text", text: "arg:" + String(input.args?.text ?? "") },
					{ kind: "text", text: "result:" + resultText },
					{ kind: "text", text: ctxText },
					{
						kind: "badge",
						text: input.partial ? "partial" : "final",
						style: input.isError ? "warning" : "muted",
					},
				],
			}];
		},
	});
	pi.registerTool({
		name: "plain_tool",
		label: "Plain Tool",
		description: "Uses generic fallback",
		parameters: { type: "object" },
	});
}
`), 0o600); err != nil {
		t.Fatalf("write extension: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	host := &Host{}
	if err := host.Start(ctx); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	t.Cleanup(func() {
		if err := host.Stop(); err != nil {
			t.Fatalf("Stop error: %v", err)
		}
	})

	loaded, err := host.LoadExtensionContext(ctx, extensionPath)
	if err != nil {
		t.Fatalf("LoadExtensionContext error: %v", err)
	}
	renderInfo := findToolInfo(t, loaded.Tools, "render_tool")
	if !renderInfo.HasRenderCall || !renderInfo.HasRenderResult {
		t.Fatalf("render_tool HasRenderCall/HasRenderResult = %v/%v, want true/true",
			renderInfo.HasRenderCall, renderInfo.HasRenderResult)
	}
	if renderInfo.RenderShell != "self" {
		t.Fatalf("render_tool RenderShell = %q, want self", renderInfo.RenderShell)
	}
	plainInfo := findToolInfo(t, loaded.Tools, "plain_tool")
	if plainInfo.HasRenderCall || plainInfo.HasRenderResult {
		t.Fatalf("plain_tool HasRenderCall/HasRenderResult = %v/%v, want false/false",
			plainInfo.HasRenderCall, plainInfo.HasRenderResult)
	}

	ext := loadExtensionWithFactory(t, host, extensionPath)
	renderTool := findToolDefinition(t, ext.Tools, "render_tool")
	if renderTool.RenderCall == nil || renderTool.RenderResult == nil {
		t.Fatal("render_tool RenderCall/RenderResult = nil, want renderers")
	}
	if renderTool.RenderShell != extension.ToolRenderShellSelf {
		t.Fatalf("render_tool RenderShell = %q, want self", renderTool.RenderShell)
	}
	plainTool := findToolDefinition(t, ext.Tools, "plain_tool")
	if plainTool.RenderCall != nil || plainTool.RenderResult != nil {
		t.Fatal("plain_tool RenderCall/RenderResult != nil, want generic fallback")
	}

	callInput := extension.ToolCallRenderInput{
		Args:             map[string]any{"text": "hello"},
		ToolCallID:       "call-1",
		CWD:              "/repo",
		ExecutionStarted: true,
		ArgsComplete:     true,
		Expanded:         true,
		ShowImages:       true,
		IsError:          true,
		State:            map[string]any{"mode": "go"},
		LastBlocks:       []render.Block{render.Text("previous")},
	}
	wantCall := []render.Block{render.Text("call:hello:expanded:call-1:/repo:started:complete:images:error:go:last-1")}

	callBlocks, err := host.InvokeToolRenderCall(ctx, "render_tool", callInput)
	if err != nil {
		t.Fatalf("InvokeToolRenderCall error: %v", err)
	}
	if !reflect.DeepEqual(callBlocks, wantCall) {
		t.Fatalf("InvokeToolRenderCall blocks = %#v, want %#v", callBlocks, wantCall)
	}
	if rendered := renderTool.RenderCall(callInput); !reflect.DeepEqual(rendered, wantCall) {
		t.Fatalf("ToolDefinition.RenderCall blocks = %#v, want %#v", rendered, wantCall)
	}

	resultInput := extension.ToolResultRenderInput{
		Args: map[string]any{"text": "hello"},
		Result: extension.RenderResult{
			Content: []ai.ContentBlock{{Type: ai.ContentText, Text: "done"}},
			Details: map[string]any{
				"source": "go",
			},
			IsError: false,
		},
		ToolCallID:       "call-1",
		CWD:              "/repo",
		ExecutionStarted: true,
		ArgsComplete:     true,
		Expanded:         true,
		Partial:          true,
		ShowImages:       true,
		IsError:          true,
		State:            map[string]any{"mode": "go"},
		LastBlocks:       []render.Block{render.Text("previous")},
	}
	wantResult := []render.Block{
		render.Group("expanded", []render.Block{
			render.Text("arg:hello"),
			render.Text("result:done"),
			render.Text("ctx:call-1:/repo:started:complete:images:error:go:last-1"),
			render.Badge("partial", "warning"),
		}),
	}

	resultBlocks, err := host.InvokeToolRenderResult(ctx, "render_tool", resultInput)
	if err != nil {
		t.Fatalf("InvokeToolRenderResult error: %v", err)
	}
	if !reflect.DeepEqual(resultBlocks, wantResult) {
		t.Fatalf("InvokeToolRenderResult blocks = %#v, want %#v", resultBlocks, wantResult)
	}
	if rendered := renderTool.RenderResult(resultInput); !reflect.DeepEqual(rendered, wantResult) {
		t.Fatalf("ToolDefinition.RenderResult blocks = %#v, want %#v", rendered, wantResult)
	}
}

func TestHostLoadsRegisteredProvider(t *testing.T) {
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skipf("bun not on PATH: %v", err)
	}

	extensionPath := filepath.Join(t.TempDir(), "provider-extension.ts")
	if err := os.WriteFile(extensionPath, []byte(`
export default function (pi) {
	pi.registerProvider("js-openai", {
		name: "JS OpenAI",
		baseUrl: "https://provider.test/v1",
		apiKey: "JS_OPENAI_KEY",
		api: "openai-responses",
		headers: { "X-Provider": "js" },
		authHeader: true,
		compat: { sendSessionIdHeader: true },
		models: [
			{
				id: "js-gpt",
				name: "JS GPT",
				reasoning: true,
				input: ["text", "image"],
				cost: { input: 1, output: 2, cacheRead: 0.5, cacheWrite: 0.25 },
				contextWindow: 123,
				maxTokens: 45,
				headers: { "X-Model": "js-model" },
				compat: { supportsLongCacheRetention: true }
			}
		],
		modelOverrides: {
			"gpt-5": {
				name: "GPT override",
				reasoning: false,
				input: ["text"],
				contextWindow: 99,
				headers: { "X-Override": "js-override" }
			}
		},
		oauth: {
			name: "Batch 3b",
			async login() { return {}; },
			async refreshToken(credentials) { return credentials; },
			getApiKey() { return "oauth-key"; }
		}
	});
}
`), 0o600); err != nil {
		t.Fatalf("write extension: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	host := &Host{}
	if err := host.Start(ctx); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	t.Cleanup(func() {
		if err := host.Stop(); err != nil {
			t.Fatalf("Stop error: %v", err)
		}
	})

	loaded, err := host.LoadExtensionContext(ctx, extensionPath)
	if err != nil {
		t.Fatalf("LoadExtensionContext error: %v", err)
	}
	if len(loaded.Providers) != 1 || loaded.Providers[0].Name != "js-openai" {
		t.Fatalf("loaded providers = %#v, want js-openai", loaded.Providers)
	}

	cfg := loaded.Providers[0].Config
	if cfg.BaseURL == nil || *cfg.BaseURL != "https://provider.test/v1" {
		t.Fatalf("provider baseUrl = %v, want provider URL", cfg.BaseURL)
	}
	if cfg.API == nil || *cfg.API != "openai-responses" {
		t.Fatalf("provider api = %v, want openai-responses", cfg.API)
	}
	if cfg.AuthHeader == nil || !*cfg.AuthHeader {
		t.Fatalf("provider authHeader = %v, want true", cfg.AuthHeader)
	}
	if cfg.Headers["X-Provider"] != "js" {
		t.Fatalf("provider headers = %#v, want X-Provider", cfg.Headers)
	}
	if cfg.Compat == nil || cfg.Compat.SendSessionIdHeader == nil || !*cfg.Compat.SendSessionIdHeader {
		t.Fatalf("provider compat = %#v, want sendSessionIdHeader", cfg.Compat)
	}
	if len(cfg.Models) != 1 || cfg.Models[0].ID != "js-gpt" {
		t.Fatalf("provider models = %#v, want js-gpt", cfg.Models)
	}
	if cfg.Models[0].Cost == nil || cfg.Models[0].Cost.Output == nil || *cfg.Models[0].Cost.Output != 2 {
		t.Fatalf("model cost = %#v, want output 2", cfg.Models[0].Cost)
	}
	override, ok := cfg.ModelOverrides["gpt-5"]
	if !ok || override.Name == nil || *override.Name != "GPT override" {
		t.Fatalf("modelOverrides = %#v, want gpt-5 override", cfg.ModelOverrides)
	}
	if cfg.OAuth == nil || cfg.OAuth.Name != "Batch 3b" {
		t.Fatalf("provider oauth = %#v, want Batch 3b marker", cfg.OAuth)
	}
}

func TestJSOAuthProviderRefreshTokenGetAPIKeyAndLoginPlaceholder(t *testing.T) {
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skipf("bun not on PATH: %v", err)
	}

	extensionPath := filepath.Join(t.TempDir(), "oauth-extension.ts")
	if err := os.WriteFile(extensionPath, []byte(`
export default function (pi) {
	pi.registerProvider("js-oauth", {
		name: "JS OAuth",
		baseUrl: "https://provider.test/v1",
		api: "openai-responses",
		models: [{ id: "js-oauth-model" }],
		oauth: {
			name: "JS OAuth",
			async login() { return {}; },
			async refreshToken(credentials) {
				if (credentials.refresh !== "refresh-old") {
					throw new Error("unexpected refresh input");
				}
				return {
					refresh: "refresh-new",
					access: "access-new",
					expires: 123456789,
					accountId: "acct-1"
				};
			},
			getApiKey(credentials) {
				return "api:" + credentials.access + ":" + credentials.accountId;
			}
		}
	});
}
`), 0o600); err != nil {
		t.Fatalf("write extension: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	host := &Host{}
	if err := host.Start(ctx); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	t.Cleanup(func() {
		if err := host.Stop(); err != nil {
			t.Fatalf("Stop error: %v", err)
		}
	})

	if _, err := host.LoadExtensionContext(ctx, extensionPath); err != nil {
		t.Fatalf("LoadExtensionContext error: %v", err)
	}

	provider := jsOAuthProvider{
		host: host,
		id:   "js-oauth",
		name: "JS OAuth",
	}
	credentials, err := provider.RefreshToken(ctx, "refresh-old")
	if err != nil {
		t.Fatalf("RefreshToken error: %v", err)
	}
	if credentials.Refresh != "refresh-new" ||
		credentials.Access != "access-new" ||
		credentials.Expires != 123456789 ||
		credentials.AccountID != "acct-1" {
		t.Fatalf("credentials = %+v, want JS-produced credentials", credentials)
	}

	apiKey := provider.GetAPIKey(aioauth.Credentials{
		Access:    "access-new",
		AccountID: "acct-1",
	})
	if apiKey != "api:access-new:acct-1" {
		t.Fatalf("GetAPIKey = %q, want JS-produced key", apiKey)
	}

	_, err = provider.Login(ctx, aioauth.LoginCallbacks{})
	if err == nil ||
		!strings.Contains(err.Error(), `extension provider "js-oauth" interactive login is not yet supported`) {
		t.Fatalf("Login error = %v, want not-yet-supported error", err)
	}
}

func TestJSOAuthProviderRefreshTokenRedactsCredentialInError(t *testing.T) {
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skipf("bun not on PATH: %v", err)
	}

	extensionPath := filepath.Join(t.TempDir(), "oauth-error-extension.ts")
	if err := os.WriteFile(extensionPath, []byte(`
export default function (pi) {
	pi.registerProvider("js-oauth-error", {
		name: "JS OAuth Error",
		baseUrl: "https://provider.test/v1",
		api: "openai-responses",
		models: [{ id: "js-oauth-error-model" }],
		oauth: {
			name: "JS OAuth Error",
			async login() { return {}; },
			async refreshToken(credentials) {
				throw new Error("refresh failed for " + credentials.refresh);
			},
			getApiKey() { return ""; }
		}
	});
}
`), 0o600); err != nil {
		t.Fatalf("write extension: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	host := &Host{}
	if err := host.Start(ctx); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	t.Cleanup(func() {
		if err := host.Stop(); err != nil {
			t.Fatalf("Stop error: %v", err)
		}
	})

	if _, err := host.LoadExtensionContext(ctx, extensionPath); err != nil {
		t.Fatalf("LoadExtensionContext error: %v", err)
	}

	provider := jsOAuthProvider{
		host: host,
		id:   "js-oauth-error",
		name: "JS OAuth Error",
	}
	_, err := provider.RefreshToken(ctx, "refresh-secret")
	if err == nil {
		t.Fatal("RefreshToken error = nil, want JS error")
	}
	if strings.Contains(err.Error(), "refresh-secret") {
		t.Fatalf("RefreshToken error leaked token material: %v", err)
	}
}

func TestHostExposesModelRegistryAndIsAbortedOnContext(t *testing.T) {
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skipf("bun not on PATH: %v", err)
	}

	extensionPath := filepath.Join(t.TempDir(), "ctx-probe-extension.ts")
	if err := os.WriteFile(extensionPath, []byte(`
export default function (pi) {
	pi.registerCommand("ctx_probe", {
		description: "Probe ctx bridge",
		async handler(_args, ctx) {
			const models = Array.isArray(ctx.modelRegistry) ? ctx.modelRegistry : [];
			const first = models[0] ?? {};
			const aborted = await ctx.isAborted();
			ctx.ui.notify(
				"model:" +
					first.id +
					":" +
					first.provider +
					":" +
					first.displayName +
					":" +
					first.contextWindow +
					":" +
					first.reasoning,
				"info",
			);
			ctx.ui.notify("aborted:" + aborted, "info");
		},
	});
}
`), 0o600); err != nil {
		t.Fatalf("write extension: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	host := &Host{}
	if err := host.Start(ctx); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	t.Cleanup(func() {
		if err := host.Stop(); err != nil {
			t.Fatalf("Stop error: %v", err)
		}
	})

	loaded, err := host.LoadExtensionContext(ctx, extensionPath)
	if err != nil {
		t.Fatalf("LoadExtensionContext error: %v", err)
	}
	if len(loaded.Commands) != 1 || loaded.Commands[0].Name != "ctx_probe" {
		t.Fatalf("loaded commands = %#v, want ctx_probe", loaded.Commands)
	}

	rec := &ctxBridgeRecorder{}
	if err := host.InvokeCommand(
		ctx,
		"ctx_probe",
		[]string{"probe"},
		rec.uiResponder,
		rec.ctxResponder,
	); err != nil {
		t.Fatalf("InvokeCommand error: %v", err)
	}
	if !slices.Contains(
		rec.notifications,
		"model:registry-a:provider-a:Registry A:111:true",
	) {
		t.Fatalf("notifications = %v, want model registry notification", rec.notifications)
	}
	if !slices.Contains(rec.notifications, "aborted:true") {
		t.Fatalf("notifications = %v, want aborted notification", rec.notifications)
	}
}

func TestHandleContextRequestModelRegistryAndIsAborted(t *testing.T) {
	extCtx := extension.ExtensionContext{
		ModelRegistry: func() []extension.ModelInfo {
			return []extension.ModelInfo{
				{
					ID:            "registry-a",
					Provider:      "provider-a",
					DisplayName:   "Registry A",
					ContextWindow: 111,
					Reasoning:     true,
				},
			}
		},
		IsAborted: func() bool {
			return true
		},
	}

	gotModels, err := handleContextRequest(
		context.Background(),
		extCtx,
		ContextRequest{Method: "modelRegistry"},
	)
	if err != nil {
		t.Fatalf("modelRegistry error: %v", err)
	}
	models, ok := gotModels.([]extension.ModelInfo)
	if !ok || len(models) != 1 || models[0].ID != "registry-a" {
		t.Fatalf("modelRegistry = %#v, want registry-a", gotModels)
	}

	gotAborted, err := handleContextRequest(
		context.Background(),
		extCtx,
		ContextRequest{Method: "isAborted"},
	)
	if err != nil {
		t.Fatalf("isAborted error: %v", err)
	}
	if gotAborted != true {
		t.Fatalf("isAborted = %#v, want true", gotAborted)
	}

	gotDefaultModels, err := handleContextRequest(
		context.Background(),
		extension.ExtensionContext{},
		ContextRequest{Method: "modelRegistry"},
	)
	if err != nil {
		t.Fatalf("default modelRegistry error: %v", err)
	}
	if models, ok := gotDefaultModels.([]extension.ModelInfo); !ok || len(models) != 0 {
		t.Fatalf("default modelRegistry = %#v, want empty slice", gotDefaultModels)
	}

	gotDefaultAborted, err := handleContextRequest(
		context.Background(),
		extension.ExtensionContext{},
		ContextRequest{Method: "isAborted"},
	)
	if err != nil {
		t.Fatalf("default isAborted error: %v", err)
	}
	if gotDefaultAborted != false {
		t.Fatalf("default isAborted = %#v, want false", gotDefaultAborted)
	}
}

func TestContextResponderAnswersIsAbortedAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	extCtx := extension.ExtensionContext{
		IsAborted: func() bool {
			return ctx.Err() != nil
		},
	}
	cancel()

	resp, err := ctxResponder(extCtx)(ctx, ContextRequest{Method: "isAborted"})
	if err != nil {
		t.Fatalf("ctxResponder error: %v", err)
	}
	if resp.Error != "" {
		t.Fatalf("ctxResponder error response = %q, want empty", resp.Error)
	}
	if resp.Value != true {
		t.Fatalf("ctxResponder value = %#v, want true", resp.Value)
	}
}

func confirmDestructiveExtensionPath(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}

	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	return filepath.Join(
		root,
		".agents",
		"references",
		"pi",
		"packages",
		"coding-agent",
		"examples",
		"extensions",
		"confirm-destructive.ts",
	)
}

func phase2CExtensionPath(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}

	return filepath.Join(filepath.Dir(file), "testdata", "phase2c-extension.ts")
}

func loadExtensionWithFactory(t *testing.T, host *Host, path string) extension.Extension {
	t.Helper()

	resultCh := make(chan extensionFactoryResult, 1)
	go func() {
		ext, err := extension.Load(
			path,
			hook.NewRegistry(),
			func(context.Context) extension.ExtensionContext {
				return extension.ExtensionContext{}
			},
			ExtensionFactory(host, path),
		)
		resultCh <- extensionFactoryResult{ext: ext, err: err}
	}()

	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("ExtensionFactory load error: %v", result.err)
		}
		return result.ext
	case <-time.After(2 * time.Second):
		t.Fatal("ExtensionFactory load did not finish")
	}
	return extension.Extension{}
}

type extensionFactoryResult struct {
	ext extension.Extension
	err error
}

func findToolInfo(t *testing.T, tools []ToolInfo, name string) ToolInfo {
	t.Helper()

	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool info %q not found in %#v", name, tools)
	return ToolInfo{}
}

func findToolDefinition(
	t *testing.T,
	tools []extension.ToolDefinition,
	name string,
) extension.ToolDefinition {
	t.Helper()

	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool definition %q not found in %#v", name, tools)
	return extension.ToolDefinition{}
}

type phase2CInvocationRecorder struct {
	notifications []string
	steers        []string
	followUps     []string
}

func (r *phase2CInvocationRecorder) uiResponder(_ context.Context, request UIRequest) (UIResponse, error) {
	switch request.Method {
	case "notify":
		r.notifications = append(r.notifications, request.Message)
		return UIResponse{ID: request.ID}, nil
	default:
		return UIResponse{}, errors.New("unexpected UI method")
	}
}

func (r *phase2CInvocationRecorder) ctxResponder(
	_ context.Context,
	request ContextRequest,
) (ContextResponse, error) {
	switch request.Method {
	case "sessionManager.getMessages", "sessionManager.getEntries":
		return ContextResponse{Value: []any{}}, nil
	case "model":
		model, _ := ai.GetModel("", "claude-sonnet-4-5")
		return ContextResponse{Value: model}, nil
	case "steer":
		r.steers = append(r.steers, request.Args["text"].(string))
		return ContextResponse{}, nil
	case "followUp":
		r.followUps = append(r.followUps, request.Args["text"].(string))
		return ContextResponse{}, nil
	default:
		return ContextResponse{}, errors.New("unexpected ctx method")
	}
}

type ctxBridgeRecorder struct {
	notifications []string
}

func (r *ctxBridgeRecorder) uiResponder(_ context.Context, request UIRequest) (UIResponse, error) {
	switch request.Method {
	case "notify":
		r.notifications = append(r.notifications, request.Message)
		return UIResponse{ID: request.ID}, nil
	default:
		return UIResponse{}, errors.New("unexpected UI method")
	}
}

func (r *ctxBridgeRecorder) ctxResponder(
	_ context.Context,
	request ContextRequest,
) (ContextResponse, error) {
	switch request.Method {
	case "sessionManager.getMessages", "sessionManager.getEntries":
		return ContextResponse{Value: []any{}}, nil
	case "model":
		return ContextResponse{Value: nil}, nil
	case "modelRegistry":
		return ContextResponse{
			Value: []extension.ModelInfo{
				{
					ID:            "registry-a",
					Provider:      "provider-a",
					DisplayName:   "Registry A",
					ContextWindow: 111,
					Reasoning:     true,
				},
			},
		}, nil
	case "isAborted":
		return ContextResponse{Value: true}, nil
	default:
		// host.ts pre-fetches optional ctx values (cwd, hasUI, isIdle,
		// hasPendingMessages, getSystemPrompt) via optionalRequest; answer them
		// benignly so the JS-side defaults apply instead of racing a bridge error.
		return ContextResponse{Value: nil}, nil
	}
}

func sawMethod(requests []UIRequest, method string) bool {
	return slices.ContainsFunc(requests, func(request UIRequest) bool {
		return request.Method == method
	})
}

func sameCancel(got, want *bool) bool {
	if got == nil || want == nil {
		return got == want
	}
	return *got == *want
}

//go:fix inline
func boolPtr(value bool) *bool {
	return new(value)
}

func boolValue(value *bool) string {
	if value == nil {
		return "<nil>"
	}
	if *value {
		return "true"
	}
	return "false"
}

func noopContextResponder(_ context.Context, request ContextRequest) (ContextResponse, error) {
	switch request.Method {
	case "sessionManager.getMessages", "sessionManager.getEntries":
		return ContextResponse{Value: []any{}}, nil
	case "model":
		return ContextResponse{Value: nil}, nil
	default:
		return ContextResponse{}, errors.New("unexpected ctx method")
	}
}
