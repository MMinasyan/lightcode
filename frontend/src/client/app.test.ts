// @vitest-environment happy-dom
// The mounted target App over the real reader, composer and renderer with
// faithfully shaped generated SDK results: every received status arrives as
// {data,error,request,response} through the Response object, and only a
// binding failure leaves response absent. The real-stack owner (stack.test.ts)
// drives the actual SDK on top of these; this file covers the view, keyboard,
// membership, draft and repair behaviors.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { mount, tick, unmount } from 'svelte';

import * as sdk from '../generated/protocol/sdk.gen';
import type { Hydration, Session } from '../generated/protocol/types.gen';

vi.mock('../generated/protocol/sdk.gen', () => ({
  compactSession: vi.fn(),
  createSession: vi.fn(),
  getConfiguration: vi.fn(),
  getSessionHistory: vi.fn(),
  getSessionHydration: vi.fn(),
  getWarnings: vi.fn(),
  interruptSession: vi.fn(),
  listModels: vi.fn(),
  listSessions: vi.fn(),
  setAgentTypeModel: vi.fn(),
  stopSession: vi.fn(),
  submitSession: vi.fn(),
}));

const INSTANCE = '0123456789abcdef0123456789abcdef';
const mounted: Array<ReturnType<typeof mount>> = [];

// Descending last_activity then Session ID orders RFC3339 strings correctly;
// the fixtures use comparable timestamps.
const NEWER = '2026-10-02T10:00:00Z';
const OLDER = '2026-10-01T05:00:00Z';

function sessionRev(durable: string) {
  return { instance_id: INSTANCE, durable_revision: durable, local_revision: '0' };
}

function rootSession(id: string, lastActivity: string): Session {
  return {
    session_id: id,
    workspace: '/ws',
    lifecycle: 'open',
    agent_type: 'primary',
    created_at: '0',
    last_activity: lastActivity,
    session_revision: sessionRev('1'),
  };
}

function makeHydration(overrides: Partial<Hydration> = {}): Hydration {
  return {
    session: rootSession('root1', NEWER),
    selected_model: null,
    session_revision: sessionRev('1'),
    configuration_revision: { instance_id: INSTANCE, generation: '1' },
    warnings_revision: { instance_id: INSTANCE, revision: '1' },
    operations: [],
    pending: { steering: [], queued: [] },
    conversation: { items: [] },
    usage: { totals: { by_model: [] }, context: { used_tokens: '0', context_window: 0 } },
    warnings: [],
    background: [],
    ...overrides,
  };
}

// ok models the generated client's success result: response present and ok.
function ok(data: unknown) {
  return { data, error: undefined, request: new Request('http://runtime.invalid/v1/x'), response: new Response('x', { status: 200 }) };
}

// httpError models a RECEIVED non-2xx: the generated error is the in-band
// result with a real response object, never a rejected promise.
function httpError(status: number, error: unknown) {
  return { data: undefined, error, request: new Request('http://runtime.invalid/v1/x'), response: new Response('{}', { status }) };
}

// bridgeLoss models a binding/fetch failure: response absent. The generated
// client resolves this shape; it does not reject.
function bridgeLoss(message: string) {
  return { data: undefined, error: new Error(message), request: undefined, response: undefined };
}

// receivedInvalid models a 2xx whose body could not be decoded: response
// present and ok, no result.
function receivedInvalid(message: string) {
  return { data: undefined, error: new SyntaxError(message), request: undefined, response: new Response('ok', { status: 200 }) };
}

// noContent models a REAL 204 success: the generated client hands back an
// empty object as data with an ok response whose status is 204.
function noContent() {
  return { data: undefined, error: undefined, request: new Request('http://runtime.invalid/v1/x'), response: new Response(null, { status: 204 }) };
}

function fakeConnection() {
  const client = {};
  let installed = '';
  const connection: {
    handler: ((event: never) => void) | null;
    failureHandler: ((error: unknown, generation: string) => void) | null;
    openMock: ReturnType<typeof vi.fn>;
    disposeMock: ReturnType<typeof vi.fn>;
    getClient: () => object;
    setEventHandler: (handler: (event: never) => void) => void;
    setFailureHandler: (handler: (error: unknown, generation: string) => void) => void;
    isCurrentGeneration: (generation: string) => boolean;
    open: () => Promise<{ instance_id: string; generation: string; workspace: string }>;
    dispose: () => void;
  } = {
    handler: null,
    failureHandler: null,
    openMock: vi.fn(),
    disposeMock: vi.fn(),
    getClient: () => client,
    setEventHandler: (handler) => { connection.handler = handler; },
    setFailureHandler: (handler) => { connection.failureHandler = handler; },
    // Mirrors RuntimeConnection: only the generation the connection currently
    // presents is "current"; a superseded pull's failure is subsumed.
    isCurrentGeneration: (generation) => generation === installed,
    open: async () => {
      const info = await connection.openMock();
      installed = info.generation;
      return info;
    },
    dispose: () => { connection.disposeMock(); installed = ''; },
  };
  return connection;
}

async function mountApp(connection: ReturnType<typeof fakeConnection>) {
  const { default: App } = await import('./App.svelte');
  const target = document.createElement('div');
  document.body.appendChild(target);
  const component = mount(App, { target, props: { connection } });
  mounted.push(component);
  await vi.waitFor(() => expect(vi.mocked(sdk.getSessionHydration)).toHaveBeenCalled());
  await tick();
  return { connection, component, target };
}

// mountAppNoWait mounts without waiting for a hydration: for cases where the
// selection is still gated behind a held list/create.
async function mountAppNoWait(connection: ReturnType<typeof fakeConnection>) {
  const { default: App } = await import('./App.svelte');
  const target = document.createElement('div');
  document.body.appendChild(target);
  const component = mount(App, { target, props: { connection } });
  mounted.push(component);
  await settle();
  return { connection, component, target };
}

// basicMocks installs the standard successful composition: one connection,
// two open roots, an empty hydration and accepted global clocks.
function basicMocks(connection: ReturnType<typeof fakeConnection>) {
  connection.openMock.mockResolvedValue({ instance_id: INSTANCE, generation: '1', workspace: '/ws' });
  vi.mocked(sdk.listSessions).mockResolvedValue(ok([rootSession('root1', NEWER), rootSession('root2', OLDER)]));
  vi.mocked(sdk.getSessionHydration).mockResolvedValue(ok(makeHydration()));
  vi.mocked(sdk.getConfiguration).mockResolvedValue(ok({
    configuration_revision: { instance_id: INSTANCE, generation: '1' },
    settings: { sessions: { auto_archive: false, archive_after_days: 1, delete_after_archive_days: 1 }, plugins: {} },
    agents: [],
    providers: [],
  }));
  vi.mocked(sdk.getWarnings).mockResolvedValue(ok({
    warnings_revision: { instance_id: INSTANCE, revision: '1' },
    warnings: [],
  }));
}

async function mountBasic() {
  const connection = fakeConnection();
  basicMocks(connection);
  const { component, target } = await mountApp(connection);
  return { connection, component, target };
}

function deliverEvent(connection: ReturnType<typeof fakeConnection>, event: unknown) {
  connection.handler?.(event as never);
}

