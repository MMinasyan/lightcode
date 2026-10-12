// The actual desktop reader, driven through a scripted generated transport:
// A-to-B supersession, the stale page below the one floor, terminal 1-50 plus
// fresh 52-101 bridged by 2-51, several missing pages, unresolved IDs beyond
// the first overlap, oldest cursor retention, hints during the chain, the
// newer-mid-chain floor behavior, rejection, repair and ordinary-error rules,
// chain draining, lifecycle/orphan suppression, invalid item identity and the
// merged-geometry cursor rules. No test-only replica of the chain exists.
import { describe, expect, it, vi } from 'vitest';

import type {
  AssistantItem,
  ConversationItem,
  HistoryPage,
  Hydration,
  SessionRevision,
} from '../generated/protocol/types.gen';
import {
  SessionReader,
  failRead,
  okRead,
  unresolvedAssistantIDs,
  type ReaderOutcome,
} from './reader';

const INSTANCE = '0123456789abcdef0123456789abcdef';

function revision(durable: string, local = '0', instance = INSTANCE): SessionRevision {
  return { instance_id: instance, durable_revision: durable, local_revision: local };
}

function inputItem(id: string): ConversationItem {
  return {
    item_id: id,
    kind: 'input',
    committed_at: '0',
    origin: 'user',
    content: [{ kind: 'text', text: `item ${id}` }],
  };
}

function assistantItem(id: string, pendingCall = false): ConversationItem {
  return {
    item_id: id,
    kind: 'assistant',
    committed_at: '0',
    status: 'completed',
    source: 'provider/model',
    content: [{ kind: 'text', text: `assistant ${id}` }],
    tool_calls: pendingCall
      ? [{ id: `call-${id}`, name: 'tool', arguments: '{}' }]
      : [{ id: `call-${id}`, name: 'tool', arguments: '{}', status: 'success' }],
  } satisfies AssistantItem;
}

function range(start: number, end: number): ConversationItem[] {
  const items: ConversationItem[] = [];
  for (let i = start; i <= end; i += 1) {
    items.push(inputItem(`i${i}`));
  }
  return items;
}

function hydration(revisionValue: SessionRevision, items: ConversationItem[], olderCursor: string | null, extra: Partial<Hydration> = {}): Hydration {
  return {
    session: {
      session_id: 's1',
      workspace: '/ws',
      lifecycle: 'open',
      agent_type: 'primary',
      created_at: '0',
      last_activity: '0',
      session_revision: revisionValue,
    },
    selected_model: null,
    session_revision: revisionValue,
    configuration_revision: { instance_id: revisionValue.instance_id, generation: '1' },
    warnings_revision: { instance_id: revisionValue.instance_id, revision: '1' },
    operations: [],
    pending: { steering: [], queued: [] },
    conversation: olderCursor === null ? { items } : { items, older_cursor: olderCursor },
    usage: { totals: { by_model: [] }, context: { used_tokens: '0', context_window: 0 } },
    warnings: [],
    background: [],
    ...extra,
  };
}

function page(revisionValue: SessionRevision, items: ConversationItem[], olderCursor: string | null): HistoryPage {
  return olderCursor === null ? { session_revision: revisionValue, items } : { session_revision: revisionValue, items, older_cursor: olderCursor };
}

function ids(items: ConversationItem[]): string[] {
  return items.map((item) => item.item_id);
}

interface Harness {
  reader: SessionReader;
  hydrationMock: ReturnType<typeof vi.fn>;
  historyMock: ReturnType<typeof vi.fn>;
  stamp: { instance: string; generation: string; selection: number; sessionID: string };
  repairMock: ReturnType<typeof vi.fn>;
  adopted: Hydration[];
  refresh: () => Promise<void>;
  settle: () => Promise<void>;
}

function makeHarness(): Harness {
  const hydrationMock = vi.fn();
  const historyMock = vi.fn();
  const repairMock = vi.fn();
  const adopted: Hydration[] = [];
  const stamp = { instance: INSTANCE, generation: '1', selection: 0, sessionID: 's1' };
  // The scripted mocks speak the reader's own ReaderOutcome contract directly:
  // okRead/failRead values are passed through with no value conversion, no
  // rejected-promise encoding and no marker objects.
  const transport = {
    hydration: (captured: { sessionID: string }) => hydrationMock(captured),
    history: (captured: { sessionID: string }, cursor: string) => historyMock(captured, cursor),
  };
  const reader = new SessionReader(
    transport,
    () => ({ ...stamp }),
    repairMock,
    () => {},
    (value) => adopted.push(value),
  );
  async function settle(): Promise<void> {
    await vi.waitFor(() => {
      expect(reader.isBusy()).toBe(false);
    });
    await new Promise((resolve) => setTimeout(resolve, 0));
  }
  async function refresh(): Promise<void> {
    reader.markDirty();
    await settle();
  }
  return {
    reader,
    hydrationMock,
    historyMock,
    stamp,
    repairMock,
    adopted,
    refresh,
    settle,
  };
}

async function flush(): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, 0));
}

describe('unresolvedAssistantIDs', () => {
  it('collects only assistant items with a pending tool call', () => {
    const items = [
      inputItem('a'),
      assistantItem('b', true),
      assistantItem('c', false),
      assistantItem('d', true),
    ];
    expect(unresolvedAssistantIDs(items)).toEqual(['b', 'd']);
  });
});

