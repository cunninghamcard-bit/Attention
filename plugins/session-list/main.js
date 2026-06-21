// src/sdk/sdk/src/component.ts
var Component = class {
  disposers = [];
  children = [];
  loaded = false;
  load() {
    if (this.loaded) return;
    this.loaded = true;
    this.onload();
    this.children.forEach((c) => c.load());
  }
  unload() {
    if (!this.loaded) return;
    this.loaded = false;
    [...this.children].reverse().forEach((c) => c.unload());
    this.children = [];
    [...this.disposers].reverse().forEach((d) => d());
    this.disposers = [];
    this.onunload();
  }
  onload() {
  }
  onunload() {
  }
  register(cb) {
    this.disposers.push(cb);
  }
  registerEvent(unsub) {
    this.register(unsub);
  }
  registerInterval(id) {
    this.register(() => clearInterval(id));
    return id;
  }
  registerDomEvent(el, type, fn) {
    el.addEventListener(type, fn);
    this.register(() => el.removeEventListener(type, fn));
  }
  addChild(c) {
    this.children.push(c);
    if (this.loaded) c.load();
    return c;
  }
};

// src/sdk/sdk/src/view.ts
var View = class extends Component {
  onClose() {
  }
  getState() {
    return null;
  }
  setState(_state) {
  }
};

// src/sdk/sdk/src/plugin.ts
var Plugin = class extends Component {
  constructor(along, manifest, host) {
    super();
    this.along = along;
    this.manifest = manifest;
    this.host = host;
  }
  along;
  manifest;
  host;
  async loadData() {
    return this.along.protocol.request("GET", `/v1/plugins/${this.manifest.id}/data`);
  }
  async saveData(data) {
    await this.along.protocol.request("PUT", `/v1/plugins/${this.manifest.id}/data`, data);
  }
  addCommand(cmd) {
    const fullId = `${this.manifest.id}:${cmd.id}`;
    this.host.addCommand(fullId, {
      name: cmd.name,
      callback: cmd.callback,
      checkCallback: cmd.checkCallback
    });
    this.register(() => this.host.removeCommand(fullId));
  }
  registerView(type, factory) {
    this.host.registerViewType(type, factory);
    this.register(() => this.host.unregisterViewType(type));
  }
  addStatusBarItem() {
    const el = document.createElement("span");
    this.host.statusBarEl().appendChild(el);
    this.register(() => el.remove());
    return el;
  }
  addSettingTab(tab) {
    this.host.addSettingTab(tab);
    this.register(() => this.host.removeSettingTab(tab.id));
  }
};

// src/plugins/session-list/src/view.ts
var SessionListView = class extends View {
  constructor(along) {
    super();
    this.along = along;
  }
  along;
  getViewType() {
    return "session-list";
  }
  onOpen(el) {
    const render = () => {
      el.replaceChildren();
      const root = document.createElement("div");
      root.style.display = "flex";
      root.style.flexDirection = "column";
      root.style.gap = "8px";
      root.style.padding = "12px";
      const createButton = document.createElement("button");
      createButton.type = "button";
      createButton.textContent = "\u65B0\u5EFA\u4F1A\u8BDD";
      createButton.onclick = () => {
        void this.along.sessions.create();
      };
      root.appendChild(createButton);
      const list = document.createElement("div");
      list.style.display = "flex";
      list.style.flexDirection = "column";
      list.style.gap = "4px";
      for (const session of this.along.sessions.index().sessions) {
        list.appendChild(this.sessionRow(session));
      }
      root.appendChild(list);
      el.appendChild(root);
    };
    render();
    this.register(this.along.sessions.onIndexChanged(render));
  }
  sessionRow(session) {
    const row = document.createElement("button");
    row.type = "button";
    row.textContent = `${session.name || session.id} / ${session.id}`;
    row.style.textAlign = "left";
    row.onclick = () => {
      this.along.workspace.openView("chat", { sessionId: session.id }, "tab");
    };
    return row;
  }
};

// src/plugins/session-list/main.ts
var SessionListPlugin = class extends Plugin {
  onload() {
    this.registerView("session-list", () => new SessionListView(this.along));
    this.along.workspace.setDefaultView("session-list");
    this.addCommand({
      id: "open",
      name: "\u6253\u5F00\u4F1A\u8BDD\u5217\u8868",
      callback: () => {
        this.along.workspace.openView("session-list", null, "left");
      }
    });
  }
};
export {
  SessionListPlugin as default
};
