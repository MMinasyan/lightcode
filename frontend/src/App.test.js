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
        onPermissionRequest({ id: 'p1', tool: 'bash', args: 'ls', canAllowAll: false });
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

describe('App project-switch model ordering', () => {
  it('does not fetch the current model on project switch; the navigation boundary carries it', () => {
    // A project switch delivers an ordered navigation boundary carrying the
    // destination's model, which the snapshot applies. An out-of-band
    // CurrentModel here could surface the destination's model before its
    // boundary does, so it must not be fetched.
    const body = functionBodySource('handleProjectSwitched');
    expect(body).not.toMatch(/CurrentModel\(/);
    expect(body).not.toMatch(/refreshCurrentModel\(/);
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
      messages: [{ type: 'user', content: 'question', turn: 1 }],
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
