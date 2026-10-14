// @vitest-environment happy-dom
// The real-stack mounted owner: the actual RuntimeConnection and the shared
// native invocation boundary drive the mounted App over MOCKED GENERATED HOST
// BINDINGS ONLY. This is where the actual lossless call contract is proven:
// native rejection is loss, the invalid bit is invalid delivery, and a received
// status is otherwise a classified result. Nothing here mocks the invocation
// boundary; the scripted bridge stands in for the native host's Call binding.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { mount, tick, unmount } from 'svelte';

vi.mock('../../client-bindings/wailsjs/go/desktopclient/Host', () => ({
  AppVersion: vi.fn(),
  Call: vi.fn(),
  NextEvent: vi.fn(),
  Open: vi.fn(),
  PickDirectory: vi.fn(),
  SetTitle: vi.fn(),
}));

import { AppVersion, Call, Open, NextEvent, PickDirectory, SetTitle } from '../../client-bindings/wailsjs/go/desktopclient/Host';
import type { desktopclient } from '../../client-bindings/wailsjs/go/models';
import { RuntimeConnection } from './connection';
import { extractSettingsText } from './exactjson';
import { invokeNative } from './results';

const INSTANCE = '0123456789abcdef0123456789abcdef';
const NEWER = '2026-10-02T10:00:00Z';
const OLDER = '2026-10-01T05:00:00Z';
const mounted: Array<ReturnType<typeof mount>> = [];

function session(id: string, lastActivity: string, parent?: string) {
  return {
    session_id: id,
    workspace: '/ws',
    ...(parent ? { parent_session_id: parent } : {}),
    lifecycle: 'open',
    agent_type: 'primary',
    created_at: '0',
    last_activity: lastActivity,
    session_revision: { instance_id: INSTANCE, durable_revision: '1', local_revision: '0' },
  };
}

