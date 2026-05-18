import { get } from 'svelte/store';
import { beforeEach, describe, expect, it } from 'vitest';

import {
  appendSubagentEvent,
  closeViewer,
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
