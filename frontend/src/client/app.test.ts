// @vitest-environment happy-dom
// The mounted target App over the real reader, composer and renderer with
// faithfully shaped native call results: every received status arrives as the
// lossless local result {status,body,invalid} and only a binding
// rejection is loss. The real-stack owner (stack.test.ts) drives the actual
// generated host surface on top of these; this file covers the view, keyboard,
// membership, draft and repair behaviors.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { mount, tick, unmount } from 'svelte';

// The native bindings module: the App and its editors import Call, AppVersion,
// PickDirectory and SetTitle directly, so the whole module is replaced.
vi.mock('../../client-bindings/wailsjs/go/desktopclient/Host', () => ({
  AppVersion: vi.fn(),
  Call: vi.fn(),
  NextEvent: vi.fn(),
  Open: vi.fn(),
  PickDirectory: vi.fn(),
  SetTitle: vi.fn(),
}));

// The Wails runtime bridge: the assistant Markdown link action forwards the
// exact href here instead of letting the webview navigate itself.
vi.mock('../../client-bindings/wailsjs/runtime/runtime', () => ({ BrowserOpenURL: vi.fn() }));

import type { Hydration } from '../generated/protocol/types.gen';
import { AppVersion, Call, PickDirectory, SetTitle } from '../../client-bindings/wailsjs/go/desktopclient/Host';
import { BrowserOpenURL } from '../../client-bindings/wailsjs/runtime/runtime';
import { SessionReader } from './reader';
import { AttemptLedger } from './attempts';
import { assertNativeInvocations } from './nativecontract';
import {
  INSTANCE,
  NEWER,
  OLDER,
  hydration,
  json,
  native,
  revision,
  session,
} from './test-fixtures';

// The scripted operations the mounted view addresses, keyed by the names its
// call sites use. The shared Call binding dispatches every composed envelope
// to them by generated operation name and records the exact params text; a
// scripted loss marker rejects the binding like a native failure.
const sdk: Record<string, ReturnType<typeof vi.fn>> = {
  ArchiveSession: vi.fn(),
  CompactSession: vi.fn(),
  ConnectProvider: vi.fn(),
  CreateProvider: vi.fn(),
  CreateSession: vi.fn(),
  DeleteProviderDetail: vi.fn(),
  DeleteProviderModel: vi.fn(),
  DeleteSession: vi.fn(),
  DiscoverProviderCandidates: vi.fn(),
  DiscoverProviderModelCandidates: vi.fn(),
  DisconnectProvider: vi.fn(),
  ForkSession: vi.fn(),
  GetConfiguration: vi.fn(),
  GetProviderDetail: vi.fn(),
  GetSessionCodeSnapshots: vi.fn(),
  GetSessionHistory: vi.fn(),
  GetSessionHydration: vi.fn(),
  GetWarnings: vi.fn(),
  InterruptSession: vi.fn(),
  ListModels: vi.fn(),
  ListProviders: vi.fn(),
  ListSessions: vi.fn(),
  ListWorkspaces: vi.fn(),
  ReadWorkspaceFile: vi.fn(),
  ReloadConfiguration: vi.fn(),
  ReopenSession: vi.fn(),
  ResetProviderField: vi.fn(),
  ResetProviderModelField: vi.fn(),
  RevertSessionCode: vi.fn(),
  SetAgentTypeModel: vi.fn(),
  SetSessionAgentType: vi.fn(),
  StopSession: vi.fn(),
  SubmitSession: vi.fn(),
  UpdateConfigurationSettings: vi.fn(),
  UpdateProviderDetail: vi.fn(),
  UpdateProviderModel: vi.fn(),
};
vi.mocked(Call).mockImplementation(async (_generation, operation, params) => {
  const scripted = sdk[operation];
  if (scripted === undefined) throw new Error(`no scripted operation for ${operation}`);
  const result = await scripted(params);
  // A successful binding call always returns a struct; an unscripted
  // operation stands in for a failed binding call.
  if (result === undefined) throw new Error('transport unavailable');
  return result;
});
const mounted: Array<ReturnType<typeof mount>> = [];

function fakeConnection() {
  const client = { generation: '1' };
  let installed = '';
  let installedInfo: { instance_id: string; generation: string; workspace: string } | null = null;
  const connection: {
    handler: ((event: never) => void) | null;
    failureHandler: ((error: unknown, generation: string) => void) | null;
    openMock: ReturnType<typeof vi.fn>;
    disposeMock: ReturnType<typeof vi.fn>;
    getClient: () => object;
    setEventHandler: (handler: (event: never) => void) => void;
    setFailureHandler: (handler: (error: unknown, generation: string) => void) => void;
    isCurrentGeneration: (generation: string) => boolean;
    getInfo: () => { instance_id: string; generation: string; workspace: string } | null;
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
    // Mirrors RuntimeConnection: the installed info retires at open start and
    // after disposal, and only the resolved object installs.
    getInfo: () => installedInfo,
    open: async () => {
      installedInfo = null;
      const info = await connection.openMock();
      installedInfo = info;
      installed = info.generation;
      return info;
    },
    dispose: () => { connection.disposeMock(); installedInfo = null; installed = ''; },
  };
  return connection;
}

async function mountApp(connection: ReturnType<typeof fakeConnection>) {
  const { default: App } = await import('./App.svelte');
  const target = document.createElement('div');
  document.body.appendChild(target);
  const component = mount(App, { target, props: { connection } });
  mounted.push(component);
  await vi.waitFor(() => expect(vi.mocked(sdk.GetSessionHydration)).toHaveBeenCalled());
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
// two open roots, an empty hydration and accepted global clocks. The native
// bindings get their inert defaults here; individual tests override them.
function basicMocks(connection: ReturnType<typeof fakeConnection>) {
  connection.openMock.mockResolvedValue({ instance_id: INSTANCE, generation: '1', workspace: '/ws' });
  vi.mocked(sdk.ListSessions).mockImplementation(async () => json(200, [session('root1', NEWER), session('root2', OLDER)]));
  vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, hydration()));
  vi.mocked(sdk.GetConfiguration).mockResolvedValue(json(200, {
    configuration_revision: { instance_id: INSTANCE, generation: '1' },
    settings: { sessions: { auto_archive: false, archive_after_days: 1, delete_after_archive_days: 1 }, plugins: {} },
    agents: [
      { name: 'primary', description: 'Primary agent', subagent: false, model: '', readonly: false, write_dir: '', tools: [], capabilities: [] },
      { name: 'compact', description: 'Compaction', subagent: false, model: '', readonly: true, write_dir: '', tools: [], capabilities: [] },
    ],
    providers: [],
  }));
  vi.mocked(sdk.GetWarnings).mockResolvedValue(json(200, {
    warnings_revision: { instance_id: INSTANCE, revision: '1' },
    warnings: [],
  }));
  vi.mocked(SetTitle).mockResolvedValue(undefined);
  vi.mocked(PickDirectory).mockResolvedValue('');
  vi.mocked(AppVersion).mockResolvedValue('dev (test)');
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
  // The mounted UI's recorded native invocations are asserted here, outside
  // every application promise and catch, before clearAllMocks removes them: a
  // violating envelope fails the teardown even when the application's own
  // expected outcome still rendered. The DOM and mock cleanup stays in the
  // finally block so an assertion failure cannot contaminate the next test.
  const mountedUI = mounted.length > 0;
  for (const component of mounted.splice(0)) {
    unmount(component);
  }
  await tick();
  try {
    if (mountedUI) {
      assertNativeInvocations(vi.mocked(Call).mock.calls);
    }
  } finally {
    document.body.innerHTML = '';
    vi.clearAllMocks();
    // clearAllMocks does not reset implementations; a previous test's
    // mockImplementation can leak into the next test.  Reset the two
    // highest-traffic mocks to a neutral default so every test that needs
    // specific behavior sets its own implementation explicitly.
    vi.mocked(sdk.ListSessions).mockImplementation(async () => json(200, []));
    vi.mocked(sdk.GetSessionHydration).mockImplementation(async () => json(200, hydration()));
  }
});

describe('initial composition', () => {
  it('hydrates the newest open root and never selects a model automatically', async () => {
    const { target } = await mountBasic();
    expect(vi.mocked(sdk.ListSessions)).toHaveBeenCalledWith('{"query":{"workspace":"/ws","lifecycle":"open"}}');
    expect(vi.mocked(sdk.GetSessionHydration)).toHaveBeenCalledWith('{"session_id":"root1"}');
    expect(vi.mocked(sdk.ListModels)).not.toHaveBeenCalled();
    expect(vi.mocked(sdk.SetAgentTypeModel)).not.toHaveBeenCalled();
    expect(target.textContent).toContain('root1');
  });

  it('reads the independent global floors at connection and gates hydration with them', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    // The global configuration clock advances to generation 2 while the
    // hydration keeps carrying generation 1: the accepted floor rejects it
    // and the reader re-reads until the projection catches up.
    vi.mocked(sdk.GetConfiguration).mockResolvedValue(json(200, {
      configuration_revision: { instance_id: INSTANCE, generation: '2' },
      settings: { sessions: { auto_archive: false, archive_after_days: 1, delete_after_archive_days: 1 }, plugins: {} },
      agents: [],
      providers: [],
    }));
    const hydrationV1 = hydration();
    const hydrationV2 = hydration({
      configuration_revision: { instance_id: INSTANCE, generation: '2' },
      conversation: { items: [{ item_id: 'i9', kind: 'input', committed_at: '0', origin: 'user', content: [{ kind: 'text', text: 'item i9' }] } as Hydration['conversation']['items'][number]] },
    });
    vi.mocked(sdk.GetSessionHydration)
      .mockResolvedValueOnce(json(200, hydrationV1))
      .mockResolvedValue(json(200, hydrationV2));
    await mountApp(connection);
    await settle();
    // The stale-body hydration never rendered; the accepted-floor fresh read did.
    expect(vi.mocked(sdk.GetSessionHydration).mock.calls.length).toBeGreaterThanOrEqual(2);
    await vi.waitFor(() => expect(document.body.textContent).toContain('item i9'));
  });

  it('rejects a malformed initial global revision before adopting it', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    // The very first configuration read carries a revision outside the closed
    // decimal shape. No reader floor exists yet, so only the initial global
    // validation can reject it; the repair's own read is valid.
    vi.mocked(sdk.GetConfiguration)
      .mockResolvedValueOnce(json(200, {
        configuration_revision: { instance_id: INSTANCE, generation: 'not-a-number' },
        settings: { sessions: { auto_archive: false, archive_after_days: 1, delete_after_archive_days: 1 }, plugins: {} },
        agents: [],
        providers: [],
      }))
      .mockResolvedValue(json(200, {
        configuration_revision: { instance_id: INSTANCE, generation: '1' },
        settings: { sessions: { auto_archive: false, archive_after_days: 1, delete_after_archive_days: 1 }, plugins: {} },
        agents: [],
        providers: [],
      }));
    // No hydration floor ever exists: the initial global comparison is the only
    // place that can reject this revision.
    vi.mocked(sdk.GetSessionHydration).mockImplementation(() => new Promise(() => {}) as never);
    const { target } = await mountAppNoWait(connection);
    await vi.waitFor(() => expect(connection.openMock.mock.calls.length).toBe(2));
    expect(target.querySelector('.note.warn')).not.toBeNull();
  });

  it('repairs once on a foreign global floor and converges after the reopen clears it', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    const foreign = { instance_id: 'ffffffffffffffffffffffffffffffff', generation: '1' };
    vi.mocked(sdk.GetConfiguration)
      .mockResolvedValueOnce(json(200, {
        configuration_revision: foreign,
        settings: { sessions: { auto_archive: false, archive_after_days: 1, delete_after_archive_days: 1 }, plugins: {} },
        agents: [],
        providers: [],
      }))
      .mockResolvedValue(json(200, {
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

  it('a repair discards a held initial list and never recomposes an absent selection', async () => {
    const connection = fakeConnection();
    connection.openMock.mockResolvedValue({ instance_id: INSTANCE, generation: '1', workspace: '/ws' });
    vi.mocked(sdk.GetConfiguration).mockResolvedValue(json(200, {
      configuration_revision: { instance_id: INSTANCE, generation: '1' },
      settings: { sessions: { auto_archive: false, archive_after_days: 1, delete_after_archive_days: 1 }, plugins: {} },
      agents: [], providers: [],
    }));
    vi.mocked(sdk.GetWarnings).mockResolvedValue(json(200, { warnings_revision: { instance_id: INSTANCE, revision: '1' }, warnings: [] }));
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, hydration()));
    // The boot list is held; a repair then completes while it is parked.
    const { promise: listGate, resolve: releaseList } = Promise.withResolvers();
    let listCalls = 0;
    vi.mocked(sdk.ListSessions).mockImplementation(async () => {
      listCalls += 1;
      if (listCalls === 1) {
        return listGate;
      }
      return json(200, [session('root1', NEWER)]);
    });
    const { target } = await mountAppNoWait(connection);
    await settle();
    // Supersede the held boot list with a repair (new generation).
    connection.openMock.mockResolvedValue({ instance_id: INSTANCE, generation: '2', workspace: '/ws' });
    connection.failureHandler?.(new Error('loss during boot'), '1');
    await vi.waitFor(() => expect(vi.mocked(sdk.GetConfiguration).mock.calls.length).toBeGreaterThan(1));
    // Release the stale boot list AFTER the repair; its adoption is discarded
    // and the ended attempt is never rescheduled by the repair.
    releaseList(json(200, [session('root-stale', NEWER)]));
    await settle();
    await settle();
    expect(target.textContent).not.toContain('root-stale');
    expect(target.textContent).toContain('no session');
  });

  it('creates a primary session when no open root exists', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ListSessions).mockResolvedValue(json(200, []));
    vi.mocked(sdk.CreateSession).mockResolvedValue(json(200, session('primary1', NEWER)));
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, hydration({ session: session('primary1', NEWER) })));
    const { target } = await mountApp(connection);
    expect(vi.mocked(sdk.CreateSession)).toHaveBeenCalledWith('{"body":{"workspace":"/ws","agent_type":"primary"}}');
    expect(vi.mocked(sdk.GetSessionHydration)).toHaveBeenCalledWith('{"session_id":"primary1"}');
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
      { item_id: 'a1', kind: 'assistant', committed_at: '0', operation_id: 'op-a1', status: 'completed', source: 'prov/model', content: [{ kind: 'text', text: '**bold** reply' }], refusal: 'refused here', tool_calls: [{ id: 'c1', name: 'read_file', arguments: '{"path":"a.go"}', status: 'success', content: 'ok', normalized_arguments: { raw: JSON.rawJSON('9007199254740993'), huge: JSON.rawJSON('1e400') }, metadata: { note: { n: JSON.rawJSON('9007199254740993') } } }] },
      { item_id: 's1', kind: 'signal', committed_at: '0', subtype: 'interruption', content: 'stopped' },
      { item_id: 'c1', kind: 'context_checkpoint', committed_at: '0', strategy: 'summarize', model: 'prov/compact', messages: [{ role: 'assistant', source: 'prov/compact', content: [{ kind: 'text', text: 'summary here' }] }] },
      { item_id: 'o1', kind: 'operation_end', committed_at: '0', operation_id: 'op-o1', status: 'failure', detail: 'failed detail' },
    ];
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, hydration({
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
    // Exact numeric lexemes survive the normalized-argument and metadata
    // stringify displays verbatim.
    expect(target.textContent).toContain('9007199254740993');
    expect(target.textContent).toContain('1e400');
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
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, hydration({
      conversation: {
        items: [
          { item_id: 'u1', kind: 'input', committed_at: '0', origin: 'user', content: [{ kind: 'opaque', opaque_wire_type: 'odd', extra: { x: '<b>not html</b>', big: JSON.rawJSON('9007199254740993'), huge: JSON.rawJSON('1e400') } }] },
          { item_id: 'a1', kind: 'assistant', committed_at: '0', status: 'completed', source: 'p/m', content: [{ kind: 'text', text: '<script>window.__xss=1</script>text' }], tool_calls: [], extra: { unknown: 'field', n: JSON.rawJSON('9007199254740993') } },
        ] as Hydration['conversation']['items'],
      },
    })));
    const { target } = await mountApp(connection);
    expect(target.querySelector('script')).toBeNull();
    expect((window as unknown as { __xss?: number }).__xss).toBeUndefined();
    expect(target.textContent).toContain('[opaque odd');
    expect(target.textContent).toContain('"unknown":"field"');
    // Part-extra and item-extra numeric lexemes stay exact.
    expect(target.textContent).toContain('9007199254740993');
    expect(target.textContent).toContain('1e400');
  });

  it('keeps exact lexemes through older-page reads and an unchanged Fork', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    const lex = JSON.rawJSON('9007199254740993');
    const overflow = JSON.rawJSON('1e400');
    const opaqueInput = { ...inputItem('i-lex', 'user'), content: [{ kind: 'text' as const, text: 'hello' }, { kind: 'opaque' as const, opaque_wire_type: 'w', extra: { big: lex, huge: overflow } }] };
    const opaqueAssistant = { item_id: 'a-lex', kind: 'assistant' as const, committed_at: '0', status: 'completed' as const, source: 'p/m', content: [{ kind: 'text' as const, text: 'answer' }], tool_calls: [], extra: { n: lex } };
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, hydration({
      conversation: { items: [opaqueAssistant as Hydration['conversation']['items'][number]], older_cursor: 'p1' },
    })));
    vi.mocked(sdk.GetSessionHistory).mockResolvedValue(json(200, { session_revision: revision('1'), items: [opaqueInput], older_cursor: null }));
    vi.mocked(sdk.ForkSession).mockResolvedValue(json(200, { session: session('dest1', NEWER), operation: runningOperation('caller') }));
    const { target } = await mountApp(connection);
    await settle();
    (target.querySelector('[aria-label="Load older messages"]') as HTMLElement).click();
    await vi.waitFor(() => expect(target.textContent).toContain('1e400'));
    expect(target.textContent).toContain('9007199254740993');
    (target.querySelector('[aria-label="Fork from input i-lex"]') as HTMLElement).click();
    await settle();
    const textarea = target.querySelector('[aria-label="Replacement content JSON"]') as HTMLTextAreaElement;
    expect(textarea.value).toContain('"big": 9007199254740993');
    expect(textarea.value).toContain('"huge": 1e400');
    (target.querySelector('[aria-label="Submit fork"]') as HTMLElement).click();
    await settle();
    await settle();
    const body = bodyOf(vi.mocked(sdk.ForkSession).mock.calls[0][0] as string);
    expect(body).toContain('"big": 9007199254740993');
    expect(body).toContain('"huge": 1e400');
  });
});

describe('provisional activity', () => {
  function runningHydration() {
    return hydration({
      active_operation: { operation_id: 'op1', request_kind: 'message', status: 'running', admitted_at: '0', agent_type: 'primary', model: 'prov/m', usage: { totals: { by_model: [] } } },
    });
  }

  it('shows bounded text with repeated positions, refusal and tool rows, and discards unmatched progress', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, runningHydration()));
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
    const readsBefore = vi.mocked(sdk.GetSessionHydration).mock.calls.length;
    deliverEvent(connection, { kind: 'text_delta', scope: { kind: 'operation', session_id: 'root1', operation_id: 'other-op' }, position: 0, content: 'stale' });
    await vi.waitFor(() => expect(vi.mocked(sdk.GetSessionHydration).mock.calls.length).toBeGreaterThan(readsBefore));
    await settle();
    expect(target.querySelector('.provisional-text')).toBeNull();
  });

  it('renders progress delivered before an unobserved terminal only in the provisional area', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, runningHydration()));
    const { target } = await mountApp(connection);
    await vi.waitFor(() => expect(target.querySelector('.activity-label')).not.toBeNull());
    await settle();
    await settle();
    // Valid queued progress is delivered before the terminal failure is
    // observed.
    deliverEvent(connection, { kind: 'text_delta', scope: { kind: 'operation', session_id: 'root1', operation_id: 'op1' }, position: 0, content: 'live text' });
    await settle();
    expect(target.querySelector('.provisional-text')?.textContent).toBe('live text');
    // The provisional delivery never becomes a canonical transcript item.
    for (const node of target.querySelectorAll('.message, .system-msg, .queued-msg')) {
      expect(node.textContent).not.toContain('live text');
    }
    // The terminal failure is observed afterwards; the existing repair
    // clears the provisional progress.
    connection.failureHandler?.(new Error('event stream resync required'), '1');
    await vi.waitFor(() => expect(target.querySelector('.provisional-text')).toBeNull());
  });

  it('marks truncation after the trailing 64 KiB bound', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, runningHydration()));
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
    const readsBefore = vi.mocked(sdk.GetSessionHydration).mock.calls.length;
    deliverEvent(connection, {
      kind: 'configuration_changed',
      scope: { kind: 'session', session_id: 'other-session' },
      configuration_revision: { instance_id: INSTANCE, generation: '2' },
    });
    await vi.waitFor(() => expect(vi.mocked(sdk.GetConfiguration).mock.calls.length).toBeGreaterThan(1));
    await vi.waitFor(() => expect(vi.mocked(sdk.GetSessionHydration).mock.calls.length).toBeGreaterThan(readsBefore));
  });
});

