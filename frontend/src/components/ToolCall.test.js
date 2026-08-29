// @vitest-environment happy-dom
import { describe, expect, it, beforeEach, afterEach } from 'vitest';
import { flushSync, mount, tick, unmount } from 'svelte';
import { get } from 'svelte/store';
import { closeViewer, viewer } from '../lib/viewer.js';

// The task row's subtask affordance must drive the child-viewer hydration
// through its production caller (openSubagentTranscript), not through the
// helper directly: the only thing connecting an open to the hydration result
// is the generation value openSubagentViewer returns, and dropping it makes
// the guard reject every result silently. viewer.test.js calls
// hydrateSubagentViewer directly and stays green under that mutation, and the
// Go source-text contract check cannot see a threading break either. This
// mounts the real component and clicks the real row.

const backend = {
  HydrateSession: async () => ({}),
  ReadFileContent: async () => ({ content: '', superseded: false }),
};

const taskRow = {
  name: 'task',
  args: JSON.stringify({ tasks: [{ subagent_type: 'explore', prompt: 'find the bug' }] }),
  subagentSessionIds: [{ index: 0, sessionId: 'child-1' }],
};

beforeEach(() => {
  closeViewer();
  window.go = { main: { App: backend } };
  backend.HydrateSession = async () => ({
    messages: [],
    tail: [],
    errors: [],
    cursor: { committedTurn: 1, committedSeq: 0, rewriteEpoch: 0 },
  });
  backend.ReadFileContent = async () => ({ content: '', superseded: false });
});

afterEach(() => {
  document.body.innerHTML = '';
});

async function settle() {
  flushSync();
  await new Promise((r) => setTimeout(r, 0));
  await tick();
}

async function mountTaskRow() {
  const { default: ToolCall } = await import('./ToolCall.svelte');
  const target = document.createElement('div');
  document.body.appendChild(target);
  const toolCall = mount(ToolCall, { target, props: taskRow });
  await tick();
  return { toolCall, target };
}

async function mountReadRow(viewerOwner) {
  const { default: ToolCall } = await import('./ToolCall.svelte');
  const target = document.createElement('div');
  document.body.appendChild(target);
  const toolCall = mount(ToolCall, {
    target,
    props: {
      name: 'read_file',
      args: JSON.stringify({ path: 'src/main.go' }),
      viewerOwner,
    },
  });
  await tick();
  return { toolCall, target };
}

describe('ToolCall subtask viewer threading', () => {
  it('hydrates the child viewer through the production caller with the stubbed messages', async () => {
    backend.HydrateSession = async () => ({
      messages: [{ type: 'assistant', content: 'child answer' }],
      tail: [],
      errors: [],
      cursor: { committedTurn: 1, committedSeq: 1, rewriteEpoch: 0 },
    });
    const { toolCall, target } = await mountTaskRow();

    const row = target.querySelector('.subtask-row');
    expect(row).toBeTruthy();
    row.click();
    await settle();

    const v = get(viewer);
    expect(v.title).toBe('explore: find the bug');
    expect(v.sessionId).toBe('child-1');
    expect(v.live).toBe(true);
    expect(v.generation).toBeGreaterThan(0);
    expect(v.messages).toEqual([{ type: 'assistant', content: 'child answer' }]);

    unmount(toolCall);
  });

  it('shows the hydration failure in the child viewer instead of an empty open', async () => {
    backend.HydrateSession = async () => { throw new Error('boom'); };
    const { toolCall, target } = await mountTaskRow();

    target.querySelector('.subtask-row').click();
    await settle();

    const v = get(viewer);
    expect(v.sessionId).toBe('child-1');
    expect(v.live).toBe(true);
    expect(v.messages).toEqual([{ type: 'error', content: 'Error: boom' }]);

    unmount(toolCall);
  });
});