describe('SessionReader hydration adoption', () => {
  it('adopts the first hydration, its page, floor and oldest cursor', async () => {
    const harness = makeHarness();
    harness.hydrationMock.mockResolvedValue(okRead(hydration(revision('1'), range(1, 5), 'c1')));
    await harness.refresh();
    expect(ids(harness.reader.getItems())).toEqual(['i1', 'i2', 'i3', 'i4', 'i5']);
    expect(harness.reader.getHydration()).not.toBeNull();
    expect(harness.reader.getFloor()).toEqual(revision('1'));
    expect(harness.reader.getOldestCursor()).toBe('c1');
    expect(harness.reader.hasOlder()).toBe(true);
    expect(harness.reader.getError()).toBeNull();
    expect(harness.repairMock).not.toHaveBeenCalled();
  });

  it('accepts an equal session revision with newer warnings', async () => {
    const harness = makeHarness();
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(1, 3), null, {
        warnings_revision: { instance_id: INSTANCE, revision: '1' },
      })))
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(1, 4), null, {
        warnings_revision: { instance_id: INSTANCE, revision: '2' },
      })));
    await harness.refresh();
    expect(harness.reader.hint({ instance_id: INSTANCE, revision: '2' }, 'warning')).toBe('dirty');
    await harness.settle();
    expect(ids(harness.reader.getItems())).toEqual(['i1', 'i2', 'i3', 'i4']);
    expect(harness.hydrationMock).toHaveBeenCalledTimes(2);
    expect(harness.adopted).toHaveLength(2);
  });

  it('rejects a whole stale hydration even when its configuration clock is newer', async () => {
    const harness = makeHarness();
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(1, 3), null)))
      .mockResolvedValueOnce(okRead(hydration(revision('1'), range(90, 99), null, {
        configuration_revision: { instance_id: INSTANCE, generation: '9' },
        warnings_revision: { instance_id: INSTANCE, revision: '9' },
      })))
      .mockResolvedValueOnce(okRead(hydration(revision('3'), range(1, 4), null)));
    await harness.refresh();
    await harness.refresh();
    // The stale body was rejected whole: its page, its queue and its clocks
    // never replaced the accepted projection; the fresh read adopted.
    expect(ids(harness.reader.getItems())).toEqual(['i1', 'i2', 'i3', 'i4']);
    expect(harness.reader.getFloor()).toEqual(revision('3'));
    expect(harness.adopted.map((value) => value.session_revision.durable_revision)).toEqual(['2', '3']);
  });

  it('rejects a hydration below the conversation floor with no chain-start exemption', async () => {
    const harness = makeHarness();
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(1, 3), null)))
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(1, 3), null)))
      .mockResolvedValueOnce(okRead(hydration(revision('1'), range(1, 3), null)))
      .mockResolvedValueOnce(okRead(hydration(revision('3'), range(1, 5), null)));
    await harness.refresh();
    await harness.refresh();
    await harness.refresh();
    // The below-floor rev-1 body was rejected whole and the fresh rev-3 read
    // adopted; the equal rev-2 refresh was legal.
    expect(ids(harness.reader.getItems())).toEqual(['i1', 'i2', 'i3', 'i4', 'i5']);
    expect(harness.adopted.map((value) => value.session_revision.durable_revision)).toEqual(['2', '2', '3']);
  });
});