describe('composer', () => {
  it('captures generated submit requests with a minted ID and clears only the matching draft', async () => {
    vi.mocked(sdk.SubmitSession).mockResolvedValue(json(200, { disposition: 'admitted' }));
    const { target } = await mountBasic();
    typeInto(target, 'hello there');
    await settle();
    (target.querySelector('.send') as HTMLElement).click();
    await settle();
    const call = JSON.parse(vi.mocked(sdk.SubmitSession).mock.calls[0][0]) as { session_id: string; body: { operation_id: string; mode: string; content: unknown[] } };
    expect(call.session_id).toBe('root1');
    expect(call.body.operation_id).toMatch(/^[0-9a-f]{32}$/);
    expect(call.body.mode).toBe('regular');
    expect(call.body.content).toEqual([{ kind: 'text', text: 'hello there' }]);
    expect((target.querySelector('textarea') as HTMLTextAreaElement).value).toBe('');
    // The settled attempt leaves the ledger instead of accumulating.
    expect(target.querySelectorAll('.attempt')).toHaveLength(0);
  });

  it('sends queued mode through the explicit Queue action', async () => {
    vi.mocked(sdk.SubmitSession).mockResolvedValue(json(200, { disposition: 'queued' }));
    const { target } = await mountBasic();
    typeInto(target, 'queued message');
    await settle();
    (target.querySelector('.queue') as HTMLElement).click();
    await settle();
    const call = JSON.parse(vi.mocked(sdk.SubmitSession).mock.calls[0][0]) as { body: { mode: string; content: unknown[] } };
    expect(call.body.mode).toBe('queued');
    expect(call.body.content).toEqual([{ kind: 'text', text: 'queued message' }]);
  });

  it('forms independent work from unchanged text while a response is held', async () => {
    const { promise: firstGate, resolve: releaseFirst } = Promise.withResolvers();
    vi.mocked(sdk.SubmitSession)
      .mockImplementationOnce(() => firstGate as never)
      .mockResolvedValue(json(200, { disposition: 'queued' }));
    const { target } = await mountBasic();
    typeInto(target, 'same text');
    await settle();
    (target.querySelector('.send') as HTMLElement).click();
    await settle();
    // The held response blocks nothing: the explicit Queue action forms
    // independent work from the unchanged text with its own caller ID and
    // captured mode/content.
    (target.querySelector('.queue') as HTMLElement).click();
    await settle();
    const calls = vi.mocked(sdk.SubmitSession).mock.calls.map((call) =>
      JSON.parse(call[0]) as { body: { operation_id: string; mode: string; content: unknown[] } });
    expect(calls).toHaveLength(2);
    expect(calls[0].body.operation_id).toMatch(/^[0-9a-f]{32}$/);
    expect(calls[1].body.operation_id).toMatch(/^[0-9a-f]{32}$/);
    expect(calls[0].body.operation_id).not.toBe(calls[1].body.operation_id);
    expect(calls[0].body.mode).toBe('regular');
    expect(calls[1].body.mode).toBe('queued');
    expect(calls[0].body.content).toEqual([{ kind: 'text', text: 'same text' }]);
    expect(calls[1].body.content).toEqual([{ kind: 'text', text: 'same text' }]);
    releaseFirst(json(200, { disposition: 'admitted' }));
    await settle();
  });

  it('keeps the draft and shows the generated error on a known rejection', async () => {
    vi.mocked(sdk.SubmitSession).mockResolvedValue(json(400, { code: 'conflict', message: 'busy now' }));
    const { target } = await mountBasic();
    typeInto(target, 'rejected message');
    await settle();
    (target.querySelector('.send') as HTMLElement).click();
    await settle();
    expect(target.querySelector('.attempt.rejected')?.textContent).toContain('busy now');
    expect((target.querySelector('textarea') as HTMLTextAreaElement).value).toBe('rejected message');
  });

  it('a 200 null document is never a successful Submit: the draft and attempt stay as unknown', async () => {
    vi.mocked(sdk.SubmitSession).mockResolvedValue({ status: 200, body: 'null', invalid: false });
    const { target } = await mountBasic();
    typeInto(target, 'undecodable outcome');
    await settle();
    (target.querySelector('.send') as HTMLElement).click();
    await settle();
    await settle();
    // Invalid delivery is not success: the matching draft is not cleared, the
    // attempt is retained as unknown, and nothing is retransmitted.
    expect((target.querySelector('textarea') as HTMLTextAreaElement).value).toBe('undecodable outcome');
    expect(target.querySelector('.attempt.unknown')?.textContent).toContain('Outcome unknown');
    expect(target.querySelector('.attempt.rejected')).toBeNull();
    expect(vi.mocked(sdk.SubmitSession)).toHaveBeenCalledTimes(1);
  });

  it('shows Outcome unknown on transport loss and retries with the same ID', async () => {
    vi.mocked(sdk.SubmitSession)
      .mockRejectedValueOnce(new Error('net down'))
      .mockRejectedValueOnce(new Error('net down again'));
    const { target } = await mountBasic();
    typeInto(target, 'uncertain');
    await settle();
    (target.querySelector('.send') as HTMLElement).click();
    await settle();
    const first = (JSON.parse(vi.mocked(sdk.SubmitSession).mock.calls[0][0]) as { body: { operation_id: string } }).body.operation_id;
    const unknownRow = target.querySelector('.attempt.unknown') as HTMLElement;
    expect(unknownRow?.textContent).toContain('Outcome unknown');
    // The immutable-ID duplication warning sits beside the Retry affordance
    // BEFORE it is invoked (no post-invocation notice lifetime).
    expect(unknownRow.querySelector('.retry-warning')?.textContent).toContain('may be duplicated');
    unknownRow.querySelectorAll('.attempt-btn')[0].click();
    await settle();
    const second = (JSON.parse(vi.mocked(sdk.SubmitSession).mock.calls[1][0]) as { body: { operation_id: string } }).body.operation_id;
    expect(second).toBe(first);
  });

  it('never clears a newer draft on a late success', async () => {
    const { promise: submitGate, resolve: resolveSubmit } = Promise.withResolvers();
    vi.mocked(sdk.SubmitSession).mockImplementationOnce(() => submitGate);
    const { target } = await mountBasic();
    typeInto(target, 'first');
    await settle();
    (target.querySelector('.send') as HTMLElement).click();
    typeInto(target, 'second draft');
    await settle();
    resolveSubmit(json(200, { disposition: 'admitted' }));
    await settle();
    expect((target.querySelector('textarea') as HTMLTextAreaElement).value).toBe('second draft');
  });

  it('submits on Enter', async () => {
    vi.mocked(sdk.SubmitSession).mockResolvedValue(json(200, { disposition: 'admitted' }));
    const { target } = await mountBasic();
    typeInto(target, 'enter send');
    await settle();
    const textarea = target.querySelector('textarea') as HTMLTextAreaElement;
    textarea.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    await settle();
    expect(vi.mocked(sdk.SubmitSession)).toHaveBeenCalledTimes(1);
  });

  it('keeps an unknown attempt against its own session when the root switches and does not resend', async () => {
    const { promise: submitGate, reject: resolveSubmit } = Promise.withResolvers();
    vi.mocked(sdk.SubmitSession).mockImplementationOnce(() => submitGate);
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
    resolveSubmit(new Error('gone'));
    await settle();
    expect(vi.mocked(sdk.SubmitSession)).toHaveBeenCalledTimes(1);
    const attempt = { ...vi.mocked(sdk.SubmitSession).mock.calls[0][0] };
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
    return hydration({
      active_operation: { operation_id: 'op1', request_kind: 'message', status: 'running', admitted_at: '0', agent_type: 'primary', model: 'prov/m', usage: { totals: { by_model: [] } } },
    });
  }

  it('issues Interrupt and Stop as separate addressed controls and a 204 success writes no note', async () => {
    vi.mocked(sdk.InterruptSession).mockResolvedValue(native(204, '', false));
    vi.mocked(sdk.StopSession).mockResolvedValue(native(204, '', false));
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, activeHydration()));
    const { target } = await mountApp(connection);
    await settle();
    const readsBefore = vi.mocked(sdk.GetSessionHydration).mock.calls.length;
    (target.querySelector('.interrupt') as HTMLElement).click();
    await settle();
    expect(vi.mocked(sdk.InterruptSession)).toHaveBeenCalledWith('{"session_id":"root1"}');
    // A successful 204 is a valid success: no outcome/error note is written,
    // and the stamped view is refreshed by a further hydration read.
    expect(target.querySelector('.note')).toBeNull();
    await vi.waitFor(() => expect(vi.mocked(sdk.GetSessionHydration).mock.calls.length).toBeGreaterThan(readsBefore));
    const readsAfterInterrupt = vi.mocked(sdk.GetSessionHydration).mock.calls.length;
    (target.querySelector('.stop') as HTMLElement).click();
    await settle();
    expect(vi.mocked(sdk.StopSession)).toHaveBeenCalledWith('{"session_id":"root1"}');
    expect(target.querySelector('.note')).toBeNull();
    await vi.waitFor(() => expect(vi.mocked(sdk.GetSessionHydration).mock.calls.length).toBeGreaterThan(readsAfterInterrupt));
  });

  it('reports an addressed control rejection and unknown outcome even after a stamp move', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, activeHydration()));
    const { target } = await mountApp(connection);
    await settle();
    const { promise: interruptGate, resolve: resolveInterrupt } = Promise.withResolvers();
    vi.mocked(sdk.InterruptSession).mockImplementationOnce(() => interruptGate);
    (target.querySelector('.interrupt') as HTMLElement).click();
    // Move the selection to root2 while the root1 control is in flight.
    await selectRoot2(target);
    // The UI and the addressed reader are root2 before the held call lands.
    expect(target.querySelector('.toolbar')?.textContent).toContain('root2');
    await vi.waitFor(() => expect(vi.mocked(sdk.GetSessionHydration)).toHaveBeenLastCalledWith('{"session_id":"root2"}'));
    resolveInterrupt(json(500, { code: 'internal', message: 'interrupt blew up' }));
    await settle();
    await settle();
    // The root1 failure is still reported against its captured address...
    expect(target.querySelector('.note.outcome')?.textContent).toContain('Interrupt failed for session root1');
    expect(target.querySelector('.note.outcome')?.textContent).toContain('interrupt blew up');
    // ...with no view adoption into root2: the displayed session stays root2.
    expect(target.querySelector('.toolbar')?.textContent).toContain('root2');
    // An unknown transport outcome is likewise reported for its address.
    const { promise: stopGate, reject: resolveStop } = Promise.withResolvers();
    vi.mocked(sdk.StopSession).mockImplementationOnce(() => stopGate);
    (target.querySelector('.stop') as HTMLElement).click();
    resolveStop(new Error('stop lost'));
    await settle();
    await settle();
    expect(target.querySelector('.note.outcome')?.textContent).toContain('Stop outcome unknown for session root2');
    expect(target.querySelector('.toolbar')?.textContent).toContain('root2');
  });

  it('interrupts on composer Escape and closes modals first', async () => {
    vi.mocked(sdk.InterruptSession).mockResolvedValue(json(200, undefined));
    vi.mocked(sdk.ListModels).mockResolvedValue(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '1' }, models: [] }));
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, activeHydration()));
    const { target } = await mountApp(connection);
    await settle();
    // Escape with an open modal closes the modal and never interrupts.
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'm', ctrlKey: true }));
    await settle();
    expect(target.querySelector('dialog')).not.toBeNull();
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    await settle();
    expect(target.querySelector('dialog')).toBeNull();
    expect(vi.mocked(sdk.InterruptSession)).not.toHaveBeenCalled();
    // Escape focused in the composer interrupts.
    const textarea = target.querySelector('textarea') as HTMLTextAreaElement;
    textarea.focus();
    textarea.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    await settle();
    expect(vi.mocked(sdk.InterruptSession)).toHaveBeenCalledWith('{"session_id":"root1"}');
  });

  it('submits a manual compact with a minted caller ID', async () => {
    vi.mocked(sdk.CompactSession).mockResolvedValue(json(200, { operation_id: 'op1', request_kind: 'compact', status: 'running', admitted_at: '0', agent_type: 'primary', model: 'prov/m', usage: { totals: { by_model: [] } } }));
    const { target } = await mountBasic();
    // The toolbar has no duplicate Compact; the composer owns the control.
    expect(target.querySelector('.toolbar .compact-btn')).toBeNull();
    (target.querySelector('.composer .compact') as HTMLElement).click();
    await settle();
    const call = JSON.parse(vi.mocked(sdk.CompactSession).mock.calls[0][0]) as { session_id: string; body: { operation_id: string } };
    expect(call.session_id).toBe('root1');
    expect(call.body.operation_id).toMatch(/^[0-9a-f]{32}$/);
  });

  it('labels an active compact Operation without naming its transport model', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, hydration({
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
    vi.mocked(sdk.ListModels).mockResolvedValue(json(200, {
      configuration_revision: { instance_id: INSTANCE, generation: '1' },
      models: [
        { ref: 'prov/a', provider: 'prov', provider_name: 'Provider', model: 'a', display_name: 'Model A', context_window: 1000, max_output_tokens: 10, hidden: false, provider_hidden: false, incomplete: false, source: 'user' },
      ],
    }));
    vi.mocked(sdk.SetAgentTypeModel).mockResolvedValue(json(200, {
      configuration_revision: { instance_id: INSTANCE, generation: '2' },
      result: { name: 'primary', description: '', subagent: false, model: 'prov/a', readonly: false, write_dir: '', tools: [], capabilities: [] },
    }));
    const { target } = await mountBasic();
    // After the mutation the Runtime publishes the newer configuration clock in
    // the hydration too, so the fresh mandated read converges.
    let hydrationReads = 0;
    vi.mocked(sdk.GetSessionHydration).mockImplementation(async () => {
      hydrationReads += 1;
      return json(200, hydration(hydrationReads > 1
        ? { configuration_revision: { instance_id: INSTANCE, generation: '2' } }
        : {}));
    });
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'm', ctrlKey: true }));
    await settle();
    expect(vi.mocked(sdk.ListModels)).toHaveBeenCalledWith('{"query":{"all":false}}');
    expect(target.textContent).toContain('Model for agent type');
    expect(target.textContent).toContain('primary');
    expect(target.textContent).toContain('all sessions using this agent type');
    (target.querySelector('.mp-btn') as HTMLElement).click();
    await settle();
    (target.querySelector('.mp .item') as HTMLElement).click();
    await settle();
    expect(vi.mocked(sdk.SetAgentTypeModel)).toHaveBeenCalledWith('{"query":{"agent_type":"primary"},"body":{"model":"prov/a"}}');
    // Correction 5: the success is reported, and the current configuration view
    // is refreshed through its OWN generated read (not by force-adopting the
    // mutation's returned clock).
    expect(target.querySelector('.note.outcome')?.textContent).toContain('Model selection applied');
    await vi.waitFor(() => expect(vi.mocked(sdk.GetConfiguration).mock.calls.length).toBeGreaterThanOrEqual(2));
  });

  it('keeps an unavailable selected_model from the accepted hydration and reports a selection rejection', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, hydration({ selected_model: 'prov/missing' })));
    vi.mocked(sdk.ListModels).mockResolvedValue(json(200, {
      configuration_revision: { instance_id: INSTANCE, generation: '1' },
      models: [
        { ref: 'prov/available', provider: 'prov', provider_name: 'Provider', model: 'available', display_name: 'Available', context_window: 1000, max_output_tokens: 10, hidden: false, provider_hidden: false, incomplete: false, source: 'user' },
      ],
    }));
    vi.mocked(sdk.SetAgentTypeModel).mockResolvedValue(json(404, { code: 'not_found', message: 'model not available' }));
    const { target } = await mountApp(connection);
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'm', ctrlKey: true }));
    await settle();
    await settle();
    // The accepted hydration's selected_model is retained as-is; the offered
    // list does not automatically replace it.
    expect(target.textContent).toContain('Currently selected model:');
    expect(target.textContent).toContain('prov/missing');
    expect(vi.mocked(sdk.SetAgentTypeModel)).not.toHaveBeenCalled();
    (target.querySelector('.mp-btn') as HTMLElement).click();
    await settle();
    (target.querySelector('.mp .item') as HTMLElement).click();
    await settle();
    await settle();
    // The generated rejection is displayed and the accepted selection is not
    // changed by the client.
    expect(vi.mocked(sdk.SetAgentTypeModel)).toHaveBeenCalledWith('{"query":{"agent_type":"primary"},"body":{"model":"prov/available"}}');
    expect(target.querySelector('.note.outcome')?.textContent).toContain('Model selection failed');
    expect(target.querySelector('.note.outcome')?.textContent).toContain('model not available');
    expect(target.textContent).toContain('prov/missing');
  });

  it('a model success from an old selection reports success but pollutes no current floor and starts no repair', async () => {
    vi.mocked(sdk.ListModels).mockResolvedValue(json(200, {
      configuration_revision: { instance_id: INSTANCE, generation: '1' },
      models: [{ ref: 'prov/a', provider: 'prov', provider_name: 'Provider', model: 'a', display_name: 'Model A', context_window: 1000, max_output_tokens: 10, hidden: false, provider_hidden: false, incomplete: false, source: 'user' }],
    }));
    const { promise: modelGate, resolve: resolveModel } = Promise.withResolvers();
    vi.mocked(sdk.SetAgentTypeModel).mockImplementation(() => modelGate);
    const { target } = await mountBasic();
    const opensBefore = target ? 0 : 0;
    void opensBefore;
    const configReadsBefore = vi.mocked(sdk.GetConfiguration).mock.calls.length;
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'm', ctrlKey: true }));
    await settle();
    (target.querySelector('.mp-btn') as HTMLElement).click();
    await settle();
    (target.querySelector('.mp .item') as HTMLElement).click();
    await settle();
    // Move the selection to root2 while the model mutation is in flight.
    await selectRoot2(target);
    // The old-owner response finally lands carrying a foreign clock.
    resolveModel(json(200, {
      configuration_revision: { instance_id: 'ffffffffffffffffffffffffffffffff', generation: '9' },
      result: { name: 'primary', description: '', subagent: false, model: 'prov/a', readonly: false, write_dir: '', tools: [], capabilities: [] },
    }));
    await settle();
    await settle();
    // Success is still reported for the captured address...
    expect(target.querySelector('.note.outcome')?.textContent).toContain('Model selection applied');
    // ...but no repair/reconnect was triggered and the current configuration
    // refresh is a plain read, not a force-adopted foreign floor.
    expect(vi.mocked(sdk.GetConfiguration).mock.calls.length).toBeGreaterThanOrEqual(configReadsBefore + 1);
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
    expect(vi.mocked(sdk.GetSessionHydration)).toHaveBeenLastCalledWith('{"session_id":"root2"}');
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
      session_revision: revision('1'),
    };
  }

  it('unions both lifecycle lists with live membership, keeps jobs separate and inspects passively', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, hydration({
      background: [{ kind: 'child', id: 'child-open' }, { kind: 'child', id: 'child-live-only' }, { kind: 'job', id: 'job1' }],
    })));
    vi.mocked(sdk.ListSessions).mockImplementation(async (options) => {
      const lifecycle = (JSON.parse(options) as { query: { lifecycle: string } }).query.lifecycle;
      if (lifecycle === 'open') {
        return json(200, [
          session('root1', NEWER),
          childSession('child-open', 'open'),
          childSession('child-both', 'open'),
          { ...childSession('child-other-root', NEWER), parent_session_id: 'other-root' },
        ]);
      }
      return json(200, [childSession('child-arch', 'archived'), childSession('child-both', 'archived')]);
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
    expect(vi.mocked(sdk.GetSessionHydration)).toHaveBeenLastCalledWith('{"session_id":"child-live-only"}');
    const childPane = target.querySelector('.child-pane') as HTMLElement;
    expect(childPane).not.toBeNull();
    expect(childPane.querySelector('textarea')).toBeNull();
    expect(childPane.querySelector('.send')).toBeNull();
    expect(childPane.querySelector('.compact')).toBeNull();
  });

  it('restores the Markdown link and copy actions on the root and passive child renderers', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    const connection = fakeConnection();
    basicMocks(connection);
    const rootItems = [{
      item_id: 'root-a', kind: 'assistant' as const, committed_at: '0', status: 'completed' as const, source: 'p/m',
      content: [{ kind: 'text' as const, text: 'root raw **markdown**' }], tool_calls: [],
    }];
    const childItems = [{
      item_id: 'child-a', kind: 'assistant' as const, committed_at: '0', status: 'completed' as const, source: 'p/m',
      content: [{ kind: 'text' as const, text: 'child raw [docs](https://example.com/child?x=1)' }], tool_calls: [],
    }];
    vi.mocked(sdk.GetSessionHydration).mockImplementation(async (options) => {
      const id = (JSON.parse(options) as { session_id: string }).session_id;
      if (id === 'child1') {
        return json(200, hydration({
          session: childSession('child1', 'open'),
          conversation: { items: childItems as Hydration['conversation']['items'] },
        }));
      }
      return json(200, hydration({
        conversation: { items: rootItems as Hydration['conversation']['items'] },
        background: [{ kind: 'child', id: 'child1' }],
      }));
    });
    const { target } = await mountApp(connection);
    // Root pane: the exact raw Markdown is copied, never the rendered HTML.
    const rootRow = target.querySelector('.content > .message-list .message.assistant') as HTMLElement;
    expect(rootRow.querySelector('.md strong')?.textContent).toBe('markdown');
    (rootRow.querySelector('.copy-icon') as HTMLElement).click();
    await settle();
    expect(writeText).toHaveBeenCalledExactlyOnceWith('root raw **markdown**');
    // The passive child pane keeps the same link and copy behavior.
    (target.querySelector('[aria-label="Background sessions"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Inspect child session child1"]') as HTMLElement).click();
    await settle();
    await settle();
    const childPane = target.querySelector('.child-pane') as HTMLElement;
    expect(childPane.querySelector('.fork')).toBeNull();
    const childRow = childPane.querySelector('.message.assistant') as HTMLElement;
    const anchor = childRow.querySelector('.md a') as HTMLAnchorElement;
    const click = new MouseEvent('click', { bubbles: true, cancelable: true });
    anchor.dispatchEvent(click);
    expect(click.defaultPrevented).toBe(true);
    expect(vi.mocked(BrowserOpenURL)).toHaveBeenCalledExactlyOnceWith('https://example.com/child?x=1');
    writeText.mockClear();
    (childRow.querySelector('.copy-icon') as HTMLElement).click();
    await settle();
    expect(writeText).toHaveBeenCalledExactlyOnceWith('child raw [docs](https://example.com/child?x=1)');
  });

  it('renders the child own pending projection in the passive inspector, including repeats', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ListSessions).mockResolvedValue(json(200, [
      session('root1', NEWER),
      { ...session('kid', OLDER), parent_session_id: 'root1' },
    ]));
    vi.mocked(sdk.GetSessionHydration).mockImplementation(async (options) => {
      const id = (JSON.parse(options) as { session_id: string }).session_id;
      if (id === 'kid') {
        return json(200, hydration({
          session: session('kid', OLDER),
          pending: {
            steering: [
              { operation_id: 'dup', origin: 'user', content: [{ kind: 'text', text: 'kid steering first' }] },
              { operation_id: 'dup', origin: 'user', content: [{ kind: 'text', text: 'kid steering second' }] },
            ],
            queued: [{ operation_id: 'q1', origin: 'user', content: [{ kind: 'text', text: 'kid queued one' }] }],
          },
        }));
      }
      return json(200, hydration());
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

  it('child-only provisional progress does not recompute the root pane derivation', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ListSessions).mockResolvedValue(json(200, [
      session('root1', NEWER),
      { ...session('kid', OLDER), parent_session_id: 'root1' },
    ]));
    vi.mocked(sdk.GetSessionHydration).mockImplementation(async (options) => {
      const id = (JSON.parse(options) as { session_id: string }).session_id;
      return json(200, hydration({
        session: session(id, OLDER),
        active_operation: {
          operation_id: id === 'kid' ? 'kop' : 'rop',
          request_kind: 'message',
          status: 'running',
          admitted_at: '0',
          agent_type: 'primary',
          model: 'prov/m',
          usage: { totals: { by_model: [] } },
        },
      }));
    });
    const { target } = await mountApp(connection);
    await vi.waitFor(() => expect(target.textContent).toContain('root1'));
    await settle();
    await settle();
    (target.querySelector('.bg-btn') as HTMLElement).click();
    await vi.waitFor(() => expect([...target.querySelectorAll('dialog .row')].some((row) => row.textContent?.includes('kid'))).toBe(true));
    ([...target.querySelectorAll('dialog .row')].find((row) => row.textContent?.includes('kid')) as HTMLElement).click();
    await settle();
    expect(target.querySelector('.child-pane')).not.toBeNull();

    // Observe the pane derivations through their actual owner getters: each
    // viewOf consumes its own reader plus the shared attempt owner.
    const getItems = vi.spyOn(SessionReader.prototype, 'getItems');
    const outstanding = vi.spyOn(AttemptLedger.prototype, 'outstanding');
    deliverEvent(connection, { kind: 'text_delta', scope: { kind: 'operation', session_id: 'kid', operation_id: 'kop' }, position: 0, content: 'kid live' });
    await settle();
    expect(target.querySelector('.child-pane')?.querySelector('.provisional-text')?.textContent).toBe('kid live');
    expect(outstanding.mock.calls.map((call) => call[0])).not.toContain('root1');
    const rootReads = getItems.mock.instances.filter((reader) => (reader as SessionReader).getHydration()?.session.session_id === 'root1');
    expect(rootReads).toHaveLength(0);
    getItems.mockRestore();
    outstanding.mockRestore();
  });

  it('inspector changes never strand an in-flight root read', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ListSessions).mockImplementation(async (options) => {
      const query = (JSON.parse(options) as { query: { lifecycle: string } }).query;
      if (query.lifecycle === 'open') {
        return json(200, [session('root1', NEWER), { ...session('kid', NEWER), parent_session_id: 'root1' }]);
      }
      return json(200, []);
    });
    vi.mocked(sdk.GetSessionHydration).mockImplementation(async (options) => {
      const id = (JSON.parse(options) as { session_id: string }).session_id;
      return json(200, hydration({
        session: session(id, NEWER),
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
    vi.mocked(sdk.InterruptSession).mockResolvedValue(json(200, undefined));
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ListSessions).mockImplementation(async () => json(200, [session('root1', NEWER), childSession('child-open', 'open')]));
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
    expect(vi.mocked(sdk.InterruptSession)).toHaveBeenCalledWith('{"session_id":"root1"}');
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
  it.each([
    {
      title: 'shows own warnings and a global-only indicator, filtering other sessions',
      warnings: [
        { source: 'a', kind: 'k', message: 'own message', session_id: 'root1' },
        { source: 'a', kind: 'k', message: 'global message' },
        { source: 'a', kind: 'k', message: 'other session message', session_id: 'other' },
      ],
      verify: (target: HTMLElement) => {
        expect(target.querySelector('dialog')).not.toBeNull();
        expect(target.textContent).toContain('Global');
        expect(target.textContent).toContain('global message');
        expect(target.textContent).toContain('This session');
        expect(target.textContent).toContain('own message');
        expect(target.textContent).not.toContain('other session message');
      },
    },
    {
      title: 'renders colliding warning tuples as distinct rows (unique tuples, non-injective key encoding)',
      // Two DISTINCT tuples whose old concatenated key collides: ('p','a','bc')
      // and ('p','ab','c') both concatenate to 'pabc' (within one plugin), and
      // ('x','yz','w') and ('xy','z','w') both concatenate to 'xyzw' (across
      // plugin-source prefixes). Presentation is stateless and must not dedup
      // or mint identities: every distinct report renders once.
      warnings: [
        { source: 'p', kind: 'a', message: 'bc' },
        { source: 'p', kind: 'ab', message: 'c' },
        { source: 'x', kind: 'yz', message: 'w' },
        { source: 'xy', kind: 'z', message: 'w' },
      ],
      verify: (target: HTMLElement) => {
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
      },
    },
    {
      title: 'preserves own/sibling filtering alongside colliding warning keys',
      warnings: [
        { source: 'p', kind: 'a', message: 'bc', session_id: 'root1' },
        { source: 'p', kind: 'ab', message: 'c', session_id: 'root1' },
        { source: 'p', kind: 'a', message: 'bc', session_id: 'sibling' },
      ],
      verify: (target: HTMLElement) => {
        // The two colliding OWN tuples both render; the sibling is filtered out.
        const ownRows = [...target.querySelectorAll('.warning.own')];
        expect(ownRows).toHaveLength(2);
        expect(target.textContent).not.toContain('sibling');
      },
    },
  ])('$title', async ({ warnings, verify }) => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, hydration({ warnings })));
    const { target } = await mountApp(connection);
    await settle();
    (target.querySelector('.warn') as HTMLElement).click();
    await settle();
    verify(target);
  });

  it('replaces warning rows on authoritative replacement', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    const first = json(200, hydration({
      warnings: [{ source: 'p', kind: 'a', message: 'bc' }, { source: 'p', kind: 'ab', message: 'c' }],
    }));
    const second = json(200, hydration({
      session_revision: { instance_id: INSTANCE, durable_revision: '2', local_revision: '0' },
      warnings: [{ source: 'q', kind: 'r', message: 's' }],
    }));
    vi.mocked(sdk.GetSessionHydration).mockResolvedValueOnce(first).mockResolvedValue(second);
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
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, hydration({
      usage: {
        totals: {
          by_model: [
            { model: 'p/m', usage: { input_tokens: '9007199254740993', cached_input_tokens: '2', output_tokens: '3' } },
            { model: 'p/m2', usage: { input_tokens: '2', cached_input_tokens: '0', output_tokens: '9007199254740992' } },
          ],
        },
        context: { used_tokens: '123456789012345678901', context_window: JSON.rawJSON('200000') },
      },
    })));
    const { target } = await mountApp(connection);
    await settle();
    // input total 9007199254740993+2 = 9007199254740995; output total
    // 3+9007199254740992 = 9007199254740995; cache total 2.
    expect(target.querySelector('.tokens')?.textContent).toContain('↑ 9007199254740995');
    expect(target.querySelector('.tokens')?.textContent).toContain('↓ 9007199254740995');
    expect(target.querySelector('.context')?.textContent).toContain('123456789012345678901');
    // The typed context consumer interprets its exact window token.
    expect(target.querySelector('.context')?.textContent).toContain('/ 200000');
  });
});