function hydration(sessionID: string, overrides: Record<string, unknown> = {}) {
  return {
    session: session(sessionID, NEWER),
    selected_model: null,
    session_revision: { instance_id: INSTANCE, durable_revision: '1', local_revision: '0' },
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

function json(status: number, value: unknown): desktopclient.HTTPResponse {
  return native(status, 'application/json', JSON.stringify(value), false);
}

// native supplies one scripted local result exactly as the native owner
// returns it, including the explicit invalid-delivery bit; producer
// validation stays at the Go response owner.
function native(status: number, content_type: string, body: string, invalid: boolean): desktopclient.HTTPResponse {
  return { status, content_type, body, invalid };
}

// routeState is the scripted in-memory Runtime behind the byte bridge.
interface Delayed {
  delay: Promise<{ status: number; body: unknown }>;
}

interface Script {
  submit?: (body: unknown) => { status: number; body: unknown } | 'throw' | Delayed;
  compact?: (body: unknown) => { status: number; body: unknown } | 'throw';
  stop?: () => { status: number; body: unknown } | 'throw' | Delayed;
  events?: string[];
  // Scripted raw hydration responses consumed in order (e.g. an empty 200).
  hydration?: Array<{ status: number; content_type: string; body: string; invalid: boolean }>;
  // Scripted GetConfiguration results consumed in order: each waits for its own
  // gate and then answers with its outcome, so one held global read can span a
  // connection retirement.
  configuration?: Array<{ wait: Promise<void>; outcome: { status: number; content_type: string; body: string; invalid: boolean } | 'throw' }>;
  // A held successor Open: opens after the first await it, retiring the
  // installed connection until the test releases the new info.
  openGate?: Promise<desktopclient.ConnectionInfo>;
  // An exact raw ConfigurationView body, so numeric tokens beyond Number
  // precision reach the frontend exactly as the producer wrote them.
  configurationRaw?: string;
  // An exact raw ProviderDetail body for the provider editor's text read.
  providerDetailRaw?: string;
  // Scripted provider-discovery candidates for the create form.
  discoverCandidates?: unknown[];
  rewind?: (body: unknown) => { status: number; body: unknown } | 'throw';
}

function bridge(script: Script = {}) {
  const calls: Array<{ generation: string; operation: string; params: string }> = [];
  vi.mocked(Call).mockImplementation(async (generation, operation, params) => {
    calls.push({ generation, operation, params });
    const envelope = (params === '' ? {} : JSON.parse(params)) as {
      session_id?: string;
      body?: Record<string, unknown>;
      query?: Record<string, unknown>;
    };
    const body = envelope.body ?? {};
    if (operation === 'ListSessions') {
      return json(200, [session('root1', NEWER), session('root2', OLDER)]);
    }
    if (operation === 'GetSessionHydration') {
      if (script.hydration && script.hydration.length > 0) {
        const raw = script.hydration.shift()!;
        return native(raw.status, raw.content_type, raw.body, raw.invalid);
      }
      return json(200, hydration(envelope.session_id!));
    }
    if (operation === 'GetConfiguration') {
      const scriptedRead = script.configuration?.shift();
      if (scriptedRead) {
        await scriptedRead.wait;
        if (scriptedRead.outcome === 'throw') throw new Error('native bridge: configuration lost');
        return native(scriptedRead.outcome.status, scriptedRead.outcome.content_type, scriptedRead.outcome.body, scriptedRead.outcome.invalid);
      }
      if (script.configurationRaw) {
        return native(200, 'application/json', script.configurationRaw, false);
      }
      return json(200, {
        configuration_revision: { instance_id: INSTANCE, generation: '1' },
        settings: { sessions: { auto_archive: false, archive_after_days: 1, delete_after_archive_days: 1 }, plugins: {} },
        agents: [],
        providers: [],
      });
    }
    if (operation === 'UpdateConfigurationSettings') {
      return json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: { sessions: {}, plugins: {} } });
    }
    if (operation === 'ListProviders') {
      return json(200, {
        configuration_revision: { instance_id: INSTANCE, generation: '1' },
        providers: [{ id: 'prov1', name: 'Prov One', builtin: false, connected: true }],
      });
    }
    if (operation === 'GetProviderDetail') {
      if (script.providerDetailRaw) {
        return native(200, 'application/json', script.providerDetailRaw, false);
      }
      return json(404, { code: 'not_found', message: 'no provider detail fixture' });
    }
    if (operation === 'UpdateProviderDetail' || operation === 'UpdateProviderModel') {
      return json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: null });
    }
    if (operation === 'DiscoverProviderModelCandidates') {
      return json(200, []);
    }
    if (operation === 'DiscoverProviderCandidates') {
      return json(200, script.discoverCandidates ?? []);
    }
    if (operation === 'CreateProvider') {
      return json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: null });
    }
    if (operation === 'RevertSessionCode') {
      if (script.rewind === 'throw') throw new Error('native bridge: rewind lost');
      if (typeof script.rewind === 'function') {
        const outcome = script.rewind(body);
        return json(outcome.status, outcome.body);
      }
      return json(200, { restored: [], skipped: [] });
    }
    if (operation === 'ForkSession') {
      return json(200, {
        session: session('dest1', NEWER),
        operation: { operation_id: body.operation_id, request_kind: 'message', status: 'running', admitted_at: '0', agent_type: 'primary', model: 'prov/m', usage: { totals: { by_model: [] } } },
      });
    }
    if (operation === 'GetWarnings') {
      return json(200, { warnings_revision: { instance_id: INSTANCE, revision: '1' }, warnings: [] });
    }
    if (operation === 'SubmitSession') {
      if (script.submit === 'throw') {
        throw new Error('native bridge: connection lost mid-request');
      }
      if (typeof script.submit === 'function') {
        const outcome = script.submit(body);
        if ('delay' in outcome) {
          const resolved = await outcome.delay;
          if (resolved.status === 0) throw new Error('native bridge: late loss');
          return json(resolved.status, resolved.body);
        }
        return json(outcome.status, outcome.body);
      }
      return json(200, { disposition: 'admitted' });
    }
    if (operation === 'CompactSession') {
      if (script.compact === 'throw' || (typeof script.compact === 'function' && script.compact(body) === 'throw')) {
        throw new Error('native bridge: connection lost mid-request');
      }
      return json(200, { operation_id: body.operation_id, request_kind: 'compact', status: 'running', admitted_at: '0', agent_type: 'primary', model: 'prov/m', usage: { totals: { by_model: [] } } });
    }
    if (operation === 'InterruptSession') {
      return native(204, '', '', false);
    }
    if (operation === 'StopSession' && script.stop) {
      const outcome = script.stop();
      if (outcome === 'throw') throw new Error('native bridge: stop lost');
      if ('delay' in outcome) {
        const resolved = await outcome.delay;
        if (resolved.status === 0) throw new Error('native bridge: stop lost');
        return json(resolved.status, resolved.body);
      }
      return json(outcome.status, outcome.body);
    }
    if (operation === 'StopSession') {
      return native(204, '', '', false);
    }
    return json(404, { code: 'not_found', message: 'no fixture for ' + operation });
  });
  let opens = 0;
  vi.mocked(Open).mockImplementation(async () => {
    opens += 1;
    if (script.openGate !== undefined && opens > 1) {
      return script.openGate;
    }
    return { instance_id: INSTANCE, generation: '1', workspace: '/ws' };
  });
  vi.mocked(SetTitle).mockResolvedValue(undefined);
  vi.mocked(AppVersion).mockResolvedValue('dev (test)');
  vi.mocked(PickDirectory).mockResolvedValue('');
  const events = script.events ?? [];
  let pullIndex = 0;
  vi.mocked(NextEvent).mockImplementation(async () => {
    if (pullIndex < events.length) {
      // Real hints arrive after composition settled the initial read; the
      // scripted queue yields one macrotask before each delivery.
      await new Promise((resolve) => setTimeout(resolve, 5));
      return events[pullIndex++];
    }
    return new Promise<string>(() => {});
  });
  return calls;
}

