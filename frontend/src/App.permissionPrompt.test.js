// @vitest-environment happy-dom
import { describe, expect, it, beforeAll, afterEach } from 'vitest';
import { flushSync, mount, tick, unmount } from 'svelte';
import { get } from 'svelte/store';
import { closeViewer, viewer } from './lib/viewer.js';

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
  CurrentModel: async () => ({ model: { ref: 'prov/model', provider: 'prov', model: 'model', displayName: 'Model' }, superseded: false }),
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

  it('after a failed hydration the snapshot-gated listeners stay inert and a boundary recovers', async () => {
    backend.HydrateSession = async () => { throw new Error('session unavailable'); };
    const { app, target } = await mountApp();

    expect(target.querySelector('.error-msg')?.textContent).toContain('Load session failed');
    expect(target.querySelector('.label.session').textContent).toBe('new session');

    // Warnings, usage, queue, compaction_start, and unsequenced error must all
    // stay inert over the unseeded view. Usage is observed non-vacuously: the
    // frame carries a non-zero context window that would create the context ring
    // and repaint the token counts if it were applied, so their absence proves
    // it was gated.
    fire('warnings', [{ kind: 'rules_too_large', message: 'rules too large' }]);
    fire('usage', { total: { cache: 100, input: 200, output: 300, known: true }, perModel: [], contextUsed: 50, contextWindow: 100 });
    fire('queue_changed', { version: 1, items: [{ id: 'q1', content: 'queued' }] });
    fire('compaction_start', null);
    await tick();

    expect(target.querySelector('.warn-icon')).toBeNull();
    // Usage inert: the non-zero context window produced no context ring...
    expect(target.querySelector('.context-ring')).toBeNull();
    // ...and the token counts stayed at their empty default.
    expect(target.querySelector('.tokens')?.textContent).toBe('⚡ 0 ↑ 0 ↓ 0');
    expect(target.querySelector('.queued-msg')).toBeNull();
    // compaction_start inert: no 'Compacting' activity over the unseeded view.
    expect(target.querySelector('.activity')).toBeNull();
    expect(target.querySelector('.system-msg')).toBeNull();
    expect(target.querySelector('.message.user')).toBeNull();
    expect(target.querySelector('.message.assistant')).toBeNull();

    // compaction_end, the unsequenced error, and a notice-only turn_action
    // arrive after a separate tick so start/end do not cancel within one tick;
    // each stays inert too.
    fire('compaction_end', null);
    fire('error', { message: 'unsequenced boom' });
    fire('turn_action', { skippedFiles: [{ path: 'x.txt', reason: 'outside session' }], warning: 'notice only boom' });
    await tick();
    expect(target.querySelector('.activity')).toBeNull();
    expect(target.querySelector('.system-msg')).toBeNull();
    // Only the hydration's own load-failure error renders; the fired unsequenced
    // error and the notice-only turn_action warning must not add another.
    const errs = [...target.querySelectorAll('.error-msg')];
    expect(errs).toHaveLength(1);
    expect(errs[0].textContent).toContain('Load session failed');

    // A stateful navigation boundary seeds the view and recovers it; normal
    // subsequent events apply again.
    fire('navigation', navState());
    await tick();
    expect(target.querySelector('.label.session').textContent).toBe('s1');
    expect(target.querySelector('.message.user .plain').textContent).toBe('hello');

    fire('warnings', [{ kind: 'rules_too_large', message: 'rules too large' }]);
    // Usage resumes: a non-zero context window now creates the context ring and
    // repaints the token counts.
    fire('usage', { total: { cache: 100, input: 200, output: 300, known: true }, perModel: [], contextUsed: 50, contextWindow: 100 });
    fire('queue_changed', { version: 1, items: [{ id: 'q1', content: 'queued' }] });
    fire('error', { message: 'admitted after recovery' });
    await tick();
    expect(target.querySelector('.warn-icon')).toBeTruthy();
    expect(target.querySelector('.context-ring')).toBeTruthy();
    expect(target.querySelector('.tokens')?.textContent).toBe('⚡ 100 ↑ 200 ↓ 300');
    expect(target.querySelector('.queued-msg')?.textContent).toBe('queued');
    expect(target.querySelector('.error-msg')?.textContent).toContain('admitted after recovery');

    // Compaction resumes as an independent indicator: start raises 'Compacting'
    // and end clears it, each after its own tick.
    fire('compaction_start', null);
    await tick();
    expect(target.querySelector('.activity')?.textContent).toContain('Compacting');
    fire('compaction_end', null);
    await tick();
    expect(target.querySelector('.activity')).toBeNull();

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

  it('renders retained affected-file lists and hides staging-only controls and project save when disabled', async () => {
    const { app, target } = await mountApp();

    fire('permission_request', {
      id: 'a', sessionId: 's1', projectId: 'p1', tool: 'apply_patch',
      args: '/tmp/project/a.txt',
      canSaveProject: false,
      batchFiles: ['/tmp/project/a.txt', '/tmp/project/.env'],
      batchResolvedFiles: ['/tmp/project/a.txt', '/tmp/project/.env'],
    });
    await tick();

    // The retained affected-file list renders as a list.
    const batch = target.querySelector('.prompt .batch-list');
    expect(batch).toBeTruthy();
    expect(batch.textContent).toContain('/tmp/project/a.txt');
    expect(batch.textContent).toContain('/tmp/project/.env');
    expect(batch.textContent).toContain('Affected files');

    // Staging-only controls are absent: no Allow-all action and no batch
    // position indicator.
    const actions = [...target.querySelectorAll('.prompt .actions button')].map(b => b.textContent);
    expect(actions).not.toContain('Allow all');
    expect(target.querySelector('.prompt').textContent).not.toMatch(/\(\d+\/\d+\)/);

    // DisableProjectSave is delivered as canSaveProject=false: the project-save
    // button is hidden, leaving only Allow and Deny.
    expect(target.querySelector('.prompt .actions .project')).toBeNull();
    expect(actions).toEqual(['Allow', 'Deny']);

    unmount(app);
  });

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

describe('App hydration replay and queue guard', () => {
  // The boot barrier's buffering path (agents.md §2): every event listener is
  // registered before the first hydration read, one snapshot of the
  // adapter-current session is applied, then whatever arrived during that
  // window replays in order — so a turn already streaming when the frontend
  // attaches is never silently lost, while the transcript gate drops anything
  // the snapshot already covers (sequence at or below its high-water).

  it('replays sequenced frames buffered during the pending read, in delivery order, dropping the frame the snapshot already covers', async () => {
    // Hold the hydration read unresolved so frames delivered now are buffered
    // by the listener wrappers rather than applied to the view.
    let resolveHydrate;
    backend.HydrateSession = () => new Promise((resolve) => { resolveHydrate = resolve; });
    const { app, target } = await mountApp();

    // The stale frame is deliberately first: admitSequenced advances the
    // high-water on every admitted frame, so a stale frame placed after the
    // two live ones would be dropped by the advanced gate even if snapshot
    // cursor seeding were broken. Placed first, it is tested against the
    // seeded high-water alone.
    fire('user_message', { seq: 1, turn: 1, content: 'stale' });
    fire('user_message', { seq: 2, turn: 1, content: 'second' });
    fire('user_message', { seq: 3, turn: 1, content: 'third' });

    // Nothing renders while the read is pending: the frames are buffered, not
    // applied (the snapshot must land first, or its replace clobbers them).
    expect(target.querySelector('.message.user')).toBeNull();

    // The snapshot's high-water comes only from cursor.committedSeq, tail[].seq
    // and errors[].seq; the file's default mock returns none of them, so its
    // gate would be {highWater: 0} and every frame admitted. Carrying
    // committedSeq 1 seeds the gate above the stale seq-1 frame.
    resolveHydrate({ ...navState(), messages: [], cursor: { committedTurn: 1, committedSeq: 1, rewriteEpoch: 0 } });
    await settle();

    // The two live frames replay in delivery order; the stale frame is absent.
    const rows = [...target.querySelectorAll('.message.user .plain')].map((el) => el.textContent);
    expect(rows).toEqual(['second', 'third']);

    unmount(app);
  });

  it('buffers a queue_changed event delivered while hydration is pending and replays it after the snapshot', async () => {
    let resolveHydrate;
    backend.HydrateSession = () => new Promise((resolve) => { resolveHydrate = resolve; });
    const { app, target } = await mountApp();

    // The event is versioned above the default watermark (0) but must be held:
    // applied immediately, it would mutate messageQueue, and the snapshot then
    // clobbers the queue and re-seeds the watermark, so the live update is
    // lost and never replayed.
    fire('queue_changed', { version: 1, items: [{ id: 'q1', content: 'queued during read' }] });
    expect(target.querySelector('.queued-msg')).toBeNull();

    resolveHydrate(navState());
    await settle();

    expect(target.querySelector('.queued-msg')?.textContent).toBe('queued during read');

    unmount(app);
  });

  it('drops stale queue_changed versions, applies higher ones, and re-seeds the watermark from a boundary', async () => {
    const { app, target } = await mountApp();

    // The default hydration mock carries no queue, so the watermark is 0.
    fire('queue_changed', { version: 5, items: [{ id: 'a', content: 'A' }] });
    await tick();
    expect(target.querySelector('.queued-msg')?.textContent).toBe('A');

    // Equal to the watermark: ignored. The equality is the load-bearing half
    // of the guard: an older version would be dropped even if the guard
    // regressed from `<=` to `<`, while an equal one only drops under `<=`.
    fire('queue_changed', { version: 5, items: [{ id: 'x', content: 'X' }] });
    await tick();
    expect(target.querySelector('.queued-msg')?.textContent).toBe('A');

    // A navigation boundary re-seeds the watermark from the snapshot's queue
    // version (2), below the live watermark (5)...
    fire('navigation', { ...navState(), queue: { version: 2, items: [{ id: 'b', content: 'B' }] } });
    await tick();
    expect(target.querySelector('.queued-msg')?.textContent).toBe('B');

    // ...so a version above the re-seeded watermark applies; without the
    // re-seed it would be dropped against the stale 5.
    fire('queue_changed', { version: 3, items: [{ id: 'c', content: 'C' }] });
    await tick();
    expect(target.querySelector('.queued-msg')?.textContent).toBe('C');

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

describe('App ordered presentation boundaries', () => {
  it('a buffered navigation boundary survives stale hydration success and failure', async () => {
    // Success half: the navigation boundary for B is buffered while the
    // hydration read (for A) is still in flight; the stale A snapshot applies
    // first and the buffered boundary replaces it — B wins.
    let resolveHydrate;
    backend.HydrateSession = () => new Promise((resolve) => { resolveHydrate = resolve; });
    let { app, target } = await mountApp();

    fire('navigation', { ...navState(), session: { id: 'B' }, messages: [{ type: 'user', content: 'B row' }] });
    await tick();
    expect(target.querySelector('.label.session').textContent).toBe('new session');

    resolveHydrate({ ...navState(), session: { id: 'A' }, messages: [{ type: 'user', content: 'A row' }] });
    await settle();
    expect(target.querySelector('.label.session').textContent).toBe('B');
    expect(target.querySelector('.message.user .plain').textContent).toBe('B row');
    unmount(app);

    // Failure half: the buffered boundary is the first stateful frame, so the
    // failed hydration discards nothing before it; the boundary seeds the
    // view and its gate stays open for live frames after it.
    let failHydrate;
    backend.HydrateSession = () => new Promise((resolve, reject) => { failHydrate = reject; });
    ({ app, target } = await mountApp());
    fire('navigation', { ...navState(), session: { id: 'B' }, messages: [{ type: 'user', content: 'B row' }] });
    fire('token', { seq: 2, content: 'live after B' });
    failHydrate(new Error('session unavailable'));
    await settle();

    expect(target.querySelector('.label.session').textContent).toBe('B');
    expect(target.querySelector('.message.user .plain').textContent).toBe('B row');
    // The live frame after the boundary renders; partial assistant markdown is
    // debounced, so wait out the timer before reading the text.
    await new Promise((r) => setTimeout(r, 60));
    expect(target.querySelector('.message.assistant')?.textContent).toContain('live after B');
    expect((await setDraft(target, 'hello')).disabled).toBe(false);
    unmount(app);
  });

  it('a partial history revert renders its ordered warning exactly once', async () => {
    // The reconciled partial revert publishes the ordered turn_action frame
    // (state + warning) first; the direct method settles after it. The
    // changed presentation generation must drop the late rejection — the
    // warning renders once, through the ordered frame only.
    let rejectRevert;
    backend.ApplyTurnAction = () => new Promise((resolve, reject) => { rejectRevert = reject; });
    const { app, target } = await mountApp();

    // A user row with a turn so the revert affordance is reachable.
    fire('navigation', { ...navState(), messages: [{ type: 'user', content: 'hello', turn: 1 }] });
    await tick();
    target.querySelector('.revert-icon').click();
    await settle();
    [...target.querySelectorAll('.revert-menu .menu-item')].find((b) => b.textContent === 'Revert history').click();
    await tick();
    target.querySelector('.confirm-btn.yes').click();
    await settle();

    fire('turn_action', {
      state: { ...navState(), session: { id: 's1' } },
      skippedFiles: [],
      warning: 'kept 2 files changed outside this session',
    });
    await tick();
    rejectRevert(new Error('kept 2 files changed outside this session'));
    await settle();

    // Exactly one error row, and it is the frame's warning: a second renderer
    // or an ungated promise rejection would duplicate it.
    const errs = [...target.querySelectorAll('.error-msg')];
    expect(errs).toHaveLength(1);
    expect(errs[0].textContent).toContain('kept 2 files changed outside this session');
    unmount(app);
  });
});

describe('App ordered turn-action outcomes', () => {
  // revertHistoryBoundary is the ordered turn_action boundary a history revert
  // publishes: the reverted session's complete state, the input prefill, the
  // kept-files notice, and the warning, in that order.
  function revertHistoryBoundary(overrides = {}) {
    return {
      state: { ...navState(), session: { id: 's1' } },
      prefill: 'draft text',
      skippedFiles: [{ path: 'x.txt', reason: 'outside session' }],
      warning: 'committed sync failure',
      ...overrides,
    };
  }

  // mountRevertInFlight seeds a session with a user row, opens the revert
  // history confirm flow, and holds the ApplyTurnAction promise unresolved so
  // the test can drive the boundary and the promise settle in either order.
  async function mountRevertInFlight() {
    let resolveRevert;
    let rejectRevert;
    backend.ApplyTurnAction = () => new Promise((resolve, reject) => {
      resolveRevert = resolve;
      rejectRevert = reject;
    });
    const { app, target } = await mountApp();
    fire('navigation', { ...navState(), messages: [{ type: 'user', content: 'hello', turn: 1 }] });
    await tick();
    target.querySelector('.revert-icon').click();
    await settle();
    [...target.querySelectorAll('.revert-menu .menu-item')].find((b) => b.textContent === 'Revert history').click();
    await tick();
    target.querySelector('.confirm-btn.yes').click();
    await settle();
    return { app, target, resolveRevert, rejectRevert };
  }

  async function setDraftText(target, text) {
    const ta = target.querySelector('textarea');
    ta.value = text;
    ta.dispatchEvent(new Event('input'));
    await tick();
  }

  it('a history revert boundary applies prefill between the snapshot and the notices, never from the promise', async () => {
    const { app, target, resolveRevert } = await mountRevertInFlight();

    // The promise settles before the ordered boundary delivers: its result
    // prefill must not be applied — the ordered boundary is the input prefill
    // authority.
    resolveRevert({ prefill: 'ignored by promise' });
    await settle();
    fire('turn_action', revertHistoryBoundary());
    await tick();

    expect(target.querySelector('textarea').value).toBe('draft text');
    // The kept-files notice and the warning follow the state and the prefill
    // in one ordered frame.
    const order = [...target.querySelector('.message-list').children].map((el) => el.className);
    const iUser = order.findIndex((c) => c.includes('message user'));
    const iSys = order.findIndex((c) => c.includes('system-msg'));
    const iErr = order.findIndex((c) => c.includes('error-msg'));
    expect(iUser).toBeGreaterThanOrEqual(0);
    expect(iSys).toBeGreaterThan(iUser);
    expect(iErr).toBeGreaterThan(iSys);
    const errs = [...target.querySelectorAll('.error-msg')];
    expect(errs).toHaveLength(1);
    expect(errs[0].textContent).toContain('committed sync failure');

    unmount(app);
  });

  it('a legitimate empty prefill clears the composer', async () => {
    const { app, target, resolveRevert } = await mountRevertInFlight();
    await setDraftText(target, 'typed draft');

    fire('turn_action', revertHistoryBoundary({ prefill: '' }));
    await tick();
    expect(target.querySelector('textarea').value).toBe('');

    resolveRevert({});
    await settle();
    unmount(app);
  });

  it('a nil prefill (fork or code revert) leaves the composer untouched', async () => {
    const { app, target, resolveRevert } = await mountRevertInFlight();
    await setDraftText(target, 'typed draft');

    fire('turn_action', { state: { ...navState(), session: { id: 's2' } }, skippedFiles: [] });
    await tick();
    expect(target.querySelector('textarea').value).toBe('typed draft');

    resolveRevert({});
    await settle();
    unmount(app);
  });

  it('typed committed history settles to one warning when the boundary arrives first', async () => {
    const { app, target, rejectRevert } = await mountRevertInFlight();

    fire('turn_action', revertHistoryBoundary());
    await tick();
    rejectRevert(new Error('committed sync failure'));
    await settle();

    expect(target.querySelector('textarea').value).toBe('draft text');
    const errs = [...target.querySelectorAll('.error-msg')];
    expect(errs).toHaveLength(1);
    expect(errs[0].textContent).toContain('committed sync failure');

    unmount(app);
  });

  it('typed committed history settles to one warning when the rejection arrives first', async () => {
    const { app, target, rejectRevert } = await mountRevertInFlight();

    rejectRevert(new Error('committed sync failure'));
    await settle();
    fire('turn_action', revertHistoryBoundary());
    await tick();

    // The transient rejection error was replaced by the stateful boundary:
    // exactly one warning remains, with the prefill applied.
    expect(target.querySelector('textarea').value).toBe('draft text');
    const errs = [...target.querySelectorAll('.error-msg')];
    expect(errs).toHaveLength(1);
    expect(errs[0].textContent).toContain('committed sync failure');

    unmount(app);
  });

  it('an unrelated navigation between the rejection and the revert boundary stays deterministic', async () => {
    const { app, target, rejectRevert } = await mountRevertInFlight();

    // The unrelated navigation lands first: its generation advance suppresses
    // the stale rejection on B...
    fire('navigation', { ...navState(), session: { id: 'B' }, messages: [{ type: 'user', content: 'B row' }] });
    await tick();
    rejectRevert(new Error('committed sync failure'));
    await settle();
    expect(target.querySelector('.label.session').textContent).toBe('B');
    expect(target.querySelector('.error-msg')).toBeNull();

    // ...and the revert boundary still applies when delivered, as the ordered
    // authority over the reverted session.
    fire('turn_action', revertHistoryBoundary());
    await tick();
    expect(target.querySelector('.label.session').textContent).toBe('s1');
    expect(target.querySelector('textarea').value).toBe('draft text');
    const errs = [...target.querySelectorAll('.error-msg')];
    expect(errs).toHaveLength(1);
    expect(errs[0].textContent).toContain('committed sync failure');

    unmount(app);
  });
});

describe('App child link survives navigation', () => {
  it('a live subagent link on a parent tool row still opens the same child viewer after navigating away and back', async () => {
    const { app, target } = await mountApp();

    // Seed the transcript gate from the navigation boundary, then deliver the
    // parent task row live: the row exists without any child association yet.
    fire('navigation', navState());
    await tick();
    fire('tool_start', { seq: 2, id: 'parent-task', name: 'task', args: JSON.stringify({ tasks: [{ subagent_type: 'explore', prompt: 'find the bug' }] }) });
    await tick();

    // Without the id-keyed association the row opens no child viewer: the
    // click is a no-op until the link exists.
    target.querySelector('.subtask-row').click();
    await settle();
    expect(get(viewer)).toBeNull();

    // The live subagent start folds the link into the row; the same click now
    // opens the child viewer.
    fire('subagent_session_start', { taskIndex: 0, sessionId: 'child-1', taskToolCallId: 'parent-task' });
    await tick();
    target.querySelector('.subtask-row').click();
    await settle();
    expect(get(viewer).sessionId).toBe('child-1');
    expect(get(viewer).live).toBe(true);
    closeViewer();

    // Navigate away: the view is replaced wholesale and the row is gone.
    fire('navigation', { ...navState(), session: { id: 'other' }, messages: [] });
    await tick();
    expect(target.querySelector('.label.session').textContent).toBe('other');
    expect(target.querySelector('.subtask-row')).toBeNull();

    // Navigate back before child completion. The authoritative live tail
    // carries the linked in-progress tool row, so it opens the same child
    // viewer while the child hydration read is still pending.
    let childResolve;
    backend.HydrateSession = () => new Promise((resolve) => { childResolve = resolve; });
    fire('navigation', {
      ...navState(),
      session: { id: 's1' },
      busy: true,
      messages: [
        { type: 'user', content: 'hello' },
        {
          type: 'tool',
          id: 'parent-task',
          name: 'task',
          args: JSON.stringify({ tasks: [{ subagent_type: 'explore', prompt: 'find the bug' }] }),
          done: false,
          success: true,
          result: '',
          subagentSessionIds: [{ index: 0, sessionId: 'child-1' }],
        },
      ],
      tail: [{
        seq: 2,
        message: {
          type: 'tool',
          id: 'parent-task',
          name: 'task',
          args: JSON.stringify({ tasks: [{ subagent_type: 'explore', prompt: 'find the bug' }] }),
          done: false,
          success: true,
          result: '',
          subagentSessionIds: [{ index: 0, sessionId: 'child-1' }],
        },
      }],
    });
    await tick();
    target.querySelector('.subtask-row').click();
    await settle();

    // Before completion: the same child viewer is open and still reading.
    const v = get(viewer);
    expect(v.sessionId).toBe('child-1');
    expect(v.live).toBe(true);
    expect(v.reading).toBe(true);
    expect(v.messages).toEqual([]);

    // The read completes into that viewer.
    childResolve({ messages: [{ type: 'assistant', content: 'child answer' }], tail: [], errors: [], cursor: { committedTurn: 1, committedSeq: 0, rewriteEpoch: 0 } });
    await settle();
    expect(get(viewer).messages).toEqual([{ type: 'assistant', content: 'child answer' }]);
    expect(get(viewer).reading).toBe(false);

    closeViewer();
    unmount(app);
  });
});
