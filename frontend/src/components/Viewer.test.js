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

function liveViewer(path) {
  viewer.set({
    title: 'Child',
    live: true,
    messages: [{ type: 'tool', name: 'read_file', args: JSON.stringify({ path }) }],
  });
}

async function settle() {
  await new Promise(resolve => setTimeout(resolve, 0));
  await tick();
}

describe('Viewer viewer ownership forwarding', () => {
  it('delivers the App-owned callback to a child ToolCall row', async () => {
    const { default: Viewer } = await import('./Viewer.svelte');
    const target = document.createElement('div');
    document.body.appendChild(target);
    const owner = () => ({ sessionId: 'root', presentationGeneration: 4 });
    backend.ReadFileContent = async (sessionID, path) => {
      expect(sessionID).toBe('root');
      expect(path).toBe('src/child.go');
      return { content: 'child content', superseded: false };
    };
    liveViewer('src/child.go');
    const component = mount(Viewer, { target, props: { viewerOwner: owner } });
    await tick();
    target.querySelector('.arg.path').click();
    await settle();
    expect(get(viewer)).toMatchObject({ title: 'src/child.go', content: 'child content' });
    unmount(component);
  });

  it('drops a read settling after root navigation destroys the child viewer', async () => {
    const { default: Viewer } = await import('./Viewer.svelte');
    const target = document.createElement('div');
    document.body.appendChild(target);
    const ownerState = { sessionId: 'A', presentationGeneration: 1 };
    let resolveRead;
    backend.ReadFileContent = () => new Promise(resolve => { resolveRead = resolve; });
    liveViewer('src/child.go');
    const component = mount(Viewer, { target, props: { viewerOwner: () => ({ ...ownerState }) } });
    await tick();
    target.querySelector('.arg.path').click();
    ownerState.sessionId = 'B';
    ownerState.presentationGeneration = 2;
    closeViewer();
    unmount(component);
    resolveRead({ content: 'old child', superseded: false });
    await settle();
    expect(get(viewer)).toBeNull();
  });
});
