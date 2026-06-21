export default function activate(along, ctx) {
  // Tool blocks on a ui.confirm and returns whatever the client resolved with.
  along.tools.register({ name: "askConfirm", inputSchema: {} }, async () => {
    const answer = await along.ui.confirm("Proceed?", "details");
    return { content: "asked", details: { answer, owner: ctx.owner } };
  });
  return () => {};
}