describe('SessionReader bridge walk', () => {
  it('joins a terminal 1-50 cache with a fresh 52-101 page bridged by 2-51 into 1-101', async () => {
    const harness = makeHarness();
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('1'), range(1, 50), 'c1')))
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(52, 101), 'c2')));
    harness.historyMock.mockResolvedValue(okRead(page(revision('2'), range(2, 51), null)));
    await harness.refresh();
    await harness.refresh();
    expect(harness.historyMock).toHaveBeenCalledExactlyOnceWith(expect.anything(), 'c2');
    expect(ids(harness.reader.getItems())).toEqual(range(1, 101).map((item) => item.item_id));
    expect(harness.reader.getFloor()).toEqual(revision('2'));
    // The walk extended the newest edge; the oldest-edge cursor is preserved.
    expect(harness.reader.getOldestCursor()).toBe('c1');
  });

  it('bridges several missing pages in server order', async () => {
    const harness = makeHarness();
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('1'), range(1, 50), 'c1')))
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(80, 101), 'cA')));
    harness.historyMock
      .mockResolvedValueOnce(okRead(page(revision('2'), range(60, 79), 'cB')))
      .mockResolvedValueOnce(okRead(page(revision('2'), range(2, 59), null)));
    await harness.refresh();
    await harness.refresh();
    expect(harness.historyMock.mock.calls.map((call) => call[1])).toEqual(['cA', 'cB']);
    expect(ids(harness.reader.getItems())).toEqual(range(1, 101).map((item) => item.item_id));
  });

  it('walks past the anchor until every retained unresolved assistant ID is encountered', async () => {
    const harness = makeHarness();
    const cached = [...range(1, 40), assistantItem('i41', true), ...range(42, 50)];
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('1'), cached, 'c1')))
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(45, 101), 'c2')));
    harness.historyMock.mockResolvedValue(okRead(page(revision('2'), [assistantItem('i41', false), ...range(42, 44)], null)));
    await harness.refresh();
    await harness.refresh();
    // The anchor (i50) is inside the newest page, but the unresolved i41 is
    // not: the walk fetched the bridge page and its terminal call replaced
    // the cached pending one.
    expect(harness.historyMock).toHaveBeenCalledExactlyOnceWith(expect.anything(), 'c2');
    const items = harness.reader.getItems();
    expect(ids(items)).toEqual(range(1, 101).map((item) => item.item_id));
    const replaced = items.find((item) => item.item_id === 'i41') as AssistantItem;
    expect(replaced.tool_calls[0].status).toBe('success');
  });

  it('keeps the prior contiguous display while the bridge is pending', async () => {
    const harness = makeHarness();
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('1'), range(1, 50), 'c1')))
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(52, 101), 'c2')));
    let resolveBridge: (value: ReaderOutcome<HistoryPage>) => void = () => {};
    harness.historyMock.mockImplementationOnce(() => new Promise<ReaderOutcome<HistoryPage>>((resolve) => { resolveBridge = resolve; }));
    await harness.refresh();
    harness.reader.markDirty();
    await vi.waitFor(() => expect(harness.historyMock).toHaveBeenCalledTimes(1));
    await flush();
    // The hydration projection was replaced, but the disconnected newest page
    // is not rendered: the prior contiguous range stays.
    expect(harness.reader.isBusy()).toBe(true);
    expect(ids(harness.reader.getItems())).toEqual(range(1, 50).map((item) => item.item_id));
    resolveBridge(okRead(page(revision('2'), range(2, 51), null)));
    await harness.settle();
    expect(ids(harness.reader.getItems())).toEqual(range(1, 101).map((item) => item.item_id));
  });

  it('re-checks acceptance at commit: an independent config floor that advanced during the held bridge rejects the whole body', async () => {
    const harness = makeHarness();
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('1'), range(1, 5), 'c1')))
      // The pending body carries configuration generation 1.
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(7, 10), 'cA')))
      // After rejection the reader re-reads and the body catches up (gen 2),
      // covering the whole connected range so no further bridge is needed.
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(1, 10), null, {
        configuration_revision: { instance_id: INSTANCE, generation: '2' },
      })));
    let resolveBridge: (value: ReaderOutcome<HistoryPage>) => void = () => {};
    harness.historyMock.mockImplementationOnce(() => new Promise<ReaderOutcome<HistoryPage>>((resolve) => { resolveBridge = resolve; }));
    await harness.refresh();
    harness.reader.markDirty();
    await vi.waitFor(() => expect(harness.historyMock).toHaveBeenCalledTimes(1));
    // The global configuration floor advances WHILE the bridge is held.
    harness.reader.setFloors({ instance_id: INSTANCE, generation: '2' }, null);
    resolveBridge(okRead(page(revision('2'), range(5, 6), null)));
    await harness.settle();
    // At commit the gen-1 body is now stale against the gen-2 floor: rejected
    // whole. The connected bridge (i6) joined the cache, the forbidden newest
    // page (i7..i10) and its projection were NOT adopted, and the reader re-
    // read to converge on the catching-up body.
    expect(harness.adopted.map((v) => v.configuration_revision.generation)).toEqual(['1', '2']);
    expect(harness.adopted.map((v) => v.conversation.items.length)).toEqual([5, 10]);
    expect(ids(harness.reader.getItems())).toEqual(range(1, 10).map((item) => item.item_id));
    expect(harness.hydrationMock).toHaveBeenCalledTimes(3);
  });

  it('re-checks acceptance at commit: a warning floor advancing during the bridge rejects the stale body', async () => {
    const harness = makeHarness();
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('1'), range(1, 5), 'c1')))
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(7, 10), 'cA')))
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(1, 10), null, {
        warnings_revision: { instance_id: INSTANCE, revision: '2' },
      })));
    let resolveBridge: (value: ReaderOutcome<HistoryPage>) => void = () => {};
    harness.historyMock.mockImplementationOnce(() => new Promise<ReaderOutcome<HistoryPage>>((resolve) => { resolveBridge = resolve; }));
    await harness.refresh();
    harness.reader.markDirty();
    await vi.waitFor(() => expect(harness.historyMock).toHaveBeenCalledTimes(1));
    harness.reader.setFloors(null, { instance_id: INSTANCE, revision: '2' });
    resolveBridge(okRead(page(revision('2'), range(5, 6), null)));
    await harness.settle();
    expect(harness.adopted).toHaveLength(2);
    expect(ids(harness.reader.getItems())).toEqual(range(1, 10).map((item) => item.item_id));
  });

  it('equal floors at commit still accept the body (no spurious rejection)', async () => {
    const harness = makeHarness();
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('1'), range(1, 5), 'c1')))
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(7, 10), 'cA')));
    // Same global clocks the body already satisfies (config gen1 / warnings r1).
    harness.reader.setFloors({ instance_id: INSTANCE, generation: '1' }, { instance_id: INSTANCE, revision: '1' });
    harness.historyMock.mockResolvedValueOnce(okRead(page(revision('2'), range(5, 6), null)));
    await harness.refresh();
    harness.reader.markDirty();
    await harness.settle();
    // The bridge joined and the gen-matching body was adopted at commit.
    expect(ids(harness.reader.getItems())).toEqual(range(1, 10).map((item) => item.item_id));
    expect(harness.adopted).toHaveLength(2);
  });

  it('preserves the display and reports an ordinary bridge error', async () => {
    const harness = makeHarness();
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('1'), range(1, 50), 'c1')))
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(52, 101), 'c2')));
    harness.historyMock.mockResolvedValue(failRead('ordinary', 'bridge failed'));
    await harness.refresh();
    await harness.refresh();
    expect(ids(harness.reader.getItems())).toEqual(range(1, 50).map((item) => item.item_id));
    expect(harness.reader.getError()).toBe('bridge failed');
    expect(harness.repairMock).not.toHaveBeenCalled();
    // The rejected chain left the projection untouched: no newer hydration
    // was installed over the disconnected walk.
    expect(harness.reader.getFloor()).toEqual(revision('1'));
  });

  it('requests repair when the cursor exhausts without the expected anchor', async () => {
    const harness = makeHarness();
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('1'), range(1, 50), 'c1')))
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(52, 101), 'c2')));
    harness.historyMock.mockResolvedValue(okRead(page(revision('2'), range(30, 40), null)));
    await harness.refresh();
    await harness.refresh();
    expect(harness.repairMock).toHaveBeenCalledOnce();
    expect(harness.reader.getItems()).toEqual([]);
    expect(harness.reader.getHydration()).toBeNull();
  });
});

