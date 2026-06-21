type Handler<Event = Record<string, unknown>, Result = unknown> = (
	event: Event,
	ctx: Record<string, unknown>,
) => Result | Promise<Result>;
type ImageContent = {
	mimeType: string;
	data: string;
};
type SystemPromptOptions = {
	/** Custom system prompt (replaces default). */
	customPrompt?: string;
	/** Tools included in the prompt. */
	selectedTools?: string[];
	/** Optional one-line tool snippets keyed by tool name. */
	toolSnippets?: Record<string, string>;
	/** Additional guideline bullets appended to the default system prompt guidelines. */
	promptGuidelines?: string[];
	/** Text appended to the system prompt. */
	appendSystemPrompt?: string;
	/** Working directory. */
	cwd: string;
	/** Pre-loaded context files. */
	contextFiles?: Array<{ path: string; content: string }>;
	/** Pre-loaded skills. */
	skills?: Array<{ name: string; description: string }>;
};
type BeforeAgentStartEvent = {
	type: "before_agent_start";
	prompt: string;
	images?: ImageContent[];
	systemPrompt: string;
	/** Mirrors pi BuildSystemPromptOptions; see core/system-prompt.ts:8-26. */
	systemPromptOptions: SystemPromptOptions;
};
type BeforeAgentStartMessage = {
	customType: string;
	content?: unknown;
	display?: boolean;
	details?: unknown;
};
type BeforeAgentStartEventResult = {
	message?: BeforeAgentStartMessage;
	systemPrompt?: string;
};
type CommandDefinition = {
	description?: string;
	handler?: (args: string, ctx: Record<string, unknown>) => unknown | Promise<unknown>;
};
type ModelInfo = {
	id: string;
	provider: string;
	displayName: string;
	contextWindow: number;
	reasoning: boolean;
};
type ToolDefinition = {
	name?: unknown;
	label?: unknown;
	description?: unknown;
	parameters?: unknown;
	handler?: (args: Record<string, unknown>, ctx: Record<string, unknown>) => unknown | Promise<unknown>;
	execute?: (
		toolCallId: string,
		args: Record<string, unknown>,
		signal: AbortSignal | undefined,
		onUpdate: ((partialResult: unknown) => void) | undefined,
		ctx: Record<string, unknown>,
	) => unknown | Promise<unknown>;
	renderShell?: "default" | "self";
	renderCall?: (input: ToolCallRenderInput) => RenderBlock[];
	renderResult?: (input: ToolResultRenderInput) => RenderBlock[];
};
type ToolCallRenderInput = {
	args?: Record<string, unknown>;
	toolCallId?: string;
	cwd?: string;
	executionStarted?: boolean;
	argsComplete?: boolean;
	expanded?: boolean;
	showImages?: boolean;
	isError?: boolean;
	state?: unknown;
	lastBlocks?: RenderBlock[];
};
type ToolResultRenderInput = {
	args?: Record<string, unknown>;
	result?: {
		content?: unknown[];
		details?: unknown;
		isError?: boolean;
	};
	toolCallId?: string;
	cwd?: string;
	executionStarted?: boolean;
	argsComplete?: boolean;
	expanded?: boolean;
	partial?: boolean;
	showImages?: boolean;
	isError?: boolean;
	state?: unknown;
	lastBlocks?: RenderBlock[];
};
type RenderBlock = {
	kind: string;
	label?: string;
	text?: string;
	style?: string;
	children?: RenderBlock[];
	language?: string;
	mimeType?: string;
	data?: string;
};
type ProviderModelCost = {
	input?: number;
	output?: number;
	cacheRead?: number;
	cacheWrite?: number;
};
type ProviderModelDefinition = {
	id: string;
	name?: string;
	api?: string;
	baseUrl?: string;
	reasoning?: boolean;
	thinkingLevelMap?: Record<string, string | null>;
	input?: Array<"text" | "image">;
	cost?: ProviderModelCost;
	contextWindow?: number;
	maxTokens?: number;
	headers?: Record<string, string>;
	compat?: Record<string, unknown>;
};
type ProviderModelOverrideDefinition = {
	name?: string;
	reasoning?: boolean;
	thinkingLevelMap?: Record<string, string | null>;
	input?: Array<"text" | "image">;
	cost?: ProviderModelCost;
	contextWindow?: number;
	maxTokens?: number;
	headers?: Record<string, string>;
	compat?: Record<string, unknown>;
};
type OAuthCredentials = {
	refresh?: string;
	access?: string;
	expires?: number;
	accountId?: string;
};
type ProviderOAuthDefinition = {
	name?: string;
	login?: (callbacks: unknown) => Promise<OAuthCredentials> | OAuthCredentials;
	refreshToken?: (credentials: OAuthCredentials) => Promise<OAuthCredentials> | OAuthCredentials;
	getApiKey?: (credentials: OAuthCredentials) => string;
	modifyModels?: unknown;
};
type SerializableProviderOAuthDefinition = {
	name: string;
};
// Serializable subset of pi.registerProvider config. OAuth callbacks stay in
// the JS map; the loaded payload carries only the display-name marker:
// .agents/references/pi/packages/coding-agent/src/core/extensions/types.ts:1317-1347
// .agents/references/pi/packages/coding-agent/src/core/model-registry.ts:860-928.
type ProviderDefinition = {
	name?: string;
	baseUrl?: string;
	apiKey?: string;
	api?: string;
	headers?: Record<string, string>;
	authHeader?: boolean;
	compat?: Record<string, unknown>;
	models?: ProviderModelDefinition[];
	modelOverrides?: Record<string, ProviderModelOverrideDefinition>;
	oauth?: ProviderOAuthDefinition;
	streamSimple?: unknown;
};
type SerializableProviderDefinition = Omit<ProviderDefinition, "oauth" | "streamSimple"> & {
	oauth?: SerializableProviderOAuthDefinition;
};
type ExtensionAPI = {
	on(
		eventType: "before_agent_start",
		handler: Handler<BeforeAgentStartEvent, BeforeAgentStartEventResult>,
	): void;
	on(eventType: string, handler: Handler): void;
	registerCommand(name: string, options: CommandDefinition): void;
	registerTool(tool: ToolDefinition): void;
	registerProvider(name: string, definition: ProviderDefinition): void;
	unregisterProvider(name: string): void;
};
type UIResponse = {
	type: "ui_response";
	id: string;
	value?: string;
	confirmed?: boolean;
	cancelled?: boolean;
};
type CtxResponse = {
	type: "ctx_response";
	id: string;
	value?: unknown;
	error?: string;
};
type SessionBeforeForkResult = {
	cancel?: boolean;
	skipConversationRestore?: boolean;
};
type CancellableHookResult = SessionBeforeForkResult;

