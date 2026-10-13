// The composer attempt ledger: minted caller IDs, immutable attempt content,
// acceptance by hydration presence only, absence as non-evidence, settlement
// removal after the consumer takes the evidence, and retry keeping the same
// ID without resurrection of removed attempts.
import { describe, expect, it, vi } from 'vitest';

import type { Hydration } from '../generated/protocol/types.gen';
import { AttemptLedger, mintOperationID } from './attempts';
import type { RestoreOutcome } from './restore';

const INSTANCE = '0123456789abcdef0123456789abcdef';

function makeHydration(overrides: Partial<Hydration> = {}): Hydration {
  return {
    session: {
      session_id: 's1',
      workspace: '/ws',
      lifecycle: 'open',
      agent_type: 'primary',
      created_at: '0',
      last_activity: '0',
      session_revision: { instance_id: INSTANCE, durable_revision: '1', local_revision: '0' },
    },
    selected_model: null,
    session_revision: { instance_id: INSTANCE, durable_revision: '1', local_revision: '0' },
    configuration_revision: { instance_id: INSTANCE, generation: '1' },
    warnings_revision: { instance_id: INSTANCE, revision: '1' },
    operations: [],
    pending: { steering: [], queued: [] },
    conversation: { items: [] },
    usage: { totals: { by_model: [] }, context: { used_tokens: '0', context_window: 0 } },
    warnings: [],
    background: [],
    ...overrides,
  };
}

describe('mintOperationID', () => {
  it('mints exactly 16 crypto-random bytes in lowercase hex', () => {
    const ids = new Set(Array.from({ length: 64 }, () => mintOperationID()));
    expect(ids.size).toBe(64);
    for (const id of ids) {
      expect(id).toMatch(/^[0-9a-f]{32}$/);
    }
  });
});

describe('AttemptLedger', () => {
  it('keeps the immutable content and ID through every status change', () => {
    const ledger = new AttemptLedger(() => {});
    const attempt = ledger.create('submit', 'regular', [{ kind: 'text', text: 'hello' }], 's1', 3);
    ledger.markUnknown(attempt.id);
    ledger.markRetrying(attempt.id);
    ledger.markRejected(attempt.id, 'conflict');
    const stored = ledger.getAttempts().find((entry) => entry.id === attempt.id);
    expect(stored).toMatchObject({
      id: attempt.id,
      kind: 'submit',
      mode: 'regular',
      content: [{ kind: 'text', text: 'hello' }],
      sessionID: 's1',
      draftRevision: 3,
      status: 'rejected',
      error: 'conflict',
    });
  });

  it('returns newly accepted descriptors and removes them; absence stays unknown', () => {
    const ledger = new AttemptLedger(() => {});
    const admitted = ledger.create('submit', 'regular', [{ kind: 'text', text: 'one' }], 's1', 1);
    const queued = ledger.create('submit', 'queued', [{ kind: 'text', text: 'two' }], 's1', 2);
    const steered = ledger.create('submit', 'regular', [{ kind: 'text', text: 'three' }], 's1', 3);
    const unseen = ledger.create('submit', 'regular', [{ kind: 'text', text: 'four' }], 's1', 4);
    ledger.markUnknown(admitted.id);
    ledger.markUnknown(queued.id);
    ledger.markUnknown(steered.id);
    ledger.markUnknown(unseen.id);
    const accepted = ledger.adoptHydration('s1', makeHydration({
      operations: [{ operation_id: admitted.id, request_kind: 'message', status: 'running', admitted_at: '0', agent_type: 'primary', model: 'p/m', usage: { totals: { by_model: [] } } }],
      pending: {
        steering: [{ operation_id: steered.id, origin: 'user', content: [] }],
        queued: [{ operation_id: queued.id, origin: 'user', content: [] }],
      },
    }));
    expect(accepted.map((entry) => entry.id).sort()).toEqual([admitted.id, queued.id, steered.id].sort());
    // The consumer can still read the captured draft revision from each
    // returned descriptor before the entries leave the ledger.
    expect(accepted.map((entry) => entry.draftRevision)).toEqual([1, 2, 3]);
    const remaining = ledger.getAttempts();
    expect(remaining).toHaveLength(1);
    expect(remaining[0].id).toBe(unseen.id);
    expect(remaining[0].status).toBe('unknown');
  });

  it('scopes acceptance evidence to the attempt session', () => {
    const ledger = new AttemptLedger(() => {});
    const other = ledger.create('submit', 'regular', [{ kind: 'text', text: 'x' }], 's2', 1);
    ledger.markUnknown(other.id);
    const accepted = ledger.adoptHydration('s1', makeHydration({
      operations: [{ operation_id: other.id, request_kind: 'message', status: 'running', admitted_at: '0', agent_type: 'primary', model: 'p/m', usage: { totals: { by_model: [] } } }],
    }));
    expect(accepted).toHaveLength(0);
    expect(ledger.getAttempts().find((attempt) => attempt.id === other.id)?.status).toBe('unknown');
  });

  it('a settled delivered attempt is removed without accumulating', () => {
    const ledger = new AttemptLedger(() => {});
    const attempt = ledger.create('compact', 'regular', [], 's1', 0);
    ledger.remove(attempt.id);
    expect(ledger.getAttempts()).toHaveLength(0);
    expect(ledger.outstanding('s1')).toHaveLength(0);
  });

  it('never resurrects a removed attempt through a late completion', () => {
    const ledger = new AttemptLedger(() => {});
    const attempt = ledger.create('submit', 'regular', [{ kind: 'text', text: 'bye' }], 's1', 1);
    ledger.remove(attempt.id);
    ledger.markRejected(attempt.id, 'late');
    ledger.markUnknown(attempt.id);
    expect(ledger.getAttempts()).toHaveLength(0);
  });

  it('lists only unknown and rejected attempts as outstanding', () => {
    const ledger = new AttemptLedger(() => {});
    ledger.create('submit', 'regular', [{ kind: 'text', text: 'a' }], 's1', 1);
    const unknown = ledger.create('submit', 'regular', [{ kind: 'text', text: 'b' }], 's1', 2);
    const rejected = ledger.create('submit', 'regular', [{ kind: 'text', text: 'c' }], 's1', 3);
    ledger.markUnknown(unknown.id);
    ledger.markRejected(rejected.id, 'conflict');
    const outstanding = ledger.outstanding('s1').map((attempt) => attempt.id);
    expect(outstanding).toEqual([unknown.id, rejected.id]);
  });

  it('notifies its subscriber on every change', () => {
    const onChange = vi.fn();
    const ledger = new AttemptLedger(onChange);
    const attempt = ledger.create('submit', 'regular', [{ kind: 'text', text: 'x' }], 's1', 1);
    ledger.markUnknown(attempt.id);
    ledger.adoptHydration('s1', makeHydration({
      operations: [{ operation_id: attempt.id, request_kind: 'message', status: 'running', admitted_at: '0', agent_type: 'primary', model: 'p/m', usage: { totals: { by_model: [] } } }],
    }));
    expect(ledger.getAttempts()).toHaveLength(0);
    expect(onChange.mock.calls.length).toBeGreaterThanOrEqual(3);
  });
});