describe('repair', () => {
  it('carries generation identity: a current-generation loss during a fresh read re-drives the same owner; a subsumed loss is ignored', async () => {
    const { connection } = await mountBasic();
    const opensBefore = connection.openMock.mock.calls.length;
    const readsAtBoot = vi.mocked(sdk.GetSessionHydration).mock.calls.length;
    connection.openMock.mockResolvedValue({ instance_id: INSTANCE, generation: '2', workspace: '/ws' });
    // Gate the repair's fresh hydration so the second loss lands strictly
    // before the read completes.
    const { promise: readGate, resolve: settleRead } = Promise.withResolvers();
    vi.mocked(sdk.GetSessionHydration).mockImplementationOnce(
      () => readGate,
    );
    // A genuinely current (installed generation '1') loss.
    connection.failureHandler?.(new Error('first loss'), '1');
    await vi.waitFor(() => expect(vi.mocked(sdk.GetSessionHydration).mock.calls.length).toBe(readsAtBoot + 1));
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
    settleRead(json(200, hydration()));
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

  it('a 200 null configuration document is invalid delivery and repairs without a consumer exception', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetConfiguration).mockResolvedValueOnce({ status: 200, body: 'null', invalid: false });
    connection.openMock.mockResolvedValue({ instance_id: INSTANCE, generation: '2', workspace: '/ws' });
    const opensBefore = connection.openMock.mock.calls.length;
    const { target } = await mountApp(connection);
    // The null global document is invalid delivery on the authoritative read:
    // the existing invalid handling repairs through the owner instead of
    // dereferencing the null snapshot.
    await vi.waitFor(() => expect(connection.openMock.mock.calls.length).toBeGreaterThan(opensBefore));
    await vi.waitFor(() => expect(target.textContent).toContain('configuration: received success without a result'));
  });

  it('a repair whose fresh read hits invalid delivery reports inside the owner without recursion', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    await mountApp(connection);
    await settle();
    const opensBefore = connection.openMock.mock.calls.length;
    const readsAtBoot = vi.mocked(sdk.GetSessionHydration).mock.calls.length;
    connection.openMock.mockResolvedValue({ instance_id: INSTANCE, generation: '2', workspace: '/ws' });
    const { promise: readGate, resolve: settleRead } = Promise.withResolvers();
    vi.mocked(sdk.GetSessionHydration).mockImplementationOnce(
      () => readGate,
    );
    connection.failureHandler?.(new Error('stream lost'), '1');
    await vi.waitFor(() => expect(vi.mocked(sdk.GetSessionHydration).mock.calls.length).toBe(readsAtBoot + 1));
    // The owner's fresh read decodes to an invalid body: it reports inside the
    // owner as that read's failure instead of a synchronous recursive Open.
    settleRead(native(200, 'hydration body undecodable', true));
    await settle();
    await settle();
    await settle();
    expect(document.body.textContent).toContain('invalid');
    // No unbounded recursion: at most the one coalesced re-drive happened.
    expect(connection.openMock.mock.calls.length).toBeLessThanOrEqual(opensBefore + 2);
  });
});

  it('adopts a Runtime-global read completed during a navigation on its connection alone', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetSessionHydration).mockImplementation(async (params: string) => {
      const id = (JSON.parse(params) as { session_id: string }).session_id;
      return json(200, hydration({
        session: session(id, NEWER),
        configuration_revision: { instance_id: INSTANCE, generation: '2' },
      }));
    });
    const { target } = await mountApp(connection);
    await vi.waitFor(() => expect(target.textContent).toContain('root1'));
    // A configuration update arrives and its own read is held while the user
    // navigates to another root.
    const { promise: held, resolve: releaseConfiguration } = Promise.withResolvers();
    let reads = 0;
    vi.mocked(sdk.GetConfiguration).mockImplementation(async () => {
      reads += 1;
      return held as never;
    });
    deliverEvent(connection, {
      kind: 'configuration_changed',
      scope: { kind: 'session', session_id: 'root1' },
      configuration_revision: { instance_id: INSTANCE, generation: '2' },
    });
    await settle();
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    await settle();
    (target.querySelector('[aria-label="Select session root2"]') as HTMLElement).click();
    await vi.waitFor(() => expect(target.textContent).toContain('root2'));
    releaseConfiguration(json(200, {
      configuration_revision: { instance_id: INSTANCE, generation: '2' },
      settings: { sessions: { auto_archive: false, archive_after_days: 1, delete_after_archive_days: 1 }, plugins: {} },
      agents: [{ name: 'reviewer', description: 'Review changes', subagent: false, model: '', readonly: false, write_dir: '', tools: [], capabilities: [] }],
      providers: [],
    }));
    await settle();
    await settle();
    // The navigation scheduled no second global read: the one held read is the
    // only one this update caused.
    expect(reads).toBe(1);
    // The completed global payload is adopted across the navigation.
    (target.querySelector('[aria-label="Settings"]') as HTMLElement).click();
    await settle();
    await settle();
    (target.querySelector('[aria-label="Agent type"]') as HTMLElement).click();
    await vi.waitFor(() => expect(target.textContent).toContain('reviewer'));
  });

// --- Step 8: navigation, configuration, viewer and code tasks ----------------

const RAW_CONFIGURATION = '{"configuration_revision":{"instance_id":"' + INSTANCE + '","generation":"1"},'
  + '"settings":{"sessions":{"auto_archive":false,"archive_after_days":-9223372036854775808,"delete_after_archive_days":1},'
  + '"plugins":{"tools":"{\\"max_output_bytes\\":9007199254740993}"}},'
  + '"agents":[],"providers":[]}';

const EXACT_SETTINGS_TEXT = '{"sessions":{"auto_archive":false,"archive_after_days":-9223372036854775808,"delete_after_archive_days":1},'
  + '"plugins":{"tools":"{\\"max_output_bytes\\":9007199254740993}"}}';

function inputItem(itemID: string, origin: 'user' | 'runtime' | 'plugin', operationID?: string) {
  return {
    item_id: itemID,
    kind: 'input' as const,
    committed_at: '0',
    ...(operationID ? { operation_id: operationID } : {}),
    origin,
    content: [{ kind: 'text' as const, text: 'text ' + itemID }],
  };
}

function runningOperation(id: string) {
  return {
    operation_id: id,
    request_kind: 'message' as const,
    status: 'running' as const,
    admitted_at: '0',
    agent_type: 'primary',
    model: 'p/m',
    usage: { totals: { by_model: [] } },
  };
}

function providerFixture(overrides: Record<string, unknown> = {}) {
  return {
    id: 'prov1',
    name: 'Prov One',
    builtin: false,
    connected: true,
    key_source: 'managed',
    base_url: 'http://127.0.0.1:1/v1',
    api_key_env: 'PROV1_KEY',
    headers: {},
    user_headers: {},
    system_role: 'system',
    usage_in_stream: false,
    discovery: true,
    hidden: false,
    models: [modelFixture()],
    ...overrides,
  };
}

function modelFixture(overrides: Record<string, unknown> = {}) {
  return {
    id: 'm1',
    name: 'Model One',
    context_window: 1000,
    max_output_tokens: 100,
    input_modalities: ['text'],
    system_role: 'system',
    usage_in_stream: false,
    hidden: false,
    source: 'user',
    incomplete: false,
    provider_hidden: false,
    usable: true,
    ...overrides,
  };
}

async function openSettings(target: HTMLElement) {
  (target.querySelector('[aria-label="Settings"]') as HTMLElement).click();
  await settle();
  await settle();
}

async function changeInput(target: HTMLElement, label: string, value: string) {
  const input = target.querySelector(`[aria-label="${label}"]`) as HTMLInputElement;
  input.value = value;
  input.dispatchEvent(new Event('input', { bubbles: true }));
  input.dispatchEvent(new Event('change', { bubbles: true }));
  await tick();
}

async function toggleCheckbox(target: HTMLElement, label: string, checked: boolean) {
  const input = target.querySelector(`[aria-label="${label}"]`) as HTMLInputElement;
  input.checked = checked;
  input.dispatchEvent(new Event('change', { bubbles: true }));
  await tick();
}

// The exact authored body member text of one composed invocation envelope,
// preserved byte-exact without any JSON round trip.
function bodyOf(call: string): string {
  const marker = '"body":';
  const at = call.indexOf(marker);
  return call.slice(at + marker.length, call.length - 1);
}

// The owned-input Fork action shared by the replacement and restore scenarios:
// open the dialog for i-owned and, when asked, enable the rewind toggle before
// submitting.
async function openOwnedFork(target: HTMLElement, rewind: boolean) {
  (target.querySelector('[aria-label="Fork from input i-owned"]') as HTMLElement).click();
  await settle();
  if (rewind) await toggleCheckbox(target, 'Rewind code before Fork', true);
  (target.querySelector('[aria-label="Submit fork"]') as HTMLElement).click();
}

// The mounted provider editor shared by the plain and discovery cases: the
// list and detail reads answer from the supplied providers, while every
// mutation, external-key and raw-body mock stays a per-case concern.
function editorConnection(providers: unknown[]) {
  const connection = fakeConnection();
  basicMocks(connection);
  vi.mocked(sdk.ListProviders).mockResolvedValue(json(200, {
    configuration_revision: { instance_id: INSTANCE, generation: '1' },
    providers: providers as never,
  }));
  vi.mocked(sdk.GetProviderDetail).mockImplementation(async (params: string) => {
    const id = (JSON.parse(params) as { query: { provider_id: string } }).query.provider_id;
    return json(200, {
      configuration_revision: { instance_id: INSTANCE, generation: '1' },
      provider: (providers as Array<{ id: string }>).find((provider) => provider.id === id) as never,
    });
  });
  return connection;
}

async function openProviderEditor(target: HTMLElement) {
  await openSettings(target);
  (target.querySelector('[aria-label="Providers"]') as HTMLElement).click();
  await settle();
}

// openNewProvider opens the authored create form on top of the provider list.
async function openNewProvider(target: HTMLElement) {
  await openProviderEditor(target);
  (target.querySelector('[aria-label="New provider"]') as HTMLElement).click();
  await settle();
}

// openSnapshots opens the code-snapshots dialog. Each case keeps its own
// settle counts and any held read/release gesture.
async function openSnapshots(target: HTMLElement) {
  await openSettings(target);
  (target.querySelector('[aria-label="Snapshots"]') as HTMLElement).click();
}

// The richer Fork fixture: four inputs and three owned operations, plus the
// plain Fork success default the restore cases rely on. Per-case overrides and
// scripts stay with their cases.
function forkConnection(overrides: Partial<Hydration> = {}) {
  const connection = fakeConnection();
  basicMocks(connection);
  vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, hydration({
    conversation: {
      items: [
        inputItem('i-owned', 'user', 'op-owned'),
        inputItem('i-copied', 'user'),
        inputItem('i-runtime', 'runtime', 'op-runtime'),
        inputItem('i-plugin', 'plugin', 'op-plugin'),
      ],
    },
    operations: [runningOperation('op-owned'), runningOperation('op-runtime'), runningOperation('op-plugin')],
    ...overrides,
  })));
  vi.mocked(sdk.ForkSession).mockResolvedValue(json(200, {
    session: session('dest1', NEWER),
    operation: runningOperation('x'),
  }));
  return connection;
}

describe('repair projection ownership', () => {
  function ownedHydration() {
    return hydration({
      conversation: { items: [inputItem('i-owned', 'user', 'op-owned')] },
      operations: [runningOperation('op-owned')],
      warnings: [{ source: 's', kind: 'k', message: 'own warning', session_id: 'root1' }],
    });
  }

  async function repair(connection: ReturnType<typeof fakeConnection>) {
    connection.openMock.mockResolvedValue({ instance_id: INSTANCE, generation: '2', workspace: '/ws' });
    connection.failureHandler?.(new Error('lost'), '1');
    await settle();
    await settle();
    await settle();
  }

  it('closes every runtime-data dialog when a new-instance repair starts', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ListWorkspaces).mockResolvedValue(json(200, [{ root: '/ws', display_name: 'ws' }]));
    vi.mocked(sdk.ListModels).mockResolvedValue(json(200, {
      configuration_revision: { instance_id: INSTANCE, generation: '1' },
      models: [{ ref: 'prov/a', provider: 'prov', provider_name: 'Provider', model: 'a', display_name: 'Model A', context_window: 1000, max_output_tokens: 10, hidden: false, provider_hidden: false, incomplete: false, source: 'user' }],
    }));
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, ownedHydration()));
    const { target } = await mountApp(connection);
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'm', ctrlKey: true }));
    await settle();
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    await settle();
    (target.querySelector('[aria-label="Select workspace"]') as HTMLElement).click();
    await settle();
    await openSettings(target);
    (target.querySelector('[aria-label="Agent type"]') as HTMLElement).click();
    await settle();
    await openSettings(target);
    (target.querySelector('[aria-label="Providers"]') as HTMLElement).click();
    await settle();
    await openSettings(target);
    (target.querySelector('[aria-label="Snapshots"]') as HTMLElement).click();
    await settle();
    await openSettings(target);
    (target.querySelector('[aria-label="File viewer"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Show token usage"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Background sessions"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Show warnings"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Fork from input i-owned"]') as HTMLElement).click();
    await settle();
    expect(target.querySelectorAll('dialog').length).toBeGreaterThan(0);
    await repair(connection);
    // Every runtime-data dialog closed with the discarded generation.
    expect(target.querySelectorAll('dialog')).toHaveLength(0);
  });

  it('discards cached picker rows before the new instance answers', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    const { target } = await mountApp(connection);
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    await settle();
    expect(target.textContent).toContain('root1');
    await repair(connection);
    // The reopened picker reads fresh; the old cached rows are gone while the
    // read is still parked.
    vi.mocked(sdk.ListSessions).mockImplementation(() => new Promise(() => {}) as never);
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    expect(target.textContent).not.toContain('Select session root1');
    expect(target.textContent).toContain('loading…');
  });

  it('a failed fresh configuration/warning read leaves no old-owner roster or warning data', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetConfiguration).mockResolvedValue(json(200, {
      configuration_revision: { instance_id: INSTANCE, generation: '1' },
      settings: { sessions: { auto_archive: false, archive_after_days: 1, delete_after_archive_days: 1 }, plugins: {} },
      agents: [{ name: 'other-agent', description: 'Old owner roster', subagent: false, model: '', readonly: false, write_dir: '', tools: [], capabilities: [] }],
      providers: [],
    }));
    vi.mocked(sdk.GetWarnings).mockResolvedValue(json(200, {
      warnings_revision: { instance_id: INSTANCE, revision: '1' },
      warnings: [{ source: 'g', kind: 'k', message: 'old owner global' }],
    }));
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, hydration({
      warnings: [{ source: 'g', kind: 'k', message: 'old owner global' }],
    })));
    const { target } = await mountApp(connection);
    await settle();
    expect(target.querySelector('[aria-label="Show warnings"]')).not.toBeNull();
    // The repair's own fresh global reads fail; no stale payload may survive.
    vi.mocked(sdk.GetConfiguration).mockRejectedValue(new Error('configuration down'));
    vi.mocked(sdk.GetWarnings).mockRejectedValue(new Error('warnings down'));
    vi.mocked(sdk.GetSessionHydration).mockRejectedValue(new Error('hydration down'));
    await repair(connection);
    expect(target.querySelector('[aria-label="Show warnings"]')).toBeNull();
    await openSettings(target);
    (target.querySelector('[aria-label="Agent type"]') as HTMLElement).click();
    await settle();
    expect(target.textContent).toContain('(no agent definitions)');
    expect(target.textContent).not.toContain('other-agent');
  });

  it('preserves the composer draft and pending attempts across a repair without resending', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, ownedHydration()));
    const { promise: submitGate, reject: releaseSubmit } = Promise.withResolvers();
    const { promise: forkGate, reject: releaseFork } = Promise.withResolvers();
    vi.mocked(sdk.SubmitSession).mockImplementationOnce(() => submitGate as never);
    vi.mocked(sdk.ForkSession).mockImplementationOnce(() => forkGate as never);
    const { target } = await mountApp(connection);
    await settle();
    (target.querySelector('[aria-label="Message input"]') as HTMLTextAreaElement).value = 'keep this draft';
    (target.querySelector('[aria-label="Message input"]') as HTMLTextAreaElement).dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    (target.querySelector('[aria-label="Send"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Fork from input i-owned"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Submit fork"]') as HTMLElement).click();
    await settle();
    await repair(connection);
    // The open Fork form is discarded with the generation; the submitted
    // attempts and the composer draft survive.
    expect(target.querySelector('[aria-label="Close fork dialog"]')).toBeNull();
    expect((target.querySelector('[aria-label="Message input"]') as HTMLTextAreaElement).value).toBe('keep this draft');
    releaseSubmit(new Error('submit lost'));
    releaseFork(new Error('fork lost'));
    await settle();
    await settle();
    expect(vi.mocked(sdk.SubmitSession)).toHaveBeenCalledTimes(1);
    expect(vi.mocked(sdk.ForkSession)).toHaveBeenCalledTimes(1);
    // Both outstanding attempts remain reachable with Retry/Abandon.
    const rows = [...target.querySelectorAll('.attempt')];
    expect(rows.length).toBeGreaterThanOrEqual(2);
    expect(target.querySelectorAll('.attempt [aria-label="Abandon"]').length).toBeGreaterThanOrEqual(2);
  });

  it('reports an addressed pending mutation after unmount and publishes no stale read body', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    const prov1 = providerFixture({ connected: false, key_source: 'external' });
    const prov2 = providerFixture({ id: 'prov2', name: 'Prov Two' });
    vi.mocked(sdk.ListProviders).mockResolvedValue(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '1' }, providers: [prov1, prov2] }));
    const { promise: readGate, resolve: releaseRead } = Promise.withResolvers();
    vi.mocked(sdk.GetProviderDetail).mockImplementation(async (params: string) => {
      const id = (JSON.parse(params) as { query: { provider_id: string } }).query.provider_id;
      if (id === 'prov2') return readGate as never;
      return json(200, { configuration_revision: { instance_id: INSTANCE, generation: '1' }, provider: prov1 });
    });
    const { promise: mutationGate, resolve: releaseMutation } = Promise.withResolvers();
    vi.mocked(sdk.ConnectProvider).mockImplementationOnce(() => mutationGate as never);
    const { target } = await mountApp(connection);
    await openSettings(target);
    (target.querySelector('[aria-label="Providers"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Select provider prov1"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Connect provider"]') as HTMLElement).click();
    await settle();
    const key = target.querySelector('[aria-label="Connect api key"]') as HTMLInputElement;
    key.value = 'connect-secret';
    key.dispatchEvent(new Event('input', { bubbles: true }));
    (target.querySelector('[aria-label="Submit connect"]') as HTMLElement).click();
    await settle();
    // A second detail read parks; the repair unmounts the editor with both the
    // mutation and the read outstanding.
    (target.querySelector('[aria-label="Select provider prov2"]') as HTMLElement).click();
    await settle();
    await repair(connection);
    expect(target.querySelector('[aria-label="Close provider editor"]')).toBeNull();
    // The stale read parked across the unmount publishes nothing.
    releaseRead(native(200, JSON.stringify({ configuration_revision: { instance_id: INSTANCE, generation: '1' }, provider: prov2 }), false));
    await settle();
    expect(target.querySelector('.note.outcome')).toBeNull();
    // The addressed pending mutation still reports through the App note.
    releaseMutation(json(401, { code: 'unauthorized', message: 'key refused' }));
    await settle();
    await settle();
    expect(target.querySelector('.note.outcome')?.textContent).toContain('Provider connect failed for prov1');
    expect(target.querySelector('.note.outcome')?.textContent).toContain('key refused');
  });
});

describe('workspace navigation', () => {
  it('lists generated workspaces and re-composes the initial selection on an explicit change', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ListWorkspaces).mockResolvedValue(json(200, [
      { root: '/ws', display_name: 'ws' },
      { root: '/ws2', display_name: 'ws2' },
    ]));
    const { target } = await mountApp(connection);
    (target.querySelector('[aria-label="Select workspace"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Select workspace /ws2"]') as HTMLElement).click();
    await settle();
    await settle();
    const listCall = JSON.parse(vi.mocked(sdk.ListSessions).mock.calls.at(-1)?.[0]) as { query: { workspace: string } };
    expect(listCall.query.workspace).toBe('/ws2');
    expect(vi.mocked(SetTitle)).toHaveBeenCalledWith('/ws2');
    // The same shared initial-selection composition selected the newest root.
    expect(vi.mocked(sdk.GetSessionHydration).mock.calls.at(-1)?.[0]).toEqual('{"session_id":"root1"}');
  });

  it('picks a directory natively and ignores a cancelled dialog', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ListWorkspaces).mockResolvedValue(json(200, [{ root: '/ws', display_name: 'ws' }]));
    vi.mocked(PickDirectory).mockResolvedValueOnce('').mockResolvedValueOnce('/ws3');
    const { target } = await mountApp(connection);
    const listsBefore = vi.mocked(sdk.ListSessions).mock.calls.length;
    (target.querySelector('[aria-label="Select workspace"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Choose workspace directory"]') as HTMLElement).click();
    await settle();
    expect(vi.mocked(sdk.ListSessions).mock.calls.length).toBe(listsBefore);
    (target.querySelector('[aria-label="Choose workspace directory"]') as HTMLElement).click();
    await settle();
    await settle();
    const listCall = JSON.parse(vi.mocked(sdk.ListSessions).mock.calls.at(-1)?.[0]) as { query: { workspace: string } };
    expect(listCall.query.workspace).toBe('/ws3');
  });

  it('ignores a superseded workspace list/create completion and keeps the newest choice', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ListWorkspaces).mockResolvedValue(json(200, [
      { root: '/ws2', display_name: 'ws2' },
      { root: '/ws3', display_name: 'ws3' },
    ]));
    const { promise: staleGate, resolve: releaseStale } = Promise.withResolvers();
    vi.mocked(sdk.ListSessions).mockImplementation(async (options) => {
      const workspace = (JSON.parse(options) as { query: { workspace: string } }).query.workspace;
      if (workspace === '/ws2') return staleGate as never;
      return json(200, [session('root3', NEWER)]);
    });
    const { target } = await mountApp(connection);
    (target.querySelector('[aria-label="Select workspace"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Select workspace /ws2"]') as HTMLElement).click();
    await settle();
    // Choose /ws3 while the /ws2 list is still held.
    (target.querySelector('[aria-label="Select workspace"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Select workspace /ws3"]') as HTMLElement).click();
    await settle();
    releaseStale(json(200, [session('stale-root', NEWER)]));
    await settle();
    await settle();
    expect(target.textContent).toContain('root3');
    expect(vi.mocked(sdk.GetSessionHydration).mock.calls.some(
      (call) => (JSON.parse(call[0] as string) as { session_id: string }).session_id === 'stale-root',
    )).toBe(false);
    expect(vi.mocked(sdk.CreateSession)).not.toHaveBeenCalled();
  });

  it('rejects a directory chosen after another Workspace selection, before composition or title', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ListWorkspaces).mockResolvedValue(json(200, [
      { root: '/ws', display_name: 'ws' },
      { root: '/ws2', display_name: 'ws2' },
    ]));
    const { promise: pickGate, resolve: releasePick } = Promise.withResolvers<string>();
    vi.mocked(PickDirectory).mockImplementationOnce(() => pickGate as never);
    const { target } = await mountApp(connection);
    (target.querySelector('[aria-label="Select workspace"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Choose workspace directory"]') as HTMLElement).click();
    await settle();
    // Another Workspace selection supersedes the parked native dialog.
    (target.querySelector('[aria-label="Select workspace /ws2"]') as HTMLElement).click();
    await settle();
    await settle();
    releasePick('/ws3');
    await settle();
    await settle();
    expect(vi.mocked(SetTitle)).not.toHaveBeenCalledWith('/ws3');
    expect(vi.mocked(sdk.ListSessions).mock.calls.some(
      (call) => (JSON.parse(call[0] as string) as { query: { workspace: string } }).query.workspace === '/ws3',
    )).toBe(false);
    expect(target.textContent).toContain('/ws2');
  });

  it('rejects a superseded directory failure before any current note change', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ListWorkspaces).mockResolvedValue(json(200, [{ root: '/ws', display_name: 'ws' }]));
    const { promise: pickGate, reject: rejectPick } = Promise.withResolvers<string>();
    vi.mocked(PickDirectory).mockImplementationOnce(() => pickGate as never);
    const { target } = await mountApp(connection);
    (target.querySelector('[aria-label="Select workspace"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Choose workspace directory"]') as HTMLElement).click();
    await settle();
    // A new-instance repair supersedes the parked dialog.
    connection.openMock.mockResolvedValue({ instance_id: INSTANCE, generation: '2', workspace: '/ws' });
    connection.failureHandler?.(new Error('lost'), '1');
    await settle();
    await settle();
    await settle();
    rejectPick(new Error('dialog failed'));
    await settle();
    await settle();
    expect(target.querySelector('.note.outcome')).toBeNull();
    expect(target.textContent).toContain('/ws');
  });

  it('reports a current directory pick failure and keeps the current Workspace', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ListWorkspaces).mockResolvedValue(json(200, [{ root: '/ws', display_name: 'ws' }]));
    vi.mocked(PickDirectory).mockRejectedValueOnce(new Error('dialog failed'));
    const { target } = await mountApp(connection);
    (target.querySelector('[aria-label="Select workspace"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Choose workspace directory"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(target.querySelector('.note.outcome')?.textContent).toContain('Directory pick failed');
    expect(target.textContent).toContain('/ws');
  });
});

