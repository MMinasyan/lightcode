import { describe, expect, it } from 'vitest';

import {
  mergeSubagentLinks,
  rememberSubagentLink,
  subagentLinksFromMetadata,
  takePendingSubagentLinks,
} from './subagentLinks.js';

describe('subagent link helpers', () => {
  it('stores child session links before the task row exists and consumes them later', () => {
    let pending = {};
    pending = rememberSubagentLink(pending, 'task-call', { index: 0, sessionId: 'child-1' });
    pending = rememberSubagentLink(pending, 'task-call', { index: 1, sessionId: 'child-2' });

    const result = takePendingSubagentLinks(pending, 'task-call');
    expect(result.links).toEqual([
      { index: 0, sessionId: 'child-1' },
      { index: 1, sessionId: 'child-2' },
    ]);
    expect(result.pending).toEqual({});
  });

  it('deduplicates repeated live starts and persisted metadata links', () => {
    expect(mergeSubagentLinks(
      [{ index: 0, sessionId: 'child-1' }],
      [{ index: 0, sessionId: 'child-1' }],
      [{ index: 1, sessionId: 'child-2' }],
    )).toEqual([
      { index: 0, sessionId: 'child-1' },
      { index: 1, sessionId: 'child-2' },
    ]);
  });

  it('recovers persisted links from snake-case and camel-case metadata', () => {
    expect(subagentLinksFromMetadata({
      subagent_session_ids: [{ index: 0, sessionId: 'child-1' }],
    })).toEqual([{ index: 0, sessionId: 'child-1' }]);

    expect(subagentLinksFromMetadata({
      subagentSessionIds: [{ index: 1, session_id: 'child-2' }],
    })).toEqual([{ index: 1, sessionId: 'child-2' }]);
  });
});
