// @vitest-environment happy-dom
import { describe, expect, it, beforeAll, afterAll } from 'vitest';
import { flushSync, mount, tick, unmount } from 'svelte';

// A committed lifecycle outcome must settle to exactly one visible error no matter which surface reaches first: the selector's catch (a transient shown by App.showError) or its own boundary+error pair. These tests drive a mounted real App through its live handlers with an actually-opened session selector and controlled backend ordering — not copies of either handler:
// - rejection-first: the plain-shaped rejection settles before any frame lands, so this catch is sole at that moment; when the committed pair arrives afterwards, applying its stateful snapshot replaces the transcript wholesale (wiping the transient) and only then does the paired unsequenced error append — exactly one visible final error remains, carrying the backend's text.
// - frame-first: the boundary applies before the rejection settles, advancing the presentation generation; the catch then classifies stale against its captured value and stays silent — again exactly one visible error, the pair's.

const listeners = new Map();

beforeAll(() => {
  window.runtime = {
    EventsOnMultiple: (name, cb) => { listeners.set(name, cb); return () => {}; },
    EventsOn: (name, cb) => { listeners.set(name, cb); return () => {}; },
  };
});

function fire(name, data) {
  const cb = listeners.get(name);
  if (!cb) throw new Error(`no live listener registered for '${name}'`);
  cb(data);
}