describe('session picker tasks', () => {
  async function openPicker(target: HTMLElement) {
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    await settle();
  }

  it('archives then deletes an open root and clears only its local selection', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ArchiveSession).mockResolvedValue(json(200, session('root1', NEWER)));
    vi.mocked(sdk.DeleteSession).mockResolvedValue(native(204, '', false));
    const { target } = await mountApp(connection);
    await openPicker(target);
    (target.querySelector('[aria-label="Delete session root1"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(vi.mocked(sdk.ArchiveSession)).toHaveBeenCalledWith('{"session_id":"root1"}');
    expect(vi.mocked(sdk.DeleteSession)).toHaveBeenCalledWith('{"session_id":"root1"}');
    // The selected root's removal clears only its selection; nothing else is
    // selected and no replacement Session is created.
    expect(target.textContent).toContain('no session');
    expect(vi.mocked(sdk.CreateSession)).not.toHaveBeenCalled();
    // The list was refreshed after the deletion.
    expect(vi.mocked(sdk.ListSessions).mock.calls.length).toBeGreaterThan(3);
    expect(target.textContent).not.toContain('Delete outcome unknown');
  });

  it.each([
    ['reports a lost Delete as outcome unknown and never as success', () => Promise.reject(new Error('delete lost'))],
    ['reports an invalid-delivery Delete as outcome unknown and never as success', () => Promise.resolve(native(200, 'delete body undecodable', true))],
  ])('%s', async (_title, deleteOutcome) => {
    vi.mocked(sdk.ArchiveSession).mockResolvedValue(json(200, session('root2', OLDER)));
    vi.mocked(sdk.DeleteSession).mockImplementation(deleteOutcome);
    const { target } = await mountBasic();
    await openPicker(target);
    (target.querySelector('[aria-label="Delete session root2"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(vi.mocked(sdk.ArchiveSession)).toHaveBeenCalledWith('{"session_id":"root2"}');
    expect(target.textContent).toContain('Delete outcome unknown for session root2; nothing was retransmitted.');
    expect(vi.mocked(sdk.DeleteSession)).toHaveBeenCalledTimes(1);
  });

  it('stops the open-root delete sequence after an unknown archive', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ArchiveSession).mockRejectedValue(new Error('archive lost'));
    const { target } = await mountApp(connection);
    await openPicker(target);
    (target.querySelector('[aria-label="Delete session root2"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(vi.mocked(sdk.ArchiveSession)).toHaveBeenCalledTimes(1);
    expect(vi.mocked(sdk.DeleteSession)).not.toHaveBeenCalled();
    // The unknown Archive is reported through the one shared addressed-outcome
    // rule; the sequence stops, so no Delete follows.
    expect(target.textContent).toContain('Archive outcome unknown for session root2; nothing was retransmitted.');
  });

  it('resumes an archived root through Reopen before hydrating it', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    const archived = { ...session('root9', OLDER), lifecycle: 'archived' as const };
    vi.mocked(sdk.ListSessions).mockImplementation(async (options) => {
      const lifecycle = (JSON.parse(options) as { query: { lifecycle: string } }).query.lifecycle;
      return lifecycle === 'open' ? json(200, [session('root1', NEWER)]) : json(200, [archived]);
    });
    vi.mocked(sdk.ReopenSession).mockResolvedValue(json(200, { ...archived, lifecycle: 'open' as const }));
    const { target } = await mountApp(connection);
    await openPicker(target);
    (target.querySelector('[aria-label="Archived sessions"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Resume session root9"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(vi.mocked(sdk.ReopenSession)).toHaveBeenCalledWith('{"session_id":"root9"}');
    expect(vi.mocked(sdk.GetSessionHydration).mock.calls.at(-1)?.[0]).toEqual('{"session_id":"root9"}');
  });

  it('invalidates an older list after a deletion, including a nonselected one', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    const { promise: firstDeleteList, resolve: releaseFirstDeleteList } = Promise.withResolvers();
    let openCalls = 0;
    vi.mocked(sdk.ListSessions).mockImplementation(async (options) => {
      const lifecycle = (JSON.parse(options) as { query: { lifecycle: string } }).query.lifecycle;
      if (lifecycle !== 'open') return json(200, []);
      openCalls += 1;
      if (openCalls <= 2) return json(200, [session('root1', NEWER), session('root2', OLDER)]);
      if (openCalls === 3) return firstDeleteList as never;
      return json(200, [session('root1', NEWER)]);
    });
    vi.mocked(sdk.DeleteSession).mockResolvedValue(native(204, '', false));
    vi.mocked(sdk.ArchiveSession).mockResolvedValue(json(200, session('root2', OLDER)));
    const { target } = await mountApp(connection);
    await openPicker(target);
    // Delete the NONselected root2; its refresh is held.
    (target.querySelector('[aria-label="Delete session root2"]') as HTMLElement).click();
    await settle();
    // A newer list (from deleting the selected root1) resolves first.
    (target.querySelector('[aria-label="Delete session root1"]') as HTMLElement).click();
    await settle();
    await settle();
    // The held older reply still carries root2; it must be ignored.
    releaseFirstDeleteList(json(200, [session('root1', NEWER), session('root2', OLDER)]));
    await settle();
    await settle();
    const rows = [...target.querySelectorAll('dialog .row')];
    expect(rows).toHaveLength(1);
    expect(rows[0].textContent).toContain('root1');
    expect(target.textContent).toContain('no session');
    expect(target.textContent).not.toContain('Delete outcome unknown');
  });

  it('keeps the archive-success selection and list state while Delete is parked', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    let archived = false;
    vi.mocked(sdk.ArchiveSession).mockImplementation(async () => {
      archived = true;
      return json(200, session('root1', NEWER));
    });
    vi.mocked(sdk.ListSessions).mockImplementation(async (options) => {
      const lifecycle = (JSON.parse(options) as { query: { lifecycle: string } }).query.lifecycle;
      if (lifecycle === 'open') {
        return json(200, archived ? [session('root2', OLDER)] : [session('root1', NEWER), session('root2', OLDER)]);
      }
      return json(200, []);
    });
    const { promise: deleteGate, resolve: releaseDelete } = Promise.withResolvers();
    vi.mocked(sdk.DeleteSession).mockImplementationOnce(() => deleteGate as never);
    const { target } = await mountApp(connection);
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    await settle();
    (target.querySelector('[aria-label="Delete session root1"]') as HTMLElement).click();
    await settle();
    await settle();
    // The archive stage already cleared only the matching selected root and
    // refreshed the list while Delete is still parked.
    expect(target.textContent).toContain('no session');
    const rows = [...target.querySelectorAll('dialog .row')].map((row) => row.textContent ?? '');
    expect(rows.some((text) => text.includes('root1'))).toBe(false);
    expect(rows.some((text) => text.includes('root2'))).toBe(true);
    releaseDelete(native(204, '', false));
    await settle();
    await settle();
    expect(target.textContent).toContain('no session');
    expect(vi.mocked(sdk.DeleteSession)).toHaveBeenCalledTimes(1);
  });

  it('dispatches Delete without waiting for the archive-stage list refresh', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    let openListCalls = 0;
    const { promise: stageGate, resolve: releaseStage } = Promise.withResolvers();
    vi.mocked(sdk.ListSessions).mockImplementation(async (options) => {
      const lifecycle = (JSON.parse(options) as { query: { lifecycle: string } }).query.lifecycle;
      if (lifecycle === 'archived') {
        if (openListCalls === 3) return stageGate as never;
        return json(200, []);
      }
      openListCalls += 1;
      if (openListCalls === 3) return stageGate as never;
      return json(200, openListCalls >= 4 ? [session('root2', OLDER)] : [session('root1', NEWER), session('root2', OLDER)]);
    });
    vi.mocked(sdk.ArchiveSession).mockResolvedValue(json(200, session('root1', NEWER)));
    vi.mocked(sdk.DeleteSession).mockResolvedValue(native(204, '', false));
    const { target } = await mountApp(connection);
    await openPicker(target);
    (target.querySelector('[aria-label="Delete session root1"]') as HTMLElement).click();
    await settle();
    await settle();
    // Delete was dispatched even though the archive-stage lifecycle reads are
    // still parked, and the selected-root clear was immediate.
    expect(vi.mocked(sdk.DeleteSession)).toHaveBeenCalledWith('{"session_id":"root1"}');
    expect(target.textContent).toContain('no session');
    releaseStage(json(200, [session('root1', NEWER), session('root2', OLDER)]));
    await settle();
    await settle();
    // The superseded archive-stage read cannot resurrect the removed row; the
    // final awaited refresh lists the post-delete state.
    const rows = [...target.querySelectorAll('dialog .row')].map((row) => row.textContent ?? '');
    expect(rows.some((text) => text.includes('root1'))).toBe(false);
  });

  it('nested lifecycle keydown stays with the nested control while row keys still activate', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ListSessions).mockImplementation(async (options) => {
      const lifecycle = (JSON.parse(options) as { query: { lifecycle: string } }).query.lifecycle;
      return lifecycle === 'open'
        ? json(200, [session('root1', NEWER), session('root2', OLDER)])
        : json(200, [{ ...session('arch8', OLDER), lifecycle: 'archived' as const }]);
    });
    vi.mocked(sdk.ArchiveSession).mockImplementation(async (options) => {
      const id = (JSON.parse(options) as { session_id: string }).session_id;
      return json(200, session(id, OLDER));
    });
    vi.mocked(sdk.DeleteSession).mockResolvedValue(native(204, '', false));
    vi.mocked(sdk.ReopenSession).mockResolvedValue(json(200, session('arch8', OLDER)));
    const { target } = await mountApp(connection);
    await openPicker(target);
    const hydratedIDs = () => vi.mocked(sdk.GetSessionHydration).mock.calls.map(
      (call) => (JSON.parse(call[0] as string) as { session_id: string }).session_id,
    );
    // A nested Archive button's keydown bubbles to its row: the row must not
    // select the row subject or close the picker.
    const archive = target.querySelector('[aria-label="Archive session root2"]') as HTMLElement;
    const archiveKey = new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true });
    archive.dispatchEvent(archiveKey);
    archive.dispatchEvent(new KeyboardEvent('keyup', { key: 'Enter', bubbles: true, cancelable: true }));
    await settle();
    expect(target.querySelector('#session-picker-title')).not.toBeNull();
    expect(hydratedIDs()).not.toContain('root2');
    expect(archiveKey.defaultPrevented).toBe(false);
    archive.click();
    await settle();
    await settle();
    expect(vi.mocked(sdk.ArchiveSession)).toHaveBeenCalledWith('{"session_id":"root2"}');
    // A nested Delete button on an archived row must not resume that row.
    (target.querySelector('[aria-label="Archived sessions"]') as HTMLElement).click();
    await settle();
    const archivedDelete = target.querySelector('[aria-label="Delete session arch8"]') as HTMLElement;
    const deleteKey = new KeyboardEvent('keydown', { key: ' ', bubbles: true, cancelable: true });
    archivedDelete.dispatchEvent(deleteKey);
    await settle();
    expect(vi.mocked(sdk.ReopenSession)).not.toHaveBeenCalled();
    expect(target.querySelector('#session-picker-title')).not.toBeNull();
    expect(deleteKey.defaultPrevented).toBe(false);
    archivedDelete.click();
    await settle();
    await settle();
    expect(vi.mocked(sdk.ArchiveSession)).toHaveBeenCalledWith('{"session_id":"arch8"}');
    expect(vi.mocked(sdk.DeleteSession)).toHaveBeenCalledWith('{"session_id":"arch8"}');
    // Own-row Space/Enter still activate deliberately and prevent the default.
    const archivedRow = target.querySelector('[aria-label="Resume session arch8"]') as HTMLElement;
    const space = new KeyboardEvent('keydown', { key: ' ', bubbles: true, cancelable: true });
    archivedRow.dispatchEvent(space);
    await settle();
    expect(space.defaultPrevented).toBe(true);
    await vi.waitFor(() => expect(vi.mocked(sdk.ReopenSession)).toHaveBeenCalledWith('{"session_id":"arch8"}'));
    // The successful Resume closed the picker; reopen it for the open-row sibling.
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    await settle();
    const openRow = target.querySelector('[aria-label="Select session root2"]') as HTMLElement;
    const enter = new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true });
    openRow.dispatchEvent(enter);
    await settle();
    expect(enter.defaultPrevented).toBe(true);
    await vi.waitFor(() => expect(hydratedIDs()).toContain('root2'));
  });

  it('creates an explicit new session from the picker', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.CreateSession).mockResolvedValue(json(200, session('root-new', NEWER)));
    const { target } = await mountApp(connection);
    await openPicker(target);
    (target.querySelector('[aria-label="New session"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(vi.mocked(sdk.CreateSession)).toHaveBeenCalledWith('{"body":{"workspace":"/ws","agent_type":"primary"}}');
    expect(vi.mocked(sdk.GetSessionHydration).mock.calls.at(-1)?.[0]).toEqual('{"session_id":"root-new"}');
  });
});

