import { writable } from 'svelte/store';
import { admitSequenced, snapshotHighWater, snapshotMessages } from './hydration.js';

// viewer holds the currently displayed full-screen content.
// Shape: { title, content } | { title, editPreview, hunks } | { title, sessionId, live, generation, messages } | null
export const viewer = writable(null);

// nextSubagentGeneration numbers every open of a child viewer. Each open
// stores its value on the viewer object and returns it to the caller, so a
// hydration read that resolves after the viewer was closed and reopened for
// the same child can be told apart from the newer open's own read: the id
// alone no longer identifies the open.
let nextSubagentGeneration = 0;

// childGates holds one transcript gate per child session id, seeded from the
// cursor delivered with the child's snapshot and advanced by every admitted
// sequenced frame. It is a map, not a per-open value, so a child's gate stays
// consistent across opens and switches while its run is in flight.
const childGates = new Map();

function gateFor(sessionId) {
  let gate = childGates.get(sessionId);
  if (!gate) {
    gate = { highWater: 0 };
    childGates.set(sessionId, gate);
  }
  return gate;
}

// pendingFrames holds, per child session, the frames received while a
// hydration read is in flight (between opening the viewer and applying its
// snapshot). They are replayed on top of the snapshot at apply time, each by
// its own kind's rules, so nothing delivered during the read is lost when the
// snapshot replaces the state.
const pendingFrames = new Map();

export function openViewer(title, content) {
  viewer.set({ title, content });
}

export function openEditPreview(title, hunks) {
  viewer.set({ title, editPreview: true, hunks: hunks || [] });
}

export function openSubagentViewer(title, sessionId, messages = []) {
  // A fresh open starts the child's gate at zero and opens its read window:
  // frames delivered until the snapshot applies are held for replay. The
  // hydration read that follows re-seeds the gate from the snapshot's cursor.
  // The generation tags this open; the caller passes it back with the
  // hydration result so an earlier open's read cannot resolve into this one.
  const generation = ++nextSubagentGeneration;
  childGates.delete(sessionId);
  pendingFrames.set(sessionId, []);
  viewer.set({ title, sessionId, live: true, generation, messages: messages || [] });
  return generation;
}

// hydrateSubagentViewer applies a child's complete-state snapshot (durable
// messages plus cursor, as returned by the child hydration read) to the
// matching live viewer: the snapshot's rows render once, the gate is seeded
// from its cursor, and every frame received while the read was in flight is
// replayed on top of the snapshot through the same per-kind rules as live
// delivery — sequenced kinds through the gate (those already in the snapshot
// are dropped), tool results by id-keyed idempotent apply (they update a row
// in place without advancing the sequence, so the gate cannot protect them).
// generation is the value openSubagentViewer returned for the open this read
// belongs to; when the viewer was closed and reopened for the same child in
// the meantime, the read is stale and applies nothing.
export function hydrateSubagentViewer(sessionId, state, generation) {
  viewer.update(v => {
    if (!v || !v.live || v.sessionId !== sessionId) return v;
    if (v.generation !== generation) return v;
    const gate = gateFor(sessionId);
    gate.highWater = snapshotHighWater(state || {});
    const messages = snapshotMessages(state || {});
    // A live child's retained tail holds the current turn's rows and the
    // commit that clears it has not run, so a trailing assistant row that came
    // from the tail is still streaming: mark it so a replayed or subsequent
    // text delta continues the row instead of opening a second one. A
    // completed child resolves with an empty tail, so nothing is marked.
    if (state?.tail?.length > 0 && messages.length > 0 && messages[messages.length - 1].type === 'assistant') {
      messages[messages.length - 1] = { ...messages[messages.length - 1], partial: true };
    }
    const pending = pendingFrames.get(sessionId) || [];
    pendingFrames.delete(sessionId);
    let out = messages;
    for (const frame of pending) {
      out = applyFrame(out, frame, gate);
    }
    return { ...v, messages: out };
  });
}

// sequencedKinds are the child transcript kinds the gate covers. Tool results
// are deliberately not gated: EventToolCallEnd is the only kind that updates an
// existing row without advancing its sequence, so a gate would strand the row
// at "running" once its start advanced the high-water. They are applied
// id-keyed and idempotently, like the root branch.
const sequencedKinds = new Set(['token', 'tool_start', 'background_process_complete', 'user_message', 'system_signal']);

// applyFrame applies one delivered child frame to a message list, returning the
// new list. It is the single per-kind apply shared by live delivery and by the
// snapshot replay, so both paths fold rows identically.
function applyFrame(messages, event, gate) {
  if (sequencedKinds.has(event.type) && !admitSequenced(gate, event.seq)) return messages;
  const msgs = [...messages];
  if (event.type === 'token') {
    const last = msgs[msgs.length - 1];
    if (last && last.type === 'assistant' && last.partial) {
      msgs[msgs.length - 1] = { ...last, content: last.content + event.content, seq: event.seq };
    } else {
      msgs.push({ type: 'assistant', content: event.content, partial: true, seq: event.seq });
    }
  } else if (event.type === 'tool_start') {
    const last = msgs[msgs.length - 1];
    if (last && last.type === 'assistant' && last.partial) {
      msgs[msgs.length - 1] = { ...last, partial: false };
    }
    msgs.push({ type: 'tool', id: event.id, name: event.name, args: event.args, done: false, success: true, result: '', seq: event.seq });
  } else if (event.type === 'tool_result') {
    const idx = msgs.findIndex(m => m.type === 'tool' && m.id === event.id);
    if (idx >= 0) msgs[idx] = { ...msgs[idx], done: true, success: event.success, result: event.output, name: event.name || msgs[idx].name, args: event.args || msgs[idx].args, metadata: event.metadata || msgs[idx].metadata };
  } else if (event.type === 'background_process_complete') {
    const last = msgs[msgs.length - 1];
    if (last && last.type === 'assistant' && last.partial) {
      msgs[msgs.length - 1] = { ...last, partial: false };
    }
    const bg = {
      id: event.id || '',
      command: event.command || '',
      reason: event.reason || '',
      exitCode: event.exitCode ?? 0,
      output: event.output || '',
    };
    msgs.push({
      type: 'tool',
      id: bg.id,
      name: 'background_process',
      args: JSON.stringify(bg),
      done: true,
      success: event.success !== false,
      result: bg.output,
      seq: event.seq,
    });
  } else if (event.type === 'user_message') {
    const last = msgs[msgs.length - 1];
    if (last && last.type === 'assistant' && last.partial) {
      msgs[msgs.length - 1] = { ...last, partial: false };
    }
    msgs.push({ type: 'user', content: event.content || '', turn: event.turn || 0, seq: event.seq });
  } else if (event.type === 'system_signal') {
    const last = msgs[msgs.length - 1];
    if (last && last.type === 'assistant' && last.partial) {
      msgs[msgs.length - 1] = { ...last, partial: false };
    }
    msgs.push({ type: 'system', content: event.content || '', seq: event.seq });
  }
  return msgs;
}

export function appendSubagentEvent(sessionId, event) {
  viewer.update(v => {
    if (!v || !v.live || v.sessionId !== sessionId) return v;
    const pending = pendingFrames.get(sessionId);
    if (pending) pending.push(event);
    return { ...v, messages: applyFrame(v.messages, event, gateFor(sessionId)) };
  });
}

export function closeViewer() {
  viewer.set(null);
}
