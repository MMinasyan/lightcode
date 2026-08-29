import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { runInNewContext } from 'node:vm';
import { snapshotMessages, newTranscriptGate, admitSequenced } from './lib/hydration.js';
import { permissionList, seedPermissions, upsertPermission } from './lib/permissions.js';

// eventCallbackSource extracts the live event listener's arrow function
// `(data) => { ... }` out of App.svelte so the test drives the real handler
// code, not a copy of it.
function eventCallbackSource(eventName) {
  const app = readFileSync(resolve('src/App.svelte'), 'utf8');
  const marker = `EventsOn('${eventName}',`;
  const start = app.indexOf(marker);
  if (start === -1) throw new Error(`no EventsOn('${eventName}') listener in App.svelte`);
  const arrow = app.indexOf('=>', start);
  const open = app.indexOf('{', arrow);
  let depth = 0;
  let close = -1;
  for (let i = open; i < app.length; i++) {
    const c = app[i];
    if (c === '{') depth++;
    else if (c === '}') {
      depth--;
      if (depth === 0) { close = i; break; }
    }
  }
  if (close === -1) throw new Error(`unbalanced listener body for '${eventName}'`);
  const head = app.slice(start, arrow);
  return app.slice(start + head.lastIndexOf('('), close + 1);
}

// functionBodySource extracts a named top-level function's full brace-balanced
// definition (keyword, name, parameters, and body) out of App.svelte so the
// test drives the real handler code, not a copy of it. The marker also matches
// `async function name(...)` definitions.
function functionBodySource(fnName) {
  const app = readFileSync(resolve('src/App.svelte'), 'utf8');
  const marker = `function ${fnName}(`;
  const start = app.indexOf(marker);
  if (start === -1) throw new Error(`no function ${fnName} in App.svelte`);
  const open = app.indexOf('{', start);
  let depth = 0;
  let close = -1;
  for (let i = open; i < app.length; i++) {
    const c = app[i];
    if (c === '{') depth++;
    else if (c === '}') {
      depth--;
      if (depth === 0) { close = i; break; }
    }
  }
  if (close === -1) throw new Error(`unbalanced function body for ${fnName}`);
  return app.slice(start, close + 1);
}

// defineStreamingHelpers installs the real streaming-index helpers from
// App.svelte into a sandbox, so extracted handler code that calls them runs as
// written. Function declarations installed this way are non-enumerable globals,
// so the definition must happen on the final sandbox object, not on an object
// that is later spread.
function defineStreamingHelpers(sandbox) {
  runInNewContext(functionBodySource('closeStreaming'), sandbox);
  runInNewContext(functionBodySource('continueStreamingRow'), sandbox);
  return sandbox;
}

function applySnapshotSandbox(overrides = {}) {
  return defineStreamingHelpers({
    sessionId: 'prev-session',
    projectName: 'prev-project',
    messages: [],
    gate: { highWater: 0 },
    streamingIdx: -1,
    tokens: null,
    busy: false,
    compacting: false,
    lastQueueVersion: 0,
    messageQueue: [],
    warnings: [],
    permissions: new Map(),
    modelRef: 'prev/model',
    modelName: 'Prev Model',
    presentationGeneration: 0,
    rebuildFromHistory: (persisted) => (persisted || []).map((m) => ({ ...m })),
    snapshotMessages,
    newTranscriptGate,
    defaultTokens: () => ({ total: { cache: 0, input: 0, output: 0, known: true }, perModel: [], contextUsed: 0, contextWindow: 0 }),
    seedPermissions,
    permissionList,
    appendPermissionDismissedNotice: () => {},
    closeViewer: () => {},
    ...overrides,
  });
}