function typeInto(target: HTMLElement, text: string) {
  const textarea = target.querySelector('textarea') as HTMLTextAreaElement;
  textarea.value = text;
  textarea.dispatchEvent(new Event('input', { bubbles: true }));
}

async function settle() {
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();
}

afterEach(async () => {
  for (const component of mounted.splice(0)) {
    unmount(component);
  }
  document.body.innerHTML = '';
  vi.clearAllMocks();
});

describe('initial composition', () => {
  it('hydrates the newest open root and never selects a model automatically', async () => {
    const { target } = await mountBasic();
    expect(vi.mocked(sdk.listSessions)).toHaveBeenCalledWith({
      client: expect.anything(),
      query: { workspace: '/ws', lifecycle: 'open' },
    });
    expect(vi.mocked(sdk.getSessionHydration)).toHaveBeenCalledWith({ client: expect.anything(), path: { id: 'root1' } });
    expect(vi.mocked(sdk.listModels)).not.toHaveBeenCalled();
    expect(vi.mocked(sdk.setAgentTypeModel)).not.toHaveBeenCalled();
    expect(target.textContent).toContain('root1');
  });

  it('reads the independent global floors at connection and gates hydration with them', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    // The global configuration clock advances to generation 2 while the
    // hydration keeps carrying generation 1: the accepted floor rejects it
    // and the reader re-reads until the projection catches up.
    vi.mocked(sdk.getConfiguration).mockResolvedValue(ok({
      configuration_revision: { instance_id: INSTANCE, generation: '2' },
      settings: { sessions: { auto_archive: false, archive_after_days: 1, delete_after_archive_days: 1 }, plugins: {} },
      agents: [],
      providers: [],
    }));
    const hydrationV1 = makeHydration();
    const hydrationV2 = makeHydration({
      configuration_revision: { instance_id: INSTANCE, generation: '2' },
      conversation: { items: [{ item_id: 'i9', kind: 'input', committed_at: '0', origin: 'user', content: [{ kind: 'text', text: 'item i9' }] } as Hydration['conversation']['items'][number]] },
    });
    vi.mocked(sdk.getSessionHydration)
      .mockResolvedValueOnce(ok(hydrationV1))
      .mockResolvedValue(ok(hydrationV2));
    await mountApp(connection);
    await settle();
    // The stale-body hydration never rendered; the accepted-floor fresh read did.
    expect(vi.mocked(sdk.getSessionHydration).mock.calls.length).toBeGreaterThanOrEqual(2);
    await vi.waitFor(() => expect(document.body.textContent).toContain('item i9'));
  });

  it('repairs once on a foreign global floor and converges after the reopen clears it', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    const foreign = { instance_id: 'ffffffffffffffffffffffffffffffff', generation: '1' };
    vi.mocked(sdk.getConfiguration)
      .mockResolvedValueOnce(ok({
        configuration_revision: foreign,
        settings: { sessions: { auto_archive: false, archive_after_days: 1, delete_after_archive_days: 1 }, plugins: {} },
        agents: [],
        providers: [],
      }))
      .mockResolvedValue(ok({
        configuration_revision: { instance_id: INSTANCE, generation: '1' },
        settings: { sessions: { auto_archive: false, archive_after_days: 1, delete_after_archive_days: 1 }, plugins: {} },
        agents: [],
        providers: [],
      }));
    connection.openMock.mockResolvedValue({ instance_id: INSTANCE, generation: '1', workspace: '/ws' });
    const { target } = await mountApp(connection);
    await vi.waitFor(() => expect(connection.openMock.mock.calls.length).toBeGreaterThan(1));
    await settle();
    // Exactly one repair Open ran; the fresh floor read was accepted and the
    // view rendered instead of looping.
    const opensAfterRepair = connection.openMock.mock.calls.length;
    await settle();
    await settle();
    expect(connection.openMock.mock.calls.length).toBe(opensAfterRepair);
    expect(target.textContent).toContain('root1');
    expect(target.querySelector('.note.warn')).not.toBeNull();
  });

  it('a repair during the held initial list finishes and re-reads, never leaving the app sessionless', async () => {
    const connection = fakeConnection();
    connection.openMock.mockResolvedValue({ instance_id: INSTANCE, generation: '1', workspace: '/ws' });
    vi.mocked(sdk.getConfiguration).mockResolvedValue(ok({
      configuration_revision: { instance_id: INSTANCE, generation: '1' },
      settings: { sessions: { auto_archive: false, archive_after_days: 1, delete_after_archive_days: 1 }, plugins: {} },
      agents: [], providers: [],
    }));
    vi.mocked(sdk.getWarnings).mockResolvedValue(ok({ warnings_revision: { instance_id: INSTANCE, revision: '1' }, warnings: [] }));
    vi.mocked(sdk.getSessionHydration).mockResolvedValue(ok(makeHydration()));
    // The boot list is held; a repair then completes and re-runs the selection.
    let releaseList: (value: unknown) => void = () => {};
    let listCalls = 0;
    vi.mocked(sdk.listSessions).mockImplementation(async () => {
      listCalls += 1;
      if (listCalls === 1) {
        return new Promise((resolve) => { releaseList = resolve; });
      }
      return ok([rootSession('root1', NEWER)]);
    });
    const { target } = await mountAppNoWait(connection);
    await settle();
    // Supersede the held boot list with a repair (new generation).
    connection.openMock.mockResolvedValue({ instance_id: INSTANCE, generation: '2', workspace: '/ws' });
    connection.failureHandler?.(new Error('loss during boot'), '1');
    await settle();
    // Release the stale boot list AFTER the repair; its adoption must be
    // discarded and the repair's fresh list must drive selection instead.
    releaseList(ok([rootSession('root-stale', NEWER)]));
    await vi.waitFor(() => expect(target.textContent).toContain('root1'));
    expect(target.textContent).not.toContain('root-stale');
  });

  it('an unknown Create is reported for its address and never blind-retransmitted; a later list discovers its header', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    // First boot: no roots → Create, which the binding loses (no response).
    vi.mocked(sdk.listSessions).mockResolvedValue(ok([]));
    vi.mocked(sdk.createSession).mockResolvedValue(bridgeLoss('create lost'));
    const { target } = await mountAppNoWait(connection);
    await vi.waitFor(() => expect(vi.mocked(sdk.createSession)).toHaveBeenCalled());
    await settle();
    // Exactly one Create attempt, reported as unknown, not resent.
    expect(vi.mocked(sdk.createSession)).toHaveBeenCalledTimes(1);
    expect(target.querySelector('.note.outcome')?.textContent).toContain('not retransmitted');
    // A later repair's fresh list now reveals the created header's real ID.
    connection.openMock.mockResolvedValue({ instance_id: INSTANCE, generation: '2', workspace: '/ws' });
    vi.mocked(sdk.listSessions).mockResolvedValue(ok([rootSession('created-1', NEWER)]));
    vi.mocked(sdk.getSessionHydration).mockResolvedValue(ok(makeHydration({ session: rootSession('created-1', NEWER) })));
    // The loss arrives against the STILL-current generation '1' (installed by
    // boot), so it drives a real repair rather than being subsumed.
    connection.failureHandler?.(new Error('later loss'), '1');
    await vi.waitFor(() => expect(target.textContent).toContain('created-1'), { timeout: 2000 });
    expect(vi.mocked(sdk.createSession)).toHaveBeenCalledTimes(1);
  });

  it('creates a primary session when no open root exists', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.listSessions).mockResolvedValue(ok([]));
    vi.mocked(sdk.createSession).mockResolvedValue(ok(rootSession('primary1', NEWER)));
    vi.mocked(sdk.getSessionHydration).mockResolvedValue(ok(makeHydration({ session: rootSession('primary1', NEWER) })));
    const { target } = await mountApp(connection);
    expect(vi.mocked(sdk.createSession)).toHaveBeenCalledWith({
      client: expect.anything(),
      body: { workspace: '/ws', agent_type: 'primary' },
    });
    expect(vi.mocked(sdk.getSessionHydration)).toHaveBeenCalledWith({ client: expect.anything(), path: { id: 'primary1' } });
    expect(target.textContent).toContain('primary1');
  });
});

