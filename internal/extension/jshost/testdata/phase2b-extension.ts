export default function (pi: {
	on(eventType: string, handler: (event: Record<string, any>, ctx: Record<string, any>) => unknown | Promise<unknown>): void;
}) {
	pi.on("tool_call", async (event, ctx) => {
		const entries = ctx.sessionManager.getEntries();
		const messages = ctx.sessionManager.getMessages();
		const model = await ctx.model();

		if (!Array.isArray(entries)) {
			throw new Error("ctx.sessionManager.getEntries() did not return an array");
		}
		if (!Array.isArray(messages)) {
			throw new Error("ctx.sessionManager.getMessages() did not return an array");
		}
		if (!model || model.provider !== "anthropic" || model.id !== "claude-sonnet-4-5") {
			throw new Error(`ctx.model() returned ${JSON.stringify(model)}`);
		}

		event.input.path = `${event.input.path}:patched:${entries.length}:${messages.length}`;
		return {
			block: true,
			reason: `${model.provider}/${model.id}`,
		};
	});

	pi.on("before_provider_request", (event) => {
		if (event.type !== "before_provider_request") {
			throw new Error(`provider alias event type = ${event.type}`);
		}
		if (!event.payload || event.payload.prompt !== "original") {
			throw new Error(`provider alias payload = ${JSON.stringify(event.payload)}`);
		}

		return {
			payload: {
				...event.payload,
				aliasPatched: true,
				modelId: event.model.id,
			},
		};
	});
}
