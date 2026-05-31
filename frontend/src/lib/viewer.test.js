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
    openSubagentViewer('Explore', 'session-1');
    expect(get(viewer)).toEqual({ title: 'Explore', sessionId: 'session-1', live: true, messages: [] });
  });

  it('opens subagent viewers with persisted messages', () => {
    const messages = [{ type: 'assistant', content: 'done' }];
    openSubagentViewer('Explore', 'session-1', messages);
    expect(get(viewer)).toEqual({ title: 'Explore', sessionId: 'session-1', live: true, messages });
  });

  it('hydrates the matching live subagent viewer with persisted messages', () => {
    openSubagentViewer('Explore', 'session-1');
    hydrateSubagentViewer('session-2', [{ type: 'assistant', content: 'ignored' }]);
    expect(get(viewer).messages).toEqual([]);
    hydrateSubagentViewer('session-1', [{ type: 'assistant', content: 'history' }]);
    expect(get(viewer).messages).toEqual([{ type: 'assistant', content: 'history' }]);
  });

  it('does not discard live rows that arrive before persisted hydration returns', () => {
    openSubagentViewer('Explore', 'session-1');
    appendSubagentEvent('session-1', { type: 'token', content: 'live' });
    hydrateSubagentViewer('session-1', [{ type: 'assistant', content: 'history' }]);
    expect(get(viewer).messages).toEqual([
      { type: 'assistant', content: 'history' },
      { type: 'assistant', content: 'live', partial: true },
    ]);
  });

  it('does not duplicate persisted prefix on repeated hydration', () => {
    const history = [{ type: 'assistant', content: 'history' }];
    openSubagentViewer('Explore', 'session-1', history);
    appendSubagentEvent('session-1', { type: 'token', content: 'live' });
    hydrateSubagentViewer('session-1', history);
    expect(get(viewer).messages).toEqual([
      { type: 'assistant', content: 'history' },
      { type: 'assistant', content: 'live', partial: true },
    ]);
  });

  it('replaces overlapping live partial rows when persisted hydration catches up', () => {
    openSubagentViewer('Explore', 'session-1');
    appendSubagentEvent('session-1', { type: 'token', content: 'done' });
    hydrateSubagentViewer('session-1', [{ type: 'assistant', content: 'done' }]);
    expect(get(viewer).messages).toEqual([{ type: 'assistant', content: 'done' }]);
  });

  it('deduplicates live rows that overlap the end of persisted history', () => {
    openSubagentViewer('Explore', 'session-1');
    appendSubagentEvent('session-1', { type: 'token', content: 'live' });
    hydrateSubagentViewer('session-1', [
      { type: 'user', content: 'prompt' },
      { type: 'assistant', content: 'live' },
    ]);
    expect(get(viewer).messages).toEqual([
      { type: 'user', content: 'prompt' },
      { type: 'assistant', content: 'live' },
    ]);
  });

  it('aggregates consecutive token events into one partial assistant message', () => {
    openSubagentViewer('Explore', 'session-1');
    appendSubagentEvent('session-1', { type: 'token', content: 'hello ' });
    appendSubagentEvent('session-1', { type: 'token', content: 'world' });
    expect(get(viewer).messages).toEqual([{ type: 'assistant', content: 'hello world', partial: true }]);
  });

  it('finalizes partial assistant text when a tool starts', () => {
    openSubagentViewer('Explore', 'session-1');
    appendSubagentEvent('session-1', { type: 'token', content: 'checking' });
    appendSubagentEvent('session-1', { type: 'tool_start', id: 'call-1', name: 'read_file', args: '{}' });
    expect(get(viewer).messages).toEqual([
      { type: 'assistant', content: 'checking', partial: false },
      { type: 'tool', id: 'call-1', name: 'read_file', args: '{}', done: false, success: true, result: '' },
    ]);
  });

  it('marks tool messages done when results arrive', () => {
    openSubagentViewer('Explore', 'session-1');
    appendSubagentEvent('session-1', { type: 'tool_start', id: 'call-1', name: 'read_file', args: '{}' });
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
    });
  });

  it('renders background process completions in live subagent viewers', () => {
    openSubagentViewer('Explore', 'session-1');
    appendSubagentEvent('session-1', { type: 'token', content: 'waiting' });
    appendSubagentEvent('session-1', {
      type: 'background_process_complete',
      id: 'bg-1',
      command: 'printf done',
      reason: 'completed',
      exitCode: 0,
      success: true,
      output: 'done',
    });
    expect(get(viewer).messages).toEqual([
      { type: 'assistant', content: 'waiting', partial: false },
      {
        type: 'tool',
        id: 'bg-1',
        name: 'background_process',
        args: JSON.stringify({ id: 'bg-1', command: 'printf done', reason: 'completed', exitCode: 0, output: 'done' }),
        done: true,
        success: true,
        result: 'done',
      },
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