describe('SessionReader newer mid-chain floor', () => {
  it('advances the floor on a newer mid-walk page, keeps the joined history and rejects the now-older hydration whole', async () => {
    const harness = makeHarness();
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('1'), range(1, 50), 'c1')))
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(52, 101), 'c2', {
        operations: [{ operation_id: 'stale-op', request_kind: 'message', status: 'success', admitted_at: '0', agent_type: 'primary', model: 'p/m', usage: { totals: { by_model: [] } } }],
      })))
      .mockResolvedValueOnce(okRead(hydration(revision('3'), range(2, 101), 'c3')));
    harness.historyMock.mockResolvedValue(okRead(page(revision('3'), range(2, 51), null)));
    await harness.refresh();
    await harness.refresh();
    // The rev-3 bridge page advanced the floor past the pending rev-2
    // hydration: the bridge joined the cache, the rev-2 projection and newest
    // page were rejected whole, and the fresh rev-3 read adopted.
    expect(harness.adopted.map((value) => value.session_revision.durable_revision)).toEqual(['1', '3']);
    expect(harness.hydrationMock).toHaveBeenCalledTimes(3);
    expect(ids(harness.reader.getItems())).toEqual(range(1, 101).map((item) => item.item_id));
    expect(harness.reader.getFloor()).toEqual(revision('3'));
    expect(harness.reader.getHydration()?.operations).toEqual([]);
  });

  it('rejects an older mid-walk page whole and fetches a fresh hydration', async () => {
    const harness = makeHarness();
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(1, 50), 'c1')))
      .mockResolvedValueOnce(okRead(hydration(revision('3'), range(52, 101), 'c2')))
      .mockResolvedValueOnce(okRead(hydration(revision('4'), range(2, 101), 'c3')));
    harness.historyMock.mockResolvedValue(okRead(page(revision('1'), range(2, 51), null)));
    await harness.refresh();
    await harness.refresh();
    expect(harness.adopted.map((value) => value.session_revision.durable_revision)).toEqual(['2', '4']);
    expect(ids(harness.reader.getItems())).toEqual(range(1, 101).map((item) => item.item_id));
    expect(harness.reader.getFloor()).toEqual(revision('4'));
  });
});

describe('SessionReader chain draining', () => {
  it('services a queued older-page request after an ordinary refresh error', async () => {
    const harness = makeHarness();
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('1'), range(11, 15), 'c5')))
      .mockResolvedValueOnce(failRead('ordinary', 'hydration gone'));
    harness.historyMock.mockResolvedValueOnce(okRead(page(revision('1'), range(1, 10), 'c0')));
    await harness.refresh();
    harness.reader.markDirty();
    harness.reader.requestOlder();
    await harness.settle();
    // The error was reported and the recorded older-page request still ran
    // through the same chain.
    expect(ids(harness.reader.getItems())).toEqual(range(1, 15).map((item) => item.item_id));
    expect(harness.historyMock).toHaveBeenCalledExactlyOnceWith(expect.anything(), 'c5');
  });

  it('services a dirty hint recorded during an older-page step', async () => {
    const harness = makeHarness();
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('1'), range(11, 15), 'c5')))
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(11, 16), 'c5')));
    harness.historyMock.mockImplementationOnce(() => {
      // A hint lands while the older page is in flight.
      harness.reader.markDirty();
      return Promise.resolve(okRead(page(revision('1'), range(1, 10), 'c0')));
    });
    await harness.refresh();
    harness.reader.requestOlder();
    await harness.settle();
    expect(harness.hydrationMock).toHaveBeenCalledTimes(2);
    expect(ids(harness.reader.getItems())).toEqual(range(1, 16).map((item) => item.item_id));
  });

  it('reports one ordinary error without self-redirtying or retrying', async () => {
    const harness = makeHarness();
    harness.hydrationMock.mockResolvedValue(failRead('ordinary', 'not_found'));
    harness.reader.markDirty();
    await harness.settle();
    expect(harness.reader.getError()).toBe('not_found');
    expect(harness.hydrationMock).toHaveBeenCalledTimes(1);
    expect(harness.reader.isDirty()).toBe(false);
  });

  it('drains an older-page request recorded after an older-step error', async () => {
    const harness = makeHarness();
    harness.hydrationMock.mockResolvedValue(okRead(hydration(revision('1'), range(11, 15), 'c5')));
    harness.historyMock
      .mockResolvedValueOnce(failRead('ordinary', 'older page gone'))
      .mockResolvedValueOnce(okRead(page(revision('1'), range(1, 10), 'c0')));
    await harness.refresh();
    harness.reader.requestOlder();
    harness.reader.requestOlder();
    await harness.settle();
    // The first older request errored; the second recorded request is still
    // serviced instead of stranded behind the failed step.
    expect(harness.reader.getError()).toBeNull();
    expect(ids(harness.reader.getItems())).toEqual(range(1, 15).map((item) => item.item_id));
    expect(harness.historyMock).toHaveBeenCalledTimes(2);
  });
});

