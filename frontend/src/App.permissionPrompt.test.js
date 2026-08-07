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
  SessionCurrent: async () => ({ id: 's1' }),
  HydrateSession: async () => ({ session: { id: 's1' } }),
  CurrentModel: async () => ({ ref: 'prov/model', provider: 'prov', model: 'model', displayName: 'Model' }),
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
  backend.SessionCurrent = async () => ({ id: 's1' });
  backend.HydrateSession = async () => ({ session: { id: 's1' } });
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

// A complete-state navigation boundary carries everything the view needs; the
// boot-gate tests use it to prove the window recovers after a failed hydration.
function navState() {
  return {
    session: { id: 's1' },
    messages: [{ type: 'user', content: 'hello' }],
    tail: [],
    errors: [],
    cursor: { committedTurn: 1, committedSeq: 1, rewriteEpoch: 0 },
    tokens: { total: { cache: 0, input: 0, output: 0, known: true }, perModel: [], contextUsed: 0, contextWindow: 0 },
    model: { ref: 'prov/model', provider: 'prov', model: 'model', displayName: 'Model' },
    busy: false,
    compacting: false,
    queue: { items: [], version: 0 },
    warnings: [],
    permissions: [],
  };
}

async function mountApp() {
  const { default: App } = await import('./App.svelte');
  const target = document.createElement('div');
  document.body.appendChild(target);
  const app = mount(App, { target });
  await settle();
  return { app, target };
}

// setDraft types into the composer so the send button reflects the model gate
// rather than the empty-draft state.
async function setDraft(target, text) {
  const ta = target.querySelector('textarea');
  ta.value = text;
  ta.dispatchEvent(new Event('input'));
  await tick();
  return target.querySelector('button.send');
}

