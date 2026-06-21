export default function (pi: {
	registerCommand(
		name: string,
		options: {
			description?: string;
			handler: (args: string, ctx: Record<string, any>) => Promise<void> | void;
		},
	): void;
	registerTool(tool: {
		name: string;
		label: string;
		description: string;
		parameters: Record<string, any>;
		execute(
			toolCallId: string,
			args: Record<string, any>,
			signal: AbortSignal | undefined,
			onUpdate: ((partialResult: unknown) => void) | undefined,
			ctx: Record<string, any>,
		): Promise<unknown>;
	}): void;
}) {
	pi.registerCommand("phase2c", {
		description: "Run phase 2c command",
		async handler(args, ctx) {
			ctx.ui.notify(`command:${args}`, "info");
			await ctx.steer(`steer:${args}`);
		},
	});

	pi.registerTool({
		name: "phase2c_tool",
		label: "Phase 2c Tool",
		description: "Run phase 2c tool",
		parameters: {
			type: "object",
			properties: {
				text: { type: "string" },
			},
			required: ["text"],
		},
		async execute(toolCallId, args, _signal, onUpdate, ctx) {
			const model = await ctx.model();
			ctx.ui.notify(`tool:${args.text}:${toolCallId}`, "info");
			await ctx.followUp(`follow:${args.text}`);
			onUpdate?.({
				content: [{ type: "text", text: `partial:${args.text}` }],
				details: { toolCallId },
			});

			return {
				content: [{ type: "text", text: `done:${args.text}:${model.id}` }],
				details: { toolCallId, provider: model.provider },
				isError: true,
			};
		},
	});
}
