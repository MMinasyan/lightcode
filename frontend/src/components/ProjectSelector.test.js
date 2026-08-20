// @vitest-environment happy-dom
import { describe, expect, it } from 'vitest';
import { flushSync, mount, tick, unmount } from 'svelte';
import { writable } from 'svelte/store';

// The project selector's post-switch settlement is the frontend half of the committed-outcome contract for ProjectSwitch: only ever one surface may show a given failure — this component's catch or the backend frames it owns. These tests mount the real component (through its test harness, since Svelte 5 removed $set and a live legacy component's props move only when its parent re-rendered) and drive its production handlers through rejected switches:
// - same-generation plain rejections stay sole — no backend frame exists for a precommit switch;
// - a generation advanced between call and settle suppresses the stale catch (A->B), as does a changed presented session at any generation — committed fallbacks carry boundary+error pairs whose stateful snapshot owns presentation either way;
// - every error closes, while an ordinary load failure stays visible exactly as before with no close;
// - SessionCurrent is never called: project identity settles through the ordered navigation boundaries alone.

let ProjectSelectorHarness;

beforeEach(async () => {
  ({ default: ProjectSelectorHarness } = await import('./ProjectSelectorTestHarness.svelte'));
});

const calls = [];
function makeBackend() {
  const overrides = {};
  for (const name of ['ProjectList', 'ProjectSwitch', 'ProjectPickAndSwitch', 'ProjectCurrent']) {
    overrides[name] = (...args) => { throw new Error(`unstubbed binding ${name} was called`); };
  }
  const api = {};
  for (const name of Object.keys(overrides)) {
    api[name] = (...args) => { calls.push({ name, args }); return overrides[name](...args); };
  }
  // SessionCurrent is installed on the binding surface to fail loudly if reached: this component must never call it.
  api.SessionCurrent = () => { calls.push({ name: 'SessionCurrent' }); throw new Error('ProjectSelector must not read SessionCurrent'); };
  window.go = { main: { App: api } }; // wailsjs bindings resolve through window['go']['main']['App'][name]
  return overrides; // tests assign their prepared outcomes here (the api wrapper records the call before delegating)
}

function mountSelector() {
  calls.length = 0;
  const genStore = writable(0);
  const sessStore = writable('s1'); // a presented session exists (seeded view) — its movement is part of the gate under test
  const seen = [];
  const target = document.createElement('div');
  document.body.appendChild(target);
  const comp = mount(ProjectSelectorHarness, {
    target,
    props: { genStore, sessStore },
    events: { close: () => seen.push('close'), error: (e) => seen.push(['error', e.detail]) },
  });
  return { comp, target, seen, genStore, sessStore };
}

async function settle() {
  flushSync();
  await new Promise((r) => setTimeout(r, 0));
  await tick();
}

function clickProject(targetEl, nameText) {
  const els = [...targetEl.querySelectorAll('.row')];
  const row = els.find((r) => r.querySelector('.name').textContent === nameText);
  if (!row) throw new Error(`no rendered project row for ${nameText}`);
  return row;
}

