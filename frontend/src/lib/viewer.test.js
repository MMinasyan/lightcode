import { get } from 'svelte/store';
import { beforeEach, describe, expect, it } from 'vitest';

import {
  appendSubagentEvent,
  closeViewer,
  hydrateSubagentViewer,
  openEditPreview,
  openSubagentViewer,
  openViewer,
  viewer,
} from './viewer.js';

beforeEach(() => {
  closeViewer();
});

describe('viewer store', () => {
  it('opens static content', () => {
    openViewer('Title', 'body');
    expect(get(viewer)).toEqual({ title: 'Title', content: 'body' });
  });

  it('opens edit previews with a safe hunks default', () => {
    openEditPreview('Patch', null);
    expect(get(viewer)).toEqual({ title: 'Patch', editPreview: true, hunks: [] });
  });

  it('opens live subagent viewers', () => {
    const generation = openSubagentViewer('Explore', 'session-1');
    expect(get(viewer)).toEqual({ title: 'Explore', sessionId: 'session-1', live: true, generation, gate: { highWater: 0 }, reading: true, transcriptReplay: true, pending: [], messages: [] });
  });

  it('opens subagent viewers with persisted messages', () => {
    const messages = [{ type: 'assistant', content: 'done' }];
    const generation = openSubagentViewer('Explore', 'session-1', messages);
    expect(get(viewer)).toEqual({ title: 'Explore', sessionId: 'session-1', live: true, generation, gate: { highWater: 0 }, reading: true, transcriptReplay: true, pending: [], messages });
  });

  it('hydrates the matching live subagent viewer with a state snapshot', () => {
    const generation = openSubagentViewer('Explore', 'session-1');
    hydrateSubagentViewer('session-2', { messages: [{ type: 'assistant', content: 'ignored' }] }, generation);
    expect(get(viewer).messages).toEqual([]);
    hydrateSubagentViewer('session-1', { messages: [{ type: 'assistant', content: 'history' }] }, generation);
    expect(get(viewer).messages).toEqual([{ type: 'assistant', content: 'history' }]);
  });

  it('does not discard live rows that arrive before persisted hydration returns', () => {
    const generation = openSubagentViewer('Explore', 'session-1');
    appendSubagentEvent('session-1', { type: 'token', seq: 2, content: 'live' });
    hydrateSubagentViewer('session-1', { messages: [{ type: 'assistant', content: 'history' }], cursor: { committedSeq: 1 } }, generation);
    expect(get(viewer).messages).toEqual([
      { type: 'assistant', content: 'history' },
      { type: 'assistant', content: 'live', partial: true, seq: 2 },
    ]);
  });

  it('does not duplicate persisted prefix on repeated hydration', () => {
    const history = [{ type: 'assistant', content: 'history' }];
    const generation = openSubagentViewer('Explore', 'session-1', history);
    appendSubagentEvent('session-1', { type: 'token', seq: 2, content: 'live' });
    hydrateSubagentViewer('session-1', { messages: history, cursor: { committedSeq: 1 } }, generation);
    expect(get(viewer).messages).toEqual([
      { type: 'assistant', content: 'history' },
      { type: 'assistant', content: 'live', partial: true, seq: 2 },
    ]);
  });

  it('replaces overlapping live partial rows when persisted hydration catches up', () => {
    const generation = openSubagentViewer('Explore', 'session-1');
    appendSubagentEvent('session-1', { type: 'token', seq: 1, content: 'done' });
    hydrateSubagentViewer('session-1', { messages: [{ type: 'assistant', content: 'done' }], cursor: { committedSeq: 1 } }, generation);
    expect(get(viewer).messages).toEqual([{ type: 'assistant', content: 'done' }]);
  });

  it('deduplicates live rows that overlap the end of persisted history', () => {
    const generation = openSubagentViewer('Explore', 'session-1');
    appendSubagentEvent('session-1', { type: 'token', seq: 2, content: 'live' });
    hydrateSubagentViewer('session-1', {
      messages: [
        { type: 'user', content: 'prompt' },
        { type: 'assistant', content: 'live' },
      ],
      cursor: { committedSeq: 2 },
    }, generation);
    expect(get(viewer).messages).toEqual([
      { type: 'user', content: 'prompt' },
      { type: 'assistant', content: 'live' },
    ]);
  });

  it('aggregates consecutive token events into one partial assistant message', () => {
    openSubagentViewer('Explore', 'session-1');
    appendSubagentEvent('session-1', { type: 'token', seq: 1, content: 'hello ' });
    appendSubagentEvent('session-1', { type: 'token', seq: 2, content: 'world' });
    expect(get(viewer).messages).toEqual([{ type: 'assistant', content: 'hello world', partial: true, seq: 2 }]);
  });

  it('finalizes partial assistant text when a tool starts', () => {
    openSubagentViewer('Explore', 'session-1');
    appendSubagentEvent('session-1', { type: 'token', seq: 1, content: 'checking' });
    appendSubagentEvent('session-1', { type: 'tool_start', seq: 2, id: 'call-1', name: 'read_file', args: '{}' });
    expect(get(viewer).messages).toEqual([
      { type: 'assistant', content: 'checking', partial: false, seq: 1 },
      { type: 'tool', id: 'call-1', name: 'read_file', args: '{}', done: false, success: true, result: '', seq: 2 },
    ]);
  });

  it('marks tool messages done when results arrive', () => {
    openSubagentViewer('Explore', 'session-1');
    appendSubagentEvent('session-1', { type: 'tool_start', seq: 1, id: 'call-1', name: 'read_file', args: '{}' });
    appendSubagentEvent('session-1', {
      type: 'tool_result',
      id: 'call-1',
      success: false,
      output: 'denied',
      metadata: { reason: 'test' },
    });
    expect(get(viewer).messages[0]).toEqual({
      type: 'tool',
      id: 'call-1',
      name: 'read_file',
      args: '{}',
      done: true,
      success: false,
      result: 'denied',
      metadata: { reason: 'test' },
      seq: 1,
    });
  });

  it('renders background process completions in live subagent viewers', () => {
    openSubagentViewer('Explore', 'session-1');
    appendSubagentEvent('session-1', { type: 'token', seq: 1, content: 'waiting' });
    appendSubagentEvent('session-1', {
      type: 'background_process_complete',
      seq: 2,
      id: 'bg-1',
      command: 'printf done',
      reason: 'completed',
      exitCode: 0,
      success: true,
      output: 'done',
    });
    expect(get(viewer).messages).toEqual([
      { type: 'assistant', content: 'waiting', partial: false, seq: 1 },
      {
        type: 'tool',
        id: 'bg-1',
        name: 'background_process',
        args: JSON.stringify({ id: 'bg-1', command: 'printf done', reason: 'completed', exitCode: 0, output: 'done' }),
        done: true,
        success: true,
        result: 'done',
        seq: 2,
      },
    ]);
  });

  it('renders user messages and system signals in live subagent viewers', () => {
    openSubagentViewer('Explore', 'session-1');
    appendSubagentEvent('session-1', { type: 'user_message', seq: 1, turn: 2, content: 'question' });
    appendSubagentEvent('session-1', { type: 'system_signal', seq: 2, content: 'System: interrupted' });
    expect(get(viewer).messages).toEqual([
      { type: 'user', content: 'question', turn: 2, seq: 1 },
      { type: 'system', content: 'System: interrupted', seq: 2 },
    ]);
  });

  it('ignores events for other sessions', () => {
    openSubagentViewer('Explore', 'session-1');
    appendSubagentEvent('session-2', { type: 'token', content: 'ignored' });
    expect(get(viewer).messages).toEqual([]);
  });

  it('closes the active viewer', () => {
    openViewer('Title', 'body');
    closeViewer();
    expect(get(viewer)).toBeNull();
  });
});

