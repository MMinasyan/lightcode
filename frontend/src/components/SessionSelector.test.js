// @vitest-environment happy-dom
import { describe, expect, it, beforeEach } from 'vitest';
import { flushSync, mount, tick, unmount } from 'svelte';
import { writable } from 'svelte/store';

// The session selector's post-mutation settlement is the frontend half of the committed-outcome contract: only ever one surface may show a given failure — this component's catch or the backend frames it owns. These tests mount the real component (through its test harness, since Svelte 5 removed $set and a live legacy component's props move only when its parent re-renders) and drive its production handlers through rejected and successful archive/delete/open/new calls:
// - an unseeded plain rejection stays sole — no backend frame is visible until some snapshot seeds the view;
// - the one mandated SessionCurrent read fires only for a rejected removal of the presented session in an unseeded view, distinguishing precommit (target still current — row kept) from committed detach/newer navigation (row removed locally, its frames own the error);
// - seeded views suppress every removal catch: App shows that path's own unsequenced frame;
// - a generation advanced between call and settle suppresses stale catches in both components (A->B), while same-generation plain rejections remain visible as sole;
// - successful rows leave the local list without any reload, and every error closes.

let SessionSelectorHarness;

beforeEach(async () => {
  ({ default: SessionSelectorHarness } = await import('./SessionSelectorTestHarness.svelte'));
});

const calls = [];
// makeBackend installs the recorded wailsjs binding surface every test drives. A call to a name with no override fails loudly instead of silently succeeding — nothing here may reach an owner this component does not own.
function makeBackend() {
  const overrides = {};
  for (const name of ['SessionList', 'SessionCurrent', 'SessionSwitch', 'SessionNew', 'SessionArchive', 'SessionDelete']) {
    overrides[name] = (...args) => { throw new Error(`unstubbed binding ${name} was called`); };
  }
  const api = {};
  for (const name of Object.keys(overrides)) {
    api[name] = (...args) => { calls.push({ name, args }); return overrides[name](...args); };
  }
  window.go = { main: { App: api } };
  return overrides; // tests assign their prepared outcomes here
}

// mountSelector mounts the harness with a fresh generation store and recorded events.
function mountSelector({ seeded = false, currentSessionId = '' } = {}) {
  calls.length = 0;
  const genStore = writable(0);
  const seen = [];
  const target = document.createElement('div');
  document.body.appendChild(target);
  const comp = mount(SessionSelectorHarness, {
    target,
    props: { genStore, seeded, currentSessionId },
    events: { close: () => seen.push('close'), error: (e) => seen.push(['error', e.detail]) },
  });
  return { comp, target, seen, genStore };
}

async function settle() {
  flushSync();
  await new Promise((r) => setTimeout(r, 0));
  await tick();
}

function clickRow(targetEl, rowId) {
  const els = [...targetEl.querySelectorAll('.row')];
  const row = els.find((r) => r.querySelector('.id').textContent === rowId);
  if (!row) throw new Error(`no rendered row for ${rowId}`);
  return row;
}

function clickAction(targetEl, rowId) {
  clickRow(targetEl, rowId).querySelector('.act').click();
}

const rows = [
  { id: 'a', lastActivity: 100 },
  { id: 'b', lastActivity: 200 },
];