describe('conversation rendering', () => {
  function inputItem(id: string) {
    return { item_id: id, kind: 'input', committed_at: '0', origin: 'user', content: [{ kind: 'text', text: `item ${id}` }] };
  }

  it('renders the five generated variants, refusal, tool calls and pending inputs', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    const items = [
      { item_id: 'u1', kind: 'input', committed_at: '0', origin: 'user', content: [{ kind: 'text', text: 'hello user' }] },
      { item_id: 'a1', kind: 'assistant', committed_at: '0', operation_id: 'op-a1', status: 'completed', source: 'prov/model', content: [{ kind: 'text', text: '**bold** reply' }], refusal: 'refused here', tool_calls: [{ id: 'c1', name: 'read_file', arguments: '{"path":"a.go"}', status: 'success', content: 'ok' }] },
      { item_id: 's1', kind: 'signal', committed_at: '0', subtype: 'interruption', content: 'stopped' },
      { item_id: 'c1', kind: 'compaction', committed_at: '0', summary: 'summary here', model: 'prov/compact' },
      { item_id: 'o1', kind: 'operation_end', committed_at: '0', operation_id: 'op-o1', status: 'failure', detail: 'failed detail' },
    ];
    vi.mocked(sdk.getSessionHydration).mockResolvedValue(ok(makeHydration({
      conversation: { items: items as Hydration['conversation']['items'] },
      pending: {
        steering: [{ operation_id: 'p1', origin: 'user', content: [{ kind: 'text', text: 'steering one' }] }],
        queued: [{ operation_id: 'p2', origin: 'user', content: [{ kind: 'text', text: 'queued one' }] }],
      },
    })));
    const { target } = await mountApp(connection);
    expect(target.querySelector('.message.user')?.textContent).toContain('hello user');
    expect(target.querySelector('strong')?.textContent).toBe('bold');
    expect(target.querySelector('.refusal')?.textContent).toContain('refused here');
    expect(target.querySelector('.tool-name')?.textContent).toBe('read_file');
    expect(target.textContent).toContain('Interrupted');
    expect(target.textContent).toContain('summary here');
    expect(target.textContent).toContain('failed detail');
    const steering = target.querySelector('.queued-msg.steering');
    expect(steering?.querySelector('.pending-label')?.textContent).toBe('Steering');
    expect(steering?.textContent).toContain('steering one');
    const plain = target.querySelector('.queued-msg:not(.steering)');
    expect(plain?.querySelector('.pending-label')?.textContent).toBe('Queued');
    expect(plain?.textContent).toContain('queued one');
  });

  it('renders opaque content and extra metadata without control interpretation', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.getSessionHydration).mockResolvedValue(ok(makeHydration({
      conversation: {
        items: [
          { item_id: 'u1', kind: 'input', committed_at: '0', origin: 'user', content: [{ kind: 'opaque', opaque_wire_type: 'odd', extra: { x: '<b>not html</b>' } }] },
          { item_id: 'a1', kind: 'assistant', committed_at: '0', status: 'completed', source: 'p/m', content: [{ kind: 'text', text: '<script>window.__xss=1</script>text' }], tool_calls: [], extra: { unknown: 'field' } },
        ] as Hydration['conversation']['items'],
      },
    })));
    const { target } = await mountApp(connection);
    expect(target.querySelector('script')).toBeNull();
    expect((window as unknown as { __xss?: number }).__xss).toBeUndefined();
    expect(target.textContent).toContain('[opaque odd');
    expect(target.textContent).toContain('"unknown":"field"');
  });
});

describe('provisional activity', () => {
  function runningHydration() {
    return makeHydration({
      active_operation: { operation_id: 'op1', request_kind: 'message', status: 'running', admitted_at: '0', agent_type: 'primary', model: 'prov/m', usage: { totals: { by_model: [] } } },
    });
  }

  it('shows bounded text with repeated positions, refusal and tool rows, and discards unmatched progress', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.getSessionHydration).mockResolvedValue(ok(runningHydration()));
    const { target } = await mountApp(connection);
    await vi.waitFor(() => expect(target.querySelector('.activity-label')).not.toBeNull());
    await settle();
    await settle();
    deliverEvent(connection, { kind: 'text_delta', scope: { kind: 'operation', session_id: 'root1', operation_id: 'op1' }, position: 5, content: 'live ' });
    deliverEvent(connection, { kind: 'text_delta', scope: { kind: 'operation', session_id: 'root1', operation_id: 'op1' }, position: 5, content: 'again' });
    deliverEvent(connection, { kind: 'refusal_delta', scope: { kind: 'operation', session_id: 'root1', operation_id: 'op1' }, content: 'nope' });
    deliverEvent(connection, { kind: 'tool_started', scope: { kind: 'operation', session_id: 'root1', operation_id: 'op1' }, call_id: 'c1', ordinal: 0, name: 'read_file' });
    await settle();
    expect(target.querySelector('.provisional-text')?.textContent).toBe('live again');
    expect(target.querySelector('.provisional-refusal')?.textContent).toContain('nope');
    expect(target.querySelector('.activity-tool')?.textContent).toContain('read_file');

    // Unmatched progress is discarded and dirties the hydration; the accepted
    // fresh read replaces the provisional area.
    const readsBefore = vi.mocked(sdk.getSessionHydration).mock.calls.length;
    deliverEvent(connection, { kind: 'text_delta', scope: { kind: 'operation', session_id: 'root1', operation_id: 'other-op' }, position: 0, content: 'stale' });
    await vi.waitFor(() => expect(vi.mocked(sdk.getSessionHydration).mock.calls.length).toBeGreaterThan(readsBefore));
    await settle();
    expect(target.querySelector('.provisional-text')).toBeNull();
  });

  it('marks truncation after the trailing 64 KiB bound', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.getSessionHydration).mockResolvedValue(ok(runningHydration()));
    const { target } = await mountApp(connection);
    await vi.waitFor(() => expect(target.querySelector('.activity-label')).not.toBeNull());
    await settle();
    await settle();
    deliverEvent(connection, { kind: 'text_delta', scope: { kind: 'operation', session_id: 'root1', operation_id: 'op1' }, position: 0, content: 'x'.repeat(70 * 1024) });
    await settle();
    expect(target.querySelector('.truncation')).not.toBeNull();
    expect((target.querySelector('.provisional-text')?.textContent ?? '').length).toBeLessThanOrEqual(64 * 1024);
  });

  it('dirties the displayed view on a configuration hint with a different Session scope', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    await mountApp(connection);
    await settle();
    const readsBefore = vi.mocked(sdk.getSessionHydration).mock.calls.length;
    deliverEvent(connection, {
      kind: 'configuration_changed',
      scope: { kind: 'session', session_id: 'other-session' },
      configuration_revision: { instance_id: INSTANCE, generation: '2' },
    });
    await vi.waitFor(() => expect(vi.mocked(sdk.getConfiguration).mock.calls.length).toBeGreaterThan(1));
    await vi.waitFor(() => expect(vi.mocked(sdk.getSessionHydration).mock.calls.length).toBeGreaterThan(readsBefore));
  });
});