async function mountReal(script: Script = {}) {
  const calls = bridge(script);
  const { default: App } = await import('./App.svelte');
  const connection = new RuntimeConnection();
  const target = document.createElement('div');
  document.body.appendChild(target);
  const component = mount(App, { target, props: { connection } });
  mounted.push(component);
  await vi.waitFor(() => {
    expect(calls.some((call) => call.operation === 'GetSessionHydration')).toBe(true);
  });
  await tick();
  return { connection, component, target, calls };
}

// One complete ConfigurationView for the actual global read.
function configurationView(generation: string, agentName: string) {
  return {
    configuration_revision: { instance_id: INSTANCE, generation },
    settings: { sessions: { auto_archive: false, archive_after_days: 1, delete_after_archive_days: 1 }, plugins: {} },
    agents: [{ name: agentName, description: agentName, subagent: false, model: '', readonly: false, write_dir: '', tools: [], capabilities: [] }],
    providers: [],
  };
}

function rawJSONResponse(value: unknown) {
  return { status: 200, content_type: 'application/json', body: JSON.stringify(value), invalid: false };
}

function typeInto(target: HTMLElement, text: string) {
  const textarea = target.querySelector('textarea') as HTMLTextAreaElement;
  textarea.value = text;
  textarea.dispatchEvent(new Event('input', { bubbles: true }));
}

function changeInput(target: HTMLElement, label: string, value: string) {
  const input = target.querySelector(`[aria-label="${label}"]`) as HTMLInputElement;
  input.value = value;
  input.dispatchEvent(new Event('change', { bubbles: true }));
}

async function settle() {
  await new Promise((resolve) => setTimeout(resolve, 0));
  await tick();
}

afterEach(async () => {
  for (const component of mounted.splice(0)) unmount(component);
  document.body.innerHTML = '';
  vi.clearAllMocks();
});

