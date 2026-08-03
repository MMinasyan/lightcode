import { describe, expect, it } from 'vitest';

import { admitSequenced, newTranscriptGate, snapshotHighWater, snapshotMessages } from './hydration.js';

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

  it('rejects a replayed frame for a row the disjointness filter folded into the durable half', () => {
    // A capture taken between a turn's completion marker and its commit drops
    // the retained tail (the durable half already covers it) and raises the
    // returned cursor over the dropped sequences. A frame buffered for
    // post-snapshot replay carries the dropped row's sequence and must gate as
    // already present, or the row renders twice.
    const droppedSeq = 6;
    const filtered = {
      messages: [{ type: 'user', content: 'q1' }, { type: 'assistant', content: 'a1' }],
      cursor: { committedSeq: droppedSeq },
      tail: [],
      errors: [],
    };
    const gate = newTranscriptGate(filtered);
    expect(snapshotHighWater(filtered)).toBe(droppedSeq);
    expect(admitSequenced(gate, droppedSeq)).toBe(false);
    // A frame after the raised water is still new.
    expect(admitSequenced(gate, droppedSeq + 1)).toBe(true);

    // Without the cursor raise the same filtered snapshot admits the replay:
    // the gate falls below the dropped sequence and the row renders again.
    const torn = {
      messages: filtered.messages,
      cursor: { committedSeq: 3 },
      tail: [],
      errors: [],
    };
    const tornGate = newTranscriptGate(torn);
    expect(admitSequenced(tornGate, droppedSeq)).toBe(true);
  });
});

describe('snapshot message merge', () => {
  it('puts committed messages first, then tail and errors merged by sequence', () => {
    const state = {
      messages: [{ type: 'user', content: 'q1' }, { type: 'assistant', content: 'a1' }],
      tail: [
        { seq: 7, message: { type: 'assistant', content: 'streaming' } },
        { seq: 5, message: { type: 'user', content: 'q2' } },
      ],
      errors: [{ seq: 6, message: { type: 'error', content: 'boom' } }],
    };
    expect(snapshotMessages(state).map((m) => m.content)).toEqual([
      'q1',
      'a1',
      'q2',
      'boom',
      'streaming',
    ]);
  });

  it('returns only committed messages when there is no live tail or errors', () => {
    expect(snapshotMessages({ messages: [{ content: 'only' }] }).map((m) => m.content)).toEqual(['only']);
    expect(snapshotMessages({})).toEqual([]);
  });
});
