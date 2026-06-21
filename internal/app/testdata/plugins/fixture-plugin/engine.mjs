export default function activate(along, ctx) {
  along.events.emit("engine", {
    owner: ctx.owner,
    pluginId: ctx.pluginId,
  });
}