describe('real generated stack over the byte bridge', () => {
  it('composes the initial view through the shared invocation envelopes', async () => {
    const { target, calls } = await mountReal();
    expect(calls.some((call) => call.operation === 'ListSessions' && call.params === '{"query":{"workspace":"/ws","lifecycle":"open"}}')).toBe(true);
    expect(calls.some((call) => call.operation === 'GetConfiguration')).toBe(true);
    expect(calls.some((call) => call.operation === 'GetWarnings')).toBe(true);
    const hydrationCall = calls.find((call) => call.operation === 'GetSessionHydration');
    expect(hydrationCall?.params).toBe('{"session_id":"root1"}');
    expect(hydrationCall?.generation).toBe('1');
    expect(target.textContent).toContain('root1');
  });

  it('a real native Call rejection is a transport loss, not a server rejection', async () => {
    const { target } = await mountReal({ submit: 'throw' });
    typeInto(target, 'lost message');
    await settle();
    (target.querySelector('.send') as HTMLElement).click();
    await settle();
    await settle();
    // The binding rejects; the consumer must show Outcome unknown, never a
    // generated protocol error.
    expect(target.querySelector('.attempt.unknown')?.textContent).toContain('Outcome unknown');
    expect(target.querySelector('.attempt.rejected')).toBeNull();
  });

  it('a real 400 is a known rejection that keeps the draft and reports the generated error', async () => {
    const { target } = await mountReal({
      submit: () => ({ status: 400, body: { code: 'invalid', message: 'content is not admitted' } }),
    });
    typeInto(target, 'bad message');
    await settle();
    (target.querySelector('.send') as HTMLElement).click();
    await settle();
    await settle();
    expect(target.querySelector('.attempt.rejected')?.textContent).toContain('content is not admitted');
    expect((target.querySelector('textarea') as HTMLTextAreaElement).value).toBe('bad message');
    expect(target.querySelector('.attempt.unknown')).toBeNull();
  });

  it('a real 200 disposition clears the matching draft and retires the attempt', async () => {
    const { target, calls } = await mountReal();
    typeInto(target, 'delivered text');
    await settle();
    (target.querySelector('.send') as HTMLElement).click();
    await settle();
    await settle();
    const submit = calls.find((call) => call.operation === 'SubmitSession');
    expect(submit).toBeDefined();
    const body = (JSON.parse(submit!.params) as { body: { operation_id: string; mode: string; content: unknown[] } }).body;
    expect(body.operation_id).toMatch(/^[0-9a-f]{32}$/);
    expect(body.mode).toBe('regular');
    expect(body.content).toEqual([{ kind: 'text', text: 'delivered text' }]);
    expect((target.querySelector('textarea') as HTMLTextAreaElement).value).toBe('');
    expect(target.querySelectorAll('.attempt')).toHaveLength(0);
  });

  it('a delayed loss across a root selection records unknown against the captured session and resends nothing', async () => {
    let release: () => void = () => {};
    const gate = new Promise<{ status: number; body: unknown }>((resolve) => { release = () => resolve({ status: 0, body: undefined }); });
    const { target, calls } = await mountReal({
      submit: () => ({ delay: gate }),
    });
    typeInto(target, 'in flight');
    await settle();
    (target.querySelector('.send') as HTMLElement).click();
    await settle();
    // Move to root2 while the submit is in flight.
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    const rows = target.querySelectorAll('dialog .row');
    (rows[1] as HTMLElement).click();
    await settle();
    release();
    await settle();
    await settle();
    // Exactly one submit ever left the bridge.
    expect(calls.filter((call) => call.operation === 'SubmitSession')).toHaveLength(1);
    // Back on root1 the attempt is retained as unknown, not silently dropped.
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    const backRows = target.querySelectorAll('dialog .row');
    (backRows[0] as HTMLElement).click();
    await settle();
    expect(target.querySelector('.attempt.unknown')?.textContent).toContain('Outcome unknown');
  });

  it('Compact uses the same shared loss classification', async () => {
    const { target, calls } = await mountReal({ compact: () => 'throw' });
    (target.querySelector('.composer .compact') as HTMLElement).click();
    await settle();
    await settle();
    const compactCall = calls.find((call) => call.operation === 'CompactSession');
    expect(compactCall).toBeDefined();
    expect((JSON.parse(compactCall!.params) as { body: { operation_id: string } }).body.operation_id).toMatch(/^[0-9a-f]{32}$/);
    expect(target.querySelector('.attempt.unknown')?.textContent).toContain('Outcome unknown');
    expect(target.querySelector('.attempt.rejected')).toBeNull();
  });

  it('an addressed control still reports its generated failure after the selection moved', async () => {
    let release: () => void = () => {};
    const gate = new Promise<{ status: number; body: unknown }>((resolve) => { release = () => resolve({ status: 500, body: { code: 'internal', message: 'stop blew up' } }); });
    const { target } = await mountReal({ stop: () => ({ delay: gate }) });
    (target.querySelector('.stop') as HTMLElement).click();
    await settle();
    // Move the selection while the control is in flight.
    window.dispatchEvent(new KeyboardEvent('keydown', { key: 's', ctrlKey: true }));
    await settle();
    const rows = target.querySelectorAll('dialog .row');
    (rows[1] as HTMLElement).click();
    await settle();
    release();
    await settle();
    await settle();
    // The addressed outcome is reported against the captured session even
    // though root2 is now displayed.
    expect(target.querySelector('.note.outcome')?.textContent).toContain('session root1');
    expect(target.querySelector('.note.outcome')?.textContent).toContain('stop blew up');
  });

  it('delivers live events through the real pull loop into the reader hint path', async () => {
    const newerSessionChanged = JSON.stringify({
      kind: 'session_changed',
      scope: { kind: 'session', session_id: 'root1' },
      session_revision: { instance_id: INSTANCE, durable_revision: '2', local_revision: '0' },
    });
    const { calls } = await mountReal({ events: [newerSessionChanged] });
    await vi.waitFor(() => {
      expect(calls.filter((call) => call.operation === 'GetSessionHydration').length).toBeGreaterThanOrEqual(2);
    });
  });

  it('a malformed event document ends the pull and triggers the real repair reopen', async () => {
    const { calls } = await mountReal({ events: ['{not json'] });
    await vi.waitFor(() => {
      expect(vi.mocked(Open).mock.calls.length).toBeGreaterThan(1);
    });
    // The reopened generation re-establishes SSE and re-reads the root.
    await vi.waitFor(() => {
      expect(calls.filter((call) => call.operation === 'GetSessionHydration').length).toBeGreaterThanOrEqual(2);
    });
  });

  it('a real 204 Stop is a valid success: no note, and the stamped view refreshes', async () => {
    const { target, calls } = await mountReal();
    await settle();
    const readsBefore = calls.filter((c) => c.operation === 'GetSessionHydration').length;
    (target.querySelector('.stop') as HTMLElement).click();
    await settle();
    await settle();
    // The bridge returns a real 204 for Stop; the classification treats it as
    // success, so NO outcome/error/unknown note is rendered...
    expect(target.querySelector('.note')).toBeNull();
    // ...and the stamped root view is refreshed by a further hydration read.
    await vi.waitFor(() => {
      expect(calls.filter((c) => c.operation === 'GetSessionHydration').length).toBeGreaterThan(readsBefore);
    });
    expect(target.querySelector('.note')).toBeNull();
  });

  it.each(['', 'not json'])('a required-body hydration returning a %j 200 is invalid delivery while SSE stays healthy', async (unusableBody) => {
    // Boot reads a valid hydration; the first re-read (a hint-driven refresh)
    // returns a 200 whose body cannot be consumed. The native invalid bit
    // survives the binding and the actual reader repairs through the fresh
    // Open, while the event pull stays live and no blank projection is ever
    // adopted.
    const unusable = { status: 200, content_type: 'application/json', body: unusableBody, invalid: true };
    const newerHint = JSON.stringify({
      kind: 'session_changed',
      scope: { kind: 'session', session_id: 'root1' },
      session_revision: { instance_id: INSTANCE, durable_revision: '2', local_revision: '0' },
    });
    const { target, calls } = await mountReal({ hydration: [unusable], events: [newerHint] });
    await vi.waitFor(() => {
      expect(calls.filter((c) => c.operation === 'GetSessionHydration').length).toBeGreaterThanOrEqual(2);
    });
    // The invalid read drove a repair (a second Open), and no blank view was
    // adopted: the session label remains root1, never reset to a sessionless
    // render from the unusable body.
    await vi.waitFor(() => expect(vi.mocked(Open).mock.calls.length).toBeGreaterThan(1));
    expect(target.textContent).toContain('root1');
  });

  it.each(['success', 'loss', 'invalid'])('a global read completed against a retired connection (%s) neither repopulates globals nor republishes', async (variant) => {
    let releaseOpen: (info: desktopclient.ConnectionInfo) => void = () => {};
    const openGate = new Promise<desktopclient.ConnectionInfo>((resolve) => { releaseOpen = resolve; });
    let releaseRead: () => void = () => {};
    const readGate = new Promise<void>((resolve) => { releaseRead = resolve; });
    const retired = variant === 'success'
      ? rawJSONResponse(configurationView('9', 'stale'))
      : variant === 'loss'
        ? ('throw' as const)
        : { status: 200, content_type: 'application/json', body: '{not json', invalid: true };
    const { target, calls, connection } = await mountReal({
      openGate,
      configuration: [
        { wait: Promise.resolve(), outcome: rawJSONResponse(configurationView('1', 'boot')) },
        { wait: readGate, outcome: retired },
        { wait: Promise.resolve(), outcome: rawJSONResponse(configurationView('1', 'successor')) },
      ],
      events: [
        JSON.stringify({
          kind: 'configuration_changed',
          scope: { kind: 'session', session_id: 'root1' },
          configuration_revision: { instance_id: INSTANCE, generation: '2' },
        }),
        '{not json',
      ],
    });
    // The update's own read parks, then the pull failure opens a successor whose
    // Open is held: the installed connection is retired meanwhile.
    await vi.waitFor(() => expect(calls.filter((call) => call.operation === 'GetConfiguration').length).toBe(2));
    await vi.waitFor(() => expect(vi.mocked(Open).mock.calls.length).toBe(2));
    expect(connection.getInfo()).toBeNull();
    // The retired read completes against the retirement window.
    releaseRead();
    await settle();
    await settle();
    expect(target.querySelector('.note.outcome')).toBeNull();
    // The successor installs and adopts its own global read.
    releaseOpen({ instance_id: INSTANCE, generation: '2', workspace: '/ws' });
    await vi.waitFor(() => expect(connection.getInfo()).not.toBeNull());
    await settle();
    await settle();
    await settle();
    // No extra repair was requested by the retired read...
    expect(vi.mocked(Open).mock.calls.length).toBe(2);
    // ...the stale payload never repopulated the globals, and the successor's
    // own payload did.
    (target.querySelector('[aria-label="Settings"]') as HTMLElement).click();
    await settle();
    await settle();
    (target.querySelector('[aria-label="Agent type"]') as HTMLElement).click();
    await settle();
    expect(target.textContent).toContain('successor');
    expect(target.textContent).not.toContain('stale');
  });

  it('preserves every repeated pending occurrence across initial and replacement hydration', async () => {
    // The producer may buffer an id more than once; the real native
    // call→reader path must render every FIFO occurrence on the initial body
    // and again on a replacement body, without deduplicating or minting
    // attempt rows.
    const firstBody = hydration('root1', {
      pending: {
        steering: [
          { operation_id: 'dup', origin: 'user', content: [{ kind: 'text', text: 'steer first' }] },
          { operation_id: 'dup', origin: 'user', content: [{ kind: 'text', text: 'steer second' }] },
        ],
        queued: [
          { operation_id: 'same', origin: 'user', content: [{ kind: 'text', text: 'queue first' }] },
          { operation_id: 'same', origin: 'user', content: [{ kind: 'text', text: 'queue second' }] },
        ],
      },
    });
    const secondBody = hydration('root1', {
      session_revision: { instance_id: INSTANCE, durable_revision: '2', local_revision: '0' },
      pending: {
        steering: [
          { operation_id: 'dup', origin: 'user', content: [{ kind: 'text', text: 'steer first' }] },
          { operation_id: 'dup', origin: 'user', content: [{ kind: 'text', text: 'steer second' }] },
          { operation_id: 'dup', origin: 'user', content: [{ kind: 'text', text: 'steer third' }] },
        ],
        queued: [
          { operation_id: 'same', origin: 'user', content: [{ kind: 'text', text: 'queue first' }] },
          { operation_id: 'same', origin: 'user', content: [{ kind: 'text', text: 'queue second' }] },
        ],
      },
    });
    const newerHint = JSON.stringify({
      kind: 'session_changed',
      scope: { kind: 'session', session_id: 'root1' },
      session_revision: { instance_id: INSTANCE, durable_revision: '2', local_revision: '0' },
    });
    const { target } = await mountReal({
      hydration: [json(200, firstBody), json(200, secondBody)],
      events: [newerHint],
    });
    // The replacement body is the live one once its re-read lands. It carries
    // three SAME-id steering occurrences and two SAME-id queued occurrences;
    // its first two steering rows are exactly the initial body's duplicates, so
    // this single final render proves both bodies preserved every occurrence.
    await vi.waitFor(() => {
      expect(target.querySelectorAll('.queued-msg.steering')).toHaveLength(3);
    });
    const steeringRows = [...target.querySelectorAll('.queued-msg.steering')];
    expect(steeringRows.map((r) => r.textContent)).toEqual([
      expect.stringContaining('steer first'),
      expect.stringContaining('steer second'),
      expect.stringContaining('steer third'),
    ]);
    const queuedRows = [...target.querySelectorAll('.queued-msg:not(.steering)')];
    expect(queuedRows).toHaveLength(2);
    expect(queuedRows[0].textContent).toContain('queue first');
    expect(queuedRows[1].textContent).toContain('queue second');
    // Pending rows never mint composer attempt rows.
    expect(target.querySelectorAll('.attempt')).toHaveLength(0);
  });
});

