import { Plugin, View } from '@along/sdk';

class HelloView extends View {
  getViewType() {
    return 'hello-world-view';
  }

  onOpen(el: HTMLElement) {
    el.textContent = 'hello';
  }
}

export default class HelloWorld extends Plugin {
  onload() {
    this.addCommand({
      id: 'greet',
      name: 'Hello: greet',
      callback: () => {
        this.statusEl.textContent = `hello @ ${this.along.sessions.index().sessions.length} sessions`;
      },
    });
    this.registerView('hello-world-view', () => new HelloView());
    this.statusEl = this.addStatusBarItem();
    this.statusEl.textContent = 'hello-world ready';
  }

  private statusEl!: HTMLElement;
}
