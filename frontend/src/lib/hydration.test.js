import { describe, expect, it } from 'vitest';

import { admitSequenced, newTranscriptGate, snapshotHighWater } from './hydration.js';

describe('transcript high-water gating', () => {
  it('takes the high-water from the max of the cursor, tail, and error rows', () => {
    const state = {
      cursor: { committedSeq: 4 },
      tail: [{ seq: 6 }, { seq: 5 }],
      errors: [{ seq: 7 }],
    };
    expect(snapshotHighWater(state)).toBe(7);
  });

  it('falls back to the committed cursor when the tail and errors are empty', () => {
    expect(snapshotHighWater({ cursor: { committedSeq: 3 } })).toBe(3);
    expect(snapshotHighWater({})).toBe(0);
  });

  it('drops a live item already represented in the snapshot and admits later ones', () => {
    const gate = newTranscriptGate({ cursor: { committedSeq: 2 }, tail: [{ seq: 5 }] });
    // High-water is 5. Items at or below it are already shown.
    expect(admitSequenced(gate, 3)).toBe(false);
    expect(admitSequenced(gate, 5)).toBe(false);
    // The next streamed item is new and advances the water.
    expect(admitSequenced(gate, 6)).toBe(true);
    expect(gate.highWater).toBe(6);
    expect(admitSequenced(gate, 6)).toBe(false);
    expect(admitSequenced(gate, 7)).toBe(true);
    expect(gate.highWater).toBe(7);
  });

  it('admits a new delta that extends a snapshot-represented text row', () => {
    // The snapshot ends mid-row at seq 5; a following delta of the same row
    // carries seq 6 and must render, not drop.
    const gate = newTranscriptGate({ tail: [{ seq: 5 }] });
    expect(admitSequenced(gate, 6)).toBe(true);
  });

  it('always admits an unsequenced item so an id-keyed tool result still applies', () => {
    const gate = newTranscriptGate({ tail: [{ seq: 9 }] });
    expect(admitSequenced(gate, null)).toBe(true);
    expect(admitSequenced(gate, undefined)).toBe(true);
    // The water is unchanged by an unsequenced admission.
    expect(gate.highWater).toBe(9);
  });
});