describe('resume selection intent', () => {  function archivedSession(id: string) {
    return { ...session(id, OLDER), lifecycle: 'archived' as const };
  }

  function archivedConnection() {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ListSessions).mockImplementation(async (options) => {
      const lifecycle = (JSON.parse(options) as { query: { lifecycle: string } }).query.lifecycle;
      return lifecycle === 'open'
        ? json(200, [session('root1', NEWER), session('root2', OLDER)])
        : json(200, [archivedSession('arch8'), archivedSession('arch9')]);
    });
    vi.mocked(sdk.ReopenSession).mockResolvedValue(json(200, session('arch9', OLDER)));
    return connection;
  }

  async function openArchived(target: HTMLElement) {
    if (!target.querySelector('#session-picker-title')) {
      window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
      await settle();
      await settle();
    }
    (target.querySelector('[aria-label="Archived sessions"]') as HTMLElement).click();
    await settle();
  }

  // A fresh picker instance on the archived tab: closes a still-open picker so
  // its tab state cannot leak across a scenario.
  async function reopenArchived(target: HTMLElement) {
    if (target.querySelector('#session-picker-title')) {
      (target.querySelector('[aria-label="Close session picker"]') as HTMLElement).click();
      await settle();
    }
    await openArchived(target);
  }

  function hydratedIDs() {
    return vi.mocked(sdk.GetSessionHydration).mock.calls.map((call) => (JSON.parse(call[0] as string) as { session_id: string }).session_id);
  }

  it('adopts only the newest resume when the later Reopen settles first', async () => {
    const connection = archivedConnection();
    const { promise: first, resolve: releaseFirst } = Promise.withResolvers();
    vi.mocked(sdk.ReopenSession)
      .mockImplementationOnce(() => first as never)
      .mockResolvedValueOnce(json(200, session('arch9', OLDER)));
    const { target } = await mountApp(connection);
    await openArchived(target);
    (target.querySelector('[aria-label="Resume session arch8"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Resume session arch9"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(hydratedIDs()).toContain('arch9');
    releaseFirst(json(200, session('arch8', OLDER)));
    await settle();
    await settle();
    // The stale success reports no adoption and hydrates no unrelated root.
    expect(hydratedIDs()).not.toContain('arch8');
    expect(target.textContent).toContain('arch9');
  });

  it('keeps the newest resume intent when the earlier Reopen settles first', async () => {
    const connection = archivedConnection();
    const { promise: first, resolve: releaseFirst } = Promise.withResolvers();
    const { promise: second, resolve: releaseSecond } = Promise.withResolvers();
    vi.mocked(sdk.ReopenSession)
      .mockImplementationOnce(() => first as never)
      .mockImplementationOnce(() => second as never);
    const { target } = await mountApp(connection);
    await openArchived(target);
    (target.querySelector('[aria-label="Resume session arch8"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Resume session arch9"]') as HTMLElement).click();
    await settle();
    // A settles first: its success is stale and never adopted or hydrated.
    releaseFirst(json(200, session('arch8', OLDER)));
    await settle();
    await settle();
    expect(hydratedIDs()).not.toContain('arch8');
    // B's own success still adopts arch9.
    releaseSecond(json(200, session('arch9', OLDER)));
    await settle();
    await settle();
    expect(hydratedIDs()).toContain('arch9');
  });

  it('a rejected or lost newest resume keeps the prior root and never adopts a stale success', async () => {
    const connection = archivedConnection();
    const { promise: first, resolve: releaseFirst } = Promise.withResolvers();
    vi.mocked(sdk.ReopenSession)
      .mockImplementationOnce(() => first as never)
      .mockResolvedValueOnce(json(404, { code: 'not_found', message: 'no longer archived' }));
    const { target } = await mountApp(connection);
    await openArchived(target);
    (target.querySelector('[aria-label="Resume session arch8"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Resume session arch9"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(target.textContent).toContain('Resume failed for session arch9');
    releaseFirst(json(200, session('arch8', OLDER)));
    await settle();
    await settle();
    // The prior root is preserved; the stale A success never lands.
    expect(hydratedIDs()).not.toContain('arch8');
    expect(target.querySelector('.toolbar')?.textContent).toContain('root1');
    // A lost newest resume behaves the same.
    const { promise: second, resolve: releaseSecond } = Promise.withResolvers();
    vi.mocked(sdk.ReopenSession)
      .mockImplementationOnce(() => second as never)
      .mockRejectedValueOnce(new Error('reopen lost'));
    await reopenArchived(target);
    (target.querySelector('[aria-label="Resume session arch8"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Resume session arch9"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(target.textContent).toContain('Resume outcome unknown for session arch9');
    releaseSecond(json(200, session('arch8', OLDER)));
    await settle();
    await settle();
    expect(hydratedIDs()).not.toContain('arch8');
    expect(target.querySelector('.toolbar')?.textContent).toContain('root1');
  });

  it('later root, New and Workspace selection supersede a parked resume', async () => {    const connection = archivedConnection();
    vi.mocked(sdk.ListWorkspaces).mockResolvedValue(json(200, [
      { root: '/ws', display_name: 'ws' },
      { root: '/ws2', display_name: 'ws2' },
    ]));
    vi.mocked(sdk.CreateSession).mockResolvedValue(json(200, session('root-new', NEWER)));
    const { promise: first, resolve: releaseFirst } = Promise.withResolvers();
    vi.mocked(sdk.ReopenSession).mockImplementationOnce(() => first as never);
    const { target } = await mountApp(connection);
    await openArchived(target);
    (target.querySelector('[aria-label="Resume session arch8"]') as HTMLElement).click();
    await settle();
    // A later explicit root selection supersedes the parked resume.
    (target.querySelector('[aria-label="Open sessions"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Select session root2"]') as HTMLElement).click();
    await settle();
    releaseFirst(json(200, session('arch8', OLDER)));
    await settle();
    await settle();
    expect(hydratedIDs()).not.toContain('arch8');
    expect(target.querySelector('.toolbar')?.textContent).toContain('root2');
    // A Workspace composition supersedes a parked resume too.
    const { promise: second, resolve: releaseSecond } = Promise.withResolvers();
    vi.mocked(sdk.ReopenSession).mockImplementationOnce(() => second as never);
    await reopenArchived(target);
    (target.querySelector('[aria-label="Resume session arch8"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Select workspace"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Select workspace /ws2"]') as HTMLElement).click();
    await settle();
    await settle();
    releaseSecond(json(200, session('arch8', OLDER)));
    await settle();
    await settle();
    expect(hydratedIDs()).not.toContain('arch8');
    expect(target.textContent).toContain('/ws2');
    // A picker New that succeeds supersedes a parked resume as well.
    const { promise: third, resolve: releaseThird } = Promise.withResolvers();
    vi.mocked(sdk.ReopenSession).mockImplementationOnce(() => third as never);
    await reopenArchived(target);
    (target.querySelector('[aria-label="Resume session arch8"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Open sessions"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="New session"]') as HTMLElement).click();
    await settle();
    await settle();
    releaseThird(json(200, session('arch8', OLDER)));
    await settle();
    await settle();
    expect(hydratedIDs()).not.toContain('arch8');
    expect(target.textContent).toContain('root-new');
  });

  it('a failed Resume re-reads the retained root without another hint and keeps its loaded history', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ListSessions).mockImplementation(async (options) => {
      const lifecycle = (JSON.parse(options) as { query: { lifecycle: string } }).query.lifecycle;
      return lifecycle === 'open'
        ? json(200, [session('root1', NEWER)])
        : json(200, [{ ...session('arch8', OLDER), lifecycle: 'archived' as const }]);
    });
    vi.mocked(sdk.ReopenSession).mockResolvedValue(json(404, { code: 'not_found', message: 'no longer archived' }));
    const initial = hydration({
      conversation: { items: [inputItem('i-old', 'user', 'op-old')] },
      operations: [runningOperation('op-old')],
    });
    const fresh = hydration({
      conversation: { items: [inputItem('i-old', 'user', 'op-old'), inputItem('i-new', 'user', 'op-new')] },
      operations: [runningOperation('op-old')],
    });
    const { promise: parked, resolve: releaseParked } = Promise.withResolvers();
    vi.mocked(sdk.GetSessionHydration)
      .mockResolvedValueOnce(json(200, initial))
      .mockImplementationOnce(() => parked as never)
      .mockResolvedValue(json(200, fresh));
    const { target } = await mountApp(connection);
    await vi.waitFor(() => expect(target.textContent).toContain('i-old'));
    // A revision hint starts the refresh that the failed Resume supersedes.
    deliverEvent(connection, {
      kind: 'session_changed',
      scope: { session_id: 'root1' },
      session_revision: { instance_id: INSTANCE, durable_revision: '2', local_revision: '0' },
    });
    await vi.waitFor(() => expect(vi.mocked(sdk.GetSessionHydration)).toHaveBeenCalledTimes(2));
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    await settle();
    (target.querySelector('[aria-label="Archived sessions"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Resume session arch8"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(target.textContent).toContain('Resume failed for session arch8');
    // The superseded parked response settles: the retained root re-reads with
    // the fresh stamp WITHOUT another hint and keeps its loaded history.
    releaseParked(json(200, initial));
    await settle();
    await settle();
    expect(vi.mocked(sdk.GetSessionHydration)).toHaveBeenCalledTimes(3);
    expect(target.querySelector('.toolbar')?.textContent).toContain('root1');
    expect(target.textContent).toContain('i-old');
    expect(target.textContent).toContain('i-new');
  });

  it('a stale failed Resume never refreshes a newer selected root', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ListSessions).mockImplementation(async (options) => {
      const lifecycle = (JSON.parse(options) as { query: { lifecycle: string } }).query.lifecycle;
      return lifecycle === 'open'
        ? json(200, [session('root1', NEWER), session('root2', OLDER)])
        : json(200, [{ ...session('arch8', OLDER), lifecycle: 'archived' as const }]);
    });
    const { promise: resumeGate, resolve: releaseResume } = Promise.withResolvers();
    vi.mocked(sdk.ReopenSession).mockImplementationOnce(() => resumeGate as never);
    const { target } = await mountApp(connection);
    await openArchived(target);
    (target.querySelector('[aria-label="Resume session arch8"]') as HTMLElement).click();
    await settle();
    // Move to another root while the Resume is parked.
    (target.querySelector('[aria-label="Open sessions"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Select session root2"]') as HTMLElement).click();
    await settle();
    await settle();
    const root2Reads = vi.mocked(sdk.GetSessionHydration).mock.calls.filter(
      (call) => (JSON.parse(call[0] as string) as { session_id: string }).session_id === 'root2',
    ).length;
    releaseResume(json(404, { code: 'not_found', message: 'no longer archived' }));
    await settle();
    await settle();
    // The addressed failure is still reported, but the newer root is neither
    // dirtied nor refreshed by the stale Resume.
    expect(target.textContent).toContain('Resume failed for session arch8');
    expect(target.querySelector('.toolbar')?.textContent).toContain('root2');
    const root2ReadsAfter = vi.mocked(sdk.GetSessionHydration).mock.calls.filter(
      (call) => (JSON.parse(call[0] as string) as { session_id: string }).session_id === 'root2',
    ).length;
    expect(root2ReadsAfter).toBe(root2Reads);
  });
});

describe('agent type ownership', () => {
  it('offers every returned definition including compact and dispatches the actual name', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetConfiguration).mockResolvedValue(json(200, {
      configuration_revision: { instance_id: INSTANCE, generation: '1' },
      settings: { sessions: { auto_archive: false, archive_after_days: 1, delete_after_archive_days: 1 }, plugins: {} },
      agents: [
        { name: 'primary', description: 'Primary agent', subagent: false, model: '', readonly: false, write_dir: '', tools: [], capabilities: [] },
        { name: 'compact', description: 'Compaction', subagent: false, model: '', readonly: true, write_dir: '', tools: [], capabilities: [] },
        { name: 'reviewer', description: 'Reviewer', subagent: false, model: '', readonly: false, write_dir: '', tools: [], capabilities: [] },
      ],
      providers: [],
    }));
    vi.mocked(sdk.SetSessionAgentType).mockResolvedValue(json(200, session('root1', NEWER)));
    const { target } = await mountApp(connection);
    const openAgentPicker = async () => {
      await openSettings(target);
      (target.querySelector('[aria-label="Agent type"]') as HTMLElement).click();
      await settle();
    };
    await openAgentPicker();
    const choices = [...target.querySelectorAll('dialog .row')].map((row) => row.textContent);
    expect(choices.some((text) => text?.includes('primary'))).toBe(true);
    expect(choices.some((text) => text?.includes('compact'))).toBe(true);
    expect(choices.some((text) => text?.includes('reviewer'))).toBe(true);
    (target.querySelector('[aria-label="Set session agent type compact"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(vi.mocked(sdk.SetSessionAgentType)).toHaveBeenCalledWith('{"session_id":"root1","body":{"agent_type":"compact"}}');
    // A second returned definition dispatches its own actual name as well.
    await openAgentPicker();
    (target.querySelector('[aria-label="Set session agent type reviewer"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(vi.mocked(sdk.SetSessionAgentType).mock.calls.at(-1)?.[0]).toEqual('{"session_id":"root1","body":{"agent_type":"reviewer"}}');
    // The server stays the accepting owner: a generated rejection is displayed.
    vi.mocked(sdk.SetSessionAgentType).mockResolvedValue(json(400, { code: 'invalid', message: 'unknown agent type' }));
    await openAgentPicker();
    (target.querySelector('[aria-label="Set session agent type compact"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(target.textContent).toContain('Agent type change failed');
    expect(target.textContent).toContain('unknown agent type');
  });
});

describe('settings editor', () => {
  it('saves the complete body with every unedited token and plugin string preserved', async () => {
    const connection = settingsRawConnection();
    const { target } = await mountApp(connection);
    await openSettings(target);
    expect(target.textContent).toContain('Uses plugin defaults');
    const readsBeforeSave = vi.mocked(sdk.GetConfiguration).mock.calls.length;
    (target.querySelector('[aria-label="Save settings"]') as HTMLElement).click();
    await settle();
    await settle();
    const call = vi.mocked(sdk.UpdateConfigurationSettings).mock.calls[0][0] as string;
    expect(bodyOf(call)).toBe(`{"settings":${EXACT_SETTINGS_TEXT}}`);
    // The published mutation triggers no Reload; the editor's exact read
    // re-runs after the save and replaces the text.
    expect(vi.mocked(sdk.ReloadConfiguration)).not.toHaveBeenCalled();
    expect(vi.mocked(sdk.GetConfiguration).mock.calls.length).toBeGreaterThanOrEqual(readsBeforeSave + 1);
  });

  it('authors an absent plugin document on its first member edit, one member at a time', async () => {
    const connection = settingsRawConnection();
    const { target } = await mountApp(connection);
    await openSettings(target);
    expect(target.textContent).toContain('Uses plugin defaults');
    await changeInput(target, 'jobs max_background_processes', '3');
    await changeInput(target, 'jobs max_output_bytes', '9');
    (target.querySelector('[aria-label="Save settings"]') as HTMLElement).click();
    await settle();
    await settle();
    const body = bodyOf(vi.mocked(sdk.UpdateConfigurationSettings).mock.calls[0][0] as never);
    // The absent document became {}-plus-its-edited-members, composed one member
    // at a time, and the unedited tools document keeps its exact lexeme.
    expect(body).toContain('"jobs":"{\\"max_background_processes\\":3,\\"max_output_bytes\\":9}"');
    expect(body).toContain('"tools":"{\\"max_output_bytes\\":9007199254740993}"');
  });

  it('gates every authored control while a save is pending and sends the complete body with no precondition', async () => {
    const connection = settingsRawConnection();
    const { promise: gate, resolve: releaseSave } = Promise.withResolvers();
    vi.mocked(sdk.UpdateConfigurationSettings).mockImplementationOnce(() => gate as never);
    const { target } = await mountApp(connection);
    await openSettings(target);
    await changeInput(target, 'archive_after_days', '5');
    await settle();
    (target.querySelector('[aria-label="Save settings"]') as HTMLElement).click();
    await settle();
    // The pending Save gates the editor: no second local operation exists.
    expect((target.querySelector('[aria-label="archive_after_days"]') as HTMLInputElement).disabled).toBe(true);
    expect((target.querySelector('[aria-label="jobs max_background_processes"]') as HTMLInputElement).disabled).toBe(true);
    expect((target.querySelector('[aria-label="Save settings"]') as HTMLButtonElement).disabled).toBe(true);
    releaseSave(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: {} }));
    await settle();
    await settle();
    const call = vi.mocked(sdk.UpdateConfigurationSettings).mock.calls[0][0] as string;
    // The complete captured document is sent once; whole-settings LWW keeps
    // the endpoint's established contract without a revision precondition.
    expect(bodyOf(call)).toContain('"archive_after_days":5');
    expect(bodyOf(call)).toContain('"delete_after_archive_days":1');
    expect(bodyOf(call)).not.toContain('revision');
    expect(vi.mocked(sdk.UpdateConfigurationSettings)).toHaveBeenCalledTimes(1);
    // The success refresh releases the editor.
    expect((target.querySelector('[aria-label="Save settings"]') as HTMLButtonElement).disabled).toBe(false);
  });

  it('displays a known settings save rejection and retains the editor draft', async () => {
    const connection = settingsRawConnection();
    vi.mocked(sdk.UpdateConfigurationSettings).mockResolvedValueOnce(
      json(400, { code: 'invalid', message: 'sessions interval rejected' }),
    );
    const { target } = await mountApp(connection);
    await openSettings(target);
    await changeInput(target, 'archive_after_days', '5');
    await settle();
    (target.querySelector('[aria-label="Save settings"]') as HTMLElement).click();
    await settle();
    await settle();
    // The generated error is displayed and the editor's draft is retained.
    expect(target.textContent).toContain('Settings save failed');
    expect(target.textContent).toContain('sessions interval rejected');
    // A subsequent save carries the retained edited document.
    (target.querySelector('[aria-label="Save settings"]') as HTMLElement).click();
    await settle();
    await settle();
    const call = vi.mocked(sdk.UpdateConfigurationSettings).mock.calls.at(-1)?.[0] as string;
    expect(bodyOf(call)).toContain('"archive_after_days":5');
    expect(target.textContent).toContain('Settings saved');
  });
});

// --- Host-local appearance and version (no Runtime document involvement) ------

describe('host-local appearance and version', () => {
  const DEFAULTS = { wrapCode: false, fontScale: 100, viewerFraction: 0.5 };

  afterEach(async () => {
    // The shared store is a module singleton; restore its defaults so this
    // surface leaves no appearance state for its neighbours.
    const { settings } = await import('../lib/settings.js');
    settings.set({ ...DEFAULTS });
    localStorage.removeItem('lightcode.settings');
  });

  it('edits and persists the shared store with the 50-200 clamp and never touches the Runtime document', async () => {
    const connection = settingsRawConnection();
    const { target } = await mountApp(connection);
    await openSettings(target);
    expect((target.querySelector('[aria-label="Version"]') as HTMLElement).textContent).toBe('dev (test)');
    await toggleCheckbox(target, 'Wrap code blocks', true);
    await changeInput(target, 'Font scale', '900');
    expect(JSON.parse(localStorage.getItem('lightcode.settings') as string))
      .toEqual({ wrapCode: true, fontScale: 200, viewerFraction: 0.5 });
    await changeInput(target, 'Font scale', '10');
    expect(JSON.parse(localStorage.getItem('lightcode.settings') as string))
      .toEqual({ wrapCode: true, fontScale: 50, viewerFraction: 0.5 });
    expect((target.querySelector('[aria-label="Font scale"]') as HTMLInputElement).value).toBe('50');
    await toggleCheckbox(target, 'Wrap code blocks', false);
    expect(JSON.parse(localStorage.getItem('lightcode.settings') as string).wrapCode).toBe(false);
    // Appearance is client-owned UI state: no configuration write.
    expect(vi.mocked(sdk.UpdateConfigurationSettings)).not.toHaveBeenCalled();
  });

  it('keeps the host-local rows live while the Runtime settings read is pending', async () => {
    const connection = settingsRawConnection();
    const { promise: gate, resolve: release } = Promise.withResolvers();
    let reads = 0;
    // The boot global read resolves; the editor's own exact read stays parked.
    vi.mocked(sdk.GetConfiguration).mockImplementation(async () => {
      reads += 1;
      return reads === 1 ? native(200, RAW_CONFIGURATION, false) : (gate as never);
    });
    const { target } = await mountApp(connection);
    (target.querySelector('[aria-label="Settings"]') as HTMLElement).click();
    await settle();
    // Runtime settings are still pending...
    expect(target.textContent).toContain('Loading…');
    // ...while the host-local rows are already live.
    expect((target.querySelector('[aria-label="Version"]') as HTMLElement).textContent).toBe('dev (test)');
    await toggleCheckbox(target, 'Wrap code blocks', true);
    expect(JSON.parse(localStorage.getItem('lightcode.settings') as string).wrapCode).toBe(true);
    release(native(200, RAW_CONFIGURATION, false));
    await settle();
    await settle();
    expect(target.textContent).not.toContain('Loading…');
    expect(target.querySelector('[aria-label="archive_after_days"]')).not.toBeNull();
  });

  it('keeps the host-local rows live when the Runtime settings read is unavailable', async () => {
    const connection = settingsRawConnection();
    let reads = 0;
    vi.mocked(sdk.GetConfiguration).mockImplementation(async () => {
      reads += 1;
      if (reads === 1) return native(200, RAW_CONFIGURATION, false);
      throw new Error('runtime unavailable');
    });
    const { target } = await mountApp(connection);
    (target.querySelector('[aria-label="Settings"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(target.textContent).toContain('runtime unavailable');
    expect(target.querySelector('[aria-label="archive_after_days"]')).toBeNull();
    expect((target.querySelector('[aria-label="Version"]') as HTMLElement).textContent).toBe('dev (test)');
    await toggleCheckbox(target, 'Wrap code blocks', true);
    await changeInput(target, 'Font scale', '150');
    expect(JSON.parse(localStorage.getItem('lightcode.settings') as string))
      .toEqual({ wrapCode: true, fontScale: 150, viewerFraction: 0.5 });
  });

  it('displays the actual version read failure while keeping the fallback and usable appearance controls', async () => {
    vi.mocked(AppVersion).mockRejectedValueOnce(new Error('version unavailable'));
    const connection = settingsRawConnection();
    const { target } = await mountApp(connection);
    await openSettings(target);
    // The retained fallback survives a failed read...
    expect((target.querySelector('[aria-label="Version"]') as HTMLElement).textContent).toBe('');
    // ...and the actual failure reaches the existing visible note surface.
    expect(target.querySelector('.note.outcome')?.textContent).toContain('version unavailable');
    await toggleCheckbox(target, 'Wrap code blocks', true);
    expect(JSON.parse(localStorage.getItem('lightcode.settings') as string).wrapCode).toBe(true);
  });
});

describe('provider and model tasks', () => {
  it('sends only the edited field and refreshes through its own stamped read', async () => {
    const connection = editorConnection([providerFixture()]);
    vi.mocked(sdk.UpdateProviderDetail).mockResolvedValue(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: providerFixture() }));
    const { target } = await mountApp(connection);
    await openProviderEditor(target);
    (target.querySelector('[aria-label="Select provider prov1"]') as HTMLElement).click();
    await settle();
    await changeInput(target, 'provider name', 'Renamed');
    await settle();
    await settle();
    const params = vi.mocked(sdk.UpdateProviderDetail).mock.calls[0][0] as string;
    expect((JSON.parse(params) as { query: unknown }).query).toEqual({ provider_id: 'prov1' });
    expect(bodyOf(params)).toBe('{"provider":{"name":"Renamed"}}');
    expect(vi.mocked(sdk.ReloadConfiguration)).not.toHaveBeenCalled();
    expect(vi.mocked(sdk.GetProviderDetail).mock.calls.length).toBeGreaterThan(1);
  });

  it('malformed authored member JSON is a known local rejection with its input retained and no dispatch', async () => {
    const connection = editorConnection([providerFixture()]);
    vi.mocked(sdk.UpdateProviderDetail).mockResolvedValue(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: providerFixture() }));
    const { target } = await mountApp(connection);
    await openProviderEditor(target);
    (target.querySelector('[aria-label="Select provider prov1"]') as HTMLElement).click();
    await settle();
    await changeInput(target, 'provider extra_body', '{not json');
    await settle();
    await settle();
    // The malformed composed invocation text is a known local rejection: no
    // native dispatch, the authored value is retained, and no unknown outcome.
    expect(vi.mocked(sdk.UpdateProviderDetail)).not.toHaveBeenCalled();
    expect(target.textContent).toContain('Provider edit failed');
    expect(target.textContent).not.toContain('Outcome unknown');
    expect((target.querySelector('[aria-label="provider extra_body"]') as HTMLTextAreaElement).value).toBe('{not json');
  });

  it('renders no retired provider controls beside the live members', async () => {
    const connection = editorConnection([providerFixture()]);
    const { target } = await mountApp(connection);
    await openProviderEditor(target);
    (target.querySelector('[aria-label="Select provider prov1"]') as HTMLElement).click();
    await settle();
    // The retired controls have no row and no reset affordance...
    expect(target.querySelector('[aria-label="provider max_tokens_field"]')).toBeNull();
    expect(target.querySelector('[aria-label="Reset provider max_tokens_field"]')).toBeNull();
    expect(target.querySelector('[aria-label="provider options"]')).toBeNull();
    expect(target.querySelector('[aria-label="Reset provider options"]')).toBeNull();
    // ...while the live provider and model members stay mounted.
    expect(target.querySelector('[aria-label="provider extra_body"]')).not.toBeNull();
    expect(target.querySelector('[aria-label="provider api_key_env"]')).not.toBeNull();
    expect(target.querySelector('[aria-label="m1 max_output_tokens"]')).not.toBeNull();
  });

  it('resets one provider field through the generated reset path', async () => {
    const connection = editorConnection([providerFixture()]);
    vi.mocked(sdk.ResetProviderField).mockResolvedValue(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: providerFixture() }));
    const { target } = await mountApp(connection);
    await openProviderEditor(target);
    (target.querySelector('[aria-label="Select provider prov1"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Reset provider name"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(vi.mocked(sdk.ResetProviderField)).toHaveBeenCalledWith('{"field":"name","query":{"provider_id":"prov1"}}');
  });

  it('resets one model field through the generated model reset path', async () => {
    const connection = editorConnection([providerFixture()]);
    vi.mocked(sdk.ResetProviderModelField).mockResolvedValue(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: providerFixture() }));
    const { target } = await mountApp(connection);
    await openProviderEditor(target);
    (target.querySelector('[aria-label="Select provider prov1"]') as HTMLElement).click();
    await settle();
    // The one reset rule selects the model operation and adds the model_id.
    (target.querySelector('[aria-label="Reset m1 name"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(vi.mocked(sdk.ResetProviderModelField)).toHaveBeenCalledWith('{"field":"name","query":{"provider_id":"prov1","model_id":"m1"}}');
  });

  it('resets model input_modalities through the shared model reset rule', async () => {
    const connection = editorConnection([providerFixture()]);
    vi.mocked(sdk.ResetProviderModelField).mockResolvedValue(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: providerFixture() }));
    const { target } = await mountApp(connection);
    await openProviderEditor(target);
    (target.querySelector('[aria-label="Select provider prov1"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Reset m1 input_modalities"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(vi.mocked(sdk.ResetProviderModelField)).toHaveBeenCalledWith('{"field":"input_modalities","query":{"provider_id":"prov1","model_id":"m1"}}');
    // A reset sends no metadata echo and triggers no reload.
    expect(vi.mocked(sdk.UpdateProviderModel)).not.toHaveBeenCalled();
    expect(vi.mocked(sdk.ReloadConfiguration)).not.toHaveBeenCalled();
  });

  it('resets model cost through the shared model reset rule', async () => {
    const connection = editorConnection([providerFixture()]);
    vi.mocked(sdk.ResetProviderModelField).mockResolvedValue(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: providerFixture() }));
    const { target } = await mountApp(connection);
    await openProviderEditor(target);
    (target.querySelector('[aria-label="Select provider prov1"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Reset m1 cost"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(vi.mocked(sdk.ResetProviderModelField)).toHaveBeenCalledWith('{"field":"cost","query":{"provider_id":"prov1","model_id":"m1"}}');
    expect(vi.mocked(sdk.UpdateProviderModel)).not.toHaveBeenCalled();
    expect(vi.mocked(sdk.ReloadConfiguration)).not.toHaveBeenCalled();
  });

  it('forwards an authored null and a raw JSON member verbatim', async () => {
    const connection = editorConnection([providerFixture()]);
    vi.mocked(sdk.UpdateProviderDetail).mockResolvedValue(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: providerFixture() }));
    const { target } = await mountApp(connection);
    await openProviderEditor(target);
    (target.querySelector('[aria-label="Select provider prov1"]') as HTMLElement).click();
    await settle();
    const textarea = target.querySelector('[aria-label="provider extra_body"]') as HTMLTextAreaElement;
    textarea.value = 'null';
    textarea.dispatchEvent(new Event('change', { bubbles: true }));
    await settle();
    await settle();
    const call = vi.mocked(sdk.UpdateProviderDetail).mock.calls[0][0] as string;
    expect(bodyOf(call)).toBe('{"provider":{"extra_body":null}}');
  });

  it('keeps Reload explicit: no automatic reload after a published model mutation', async () => {
    const connection = editorConnection([providerFixture()]);
    vi.mocked(sdk.UpdateProviderModel).mockResolvedValue(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: modelFixture() }));
    vi.mocked(sdk.ReloadConfiguration).mockResolvedValue(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '3' } }));
    const { target } = await mountApp(connection);
    await openProviderEditor(target);
    (target.querySelector('[aria-label="Select provider prov1"]') as HTMLElement).click();
    await settle();
    await toggleCheckbox(target, 'm1 hidden', true);
    await settle();
    await settle();
    const params = vi.mocked(sdk.UpdateProviderModel).mock.calls[0][0] as string;
    expect((JSON.parse(params) as { query: unknown }).query).toEqual({ provider_id: 'prov1', model_id: 'm1' });
    expect(bodyOf(params)).toBe('{"model":{"hidden":true}}');
    expect(vi.mocked(sdk.ReloadConfiguration)).not.toHaveBeenCalled();
    (target.querySelector('[aria-label="Reload configuration"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(vi.mocked(sdk.ReloadConfiguration)).toHaveBeenCalledTimes(1);
  });

  it('reports an external credential refusal from connect', async () => {
    const disconnected = providerFixture({ connected: false, key_source: 'external' });
    const connection = editorConnection([disconnected]);
    vi.mocked(sdk.ConnectProvider).mockResolvedValue(json(401, { code: 'unauthorized', message: 'external key rejected' }));
    const { target } = await mountApp(connection);
    await openProviderEditor(target);
    (target.querySelector('[aria-label="Select provider prov1"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Connect provider"]') as HTMLElement).click();
    await settle();
    const keyInput = target.querySelector('[aria-label="Connect api key"]') as HTMLInputElement;
    keyInput.value = 'secret-value';
    keyInput.dispatchEvent(new Event('input', { bubbles: true }));
    (target.querySelector('[aria-label="Submit connect"]') as HTMLElement).click();
    await settle();
    await settle();
    const call = JSON.parse(vi.mocked(sdk.ConnectProvider).mock.calls[0][0]) as { body: { api_key?: string } };
    expect(call.body).toEqual({ api_key: 'secret-value' });
    expect(target.querySelector('.note.outcome')?.textContent).toContain('external key rejected');
  });

  it('discovers candidates and selects them into the authored create body', async () => {
    const connection = editorConnection([]);
    vi.mocked(sdk.DiscoverProviderCandidates).mockResolvedValue(json(200, [
      { id: 'cand-1', name: 'Candidate One', context_window: 4096, max_output_tokens: 512, usable: true },
    ]));
    vi.mocked(sdk.CreateProvider).mockResolvedValue(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: null }));
    const { target } = await mountApp(connection);
    await openNewProvider(target);
    await changeInput(target, 'Provider id', 'prov2');
    await changeInput(target, 'Provider name', 'Prov Two');
    await changeInput(target, 'Provider base url', 'http://127.0.0.1:2/v1');
    await changeInput(target, 'Provider api key env', 'PROV2_KEY');
    const key = target.querySelector('[aria-label="Provider api key"]') as HTMLInputElement;
    key.value = 'create-secret';
    key.dispatchEvent(new Event('input', { bubbles: true }));
    (target.querySelector('[aria-label="Discover provider candidates"]') as HTMLElement).click();
    await settle();
    await settle();
    const discover = JSON.parse(vi.mocked(sdk.DiscoverProviderCandidates).mock.calls[0][0]) as { body: Record<string, unknown> };
    expect(discover.body).toEqual({ base_url: 'http://127.0.0.1:2/v1', api_key_env: 'PROV2_KEY', api_key: 'create-secret' });
    expect((target.querySelector('[aria-label="Select candidate cand-1"]') as HTMLInputElement).checked).toBe(true);
    (target.querySelector('[aria-label="Create provider"]') as HTMLElement).click();
    await settle();
    await settle();
    const call = JSON.parse(vi.mocked(sdk.CreateProvider).mock.calls[0][0]) as { body: Record<string, unknown> };
    // The composed invocation text carries the exact-parsed candidate tokens
    // verbatim, so compare the actual dispatched document.
    expect(JSON.parse(JSON.stringify(call.body))).toEqual({
      id: 'prov2',
      provider: { name: 'Prov Two', base_url: 'http://127.0.0.1:2/v1', api_key_env: 'PROV2_KEY' },
      models: { 'cand-1': { name: 'Candidate One', context_window: 4096, max_output_tokens: 512 } },
      api_key: 'create-secret',
    });
  });

  it('treats prototype-named candidates as ordinary data and never inherits selection', async () => {
    const connection = editorConnection([]);
    vi.mocked(sdk.DiscoverProviderCandidates).mockResolvedValue(json(200, [
      { id: '__proto__', name: 'Proto Model', context_window: 4096, max_output_tokens: 512, usable: true },
      { id: 'toString', name: 'String Model', context_window: 2048, max_output_tokens: 256, usable: true },
      { id: 'constructor', name: 'Ctor Model', context_window: 1024, max_output_tokens: 128, usable: false },
    ]));
    vi.mocked(sdk.CreateProvider).mockResolvedValue(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: null }));
    const { target } = await mountApp(connection);
    await openNewProvider(target);
    await changeInput(target, 'Provider id', 'protos');
    await changeInput(target, 'Provider base url', 'http://127.0.0.1:9/v1');
    (target.querySelector('[aria-label="Discover provider candidates"]') as HTMLElement).click();
    await settle();
    await settle();
    // An unavailable prototype-named candidate is not selected by inheritance.
    expect((target.querySelector('[aria-label="Select candidate constructor"]') as HTMLInputElement).checked).toBe(false);
    // The user unselects the ordinary prototype-named candidate; __proto__ stays selected.
    expect((target.querySelector('[aria-label="Select candidate toString"]') as HTMLInputElement).checked).toBe(true);
    (target.querySelector('[aria-label="Select candidate toString"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Create provider"]') as HTMLElement).click();
    await settle();
    await settle();
    const text = bodyOf(vi.mocked(sdk.CreateProvider).mock.calls[0][0] as string);
    expect(text).toContain('"models":{"__proto__":{"name":"Proto Model","context_window":4096,"max_output_tokens":512}}');
    expect(text).not.toContain('toString');
    expect(text).not.toContain('constructor');
  });

  it('omits a blank api_key_env from the create body so Runtime can allocate with a supplied key', async () => {
    const connection = editorConnection([]);
    vi.mocked(sdk.CreateProvider).mockResolvedValue(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: null }));
    const { target } = await mountApp(connection);
    await openNewProvider(target);
    await changeInput(target, 'Provider id', 'prov2');
    await changeInput(target, 'Provider name', 'Authored Name');
    await changeInput(target, 'Provider base url', 'http://127.0.0.1:2/v1');
    const key = target.querySelector('[aria-label="Provider api key"]') as HTMLInputElement;
    key.value = 'supplied-key';
    key.dispatchEvent(new Event('input', { bubbles: true }));
    (target.querySelector('[aria-label="Create provider"]') as HTMLElement).click();
    await settle();
    await settle();
    const body = (JSON.parse(vi.mocked(sdk.CreateProvider).mock.calls[0][0]) as { body: Record<string, unknown> }).body;
    const provider = body.provider as Record<string, unknown>;
    expect('api_key_env' in provider).toBe(false);
    expect(provider.name).toBe('Authored Name');
    expect(body.api_key).toBe('supplied-key');
    // No client-side allocation, reload or disconnect policy follows the create.
    expect(vi.mocked(sdk.ReloadConfiguration)).not.toHaveBeenCalled();
    expect(vi.mocked(sdk.ConnectProvider)).not.toHaveBeenCalled();
    expect(vi.mocked(sdk.DisconnectProvider)).not.toHaveBeenCalled();
  });

  it('refreshes the provider list for every successful metadata mutation, including a model edit', async () => {
    const connection = editorConnection([providerFixture()]);
    vi.mocked(sdk.UpdateProviderModel).mockResolvedValue(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: modelFixture() }));
    const { target } = await mountApp(connection);
    await openProviderEditor(target);
    (target.querySelector('[aria-label="Select provider prov1"]') as HTMLElement).click();
    await settle();
    const listReadsBefore = vi.mocked(sdk.ListProviders).mock.calls.length;
    await toggleCheckbox(target, 'm1 hidden', true);
    await settle();
    await settle();
    expect(vi.mocked(sdk.UpdateProviderModel)).toHaveBeenCalledTimes(1);
    // The provider's connected label is derived from model metadata, so a
    // successful model mutation refreshes the provider list too.
    expect(vi.mocked(sdk.ListProviders).mock.calls.length).toBeGreaterThan(listReadsBefore);
  });

  it('sends an explicit api_key_env unchanged and a keyless create carries no api_key', async () => {
    const connection = editorConnection([]);
    vi.mocked(sdk.CreateProvider).mockResolvedValue(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: null }));
    const { target } = await mountApp(connection);
    await openNewProvider(target);
    await changeInput(target, 'Provider id', 'prov2');
    await changeInput(target, 'Provider base url', 'http://127.0.0.1:2/v1');
    await changeInput(target, 'Provider api key env', 'AUTHORED_KEY');
    (target.querySelector('[aria-label="Create provider"]') as HTMLElement).click();
    await settle();
    await settle();
    const body = (JSON.parse(vi.mocked(sdk.CreateProvider).mock.calls[0][0]) as { body: Record<string, unknown> }).body;
    expect((body.provider as Record<string, unknown>).api_key_env).toBe('AUTHORED_KEY');
    expect('api_key' in body).toBe(false);
  });
});

