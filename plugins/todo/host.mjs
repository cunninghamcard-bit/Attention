export default function activate(along, ctx) {
  let todos = [];
  let nextId = 1;

  const cloneTodos = () => todos.map((todo) => ({ ...todo }));

  const emitUpdated = () => {
    along.events.emit("todo.updated", { todos: cloneTodos() });
  };

  along.tools.register(
    {
      name: "addTodo",
      description: "Append a todo",
      inputSchema: {
        type: "object",
        properties: {
          text: { type: "string" },
        },
        required: ["text"],
      },
    },
    (args) => {
      const input = requireObject(args, "addTodo args");
      const text = requireString(input.text, "addTodo args.text");
      const todo = { id: nextId, text, done: false };
      nextId += 1;
      todos = [...todos, todo];
      emitUpdated();
      return { content: "added", details: { count: todos.length } };
    },
  );

  along.tools.register(
    {
      name: "toggleTodo",
      description: "Toggle a todo by id",
      inputSchema: {
        type: "object",
        properties: {
          id: { type: "integer" },
        },
        required: ["id"],
      },
    },
    (args) => {
      const input = requireObject(args, "toggleTodo args");
      const id = requirePositiveInteger(input.id, "toggleTodo args.id");
      let found = false;
      todos = todos.map((todo) => {
        if (todo.id !== id) {
          return todo;
        }
        found = true;
        return { ...todo, done: !todo.done };
      });
      if (!found) {
        throw new Error(`todo ${id} not found`);
      }
      emitUpdated();
      return { content: "toggled" };
    },
  );

  along.commands.on("setTodos", (payload) => {
    const input = requireObject(payload, "setTodos payload");
    if (!Array.isArray(input.todos)) {
      throw new Error("setTodos payload.todos must be an array");
    }
    todos = input.todos.map((todo, index) => normalizeTodo(todo, index));
    nextId = todos.reduce((max, todo) => Math.max(max, todo.id), 0) + 1;
    emitUpdated();
    return { count: todos.length };
  });
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

function requireBoolean(value, label) {
  if (typeof value !== "boolean") {
    throw new Error(`${label} must be a boolean`);
  }
  return value;
}

function requirePositiveInteger(value, label) {
  if (!Number.isInteger(value) || value <= 0) {
    throw new Error(`${label} must be a positive integer`);
  }
  return value;
}

function normalizeTodo(value, index) {
  const todo = requireObject(value, `setTodos payload.todos[${index}]`);
  return {
    id: requirePositiveInteger(todo.id, `setTodos payload.todos[${index}].id`),
    text: requireString(todo.text, `setTodos payload.todos[${index}].text`),
    done: requireBoolean(todo.done, `setTodos payload.todos[${index}].done`),
  };
}