describe('ToolCall file viewer ownership', () => {
  it('opens same-presentation success and error results', async () => {
    const ownerState = { sessionId: 'A', presentationGeneration: 1 };
    const owner = () => ({ ...ownerState });
    backend.ReadFileContent = async (sessionID, path) => {
      expect(sessionID).toBe('A');
      expect(path).toBe('src/main.go');
      return { content: 'package main', superseded: false };
    };
    let mounted = await mountReadRow(owner);
    mounted.target.querySelector('.arg.path').click();
    await settle();
    expect(get(viewer)).toMatchObject({ title: 'src/main.go', content: 'package main' });
    unmount(mounted.toolCall);

    closeViewer();
    backend.ReadFileContent = async () => { throw new Error('read failed'); };
    mounted = await mountReadRow(owner);
    mounted.target.querySelector('.arg.path').click();
    await settle();
    expect(get(viewer)).toMatchObject({ title: 'src/main.go', content: 'Error: read failed' });
    unmount(mounted.toolCall);
  });

  it.each([
    ['stale success', (resolve) => resolve({ content: 'old', superseded: false })],
    ['stale error', (_resolve, reject) => reject(new Error('old failure'))],
  ])('%s does not reopen the viewer after A-to-B navigation', async (_name, settleRead) => {
    const ownerState = { sessionId: 'A', presentationGeneration: 1 };
    const owner = () => ({ ...ownerState });
    let resolveRead;
    let rejectRead;
    backend.ReadFileContent = () => new Promise((resolve, reject) => {
      resolveRead = resolve;
      rejectRead = reject;
    });
    const mounted = await mountReadRow(owner);
    mounted.target.querySelector('.arg.path').click();
    ownerState.sessionId = 'B';
    ownerState.presentationGeneration = 2;
    closeViewer();
    settleRead(resolveRead, rejectRead);
    await settle();
    expect(get(viewer)).toBeNull();
    unmount(mounted.toolCall);
  });

  it('drops an A-to-B-to-A result using the presentation generation', async () => {
    const ownerState = { sessionId: 'A', presentationGeneration: 1 };
    const owner = () => ({ ...ownerState });
    let resolveRead;
    backend.ReadFileContent = () => new Promise(resolve => { resolveRead = resolve; });
    const mounted = await mountReadRow(owner);
    mounted.target.querySelector('.arg.path').click();
    ownerState.sessionId = 'B';
    ownerState.presentationGeneration = 2;
    ownerState.sessionId = 'A';
    ownerState.presentationGeneration = 3;
    resolveRead({ content: 'old A', superseded: false });
    await settle();
    expect(get(viewer)).toBeNull();
    unmount(mounted.toolCall);
  });

  it('drops a backend-superseded result', async () => {
    const ownerState = { sessionId: 'A', presentationGeneration: 1 };
    backend.ReadFileContent = async () => ({ superseded: true });
    const mounted = await mountReadRow(() => ({ ...ownerState }));
    mounted.target.querySelector('.arg.path').click();
    await settle();
    expect(get(viewer)).toBeNull();
    unmount(mounted.toolCall);
  });
});

describe('ToolCall removed-memory vs retained-tool rendering', () => {
  async function mountRow(props) {
    const { default: ToolCall } = await import('./ToolCall.svelte');
    const target = document.createElement('div');
    document.body.appendChild(target);
    const toolCall = mount(ToolCall, { target, props });
    await tick();
    return { toolCall, target };
  }

  it('renders a forged removed memory tool through the generic renderer', async () => {
    for (const name of ['save_memory', 'search_memory', 'search_history']) {
      const { toolCall, target } = await mountRow({
        name,
        args: JSON.stringify({ key: 'value' }),
        done: true,
      });
      // The removed tools fell through to the generic {:else} branch once their
      // dedicated branches were deleted: a plain tool-name + arg line with no
      // read-file-specific .arg.path affordance. A regression that re-added a
      // dedicated branch would change this shape and fail here.
      const nameEl = target.querySelector('.tool-name');
      expect(nameEl).toBeTruthy();
      expect(nameEl.textContent).toBe(name);
      expect(target.querySelector('.arg.path')).toBeNull();
      const arg = target.querySelector('.arg');
      expect(arg).toBeTruthy();
      expect(arg.textContent).toBe(JSON.stringify({ key: 'value' }));
      unmount(toolCall);
    }
  });

  it('renders a retained ordinary tool through its dedicated renderer', async () => {
    const { toolCall, target } = await mountRow({
      name: 'read_file',
      args: JSON.stringify({ path: 'src/main.go' }),
      done: true,
    });
    // The retained read_file takes its dedicated branch, which renders a clickable
    // .arg.path affordance the generic renderer never produces — the distinguishing
    // shape between a removed tool and a retained one.
    const nameEl = target.querySelector('.tool-name');
    expect(nameEl).toBeTruthy();
    expect(nameEl.textContent).toBe('read_file');
    expect(target.querySelector('.arg.path')).toBeTruthy();
    unmount(toolCall);
  });
});