describe('fork attempts', () => {
  function fork(ledger: AttemptLedger, rewind = true) {
    return ledger.createFork('s1', 'item-1', 'op-1', '[{"kind":"text","text":"edited"}]', rewind, 7);
  }

  it('captures the immutable Fork payload with a minted caller ID', () => {
    const ledger = new AttemptLedger(() => {});
    const attempt = fork(ledger);
    expect(attempt.id).toMatch(/^[0-9a-f]{32}$/);
    expect(attempt).toMatchObject({
      kind: 'fork',
      sessionID: 's1',
      boundaryItemID: 'item-1',
      boundaryOperationID: 'op-1',
      replacementText: '[{"kind":"text","text":"edited"}]',
      rewind: true,
      draftRevision: 7,
      status: 'pending',
      restore: null,
    });
    expect(ledger.get(attempt.id)).toBe(attempt);
  });

  it('stores each handled restore outcome directly, unknown included', () => {
    const ledger = new AttemptLedger(() => {});
    const handled = fork(ledger);
    const failure: RestoreOutcome = { kind: 'failed', result: { restored: ['a'], skipped: [] }, message: 'partial' };
    ledger.markRestore(handled.id, failure);
    expect(ledger.get(handled.id)?.restore).toEqual(failure);
    const unknown = fork(ledger);
    const lost: RestoreOutcome = { kind: 'unknown', message: 'lost' };
    ledger.markRestore(unknown.id, lost);
    expect(ledger.get(unknown.id)?.restore).toEqual(lost);
  });

  it('lists a Fork whose rewind outcome is unknown as outstanding for explicit abandonment', () => {
    const ledger = new AttemptLedger(() => {});
    const unknown = fork(ledger);
    ledger.markRestore(unknown.id, { kind: 'unknown', message: 'lost' });
    expect(ledger.outstanding('s1').map((entry) => entry.id)).toEqual([unknown.id]);
  });

  it('accepts a Fork attempt through a destination hydration with immutable source lineage', () => {
    const ledger = new AttemptLedger(() => {});
    const attempt = fork(ledger);
    ledger.markUnknown(attempt.id);
    const accepted = ledger.adoptHydration('dest1', makeHydration({
      session: {
        session_id: 'dest1',
        workspace: '/ws',
        source_session_id: 's1',
        lifecycle: 'open',
        agent_type: 'primary',
        created_at: '0',
        last_activity: '0',
        session_revision: { instance_id: INSTANCE, durable_revision: '1', local_revision: '0' },
      },
      operations: [{ operation_id: attempt.id, request_kind: 'message', status: 'running', admitted_at: '0', agent_type: 'primary', model: 'p/m', usage: { totals: { by_model: [] } } }],
    }));
    expect(accepted.map((entry) => entry.id)).toEqual([attempt.id]);
    expect(ledger.getAttempts()).toHaveLength(0);
  });

  it('never accepts a Fork attempt from an unrelated destination or from source absence', () => {
    const ledger = new AttemptLedger(() => {});
    const attempt = fork(ledger);
    ledger.markUnknown(attempt.id);
    // Another destination without the lineage does not accept.
    const unrelated = ledger.adoptHydration('dest2', makeHydration({
      session: {
        session_id: 'dest2',
        workspace: '/ws',
        lifecycle: 'open',
        agent_type: 'primary',
        created_at: '0',
        last_activity: '0',
        session_revision: { instance_id: INSTANCE, durable_revision: '1', local_revision: '0' },
      },
      operations: [{ operation_id: attempt.id, request_kind: 'message', status: 'running', admitted_at: '0', agent_type: 'primary', model: 'p/m', usage: { totals: { by_model: [] } } }],
    }));
    expect(unrelated).toHaveLength(0);
    // The source Session's own hydration absence proves nothing.
    const source = ledger.adoptHydration('s1', makeHydration());
    expect(source).toHaveLength(0);
    expect(ledger.get(attempt.id)?.status).toBe('unknown');
  });
});
