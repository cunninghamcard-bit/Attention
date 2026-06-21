export default function activate(along, ctx) {
  along.tools.register({ name: "greet", inputSchema: {} });
  along.tools.register({ name: "shout", inputSchema: {} }, (args) => {
    return {
      content: String(args?.text ?? "").toUpperCase(),
      details: { owner: ctx.owner },
    };
  });
  along.tools.register({ name: "fail", inputSchema: {} }, () => {
    throw new Error("tool exploded");
  });
  along.commands.on("hello", () => {});
  along.commands.on("echo", (payload) => {
    return { ok: true, got: payload };
  });
  along.commands.on("explode", () => {
    throw new Error("command exploded");
  });
  along.events.emit("plugin:activated", {
    msg: "hello",
    owner: ctx.owner,
    sessionId: ctx.sessionId ?? null,
  });
  return () => {
    along.events.emit("plugin:disposed", {
      msg: "bye",
      owner: ctx.owner,
      sessionId: ctx.sessionId ?? null,
    });
  };
}