const handlers = new Map<string, Handler[]>();
const commands = new Map<string, CommandDefinition>();
const tools = new Map<string, ToolDefinition>();
const providers = new Map<string, ProviderDefinition>();
const pendingUI = new Map<string, (response: UIResponse) => void>();
const pendingCtx = new Map<string, { resolve: (value: unknown) => void; reject: (error: Error) => void }>();
let nextUIID = 0;
let nextCtxID = 0;

function writeMessage(value: unknown) {
	process.stdout.write(`${JSON.stringify(value)}\n`);
}

function errorMessage(error: unknown): string {
	if (error instanceof Error) {
		return error.stack ?? error.message;
	}
	return String(error);
}

function writeError(error: unknown) {
	writeMessage({ type: "error", message: errorMessage(error) });
}

function toFileURL(path: string): string {
	if (path.startsWith("file://")) {
		return path;
	}
	return `file://${path}`;
}

function isRecord(value: unknown): value is Record<string, unknown> {
	return value !== null && typeof value === "object" && !Array.isArray(value);
}

function stringRecord(value: unknown): Record<string, string> | undefined {
	if (!isRecord(value)) {
		return undefined;
	}

	const out: Record<string, string> = {};
	for (const [key, recordValue] of Object.entries(value)) {
		if (typeof recordValue !== "string") {
			return undefined;
		}
		out[key] = recordValue;
	}
	return out;
}

