import { View, type Along, type SessionMeta } from '@along/sdk';

export class SessionListView extends View {
  constructor(private readonly along: Along) {
    super();
  }

  getViewType() {
    return 'session-list';
  }

  onOpen(el: HTMLElement) {
    const render = () => {
      el.replaceChildren();

      const root = document.createElement('div');
      root.style.display = 'flex';
      root.style.flexDirection = 'column';
      root.style.gap = '8px';
      root.style.padding = '12px';

      const createButton = document.createElement('button');
      createButton.type = 'button';
      createButton.textContent = '新建会话';
      createButton.onclick = () => {
        void this.along.sessions.create();
      };
      root.appendChild(createButton);

      const list = document.createElement('div');
      list.style.display = 'flex';
      list.style.flexDirection = 'column';
      list.style.gap = '4px';

      for (const session of this.along.sessions.index().sessions) {
        list.appendChild(this.sessionRow(session));
      }

      root.appendChild(list);
      el.appendChild(root);
    };

    render();
    this.register(this.along.sessions.onIndexChanged(render));
  }

  private sessionRow(session: SessionMeta): HTMLElement {
    const row = document.createElement('button');
    row.type = 'button';
    row.textContent = `${session.name || session.id} / ${session.id}`;
    row.style.textAlign = 'left';
    row.onclick = () => {
      this.along.workspace.openView('chat', { sessionId: session.id }, 'tab');
    };
    return row;
  }
}