describe('composer', () => {
  it('captures generated submit requests with a minted ID and clears only the matching draft', async () => {
    vi.mocked(sdk.submitSession).mockResolvedValue(ok({ disposition: 'admitted' }));
    const { target } = await mountBasic();
    typeInto(target, 'hello there');
    await settle();
    (target.querySelector('.send') as HTMLElement).click();
    await settle();
    const call = vi.mocked(sdk.submitSession).mock.calls[0][0] as { path: { id: string }; body: { operation_id: string; mode: string; content: unknown[] } };
    expect(call.path).toEqual({ id: 'root1' });
    expect(call.body.operation_id).toMatch(/^[0-9a-f]{32}$/);
    expect(call.body.mode).toBe('regular');
    expect(call.body.content).toEqual([{ kind: 'text', text: 'hello there' }]);
    expect((target.querySelector('textarea') as HTMLTextAreaElement).value).toBe('');
    // The settled attempt leaves the ledger instead of accumulating.
    expect(target.querySelectorAll('.attempt')).toHaveLength(0);
  });

  it('sends queued mode through the explicit Queue action', async () => {
    vi.mocked(sdk.submitSession).mockResolvedValue(ok({ disposition: 'queued' }));
    const { target } = await mountBasic();
    typeInto(target, 'queued message');
    await settle();
    (target.querySelector('.queue') as HTMLElement).click();
    await settle();
    const call = vi.mocked(sdk.submitSession).mock.calls[0][0] as { body: { mode: string; content: unknown[] } };
    expect(call.body.mode).toBe('queued');
    expect(call.body.content).toEqual([{ kind: 'text', text: 'queued message' }]);
  });

  it('keeps the draft and shows the generated error on a known rejection', async () => {
    vi.mocked(sdk.submitSession).mockResolvedValue(httpError(400, { code: 'conflict', message: 'busy now' }));
    const { target } = await mountBasic();
    typeInto(target, 'rejected message');
    await settle();
    (target.querySelector('.send') as HTMLElement).click();
    await settle();
    expect(target.querySelector('.attempt.rejected')?.textContent).toContain('busy now');
    expect((target.querySelector('textarea') as HTMLTextAreaElement).value).toBe('rejected message');
  });

  it('shows Outcome unknown on transport loss and retries with the same ID', async () => {
    vi.mocked(sdk.submitSession)
      .mockResolvedValueOnce(bridgeLoss('net down'))
      .mockResolvedValueOnce(bridgeLoss('net down again'));
    const { target } = await mountBasic();
    typeInto(target, 'uncertain');
    await settle();
    (target.querySelector('.send') as HTMLElement).click();
    await settle();
    const first = (vi.mocked(sdk.submitSession).mock.calls[0][0] as { body: { operation_id: string } }).body.operation_id;
    const unknownRow = target.querySelector('.attempt.unknown') as HTMLElement;
    expect(unknownRow?.textContent).toContain('Outcome unknown');
    // The immutable-ID duplication warning sits beside the Retry affordance
    // BEFORE it is invoked (no post-invocation notice lifetime).
    expect(unknownRow.querySelector('.retry-warning')?.textContent).toContain('may be duplicated');
    unknownRow.querySelectorAll('.attempt-btn')[0].click();
    await settle();
    const second = (vi.mocked(sdk.submitSession).mock.calls[1][0] as { body: { operation_id: string } }).body.operation_id;
    expect(second).toBe(first);
  });

  it('never clears a newer draft on a late success', async () => {
    let resolveSubmit: (value: unknown) => void = () => {};
    vi.mocked(sdk.submitSession).mockImplementationOnce(() => new Promise((resolve) => { resolveSubmit = resolve; }));
    const { target } = await mountBasic();
    typeInto(target, 'first');
    await settle();
    (target.querySelector('.send') as HTMLElement).click();
    typeInto(target, 'second draft');
    await settle();
    resolveSubmit(ok({ disposition: 'admitted' }));
    await settle();
    expect((target.querySelector('textarea') as HTMLTextAreaElement).value).toBe('second draft');
  });

  it('submits on Enter', async () => {
    vi.mocked(sdk.submitSession).mockResolvedValue(ok({ disposition: 'admitted' }));
    const { target } = await mountBasic();
    typeInto(target, 'enter send');
    await settle();
    const textarea = target.querySelector('textarea') as HTMLTextAreaElement;
    textarea.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(vi.mocked(sdk.submitSession)).toHaveBeenCalledTimes(1);
  });

  it('keeps an unknown attempt against its own session when the root switches and does not resend', async () => {
    let resolveSubmit: (value: unknown) => void = () => {};
    vi.mocked(sdk.submitSession).mockImplementationOnce(() => new Promise((resolve) => { resolveSubmit = resolve; }));
    const { target } = await mountBasic();
    typeInto(target, 'delayed');
    await settle();
    (target.querySelector('.send') as HTMLElement).click();
    await settle();
    // Move to root2 while the submit is outstanding.
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    const rows = target.querySelectorAll('dialog .row');
    (rows[1] as HTMLElement).click();
    await settle();
    // The outcome finally arrives as a transport loss: recorded against the
    // captured session, never retransmitted.
    resolveSubmit(bridgeLoss('gone'));
    await settle();
    expect(vi.mocked(sdk.submitSession)).toHaveBeenCalledTimes(1);
    const attempt = { ...vi.mocked(sdk.submitSession).mock.calls[0][0] };
    void attempt;
    // Switching back shows the retained unknown attempt for that session.
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    const backRows = target.querySelectorAll('dialog .row');
    (backRows[0] as HTMLElement).click();
    await settle();
    expect(target.querySelector('.attempt.unknown')?.textContent).toContain('Outcome unknown');
  });
});

