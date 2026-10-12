// The provisional Model activity tracker: ONE combined chronological trailing
// 64 KiB text/refusal range with a single visible truncation marker, ordered
// nonempty pieces, adjacent same-kind combination, refusal kept visibly distinct,
// native TextEncoder/TextDecoder boundary-safe trimming, positions ignored as
// sequence numbers, tool activity keyed by call ID inside one accepted active
// Operation, and unmatched progress refused without mutation.
import { describe, expect, it, vi } from 'vitest';

import type {
  RefusalDeltaEvent,
  TextDeltaEvent,
  ToolFinishedEvent,
  ToolStartedEvent,
} from '../generated/protocol/types.gen';
import { ACTIVITY_BYTE_LIMIT, ProvisionalActivity, keepUtf8Suffix } from './activity';

const OP = 'op1';

function textEvent(operationID: string, content: string, position = 0): TextDeltaEvent {
  return { kind: 'text_delta', scope: { kind: 'operation', session_id: 's1', operation_id: operationID }, position, content };
}

function refusalEvent(operationID: string, content: string): RefusalDeltaEvent {
  return { kind: 'refusal_delta', scope: { kind: 'operation', session_id: 's1', operation_id: operationID }, content };
}

function toolStarted(operationID: string, callID: string, name: string): ToolStartedEvent {
  return { kind: 'tool_started', scope: { kind: 'operation', session_id: 's1', operation_id: operationID }, call_id: callID, ordinal: 0, name };
}

function toolFinished(operationID: string, callID: string, status: 'success'): ToolFinishedEvent {
  return { kind: 'tool_finished', scope: { kind: 'operation', session_id: 's1', operation_id: operationID }, call_id: callID, status };
}

function totalBytes(state: { pieces: { content: string }[] } | null): number {
  if (state === null) return 0;
  return state.pieces.reduce((sum, piece) => sum + new TextEncoder().encode(piece.content).length, 0);
}

describe('keepUtf8Suffix', () => {
  it('keeps a range at or under the limit untouched', () => {
    const text = 'a'.repeat(ACTIVITY_BYTE_LIMIT);
    expect(keepUtf8Suffix(text, ACTIVITY_BYTE_LIMIT)).toBe(text);
  });

  it('keeps the trailing bytes and cuts on a code point boundary', () => {
    const text = 'x'.repeat(10) + 'a'.repeat(ACTIVITY_BYTE_LIMIT);
    expect(keepUtf8Suffix(text, ACTIVITY_BYTE_LIMIT)).toBe('a'.repeat(ACTIVITY_BYTE_LIMIT));
    // 6 leading bytes drop (zz + two 'a'); the two-emoji tail survives whole.
    const mixed = 'a'.repeat(ACTIVITY_BYTE_LIMIT - 8) + '😀😀';
    const kept = keepUtf8Suffix('zzaa' + mixed, ACTIVITY_BYTE_LIMIT);
    expect(kept).toBe(mixed);
    expect(new TextEncoder().encode(kept).length).toBe(ACTIVITY_BYTE_LIMIT);
  });

  it('never emits a split surrogate for a multibyte boundary', () => {
    // 'é' is 2 bytes; a 1-byte budget cannot hold any code point → empty.
    expect(keepUtf8Suffix('ééé', 1)).toBe('');
    const kept = keepUtf8Suffix('é'.repeat(10), 3);
    // 3 bytes cannot hold a whole 2-byte 'é' twice → exactly one survives.
    expect(kept).toBe('é');
    expect(new TextEncoder().encode(kept).length).toBeLessThanOrEqual(3);
  });

  it('preserves a literal replacement character at the kept suffix boundary', () => {
    // U+FFFD is legitimate content (EF BF BD). The window is exactly the
    // literal char plus trailing ASCII; nothing may be stripped from it.
    const kept = keepUtf8Suffix('x' + '\uFFFD' + 'a'.repeat(20), 23);
    expect(kept).toBe('\uFFFD' + 'a'.repeat(20));
  });

  it('preserves a literal BOM at the kept suffix boundary', () => {
    // U+FEFF is legitimate content here, not an encoding marker to drop.
    const kept = keepUtf8Suffix('x' + '\uFEFF' + 'a'.repeat(20), 23);
    expect(kept).toBe('\uFEFF' + 'a'.repeat(20));
  });

  it('starts the kept suffix on a code point boundary for a split sequence', () => {
    // The window begins inside '中' (E4 B8 AD); only the complete trailing
    // ASCII point survives, with no bogus leading replacement character.
    const kept = keepUtf8Suffix('中a', 2);
    expect(kept).toBe('a');
  });
});