describe('SessionReader stamps, lifecycle and supersession', () => {
  it('ignores a hydration success settled under a newer selection (A-to-B)', async () => {
    const harness = makeHarness();
    harness.hydrationMock.mockImplementation(() => {
      // The selection moves to session B while the read for A is in flight.
      harness.stamp.sessionID = 's2';
      harness.stamp.selection += 1;
      return Promise.resolve(okRead(hydration(revision('1'), range(1, 5), null)));
    });
    harness.reader.markDirty();
    await harness.settle();
    expect(harness.reader.getItems()).toEqual([]);
    expect(harness.reader.getHydration()).toBeNull();
    expect(harness.reader.getError()).toBeNull();
  });

  it('ignores a hydration error settled under a newer selection', async () => {
    const harness = makeHarness();
    harness.hydrationMock.mockImplementation(async () => {
      harness.stamp.selection += 1;
      return failRead('ordinary', 'session gone');
    });
    harness.reader.markDirty();
    await harness.settle();
    expect(harness.reader.getError()).toBeNull();
  });

  it('reports an ordinary hydration error under a matching stamp', async () => {
    const harness = makeHarness();
    harness.hydrationMock.mockResolvedValue(failRead('ordinary', 'not_found'));
    harness.reader.markDirty();
    await harness.settle();
    expect(harness.reader.getError()).toBe('not_found');
  });

  it('reads again when a hint marks the chain dirty during the read', async () => {
    const harness = makeHarness();
    let releaseFirst: (value: ReaderOutcome<Hydration>) => void = () => {};
    harness.hydrationMock
      .mockImplementationOnce(() => new Promise<ReaderOutcome<Hydration>>((resolve) => { releaseFirst = resolve; }))
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(1, 5), null)));
    harness.reader.markDirty();
    // Wait until the real hydration call is pending (in flight).
    await vi.waitFor(() => expect(harness.hydrationMock).toHaveBeenCalledTimes(1));
    // A hint lands BEFORE the first read is released: it marks the chain dirty
    // without starting a parallel chain.
    expect(harness.reader.hint(revision('2'), 'session')).toBe('dirty');
    expect(harness.hydrationMock).toHaveBeenCalledTimes(1);
    // Release the first read: it adopts successfully, and the dirty work
    // recorded during the read drives the second refresh/adoption in the same
    // serialized chain.
    releaseFirst(okRead(hydration(revision('1'), range(1, 3), null)));
    await harness.settle();
    expect(harness.hydrationMock).toHaveBeenCalledTimes(2);
    expect(harness.adopted).toHaveLength(2);
    expect(harness.adopted.map((v) => v.session_revision.durable_revision)).toEqual(['1', '2']);
    expect(ids(harness.reader.getItems())).toEqual(range(1, 5).map((item) => item.item_id));
  });

  it('discards a hydration settled after disposal even with an unchanged stamp', async () => {
    const harness = makeHarness();
    let resolveHydration: (value: ReaderOutcome<Hydration>) => void = () => {};
    harness.hydrationMock.mockImplementationOnce(() => new Promise<ReaderOutcome<Hydration>>((resolve) => { resolveHydration = resolve; }));
    harness.reader.markDirty();
    await flush();
    harness.reader.dispose();
    resolveHydration(okRead(hydration(revision('1'), range(1, 5), null)));
    await harness.settle();
    expect(harness.reader.getItems()).toEqual([]);
    expect(harness.reader.getHydration()).toBeNull();
    expect(harness.adopted).toHaveLength(0);
    expect(harness.repairMock).not.toHaveBeenCalled();
  });

  it('emits no orphan bridge adoption after a replacement disposes the old reader', async () => {
    const harness = makeHarness();
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('1'), range(1, 50), 'c1')))
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(52, 101), 'c2')));
    let resolveBridge: (value: ReaderOutcome<HistoryPage>) => void = () => {};
    harness.historyMock.mockImplementationOnce(() => new Promise<ReaderOutcome<HistoryPage>>((resolve) => { resolveBridge = resolve; }));
    await harness.refresh();
    harness.reader.markDirty();
    await vi.waitFor(() => expect(harness.historyMock).toHaveBeenCalledTimes(1));
    // A replacement disposes the old reader (root switch / repair build a fresh
    // one); the in-flight bridge of the replaced reader never adopts.
    const oldReader = harness.reader;
    oldReader.dispose();
    resolveBridge(okRead(page(revision('2'), range(2, 51), null)));
    await harness.settle();
    // The disposed reader's state is untouched and never fires a late adopt.
    expect(ids(oldReader.getItems())).toEqual(range(1, 50).map((item) => item.item_id));
    expect(harness.adopted).toHaveLength(1);
    expect(harness.repairMock).not.toHaveBeenCalled();
  });
});