describe('controls', () => {
  function activeHydration() {
    return makeHydration({
      active_operation: { operation_id: 'op1', request_kind: 'message', status: 'running', admitted_at: '0', agent_type: 'primary', model: 'prov/m', usage: { totals: { by_model: [] } } },
    });
  }

  it('issues Interrupt and Stop as separate addressed controls and a 204 success writes no note', async () => {
    vi.mocked(sdk.interruptSession).mockResolvedValue(noContent());
    vi.mocked(sdk.stopSession).mockResolvedValue(noContent());
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.getSessionHydration).mockResolvedValue(ok(activeHydration()));
    const { target } = await mountApp(connection);
    await settle();
    const readsBefore = vi.mocked(sdk.getSessionHydration).mock.calls.length;
    (target.querySelector('.interrupt') as HTMLElement).click();
    await settle();
    expect(vi.mocked(sdk.interruptSession)).toHaveBeenCalledWith({ client: expect.anything(), path: { id: 'root1' } });
    // A successful 204 is a valid success: no outcome/error note is written,
    // and the stamped view is refreshed by a further hydration read.
    expect(target.querySelector('.note')).toBeNull();
    await vi.waitFor(() => expect(vi.mocked(sdk.getSessionHydration).mock.calls.length).toBeGreaterThan(readsBefore));
    const readsAfterInterrupt = vi.mocked(sdk.getSessionHydration).mock.calls.length;
    (target.querySelector('.stop') as HTMLElement).click();
    await settle();
    expect(vi.mocked(sdk.stopSession)).toHaveBeenCalledWith({ client: expect.anything(), path: { id: 'root1' } });
    expect(target.querySelector('.note')).toBeNull();
    await vi.waitFor(() => expect(vi.mocked(sdk.getSessionHydration).mock.calls.length).toBeGreaterThan(readsAfterInterrupt));
  });

  it('reports an addressed control rejection and unknown outcome even after a stamp move', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.getSessionHydration).mockResolvedValue(ok(activeHydration()));
    const { target } = await mountApp(connection);
    await settle();
    let resolveInterrupt: (value: unknown) => void = () => {};
    vi.mocked(sdk.interruptSession).mockImplementationOnce(() => new Promise((resolve) => { resolveInterrupt = resolve; }));
    (target.querySelector('.interrupt') as HTMLElement).click();
    // Move the selection to root2 while the root1 control is in flight.
    await selectRoot2(target);
    // The UI and the addressed reader are root2 before the held call lands.
    expect(target.querySelector('.toolbar')?.textContent).toContain('root2');
    await vi.waitFor(() => expect(vi.mocked(sdk.getSessionHydration)).toHaveBeenLastCalledWith({ client: expect.anything(), path: { id: 'root2' } }));
    resolveInterrupt(httpError(500, { code: 'internal', message: 'interrupt blew up' }));
    await settle();
    await settle();
    // The root1 failure is still reported against its captured address...
    expect(target.querySelector('.note.outcome')?.textContent).toContain('Interrupt failed for session root1');
    expect(target.querySelector('.note.outcome')?.textContent).toContain('interrupt blew up');
    // ...with no view adoption into root2: the displayed session stays root2.
    expect(target.querySelector('.toolbar')?.textContent).toContain('root2');
    // An unknown transport outcome is likewise reported for its address.
    let resolveStop: (value: unknown) => void = () => {};
    vi.mocked(sdk.stopSession).mockImplementationOnce(() => new Promise((resolve) => { resolveStop = resolve; }));
    (target.querySelector('.stop') as HTMLElement).click();
    resolveStop(bridgeLoss('stop lost'));
    await settle();
    await settle();
    expect(target.querySelector('.note.outcome')?.textContent).toContain('Stop outcome unknown for session root2');
    expect(target.querySelector('.toolbar')?.textContent).toContain('root2');
  });

  it('interrupts on composer Escape and closes modals first', async () => {
    vi.mocked(sdk.interruptSession).mockResolvedValue(ok(undefined));
    vi.mocked(sdk.listModels).mockResolvedValue(ok({ configuration_revision: { instance_id: INSTANCE, generation: '1' }, models: [] }));
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.getSessionHydration).mockResolvedValue(ok(activeHydration()));
    const { target } = await mountApp(connection);
    await settle();
    // Escape with an open modal closes the modal and never interrupts.
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'm', ctrlKey: true }));
    await settle();
    expect(target.querySelector('dialog')).not.toBeNull();
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    await settle();
    expect(target.querySelector('dialog')).toBeNull();
    expect(vi.mocked(sdk.interruptSession)).not.toHaveBeenCalled();
    // Escape focused in the composer interrupts.
    const textarea = target.querySelector('textarea') as HTMLTextAreaElement;
    textarea.focus();
    textarea.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    await settle();
    expect(vi.mocked(sdk.interruptSession)).toHaveBeenCalledWith({ client: expect.anything(), path: { id: 'root1' } });
  });

  it('submits a manual compact with a minted caller ID', async () => {
    vi.mocked(sdk.compactSession).mockResolvedValue(ok({ operation_id: 'op1', request_kind: 'compact', status: 'running', admitted_at: '0', agent_type: 'primary', model: 'prov/m', usage: { totals: { by_model: [] } } }));
    const { target } = await mountBasic();
    // The toolbar has no duplicate Compact; the composer owns the control.
    expect(target.querySelector('.toolbar .compact-btn')).toBeNull();
    (target.querySelector('.composer .compact') as HTMLElement).click();
    await settle();
    const call = vi.mocked(sdk.compactSession).mock.calls[0][0] as { path: { id: string }; body: { operation_id: string } };
    expect(call.path).toEqual({ id: 'root1' });
    expect(call.body.operation_id).toMatch(/^[0-9a-f]{32}$/);
  });

  it('labels an active compact Operation without naming its transport model', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.getSessionHydration).mockResolvedValue(ok(makeHydration({
      active_operation: { operation_id: 'op1', request_kind: 'compact', status: 'running', admitted_at: '0', agent_type: 'primary', model: 'prov/compact-transport', usage: { totals: { by_model: [] } } },
    })));
    const { target } = await mountApp(connection);
    await settle();
    expect(target.querySelector('.activity-label')?.textContent).toContain('Compacting');
    expect(target.textContent).not.toContain('prov/compact-transport');
  });
});

function selectRoot2(target: HTMLElement) {
  window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
  // The picker opens after its generated read settles; clicking the row is
  // what actually moves the selection.
  return vi.waitFor(() => expect(target.querySelectorAll('dialog .row').length).toBe(2))
    .then(() => {
      (target.querySelectorAll('dialog .row')[1] as HTMLElement).click();
      return settle();
    });
}

