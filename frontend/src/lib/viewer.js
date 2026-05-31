import { writable } from 'svelte/store';

// viewer holds the currently displayed full-screen content.
// Shape: { title, content } | { title, editPreview, hunks } | { title, sessionId, live, messages } | null
export const viewer = writable(null);

export function openViewer(title, content) {
  viewer.set({ title, content });
}

export function openEditPreview(title, hunks) {
  viewer.set({ title, editPreview: true, hunks: hunks || [] });
}

export function openSubagentViewer(title, sessionId, messages = []) {
  viewer.set({ title, sessionId, live: true, messages: messages || [] });
}

export function hydrateSubagentViewer(sessionId, messages) {
  viewer.update(v => {
    if (!v || !v.live || v.sessionId !== sessionId) return v;
    return { ...v, messages: mergeHydratedMessages(v.messages || [], messages || []) };
  });
}

function mergeHydratedMessages(existing, persisted) {
  if (existing.length === 0) return persisted;
  if (persisted.length === 0) return existing;
  if (startsWithMessages(persisted, existing)) return persisted;
  if (startsWithMessages(existing, persisted)) return existing;
  const overlap = suffixPrefixOverlap(persisted, existing);
  if (overlap > 0) return [...persisted, ...existing.slice(overlap)];
  return [...persisted, ...existing];
}

function startsWithMessages(messages, prefix) {
  if (prefix.length > messages.length) return false;
  for (let i = 0; i < prefix.length; i++) {
    if (!sameHydratedMessage(messages[i], prefix[i])) return false;
  }
  return true;
}

function suffixPrefixOverlap(left, right) {
  const max = Math.min(left.length, right.length);
  for (let size = max; size > 0; size--) {
    let matches = true;
    for (let i = 0; i < size; i++) {
      if (!sameHydratedMessage(left[left.length - size + i], right[i])) {
        matches = false;
        break;
      }
    }
    if (matches) return size;
  }
  return 0;
}

function sameHydratedMessage(a, b) {
  if (!a || !b || a.type !== b.type) return false;
  if (a.type === 'assistant') return (a.content || '') === (b.content || '');
  if (a.type === 'tool') {
    if (a.id || b.id) return a.id === b.id;
    return (a.name || '') === (b.name || '') && (a.args || '') === (b.args || '');
  }
  return JSON.stringify(a) === JSON.stringify(b);
}

export function appendSubagentEvent(sessionId, event) {
  viewer.update(v => {
    if (!v || !v.live || v.sessionId !== sessionId) return v;
    const msgs = [...v.messages];
    if (event.type === 'token') {
      const last = msgs[msgs.length - 1];
      if (last && last.type === 'assistant' && last.partial) {
        msgs[msgs.length - 1] = { ...last, content: last.content + event.content };
      } else {
        msgs.push({ type: 'assistant', content: event.content, partial: true });
      }
    } else if (event.type === 'tool_start') {
      const last = msgs[msgs.length - 1];
      if (last && last.type === 'assistant' && last.partial) {
        msgs[msgs.length - 1] = { ...last, partial: false };
      }
      msgs.push({ type: 'tool', id: event.id, name: event.name, args: event.args, done: false, success: true, result: '' });
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
      });
    }
    return { ...v, messages: msgs };
  });
}

export function closeViewer() {
  viewer.set(null);
}