describe('child stream lifecycle', () => {
  it('applies the snapshot and rejects a replayed frame already in it', () => {
    const generation = openSubagentViewer('Explore', 'session-1');
    hydrateSubagentViewer('session-1', {
      messages: [{ type: 'assistant', content: 'done' }],
      cursor: { committedSeq: 3 },
    }, generation);
    appendSubagentEvent('session-1', { type: 'token', seq: 2, content: 'already in snapshot' });
    appendSubagentEvent('session-1', { type: 'token', seq: 4, content: 'new' });
    expect(get(viewer).messages).toEqual([
      { type: 'assistant', content: 'done' },
      { type: 'assistant', content: 'new', partial: true, seq: 4 },
    ]);
  });

  it('keeps live rows delivered during the hydration read, above the snapshot high-water', () => {
    const generation = openSubagentViewer('Explore', 'session-1');
    // Delivered while the backend read is in flight: the snapshot covers rows
    // through seq 1, so this delta (seq 2) coalesced onto the snapshot's open
    // assistant row in the coordinator and folds into it here.
    appendSubagentEvent('session-1', { type: 'token', seq: 2, content: 'live' });
    hydrateSubagentViewer('session-1', {
      messages: [
        { type: 'user', content: 'prompt' },
        { type: 'assistant', content: 'earlier' },
      ],
      tail: [{ seq: 1, message: { type: 'assistant', content: 'earlier' } }],
      cursor: { committedSeq: 0 },
    }, generation);
    expect(get(viewer).messages).toEqual([
      { type: 'user', content: 'prompt' },
      { type: 'assistant', content: 'earlierlive', partial: true, seq: 2 },
    ]);
  });

  // Opening a live child mid-response delivers its current assistant row
  // through the retained tail; that row is still streaming, so the next text
  // delta must continue it rather than start a second row.
  it('continues a live child in-flight assistant row from the snapshot', () => {
    const generation = openSubagentViewer('Explore', 'session-1');
    hydrateSubagentViewer('session-1', {
      messages: [{ type: 'assistant', content: 'thinking ' }],
      tail: [{ seq: 1, message: { type: 'assistant', content: 'thinking ' } }],
      cursor: { committedSeq: 0 },
    }, generation);
    appendSubagentEvent('session-1', { type: 'token', seq: 2, content: 'more' });
    expect(get(viewer).messages).toEqual([
      { type: 'assistant', content: 'thinking more', partial: true, seq: 2 },
    ]);
  });

  // A completed child resolves from its durable store with an empty tail:
  // nothing is streaming, so nothing is marked, and the next delta opens a
  // new row.
  it('marks nothing streaming for a completed child', () => {
    const generation = openSubagentViewer('Explore', 'session-1');
    hydrateSubagentViewer('session-1', {
      messages: [{ type: 'assistant', content: 'final answer' }],
      cursor: { committedSeq: 0 },
    }, generation);
    expect(get(viewer).messages).toEqual([{ type: 'assistant', content: 'final answer' }]);
    appendSubagentEvent('session-1', { type: 'token', seq: 1, content: 'new turn' });
    expect(get(viewer).messages).toEqual([
      { type: 'assistant', content: 'final answer' },
      { type: 'assistant', content: 'new turn', partial: true, seq: 1 },
    ]);
  });

  it('discards frames buffered before a terminal completed-child hydration', () => {
    const generation = openSubagentViewer('Explore', 'session-1');
    appendSubagentEvent('session-1', { type: 'token', seq: 2, content: 'stale live frame' });
    hydrateSubagentViewer('session-1', {
      messages: [{ type: 'assistant', content: 'durable answer' }],
      transcriptReplay: false,
      cursor: { committedSeq: 1 },
    }, generation);
    expect(get(viewer).messages).toEqual([{ type: 'assistant', content: 'durable answer' }]);
  });

  it('drops a sequenced frame delivered after terminal hydration resolves', async () => {
    const generation = openSubagentViewer('Explore', 'session-1');
    let resolveHydration;
    const hydration = new Promise(resolve => { resolveHydration = resolve; });
    const applied = hydration.then(state => hydrateSubagentViewer('session-1', state, generation));

    resolveHydration({
      messages: [{ type: 'assistant', content: 'durable answer' }],
      transcriptReplay: false,
      cursor: { committedSeq: 1 },
    });
    await applied;
    appendSubagentEvent('session-1', { type: 'token', seq: 2, content: 'late frame' });

    expect(get(viewer).messages).toEqual([{ type: 'assistant', content: 'durable answer' }]);
  });

  it('applies an above-cursor frame delivered after live hydration resolves', async () => {
    const generation = openSubagentViewer('Explore', 'session-1');
    let resolveHydration;
    const hydration = new Promise(resolve => { resolveHydration = resolve; });
    const applied = hydration.then(state => hydrateSubagentViewer('session-1', state, generation));

    resolveHydration({
      messages: [{ type: 'assistant', content: 'durable prefix' }],
      transcriptReplay: true,
      cursor: { committedSeq: 1 },
    });
    await applied;
    appendSubagentEvent('session-1', { type: 'token', seq: 2, content: 'live frame' });

    expect(get(viewer).messages).toEqual([
      { type: 'assistant', content: 'durable prefix' },
      { type: 'assistant', content: 'live frame', partial: true, seq: 2 },
    ]);
  });

  // A tool result is id-keyed and never gated, so the gate cannot protect it
  // when the snapshot replaces the state: a result received during the read
  // must be applied on top of the snapshot's running row, not discarded with
  // the live copy it updated.
  it('applies a tool result received during the read on top of the snapshot', () => {
    const generation = openSubagentViewer('Explore', 'session-1');
    // The snapshot captured the row while the tool was running; the result
    // arrives while the read is in flight and finds no row in the empty live
    // view, so only the snapshot apply can carry it.
    appendSubagentEvent('session-1', { type: 'tool_result', id: 'call-1', success: true, output: 'patched', metadata: { ok: true } });
    hydrateSubagentViewer('session-1', {
      messages: [{ type: 'tool', id: 'call-1', name: 'apply_patch', args: '{}', done: false, success: true, result: '' }],
      tail: [{ seq: 2, message: { type: 'tool', id: 'call-1', name: 'apply_patch', args: '{}', done: false, success: true, result: '' } }],
      cursor: { committedSeq: 2 },
    }, generation);
    const row = get(viewer).messages.find((m) => m.type === 'tool' && m.id === 'call-1');
    expect(row.done).toBe(true);
    expect(row.success).toBe(true);
    expect(row.result).toBe('patched');
  });

  // The same rule when both the start and the result arrived during the read:
  // the replayed start appends the row, the replayed result updates it.
  it('applies a tool result received during the read when its start also arrived during the read', () => {
    const generation = openSubagentViewer('Explore', 'session-1');
    appendSubagentEvent('session-1', { type: 'tool_start', seq: 2, id: 'call-1', name: 'read_file', args: '{}' });
    appendSubagentEvent('session-1', { type: 'tool_result', id: 'call-1', success: false, output: 'denied' });
    hydrateSubagentViewer('session-1', {
      messages: [
        { type: 'assistant', content: 'later' },
        { type: 'tool', id: 'call-1', name: 'read_file', args: '{}', done: false, success: true, result: '' },
      ],
      cursor: { committedSeq: 0 },
    }, generation);
    const row = get(viewer).messages.find((m) => m.type === 'tool' && m.id === 'call-1');
    expect(row.done).toBe(true);
    expect(row.success).toBe(false);
    expect(row.result).toBe('denied');
  });

  it('does not duplicate a turn returned by both halves of a snapshot', () => {
    const generation = openSubagentViewer('Explore', 'session-1');
    appendSubagentEvent('session-1', { type: 'token', seq: 1, content: 'window' });
    // The durable half already carries the turn and the cursor was raised over
    // the dropped tail, so the live row must be replaced by the snapshot copy.
    hydrateSubagentViewer('session-1', {
      messages: [{ type: 'assistant', content: 'window' }],
      cursor: { committedSeq: 1 },
    }, generation);
    expect(get(viewer).messages).toEqual([{ type: 'assistant', content: 'window' }]);
  });

  it('applies a repeated hydration idempotently', () => {
    const generation = openSubagentViewer('Explore', 'session-1');
    const state = { messages: [{ type: 'assistant', content: 'history' }], cursor: { committedSeq: 1 } };
    hydrateSubagentViewer('session-1', state, generation);
    hydrateSubagentViewer('session-1', state, generation);
    expect(get(viewer).messages).toEqual([{ type: 'assistant', content: 'history' }]);
  });

  // The strand case that carves tool results out of the gate: a result arriving
  // after later rows advanced the high-water still renders, because the result
  // updates its row in place without advancing the sequence. A uniformly-gated
  // implementation would drop it as stale and strand the row at "running".
  it('renders a tool result that arrives after later rows advanced the high-water', () => {
    const generation = openSubagentViewer('Explore', 'session-1');
    appendSubagentEvent('session-1', { type: 'tool_start', seq: 2, id: 'call-1', name: 'read_file', args: '{}' });
    hydrateSubagentViewer('session-1', {
      messages: [
        { type: 'assistant', content: 'later' },
        { type: 'tool', id: 'call-1', name: 'read_file', args: '{}', done: false, success: true, result: '' },
      ],
      tail: [{ seq: 2, message: { type: 'tool', id: 'call-1', name: 'read_file', args: '{}', done: false, success: true, result: '' } }],
      cursor: { committedSeq: 5 },
    }, generation);
    appendSubagentEvent('session-1', { type: 'tool_result', seq: 2, id: 'call-1', success: false, output: 'denied', metadata: { reason: 'test' } });
    const row = get(viewer).messages.find((m) => m.type === 'tool' && m.id === 'call-1');
    expect(row.done).toBe(true);
    expect(row.success).toBe(false);
    expect(row.result).toBe('denied');
  });

  // An apply_patch child tool call emits two ends on the same row (stage-time
  // and real result); the last one wins. Both carry the row's original
  // sequence, so a uniformly-gated implementation would drop both.
  it('renders the last of two apply_patch ends', () => {
    const generation = openSubagentViewer('Explore', 'session-1');
    appendSubagentEvent('session-1', { type: 'tool_start', seq: 2, id: 'call-1', name: 'apply_patch', args: '{}' });
    hydrateSubagentViewer('session-1', {
      messages: [{ type: 'tool', id: 'call-1', name: 'apply_patch', args: '{}', done: false, success: true, result: '' }],
      tail: [{ seq: 2, message: { type: 'tool', id: 'call-1', name: 'apply_patch', args: '{}', done: false, success: true, result: '' } }],
      cursor: { committedSeq: 5 },
    }, generation);
    appendSubagentEvent('session-1', { type: 'tool_result', seq: 2, id: 'call-1', success: true, output: 'staged' });
    appendSubagentEvent('session-1', { type: 'tool_result', seq: 2, id: 'call-1', success: true, output: 'real result' });
    const row = get(viewer).messages.find((m) => m.type === 'tool' && m.id === 'call-1');
    expect(row.done).toBe(true);
    expect(row.result).toBe('real result');
  });

  it('gates sequenced frames per child session', () => {
    const generation = openSubagentViewer('Explore', 'session-1');
    hydrateSubagentViewer('session-1', { messages: [], cursor: { committedSeq: 1 } }, generation);
    // A frame for another live child must not leak into this viewer, and its
    // own gate must not advance this child's.
    appendSubagentEvent('session-2', { type: 'token', seq: 2, content: 'other child' });
    expect(get(viewer).messages).toEqual([]);
  });

  // Closing and reopening the same child while the first open's hydration read
  // is still in flight must not let the older hydration resolve into the newer
  // open: it names the same session id, so only the generation can tell them
  // apart. The stale read would seed the gate from its old cursor and consume
  // the newer open's pending frames.
  it('ignores a stale hydration for an earlier open of the same child', () => {
    const staleGeneration = openSubagentViewer('Explore', 'session-1');
    closeViewer();
    openSubagentViewer('Explore', 'session-1');
    appendSubagentEvent('session-1', { type: 'token', seq: 2, content: 'live' });
    hydrateSubagentViewer('session-1', { messages: [{ type: 'assistant', content: 'stale' }], cursor: { committedSeq: 5 } }, staleGeneration);
    expect(get(viewer).messages).toEqual([{ type: 'assistant', content: 'live', partial: true, seq: 2 }]);
    // The newer open's gate must survive the rejected read: a frame above its
    // own high-water still streams, where a stale-seeded gate would drop it.
    appendSubagentEvent('session-1', { type: 'token', seq: 3, content: ' still here' });
    expect(get(viewer).messages).toEqual([{ type: 'assistant', content: 'live still here', partial: true, seq: 3 }]);
  });

  // The gate and the pending buffer live on the viewer object itself, not in
  // module-level per-session maps: a fresh open owns its own gate, read
  // window, and buffer; buffered frames live on that object; closing drops
  // the object with them; and a new open owns distinct objects/arrays with no
  // old pending state. This fails against the module-map design, which keeps
  // the state off the object entirely (no gate/reading/pending fields and no
  // per-open identity).
  it('owns the child stream state on the viewer object itself', () => {
    // A fresh open owns its own gate, read window, and buffer.
    const firstGen = openSubagentViewer('Explore', 'session-1');
    const first = get(viewer);
    expect(first.gate).toEqual({ highWater: 0 });
    expect(first.reading).toBe(true);
    expect(first.pending).toEqual([]);

    // Buffered frames live on that object: the same viewer object carries
    // them in its own pending array while the read is in flight.
    appendSubagentEvent('session-1', { type: 'token', seq: 2, content: 'buffered' });
    const buffered = get(viewer);
    expect(buffered.pending).toEqual([{ type: 'token', seq: 2, content: 'buffered' }]);
    expect(buffered.messages).toEqual([{ type: 'assistant', content: 'buffered', partial: true, seq: 2 }]);

    // Close drops the object; the old gate and buffer go with it.
    closeViewer();
    expect(get(viewer)).toBeNull();

    // A new open owns distinct objects and arrays, with no old pending state.
    const secondGen = openSubagentViewer('Explore', 'session-1');
    const second = get(viewer);
    expect(secondGen).toBe(firstGen + 1);
    expect(second.gate).not.toBe(first.gate);
    expect(second.pending).not.toBe(buffered.pending);
    expect(second.reading).toBe(true);
    expect(second.pending).toEqual([]);
    expect(second.messages).toEqual([]);

    // The new open's own buffer takes its own frames; the old buffered frame
    // never reappears.
    appendSubagentEvent('session-1', { type: 'token', seq: 1, content: 'fresh' });
    expect(get(viewer).pending).toEqual([{ type: 'token', seq: 1, content: 'fresh' }]);
    expect(get(viewer).messages).toEqual([{ type: 'assistant', content: 'fresh', partial: true, seq: 1 }]);
  });
});
