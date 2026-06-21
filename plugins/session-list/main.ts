import { Plugin } from '@along/sdk';
import { SessionListView } from './src/view';

export default class SessionListPlugin extends Plugin {
  onload() {
    this.registerView('session-list', () => new SessionListView(this.along));
    this.along.workspace.setDefaultView('session-list');
    this.addCommand({
      id: 'open',
      name: '打开会话列表',
      callback: () => {
        this.along.workspace.openView('session-list', null, 'left');
      },
    });
  }
}