describe('SessionReader hints', () => {
  it('covers hints at or below the accepted clocks and dirties on newer ones', async () => {
    const harness = makeHarness();
    harness.hydrationMock.mockResolvedValue(okRead(hydration(revision('2'), range(1, 3), null)));
    await harness.refresh();
    expect(harness.hydrationMock).toHaveBeenCalledTimes(1);
    expect(harness.reader.hint(revision('2'), 'session')).toBe('covered');
    expect(harness.reader.hint({ instance_id: INSTANCE, generation: '1' }, 'configuration')).toBe('covered');
    expect(harness.reader.hint({ instance_id: INSTANCE, revision: '1' }, 'warning')).toBe('covered');
    await harness.settle();
    expect(harness.hydrationMock).toHaveBeenCalledTimes(1);
    harness.hydrationMock.mockResolvedValueOnce(okRead(hydration(revision('3'), range(1, 4), null)));
    expect(harness.reader.hint(revision('3'), 'session')).toBe('dirty');
    await harness.settle();
    expect(harness.hydrationMock).toHaveBeenCalledTimes(2);
    expect(ids(harness.reader.getItems())).toEqual(range(1, 4).map((item) => item.item_id));
  });

  it('dirties on a configuration hint newer than the accepted hydration', async () => {
    const harness = makeHarness();
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('1'), range(1, 2), null)))
      .mockResolvedValueOnce(okRead(hydration(revision('1'), range(1, 3), null, {
        configuration_revision: { instance_id: INSTANCE, generation: '4' },
      })));
    await harness.refresh();
    expect(harness.reader.hint({ instance_id: INSTANCE, generation: '4' }, 'configuration')).toBe('dirty');
    await harness.settle();
    expect(harness.hydrationMock).toHaveBeenCalledTimes(2);
  });

  it('requests repair for a malformed hint revision', async () => {
    const harness = makeHarness();
    harness.hydrationMock.mockResolvedValue(okRead(hydration(revision('1'), range(1, 2), null)));
    await harness.refresh();
    expect(harness.reader.hint(revision('not-a-number'), 'session')).toBe('repair');
    expect(harness.repairMock).toHaveBeenCalledOnce();
  });

  it('dirties when no hydration has been accepted yet', () => {
    const harness = makeHarness();
    harness.hydrationMock.mockImplementation(() => new Promise<ReaderOutcome<Hydration>>(() => {}));
    expect(harness.reader.hint(revision('1'), 'session')).toBe('dirty');
  });
});

describe('SessionReader revision-hint validity', () => {
  // The empty-hydration fast path must not bypass validity: a current
  // malformed / out-of-range / foreign hint repairs the reader even before the
  // first hydration is accepted. Only a VALID hint takes the dirty fast path.
  it('repairs on a malformed session hint during a held initial hydration', () => {
    const harness = makeHarness();
    harness.hydrationMock.mockImplementation(() => new Promise<ReaderOutcome<Hydration>>(() => {}));
    expect(harness.reader.hint(revision('not-a-number'), 'session')).toBe('repair');
    expect(harness.repairMock).toHaveBeenCalledOnce();
  });

  it('repairs on an out-of-range session hint before the first adoption', () => {
    const harness = makeHarness();
    harness.hydrationMock.mockImplementation(() => new Promise<ReaderOutcome<Hydration>>(() => {}));
    expect(harness.reader.hint(revision('0'), 'session')).toBe('repair');
    expect(harness.repairMock).toHaveBeenCalledOnce();
  });

  it('repairs on a foreign-instance warning hint before the first adoption', () => {
    const harness = makeHarness();
    harness.hydrationMock.mockImplementation(() => new Promise<ReaderOutcome<Hydration>>(() => {}));
    expect(harness.reader.hint({ instance_id: 'ffffffffffffffffffffffffffffffff', revision: '1' }, 'warning')).toBe('repair');
    expect(harness.repairMock).toHaveBeenCalledOnce();
  });

  it('repairs on a malformed configuration hint before the first adoption', () => {
    const harness = makeHarness();
    harness.hydrationMock.mockImplementation(() => new Promise<ReaderOutcome<Hydration>>(() => {}));
    expect(harness.reader.hint({ instance_id: INSTANCE, generation: 'not-a-number' }, 'configuration')).toBe('repair');
    expect(harness.repairMock).toHaveBeenCalledOnce();
  });

  it('still dirties on a VALID hint during a held initial hydration', () => {
    const harness = makeHarness();
    harness.hydrationMock.mockImplementation(() => new Promise<ReaderOutcome<Hydration>>(() => {}));
    expect(harness.reader.hint(revision('1'), 'session')).toBe('dirty');
    expect(harness.repairMock).not.toHaveBeenCalled();
  });

  it('repairs on an out-of-range configuration hint AFTER adoption', async () => {
    const harness = makeHarness();
    harness.hydrationMock.mockResolvedValue(okRead(hydration(revision('1'), range(1, 2), null)));
    await harness.refresh();
    expect(harness.reader.hint({ instance_id: INSTANCE, generation: '18446744073709551616' }, 'configuration')).toBe('repair');
    expect(harness.repairMock).toHaveBeenCalledOnce();
  });
});