describe('App permission resolution listener', () => {
  it('clears the resolved prompt from the pending map when permission_resolved arrives', () => {
    const app = readFileSync(resolve('src/App.svelte'), 'utf8');

    // The listener must be registered under the resolution event name, so a
    // cancelled prompt is cleared before turn end rather than left answerable.
    expect(app).toMatch(/EventsOn\(\s*'permission_resolved'/);

    // The listener must act: drop the resolved id from the pending permissions
    // map through the map helper.
    expect(app).toMatch(/permissions\s*=\s*removePermission\(\s*permissions,\s*data\.id\s*\)/);
  });
});

describe('App turn-end permission reset', () => {
  it('drives turn_end then permission_request through the real handlers and keeps the request pending', () => {
    const sandbox = {
      permissions: new Map(),
      busy: true,
      snapshotApplied: true,
      closeStreaming: () => {},
      upsertPermission,
    };

    // Run the live handlers from App.svelte in order: a turn end clears the
    // pending map, then a fresh permission request must still land in it.
    runInNewContext(
      `
        const onTurnEnd = ${eventCallbackSource('turn_end')};
        const onPermissionRequest = ${eventCallbackSource('permission_request')};
        onTurnEnd({});
        onPermissionRequest({ id: 'p1', tool: 'bash', args: 'ls' });
      `,
      sandbox,
    );

    // The reactive render reads permissionList(permissions)[0]; the request
    // must come back so PermissionPrompt renders.
    expect(permissionList(sandbox.permissions).map((p) => p.id)).toEqual(['p1']);
  });
});

describe('App turn-end gate over unseeded state', () => {
  it('leaves the pending permissions and streaming alone when no snapshot was applied', () => {
    const sandbox = {
      snapshotApplied: false,
      busy: true,
      streamingIdx: 3,
      closeStreaming: () => { sandbox.streamingIdx = -1; },
      permissions: new Map([['a', { id: 'a' }]]),
    };

    runInNewContext(`(${eventCallbackSource('turn_end')})({});`, sandbox);

    // The listener is gated on a snapshot having been applied: over unseeded
    // state it must not clear the pending map, drop streaming, or touch busy.
    expect(sandbox.permissions.has('a')).toBe(true);
    expect(sandbox.streamingIdx).toBe(3);
    expect(sandbox.busy).toBe(true);
  });
});

describe('App submit gate over unseeded state', () => {
  function submitSandbox(overrides = {}) {
    const sandbox = {
      modelRef: 'prov/model',
      snapshotApplied: false,
      readOnly: false,
      shown: null,
      prefilled: null,
      showError: (err) => { sandbox.shown = err; },
      inputArea: { prefill: (c) => { sandbox.prefilled = c; } },
      Submit: async (content) => { sandbox.submitted = (sandbox.submitted || []).concat(content); return { started: false }; },
      ...overrides,
    };
    return sandbox;
  }

  it('blocks handleSubmit when no snapshot was applied even with a model present', () => {
    const sandbox = submitSandbox();
    runInNewContext(`(async ${functionBodySource('handleSubmit')})({ detail: 'hello' });`, sandbox);

    // The composer gate (button state) alone is not the gate: the handler must
    // refuse too, keeping the draft, or a stale dispatch would submit into a
    // session the view never loaded.
    expect(sandbox.submitted).toBeUndefined();
    expect(sandbox.prefilled).toBe('hello');
    expect(sandbox.shown).toBeTruthy();
  });

  it('blocks handleSubmit for a read-only session', () => {
    const sandbox = submitSandbox({ snapshotApplied: true, readOnly: true });
    runInNewContext(`(async ${functionBodySource('handleSubmit')})({ detail: 'hello' });`, sandbox);

    expect(sandbox.submitted).toBeUndefined();
    expect(sandbox.prefilled).toBe('hello');
    expect(sandbox.shown).toBeTruthy();
  });
});

describe('App snapshot model application', () => {
  it('applies the destination session model to the selector when a snapshot is applied', () => {
    const sandbox = applySnapshotSandbox();
    runInNewContext(
      `(${functionBodySource('applySnapshot')})({
        session: { id: 'dest-session' },
        messages: [],
        tokens: { total: { cache: 0, input: 0, output: 0, known: true }, perModel: [], contextUsed: 0, contextWindow: 0 },
        model: { ref: 'prov/dest', provider: 'prov', model: 'dest', displayName: 'Dest Model' },
        queue: { items: [], version: 3 },
        warnings: [],
        permissions: [],
      });`,
      sandbox,
    );

    // The selector follows the destination session, and the send gate keys off
    // the same modelRef, so switching sessions leaves the selector (and the
    // gate) on the destination's model, not the previous session's.
    expect(sandbox.sessionId).toBe('dest-session');
    expect(sandbox.modelRef).toBe('prov/dest');
    expect(sandbox.modelName).toBe('Dest Model');
  });

  it('degrades to the bare ref when the captured model has no display name', () => {
    const sandbox = applySnapshotSandbox();
    runInNewContext(
      `(${functionBodySource('applySnapshot')})({
        session: { id: 'dest-session' },
        messages: [],
        tokens: { total: { cache: 0, input: 0, output: 0, known: true }, perModel: [], contextUsed: 0, contextWindow: 0 },
        model: { ref: 'prov/dest', provider: 'prov', model: 'dest', displayName: '' },
        queue: { items: [], version: 0 },
        warnings: [],
        permissions: [],
      });`,
      sandbox,
    );
    expect(sandbox.modelRef).toBe('prov/dest');
    expect(sandbox.modelName).toBe('prov/dest');
  });
});

describe('App snapshot project label', () => {
  it('moves the toolbar project label to the destination session project', () => {
    const sandbox = applySnapshotSandbox();
    runInNewContext(
      `(${functionBodySource('applySnapshot')})({
        session: { id: 'dest-session', projectPath: '/home/user/code/other-project' },
        messages: [],
        tokens: { total: { cache: 0, input: 0, output: 0, known: true }, perModel: [], contextUsed: 0, contextWindow: 0 },
        queue: { items: [], version: 0 },
        warnings: [],
        permissions: [],
      });`,
      sandbox,
    );
    // The toolbar shows the basename of the destination project, so a session
    // switch into another project moves the label with it instead of leaving it
    // on the previous project.
    expect(sandbox.projectName).toBe('other-project');
  });

  it('updates the label to the path itself for a root project, matching the backend title', () => {
    // The backend's basename of a root directory is the root itself, so a
    // switch to a root project titles the window with the root; the toolbar
    // label must agree instead of keeping the previous project.
    const sandbox = applySnapshotSandbox();
    runInNewContext(
      `(${functionBodySource('applySnapshot')})({
        session: { id: 'dest-session', projectPath: '/' },
        messages: [],
        tokens: { total: { cache: 0, input: 0, output: 0, known: true }, perModel: [], contextUsed: 0, contextWindow: 0 },
        queue: { items: [], version: 0 },
        warnings: [],
        permissions: [],
      });`,
      sandbox,
    );
    expect(sandbox.projectName).toBe('/');
  });

  it('leaves the toolbar project label alone when the snapshot carries no project path', () => {
    const sandbox = applySnapshotSandbox();
    runInNewContext(
      `(${functionBodySource('applySnapshot')})({
        session: { id: 'dest-session' },
        messages: [],
        tokens: { total: { cache: 0, input: 0, output: 0, known: true }, perModel: [], contextUsed: 0, contextWindow: 0 },
        queue: { items: [], version: 0 },
        warnings: [],
        permissions: [],
      });`,
      sandbox,
    );
    expect(sandbox.projectName).toBe('prev-project');
  });
});

describe('App project-switch no-fetch', () => {
  it('keeps no project-switch handler: the navigation boundary carries the destination state', () => {
    // A project switch delivers an ordered navigation boundary carrying the
    // destination's model and project name, which the snapshot applies; a
    // separate fetch could surface the destination's project name before its
    // boundary does. The project-name fetch is therefore called exactly once,
    // at mount, for the startup case where no session exists and no snapshot
    // can answer — a second call site anywhere, under any name, is an
    // out-of-band fetch of the destination project.
    const code = readFileSync(resolve('src/App.svelte'), 'utf8')
      .split('\n')
      .filter((line) => !line.trim().startsWith('//'))
      .join('\n');
    expect(code.match(/ProjectName\(/g) || []).toHaveLength(1);
  });
});

describe('App snapshot closes the child viewer', () => {
  it('closes the child viewer when a snapshot replaces the root view', () => {
    // A snapshot replaces the whole root view (navigation, detach, and the
    // failed-hydration recovery path). A child viewer open for the previous
    // session can no longer receive that child's frames, so it must close
    // rather than freeze with a live badge over a dead view.
    const sandbox = applySnapshotSandbox({
      closeViewer: () => { sandbox.viewerClosed = true; },
    });
    runInNewContext(
      `(${functionBodySource('applySnapshot')})({
        session: { id: 'dest-session' },
        messages: [],
        tokens: { total: { cache: 0, input: 0, output: 0, known: true }, perModel: [], contextUsed: 0, contextWindow: 0 },
        queue: { items: [], version: 0 },
        warnings: [],
        permissions: [],
      });`,
      sandbox,
    );
    expect(sandbox.viewerClosed).toBe(true);
  });
});

// rootStreamingSandbox is the state a root view holds while driving the real
// snapshot, resync, token, user-message, and turn-start handlers out of
// App.svelte: the transcript state plus the helpers those handlers call.
function rootStreamingSandbox(overrides = {}) {
  const sandbox = {
    ...applySnapshotSandbox(),
    snapshotApplied: true,
    nextId: 0,
    currentTurn: 0,
    mid: () => sandbox.nextId++,
    admitSequenced,
    ...overrides,
  };
  // The real streaming helpers, so the handlers exercise the actual index
  // continuation and finalisation. The spread above cannot carry them (vm
  // globals are non-enumerable), so they are defined on this final object.
  return defineStreamingHelpers(sandbox);
}

function openAssistantSnapshot(sandbox) {
  runInNewContext(
    `(${functionBodySource('applySnapshot')})({
      session: { id: 'dest-session' },
       messages: [
         { type: 'user', content: 'question', turn: 1 },
         { type: 'assistant', content: 'thinking ', turn: 1 },
       ],
      tail: [{ seq: 1, message: { type: 'assistant', content: 'thinking ', turn: 1 } }],
      assistantOpen: true,
      tokens: { total: { cache: 0, input: 0, output: 0, known: true }, perModel: [], contextUsed: 0, contextWindow: 0 },
      queue: { items: [], version: 0 },
      warnings: [],
      permissions: [],
    });`,
    sandbox,
  );
}

function openAssistantResync(sandbox) {
  runInNewContext(
    `(${functionBodySource('applyResync')})({
      session: { id: 'dest-session' },
      messages: [
        { type: 'user', content: 'question', turn: 1 },
        { type: 'assistant', content: 'thinking ', turn: 1 },
      ],
      assistantOpen: true,
      tokens: { total: { cache: 0, input: 0, output: 0, known: true }, perModel: [], contextUsed: 0, contextWindow: 0 },
    });`,
    sandbox,
  );
}

describe('App streaming continuation across boundaries', () => {
  it('continues a turn a snapshot captured mid-stream instead of opening a second row', () => {
    // A turn already streaming when the snapshot applies: the trailing
    // assistant row from the retained tail is still open, so the first token
    // after the snapshot must extend it, not open a fresh row.
    const sandbox = rootStreamingSandbox();
    openAssistantSnapshot(sandbox);
    runInNewContext(`(${functionBodySource('applyToken')})({ content: 'more', seq: 2 });`, sandbox);
    expect(sandbox.messages).toHaveLength(2);
    expect(sandbox.messages[1].content).toBe('thinking more');
    expect(sandbox.messages[1].partial).toBe(true);
    expect(sandbox.streamingIdx).toBe(1);
  });

  it('leaves the continuation alone when a same-turn start replays after the snapshot', () => {
    // A turn start replayed from the hydration buffer names the turn the
    // snapshot already carried; it must not disturb the continuation.
    const sandbox = rootStreamingSandbox();
    openAssistantSnapshot(sandbox);
    runInNewContext(`(${eventCallbackSource('turn_start')})({ turn: 1 });`, sandbox);
    runInNewContext(`(${functionBodySource('applyToken')})({ content: 'more', seq: 2 });`, sandbox);
    expect(sandbox.messages).toHaveLength(2);
    expect(sandbox.messages[1].content).toBe('thinking more');
    expect(sandbox.messages[1].partial).toBe(true);
    expect(sandbox.streamingIdx).toBe(1);
  });

  it('closes the continuation and leaves it unmarked when a different turn starts', () => {
    const sandbox = rootStreamingSandbox();
    openAssistantSnapshot(sandbox);
    runInNewContext(`(${eventCallbackSource('turn_start')})({ turn: 2 });`, sandbox);
    expect(sandbox.messages[1].partial).toBe(false);
    expect(sandbox.streamingIdx).toBe(-1);
    // The next token opens a new row for the new turn.
    runInNewContext(`(${functionBodySource('applyToken')})({ content: 'new turn', seq: 2 });`, sandbox);
    expect(sandbox.messages).toHaveLength(3);
    expect(sandbox.messages[2].content).toBe('new turn');
  });

  it('continues a turn a resync delivered mid-stream instead of opening a second row', () => {
    const sandbox = rootStreamingSandbox({ sessionId: 'dest-session' });
    runInNewContext(
      `(${functionBodySource('applyResync')})({
        session: { id: 'dest-session' },
        messages: [
          { type: 'user', content: 'question', turn: 1 },
          { type: 'assistant', content: 'thinking ', turn: 1 },
        ],
        assistantOpen: true,
        tokens: { total: { cache: 0, input: 0, output: 0, known: true }, perModel: [], contextUsed: 0, contextWindow: 0 },
      });`,
      sandbox,
    );
    runInNewContext(`(${functionBodySource('applyToken')})({ content: 'more', seq: 2 });`, sandbox);
    expect(sandbox.messages).toHaveLength(2);
    expect(sandbox.messages[1].content).toBe('thinking more');
    expect(sandbox.messages[1].partial).toBe(true);
    expect(sandbox.streamingIdx).toBe(1);
  });

  it('opens a new row after a finalising frame closes the continuation', () => {
    // The finalisation works off the index: a boundary frame closes the
    // marked row, and a token delivered after it must open a new row rather
    // than extending the finalised one.
    const sandbox = rootStreamingSandbox();
    openAssistantSnapshot(sandbox);
    runInNewContext(`(${functionBodySource('applyUserMessage')})({ content: 'follow-up', seq: 2 });`, sandbox);
    expect(sandbox.messages[1].partial).toBe(false);
    expect(sandbox.messages[1].content).toBe('thinking ');
    expect(sandbox.streamingIdx).toBe(-1);
    runInNewContext(`(${functionBodySource('applyToken')})({ content: 'reply', seq: 3 });`, sandbox);
    expect(sandbox.messages).toHaveLength(4);
    expect(sandbox.messages[3].content).toBe('reply');
    expect(sandbox.messages[3].partial).toBe(true);
  });

  it('resets a snapshot continuation when the rebuilt state closes the span', () => {
    // A rebuild arriving while a continuation is already established, with
    // the published fact false, must clear the index: a following token opens
    // a new row instead of appending to the row the previous continuation
    // pointed at, and no row is left marked.
    const sandbox = rootStreamingSandbox();
    openAssistantSnapshot(sandbox);
    runInNewContext(
      `(${functionBodySource('applySnapshot')})({
        session: { id: 'dest-session' },
        messages: [
          { type: 'user', content: 'question', turn: 1 },
          { type: 'assistant', content: 'other', turn: 1 },
        ],
        assistantOpen: false,
        tokens: { total: { cache: 0, input: 0, output: 0, known: true }, perModel: [], contextUsed: 0, contextWindow: 0 },
        queue: { items: [], version: 0 },
        warnings: [],
        permissions: [],
      });`,
      sandbox,
    );
    runInNewContext(`(${functionBodySource('applyToken')})({ content: 'more', seq: 2 });`, sandbox);
    expect(sandbox.messages).toHaveLength(3);
    expect(sandbox.messages[1].content).toBe('other');
    expect(sandbox.messages[1].partial).toBeFalsy();
    expect(sandbox.messages[2].content).toBe('more');
    expect(sandbox.messages[2].partial).toBe(true);
  });

  it('resets a snapshot continuation when the rebuilt last row is not an assistant row', () => {
    // A rebuild whose published fact is true but whose last row is not an
    // assistant row must also reset: the structural guard stops the marking,
    // and a stale index must not survive to direct the next token into that
    // row.
    const sandbox = rootStreamingSandbox();
    openAssistantSnapshot(sandbox);
    runInNewContext(
      `(${functionBodySource('applySnapshot')})({
        session: { id: 'dest-session' },
        messages: [
          { type: 'user', content: 'question', turn: 1 },
          { type: 'user', content: 'follow-up', turn: 1 },
        ],
        assistantOpen: true,
        tokens: { total: { cache: 0, input: 0, output: 0, known: true }, perModel: [], contextUsed: 0, contextWindow: 0 },
        queue: { items: [], version: 0 },
        warnings: [],
        permissions: [],
      });`,
      sandbox,
    );
    runInNewContext(`(${functionBodySource('applyToken')})({ content: 'reply', seq: 2 });`, sandbox);
    expect(sandbox.messages).toHaveLength(3);
    expect(sandbox.messages[1].content).toBe('follow-up');
    expect(sandbox.messages[1].partial).toBeFalsy();
    expect(sandbox.messages[2].content).toBe('reply');
    expect(sandbox.messages[2].partial).toBe(true);
  });

  it('resets a resync continuation when the rebuilt state closes the span', () => {
    const sandbox = rootStreamingSandbox({ sessionId: 'dest-session' });
    openAssistantResync(sandbox);
    runInNewContext(
      `(${functionBodySource('applyResync')})({
        session: { id: 'dest-session' },
        messages: [
          { type: 'user', content: 'question', turn: 1 },
          { type: 'assistant', content: 'other', turn: 1 },
        ],
        assistantOpen: false,
        tokens: { total: { cache: 0, input: 0, output: 0, known: true }, perModel: [], contextUsed: 0, contextWindow: 0 },
      });`,
      sandbox,
    );
    runInNewContext(`(${functionBodySource('applyToken')})({ content: 'more', seq: 2 });`, sandbox);
    expect(sandbox.messages).toHaveLength(3);
    expect(sandbox.messages[1].content).toBe('other');
    expect(sandbox.messages[1].partial).toBeFalsy();
    expect(sandbox.messages[2].content).toBe('more');
    expect(sandbox.messages[2].partial).toBe(true);
  });

  it('resets a resync continuation when the rebuilt last row is not an assistant row', () => {
    const sandbox = rootStreamingSandbox({ sessionId: 'dest-session' });
    openAssistantResync(sandbox);
    runInNewContext(
      `(${functionBodySource('applyResync')})({
        session: { id: 'dest-session' },
        messages: [
          { type: 'user', content: 'question', turn: 1 },
          { type: 'user', content: 'follow-up', turn: 1 },
        ],
        assistantOpen: true,
        tokens: { total: { cache: 0, input: 0, output: 0, known: true }, perModel: [], contextUsed: 0, contextWindow: 0 },
      });`,
      sandbox,
    );
    runInNewContext(`(${functionBodySource('applyToken')})({ content: 'reply', seq: 2 });`, sandbox);
    expect(sandbox.messages).toHaveLength(3);
    expect(sandbox.messages[1].content).toBe('follow-up');
    expect(sandbox.messages[1].partial).toBeFalsy();
    expect(sandbox.messages[2].content).toBe('reply');
    expect(sandbox.messages[2].partial).toBe(true);
  });
});

// pendingSubmitSandbox is the state a mounted view holds while a submit promise
// is in flight: the captured session/generation plus the continuation hooks
// (error row, draft prefill) the stale-completion tests assert against.
function pendingSubmitSandbox(overrides = {}) {
  const sandbox = {
    ...rootStreamingSandbox(),
    sessionId: 'A',
    presentationGeneration: 1,
    snapshotApplied: true,
    readOnly: false,
    busy: false,
    streamingIdx: -1,
    currentTurn: 1,
    shown: null,
    prefilled: null,
    submitResolve: null,
    submitReject: null,
    Submit: () => new Promise((resolve, reject) => {
      sandbox.submitResolve = resolve;
      sandbox.submitReject = reject;
    }),
    showError: (err) => { sandbox.shown = err; },
    inputArea: { prefill: (c) => { sandbox.prefilled = c; } },
    ...overrides,
  };
  return sandbox;
}

function navigateSandboxTo(sandbox, id) {
  runInNewContext(
    `(${functionBodySource('applySnapshot')})({
      session: { id: ${JSON.stringify(id)} },
      messages: [],
      tokens: { total: { cache: 0, input: 0, output: 0, known: true }, perModel: [], contextUsed: 0, contextWindow: 0 },
      queue: { items: [], version: 0 },
      warnings: [],
      permissions: [],
    });`,
    sandbox,
  );
}

describe('App promise continuation gate table (session + generation)', () => {
  // Every accepted-operation promise path: a completion captured on A must not
  // touch the view when it settles after A was replaced by B and re-selected
  // (A→B→A). The session id alone cannot reject it — it matches again — so
  // only the generation term can; each row therefore fails if the generation
  // term is removed from the gate. The same settlement on the still-current
  // view must still apply.

  function pathSandbox(backend, overrides = {}) {
    const sandbox = {
      ...rootStreamingSandbox(),
      sessionId: 'A',
      presentationGeneration: 1,
      snapshotApplied: true,
      readOnly: false,
      showModelSelector: true,
      shown: null,
      prefilled: null,
      inputArea: { prefill: (c) => { sandbox.prefilled = c; } },
      showError: (err) => { sandbox.shown = err; },
      ...overrides,
    };
    sandbox[backend] = () => new Promise((resolve, reject) => {
      sandbox[`${backend}Resolve`] = resolve;
      sandbox[`${backend}Reject`] = reject;
    });
    return sandbox;
  }

  const cases = [
    {
      name: 'submit',
      newSandbox: () => pathSandbox('Submit'),
      invoke: () => `(async ${functionBodySource('handleSubmit')})({ detail: 'hello' })`,
      staleSettle: (s) => s.SubmitReject(new Error('submit failed')),
      staleAssert: (s) => {
        expect(s.shown).toBeNull();
        expect(s.prefilled).toBeNull();
      },
      currentSettle: (s) => s.SubmitReject(new Error('submit failed')),
      currentAssert: (s) => {
        expect(s.shown).toBeTruthy();
        expect(s.prefilled).toBe('hello');
      },
    },
    {
      name: 'compact',
      newSandbox: () => pathSandbox('CompactNow'),
      invoke: () => `(async ${functionBodySource('handleCompact')})()`,
      staleSettle: (s) => s.CompactNowReject(new Error('compact failed')),
      staleAssert: (s) => { expect(s.shown).toBeNull(); },
      currentSettle: (s) => s.CompactNowReject(new Error('compact failed')),
      currentAssert: (s) => { expect(s.shown).toBeTruthy(); },
    },
    {
      name: 'model',
      newSandbox: () => pathSandbox('SwitchModel'),
      invoke: () => `(async ${functionBodySource('handleModelSelect')})({ detail: { ref: 'prov/new', displayName: 'New Model' } })`,
      staleSettle: (s) => s.SwitchModelResolve({}),
      staleAssert: (s) => {
        expect(s.showModelSelector).toBe(true);
        expect(s.shown).toBeNull();
      },
      currentSettle: (s) => s.SwitchModelResolve({}),
      currentAssert: (s) => { expect(s.showModelSelector).toBe(false); },
    },
    {
      name: 'revert-code',
      newSandbox: () => pathSandbox('ApplyTurnAction'),
      invoke: () => `(async ${functionBodySource('handleRevertCode')})({ detail: { turn: 3 } })`,
      staleSettle: (s) => s.ApplyTurnActionReject(new Error('revert failed')),
      staleAssert: (s) => { expect(s.shown).toBeNull(); },
      currentSettle: (s) => s.ApplyTurnActionReject(new Error('revert failed')),
      currentAssert: (s) => { expect(s.shown).toBeTruthy(); },
    },
    {
      name: 'fork',
      newSandbox: () => pathSandbox('ApplyTurnAction'),
      invoke: () => `(async ${functionBodySource('handleFork')})({ detail: { turn: 3, alsoRevertCode: false } })`,
      staleSettle: (s) => s.ApplyTurnActionReject(new Error('fork failed')),
      staleAssert: (s) => { expect(s.shown).toBeNull(); },
      currentSettle: (s) => s.ApplyTurnActionReject(new Error('fork failed')),
      currentAssert: (s) => { expect(s.shown).toBeTruthy(); },
    },
  ];

  it('A→B→A stale settlement never mutates the re-selected view; current-view settlement still applies', async () => {
    for (const tc of cases) {
      // Stale: captured on A, settles after A was replaced by B and then
      // re-selected. Session matches again; only the generation can reject.
      const stale = tc.newSandbox();
      runInNewContext(tc.invoke(), stale);
      navigateSandboxTo(stale, 'B');
      navigateSandboxTo(stale, 'A');
      tc.staleSettle(stale);
      await new Promise((r) => setTimeout(r, 0));
      tc.staleAssert(stale);
      expect(stale.sessionId).toBe('A');
      expect(stale.presentationGeneration).toBe(3);

      // Positive: the same settlement on the still-current view applies.
      const current = tc.newSandbox();
      runInNewContext(tc.invoke(), current);
      tc.currentSettle(current);
      await new Promise((r) => setTimeout(r, 0));
      tc.currentAssert(current);
    }
  });
});

describe('App promise completion gated by presentation', () => {
  it('a stale A promise completion cannot mutate the newer B view', async () => {
    // Success half: the submit captured on A settles after a navigation
    // boundary replaced the view with B. The activity fields are owned by the
    // ordered turn frames, and a stale completion must not raise busy,
    // streaming, or the turn counter on the newer view.
    const sandbox = pendingSubmitSandbox();
    runInNewContext(`(async ${functionBodySource('handleSubmit')})({ detail: 'hello' });`, sandbox);
    navigateSandboxTo(sandbox, 'B');
    sandbox.submitResolve({ started: true, turn: 5 });
    await new Promise((r) => setTimeout(r, 0));

    expect(sandbox.shown).toBeNull();
    expect(sandbox.prefilled).toBeNull();
    expect(sandbox.busy).toBe(false);
    expect(sandbox.streamingIdx).toBe(-1);
    // The sandbox's rebuildFromHistory stub does not reset currentTurn, so the
    // pre-navigation value 1 must survive untouched: the stale completion must
    // not advance it to the settled turn.
    expect(sandbox.currentTurn).toBe(1);
    expect(sandbox.sessionId).toBe('B');
    expect(sandbox.presentationGeneration).toBe(2);

    // Rejection half: a stale failure is equally inert on the newer view.
    const sandbox2 = pendingSubmitSandbox();
    runInNewContext(`(async ${functionBodySource('handleSubmit')})({ detail: 'hello' });`, sandbox2);
    navigateSandboxTo(sandbox2, 'B');
    sandbox2.submitReject(new Error('submission failed'));
    await new Promise((r) => setTimeout(r, 0));
    expect(sandbox2.shown).toBeNull();
    expect(sandbox2.prefilled).toBeNull();
    expect(sandbox2.sessionId).toBe('B');

    // Positive half: a precommit error on the still-current view still shows
    // exactly once, with the draft kept — the gate must not over-drop.
    const sandbox3 = pendingSubmitSandbox();
    runInNewContext(`(async ${functionBodySource('handleSubmit')})({ detail: 'hello' });`, sandbox3);
    sandbox3.submitReject(new Error('precommit'));
    await new Promise((r) => setTimeout(r, 0));
    expect(sandbox3.shown).toBeTruthy();
    expect(sandbox3.prefilled).toBe('hello');
    expect(sandbox3.sessionId).toBe('A');
  });

  it('a stale model switch cannot close or error the newer session selector', async () => {
    // A model switch captured on A settles after navigation replaced the view
    // with B: B's selector stays open and B shows no error, on success and on
    // failure alike.
    const sandbox = {
      ...applySnapshotSandbox(),
      sessionId: 'A',
      presentationGeneration: 1,
      showModelSelector: true,
      shown: null,
      switchResolve: null,
      switchReject: null,
      SwitchModel: () => new Promise((resolve, reject) => {
        sandbox.switchResolve = resolve;
        sandbox.switchReject = reject;
      }),
      showError: (err) => { sandbox.shown = err; },
    };
    runInNewContext(`(async ${functionBodySource('handleModelSelect')})({ detail: { ref: 'prov/new', displayName: 'New Model' } });`, sandbox);
    navigateSandboxTo(sandbox, 'B');
    sandbox.switchResolve({});
    await new Promise((r) => setTimeout(r, 0));
    expect(sandbox.showModelSelector).toBe(true);
    expect(sandbox.shown).toBeNull();

    // A stale failure (captured on B, settling after B was replaced) is
    // equally inert.
    runInNewContext(`(async ${functionBodySource('handleModelSelect')})({ detail: { ref: 'prov/new', displayName: 'New Model' } });`, sandbox);
    navigateSandboxTo(sandbox, 'C');
    sandbox.switchReject(new Error('switch failed'));
    await new Promise((r) => setTimeout(r, 0));
    expect(sandbox.showModelSelector).toBe(true);
    expect(sandbox.shown).toBeNull();

    // A switch that settles on the still-current view closes the selector and
    // shows its error — the gate must not over-drop.
    runInNewContext(`(async ${functionBodySource('handleModelSelect')})({ detail: { ref: 'prov/current', displayName: 'Current' } });`, sandbox);
    sandbox.switchResolve({});
    await new Promise((r) => setTimeout(r, 0));
    expect(sandbox.showModelSelector).toBe(false);
    runInNewContext(`(async ${functionBodySource('handleModelSelect')})({ detail: { ref: 'prov/current', displayName: 'Current' } });`, sandbox);
    sandbox.switchReject(new Error('switch failed'));
    await new Promise((r) => setTimeout(r, 0));
    expect(sandbox.shown).toBeTruthy();
  });
});

describe('App settings-refresh model gate', () => {
  // refreshCurrentModel is the settings-close reload path. It captures the
  // session id, presentation generation, and both model fields at call time and
  // mutates only while all four still match, so a settings refresh captured on A
  // cannot overwrite a newer B view (A→B), a detach, a re-selected A (A→B→A), or
  // a newer same-session model frame delivered before the old promise resolves.

  // deferredCurrentSandbox is a mounted settings view holding a pending
  // CurrentModel promise plus the state applySnapshot/navigation need.
  function deferredCurrentSandbox(overrides = {}) {
    const sandbox = {
      ...applySnapshotSandbox(),
      sessionId: 'A',
      presentationGeneration: 1,
      modelRef: 'prov/a',
      modelName: 'Model A',
      shown: null,
      currentResolve: null,
      currentReject: null,
      CurrentModel: () => new Promise((resolve, reject) => {
        sandbox.currentResolve = resolve;
        sandbox.currentReject = reject;
      }),
      showError: (err) => { sandbox.shown = err; },
      ...overrides,
    };
    return sandbox;
  }

  // navigateSandboxToWithModel navigates to a session whose snapshot carries an
  // explicit model, so a fixture can hold the model fields at their captured
  // values and isolate the session or generation term of the refresh guard.
  function navigateSandboxToWithModel(sandbox, id, ref, displayName) {
    runInNewContext(
      `(${functionBodySource('applySnapshot')})({
        session: { id: ${JSON.stringify(id)} },
        model: { ref: ${JSON.stringify(ref)}, displayName: ${JSON.stringify(displayName)} },
        messages: [],
        tokens: { total: { cache: 0, input: 0, output: 0, known: true }, perModel: [], contextUsed: 0, contextWindow: 0 },
        queue: { items: [], version: 0 },
        warnings: [],
        permissions: [],
      });`,
      sandbox,
    );
  }

  it('a settings refresh captured on A is inert after a B navigation whose model matches the capture (session ownership)', async () => {
    const sandbox = deferredCurrentSandbox();
    runInNewContext(`(async ${functionBodySource('refreshCurrentModel')})();`, sandbox);
    // B carries the captured model values, so the model terms cannot reject: a
    // model-only guard would apply the stale result. Only the session term can.
    navigateSandboxToWithModel(sandbox, 'B', 'prov/a', 'Model A');
    sandbox.currentResolve({ model: { ref: 'prov/new', provider: 'prov', model: 'new', displayName: 'New Model' }, superseded: false });
    await new Promise((r) => setTimeout(r, 0));
    expect(sandbox.sessionId).toBe('B');
    expect(sandbox.presentationGeneration).toBe(2);
    expect(sandbox.modelRef).toBe('prov/a');
    expect(sandbox.modelName).toBe('Model A');
    expect(sandbox.shown).toBeNull();
  });

  it('a settings refresh captured on A is inert after a detach leaves an empty route', async () => {
    const sandbox = deferredCurrentSandbox();
    runInNewContext(`(async ${functionBodySource('refreshCurrentModel')})();`, sandbox);
    // A detach boundary carries an empty state: the view is replaced wholesale
    // with no session and no model, and the stale result must not resurrect it.
    runInNewContext(
      `(${functionBodySource('applySnapshot')})({ session: {}, messages: [], tokens: { total: { cache: 0, input: 0, output: 0, known: true }, perModel: [], contextUsed: 0, contextWindow: 0 }, queue: { items: [], version: 0 }, warnings: [], permissions: [] });`,
      sandbox,
    );
    sandbox.currentResolve({ model: { ref: 'prov/new', provider: 'prov', model: 'new', displayName: 'New Model' }, superseded: false });
    await new Promise((r) => setTimeout(r, 0));
    expect(sandbox.sessionId).toBe('');
    expect(sandbox.presentationGeneration).toBe(2);
    expect(sandbox.modelRef).toBe('');
    expect(sandbox.modelName).toBe('');
    expect(sandbox.shown).toBeNull();
  });

  it('a settings refresh captured on A is inert after a re-selected A whose model matches the capture (generation ownership)', async () => {
    const sandbox = deferredCurrentSandbox();
    runInNewContext(`(async ${functionBodySource('refreshCurrentModel')})();`, sandbox);
    navigateSandboxTo(sandbox, 'B');
    // Re-select A carrying the captured model values, so session and model terms
    // all match again: only the advanced generation can reject the stale result.
    navigateSandboxToWithModel(sandbox, 'A', 'prov/a', 'Model A');
    sandbox.currentResolve({ model: { ref: 'prov/new', provider: 'prov', model: 'new', displayName: 'New Model' }, superseded: false });
    await new Promise((r) => setTimeout(r, 0));
    expect(sandbox.sessionId).toBe('A');
    expect(sandbox.presentationGeneration).toBe(3);
    expect(sandbox.modelRef).toBe('prov/a');
    expect(sandbox.modelName).toBe('Model A');
    expect(sandbox.shown).toBeNull();
  });

  it('a newer same-session model frame changing both model fields wins', async () => {
    const sandbox = deferredCurrentSandbox();
    runInNewContext(`(async ${functionBodySource('refreshCurrentModel')})();`, sandbox);
    runInNewContext(
      `(${eventCallbackSource('model')})({ rootId: 'A', model: { ref: 'prov/new', provider: 'prov', model: 'new', displayName: 'New Model' } });`,
      sandbox,
    );
    sandbox.currentResolve({ model: { ref: 'prov/a', provider: 'prov', model: 'a', displayName: 'Model A' }, superseded: false });
    await new Promise((r) => setTimeout(r, 0));
    expect(sandbox.modelRef).toBe('prov/new');
    expect(sandbox.modelName).toBe('New Model');
    expect(sandbox.shown).toBeNull();
  });

  it('a newer same-session model frame changing only modelRef wins (modelRef ownership)', async () => {
    const sandbox = deferredCurrentSandbox();
    runInNewContext(`(async ${functionBodySource('refreshCurrentModel')})();`, sandbox);
    // The newer frame changes modelRef while modelName stays at the capture, so
    // a modelName-only guard would let the stale result through.
    runInNewContext(
      `(${eventCallbackSource('model')})({ rootId: 'A', model: { ref: 'prov/new', provider: 'prov', model: 'new', displayName: 'Model A' } });`,
      sandbox,
    );
    sandbox.currentResolve({ model: { ref: 'prov/a', provider: 'prov', model: 'a', displayName: 'Model A' }, superseded: false });
    await new Promise((r) => setTimeout(r, 0));
    expect(sandbox.modelRef).toBe('prov/new');
    expect(sandbox.modelName).toBe('Model A');
    expect(sandbox.shown).toBeNull();
  });

  it('a newer same-session model frame changing only modelName wins (modelName ownership)', async () => {
    const sandbox = deferredCurrentSandbox();
    runInNewContext(`(async ${functionBodySource('refreshCurrentModel')})();`, sandbox);
    // The newer frame changes modelName while modelRef stays at the capture, so
    // a modelRef-only guard would let the stale result through.
    runInNewContext(
      `(${eventCallbackSource('model')})({ rootId: 'A', model: { ref: 'prov/a', provider: 'prov', model: 'a', displayName: 'Renamed Model' } });`,
      sandbox,
    );
    sandbox.currentResolve({ model: { ref: 'prov/a', provider: 'prov', model: 'a', displayName: 'Model A' }, superseded: false });
    await new Promise((r) => setTimeout(r, 0));
    expect(sandbox.modelRef).toBe('prov/a');
    expect(sandbox.modelName).toBe('Renamed Model');
    expect(sandbox.shown).toBeNull();
  });

  it('applies the result on the unchanged current view, overwriting the old model', async () => {
    const sandbox = deferredCurrentSandbox({ modelRef: 'prov/old', modelName: 'Old Model' });
    runInNewContext(`(async ${functionBodySource('refreshCurrentModel')})();`, sandbox);
    // The view begins with old model values and resolves a different result: the
    // assertion proves the refresh actually applied, not that it matched in place.
    sandbox.currentResolve({ model: { ref: 'prov/new', provider: 'prov', model: 'new', displayName: 'New Model' }, superseded: false });
    await new Promise((r) => setTimeout(r, 0));
    expect(sandbox.modelRef).toBe('prov/new');
    expect(sandbox.modelName).toBe('New Model');
  });

  it('a superseded result is inert even while all four captures still match', async () => {
    const sandbox = deferredCurrentSandbox();
    runInNewContext(`(async ${functionBodySource('refreshCurrentModel')})();`, sandbox);
    // All four frontend captures still match the unchanged view, but the backend
    // marked the result superseded (routing advanced while the refresh was in
    // flight), so the refresh must not apply anything.
    sandbox.currentResolve({ superseded: true });
    await new Promise((r) => setTimeout(r, 0));
    expect(sandbox.modelRef).toBe('prov/a');
    expect(sandbox.modelName).toBe('Model A');
    expect(sandbox.shown).toBeNull();
  });

  it('a wrong-root model frame stays ignored by the model listener', async () => {
    const sandbox = deferredCurrentSandbox();
    // The root differs from the presented session, so the ordered model item
    // must not touch the selector.
    runInNewContext(
      `(${eventCallbackSource('model')})({ rootId: 'X', model: { ref: 'prov/other', provider: 'prov', model: 'other', displayName: 'Other' } });`,
      sandbox,
    );
    expect(sandbox.modelRef).toBe('prov/a');
    expect(sandbox.modelName).toBe('Model A');
  });

  it('a stale refresh rejection is inert after a B navigation whose model matches the capture (session ownership)', async () => {
    const sandbox = deferredCurrentSandbox();
    runInNewContext(`(async ${functionBodySource('refreshCurrentModel')})();`, sandbox);
    navigateSandboxToWithModel(sandbox, 'B', 'prov/a', 'Model A');
    sandbox.currentReject(new Error('model load failed'));
    await new Promise((r) => setTimeout(r, 0));
    expect(sandbox.sessionId).toBe('B');
    expect(sandbox.presentationGeneration).toBe(2);
    expect(sandbox.shown).toBeNull();
  });

  it('a stale refresh rejection is inert after a detach leaves an empty route', async () => {
    const sandbox = deferredCurrentSandbox();
    runInNewContext(`(async ${functionBodySource('refreshCurrentModel')})();`, sandbox);
    runInNewContext(
      `(${functionBodySource('applySnapshot')})({ session: {}, messages: [], tokens: { total: { cache: 0, input: 0, output: 0, known: true }, perModel: [], contextUsed: 0, contextWindow: 0 }, queue: { items: [], version: 0 }, warnings: [], permissions: [] });`,
      sandbox,
    );
    sandbox.currentReject(new Error('model load failed'));
    await new Promise((r) => setTimeout(r, 0));
    expect(sandbox.sessionId).toBe('');
    expect(sandbox.shown).toBeNull();
  });

  it('a stale refresh rejection is inert after a re-selected A whose model matches the capture (generation ownership)', async () => {
    const sandbox = deferredCurrentSandbox();
    runInNewContext(`(async ${functionBodySource('refreshCurrentModel')})();`, sandbox);
    navigateSandboxTo(sandbox, 'B');
    navigateSandboxToWithModel(sandbox, 'A', 'prov/a', 'Model A');
    sandbox.currentReject(new Error('model load failed'));
    await new Promise((r) => setTimeout(r, 0));
    expect(sandbox.sessionId).toBe('A');
    expect(sandbox.presentationGeneration).toBe(3);
    expect(sandbox.shown).toBeNull();
  });

  it('a stale refresh rejection is inert after a same-session model frame changing only modelRef (modelRef ownership)', async () => {
    const sandbox = deferredCurrentSandbox();
    runInNewContext(`(async ${functionBodySource('refreshCurrentModel')})();`, sandbox);
    runInNewContext(
      `(${eventCallbackSource('model')})({ rootId: 'A', model: { ref: 'prov/new', provider: 'prov', model: 'new', displayName: 'Model A' } });`,
      sandbox,
    );
    sandbox.currentReject(new Error('model load failed'));
    await new Promise((r) => setTimeout(r, 0));
    expect(sandbox.modelRef).toBe('prov/new');
    expect(sandbox.modelName).toBe('Model A');
    expect(sandbox.shown).toBeNull();
  });

  it('a stale refresh rejection is inert after a same-session model frame changing only modelName (modelName ownership)', async () => {
    const sandbox = deferredCurrentSandbox();
    runInNewContext(`(async ${functionBodySource('refreshCurrentModel')})();`, sandbox);
    runInNewContext(
      `(${eventCallbackSource('model')})({ rootId: 'A', model: { ref: 'prov/a', provider: 'prov', model: 'a', displayName: 'Renamed Model' } });`,
      sandbox,
    );
    sandbox.currentReject(new Error('model load failed'));
    await new Promise((r) => setTimeout(r, 0));
    expect(sandbox.modelRef).toBe('prov/a');
    expect(sandbox.modelName).toBe('Renamed Model');
    expect(sandbox.shown).toBeNull();
  });

  it('a refresh rejection on the still-current view remains visible', async () => {
    const sandbox = deferredCurrentSandbox();
    runInNewContext(`(async ${functionBodySource('refreshCurrentModel')})();`, sandbox);
    sandbox.currentReject(new Error('model load failed'));
    await new Promise((r) => setTimeout(r, 0));
    expect(sandbox.shown).toBeTruthy();
    expect(sandbox.shown.message).toBe('model load failed');
    expect(sandbox.modelRef).toBe('prov/a');
  });
});