// navState builds one complete-state snapshot the way a backend boundary carries it. The user message carries its turn so Message renders its revert menu — the GUI's fork entry point.
function navState(id) {
  return {
    session: { id, projectPath: '/p/alpha' },
    messages: [{ type: 'user', content: `hello from ${id}`, turn: 1 }],
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

// mountApp mounts the real App over a backend whose lifecycle mutations (SessionNew, ApplyTurnAction's fork, ProjectSwitch) are deferred until the test decides. Every other binding returns prepared values; nothing here reaches an owner this suite does not drive. With { unseeded: true } the startup hydration fails — snapshotApplied stays false and presentationGeneration 0 while hydrated becomes true, so opened selectors capture mutSeeded=false exactly as a no-session startup presents them in production, and every 'error' event is gated out until some stateful boundary seeds the view (the committed pair's own admission order is what those rows settle).
async function mountApp({ unseeded = false } = {}) {
  let rejectNew = null; // set to the pending create's rejection once it is in flight
  let rejectFork = null; // set to the pending fork action's rejection once it is in flight
  let rejectSwitch = null; // set to the pending project switch's rejection once it is in flight
  const backend = {
    SessionCurrent: async () => ({ id: 'a' }),
    HydrateSession: unseeded ? async () => { throw new Error('no startup session'); } : (id) => navState(id || 'a'),
    CurrentModel: async () => ({ model: { ref: 'prov/model', provider: 'prov', model: 'model', displayName: 'Model' }, superseded: false }),
    ProjectName: async () => 'alpha',
    CompactNow: async () => ({}),
    RespondPermission: async () => ({}),
    SaveProjectPermission: async () => ({}),
    PermissionSuggest: async () => [],
    SessionList: async () => [{ id: 'a' }, { id: 'b' }], // the opened selector's list; identity comes from the seeded prop, not an owner read
    SessionNew: () => new Promise((_, rj) => { rejectNew = rj; }),
    ApplyTurnAction: (turn, action) => { if (action !== 'fork') throw new Error(`unstubbed turn action ${action}`); return new Promise((_, rj) => { rejectFork = rj; }); }, // the GUI's fork route — MessageList dispatches it, handleFork settles its rejection
    ProjectList: async () => [{ name: 'alpha', path: '/p/alpha' }, { name: 'beta', path: '/p/beta' }],
    ProjectCurrent: async () => ({ path: '/p/alpha' }),
    ProjectSwitch: (path) => new Promise((_, rj) => { if (path !== '/p/beta') throw new Error(`unstubbed project switch to ${path}`); rejectSwitch = rj; }),
  };
  window.go = { main: { App: backend } };

  const target = document.createElement('div');
  document.body.appendChild(target);
  const app = mount(App, { target });
  flushSync();
  await new Promise((r) => setTimeout(r, 0));
  await tick();
  return { app, target, rejectNew: () => rejectNew, rejectFork: () => rejectFork, rejectSwitch: () => rejectSwitch };
}

async function settle() {
  flushSync();
  await new Promise((r) => setTimeout(r, 0));
  await tick();
}

// openForkMenu walks the GUI's fork affordance to a confirmed call on turn 1: the seeded user message's revert menu, "Fork from here", then Yes. Returns once ApplyTurnAction('fork') is in flight.
async function confirmFork(targetEl) {
  targetEl.querySelector('.revert-icon').click(); // open the seeded user turn's revert options
  await settle();
  [...targetEl.querySelectorAll('.menu-item')].find((b) => b.textContent === 'Fork from here').click();
  await settle();
  [...targetEl.querySelectorAll('.confirm-btn.yes')][0].click(); // confirm — handleFork calls ApplyTurnAction(1, 'fork', false)
  flushSync();
}

// openProjectSelectorRow opens the project selector and clicks the row for path. Returns once ProjectSwitch(path) is in flight.
async function clickProjectPath(targetEl, path) {
  [...targetEl.querySelectorAll('.label.project')][0].click(); // toolbar label — App opens it only once hydrated
  await settle();
  const rows = [...targetEl.querySelectorAll('div.row')];
  const row = rows.find((r) => r.querySelector('.path') && r.querySelector('.path').textContent === path);
  if (!row) throw new Error(`no project row for ${path}`);
  row.click(); // pick(path) — ProjectSwitch goes out and stays pending
  flushSync();
}

let App;
beforeAll(async () => { ({ default: App } = await import('./App.svelte')); });

function openSessionSelector(targetEl) {
  const label = [...targetEl.querySelectorAll('.label.session')][0];
  if (!label) throw new Error('no session label in the toolbar');
  label.click(); // dispatches openSessionSelector — App opens it only once hydrated, which mountApp has settled past
}

function countVisible(targetEl, text) {
  return targetEl.textContent.split(text).length - 1;
}

describe('committed lifecycle settlement over a mounted App', () => {
  it('rejection-first: the transient catch is sole until its pair arrives, then exactly one final error remains — the backend frame\'s', async () => {
    const m = await mountApp();
    openSessionSelector(m.target);
    flushSync();
    await tick();

    // click "+" in the opened selector — the create call goes out and stays pending.
    [...m.target.querySelectorAll('.menu .new')].find((b) => b.textContent === '+').click();
    expect(typeof m.rejectNew()).toBe('function'); // in flight: the deferred rejection is live now

    // Rejection settles before any frame lands: while unseeded-of-frames, this catch is sole.
    m.rejectNew()(new Error('create failed'));
    flushSync();
    await new Promise((r) => setTimeout(r, 0));
    await tick();
    expect(countVisible(m.target, 'create failed')).toBe(1); // exactly one visible error at this moment — the transient catch

    // The committed pair arrives: boundary first (its stateful snapshot replaces the transcript wholesale), then its unsequenced error.
    fire('navigation', navState('b'));
    flushSync();
    await new Promise((r) => setTimeout(r, 0));
    await tick();
    expect(countVisible(m.target, 'create failed')).toBe(0); // the snapshot replacement wiped the transient catch — it is gone from the view

    fire('error', { message: 'commit failed' });
    flushSync();
    await new Promise((r) => setTimeout(r, 0));
    await tick();

    expect(countVisible(m.target, 'create failed')).toBe(0); // still no transient — nothing resurrects it after the pair lands
    expect(countVisible(m.target, 'commit failed')).toBe(1); // exactly one visible final error: the backend frame's own text
    unmount(m.app);
  });

  it('frame-first: a boundary applied before the rejection settles suppresses the stale catch — again exactly one visible error', async () => {
    const m = await mountApp();
    openSessionSelector(m.target);
    flushSync();
    await tick();

    [...m.target.querySelectorAll('.menu .new')].find((b) => b.textContent === '+').click(); // create in flight
    expect(typeof m.rejectNew()).toBe('function');

    // The committed pair lands before the rejection settles: applying its snapshot advances the presentation generation the selector captured against.
    fire('navigation', navState('b'));
    flushSync();
    await new Promise((r) => setTimeout(r, 0));
    await tick();
    fire('error', { message: 'commit failed' });
    flushSync();
    await new Promise((r) => setTimeout(r, 0));
    await tick();

    // Now the rejection settles — against a generation that already moved on. Its catch must stay silent; only the pair's error is visible.
    m.rejectNew()(new Error('create failed'));
    flushSync();
    await new Promise((r) => setTimeout(r, 0));
    await tick();

    expect(countVisible(m.target, 'commit failed')).toBe(1); // exactly one visible final error: the pair's own text
    expect(countVisible(m.target, 'create failed')).toBe(0); // no transient catch over a presentation that already moved on without it
    unmount(m.app);
  });

  afterAll(() => { document.body.innerHTML = ''; });
});

// The same settlement contract for the other two committed lifecycle routes: fork's pair rides an ordered turn_action boundary (its state replaces the transcript wholesale), and ProjectSwitch fallback's rides a navigation boundary. Both settle to exactly one visible final error under either schedule — the handler's transient catch or its own paired frame, never both.
describe('committed ProjectSwitch/fork settlement over a mounted App', () => {
  it('fork rejection-first: handleFork\'s transient is sole until its turn_action pair arrives, then exactly one final error remains', async () => {
    const m = await mountApp();
    await confirmFork(m.target);
    expect(typeof m.rejectFork()).toBe('function'); // in flight

    m.rejectFork()(new Error('fork failed')); // settles before any frame lands: the catch is sole at that moment
    await settle();
    expect(countVisible(m.target, 'fork failed')).toBe(1); // exactly one visible error — handleFork's transient

    fire('turn_action', { state: navState('b'), skippedFiles: [] }); // the pair's boundary first: its snapshot replaces the transcript wholesale (wiping the transient)
    await settle();
    expect(countVisible(m.target, 'fork failed')).toBe(0); // gone from the view — nothing resurrects it after the pair lands

    fire('error', { message: 'commit failed' });
    await settle();
    expect(countVisible(m.target, 'fork failed')).toBe(0);
    expect(countVisible(m.target, 'commit failed')).toBe(1); // exactly one visible final error: the backend frame's own text
    unmount(m.app);
  });

  it('fork frame-first: a turn_action boundary applied before the rejection settles suppresses handleFork\'s stale catch — again exactly one visible error', async () => {
    const m = await mountApp();
    await confirmFork(m.target);
    expect(typeof m.rejectFork()).toBe('function');

    fire('turn_action', { state: navState('b'), skippedFiles: [] }); // the pair lands before the rejection settles: session and generation both move on without it
    await settle();
    fire('error', { message: 'commit failed' });
    await settle();
    expect(countVisible(m.target, 'commit failed')).toBe(1);

    m.rejectFork()(new Error('fork failed')); // now settles against a presentation that already moved on — its stale guard must stay silent
    await settle();
    expect(countVisible(m.target, 'commit failed')).toBe(1); // exactly one visible final error: the pair's own text
    expect(countVisible(m.target, 'fork failed')).toBe(0); // no transient over a presentation that already moved on without it
    unmount(m.app);
  });

  it('project switch rejection-first: ProjectSelector\'s transient is sole until its navigation pair arrives, then exactly one final error remains', async () => {
    const m = await mountApp();
    await clickProjectPath(m.target, '/p/beta');
    expect(typeof m.rejectSwitch()).toBe('function'); // in flight

    m.rejectSwitch()(new Error('switch failed')); // same generation at settle time: nothing newer owns presentation yet — this catch is sole
    await settle();
    expect(countVisible(m.target, 'switch failed')).toBe(1); // exactly one visible error — the transient

    fire('navigation', navState('b')); // the pair's boundary first: its snapshot replaces the transcript wholesale (wiping the transient)
    await settle();
    expect(countVisible(m.target, 'switch failed')).toBe(0); // gone from the view — nothing resurrects it after the pair lands

    fire('error', { message: 'commit failed' });
    await settle();
    expect(countVisible(m.target, 'switch failed')).toBe(0);
    expect(countVisible(m.target, 'commit failed')).toBe(1); // exactly one visible final error: the backend frame's own text
    unmount(m.app);
  });

  it('project switch frame-first: a navigation boundary applied before the rejection settles suppresses ProjectSelector\'s stale catch — again exactly one visible error', async () => {
    const m = await mountApp();
    await clickProjectPath(m.target, '/p/beta');
    expect(typeof m.rejectSwitch()).toBe('function');

    fire('navigation', navState('b')); // the pair lands before the rejection settles: session and generation both move on without it
    await settle();
    fire('error', { message: 'commit failed' });
    await settle();
    expect(countVisible(m.target, 'commit failed')).toBe(1);

    m.rejectSwitch()(new Error('switch failed')); // now settles against a presentation that already moved on — its catch must stay silent
    await settle();
    expect(countVisible(m.target, 'commit failed')).toBe(1); // exactly one visible final error: the pair's own text
    expect(countVisible(m.target, 'switch failed')).toBe(0); // no transient over a presentation that already moved on without it
    unmount(m.app);
  });

  // The same three committed schedules with an UNSEEDED view at capture time (failed startup hydration): the selector captures mutSeeded=false and generation=0 exactly as production presents them, and the 'error' event stays gated out until a stateful boundary seeds — so each row settles through the pair's own admission order (boundary first, its error admitted only after it seeded), never through any transient that could survive seeding.
  function expectUnseeded(targetEl) {
    // The failed startup hydration's notice is visible: snapshotApplied is false and presentationGeneration has not advanced — the committed rows below really run against an unseeded capture, or they silently collapse into their seeded-view siblings above.
    expect(countVisible(targetEl, 'Load session failed')).toBe(1);
  }

  it('unseeded SessionNew rejection-first: sole transient until its pair seeds and wipes; exactly one final error', async () => {
    const m = await mountApp({ unseeded: true });
    openSessionSelector(m.target); // opens even though the view never seeded — only hydrated gates it
    flushSync();
    expectUnseeded(m.target);

    [...m.target.querySelectorAll('.menu .new')].find((b) => b.textContent === '+').click(); // create in flight, captured unseeded at generation 0
    m.rejectNew()(new Error('create failed')); // settles before any frame lands: while nothing has seeded the view this catch is sole — no backend error could be visible yet (the 'error' gate drops it until a snapshot seeds)
    await settle();
    expect(countVisible(m.target, 'create failed')).toBe(1);

    fire('navigation', navState('b')); // the pair's boundary: its stateful snapshot SEEDS the view and replaces messages wholesale — wiping the transient
    await settle();
    expect(countVisible(m.target, 'Load session failed')).toBe(0); // seeding replaced the whole transcript, failed-hydration notice included
    fire('error', { message: 'commit failed' }); // now admitted: snapshotApplied is true again by the time this frame drains (FIFO put its boundary first)
    await settle();

    expect(countVisible(m.target, 'create failed')).toBe(0);
    expect(countVisible(m.target, 'commit failed')).toBe(1); // exactly one visible final error: the backend frame's own text
    unmount(m.app);
  });

  it('unseeded SessionNew frame-first: pair seeds before the rejection settles; stale catch suppressed — again exactly one visible error', async () => {
    const m = await mountApp({ unseeded: true });
    openSessionSelector(m.target);
    flushSync();
    expectUnseeded(m.target);

    [...m.target.querySelectorAll('.menu .new')].find((b) => b.textContent === '+').click(); // create in flight, captured at generation 0
    fire('navigation', navState('b')); // the pair lands first: seeding advances presentationGeneration past the capture (and replaces nothing yet but arms the gate for its error frame)
    await settle();
    fire('error', { message: 'commit failed' });
    await settle();
    expect(countVisible(m.target, 'commit failed')).toBe(1);

    m.rejectNew()(new Error('create failed')); // settles against a generation that already moved on — its catch must stay silent even though the capture was unseeded (settleNavigation gates on generation alone)
    await settle();
    expect(countVisible(m.target, 'commit failed')).toBe(1); // exactly one visible final error: the pair's own text
    expect(countVisible(m.target, 'create failed')).toBe(0);
    unmount(m.app);
  });

  it('unseeded ProjectSwitch rejection-first: sole transient until its pair seeds and wipes; exactly one final error', async () => {
    const m = await mountApp({ unseeded: true });
    await clickProjectPath(m.target, '/p/beta'); // captured unseeded at generation 0
    expectUnseeded(m.target);

    m.rejectSwitch()(new Error('switch failed')); // same-generation rejection while nothing has seeded the view — this catch is sole (settleSwitch gates on generation+session; no frame could be visible yet)
    await settle();
    expect(countVisible(m.target, 'switch failed')).toBe(1);

    fire('navigation', navState('b')); // the pair's boundary seeds and replaces messages wholesale — wiping the transient
    await settle();
    expect(countVisible(m.target, 'Load session failed')).toBe(0);
    fire('error', { message: 'commit failed' });
    await settle();

    expect(countVisible(m.target, 'switch failed')).toBe(0);
    expect(countVisible(m.target, 'commit failed')).toBe(1); // exactly one visible final error: the backend frame's own text
    unmount(m.app);
  });

  it('unseeded ProjectSwitch frame-first: pair seeds before the rejection settles; stale catch suppressed — again exactly one visible error', async () => {
    const m = await mountApp({ unseeded: true });
    await clickProjectPath(m.target, '/p/beta'); // captured at generation 0

    fire('navigation', navState('b')); // the pair lands first: seeding advances both session and presentationGeneration past the capture
    await settle();
    fire('error', { message: 'commit failed' });
    await settle();
    expect(countVisible(m.target, 'commit failed')).toBe(1);

    m.rejectSwitch()(new Error('switch failed')); // settles against a moved-on presentation — its catch must stay silent
    await settle();
    expect(countVisible(m.target, 'commit failed')).toBe(1); // exactly one visible final error: the pair's own text
    expect(countVisible(m.target, 'switch failed')).toBe(0);
    unmount(m.app);
  });

  it('unseeded fork rejection-first: sole transient until its turn_action pair seeds and wipes; exactly one final error', async () => {
    const m = await mountApp({ unseeded: true });
    fire('user_message', { content: 'seed turn', turn: 1 }); // a user message appends even before any snapshot seeded the view — its revert menu is the GUI's fork affordance, so an unseeded capture can reach it deterministically
    await settle();
    expectUnseeded(m.target);

    await confirmFork(m.target); // captured at generation 0 with mutSeeded=false semantics: handleFork gates on session+generation only
    m.rejectFork()(new Error('fork failed')); // settles before any frame lands — sole while unseeded (no error could render yet)
    await settle();
    expect(countVisible(m.target, 'fork failed')).toBe(1);

    fire('turn_action', { state: navState('b'), skippedFiles: [] }); // the pair's boundary seeds and replaces messages wholesale — wiping the transient
    await settle();
    expect(countVisible(m.target, 'Load session failed')).toBe(0);
    fire('error', { message: 'commit failed' });
    await settle();

    expect(countVisible(m.target, 'fork failed')).toBe(0);
    expect(countVisible(m.target, 'commit failed')).toBe(1); // exactly one visible final error: the backend frame's own text
    unmount(m.app);
  });

  it('unseeded fork frame-first: pair seeds before the rejection settles; stale catch suppressed — again exactly one visible error', async () => {
    const m = await mountApp({ unseeded: true });
    fire('user_message', { content: 'seed turn', turn: 1 }); // same affordance seeding as above
    await settle();

    await confirmFork(m.target); // captured at generation 0
    fire('turn_action', { state: navState('b'), skippedFiles: [] }); // the pair lands first: session and presentationGeneration both move on without this rejection
    await settle();
    fire('error', { message: 'commit failed' });
    await settle();
    expect(countVisible(m.target, 'commit failed')).toBe(1);

    m.rejectFork()(new Error('fork failed')); // settles against a moved-on presentation — handleFork's stale guard must stay silent
    await settle();
    expect(countVisible(m.target, 'commit failed')).toBe(1); // exactly one visible final error: the pair's own text
    expect(countVisible(m.target, 'fork failed')).toBe(0);
    unmount(m.app);
  });

  afterAll(() => { document.body.innerHTML = ''; });
});