describe('Step 8 real generated captures over the byte bridge', () => {
  const RAW_CONFIGURATION = '{"configuration_revision":{"instance_id":"' + INSTANCE + '","generation":"1"},'
    + '"settings":{"sessions":{"auto_archive":true,"archive_after_days":-9223372036854775808,"delete_after_archive_days":1e400},'
    + '"plugins":{"tools":"{\\"max_output_bytes\\":9007199254740993}"}},"agents":[],"providers":[]}';

  const forkInput = {
    item_id: 'i1',
    kind: 'input',
    committed_at: '0',
    operation_id: 'op1',
    origin: 'user',
    content: [{ kind: 'text', text: 'original' }],
  };
  const forkOperation = {
    operation_id: 'op1',
    request_kind: 'message',
    status: 'running',
    admitted_at: '0',
    agent_type: 'primary',
    model: 'prov/m',
    usage: { totals: { by_model: [] } },
  };

  async function submitRewindFork(target: HTMLElement) {
    (target.querySelector('[aria-label="Fork from input i1"]') as HTMLElement).click();
    await settle();
    const checkbox = target.querySelector('[aria-label="Rewind code before Fork"]') as HTMLInputElement;
    checkbox.checked = true;
    checkbox.dispatchEvent(new Event('change', { bubbles: true }));
    await settle();
    (target.querySelector('[aria-label="Submit fork"]') as HTMLElement).click();
  }

  it('proves lossless settings translation: exact read text and exact complete-body save', async () => {
    const { target, calls } = await mountReal({ configurationRaw: RAW_CONFIGURATION });
    (target.querySelector('[aria-label="Settings"]') as HTMLElement).click();
    // The editor's own exact read is the second ConfigurationView request.
    await vi.waitFor(() => {
      expect(calls.filter((call) => call.operation === 'GetConfiguration').length).toBeGreaterThanOrEqual(2);
    });
    (target.querySelector('[aria-label="Save settings"]') as HTMLElement).click();
    await vi.waitFor(() => {
      expect(calls.some((call) => call.operation === 'UpdateConfigurationSettings')).toBe(true);
    });
    const put = calls.find((call) => call.operation === 'UpdateConfigurationSettings');
    expect(put?.params).toBe(
      '{"body":{"settings":{"sessions":{"auto_archive":true,"archive_after_days":-9223372036854775808,"delete_after_archive_days":1e400},'
      + '"plugins":{"tools":"{\\"max_output_bytes\\":9007199254740993}"}}}}',
    );
  });

  it('captures the ordered rewind-then-Fork requests from the shared invocation', async () => {
    const { target, calls } = await mountReal({
      hydration: [json(200, hydration('root1', { conversation: { items: [forkInput] }, operations: [forkOperation] }))],
      rewind: () => ({ status: 200, body: { restored: ['a.ts'], skipped: [] } }),
    });
    await submitRewindFork(target);
    await vi.waitFor(() => expect(calls.some((call) => call.operation === 'ForkSession')).toBe(true));
    const rewindIndex = calls.findIndex((call) => call.operation === 'RevertSessionCode');
    const forkIndex = calls.findIndex((call) => call.operation === 'ForkSession');
    expect(rewindIndex).toBeGreaterThanOrEqual(0);
    expect(forkIndex).toBeGreaterThan(rewindIndex);
    expect((JSON.parse(calls[rewindIndex].params) as { body: unknown }).body).toEqual({ boundary_operation_id: 'op1' });
    const forkBody = (JSON.parse(calls[forkIndex].params) as { body: { boundary_item_id: string; operation_id: string; content: unknown } }).body;
    expect(forkBody.boundary_item_id).toBe('i1');
    expect(forkBody.content).toEqual([{ kind: 'text', text: 'original' }]);
    expect(forkBody.operation_id).toMatch(/^[0-9a-f]{32}$/);
    // The destination was adopted through a real generated hydration read.
    await vi.waitFor(() => {
      expect(calls.some((call) => call.operation === 'GetSessionHydration' && call.params === '{"session_id":"dest1"}')).toBe(true);
    });
  });

  it('never sends Fork after a real transport-unknown rewind', async () => {
    const { target, calls } = await mountReal({
      hydration: [json(200, hydration('root1', { conversation: { items: [forkInput] }, operations: [forkOperation] }))],
      rewind: 'throw',
    });
    await submitRewindFork(target);
    await vi.waitFor(() => expect(target.textContent).toContain('Rewind outcome unknown'));
    expect(calls.filter((call) => call.operation === 'ForkSession')).toHaveLength(0);
    // The retained attempt offers abandonment, never a Fork retry.
    const row = target.querySelector('.attempt.unknown') as HTMLElement;
    expect(row.querySelector('[aria-label="Retry"]')).toBeNull();
    expect(row.querySelector('[aria-label="Abandon"]')).not.toBeNull();
  });

  it('keeps provider and model numeric lexemes exact from a real byte-bridge read to write', async () => {
    const rawDetail = '{"configuration_revision":{"instance_id":"' + INSTANCE + '","generation":"1"},'
      + '"provider":{"id":"prov1","name":"Prov One","builtin":false,"connected":true,"key_source":"managed",'
      + '"base_url":"http://127.0.0.1:1/v1","api_key_env":"PROV1_KEY","headers":{},"user_headers":{},'
      + '"system_role":"system","usage_in_stream":false,"max_tokens_field":"max_tokens","discovery":true,"hidden":false,'
      + '"protocol_metadata":{"limit":9007199254740993,"rate":1e400},'
      + '"models":[{"id":"m1","name":"Model One","context_window":9007199254740993,"max_output_tokens":1e400,'
      + '"input_modalities":["text"],"system_role":"system","usage_in_stream":false,"hidden":false,"source":"user",'
      + '"incomplete":false,"provider_hidden":false,"usable":true,"cost":{"input":9007199254740993,"output":2.5}}]}}';
    const { target, calls } = await mountReal({ providerDetailRaw: rawDetail });
    (target.querySelector('[aria-label="Settings"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Providers"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Select provider prov1"]') as HTMLElement).click();
    await settle();
    await settle();
    // Read -> display: the exact lexemes survive the generated text read.
    expect((target.querySelector('[aria-label="m1 context_window"]') as HTMLInputElement).value).toBe('9007199254740993');
    expect((target.querySelector('[aria-label="m1 max_output_tokens"]') as HTMLInputElement).value).toBe('1e400');
    expect((target.querySelector('[aria-label="m1 cost input"]') as HTMLInputElement).value).toBe('9007199254740993');
    expect((target.querySelector('[aria-label="provider protocol_metadata"]') as HTMLTextAreaElement).value)
      .toBe('{"limit":9007199254740993,"rate":1e400}');
    // Display -> edit -> write: the reconstructed cost member keeps its exact tokens.
    changeInput(target, 'm1 cost output', '3');
    await vi.waitFor(() => expect(calls.some((call) => call.operation === 'UpdateProviderModel')).toBe(true));
    const put = calls.find((call) => call.operation === 'UpdateProviderModel');
    expect(put?.params).toBe('{"query":{"provider_id":"prov1","model_id":"m1"},"body":{"model":{"cost":{"input":9007199254740993,"output":3}}}}');
  });

  it('captures prototype-named selected candidates as ordinary create-body members', async () => {
    const { target, calls } = await mountReal({
      discoverCandidates: [
        { id: '__proto__', name: 'Proto Model', context_window: 4096, max_output_tokens: 512, usable: true },
        { id: 'toString', name: 'String Model', context_window: 2048, max_output_tokens: 256, usable: true },
        { id: 'constructor', name: 'Ctor Model', context_window: 1024, max_output_tokens: 128, usable: false },
      ],
    });
    (target.querySelector('[aria-label="Settings"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Providers"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="New provider"]') as HTMLElement).click();
    await settle();
    // The create form's bound inputs update on input, not change.
    const idInput = target.querySelector('[aria-label="Provider id"]') as HTMLInputElement;
    idInput.value = 'protos';
    idInput.dispatchEvent(new Event('input', { bubbles: true }));
    const baseInput = target.querySelector('[aria-label="Provider base url"]') as HTMLInputElement;
    baseInput.value = 'http://127.0.0.1:9/v1';
    baseInput.dispatchEvent(new Event('input', { bubbles: true }));
    await settle();
    (target.querySelector('[aria-label="Discover provider candidates"]') as HTMLElement).click();
    await settle();
    await settle();
    // An unavailable prototype-named candidate is not selected by inheritance.
    expect((target.querySelector('[aria-label="Select candidate constructor"]') as HTMLInputElement).checked).toBe(false);
    // Unselect the ordinary prototype-named candidate; __proto__ stays selected.
    (target.querySelector('[aria-label="Select candidate toString"]') as HTMLElement).click();
    await settle();
    (target.querySelector('[aria-label="Create provider"]') as HTMLElement).click();
    await vi.waitFor(() => expect(calls.some((call) => call.operation === 'CreateProvider')).toBe(true));
    const create = calls.find((call) => call.operation === 'CreateProvider');
    expect(create?.params).toContain('"models":{"__proto__":{"name":"Proto Model","context_window":4096,"max_output_tokens":512}}');
    expect(create?.params).not.toContain('toString');
    expect(create?.params).not.toContain('constructor');
  });
});