describe('keyboard pickers', () => {
  it('Ctrl+M reads models and applies a type-wide SetAgentTypeModel', async () => {
    vi.mocked(sdk.listModels).mockResolvedValue(ok({
      configuration_revision: { instance_id: INSTANCE, generation: '1' },
      models: [
        { ref: 'prov/a', provider: 'prov', provider_name: 'Provider', model: 'a', display_name: 'Model A', context_window: 1000, max_output_tokens: 10, hidden: false, provider_hidden: false, incomplete: false, source: 'user' },
      ],
    }));
    vi.mocked(sdk.setAgentTypeModel).mockResolvedValue(ok({
      configuration_revision: { instance_id: INSTANCE, generation: '2' },
      result: { name: 'primary', description: '', subagent: false, model: 'prov/a', readonly: false, write_dir: '', tools: [], capabilities: [] },
    }));
    const { target } = await mountBasic();
    // After the mutation the Runtime publishes the newer configuration clock in
    // the hydration too, so the fresh mandated read converges.
    let hydrationReads = 0;
    vi.mocked(sdk.getSessionHydration).mockImplementation(async () => {
      hydrationReads += 1;
      return ok(makeHydration(hydrationReads > 1
        ? { configuration_revision: { instance_id: INSTANCE, generation: '2' } }
        : {}));
    });
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'm', ctrlKey: true }));
    await settle();
    expect(vi.mocked(sdk.listModels)).toHaveBeenCalledWith({ client: expect.anything(), query: { all: false } });
    expect(target.textContent).toContain('Model for agent type');
    expect(target.textContent).toContain('primary');
    expect(target.textContent).toContain('all sessions using this agent type');
    (target.querySelector('.mp-btn') as HTMLElement).click();
    await settle();
    (target.querySelector('.mp .item') as HTMLElement).click();
    await settle();
    expect(vi.mocked(sdk.setAgentTypeModel)).toHaveBeenCalledWith({
      client: expect.anything(),
      query: { agent_type: 'primary' },
      body: { model: 'prov/a' },
    });
    // Correction 5: the success is reported, and the current configuration view
    // is refreshed through its OWN generated read (not by force-adopting the
    // mutation's returned clock).
    expect(target.querySelector('.note.outcome')?.textContent).toContain('Model selection applied');
    await vi.waitFor(() => expect(vi.mocked(sdk.getConfiguration).mock.calls.length).toBeGreaterThanOrEqual(2));
  });

  it('a model success from an old selection reports success but pollutes no current floor and starts no repair', async () => {
    vi.mocked(sdk.listModels).mockResolvedValue(ok({
      configuration_revision: { instance_id: INSTANCE, generation: '1' },
      models: [{ ref: 'prov/a', provider: 'prov', provider_name: 'Provider', model: 'a', display_name: 'Model A', context_window: 1000, max_output_tokens: 10, hidden: false, provider_hidden: false, incomplete: false, source: 'user' }],
    }));
    let resolveModel: (value: unknown) => void = () => {};
    vi.mocked(sdk.setAgentTypeModel).mockImplementation(() => new Promise((resolve) => { resolveModel = resolve; }));
    const { target } = await mountBasic();
    const opensBefore = target ? 0 : 0;
    void opensBefore;
    const configReadsBefore = vi.mocked(sdk.getConfiguration).mock.calls.length;
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'm', ctrlKey: true }));
    await settle();
    (target.querySelector('.mp-btn') as HTMLElement).click();
    await settle();
    (target.querySelector('.mp .item') as HTMLElement).click();
    await settle();
    // Move the selection to root2 while the model mutation is in flight.
    await selectRoot2(target);
    // The old-owner response finally lands carrying a foreign clock.
    resolveModel(ok({
      configuration_revision: { instance_id: 'ffffffffffffffffffffffffffffffff', generation: '9' },
      result: { name: 'primary', description: '', subagent: false, model: 'prov/a', readonly: false, write_dir: '', tools: [], capabilities: [] },
    }));
    await settle();
    await settle();
    // Success is still reported for the captured address...
    expect(target.querySelector('.note.outcome')?.textContent).toContain('Model selection applied');
    // ...but no repair/reconnect was triggered and the current configuration
    // refresh is a plain read, not a force-adopted foreign floor.
    expect(vi.mocked(sdk.getConfiguration).mock.calls.length).toBeGreaterThanOrEqual(configReadsBefore + 1);
    expect(target.querySelector('.note.warn')).toBeNull();
  });

  it('Ctrl+S selects open roots through generated reads', async () => {
    const { target } = await mountBasic();
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    const rows = target.querySelectorAll('dialog .row');
    expect(rows).toHaveLength(2);
    expect(rows[0].textContent).toContain('root1');
    expect(rows[1].textContent).toContain('root2');
    (rows[1] as HTMLElement).click();
    await settle();
    expect(vi.mocked(sdk.getSessionHydration)).toHaveBeenLastCalledWith({ client: expect.anything(), path: { id: 'root2' } });
    expect(target.textContent).toContain('root2');
  });
});