function serializeProviderConfig(definition: ProviderDefinition): SerializableProviderDefinition {
	const config: SerializableProviderDefinition = {};
	if (typeof definition.name === "string") config.name = definition.name;
	if (typeof definition.baseUrl === "string") config.baseUrl = definition.baseUrl;
	if (typeof definition.apiKey === "string") config.apiKey = definition.apiKey;
	if (typeof definition.api === "string") config.api = definition.api;
	if (typeof definition.authHeader === "boolean") config.authHeader = definition.authHeader;
	if (isRecord(definition.compat)) config.compat = definition.compat;
	if (Array.isArray(definition.models)) config.models = definition.models;
	if (isRecord(definition.modelOverrides)) {
		config.modelOverrides = definition.modelOverrides as Record<string, ProviderModelOverrideDefinition>;
	}

	const headers = stringRecord(definition.headers);
	if (headers) config.headers = headers;
	if (isRecord(definition.oauth) && typeof definition.oauth.name === "string") {
		config.oauth = { name: definition.oauth.name };
	}
	return config;
}

async function handleLoad(message: Record<string, unknown>) {
	const path = message.path;
	if (typeof path !== "string" || path === "") {
		throw new Error("load.path must be a non-empty string");
	}

	handlers.clear();
	commands.clear();
	tools.clear();
	providers.clear();

	const mod = await import(toFileURL(path));
	const factory = mod.default;
	if (typeof factory !== "function") {
		throw new Error(`extension ${path} has no default factory function`);
	}

	const pi: ExtensionAPI = {
		on(eventType: string, handler: Handler) {
			const list = handlers.get(eventType) ?? [];
			list.push(handler);
			handlers.set(eventType, list);
		},
		registerCommand(name: string, options: CommandDefinition) {
			if (typeof name !== "string" || name === "") {
				throw new Error("registerCommand name must be a non-empty string");
			}
			commands.set(name, { ...options });
		},
		registerTool(tool: ToolDefinition) {
			const name = tool.name;
			if (typeof name !== "string" || name === "") {
				throw new Error("registerTool tool.name must be a non-empty string");
			}
			tools.set(name, { ...tool });
		},
		registerProvider(name: string, definition: ProviderDefinition) {
			if (typeof name !== "string" || name === "") {
				throw new Error("registerProvider name must be a non-empty string");
			}
			if (!isRecord(definition)) {
				throw new Error("registerProvider definition must be an object");
			}
			providers.set(name, { ...definition });
		},
		unregisterProvider(name: string) {
			if (typeof name !== "string" || name === "") {
				throw new Error("unregisterProvider name must be a non-empty string");
			}
			providers.delete(name);
		},
	};

	await factory(pi);
	writeMessage({
		type: "loaded",
		events: Array.from(handlers.keys()),
		commands: Array.from(commands.entries()).map(([name, command]) => ({
			name,
			description: typeof command.description === "string" ? command.description : "",
		})),
		tools: Array.from(tools.values()).map((tool) => ({
			name: tool.name,
			label: typeof tool.label === "string" ? tool.label : typeof tool.name === "string" ? tool.name : "",
			description: typeof tool.description === "string" ? tool.description : "",
			parameters:
				tool.parameters && typeof tool.parameters === "object" && !Array.isArray(tool.parameters) ? tool.parameters : {},
			hasRenderCall: typeof tool.renderCall === "function",
			hasRenderResult: typeof tool.renderResult === "function",
			renderShell: tool.renderShell === "self" ? "self" : tool.renderShell === "default" ? "default" : undefined,
		})),
		providers: Array.from(providers.entries()).map(([name, provider]) => ({
			name,
			config: serializeProviderConfig(provider),
		})),
	});
}

function makeUI(hookID: string) {
	function nextID(): string {
		nextUIID += 1;
		return `${hookID}:ui-${nextUIID}`;
	}

	function request(method: string, fields: Record<string, unknown>, resolveValue: (response: UIResponse) => unknown) {
		const id = nextID();
		writeMessage({ type: "ui_request", id, method, ...fields });

		return new Promise((resolve) => {
			pendingUI.set(id, (response) => {
				pendingUI.delete(id);
				if (response.cancelled) {
					resolve(method === "confirm" ? false : undefined);
					return;
				}
				resolve(resolveValue(response));
			});
		});
	}

	return {
		confirm(title: string, message: string) {
			return request("confirm", { title, message }, (response) => response.confirmed === true);
		},
		select(title: string, options: string[]) {
			return request("select", { title, options }, (response) => response.value);
		},
		input(title: string) {
			return request("input", { title }, (response) => response.value);
		},
		notify(message: string, notifyType?: "info" | "warning" | "error") {
			writeMessage({
				type: "ui_request",
				id: nextID(),
				method: "notify",
				message,
				notifyType,
			});
		},
	};
}