describe('provider exact reads', () => {
  const RAW_DETAIL = '{"configuration_revision":{"instance_id":"' + INSTANCE + '","generation":"1"},'
    + '"provider":{"id":"prov1","name":"Prov One","builtin":false,"connected":true,"key_source":"managed",'
    + '"base_url":"http://127.0.0.1:1/v1","api_key_env":"PROV1_KEY","headers":{},"user_headers":{},'
    + '"system_role":"system","usage_in_stream":false,"discovery":true,"hidden":false,'
    + '"protocol_metadata":{"limit":9007199254740993,"rate":1e400},'
    + '"models":[{"id":"m1","name":"Model One","context_window":9007199254740993,"max_output_tokens":1e400,'
    + '"input_modalities":["text"],"system_role":"system","usage_in_stream":false,"hidden":false,"source":"user",'
    + '"incomplete":false,"provider_hidden":false,"usable":true,"cost":{"input":9007199254740993,"output":2.5}}]}}';

  async function exactEditor(rawDetail: string) {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ListProviders).mockResolvedValue(json(200, {
      configuration_revision: { instance_id: INSTANCE, generation: '1' },
      providers: [providerFixture()],
    }));
    vi.mocked(sdk.GetProviderDetail).mockResolvedValue(native(200, rawDetail, false));
    vi.mocked(sdk.UpdateProviderModel).mockResolvedValue(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: modelFixture() }));
    const { target } = await mountApp(connection);
    await openSettings(target);
    (target.querySelector('[aria-label="Providers"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Select provider prov1"]') as HTMLElement).click();
    await settle();
    await settle();
    return target;
  }

  it('retains opaque provider/model lexemes from the exact read through a cost reconstruction', async () => {
    const target = await exactEditor(RAW_DETAIL);
    expect((target.querySelector('[aria-label="m1 context_window"]') as HTMLInputElement).value).toBe('9007199254740993');
    expect((target.querySelector('[aria-label="m1 max_output_tokens"]') as HTMLInputElement).value).toBe('1e400');
    expect((target.querySelector('[aria-label="m1 cost input"]') as HTMLInputElement).value).toBe('9007199254740993');
    expect((target.querySelector('[aria-label="provider protocol_metadata"]') as HTMLTextAreaElement).value)
      .toBe('{"limit":9007199254740993,"rate":1e400}');
    await changeInput(target, 'm1 cost output', '3');
    await settle();
    await settle();
    const call = vi.mocked(sdk.UpdateProviderModel).mock.calls[0][0] as string;
    expect(bodyOf(call)).toBe('{"model":{"cost":{"input":9007199254740993,"output":3}}}');
  });

  it('prefills candidate number fields with their exact tokens and writes them unchanged', async () => {
    const target = await exactEditor(RAW_DETAIL);
    vi.mocked(sdk.DiscoverProviderModelCandidates).mockResolvedValue(native(200,
      '[{"id":"cand-big","name":"Big","context_window":9007199254740993,"max_output_tokens":1e400,"usable":true}]',
    false));
    (target.querySelector('[aria-label="Add model"]') as HTMLElement).click();
    await settle();
    await settle();
    (target.querySelector('[aria-label="Select candidate cand-big"]') as HTMLElement).click();
    await tick();
    expect((target.querySelector('[aria-label="Model context window"]') as HTMLInputElement).value).toBe('9007199254740993');
    expect((target.querySelector('[aria-label="Model max output"]') as HTMLInputElement).value).toBe('1e400');
    (target.querySelector('[aria-label="Upsert model"]') as HTMLElement).click();
    await settle();
    await settle();
    const call = vi.mocked(sdk.UpdateProviderModel).mock.calls[0][0] as string;
    expect(bodyOf(call)).toBe('{"model":{"name":"Big","context_window":9007199254740993,"max_output_tokens":1e400}}');
  });
});

describe('provider discovery supersession and busy controls', () => {
  // One-shot implementation queues are per-test state: reset the SDK calls
  // this describe scripts so a failing case never leaks an unconsumed gate
  // into the next mounted editor.
  beforeEach(() => {
    for (const call of [
      sdk.ListProviders,
      sdk.GetProviderDetail,
      sdk.DiscoverProviderModelCandidates,
      sdk.DiscoverProviderCandidates,
      sdk.UpdateProviderDetail,
      sdk.UpdateProviderModel,
      sdk.ConnectProvider,
      sdk.CreateProvider,
    ]) {
      vi.mocked(call).mockReset();
    }
  });

  function candidate(id: string) {
    return { id, name: id, context_window: 8, max_output_tokens: 1, usable: true };
  }

  it('publishes only the newest model-discovery response for the same provider', async () => {
    const connection = editorConnection([providerFixture()]);
    const { promise: first, resolve: releaseFirst } = Promise.withResolvers();
    vi.mocked(sdk.DiscoverProviderModelCandidates)
      .mockImplementationOnce(() => first as never)
      .mockResolvedValueOnce(json(200, [candidate('cand-new')]));
    const { target } = await mountApp(connection);
    await openProviderEditor(target);
    (target.querySelector('[aria-label="Select provider prov1"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Add model"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Add model"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(target.querySelector('[aria-label="Select candidate cand-new"]')).not.toBeNull();
    releaseFirst(native(200, JSON.stringify([candidate('cand-old')]), false));
    await settle();
    await settle();
    expect(target.querySelector('[aria-label="Select candidate cand-new"]')).not.toBeNull();
    expect(target.querySelector('[aria-label="Select candidate cand-old"]')).toBeNull();
  });

  it('discards an earlier provider discovery success after the selection moves', async () => {
    const connection = editorConnection([providerFixture(), providerFixture({ id: 'prov2', name: 'Prov Two' })]);
    const { promise: first, resolve: releaseFirst } = Promise.withResolvers();
    const { promise: second, resolve: releaseSecond } = Promise.withResolvers();
    vi.mocked(sdk.DiscoverProviderModelCandidates)
      .mockImplementationOnce(() => first as never)
      .mockImplementationOnce(() => second as never);
    const { target } = await mountApp(connection);
    await openProviderEditor(target);
    (target.querySelector('[aria-label="Select provider prov1"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Add model"]') as HTMLElement).click();
    await settle();
    // Move to prov2 and open its own discovery panel.
    (target.querySelector('[aria-label="Select provider prov2"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Add model"]') as HTMLElement).click();
    await settle();
    releaseFirst(native(200, JSON.stringify([candidate('cand-a')]), false));
    await settle();
    await settle();
    // The stale success belongs to prov1 and must not populate prov2's panel.
    expect(target.querySelector('[aria-label="Select candidate cand-a"]')).toBeNull();
    releaseSecond(native(200, JSON.stringify([candidate('cand-b')]), false));
    await settle();
    await settle();
    expect(target.querySelector('[aria-label="Select candidate cand-b"]')).not.toBeNull();
    expect(target.querySelector('[aria-label="Select candidate cand-a"]')).toBeNull();
  });

  it('discards an earlier provider discovery error after the selection moves', async () => {
    const connection = editorConnection([providerFixture(), providerFixture({ id: 'prov2', name: 'Prov Two' })]);
    const { promise: first, resolve: releaseFirst } = Promise.withResolvers();
    vi.mocked(sdk.DiscoverProviderModelCandidates).mockImplementationOnce(() => first as never);
    const { target } = await mountApp(connection);
    await openProviderEditor(target);
    (target.querySelector('[aria-label="Select provider prov1"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Add model"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Select provider prov2"]') as HTMLElement).click();
    await settle();
    releaseFirst(json(404, { code: 'not_found', message: 'provider gone' }));
    await settle();
    await settle();
    expect(target.textContent).not.toContain('Model discovery failed');
  });

  it('invalidates an earlier discovery request across an A-to-B-to-A selection move', async () => {
    const connection = editorConnection([providerFixture(), providerFixture({ id: 'prov2', name: 'Prov Two' })]);
    const { promise: first, resolve: releaseFirst } = Promise.withResolvers();
    vi.mocked(sdk.DiscoverProviderModelCandidates)
      .mockImplementationOnce(() => first as never)
      .mockResolvedValueOnce(json(200, [candidate('cand-new')]));
    const { target } = await mountApp(connection);
    await openProviderEditor(target);
    (target.querySelector('[aria-label="Select provider prov1"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Add model"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Select provider prov2"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Select provider prov1"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Add model"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(target.querySelector('[aria-label="Select candidate cand-new"]')).not.toBeNull();
    releaseFirst(native(200, JSON.stringify([candidate('cand-old')]), false));
    await settle();
    await settle();
    expect(target.querySelector('[aria-label="Select candidate cand-new"]')).not.toBeNull();
    expect(target.querySelector('[aria-label="Select candidate cand-old"]')).toBeNull();
  });

  it('discovers provider candidates from a base URL without an environment name', async () => {
    const connection = editorConnection([]);
    vi.mocked(sdk.DiscoverProviderCandidates).mockResolvedValue(json(200, []));
    const { target } = await mountApp(connection);
    await openNewProvider(target);
    await changeInput(target, 'Provider base url', 'http://127.0.0.1:9/v1');
    const discoverButton = target.querySelector('[aria-label="Discover provider candidates"]') as HTMLButtonElement;
    expect(discoverButton.disabled).toBe(false);
    discoverButton.click();
    await settle();
    await settle();
    expect(vi.mocked(sdk.DiscoverProviderCandidates)).toHaveBeenCalledTimes(1);
    expect((JSON.parse(vi.mocked(sdk.DiscoverProviderCandidates).mock.calls[0][0]) as { body: unknown }).body)
      .toEqual({ base_url: 'http://127.0.0.1:9/v1', api_key_env: '' });
    // A supplied one-off key still needs no environment name.
    const key = target.querySelector('[aria-label="Provider api key"]') as HTMLInputElement;
    key.value = 'supplied-key';
    key.dispatchEvent(new Event('input', { bubbles: true }));
    discoverButton.click();
    await settle();
    await settle();
    expect((JSON.parse(vi.mocked(sdk.DiscoverProviderCandidates).mock.calls[1][0]) as { body: unknown }).body)
      .toEqual({ base_url: 'http://127.0.0.1:9/v1', api_key_env: '', api_key: 'supplied-key' });
  });

  it('keeps provider and model controls disabled through a parked mutation and its parked refresh', async () => {
    const connection = editorConnection([providerFixture()]);
    const { promise: writeGate, resolve: releaseWrite } = Promise.withResolvers();
    const { promise: refreshGate, resolve: releaseRefresh } = Promise.withResolvers();
    vi.mocked(sdk.UpdateProviderDetail).mockImplementationOnce(() => writeGate as never);
    vi.mocked(sdk.GetProviderDetail)
      .mockResolvedValueOnce(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '1' }, provider: providerFixture() }))
      .mockImplementationOnce(() => refreshGate as never);
    const { target } = await mountApp(connection);
    await openProviderEditor(target);
    (target.querySelector('[aria-label="Select provider prov1"]') as HTMLElement).click();
    await settle();
    await changeInput(target, 'provider name', 'Renamed');
    await settle();
    const control = (label: string) => target.querySelector(`[aria-label="${label}"]`) as HTMLInputElement;
    expect(control('provider name').disabled).toBe(true);
    expect(control('provider usage_in_stream').disabled).toBe(true);
    expect(control('provider extra_body').disabled).toBe(true);
    expect(control('m1 name').disabled).toBe(true);
    expect(control('m1 hidden').disabled).toBe(true);
    expect(control('m1 cost input').disabled).toBe(true);
    releaseWrite(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: providerFixture() }));
    await settle();
    await settle();
    // Still busy: the required follow-up detail read is parked.
    expect(control('provider name').disabled).toBe(true);
    expect(control('m1 hidden').disabled).toBe(true);
    releaseRefresh(native(200, JSON.stringify({ configuration_revision: { instance_id: INSTANCE, generation: '1' }, provider: providerFixture({ name: 'Renamed' }) }), false));
    await settle();
    await settle();
    expect(control('provider name').disabled).toBe(false);
    expect(control('m1 hidden').disabled).toBe(false);
  });

  it('keeps the connect form disabled through its mutation and the provider-list refresh', async () => {
    const disconnected = providerFixture({ connected: false, key_source: 'external' });
    const connection = editorConnection([disconnected]);
    const { promise: connectGate, resolve: releaseConnect } = Promise.withResolvers();
    const { promise: listGate, resolve: releaseList } = Promise.withResolvers();
    vi.mocked(sdk.ConnectProvider).mockImplementationOnce(() => connectGate as never);
    vi.mocked(sdk.ListProviders)
      .mockImplementationOnce(async () => json(200, { configuration_revision: { instance_id: INSTANCE, generation: '1' }, providers: [disconnected] }))
      .mockImplementationOnce(() => listGate as never);
    const { target } = await mountApp(connection);
    await openProviderEditor(target);
    (target.querySelector('[aria-label="Select provider prov1"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Connect provider"]') as HTMLElement).click();
    await settle();
    const key = target.querySelector('[aria-label="Connect api key"]') as HTMLInputElement;
    key.value = 'connect-secret';
    key.dispatchEvent(new Event('input', { bubbles: true }));
    (target.querySelector('[aria-label="Submit connect"]') as HTMLElement).click();
    await settle();
    expect(key.disabled).toBe(true);
    expect((target.querySelector('[aria-label="Submit connect"]') as HTMLButtonElement).disabled).toBe(true);
    releaseConnect(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: disconnected }));
    await settle();
    await settle();
    // The mutation settled, but its provider-list refresh is still parked.
    expect((target.querySelector('[aria-label="Connect provider"]') as HTMLButtonElement).disabled).toBe(true);
    releaseList(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, providers: [disconnected] }));
    await settle();
    await settle();
    expect((target.querySelector('[aria-label="Connect provider"]') as HTMLButtonElement).disabled).toBe(false);
  });

  it('keeps the create form and candidate selection disabled while the create is parked', async () => {
    const connection = editorConnection([]);
    vi.mocked(sdk.DiscoverProviderCandidates).mockResolvedValue(json(200, [candidate('cand-1')]));
    const { promise: createGate, resolve: releaseCreate } = Promise.withResolvers();
    vi.mocked(sdk.CreateProvider).mockImplementationOnce(() => createGate as never);
    const { target } = await mountApp(connection);
    await openNewProvider(target);
    await changeInput(target, 'Provider id', 'prov2');
    await changeInput(target, 'Provider base url', 'http://127.0.0.1:2/v1');
    await changeInput(target, 'Provider api key env', 'PROV2_KEY');
    (target.querySelector('[aria-label="Discover provider candidates"]') as HTMLElement).click();
    await settle();
    await settle();
    const checkbox = target.querySelector('[aria-label="Select candidate cand-1"]') as HTMLInputElement;
    expect(checkbox.disabled).toBe(false);
    (target.querySelector('[aria-label="Create provider"]') as HTMLElement).click();
    await settle();
    expect((target.querySelector('[aria-label="Provider id"]') as HTMLInputElement).disabled).toBe(true);
    expect((target.querySelector('[aria-label="Provider base url"]') as HTMLInputElement).disabled).toBe(true);
    expect(checkbox.disabled).toBe(true);
    expect((target.querySelector('[aria-label="Discover provider candidates"]') as HTMLButtonElement).disabled).toBe(true);
    releaseCreate(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: null }));
    await settle();
    await settle();
  });

  it('keeps the upsert form and candidate selection disabled while the upsert is parked', async () => {
    const connection = editorConnection([providerFixture()]);
    vi.mocked(sdk.DiscoverProviderModelCandidates).mockResolvedValue(json(200, [candidate('cand-9')]));
    const { promise: upsertGate, resolve: releaseUpsert } = Promise.withResolvers();
    const { promise: detailGate, resolve: releaseDetail } = Promise.withResolvers();
    vi.mocked(sdk.UpdateProviderModel).mockImplementationOnce(() => upsertGate as never);
    vi.mocked(sdk.GetProviderDetail)
      .mockResolvedValueOnce(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '1' }, provider: providerFixture() }))
      .mockImplementationOnce(() => detailGate as never);
    const { target } = await mountApp(connection);
    await openProviderEditor(target);
    (target.querySelector('[aria-label="Select provider prov1"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Add model"]') as HTMLElement).click();
    await settle();
    await settle();
    (target.querySelector('[aria-label="Select candidate cand-9"]') as HTMLElement).click();
    await tick();
    (target.querySelector('[aria-label="Upsert model"]') as HTMLElement).click();
    await settle();
    expect((target.querySelector('[aria-label="Model id"]') as HTMLInputElement).disabled).toBe(true);
    expect((target.querySelector('[aria-label="Model name"]') as HTMLInputElement).disabled).toBe(true);
    expect((target.querySelector('[aria-label="Select candidate cand-9"]') as HTMLButtonElement).disabled).toBe(true);
    releaseUpsert(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: modelFixture() }));
    await settle();
    await settle();
    // The upsert settled, but its required follow-up detail read is parked.
    expect((target.querySelector('[aria-label="Add model"]') as HTMLButtonElement).disabled).toBe(true);
    releaseDetail(native(200, JSON.stringify({ configuration_revision: { instance_id: INSTANCE, generation: '2' }, provider: providerFixture() }), false));
    await settle();
    await settle();
    expect((target.querySelector('[aria-label="Add model"]') as HTMLButtonElement).disabled).toBe(false);
  });
});

describe('file viewer', () => {
  it.each([
    {
      title: 'reads an explicit workspace/path and displays canonical path and truncation',
      path: 'src/a.ts',
      reply: json(200, { content: 'file body', canonical_path: '/ws/src/a.ts', truncated: true }),
      verify: (target: HTMLElement) => {
        expect(vi.mocked(sdk.ReadWorkspaceFile)).toHaveBeenCalledWith('{"body":{"workspace":"/ws","path":"src/a.ts"}}');
        expect(target.textContent).toContain('/ws/src/a.ts');
        expect(target.textContent).toContain('truncated');
        expect(target.textContent).toContain('file body');
      },
    },
    {
      title: 'shows a denied read as an addressed error',
      path: '../etc/passwd',
      reply: json(404, { code: 'not_found', message: 'path is outside the workspace' }),
      verify: (target: HTMLElement) => {
        expect(target.textContent).toContain('path is outside the workspace');
      },
    },
  ])('$title', async ({ path, reply, verify }) => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ReadWorkspaceFile).mockResolvedValue(reply);
    const { target } = await mountApp(connection);
    await openSettings(target);
    (target.querySelector('[aria-label="File viewer"]') as HTMLElement).click();
    await settle();
    await changeInput(target, 'File path', path);
    (target.querySelector('[aria-label="Open file"]') as HTMLElement).click();
    await settle();
    await settle();
    verify(target);
  });

  it.each([
    {
      title: 'ignores a superseded viewer reply and keeps the newest read',
      newest: json(200, { content: 'second body', canonical_path: '/ws/b.ts', truncated: false }),
      stale: json(200, { content: 'stale body', canonical_path: '/ws/a.ts', truncated: false }),
      absent: 'stale body',
    },
    {
      title: 'ignores a superseded viewer error and keeps the newest read',
      newest: json(200, { content: 'fresh body', canonical_path: '/ws/b.ts', truncated: false }),
      stale: json(404, { code: 'not_found', message: 'stale denial' }),
      absent: 'stale denial',
    },
  ])('$title', async ({ newest, stale, absent }) => {
    const connection = fakeConnection();
    basicMocks(connection);
    const { promise: first, resolve: releaseFirst } = Promise.withResolvers();
    vi.mocked(sdk.ReadWorkspaceFile)
      .mockImplementationOnce(() => first as never)
      .mockResolvedValueOnce(newest);
    const { target } = await mountApp(connection);
    await openSettings(target);
    (target.querySelector('[aria-label="File viewer"]') as HTMLElement).click();
    await settle();
    await changeInput(target, 'File path', 'a.ts');
    (target.querySelector('[aria-label="Open file"]') as HTMLElement).click();
    await settle();
    await changeInput(target, 'File path', 'b.ts');
    (target.querySelector('[aria-label="Open file"]') as HTMLElement).click();
    await settle();
    await settle();
    // The stale first reply is released only after the newer reply published.
    releaseFirst(stale);
    await settle();
    await settle();
    expect(target.textContent).toContain('/ws/b.ts');
    expect(target.textContent).not.toContain(absent);
  });
});

describe('code snapshots', () => {
  it('reverts current code to a chosen Operation boundary and shows a partial failure', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetSessionCodeSnapshots).mockResolvedValue(json(200, [
      { operation_id: 'op1', group_item_id: 'group1', files: [{ path: 'a.ts', existed: true }] },
    ]));
    vi.mocked(sdk.RevertSessionCode).mockResolvedValue(json(409, {
      restored: ['a.ts'],
      skipped: [{ path: 'b.ts', reason: 'io' }],
      error: { code: 'storage', message: 'partial failure' },
    }));
    const { target } = await mountApp(connection);
    await openSnapshots(target);
    await settle();
    await settle();
    expect(target.textContent).toContain('Operation op1');
    (target.querySelector('[aria-label="Revert code to before operation op1"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(vi.mocked(sdk.RevertSessionCode)).toHaveBeenCalledWith('{"session_id":"root1","body":{"boundary_operation_id":"op1"}}');
    expect(target.textContent).toContain('Restored: a.ts');
    expect(target.textContent).toContain('Skipped: b.ts (io)');
    expect(target.textContent).toContain('Restore failed: partial failure');
  });



  it('reports a current rewind outcome addressed to its loaded Session', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetSessionCodeSnapshots).mockResolvedValue(json(200, [
      { operation_id: 'op1', group_item_id: 'group1', files: [{ path: 'a.ts', existed: true }] },
    ]));
    vi.mocked(sdk.RevertSessionCode).mockResolvedValue(json(409, {
      restored: ['a.ts'],
      skipped: [],
      error: { code: 'storage', message: 'partial failure' },
    }));
    const { target } = await mountApp(connection);
    await openSnapshots(target);
    await settle();
    await settle();
    (target.querySelector('[aria-label="Revert code to before operation op1"]') as HTMLElement).click();
    await settle();
    await settle();
    const note = target.querySelector('.note.outcome')?.textContent ?? '';
    expect(note).toContain('session root1');
    expect(note).toContain('Restored: a.ts');
    expect(note).toContain('Restore failed: partial failure');
  });


  it('reports a pending rewind after a repair unmounts the dialog, without retransmitting', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetSessionCodeSnapshots).mockResolvedValue(json(200, [
      { operation_id: 'op1', group_item_id: 'group1', files: [{ path: 'a.ts', existed: true }] },
    ]));
    const { promise: revertGate, reject: releaseRevert } = Promise.withResolvers();
    vi.mocked(sdk.RevertSessionCode).mockImplementationOnce(() => revertGate as never);
    const { target } = await mountApp(connection);
    await openSnapshots(target);
    await settle();
    await settle();
    (target.querySelector('[aria-label="Revert code to before operation op1"]') as HTMLElement).click();
    await settle();
    // A new-instance repair unmounts the dialog while the rewind is parked.
    connection.openMock.mockResolvedValue({ instance_id: INSTANCE, generation: '2', workspace: '/ws' });
    connection.failureHandler?.(new Error('lost'), '1');
    await settle();
    await settle();
    await settle();
    expect(target.querySelector('[aria-label="Close snapshots"]')).toBeNull();
    releaseRevert(new Error('rewind lost'));
    await settle();
    await settle();
    const note = target.querySelector('.note.outcome')?.textContent ?? '';
    expect(note).toContain('Rewind outcome unknown');
    expect(note).toContain('session root1');
    expect(vi.mocked(sdk.RevertSessionCode)).toHaveBeenCalledTimes(1);
  });
});

describe('usage details and global warnings', () => {
  it('lists exact per-model usage rows without inventing zero rows', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, hydration({
      usage: {
        totals: {
          by_model: [
            { model: 'p/m', usage: { input_tokens: '9007199254740993', cached_input_tokens: '2', output_tokens: '3' } },
          ],
        },
        context: { used_tokens: '123456789012345678901', context_window: 200000 },
      },
    })));
    const { target } = await mountApp(connection);
    (target.querySelector('[aria-label="Show token usage"]') as HTMLElement).click();
    await settle();
    expect(target.textContent).toContain('9007199254740993');
    expect(target.textContent).toContain('123456789012345678901');
    // Exactly one model row plus the header; absence is not displayed as zero.
    expect(target.querySelectorAll('dialog .row')).toHaveLength(2);
    // An absent estimate is displayed as unknown, never as a zero.
    expect(target.querySelector('dialog .estimate')?.textContent).toContain('unknown');
  });

  it('renders a present session cost estimate as an approximate USD value', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, hydration({
      usage: {
        totals: {
          by_model: [
            { model: 'p/m', usage: { input_tokens: '10', cached_input_tokens: '2', output_tokens: '3' } },
          ],
          estimated_cost_usd: 0.015,
        },
        context: { used_tokens: '12', context_window: 100 },
      },
    })));
    const { target } = await mountApp(connection);
    (target.querySelector('[aria-label="Show token usage"]') as HTMLElement).click();
    await settle();
    expect(target.querySelector('dialog .estimate')?.textContent).toContain('$0.015');
  });

  it('renders a known-zero session cost estimate as zero', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, hydration({
      usage: {
        totals: { by_model: [], estimated_cost_usd: 0 },
        context: { used_tokens: '0', context_window: 0 },
      },
    })));
    const { target } = await mountApp(connection);
    (target.querySelector('[aria-label="Show token usage"]') as HTMLElement).click();
    await settle();
    const estimate = target.querySelector('dialog .estimate')?.textContent ?? '';
    expect(estimate).toContain('$0');
    expect(estimate).not.toContain('unknown');
  });

  it('falls back to GetWarnings globals when no hydration is displayed', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetWarnings).mockResolvedValue(json(200, {
      warnings_revision: { instance_id: INSTANCE, revision: '1' },
      warnings: [
        { source: 'g', kind: 'k', message: 'global warning' },
        { source: 's', kind: 'k', message: 'session warning', session_id: 'root1' },
      ],
    }));
    vi.mocked(sdk.ArchiveSession).mockResolvedValue(json(200, session('root1', NEWER)));
    vi.mocked(sdk.DeleteSession).mockResolvedValue(native(204, '', false));
    const { target } = await mountApp(connection);
    // Remove the selected root: no hydration remains, so the global-only
    // indicator filters absent session_id from the GetWarnings snapshot.
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    await settle();
    (target.querySelector('[aria-label="Delete session root1"]') as HTMLElement).click();
    await settle();
    await settle();
    const warn = target.querySelector('.warn') as HTMLElement;
    expect(warn.textContent).toContain('⚠ 1');
    warn.click();
    await settle();
    expect(target.textContent).toContain('global warning');
    expect(target.textContent).not.toContain('session warning');
    expect(target.textContent).not.toContain('Delete outcome unknown');
  });
});

