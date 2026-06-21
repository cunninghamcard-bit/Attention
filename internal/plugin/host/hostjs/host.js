import readline from "node:readline";
import { pathToFileURL } from "node:url";

const instances = new Map();
let uiCorrSeq = 0;

function writeFrame(frame) {
  const encoded = JSON.stringify(frame);
  if (encoded === undefined) {
    throw new Error("frame did not encode to JSON");
  }
  process.stdout.write(`${encoded}\n`);
}

function errorText(error) {
  if (error instanceof Error) {
    return error.stack || error.message;
  }
  return String(error);
}

function fatal(error) {
  const frame = { t: "fatal", error: errorText(error) };
  try {
    writeFrame(frame);
  } finally {
    process.exitCode = 1;
    process.stdout.write("", () => process.exit(1));
  }
}

function hasOwn(object, property) {
  return Object.prototype.hasOwnProperty.call(object, property);
}

function requireObject(value, label) {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`);
  }
  return value;
}

function requireString(value, label) {
  if (typeof value !== "string" || value.length === 0) {
    throw new Error(`${label} must be a non-empty string`);
  }
  return value;
}

function requireIndex(value, label) {
  if (!Number.isInteger(value) || value < 0) {
    throw new Error(`${label} must be a non-negative integer`);
  }
  return value;
}

function optionalString(value, label) {
  if (value === undefined || value === null) {
    return undefined;
  }
  if (typeof value !== "string") {
    throw new Error(`${label} must be a string`);
  }
  return value;
}

function optionalTimeoutMs(value, label) {
  if (value === undefined || value === null) {
    return undefined;
  }
  if (!Number.isInteger(value) || value <= 0) {
    throw new Error(`${label} must be a positive integer`);
  }
  return value;
}

function instanceKey(frame) {
  return [
    requireString(frame.pluginId, "pluginId"),
    requireString(frame.owner, "owner"),
    frame.sessionId || "",
    frame.envId || "",
  ].join("\u0000");
}

function baseFrame(frame) {
  return {
    corrId: frame.corrId,
    pluginId: frame.pluginId,
    owner: frame.owner,
    sessionId: frame.sessionId,
    envId: frame.envId,
  };
}

function moduleSpecifier(modulePath) {
  requireString(modulePath, "modulePath");
  if (modulePath.startsWith("file:") || modulePath.startsWith("data:")) {
    return modulePath;
  }
  return pathToFileURL(modulePath).href;
}

function removeDeclaration(list, declaration) {
  const index = list.indexOf(declaration);
  if (index >= 0) {
    list.splice(index, 1);
  }
}

function removeRegisteredHandler(instance, kind, name, handler) {
  const handlers = instance.handlers[kind];
  if (handlers.get(name) === handler) {
    handlers.delete(name);
  }
}

function hookHandlers(instance, point) {
  let handlers = instance.handlers.hooks.get(point);
  if (!handlers) {
    handlers = [];
    instance.handlers.hooks.set(point, handlers);
  }
  return handlers;
}

function removeRegisteredHook(instance, point, index, handler) {
  const handlers = instance.handlers.hooks.get(point);
  if (handlers?.[index] === handler) {
    handlers[index] = undefined;
  }
}

function normalizeTool(input) {
  const tool = requireObject(input, "tool registration");
  const declaration = { name: requireString(tool.name, "tool.name") };
  if (hasOwn(tool, "description")) {
    declaration.description = requireString(tool.description, "tool.description");
  }
  if (hasOwn(tool, "schema")) {
    declaration.schema = tool.schema;
  } else if (hasOwn(tool, "inputSchema")) {
    declaration.schema = tool.inputSchema;
  }
  return declaration;
}

function normalizeHook(input) {
  if (typeof input === "string") {
    return { point: requireString(input, "hook point") };
  }
  const hook = requireObject(input, "hook registration");
  return { point: requireString(hook.point, "hook.point") };
}

function normalizeCommand(input) {
  if (typeof input === "string") {
    return { name: requireString(input, "command name") };
  }
  const command = requireObject(input, "command registration");
  return { name: requireString(command.name, "command.name") };
}

function nextUICorrID() {
  uiCorrSeq += 1;
  return `ui_${uiCorrSeq}`;
}

function requestOptions(input) {
  if (input === undefined || input === null) {
    return {};
  }
  return requireObject(input, "ui request options");
}

function normalizeUIOptions(options) {
  const out = {};
  if (hasOwn(options, "timeoutMs")) {
    out.timeoutMs = optionalTimeoutMs(options.timeoutMs, "ui options.timeoutMs");
  }
  if (hasOwn(options, "default")) {
    out.default = options.default;
  }
  return out;
}

function sendUIRequest(instance, kind, payload) {
  if (!instance.sessionId) {
    return Promise.reject(new Error("ui requests require a session-scoped plugin instance"));
  }

  const corrId = nextUICorrID();
  const request = {
    kind,
    ...payload,
  };
  const hasDefault = hasOwn(request, "default");

  return new Promise((resolve, reject) => {
    instance.pendingUIByCorr.set(corrId, {
      corrId,
      requestId: "",
      kind,
      hasDefault,
      resolve,
      reject,
    });
    try {
      writeFrame({
        t: "ui.request",
        corrId,
        pluginId: instance.pluginId,
        owner: instance.owner,
        sessionId: instance.sessionId,
        envId: instance.envId,
        name: kind,
        payload: request,
      });
    } catch (error) {
      instance.pendingUIByCorr.delete(corrId);
      reject(error);
    }
  });
}

function buildUI(instance) {
  return {
    confirm(title, body, options) {
      const payload = {
        title: requireString(title, "ui.confirm title"),
        ...normalizeUIOptions(requestOptions(options)),
      };
      const bodyText = optionalString(body, "ui.confirm body");
      if (bodyText !== undefined) {
        payload.body = bodyText;
      }
      return sendUIRequest(instance, "confirm", payload);
    },
    select(title, options, request) {
      if (!Array.isArray(options) || options.some((option) => typeof option !== "string")) {
        throw new Error("ui.select options must be an array of strings");
      }
      return sendUIRequest(instance, "select", {
        title: requireString(title, "ui.select title"),
        options: [...options],
        ...normalizeUIOptions(requestOptions(request)),
      });
    },
    input(title, placeholder, options) {
      const payload = {
        title: requireString(title, "ui.input title"),
        ...normalizeUIOptions(requestOptions(options)),
      };
      const placeholderText = optionalString(placeholder, "ui.input placeholder");
      if (placeholderText !== undefined) {
        payload.body = placeholderText;
      }
      return sendUIRequest(instance, "input", payload);
    },
    editor(title, prefill, options) {
      const payload = {
        title: requireString(title, "ui.editor title"),
        ...normalizeUIOptions(requestOptions(options)),
      };
      if (prefill !== undefined) {
        payload.default = prefill;
      }
      return sendUIRequest(instance, "editor", payload);
    },
    notify(message, type) {
      if (!instance.sessionId) {
        throw new Error("ui.notify requires a session-scoped plugin instance");
      }
      const payload = {
        kind: "notify",
        title: requireString(message, "ui.notify message"),
      };
      const typeText = optionalString(type, "ui.notify type");
      if (typeText !== undefined) {
        payload.body = typeText;
      }
      writeFrame({
        t: "ui.request",
        pluginId: instance.pluginId,
        owner: instance.owner,
        sessionId: instance.sessionId,
        envId: instance.envId,
        name: "notify",
        payload,
      });
    },
  };
}

function buildAlong(instance) {
  return {
    events: {
      emit(name, payload) {
        writeFrame({
          t: "events.emit",
          pluginId: instance.pluginId,
          owner: instance.owner,
          sessionId: instance.sessionId,
          envId: instance.envId,
          name: requireString(name, "event name"),
          payload: payload === undefined ? null : payload,
        });
      },
    },
    ui: buildUI(instance),
    log(level, msg) {
      writeFrame({
        t: "log",
        pluginId: instance.pluginId,
        owner: instance.owner,
        sessionId: instance.sessionId,
        envId: instance.envId,
        level: requireString(level, "log level"),
        msg: String(msg),
      });
    },
    tools: {
      register(input, handler) {
        if (!instance.collecting) {
          throw new Error("tools.register called after activate returned");
        }
        if (handler !== undefined && typeof handler !== "function") {
          throw new Error("tool handler must be a function");
        }
        const declaration = normalizeTool(input);
        instance.declarations.tools.push(declaration);
        if (handler !== undefined) {
          instance.handlers.tools.set(declaration.name, handler);
        }
        return () => {
          removeDeclaration(instance.declarations.tools, declaration);
          removeRegisteredHandler(instance, "tools", declaration.name, handler);
        };
      },
    },
    hooks: {
      register(input, handler) {
        if (!instance.collecting) {
          throw new Error("hooks.register called after activate returned");
        }
        if (typeof handler !== "function") {
          throw new Error("hook handler must be a function");
        }
        const declaration = normalizeHook(input);
        const handlers = hookHandlers(instance, declaration.point);
        declaration.index = handlers.length;
        handlers.push(handler);
        instance.declarations.hooks.push(declaration);
        return () => {
          removeDeclaration(instance.declarations.hooks, declaration);
          removeRegisteredHook(instance, declaration.point, declaration.index, handler);
        };
      },
    },
    commands: {
      on(input, handler) {
        if (!instance.collecting) {
          throw new Error("commands.on called after activate returned");
        }
        if (handler !== undefined && typeof handler !== "function") {
          throw new Error("command handler must be a function");
        }
        const declaration = normalizeCommand(input);
        instance.declarations.commands.push(declaration);
        if (handler !== undefined) {
          instance.handlers.commands.set(declaration.name, handler);
        }
        return () => {
          removeDeclaration(instance.declarations.commands, declaration);
          removeRegisteredHandler(instance, "commands", declaration.name, handler);
        };
      },
    },
  };
}

function rejectPendingUI(instance, error) {
  for (const pending of instance.pendingUIByCorr.values()) {
    pending.reject(error);
  }
  for (const pending of instance.pendingUIByRequest.values()) {
    pending.reject(error);
  }
  instance.pendingUIByCorr.clear();
  instance.pendingUIByRequest.clear();
}

function routeDescription(frame) {
  const parts = [
    frame.pluginId || "<missing-plugin>",
    frame.owner || "<missing-owner>",
  ];
  if (frame.sessionId) {
    parts.push(`session:${frame.sessionId}`);
  }
  if (frame.envId) {
    parts.push(`env:${frame.envId}`);
  }
  return parts.join("/");
}

function buildContext(frame, instance) {
  return {
    pluginId: frame.pluginId,
    owner: frame.owner,
    sessionId: frame.sessionId,
    envId: frame.envId,
    seed: hasOwn(frame, "ctxSeed") ? frame.ctxSeed : null,
    register(disposeFn) {
      if (typeof disposeFn !== "function") {
        throw new Error("ctx.register expects a function");
      }
      instance.disposers.push(disposeFn);
      return disposeFn;
    },
  };
}

async function handleActivate(frame) {
  const key = instanceKey(frame);
  if (instances.has(key)) {
    writeFrame({
      t: "registered",
      ...baseFrame(frame),
      isError: true,
      error: `instance already active: ${frame.pluginId}/${frame.owner}`,
    });
    return;
  }

  const instance = {
    key,
    pluginId: frame.pluginId,
    owner: frame.owner,
    sessionId: frame.sessionId,
    envId: frame.envId,
    declarations: { tools: [], hooks: [], commands: [] },
    handlers: { tools: new Map(), hooks: new Map(), commands: new Map() },
    disposers: [],
    pendingUIByCorr: new Map(),
    pendingUIByRequest: new Map(),
    collecting: true,
  };

  try {
    const module = await import(moduleSpecifier(frame.modulePath));
    if (typeof module.default !== "function") {
      throw new Error("plugin module default export must be activate(along, ctx)");
    }
    const result = await module.default(buildAlong(instance), buildContext(frame, instance));
    if (typeof result === "function") {
      instance.disposers.push(result);
    } else if (result !== undefined && result !== null) {
      throw new Error("activate must return a dispose function or nothing");
    }
    instance.collecting = false;
    instances.set(key, instance);
    writeFrame({
      t: "registered",
      ...baseFrame(frame),
      tools: instance.declarations.tools,
      hooks: instance.declarations.hooks,
      commands: instance.declarations.commands,
    });
  } catch (error) {
    instance.collecting = false;
    writeFrame({
      t: "registered",
      ...baseFrame(frame),
      isError: true,
      error: errorText(error),
    });
  }
}

async function handleDispose(frame) {
  const key = instanceKey(frame);
  const instance = instances.get(key);
  if (!instance) {
    writeFrame({
      t: "disposed",
      ...baseFrame(frame),
      isError: true,
      error: `instance not active: ${frame.pluginId}/${frame.owner}`,
    });
    return;
  }

  instances.delete(key);
  rejectPendingUI(instance, new Error("plugin instance disposed"));
  for (const disposeFn of [...instance.disposers].reverse()) {
    try {
      await disposeFn();
    } catch (error) {
      writeFrame({
        t: "log",
        pluginId: instance.pluginId,
        owner: instance.owner,
        sessionId: instance.sessionId,
        envId: instance.envId,
        level: "error",
        msg: `dispose failed: ${errorText(error)}`,
      });
    }
  }
  writeFrame({ t: "disposed", ...baseFrame(frame) });
}

function handleUIRequestAck(frame) {
  const key = instanceKey(frame);
  const instance = instances.get(key);
  if (!instance) {
    return;
  }
  const corrId = requireString(frame.corrId, "ui.request ack corrId");
  const payload = requireObject(hasOwn(frame, "payload") ? frame.payload : null, "ui.request ack payload");
  const requestId = requireString(payload.requestId, "ui.request ack requestId");
  const pending = instance.pendingUIByCorr.get(corrId);
  if (!pending) {
    return;
  }
  instance.pendingUIByCorr.delete(corrId);
  pending.requestId = requestId;
  instance.pendingUIByRequest.set(requestId, pending);
}

function pendingUIForFrame(instance, frame, payload) {
  if (payload && typeof payload.requestId === "string" && payload.requestId.length > 0) {
    const pending = instance.pendingUIByRequest.get(payload.requestId);
    if (pending) {
      instance.pendingUIByRequest.delete(payload.requestId);
      return pending;
    }
  }
  if (frame.corrId) {
    const pending = instance.pendingUIByCorr.get(frame.corrId);
    if (pending) {
      instance.pendingUIByCorr.delete(frame.corrId);
      return pending;
    }
  }
  return null;
}

function handleUIResolved(frame) {
  const key = instanceKey(frame);
  const instance = instances.get(key);
  if (!instance) {
    return;
  }

  const payload = frame.isError
    ? null
    : requireObject(hasOwn(frame, "payload") ? frame.payload : null, "ui.resolved payload");
  const pending = pendingUIForFrame(instance, frame, payload);
  if (!pending) {
    return;
  }

  if (frame.isError) {
    pending.reject(new Error(frame.error || "ui request failed"));
    return;
  }

  const resolvedBy = requireString(payload.resolvedBy, "ui.resolved resolvedBy");
  if (resolvedBy === "cancelled") {
    pending.reject(new Error("ui request cancelled"));
    return;
  }
  if (resolvedBy === "timeout" && !pending.hasDefault) {
    pending.reject(new Error("ui request timed out"));
    return;
  }
  pending.resolve(hasOwn(payload, "value") ? payload.value : null);
}

async function handleCommandDispatch(frame) {
  try {
    const key = instanceKey(frame);
    const instance = instances.get(key);
    if (!instance) {
      writeFrame({
        t: "command.done",
        ...baseFrame(frame),
        name: frame.name,
        isError: true,
        error: `plugin instance not active: ${routeDescription(frame)}`,
      });
      return;
    }

    const name = requireString(frame.name, "command name");
    const handler = instance.handlers.commands.get(name);
    if (!handler) {
      writeFrame({
        t: "command.done",
        ...baseFrame(frame),
        name,
        isError: true,
        error: `unknown command ${JSON.stringify(name)} for instance ${routeDescription(frame)}`,
      });
      return;
    }

    const result = await handler(hasOwn(frame, "payload") ? frame.payload : null);
    writeFrame({
      t: "command.done",
      ...baseFrame(frame),
      name,
      payload: result === undefined ? null : result,
    });
  } catch (error) {
    writeFrame({
      t: "command.done",
      ...baseFrame(frame),
      name: frame.name,
      isError: true,
      error: errorText(error),
    });
  }
}

async function handleToolExecute(frame) {
  try {
    const key = instanceKey(frame);
    const instance = instances.get(key);
    if (!instance) {
      writeFrame({
        t: "tool.result",
        ...baseFrame(frame),
        name: frame.name,
        isError: true,
        error: `plugin instance not active: ${routeDescription(frame)}`,
      });
      return;
    }

    const name = requireString(frame.name, "tool name");
    const handler = instance.handlers.tools.get(name);
    if (!handler) {
      writeFrame({
        t: "tool.result",
        ...baseFrame(frame),
        name,
        isError: true,
        error: `unknown tool ${JSON.stringify(name)} for instance ${routeDescription(frame)}`,
      });
      return;
    }

    const result = await handler(hasOwn(frame, "payload") ? frame.payload : {}, {
      toolCallID: frame.corrId || "",
      update() {},
    });
    writeFrame({
      t: "tool.result",
      ...baseFrame(frame),
      name,
      payload: result === undefined ? null : result,
    });
  } catch (error) {
    writeFrame({
      t: "tool.result",
      ...baseFrame(frame),
      name: frame.name,
      isError: true,
      error: errorText(error),
    });
  }
}

async function handleHookDispatch(frame) {
  try {
    const key = instanceKey(frame);
    const instance = instances.get(key);
    if (!instance) {
      writeFrame({
        t: "hook.result",
        ...baseFrame(frame),
        name: frame.name,
        isError: true,
        error: `plugin instance not active: ${routeDescription(frame)}`,
      });
      return;
    }

    const point = requireString(frame.name, "hook point");
    const payload = requireObject(hasOwn(frame, "payload") ? frame.payload : null, "hook payload");
    const index = requireIndex(payload.index, "hook payload.index");
    const handlers = instance.handlers.hooks.get(point) ?? [];
    const handler = handlers[index];
    if (typeof handler !== "function") {
      writeFrame({
        t: "hook.result",
        ...baseFrame(frame),
        name: point,
        isError: true,
        error: `unknown hook ${JSON.stringify(point)}#${index} for instance ${routeDescription(frame)}`,
      });
      return;
    }

    const result = await handler(hasOwn(payload, "event") ? payload.event : null);
    writeFrame({
      t: "hook.result",
      ...baseFrame(frame),
      name: point,
      payload: result === undefined ? null : result,
    });
  } catch (error) {
    writeFrame({
      t: "hook.result",
      ...baseFrame(frame),
      name: frame.name,
      isError: true,
      error: errorText(error),
    });
  }
}

async function handleFrame(frame) {
  switch (frame.t) {
    case "activate":
      await handleActivate(frame);
      return;
    case "dispose":
      await handleDispose(frame);
      return;
    case "command.dispatch":
      await handleCommandDispatch(frame);
      return;
    case "tool.execute":
      await handleToolExecute(frame);
      return;
    case "hook.dispatch":
      await handleHookDispatch(frame);
      return;
    case "ui.request":
      handleUIRequestAck(frame);
      return;
    case "ui.resolved":
      handleUIResolved(frame);
      return;
    case "ping":
      writeFrame({ t: "pong", corrId: frame.corrId });
      return;
    default:
      writeFrame({
        t: "log",
        level: "warn",
        msg: `unknown frame type: ${String(frame.t)}`,
      });
  }
}

const rl = readline.createInterface({
  input: process.stdin,
  crlfDelay: Infinity,
});

rl.on("line", (line) => {
  let frame;
  try {
    frame = JSON.parse(line);
  } catch (error) {
    fatal(error);
    return;
  }
  handleFrame(frame).catch(fatal);
});

rl.on("close", () => {
  process.exit(0);
});

process.on("uncaughtException", fatal);
process.on("unhandledRejection", fatal);