describe('the shared invocation boundary', () => {
  const handle = Object.freeze({ generation: '1' });

  it('rejects malformed authored invocation text locally and dispatches nothing', async () => {
    const result = await invokeNative(
      () => handle,
      'fork',
      'ForkSession',
      '{"session_id":"root1","body":{"boundary_item_id":"i1","operation_id":"op1","content":[ {"kind":"text",',
      JSON.parse,
    );
    expect(result.kind).toBe('rejection');
    if (result.kind === 'rejection') expect(result.payload).toBeUndefined();
    expect(Call).not.toHaveBeenCalled();
  });

  it('passes well-formed opaque overflow lexemes through the syntax check unchanged', async () => {
    vi.mocked(Call).mockResolvedValue(json(200, {}));
    const params = '{"body":{"extra_body":{"n":9007199254740993},"options":{"big":1e400}}}';
    const result = await invokeNative(() => handle, 'edit', 'UpdateProviderDetail', params, JSON.parse);
    expect(result.kind).toBe('ok');
    expect(Call).toHaveBeenCalledExactlyOnceWith('1', 'UpdateProviderDetail', params);
  });

  it('classifies an undecodable accepted body as invalid delivery, distinct from native-call loss', async () => {
    // The exact-text settings decoder cannot consume this valid JSON body.
    vi.mocked(Call).mockResolvedValue(native(200, 'application/json', '{"configuration_revision":{"instance_id":"x","generation":"1"}}', false));
    const undecodable = await invokeNative(() => handle, 'configuration', 'GetConfiguration', '{}', extractSettingsText);
    expect(undecodable.kind).toBe('invalid');
    vi.mocked(Call).mockRejectedValue(new Error('call carries a stale connection generation'));
    const lost = await invokeNative(() => handle, 'configuration', 'GetConfiguration', '{}', extractSettingsText);
    expect(lost.kind).toBe('loss');
  });

  it('treats a null success document as invalid delivery beside its non-null and 204 siblings', async () => {
    vi.mocked(Call).mockResolvedValue({ status: 200, content_type: 'application/json', body: 'null', invalid: false });
    const nulled = await invokeNative(() => handle, 'submit', 'SubmitSession', '{"session_id":"root1","body":{}}', JSON.parse);
    expect(nulled.kind).toBe('invalid');
    if (nulled.kind === 'ok') throw new Error('null document accepted as ok');
    vi.mocked(Call).mockResolvedValue(json(200, { disposition: 'admitted' }));
    const accepted = await invokeNative(() => handle, 'submit', 'SubmitSession', '{"session_id":"root1","body":{}}', JSON.parse);
    expect(accepted.kind).toBe('ok');
    if (accepted.kind === 'ok') expect(accepted.data).toEqual({ disposition: 'admitted' });
    vi.mocked(Call).mockResolvedValue({ status: 204, content_type: 'application/json', body: '', invalid: false });
    const empty = await invokeNative(() => handle, 'interrupt', 'InterruptSession', '{"session_id":"root1"}', JSON.parse);
    expect(empty.kind).toBe('ok');
    if (empty.kind === 'ok') expect(empty.data).toEqual({});
  });
});
