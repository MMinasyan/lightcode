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

// The backend surface this path touches: HydrateSession is the only binding
// openSubagentTranscript calls. ReadFileContent is imported by the component
// but only reached from the read_file/write_file path, which this test does
// not exercise, so it needs no stub.
const backend = {
  HydrateSession: async () => ({}),
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