function cloneValue<T>(value: T): T {
	if (value === undefined || value === null) {
		return value;
	}
	return structuredClone(value);
}

function makeCtxRequester(hookID: string) {
	function nextID(): string {
		nextCtxID += 1;
		return `${hookID}:ctx-${nextCtxID}`;
	}

	return function request(method: string, args: Record<string, unknown> = {}) {
		const id = nextID();
		writeMessage({ type: "ctx_request", id, method, args });

		return new Promise((resolve, reject) => {
			pendingCtx.set(id, { resolve, reject });
		});
	};
}

function textFromInput(input: unknown): string {
	if (typeof input === "string") {
		return input;
	}
	if (input && typeof input === "object" && "text" in input) {
		const text = (input as { text?: unknown }).text;
		return typeof text === "string" ? text : "";
	}
	return "";
}

async function makeCtx(hookID: string) {
	const request = makeCtxRequester(hookID);
	async function optionalRequest<T>(method: string, defaultValue: T): Promise<T> {
		try {
			const value = await request(method);
			return (value ?? defaultValue) as T;
		} catch {
			return defaultValue;
		}
	}

	const [
		messages,
		entries,
		modelValue,
		cwdValue,
		hasUIValue,
		isIdleValue,
		hasPendingMessagesValue,
		systemPromptValue,
		modelRegistryValue,
	] = await Promise.all([
		request("sessionManager.getMessages"),
		request("sessionManager.getEntries"),
		request("model"),
		optionalRequest("cwd", ""),
		optionalRequest("hasUI", true),
		optionalRequest("isIdle", false),
		optionalRequest("hasPendingMessages", false),
		optionalRequest("getSystemPrompt", ""),
		optionalRequest<ModelInfo[]>("modelRegistry", []),
	]);

	const modelAccessor = async () => request("model");
	const model =
		modelValue && typeof modelValue === "object" && !Array.isArray(modelValue)
			? new Proxy(modelAccessor, {
					get(target, prop, receiver) {
						if (prop in target) {
							return Reflect.get(target, prop, receiver);
						}
						return (modelValue as Record<PropertyKey, unknown>)[prop];
					},
				})
			: undefined;
	const cwd = typeof cwdValue === "string" ? cwdValue : "";
	const hasUI = typeof hasUIValue === "boolean" ? hasUIValue : true;
	const isIdle = typeof isIdleValue === "boolean" ? isIdleValue : false;
	const hasPendingMessages =
		typeof hasPendingMessagesValue === "boolean" ? hasPendingMessagesValue : false;
	const systemPrompt = typeof systemPromptValue === "string" ? systemPromptValue : "";
	const modelRegistry = Array.isArray(modelRegistryValue) ? modelRegistryValue : [];

	return {
		hasUI,
		cwd,
		ui: makeUI(hookID),
		sessionManager: {
			getMessages() {
				return cloneValue(messages ?? []);
			},
			getEntries() {
				return cloneValue(entries ?? []);
			},
		},
		modelRegistry: cloneValue(modelRegistry),
		model,
		isIdle() {
			return isIdle;
		},
		// TODO(jshost): replace pollable isAborted() with a push-based AbortSignal
		// bridge matching pi ctx.signal:
		// .agents/references/pi/packages/coding-agent/src/core/extensions/types.ts:313-314.
		isAborted() {
			return optionalRequest("isAborted", false);
		},
		abort() {
			return request("abort");
		},
		hasPendingMessages() {
			return hasPendingMessages;
		},
		getSystemPrompt() {
			return systemPrompt;
		},
		compact(options?: Record<string, unknown>) {
			return request("compact", options ?? {});
		},
		setModel(provider: string, id: string) {
			return request("setModel", { provider, id });
		},
		setThinkingLevel(level: string) {
			return request("setThinkingLevel", { level });
		},
		steer(input: unknown) {
			return request("steer", { text: textFromInput(input) });
		},
		followUp(input: unknown) {
			return request("followUp", { text: textFromInput(input) });
		},
		// TODO(jshost): expose shutdown/newSession/fork/switchSession/
		// navigateTree/reload after the bridge can model pi session replacement
		// callbacks:
		// .agents/references/pi/packages/coding-agent/src/core/extensions/types.ts:319-320
		// .agents/references/pi/packages/coding-agent/src/core/extensions/types.ts:333-363.
	};
}

