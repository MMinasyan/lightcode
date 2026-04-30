import { writable } from 'svelte/store';

// viewer holds the currently displayed full-screen content.
// Shape: { title, content } | { title, sessionId, live, messages } | null
export const viewer = writable(null);

export function openViewer(title, content) {
  viewer.set({ title, content });
}

export function openSubagentViewer(title, sessionId) {
  viewer.set({ title, sessionId, live: true, messages: [] });
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
      if (idx >= 0) msgs[idx] = { ...msgs[idx], done: true, success: event.success, result: event.output };
    }
    return { ...v, messages: msgs };
  });
}

export function closeViewer() {
  viewer.set(null);
}