describe('App boot barrier over unseeded state', () => {
  it('a failed session hydration shows its error, renders no transcript, drops live frames, and blocks submission until a navigation boundary arrives', async () => {
    backend.HydrateSession = async () => { throw new Error('session unavailable'); };
    const { app, target } = await mountApp();

    // The error path shows its error, and no session is named.
    expect(target.querySelector('.error-msg')?.textContent).toContain('Load session failed');
    expect(target.querySelector('.label.session').textContent).toBe('new session');

    // A sequenced frame arriving after the failed hydration renders nothing.
    fire('token', { seq: 1, content: 'live delta' });
    await tick();
    expect(target.querySelector('.message.assistant')).toBeNull();

    // The composer cannot submit while no snapshot was applied.
    expect((await setDraft(target, 'hello')).disabled).toBe(true);

    // A navigation boundary carries complete state and recovers the view,
    // including the composer and the live stream.
    fire('navigation', navState());
    await tick();
    expect(target.querySelector('.label.session').textContent).toBe('s1');
    expect(target.querySelector('.message.user .plain').textContent).toBe('hello');
    expect((await setDraft(target, 'hello')).disabled).toBe(false);
    fire('token', { seq: 2, content: 'after' });
    await tick();
    expect(target.querySelector('.message.assistant')).toBeTruthy();

    unmount(app);
  });

  it('drops a warnings frame buffered while hydration is pending, and still recovers on navigation', async () => {
    // Hold the hydration call unresolved so a frame delivered now is buffered
    // rather than delivered after the barrier already opened.
    let failHydrate;
    backend.HydrateSession = () => new Promise((resolve, reject) => { failHydrate = reject; });
    const { app, target } = await mountApp();

    // A warnings frame is not sequence-gated and not listener-gated, so only
    // the buffered-frame discard keeps it off a session that never loaded.
    fire('warnings', [{ kind: 'rules_too_large', message: 'rules too large' }]);
    failHydrate(new Error('session unavailable'));
    await settle();

    expect(target.querySelector('.warn-icon')).toBeNull();
    expect(target.querySelector('.error-msg')?.textContent).toContain('Load session failed');

    // The discard costs no recoverability: a navigation boundary still applies.
    fire('navigation', navState());
    await tick();
    expect(target.querySelector('.label.session').textContent).toBe('s1');
    expect(target.querySelector('.message.user .plain').textContent).toBe('hello');
    fire('token', { seq: 2, content: 'after' });
    await tick();
    expect(target.querySelector('.message.assistant')).toBeTruthy();

    unmount(app);
  });

  it('a failed current-session lookup shows its error and closes the same gate', async () => {
    backend.SessionCurrent = async () => { throw new Error('no backend'); };
    const { app, target } = await mountApp();

    expect(target.querySelector('.error-msg')?.textContent).toContain('Load session failed');
    expect(target.querySelector('.label.session').textContent).toBe('new session');

    fire('user_message', { seq: 1, turn: 1, content: 'hello' });
    await tick();
    expect(target.querySelector('.message.user')).toBeNull();

    expect((await setDraft(target, 'hello')).disabled).toBe(true);

    fire('navigation', navState());
    await tick();
    expect(target.querySelector('.label.session').textContent).toBe('s1');
    expect((await setDraft(target, 'hello')).disabled).toBe(false);

    unmount(app);
  });

  it('an empty startup names no session, invents no error, drops live frames, and recovers on navigation', async () => {
    backend.SessionCurrent = async () => null;
    const { app, target } = await mountApp();

    // The no-id path is an ordinary empty startup: no error is invented...
    expect(target.querySelector('.error-msg')).toBeNull();
    expect(target.querySelector('.label.session').textContent).toBe('new session');

    // ...but the gate is closed all the same: a frame for a session the view
    // never loaded cannot render.
    fire('token', { seq: 1, content: 'live delta' });
    await tick();
    expect(target.querySelector('.message.assistant')).toBeNull();

    expect((await setDraft(target, 'hello')).disabled).toBe(true);

    fire('navigation', navState());
    await tick();
    expect(target.querySelector('.label.session').textContent).toBe('s1');
    expect(target.querySelector('.message.user .plain').textContent).toBe('hello');
    expect((await setDraft(target, 'hello')).disabled).toBe(false);

    unmount(app);
  });

  it('after a failed hydration the ungated live listeners leave the view unchanged', async () => {
    backend.HydrateSession = async () => { throw new Error('session unavailable'); };
    const { app, target } = await mountApp();

    // A permission request must not become an answerable prompt over a session
    // that was never loaded, and its resolution must not either.
    fire('permission_request', { id: 'a', sessionId: 's1', projectId: 'p1', tool: 'bash', args: 'rm a', canSaveProject: true });
    await tick();
    expect(target.querySelector('.prompt')).toBeNull();
    fire('permission_resolved', { id: 'a' });
    await tick();
    expect(target.querySelector('.prompt')).toBeNull();

    // turn_start must not raise a busy spinner over the unseeded view...
    fire('turn_start', { turn: 1 });
    await tick();
    expect(target.querySelector('.activity')).toBeNull();
    // ...and turn_end must not touch the view either.
    fire('turn_end', {});
    await tick();
    expect(target.querySelector('.activity')).toBeNull();
    expect(target.querySelector('.prompt')).toBeNull();

    unmount(app);
  });

  it('a read-only hydration leaves the composer unable to submit', async () => {
    backend.HydrateSession = async () => ({
      session: { id: 's1' },
      messages: [],
      tail: [],
      errors: [],
      cursor: { committedTurn: 0, committedSeq: 0, rewriteEpoch: 0 },
      model: { ref: 'prov/model', provider: 'prov', model: 'model', displayName: 'Model' },
      readOnly: true,
    });
    const { app, target } = await mountApp();

    expect(target.querySelector('.label.session').textContent).toBe('s1');
    expect((await setDraft(target, 'hello')).disabled).toBe(true);

    unmount(app);
  });
});