async function handleHook(message: Record<string, unknown>) {
	const id = message.id;
	if (typeof id !== "string" || id === "") {
		throw new Error("hook.id must be a non-empty string");
	}

	const event = message.event;
	if (!event || typeof event !== "object" || Array.isArray(event)) {
		throw new Error("hook.event must be an object");
	}

	const eventType = (event as Record<string, unknown>).type;
	if (typeof eventType !== "string" || eventType === "") {
		throw new Error("hook.event.type must be a non-empty string");
	}

	const ctx = await makeCtx(id);

	let result: unknown;
	let hasResult = false;
	for (const handler of handlers.get(eventType) ?? []) {
		try {
			const handlerResult = await handler(event as Record<string, unknown>, ctx);
			if (handlerResult !== undefined) {
				result = handlerResult;
				hasResult = true;
				if ((handlerResult as CancellableHookResult).cancel === true) {
					break;
				}
			}
		} catch (error) {
			// pi reports a throwing handler as a recoverable extension error
			// and continues with the remaining handlers (runner.ts:698-707);
			// a handler bug must never escape as a fatal host error line.
			console.error(`[jshost] ${eventType} handler error: ${errorMessage(error)}`);
		}
	}

	if (eventType === "tool_call") {
		const resultObject =
			result && typeof result === "object" && !Array.isArray(result) ? { ...(result as Record<string, unknown>) } : {};
		resultObject.input = (event as Record<string, unknown>).input;
		result = resultObject;
		hasResult = true;
	}

	writeMessage({ type: "hook_result", id, result: hasResult ? result : null });
}

async function handleCommandInvoke(message: Record<string, unknown>) {
	const id = message.id;
	if (typeof id !== "string" || id === "") {
		throw new Error("command_invoke.id must be a non-empty string");
	}

	try {
		const name = message.name;
		if (typeof name !== "string" || name === "") {
			throw new Error("command_invoke.name must be a non-empty string");
		}
		if (!Array.isArray(message.args) || !message.args.every((arg) => typeof arg === "string")) {
			throw new Error("command_invoke.args must be a string array");
		}

		const command = commands.get(name);
		if (!command || typeof command.handler !== "function") {
			throw new Error(`command ${name} not found`);
		}

		const ctx = await makeCtx(id);
		await command.handler(message.args.join(" "), ctx);
		writeMessage({ type: "command_result", id });
	} catch (error) {
		writeMessage({ type: "command_result", id, error: errorMessage(error) });
	}
}

async function handleToolInvoke(message: Record<string, unknown>) {
	const id = message.id;
	if (typeof id !== "string" || id === "") {
		throw new Error("tool_invoke.id must be a non-empty string");
	}

	try {
		const name = message.name;
		if (typeof name !== "string" || name === "") {
			throw new Error("tool_invoke.name must be a non-empty string");
		}
		const toolCallId = message.toolCallId;
		if (typeof toolCallId !== "string") {
			throw new Error("tool_invoke.toolCallId must be a string");
		}
		const args =
			message.args && typeof message.args === "object" && !Array.isArray(message.args)
				? (message.args as Record<string, unknown>)
				: {};

		const tool = tools.get(name);
		if (!tool) {
			throw new Error(`tool ${name} not found`);
		}

		const ctx = await makeCtx(id);
		const onUpdate = (partialResult: unknown) => {
			writeMessage({ type: "tool_update", id, partialResult: normalizeToolResult(partialResult) });
		};

		let result: unknown;
		if (typeof tool.execute === "function") {
			result = await tool.execute(toolCallId, args, undefined, onUpdate, ctx);
		} else if (typeof tool.handler === "function") {
			result = await tool.handler(args, ctx);
		} else {
			throw new Error(`tool ${name} has no execute or handler`);
		}

		writeMessage({ type: "tool_result", id, ...normalizeToolResult(result) });
	} catch (error) {
		writeMessage({
			type: "tool_result",
			id,
			content: [{ type: "text", text: errorMessage(error) }],
			isError: true,
		});
	}
}

