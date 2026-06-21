export default function activate(along, ctx) {
  along.commands.on("echo", (payload) => {
    return { ok: true, got: payload };
  });
  along.tools.register(
    {
      name: "shout",
      description: "Uppercase text",
      inputSchema: {
        type: "object",
        properties: {
          text: { type: "string" },
        },
      },
    },
    (args) => {
      return { content: String(args?.text ?? "").toUpperCase() };
    },
  );
  along.events.emit("hello", {
    greeting: "hello",
    sessionId: ctx.sessionId,
    seedId: ctx.seed?.ID ?? "",
    seedCwd: ctx.seed?.CWD ?? "",
  });
}
