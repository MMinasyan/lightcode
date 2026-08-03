// @vitest-environment happy-dom
import { describe, expect, it, beforeAll, afterEach } from 'vitest';
import { flushSync, mount, tick, unmount } from 'svelte';

// The permission prompt keeps its own in-progress selection (open suggestion
// list, chosen rules, pending save). The render site in App.svelte must rebuild
// the prompt when the displayed request changes, or a half-made selection for
// one request would carry over to the next pending request and save a rule
// against the wrong request and session.
//
// This mounts the real App and drives its live handlers: a request A is shown,
// a selection is started for it, then A is resolved elsewhere and the next
// pending request B becomes the displayed one. The in-progress selection must
// be gone — not carried over to B.

// Live event listeners App.svelte registers in onMount; the test drives the
// real handler code through them.
const listeners = new Map();

// Backend surface App.svelte touches on mount and PermissionPrompt touches
// during the scenario.
const backend = {
  SessionCurrent: async () => null,
  HydrateSession: async () => ({}),
  CurrentModel: async () => ({}),
  ProjectName: async () => '',
  CompactNow: async () => ({}),
  RespondPermission: async () => ({}),
  SaveProjectPermission: async () => ({}),
  PermissionSuggest: async () => [{ rule: 'rm a', label: 'rm a' }],
};

beforeAll(() => {
  window.runtime = {
    EventsOnMultiple: (name, cb) => { listeners.set(name, cb); return () => {}; },
    EventsOn: (name, cb) => { listeners.set(name, cb); return () => {}; },
  };
  window.go = { main: { App: backend } };
});

afterEach(() => {
  document.body.innerHTML = '';
});

function fire(name, data) {
  const cb = listeners.get(name);
  if (!cb) throw new Error(`no live listener registered for '${name}'`);
  cb(data);
}

async function settle() {
  // onMount is async (model/project fetch, hydration); flush pending effects
  // and let a macrotask pass so the buffered event delivery is live.
  flushSync();
  await new Promise((r) => setTimeout(r, 0));
  await tick();
}

describe('App permission prompt render site', () => {
  it('rebuilds the prompt when the displayed request advances, dropping an in-progress selection', async () => {
    const { default: App } = await import('./App.svelte');
    const target = document.createElement('div');
    document.body.appendChild(target);
    const app = mount(App, { target });
    await settle();

    // The first pending request is shown.
    fire('permission_request', { id: 'a', sessionId: 's1', projectId: 'p1', tool: 'bash', args: 'rm a', canSaveProject: true });
    await tick();
    expect(target.querySelector('.prompt .args').textContent).toContain('rm a');

    // A project-save selection is started for A: open the suggestions and
    // check one rule.
    target.querySelector('button.project').click();
    await settle();
    const box = target.querySelector('.suggest-row input[type="checkbox"]');
    expect(box).toBeTruthy();
    box.click();
    await tick();
    expect(target.querySelector('.suggest-actions .save').disabled).toBe(false);

    // A is answered/cancelled elsewhere; the next pending request B becomes
    // the displayed one.
    fire('permission_request', { id: 'b', sessionId: 's1', projectId: 'p1', tool: 'bash', args: 'rm b', canSaveProject: true });
    fire('permission_resolved', { id: 'a' });
    await tick();

    // The prompt now shows B...
    expect(target.querySelector('.prompt .args').textContent).toContain('rm b');
    // ...and the in-progress selection for A is gone, not carried over: the
    // suggestion list is closed and reopening it shows nothing checked.
    expect(target.querySelector('.suggest-panel')).toBeNull();

    target.querySelector('button.project').click();
    await settle();
    expect(target.querySelector('.suggest-row input[type="checkbox"]').checked).toBe(false);

    unmount(app);
  });
});
