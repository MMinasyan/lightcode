// @vitest-environment happy-dom
import { beforeEach, afterEach, describe, expect, it } from 'vitest';
import { mount, tick, unmount } from 'svelte';
import { get } from 'svelte/store';
import { closeViewer, viewer } from '../lib/viewer.js';

const backend = {
  ReadFileContent: async () => ({ content: '', superseded: false }),
};

beforeEach(() => {
  closeViewer();
  window.go = { main: { App: backend } };
});

afterEach(() => {
  document.body.innerHTML = '';
});

describe('MessageList viewer ownership forwarding', () => {
  it('delivers the App-owned callback to a root ToolCall row', async () => {
    const { default: MessageList } = await import('./MessageList.svelte');
    const target = document.createElement('div');
    document.body.appendChild(target);
    const owner = () => ({ sessionId: 'root', presentationGeneration: 4 });
    backend.ReadFileContent = async (sessionID, path) => {
      expect(sessionID).toBe('root');
      expect(path).toBe('src/root.go');
      return { content: 'root content', superseded: false };
    };
    const component = mount(MessageList, {
      target,
      props: {
        messages: [{ _id: 1, type: 'tool', name: 'read_file', args: JSON.stringify({ path: 'src/root.go' }) }],
        viewerOwner: owner,
      },
    });
    await tick();
    target.querySelector('.arg.path').click();
    await new Promise(resolve => setTimeout(resolve, 0));
    await tick();
    expect(get(viewer)).toMatchObject({ title: 'src/root.go', content: 'root content' });
    unmount(component);
  });
});
