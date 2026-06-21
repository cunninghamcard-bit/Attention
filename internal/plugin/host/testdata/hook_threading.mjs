export default function activate(along) {
  along.hooks.register("tool_call", (event) => {
    return {
      input: {
        ...event.input,
        trace: `${event.input?.trace ?? ""}0`,
      },
    };
  });
  along.hooks.register("tool_call", (event) => {
    return {
      input: {
        ...event.input,
        trace: `${event.input?.trace ?? ""}1`,
        seenBySecond: event.input?.trace,
      },
    };
  });
}
