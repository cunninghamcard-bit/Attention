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

// src/plugins/hello-world/main.ts
var HelloView = class extends View {
  getViewType() {
    return "hello-world-view";
  }
  onOpen(el) {
    el.textContent = "hello";
  }
};
var HelloWorld = class extends Plugin {
  onload() {
    this.addCommand({
      id: "greet",
      name: "Hello: greet",
      callback: () => {
        this.statusEl.textContent = `hello @ ${this.along.sessions.index().sessions.length} sessions`;
      }
    });
    this.registerView("hello-world-view", () => new HelloView());
    this.statusEl = this.addStatusBarItem();
    this.statusEl.textContent = "hello-world ready";
  }
  statusEl;
};
export {
  HelloWorld as default
};