describe('child inspection and background', () => {
  function childSession(id: string, lifecycle: 'open' | 'archived'): Session {
    return {
      session_id: id,
      workspace: '/ws',
      parent_session_id: 'root1',
      lifecycle,
      agent_type: 'secondary',
      created_at: '0',
      last_activity: OLDER,
      session_revision: sessionRev('1'),
    };
  }

  it('unions both lifecycle lists with live membership, keeps jobs separate and inspects passively', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.getSessionHydration).mockResolvedValue(ok(makeHydration({
      background: [{ kind: 'child', id: 'child-open' }, { kind: 'child', id: 'child-live-only' }, { kind: 'job', id: 'job1' }],
    })));
    vi.mocked(sdk.listSessions).mockImplementation(async (options) => {
      const lifecycle = (options as { query: { lifecycle: string } }).query.lifecycle;
      if (lifecycle === 'open') {
        return ok([
          rootSession('root1', NEWER),
          childSession('child-open', 'open'),
          childSession('child-both', 'open'),
          { ...childSession('child-other-root', NEWER), parent_session_id: 'other-root' },
        ]);
      }
      return ok([childSession('child-arch', 'archived'), childSession('child-both', 'archived')]);
    });
    const { target } = await mountApp(connection);
    await vi.waitFor(() => expect(target.textContent).toContain('root1'));
    await settle();
    await settle();
    (target.querySelector('.bg-btn') as HTMLElement).click();
    await vi.waitFor(() => expect(target.querySelectorAll('dialog .row').length).toBeGreaterThan(0));
    const rows = [...target.querySelectorAll('dialog .row')];
    const text = rows.map((row) => row.textContent ?? '').join('|');
    expect(text).toContain('child-open');
    expect(text).toContain('child-arch');
    expect(text).toContain('child-live-only');
    // One ID-keyed row even when both independently captured lists overlap.
    expect(rows.filter((row) => row.textContent?.includes('child-both'))).toHaveLength(1);
    // Jobs stay a separate section and are never child rows.
    expect(target.querySelector('.job')?.textContent).toContain('job1');
    // A live child missing from both lists carries no fabricated header.
    const liveRow = rows.find((row) => row.textContent?.includes('child-live-only'));
    expect(liveRow?.textContent).toContain('header not listed');
    expect(liveRow?.textContent).not.toContain('secondary');
    // The child pane has no submit/fork/follow-up controls.
    (liveRow as HTMLElement).click();
    await settle();
    expect(vi.mocked(sdk.getSessionHydration)).toHaveBeenLastCalledWith({ client: expect.anything(), path: { id: 'child-live-only' } });
    const childPane = target.querySelector('.child-pane') as HTMLElement;
    expect(childPane).not.toBeNull();
    expect(childPane.querySelector('textarea')).toBeNull();
    expect(childPane.querySelector('.send')).toBeNull();
    expect(childPane.querySelector('.compact')).toBeNull();
  });

  it('renders the child own pending projection in the passive inspector, including repeats', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.listSessions).mockResolvedValue(ok([
      rootSession('root1', NEWER),
      { ...rootSession('kid', OLDER), parent_session_id: 'root1' },
    ]));
    vi.mocked(sdk.getSessionHydration).mockImplementation(async (options) => {
      const id = (options as { path: { id: string } }).path.id;
      if (id === 'kid') {
        return ok(makeHydration({
          session: rootSession('kid', OLDER),
          pending: {
            steering: [
              { operation_id: 'dup', origin: 'user', content: [{ kind: 'text', text: 'kid steering first' }] },
              { operation_id: 'dup', origin: 'user', content: [{ kind: 'text', text: 'kid steering second' }] },
            ],
            queued: [{ operation_id: 'q1', origin: 'user', content: [{ kind: 'text', text: 'kid queued one' }] }],
          },
        }));
      }
      return ok(makeHydration());
    });
    const { target } = await mountApp(connection);
    await vi.waitFor(() => expect(target.textContent).toContain('root1'));
    await settle();
    await settle();
    (target.querySelector('.bg-btn') as HTMLElement).click();
    await vi.waitFor(() => expect([...target.querySelectorAll('dialog .row')].some((row) => row.textContent?.includes('kid'))).toBe(true));
    ([...target.querySelectorAll('dialog .row')].find((row) => row.textContent?.includes('kid')) as HTMLElement).click();
    await settle();
    const childPane = target.querySelector('.child-pane') as HTMLElement;
    expect(childPane).not.toBeNull();
    // The child's ACTUAL pending projection renders — not an invented empty
    // queue — preserving every repeated occurrence and the two kind labels.
    const steeringRows = [...childPane.querySelectorAll('.queued-msg.steering')];
    expect(steeringRows).toHaveLength(2);
    expect(steeringRows[0].textContent).toContain('kid steering first');
    expect(steeringRows[1].textContent).toContain('kid steering second');
    const queuedRows = [...childPane.querySelectorAll('.queued-msg:not(.steering)')];
    expect(queuedRows).toHaveLength(1);
    expect(queuedRows[0].textContent).toContain('kid queued one');
    // Still passive: no controls alongside the pending rows.
    expect(childPane.querySelector('textarea')).toBeNull();
    expect(childPane.querySelector('.send')).toBeNull();
    expect(childPane.querySelector('.compact')).toBeNull();
    expect(childPane.querySelector('.queue')).toBeNull();
  });

  it('inspector changes never strand an in-flight root read', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.listSessions).mockImplementation(async (options) => {
      const query = (options as { query: { lifecycle: string } }).query;
      if (query.lifecycle === 'open') {
        return ok([rootSession('root1', NEWER), { ...rootSession('kid', NEWER), parent_session_id: 'root1' }]);
      }
      return ok([]);
    });
    vi.mocked(sdk.getSessionHydration).mockImplementation(async (options) => {
      const id = (options as { path: { id: string } }).path.id;
      return ok(makeHydration({
        session: rootSession(id, NEWER),
        conversation: { items: [{ item_id: `first-${id}`, kind: 'input', committed_at: '0', origin: 'user', content: [{ kind: 'text', text: `hello ${id}` }] } as Hydration['conversation']['items'][number]] },
      }));
    });
    const { target } = await mountApp(connection);
    await vi.waitFor(() => expect(target.querySelector('.message.user')?.textContent).toContain('hello root1'));
    // Open the inspector: only the child generation moves.
    (target.querySelector('.bg-btn') as HTMLElement).click();
    await vi.waitFor(() => expect([...target.querySelectorAll('dialog .row')].some((row) => row.textContent?.includes('kid'))).toBe(true));
    ([...target.querySelectorAll('dialog .row')].find((row) => row.textContent?.includes('kid')) as HTMLElement).click();
    await settle();
    expect(target.querySelector('.child-pane')?.textContent).toContain('hello kid');
    // The root keeps its own accepted hydration: independent authority.
    expect(target.querySelector('.content > .message-list, .content .message-list')?.textContent).toContain('hello root1');
  });

  it('Escape from the composer interrupts and leaves the nonmodal child pane open; pane Escape closes only from inside it', async () => {
    vi.mocked(sdk.interruptSession).mockResolvedValue(ok(undefined));
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.listSessions).mockImplementation(async () => ok([rootSession('root1', NEWER), childSession('child-open', 'open')]));
    const { target } = await mountApp(connection);
    await vi.waitFor(() => expect(target.textContent).toContain('root1'));
    await settle();
    (target.querySelector('.bg-btn') as HTMLElement).click();
    await vi.waitFor(() => expect([...target.querySelectorAll('dialog .row')].some((row) => row.textContent?.includes('child-open'))).toBe(true));
    ([...target.querySelectorAll('dialog .row')].find((row) => row.textContent?.includes('child-open')) as HTMLElement).click();
    await settle();
    expect(target.querySelector('.child-pane')).not.toBeNull();
    // Focus the composer and press Escape: interrupt, pane stays.
    const textarea = target.querySelector('.composer textarea, textarea') as HTMLTextAreaElement;
    textarea.focus();
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    await settle();
    expect(vi.mocked(sdk.interruptSession)).toHaveBeenCalledWith({ client: expect.anything(), path: { id: 'root1' } });
    expect(target.querySelector('.child-pane')).not.toBeNull();
    // Focus inside the inspector and press Escape: the pane closes.
    const childClose = target.querySelector('.child-close') as HTMLElement;
    childClose.focus();
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    await settle();
    expect(target.querySelector('.child-pane')).toBeNull();
  });
});