describe('SessionReader permanent retirement', () => {
  // Repair invalidation permanently retires the reader before its callback;
  // permanent disposal + the async stamp guard replace the chain token. An
  // ordinary read error, by contrast, leaves the reader reusable.
  it('repair invalidation permanently retires the reader and suppresses its late completion', async () => {
    const harness = makeHarness();
    let resolveRead: (value: ReaderOutcome<Hydration>) => void = () => {};
    harness.hydrationMock.mockImplementation(() => new Promise<ReaderOutcome<Hydration>>((resolve) => { resolveRead = resolve; }));
    harness.reader.markDirty();
    await vi.waitFor(() => expect(harness.hydrationMock).toHaveBeenCalledTimes(1));
    // An invalid delivery retires the reader while its own read is in flight.
    harness.reader.hint(revision('not-a-number'), 'session');
    expect(harness.repairMock).toHaveBeenCalledOnce();
    // The late read settles after retirement: no adoption, no error write.
    resolveRead(okRead(hydration(revision('1'), range(1, 5), null)));
    await harness.settle();
    expect(harness.adopted).toHaveLength(0);
    expect(harness.reader.getError()).toBeNull();
    expect(harness.reader.getItems()).toEqual([]);
    // The retired reader is permanently gone: later work requests do nothing.
    const readsBefore = harness.hydrationMock.mock.calls.length;
    harness.reader.markDirty();
    harness.reader.requestOlder();
    await harness.settle();
    expect(harness.hydrationMock).toHaveBeenCalledTimes(readsBefore);
  });

  it('an ordinary read error does not dispose the reader and a later hint recovers', async () => {
    const harness = makeHarness();
    harness.hydrationMock
      .mockResolvedValueOnce(failRead('ordinary', 'transient read failure'))
      .mockResolvedValueOnce(okRead(hydration(revision('1'), range(1, 4), null)));
    await harness.refresh();
    expect(harness.reader.getError()).toBe('transient read failure');
    // Still alive: a later hint starts a fresh read and adopts.
    expect(harness.reader.hint(revision('2'), 'session')).toBe('dirty');
    await harness.settle();
    expect(ids(harness.reader.getItems())).toEqual(range(1, 4).map((item) => item.item_id));
    expect(harness.reader.getError()).toBeNull();
    expect(harness.hydrationMock).toHaveBeenCalledTimes(2);
  });
});

describe('SessionReader malformed, foreign and incomparable revisions', () => {
  it('requests repair for a foreign hydration instance', async () => {
    const harness = makeHarness();
    harness.hydrationMock.mockResolvedValue(okRead(hydration(revision('1', '0', 'ffffffffffffffffffffffffffffffff'), range(1, 2), null)));
    harness.reader.markDirty();
    await harness.settle();
    expect(harness.repairMock).toHaveBeenCalledOnce();
    expect(harness.reader.getItems()).toEqual([]);
  });

  it('requests repair for an incomparable follow-up hydration', async () => {
    const harness = makeHarness();
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('2', '2'), range(1, 2), null)))
      .mockResolvedValueOnce(okRead(hydration(revision('3', '1'), range(3, 4), null)));
    await harness.refresh();
    await harness.refresh();
    expect(harness.repairMock).toHaveBeenCalledOnce();
  });

  it('requests repair for a history page with a foreign instance', async () => {
    const harness = makeHarness();
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('1'), range(1, 50), 'c1')))
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(52, 101), 'c2')));
    harness.historyMock.mockResolvedValue(okRead(page(revision('2', '0', 'ffffffffffffffffffffffffffffffff'), range(2, 51), null)));
    await harness.refresh();
    await harness.refresh();
    expect(harness.repairMock).toHaveBeenCalledOnce();
  });

  it('routes an invalid delivered item identity to repair instead of escaping', async () => {
    const harness = makeHarness();
    harness.hydrationMock.mockResolvedValue(okRead(hydration(revision('1'), [inputItem('')], null)));
    harness.reader.markDirty();
    await harness.settle();
    expect(harness.repairMock).toHaveBeenCalledOnce();
    expect(harness.reader.getItems()).toEqual([]);
  });

  it('routes an invalid bridge-page item identity to repair', async () => {
    const harness = makeHarness();
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('1'), range(1, 5), 'c1')))
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(7, 10), 'c2')));
    harness.historyMock.mockResolvedValueOnce(okRead(page(revision('2'), [{ item_id: '', kind: 'signal', committed_at: '0', subtype: 'interruption', content: 'x' } as ConversationItem], null)));
    await harness.refresh();
    await harness.refresh();
    expect(harness.repairMock).toHaveBeenCalledOnce();
  });
});

