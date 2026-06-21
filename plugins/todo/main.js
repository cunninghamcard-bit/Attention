import { Plugin, View } from '@along/sdk';

class TodoView extends View {
  getViewType() {
    return 'todo';
  }

  onOpen(el) {
    const root = document.createElement('div');
    root.dataset.alongTodoView = 'placeholder';
    el.replaceChildren(root);
  }
}

export default class TodoPlugin extends Plugin {
  onload() {
    this.registerView('todo', () => new TodoView());
  }
}