describe('warnings and usage', () => {
  it('shows own warnings and a global-only indicator, filtering other sessions', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.getSessionHydration).mockResolvedValue(ok(makeHydration({
      warnings: [
        { source: 'a', kind: 'k', message: 'own message', session_id: 'root1' },
        { source: 'a', kind: 'k', message: 'global message' },
        { source: 'a', kind: 'k', message: 'other session message', session_id: 'other' },
      ],
    })));
    const { target } = await mountApp(connection);
    await settle();
    (target.querySelector('.warn') as HTMLElement).click();
    await settle();
    expect(target.querySelector('dialog')).not.toBeNull();
    expect(target.textContent).toContain('Global');
    expect(target.textContent).toContain('global message');
    expect(target.textContent).toContain('This session');
    expect(target.textContent).toContain('own message');
    expect(target.textContent).not.toContain('other session message');
  });

  it('renders colliding warning tuples as distinct rows (unique tuples, non-injective key encoding)', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    // Two DISTINCT tuples whose old concatenated key collides: ('p','a','bc')
    // and ('p','ab','c') both concatenate to 'pabc' (within one plugin), and
    // ('x','yz','w') and ('xy','z','w') both concatenate to 'xyzw' (across
    // plugin-source prefixes). Presentation is stateless and must not dedup
    // or mint identities: every distinct report renders once.
    vi.mocked(sdk.getSessionHydration).mockResolvedValue(ok(makeHydration({
      warnings: [
        { source: 'p', kind: 'a', message: 'bc' },
        { source: 'p', kind: 'ab', message: 'c' },
        { source: 'x', kind: 'yz', message: 'w' },
        { source: 'xy', kind: 'z', message: 'w' },
      ],
    })));
    const { target } = await mountApp(connection);
    await settle();
    (target.querySelector('.warn') as HTMLElement).click();
    await settle();
    const rows = [...target.querySelectorAll('.warning.global')];
    expect(rows).toHaveLength(4);
    const texts = rows.map((r) => r.textContent?.replace(/\s+/g, ' ').trim());
    // The within-plugin pair ('p','a','bc') and ('p','ab','c') is two distinct
    // tuples whose display text also collides — multiplicity is preserved as
    // two rows regardless.
    expect(texts.filter((t) => t === 'p · abc')).toHaveLength(2);
    // The across-prefix pair renders distinguishably.
    expect(texts).toContain('x · yzw');
    expect(texts).toContain('xy · zw');
    expect(target.querySelector('.warning.own')).toBeNull();
  });

  it('preserves own/sibling filtering alongside colliding warning keys', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.getSessionHydration).mockResolvedValue(ok(makeHydration({
      warnings: [
        { source: 'p', kind: 'a', message: 'bc', session_id: 'root1' },
        { source: 'p', kind: 'ab', message: 'c', session_id: 'root1' },
        { source: 'p', kind: 'a', message: 'bc', session_id: 'sibling' },
      ],
    })));
    const { target } = await mountApp(connection);
    await settle();
    (target.querySelector('.warn') as HTMLElement).click();
    await settle();
    // The two colliding OWN tuples both render; the sibling is filtered out.
    const ownRows = [...target.querySelectorAll('.warning.own')];
    expect(ownRows).toHaveLength(2);
    expect(target.textContent).not.toContain('sibling');
  });

  it('replaces warning rows on authoritative replacement', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    const first = ok(makeHydration({
      warnings: [{ source: 'p', kind: 'a', message: 'bc' }, { source: 'p', kind: 'ab', message: 'c' }],
    }));
    const second = ok(makeHydration({
      session_revision: { instance_id: INSTANCE, durable_revision: '2', local_revision: '0' },
      warnings: [{ source: 'q', kind: 'r', message: 's' }],
    }));
    vi.mocked(sdk.getSessionHydration).mockResolvedValueOnce(first).mockResolvedValue(second);
    const { target } = await mountApp(connection);
    await settle();
    (target.querySelector('.warn') as HTMLElement).click();
    await settle();
    expect([...target.querySelectorAll('.warning.global')]).toHaveLength(2);
    // A newer hydration (hint-driven) authoritatively replaces the rows.
    deliverEvent(connection, {
      kind: 'session_changed',
      scope: { kind: 'session', session_id: 'root1' },
      session_revision: { instance_id: INSTANCE, durable_revision: '2', local_revision: '0' },
    });
    await vi.waitFor(() => expect([...target.querySelectorAll('.warning.global')]).toHaveLength(1));
    expect(target.querySelector('.warning.global')?.textContent?.replace(/\s+/g, ' ').trim()).toBe('q · rs');
  });

  it('displays exact decimal usage without rounding', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.getSessionHydration).mockResolvedValue(ok(makeHydration({
      usage: {
        totals: {
          by_model: [
            { model: 'p/m', usage: { input_tokens: '9007199254740993', cached_input_tokens: '2', output_tokens: '3' } },
            { model: 'p/m2', usage: { input_tokens: '2', cached_input_tokens: '0', output_tokens: '9007199254740992' } },
          ],
        },
        context: { used_tokens: '123456789012345678901', context_window: 200000 },
      },
    })));
    const { target } = await mountApp(connection);
    await settle();
    // input total 9007199254740993+2 = 9007199254740995; output total
    // 3+9007199254740992 = 9007199254740995; cache total 2.
    expect(target.querySelector('.tokens')?.textContent).toContain('↑ 9007199254740995');
    expect(target.querySelector('.tokens')?.textContent).toContain('↓ 9007199254740995');
    expect(target.querySelector('.context')?.textContent).toContain('123456789012345678901');
  });
});

describe('repair', () => {
  it('carries generation identity: a current-generation loss during a fresh read re-drives the same owner; a subsumed loss is ignored', async () => {
    const { connection } = await mountBasic();
    const opensBefore = connection.openMock.mock.calls.length;
    const readsAtBoot = vi.mocked(sdk.getSessionHydration).mock.calls.length;
    connection.openMock.mockResolvedValue({ instance_id: INSTANCE, generation: '2', workspace: '/ws' });
    // Gate the repair's fresh hydration so the second loss lands strictly
    // before the read completes.
    let settleRead: (value: unknown) => void = () => {};
    vi.mocked(sdk.getSessionHydration).mockImplementationOnce(
      () => new Promise((resolve) => { settleRead = resolve; }),
    );
    // A genuinely current (installed generation '1') loss.
    connection.failureHandler?.(new Error('first loss'), '1');
    await vi.waitFor(() => expect(vi.mocked(sdk.getSessionHydration).mock.calls.length).toBe(readsAtBoot + 1));
    await vi.waitFor(() => expect(connection.openMock.mock.calls.length).toBe(opensBefore + 1));
    // Loss of the NEWLY installed generation '2' while the owner's read is
    // still gated must coalesce into a re-drive, not be swallowed.
    connection.failureHandler?.(new Error('second loss'), '2');
    await settle();
    await settle();
    // The running owner has not finished the gated read, so it has not
    // released; the coalesced re-drive opens exactly one more time.
    // The Runtime's fresh read is monotonic: the held read and the steady-state
    // mock must not regress, or a mandated fresh read is always "stale". Resolve
    // the gated read with the same current body the steady state serves.
    settleRead(ok(makeHydration()));
    await vi.waitFor(() => expect(connection.openMock.mock.calls.length).toBe(opensBefore + 2));
    // A late callback for an already-replaced generation is subsumed: it must
    // not invalidate the freshly adopted views or open again.
    const opensSettled = connection.openMock.mock.calls.length;
    connection.failureHandler?.(new Error('stale old-generation loss'), '1');
    await settle();
    await settle();
    expect(connection.openMock.mock.calls.length).toBe(opensSettled);
  });

  it('does not reconnect after disposal', async () => {
    const { connection, component } = await mountBasic();
    unmount(component);
    const opensBefore = connection.openMock.mock.calls.length;
    connection.failureHandler?.(new Error('late stream loss'), '1');
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(connection.openMock.mock.calls.length).toBe(opensBefore);
    expect(connection.disposeMock).toHaveBeenCalled();
  });

  it('a repair whose fresh read hits invalid delivery reports inside the owner without recursion', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    await mountApp(connection);
    await settle();
    const opensBefore = connection.openMock.mock.calls.length;
    const readsAtBoot = vi.mocked(sdk.getSessionHydration).mock.calls.length;
    connection.openMock.mockResolvedValue({ instance_id: INSTANCE, generation: '2', workspace: '/ws' });
    let settleRead: (value: unknown) => void = () => {};
    vi.mocked(sdk.getSessionHydration).mockImplementationOnce(
      () => new Promise((resolve) => { settleRead = resolve; }),
    );
    connection.failureHandler?.(new Error('stream lost'), '1');
    await vi.waitFor(() => expect(vi.mocked(sdk.getSessionHydration).mock.calls.length).toBe(readsAtBoot + 1));
    // The owner's fresh read decodes to an invalid body: it reports inside the
    // owner as that read's failure instead of a synchronous recursive Open.
    settleRead(receivedInvalid('hydration body undecodable'));
    await settle();
    await settle();
    await settle();
    expect(document.body.textContent).toContain('invalid');
    // No unbounded recursion: at most the one coalesced re-drive happened.
    expect(connection.openMock.mock.calls.length).toBeLessThanOrEqual(opensBefore + 2);
  });
});