describe('replacement Fork and ordered rewind', () => {
  it('exposes Fork only on root user-origin inputs, with rewind only for an owned input', async () => {
    const connection = forkConnection();
    const { target } = await mountApp(connection);
    expect(target.querySelectorAll('.fork')).toHaveLength(2);
    expect(target.querySelector('[aria-label="Fork from input i-runtime"]')).toBeNull();
    expect(target.querySelector('[aria-label="Fork from input i-plugin"]')).toBeNull();
    (target.querySelector('[aria-label="Fork from input i-owned"]') as HTMLElement).click();
    await settle();
    expect(target.querySelector('[aria-label="Rewind code before Fork"]')).not.toBeNull();
    (target.querySelector('[aria-label="Close fork dialog"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Fork from input i-copied"]') as HTMLElement).click();
    await settle();
    // A copied input keeps a normal Fork: no invented Operation boundary.
    expect(target.querySelector('[aria-label="Rewind code before Fork"]')).toBeNull();
  });

  it('leaves the child inspector passive: no Fork actions there', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetSessionHydration).mockImplementation(async (options) => {
      const id = (JSON.parse(options) as { session_id: string }).session_id;
      if (id === 'child1') {
        return json(200, hydration({ conversation: { items: [{ ...inputItem('child-owned', 'user', 'op-child'), content: [{ kind: 'text' as const, text: 'hi' }, { kind: 'opaque' as const, opaque_wire_type: 'w', extra: { big: JSON.rawJSON('9007199254740993') } }] }] } }));
      }
      return json(200, hydration({ background: [{ kind: 'child', id: 'child1' }] }));
    });
    const { target } = await mountApp(connection);
    (target.querySelector('[aria-label="Background sessions"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Inspect child session child1"]') as HTMLElement).click();
    await settle();
    await settle();
    const childPane = target.querySelector('.child-pane') as HTMLElement;
    expect(childPane).not.toBeNull();
    expect(childPane.querySelector('.fork')).toBeNull();
    // The child read keeps exact numeric lexemes.
    expect(childPane.textContent).toContain('9007199254740993');
  });

  it('prefills the generated content parts and forwards the exact edited replacement text', async () => {
    const connection = forkConnection();
    const multipart = [
      { kind: 'text' as const, text: 'hi' },
      { kind: 'image_url' as const, url: 'http://img/x.png' },
      { kind: 'opaque' as const, opaque_wire_type: 'w', extra: { a: 1 } },
    ];
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, hydration({
      conversation: { items: [{ ...inputItem('i-owned', 'user', 'op-owned'), content: multipart }] },
      operations: [runningOperation('op-owned')],
    })));
    vi.mocked(sdk.ForkSession).mockResolvedValue(json(200, { session: session('dest1', NEWER), operation: runningOperation('caller') }));
    const { target } = await mountApp(connection);
    (target.querySelector('[aria-label="Fork from input i-owned"]') as HTMLElement).click();
    await settle();
    const textarea = target.querySelector('[aria-label="Replacement content JSON"]') as HTMLTextAreaElement;
    expect(JSON.parse(textarea.value)).toEqual(multipart);
    const edited = '[ {"kind":"text","text":"edited"} ]';
    textarea.value = edited;
    textarea.dispatchEvent(new Event('input', { bubbles: true }));
    (target.querySelector('[aria-label="Submit fork"]') as HTMLElement).click();
    await settle();
    await settle();
    const call = vi.mocked(sdk.ForkSession).mock.calls[0][0] as string;
    const body = JSON.parse(bodyOf(call)) as { content: unknown };
    expect(body.content).toEqual(JSON.parse(edited));
    expect(bodyOf(call)).toContain('"content":[ {"kind":"text","text":"edited"} ]');
  });

  it('malformed authored replacement content is a known local rejection with its draft retained and no dispatch', async () => {
    const connection = forkConnection();
    vi.mocked(sdk.ForkSession).mockResolvedValue(json(200, { session: session('dest1', NEWER), operation: runningOperation('caller') }));
    const { target } = await mountApp(connection);
    (target.querySelector('[aria-label="Fork from input i-owned"]') as HTMLElement).click();
    await settle();
    const textarea = target.querySelector('[aria-label="Replacement content JSON"]') as HTMLTextAreaElement;
    const malformed = '[ {"kind":"text",';
    textarea.value = malformed;
    textarea.dispatchEvent(new Event('input', { bubbles: true }));
    (target.querySelector('[aria-label="Submit fork"]') as HTMLElement).click();
    await settle();
    await settle();
    // The malformed composed invocation text never reaches the native binding;
    // the known local rejection retains the draft and never becomes Outcome
    // unknown or a fabricated Runtime error.
    expect(vi.mocked(sdk.ForkSession)).not.toHaveBeenCalled();
    expect(target.querySelector('.attempt.rejected')).not.toBeNull();
    expect(target.querySelector('.attempt.unknown')).toBeNull();
    expect((target.querySelector('[aria-label="Replacement content JSON"]') as HTMLTextAreaElement).value).toBe(malformed);
  });

  it('a 200 null Fork document is never accepted and raises no consumer exception', async () => {
    const connection = forkConnection();
    vi.mocked(sdk.ForkSession).mockResolvedValue({ status: 200, body: 'null', invalid: false });
    const { target } = await mountApp(connection);
    await openOwnedFork(target, false);
    await settle();
    await settle();
    // The null success document is invalid delivery: the attempt stays
    // unknown with its draft and no destination is adopted.
    expect(target.querySelector('.attempt.unknown')?.textContent).toContain('Outcome unknown');
    expect((target.querySelector('[aria-label="Replacement content JSON"]') as HTMLTextAreaElement).value).toContain('text i-owned');
    expect(target.textContent).not.toContain('Fork admitted');
    expect(vi.mocked(sdk.ForkSession)).toHaveBeenCalledTimes(1);
  });

  it('awaits the rewind reply, shows it, then issues the Fork with the same attempt identity', async () => {
    const connection = forkConnection();
    const order: string[] = [];
    vi.mocked(sdk.RevertSessionCode).mockImplementation(async () => {
      order.push('rewind');
      return json(200, { restored: ['a.ts'], skipped: [{ path: 'b.ts', reason: 'unchanged' }] });
    });
    vi.mocked(sdk.ForkSession).mockImplementation(async (options) => {
      order.push('fork');
      const request = options as string;
      const body = JSON.parse(bodyOf(request)) as { operation_id: string };
      return json(200, { session: session('dest1', NEWER), operation: runningOperation(body.operation_id) });
    });
    const { target } = await mountApp(connection);
    await openOwnedFork(target, true);
    await settle();
    await settle();
    await settle();
    expect(order).toEqual(['rewind', 'fork']);
    expect(vi.mocked(sdk.RevertSessionCode)).toHaveBeenCalledWith('{"session_id":"root1","body":{"boundary_operation_id":"op-owned"}}');
    const forkCall = vi.mocked(sdk.ForkSession).mock.calls[0][0] as string;
    const forkBody = JSON.parse(bodyOf(forkCall)) as { boundary_item_id: string; operation_id: string; content: unknown };
    expect(forkBody.boundary_item_id).toBe('i-owned');
    expect(forkBody.operation_id).toMatch(/^[0-9a-f]{32}$/);
    expect(forkBody.content).toEqual([{ kind: 'text', text: 'text i-owned' }]);
    // The returned destination is adopted while the initiating selection still
    // matches.
    expect(vi.mocked(sdk.GetSessionHydration).mock.calls.at(-1)?.[0]).toEqual('{"session_id":"dest1"}');
  });

  it('shows a known partial restore failure before still issuing Fork', async () => {
    const connection = forkConnection();
    const { promise: forkGate, resolve: releaseFork } = Promise.withResolvers();
    vi.mocked(sdk.RevertSessionCode).mockResolvedValue(json(409, {
      restored: ['a.ts'],
      skipped: [{ path: 'b.ts', reason: 'io' }],
      error: { code: 'storage', message: 'partial failure' },
    }));
    vi.mocked(sdk.ForkSession).mockImplementationOnce(() => forkGate as never);
    const { target } = await mountApp(connection);
    await openOwnedFork(target, true);
    await settle();
    await settle();
    // The complete known rewind result is displayed before Fork settles.
    expect(target.textContent).toContain('Restored: a.ts');
    expect(target.textContent).toContain('Skipped: b.ts (io)');
    expect(target.textContent).toContain('Restore failed: partial failure');
    expect(vi.mocked(sdk.ForkSession)).toHaveBeenCalledTimes(1);
    releaseFork(json(200, { session: session('dest1', NEWER), operation: runningOperation('x') }));
    await settle();
    await settle();
    // The fast successful Fork closes the dialog but the earlier effects stay
    // published with the addressed outcome.
    expect(target.querySelector('[aria-label="Close fork dialog"]')).toBeNull();
    const outcome = target.querySelector('.note.outcome')?.textContent ?? '';
    expect(outcome).toContain('Restored: a.ts');
    expect(outcome).toContain('Skipped: b.ts (io)');
    expect(outcome).toContain('Restore failed: partial failure');
    expect(outcome).toContain('Fork admitted to session dest1.');
  });

  it('publishes a handled restore success with the addressed Fork outcome', async () => {
    const connection = forkConnection();
    vi.mocked(sdk.RevertSessionCode).mockResolvedValue(json(200, { restored: ['a.ts'], skipped: [] }));
    const { target } = await mountApp(connection);
    await openOwnedFork(target, true);
    await settle();
    await settle();
    await settle();
    const outcome = target.querySelector('.note.outcome')?.textContent ?? '';
    expect(outcome).toContain('Restored: a.ts');
    expect(outcome).toContain('Fork admitted to session dest1.');
  });

  it('a late non-adopted destination still reports the earlier restore effects', async () => {
    const connection = forkConnection();
    const { promise: forkGate, resolve: releaseFork } = Promise.withResolvers();
    vi.mocked(sdk.RevertSessionCode).mockResolvedValue(json(409, {
      restored: ['a.ts'],
      skipped: [],
      error: { code: 'storage', message: 'partial failure' },
    }));
    vi.mocked(sdk.ForkSession).mockImplementationOnce(() => forkGate as never);
    const { target } = await mountApp(connection);
    await openOwnedFork(target, true);
    await settle();
    await settle();
    // Move the selection while the Fork is parked.
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    (target.querySelectorAll('dialog .row')[1] as HTMLElement).click();
    await settle();
    releaseFork(json(200, { session: session('dest1', NEWER), operation: runningOperation('x') }));
    await settle();
    await settle();
    const outcome = target.querySelector('.note.outcome')?.textContent ?? '';
    expect(outcome).toContain('Restored: a.ts');
    expect(outcome).toContain('Restore failed: partial failure');
    expect(outcome).toContain('selection moved');
    expect(target.textContent).toContain('root2');
  });

  it('stops on an unknown rewind: Fork is never sent and no Retry could bypass it', async () => {
    const connection = forkConnection();
    vi.mocked(sdk.RevertSessionCode).mockRejectedValue(new Error('rewind lost'));
    const { target } = await mountApp(connection);
    await openOwnedFork(target, true);
    await settle();
    await settle();
    expect(vi.mocked(sdk.RevertSessionCode)).toHaveBeenCalledTimes(1);
    expect(vi.mocked(sdk.ForkSession)).not.toHaveBeenCalled();
    expect(target.textContent).toContain('Rewind outcome unknown');
    // The dialog and the retained composer row both offer abandonment only.
    expect(target.querySelector('[aria-label="Abandon fork attempt"]')).not.toBeNull();
    const row = target.querySelector('.attempt.unknown') as HTMLElement;
    expect(row).not.toBeNull();
    expect(row.querySelector('[aria-label="Retry"]')).toBeNull();
    expect(row.querySelector('[aria-label="Abandon"]')).not.toBeNull();
  });

  it('retries an unknown Fork with the same identity and never reruns the handled restore', async () => {
    const connection = forkConnection();
    vi.mocked(sdk.RevertSessionCode).mockResolvedValue(json(200, { restored: [], skipped: [] }));
    vi.mocked(sdk.ForkSession)
      .mockRejectedValueOnce(new Error('fork lost'))
      .mockImplementationOnce(async (options) => {
        const request = options as string;
        const body = JSON.parse(bodyOf(request)) as { operation_id: string };
        return json(200, { session: session('dest1', NEWER), operation: runningOperation(body.operation_id) });
      });
    const { target } = await mountApp(connection);
    await openOwnedFork(target, true);
    await settle();
    await settle();
    expect(target.textContent).toContain('Outcome unknown');
    const firstBody = bodyOf(vi.mocked(sdk.ForkSession).mock.calls[0][0] as never);
    (target.querySelector('[aria-label="Retry"]') as HTMLElement).click();
    await settle();
    await settle();
    await settle();
    expect(vi.mocked(sdk.RevertSessionCode)).toHaveBeenCalledTimes(1);
    expect(vi.mocked(sdk.ForkSession)).toHaveBeenCalledTimes(2);
    expect(bodyOf(vi.mocked(sdk.ForkSession).mock.calls[1][0] as never)).toBe(firstBody);
    expect(vi.mocked(sdk.GetSessionHydration).mock.calls.at(-1)?.[0]).toEqual('{"session_id":"dest1"}');
    // The explicit retry keeps the handled restore outcome in the report
    // without repeating the restore stage.
    expect(target.querySelector('.note.outcome')?.textContent).toContain('Restored: (none)');
    expect(target.querySelector('.note.outcome')?.textContent).toContain('Fork admitted to session dest1.');
  });

  it('a held Retry of an unknown Fork transitions to pending and permits no second retry while its response is held', async () => {
    const connection = forkConnection();
    vi.mocked(sdk.RevertSessionCode).mockResolvedValue(json(200, { restored: [], skipped: [] }));
    const { promise: firstGate, resolve: releaseFork } = Promise.withResolvers();
    const { promise: retryGate, resolve: releaseRetry } = Promise.withResolvers();
    vi.mocked(sdk.ForkSession)
      .mockImplementationOnce(() => firstGate as never)
      .mockImplementationOnce(() => retryGate as never);
    const { target } = await mountApp(connection);
    await openOwnedFork(target, true);
    await settle();
    await settle();
    expect(vi.mocked(sdk.RevertSessionCode)).toHaveBeenCalledTimes(1);
    expect(vi.mocked(sdk.ForkSession)).toHaveBeenCalledTimes(1);
    const firstBody = bodyOf(vi.mocked(sdk.ForkSession).mock.calls[0][0] as never);
    releaseFork(new Error('fork lost'));
    await settle();
    await settle();
    expect(target.textContent).toContain('Outcome unknown');
    expect(target.querySelector('[aria-label="Retry fork"]')).not.toBeNull();
    expect(target.textContent).toContain('Restored: (none)');
    (target.querySelector('[aria-label="Retry fork"]') as HTMLElement).click();
    await settle();
    // The explicit Retry moves the attempt to pending: the old unknown Retry
    // Fork control is gone, so no second retry is permitted while the retry's
    // response is held.
    expect(target.querySelector('[aria-label="Retry fork"]')).toBeNull();
    expect(target.querySelector('[aria-label="Submit fork"]')?.textContent).toContain('Forking…');
    expect(vi.mocked(sdk.ForkSession)).toHaveBeenCalledTimes(2);
    // One retry keeps the immutable identity/payload and never reruns the
    // handled restore.
    expect(bodyOf(vi.mocked(sdk.ForkSession).mock.calls[1][0] as never)).toBe(firstBody);
    expect(vi.mocked(sdk.RevertSessionCode)).toHaveBeenCalledTimes(1);
    expect(target.textContent).toContain('Restored: (none)');
    releaseRetry(json(200, { session: session('dest1', NEWER), operation: runningOperation('x') }));
    await settle();
    await settle();
    await settle();
    // The released outcome lands with the held restore report and nothing
    // retransmits.
    expect(vi.mocked(sdk.ForkSession)).toHaveBeenCalledTimes(2);
    expect(target.querySelector('.note.outcome')?.textContent).toContain('Restored: (none)');
    expect(target.querySelector('.note.outcome')?.textContent).toContain('Fork admitted to session dest1.');
    expect(vi.mocked(sdk.GetSessionHydration).mock.calls.at(-1)?.[0]).toEqual('{"session_id":"dest1"}');
  });

  it('a held Retry of a rejected Fork transitions to pending and its release reports the final outcome beside the newer draft', async () => {
    const connection = forkConnection();
    const { promise: firstGate, resolve: releaseFork } = Promise.withResolvers();
    const { promise: retryGate, resolve: releaseRetry } = Promise.withResolvers();
    vi.mocked(sdk.ForkSession)
      .mockImplementationOnce(() => firstGate as never)
      .mockImplementationOnce(() => retryGate as never);
    const { target } = await mountApp(connection);
    await openOwnedFork(target, false);
    await settle();
    const firstBody = bodyOf(vi.mocked(sdk.ForkSession).mock.calls[0][0] as never);
    releaseFork(json(409, { code: 'conflict', message: 'fork rejected' }));
    await settle();
    await settle();
    expect(target.textContent).toContain('fork rejected');
    expect(target.querySelector('[aria-label="Retry fork"]')).not.toBeNull();
    (target.querySelector('[aria-label="Retry fork"]') as HTMLElement).click();
    await settle();
    // Pending transition: the old rejected Retry Fork control is gone while
    // the retry's response is held.
    expect(target.querySelector('[aria-label="Retry fork"]')).toBeNull();
    expect(target.querySelector('[aria-label="Submit fork"]')?.textContent).toContain('Forking…');
    expect(vi.mocked(sdk.ForkSession)).toHaveBeenCalledTimes(2);
    expect(bodyOf(vi.mocked(sdk.ForkSession).mock.calls[1][0] as never)).toBe(firstBody);
    // A newer replacement draft is typed while the retry is held.
    const textarea = target.querySelector('[aria-label="Replacement content JSON"]') as HTMLTextAreaElement;
    textarea.value = '[{"kind":"text","text":"newer draft"}]';
    textarea.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    releaseRetry(json(409, { code: 'conflict', message: 'fork rejected again' }));
    await settle();
    await settle();
    // The retry's own final outcome is reported, the dialog stays open with
    // the newer draft retained, and nothing retransmits.
    expect(target.textContent).toContain('fork rejected again');
    expect(target.querySelector('[aria-label="Close fork dialog"]')).not.toBeNull();
    expect((target.querySelector('[aria-label="Replacement content JSON"]') as HTMLTextAreaElement).value).toBe('[{"kind":"text","text":"newer draft"}]');
    expect(vi.mocked(sdk.ForkSession)).toHaveBeenCalledTimes(2);
  });

  it('never adopts a late destination over a moved selection and keeps a newer replacement draft', async () => {
    const connection = forkConnection();
    const { promise: forkGate, resolve: releaseFork } = Promise.withResolvers();
    vi.mocked(sdk.ForkSession).mockImplementationOnce(() => forkGate as never);
    const { target } = await mountApp(connection);
    await openOwnedFork(target, false);
    await settle();
    // A newer replacement draft is typed while the Fork is in flight.
    const textarea = target.querySelector('[aria-label="Replacement content JSON"]') as HTMLTextAreaElement;
    textarea.value = '[{"kind":"text","text":"newer draft"}]';
    textarea.dispatchEvent(new Event('input', { bubbles: true }));
    // The selection moves to another root before the Fork settles.
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    (target.querySelectorAll('dialog .row')[1] as HTMLElement).click();
    await settle();
    releaseFork(json(200, { session: session('dest1', NEWER), operation: runningOperation('x') }));
    await settle();
    await settle();
    expect(target.textContent).toContain('selection moved');
    // The moved view stays; the destination is not adopted.
    expect(target.textContent).toContain('root2');
    expect(vi.mocked(sdk.GetSessionHydration).mock.calls.some(
      (call) => (JSON.parse(call[0] as string) as { session_id: string }).session_id === 'dest1',
    )).toBe(false);
    // One active first-party dialog: opening the picker retired the Fork form.
    expect(target.querySelector('[aria-label="Close fork dialog"]')).toBeNull();
  });

  it('a rejected Fork keeps a newer replacement draft while the dialog stays open', async () => {
    const connection = forkConnection();
    vi.mocked(sdk.ForkSession).mockResolvedValue(json(409, { code: 'conflict', message: 'fork rejected' }));
    const { target } = await mountApp(connection);
    await openOwnedFork(target, false);
    await settle();
    // A newer replacement draft is typed while the Fork is in flight.
    const textarea = target.querySelector('[aria-label="Replacement content JSON"]') as HTMLTextAreaElement;
    textarea.value = '[{"kind":"text","text":"newer draft"}]';
    textarea.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    await settle();
    // The rejection is displayed, the dialog stays open for Retry/Abandon, and
    // the newer draft is not cleared by the late settlement.
    expect(target.textContent).toContain('fork rejected');
    const dialogTextarea = document.querySelector('[aria-label="Replacement content JSON"]') as HTMLTextAreaElement;
    expect(dialogTextarea.value).toBe('[{"kind":"text","text":"newer draft"}]');
  });
});

describe('restore outcomes through the fork path', () => {
  it('warns on a plain pre-traversal restore rejection and still issues Fork', async () => {
    const connection = forkConnection();
    vi.mocked(sdk.RevertSessionCode).mockResolvedValue(json(404, { code: 'not_found', message: 'no snapshot group' }));
    const { target } = await mountApp(connection);
    await openOwnedFork(target, true);
    await settle();
    await settle();
    await settle();
    expect(vi.mocked(sdk.ForkSession)).toHaveBeenCalledTimes(1);
  });

  it.each([
    ['treats a malformed 2xx restore body as outcome unknown and never sends Fork', json(200, {})],
    ['treats a 2xx restore body carrying an error member as outcome unknown and never sends Fork', json(200, {
      restored: ['a.ts'],
      skipped: [],
      error: { code: 'storage', message: 'partial failure' },
    })],
  ])('%s', async (_title, restoreBody) => {
    const connection = forkConnection();
    vi.mocked(sdk.RevertSessionCode).mockResolvedValue(restoreBody);
    const { target } = await mountApp(connection);
    await openOwnedFork(target, true);
    await settle();
    await settle();
    expect(vi.mocked(sdk.ForkSession)).not.toHaveBeenCalled();
    expect(target.textContent).toContain('Rewind outcome unknown');
  });
});

describe('editor read supersession', () => {
  it('ignores a superseded provider detail read after switching providers', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    const prov1 = providerFixture();
    const prov2 = providerFixture({ id: 'prov2', name: 'Prov Two' });
    vi.mocked(sdk.ListProviders).mockResolvedValue(json(200, {
      configuration_revision: { instance_id: INSTANCE, generation: '1' },
      providers: [prov1, prov2],
    }));
    const { promise: first, resolve: releaseFirst } = Promise.withResolvers();
    vi.mocked(sdk.GetProviderDetail).mockImplementation(async (params: string) => {
      const id = (JSON.parse(params) as { query: { provider_id: string } }).query.provider_id;
      if (id === 'prov1') return first as never;
      return json(200, { configuration_revision: { instance_id: INSTANCE, generation: '1' }, provider: prov2 });
    });
    const { target } = await mountApp(connection);
    await openSettings(target);
    await openSettings(target);
    (target.querySelector('[aria-label="Providers"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Select provider prov1"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Select provider prov2"]') as HTMLElement).click();
    await settle();
    await settle();
    releaseFirst(native(200, JSON.stringify({ configuration_revision: { instance_id: INSTANCE, generation: '1' }, provider: prov1 }), false));
    await settle();
    await settle();
    expect((target.querySelector('[aria-label="provider name"]') as HTMLInputElement).value).toBe('Prov Two');
  });
});

