// @vitest-environment happy-dom
// The real-stack mounted owner: the actual RuntimeConnection, its credential-
// free custom fetch, and the generated SDK drive the mounted App over MOCKED
// GENERATED HOST BINDINGS ONLY. This is where the actual throwOnError=false
// contract is proven: a received status is always a result and only a binding
// rejection leaves the response absent. Nothing here mocks the SDK surface.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { mount, tick, unmount } from 'svelte';

vi.mock('../../client-bindings/wailsjs/go/desktopclient/Host', () => ({
  NextEvent: vi.fn(),
  Open: vi.fn(),
  Request: vi.fn(),
  PatchJSON: vi.fn(),
  PickDirectory: vi.fn(),
  SetTitle: vi.fn(),
}));

import { Open, NextEvent, Request, PatchJSON, PickDirectory, SetTitle } from '../../client-bindings/wailsjs/go/desktopclient/Host';
import { RuntimeConnection } from './connection';
import { parseExactJSON } from './exactjson';

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

function json(status: number, value: unknown) {
  return { status, content_type: 'application/json', body: JSON.stringify(value) };
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
  hydration?: Array<{ status: number; content_type: string; body: string }>;
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
  const calls: Array<{ method: string; path: string; body: string; generation: string }> = [];
  vi.mocked(Request).mockImplementation(async (generation, method, path, body) => {
    calls.push({ generation, method, path, body });
    const url = new URL(path, 'http://runtime.invalid');
    const p = url.pathname;
    if (method === 'GET' && p === '/v1/sessions') {
      return json(200, [session('root1', NEWER), session('root2', OLDER)]);
    }
    const hydrationMatch = /^\/v1\/sessions\/([^/]+)\/hydration$/.exec(p);
    if (method === 'GET' && hydrationMatch) {
      if (script.hydration && script.hydration.length > 0) {
        return script.hydration.shift();
      }
      return json(200, hydration(hydrationMatch[1]));
    }
    if (method === 'GET' && p === '/v1/configuration') {
      if (script.configurationRaw) {
        return { status: 200, content_type: 'application/json', body: script.configurationRaw };
      }
      return json(200, {
        configuration_revision: { instance_id: INSTANCE, generation: '1' },
        settings: { sessions: { auto_archive: false, archive_after_days: 1, delete_after_archive_days: 1 }, plugins: {} },
        agents: [],
        providers: [],
      });
    }
    if (method === 'PUT' && p === '/v1/configuration/settings') {
      return json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: { sessions: {}, plugins: {} } });
    }
    if (method === 'GET' && p === '/v1/providers') {
      return json(200, {
        configuration_revision: { instance_id: INSTANCE, generation: '1' },
        providers: [{ id: 'prov1', name: 'Prov One', builtin: false, connected: true }],
      });
    }
    if (method === 'GET' && p === '/v1/providers/detail') {
      if (script.providerDetailRaw) {
        return { status: 200, content_type: 'application/json', body: script.providerDetailRaw };
      }
      return json(404, { code: 'not_found', message: 'no provider detail fixture' });
    }
    if (method === 'PUT' && p === '/v1/providers/detail') {
      return json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: null });
    }
    if (method === 'PUT' && p === '/v1/providers/models') {
      return json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: null });
    }
    if (method === 'POST' && p === '/v1/providers/models/discover') {
      return json(200, []);
    }
    if (method === 'POST' && p === '/v1/providers/discover') {
      return json(200, script.discoverCandidates ?? []);
    }
    if (method === 'POST' && p === '/v1/providers') {
      return json(200, { configuration_revision: { instance_id: INSTANCE, generation: '2' }, result: null });
    }
    const rewindMatch = /^\/v1\/sessions\/([^/]+)\/revert-code$/.exec(p);
    if (method === 'POST' && rewindMatch) {
      if (script.rewind === 'throw') throw new Error('native bridge: rewind lost');
      if (typeof script.rewind === 'function') {
        const outcome = script.rewind(JSON.parse(body));
        return json(outcome.status, outcome.body);
      }
      return json(200, { restored: [], skipped: [] });
    }
    const forkMatch = /^\/v1\/sessions\/([^/]+)\/fork$/.exec(p);
    if (method === 'POST' && forkMatch) {
      const request = JSON.parse(body) as { operation_id: string };
      return json(200, {
        session: session('dest1', NEWER),
        operation: { operation_id: request.operation_id, request_kind: 'message', status: 'running', admitted_at: '0', agent_type: 'primary', model: 'prov/m', usage: { totals: { by_model: [] } } },
      });
    }
    if (method === 'GET' && p === '/v1/warnings') {
      return json(200, { warnings_revision: { instance_id: INSTANCE, revision: '1' }, warnings: [] });
    }
    const submitMatch = /^\/v1\/sessions\/([^/]+)\/submit$/.exec(p);
    if (method === 'POST' && submitMatch) {
      if (script.submit === 'throw') {
        throw new Error('native bridge: connection lost mid-request');
      }
      if (typeof script.submit === 'function') {
        const outcome = script.submit(JSON.parse(body));
        if ('delay' in outcome) {
          const resolved = await outcome.delay;
          if (resolved.status === 0) throw new Error('native bridge: late loss');
          return json(resolved.status, resolved.body);
        }
        return json(outcome.status, outcome.body);
      }
      return json(200, { disposition: 'admitted' });
    }
    const compactMatch = /^\/v1\/sessions\/([^/]+)\/compact$/.exec(p);
    if (method === 'POST' && compactMatch) {
      if (script.compact === 'throw' || (typeof script.compact === 'function' && script.compact(JSON.parse(body)) === 'throw')) {
        throw new Error('native bridge: connection lost mid-request');
      }
      return json(200, { operation_id: JSON.parse(body).operation_id, request_kind: 'compact', status: 'running', admitted_at: '0', agent_type: 'primary', model: 'prov/m', usage: { totals: { by_model: [] } } });
    }
    if (/^\/v1\/sessions\/[^/]+\/interrupt$/.test(p) && method === 'POST') {
      return { status: 204, content_type: '', body: '' };
    }
    const stopMatch = /^\/v1\/sessions\/([^/]+)\/stop$/.exec(p);
    if (method === 'POST' && stopMatch && script.stop) {
      const outcome = script.stop();
      if (outcome === 'throw') throw new Error('native bridge: stop lost');
      if ('delay' in outcome) {
        const resolved = await outcome.delay;
        if (resolved.status === 0) throw new Error('native bridge: stop lost');
        return json(resolved.status, resolved.body);
      }
      return json(outcome.status, outcome.body);
    }
    if (/^\/v1\/sessions\/[^/]+\/stop$/.test(p) && method === 'POST') {
      return { status: 204, content_type: '', body: '' };
    }
    return json(404, { code: 'not_found', message: 'no fixture for ' + method + ' ' + p });
  });
  vi.mocked(Open).mockResolvedValue({ instance_id: INSTANCE, generation: '1', workspace: '/ws' });
  vi.mocked(SetTitle).mockResolvedValue(undefined);
  vi.mocked(PickDirectory).mockResolvedValue('');
  vi.mocked(PatchJSON).mockImplementation(async (document, patch) => {
    const members = parseExactJSON(document) as Record<string, unknown>;
    const edits = parseExactJSON(patch) as Record<string, unknown>;
    for (const [name, value] of Object.entries(edits)) members[name] = value;
    return JSON.stringify(members);
  });
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
    expect(calls.some((call) => call.path.includes('/hydration'))).toBe(true);
  });
  await tick();
  return { connection, component, target, calls };
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
  it('composes the initial view through the actual SDK request encoding', async () => {
    const { target, calls } = await mountReal();
    expect(calls.some((call) => call.method === 'GET' && call.path === '/v1/sessions?workspace=%2Fws&lifecycle=open')).toBe(true);
    expect(calls.some((call) => call.path === '/v1/configuration')).toBe(true);
    expect(calls.some((call) => call.path === '/v1/warnings')).toBe(true);
    const hydrationCall = calls.find((call) => call.path.includes('/hydration'));
    expect(hydrationCall?.path).toBe('/v1/sessions/root1/hydration');
    expect(hydrationCall?.generation).toBe('1');
    expect(target.textContent).toContain('root1');
  });

  it('a real native Request rejection is a transport loss, not a server rejection', async () => {
    const { target } = await mountReal({ submit: 'throw' });
    typeInto(target, 'lost message');
    await settle();
    (target.querySelector('.send') as HTMLElement).click();
    await settle();
    await settle();
    // The generated SDK resolves {error, response: undefined}; the consumer
    // must show Outcome unknown, never a generated protocol error.
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
    const submit = calls.find((call) => call.path.endsWith('/submit'));
    expect(submit).toBeDefined();
    const body = JSON.parse(submit!.body) as { operation_id: string; mode: string; content: unknown[] };
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
    expect(calls.filter((call) => call.path.endsWith('/submit'))).toHaveLength(1);
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
    const compactCall = calls.find((call) => call.path.endsWith('/compact'));
    expect(compactCall).toBeDefined();
    expect((JSON.parse(compactCall!.body) as { operation_id: string }).operation_id).toMatch(/^[0-9a-f]{32}$/);
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
      expect(calls.filter((call) => call.path.includes('/hydration')).length).toBeGreaterThanOrEqual(2);
    });
  });

  it('a malformed event document ends the pull and triggers the real repair reopen', async () => {
    const { calls } = await mountReal({ events: ['{not json'] });
    await vi.waitFor(() => {
      expect(vi.mocked(Open).mock.calls.length).toBeGreaterThan(1);
    });
    // The reopened generation re-establishes SSE and re-reads the root.
    await vi.waitFor(() => {
      expect(calls.filter((call) => call.path.includes('/hydration')).length).toBeGreaterThanOrEqual(2);
    });
  });

  it('a real 204 Stop is a valid success: no note, and the stamped view refreshes', async () => {
    const { target, calls } = await mountReal();
    await settle();
    const readsBefore = calls.filter((c) => c.path.includes('/hydration')).length;
    (target.querySelector('.stop') as HTMLElement).click();
    await settle();
    await settle();
    // The bridge returns a real 204 for /stop; the classification treats it as
    // success, so NO outcome/error/unknown note is rendered...
    expect(target.querySelector('.note')).toBeNull();
    // ...and the stamped root view is refreshed by a further hydration read.
    await vi.waitFor(() => {
      expect(calls.filter((c) => c.path.includes('/hydration')).length).toBeGreaterThan(readsBefore);
    });
    expect(target.querySelector('.note')).toBeNull();
  });

  it('a required-body hydration returning an empty 200 is invalid delivery, never adopted as a Hydration', async () => {
    // Boot reads a valid hydration; the first re-read (a hint-driven refresh)
    // returns a 200 with an empty application/json body. The SDK yields no
    // data; the reader must route that to repair, never render it as a
    // (blank) projection.
    const empty = { status: 200, content_type: 'application/json', body: '' };
    const newerHint = JSON.stringify({
      kind: 'session_changed',
      scope: { kind: 'session', session_id: 'root1' },
      session_revision: { instance_id: INSTANCE, durable_revision: '2', local_revision: '0' },
    });
    const { target, calls } = await mountReal({ hydration: [empty], events: [newerHint] });
    await vi.waitFor(() => {
      expect(calls.filter((c) => c.path.includes('/hydration')).length).toBeGreaterThanOrEqual(2);
    });
    // The invalid read drove a repair (a second Open), and no blank view was
    // adopted: the session label remains root1, never reset to a sessionless
    // render from the empty body.
    await vi.waitFor(() => expect(vi.mocked(Open).mock.calls.length).toBeGreaterThan(1));
    expect(target.textContent).toContain('root1');
  });

  it('preserves every repeated pending occurrence across initial and replacement hydration (real SDK)', async () => {
    // The producer may buffer an id more than once; the real fetch→SDK→reader
    // path must render every FIFO occurrence on the initial body and again on
    // a replacement body, without deduplicating or minting attempt rows.
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
      expect(calls.filter((call) => call.method === 'GET' && call.path === '/v1/configuration').length).toBeGreaterThanOrEqual(2);
    });
    (target.querySelector('[aria-label="Save settings"]') as HTMLElement).click();
    await vi.waitFor(() => {
      expect(calls.some((call) => call.method === 'PUT' && call.path === '/v1/configuration/settings')).toBe(true);
    });
    const put = calls.find((call) => call.method === 'PUT' && call.path === '/v1/configuration/settings');
    expect(put?.body).toBe(
      '{"settings":{"sessions":{"auto_archive":true,"archive_after_days":-9223372036854775808,"delete_after_archive_days":1e400},'
      + '"plugins":{"tools":"{\\"max_output_bytes\\":9007199254740993}"}}}',
    );
  });

  it('captures the ordered rewind-then-Fork requests from the actual generated SDK', async () => {
    const { target, calls } = await mountReal({
      hydration: [json(200, hydration('root1', { conversation: { items: [forkInput] }, operations: [forkOperation] }))],
      rewind: () => ({ status: 200, body: { restored: ['a.ts'], skipped: [] } }),
    });
    await submitRewindFork(target);
    await vi.waitFor(() => expect(calls.some((call) => call.path.endsWith('/fork'))).toBe(true));
    const rewindIndex = calls.findIndex((call) => call.path.endsWith('/revert-code'));
    const forkIndex = calls.findIndex((call) => call.path.endsWith('/fork'));
    expect(rewindIndex).toBeGreaterThanOrEqual(0);
    expect(forkIndex).toBeGreaterThan(rewindIndex);
    expect(JSON.parse(calls[rewindIndex].body)).toEqual({ boundary_operation_id: 'op1' });
    const forkBody = JSON.parse(calls[forkIndex].body) as { boundary_item_id: string; operation_id: string; content: unknown };
    expect(forkBody.boundary_item_id).toBe('i1');
    expect(forkBody.content).toEqual([{ kind: 'text', text: 'original' }]);
    expect(forkBody.operation_id).toMatch(/^[0-9a-f]{32}$/);
    // The destination was adopted through a real generated hydration read.
    await vi.waitFor(() => {
      expect(calls.some((call) => call.method === 'GET' && call.path === '/v1/sessions/dest1/hydration')).toBe(true);
    });
  });

  it('never sends Fork after a real transport-unknown rewind', async () => {
    const { target, calls } = await mountReal({
      hydration: [json(200, hydration('root1', { conversation: { items: [forkInput] }, operations: [forkOperation] }))],
      rewind: 'throw',
    });
    await submitRewindFork(target);
    await vi.waitFor(() => expect(target.textContent).toContain('Rewind outcome unknown'));
    expect(calls.filter((call) => call.path.endsWith('/fork'))).toHaveLength(0);
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
    await vi.waitFor(() => expect(calls.some((call) => call.method === 'PUT' && call.path.startsWith('/v1/providers/models'))).toBe(true));
    const put = calls.find((call) => call.method === 'PUT' && call.path.startsWith('/v1/providers/models'));
    expect(put?.body).toBe('{"model":{"cost":{"input":9007199254740993,"output":3}}}');
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
    await vi.waitFor(() => expect(calls.some((call) => call.method === 'POST' && call.path === '/v1/providers')).toBe(true));
    const create = calls.find((call) => call.method === 'POST' && call.path === '/v1/providers');
    expect(create?.body).toContain('"models":{"__proto__":{"name":"Proto Model","context_window":4096,"max_output_tokens":512}}');
    expect(create?.body).not.toContain('toString');
    expect(create?.body).not.toContain('constructor');
  });
});