describe('ProvisionalActivity', () => {
  it('refuses progress that does not address the accepted active Operation', () => {
    const activity = new ProvisionalActivity(() => {});
    expect(activity.accept(null, textEvent(OP, 'hello'))).toBe(false);
    expect(activity.getState()).toBeNull();
    expect(activity.accept('other-op', textEvent(OP, 'hello'))).toBe(false);
    expect(activity.getState()).toBeNull();
  });

  it('keeps ordered text/refusal pieces and combines adjacent same-kind', () => {
    const activity = new ProvisionalActivity(() => {});
    activity.accept(OP, textEvent(OP, 'hello '));
    activity.accept(OP, textEvent(OP, 'world'));
    activity.accept(OP, refusalEvent(OP, 'no'));
    activity.accept(OP, textEvent(OP, ' back'));
    expect(activity.getState()).toMatchObject({ operationID: OP, truncated: false });
    expect(activity.getState()?.pieces).toEqual([
      { kind: 'text', content: 'hello world' },
      { kind: 'refusal', content: 'no' },
      { kind: 'text', content: ' back' },
    ]);
  });

  it('applies ONE combined 64 KiB budget across mixed text and refusal', () => {
    const activity = new ProvisionalActivity(() => {});
    // Fill the shared range past its single budget with an interleaved kind.
    activity.accept(OP, textEvent(OP, 't'.repeat(ACTIVITY_BYTE_LIMIT - 10)));
    activity.accept(OP, refusalEvent(OP, 'r'.repeat(10)));
    activity.accept(OP, textEvent(OP, 'more'));
    const state = activity.getState()!;
    expect(state.truncated).toBe(true);
    // One budget, not two: the total across both kinds stays within the cap.
    expect(totalBytes(state)).toBeLessThanOrEqual(ACTIVITY_BYTE_LIMIT);
    // The oldest content (the leading text) trimmed first; the newest tail is
    // the refusal + text boundary preserved at a code point boundary.
    const last = state.pieces[state.pieces.length - 1];
    expect(last).toMatchObject({ kind: 'text', content: 'more' });
  });

  it('ignores repeated positions and empty fragments without a backlog', () => {
    const activity = new ProvisionalActivity(() => {});
    activity.accept(OP, textEvent(OP, 'one', 5));
    activity.accept(OP, textEvent(OP, 'two', 5));
    activity.accept(OP, textEvent(OP, '', 7));
    expect(activity.getState()?.pieces).toEqual([{ kind: 'text', content: 'onetwo' }]);
    expect(activity.getState()?.tools).toEqual([]);
  });

  it('keys tool activity by call ID with name and status only', () => {
    const activity = new ProvisionalActivity(() => {});
    activity.accept(OP, toolStarted(OP, 'c1', 'read_file'));
    activity.accept(OP, toolStarted(OP, 'c2', 'write_file'));
    activity.accept(OP, toolFinished(OP, 'c1', 'success'));
    expect(activity.getState()?.tools).toEqual([
      { callID: 'c1', name: 'read_file', status: 'success' },
      { callID: 'c2', name: 'write_file', status: null },
    ]);
    activity.accept(OP, toolFinished(OP, 'c3', 'success'));
    expect(activity.getState()?.tools).toHaveLength(3);
  });

  it('resets its buffer when the Operation changes', () => {
    const activity = new ProvisionalActivity(() => {});
    activity.accept(OP, textEvent(OP, 'old'));
    activity.accept('op2', textEvent('op2', 'new'));
    expect(activity.getState()).toMatchObject({ operationID: 'op2', pieces: [{ kind: 'text', content: 'new' }] });
  });

  it('clears on authoritative replacement', () => {
    const onChange = vi.fn();
    const activity = new ProvisionalActivity(onChange);
    activity.accept(OP, textEvent(OP, 'gone'));
    activity.clear();
    expect(activity.getState()).toBeNull();
    expect(onChange).toHaveBeenCalled();
  });
});