async function handleToolRender(message: Record<string, unknown>) {
	const id = message.id;
	if (typeof id !== "string" || id === "") {
		throw new Error("tool_render.id must be a non-empty string");
	}

	try {
		const name = message.name;
		if (typeof name !== "string" || name === "") {
			throw new Error("tool_render.name must be a non-empty string");
		}
		const tool = tools.get(name);
		const phase = message.phase === "result" ? "result" : "call";
		const renderFn = phase === "result" ? tool?.renderResult : tool?.renderCall;
		if (!tool || typeof renderFn !== "function") {
			writeMessage({ type: "tool_render_result", id, blocks: [] });
			return;
		}

		const args =
			message.args && typeof message.args === "object" && !Array.isArray(message.args)
				? (message.args as Record<string, unknown>)
				: {};
		let blocks: RenderBlock[];
		if (phase === "result") {
			const result = isRecord(message.result)
				? {
						content: Array.isArray(message.result.content) ? message.result.content : [],
						details: message.result.details,
						isError: message.result.isError === true,
					}
				: undefined;
			const input: ToolResultRenderInput = {
				args,
				result,
				toolCallId: typeof message.toolCallId === "string" ? message.toolCallId : undefined,
				cwd: typeof message.cwd === "string" ? message.cwd : undefined,
				executionStarted: message.executionStarted === true,
				argsComplete: message.argsComplete === true,
				expanded: message.expanded === true,
				partial: message.partial === true,
				showImages: message.showImages === true,
				isError: message.isError === true,
				state: message.state,
				lastBlocks: Array.isArray(message.lastBlocks) ? (message.lastBlocks as RenderBlock[]) : undefined,
			};
			blocks = (tool.renderResult as (i: ToolResultRenderInput) => RenderBlock[])(input);
		} else {
			const input: ToolCallRenderInput = {
				args,
				toolCallId: typeof message.toolCallId === "string" ? message.toolCallId : undefined,
				cwd: typeof message.cwd === "string" ? message.cwd : undefined,
				executionStarted: message.executionStarted === true,
				argsComplete: message.argsComplete === true,
				expanded: message.expanded === true,
				showImages: message.showImages === true,
				isError: message.isError === true,
				state: message.state,
				lastBlocks: Array.isArray(message.lastBlocks) ? (message.lastBlocks as RenderBlock[]) : undefined,
			};
			blocks = (tool.renderCall as (i: ToolCallRenderInput) => RenderBlock[])(input);
		}
		writeMessage({ type: "tool_render_result", id, blocks: Array.isArray(blocks) ? blocks : [] });
	} catch (error) {
		const messageText = error instanceof Error ? error.message : String(error);
		writeMessage({ type: "tool_render_result", id, error: messageText });
	}
}

async function handleOAuthInvoke(message: Record<string, unknown>) {
	const id = message.id;
	if (typeof id !== "string" || id === "") {
		throw new Error("oauth_invoke.id must be a non-empty string");
	}

	let credentials: OAuthCredentials = {};
	try {
		const providerName = message.provider;
		if (typeof providerName !== "string" || providerName === "") {
			throw new Error("oauth_invoke.provider must be a non-empty string");
		}
		const method = message.method;
		if (typeof method !== "string" || method === "") {
			throw new Error("oauth_invoke.method must be a non-empty string");
		}
		const args =
			message.args && typeof message.args === "object" && !Array.isArray(message.args)
				? (message.args as Record<string, unknown>)
				: {};
		credentials = credentialsFromArgs(args);

		const provider = providers.get(providerName);
		const oauth = provider?.oauth;
		if (!oauth) {
			throw new Error(`oauth provider ${providerName} not found`);
		}

		switch (method) {
			case "refreshToken": {
				if (typeof oauth.refreshToken !== "function") {
					throw new Error(`oauth provider ${providerName} has no refreshToken`);
				}
				const refreshed = await oauth.refreshToken(credentials);
				writeMessage({ type: "oauth_result", id, credentials: refreshed });
				return;
			}
			case "getApiKey": {
				if (typeof oauth.getApiKey !== "function") {
					throw new Error(`oauth provider ${providerName} has no getApiKey`);
				}
				const apiKey = oauth.getApiKey(credentials);
				writeMessage({ type: "oauth_result", id, apiKey });
				return;
			}
			case "login": {
				if (typeof oauth.login !== "function") {
					throw new Error(`oauth provider ${providerName} has no login`);
				}
				const loggedIn = await oauth.login({});
				writeMessage({ type: "oauth_result", id, credentials: loggedIn });
				return;
			}
			default:
				throw new Error(`unknown oauth method: ${method}`);
		}
	} catch (error) {
		writeMessage({ type: "oauth_result", id, error: oauthErrorMessage(error, credentials) });
	}
}

