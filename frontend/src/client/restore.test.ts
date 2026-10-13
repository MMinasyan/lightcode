// The shared code-restore union rule: a 200 CodeRevertResult without an error
// member, a non-2xx partial CodeRevertResult
// with its non-null Error, or a plain non-2xx generated Error. A body matching
// neither branch (or a loss/undecodable delivery) is outcome unknown, never a
// fabricated protocol error and never an empty success; the generated closed
// error-code set is checked at runtime.
import { describe, expect, it } from 'vitest';

import { classifyRestore, ERROR_CODES, restoreLines } from './restore';
import type { ClassifiedResult } from './results';

function ok(data: unknown): ClassifiedResult {
  return { kind: 'ok', data };
}

function rejection(payload: unknown): ClassifiedResult {
  return { kind: 'rejection', message: 'received', payload };
}

describe('classifyRestore', () => {
  it('keeps a completed 200 result with its restored and skipped members', () => {
    const outcome = classifyRestore(ok({ restored: ['a.ts'], skipped: [{ path: 'b.ts', reason: 'changed' }] }), 'code rewind');
    expect(outcome.kind).toBe('ok');
    if (outcome.kind !== 'ok') return;
    expect(outcome.result.restored).toEqual(['a.ts']);
    expect(outcome.result.skipped).toEqual([{ path: 'b.ts', reason: 'changed' }]);
    expect(restoreLines(outcome)).toEqual(['Restored: a.ts', 'Skipped: b.ts (changed)']);
  });

  it('treats a 2xx without decodable restore arrays as unknown, not empty success', () => {
    const outcome = classifyRestore(ok({}), 'code rewind');
    expect(outcome).toEqual({ kind: 'unknown', message: 'code rewind: restore body is not a decodable result' });
  });

  it('keeps a non-2xx partial result with its non-null Error and complete file effects', () => {
    const outcome = classifyRestore(rejection({
      restored: ['a.ts'],
      skipped: [{ path: 'b.ts', reason: 'io' }],
      error: { code: 'storage', message: 'traversal failed' },
    }), 'code rewind');
    expect(outcome.kind).toBe('failed');
    if (outcome.kind !== 'failed') return;
    expect(outcome.message).toBe('traversal failed');
    expect(outcome.result?.restored).toEqual(['a.ts']);
    expect(restoreLines(outcome)).toEqual(['Restored: a.ts', 'Skipped: b.ts (io)', 'Restore failed: traversal failed']);
  });

  it('maps a plain generated rejection to a known failure with no file effects', () => {
    const outcome = classifyRestore(rejection({ code: 'conflict', message: 'busy' }), 'code rewind');
    expect(outcome).toEqual({ kind: 'failed', result: null, message: 'busy' });
  });

  it('never fabricates a protocol error from a partial-looking body without its Error', () => {
    const outcome = classifyRestore(rejection({ restored: [], skipped: [] }), 'code rewind');
    expect(outcome.kind).toBe('unknown');
  });

  it('rejects an unknown error code: only the generated closed set is accepted', () => {
    expect(Object.keys(ERROR_CODES)).toHaveLength(9);
    const outcome = classifyRestore(rejection({ code: 'made_up', message: 'x' }), 'code rewind');
    expect(outcome.kind).toBe('unknown');
  });

  it('maps loss and undecodable delivery to outcome unknown', () => {
    expect(classifyRestore({ kind: 'loss', message: 'transport unavailable' }, 'code rewind'))
      .toEqual({ kind: 'unknown', message: 'transport unavailable' });
    expect(classifyRestore({ kind: 'invalid', message: 'undecodable' }, 'code rewind'))
      .toEqual({ kind: 'unknown', message: 'undecodable' });
    expect(restoreLines({ kind: 'unknown', message: 'lost' })).toEqual(['Rewind outcome unknown: lost']);
  });
});

// First-review correction regressions: consumed restore shape and the
// partial-branch-first rule (nearest-null siblings included).
describe('classifyRestore consumed shape', () => {
  it('rejects malformed restored/skipped elements as unknown, never empty success', () => {
    expect(classifyRestore(ok({ restored: [1], skipped: [] }), 'code rewind').kind).toBe('unknown');
    expect(classifyRestore(ok({ restored: [], skipped: [null] }), 'code rewind').kind).toBe('unknown');
    expect(classifyRestore(ok({ restored: [], skipped: [{ path: 'a' }] }), 'code rewind').kind).toBe('unknown');
    expect(classifyRestore(ok({ restored: [], skipped: [{ path: 'a', reason: 2 }] }), 'code rewind').kind).toBe('unknown');
  });

  it('recognizes any result-shape rejection as partial first: malformed or partial-without-error never falls through to plain Error', () => {
    // Hybrid: a valid error code/message plus a result member, but no arrays.
    expect(classifyRestore(rejection({ code: 'storage', message: 'x', restored: [] }), 'code rewind').kind).toBe('unknown');
    // Partial without error: never plain Error, file effects are not dropped silently.
    expect(classifyRestore(rejection({ restored: ['a'], skipped: [] }), 'code rewind').kind).toBe('unknown');
    // Malformed array element in the partial branch.
    expect(classifyRestore(rejection({ restored: ['a'], skipped: [null], error: { code: 'storage', message: 'x' } }), 'code rewind').kind).toBe('unknown');
    // Nearest plain sibling still classifies as the known plain failure.
    expect(classifyRestore(rejection({ code: 'storage', message: 'x' }), 'code rewind'))
      .toEqual({ kind: 'failed', result: null, message: 'x' });
  });

  it('treats any error member on a successful status as invalid delivery, never success or known failure', () => {
    // A valid generated Error, an explicit null and a malformed member are one
    // uniform unknown outcome: a completed success is the required result
    // without an error member.
    for (const error of [{ code: 'storage', message: 'partial' }, null, { code: 'made_up' }, 7]) {
      expect(classifyRestore(ok({ restored: ['a'], skipped: [], error }), 'code rewind').kind).toBe('unknown');
    }
    // Nearest sibling: the same completed result without the member is success.
    expect(classifyRestore(ok({ restored: ['a'], skipped: [] }), 'code rewind').kind).toBe('ok');
  });
});