describe('SessionSelector committed-outcome settlement', () => {
  it('unseeded rejected current archive that stays current keeps the row and keeps this catch sole, with exactly one post-rejection read', async () => {
    const backend = makeBackend();
    backend.SessionList = async () => rows;
    let reads = 0;
    // Load-time identity plus exactly one more — the mandated distinction. The target is still
    // current after rejection: a precommit failure whose row must stay, and this catch stays sole.
    backend.SessionCurrent = async () => { reads++; return { id: 'a' }; };
    let rejectArchive = null; // set to the pending call's rejection once it is in flight
    backend.SessionArchive = () => new Promise((_, rj) => { rejectArchive = rj; });

    const m = mountSelector({ seeded: false, currentSessionId: '' });
    await settle();
    expect(reads).toBe(1); // load read only — a steady-state selector re-resolves identity nowhere else yet

    clickAction(m.target, 'a');
    rejectArchive(new Error('archive failed')); // in flight: the rejection settles now
    await settle();

    expect(m.seen.filter((e) => e === 'close')).toHaveLength(1); // closed on the error
    const errs = m.seen.filter((e) => Array.isArray(e));
    expect(errs).toHaveLength(1); // exactly one visible error — no frame owns it while unseeded
    expect(reads).toBe(2); // load + the single mandated post-rejection read, nothing more
    expect(clickRow(m.target, 'a')).toBeTruthy(); // precommit: target still current, its row stays

    unmount(m.comp);
  });

  it('unseeded rejected removal of the presented session that already left removes the row locally and lets its frames own the error', async () => {
    const backend = makeBackend();
    backend.SessionList = async () => rows;
    let reads = 0;
    // The one load read reports 'a' as current. After rejection, settleRemoval performs exactly one more — the mandated distinction — and it finds the target gone: a committed detach (or newer navigation) ran durably, so its frames own this error once they land. No dispatch here, and the row leaves the local list on the spot.
    let postRejection = false;
    backend.SessionCurrent = async () => { reads++; return postRejection ? { id: '' } : { id: 'a' }; };
    let rejectArchive = null; // set to the pending call's rejection once it is in flight
    backend.SessionArchive = () => new Promise((_, rj) => { rejectArchive = rj; });

    const m = mountSelector({ seeded: false, currentSessionId: '' });
    await settle();
    expect(reads).toBe(1); // the single load read — nothing else re-resolves identity in an unseeded selector until a mutation settles

    clickAction(m.target, 'a'); // remove the presented session; capture runs with mutCurrentId = 'a'
    postRejection = true;       // from here on the owner reports it gone (committed detach / newer navigation already moved)
    rejectArchive(new Error('archive committed but failed')); // in flight: the rejection settles now
    await settle();

    expect(m.seen.filter((e) => e === 'close')).toHaveLength(1); // closed on the error
    expect(m.seen.filter((e) => Array.isArray(e))).toHaveLength(0); // suppressed — its frames own this one once they land
    expect(reads).toBe(2); // load + exactly one post-rejection read: precommit vs committed, nothing more
    const row = [...m.target.querySelectorAll('.row')].find((r) => r.querySelector('.id').textContent === 'a');
    expect(row).toBeFalsy(); // the removal ran durably — reconcile the local list to it

    unmount(m.comp);
  });

  it('unseeded rejected noncurrent archive performs no post-rejection read and keeps this catch sole', async () => {
    const backend = makeBackend();
    backend.SessionList = async () => rows;
    let reads = 0;
    backend.SessionCurrent = async () => { reads++; return { id: 'a' }; }; // presented session is a, not the target b
    let rejectArchive = null; // set to the pending call's rejection once it is in flight
    backend.SessionArchive = () => new Promise((_, rj) => { rejectArchive = rj; });

    const m = mountSelector({ seeded: false, currentSessionId: '' });
    await settle();

    clickAction(m.target, 'b'); // a noncurrent target: no mandated read may fire for it
    rejectArchive(new Error('archive failed'));
    await settle();

    expect(reads).toBe(1); // load only — the distinction is required solely for rejected removal of the presented session
    const errs = m.seen.filter((e) => Array.isArray(e));
    expect(errs).toHaveLength(1); // gated catch stays sole while unseeded
    expect(m.seen.filter((e) => e === 'close')).toHaveLength(1);

    unmount(m.comp);
  });

  it('seeded rejected archive suppresses the catch — App shows this path\'s own frame — and never re-resolves identity', async () => {
    const backend = makeBackend();
    backend.SessionList = async () => rows;
    let reads = 0;
    backend.SessionCurrent = async () => { reads++; return { id: 'a' }; }; // must stay uncalled in a seeded view, at mount or post-mutation
    let rejectArchive = null; // set to the pending call's rejection once it is in flight
    backend.SessionArchive = () => new Promise((_, rj) => { rejectArchive = rj; });

    const m = mountSelector({ seeded: true, currentSessionId: 'a' }); // presented session comes from the prop — no owner read at open either
    await settle();
    expect(reads).toBe(0);

    clickAction(m.target, 'b');
    rejectArchive(new Error('archive committed but failed')); // in flight: the rejection settles now
    await settle();

    expect(m.seen.filter((e) => e === 'close')).toHaveLength(1); // still closed on the error
    expect(m.seen.filter((e) => Array.isArray(e))).toHaveLength(0); // suppressed: App shows this path's unsequenced frame (plain or committed alike — noncurrent ones are generation-neutral, so only the seed gate can tell them apart from a stale catch)
    expect(reads).toBe(0);

    unmount(m.comp);
  });

  it('a rejected open whose generation advanced while in flight is suppressed as stale', async () => {
    const backend = makeBackend();
    backend.SessionList = async () => rows;
    backend.SessionCurrent = async () => ({ id: '' });
    let rejectSwitch = null; // set to the pending call's rejection once it is in flight
    backend.SessionSwitch = () => new Promise((_, rj) => { rejectSwitch = rj; });

    const m = mountSelector({ seeded: false, currentSessionId: '' });
    await settle();

    clickRow(m.target, 'b').click(); // pick b — the call is in flight
    m.genStore.set(1); // a boundary applied while it was in flight (A->B): presentation moved on without this rejection
    flushSync();
    await tick();
    rejectSwitch(new Error('switch failed'));
    await settle();

    expect(m.seen.filter((e) => e === 'close')).toHaveLength(1); // still closed on the error
    expect(m.seen.filter((e) => Array.isArray(e))).toHaveLength(0); // stale: the newer presentation owns whatever this failure was about

    unmount(m.comp);
  });

  it('a same-generation plain new rejection stays sole — no backend frame exists for a precommit create', async () => {
    const backend = makeBackend();
    backend.SessionList = async () => rows;
    backend.SessionCurrent = async () => ({ id: '' });
    let rejectNew = null; // set to the pending call's rejection once it is in flight
    backend.SessionNew = () => new Promise((_, rj) => { rejectNew = rj; });

    const m = mountSelector({ seeded: false, currentSessionId: '' });
    await settle();

    [...m.target.querySelectorAll('.new')].find((b) => b.textContent === '+').click(); // the "+" button creates a session
    rejectNew(new Error('create failed')); // same generation at settle time: nothing newer owns presentation
    await settle();

    const errs = m.seen.filter((e) => Array.isArray(e));
    expect(errs).toHaveLength(1); // same generation: nothing newer owns presentation, so this catch is sole — and it carries the rejection text as-is
    expect(errs[0]).toEqual(['error', 'create failed']);
    expect(m.seen.filter((e) => e === 'close')).toHaveLength(1);

    unmount(m.comp);
  });

  it('a successful delete removes the row locally, never reloads the list, and leaves the selector open', async () => {
    let lists = 0;
    const backend = makeBackend();
    backend.SessionList = async (state) => { lists++; return state === 'archived' ? rows : []; };
    backend.SessionCurrent = async () => ({ id: '' }); // no presented session in this scenario — the target is plainly noncurrent
    backend.SessionDelete = async (id) => {}; // success — the call itself is already recorded by the harness surface

    const m = mountSelector({ seeded: false, currentSessionId: '' });
    await settle();
    [...m.target.querySelectorAll('.tab')].find((b) => b.textContent === 'Archived').click(); // delete affordance lives in the archived view
    await settle();
    expect(lists).toBe(2); // active + archived — both at open, before any mutation

    clickAction(m.target, 'b'); // remove() → SessionDelete('b') succeeds
    await settle();

    expect(lists).toBe(2); // never re-listed after a mutation: every other outcome is owned by backend frames, and a reload could race them or resurrect a row a boundary already removed
    const row = [...m.target.querySelectorAll('.row')].find((r) => r.querySelector('.id').textContent === 'b');
    expect(row).toBeFalsy(); // the successful removal leaves the local list on the spot
    expect(clickRow(m.target, 'a')).toBeTruthy(); // untouched rows stay
    expect(calls.filter((c) => c.name === 'SessionDelete').length).toBe(1);
    expect(m.seen.filter((e) => e === 'close')).toHaveLength(0); // success keeps the selector open

    unmount(m.comp);
  });

  it('unseeded rejected current archive whose one mandated distinction read fails retains this same-generation catch — no second read, nothing else', async () => {
    const backend = makeBackend();
    backend.SessionList = async () => rows;
    let reads = 0;
    // The load read reports 'a' as current. After rejection the single mandated distinction read is attempted once and fails: the failure of that very read must not swallow this same-generation error — it stays sole, shown from lastErr exactly as captured before any call went out. No retry exists to count here: reads stops at two either way.
    backend.SessionCurrent = async () => { reads++; if (reads === 2) throw new Error('owner read failed'); return { id: 'a' }; };
    let rejectArchive = null; // set to the pending call's rejection once it is in flight
    backend.SessionArchive = () => new Promise((_, rj) => { rejectArchive = rj; });

    const m = mountSelector({ seeded: false, currentSessionId: '' });
    await settle();
    expect(reads).toBe(1); // the single load read — nothing else re-resolves identity in an unseeded selector until a mutation settles

    clickAction(m.target, 'a'); // remove the presented session; capture runs with mutCurrentId = 'a'
    rejectArchive(new Error('archive failed')); // in flight: the rejection settles now and its distinction read fails under it
    await settle();

    expect(reads).toBe(2); // load + exactly one post-rejection attempt — no retry, no second mandated read
    const errs = m.seen.filter((e) => Array.isArray(e));
    expect(errs).toHaveLength(1); // the failed distinction still shows this catch: same generation at settle time, unseeded, nothing else owns it
    expect(errs[0]).toEqual(['error', 'archive failed']); // lastErr's own text — not the read failure, which is swallowed by design (only ever one surface may show a given failure)
    expect(m.seen.filter((e) => e === 'close')).toHaveLength(1);

    unmount(m.comp);
  });

  it('unseeded rejected delete of a noncurrent target performs no post-rejection read and keeps this catch sole', async () => {
    let lists = 0;
    const backend = makeBackend();
    backend.SessionList = async (state) => { lists++; return state === 'archived' ? rows : []; }; // the delete affordance lives in the archived view, where no row is presented-current
    let reads = 0;
    backend.SessionCurrent = async () => { reads++; return { id: '' }; }; // nothing presented — every target here is plainly noncurrent
    let rejectDelete = null; // set to the pending call's rejection once it is in flight
    backend.SessionDelete = () => new Promise((_, rj) => { rejectDelete = rj; });

    const m = mountSelector({ seeded: false, currentSessionId: '' });
    await settle();
    [...m.target.querySelectorAll('.tab')].find((b) => b.textContent === 'Archived').click(); // into the view whose rows carry delete affordances (its re-list reads identity like any load — counted in the baseline below)
    await settle();
    const baseline = reads;

    clickAction(m.target, 'b'); // remove('b') → SessionDelete rejects; a noncurrent target may never trigger the distinction read
    rejectDelete(new Error('delete failed'));
    await settle();

    expect(reads).toBe(baseline); // delete and archive share one settlement: neither re-resolves identity for a noncurrent target, so no post-rejection read exists to count here
    const errs = m.seen.filter((e) => Array.isArray(e));
    expect(errs).toHaveLength(1); // gated catch stays sole while unseeded; it carries the rejection text as-is
    expect(errs[0]).toEqual(['error', 'delete failed']);
    expect(m.seen.filter((e) => e === 'close')).toHaveLength(1);

    unmount(m.comp);
  });

  it('a seeded rejected removal of the presented session itself is suppressed with zero owner reads — App shows this path\'s own frame', async () => {
    const backend = makeBackend();
    backend.SessionList = async () => rows;
    let reads = 0;
    backend.SessionCurrent = async () => { reads++; return { id: 'a' }; }; // must stay uncalled in a seeded view, at mount or post-mutation — even for the presented session itself
    let rejectArchive = null; // set to the pending call's rejection once it is in flight
    backend.SessionArchive = () => new Promise((_, rj) => { rejectArchive = rj; });

    const m = mountSelector({ seeded: true, currentSessionId: 'a' }); // presented session a comes from App — no owner read at open either
    await settle();
    expect(reads).toBe(0);

    clickAction(m.target, 'a'); // archive the very row that is being presented; capture runs with mutCurrentId = 'a', exactly like an unseeded current removal
    rejectArchive(new Error('archive committed but failed')); // in flight: the rejection settles now — seeded views never re-resolve identity to distinguish precommit from committed, because App's own frame owns both
    await settle();

    expect(reads).toBe(0); // zero owner reads across mount and settlement alike
    const errs = m.seen.filter((e) => Array.isArray(e));
    expect(errs).toHaveLength(0); // suppressed: showing here would double the unsequenced frame App already shows for this path (plain or committed — its pair arrives either way once seeded)
    expect(m.seen.filter((e) => e === 'close')).toHaveLength(1);

    unmount(m.comp);
  });

  it('an unseeded rejected current archive whose mandated read is still pending when boundaries land suppresses the stale catch, even though the owner answers with the captured session', async () => {
    const backend = makeBackend();
    backend.SessionList = async () => rows;
    let reads = 0;
    let releaseRead = null; // set once the post-rejection distinction read is in flight and pending
    // The load read resolves normally. The mandated post-rejection read stays PENDING across two applied boundaries (A->B->A) and then answers with the captured session: presentation moved on without this rejection, so its catch must stay silent even though the owner still reports 'a' as current at resolution time — classification is against what owns presentation by settle time, not against a stale read.
    backend.SessionCurrent = async () => { reads++; if (reads === 1) return { id: 'a' }; await new Promise((r) => { releaseRead = r; }); return { id: 'a' }; };
    let rejectArchive = null; // set to the pending call's rejection once it is in flight
    backend.SessionArchive = () => new Promise((_, rj) => { rejectArchive = rj; });

    const m = mountSelector({ seeded: false, currentSessionId: '' });
    await settle();
    expect(reads).toBe(1); // the single load read — nothing else re-resolves identity in an unseeded selector until a mutation settles

    clickAction(m.target, 'a'); // remove the presented session; capture runs with mutGen = 0 and mutCurrentId = 'a'
    rejectArchive(new Error('archive failed')); // settles now: close dispatches, then exactly one distinction read goes out — and stays pending here
    await settle();
    expect(typeof releaseRead).toBe('function'); // deterministic pre-condition: the mandated read is in flight right now

    m.genStore.set(1); // a boundary applied while the read was still pending (A->B)
    flushSync();
    await tick();
    m.genStore.set(2); // and another before it resolved (B->A): presentation moved on without this rejection, generation advanced twice
    flushSync();
    await tick();

    releaseRead(); // the read finally answers — with the captured session still current at the owner
    await settle();

    expect(reads).toBe(2); // load + exactly one post-rejection attempt: no retry exists for a pending-then-resolved distinction either
    const errs = m.seen.filter((e) => Array.isArray(e));
    expect(errs).toHaveLength(0); // stale at recheck time: generation 2 !== captured 0 — the applied boundaries own whatever this failure was about, so no catch over a presentation that already moved on without it (the read's 'a' answer cannot resurrect it)
    expect(m.seen.filter((e) => e === 'close')).toHaveLength(1);

    unmount(m.comp);
  });

  it('unseeded rejected current delete that stays current keeps the row and this catch sole, with exactly one post-rejection read', async () => {
    let lists = 0;
    const backend = makeBackend();
    // The owner legitimately reports 'a' as both presented-current AND archived (stale-archived): a rejected delete of it is classified by the same mandated distinction a current archive uses — still current after rejection means precommit: row kept, catch sole.
    backend.SessionList = async () => { lists++; return rows; };
    let reads = 0;
    // Load read plus exactly one more — the mandated distinction (the archived tab's re-list is counted in the baseline below). The target is still current after rejection: a precommit failure whose row must stay.
    backend.SessionCurrent = async () => { reads++; return { id: 'a' }; };
    let rejectDelete = null; // set to the pending call's rejection once it is in flight
    backend.SessionDelete = () => new Promise((_, rj) => { rejectDelete = rj; });

    const m = mountSelector({ seeded: false, currentSessionId: '' });
    await settle();
    [...m.target.querySelectorAll('.tab')].find((b) => b.textContent === 'Archived').click(); // into the view whose rows carry delete affordances (its re-list reads identity like any load — counted in the baseline below)
    await settle();
    const baseline = reads;

    clickAction(m.target, 'a'); // remove('a') → SessionDelete rejects on the presented session itself: capture ran with mutCurrentId = 'a'
    rejectDelete(new Error('delete failed'));
    await settle();

    expect(reads).toBe(baseline + 1); // exactly one post-rejection read — delete and archive share the mandated distinction, nothing more (and none on a second attempt)
    const errs = m.seen.filter((e) => Array.isArray(e));
    expect(errs).toHaveLength(1); // precommit: target still current at settle time with no generation movement — this catch stays sole while unseeded
    expect(errs[0]).toEqual(['error', 'delete failed']);
    expect(clickRow(m.target, 'a')).toBeTruthy(); // the removal did not run durably: its row stays in the local list
    expect(calls.filter((c) => c.name === 'SessionDelete').length).toBe(1); // it was a delete route all along — archive never fired for this target
    expect(m.seen.filter((e) => e === 'close')).toHaveLength(1);

    unmount(m.comp);
  });

  it('a seeded rejected delete suppresses the catch with zero owner reads — App shows this path\'s own frame', async () => {
    let lists = 0;
    const backend = makeBackend();
    backend.SessionList = async (state) => { lists++; return state === 'archived' ? rows : []; }; // delete affordances live in the archived view, where both targets render
    let reads = 0;
    backend.SessionCurrent = async () => { reads++; return { id: 'b' }; }; // must stay uncalled in a seeded view — at mount, across tab re-lists, and through settlement alike (the presented session comes from the prop)
    let rejectDelete = null; // set to the pending call's rejection once it is in flight
    backend.SessionDelete = () => new Promise((_, rj) => { rejectDelete = rj; });

    const m = mountSelector({ seeded: true, currentSessionId: 'b' }); // presented session b comes from App — no owner read at open either
    await settle();
    [...m.target.querySelectorAll('.tab')].find((b) => b.textContent === 'Archived').click();
    await settle();

    clickAction(m.target, 'a'); // delete a noncurrent target in the seeded view; capture runs with mutSeeded = true and mutCurrentId = 'b'
    rejectDelete(new Error('delete committed but failed')); // in flight: the rejection settles now — seeded views never re-resolve identity to distinguish precommit from committed, because App's own frame owns both
    await settle();

    expect(reads).toBe(0); // zero owner reads across mount, tab switch, and settlement alike
    const errs = m.seen.filter((e) => Array.isArray(e));
    expect(errs).toHaveLength(0); // suppressed: showing here would double the unsequenced frame App already shows for this path (plain or committed — its pair arrives either way once seeded)
    expect(calls.filter((c) => c.name === 'SessionDelete').length).toBe(1); // a delete route, settled without any archive call and without touching identity
    expect(m.seen.filter((e) => e === 'close')).toHaveLength(1);

    unmount(m.comp);
  });

  it('a seeded rejected DELETE of the presented session itself is suppressed with zero owner reads — target equals captured current makes no difference to the seed gate', async () => {
    const backend = makeBackend();
    // The owner legitimately reports 'a' as both presented-current AND archived (stale-archived): this row drives that very row's delete affordance, so capture runs with cap.currentId === target — exactly like an unseeded current removal would classify. In a seeded view the seed gate must suppress before any identity distinction exists to run: App shows this path's own frame either way (plain or committed), and no owner read may fire anywhere in mount, tab switch, or settlement.
    backend.SessionList = async () => rows; // both views carry 'a' — active for presentation, archived where the delete affordance lives
    let reads = 0;
    backend.SessionCurrent = async () => { reads++; return { id: 'a' }; }; // must stay uncalled in a seeded view at every step below — this is what zero-reads proves against any regression that reorders identity distinction ahead of the seed gate for current targets
    let rejectDelete = null; // set to the pending call's rejection once it is in flight
    backend.SessionDelete = () => new Promise((_, rj) => { rejectDelete = rj; });

    const m = mountSelector({ seeded: true, currentSessionId: 'a' }); // presented session a comes from App — no owner read at open either
    await settle();
    expect(reads).toBe(0); // the seed gate holds already at mount for a seeded view
    [...m.target.querySelectorAll('.tab')].find((b) => b.textContent === 'Archived').click(); // into the view whose rows carry delete affordances; its re-list must not read identity in a seeded view either
    await settle();

    clickAction(m.target, 'a'); // remove('a') → SessionDelete on the presented session itself — capture runs with cap = {seeded true, currentId 'a'}, target equal to it
    rejectDelete(new Error('delete committed but failed')); // in flight: the rejection settles now against this action's own seeded capture
    await settle();

    expect(reads).toBe(0); // zero owner reads across mount, tab switch, and settlement alike — even with target === captured current, a seeded view never performs the distinction read because App's frame owns both plain and committed outcomes for this path
    const errs = m.seen.filter((e) => Array.isArray(e));
    expect(errs).toHaveLength(0); // suppressed: showing here would double the unsequenced frame App already shows (its pair arrives either way once seeded), regardless of whether the target was current or not at capture time
    expect(calls.filter((c) => c.name === 'SessionDelete').length).toBe(1); // a delete route against the presented session — settled by the seed gate alone, with no archive call and no identity read to substitute for it
    expect(m.seen.filter((e) => e === 'close')).toHaveLength(1);

    unmount(m.comp);
  });

  it('overlapping actions settle against their own captures: A cannot surface its stale error after B captured over the presentation, while B keeps its own settlement', async () => {
    const backend = makeBackend();
    backend.SessionList = async () => rows;
    let reads = 0;
    backend.SessionCurrent = async () => { reads++; return { id: 'a' }; }; // presented session a in an unseeded view — the load read is its only steady-state identity resolution
    let rejectArchive = null; // set to A's pending call rejection once it is in flight
    backend.SessionArchive = () => new Promise((_, rj) => { rejectArchive = rj; });
    let rejectNew = null; // set to B's pending create rejection once it is in flight
    backend.SessionNew = () => new Promise((_, rj) => { rejectNew = rj; });

    const m = mountSelector({ seeded: false, currentSessionId: '' });
    await settle();
    expect(reads).toBe(1); // load read only — nothing else re-resolves identity until a mutation settles

    clickAction(m.target, 'b'); // A: archive noncurrent b at generation 0 — its capture is {unseeded, gen 0} and stays with this action alone
    expect(typeof rejectArchive).toBe('function'); // in flight before anything else happens

    m.genStore.set(1); // a boundary applied while A was still pending (A->B): presentation moved on without it, generation advanced past A's capture
    flushSync();
    await tick();

    [...m.target.querySelectorAll('.menu .new')].find((b) => b.textContent === '+').click(); // B: new session — captured at the MOVED generation 1; under a shared global this is also where it overwrote whatever A had recorded
    expect(typeof rejectNew).toBe('function');

    rejectArchive(new Error('archive failed')); // A settles first, against ITS OWN capture {gen 0} versus presentation now at gen 1 — stale: the boundary owns presentation, so no error may surface for A even though B's call just ran and nothing else moved since
    await settle();
    const errsAfterA = m.seen.filter((e) => Array.isArray(e));
    expect(errsAfterA).toHaveLength(0); // action A cannot surface a stale error after the presentation it captured against has been overwritten — its capture says gen 0, live is gen 1

    rejectNew(new Error('create failed')); // B settles against ITS OWN capture {gen 1} versus live gen 1 — same generation: nothing newer owns presentation for this failure
    await settle();

    const errs = m.seen.filter((e) => Array.isArray(e));
    expect(errs).toHaveLength(1); // exactly one visible error across both actions — A's stale catch never surfaced, B's own did
    expect(errs[0]).toEqual(['error', 'create failed']); // action B retains its correct same-generation settlement: sole visible error carrying its own rejection text
    expect(reads).toBe(1); // no mandated read fired for either (A targeted noncurrent b; B is a navigation path) — overlapping actions add no identity re-resolution of their own
    expect(m.seen.filter((e) => e === 'close')).toHaveLength(2); // both settlements closed on error, as every rejection does

    unmount(m.comp);
  });
});