describe('SessionReader invalid delivery', () => {
  it('routes an undecodable hydration success to repair, not an ordinary error', async () => {
    const harness = makeHarness();
    harness.hydrationMock.mockResolvedValue(failRead('invalid', 'received success without a result'));
    harness.reader.markDirty();
    await harness.settle();
    expect(harness.repairMock).toHaveBeenCalledOnce();
    expect(harness.reader.getError()).toBeNull();
  });

  it('routes an undecodable history page to repair during the walk', async () => {
    const harness = makeHarness();
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('1'), range(1, 50), 'c1')))
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(52, 101), 'c2')));
    harness.historyMock.mockResolvedValue(failRead('invalid', 'history body undecodable'));
    await harness.refresh();
    await harness.refresh();
    expect(harness.repairMock).toHaveBeenCalledOnce();
    expect(harness.reader.getItems()).toEqual([]);
  });

  it('routes an undecodable older page to repair', async () => {
    const harness = makeHarness();
    harness.hydrationMock.mockResolvedValue(okRead(hydration(revision('1'), range(11, 15), 'c1')));
    harness.historyMock.mockResolvedValue(failRead('invalid', 'older page undecodable'));
    await harness.refresh();
    harness.reader.requestOlder();
    await harness.settle();
    expect(harness.repairMock).toHaveBeenCalledOnce();
  });
});

describe('SessionReader user-requested older pages', () => {
  it('extends the oldest edge through the same chain and keeps the new cursor', async () => {
    const harness = makeHarness();
    harness.hydrationMock.mockResolvedValue(okRead(hydration(revision('1'), range(11, 15), 'c1')));
    harness.historyMock.mockResolvedValue(okRead(page(revision('1'), range(1, 10), 'c0')));
    await harness.refresh();
    harness.reader.requestOlder();
    await harness.settle();
    expect(harness.historyMock).toHaveBeenCalledExactlyOnceWith(expect.anything(), 'c1');
    expect(ids(harness.reader.getItems())).toEqual(range(1, 15).map((item) => item.item_id));
    expect(harness.reader.getOldestCursor()).toBe('c0');
    expect(harness.reader.getHydration()).not.toBeNull();
  });

  it('rejects an older page below the floor and refreshes hydration instead', async () => {
    const harness = makeHarness();
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('3'), range(11, 15), 'c1')))
      .mockResolvedValueOnce(okRead(hydration(revision('4'), range(1, 15), null)));
    harness.historyMock.mockResolvedValue(okRead(page(revision('2'), range(1, 10), 'c0')));
    await harness.refresh();
    harness.reader.requestOlder();
    await harness.settle();
    expect(ids(harness.reader.getItems())).toEqual(range(1, 15).map((item) => item.item_id));
    expect(harness.reader.getFloor()).toEqual(revision('4'));
  });

  it('is a no-op without an oldest cursor', async () => {
    const harness = makeHarness();
    harness.hydrationMock.mockResolvedValue(okRead(hydration(revision('1'), range(1, 3), null)));
    await harness.refresh();
    harness.reader.requestOlder();
    await harness.settle();
    expect(harness.historyMock).not.toHaveBeenCalled();
    expect(ids(harness.reader.getItems())).toEqual(range(1, 3).map((item) => item.item_id));
  });
});

describe('SessionReader oldest-cursor geometry', () => {
  it('uses the walk final nonnull cursor when the bridge extends the oldest edge', async () => {
    const harness = makeHarness();
    const cached = [assistantItem('i11', true), inputItem('i12'), inputItem('i13')];
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('1'), cached, 'c5')))
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(13, 20), 'cA')));
    // The bridge page is in ascending server order and starts before the
    // cached head i11: the merged oldest edge moved, so the oldest cursor
    // becomes the walk final older_cursor cB.
    harness.historyMock.mockResolvedValueOnce(okRead(page(revision('2'), [inputItem('i10'), assistantItem('i11', false), inputItem('i12')], 'cB')));
    await harness.refresh();
    await harness.refresh();
    expect(ids(harness.reader.getItems())).toEqual(['i10', 'i11', 'i12', ...range(13, 20).map((item) => item.item_id)]);
    expect(harness.reader.getOldestCursor()).toBe('cB');
  });

  it('exhausts the oldest cursor when the extending walk has no older cursor', async () => {
    const harness = makeHarness();
    const cached = [assistantItem('i11', true), inputItem('i12'), inputItem('i13')];
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('1'), cached, 'c5')))
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(13, 20), 'cA')));
    harness.historyMock.mockResolvedValueOnce(okRead(page(revision('2'), [inputItem('i10'), assistantItem('i11', false), inputItem('i12')], null)));
    await harness.refresh();
    await harness.refresh();
    expect(harness.reader.getOldestCursor()).toBeNull();
    expect(harness.reader.hasOlder()).toBe(false);
  });

  it('applies the same geometry when a newer floor rejects the pending hydration', async () => {
    const harness = makeHarness();
    harness.hydrationMock
      .mockResolvedValueOnce(okRead(hydration(revision('1'), range(11, 50), 'c1')))
      .mockResolvedValueOnce(okRead(hydration(revision('2'), range(52, 101), 'c2')))
      .mockResolvedValueOnce(okRead(hydration(revision('3'), range(1, 101), null)));
    // The bridge extends before the cached head i11 and carries the newer
    // rev-3 floor; the rev-2 hydration page is rejected whole.
    harness.historyMock.mockResolvedValueOnce(okRead(page(revision('3'), range(1, 51), 'cD')));
    await harness.refresh();
    await harness.refresh();
    expect(ids(harness.reader.getItems())).toEqual(range(1, 101).map((item) => item.item_id));
    expect(harness.reader.getOldestCursor()).toBe('cD');
    expect(harness.adopted.map((value) => value.session_revision.durable_revision)).toEqual(['1', '3']);
  });
});