describe('App permission prompt on a boundary', () => {
  // A snapshot replaces the view wholesale, so a prompt open for the session
  // being left closes unanswered. The request is not lost — it stays pending
  // for its own session — so the snapshot-apply path must say so in a system
  // row naming what was dismissed.

  function promptForA() {
    fire('permission_request', { id: 'a', sessionId: 's1', projectId: 'p1', tool: 'bash', args: 'rm a', canSaveProject: true });
  }

  it('a navigation boundary for another session closes the prompt with a notice, and the request still answers through its own session', async () => {
    const responses = [];
    backend.RespondPermission = async (sessionId, id, action) => { responses.push({ sessionId, id, action }); };
    const { app, target } = await mountApp();

    // A prompt is open for session s1.
    promptForA();
    await tick();
    expect(target.querySelector('.prompt .args').textContent).toContain('rm a');

    // A navigation boundary for s2 replaces the view: the prompt closes and a
    // notice names what was dismissed instead of letting it vanish silently.
    fire('navigation', { ...navState(), session: { id: 's2' } });
    await tick();
    expect(target.querySelector('.prompt')).toBeNull();
    expect(target.querySelector('.label.session').textContent).toBe('s2');
    const notice = target.querySelector('.system-msg');
    expect(notice?.textContent).toContain('rm a');
    expect(notice?.textContent).toContain('s1');

    // The request was not lost: a boundary back to s1 carrying the pending
    // request shows the prompt again, and answering it resolves through s1.
    fire('navigation', { ...navState(), session: { id: 's1' }, permissions: [{ id: 'a', session_id: 's1', project_id: 'p1', tool: 'bash', args: 'rm a' }] });
    await tick();
    expect(target.querySelector('.prompt .args').textContent).toContain('rm a');
    target.querySelector('.prompt .actions .allow').click();
    await settle();
    expect(responses).toEqual([{ sessionId: 's1', id: 'a', action: 'allow' }]);

    unmount(app);
  });

  it('a read-only navigation destination closes the prompt the same way', async () => {
    const { app, target } = await mountApp();

    promptForA();
    await tick();
    expect(target.querySelector('.prompt')).toBeTruthy();

    // The destination is a session another process drives, opened read-only;
    // the dismissal notice appears the same as for an ordinary destination.
    fire('navigation', { ...navState(), session: { id: 's2' }, readOnly: true });
    await tick();
    expect(target.querySelector('.prompt')).toBeNull();
    const notice = target.querySelector('.system-msg');
    expect(notice?.textContent).toContain('rm a');
    expect(notice?.textContent).toContain('s1');

    unmount(app);
  });

  it('a turn-action boundary for a fork destination closes the prompt the same way', async () => {
    const { app, target } = await mountApp();

    promptForA();
    await tick();
    expect(target.querySelector('.prompt')).toBeTruthy();

    // A fork boundary for a new session reaches the same snapshot-apply path
    // as navigation, so it carries the same dismissal notice without a second
    // guard at the event handler.
    fire('turn_action', { state: { ...navState(), session: { id: 's2' } }, skippedFiles: [] });
    await tick();
    expect(target.querySelector('.prompt')).toBeNull();
    const notice = target.querySelector('.system-msg');
    expect(notice?.textContent).toContain('rm a');
    expect(notice?.textContent).toContain('s1');

    unmount(app);
  });

  it('a boundary for the same session does not invent a dismissal notice', async () => {
    const { app, target } = await mountApp();

    // A prompt open for s1 survives a boundary that stays on s1: the snapshot
    // carries the pending request, so the prompt is re-seeded, not dismissed.
    promptForA();
    await tick();
    fire('navigation', { ...navState(), session: { id: 's1' }, permissions: [{ id: 'a', session_id: 's1', project_id: 'p1', tool: 'bash', args: 'rm a' }] });
    await tick();
    expect(target.querySelector('.prompt .args').textContent).toContain('rm a');
    expect(target.querySelector('.system-msg')).toBeNull();

    unmount(app);
  });
});

describe('App turn-action fork warning', () => {
  it('a fork whose code revert failed shows the warning after the state and the kept-files notice are applied', async () => {
    const { app, target } = await mountApp();

    // The fork's state arrives as the ordered turn_action boundary, which
    // replaces the transcript wholesale; the failed code revert's warning must
    // ride that same frame and be appended after the state and the skip
    // notice, or the replace clobbers it.
    fire('turn_action', {
      state: { ...navState(), session: { id: 's2' } },
      skippedFiles: [{ path: 'x.txt', reason: 'outside session' }],
      warning: 'forked, but the code revert failed: boom',
    });
    await tick();

    // The destination state is applied...
    expect(target.querySelector('.label.session').textContent).toBe('s2');
    expect(target.querySelector('.message.user .plain').textContent).toBe('hello');
    // ...then the kept-files notice, then the warning, in that order.
    const order = [...target.querySelector('.message-list').children].map((el) => el.className);
    const iUser = order.findIndex((c) => c.includes('message user'));
    const iSys = order.findIndex((c) => c.includes('system-msg'));
    const iErr = order.findIndex((c) => c.includes('error-msg'));
    expect(iUser).toBeGreaterThanOrEqual(0);
    expect(iSys).toBeGreaterThan(iUser);
    expect(iErr).toBeGreaterThan(iSys);
    expect(target.querySelector('.error-msg').textContent).toContain('code revert failed');

    unmount(app);
  });

  it('a turn-action frame without a warning renders none', async () => {
    const { app, target } = await mountApp();

    fire('turn_action', { state: { ...navState(), session: { id: 's2' } }, skippedFiles: [] });
    await tick();
    expect(target.querySelector('.label.session').textContent).toBe('s2');
    expect(target.querySelector('.error-msg')).toBeNull();

    unmount(app);
  });
});
