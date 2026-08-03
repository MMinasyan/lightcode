import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { runInNewContext } from 'node:vm';
import { snapshotMessages, newTranscriptGate } from './lib/hydration.js';
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

function applySnapshotSandbox(overrides = {}) {
  return {
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
    ...overrides,
  };
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