describe('read lifetime boundaries', () => {
  // Switching the root while another surface's read is parked moves the
  // shared selection stamp without starting a replacement read there.
  async function selectRoot2ByLabel(target: HTMLElement) {
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    await settle();
    (target.querySelector('[aria-label="Select session root2"]') as HTMLElement).click();
    await settle();
  }

  it('releases its own viewer loading when a superseded read settles without a newer read', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    const { promise: read, resolve: releaseRead } = Promise.withResolvers();
    vi.mocked(sdk.ReadWorkspaceFile).mockImplementationOnce(() => read as never);
    const { target } = await mountApp(connection);
    await openSettings(target);
    (target.querySelector('[aria-label="File viewer"]') as HTMLElement).click();
    await settle();
    await changeInput(target, 'File path', 'a.ts');
    (target.querySelector('[aria-label="Open file"]') as HTMLElement).click();
    await settle();
    expect(target.textContent).toContain('Opening…');
    // The selection moves; no newer viewer read starts.
    await selectRoot2ByLabel(target);
    releaseRead(json(200, { content: 'late body', canonical_path: '/ws/a.ts', truncated: false }));
    await settle();
    await settle();
    expect(target.textContent).not.toContain('late body');
    expect(target.textContent).not.toContain('Opening…');
  });

  it('lets no older same-stamp viewer reply publish over or clear a newer read', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    const { promise: first, resolve: releaseFirst } = Promise.withResolvers();
    const { promise: second, resolve: releaseSecond } = Promise.withResolvers();
    vi.mocked(sdk.ReadWorkspaceFile)
      .mockImplementationOnce(() => first as never)
      .mockImplementationOnce(() => second as never);
    const { target } = await mountApp(connection);
    await openSettings(target);
    (target.querySelector('[aria-label="File viewer"]') as HTMLElement).click();
    await settle();
    await changeInput(target, 'File path', 'a.ts');
    (target.querySelector('[aria-label="Open file"]') as HTMLElement).click();
    await settle();
    await changeInput(target, 'File path', 'b.ts');
    (target.querySelector('[aria-label="Open file"]') as HTMLElement).click();
    await settle();
    // The newer read owns the loading state.
    expect(target.textContent).toContain('Opening…');
    releaseFirst(json(200, { content: 'stale body', canonical_path: '/ws/a.ts', truncated: false }));
    await settle();
    await settle();
    expect(target.textContent).not.toContain('stale body');
    expect(target.textContent).toContain('Opening…');
    releaseSecond(json(200, { content: 'fresh body', canonical_path: '/ws/b.ts', truncated: false }));
    await settle();
    await settle();
    expect(target.textContent).toContain('fresh body');
    expect(target.textContent).not.toContain('Opening…');
  });

  it('a superseded settings read publishes nothing and a reopen reads fresh', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    const { promise: textRead, resolve: releaseText } = Promise.withResolvers();
    let configCalls = 0;
    vi.mocked(sdk.GetConfiguration).mockImplementation(async () => {
      configCalls += 1;
      // The boot global read is the first call; the editor's exact read is the
      // parked second call, and a later editor read serves the fresh document.
      if (configCalls === 2) return textRead as never;
      if (configCalls > 2) return native(200, RAW_CONFIGURATION, false);
      return json(200, {
        configuration_revision: { instance_id: INSTANCE, generation: '1' },
        settings: { sessions: { auto_archive: false, archive_after_days: 1, delete_after_archive_days: 1 }, plugins: {} },
        agents: [], providers: [],
      });
    });
    const { target } = await mountApp(connection);
    (target.querySelector('[aria-label="Settings"]') as HTMLElement).click();
    await settle();
    expect(target.textContent).toContain('Loading…');
    // The selection move retires the editor; the stale body publishes nothing.
    await selectRoot2ByLabel(target);
    releaseText(native(200, RAW_CONFIGURATION, false));
    await settle();
    await settle();
    expect(target.querySelector('[aria-label="Close settings"]')).toBeNull();
    expect(target.textContent).not.toContain('Uses plugin defaults');
    // A reopen is a fresh read, not the superseded body.
    (target.querySelector('[aria-label="Settings"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(target.textContent).toContain('Uses plugin defaults');
  });

  it('a reopened model dialog discards an older same-stamp read and keeps the newer loading', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    const { promise: first, resolve: releaseFirst } = Promise.withResolvers();
    const { promise: second, resolve: releaseSecond } = Promise.withResolvers();
    vi.mocked(sdk.ListModels)
      .mockImplementationOnce(() => first as never)
      .mockImplementationOnce(() => second as never);
    const { target } = await mountApp(connection);
    const model = (ref: string, name: string) => ({
      ref, provider: 'prov', provider_name: 'Provider', model: ref, display_name: name,
      context_window: 1, max_output_tokens: 1, hidden: false, provider_hidden: false, incomplete: false, source: 'user',
    });
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'm', ctrlKey: true }));
    await settle();
    expect(target.textContent).toContain('Loading models...');
    // Close and reopen while the first read is still parked.
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }));
    await settle();
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 'm', ctrlKey: true }));
    await settle();
    releaseFirst(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '1' }, models: [model('prov/a', 'Model A')] }));
    await settle();
    await settle();
    // The older read neither populated the reopened dialog nor cleared its loading.
    expect(target.textContent).toContain('Loading models...');
    releaseSecond(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '1' }, models: [model('prov/b', 'Model B')] }));
    await settle();
    await settle();
    expect(target.textContent).not.toContain('Loading models...');
    (target.querySelector('.mp-btn') as HTMLElement).click();
    await settle();
    const items = [...target.querySelectorAll('.mp .item')].map((item) => item.textContent);
    expect(items.some((text) => text?.includes('Model B'))).toBe(true);
    expect(items.some((text) => text?.includes('Model A'))).toBe(false);
  });
});

// --- First-review correction regressions: settings edit tail and documents ----

const EDITED_SESSIONS_TEXT = '{"sessions":{"auto_archive":false,"archive_after_days":5,"delete_after_archive_days":1},'
  + '"plugins":{"tools":"{\\"max_output_bytes\\":9007199254740993}"}}';

function settingsRawConnection(raw = RAW_CONFIGURATION) {
  const connection = fakeConnection();
  basicMocks(connection);
  // The one raw document serves the global read and the editor's exact read.
  vi.mocked(sdk.GetConfiguration).mockImplementation(async () => native(200, raw, false));
  vi.mocked(sdk.UpdateConfigurationSettings).mockResolvedValue(json(200, {
    configuration_revision: { instance_id: INSTANCE, generation: '2' },
    result: { sessions: { auto_archive: false, archive_after_days: 1, delete_after_archive_days: 1 }, plugins: {} },
  }));
  return connection;
}

describe('settings local edit tail', () => {
  it('applies each local edit synchronously and keeps both in the saved complete body', async () => {
    const connection = settingsRawConnection();
    const { target } = await mountApp(connection);
    await openSettings(target);
    await changeInput(target, 'archive_after_days', '5');
    expect((target.querySelector('[aria-label="archive_after_days"]') as HTMLInputElement).value).toBe('5');
    // No native edit state is entered: the controls stay live.
    expect((target.querySelector('[aria-label="delete_after_archive_days"]') as HTMLInputElement).disabled).toBe(false);
    expect((target.querySelector('[aria-label="Save settings"]') as HTMLButtonElement).disabled).toBe(false);
    await changeInput(target, 'delete_after_archive_days', '7');
    expect((target.querySelector('[aria-label="delete_after_archive_days"]') as HTMLInputElement).value).toBe('7');
    (target.querySelector('[aria-label="Save settings"]') as HTMLElement).click();
    await settle();
    await settle();
    const call = vi.mocked(sdk.UpdateConfigurationSettings).mock.calls[0][0] as string;
    expect(bodyOf(call)).toContain('"archive_after_days":5');
    expect(bodyOf(call)).toContain('"delete_after_archive_days":7');
  });

  it('keeps every unedited numeric lexeme of a local plugin edit exact', async () => {
    const connection = settingsRawConnection();
    const { target } = await mountApp(connection);
    await openSettings(target);
    await changeInput(target, 'jobs max_background_processes', '3');
    (target.querySelector('[aria-label="Save settings"]') as HTMLElement).click();
    await settle();
    await settle();
    const body = bodyOf(vi.mocked(sdk.UpdateConfigurationSettings).mock.calls[0][0] as never);
    expect(body).toContain('"archive_after_days":-9223372036854775808');
    expect(body).toContain('"tools":"{\\"max_output_bytes\\":9007199254740993}"');
    expect(body).toContain('"jobs":"{\\"max_background_processes\\":3}"');
  });

  it('a rejected local edit shows its error, leaves the document unchanged and a later edit recovers', async () => {
    const connection = settingsRawConnection();
    const { target } = await mountApp(connection);
    await openSettings(target);
    // A malformed composed member is a local syntax rejection with no Runtime
    // payload: the exact document is untouched.
    await changeInput(target, 'archive_after_days', '5x');
    await settle();
    expect(target.textContent).toContain('Edit rejected');
    (target.querySelector('[aria-label="Save settings"]') as HTMLElement).click();
    await settle();
    await settle();
    const first = bodyOf(vi.mocked(sdk.UpdateConfigurationSettings).mock.calls[0][0] as never);
    expect(first).toContain('"archive_after_days":-9223372036854775808');
    expect(first).not.toContain('5x');
    // A later explicit valid edit recovers against the last valid document.
    await changeInput(target, 'delete_after_archive_days', '7');
    (target.querySelector('[aria-label="Save settings"]') as HTMLElement).click();
    await settle();
    await settle();
    const body = bodyOf(vi.mocked(sdk.UpdateConfigurationSettings).mock.calls.at(-1)?.[0] as never);
    expect(body).toContain('"delete_after_archive_days":7');
    expect(body).toContain('"archive_after_days":-9223372036854775808');
  });
});

// --- First-review correction regressions: snapshots, repair, clocks, cost ----

describe('loaded snapshot identity', () => {
  it('a selection move retires the snapshots dialog and a reopen reads the new session', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetSessionCodeSnapshots).mockResolvedValue(json(200, [
      { operation_id: 'op1', group_item_id: 'g1', files: [{ path: 'a.ts', existed: true }] },
    ]));
    const { target } = await mountApp(connection);
    await openSnapshots(target);
    await settle();
    await settle();
    expect(target.textContent).toContain('loaded root1');
    // One active first-party dialog: the move retires the snapshots dialog and
    // no revert action remains for the previously loaded identity.
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    await settle();
    (target.querySelectorAll('dialog .row')[1] as HTMLElement).click();
    await settle();
    expect(target.querySelector('[aria-label="Revert code to before operation op1"]')).toBeNull();
    expect(vi.mocked(sdk.RevertSessionCode)).not.toHaveBeenCalled();
    // A reopen reads the currently selected session through the plain task.
    await openSnapshots(target);
    await settle();
    await settle();
    expect(vi.mocked(sdk.GetSessionCodeSnapshots).mock.calls.at(-1)?.[0]).toEqual('{"session_id":"root2"}');
  });


});

describe('workspace and empty-selection repair', () => {
  it('a repair preserves the selected Workspace and title', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    const { target } = await mountApp(connection);
    connection.openMock.mockResolvedValue({ instance_id: INSTANCE, generation: '2', workspace: '/other' });
    connection.failureHandler?.(new Error('lost'), '1');
    await vi.waitFor(() => {
      expect(vi.mocked(sdk.GetConfiguration).mock.calls.length).toBeGreaterThan(1);
    });
    await settle();
    await settle();
    expect(target.textContent).toContain('/ws');
    expect(target.textContent).not.toContain('/other');
    expect(vi.mocked(SetTitle).mock.calls.at(-1)?.[0]).toBe('/ws');
  });

  it('a repair does not recompose an intentionally empty selection', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ArchiveSession).mockResolvedValue(json(200, session('root1', NEWER)));
    vi.mocked(sdk.DeleteSession).mockResolvedValue(native(204, '', false));
    const { target } = await mountApp(connection);
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    await settle();
    (target.querySelector('[aria-label="Delete session root1"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(target.textContent).toContain('no session');
    const createsBefore = vi.mocked(sdk.CreateSession).mock.calls.length;
    const listsBefore = vi.mocked(sdk.ListSessions).mock.calls.length;
    connection.openMock.mockResolvedValue({ instance_id: INSTANCE, generation: '2', workspace: '/other' });
    connection.failureHandler?.(new Error('lost'), '1');
    await vi.waitFor(() => {
      expect(vi.mocked(sdk.GetConfiguration).mock.calls.length).toBeGreaterThan(1);
    });
    await settle();
    await settle();
    expect(vi.mocked(sdk.CreateSession).mock.calls.length).toBe(createsBefore);
    expect(vi.mocked(sdk.ListSessions).mock.calls.length).toBe(listsBefore);
    expect(target.textContent).toContain('no session');
    expect(target.textContent).not.toContain('Delete outcome unknown');
  });
});

describe('session mutation commits', () => {
  it('an archive success clears the selected root even when Delete then fails', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ArchiveSession).mockResolvedValue(json(200, session('root1', NEWER)));
    vi.mocked(sdk.DeleteSession).mockResolvedValue(json(500, { code: 'internal', message: 'delete blew up' }));
    const { target } = await mountApp(connection);
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    await settle();
    (target.querySelector('[aria-label="Delete session root1"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(vi.mocked(sdk.DeleteSession)).toHaveBeenCalledWith('{"session_id":"root1"}');
    expect(target.textContent).toContain('no session');
    expect(target.textContent).toContain('delete blew up');
  });

  it('a stale archive completion cannot clear a newer selection', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    const { promise: pending, resolve: releaseArchive } = Promise.withResolvers();
    vi.mocked(sdk.ArchiveSession).mockImplementationOnce(() => pending as never);
    vi.mocked(sdk.DeleteSession).mockResolvedValue(native(204, '', false));
    const { target } = await mountApp(connection);
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    await settle();
    (target.querySelector('[aria-label="Delete session root1"]') as HTMLElement).click();
    await settle();
    // Move to root2 while the archive of root1 is parked: the first Ctrl+S
    // closes the still-open picker, the second reopens a fresh list.
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    await settle();
    (target.querySelectorAll('dialog .row')[1] as HTMLElement).click();
    await settle();
    releaseArchive(json(200, session('root1', NEWER)));
    await settle();
    await settle();
    expect(target.textContent).toContain('root2');
    expect(target.textContent).not.toContain('no session');
    expect(target.textContent).not.toContain('Delete outcome unknown');
  });
});

describe('global snapshot adoption', () => {
  const OTHER_AGENT = { name: 'other-agent', description: '', subagent: false, model: '', readonly: false, write_dir: '', tools: [], capabilities: [] };

  it.each([
    {
      title: 'never publishes a stale configuration payload with its older clock',
      readGeneration: '0',
      visible: 'primary',
      absent: 'other-agent',
    },
    {
      title: 'adopts an equal-clock configuration payload',
      readGeneration: '1',
      visible: 'other-agent',
      absent: null,
    },
  ])('$title', async ({ readGeneration, visible, absent }) => {
    const connection = fakeConnection();
    basicMocks(connection);
    const { target } = await mountApp(connection);
    vi.mocked(sdk.GetConfiguration).mockResolvedValue(json(200, {
      configuration_revision: { instance_id: INSTANCE, generation: readGeneration },
      settings: { sessions: { auto_archive: false, archive_after_days: 1, delete_after_archive_days: 1 }, plugins: {} },
      agents: [OTHER_AGENT],
      providers: [],
    }));
    deliverEvent(connection, {
      kind: 'configuration_changed',
      configuration_revision: { instance_id: INSTANCE, generation: '1' },
    });
    await settle();
    await settle();
    await openSettings(target);
    (target.querySelector('[aria-label="Agent type"]') as HTMLElement).click();
    await settle();
    expect(target.textContent).toContain(visible);
    if (absent !== null) expect(target.textContent).not.toContain(absent);
  });

  it('never publishes a stale warnings payload with its older clock', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetWarnings).mockResolvedValue(json(200, {
      warnings_revision: { instance_id: INSTANCE, revision: '2' },
      warnings: [{ source: 'g', kind: 'k', message: 'g-old' }],
    }));
    vi.mocked(sdk.ArchiveSession).mockResolvedValue(json(200, session('root1', NEWER)));
    vi.mocked(sdk.DeleteSession).mockResolvedValue(native(204, '', false));
    // The displayed hydration must clear the accepted warnings floor ('2');
    // the fixture carries the matching revision so the reader is not held
    // dirty by an inconsistent mock.
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, hydration({
      warnings_revision: { instance_id: INSTANCE, revision: '2' },
    })));
    const { target } = await mountApp(connection);
    vi.mocked(sdk.GetWarnings).mockResolvedValue(json(200, {
      warnings_revision: { instance_id: INSTANCE, revision: '1' },
      warnings: [{ source: 'g', kind: 'k', message: 'g-stale' }],
    }));
    deliverEvent(connection, {
      kind: 'warning_changed',
      warnings_revision: { instance_id: INSTANCE, revision: '2' },
    });
    await settle();
    await settle();
    // Remove the selected root so the global-only fallback is displayed.
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    await settle();
    (target.querySelector('[aria-label="Delete session root1"]') as HTMLElement).click();
    await settle();
    await settle();
    (target.querySelector('.warn') as HTMLElement).click();
    await settle();
    expect(target.textContent).toContain('g-old');
    expect(target.textContent).not.toContain('g-stale');
    expect(target.textContent).not.toContain('Delete outcome unknown');
  });
});

describe('cost whole-member replacement', () => {
  async function costEditor(model: Record<string, unknown>) {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ListProviders).mockResolvedValue(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '1' }, providers: [providerFixture({ models: [model] })] }));
    vi.mocked(sdk.GetProviderDetail).mockResolvedValue(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '1' }, provider: providerFixture({ models: [model] }) }));
    vi.mocked(sdk.UpdateProviderModel).mockResolvedValue(json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: model as never }));
    const { target } = await mountApp(connection);
    await openSettings(target);
    (target.querySelector('[aria-label="Providers"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Select provider prov1"]') as HTMLElement).click();
    await settle();
    return target;
  }

  it('preserves present siblings, omits absent ones and omits a cleared price', async () => {
    // Absent siblings are omitted: the first read carries input only.
    const single = modelFixture({ cost: { input: 1 } });
    const first = await costEditor(single);
    await changeInput(first, 'm1 cost input', '2');
    await settle();
    await settle();
    let call = vi.mocked(sdk.UpdateProviderModel).mock.calls[0][0] as string;
    expect(bodyOf(call)).toBe('{"model":{"cost":{"input":2}}}');
    // Present zero survives and only the cost top-level field is sent. The
    // exact read serves a fresh document per call, so this editor reads the
    // zero-bearing cost from its own detail read.
    const both = modelFixture({ cost: { input: 0, output: 5 } });
    const second = await costEditor(both);
    await changeInput(second, 'm1 cost output', '6');
    await settle();
    await settle();
    call = vi.mocked(sdk.UpdateProviderModel).mock.calls.at(-1)?.[0] as string;
    expect(bodyOf(call)).toBe('{"model":{"cost":{"input":0,"output":6}}}');
    // A cleared price is omitted, never manufactured as null.
    await changeInput(second, 'm1 cost input', '');
    await settle();
    await settle();
    call = vi.mocked(sdk.UpdateProviderModel).mock.calls.at(-1)?.[0] as string;
    expect(bodyOf(call)).toBe('{"model":{"cost":{"output":5}}}');
  });
});

describe('selection-bound Fork source', () => {
  it('an in-flight Fork cannot pair its captured item with another live root', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.GetSessionHydration).mockResolvedValue(json(200, hydration({
      conversation: { items: [inputItem('i-owned', 'user', 'op-owned')] },
      operations: [runningOperation('op-owned')],
    })));
    const { promise: forkGate, resolve: releaseFork } = Promise.withResolvers();
    vi.mocked(sdk.ForkSession).mockImplementationOnce(() => forkGate as never);
    const { target } = await mountApp(connection);
    (target.querySelector('[aria-label="Fork from input i-owned"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Submit fork"]') as HTMLElement).click();
    await settle();
    // Move to root2 while the Fork is in flight; the one-modal owner retires
    // the form but the mutation keeps its captured source Session.
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    await settle();
    (target.querySelectorAll('dialog .row')[1] as HTMLElement).click();
    await settle();
    expect(target.querySelector('[aria-label="Close fork dialog"]')).toBeNull();
    releaseFork(json(200, { session: session('dest1', NEWER), operation: runningOperation('x') }));
    await settle();
    await settle();
    const call = JSON.parse(vi.mocked(sdk.ForkSession).mock.calls[0][0]) as { session_id: string };
    expect(call.session_id).toBe('root1');
    expect(target.textContent).toContain('selection moved');
  });

  it('a Workspace change retires the open Session picker and its rows', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ListWorkspaces).mockResolvedValue(json(200, [
      { root: '/ws', display_name: 'ws' },
      { root: '/ws2', display_name: 'ws2' },
    ]));
    const { target } = await mountApp(connection);
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    await settle();
    expect(target.querySelector('#session-picker-title')).not.toBeNull();
    (target.querySelector('[aria-label="Select workspace"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Select workspace /ws2"]') as HTMLElement).click();
    await settle();
    await settle();
    expect(target.querySelector('#session-picker-title')).toBeNull();
    expect(target.querySelector('[aria-label="Select session root1"]')).toBeNull();
  });
});

// --- Second correction pass regressions: per-workspace Create uncertainty, uniform archive, discovery token, busy inputs, edit tail authority ----

describe('composition failure outcomes', () => {
  function compositionMocks(connection: ReturnType<typeof fakeConnection>, failure: 'known' | 'uncertain') {
    basicMocks(connection);
    vi.mocked(sdk.ListSessions).mockResolvedValue(json(200, []));
    if (failure === 'known') {
      vi.mocked(sdk.CreateSession).mockResolvedValueOnce(json(500, { code: 'internal', message: 'create rejected' }));
    } else {
      vi.mocked(sdk.CreateSession).mockRejectedValueOnce(new Error('create lost'));
    }
  }

  for (const failure of ['known', 'uncertain'] as const) {
    it(`a ${failure} composition Create ends the attempt visibly and a fresh explicit New works`, async () => {
      const connection = fakeConnection();
      compositionMocks(connection, failure);
      const { target } = await mountAppNoWait(connection);
      await vi.waitFor(() => expect(vi.mocked(sdk.CreateSession)).toHaveBeenCalledTimes(1));
      await settle();
      if (failure === 'known') {
        expect(target.querySelector('.note.error')?.textContent).toContain('Session creation failed');
      } else {
        const note = target.querySelector('.note.outcome')?.textContent ?? '';
        expect(note).toContain('Session create outcome unknown');
        expect(note).toContain('/ws');
        expect(note).toContain('not retransmitted');
      }
      expect(target.textContent).toContain('no session');
      // A fresh explicit New creates and adopts its destination.
      vi.mocked(sdk.CreateSession).mockResolvedValue(json(200, session('created-1', NEWER)));
      vi.mocked(sdk.GetSessionHydration).mockImplementation(async (options) => {
        const id = (JSON.parse(options) as { session_id: string }).session_id;
        return json(200, hydration({ session: session(id, NEWER) }));
      });
      window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
      await settle();
      await settle();
      (target.querySelector('[aria-label="New session"]') as HTMLElement).click();
      await vi.waitFor(() => expect(vi.mocked(sdk.CreateSession)).toHaveBeenCalledTimes(2));
      await settle();
      await settle();
      expect(vi.mocked(sdk.GetSessionHydration)).toHaveBeenCalledWith('{"session_id":"created-1"}');
      expect(target.textContent).toContain('created-1');
    });
  }

  for (const initial of ['accepted', 'lost'] as const) {
    it(`a stale ${initial} initial completion never adopts or publishes and the picker reports its captured address`, async () => {
      const connection = fakeConnection();
      basicMocks(connection);
      vi.mocked(sdk.ListSessions).mockResolvedValue(json(200, []));
      vi.mocked(sdk.ListWorkspaces).mockResolvedValue(json(200, [
        { root: '/wsB', display_name: 'B' },
      ]));
      const { promise: initialGate, resolve: releaseInitial } = Promise.withResolvers();
      const { promise: pickerGate, reject: releasePicker } = Promise.withResolvers();
      vi.mocked(sdk.CreateSession)
        .mockImplementationOnce(() => initialGate as never)
        .mockImplementationOnce(() => pickerGate as never)
        .mockResolvedValue(json(200, session('b-1', NEWER)));
      vi.mocked(sdk.GetSessionHydration).mockImplementation(async (options) => {
        const id = (JSON.parse(options) as { session_id: string }).session_id;
        return json(200, hydration({ session: session(id, NEWER) }));
      });
      const { target } = await mountAppNoWait(connection);
      await vi.waitFor(() => expect(vi.mocked(sdk.CreateSession)).toHaveBeenCalledTimes(1));
      // The picker's explicit New parks its own Create against the captured
      // workspace.
      window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
      await settle();
      await settle();
      (target.querySelector('[aria-label="New session"]') as HTMLElement).click();
      await vi.waitFor(() => expect(vi.mocked(sdk.CreateSession)).toHaveBeenCalledTimes(2));
      // B is chosen; its composition waits behind the parked initial Create.
      (target.querySelector('[aria-label="Select workspace"]') as HTMLElement).click();
      await settle();
      (target.querySelector('[aria-label="Select workspace /wsB"]') as HTMLElement).click();
      await settle();
      releaseInitial(initial === 'accepted' ? json(200, session('stale-1', NEWER)) : new Error('stale initial lost'));
      await settle();
      await settle();
      // The old initial completion neither adopts nor publishes a note.
      expect(target.textContent).not.toContain('stale-1');
      expect(target.querySelector('.note.outcome')).toBeNull();
      expect(target.querySelector('.note.error')).toBeNull();
      const workspaces = vi.mocked(sdk.CreateSession).mock.calls.map(
        (call) => (JSON.parse(call[0] as string) as { body: { workspace: string } }).body.workspace,
      );
      expect(workspaces).toEqual(['/ws', '/ws', '/wsB']);
      expect(target.textContent).toContain('b-1');
      // The stale picker outcome reports against its captured address and never
      // adopts over the newer selection.
      releasePicker(new Error('stale picker create lost'));
      await settle();
      await settle();
      expect(target.querySelector('.note.outcome')?.textContent).toContain('/ws');
      expect(target.textContent).toContain('b-1');
      expect(target.textContent).not.toContain('stale-1');
    });
  }
});

describe('uniform archive→delete for both lifecycles', () => {
  it('archived root delete also goes through archive first (no-write success)', async () => {
    const connection = fakeConnection();
    basicMocks(connection);
    vi.mocked(sdk.ListSessions).mockImplementation(async (options) => {
      const lifecycle = (JSON.parse(options) as { query: { lifecycle: string } }).query.lifecycle;
      if (lifecycle === 'open') return json(200, [session('root1', NEWER)]);
      return json(200, [{ ...session('arch1', OLDER), lifecycle: 'archived' as const }]);
    });
    vi.mocked(sdk.ArchiveSession).mockImplementation(async () => json(200, session('arch1', OLDER)));
    vi.mocked(sdk.DeleteSession).mockImplementation(async () => native(204, '', false));
    const { target } = await mountApp(connection);
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    // Switch to Archived tab.
    const archivedTab = target.querySelector('[aria-label="Archived sessions"]') as HTMLElement;
    expect(archivedTab).not.toBeNull();
    archivedTab.click();
    await settle();
    // Delete the archived root: archive is called first (no-write success), then delete.
    const deleteBtn = target.querySelector('[aria-label="Delete session arch1"]') as HTMLElement;
    expect(deleteBtn).not.toBeNull();
    deleteBtn.click();
    await settle();
    await settle();
    expect(vi.mocked(sdk.ArchiveSession)).toHaveBeenCalledWith('{"session_id":"arch1"}');
    expect(vi.mocked(sdk.DeleteSession)).toHaveBeenCalledWith('{"session_id":"arch1"}');
  });
});
