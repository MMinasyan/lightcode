import { describe, expect, it } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { runInNewContext } from 'node:vm';
import { permissionList, upsertPermission } from './lib/permissions.js';

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