function credentialsFromArgs(args: Record<string, unknown>): OAuthCredentials {
	if (!isRecord(args.credentials)) {
		return {};
	}
	return args.credentials as OAuthCredentials;
}

function oauthErrorMessage(error: unknown, credentials: OAuthCredentials): string {
	let message = error instanceof Error ? error.message : String(error);
	for (const value of Object.values(credentials)) {
		if (typeof value === "string" && value !== "") {
			message = message.split(value).join("[redacted]");
		}
	}
	return message;
}

function normalizeToolResult(value: unknown): Record<string, unknown> {
	if (value === undefined || value === null) {
		return { content: [] };
	}
	if (typeof value === "string") {
		return { content: [{ type: "text", text: value }] };
	}
	if (Array.isArray(value)) {
		return { content: value };
	}
	if (typeof value === "object") {
		const result = value as Record<string, unknown>;
		if ("content" in result || "details" in result || "terminate" in result || "isError" in result) {
			return result;
		}
		return {
			content: [{ type: "text", text: JSON.stringify(result) }],
			details: result,
		};
	}
	return { content: [{ type: "text", text: String(value) }] };
}

function handleUIResponse(message: Record<string, unknown>) {
	const id = message.id;
	if (typeof id !== "string") {
		return;
	}

	const resolve = pendingUI.get(id);
	if (!resolve) {
		return;
	}
	resolve(message as UIResponse);
}

function handleCtxResponse(message: Record<string, unknown>) {
	const id = message.id;
	if (typeof id !== "string") {
		return;
	}

	const pending = pendingCtx.get(id);
	if (!pending) {
		return;
	}
	pendingCtx.delete(id);

	if (typeof message.error === "string" && message.error !== "") {
		pending.reject(new Error(message.error));
		return;
	}
	pending.resolve((message as CtxResponse).value);
}

function handleLine(line: string) {
	if (line.trim() === "") {
		return;
	}

	let message: Record<string, unknown>;
	try {
		message = JSON.parse(line);
	} catch (error) {
		writeError(error);
		return;
	}

	switch (message.type) {
		case "load":
			void handleLoad(message).catch(writeError);
			return;
		case "hook":
			void handleHook(message).catch(writeError);
			return;
		case "command_invoke":
			void handleCommandInvoke(message).catch(writeError);
			return;
		case "tool_invoke":
			void handleToolInvoke(message).catch(writeError);
			return;
		case "tool_render":
			void handleToolRender(message).catch(writeError);
			return;
		case "oauth_invoke":
			void handleOAuthInvoke(message).catch(writeError);
			return;
		case "ui_response":
			handleUIResponse(message);
			return;
		case "ctx_response":
			handleCtxResponse(message);
			return;
		default:
			writeError(new Error(`unknown message type: ${String(message.type)}`));
	}
}

async function main() {
	const decoder = new TextDecoder();
	let buffered = "";

	for await (const chunk of Bun.stdin.stream()) {
		buffered += decoder.decode(chunk, { stream: true });

		for (;;) {
			const newline = buffered.indexOf("\n");
			if (newline === -1) {
				break;
			}

			const line = buffered.slice(0, newline);
			buffered = buffered.slice(newline + 1);
			handleLine(line);
		}
	}

	buffered += decoder.decode();
	if (buffered.length > 0) {
		handleLine(buffered);
	}
}

void main().catch(writeError);