describe('ProjectSelector committed-outcome settlement', () => {
  it('a same-generation plain switch rejection stays sole and closes the selector', async () => {
    const backend = makeBackend();
    backend.ProjectList = async () => [{ name: 'alpha', path: '/p/alpha' }, { name: 'beta', path: '/p/beta' }];
    backend.ProjectCurrent = async () => ({ path: '/p/alpha' });
    let rejectSwitch = null; // set to the pending call's rejection once it is in flight (the api wrapper records the call itself)
    backend.ProjectSwitch = () => new Promise((_, rj) => { rejectSwitch = rj; });

    const m = mountSelector();
    await settle();

    clickProject(m.target, 'beta').click(); // pick — the switch is in flight now
    expect(rejectSwitch).toBeTruthy();
    rejectSwitch(new Error('switch failed')); // same generation and presented session at settle time: nothing newer owns presentation
    await settle();

    const errs = m.seen.filter((e) => Array.isArray(e));
    expect(errs).toHaveLength(1); // sole visible error — a plain precommit switch carries no backend frame
    expect(m.seen.filter((e) => e === 'close')).toHaveLength(1);

    unmount(m.comp);
  });

  it('a rejected switch whose generation advanced while in flight is suppressed as stale', async () => {
    const backend = makeBackend();
    backend.ProjectList = async () => [{ name: 'alpha', path: '/p/alpha' }, { name: 'beta', path: '/p/beta' }];
    backend.ProjectCurrent = async () => ({ path: '/p/alpha' });
    let rejectSwitch = null; // set to the pending call's rejection once it is in flight (the api wrapper records the call itself)
    backend.ProjectSwitch = () => new Promise((_, rj) => { rejectSwitch = rj; });

    const m = mountSelector();
    await settle();

    clickProject(m.target, 'beta').click(); // pick — the switch is in flight now
    expect(rejectSwitch).toBeTruthy();
    m.genStore.set(1); // a boundary applied while it was in flight (A->B): presentation moved on without this rejection settling first
    flushSync();
    await tick();
    rejectSwitch(new Error('switch failed'));
    await settle();

    expect(m.seen.filter((e) => e === 'close')).toHaveLength(1); // still closed on the error
    expect(m.seen.filter((e) => Array.isArray(e))).toHaveLength(0); // stale: its frames (or newer navigation's snapshot) own whatever this failure was about

    unmount(m.comp);
  });

  it('a rejected switch whose presented session changed at unchanged generation is suppressed', async () => {
    const backend = makeBackend();
    backend.ProjectList = async () => [{ name: 'alpha', path: '/p/alpha' }, { name: 'beta', path: '/p/beta' }];
    backend.ProjectCurrent = async () => ({ path: '/p/alpha' });
    let rejectSwitch = null; // set to the pending call's rejection once it is in flight (the api wrapper records the call itself)
    backend.ProjectSwitch = () => new Promise((_, rj) => { rejectSwitch = rj; });

    const m = mountSelector(); // presented session s1 at capture time
    await settle();

    clickProject(m.target, 'beta').click(); // pick — the switch is in flight now
    expect(rejectSwitch).toBeTruthy();
    m.sessStore.set('s2'); // a navigation moved the presented session without this rejection settling first (same generation: nil-advance shapes are exactly why both axes gate)
    flushSync();
    await tick();
    rejectSwitch(new Error('switch failed'));
    await settle();

    expect(m.seen.filter((e) => e === 'close')).toHaveLength(1); // still closed on the error
    expect(m.seen.filter((e) => Array.isArray(e))).toHaveLength(0); // suppressed: a different session now owns presentation, so this catch must not surface over it

    unmount(m.comp);
  });

  it('an ordinary load failure stays visible exactly as before and does not close', async () => {
    const backend = makeBackend();
    backend.ProjectList = async () => { throw new Error('list failed'); }; // no mutation ran: its catch is the plain load surface, untouched by settlement gating

    const m = mountSelector();
    await settle();

    const errs = m.seen.filter((e) => Array.isArray(e));
    expect(errs).toHaveLength(1);
    expect(m.seen.filter((e) => e === 'close')).toHaveLength(0); // the selector stays open for a retry — load failure is not a mutation outcome

    unmount(m.comp);
  });

  it('never reads SessionCurrent across mount, rejection settlement, or success', async () => {
    let sessionCurrentCalls = 0; // the binding spy increments this and throws — any reach is unmistakable

    const backend = makeBackend(); // installs the recorded binding surface, including a throwing SessionCurrent spy
    window.go.main.App.SessionCurrent = () => { calls.push({ name: 'SessionCurrent' }); sessionCurrentCalls++; throw new Error('ProjectSelector must not read SessionCurrent'); };

    backend.ProjectList = async () => [{ name: 'alpha', path: '/p/alpha' }, { name: 'beta', path: '/p/beta' }];
    backend.ProjectCurrent = async () => ({ path: '/p/alpha' });

    // rejected switch first…
    let rejectSwitch = null; // set to the pending call's rejection once it is in flight (the api wrapper records the call itself)
    backend.ProjectSwitch = () => new Promise((_, rj) => { rejectSwitch = rj; });

    const m = mountSelector();
    await settle();
    clickProject(m.target, 'beta').click(); // pick — in flight now
    expect(rejectSwitch).toBeTruthy();
    m.genStore.set(1); // stale: suppressed, and no identity read may substitute for the missing frame
    flushSync();
    await tick();
    rejectSwitch(new Error('switch failed'));
    await settle();
    unmount(m.comp);

    // …and a successful one on a fresh view. The SessionCurrent spy stays live across both mounts so any reach in either phase is caught.
    const backend2 = makeBackend();
    window.go.main.App.SessionCurrent = () => { calls.push({ name: 'SessionCurrent' }); sessionCurrentCalls++; throw new Error('ProjectSelector must not read SessionCurrent'); };
    backend2.ProjectList = async () => [{ name: 'alpha', path: '/p/alpha' }, { name: 'beta', path: '/p/beta' }];
    backend2.ProjectCurrent = async () => ({ path: '/p/alpha' });
    backend2.ProjectSwitch = async (path) => {}; // success

    const m2 = mountSelector();
    await settle();
    clickProject(m2.target, 'beta').click(); // pick — succeeds and closes with the switched event
    await settle();
    unmount(m2.comp);

    expect(sessionCurrentCalls).toBe(0); // project identity settles through ordered navigation boundaries alone; an out-of-band session lookup could resurrect a route a concurrent switch already left
  });

  it('overlapping actions settle against their own captures: A cannot surface its stale error after B captured over the presentation, while B keeps its own settlement', async () => {
    const backend = makeBackend();
    backend.ProjectList = async () => [{ name: 'alpha', path: '/p/alpha' }, { name: 'beta', path: '/p/beta' }];
    backend.ProjectCurrent = async () => ({ path: '/p/alpha' }); // presented project alpha; the view presents session s1 at generation 0 (the harness defaults)
    let rejectSwitchA = null; // A's pending row-switch rejection once in flight
    backend.ProjectSwitch = (path) => { if (path !== '/p/beta') throw new Error(`unstubbed switch to ${path}`); return new Promise((_, rj) => { rejectSwitchA = rj; }); };
    let rejectOpenB = null; // B's pending open-directory rejection once in flight
    backend.ProjectPickAndSwitch = () => new Promise((_, rj) => { rejectOpenB = rj; });

    const m = mountSelector();
    await settle();

    clickProject(m.target, 'beta').click(); // A: pick /p/beta at generation 0 with presented session s1 — its capture is {gen 0, sess s1} and stays with this action alone
    expect(typeof rejectSwitchA).toBe('function'); // in flight before anything else happens

    m.genStore.set(1);   // a boundary applied while A was still pending (A->B): presentation moved on without it...
    m.sessStore.set('s2'); // ...and the presented session with it — both past what A captured
    flushSync();
    await tick();

    [...m.target.querySelectorAll('.new')].find((b) => b.textContent === '+').click(); // B: open directory — captured at the MOVED {gen 1, sess s2}; under a shared global this is also where it overwrote whatever A had recorded
    expect(typeof rejectOpenB).toBe('function');

    rejectSwitchA(new Error('switch failed')); // A settles first against ITS OWN capture {0, s1} versus live presentation now at {1, s2} — stale: the boundary owns presentation for both gate terms, so no error may surface even though B's call just ran
    await settle();
    const errsAfterA = m.seen.filter((e) => Array.isArray(e));
    expect(errsAfterA).toHaveLength(0); // action A cannot surface a stale error after the presentation it captured against has been overwritten

    rejectOpenB(new Error('open project failed')); // B settles against ITS OWN capture {1, s2} versus live {1, s2} — same generation and session: nothing newer owns presentation for this failure
    await settle();

    const errs = m.seen.filter((e) => Array.isArray(e));
    expect(errs).toHaveLength(1); // exactly one visible error across both actions — A's stale catch never surfaced, B's own did
    expect(errs[0]).toEqual(['error', 'open project failed']); // action B retains its correct same-generation settlement: sole visible error carrying its own rejection text
    expect(m.seen.filter((e) => e === 'close')).toHaveLength(2); // both settlements closed on error, as every rejected switch does

    unmount(m.comp);
  });
});
