import { describe, expect, it } from 'vitest';

import {
  normalizePermission,
  permissionList,
  removePermission,
  seedPermissions,
  upsertPermission,
} from './permissions.js';

describe('permission normalization', () => {
  it('passes a live camelCase event through unchanged', () => {
    const live = { id: 'p1', sessionId: 's', tool: 'edit_file', args: 'a', resolvedArg: 'r', canSaveProject: false, batchFiles: ['f'], batchResolvedFiles: ['rf'] };
    expect(normalizePermission(live)).toMatchObject({
      id: 'p1', sessionId: 's', tool: 'edit_file', args: 'a', resolvedArg: 'r',
      canSaveProject: false,
      batchFiles: ['f'], batchResolvedFiles: ['rf'],
    });
  });

  it('maps hydration snake_case and inverts disable_project_save into canSaveProject', () => {
    const snake = { id: 'p2', session_id: 's2', tool: 'edit_file', args: 'a', resolved_arg: 'r', disable_project_save: true, batch_files: ['f'] };
    const n = normalizePermission(snake);
    expect(n.sessionId).toBe('s2');
    expect(n.resolvedArg).toBe('r');
    expect(n.canSaveProject).toBe(false); // disable_project_save:true -> cannot save
    expect(n.batchFiles).toEqual(['f']);
  });

  it('treats an omitted disable_project_save (false) as savable', () => {
    expect(normalizePermission({ id: 'p3', tool: 't', args: 'a' }).canSaveProject).toBe(true);
  });
});

describe('permission map operations', () => {
  it('upserts by id without doubling a re-delivered request', () => {
    let map = new Map();
    map = upsertPermission(map, { id: 'p1', tool: 't', args: 'a' });
    map = upsertPermission(map, { id: 'p2', tool: 't', args: 'b' });
    map = upsertPermission(map, { id: 'p1', tool: 't', args: 'a2' }); // re-delivered
    expect(permissionList(map).map((p) => p.id)).toEqual(['p1', 'p2']);
    expect(map.get('p1').args).toBe('a2');
  });

  it('keeps insertion order even for all-numeric request ids', () => {
    let map = new Map();
    map = upsertPermission(map, { id: '90000000', tool: 't' });
    map = upsertPermission(map, { id: '10000000', tool: 't' });
    // A plain object would reorder these numerically; the map must not.
    expect(permissionList(map).map((p) => p.id)).toEqual(['90000000', '10000000']);
  });

  it('seeds a map from a hydration list and preserves order', () => {
    const map = seedPermissions([
      { id: 'p1', tool: 't', args: 'a' },
      { id: 'p2', tool: 't', args: 'b' },
    ]);
    expect(permissionList(map).map((p) => p.id)).toEqual(['p1', 'p2']);
  });

  it('removes a resolved request by id and leaves others', () => {
    let map = seedPermissions([{ id: 'p1', tool: 't' }, { id: 'p2', tool: 't' }]);
    map = removePermission(map, 'p1');
    expect(permissionList(map).map((p) => p.id)).toEqual(['p2']);
    expect(removePermission(map, undefined)).toBe(map);
    expect(removePermission(map, 'missing')).toBe(map);
  });
});
